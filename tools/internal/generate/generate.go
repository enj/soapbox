// Package generate composes the engine's extraction, staging, module, facade,
// type, dependency, and provenance phases into one complete generated module
// for an upstream release tag or a release-bounded exact commit.
//
// A plan answers what the extracted tree would contain. A generation answers
// what the published module would be: the same relocated code, plus the go.mod
// and go.sum a consumer resolves against, the curated facade a consumer imports,
// and the root evidence that says where the code came from and how it differs
// from upstream. It stops before history replay and publication, which are later
// phases with their own gates.
//
// Four properties bound what a generation may do.
//
// It is read-only outward. Nothing here creates a ref, pushes, or contacts a
// destination repository. The source cache is driven by an anonymous runner, so
// a credential that exists for publishing cannot travel to the source host, and
// the run refuses to start at all while such a credential is visible in the
// environment.
//
// It is contained. Every directory is absolute, checked, and disjoint. The
// scratch trees the phases need are created below the work root and owned by
// this run, and the final output tree is written exactly once, at the end, from
// a file set every gate has already passed.
//
// It fails closed. A generation is a sequence of gates rather than a sequence of
// steps: the pre-prune and post-prune public APIs must match, the generated
// go.mod must survive tidying without a pin floating, the type substitution must
// be provable against upstream package identities, the dependency decision must
// be reachable from measured evidence, and the root provenance must account for
// every file in the tree it describes. A gate that cannot be evaluated refuses
// rather than passes, and the shapes this first engine does not support refuse
// explicitly rather than approximating an answer.
//
// It is deterministic. Two runs over one source commit with different directory
// layouts produce byte identical reports and byte identical trees. The report
// therefore carries no absolute path, no proxy URL, no credential, no source
// remote override, and no timestamp: what it records is the profile, the source
// commit, and the content the two produce.
package generate

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/enj/soapbox/tools/internal/config"
	"github.com/enj/soapbox/tools/internal/deppolicy"
	"github.com/enj/soapbox/tools/internal/extract"
	"github.com/enj/soapbox/tools/internal/facade"
	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/gitgraph"
	"github.com/enj/soapbox/tools/internal/gocli"
	"github.com/enj/soapbox/tools/internal/gomodmap"
	"github.com/enj/soapbox/tools/internal/modgen"
	"github.com/enj/soapbox/tools/internal/modulegraph"
	"github.com/enj/soapbox/tools/internal/provenance"
	"github.com/enj/soapbox/tools/internal/relocate"
	"github.com/enj/soapbox/tools/internal/typeswap"
)

// ReportSchema is the version of the report this package emits.
const ReportSchema = 1

var (
	// ErrCredentialEnvironment reports a publishing credential visible to a run
	// that has no use for one.
	ErrCredentialEnvironment = errors.New("a generation must run without publishing credentials")
	// ErrPathConflict reports directories that overlap when they must not.
	ErrPathConflict = errors.New("the generation directories conflict")
	// ErrUnsupported reports a run shape this engine refuses rather than
	// approximates. Moving branch names remain preview inputs; reconciliation
	// resolves and supplies immutable commits instead.
	ErrUnsupported = errors.New("this generation engine does not support the requested run")
)

// resolverModulePath is the module the staging version resolver runs in.
//
// It is deliberately unresolvable. The resolver's main module is part of every
// version query the go command answers, so it has to be a module that requires
// nothing, replaces nothing, excludes nothing, and is not itself one of the
// modules being asked about. A reserved invalid top level domain guarantees the
// last of those for any upstream layout rather than for the ones known today.
const resolverModulePath = "soapbox.invalid/resolver"

// Options configures one generation.
//
// Every directory is absolute because a generation must name the same
// directories no matter where the process was started from, and because the run
// adopts none of them: the cache and work roots are created if absent and owned
// by the run thereafter, and the output tree must not exist at all.
// StagingSource names the trusted history used to resolve one intermediate
// Kubernetes staging module. Reconciliation derives the production URL from the
// module path; local fixtures may override it only while the source itself is
// also overridden to a local repository.
type StagingSource struct {
	Remote string
}

type Options struct {
	// Config is the decoded, validated profile.
	Config *config.Config
	// ProfileDir is the repository directory holding the profile, the patch
	// files its patch entries name, and the closure golden it pins.
	ProfileDir string
	// CacheRoot holds the reusable bare source cache. Both extraction passes
	// share it, so one clone serves the whole run.
	CacheRoot string
	// WorkRoot holds the scratch trees this run creates and removes.
	WorkRoot string
	// OutputRoot is where the generated module is written. It must not exist;
	// relocation never merges into or overwrites a tree.
	OutputRoot string
	// StorePath is the version index file the staging resolution caches into.
	// It is absolute, and the run creates it if it is absent.
	StorePath string
	// Ref selects the upstream source. Release tags are public inputs; the
	// reconciliation engine also supplies exact commits already fetched through a
	// bounded release head. Branches remain unsupported.
	Ref extract.Ref
	// ReleaseContext is the upstream release whose bounded history contains an
	// exact commit. It is required for RefCommit and empty for RefTag, where Ref
	// itself is the release. Dependency override expiry and staging history use
	// this context without claiming the intermediate commit is tagged.
	ReleaseContext string
	// HistoryAnchor and HistoryAnchorRelease bound intermediate source and
	// staging walks to the last published release, inclusive.
	HistoryAnchor        string
	HistoryAnchorRelease string
	// StagingSources supplies the trusted repository for each staging module
	// when an intermediate index entry must be resolved. It must name exactly the
	// required modules; a cached index hit needs no repositories.
	StagingSources map[string]StagingSource
	// PatchBranch is the tracked branch a patch's branch selector is matched
	// against. It is required only when the profile carries patches.
	PatchBranch string
	// SourceRemote overrides the profile's source repository, which is how a
	// test or an air-gapped operator points the run at a local mirror.
	SourceRemote string
	// Fetch updates the cache before the ref is resolved.
	Fetch bool
	// Offline refuses every network operation. A generation still has to read
	// the upstream licence out of the cache, so an offline run whose cache does
	// not hold that blob is a policy failure rather than a silent fetch.
	Offline bool
	// Materialize writes the generated module to OutputRoot. Without it the run
	// computes and gates the same tree and hashes it without touching a disk.
	Materialize bool
	// KeepWorktree leaves the scratch trees in place for inspection.
	KeepWorktree bool
	// Strict turns every advisory notice into a policy failure, which it does
	// before any output is written rather than after.
	Strict bool
	// Git is the runner the extraction phases drive. It must be anonymous: a
	// generation talks to the public source host and to nothing else.
	Git *gitcli.Runner
	// Go is the runner every Go toolchain phase drives. Its isolation and its
	// proxy decide where module state comes from, and the run rebases it onto
	// each scratch module rather than building runners of its own, so the
	// caller owns that environment in one place.
	Go *gocli.Runner
	// LookupEnv reads the process environment. A nil value uses os.LookupEnv.
	// It is injectable so the credential check is testable without mutating the
	// environment of a running test binary.
	LookupEnv func(string) (string, bool)
}

// Paths are the absolute directories one generation used.
//
// They are deliberately outside Report, which carries no absolute path.
type Paths struct {
	// Cache is the bare source cache directory.
	Cache string
	// Work is the scratch root this run owns.
	Work string
	// Output is the generated module destination, written only with
	// -materialize.
	Output string
	// Store is the version index file.
	Store string
	// PreModule and PostModule are the scratch relocated modules the two
	// extraction passes produced. PreModule exists only to establish the facade
	// baseline and is never a candidate for the final output.
	PreModule  string
	PostModule string
	// PreWorktree and PostWorktree are the materialized upstream source trees,
	// empty once they were removed. PreWorktree is where the type policy runs,
	// because it holds the upstream package identities the profile names.
	PreWorktree  string
	PostWorktree string
	// Resolver is the isolated scratch module the staging version resolver ran
	// in.
	Resolver string
}

// Result is one completed generation.
//
// A generation that refused still produces one whenever it measured enough to
// be worth reading. Report.Failure is what tells the two apart, and it is the
// reason a refusal is reviewable from an artifact rather than from a stderr
// line.
type Result struct {
	// Report is the deterministic record of what the generation found.
	Report Report
	// Files is the complete generated module: the relocated upstream code, the
	// generated facade, the tidied module metadata, and the root provenance.
	// It is what -materialize writes, and it is empty for a run that refused
	// before it had composed a tree.
	Files relocate.FileSet
	// Paths are the absolute directories the run used.
	Paths Paths
}

// PolicyError reports a generation that ran correctly and found the profile,
// its inputs, or the module they produce unacceptable.
//
// It exists for the same reason the extraction phase has one: the command line
// has to separate the answer "the engine worked and the answer is no" from "the
// engine could not answer". A drifted public API, a floated pin, an unprovable
// substitution, an unaccounted file, and a licence that is not the one the
// profile names are all findings about the profile. Only those exit with the
// check code CI reads as "review this".
type PolicyError struct {
	// Stage names the phase that refused, such as extract, staging, module,
	// facade, types, dependencies, provenance, or output.
	Stage string
	// Err is the underlying failure.
	Err error
}

func (e *PolicyError) Error() string {
	return "generate: " + e.Stage + ": " + e.Err.Error()
}

func (e *PolicyError) Unwrap() error { return e.Err }

// policyError wraps a failure as a refusal attributed to one stage.
func policyError(stage string, err error) error {
	return &PolicyError{Stage: stage, Err: err}
}

// runtimeError wraps a failure that means the engine could not answer.
//
// It is deliberately not a PolicyError. A caller acts on the two differently: a
// refusal is a finding about the profile that a reviewer reads, while a clone
// that timed out, a proxy that refused, a disk that filled, or a cancelled
// context is a condition to retry or repair. Folding the second into the first
// would have CI open a review for a network blip, and would make a cancelled run
// indistinguishable from an unacceptable module.
func runtimeError(stage string, err error) error {
	return fmt.Errorf("generate: %s: %w", stage, err)
}

// classify attributes a failure to one stage, as a refusal when it is one of the
// semantic outcomes that stage can reach and as a runtime failure otherwise.
//
// The sentinel lists are what make this honest. Every phase drives subprocesses
// and reads files as well as deciding something, so "this phase failed" says
// nothing about which kind of failure it was; only the error the underlying
// package chose does. Anything not on a list is treated as runtime, so a
// sentinel nobody classified fails toward "the engine could not answer" rather
// than toward an accusation about the profile.
func classify(stage string, err error, semantic ...error) error {
	for _, sentinel := range semantic {
		if errors.Is(err, sentinel) {
			return policyError(stage, err)
		}
	}
	return runtimeError(stage, err)
}

// The semantic failures of each phase, which are the ones that mean the engine
// worked and the answer is no. Everything else a phase can return is a runtime
// failure.
var (
	// stagingSemantic covers a resolved version that does not name the commit it
	// claims and a staging module that did not resolve at all. A corrupt or
	// unreadable index is deliberately absent: it is a cache to delete and
	// rebuild, not a finding about the profile.
	stagingSemantic = []error{gomodmap.ErrVersionMismatch, gomodmap.ErrUnresolvedModule}
	// moduleSemantic covers the two ways the toolchain can disagree with the
	// module this engine generated.
	moduleSemantic = []error{modgen.ErrModuleDrift, modgen.ErrPinFloated}
	// facadeSemantic covers every way the profile's published surface is not
	// publishable. All of them are statements about what the profile asked for.
	facadeSemantic = []error{
		facade.ErrSpec, facade.ErrMissingSymbol, facade.ErrKindMismatch,
		facade.ErrMutableVar, facade.ErrGeneric, facade.ErrCollision,
		facade.ErrLeak, facade.ErrUnrepresentable,
	}
	// typesSemantic covers an unusable type policy and a pairing naming a
	// package the graph does not contain. A missing graph is an engine defect
	// rather than a profile finding, so it is absent.
	typesSemantic = []error{
		typeswap.ErrInvalidOptions, typeswap.ErrPackageMissing,
		modulegraph.ErrRelabelUnproven, modulegraph.ErrPackageMissing,
	}
	// dependencySemantic covers every way the dependency section describes a
	// decision that cannot be reached.
	dependencySemantic = []error{
		deppolicy.ErrInvalidOptions, deppolicy.ErrStagingPathMalformed,
		deppolicy.ErrProposalUnknown, deppolicy.ErrOverrideExpired,
		deppolicy.ErrOverrideUnused, deppolicy.ErrIdentityMalformed,
		deppolicy.ErrIdentityUnknown, modulegraph.ErrModuleConflict,
		modulegraph.ErrPackageMissing,
	}
	// provenanceSemantic covers evidence that does not account for the tree, a
	// licence that is not the one it is labelled as, and a notice that cannot be
	// embedded without corrupting the delimiter.
	provenanceSemantic = []error{
		provenance.ErrOptions, provenance.ErrSecret,
		provenance.ErrDelimiter, provenance.ErrLicense, provenance.ErrEvidence,
	}
)

// Generate produces the complete generated module for one upstream release tag.
//
// The phases run in a fixed order because each one's inputs are the previous
// one's proven outputs, and the order is what makes the gates meaningful rather
// than decorative. The facade cannot be compared before both modules exist, the
// dependency decision cannot be reached before the facade's imports are in the
// tree the module graph is loaded from, and the root provenance cannot be
// cross-checked before the tree it describes has been composed.
//
// The final tree is written last and only once. Every gate runs against the tree
// in memory, so a run that refuses leaves no output at all rather than a
// directory an operator has to know not to trust.
func Generate(ctx context.Context, opts Options) (*Result, error) {
	r, err := newRun(ctx, opts)
	if err != nil {
		return nil, err
	}

	runErr := r.execute(ctx)
	cleanupErr := r.cleanup()
	if runErr != nil {
		// A refusal that measured something is still worth reading, so the
		// report travels with the error rather than being discarded by it. A
		// cleanup failure is joined because leaving a linked worktree behind is
		// additional state the caller must know about.
		return r.result(), errors.Join(runErr, cleanupErr)
	}
	if cleanupErr != nil {
		r.report.fail(stageCleanup, cleanupErr)
		return r.result(), cleanupErr
	}
	return r.result(), nil
}

// run is the mutable state of one generation.
//
// It exists so a failure at any stage can still return everything measured up
// to that point. Each phase writes what it proved into the report as it goes,
// rather than the driver assembling a report at the end from values a refusal
// would have prevented it from having.
type run struct {
	opts  Options
	paths Paths

	// cfg is this run's own copy of the profile. The caller's Config is never
	// written to, because a caller that reused it after a generation would be
	// holding a profile this run had edited.
	cfg *config.Config

	report Report
	files  relocate.FileSet

	// pre and post are the two extraction passes. pre drops the profile's
	// pruning so the facade has a baseline to compare against and so the type
	// policy can see the packages pruning removes.
	pre  *extract.Result
	post *extract.Result

	// root is the source module as the upstream commit declares it.
	root *gomodmap.RootModule
	// staging are the resolved staging module versions.
	staging []gomodmap.ModuleVersion

	// moduleReport is the post-prune module's verification, which holds the
	// tidied metadata the tree publishes.
	moduleReport *modgen.Report

	// preFacade and postFacade are the two generated facades, and facadeDiffs
	// are their rendered differences. The differences are carried rather than
	// recomputed because the type policy consumes them and the facade owns what
	// a public API difference is.
	preFacade   facade.Result
	postFacade  facade.Result
	facadeDiffs []string

	// types is the type policy analysis, which the root provenance reads so a
	// documented behaviour change cannot be forgotten.
	types *typeswap.Result
	// compatibilityChanges document intentional API differences introduced by
	// the selected compatibility mode.
	compatibilityChanges []provenance.BehaviorChange

	// copyFiles are the materialized staging copies, ready for composition.
	copyFiles relocate.FileSet
	// copyEvidence records the measured identity and provenance of each
	// staging module whose packages were copied, keyed by module path.
	copyEvidence map[string]copyEvidence

	// worktrees are the scratch source trees to remove on the way out.
	worktrees []string
}

// newRun validates the options and prepares the directories the phases own.
func newRun(ctx context.Context, opts Options) (*run, error) {
	if err := opts.check(); err != nil {
		return nil, err
	}
	cloned, err := cloneConfig(opts.Config)
	if err != nil {
		return nil, fmt.Errorf("generate: clone profile: %w", err)
	}
	// Every phase and the report read the same private profile. Keeping the
	// original pointer in Options would leave report initialization exposed to a
	// caller mutating a nested slice while the run is active.
	opts.Config = cloned
	opts.StagingSources = maps.Clone(opts.StagingSources)
	loaderEnv, err := opts.Go.LoaderEnv(ctx)
	if err != nil {
		return nil, runtimeError(stageOptions, fmt.Errorf("loader environment: %w", err))
	}

	work := filepath.Join(opts.WorkRoot, workDirName)
	r := &run{
		opts: opts,
		cfg:  cloned,
		paths: Paths{
			Cache:      opts.CacheRoot,
			Work:       work,
			Output:     opts.OutputRoot,
			Store:      opts.StorePath,
			PreModule:  filepath.Join(work, preDirName, moduleDirName),
			PostModule: filepath.Join(work, postDirName, moduleDirName),
			Resolver:   filepath.Join(work, resolverDirName),
		},
	}
	r.report.init(opts)
	r.report.addLoaderEnvironment(loaderEnv)

	// The scratch root is removed first so a previous run's trees can never be
	// read as this one's. Extraction refuses an output tree that already exists,
	// which would otherwise turn a stale directory into a confusing failure
	// rather than a fresh start.
	if err := os.RemoveAll(work); err != nil {
		return nil, fmt.Errorf("generate: scratch root: %w", err)
	}
	if err := os.MkdirAll(work, 0o750); err != nil {
		return nil, fmt.Errorf("generate: scratch root: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return r, nil
}

// scratch directory names. They are constants rather than literals because the
// path checks and the cleanup both have to name the same directories the phases
// were pointed at.
const (
	workDirName     = "generate"
	preDirName      = "pre"
	postDirName     = "post"
	moduleDirName   = "module"
	sourceDirName   = "source"
	resolverDirName = "resolver"
)

// execute runs the phases in order, recording the stage that refused.
func (r *run) execute(ctx context.Context) error {
	for _, phase := range []struct {
		stage string
		run   func(context.Context) error
	}{
		{stageExtract, r.runExtract},
		{stageStaging, r.runStaging},
		{stageCompatibility, r.runCompatibility},
		{stageModule, r.runModule},
		{stageFacade, r.runFacade},
		{stageTypes, r.runTypes},
		{stageDependencies, r.runDependencies},
		{stageProvenance, r.runProvenance},
		{stageOutput, r.runOutput},
	} {
		if err := ctx.Err(); err != nil {
			r.report.fail(phase.stage, err)
			return err
		}
		if err := phase.run(ctx); err != nil {
			r.report.fail(phase.stage, err)
			return err
		}
	}
	return nil
}

// stage names, shared by the driver, the PolicyError values each phase raises,
// and the report.
const (
	stageOptions       = "options"
	stageExtract       = "extract"
	stageStaging       = "staging"
	stageCompatibility = "compatibility"
	stageModule        = "module"
	stageFacade        = "facade"
	stageTypes         = "types"
	stageDependencies  = "dependencies"
	stageProvenance    = "provenance"
	stageOutput        = "output"
	stageCleanup       = "cleanup"
)

// result renders what the run measured, whether or not it finished.
func (r *run) result() *Result {
	r.report.normalize()
	return &Result{Report: r.report, Files: r.files, Paths: r.paths}
}

// cleanup removes the scratch trees this run created.
//
// The context is deliberately not the caller's. Cleanup runs on the way out of a
// cancelled run as well as a successful one, and a cancelled context would make
// every removal fail exactly when the trees most need removing. It is bounded so
// a wedged Git process cannot hold the command open indefinitely.
func (r *run) cleanup() error {
	if r.opts.KeepWorktree {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()

	var errs []error
	// The linked work trees are unregistered through Git before the scratch root
	// is removed, so the shared cache is not left holding administrative entries
	// pointing at directories that no longer exist.
	for _, dir := range r.worktrees {
		if r.opts.Git == nil {
			errs = append(errs, errors.New("generate cleanup: no Git runner is available to unregister a worktree"))
			break
		}
		cache, err := r.opts.Git.WithDir(r.paths.Cache)
		if err != nil {
			errs = append(errs, fmt.Errorf("generate cleanup: scope source cache: %w", err))
			break
		}
		if err := cache.RemoveWorktree(ctx, dir); err != nil {
			errs = append(errs, fmt.Errorf("generate cleanup: remove worktree: %w", err))
		}
	}
	if err := os.RemoveAll(r.paths.Work); err != nil {
		errs = append(errs, fmt.Errorf("generate cleanup: remove scratch root: %w", err))
	} else {
		r.paths.PreWorktree = ""
		r.paths.PostWorktree = ""
	}
	return errors.Join(errs...)
}

// cleanupTimeout bounds the work removal on the way out.
const cleanupTimeout = 2 * time.Minute

// check validates everything about the options that can be decided before the
// first subprocess starts.
func (o Options) check() error {
	if o.Config == nil {
		return errors.New("generate: a profile is required")
	}
	if o.Git == nil {
		return errors.New("generate: a Git runner is required")
	}
	if o.Go == nil {
		return errors.New("generate: a Go runner is required")
	}
	// The extraction phases refuse a credentialed runner themselves, but they
	// refuse it several subprocesses in. Refusing here states the requirement
	// where the runner is supplied, which is where a caller can act on it.
	if !o.Git.IsAnonymous() {
		return fmt.Errorf("generate: %w: the Git runner carries caller supplied environment entries", ErrCredentialEnvironment)
	}
	if err := o.checkRef(); err != nil {
		return err
	}
	if err := o.checkPaths(); err != nil {
		return err
	}
	return o.checkCredentialEnvironment()
}

// checkRef accepts reviewed release tags and engine-selected exact commits.
//
// Exact commits must already be present in a release-bounded source cache and
// carry the release context used for dependency gates and staging history.
// Branches remain a human preview input: reconciliation resolves their commits
// itself rather than letting a moving name enter generation.
func (o Options) checkRef() error {
	switch o.Ref.Kind {
	case extract.RefTag:
		if o.HistoryAnchor != "" || o.HistoryAnchorRelease != "" {
			return errors.New("generate: a release tag needs no intermediate history anchor")
		}
		if len(o.StagingSources) != 0 {
			return errors.New("generate: a release tag resolves staging modules by tag, so StagingSources must be empty")
		}
		if o.ReleaseContext != "" {
			return errors.New("generate: a release tag is its own release context, so ReleaseContext must be empty")
		}
	case extract.RefCommit:
		if err := gitgraph.ValidateSHA(o.Ref.Name); err != nil {
			return fmt.Errorf("generate: exact source commit: %w", err)
		}
		if o.ReleaseContext == "" {
			return errors.New("generate: an exact source commit requires its bounding release context")
		}
		if o.HistoryAnchor == "" || o.HistoryAnchorRelease == "" {
			return errors.New("generate: an exact source commit requires its published history anchor and release")
		}
		if err := gitgraph.ValidateSHA(o.HistoryAnchor); err != nil {
			return fmt.Errorf("generate: intermediate history anchor: %w", err)
		}
		if _, err := config.ParseSemver(o.HistoryAnchorRelease); err != nil {
			return fmt.Errorf("generate: intermediate history anchor release: %w", err)
		}
		if _, err := config.ParseSemver(o.ReleaseContext); err != nil {
			return fmt.Errorf("generate: exact source commit release context: %w", err)
		}
		if o.Fetch {
			return errors.New("generate: an exact source commit must already be present in the cache, so it cannot be fetched by object name")
		}
	case extract.RefBranch:
		return policyError(stageOptions, fmt.Errorf("%w: ref %s is a branch, and only a release tag or an engine-selected exact commit can be generated", ErrUnsupported, o.Ref))
	default:
		return fmt.Errorf("generate: ref kind %q must be %s or %s", o.Ref.Kind, extract.RefTag, extract.RefCommit)
	}
	if o.Ref.Name == "" {
		return errors.New("generate: a ref name is required")
	}
	return nil
}

func (o Options) sourceRelease() string {
	if o.Ref.Kind == extract.RefCommit {
		return o.ReleaseContext
	}
	return o.Ref.Name
}

// checkPaths proves every directory is absolute and that none of them contains
// another.
//
// Containment is what the check is really about. The run removes its own scratch
// root on the way in and on the way out, so a work root that contained the cache
// would delete a clone the operator paid for, and one that contained the profile
// directory would delete the repository the run was started from.
func (o Options) checkPaths() error {
	dirs := []namedDir{
		{"source cache root", o.CacheRoot, false},
		{"work root", o.WorkRoot, false},
		{"output tree", o.OutputRoot, false},
		{"profile directory", o.ProfileDir, false},
		{"version index", o.StorePath, true},
	}
	for _, entry := range dirs {
		if entry.path == "" {
			return fmt.Errorf("generate: the %s is required", entry.name)
		}
		if !filepath.IsAbs(entry.path) {
			return fmt.Errorf("generate: the %s %q must be absolute", entry.name, entry.path)
		}
		if entry.path != filepath.Clean(entry.path) {
			return fmt.Errorf("generate: the %s %q must be a clean path", entry.name, entry.path)
		}
	}
	for i, a := range dirs {
		for _, b := range dirs[i+1:] {
			if err := checkDisjoint(a, b); err != nil {
				return err
			}
		}
	}
	if _, err := os.Lstat(o.OutputRoot); err == nil {
		return fmt.Errorf("generate: the output tree %s already exists", o.OutputRoot)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("generate: inspect output tree: %w", err)
	}
	return nil
}

// namedDir is one directory a path check reasons about.
//
// The version index is a file rather than a tree, so containment is checked
// against the directory holding it: a store inside the scratch root would be
// removed between the run that wrote it and the run that meant to reuse it.
type namedDir struct {
	name string
	path string
	file bool
}

// dir renders the directory this entry occupies.
func (n namedDir) dir() string {
	if n.file {
		return filepath.Dir(n.path)
	}
	return n.path
}

// checkDisjoint refuses two directories where either contains the other.
//
// The profile directory and the version index are allowed to share a tree,
// because a repository that checks its index in is a reasonable layout and
// neither is ever removed by this run. Everything else is refused.
func checkDisjoint(a, b namedDir) error {
	dirA, dirB := a.dir(), b.dir()
	// The version index is operational cache state, so the conventional layout
	// keeps it in the cache root. This is the only permitted containment: placing
	// it under the profile, work tree, or generated output would make a run edit
	// its inputs or publish its own mutable cache.
	switch {
	case a.name == "source cache root" && b.name == "version index" && (dirA == dirB || contains(dirA, dirB)):
		return nil
	case b.name == "source cache root" && a.name == "version index" && (dirA == dirB || contains(dirB, dirA)):
		return nil
	}
	if dirA == dirB {
		return fmt.Errorf("generate: %w: the %s and the %s are both %s", ErrPathConflict, a.name, b.name, dirA)
	}
	if contains(dirA, dirB) {
		return fmt.Errorf("generate: %w: the %s %s contains the %s %s", ErrPathConflict, a.name, dirA, b.name, dirB)
	}
	if contains(dirB, dirA) {
		return fmt.Errorf("generate: %w: the %s %s contains the %s %s", ErrPathConflict, b.name, dirB, a.name, dirA)
	}
	return nil
}

// contains reports whether outer is an ancestor directory of inner.
//
// The comparison is on path elements rather than on the string, so /a/bc is not
// treated as living inside /a/b.
func contains(outer, inner string) bool {
	rel, err := filepath.Rel(outer, inner)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// checkCredentialEnvironment refuses to run while a publishing credential is
// visible.
//
// The anonymous runner already keeps caller supplied values away from every
// subprocess, so this is about the operator rather than the subprocess. A
// generation produces a candidate module and gates it; it never publishes one,
// so it has no use for a credential at all, and a machine holding a publishing
// token while running it is a sign the wrong command is being used.
func (o Options) checkCredentialEnvironment() error {
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
	return fmt.Errorf("generate: %w: %s is set", ErrCredentialEnvironment, strings.Join(present, ", "))
}

// cloneConfig copies a profile so the run can derive variants without writing to
// the caller's value.
//
// Canonical encoding followed by strict decoding is the schema-owned deep copy:
// every nested slice is rebuilt, normalized ordering is retained, and a field
// added to Config cannot be forgotten by a hand-maintained clone routine.
func cloneConfig(cfg *config.Config) (*config.Config, error) {
	data, err := cfg.Canonical()
	if err != nil {
		return nil, fmt.Errorf("canonical profile: %w", err)
	}
	clone, err := config.Decode(data)
	if err != nil {
		return nil, fmt.Errorf("decode canonical profile: %w", err)
	}
	return clone, nil
}

// cacheRunner points the Git runner at the shared source cache.
//
// It is stripped to anonymous even though the options already refused a
// credentialed runner, because this is the runner that reads upstream blobs and
// the guarantee that a publishing credential never reaches the source host is
// worth restating where the reads happen rather than only where they were
// configured.
func (r *run) cacheRunner() (*gitcli.Runner, error) {
	cache, err := r.opts.Git.WithDir(r.paths.Cache)
	if err != nil {
		return nil, fmt.Errorf("source cache: %w", err)
	}
	return cache.Anonymous(), nil
}
