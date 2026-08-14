// Package setup turns a copied soapbox template checkout into one derived
// repository.
//
// A derived repository is not the template with a few files removed. It is a
// different artifact: a root Go module the template deliberately does not have,
// a nested tools module that pins an immutable engine release instead of
// carrying the engine's source, and the two workflows that only a repository
// which publishes needs. Setup is therefore built around an explicit payload
// allowlist. It composes every file it owns, deletes only paths it recognises as
// the template's own, and leaves everything else exactly where it found it, so a
// file the operator added is never collateral damage of a transformation they
// asked for.
//
// The transformation is local and offline. No remote is contacted, no ref is
// created, and no GitHub state is read or written. The only effect that leaves
// this package is a write into the repository directory the operator named.
//
// Planning and applying are separate. Plan measures the repository and returns a
// manifest naming every write and every delete, with a hash over the whole of
// it. Apply measures again, refuses unless the operator approved that exact
// hash, and only then writes. The approval is required even though nothing
// outward happens, because the manifest is the only place the deletions are
// enumerated, and an approval that did not cover them would not be an approval
// of anything.
package setup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/enj/soapbox/tools/internal/buildinfo"
	"github.com/enj/soapbox/tools/internal/config"
	"github.com/enj/soapbox/tools/internal/gitcli"
)

// Sentinel failures a caller distinguishes.
var (
	// ErrNotTemplate reports a repository that is not an untouched soapbox
	// template checkout.
	ErrNotTemplate = errors.New("directory is not a soapbox template checkout")
	// ErrDirty reports a work tree with changes setup would have to reason about.
	ErrDirty = errors.New("work tree has uncommitted changes")
	// ErrUnknownOverwrite reports a payload path already held by a file the
	// template does not own.
	ErrUnknownOverwrite = errors.New("payload path is already held by a file the template does not own")
	// ErrApproval reports an apply whose approval does not name the manifest the
	// repository currently plans to.
	ErrApproval = errors.New("approval does not match the current manifest")
	// ErrSymlink reports a symbolic link in the tracked tree.
	ErrSymlink = errors.New("tracked symbolic links are refused")
	// ErrCaseCollision reports two paths that differ only by letter case.
	ErrCaseCollision = errors.New("paths differ only by letter case")
)

// Options describes one setup of one repository.
type Options struct {
	// Root is the absolute path of the template checkout. The derived repository
	// replaces it in place, so this is both the input and the output tree.
	Root string
	// Config is the validated profile the template carries. Setup reads the
	// destination module, the branch, and the publication mode from it, and
	// never edits it.
	Config *config.Config
	// EngineVersion is the immutable engine release the nested tools module pins.
	// Both spellings of one release are accepted, "v1.2.3" and the repository tag
	// "tools/v1.2.3", because an operator reads the second one off the release
	// page and the first one out of a go.mod.
	EngineVersion string
	// EngineMod is the go.mod of the engine release being pinned. Its requirements
	// become indirect requirements of the shim, as required by Go's pruned module
	// graph. The CLI reads it from the template's nested engine module; setup
	// refuses to guess a dependency graph when it is absent or names another
	// module.
	EngineMod []byte
	// EngineSum is the complete verified go.sum content for the nested tools
	// module. It covers the pinned engine and every module named by EngineMod. It
	// is optional because release checksums cannot be computed from a checkout;
	// see [composeEngineSum] for what is written when it is absent.
	EngineSum []byte
	// Git drives the repository. It needs no credential: setup reads the tracked
	// tree and the work tree status and nothing else.
	Git *gitcli.Runner
}

// check decides everything about the options that does not need the repository.
func (o Options) check() error {
	switch {
	case o.Root == "":
		return errors.New("a repository root is required")
	case !filepath.IsAbs(o.Root):
		return fmt.Errorf("repository root %q must be absolute, because a relative root means one directory to the operator and another to the engine", o.Root)
	case o.Root != filepath.Clean(o.Root):
		return fmt.Errorf("repository root %q must be a cleaned path", o.Root)
	case o.Config == nil:
		return errors.New("a validated profile is required")
	case len(o.EngineMod) == 0:
		return errors.New("the pinned engine go.mod is required so the shim module graph is complete")
	case o.Git == nil:
		return errors.New("a git runner is required")
	case o.Config.Determinism.Toolchain != buildinfo.Toolchain:
		return fmt.Errorf("profile toolchain %q must match engine toolchain %q", o.Config.Determinism.Toolchain, buildinfo.Toolchain)
	case o.Config.Destination.InternalPrefix == "tools", strings.HasPrefix(o.Config.Destination.InternalPrefix, "tools/"):
		return fmt.Errorf("destination internal prefix %q is reserved for the derived repository's engine shim", o.Config.Destination.InternalPrefix)
	}
	return nil
}

// Result is one planned or applied setup.
type Result struct {
	// Report is the manifest, whether or not it was applied.
	Report Report
	// Applied reports whether the complete manifest reached the repository.
	Applied bool
	// Partial reports that apply failed after it may have written or removed at
	// least one approved path. The repository must be inspected or reset before
	// setup is retried.
	Partial bool
}

// PolicyError reports that setup ran and the answer is no. It separates a
// repository the operator can fix from a runtime failure they cannot.
type PolicyError struct{ Err error }

func (e *PolicyError) Error() string { return e.Err.Error() }
func (e *PolicyError) Unwrap() error { return e.Err }

func policyErrorf(format string, args ...any) error {
	return &PolicyError{Err: fmt.Errorf(format, args...)}
}

// Plan measures the repository and returns the manifest without writing.
func Plan(ctx context.Context, opts Options) (*Result, error) {
	r, err := newRun(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &Result{Report: r.report}, nil
}

// Apply writes the manifest the operator approved.
//
// The plan is recomputed here rather than carried over from an earlier call, so
// the hash is compared against what the repository would produce now. A tree
// that changed between the two commands therefore fails the comparison instead
// of being written over with a stale decision, which is the whole point of
// approving a hash rather than approving a command.
//
// The result is returned alongside a refusal, because the fresh manifest is what
// tells the operator what changed under them.
func Apply(ctx context.Context, opts Options, approve string) (*Result, error) {
	approve = strings.TrimSpace(approve)
	if approve == "" {
		return nil, &PolicyError{Err: fmt.Errorf("%w: an approval is required", ErrApproval)}
	}
	r, err := newRun(ctx, opts)
	if err != nil {
		return nil, err
	}
	result := &Result{Report: r.report}
	if !strings.EqualFold(approve, r.report.Hash) {
		return result, &PolicyError{Err: fmt.Errorf("%w: approved %s, manifest is %s", ErrApproval, approve, r.report.Hash)}
	}
	if err := r.apply(ctx); err != nil {
		result.Partial = true
		return result, err
	}
	result.Applied = true
	return result, nil
}

// run holds one measured repository and the manifest computed from it.
type run struct {
	opts Options
	// tracked is every path HEAD records.
	tracked map[string]struct{}
	// payload is the composed content of every file setup owns.
	payload []composedFile
	// composedSum reports whether the payload includes tools/go.sum.
	composedSum bool
	// notices are the things an operator has to do that setup could not.
	notices []string
	report  Report
}

// newRun measures the repository and computes the manifest.
//
// Everything that can refuse does so before anything is composed, so a
// repository setup will not accept fails while it is still exactly as the
// operator left it.
func newRun(ctx context.Context, opts Options) (*run, error) {
	if err := opts.check(); err != nil {
		return nil, fmt.Errorf("setup: %w", err)
	}
	pin, err := parseEnginePin(opts.EngineVersion)
	if err != nil {
		return nil, &PolicyError{Err: fmt.Errorf("setup: %w", err)}
	}
	r := &run{opts: opts}
	if err := r.measure(ctx); err != nil {
		return nil, err
	}
	if err := r.compose(pin); err != nil {
		return nil, err
	}
	if err := r.classify(pin); err != nil {
		return nil, err
	}
	return r, nil
}

// measure reads the repository state every later decision rests on.
func (r *run) measure(ctx context.Context) error {
	root, err := r.opts.Git.RepositoryRoot(ctx)
	if err != nil {
		return fmt.Errorf("setup: %w", err)
	}
	// The comparison is on the resolved path because a root reached through a
	// symbolic link is the same repository under a different name, and refusing it
	// would refuse an ordinary macOS temporary directory.
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("setup: resolve repository root: %w", err)
	}
	resolvedOpt, err := filepath.EvalSymlinks(r.opts.Root)
	if err != nil {
		return fmt.Errorf("setup: resolve %s: %w", r.opts.Root, err)
	}
	if resolvedRoot != resolvedOpt {
		return policyErrorf("setup: %s is inside the repository rooted at %s, and setup transforms a whole repository rather than a subdirectory of one", r.opts.Root, root)
	}

	hasHead, err := r.opts.Git.HasHead(ctx)
	if err != nil {
		return fmt.Errorf("setup: %w", err)
	}
	if !hasHead {
		return policyErrorf("setup: %w: the repository has no commit, so nothing setup deleted could be recovered", ErrNotTemplate)
	}
	status, err := r.opts.Git.Status(ctx)
	if err != nil {
		return fmt.Errorf("setup: %w", err)
	}
	if len(status) > 0 {
		return policyErrorf("setup: %w: %s", ErrDirty, describeStatus(status))
	}

	entries, err := r.opts.Git.ListTree(ctx, "HEAD")
	if err != nil {
		return fmt.Errorf("setup: %w", err)
	}
	r.tracked = make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if entry.Mode == gitcli.ModeSymlink {
			return policyErrorf("setup: %w: %s is a symbolic link, and a payload decision made against a link is a decision about wherever it points", ErrSymlink, entry.Path)
		}
		r.tracked[entry.Path] = struct{}{}
	}
	return checkTemplateMarker(r.tracked)
}

// describeStatus renders the work tree changes that make a repository unusable
// here, capped so a wholly unrelated directory does not print its whole listing.
func describeStatus(entries []gitcli.StatusEntry) string {
	const shown = 5
	names := make([]string, 0, shown)
	for _, entry := range entries[:min(len(entries), shown)] {
		names = append(names, fmt.Sprintf("%s %s", entry.Code, entry.Path))
	}
	rendered := strings.Join(names, ", ")
	if len(entries) > shown {
		rendered = fmt.Sprintf("%s, and %d more", rendered, len(entries)-shown)
	}
	return fmt.Sprintf("%d paths (%s)", len(entries), rendered)
}

// digest renders the content hash the manifest records.
//
// The manifest names content rather than describing it, so an approval covers
// the bytes that will be written and the bytes that will be removed rather than
// the fact that a path was involved.
func digest(contents []byte) string {
	sum := sha256.Sum256(contents)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// engineReport renders the pinned engine for the manifest.
func engineReport(pin enginePin, sum bool) Engine {
	return Engine{
		Version:   buildinfo.Version,
		Module:    EngineModulePath,
		Require:   pin.version,
		Tag:       pin.tag,
		Toolchain: buildinfo.Toolchain,
		Go:        buildinfo.GoDirective,
		Sum:       sum,
	}
}

// moduleReport renders the derived repository's identity for the manifest.
func moduleReport(cfg *config.Config) Module {
	return Module{
		Path:       cfg.Destination.Module,
		Tools:      toolsModulePath(cfg.Destination.Module),
		Repository: cfg.Destination.Repository,
		Branch:     cfg.Destination.Branch,
	}
}
