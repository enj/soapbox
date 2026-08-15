package sync

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/enj/soapbox/tools/internal/config"
	"github.com/enj/soapbox/tools/internal/extract"
	"github.com/enj/soapbox/tools/internal/generate"
	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/gomodmap"
	"github.com/enj/soapbox/tools/internal/publish"
	"github.com/enj/soapbox/tools/internal/release"
	"github.com/enj/soapbox/tools/internal/replay"
	"github.com/enj/soapbox/tools/internal/source"
	"github.com/enj/soapbox/tools/internal/state"
)

// FinalizeOptions describes the consumer publication of one completed track.
type FinalizeOptions struct {
	Config      *config.Config
	Discovery   *Discovery
	SourceCache *source.Cache
	Destination Destination
	Generate    generate.Options
	Release     source.Release
}

type trustedFinalize struct {
	config      *config.Config
	sourceCache *source.Cache
	destination Destination
	release     source.Release
}

// TrustedApplyResult reports the consumer publication and its post-observation
// state reconciliation separately. A crash between them is recovered by
// PlanAdoptionReconciliation on the next run.
type TrustedApplyResult struct {
	Publication    *ApplyResult
	Reconciliation *publish.Result
	State          state.Record
}

// ReconciliationResult is an adoption-only state plan after a consumer push was
// observed through a completed track.
type ReconciliationResult struct {
	Document state.Document
	State    state.Record
	Publish  *publish.Plan

	publisher *publish.Publisher
}

// PlanFinal projects the immutable destination tag and returns a normal
// synchronization Result, so manual apply still approves its exact manifest and
// trusted apply self-approves that same in-memory hash.
func PlanFinal(ctx context.Context, opts FinalizeOptions) (*Result, error) {
	cfg, err := cloneFinalizeConfig(opts.Config)
	if err != nil {
		return nil, err
	}
	opts.Config = cfg
	track, err := completedFinalizeTrack(opts)
	if err != nil {
		return nil, err
	}
	if err := opts.Destination.Git.FetchExact(ctx, opts.Destination.Remote, track.Ref, track.Destination, opts.Discovery.Format.HexLength()); err != nil {
		return nil, fmt.Errorf("finalize: fetch completed progress: %w", err)
	}
	if opts.Discovery.Adopted != nil || opts.Discovery.AdoptedBranch != nil {
		return nil, errors.New("finalize: consumer refs are already present and require adoption reconciliation")
	}
	tagRef := "refs/tags/" + opts.Release.DestinationTag
	if object := opts.Discovery.Observed[tagRef]; object != "" {
		return nil, fmt.Errorf("finalize: destination tag %s already exists at %s", tagRef, object)
	}

	store, err := gomodmap.NewStore(opts.Generate.StorePath)
	if err != nil {
		return nil, fmt.Errorf("finalize: %w", err)
	}
	if err := restoreChunkMapping(ctx, ChunkOptions{
		Config: opts.Config, Discovery: opts.Discovery,
		SourceCache: opts.SourceCache, Destination: opts.Destination,
	}, store); err != nil {
		return nil, fmt.Errorf("finalize: %w", err)
	}

	generationOptions := opts.Generate
	generationOptions.Config = opts.Config
	generationOptions.Ref = extract.Ref{Kind: extract.RefTag, Name: opts.Release.Source.Name}
	generationOptions.ReleaseContext = ""
	generationOptions.HistoryAnchor = ""
	generationOptions.HistoryAnchorRelease = ""
	generationOptions.StagingSources = nil
	generationOptions.Fetch = false
	generationOptions.Materialize = false
	generated, err := generate.Generate(ctx, generationOptions)
	if err != nil {
		return nil, fmt.Errorf("finalize: %w", err)
	}
	if generated.Report.Source.Commit != opts.Release.Source.Commit || generated.Report.Source.ReleaseTag != opts.Release.DestinationTag {
		return nil, fmt.Errorf("finalize: generation describes %s -> %s, want %s -> %s", generated.Report.Source.Commit, generated.Report.Source.ReleaseTag, opts.Release.Source.Commit, opts.Release.DestinationTag)
	}

	manifest, err := composeModuleTree(ctx, opts.Destination.Git, opts.Config, track.Destination, generated.Files)
	if err != nil {
		return nil, fmt.Errorf("finalize: %w", err)
	}
	headTree, err := opts.Destination.Git.ResolveTree(ctx, track.Destination)
	if err != nil {
		return nil, fmt.Errorf("finalize: resolve completed track tree: %w", err)
	}
	if manifest.Tree != headTree {
		return nil, fmt.Errorf("finalize: completed track tree %s does not match regenerated release tree %s", headTree, manifest.Tree)
	}

	upstream, err := readRelease(ctx, opts.SourceCache.Git(), opts.SourceCache.Path(), opts.Config, opts.Release.Source.Name)
	if err != nil {
		return nil, fmt.Errorf("finalize: %w", err)
	}
	projected, err := release.Project(ctx, opts.Destination.Git, release.Options{
		Policy: opts.Config.Release.Policy,
		Source: release.Source{
			Tag: upstream.Tag, Commit: upstream.Commit, Tagger: upstream.Tagger, URL: upstream.URL,
		},
		Replay:     release.Replay{Commit: track.Destination, Tree: headTree},
		Projection: headTree,
		Bot: release.Identity{
			Name: opts.Config.Commit.Committer.Name, Email: opts.Config.Commit.Committer.Email,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("finalize: %w", err)
	}
	if projected.Target != track.Destination || projected.Commit != "" {
		return nil, fmt.Errorf("finalize: release projects target %s through commit %s, want completed track %s directly", projected.Target, projected.Commit, track.Destination)
	}

	stored, err := state.Inspect(ctx, opts.Destination.Git, opts.Discovery.StateCommit)
	if err != nil {
		return nil, fmt.Errorf("finalize: %w", err)
	}
	publisher, _, err := publisherForDestination(ctx, opts.Destination.Git, opts.Destination, opts.Config, opts.Discovery.Format)
	if err != nil {
		return nil, fmt.Errorf("finalize: %w", err)
	}
	branchRef := "refs/heads/" + opts.Config.Destination.Branch
	updates := []publish.Update{
		{
			Ref: opts.Config.Destination.StateRef, Kind: publish.KindState,
			NewObject: opts.Discovery.StateCommit, ExpectedOld: opts.Discovery.StateCommit,
			Evidence: "state:complete:" + track.Name,
		},
		{
			Ref: branchRef, Kind: publish.KindBranch, NewObject: track.Destination,
			ExpectedOld: opts.Discovery.Observed[branchRef], Evidence: "replay:" + opts.Release.Source.Ref,
		},
		{
			Ref: tagRef, Kind: publish.KindTag, NewObject: projected.Object,
			ExpectAbsent: true, Evidence: "release:" + opts.Release.Source.Name,
		},
	}
	plan, err := publisher.Plan(ctx, updates)
	if err != nil {
		return nil, fmt.Errorf("finalize: %w", err)
	}

	r := &run{
		opts: ProjectOptions{
			Config:  opts.Config,
			Module:  Module{Files: generated.Files, Report: generated.Report},
			Release: upstream, Destination: opts.Destination,
		},
		git: opts.Destination.Git, format: opts.Discovery.Format,
		publisher: publisher, observed: opts.Discovery.Observed,
		result: Result{
			Generation: generated, Tree: manifest,
			Replay:  &replay.Result{Heads: []replay.Head{{Source: track.Source, Destination: track.Destination}}},
			Release: projected, Document: opts.Discovery.State, State: stored, Publish: plan,
		},
	}
	syncManifest, err := r.manifest()
	if err != nil {
		return nil, fmt.Errorf("finalize: %w", err)
	}
	r.result.Manifest = syncManifest
	r.result.publisher = publisher
	r.result.trusted = &trustedFinalize{
		config: opts.Config, sourceCache: opts.SourceCache,
		destination: opts.Destination, release: opts.Release,
	}
	return &r.result, nil
}

// ApplyTrusted self-approves a PlanFinal result, then reconciles the consumer
// refs it observed after the state-first/consumer-scoped apply.
func ApplyTrusted(ctx context.Context, result *Result) (*TrustedApplyResult, error) {
	if result == nil || result.trusted == nil {
		return nil, errors.New("trusted finalize apply: a trusted finalization plan is required")
	}
	if result.trusted.config.Publication.Mode != config.PublicationModeAutomatic {
		return nil, fmt.Errorf("trusted finalize apply: publication mode is %q, want %q", result.trusted.config.Publication.Mode, config.PublicationModeAutomatic)
	}
	publication, err := Apply(ctx, result, ApplyOptions{Approval: result.Manifest.Hash})
	applied := &TrustedApplyResult{Publication: publication}
	if err != nil {
		return applied, err
	}
	observed, err := destinationRefMap(ctx, result.trusted.destination, result.trusted.config, result.Manifest.Objects.Format)
	if err != nil {
		return applied, fmt.Errorf("trusted finalize apply: post-publication read: %w", err)
	}
	branchRef := "refs/heads/" + result.trusted.config.Destination.Branch
	tagRef := "refs/tags/" + result.trusted.release.DestinationTag
	if observed[branchRef] != result.Manifest.Objects.Commit || observed[tagRef] != result.Manifest.Objects.Tag {
		return applied, fmt.Errorf("trusted finalize apply: consumer refs are branch %s tag %s, want %s and %s", observed[branchRef], observed[tagRef], result.Manifest.Objects.Commit, result.Manifest.Objects.Tag)
	}

	reconciliation, err := planObservedReconciliation(ctx, reconciliationOptions{
		Config: result.trusted.config, SourceCache: result.trusted.sourceCache,
		Destination: result.trusted.destination, Release: result.trusted.release,
		Document: result.Document, State: result.State, Observed: observed,
		ObservedBranchObject:  result.Manifest.Objects.Commit,
		GeneratedBranchObject: result.Manifest.Objects.Commit,
		TagObject:             result.Manifest.Objects.Tag,
	})
	if err != nil {
		return applied, fmt.Errorf("trusted finalize apply: %w", err)
	}
	reconciled, err := ApplyReconciliation(ctx, reconciliation, reconciliation.Publish.Hash())
	applied.Reconciliation, applied.State = reconciled, reconciliation.State
	if err != nil {
		return applied, err
	}
	post, err := destinationRefMap(ctx, result.trusted.destination, result.trusted.config, result.Manifest.Objects.Format)
	if err != nil {
		return applied, fmt.Errorf("trusted finalize apply: post-reconciliation read: %w", err)
	}
	if post[result.trusted.config.Destination.StateRef] != reconciliation.State.Commit || post[branchRef] != result.Manifest.Objects.Commit || post[tagRef] != result.Manifest.Objects.Tag {
		return applied, errors.New("trusted finalize apply: post-reconciliation refs do not match the applied plan")
	}
	return applied, nil
}

// PlanAdoptionReconciliation closes the crash window in which consumer refs
// landed but their observed-state successor did not.
func PlanAdoptionReconciliation(ctx context.Context, opts FinalizeOptions) (*ReconciliationResult, error) {
	switch {
	case opts.Config == nil:
		return nil, errors.New("adoption reconciliation: a profile is required")
	case opts.Discovery == nil:
		return nil, errors.New("adoption reconciliation: discovery is required")
	case opts.SourceCache == nil:
		return nil, errors.New("adoption reconciliation: a source cache is required")
	case opts.Destination.Git == nil:
		return nil, errors.New("adoption reconciliation: a destination Git runner is required")
	case opts.Discovery.Adopted == nil:
		return nil, errors.New("adoption reconciliation: an adopted consumer tag is required")
	}
	branchRef := "refs/heads/" + opts.Config.Destination.Branch
	observedBranch := opts.Discovery.Observed[branchRef]
	generatedBranch := observedBranch
	switch {
	case opts.Discovery.ControlPlaneBranch != nil:
		control := opts.Discovery.ControlPlaneBranch
		if control.Ref != branchRef || control.Object != observedBranch {
			return nil, errors.New("adoption reconciliation: control-plane branch does not match the observed consumer branch")
		}
		generatedBranch = control.Base
	case opts.Discovery.AdoptedBranch != nil:
		generatedBranch = opts.Discovery.AdoptedBranch.Object
	}
	if observedBranch == "" || generatedBranch == "" {
		return nil, errors.New("adoption reconciliation: the consumer branch is absent")
	}
	stored, err := state.Inspect(ctx, opts.Destination.Git, opts.Discovery.StateCommit)
	if err != nil {
		return nil, fmt.Errorf("adoption reconciliation: %w", err)
	}
	return planObservedReconciliation(ctx, reconciliationOptions{
		Config: opts.Config, SourceCache: opts.SourceCache, Destination: opts.Destination,
		Release: opts.Release, Document: opts.Discovery.State, State: stored,
		Observed:              opts.Discovery.Observed,
		ObservedBranchObject:  observedBranch,
		GeneratedBranchObject: generatedBranch,
		TagObject:             opts.Discovery.Adopted.Object,
		AllowLegacy:           true,
	})
}

// ApplyReconciliation applies only the state update of an exact-hash approved
// adoption plan.
func ApplyReconciliation(ctx context.Context, result *ReconciliationResult, approval string) (*publish.Result, error) {
	if result == nil || result.publisher == nil || result.Publish == nil {
		return nil, errors.New("reconciliation apply: a bound plan is required")
	}
	applied, err := result.publisher.Apply(ctx, result.Publish, publish.ApplyOptions{
		Approval: approval, Scope: publish.ScopeReconcile,
	})
	if err != nil {
		return applied, fmt.Errorf("reconciliation apply: %w", err)
	}
	return applied, nil
}

type reconciliationOptions struct {
	Config                *config.Config
	SourceCache           *source.Cache
	Destination           Destination
	Release               source.Release
	Document              state.Document
	State                 state.Record
	Observed              map[string]string
	ObservedBranchObject  string
	GeneratedBranchObject string
	TagObject             string
	AllowLegacy           bool
}

func planObservedReconciliation(ctx context.Context, opts reconciliationOptions) (*ReconciliationResult, error) {
	branchRef := "refs/heads/" + opts.Config.Destination.Branch
	tagRef := "refs/tags/" + opts.Release.DestinationTag
	if opts.Observed[branchRef] != opts.ObservedBranchObject || opts.Observed[tagRef] != opts.TagObject {
		return nil, errors.New("reconciliation: observed consumer refs do not match the proposed state")
	}
	track := completedTrackForRelease(opts.Document, opts.Release)
	if track != nil {
		if track.Destination != opts.GeneratedBranchObject {
			return nil, errors.New("reconciliation: completed track does not prove the observed consumer branch")
		}
		if opts.Observed[track.Ref] != track.Destination {
			return nil, errors.New("reconciliation: observed progress ref does not match the completed track")
		}
	} else if !opts.AllowLegacy || !legacyBranchProvesRelease(opts.Document, branchRef, opts.Release, opts.GeneratedBranchObject) {
		return nil, errors.New("reconciliation: no completed track or legacy state proves the observed consumer branch")
	}

	next := opts.Document.Clone()
	next.Digest = ""
	cursorRef := opts.Release.Source.Ref
	cursor := state.Cursor{Ref: cursorRef, Source: opts.Release.Source.Commit, Destination: opts.GeneratedBranchObject}
	cursorUpdated := false
	for i := range next.Cursors {
		if next.Cursors[i].Ref == cursorRef {
			next.Cursors[i] = cursor
			cursorUpdated = true
			break
		}
	}
	if !cursorUpdated {
		next.Cursors = append(next.Cursors, cursor)
	}
	published := make([]state.Published, 0, len(next.Published)+1)
	branchUpdated, tagUpdated := false, false
	for _, entry := range next.Published {
		switch entry.Ref {
		case branchRef:
			published = append(published, state.Published{
				Ref: branchRef, Kind: state.KindBranch,
				Object: opts.GeneratedBranchObject, Source: opts.Release.Source.Commit,
			})
			branchUpdated = true
		case tagRef:
			published = append(published, state.Published{
				Ref: tagRef, Kind: state.KindTag,
				Object: opts.TagObject, Source: opts.Release.Source.Commit,
			})
			tagUpdated = true
		default:
			published = append(published, entry)
		}
	}
	if !branchUpdated {
		published = append(published, state.Published{Ref: branchRef, Kind: state.KindBranch, Object: opts.GeneratedBranchObject, Source: opts.Release.Source.Commit})
	}
	if !tagUpdated {
		published = append(published, state.Published{Ref: tagRef, Kind: state.KindTag, Object: opts.TagObject, Source: opts.Release.Source.Commit})
	}
	next.Published = published
	if track != nil {
		next.Tracks = slices.DeleteFunc(next.Tracks, func(candidate state.Track) bool { return candidate.Name == track.Name })
	}

	merged, err := state.MergeWith(ctx, opts.Document, next, opts.SourceCache.Git().WithNoLazyFetch(), opts.Destination.Git)
	if err != nil {
		return nil, fmt.Errorf("reconciliation: %w", err)
	}
	mapping, _, err := state.LoadMapping(ctx, opts.Destination.Git, opts.State.Commit)
	if errors.Is(err, state.ErrMappingEvidenceMissing) && opts.Document.Mapping.Entries > 0 {
		return nil, fmt.Errorf("reconciliation: state names %d mapping entries but the evidence is unreachable: %w", opts.Document.Mapping.Entries, err)
	}
	if err != nil && !errors.Is(err, state.ErrMappingEvidenceMissing) {
		return nil, fmt.Errorf("reconciliation: load mapping: %w", err)
	}
	metadata, err := opts.SourceCache.Git().WithNoLazyFetch().TagInfo(ctx, opts.Release.Source.Name)
	if err != nil {
		return nil, fmt.Errorf("reconciliation: source tag: %w", err)
	}
	signature := gitcli.Signature{
		Name: opts.Config.Commit.Committer.Name, Email: opts.Config.Commit.Committer.Email,
		Date: metadata.Tagger.Date,
	}
	record, err := state.Store(ctx, opts.Destination.Git, state.StoreOptions{
		Document: merged, Mapping: mapping, Parents: []string{opts.State.Commit},
		Author: signature, Committer: signature,
	})
	if err != nil {
		return nil, fmt.Errorf("reconciliation: %w", err)
	}
	publisher, _, err := publisherForDestination(ctx, opts.Destination.Git, opts.Destination, opts.Config, opts.Document.ObjectFormat)
	if err != nil {
		return nil, fmt.Errorf("reconciliation: %w", err)
	}
	updates := []publish.Update{
		{
			Ref: opts.Config.Destination.StateRef, Kind: publish.KindState,
			NewObject: record.Commit, ExpectedOld: opts.State.Commit,
			Evidence: "state:observe:" + opts.Release.DestinationTag,
		},
		{
			Ref: branchRef, Kind: publish.KindBranch,
			NewObject: opts.ObservedBranchObject, ExpectedOld: opts.ObservedBranchObject,
			Evidence: "observe:branch:" + opts.Release.DestinationTag,
		},
		{
			Ref: tagRef, Kind: publish.KindTag,
			NewObject: opts.TagObject, ExpectedOld: opts.TagObject,
			Evidence: "observe:tag:" + opts.Release.DestinationTag,
		},
	}
	if track != nil {
		updates = append(updates, publish.Update{
			Ref: track.Ref, Kind: publish.KindProgress,
			NewObject: track.Destination, ExpectedOld: track.Destination,
			Evidence: "observe:progress:" + opts.Release.DestinationTag,
		})
	}
	plan, err := publisher.Plan(ctx, updates)
	if err != nil {
		return nil, fmt.Errorf("reconciliation: %w", err)
	}
	return &ReconciliationResult{Document: merged, State: record, Publish: plan, publisher: publisher}, nil
}

func legacyBranchProvesRelease(doc state.Document, branchRef string, rel source.Release, object string) bool {
	for _, entry := range doc.Published {
		if entry.Ref == branchRef && entry.Kind == state.KindBranch && entry.Source == rel.Source.Commit && entry.Object == object {
			return true
		}
	}
	for _, cursor := range doc.Cursors {
		if cursor.Source == rel.Source.Commit && cursor.Destination == object {
			return true
		}
	}
	return false
}

func cloneFinalizeConfig(cfg *config.Config) (*config.Config, error) {
	if cfg == nil {
		return nil, errors.New("finalize: a profile is required")
	}
	encoded, err := cfg.Canonical()
	if err != nil {
		return nil, fmt.Errorf("finalize: encode profile: %w", err)
	}
	cloned, err := config.Decode(encoded)
	if err != nil {
		return nil, fmt.Errorf("finalize: clone profile: %w", err)
	}
	return cloned, nil
}

func completedFinalizeTrack(opts FinalizeOptions) (*state.Track, error) {
	switch {
	case opts.Config == nil:
		return nil, errors.New("finalize: a profile is required")
	case opts.Discovery == nil:
		return nil, errors.New("finalize: discovery is required")
	case opts.SourceCache == nil:
		return nil, errors.New("finalize: a source cache is required")
	case opts.Destination.Git == nil:
		return nil, errors.New("finalize: a destination Git runner is required")
	}
	pending := false
	for _, candidate := range opts.Discovery.Pending {
		if candidate.Source.Source.Commit == opts.Release.Source.Commit && candidate.DestinationTag == opts.Release.DestinationTag {
			pending = true
			break
		}
	}
	if !pending && opts.Discovery.Adopted == nil {
		return nil, errors.New("finalize: release is neither pending nor adopted")
	}
	track := completedTrackForRelease(opts.Discovery.State, opts.Release)
	if track == nil {
		return nil, fmt.Errorf("finalize: release %s has no completed track", opts.Release.DestinationTag)
	}
	if opts.Discovery.Observed[track.Ref] != track.Destination {
		return nil, fmt.Errorf("finalize: progress ref %s is %s, track records %s", track.Ref, opts.Discovery.Observed[track.Ref], track.Destination)
	}
	return track, nil
}

func destinationRefMap(ctx context.Context, dest Destination, cfg *config.Config, format string) (map[string]string, error) {
	objectFormat := gitcli.ObjectFormat(format)
	_, lister, err := publisherForDestination(ctx, dest.Git, dest, cfg, objectFormat)
	if err != nil {
		return nil, err
	}
	refs, err := lister.RemoteRefs(ctx, dest.Remote)
	if err != nil {
		return nil, err
	}
	observed := make(map[string]string, len(refs))
	for _, ref := range refs {
		observed[ref.Name] = ref.Target
	}
	return observed, nil
}
