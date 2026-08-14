package setup

import (
	"cmp"
	"fmt"
	"slices"

	"github.com/enj/soapbox/tools/internal/config"
)

// upgradeOwnedPaths is the set of paths the upgrade package owns. It includes
// everything ComposePayload produces except the root go.mod, which is generated
// module output that upgrade must never overwrite.
var upgradeOwnedPaths = map[string]bool{
	toolsGoModPath:   true,
	toolsGoSumPath:   true,
	toolsMainPath:    true,
	ciWorkflowPath:   true,
	syncWorkflowPath: true,
}

// ComposeUpgradePayload builds the upgrade-owned subset of the setup payload:
// the nested tools module, the engine shim, and the workflows. The root go.mod
// is excluded because it is generated module output. The returned slice is a
// fresh copy every call.
func ComposeUpgradePayload(cfg *config.Config, engineVersion string, engineMod, engineSum []byte) ([]ComposedFile, error) {
	full, err := ComposePayload(cfg, engineVersion, engineMod, engineSum)
	if err != nil {
		return nil, err
	}
	filtered := make([]ComposedFile, 0, len(full))
	for _, f := range full {
		if upgradeOwnedPaths[f.Path] {
			filtered = append(filtered, f)
		}
	}
	return filtered, nil
}

// ComposedFile is one file setup owns, with the exact content it will hold in
// a derived repository.
type ComposedFile struct {
	Path     string
	Contents []byte
}

// ComposePayload builds every file setup owns for a given profile and engine
// pin. It is the public entry point that the upgrade package uses to compute
// what the derived repository should contain, without requiring the repository
// to be a template checkout.
func ComposePayload(cfg *config.Config, engineVersion string, engineMod, engineSum []byte) ([]ComposedFile, error) {
	pin, err := parseEnginePin(engineVersion)
	if err != nil {
		return nil, fmt.Errorf("compose: %w", err)
	}

	engineRequires, err := engineRequirements(engineMod)
	if err != nil {
		return nil, fmt.Errorf("compose: tools go.mod: %w", err)
	}

	rootMod, err := composeRootGoMod(cfg.Destination.Module)
	if err != nil {
		return nil, fmt.Errorf("compose: root go.mod: %w", err)
	}
	toolsMod, err := composeToolsGoMod(cfg.Destination.Module, pin, engineRequires)
	if err != nil {
		return nil, fmt.Errorf("compose: tools go.mod: %w", err)
	}

	inputs := workflowInputs{
		branch:    cfg.Destination.Branch,
		goVersion: goVersionOf(cfg.Determinism.Toolchain),
		mode:      cfg.Publication.Mode,
	}
	if err := inputs.check(); err != nil {
		return nil, fmt.Errorf("compose: %w", err)
	}

	payload := []ComposedFile{
		{Path: rootGoModPath, Contents: rootMod},
		{Path: toolsGoModPath, Contents: toolsMod},
		{Path: toolsMainPath, Contents: composeToolsMain()},
		{Path: ciWorkflowPath, Contents: composeCIWorkflow(inputs)},
		{Path: syncWorkflowPath, Contents: composeSyncWorkflow(inputs)},
	}

	sum, err := composeEngineSum(engineSum, pin, engineRequires)
	if err != nil {
		return nil, fmt.Errorf("compose: %w", err)
	}
	if sum != nil {
		payload = append(payload, ComposedFile{Path: toolsGoSumPath, Contents: sum})
	}

	slices.SortFunc(payload, func(a, b ComposedFile) int { return cmp.Compare(a.Path, b.Path) })

	// Validate paths.
	seen := make(map[string]bool, len(payload))
	for _, file := range payload {
		if err := config.ValidateRelPath(file.Path); err != nil {
			return nil, fmt.Errorf("compose: payload path %q: %w", file.Path, err)
		}
		if seen[file.Path] {
			return nil, fmt.Errorf("compose: payload path %q is composed twice", file.Path)
		}
		seen[file.Path] = true
	}
	return payload, nil
}

// compose builds every file setup owns, in full, before anything is written.
//
// The payload is composed rather than copied. Nothing in the derived repository
// that setup produces is a template file with edits applied to it, because an
// edit is a function of what the template happened to contain and a composition
// is a function of the profile alone. That is what makes two runs over the same
// profile produce the same bytes.
func (r *run) compose(_ enginePin) error {
	files, err := ComposePayload(r.opts.Config, r.opts.EngineVersion, r.opts.EngineMod, r.opts.EngineSum)
	if err != nil {
		return policyErrorf("setup: %w", err)
	}
	r.payload = make([]composedFile, len(files))
	for i, f := range files {
		r.payload[i] = composedFile{path: f.Path, contents: f.Contents}
	}
	// Check if go.sum was included.
	for _, f := range files {
		if f.Path == toolsGoSumPath {
			r.composedSum = true
		}
	}
	if !r.composedSum {
		r.notices = append(r.notices, engineSumNotice)
	}
	return r.checkPayload()
}

// checkPayload refuses a payload path that could escape the repository or land
// somewhere other than where it reads.
//
// Every path here is a constant of this package, so the check can only fail
// after an edit to the allowlist. That is exactly when it is worth having: the
// allowlist is the security boundary, and a boundary nothing verifies is a
// comment.
func (r *run) checkPayload() error {
	seen := make(map[string]bool, len(r.payload))
	for _, file := range r.payload {
		if err := config.ValidateRelPath(file.path); err != nil {
			return fmt.Errorf("setup: payload path %q: %w", file.path, err)
		}
		if seen[file.path] {
			return fmt.Errorf("setup: payload path %q is composed twice", file.path)
		}
		seen[file.path] = true
	}
	return nil
}
