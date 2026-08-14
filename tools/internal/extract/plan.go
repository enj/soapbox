package extract

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/enj/soapbox/tools/internal/closure"
	"github.com/enj/soapbox/tools/internal/config"
	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/patchset"
	"github.com/enj/soapbox/tools/internal/relocate"
	"github.com/enj/soapbox/tools/internal/rewrite"
	"github.com/enj/soapbox/tools/internal/source"
)

// The scratch anchor's fixed identity, date, and message.
//
// Every field is a constant because the commit's object name must depend on the
// pruned tree and its parent alone. A wall clock date or the operator's Git
// identity would make two plans over one source commit disagree, which is the
// one thing the double run determinism check exists to catch. The date is the
// Unix epoch so the commit is obviously synthetic to anyone who finds it.
const (
	scratchAnchorDate    = "1970-01-01T00:00:00+00:00"
	scratchAnchorMessage = "soapbox: scratch anchor for patch application\n"
)

// missingRemoteRef is what git prints when a refspec names a ref the remote does
// not have.
//
// Matching git's wording is not something to do lightly, and it is done here for
// the same reason internal/gitcli matches on a missing repository: git reports
// the condition only in prose, and the runner pins LC_ALL and LANG so the prose
// is stable. Getting it wrong is not dangerous either, because the failure stops
// the run either way; only the exit code an operator sees would change.
const missingRemoteRef = "couldn't find remote ref"

// cleanupTimeout bounds the work a run performs after its own context ended.
//
// Removing a work tree and re-reading the cache's refs both have to happen even
// when the caller cancelled, because the first is scratch this run created and
// the second is the invariant that makes the cache safe to reuse. Neither may
// run unbounded: a detached context with no deadline turns a cancelled command
// into one that cannot be stopped at all.
const cleanupTimeout = 30 * time.Second

// generatedOutputs maps a Kubernetes generator marker to the file its generator
// writes into the marked package.
//
// It is the evidence half of the dangling marker rule. A marker whose generated
// output the run removed describes a generator run that can never be reproduced
// in the generated module, and naming the exact file is what makes the removal
// reviewable rather than a guess. The table is evidence rather than a
// catalogue: a marker outside it is checked against its value alone, and the
// plan says so in a notice instead of letting the gap read as a clean answer.
// openapi-gen is the instructive absence, since its output is written into a
// central package rather than beside the types it describes.
var generatedOutputs = map[string]string{
	"k8s:conversion-gen":       "zz_generated.conversion.go",
	"k8s:defaulter-gen":        "zz_generated.defaults.go",
	"k8s:defaulter-gen-input":  "zz_generated.defaults.go",
	"k8s:validation-gen":       "zz_generated.validations.go",
	"k8s:validation-gen-input": "zz_generated.validations.go",
	"k8s:deepcopy-gen":         "zz_generated.deepcopy.go",
	"k8s:protobuf-gen":         "generated.pb.go",
}

// run is the mutable state of one plan.
type run struct {
	opts   Options
	cfg    *config.Config
	report Report
	paths  Paths

	cache    *source.Cache
	worktree *source.Worktree
	builder  *closure.Builder

	// revision is the resolved upstream ref.
	revision source.Revision
	// patterns is the current sparse pattern set.
	patterns []string
	// widened are the package directories the closure asked for beyond the
	// configured roots.
	widened []string
	// cacheRefs is the cache's ref state captured before the work tree existed.
	cacheRefs []gitcli.Ref
	// cacheChecked and cacheErr memoise the ref comparison, which both the
	// success path and the failure paths perform.
	cacheChecked bool
	cacheErr     error

	// closureResult is the fixed point closure.
	closureResult *closure.Result
	// prePatchFiles are the copy plan paths the closure held before any patch
	// applied, sorted.
	prePatchFiles []string
	// removed indexes every upstream file this run took out of the tree,
	// whether the profile pruned it or a patch deleted it.
	removed map[string]bool
	// closureDirs indexes the surviving package directories.
	closureDirs map[string]bool
	// kinds records why the closure selected each upstream file, which is what
	// separates the module's Go source from the data it carries.
	kinds map[string]closure.CopyKind
	// applied are the patch identifiers that applied, in application order.
	applied []string

	// results records what each transformation did, keyed by destination path,
	// and carries the final bytes of each file.
	results map[string]rewrite.Result
	// provenance indexes the destination paths of the records this engine
	// generated.
	provenance map[string]bool
	// records are the structured per-package provenance records, ordered by
	// destination directory. They are the values the committed records were
	// rendered from, kept so a caller can be handed the same evidence without
	// anyone parsing the rendered text back into a structure.
	records []*rewrite.PackageProvenance
	// tree is the final relocated file set, including provenance records.
	tree relocate.FileSet
	// notices are advisory findings, unsorted until the report is assembled.
	notices []string
}

// execute runs the plan's phases in order.
//
// The order is the pipeline's contract. Source before materialization, closure
// before pruning is committed, the scratch anchor before patches, patches before
// the fixed point, and the fixed point before anything is relocated: each phase
// depends on the previous one having settled, and a phase that ran early would
// measure a tree that no profile describes.
func (r *run) execute(ctx context.Context) (*Result, error) {
	// The destination and the toolchain are checked first so a run that could
	// never deliver its tree, or could never make the formatting claim it
	// promises, fails in a second rather than after a full clone and closure.
	if err := r.checkOutput(); err != nil {
		return nil, err
	}
	if err := r.checkToolchain(); err != nil {
		return nil, err
	}
	stages := []struct {
		name string
		run  func(context.Context) error
	}{
		{"source", r.acquireSource},
		{"materialize", r.materializeSource},
		{"closure", r.buildClosure},
		{"anchor", r.writeScratchAnchor},
		{"patch", r.applyPatches},
		{"fixed point", r.settleClosure},
		{"relocate", r.relocate},
	}
	for _, stage := range stages {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("plan %s: %w", stage.name, err)
		}
		if err := stage.run(ctx); err != nil {
			return nil, err
		}
	}
	return r.finish(ctx)
}

// checkOutput refuses a materialization target that already exists.
//
// Relocation never merges into or overwrites a tree, because a merge would keep
// files from an earlier run that the current profile no longer selects.
// Materialize enforces the same rule at the moment it writes; checking here as
// well is what turns a wasted run into an immediate answer. Nothing is created:
// the check is a stat, so a plan that then refuses leaves the destination as it
// found it and is immediately retryable.
func (r *run) checkOutput() error {
	if !r.opts.Materialize {
		return nil
	}
	switch _, err := os.Lstat(r.opts.OutputRoot); {
	case err == nil:
		return fmt.Errorf("plan: output tree %q: %w", r.opts.OutputRoot, relocate.ErrDestinationExists)
	case !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("plan: output tree %q: %w", r.opts.OutputRoot, err)
	}
	return nil
}

// acquireSource opens the cache, updates it when asked, and resolves the one ref
// the plan covers.
func (r *run) acquireSource(ctx context.Context) error {
	remote := r.opts.SourceRemote
	if remote == "" {
		remote = r.cfg.Source.Repository
	}
	cacheDir := filepath.Join(r.opts.CacheRoot, source.CacheDirName(remote))

	// Offline is checked against the directory rather than against a flag that
	// source.Open honours, because Open clones as soon as it finds nothing
	// there. By the time it could report the problem it would already have
	// reached the network.
	if r.opts.Offline {
		if _, err := os.Stat(cacheDir); err != nil {
			return fmt.Errorf("plan source: offline run needs an existing cache: %w", err)
		}
	}

	cache, err := source.Open(ctx, source.Options{
		Remote:       remote,
		CacheRoot:    r.opts.CacheRoot,
		WorktreeRoot: filepath.Join(r.opts.WorkRoot, worktreeDirName),
		Git:          r.opts.Git,
	})
	if err != nil {
		return err
	}
	r.cache = cache
	// The cache reports where it actually opened. Rebuilding the path here
	// would name a directory this run believes in rather than the one it used,
	// and the two are the same only while the derivation is.
	r.paths.Cache = cache.Path()
	r.report.Source.CacheCreated = cache.Created()
	r.report.Source.Offline = r.opts.Offline
	r.report.Source.RemoteOverridden = r.opts.SourceRemote != ""

	// A registration whose directory an interrupted run removed would block the
	// work tree this run needs. Pruning clears exactly those and moves no ref,
	// and it goes through the cache so it is serialized against the work tree
	// another run may be creating or removing at the same moment.
	if err := cache.PruneWorktrees(ctx); err != nil {
		return err
	}

	refs := source.Refs{}
	switch r.opts.Ref.Kind {
	case RefTag:
		refs.Tags = []string{r.opts.Ref.Name}
	case RefBranch:
		refs.Branches = []string{r.opts.Ref.Name}
	case RefCommit:
		// Exact commits are resolved from the already-fetched object database
		// below. Options validation refuses Fetch for this engine-only kind.
	}
	if r.opts.Fetch && !r.opts.Offline {
		if err := cache.Fetch(ctx, refs); err != nil {
			// A refspec naming a ref the remote does not have is a statement
			// about the profile rather than about this machine, and it is the
			// one fetch failure that has to reach the operator as a finding.
			// Git's own wording is the only signal it gives, and the runner
			// pins LC_ALL so the wording is stable.
			if strings.Contains(err.Error(), missingRemoteRef) {
				return contentPolicy("plan source", err)
			}
			return err
		}
		r.report.Source.Fetched = true
	}

	var resolved []source.Revision
	if r.opts.Ref.Kind == RefCommit {
		revision, resolveErr := cache.ResolveCommit(ctx, r.opts.Ref.Name)
		if resolveErr != nil {
			return fmt.Errorf("plan source: %w", resolveErr)
		}
		resolved = []source.Revision{revision}
	} else {
		resolved, err = cache.Resolve(ctx, refs)
		if err != nil {
			// A ref the cache does not hold is a statement about the profile and the
			// upstream repository, not about this machine.
			return contentPolicy("plan source", err)
		}
	}
	if len(resolved) != 1 {
		return fmt.Errorf("plan source: resolved %d selections, want exactly one", len(resolved))
	}
	r.revision = resolved[0]
	r.report.Source.RefKind = string(r.opts.Ref.Kind)
	r.report.Source.RefName = r.revision.Name
	r.report.Source.Ref = r.revision.Ref
	r.report.Source.Object = r.revision.Object
	r.report.Source.Commit = r.revision.Commit
	r.report.Source.Annotated = r.revision.Annotated

	if err := r.verifyAnchor(ctx); err != nil {
		return err
	}
	// The snapshot is taken after every legitimate ref update this run performs,
	// so anything that differs later was moved by a phase that had no business
	// moving it.
	r.cacheRefs, err = cache.Git().ListRefs(ctx)
	return err
}

// verifyAnchor proves the selected commit descends from the recorded anchor.
//
// An empty anchor means the profile has not resolved one yet, which is the state
// before the first setup, so there is nothing to verify and nothing to refuse. A
// configured anchor that the commit does not descend from is refused, because
// published history is rooted at it and a commit outside it cannot be replayed
// onto the same base.
func (r *run) verifyAnchor(ctx context.Context) error {
	anchor := r.cfg.Source.Refs.AnchorCommit
	r.report.Source.AnchorCommit = anchor
	if anchor == "" {
		return nil
	}
	// The anchor is resolved before it is compared, so a profile naming an
	// object this repository does not hold is reported as the profile finding
	// it is rather than as an ancestry check that could not run.
	if _, err := r.cache.ResolveCommit(ctx, anchor); err != nil {
		return contentPolicy("plan anchor", err)
	}
	descends, err := r.cache.Git().IsAncestor(ctx, anchor, r.revision.Commit)
	if err != nil {
		return fmt.Errorf("plan anchor: %w", err)
	}
	if !descends {
		return policyf("plan anchor", "%s %s at %s does not descend from the recorded anchor %s",
			r.opts.Ref.Kind, r.revision.Name, r.revision.Commit, anchor)
	}
	r.report.Source.AnchorVerified = true
	return nil
}

// materializeSource creates the isolated work tree the closure reads.
//
// The tree is always one this run created below its own work root. Pointing the
// closure at a directory an operator already had would let it prune that
// directory's files, and the pruning is real removal: the closure is handed a
// tree it is allowed to change.
//
// The name is unique per run rather than derived from the commit. Two plans over
// one commit are exactly the case an operator hits when comparing profiles or
// when CI runs a matrix, and a shared name would make the second fail on a
// registration the first owns, or worse, hand it a tree the first is pruning.
func (r *run) materializeSource(ctx context.Context) error {
	patterns, err := r.sparsePatterns(nil)
	if err != nil {
		return err
	}
	r.patterns = patterns

	worktree, err := r.cache.AddWorktree(ctx, source.WorktreeOptions{
		Commit:   r.revision.Commit,
		Patterns: patterns,
		// An offline run may not reach the promisor remote for a blob the cache
		// is missing, and the checkout below is the step that would.
		NoLazyFetch: r.opts.Offline,
	})
	if err != nil {
		return err
	}
	r.worktree = worktree
	r.paths.Worktree = worktree.Path()
	return nil
}

// sparsePatterns renders the pattern set for the configured roots plus whatever
// widening has added.
//
// Widened directories only ever materialize files. They are never added to the
// closure's roots, because a root seeds the traversal and expands under a
// recursive profile, so promoting a discovered package to a root would change
// which packages the closure contains and make the report describe a different
// profile from the one on disk.
func (r *run) sparsePatterns(extra []string) ([]string, error) {
	roots := slices.Concat(r.cfg.Packages.Roots, r.widened, extra)
	patterns, err := source.SparsePatterns(roots, r.cfg.Packages.Recursive)
	if err != nil {
		return nil, fmt.Errorf("plan materialize: %w", err)
	}
	return patterns, nil
}

// buildClosure computes the closure, widening the work tree whenever it reaches
// a package the pattern set did not materialize.
//
// One Builder serves the whole plan. It remembers the pre-prune baseline and
// which prune entries it has already removed, so reasserting after a patch stays
// idempotent while an upstream rename still fails; a fresh Builder over an
// already pruned tree could not tell those two apart. Widening happens before
// anything is pruned, which is why rematerializing the pristine tree between
// rounds costs nothing and keeps the baseline honest.
func (r *run) buildClosure(ctx context.Context) error {
	builder, err := closure.New(ctx, r.closureOptions())
	if err != nil {
		return closurePolicy(err)
	}
	r.builder = builder

	bound := r.widenBound()
	for round := 1; round <= bound; round++ {
		r.report.Closure.Rounds = round
		result, err := builder.Build(ctx)
		if err == nil {
			r.closureResult = result
			r.prePatchFiles = planPaths(result.CopyPlan)
			return nil
		}
		dir, widenable := missingPackageDir(err)
		if !widenable || slices.Contains(r.cfg.Packages.Roots, dir) || slices.Contains(r.widened, dir) {
			// Either the failure is not a missing materialization, or the
			// directory is already in the pattern set and widening again would
			// loop without progress. Both mean the closure's answer stands.
			return closurePolicy(err)
		}
		r.widened = append(r.widened, dir)
		patterns, err := r.sparsePatterns(nil)
		if err != nil {
			return err
		}
		if err := r.worktree.SetPatterns(ctx, patterns); err != nil {
			return err
		}
		r.patterns = patterns
	}
	return policyf("plan closure", "the package set was still growing after %d widening rounds, most recently %s",
		bound, strings.Join(r.widened, ", "))
}

// widenBound reports how many widening rounds this profile may take.
//
// Each round materializes exactly one package the closure discovered and could
// not read, so the loop terminates naturally once the closure is complete, and
// the bound exists only for the case where it does not. Deriving it from the
// profile's own package ceiling is what keeps it from becoming a second, hidden
// limit: a profile that legitimately reaches two hundred packages would be
// refused by a fixed bound of sixty-four for no reason its author could find,
// and the refusal would name widening rather than the package count. The roots
// are added because they are materialized without being discovered, and the
// slack lets the loop reach the round that proves the closure complete.
func (r *run) widenBound() int {
	limit := r.cfg.Closure.Limits.MaxPackages
	if limit <= 0 {
		// A profile with no package ceiling is bounded by the number of
		// directories a source repository can hold, so the fallback only has to
		// be larger than any real closure while still being finite.
		limit = defaultWidenCeiling
	}
	return limit + len(r.cfg.Packages.Roots) + 1
}

// planPaths lists a copy plan's paths, sorted.
func planPaths(plan []closure.CopyEntry) []string {
	out := make([]string, 0, len(plan))
	for _, entry := range plan {
		out = append(out, entry.Path)
	}
	slices.Sort(out)
	return out
}

// closureOptions maps the profile onto one closure build.
//
// The mapping is exact and total: every field the profile carries for the
// closure appears here, so a profile that names a prune entry, a required file,
// a denied import, an asset glob, or a limit cannot have it silently ignored.
func (r *run) closureOptions() closure.Options {
	return closure.Options{
		Root:          r.worktree.Path(),
		ImportPrefix:  r.cfg.Source.ImportPrefix,
		Roots:         slices.Clone(r.cfg.Packages.Roots),
		Recursive:     r.cfg.Packages.Recursive,
		IncludeTests:  r.cfg.Closure.IncludeTests,
		PruneFiles:    slices.Clone(r.cfg.Prune.Files),
		RequiredFiles: slices.Clone(r.cfg.Prune.Required),
		DeniedImports: slices.Clone(r.cfg.Deny.Imports),
		AssetGlobs:    slices.Clone(r.cfg.Packages.AssetGlobs),
		Limits: closure.Limits{
			MaxPackages:      r.cfg.Closure.Limits.MaxPackages,
			MaxFiles:         r.cfg.Closure.Limits.MaxFiles,
			MaxNonTestLines:  r.cfg.Closure.Limits.MaxNonTestLines,
			MaxPackageGrowth: r.cfg.Closure.Limits.MaxPackageGrowth,
		},
	}
}

// missingPackageDir reports the repository relative directory a closure failure
// says was not materialized.
//
// Only a genuinely absent package is widenable. The closure reports the same
// sentinel for a directory that exists and holds no Go files, but it carries the
// directory either way, so the caller compares the answer against the pattern
// set rather than this function trying to tell the two apart.
func missingPackageDir(err error) (string, bool) {
	if !errors.Is(err, closure.ErrPackageMissing) {
		return "", false
	}
	var imported *closure.ImportError
	if errors.As(err, &imported) && imported.Dir != "" {
		return imported.Dir, true
	}
	var pkg *closure.PackageError
	if errors.As(err, &pkg) && pkg.Dir != "" && pkg.Dir != "." {
		return pkg.Dir, true
	}
	return "", false
}

// closurePolicy classifies a closure failure.
//
// Every sentinel the closure defines describes the tree or the profile: a prune
// target upstream renamed, a denied import that came back, a required file that
// did not survive, a limit that was passed, a package that will not parse. All
// of them are findings a maintainer acts on, so they exit as policy failures
// rather than as engine faults. A filesystem error carries none of them and
// travels on unchanged, and so does a cancellation.
func closurePolicy(err error) error {
	return policyIf("plan closure", err, func(err error) bool {
		var options *closure.OptionsError
		var limit *closure.LimitError
		return errors.As(err, &options) || errors.As(err, &limit) ||
			errors.Is(err, closure.ErrRootMissing) ||
			errors.Is(err, closure.ErrPackageMissing) ||
			errors.Is(err, closure.ErrPackageMalformed) ||
			errors.Is(err, closure.ErrPruneMissing) ||
			errors.Is(err, closure.ErrPruneOutsideClosure) ||
			errors.Is(err, closure.ErrPruneNotMaterialized) ||
			errors.Is(err, closure.ErrPruneExcludedTest) ||
			errors.Is(err, closure.ErrPruneRequired) ||
			errors.Is(err, closure.ErrPruneLastFile) ||
			errors.Is(err, closure.ErrRequiredMissing) ||
			errors.Is(err, closure.ErrImportDenied) ||
			errors.Is(err, closure.ErrLimitExceeded) ||
			errors.Is(err, closure.ErrUnsafeSymlink) ||
			errors.Is(err, closure.ErrRecursivePattern) ||
			errors.Is(err, closure.ErrPatternNoMatch) ||
			errors.Is(err, closure.ErrPatternMalformed) ||
			errors.Is(err, closure.ErrEmbedNestedModule) ||
			errors.Is(err, closure.ErrEmbedBadName)
	})
}

// writeScratchAnchor records the materialized and pruned tree as the work tree's
// detached HEAD.
//
// Patch application needs it. Three way application resolves blobs through the
// index and the object store, and its rollback restores exactly one committed
// state, so a pruned tree that existed only as uncommitted work tree content
// could neither be merged against nor restored. The commit is made even when the
// profile carries no patches, so every plan reports the same anchor field and a
// profile that gains its first patch does not change what the report contains.
//
// Nothing leaves the work tree. HEAD there is detached, so the commit is
// unreachable from every ref in the shared cache; the objects it writes are
// garbage that the cache's own maintenance collects. The refs are compared
// afterwards to prove exactly that.
func (r *run) writeScratchAnchor(ctx context.Context) error {
	git := r.worktree.Git()
	removed := slices.Clone(r.closureResult.RemovedFiles)
	slices.Sort(removed)

	if len(removed) > 0 {
		if err := git.AddPaths(ctx, removed...); err != nil {
			return fmt.Errorf("plan anchor: %w", err)
		}
	}
	if err := git.Commit(ctx, gitcli.CommitOptions{
		Message:    scratchAnchorMessage,
		Author:     r.scratchIdentity(),
		Committer:  r.scratchIdentity(),
		AllowEmpty: true,
	}); err != nil {
		return fmt.Errorf("plan anchor: %w", err)
	}

	commit, err := git.ResolveCommit(ctx, "HEAD")
	if err != nil {
		return fmt.Errorf("plan anchor: %w", err)
	}
	tree, err := git.ResolveTree(ctx, "HEAD")
	if err != nil {
		return fmt.Errorf("plan anchor: %w", err)
	}
	r.report.Worktree.ScratchAnchor = AnchorReport{
		Commit:          commit,
		Tree:            tree,
		Parent:          r.revision.Commit,
		StagedDeletions: removed,
	}
	return nil
}

// scratchIdentity is the fixed identity the scratch anchor carries. The bot the
// profile names is used so the commit is recognisably the engine's, and the date
// is fixed so the object name depends only on the tree.
func (r *run) scratchIdentity() gitcli.Signature {
	return gitcli.Signature{
		Name:  r.cfg.Commit.Committer.Name,
		Email: r.cfg.Commit.Committer.Email,
		Date:  scratchAnchorDate,
	}
}

// applyPatches loads, selects, and applies the profile's patch series.
func (r *run) applyPatches(ctx context.Context) error {
	patches, err := patchset.Load(ctx, r.opts.ProfileDir, r.cfg.Patches)
	if err != nil {
		return patchPolicy(err)
	}
	r.report.Patches.Available = len(patches)
	r.report.Patches.Branch = r.opts.PatchBranch

	git := r.worktree.Git()
	var selected []patchset.Patch
	if len(patches) > 0 {
		selected, err = patchset.Select(ctx, git, patches, patchset.Target{
			Branch: r.opts.PatchBranch,
			Commit: r.revision.Commit,
		})
		if err != nil {
			return patchPolicy(err)
		}
	}
	r.report.Patches.Selected = patchIDs(selected)

	result, err := patchset.Apply(ctx, git, patchset.Request{
		SourceRef: r.revision.Ref,
		SourceSHA: r.revision.Commit,
		Patches:   selected,
		ReassertPrune: func(ctx context.Context, patch patchset.Patch) error {
			// The same Builder reasserts, so a patch that reintroduced a pruned
			// file is removed again and a patch that targeted one fails here
			// rather than three phases later. Naming the patch is the point of
			// recording it: the reassertion exists to catch a patch that put a
			// pruned file back, and a count cannot say which one did.
			settled, err := r.builder.Build(ctx)
			if err != nil {
				return err
			}
			r.report.Patches.Reassert = append(r.report.Patches.Reassert, PatchReassert{
				PatchID: patch.ID,
				Files:   sorted(settled.RemovedFiles),
			})
			return nil
		},
	})
	r.report.Patches.Reasserted = len(r.report.Patches.Reassert)
	if err != nil {
		return patchPolicy(err)
	}
	r.applied = result.PatchIDs()
	r.report.Patches.Applied = slices.Clone(r.applied)
	return nil
}

// patchPolicy classifies a patch phase failure.
//
// The patch package separates the two answers itself. Its sentinels describe the
// profile's patch entries and the tree they were handed, a conflict describes a
// diff that no longer applies, and both are findings a maintainer acts on. A
// failure to read a file or to run git carries none of them and is an engine or
// environment fault, so it travels on as a runtime failure.
//
// A reassertion that failed reaches here wrapped in a conflict at the prune
// stage, carrying whatever the closure reported; that is still a finding, and
// the conflict's own stage is what tells the two apart in the report.
func patchPolicy(err error) error {
	return policyIf("plan patch", err, func(err error) bool {
		var conflict *patchset.ConflictError
		return errors.As(err, &conflict) ||
			errors.Is(err, patchset.ErrNoGit) ||
			errors.Is(err, patchset.ErrDuplicatePatch) ||
			errors.Is(err, patchset.ErrEmptyPatch) ||
			errors.Is(err, patchset.ErrNoIdentifier) ||
			errors.Is(err, patchset.ErrIncompleteTarget) ||
			errors.Is(err, patchset.ErrIncompleteRequest) ||
			errors.Is(err, patchset.ErrDirtyWorkTree)
	})
}

// patchIDs renders a selected series as identifiers in application order.
func patchIDs(patches []patchset.Patch) []string {
	ids := make([]string, 0, len(patches))
	for _, patch := range patches {
		ids = append(ids, patch.ID)
	}
	return ids
}

// settleClosure recomputes the closure over the final tree.
//
// It runs even when no patch applied. A build is what proves the tree still
// satisfies the profile, and running it unconditionally means the reported
// closure always describes the bytes that were relocated rather than the bytes
// that existed before the patch phase decided it had nothing to do.
func (r *run) settleClosure(ctx context.Context) error {
	result, err := r.builder.Build(ctx)
	if err != nil {
		return closurePolicy(err)
	}
	r.closureResult = result
	r.report.Closure.Report = result.Report
	r.report.Closure.RemovedFiles = slices.Clone(r.report.Worktree.ScratchAnchor.StagedDeletions)

	r.closureDirs = make(map[string]bool, len(result.Packages))
	for _, pkg := range result.Packages {
		r.closureDirs[pkg.Dir] = true
	}

	// Everything the run took out of the tree, however it went: the profile's
	// prune entries and whatever the patch series deleted on top of them. A
	// generator marker is dangling either way, and consulting only the profile
	// would leave the module carrying an instruction a patch made impossible.
	final := indexOf(planPaths(result.CopyPlan))
	r.removed = indexOf(r.cfg.Prune.Files)
	for _, file := range r.prePatchFiles {
		if !final[file] {
			r.removed[file] = true
		}
	}
	return nil
}

// indexOf turns a list into a membership set.
func indexOf(values []string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, value := range values {
		set[value] = true
	}
	return set
}

// cleanup removes the work tree unless the operator asked to keep it.
//
// Only this run's own tree is touched. The name is unique per run, so a
// concurrent plan's tree is never the one removed here.
func (r *run) cleanup(ctx context.Context) error {
	if r.worktree == nil || r.opts.KeepWorktree {
		return nil
	}
	// The removal runs on a context detached from the caller's, because one of
	// the ways this is reached is a cancelled plan, and a cleanup that inherited
	// the cancellation would leave exactly the scratch directory it exists to
	// remove. The deadline is what keeps the detachment from turning a cancelled
	// command into an unstoppable one.
	detached, cancel := detachedContext(ctx)
	defer cancel()
	if err := r.worktree.Remove(detached); err != nil {
		return err
	}
	r.worktree = nil
	r.paths.Worktree = ""
	return nil
}

// detachedContext derives a bounded context that outlives the caller's.
func detachedContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
}

// relocate reads the closure's copy plan, maps it onto destination paths,
// rewrites what has to change, and maps the result again.
//
// Two relocation passes rather than one is deliberate. The first pass establishes
// the destination paths the rewriting steps need: a go:embed pattern is verified
// against destination paths, and a provenance record names the destination
// package. The second pass carries the rewritten bytes and the generated
// provenance files through the same validation the first pass performed, so
// nothing that was produced after the first pass reaches a tree unchecked, and
// the embed verification that is reported runs over that second pass.
func (r *run) relocate(ctx context.Context) error {
	plan, err := r.readCopyPlan(ctx)
	if err != nil {
		return err
	}
	pass1, err := relocate.Build(ctx, plan, r.relocateOptions())
	if err != nil {
		return contentPolicy("plan relocate", err)
	}
	if err := r.rewriteFiles(ctx, pass1); err != nil {
		return err
	}
	records, err := r.planProvenance(ctx, pass1)
	if err != nil {
		return err
	}
	if err := r.buildFinalTree(ctx, pass1, records); err != nil {
		return err
	}
	return r.measureGoFiles(ctx)
}

// relocateOptions maps the profile onto the relocation.
//
// Symbolic links are refused rather than resolved. The closure already refuses
// every link it would copy, so this is the second of two independent refusals,
// and the generated module is meant to be consumable everywhere rather than only
// where a link happens to resolve.
func (r *run) relocateOptions() relocate.Options {
	return relocate.Options{
		InternalPrefix: r.cfg.Destination.InternalPrefix,
		Symlinks:       relocate.SymlinkReject,
	}
}

// readCopyPlan reads every file the closure selected.
//
// Reads go through an [os.Root] opened on the work tree, so containment is
// re-checked by the operating system for each operation rather than once against
// a path that could have changed since. The mode is read from the file rather
// than taken from the plan and then checked against it: a disagreement means the
// tree changed under the run, which is worth reporting rather than resolving.
//
// The reason the closure selected each file travels with it. A .go file that
// arrived as embedded data or as a matched asset is content the module serves
// rather than source it compiles, and every later step has to be able to tell
// the two apart from something other than the file's extension.
func (r *run) readCopyPlan(ctx context.Context) (relocate.Plan, error) {
	root, err := os.OpenRoot(r.worktree.Path())
	if err != nil {
		return relocate.Plan{}, fmt.Errorf("plan relocate: %w", err)
	}
	defer root.Close()

	r.kinds = make(map[string]closure.CopyKind, len(r.closureResult.CopyPlan))
	files := make([]relocate.PlanFile, 0, len(r.closureResult.CopyPlan))
	for _, entry := range r.closureResult.CopyPlan {
		if err := ctx.Err(); err != nil {
			return relocate.Plan{}, fmt.Errorf("plan relocate: %w", err)
		}
		pkg := path.Dir(entry.Path)
		if pkg == "." || pkg == "/" {
			// Relocation preserves a file's complete upstream path below the
			// internal prefix, and a repository root file has no package to be
			// preserved under. Copying one would put an unowned file into the
			// generated module's internal tree.
			return relocate.Plan{}, policyf("plan relocate",
				"%q lies in the repository root, which has no package to relocate it under", entry.Path)
		}
		info, err := root.Lstat(entry.Path)
		if err != nil {
			return relocate.Plan{}, fmt.Errorf("plan relocate: inspect %s: %w", entry.Path, err)
		}
		mode, err := relocate.ModeOf(info.Mode())
		if err != nil {
			return relocate.Plan{}, policyf("plan relocate", "%q: %w", entry.Path, err)
		}
		if got, want := info.Mode().Perm(), entry.Mode.Perm(); got != want {
			return relocate.Plan{}, fmt.Errorf("plan relocate: %q changed mode from %v to %v while the plan ran", entry.Path, want, got)
		}
		contents, err := root.ReadFile(entry.Path)
		if err != nil {
			return relocate.Plan{}, fmt.Errorf("plan relocate: read %s: %w", entry.Path, err)
		}
		r.kinds[entry.Path] = entry.Kind
		files = append(files, relocate.PlanFile{
			Path:      entry.Path,
			Package:   pkg,
			Mode:      mode,
			Contents:  contents,
			Generated: rewriteGenerated(entry, contents),
		})
	}
	return relocate.Plan{Files: files}, nil
}
