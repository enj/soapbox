// Package extract composes the engine's source, closure, patch, relocation, and
// rewriting phases into one read-only extraction plan for a single upstream ref.
//
// A plan answers a question rather than producing a release: given this profile
// and this source commit, exactly which files would the generated module hold,
// what would be pruned, which patches would apply, what would be rewritten, and
// what does the result hash to. It stops before module generation, facade
// generation, history replay, and publication, which are later phases with their
// own gates.
//
// Three properties bound what a plan may do.
//
// It is read-only outward. The command never pushes, never creates or moves a
// ref, and never creates a tag; the only writes it performs are inside the cache
// and work directories it was given and, when explicitly asked, the output tree.
// The source cache is driven by an anonymous runner, so a credential that exists
// for publishing cannot travel to the source host.
//
// It is contained. Cache, work, output, and report roots are absolute and
// checked, the materialized work tree is one this run created below the work
// root rather than any directory an operator already had, and every read of that
// tree goes through an [os.Root] that re-checks containment per operation.
//
// It is deterministic. Two runs over one source commit with different directory
// layouts produce byte identical reports, manifests, and trees. The report
// therefore carries no absolute path, no environment value, and no secret: what
// it records is the profile, the source commit, and the content the two produce.
package extract

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/enj/soapbox/tools/internal/config"
	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/gitgraph"
	"github.com/enj/soapbox/tools/internal/relocate"
	"github.com/enj/soapbox/tools/internal/rewrite"
)

// defaultWidenCeiling bounds the sparse widening loop for a profile that sets no
// package limit of its own.
//
// Each round materializes one package the closure discovered and could not read,
// so the loop terminates naturally once the closure is complete. The bound
// exists for the case where it does not: a profile whose closure is far larger
// than its author believed would otherwise spend an unbounded number of full
// re-checkouts of a repository the size of Kubernetes before anyone saw a
// message. A profile that does set a package limit is bounded by that instead,
// so this number never decides the answer for a profile that stated one.
const defaultWidenCeiling = 4096

// worktreeDirName is the directory below the work root that holds materialized
// source trees. Naming it keeps the work root readable when an operator passes
// -keep-worktree and goes looking.
const worktreeDirName = "src"

// RefKind names the ref namespace a plan selects from.
type RefKind string

// The selectable ref kinds. A plan covers exactly one ref, because a plan is a
// statement about one source commit.
const (
	// RefTag selects an upstream release tag, which is the normal case.
	RefTag RefKind = "tag"
	// RefBranch selects a tracked upstream branch, which is how an operator
	// inspects what the next release would contain.
	RefBranch RefKind = "branch"
	// RefCommit selects an exact object already present in the source cache. It
	// is engine-only: CLI users select reviewed tags or tracked branches, while
	// reconciliation uses this kind for commits inside a release-bounded DAG.
	RefCommit RefKind = "commit"
)

// Ref is the single upstream ref a plan covers.
type Ref struct {
	// Kind names the ref namespace.
	Kind RefKind
	// Name is the short ref name, such as v1.36.1 or master.
	Name string
}

// String renders the ref for a message.
func (r Ref) String() string { return string(r.Kind) + " " + r.Name }

// Options configures one plan.
//
// Every directory is absolute because a plan must name the same directories no
// matter where the process was started from, and because the run adopts none of
// them: the cache and work roots are created if absent and owned by the run
// thereafter, and the output tree must not exist at all.
type Options struct {
	// Config is the decoded, validated profile.
	Config *config.Config
	// ProfileDir is the repository directory holding the profile, the patch
	// files its patch entries name, and the closure golden it pins.
	ProfileDir string
	// CacheRoot holds the reusable bare source cache.
	CacheRoot string
	// WorkRoot holds the scratch work trees this run creates and removes.
	WorkRoot string
	// OutputRoot is where -materialize writes the relocated tree. It must not
	// exist; relocation never merges into or overwrites a tree.
	OutputRoot string
	// Ref selects the upstream ref to plan.
	Ref Ref
	// PatchBranch is the tracked branch a patch's branch selector is matched
	// against. It is required only when the profile carries patches.
	PatchBranch string
	// SourceRemote overrides the profile's source repository, which is how a
	// test or an air-gapped operator points the run at a local mirror.
	SourceRemote string
	// Fetch updates the cache before the ref is resolved.
	Fetch bool
	// Offline refuses every network operation, including the clone an absent
	// cache would otherwise trigger and the lazy blob fetch a checkout would
	// perform without anything having asked it to.
	//
	// The refusal is carried by the materialization rather than by the runner
	// the caller passes, so a plan cannot be offline in name only: a caller that
	// builds Git the ordinary way still gets a run that fails closed on a blob
	// the cache does not hold.
	Offline bool
	// Materialize writes the relocated tree to OutputRoot. Without it the plan
	// computes the tree and hashes it without touching a disk.
	Materialize bool
	// KeepWorktree leaves the materialized source tree in place for inspection.
	KeepWorktree bool
	// Strict turns every advisory notice into a policy failure.
	Strict bool
	// Git is the runner the plan drives. It must be anonymous: a plan talks to
	// the public source host and to nothing else.
	Git *gitcli.Runner
	// LookupEnv reads the process environment. A nil value uses os.LookupEnv.
	// It is injectable so the credential check is testable without mutating the
	// environment of a running test binary.
	LookupEnv func(string) (string, bool)
}

// Result is one completed plan.
//
// A plan that refused still produces one whenever it measured enough to be worth
// reading. Report.Failure is what tells the two apart, and it is the reason a
// refusal is reviewable from an artifact rather than from a stderr line.
type Result struct {
	// Report is the deterministic record of what the plan found.
	Report Report
	// Files is the final relocated file set, including the rewritten bytes and
	// the per-package provenance records. It is what -materialize writes, and it
	// is empty for a plan that refused before relocating.
	Files relocate.FileSet
	// Provenance is the structured form of the per-package records, one entry
	// per relocated package, ordered by destination directory.
	//
	// It carries exactly what the committed SOAPBOX_PROVENANCE.txt beside each
	// package states, in the shape the root NOTICE generator consumes. Rendering
	// those records and then parsing the text back is the one way this evidence
	// could disagree with itself, so the structure the text was rendered from is
	// what leaves the plan.
	//
	// A plan that refused after relocating still carries the records it built,
	// because a refusal is when the evidence is most worth reading. It is empty
	// for a plan that refused before it got that far.
	Provenance []*rewrite.PackageProvenance
	// Paths are the absolute directories the run used. They are deliberately
	// outside Report, which carries no absolute path.
	Paths Paths
}

// Paths are the absolute directories one plan used.
type Paths struct {
	// Cache is the bare source cache directory.
	Cache string
	// Work is the scratch root the work tree was created below.
	Work string
	// Worktree is the materialized source tree, empty once it was removed.
	Worktree string
	// Output is the relocated tree destination, written only with -materialize.
	Output string
}

// ErrCredentialEnvironment reports a publishing credential visible to a command
// that must never use one.
var ErrCredentialEnvironment = errors.New("a plan must run without publishing credentials")

// ErrPathConflict reports an output tree that would sit where the run's own
// state lives.
//
// It is exported so the command line can report it as the usage failure it is.
// Every directory involved is one the operator named or defaulted, the check
// reads nothing and creates nothing, and the answer does not depend on a cache,
// a profile, or a network existing, so it belongs with the other flag problems
// rather than with the failures a run discovers.
var ErrPathConflict = errors.New("the output tree conflicts with a directory the run owns")

// validate checks everything a plan depends on before it reads anything.
//
// The order matters. A malformed option set has to fail before the first
// filesystem or network operation, so that an operator who typed the wrong flag
// gets the same answer whether or not a cache, a profile, or a network happens
// to be there.
func (o *Options) validate() error {
	switch {
	case o.Config == nil:
		return errors.New("plan: a validated profile is required")
	case o.Git == nil:
		return errors.New("plan: a git runner is required")
	case !o.Git.IsAnonymous():
		return fmt.Errorf("plan: %w: the git runner carries caller supplied environment entries", ErrCredentialEnvironment)
	}
	for _, dir := range []struct {
		name  string
		value string
	}{
		{"profile directory", o.ProfileDir},
		{"cache root", o.CacheRoot},
		{"work root", o.WorkRoot},
		{"output tree", o.OutputRoot},
	} {
		if err := validateAbsDir(dir.name, dir.value); err != nil {
			return fmt.Errorf("plan: %w", err)
		}
	}
	if err := o.CheckPaths(); err != nil {
		return fmt.Errorf("plan: %w", err)
	}
	switch o.Ref.Kind {
	case RefTag, RefBranch:
	case RefCommit:
		if err := gitgraph.ValidateSHA(o.Ref.Name); err != nil {
			return fmt.Errorf("plan: exact source commit: %w", err)
		}
		if o.Fetch {
			return errors.New("plan: an exact source commit must already be present in the cache, so it cannot be fetched by object name")
		}
	default:
		return fmt.Errorf("plan: ref kind %q must be %s, %s, or %s", o.Ref.Kind, RefTag, RefBranch, RefCommit)
	}
	if o.Ref.Name == "" {
		return fmt.Errorf("plan: a source %s name is required", o.Ref.Kind)
	}
	if o.Fetch && o.Offline {
		return errors.New("plan: -fetch and -offline cannot both be requested")
	}
	if len(o.Config.Patches) > 0 && o.PatchBranch == "" {
		return errors.New("plan: the profile carries patches, so a patch branch is required")
	}
	return o.checkCredentialEnvironment()
}

// CheckPaths refuses an output tree that would sit where the run's own state
// lives.
//
// It is exported and separate from the rest of validation because it reads
// nothing and creates nothing: every directory involved is one the caller named
// or defaulted, so a command line can decide it before it opens a profile, and
// an operator who typed the wrong flag gets the same answer whether or not a
// profile, a cache, or a network happens to be there. Plan checks it again, so
// a caller that skipped it cannot proceed on a layout that cannot work.
//
// Two different relationships are refused, for two different reasons. An output
// tree inside the materialized source root would be relocated content sitting in
// the middle of the source the next pass measures, and the closure is handed
// that tree with permission to remove files from it. An output tree that is, or
// contains, the cache, the work root, or the profile directory is worse: the
// destination must not exist when the run starts and is written atomically from
// scratch, so a directory the run reads from could not survive being the thing
// the run creates.
//
// Everything else may nest. The documented defaults put the work root below the
// cache and the output tree below the work root, so that one directory holds all
// of a run's scratch and an operator can remove all of it in one step.
func (o Options) CheckPaths() error {
	for _, resolve := range []func(string) string{literalPath, resolveExisting} {
		output := resolve(o.OutputRoot)
		worktreeRoot := resolve(filepath.Join(o.WorkRoot, worktreeDirName))
		if err := checkDisjoint("output tree", output, "materialized source root", worktreeRoot); err != nil {
			return err
		}
		for _, other := range []struct {
			name string
			dir  string
		}{
			{"source cache root", resolve(o.CacheRoot)},
			{"work root", resolve(o.WorkRoot)},
			{"materialized source root", worktreeRoot},
			{"profile directory", resolve(o.ProfileDir)},
		} {
			if err := checkNotAncestor("output tree", output, other.name, other.dir); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkCredentialEnvironment refuses to run while a publishing credential is
// visible.
//
// An anonymous runner already keeps the values the caller passed away from every
// subprocess, and that is the guarantee that matters for the source host. This
// check is about the operator rather than the subprocess: a plan is the command
// people run to see what would happen, and running it on a machine that is
// holding a publishing token is a sign that the read-only command is being
// used where the publishing one was meant. Refusing costs nothing, because the
// plan has no use for a credential at all.
func (o *Options) checkCredentialEnvironment() error {
	lookup := o.LookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}
	var present []string
	for _, name := range []string{
		"SOAPBOX_GITHUB_TOKEN",
	} {
		if name == "" {
			continue
		}
		if value, ok := lookup(name); ok && value != "" {
			present = append(present, name)
		}
	}
	if len(present) == 0 {
		return nil
	}
	// Only the variable names are reported. They are configuration, while their
	// values are the credential this message exists to keep out of a log.
	return fmt.Errorf("plan: %w: %s is set", ErrCredentialEnvironment, strings.Join(present, ", "))
}

// validateAbsDir checks one caller supplied directory path.
func validateAbsDir(name, dir string) error {
	switch {
	case dir == "":
		return fmt.Errorf("a %s is required", name)
	case !filepath.IsAbs(dir):
		return fmt.Errorf("%s %q must be absolute", name, dir)
	case filepath.Clean(dir) != dir:
		return fmt.Errorf("%s %q must be in clean form", name, dir)
	}
	return nil
}

// checkDisjoint refuses one directory that lies inside another.
func checkDisjoint(innerName, inner, outerName, outer string) error {
	if inner == outer || isInside(inner, outer) {
		return fmt.Errorf("%w: %s %q must not lie inside the %s %q",
			ErrPathConflict, innerName, inner, outerName, outer)
	}
	return nil
}

// checkNotAncestor refuses one directory that is, or contains, another.
func checkNotAncestor(name, dir, otherName, other string) error {
	if dir == other {
		return fmt.Errorf("%w: %s %q must not also be the %s", ErrPathConflict, name, dir, otherName)
	}
	if isInside(other, dir) {
		return fmt.Errorf("%w: %s %q must not contain the %s %q",
			ErrPathConflict, name, dir, otherName, other)
	}
	return nil
}

// isInside reports whether inner lies below outer.
//
// The comparison is on a path separator boundary, so a work root at /tmp/work is
// not read as containing a sibling at /tmp/work-2.
func isInside(inner, outer string) bool {
	return strings.HasPrefix(inner, outer+string(filepath.Separator))
}

// literalPath is the identity resolution, which is what the containment checks
// use before any symbolic link is followed.
func literalPath(dir string) string { return dir }

// resolveExisting renders a directory with the symbolic links in its existing
// prefix resolved.
//
// Containment checked on lexical paths alone is checked on names rather than on
// directories: an output tree reached through a link into the cache is a
// different string and the same directory. Only the existing prefix can be
// resolved, because the output tree must not exist yet, so the answer is the
// resolved ancestor with the missing remainder appended. A path nothing of which
// exists is returned unchanged, which leaves the lexical check as the answer.
func resolveExisting(dir string) string {
	remainder := ""
	for current := dir; ; {
		if resolved, err := filepath.EvalSymlinks(current); err == nil {
			return filepath.Join(resolved, remainder)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return dir
		}
		remainder = filepath.Join(filepath.Base(current), remainder)
		current = parent
	}
}

// Plan computes one extraction plan.
//
// Nothing outside the cache and work roots is written unless Materialize is set,
// and no ref is ever created, moved, or deleted. The scratch anchor commit the
// patch phase needs is recorded in the work tree's detached HEAD, which is why
// the plan can commit at all without the cache observing it; the run proves that
// by comparing the cache's refs before and after.
//
// A refusal returns both a result and an error whenever the run measured enough
// for the report to be worth reading, which is every policy failure and every
// failure that left the cache or the output in a state an operator has to see.
// A failure that stopped the run before it measured anything returns the error
// alone, because a report of nothing is not evidence.
func Plan(ctx context.Context, opts Options) (*Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("plan: %w", err)
	}
	if err := opts.validate(); err != nil {
		return nil, err
	}

	r := &run{
		opts:  opts,
		cfg:   opts.Config,
		paths: Paths{Work: opts.WorkRoot, Output: opts.OutputRoot},
	}
	r.report.Schema = ReportSchema
	if err := r.setEngine(); err != nil {
		return nil, err
	}

	result, err := r.execute(ctx)
	// The cache invariant is checked on the way out of every plan rather than
	// only the successful one. A ref that moved is the failure that corrupts
	// every run after this one, and a plan that failed for an unrelated reason
	// is exactly when nobody would think to look.
	failure := joinFailure(err, r.assertCacheUnchanged(ctx))
	if failure != nil && (result != nil || isPolicy(failure) || r.report.Worktree.CacheRefsMoved) {
		result = r.failedResult(failure)
	}

	// The work tree is removed even when the plan failed, because it is scratch
	// this run created. The removal failure is joined rather than substituted:
	// the reason the plan failed is what the operator needs first.
	cleanupErr := r.cleanup(ctx)
	if result != nil {
		// The result was assembled before the work tree was removed, so the path
		// it reports has to be corrected rather than left naming a directory
		// that is no longer there.
		result.Paths.Worktree = r.paths.Worktree
	}
	return result, errors.Join(failure, cleanupErr)
}

// joinFailure combines a phase failure with the cache invariant's answer without
// repeating one that is already the other.
//
// The successful path checks the invariant inside the final phase, so a cache
// that moved arrives here twice: once as the phase's failure and once as the
// memoised answer. Joining them blindly would print the same sentence to the
// operator two times.
func joinFailure(err, cacheErr error) error {
	if cacheErr == nil || errors.Is(err, cacheErr) {
		return err
	}
	return errors.Join(err, cacheErr)
}
