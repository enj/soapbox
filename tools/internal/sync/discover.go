package sync

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/enj/soapbox/tools/internal/config"
	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/publish"
	"github.com/enj/soapbox/tools/internal/release"
	"github.com/enj/soapbox/tools/internal/source"
	"github.com/enj/soapbox/tools/internal/state"
)

// DiscoverOptions configures the preflight discovery that precedes generation.
type DiscoverOptions struct {
	// Config is the decoded, validated profile.
	Config *config.Config
	// LocalGit is the anonymous, no-lazy-fetch runner bound to the local
	// destination repository. It is used for every local object-store
	// operation: ObjectFormat, HasHead, ResolveCommit, state.Load,
	// ObjectInfoBatch, CommitInfo. Because it is anonymous and refuses
	// promisor fetches, no credential can leak to the source host and no
	// command silently downloads objects.
	LocalGit *gitcli.Runner
	// RemoteGit is the credentialed runner used exclusively for network
	// operations: RemoteRefs and FetchExact. It must be bound to the same
	// repository root as LocalGit. A nil value with AllowLocalRemote uses the
	// local reader; a nil value without it is a refusal.
	RemoteGit *gitcli.Runner
	// Remote is the destination push target.
	Remote string
	// Identity is the canonical destination repository.
	Identity string
	// AllowLocalRemote permits a filesystem destination.
	AllowLocalRemote bool
	// Lister reads the refs the destination advertises. A nil value with
	// AllowLocalRemote uses the local reader; a nil value without it is a
	// refusal.
	Lister publish.RemoteRefLister
	// SourceCache is the opened source cache to discover releases from.
	SourceCache *source.Cache
	// StateCommitOverride, when non-empty, is the manual -state-commit flag.
	// It must exactly equal the advertised state OID or it is refused.
	StateCommitOverride string
}

// PendingRelease is one upstream release that has not yet been published to the
// destination.
type PendingRelease struct {
	// Source is the verified upstream release.
	Source source.Release
	// DestinationTag is the tag the release policy maps the source onto.
	DestinationTag string
}

// AdoptedTag is a legacy tag that exists on the remote without a matching state
// entry. Only the profile's configured firstTag is eligible for adoption; all
// other unrecorded tags are fatal.
type AdoptedTag struct {
	// Ref is the fully qualified tag ref.
	Ref string
	// Tag is the short destination tag name.
	Tag string
	// Object is the annotated tag object OID on the remote.
	Object string
	// Commit is the commit the tag peels to.
	Commit string
	// Source is the upstream source commit recorded in the provenance trailer.
	Source string
}

// AdoptedBranch is a legacy branch that state does not record in Published.
// The branch was verified against state cursors and the local HEAD, and the
// caller must record it in the outward manifest as a state-reconciliation item.
type AdoptedBranch struct {
	// Ref is the fully qualified branch ref.
	Ref string
	// Object is the commit the branch points at.
	Object string
	// Source is the upstream source commit from the cursor.
	Source string
}

// ControlPlaneBranch is a verified fast-forward of the consumer branch that
// changes only operator-owned paths. State continues to name Base as the image
// of Source; Object is the graft point a future replay must preserve.
type ControlPlaneBranch struct {
	// Ref is the fully qualified branch ref.
	Ref string
	// Base is the generated commit state last observed.
	Base string
	// Object is the current control-plane branch head.
	Object string
	// Source is the source commit Base was generated from.
	Source string
}

// ResolvedAnchor records an anchor that was absent from the profile but proved
// from state and the source cache.
type ResolvedAnchor struct {
	// Commit is the verified anchor commit.
	Commit string
	// Ref is the fully qualified tag ref, such as refs/tags/v1.36.1.
	Ref string
}

// Discovery is the result of the preflight that reads the destination and the
// source to decide what, if anything, a synchronization should do.
type Discovery struct {
	// Format is the destination's hash algorithm.
	Format gitcli.ObjectFormat
	// Observed is the destination refs at the time of the read, by ref name.
	Observed map[string]string
	// StateCommit is the state record to resume from, empty when the
	// destination holds none.
	StateCommit string
	// State is the loaded state document, zero when StateCommit is empty.
	State state.Document
	// Pending are the upstream releases not yet published, in the semver order
	// DiscoverReleases produced. An empty slice means the destination is
	// already at the fixed point.
	Pending []PendingRelease
	// Adopted is a legacy tag that was verified and adopted into state
	// tracking. It is non-nil only when the firstTag existed on the remote
	// without a state entry and passed all provenance checks. The caller must
	// record it in the outward manifest.
	Adopted *AdoptedTag
	// AdoptedBranch is a legacy branch that state does not record in Published.
	// Non-nil only when state exists but has no Published entry for the
	// configured branch and the branch was verified against cursors. The caller
	// must record it in the outward manifest.
	AdoptedBranch *AdoptedBranch
	// ControlPlaneBranch is a verified operator-only fast-forward layered over
	// the generated commit state records. It does not need state reconciliation:
	// no source image or consumer tag changed.
	ControlPlaneBranch *ControlPlaneBranch
	// ResolvedAnchor records an anchor that was absent from the profile but
	// proved from state and the source cache. Non-nil only when the profile's
	// anchorCommit was empty and state provided a provable anchor.
	ResolvedAnchor *ResolvedAnchor
}

// FixedPoint reports whether the destination already holds every discovered
// release and no adoption is pending. A discovery that needs adoption still
// requires a state-reconciliation plan even though no generation is needed.
func (d *Discovery) FixedPoint() bool {
	return len(d.Pending) == 0 && d.Adopted == nil && d.AdoptedBranch == nil && d.ResolvedAnchor == nil
}

// Discover reads the destination and the source to decide what a
// synchronization should do.
//
// It validates the configured remote, identity, and object format; lists the
// destination refs; validates every advertised ref name, OID format, and
// namespace; finds and loads the state record; validates the loaded state
// against the profile; discovers upstream releases; and computes the
// deterministic ordered set of pending releases by comparing trusted state and
// advertised destination tags.
//
// Nothing is written. The destination and source are read-only throughout.
func Discover(ctx context.Context, opts DiscoverOptions) (*Discovery, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("discovery: %w", err)
	}
	if opts.Config == nil {
		return nil, errors.New("discovery: a profile is required")
	}
	if opts.LocalGit == nil {
		return nil, errors.New("discovery: a local destination git runner is required")
	}
	if opts.SourceCache == nil {
		return nil, errors.New("discovery: a source cache is required")
	}
	if !opts.LocalGit.IsNoLazyFetch() {
		return nil, errors.New("discovery: the local git runner must refuse promisor fetches")
	}
	if !opts.LocalGit.IsAnonymous() {
		return nil, errors.New("discovery: the local git runner must be anonymous")
	}
	// An HTTPS remote requires a credentialed RemoteGit for RemoteRefs and
	// FetchExact. Failing early here rather than at the first network call
	// gives the operator one message about what is missing.
	if !isLocalRemote(opts.Remote) {
		if opts.RemoteGit == nil {
			return nil, errors.New("discovery: an HTTPS remote requires a credentialed RemoteGit runner")
		}
		if opts.RemoteGit.IsAnonymous() {
			return nil, errors.New("discovery: the RemoteGit runner for an HTTPS remote must carry credentials")
		}
	}
	// When both runners are present they must be bound to the same repository
	// root. FetchExact through RemoteGit populates the object store that
	// LocalGit reads from; two different repositories would fetch into one and
	// load from another. The comparison uses RepositoryRoot and EvalSymlinks so
	// it is stable across bind mounts and symbolic links.
	if opts.RemoteGit != nil {
		if err := assertSameRepository(ctx, opts.LocalGit, opts.RemoteGit); err != nil {
			return nil, err
		}
	}
	// Unattended discovery requires an anchor commit so the release set is
	// bounded from below. When the profile does not provide one, state may
	// supply a provable anchor; that is checked after state is loaded.
	anchorCommit := opts.Config.Source.Refs.AnchorCommit
	minimumRelease := opts.Config.Source.Refs.MinimumRelease

	dest := Destination{
		Git:              opts.LocalGit,
		Remote:           opts.Remote,
		Identity:         opts.Identity,
		AllowLocalRemote: opts.AllowLocalRemote,
		Lister:           opts.Lister,
	}
	if err := checkDestination(dest, opts.Config); err != nil {
		return nil, err
	}

	format, err := opts.LocalGit.ObjectFormat(ctx)
	if err != nil {
		return nil, fmt.Errorf("discovery: %w", err)
	}

	// List the destination refs exactly once.
	lister := opts.Lister
	if lister == nil {
		if !isLocalRemote(opts.Remote) {
			return nil, fmt.Errorf("discovery: %w: %w", ErrPublicationDisabled, publish.ErrRemoteRefsUnsupported)
		}
		lister = publish.NewLocalRemote(opts.LocalGit)
	}
	refs, err := lister.RemoteRefs(ctx, opts.Remote)
	if err != nil {
		return nil, fmt.Errorf("discovery: read the destination: %w", err)
	}

	observed, err := indexDestinationRefs(refs, opts.Config, format)
	if err != nil {
		return nil, err
	}

	// Find and load the state record, then validate it against the profile.
	stateCommit, stateDoc, err := discoverState(ctx, opts, observed, format)
	if err != nil {
		return nil, err
	}

	// Resolve the anchor commit. When the profile provides one, use it
	// directly. When it does not, state must provide a provable anchor:
	// the state anchor ref must match refs/tags/<minimumRelease>, and the
	// source cache must prove that the tag resolves to the state anchor
	// source.
	var resolvedAnchor *ResolvedAnchor
	if anchorCommit == "" {
		if stateDoc.Schema == 0 {
			return nil, errors.New("discovery: neither profile anchorCommit nor state provides a provable anchor")
		}
		wantRef := "refs/tags/" + minimumRelease
		tags, err := opts.SourceCache.ListTags(ctx)
		if err != nil {
			return nil, fmt.Errorf("discovery: list source tags for anchor: %w", err)
		}
		var anchorTag *source.Revision
		for i := range tags {
			if tags[i].Name == minimumRelease {
				anchorTag = &tags[i]
				break
			}
		}
		if anchorTag == nil {
			return nil, fmt.Errorf("discovery: source tag %s not found in cache", minimumRelease)
		}
		if !anchorTag.Annotated {
			return nil, fmt.Errorf("discovery: source tag %s is not annotated", minimumRelease)
		}
		if anchorTag.Ref != wantRef {
			return nil, fmt.Errorf("discovery: source tag %s ref %q does not match expected %q", minimumRelease, anchorTag.Ref, wantRef)
		}
		if anchorTag.Commit != stateDoc.Anchor.Source {
			return nil, fmt.Errorf("discovery: source tag %s commit %s does not match state anchor source %s",
				minimumRelease, anchorTag.Commit, stateDoc.Anchor.Source)
		}
		anchorCommit = stateDoc.Anchor.Source
		resolvedAnchor = &ResolvedAnchor{Commit: anchorTag.Commit, Ref: anchorTag.Ref}
	}

	// Verify the destination main branch against state and the anonymous HEAD.
	branchRef := "refs/heads/" + opts.Config.Destination.Branch
	branch, err := verifyDestinationBranch(ctx, opts.LocalGit, observed, branchRef, stateDoc, opts.Config)
	if err != nil {
		return nil, err
	}

	// Legacy state created before common-anchor setup used the first release
	// itself as its immutable anchor. Such an anchor proves only that release's
	// minor line: a later minor may have branched before a patch release and is
	// deliberately left for a separately approved profile/repository transition.
	legacyMinorAnchor := stateDoc.Schema != 0 &&
		stateDoc.Anchor.Ref == "refs/tags/"+minimumRelease &&
		stateDoc.Anchor.Source == anchorCommit

	// Discover upstream releases.
	releases, err := opts.SourceCache.DiscoverReleases(ctx, source.ReleaseOptions{
		Minimum:            minimumRelease,
		IncludePrereleases: opts.Config.Source.Refs.IncludePrereleases,
		Policy:             opts.Config.Release.Policy,
		Anchor:             anchorCommit,
		SameMinor:          legacyMinorAnchor,
	})
	if err != nil {
		return nil, fmt.Errorf("discovery: %w", err)
	}

	// Compute the pending set, enforcing fail-closed tag immutability.
	pending, adopted, err := computePending(ctx, releases, stateDoc, observed, opts, format)
	if err != nil {
		return nil, err
	}

	return &Discovery{
		Format:             format,
		Observed:           observed,
		StateCommit:        stateCommit,
		State:              stateDoc,
		Pending:            pending,
		Adopted:            adopted,
		AdoptedBranch:      branch.adopted,
		ControlPlaneBranch: branch.controlPlane,
		ResolvedAnchor:     resolvedAnchor,
	}, nil
}

// indexDestinationRefs validates every advertised ref and indexes it by name.
//
// Each ref name is validated with ValidateRefName, each OID is checked for
// correct width and lowercase hex, duplicate advertisements are refused, and
// every ref must fall into one of the four expected namespaces: consumer
// branches under refs/heads/, consumer tags under refs/tags/, the configured
// state ref, or the progress namespace.
func indexDestinationRefs(refs []gitcli.Ref, cfg *config.Config, format gitcli.ObjectFormat) (map[string]string, error) {
	width := format.HexLength()
	observed := make(map[string]string, len(refs))

	for _, ref := range refs {
		// Skip non-ref lines like HEAD or peeled entries.
		if !strings.HasPrefix(ref.Name, "refs/") || strings.HasSuffix(ref.Name, "^{}") {
			continue
		}
		if err := gitcli.ValidateRefName(ref.Name); err != nil {
			return nil, fmt.Errorf("discovery: destination ref: %w", err)
		}
		if err := validateHexOID(ref.Target, width); err != nil {
			return nil, fmt.Errorf("discovery: destination ref %q: %w", ref.Name, err)
		}
		if prev, dup := observed[ref.Name]; dup {
			if prev != ref.Target {
				return nil, fmt.Errorf("discovery: destination ref %q advertised as both %s and %s",
					ref.Name, prev, ref.Target)
			}
			continue
		}
		if err := checkRefNamespace(ref.Name, cfg); err != nil {
			return nil, err
		}
		observed[ref.Name] = ref.Target
	}

	return observed, nil
}

// validateHexOID checks that s is exactly width lowercase hexadecimal
// characters and is not the null object.
func validateHexOID(s string, width int) error {
	if len(s) != width {
		return fmt.Errorf("object %q must be a %d-character hex name", s, width)
	}
	null := true
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return fmt.Errorf("object %q must be lowercase hexadecimal", s)
		}
		if c != '0' {
			null = false
		}
	}
	if null {
		return fmt.Errorf("object %q is the null object name", s)
	}
	return nil
}

// checkRefNamespace refuses a destination ref that does not belong to the
// expected set: the configured main branch, consumer tags, the state ref, or
// the progress namespace. Arbitrary branches outside the configured main and
// state ref are refused.
func checkRefNamespace(name string, cfg *config.Config) error {
	mainRef := "refs/heads/" + cfg.Destination.Branch
	if name == mainRef {
		return nil
	}
	if strings.HasPrefix(name, "refs/tags/") {
		return nil
	}
	if cfg.Destination.StateRef != "" && name == cfg.Destination.StateRef {
		return nil
	}
	if cfg.Destination.ProgressRefPrefix != "" && strings.HasPrefix(name, cfg.Destination.ProgressRefPrefix) {
		return nil
	}
	return fmt.Errorf("discovery: destination ref %q is outside the expected set "+
		"(%s, refs/tags/*, %s, %s*)",
		name, mainRef, cfg.Destination.StateRef, cfg.Destination.ProgressRefPrefix)
}

// discoverState finds and loads the state record from the destination.
//
// When StateCommitOverride is set, it is required to exactly match the
// advertised state OID. The state object is always fetched via FetchExact even
// for local remotes: the local destination checkout's object store may not hold
// the state commit because FetchExact is what populates it. After fetch,
// state.Load reads through the anonymous LocalGit runner.
//
// The loaded state document is validated against the profile: repository,
// module, and object format must agree.
func discoverState(ctx context.Context, opts DiscoverOptions, observed map[string]string, format gitcli.ObjectFormat) (string, state.Document, error) {
	stateRef := opts.Config.Destination.StateRef
	if stateRef == "" {
		return "", state.Document{}, nil
	}

	advertisedOID, hasState := observed[stateRef]
	override := opts.StateCommitOverride

	if override != "" {
		if !hasState {
			return "", state.Document{}, fmt.Errorf(
				"discovery: -state-commit %s was given but the destination does not advertise %s",
				override, stateRef)
		}
		if override != advertisedOID {
			return "", state.Document{}, fmt.Errorf(
				"discovery: -state-commit %s does not match the destination's %s at %s",
				override, stateRef, advertisedOID)
		}
	}

	if !hasState {
		return "", state.Document{}, nil
	}

	// Always FetchExact the state, even for local remotes. The local
	// destination checkout's object store does not necessarily hold the state
	// commit; FetchExact is what puts it there. The credentialed RemoteGit (or
	// local equivalent) performs the fetch, then the anonymous LocalGit loads.
	fetchGit := opts.RemoteGit
	if fetchGit == nil {
		fetchGit = opts.LocalGit
	}
	if err := fetchGit.FetchExact(ctx, opts.Remote, stateRef, advertisedOID, format.HexLength()); err != nil {
		return "", state.Document{}, fmt.Errorf("discovery: fetch state: %w", err)
	}

	doc, err := state.Load(ctx, opts.LocalGit, advertisedOID)
	if err != nil {
		return "", state.Document{}, fmt.Errorf("discovery: load state: %w", err)
	}

	if err := validateStateAgainstProfile(doc, opts.Config, format); err != nil {
		return "", state.Document{}, err
	}

	return advertisedOID, doc, nil
}

// validateStateAgainstProfile checks that the loaded state document describes
// the same destination the profile names and anchors from the same point.
func validateStateAgainstProfile(doc state.Document, cfg *config.Config, format gitcli.ObjectFormat) error {
	if doc.ObjectFormat != format {
		return fmt.Errorf("discovery: state records object format %q, the destination uses %q",
			string(doc.ObjectFormat), string(format))
	}
	if doc.Destination.Repository != cfg.Destination.Repository {
		return fmt.Errorf("discovery: state records repository %q, the profile names %q",
			doc.Destination.Repository, cfg.Destination.Repository)
	}
	if doc.Destination.Module != cfg.Destination.Module {
		return fmt.Errorf("discovery: state records module %q, the profile names %q",
			doc.Destination.Module, cfg.Destination.Module)
	}
	// The anchor commit must be the one the profile configured. A state
	// document anchored elsewhere describes a different slice of upstream
	// history.
	if cfg.Source.Refs.AnchorCommit != "" && doc.Anchor.Source != cfg.Source.Refs.AnchorCommit {
		return fmt.Errorf("discovery: state anchor source %s does not match the configured anchorCommit %s",
			doc.Anchor.Source, cfg.Source.Refs.AnchorCommit)
	}
	// The anchor ref must be the tag of the minimum release the profile names.
	if cfg.Source.Refs.MinimumRelease != "" {
		wantRef := "refs/tags/" + cfg.Source.Refs.MinimumRelease
		if doc.Anchor.Ref != wantRef {
			return fmt.Errorf("discovery: state anchor ref %q does not match refs/tags/%s",
				doc.Anchor.Ref, cfg.Source.Refs.MinimumRelease)
		}
	}
	return nil
}

// assertSameRepository checks that two runners are bound to the same git
// repository by comparing RepositoryRoot after EvalSymlinks.
func assertSameRepository(ctx context.Context, local, remote *gitcli.Runner) error {
	// Use an anonymous, no-lazy-fetch copy of RemoteGit for the root query so
	// the credential never reaches a local command that might trigger a fetch.
	probe := remote.Anonymous().WithNoLazyFetch()
	localRoot, err := local.RepositoryRoot(ctx)
	if err != nil {
		return fmt.Errorf("discovery: local repository root: %w", err)
	}
	remoteRoot, err := probe.RepositoryRoot(ctx)
	if err != nil {
		return fmt.Errorf("discovery: remote repository root: %w", err)
	}
	localReal, err := filepath.EvalSymlinks(localRoot)
	if err != nil {
		return fmt.Errorf("discovery: resolve local repository root: %w", err)
	}
	remoteReal, err := filepath.EvalSymlinks(remoteRoot)
	if err != nil {
		return fmt.Errorf("discovery: resolve remote repository root: %w", err)
	}
	if localReal != remoteReal {
		return fmt.Errorf(
			"discovery: LocalGit and RemoteGit must share a repository root: %q vs %q",
			localReal, remoteReal)
	}
	return nil
}

type branchVerification struct {
	adopted      *AdoptedBranch
	controlPlane *ControlPlaneBranch
}

// verifyDestinationBranch checks the destination's main branch is consistent.
//
// Whenever the remote has the branch, the local HEAD is required to match it.
// This is checked first, before state logic, because a checkout that drifted
// from the remote is wrong regardless of what state says.
//
// When state has a Published branch entry, the remote must match that generated
// object or be a verified linear fast-forward that changes only operator-owned
// paths and carries no source-provenance trailers. The latter is retained as a
// control-plane graft without claiming that the source commit mapped to it.
//
// When state exists but has no Published branch (legacy first state), the branch
// is verified against state cursors: exactly one cursor for the configured source
// ref must exist, its Destination must equal the remote branch, and its Source
// must equal the state anchor source (the minimum release commit). The verified
// branch is returned as an AdoptedBranch.
func verifyDestinationBranch(ctx context.Context, localGit *gitcli.Runner, observed map[string]string, branchRef string, stateDoc state.Document, cfg *config.Config) (branchVerification, error) {
	remoteBranch, hasBranch := observed[branchRef]
	hasState := stateDoc.Schema != 0

	// Whenever the remote has the branch, local HEAD must exist and match.
	if hasBranch {
		if err := requireLocalHEADMatches(ctx, localGit, branchRef, remoteBranch); err != nil {
			return branchVerification{}, err
		}
	}

	if hasState {
		// Look for a Published entry for the branch.
		var publishedBranch *state.Published
		for i := range stateDoc.Published {
			if stateDoc.Published[i].Ref == branchRef {
				publishedBranch = &stateDoc.Published[i]
				break
			}
		}

		if publishedBranch != nil {
			if !hasBranch {
				return branchVerification{}, fmt.Errorf(
					"discovery: state records %s at %s but the destination does not have it",
					branchRef, publishedBranch.Object)
			}
			if publishedBranch.Object == remoteBranch {
				return branchVerification{}, nil
			}

			// A completed replay track is a crashed consumer publication and still
			// requires the tag-and-branch adoption reconciliation.
			track, err := completedTrackAt(stateDoc, remoteBranch)
			if err != nil {
				return branchVerification{}, err
			}
			if track != nil {
				return branchVerification{adopted: &AdoptedBranch{
					Ref: branchRef, Object: remoteBranch, Source: track.Source,
				}}, nil
			}

			controlPlane, err := verifyControlPlaneBranch(ctx, localGit, cfg, branchRef, *publishedBranch, remoteBranch)
			if err != nil {
				return branchVerification{}, err
			}
			return branchVerification{controlPlane: controlPlane}, nil
		}

		// State exists but does not record the branch (legacy). Verify via
		// cursor: exactly one cursor for the configured source ref must exist,
		// its Destination must equal the remote branch, and its Source must
		// equal the state anchor source.
		sourceRef := "refs/tags/" + cfg.Source.Refs.MinimumRelease
		var matchingCursor *state.Cursor
		for i := range stateDoc.Cursors {
			if stateDoc.Cursors[i].Ref == sourceRef {
				matchingCursor = &stateDoc.Cursors[i]
				break
			}
		}
		if matchingCursor == nil {
			return branchVerification{}, fmt.Errorf(
				"discovery: state has no Published entry for %s and no cursor for %s",
				branchRef, sourceRef)
		}
		if !hasBranch {
			return branchVerification{}, fmt.Errorf(
				"discovery: state cursor for %s records destination %s but the remote has no %s",
				sourceRef, matchingCursor.Destination, branchRef)
		}
		if matchingCursor.Destination != remoteBranch {
			return branchVerification{}, fmt.Errorf(
				"discovery: state cursor for %s records destination %s but remote %s is at %s",
				sourceRef, matchingCursor.Destination, branchRef, remoteBranch)
		}
		// The cursor's source must be the anchor source (minimum release commit).
		if matchingCursor.Source != stateDoc.Anchor.Source {
			return branchVerification{}, fmt.Errorf(
				"discovery: state cursor for %s has source %s, want the anchor source %s",
				sourceRef, matchingCursor.Source, stateDoc.Anchor.Source)
		}
		return branchVerification{adopted: &AdoptedBranch{
			Ref:    branchRef,
			Object: remoteBranch,
			Source: matchingCursor.Source,
		}}, nil
	}

	return branchVerification{}, nil
}

// verifyControlPlaneBranch proves that a branch advance layered operator-owned
// commits over the generated commit state records, without changing any
// generated path or introducing source-provenance commits into destination
// history. The advance is retained as a graft point rather than written into
// state as another image of the same source commit.
func verifyControlPlaneBranch(ctx context.Context, git *gitcli.Runner, cfg *config.Config, branchRef string, published state.Published, remoteBranch string) (*ControlPlaneBranch, error) {
	descends, err := git.IsAncestor(ctx, published.Object, remoteBranch)
	if err != nil {
		return nil, fmt.Errorf("discovery: verify control-plane branch ancestry: %w", err)
	}
	if !descends {
		return nil, fmt.Errorf(
			"discovery: the destination's %s is %s, which does not descend from state object %s",
			branchRef, remoteBranch, published.Object)
	}

	commits, err := git.CommitLog(ctx, gitcli.CommitLogOptions{
		Include: []string{remoteBranch}, Exclude: []string{published.Object},
	})
	if err != nil {
		return nil, fmt.Errorf("discovery: read control-plane branch advance: %w", err)
	}
	if len(commits) == 0 {
		return nil, errors.New("discovery: control-plane branch advance contains no commits")
	}
	for _, commit := range commits {
		if len(commit.Parents) != 1 {
			return nil, fmt.Errorf(
				"discovery: control-plane commit %s has %d parents, want a linear fast-forward",
				commit.SHA, len(commit.Parents))
		}
		for _, trailer := range commit.Trailers {
			if strings.EqualFold(trailer.Key, cfg.Commit.TrailerKey) {
				return nil, fmt.Errorf(
					"discovery: control-plane commit %s carries source-provenance trailer %s",
					commit.SHA, trailer.Key)
			}
		}
		changed, err := git.ChangedPaths(ctx, commit.Parents[0], commit.SHA)
		if err != nil {
			return nil, fmt.Errorf("discovery: inspect control-plane commit %s: %w", commit.SHA, err)
		}
		for _, path := range changed {
			if generatedOwnedPath(cfg, path) {
				return nil, fmt.Errorf(
					"discovery: control-plane commit %s changes generated path %s",
					commit.SHA, path)
			}
		}
	}

	files, err := git.ListTree(ctx, remoteBranch)
	if err != nil {
		return nil, fmt.Errorf("discovery: read control-plane branch tree: %w", err)
	}
	if err := checkControlPlane(files); err != nil {
		return nil, fmt.Errorf("discovery: %w", err)
	}
	return &ControlPlaneBranch{
		Ref: branchRef, Base: published.Object, Object: remoteBranch, Source: published.Source,
	}, nil
}

// requireLocalHEADMatches requires that the local HEAD exists and matches the
// remote branch. A missing HEAD when the remote has the branch is fatal: the
// local checkout is in an inconsistent state.
func completedTrackAt(doc state.Document, destination string) (*state.Track, error) {
	var found *state.Track
	for i := range doc.Tracks {
		track := &doc.Tracks[i]
		if track.Done != track.Total || track.Destination != destination {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("discovery: completed tracks %s and %s both claim destination %s", found.Name, track.Name, destination)
		}
		copy := *track
		found = &copy
	}
	return found, nil
}

func requireLocalHEADMatches(ctx context.Context, localGit *gitcli.Runner, branchRef, remoteBranch string) error {
	hasHead, err := localGit.HasHead(ctx)
	if err != nil {
		return fmt.Errorf("discovery: check local HEAD: %w", err)
	}
	if !hasHead {
		return fmt.Errorf(
			"discovery: the destination has %s at %s but the local repository has no HEAD",
			branchRef, remoteBranch)
	}
	localHead, err := localGit.ResolveCommit(ctx, "HEAD")
	if err != nil {
		return fmt.Errorf("discovery: resolve local HEAD: %w", err)
	}
	if localHead != remoteBranch {
		return fmt.Errorf(
			"discovery: the destination's %s is %s but the local HEAD is %s",
			branchRef, remoteBranch, localHead)
	}
	return nil
}

// computePending determines the ordered set of releases that have not yet been
// published to the destination, enforcing fail-closed tag immutability.
//
// Every remote consumer tag must have a matching state Published entry; every
// state tag must exist on the remote at the same object. The one exception is
// the profile's configured firstTag, which may exist on the remote without a
// state entry if it passes legacy adoption checks: it must be an annotated tag
// whose peeled commit is consistent with state and carries the correct
// provenance trailer.
//
// The pending list preserves the semver order DiscoverReleases produced.
func computePending(ctx context.Context, releases []source.Release, stateDoc state.Document, observed map[string]string, opts DiscoverOptions, format gitcli.ObjectFormat) ([]PendingRelease, *AdoptedTag, error) {
	// Index state's published tags.
	stateTagsByRef := make(map[string]state.Published, len(stateDoc.Published))
	for _, pub := range stateDoc.Published {
		if pub.Kind == state.KindTag {
			stateTagsByRef[pub.Ref] = pub
		}
	}

	firstTagRef := ""
	if opts.Config.Release.FirstTag != "" {
		firstTagRef = "refs/tags/" + opts.Config.Release.FirstTag
	}

	var adopted *AdoptedTag
	pending := make([]PendingRelease, 0, len(releases))
	for _, rel := range releases {
		tagRef := "refs/tags/" + rel.DestinationTag
		remoteOID, tagExists := observed[tagRef]
		pub, stateKnows := stateTagsByRef[tagRef]

		switch {
		case tagExists && stateKnows:
			// Both remote and state have it. They must agree on the object.
			if remoteOID != pub.Object {
				return nil, nil, fmt.Errorf(
					"discovery: destination tag %s is %s but state records %s: %w",
					tagRef, remoteOID, pub.Object, state.ErrTagMoved)
			}
			// Verify the state's recorded source matches the discovered release.
			if pub.Source != rel.Source.Commit {
				return nil, nil, fmt.Errorf(
					"discovery: state records tag %s from source %s but the discovered release is at %s",
					tagRef, pub.Source, rel.Source.Commit)
			}
			continue

		case tagExists && !stateKnows:
			// A consumer push can succeed after the complete checkpoint but before
			// its observed-state reconciliation. Recover that crash window only
			// through the exact completed track; the legacy first tag remains the
			// one other adoption path.
			if track := completedTrackForRelease(stateDoc, rel); track != nil {
				tracked, err := adoptTrackedTag(ctx, opts, rel, tagRef, remoteOID, *track, observed, format)
				if err != nil {
					return nil, nil, err
				}
				adopted = tracked
				continue
			}
			if tagRef != firstTagRef {
				return nil, nil, fmt.Errorf(
					"discovery: destination tag %s exists at %s but is not recorded in state: "+
						"only the configured firstTag %q or a complete track may be adopted",
					tagRef, remoteOID, opts.Config.Release.FirstTag)
			}
			legacyAdopted, err := adoptLegacyTag(ctx, opts, rel, tagRef, remoteOID, stateDoc, format)
			if err != nil {
				return nil, nil, err
			}
			adopted = legacyAdopted
			continue

		case !tagExists && stateKnows:
			// State says it was published but the remote doesn't have it.
			return nil, nil, fmt.Errorf(
				"discovery: state records tag %s at %s but the destination does not have it: "+
					"the tag was deleted after publication",
				tagRef, pub.Object)

		default:
			// Neither remote nor state have it — pending.
			pending = append(pending, PendingRelease{
				Source:         rel,
				DestinationTag: rel.DestinationTag,
			})
		}
	}

	// Every state-recorded tag must still exist on the remote at the same object.
	for tagRef, pub := range stateTagsByRef {
		remoteOID, exists := observed[tagRef]
		if !exists {
			return nil, nil, fmt.Errorf(
				"discovery: state records tag %s at %s but the destination does not have it: "+
					"the tag was deleted after publication",
				tagRef, pub.Object)
		}
		if remoteOID != pub.Object {
			return nil, nil, fmt.Errorf(
				"discovery: destination tag %s is %s but state records %s: %w",
				tagRef, remoteOID, pub.Object, state.ErrTagMoved)
		}
	}

	// Every remote consumer tag (refs/tags/) must either be in state or be the
	// adopted firstTag. Unknown remote tags are fatal regardless of whether
	// state exists.
	for ref := range observed {
		if !strings.HasPrefix(ref, "refs/tags/") {
			continue
		}
		if _, inState := stateTagsByRef[ref]; inState {
			continue
		}
		if adopted != nil && ref == adopted.Ref {
			continue
		}
		return nil, nil, fmt.Errorf(
			"discovery: destination tag %s exists at %s but is not recorded in state",
			ref, observed[ref])
	}

	// pending preserves the semver order DiscoverReleases produced.
	return pending, adopted, nil
}

// adoptLegacyTag verifies and adopts a legacy tag that exists on the remote
// without a state entry. Only the profile's firstTag may be adopted.
//
// The adoption checks:
//  1. FetchExact the tag from the remote.
//  2. Read the tag object by OID (must be an annotated tag, not lightweight).
//  3. The internal tag name must equal the expected destination tag.
//  4. The target type must be "commit".
//  5. The tag message must carry exactly the release source metadata keys:
//     Source-tag, Source-commit, and Source-release, matching the discovered
//     release and the configured source URL.
//  6. The target commit must be consistent with the state: it must equal the
//     state's branch destination or be a one-parent child of it.
//  7. The target commit must carry exactly the configured provenance trailer
//     key, and the trailer value must be the discovered release's source commit.
func completedTrackForRelease(doc state.Document, rel source.Release) *state.Track {
	for i := range doc.Tracks {
		track := &doc.Tracks[i]
		if track.Name == rel.DestinationTag && track.Source == rel.Source.Commit && track.Done == track.Total {
			copy := *track
			return &copy
		}
	}
	return nil
}

func adoptTrackedTag(ctx context.Context, opts DiscoverOptions, rel source.Release, tagRef, remoteOID string, track state.Track, observed map[string]string, format gitcli.ObjectFormat) (*AdoptedTag, error) {
	if observed[track.Ref] != track.Destination {
		return nil, fmt.Errorf("discovery: completed track %s records %s at %s, destination advertises %s", track.Name, track.Destination, track.Ref, observed[track.Ref])
	}
	branchRef := "refs/heads/" + opts.Config.Destination.Branch
	if observed[branchRef] != track.Destination {
		return nil, fmt.Errorf("discovery: completed track %s ends at %s but %s is %s", track.Name, track.Destination, branchRef, observed[branchRef])
	}
	fetchGit := opts.RemoteGit
	if fetchGit == nil {
		fetchGit = opts.LocalGit
	}
	if err := fetchGit.FetchExact(ctx, opts.Remote, tagRef, remoteOID, format.HexLength()); err != nil {
		return nil, fmt.Errorf("discovery: adopt tracked tag %s: fetch: %w", tagRef, err)
	}
	tagObj, err := opts.LocalGit.TagObjectByOID(ctx, remoteOID)
	if err != nil {
		return nil, fmt.Errorf("discovery: adopt tracked tag %s: %w", tagRef, err)
	}
	expectedName := rel.DestinationTag
	if tagObj.InternalName != expectedName || tagObj.TargetType != "commit" || tagObj.TargetOID != track.Destination {
		return nil, fmt.Errorf("discovery: adopt tracked tag %s: object names %s %s at %s, want tag %s commit %s", tagRef, tagObj.InternalName, tagObj.TargetType, tagObj.TargetOID, expectedName, track.Destination)
	}
	if tagObj.Tagger.Name != opts.Config.Commit.Committer.Name || tagObj.Tagger.Email != opts.Config.Commit.Committer.Email {
		return nil, fmt.Errorf("discovery: adopt tracked tag %s: tagger %q <%s> does not match configured committer %q <%s>", tagRef, tagObj.Tagger.Name, tagObj.Tagger.Email, opts.Config.Commit.Committer.Name, opts.Config.Commit.Committer.Email)
	}
	sourceURL, err := releaseURL(opts.Config.Source.Repository, rel.Source.Name)
	if err != nil {
		return nil, fmt.Errorf("discovery: adopt tracked tag %s: %w", tagRef, err)
	}
	expectedMessage := rel.DestinationTag + "\n\n" +
		release.SourceTagKey + ": " + rel.Source.Name + "\n" +
		release.SourceCommitKey + ": " + rel.Source.Commit + "\n" +
		release.SourceReleaseKey + ": " + sourceURL + "\n"
	if tagObj.Message != expectedMessage {
		return nil, fmt.Errorf("discovery: adopt tracked tag %s: message does not match expected format", tagRef)
	}
	commit, err := opts.LocalGit.CommitInfo(ctx, tagObj.TargetOID)
	if err != nil {
		return nil, fmt.Errorf("discovery: adopt tracked tag %s: read commit: %w", tagRef, err)
	}
	values := commit.TrailerValues(opts.Config.Commit.TrailerKey)
	if len(values) != 1 || values[0] != rel.Source.Commit {
		return nil, fmt.Errorf("discovery: adopt tracked tag %s: commit %s has provenance %v, want exactly %s", tagRef, tagObj.TargetOID, values, rel.Source.Commit)
	}
	return &AdoptedTag{
		Ref: tagRef, Tag: expectedName, Object: remoteOID,
		Commit: tagObj.TargetOID, Source: rel.Source.Commit,
	}, nil
}

func adoptLegacyTag(ctx context.Context, opts DiscoverOptions, rel source.Release, tagRef, remoteOID string, stateDoc state.Document, format gitcli.ObjectFormat) (*AdoptedTag, error) {
	fetchGit := opts.RemoteGit
	if fetchGit == nil {
		fetchGit = opts.LocalGit
	}
	if err := fetchGit.FetchExact(ctx, opts.Remote, tagRef, remoteOID, format.HexLength()); err != nil {
		return nil, fmt.Errorf("discovery: adopt legacy tag %s: fetch: %w", tagRef, err)
	}

	// Read the full tag object by OID.
	tagObj, err := opts.LocalGit.TagObjectByOID(ctx, remoteOID)
	if err != nil {
		return nil, fmt.Errorf("discovery: adopt legacy tag %s: %w", tagRef, err)
	}

	// The internal tag name must match the destination tag.
	expectedName := strings.TrimPrefix(tagRef, "refs/tags/")
	if tagObj.InternalName != expectedName {
		return nil, fmt.Errorf(
			"discovery: adopt legacy tag %s: internal name %q does not match expected %q",
			tagRef, tagObj.InternalName, expectedName)
	}

	// The target type must be commit.
	if tagObj.TargetType != "commit" {
		return nil, fmt.Errorf(
			"discovery: adopt legacy tag %s: target type %q, want commit",
			tagRef, tagObj.TargetType)
	}

	// Read the target commit before validating the tagger. A profile migration
	// may intentionally change the configured committer; an existing tag remains
	// adoptable only when its tagger matches either the current configuration or
	// the actual committer of the immutable target commit.
	tagCommit := tagObj.TargetOID
	commit, err := opts.LocalGit.CommitInfo(ctx, tagCommit)
	if err != nil {
		return nil, fmt.Errorf("discovery: adopt legacy tag %s: read commit: %w", tagRef, err)
	}
	matchesConfigured := tagObj.Tagger.Name == opts.Config.Commit.Committer.Name &&
		tagObj.Tagger.Email == opts.Config.Commit.Committer.Email
	matchesTarget := tagObj.Tagger.Name == commit.CommitterName &&
		tagObj.Tagger.Email == commit.CommitterEmail
	if !matchesConfigured && !matchesTarget {
		return nil, fmt.Errorf(
			"discovery: adopt legacy tag %s: tagger %q <%s> matches neither configured committer %q <%s> nor target committer %q <%s>",
			tagRef, tagObj.Tagger.Name, tagObj.Tagger.Email,
			opts.Config.Commit.Committer.Name, opts.Config.Commit.Committer.Email,
			commit.CommitterName, commit.CommitterEmail)
	}

	// Verify the tag message is exactly the expected release format.
	sourceURL, err := releaseURL(opts.Config.Source.Repository, rel.Source.Name)
	if err != nil {
		return nil, fmt.Errorf("discovery: adopt legacy tag %s: %w", tagRef, err)
	}
	expectedMessage := rel.DestinationTag + "\n\n" +
		release.SourceTagKey + ": " + rel.Source.Name + "\n" +
		release.SourceCommitKey + ": " + rel.Source.Commit + "\n" +
		release.SourceReleaseKey + ": " + sourceURL + "\n"
	if tagObj.Message != expectedMessage {
		return nil, fmt.Errorf(
			"discovery: adopt legacy tag %s: message does not match expected format:\n got:  %q\n want: %q",
			tagRef, tagObj.Message, expectedMessage)
	}

	// Verify consistency with state. Legacy adoption is permitted only when an
	// existing state record anchors the destination branch.
	if stateDoc.Schema == 0 {
		return nil, fmt.Errorf("discovery: adopt legacy tag %s: no state record anchors the destination", tagRef)
	}
	branchRef := "refs/heads/" + opts.Config.Destination.Branch
	var stateBranchObject string
	for _, pub := range stateDoc.Published {
		if pub.Ref == branchRef && pub.Kind == state.KindBranch {
			stateBranchObject = pub.Object
			break
		}
	}
	if stateBranchObject == "" {
		cursorRef := "refs/tags/" + opts.Config.Source.Refs.MinimumRelease
		for _, cursor := range stateDoc.Cursors {
			if cursor.Ref == cursorRef && cursor.Source == stateDoc.Anchor.Source {
				stateBranchObject = cursor.Destination
				break
			}
		}
	}
	if stateBranchObject == "" {
		return nil, fmt.Errorf("discovery: adopt legacy tag %s: state records no verified destination branch object", tagRef)
	}
	if tagCommit != stateBranchObject {
		parents, err := opts.LocalGit.CommitParents(ctx, tagCommit)
		if err != nil {
			return nil, fmt.Errorf("discovery: adopt legacy tag %s: read parents: %w", tagRef, err)
		}
		if len(parents) != 1 || parents[0] != stateBranchObject {
			return nil, fmt.Errorf(
				"discovery: adopt legacy tag %s: commit %s is not the state branch destination %s and not a one-parent child of it",
				tagRef, tagCommit, stateBranchObject)
		}
	}

	// Verify the provenance trailer on the commit.
	trailerKey := opts.Config.Commit.TrailerKey
	if trailerKey == "" {
		return nil, fmt.Errorf("discovery: adopt legacy tag %s: no provenance trailer key configured", tagRef)
	}
	trailerValues := commit.TrailerValues(trailerKey)
	if len(trailerValues) != 1 {
		return nil, fmt.Errorf("discovery: adopt legacy tag %s: commit %s has %d %s trailers, want exactly 1",
			tagRef, tagCommit, len(trailerValues), trailerKey)
	}
	sourceCommit := trailerValues[0]
	if sourceCommit != rel.Source.Commit {
		return nil, fmt.Errorf(
			"discovery: adopt legacy tag %s: commit %s carries %s: %s but the discovered release is at %s",
			tagRef, tagCommit, trailerKey, sourceCommit, rel.Source.Commit)
	}

	return &AdoptedTag{
		Ref:    tagRef,
		Tag:    expectedName,
		Object: remoteOID,
		Commit: tagCommit,
		Source: sourceCommit,
	}, nil
}
