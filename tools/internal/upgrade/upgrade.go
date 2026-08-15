// Package upgrade implements approval-gated upgrades of derived repositories.
//
// An upgrade recomposes the files setup owns — the nested tools go.mod and
// go.sum, the engine shim, and the two workflows — against the current profile
// and a new engine release. The root go.mod is generated module output and is
// never overwritten. Files whose composed content already matches what is on
// disk are left alone; files that differ are written atomically; and everything
// else in the repository is preserved untouched.
//
// The approval gate is the same hash-based mechanism setup uses: an upgrade is
// planned, rendered as a manifest, and applied only when the operator passes the
// manifest hash back. This ensures that an operator reviews every change,
// including workflow permission changes, before the repository is modified.
//
// An upgrade refuses to run on a repository that is not a setup-produced derived
// repository, that has uncommitted changes, that would downgrade the engine, or
// that contains setup-owned paths the operator has added outside of setup.
package upgrade

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/semver"

	"github.com/enj/soapbox/tools/internal/config"
	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/setup"
)

// Sentinel errors the upgrade package returns.
var (
	// ErrDirty means the work tree has uncommitted changes.
	ErrDirty = errors.New("work tree is dirty")
	// ErrNotDerived means the repository is not a setup-produced derived
	// repository.
	ErrNotDerived = errors.New("not a derived repository")
	// ErrUnknownOverwrite means a file that would be written exists but was not
	// produced by setup.
	ErrUnknownOverwrite = errors.New("refusing to overwrite an unrecognised file")
	// ErrApproval means the approval hash does not match the current plan.
	ErrApproval = errors.New("approval does not match the current manifest")
	// ErrUnsafePath means an owned path is not a regular file (it may be a
	// symbolic link, device, or directory where a regular file is expected).
	ErrUnsafePath = errors.New("owned path is not a regular file")
	// ErrDowngrade means the target engine version is older than the current.
	ErrDowngrade = errors.New("refusing to downgrade the engine")
)

// Options carries the inputs for an upgrade.
type Options struct {
	// Root is the absolute path to the derived repository.
	Root string
	// Config is the validated profile the repository carries.
	Config *config.Config
	// EngineVersion is the target engine release, such as tools/v1.5.0 or
	// v1.5.0.
	EngineVersion string
	// EngineMod is the go.mod of the target engine release.
	EngineMod []byte
	// EngineSum is the verified go.sum content for the nested tools module.
	EngineSum []byte
	// MigratedProfile is the migrated soapbox.yaml content. When non-nil the
	// upgrade manifest includes the profile so the approval hash binds the
	// profile change.
	MigratedProfile []byte
	// ProfilePath is the repo-relative path to the profile file, such as
	// "soapbox.yaml". Used as the derived-repository marker and as the
	// destination when MigratedProfile is written.
	ProfilePath string
	// Git drives the repository.
	Git *gitcli.Runner
}

func (o Options) check() error {
	switch {
	case o.Root == "":
		return errors.New("a repository root is required")
	case !filepath.IsAbs(o.Root):
		return fmt.Errorf("repository root %q must be absolute", o.Root)
	case o.Root != filepath.Clean(o.Root):
		return fmt.Errorf("repository root %q must be a cleaned path", o.Root)
	case o.Config == nil:
		return errors.New("a validated profile is required")
	case len(o.EngineMod) == 0:
		return errors.New("the target engine go.mod is required")
	case len(strings.TrimSpace(string(o.EngineSum))) == 0:
		return errors.New("the verified engine go.sum is required and must not be empty")
	case o.Git == nil:
		return errors.New("a git runner is required")
	}
	if o.ProfilePath != "" {
		if err := config.ValidateRelPath(o.ProfilePath); err != nil {
			return fmt.Errorf("profile path: %w", err)
		}
	}
	return nil
}

// effectiveProfilePath returns the repo-relative profile path.
func (o Options) effectiveProfilePath() string {
	if o.ProfilePath != "" {
		return o.ProfilePath
	}
	return config.DefaultFileName
}

// Result is the outcome of a plan or apply.
type Result struct {
	// Report is the upgrade manifest.
	Report Report
	// Applied is true when the manifest was written.
	Applied bool
	// Partial is true when apply failed after at least one path may have
	// changed.
	Partial bool
}

// Report is the upgrade manifest.
type Report struct {
	Schema  int      `json:"schema"`
	Engine  Engine   `json:"engine"`
	Actions []Action `json:"actions"`
	Kept    []string `json:"kept"`
	Totals  Totals   `json:"totals"`
	Notices []string `json:"notices"`
	Hash    string   `json:"hash"`
}

// Engine describes the engine release being upgraded.
type Engine struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// The kinds of change an upgrade manifest records.
const (
	ActionUpdate = "update"
	ActionCreate = "create"
)

// Action is one file write.
type Action struct {
	Path     string `json:"path"`
	Kind     string `json:"kind"`
	Digest   string `json:"digest"`
	Previous string `json:"previous,omitempty"`
	Bytes    int    `json:"bytes"`
}

// Totals count the manifest by kind.
type Totals struct {
	Update    int `json:"update"`
	Create    int `json:"create"`
	Unchanged int `json:"unchanged"`
}

// PolicyError reports that the upgrade ran and the answer is no.
type PolicyError struct{ Err error }

func (e *PolicyError) Error() string { return e.Err.Error() }
func (e *PolicyError) Unwrap() error { return e.Err }

// parseTargetVersion strips the one allowed tools/ prefix and validates the
// result as semver. It is called before any comparison so malformed input
// cannot misclassify as a downgrade.
func parseTargetVersion(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	// Strip exactly one tools/ prefix. Repeated or other prefixes are refused.
	v = strings.TrimPrefix(v, setup.EngineTagPrefix)
	if strings.HasPrefix(v, setup.EngineTagPrefix) || (!strings.HasPrefix(v, "v") && v != "") {
		return "", fmt.Errorf("engine version %q has an invalid prefix", raw)
	}
	if !semver.IsValid(v) {
		return "", fmt.Errorf("engine version %q is not valid semver", raw)
	}
	return v, nil
}

// Plan computes what an upgrade would write without changing anything.
func Plan(ctx context.Context, opts Options) (*Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("upgrade: %w", err)
	}
	if err := opts.check(); err != nil {
		return nil, fmt.Errorf("upgrade: %w", err)
	}

	// Parse and validate target version before any comparison.
	targetVersion, err := parseTargetVersion(opts.EngineVersion)
	if err != nil {
		return nil, fmt.Errorf("upgrade: %w", err)
	}
	canonicalTarget := setup.EngineTagPrefix + targetVersion

	// Verify the repository root matches the git runner's root.
	root, err := opts.Git.RepositoryRoot(ctx)
	if err != nil {
		return nil, fmt.Errorf("upgrade: %w", err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("upgrade: resolve repository root: %w", err)
	}
	resolvedOpt, err := filepath.EvalSymlinks(opts.Root)
	if err != nil {
		return nil, fmt.Errorf("upgrade: resolve %s: %w", opts.Root, err)
	}
	if resolvedRoot != resolvedOpt {
		return nil, &PolicyError{Err: fmt.Errorf("upgrade: %s is inside the repository rooted at %s", opts.Root, root)}
	}

	// Refuse a dirty work tree.
	status, err := opts.Git.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("upgrade: %w", err)
	}
	if len(status) > 0 {
		return nil, &PolicyError{Err: fmt.Errorf("upgrade: %w: %d paths", ErrDirty, len(status))}
	}

	// Build the tracked set.
	entries, err := opts.Git.ListTree(ctx, "HEAD")
	if err != nil {
		return nil, fmt.Errorf("upgrade: %w", err)
	}
	tracked := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		tracked[entry.Path] = struct{}{}
	}
	profilePath := opts.effectiveProfilePath()

	// Require markers tracked.
	if _, ok := tracked["go.mod"]; !ok {
		return nil, &PolicyError{Err: fmt.Errorf("upgrade: %w: no root go.mod is tracked", ErrNotDerived)}
	}
	if _, ok := tracked[profilePath]; !ok {
		return nil, &PolicyError{Err: fmt.Errorf("upgrade: %w: no %s is tracked", ErrNotDerived, profilePath)}
	}

	readRoot, err := os.OpenRoot(opts.Root)
	if err != nil {
		return nil, fmt.Errorf("upgrade: open repository: %w", err)
	}
	defer func() { _ = readRoot.Close() }()

	// Validate derived repo: Lstat + regular-file check + module parse for
	// both go.mod and tools/go.mod. I/O failures are runtime errors, not
	// policy findings.
	currentPin, err := readDerivedEnginePin(readRoot, opts.Config.Destination.Module)
	if err != nil {
		// Separate I/O failures from policy findings.
		var pe *policyFinding
		if errors.As(err, &pe) {
			return nil, &PolicyError{Err: fmt.Errorf("upgrade: %w: %v", ErrNotDerived, pe.reason)}
		}
		return nil, fmt.Errorf("upgrade: %w", err)
	}

	// Refuse downgrades.
	if semver.Compare(targetVersion, currentPin) < 0 {
		return nil, &PolicyError{Err: fmt.Errorf("upgrade: %w: current %s, target %s", ErrDowngrade, currentPin, targetVersion)}
	}

	// Validate MigratedProfile bytes match the Config used to compose.
	if opts.MigratedProfile != nil {
		decoded, decErr := config.Decode(opts.MigratedProfile)
		if decErr != nil {
			return nil, fmt.Errorf("upgrade: migrated profile is not valid: %w", decErr)
		}
		wantCanon, wantErr := opts.Config.Canonical()
		if wantErr != nil {
			return nil, fmt.Errorf("upgrade: encode Config canonical: %w", wantErr)
		}
		gotCanon, gotErr := decoded.Canonical()
		if gotErr != nil {
			return nil, fmt.Errorf("upgrade: encode migrated profile canonical: %w", gotErr)
		}
		if !bytes.Equal(wantCanon, gotCanon) {
			return nil, &PolicyError{Err: fmt.Errorf("upgrade: %w: migrated profile bytes do not match the Config used to compose workflows", ErrApproval)}
		}
	}

	// Compose the upgrade-owned files.
	payload, err := setup.ComposeUpgradePayload(opts.Config, opts.EngineVersion, opts.EngineMod, opts.EngineSum)
	if err != nil {
		return nil, fmt.Errorf("upgrade: %w", err)
	}

	// Require tools/go.sum in the composed payload.
	hasToolsSum := false
	for _, f := range payload {
		if f.Path == "tools/go.sum" {
			hasToolsSum = true
		}
	}
	if !hasToolsSum {
		return nil, fmt.Errorf("upgrade: the composed payload does not include tools/go.sum; the engine checksums are unusable")
	}

	// Include the migrated profile in the payload.
	if opts.MigratedProfile != nil {
		payload = append(payload, setup.ComposedFile{
			Path:     profilePath,
			Contents: opts.MigratedProfile,
		})
	}

	// Compare composed files against on-disk content.
	var actions []Action
	var kept []string
	for _, file := range payload {
		fsPath := filepath.FromSlash(file.Path)

		// Lstat before reading. Symlinks/devices/dirs are refused.
		info, lstatErr := readRoot.Lstat(fsPath)
		if lstatErr != nil && !errors.Is(lstatErr, os.ErrNotExist) {
			return nil, fmt.Errorf("upgrade: stat %s: %w", file.Path, lstatErr)
		}
		if lstatErr == nil && !info.Mode().IsRegular() {
			return nil, &PolicyError{Err: fmt.Errorf("upgrade: %w: %s", ErrUnsafePath, file.Path)}
		}

		if errors.Is(lstatErr, os.ErrNotExist) {
			actions = append(actions, Action{
				Path:   file.Path,
				Kind:   ActionCreate,
				Digest: digest(file.Contents),
				Bytes:  len(file.Contents),
			})
			continue
		}

		// Require the existing file to be tracked before comparing bytes.
		// An untracked file with identical bytes would be silently kept,
		// leaving it outside Git.
		if _, ok := tracked[file.Path]; !ok {
			return nil, &PolicyError{Err: fmt.Errorf("upgrade: %w: %s is not tracked", ErrUnknownOverwrite, file.Path)}
		}

		existing, readErr := readFileFromRoot(readRoot, fsPath)
		if readErr != nil {
			return nil, fmt.Errorf("upgrade: read %s: %w", file.Path, readErr)
		}

		if bytes.Equal(existing, file.Contents) {
			kept = append(kept, file.Path)
			continue
		}

		actions = append(actions, Action{
			Path:     file.Path,
			Kind:     ActionUpdate,
			Digest:   digest(file.Contents),
			Previous: digest(existing),
			Bytes:    len(file.Contents),
		})
	}

	slices.SortFunc(actions, func(a, b Action) int {
		return strings.Compare(a.Path, b.Path)
	})
	slices.Sort(kept)
	if kept == nil {
		kept = []string{}
	}

	var totals Totals
	for _, a := range actions {
		switch a.Kind {
		case ActionCreate:
			totals.Create++
		case ActionUpdate:
			totals.Update++
		}
	}
	totals.Unchanged = len(kept)

	currentTag := setup.EngineTagPrefix + currentPin
	report := Report{
		Schema:  1,
		Engine:  Engine{From: currentTag, To: canonicalTarget},
		Actions: actions,
		Kept:    kept,
		Totals:  totals,
		Notices: []string{},
	}
	hash, err := report.computeHash()
	if err != nil {
		return nil, err
	}
	report.Hash = hash

	return &Result{Report: report}, nil
}

// Apply writes the approved upgrade manifest into the repository.
func Apply(ctx context.Context, opts Options, approve string) (*Result, error) {
	approve = strings.TrimSpace(approve)
	if approve == "" {
		return nil, &PolicyError{Err: fmt.Errorf("upgrade: %w: an approval is required", ErrApproval)}
	}

	result, err := Plan(ctx, opts)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(approve, result.Report.Hash) {
		return result, &PolicyError{Err: fmt.Errorf("upgrade: %w: approved %s, manifest is %s", ErrApproval, approve, result.Report.Hash)}
	}

	if len(result.Report.Actions) == 0 {
		result.Applied = true
		return result, nil
	}

	// Recompose to get file contents.
	payload, err := setup.ComposeUpgradePayload(opts.Config, opts.EngineVersion, opts.EngineMod, opts.EngineSum)
	if err != nil {
		return result, fmt.Errorf("upgrade: %w", err)
	}
	contentByPath := make(map[string][]byte, len(payload)+1)
	for _, file := range payload {
		contentByPath[file.Path] = file.Contents
	}
	if opts.MigratedProfile != nil {
		contentByPath[opts.effectiveProfilePath()] = opts.MigratedProfile
	}

	root, err := os.OpenRoot(opts.Root)
	if err != nil {
		return result, fmt.Errorf("upgrade: open repository: %w", err)
	}
	defer func() { _ = root.Close() }()

	// Preflight: verify every precondition before the first write.
	for _, action := range result.Report.Actions {
		fsPath := filepath.FromSlash(action.Path)
		info, statErr := root.Lstat(fsPath)
		switch action.Kind {
		case ActionCreate:
			if statErr == nil {
				return result, &PolicyError{Err: fmt.Errorf("upgrade: %w: %s appeared after planning", ErrApproval, action.Path)}
			}
			if !errors.Is(statErr, os.ErrNotExist) {
				return result, fmt.Errorf("upgrade: stat %s: %w", action.Path, statErr)
			}
		case ActionUpdate:
			if errors.Is(statErr, os.ErrNotExist) {
				return result, &PolicyError{Err: fmt.Errorf("upgrade: %w: %s disappeared after planning", ErrApproval, action.Path)}
			}
			if statErr != nil {
				return result, fmt.Errorf("upgrade: stat %s: %w", action.Path, statErr)
			}
			if !info.Mode().IsRegular() {
				return result, &PolicyError{Err: fmt.Errorf("upgrade: %w: %s", ErrUnsafePath, action.Path)}
			}
			current, readErr := readFileFromRoot(root, fsPath)
			if readErr != nil {
				return result, fmt.Errorf("upgrade: verify preimage %s: %w", action.Path, readErr)
			}
			if got := digest(current); got != action.Previous {
				return result, &PolicyError{Err: fmt.Errorf("upgrade: %w: %s changed since the plan was made (was %s, now %s)", ErrApproval, action.Path, action.Previous, got)}
			}
		}

		// Verify recomposed content matches the approved digest.
		contents, ok := contentByPath[action.Path]
		if !ok {
			return result, fmt.Errorf("upgrade: %s was planned but not composed", action.Path)
		}
		if got := digest(contents); got != action.Digest {
			return result, fmt.Errorf("upgrade: recomposed %s is %s, the manifest approved %s", action.Path, got, action.Digest)
		}
	}

	for _, action := range result.Report.Actions {
		if err := ctx.Err(); err != nil {
			result.Partial = true
			return result, fmt.Errorf("upgrade: %w", err)
		}
		if err := writeAtomic(root, action.Path, contentByPath[action.Path]); err != nil {
			result.Partial = true
			return result, fmt.Errorf("upgrade: write %s: %w", action.Path, err)
		}
	}

	result.Applied = true
	return result, nil
}

// JSON renders the manifest canonically.
func (r Report) JSON() ([]byte, error) {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(r); err != nil {
		return nil, fmt.Errorf("upgrade report: %w", err)
	}
	return out.Bytes(), nil
}

// computeHash renders the approval hash.
func (r Report) computeHash() (string, error) {
	r.Hash = ""
	encoded, err := r.JSON()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

// Summary renders the manifest for a person.
func (r *Result) Summary() string {
	var b strings.Builder
	report := r.Report
	verb := "would upgrade"
	if r.Applied {
		verb = "upgraded"
	}
	fmt.Fprintf(&b, "soapbox upgrade %s engine %s -> %s\n", verb, report.Engine.From, report.Engine.To)
	fmt.Fprintf(&b, "  writes        %d updated, %d created\n", report.Totals.Update, report.Totals.Create)
	fmt.Fprintf(&b, "  unchanged     %d setup-owned files\n", report.Totals.Unchanged)
	for _, action := range report.Actions {
		fmt.Fprintf(&b, "  %-13s %s\n", action.Kind, action.Path)
	}
	for _, notice := range report.Notices {
		fmt.Fprintf(&b, "  notice        %s\n", notice)
	}
	fmt.Fprintf(&b, "  manifest      %s\n", report.Hash)
	switch {
	case r.Partial:
		fmt.Fprintln(&b, "\napply failed after the repository may have changed; inspect or reset it before retrying.")
	case !r.Applied:
		fmt.Fprintf(&b, "\nnothing was written. to apply exactly this manifest:\n  rerun the same upgrade command with -apply -approve %s\n", report.Hash)
	}
	return b.String()
}

// digest renders the content hash the manifest records.
func digest(contents []byte) string {
	sum := sha256.Sum256(contents)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// policyFinding is returned by readDerivedEnginePin for validation failures
// that are policy findings (wrong module, missing pin) as opposed to I/O
// errors. Plan wraps these as ErrNotDerived; I/O errors stay runtime errors.
type policyFinding struct {
	reason string
}

func (e *policyFinding) Error() string { return e.reason }

// readDerivedEnginePin reads and validates the currently pinned engine version
// from a derived repository. It Lstats both go.mod and tools/go.mod before
// reading, requires regular files, and parses them strictly.
//
// I/O failures (permissions, filesystem errors) are returned unwrapped so the
// caller can distinguish them from policy findings (wrong module, missing pin),
// which are returned as *policyFinding.
func readDerivedEnginePin(root *os.Root, destModule string) (string, error) {
	// Lstat and require regular file for root go.mod.
	if err := requireRegularFile(root, "go.mod"); err != nil {
		return "", err
	}
	rootModData, err := readFileFromRoot(root, filepath.FromSlash("go.mod"))
	if err != nil {
		return "", fmt.Errorf("read root go.mod: %w", err)
	}
	rootMod, err := modfile.Parse("go.mod", rootModData, nil)
	if err != nil {
		return "", &policyFinding{reason: fmt.Sprintf("parse root go.mod: %v", err)}
	}
	if rootMod.Module == nil || rootMod.Module.Mod.Path != destModule {
		got := ""
		if rootMod.Module != nil {
			got = rootMod.Module.Mod.Path
		}
		return "", &policyFinding{reason: fmt.Sprintf("root go.mod declares module %q, want %q", got, destModule)}
	}

	// Lstat and require regular file for tools/go.mod.
	if err := requireRegularFile(root, "tools/go.mod"); err != nil {
		return "", err
	}
	toolsData, err := readFileFromRoot(root, filepath.FromSlash("tools/go.mod"))
	if err != nil {
		return "", fmt.Errorf("read tools/go.mod: %w", err)
	}
	toolsMod, err := modfile.Parse("tools/go.mod", toolsData, nil)
	if err != nil {
		return "", &policyFinding{reason: fmt.Sprintf("parse tools/go.mod: %v", err)}
	}

	wantToolsPath := destModule + "/tools"
	if toolsMod.Module == nil || toolsMod.Module.Mod.Path != wantToolsPath {
		got := ""
		if toolsMod.Module != nil {
			got = toolsMod.Module.Mod.Path
		}
		return "", &policyFinding{reason: fmt.Sprintf("tools/go.mod declares module %q, want %q", got, wantToolsPath)}
	}

	var pin string
	for _, req := range toolsMod.Require {
		if req.Mod.Path == setup.EngineModulePath {
			if pin != "" {
				return "", &policyFinding{reason: "tools/go.mod pins the engine more than once"}
			}
			pin = req.Mod.Version
		}
	}
	if pin == "" {
		return "", &policyFinding{reason: fmt.Sprintf("tools/go.mod does not require %s", setup.EngineModulePath)}
	}
	if !semver.IsValid(pin) {
		return "", &policyFinding{reason: fmt.Sprintf("tools/go.mod pins engine version %q which is not valid semver", pin)}
	}
	return pin, nil
}

// requireRegularFile Lstats a path through the root and refuses anything
// that is not a regular file. os.ErrNotExist and permission errors are
// returned unwrapped (I/O); symlinks/dirs/devices are ErrUnsafePath policy.
func requireRegularFile(root *os.Root, name string) error {
	info, err := root.Lstat(filepath.FromSlash(name))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &policyFinding{reason: fmt.Sprintf("%s does not exist", name)}
		}
		return fmt.Errorf("stat %s: %w", name, err)
	}
	if !info.Mode().IsRegular() {
		return &PolicyError{Err: fmt.Errorf("upgrade: %w: %s", ErrUnsafePath, name)}
	}
	return nil
}

// Permissions the transformation creates.
const (
	payloadFileMode = 0o644
	payloadDirMode  = 0o750
)

// readFileFromRoot reads a file through an os.Root handle.
func readFileFromRoot(root *os.Root, name string) ([]byte, error) {
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(f)
}

// writeAtomic writes one file through a rename inside its own directory.
//
// The temporary file is created with O_CREATE|O_EXCL so a preexisting file at
// that name (tracked, ignored, or a symlink planted after planning) is refused
// rather than truncated. On any failure the temporary file is removed.
func writeAtomic(root *os.Root, name string, contents []byte) error {
	if dir := filepath.Dir(filepath.FromSlash(name)); dir != "." {
		if err := root.MkdirAll(dir, payloadDirMode); err != nil {
			return err
		}
	}
	temp := filepath.FromSlash(name + ".soapbox-upgrade.tmp")

	// O_WRONLY|O_CREATE|O_EXCL refuses any preexisting temp file.
	f, err := root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, payloadFileMode)
	if err != nil {
		return fmt.Errorf("create temp %s: %w", temp, err)
	}
	_, writeErr := f.Write(contents)
	closeErr := f.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return errors.Join(err, root.Remove(temp))
	}

	if err := root.Chmod(temp, payloadFileMode); err != nil {
		return errors.Join(err, root.Remove(temp))
	}
	if err := root.Rename(temp, filepath.FromSlash(name)); err != nil {
		return errors.Join(err, root.Remove(temp))
	}
	return nil
}
