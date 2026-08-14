package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/enj/soapbox/tools/internal/actionsctx"
	"github.com/enj/soapbox/tools/internal/config"
	"github.com/enj/soapbox/tools/internal/generate"
	"github.com/enj/soapbox/tools/internal/ghapi"
	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/gocli"
	"github.com/enj/soapbox/tools/internal/publish"
	"github.com/enj/soapbox/tools/internal/source"
	"github.com/enj/soapbox/tools/internal/sync"
)

// Fixed workflow environment names. The token is the publishing credential;
// the approval is an optional reviewed manifest hash for manual mode and never a
// credential.
const (
	tokenEnvName    = "SOAPBOX_GITHUB_TOKEN"
	approvalEnvName = "SOAPBOX_APPROVAL"
)

// syncWorkflowFile is the workflow file name the engine checks for being
// enabled. It matches the file setup generates.
const syncWorkflowFile = "sync.yml"

// syncWorkflowPath is the repository-relative path GitHub reports for the sync
// workflow. It must match what setup generates; a mismatch means the repository
// contains a different workflow at the same file name, which the engine must
// refuse rather than trust.
const syncWorkflowPath = ".github/workflows/" + syncWorkflowFile

// syncFlags holds the parsed sync flags.
//
// The generation flags are the generate command's, spelled and behaving
// identically, so an operator who generated a release synchronizes it by
// changing the verb. What is added is the destination: where the objects are
// written, where they would go, and whether this invocation is allowed to send
// them.
type syncFlags struct {
	*generateFlags
	destination *string
	remote      *string
	identity    *string
	localRemote *bool
	stateCommit *string
	apply       *bool
	approve     *string
	unattended  *bool
}

func syncFlagSet() (*flag.FlagSet, *syncFlags) {
	fs := newFlagSet("sync")
	shared := registerRunFlags(fs, runSpec{
		verb:  "synchronize",
		tree:  "generated module",
		cache: "",
		work:  "<cache>" + generateWorkSuffix,
		out:   "<cache>" + generateOutSuffix,
	})
	return fs, &syncFlags{
		generateFlags: &generateFlags{
			runFlags: shared,
			proxy:    fs.String("proxy", gocli.DefaultProxy, "module proxy every Go command resolves through, or "+gocli.ProxyOff+" to resolve nothing"),
			index:    fs.String("version-index", "", "staging version index, relative to -dir when not absolute (default <cache>/"+defaultVersionIndex+")"),
		},
		destination: fs.String("destination", "", "local destination repository the objects are written into (required)"),
		remote:      fs.String("remote", "", "push target (default destination.remote from the profile)"),
		identity:    fs.String("identity", "", "canonical destination repository recorded in the manifest (default derived from the profile)"),
		localRemote: fs.Bool("local-remote", false, "permit a filesystem destination, which only a local dry run should need"),
		stateCommit: fs.String("state-commit", "", "previous state record to resume from, empty for a destination that holds none"),
		apply:       fs.Bool("apply", false, "publish the plan, which requires -approve and a reachable destination"),
		approve:     fs.String("approve", "", "the manifest hash being approved, required by -apply"),
		unattended:  fs.Bool("unattended", false, "run as a trusted workflow, reading "+tokenEnvName+" and self-approving"),
	}
}

// runSync computes one complete synchronization and, when it is approved,
// publishes it.
//
// Every usage problem is decided before the profile is read, so a command line
// that cannot work fails the same way whether or not a profile, a cache, or a
// network happens to be there. Publication is off unless the operator asked for
// it and quoted the hash they are approving: the default outcome of this
// command is a manifest on stdout and nothing outward at all.
func runSync(ctx context.Context, env Env, args []string) error {
	fs, flags := syncFlagSet()
	if err := parseFlags(env, syncCommand(), fs, args); err != nil {
		return err
	}
	usage := commandUsage(syncCommand(), fs)
	given := setFlags(fs)

	if err := checkSyncFlags(flags, given); err != nil {
		return &usageError{err: err, usage: usage}
	}
	proxy, err := generateProxy(flags.generateFlags, given)
	if err != nil {
		return &usageError{err: err, usage: usage}
	}
	paths, err := generatePaths(env, flags.generateFlags)
	if err != nil {
		return &usageError{err: err, usage: usage}
	}
	destination, err := filepath.Abs(env.resolve(*flags.destination))
	if err != nil {
		return &usageError{err: fmt.Errorf("resolve -destination: %w", err), usage: usage}
	}

	cfg, err := config.Load(ctx, paths.config)
	if err != nil {
		return profileError(env, paths.config, err)
	}

	// Unattended mode requires automatic publication.
	if *flags.unattended && cfg.Publication.Mode != config.PublicationModeAutomatic {
		return &usageError{
			err:   fmt.Errorf("-unattended requires publication.mode %q, profile has %q", config.PublicationModeAutomatic, cfg.Publication.Mode),
			usage: usage,
		}
	}

	ref, err := selectedRef(flags.runFlags, cfg)
	if err != nil {
		return &usageError{err: err, usage: usage}
	}
	patchBranch, err := selectedPatchBranch(flags.runFlags, cfg, ref)
	if err != nil {
		return &usageError{err: err, usage: usage}
	}

	// Read the token and validate the workflow context. The token is read
	// exactly once, before any subprocess or network call, so no code path
	// can observe it from two call sites. os.LookupEnv is the production
	// lookup; tests pass a static map instead.
	token, actionsCtx, err := syncToken(flags, cfg, usage, os.LookupEnv)
	if err != nil {
		return err
	}
	workflowApply := *flags.apply
	workflowApproval := *flags.approve
	if actionsCtx != nil && !*flags.unattended {
		approval, ok := os.LookupEnv(approvalEnvName)
		if ok && approval != "" {
			if workflowApply && workflowApproval != approval {
				return &usageError{err: errors.New("the command-line approval and workflow approval differ"), usage: usage}
			}
			if err := validateWorkflowApproval(approval); err != nil {
				return &usageError{err: err, usage: usage}
			}
			workflowApply, workflowApproval = true, approval
		}
	}

	// The source runner is anonymous, exactly as a generation's is: reading
	// upstream talks to the public source host and to nothing else. The
	// destination runner is separate and is the only one a publication pushes
	// with, which is what keeps a credential from ever reaching a source read.
	sourceGit, err := gitcli.New(ctx, gitcli.Options{})
	if err != nil {
		return err
	}

	// When the Actions context was validated, compare GITHUB_SHA to the
	// destination repository HEAD before any credential is built. The check
	// uses an anonymous runner with lazy-fetch disabled so a partial or
	// promisor checkout cannot reach any remote and expose a token that does
	// not exist yet in this runner.
	//
	// The same runner verifies that the profile directory and the destination
	// are the same repository checkout, so -dir cannot supply one profile
	// while the token-bearing destination runner operates on another.
	var localDestinationGit *gitcli.Runner
	if actionsCtx != nil {
		if err := checkWorkflowSyncFlags(given); err != nil {
			return &usageError{err: err, usage: usage}
		}
		anonDest, anonErr := gitcli.New(ctx, gitcli.Options{Dir: destination, Inherit: []string{"PATH"}})
		if anonErr != nil {
			return anonErr
		}
		localDestinationGit = anonDest.WithNoLazyFetch()
		if err := verifySyncSHA(ctx, localDestinationGit, actionsCtx.SHA); err != nil {
			return err
		}
		if err := verifySyncCheckout(ctx, localDestinationGit, paths.dir); err != nil {
			return err
		}
	}

	// Build the destination runner. When a token is present it carries the
	// credential; otherwise it is anonymous and only local operations work.
	destOpts := gitcli.Options{Dir: destination, Inherit: []string{"PATH"}}
	if token != "" {
		cred, credErr := gitcli.NewGitHubTokenCredential(token)
		if credErr != nil {
			return fmt.Errorf("build destination credential: %w", credErr)
		}
		destOpts, err = cred.Apply(destOpts)
		if err != nil {
			return fmt.Errorf("apply destination credential: %w", err)
		}
	}
	destinationGit, err := gitcli.New(ctx, destOpts)
	if err != nil {
		return err
	}

	goRunner, err := generateGoRunner(ctx, paths.dir, proxy)
	if err != nil {
		return err
	}

	// Build the LookupEnv that hides the token from generate and extract.
	// The nested generation must never see the publishing credential.
	hiddenLookup := syncLookupEnv(os.LookupEnv)
	generateOpts := generate.Options{
		Config:       cfg,
		ProfileDir:   paths.dir,
		CacheRoot:    paths.cache,
		WorkRoot:     paths.work,
		OutputRoot:   paths.out,
		StorePath:    paths.store,
		Ref:          ref,
		PatchBranch:  patchBranch,
		SourceRemote: *flags.sourceRemote,
		Fetch:        *flags.fetch && !*flags.offline,
		Offline:      *flags.offline,
		Materialize:  *flags.materialize,
		KeepWorktree: *flags.keepWorktree,
		Strict:       *flags.strict,
		Git:          sourceGit,
		Go:           goRunner,
		LookupEnv:    hiddenLookup,
	}

	if actionsCtx != nil {
		if *flags.unattended {
			if err := verifySyncWorkflow(ctx, token, cfg); err != nil {
				return err
			}
		}
		cache, err := source.Open(ctx, source.Options{
			Remote: cfg.Source.Repository, CacheRoot: paths.cache,
			WorktreeRoot: filepath.Join(paths.work, "reconcile-source-worktrees"),
			Git:          sourceGit,
		})
		if err != nil {
			return syncError(err, usage)
		}
		dest := sync.Destination{
			Git: destinationGit, Remote: cfg.Destination.Remote,
			Identity: "github.com/" + cfg.Destination.Repository,
			Lister:   publish.NewHTTPSRemote(destinationGit),
		}
		budget := sync.WorkflowBudget{}
		if *flags.unattended {
			budget = sync.WorkflowBudget{
				Deadline: time.Now().Add(165 * time.Minute),
				Reserve:  10 * time.Minute,
			}
		}
		reconciled, err := sync.Reconcile(ctx, sync.ReconcileOptions{
			Config: cfg, SourceCache: cache,
			LocalGit: localDestinationGit, RemoteGit: destinationGit,
			Destination: dest, Generate: generateOpts,
			Apply: workflowApply, Approval: workflowApproval,
			Automatic: *flags.unattended, Budget: budget,
		})
		if err != nil {
			return syncError(err, usage)
		}
		if err := writeReportOutput(ctx, env, "sync", paths.report, *flags.format,
			reconciled.JSON, reconciled.Text); err != nil {
			return err
		}
		if reconciled.NeedsConfiguration {
			return syncError(errors.New("reconciliation requires the resolved source anchor to be persisted in the profile"), usage)
		}
		return nil
	}

	result, err := sync.Plan(ctx, sync.Options{
		Generate: generateOpts,
		Destination: sync.Destination{
			Git:              destinationGit,
			Remote:           syncRemote(flags, cfg),
			Identity:         syncIdentity(flags, cfg),
			AllowLocalRemote: *flags.localRemote,
		},
		StateCommit: *flags.stateCommit,
	})
	if err != nil {
		return syncError(err, usage)
	}

	if err := writeReportOutput(ctx, env, "sync", paths.report, *flags.format,
		result.Manifest.JSON, result.Manifest.Text); err != nil {
		return err
	}

	if !*flags.apply {
		return nil
	}
	applied, err := sync.Apply(ctx, result, sync.ApplyOptions{Approval: *flags.approve})
	// What a failed publication already did is reported before the failure is,
	// because it is the first thing an operator needs. A push that failed having
	// applied some of its refs, or one that failed after the bookkeeping half
	// landed, leaves a destination somebody has to reason about, and a bare
	// error would say only that something went wrong.
	writeApplied(env, applied)
	if err != nil {
		return syncError(err, usage)
	}
	return nil
}

// checkSyncFlags decides every contradiction the command line can hold, before
// anything is read.
//
// The approval pair is the one that matters. A publication asked for without a
// hash cannot be served, and a hash offered without a publication is an operator
// who believes they published and did not, so both halves are refused rather
// than one being taken as implying the other.
func checkSyncFlags(flags *syncFlags, given map[string]bool) error {
	if !slices.Contains(runFormats, *flags.format) {
		return fmt.Errorf("unsupported -format %q, want %s", *flags.format, strings.Join(runFormats, ", "))
	}
	if given["tag"] && given["branch"] {
		return errors.New("-tag and -branch select different refs, so only one may be given")
	}
	if *flags.branch != "" {
		return errors.New("a synchronization publishes a release, so -branch cannot select it")
	}
	if *flags.offline && given["fetch"] && *flags.fetch {
		return errors.New("-offline refuses every network operation, so -fetch cannot also be requested")
	}
	if *flags.destination == "" {
		return errors.New("a synchronization writes its objects into a destination repository, so -destination is required")
	}

	// Unattended mode is mutually exclusive with manual apply/approve,
	// local-remote, remote/identity overrides, state-commit, tag, and
	// source-remote. These are contradictions in intent: unattended self-
	// approves, pushes over HTTPS, and derives every parameter from the
	// profile and the Actions context. A manual override alongside it is
	// either a mistake or an attempt to change what the trusted workflow
	// publishes, both of which the engine must refuse.
	if *flags.unattended {
		switch {
		case *flags.apply:
			return errors.New("-unattended self-approves, so -apply cannot also be given")
		case *flags.approve != "":
			return errors.New("-unattended self-approves, so -approve cannot also be given")
		case *flags.localRemote:
			return errors.New("-unattended publishes over HTTPS, so -local-remote cannot also be given")
		case given["remote"]:
			return errors.New("-unattended derives the remote from the profile, so -remote cannot also be given")
		case given["identity"]:
			return errors.New("-unattended derives the identity from the profile, so -identity cannot also be given")
		case given["state-commit"]:
			return errors.New("-unattended discovers state from the destination, so -state-commit cannot also be given")
		case given["tag"]:
			return errors.New("-unattended discovers releases from the source, so -tag cannot also be given")
		case given["source-remote"]:
			return errors.New("-unattended derives the source from the profile, so -source-remote cannot also be given")
		}
		return nil
	}

	switch {
	case *flags.apply && *flags.approve == "":
		return errors.New("-apply publishes, so the manifest hash being approved must be given with -approve")
	case !*flags.apply && *flags.approve != "":
		return errors.New("-approve names a manifest to publish, so -apply must also be given")
	}
	return nil
}

// syncToken reads the publishing credential and validates the Actions context.
//
// The credential is read in exactly one place and returned as a plain string,
// which the caller puts into exactly two typed holders (GitHubTokenCredential
// and StaticBearer) and never formats again. Every code path that uses the
// token is downstream from this function.
//
// The invariant is: Actions context is validated before the token is read.
// In unattended mode, Validate runs first and only then is the token looked
// up. In manual mode with a token present, the token's existence is detected
// but its value is not returned until Validate has passed. When neither
// unattended nor a token is set, the function returns empty and no credential
// is wired.
//
// The returned Context is non-nil when validation ran, regardless of mode; the
// caller uses it to compare GITHUB_SHA to the destination HEAD.
//
// The lookup parameter is the environment reader. Production passes
// os.LookupEnv; tests pass a static map.
func checkWorkflowSyncFlags(given map[string]bool) error {
	for _, flag := range []string{
		"local-remote", "remote", "identity",
		"state-commit", "tag", "branch", "source-remote", "patch-branch",
		"offline", "fetch",
	} {
		if given[flag] {
			return fmt.Errorf("trusted workflow reconciliation derives its inputs, so -%s cannot be given", flag)
		}
	}
	return nil
}

func validateWorkflowApproval(approval string) error {
	const prefix = "sha256:"
	if len(approval) != len(prefix)+64 || !strings.HasPrefix(approval, prefix) {
		return fmt.Errorf("%s must be sha256: followed by 64 lowercase hexadecimal characters", approvalEnvName)
	}
	for _, r := range approval[len(prefix):] {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return fmt.Errorf("%s must be sha256: followed by 64 lowercase hexadecimal characters", approvalEnvName)
		}
	}
	return nil
}

func syncToken(flags *syncFlags, cfg *config.Config, usage func(io.Writer), lookup actionsctx.LookupEnv) (string, *actionsctx.Context, error) {
	if *flags.unattended {
		// Validate the Actions context before reading the token.
		actionsCtx, err := actionsctx.Validate(actionsctx.Options{
			LookupEnv:     lookup,
			Repository:    cfg.Destination.Repository,
			DefaultBranch: cfg.Destination.Branch,
		})
		if err != nil {
			return "", nil, fmt.Errorf("workflow context: %w", err)
		}

		token, ok := lookup(tokenEnvName)
		if !ok || token == "" {
			return "", nil, &usageError{
				err:   fmt.Errorf("-unattended requires %s to be set", tokenEnvName),
				usage: usage,
			}
		}
		return token, actionsCtx, nil
	}

	// Non-unattended: detect whether the token is present. If it is,
	// validate the Actions context before returning the value.
	token, haveToken := lookup(tokenEnvName)
	if haveToken && token == "" {
		haveToken = false
	}
	if !haveToken {
		return "", nil, nil
	}

	actionsCtx, err := actionsctx.Validate(actionsctx.Options{
		LookupEnv:     lookup,
		Repository:    cfg.Destination.Repository,
		DefaultBranch: cfg.Destination.Branch,
	})
	if err != nil {
		return "", nil, fmt.Errorf("workflow context: %w", err)
	}

	return token, actionsCtx, nil
}

// verifySyncSHA checks that the validated GITHUB_SHA matches the destination
// repository HEAD. A mismatch means the checkout drifted from what Actions
// recorded, which could mean the workflow is running against stale code.
func verifySyncSHA(ctx context.Context, destGit *gitcli.Runner, expectedSHA string) error {
	head, err := destGit.ResolveCommit(ctx, "HEAD")
	if err != nil {
		return fmt.Errorf("verify destination HEAD: %w", err)
	}
	if head != expectedSHA {
		return fmt.Errorf("destination HEAD %s does not match GITHUB_SHA %s", head, expectedSHA)
	}
	return nil
}

// verifySyncCheckout verifies that the destination repository and the profile
// directory belong to the same Git repository. Without this check, -dir could
// supply a profile from a nested repository (such as a submodule or an
// independently initialized subdirectory) while the token-bearing destination
// runner operates on the outer checkout, which would let a crafted profile
// control what the credential publishes.
//
// Both roots are discovered by asking Git for the work tree root from each
// directory, then resolved through symlinks and compared for exact equality.
// A containment check alone is insufficient because a nested Git repository
// inside the destination path would pass it.
func verifySyncCheckout(ctx context.Context, destGit *gitcli.Runner, profileDir string) error {
	destRoot, err := destGit.RepositoryRoot(ctx)
	if err != nil {
		return fmt.Errorf("verify checkout: destination: %w", err)
	}

	profileGit, err := gitcli.New(ctx, gitcli.Options{Dir: profileDir, Inherit: []string{"PATH"}})
	if err != nil {
		return fmt.Errorf("verify checkout: profile runner: %w", err)
	}
	profileRoot, err := profileGit.WithNoLazyFetch().RepositoryRoot(ctx)
	if err != nil {
		return fmt.Errorf("verify checkout: profile: %w", err)
	}

	destResolved, err := filepath.EvalSymlinks(destRoot)
	if err != nil {
		return fmt.Errorf("verify checkout: resolve destination root: %w", err)
	}
	profileResolved, err := filepath.EvalSymlinks(profileRoot)
	if err != nil {
		return fmt.Errorf("verify checkout: resolve profile root: %w", err)
	}

	if destResolved != profileResolved {
		return fmt.Errorf("profile repository root %s does not match destination repository root %s", profileResolved, destResolved)
	}
	return nil
}

// syncLookupEnv builds the environment lookup the nested generation uses.
//
// The wrapper always hides the token environment variable, even when no token
// was present at the time of the call. A nil return would let generate fall
// back to os.LookupEnv, and an environment mutation between this point and
// the generation's own checkCredentialEnvironment would expose the token to
// subprocess inheritance. Returning a wrapper that unconditionally blocks the
// name closes that window.
//
// The base parameter is the same lookup the caller used; production passes
// os.LookupEnv, tests pass a static map.
func syncLookupEnv(base actionsctx.LookupEnv) func(string) (string, bool) {
	return func(name string) (string, bool) {
		if name == tokenEnvName || name == approvalEnvName {
			return "", false
		}
		return base(name)
	}
}

// verifySyncWorkflow checks the remote repository state before an unattended
// publication proceeds.
//
// It builds a ghapi client from the same token the publication uses and
// delegates to verifySyncWorkflowWithClient.
func verifySyncWorkflow(ctx context.Context, token string, cfg *config.Config) error {
	auth, err := ghapi.NewStaticBearer(token)
	if err != nil {
		return fmt.Errorf("workflow verification: %w", err)
	}
	client, err := ghapi.New(ghapi.Config{Authorizer: auth})
	if err != nil {
		return fmt.Errorf("workflow verification: %w", err)
	}
	return verifySyncWorkflowWithClient(ctx, client, cfg)
}

// verifySyncWorkflowWithClient checks three properties of the destination
// repository before an unattended publication:
//
//  1. The repository's default branch matches the profile, so a renamed
//     default branch fails closed rather than pushing to a branch the
//     repository no longer treats as default.
//  2. The sync workflow is enabled. GitHub disables scheduled workflows after
//     60 days of inactivity, and a disabled workflow fails silently by never
//     running, so each unattended run confirms it is still active.
//  3. The workflow path matches the expected path, so a workflow at the same
//     file name but a different location is refused.
//
// It is separated from verifySyncWorkflow so tests can inject an httptest-
// backed client without constructing a real token.
func verifySyncWorkflowWithClient(ctx context.Context, client *ghapi.Client, cfg *config.Config) error {
	owner, repo, ok := strings.Cut(cfg.Destination.Repository, "/")
	if !ok {
		return fmt.Errorf("workflow verification: destination repository %q is not owner/name", cfg.Destination.Repository)
	}

	defaultBranch, err := client.DefaultBranch(ctx, owner, repo)
	if err != nil {
		return fmt.Errorf("workflow verification: %w", err)
	}
	if defaultBranch != cfg.Destination.Branch {
		return fmt.Errorf("workflow verification: repository default branch %q does not match profile %q",
			defaultBranch, cfg.Destination.Branch)
	}

	workflow, err := client.Workflow(ctx, owner, repo, syncWorkflowFile)
	if err != nil {
		return fmt.Errorf("workflow verification: %w", err)
	}
	if !workflow.Enabled() {
		return fmt.Errorf("workflow verification: %s is %s, not %s", syncWorkflowFile, workflow.State, ghapi.WorkflowActive)
	}
	if workflow.Path != syncWorkflowPath {
		return fmt.Errorf("workflow verification: workflow path %q does not match expected %q", workflow.Path, syncWorkflowPath)
	}

	return nil
}

// syncRemote reports the push target, which the profile decides unless the
// operator overrode it.
func syncRemote(flags *syncFlags, cfg *config.Config) string {
	if *flags.remote != "" {
		return *flags.remote
	}
	return cfg.Destination.Remote
}

// syncIdentity reports the canonical destination recorded in the manifest.
//
// It is derived from the profile's repository rather than from the remote,
// because a manifest describes a repository rather than a location and a local
// dry run's remote is a temporary directory. An https publication would derive
// the same value from its own remote, so the two agree.
func syncIdentity(flags *syncFlags, cfg *config.Config) string {
	if *flags.identity != "" {
		return *flags.identity
	}
	if cfg.Destination.Repository == "" {
		return ""
	}
	return "github.com/" + cfg.Destination.Repository
}

// writeApplied reports what a publication did, on stderr.
//
// Stdout carries the manifest and nothing else, so a workflow that captures it
// gets one artifact rather than an artifact with a log appended to it.
func writeApplied(env Env, applied *sync.ApplyResult) {
	if applied == nil {
		return
	}
	if applied.DryRun {
		fmt.Fprintf(env.Stderr, "soapbox: rehearsed, nothing was pushed\n")
		return
	}
	// A nil half is a half that was never attempted, which is what the consumer
	// half is when the bookkeeping half failed. It is reported as such rather
	// than skipped, because "not attempted" and "attempted and changed nothing"
	// are different destinations.
	for _, half := range []struct {
		name    string
		outcome *sync.Outcome
	}{
		{"non-consumer", applied.NonConsumer},
		{"consumer", applied.Consumer},
	} {
		switch {
		case half.outcome == nil:
			fmt.Fprintf(env.Stderr, "soapbox: %s refs were not attempted\n", half.name)
		case half.outcome.Failed && !half.outcome.Verified:
			fmt.Fprintf(env.Stderr,
				"soapbox: %s push failed and the destination could not be read afterwards, so %s may or may not have been published\n",
				half.name, strings.Join(half.outcome.Attempted, ", "))
		case half.outcome.Failed:
			fmt.Fprintf(env.Stderr, "soapbox: %s push failed: %s published, %s not\n",
				half.name, orNone(half.outcome.Pushed), orNone(half.outcome.Unapplied))
		case len(half.outcome.Pushed) == 0:
			fmt.Fprintf(env.Stderr, "soapbox: %s refs were already published\n", half.name)
		default:
			fmt.Fprintf(env.Stderr, "soapbox: published %s %s\n",
				half.name, strings.Join(half.outcome.Pushed, ", "))
		}
	}
}

// orNone renders a ref list for a person, naming the empty case rather than
// printing nothing where a list was promised.
func orNone(refs []string) string {
	if len(refs) == 0 {
		return "none"
	}
	return strings.Join(refs, ", ")
}

// syncError maps a synchronization failure onto the process exit code contract.
//
// A generation policy failure means the engine ran and the answer is no, which
// CI reads as something to review. A refused approval and a run shape this
// engine does not implement are the operator's to fix and are reported as usage
// problems, because in both cases the command line is what has to change.
// Everything else is a runtime failure or a cancellation and is left for the
// dispatcher to classify.
func syncError(err error, usage func(io.Writer)) error {
	var policy *generate.PolicyError
	if errors.As(err, &policy) {
		return &checkError{summary: err.Error(), err: err}
	}
	if errors.Is(err, generate.ErrPathConflict) {
		return &usageError{err: err, usage: usage}
	}
	if errors.Is(err, sync.ErrApproval) || errors.Is(err, sync.ErrPublicationDisabled) {
		return &usageError{err: err, usage: usage}
	}
	return err
}
