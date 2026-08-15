package sync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/enj/soapbox/tools/internal/buildinfo"
	"github.com/enj/soapbox/tools/internal/config"
	"github.com/enj/soapbox/tools/internal/generate"
	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/gomodmap"
	"github.com/enj/soapbox/tools/internal/publish"
	"github.com/enj/soapbox/tools/internal/source"
	"github.com/enj/soapbox/tools/internal/state"
)

var ErrWorkflowBudget = errors.New("the workflow budget cannot start another replay chunk")

// WorkflowBudget is an operational deadline. It decides whether another chunk
// starts, never the bytes or object names a started chunk produces.
type WorkflowBudget struct {
	Deadline time.Time
	Reserve  time.Duration
	Now      func() time.Time
}

func (b WorkflowBudget) Check() error {
	if b.Reserve < 0 {
		return fmt.Errorf("workflow budget reserve %s must not be negative", b.Reserve)
	}
	if b.Deadline.IsZero() {
		return nil
	}
	now := time.Now
	if b.Now != nil {
		now = b.Now
	}
	if !now().Add(b.Reserve).Before(b.Deadline) {
		return ErrWorkflowBudget
	}
	return nil
}

// ChunkOptions describes one resumable progress step toward a pending release.
type ChunkOptions struct {
	Config      *config.Config
	Discovery   *Discovery
	SourceCache *source.Cache
	Destination Destination
	Generate    generate.Options
	Release     source.Release
	Budget      WorkflowBudget
}

// ChunkResult holds a non-consumer checkpoint plan. Applying Publish moves only
// the state and progress refs; the consumer branch and release tag remain still.
type ChunkResult struct {
	DAG      *DAGResult
	Track    state.Track
	Document state.Document
	State    state.Record
	Mapping  []byte
	Publish  *publish.Plan
	Complete bool

	publisher *publish.Publisher
}

// PlanChunk projects and records one source-history chunk without moving a ref.
func PlanChunk(ctx context.Context, opts ChunkOptions) (*ChunkResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("replay chunk: %w", err)
	}
	if err := opts.Budget.Check(); err != nil {
		return nil, err
	}
	if err := checkChunkOptions(opts); err != nil {
		return nil, err
	}

	store, err := gomodmap.NewStore(opts.Generate.StorePath)
	if err != nil {
		return nil, fmt.Errorf("replay chunk: %w", err)
	}
	if err := restoreChunkMapping(ctx, opts, store); err != nil {
		return nil, err
	}

	start, err := chunkStart(ctx, opts)
	if err != nil {
		return nil, err
	}
	candidates, total, err := chunkCandidates(ctx, opts.SourceCache.Git().WithNoLazyFetch(), start, opts.Release.Source.Commit, opts.Config.Determinism.ChunkSize)
	if err != nil {
		return nil, err
	}
	if start.track != nil {
		if start.track.Done+total != start.track.Total {
			return nil, fmt.Errorf("replay chunk: track %s has done %d of %d commits, but %d commits remain", start.track.Name, start.track.Done, start.track.Total, total)
		}
		total = start.track.Total
	}

	var dag *DAGResult
	var endpoint string
	for _, candidate := range candidates {
		dag, err = ReplayDAG(ctx, DAGOptions{
			Config: opts.Config, SourceCache: opts.SourceCache, DestinationGit: opts.Destination.Git,
			Generate: opts.Generate, AnchorCommit: start.source, AnchorTag: start.tag,
			HistoryAnchorCommit: start.historySource, HistoryAnchorTag: start.historyTag,
			MappedAnchor: true, EpochParent: start.destination,
			HeadCommit: candidate, Release: opts.Release,
		})
		if err != nil {
			return nil, fmt.Errorf("replay chunk: %w", err)
		}
		endpoint = candidate
		// The first checkpoint must create at least one destination commit. A
		// track sharing the consumer cursor's destination would make two source
		// commits claim one image in state. Irrelevant runs are therefore folded
		// into the next mainline endpoint, even when that exceeds ChunkSize.
		if start.track != nil || dag.Replay.Written > 0 || candidate == opts.Release.Source.Commit {
			break
		}
	}
	if dag == nil || endpoint == "" {
		return nil, errors.New("replay chunk: no checkpoint candidate was projected")
	}
	advanced := len(dag.Replay.Records) - 1
	if advanced <= 0 {
		return nil, errors.New("replay chunk: checkpoint advanced no source commits")
	}
	done := advanced
	if start.track != nil {
		done += start.track.Done
	}
	if done > total {
		return nil, fmt.Errorf("replay chunk: checkpoint advanced to %d of %d commits", done, total)
	}
	if len(dag.Replay.Heads) != 1 || dag.Replay.Heads[0].Destination == "" {
		return nil, errors.New("replay chunk: replay produced no progress destination")
	}

	track := state.Track{
		Name:        opts.Release.DestinationTag,
		Ref:         state.ProgressNamespace + opts.Release.DestinationTag,
		Source:      endpoint,
		Destination: dag.Replay.Heads[0].Destination,
		Done:        done,
		Total:       total,
	}
	mapping, mappingState, err := snapshotChunkMapping(ctx, opts.Destination.Git, store)
	if err != nil {
		return nil, err
	}
	document, err := chunkDocument(ctx, opts, start, dag, track, mappingState)
	if err != nil {
		return nil, err
	}
	metadata, err := opts.SourceCache.Git().WithNoLazyFetch().CommitInfo(ctx, endpoint)
	if err != nil {
		return nil, fmt.Errorf("replay chunk: read checkpoint source: %w", err)
	}
	signature := gitcli.Signature{
		Name: opts.Config.Commit.Committer.Name, Email: opts.Config.Commit.Committer.Email,
		Date: metadata.CommitterDateRaw,
	}
	record, err := state.Store(ctx, opts.Destination.Git, state.StoreOptions{
		Document: document, Mapping: mapping,
		Parents: []string{opts.Discovery.StateCommit}, Author: signature, Committer: signature,
	})
	if err != nil {
		return nil, fmt.Errorf("replay chunk: %w", err)
	}

	publisher, _, err := publisherForDestination(ctx, opts.Destination.Git, opts.Destination, opts.Config, opts.Discovery.Format)
	if err != nil {
		return nil, fmt.Errorf("replay chunk: %w", err)
	}
	updates := []publish.Update{
		{
			Ref: track.Ref, Kind: publish.KindProgress, NewObject: track.Destination,
			Evidence: fmt.Sprintf("chunk:%s:%d/%d", track.Name, track.Done, track.Total),
		},
		{
			Ref: opts.Config.Destination.StateRef, Kind: publish.KindState,
			NewObject: record.Commit, ExpectedOld: opts.Discovery.StateCommit,
			Evidence: "state:chunk:" + track.Name,
		},
	}
	if old, exists := opts.Discovery.Observed[track.Ref]; exists {
		updates[0].ExpectedOld = old
	} else {
		updates[0].ExpectAbsent = true
	}
	plan, err := publisher.Plan(ctx, updates)
	if err != nil {
		return nil, fmt.Errorf("replay chunk: %w", err)
	}

	return &ChunkResult{
		DAG: dag, Track: track, Document: document, State: record,
		Mapping: mapping, Publish: plan,
		Complete:  endpoint == opts.Release.Source.Commit && done == total,
		publisher: publisher,
	}, nil
}

// ApplyCheckpoint applies only the non-consumer scope of an approved chunk plan.
func ApplyCheckpoint(ctx context.Context, result *ChunkResult, approval string, dryRun bool) (*publish.Result, error) {
	if result == nil || result.publisher == nil || result.Publish == nil {
		return nil, errors.New("replay chunk apply: a bound checkpoint plan is required")
	}
	applied, err := result.publisher.Apply(ctx, result.Publish, publish.ApplyOptions{
		Approval: approval, DryRun: dryRun, Scope: publish.ScopeNonConsumer,
	})
	if err != nil {
		return applied, fmt.Errorf("replay chunk apply: %w", err)
	}
	return applied, nil
}

type chunkPosition struct {
	source        string
	destination   string
	tag           string
	historySource string
	historyTag    string
	track         *state.Track
}

func checkChunkOptions(opts ChunkOptions) error {
	switch {
	case opts.Config == nil:
		return errors.New("replay chunk: a profile is required")
	case opts.Discovery == nil:
		return errors.New("replay chunk: discovery is required")
	case opts.SourceCache == nil:
		return errors.New("replay chunk: a source cache is required")
	case opts.Destination.Git == nil:
		return errors.New("replay chunk: a destination Git runner is required")
	case opts.Discovery.StateCommit == "" || opts.Discovery.State.Schema == 0:
		return errors.New("replay chunk: a verified prior state record is required")
	case opts.Release.Source.Commit == "" || opts.Release.DestinationTag == "":
		return errors.New("replay chunk: a pending release is required")
	case opts.Config.Determinism.ChunkSize <= 0:
		return fmt.Errorf("replay chunk: chunk size %d must be positive", opts.Config.Determinism.ChunkSize)
	case opts.Config.Destination.ProgressRefPrefix != state.ProgressNamespace:
		return fmt.Errorf("replay chunk: progress namespace %q must equal state namespace %q", opts.Config.Destination.ProgressRefPrefix, state.ProgressNamespace)
	}
	pending := false
	for _, discovered := range opts.Discovery.Pending {
		if discovered.Source.Source.Commit == opts.Release.Source.Commit &&
			discovered.DestinationTag == opts.Release.DestinationTag {
			pending = true
			break
		}
	}
	if !pending {
		return fmt.Errorf("replay chunk: release %s at %s is not in the verified pending set", opts.Release.Source.Name, opts.Release.Source.Commit)
	}
	return nil
}

func restoreChunkMapping(ctx context.Context, opts ChunkOptions, store *gomodmap.Store) error {
	prior := opts.Discovery.State.Mapping
	if prior.Entries == 0 {
		if err := store.Reset(ctx); err != nil {
			return fmt.Errorf("replay chunk: reset unrecorded mapping cache: %w", err)
		}
		return nil
	}
	data, _, err := state.LoadMapping(ctx, opts.Destination.Git, opts.Discovery.StateCommit)
	if err == nil {
		if err := store.Restore(ctx, data); err != nil {
			return fmt.Errorf("replay chunk: restore state mapping: %w", err)
		}
		return nil
	}
	if !errors.Is(err, state.ErrMappingEvidenceMissing) {
		return fmt.Errorf("replay chunk: load state mapping: %w", err)
	}
	// Legacy state could name a blob without making it reachable. It is usable
	// only when the local cache independently reproduces the exact descriptor.
	local, index, localErr := store.Snapshot(ctx)
	if localErr != nil {
		return fmt.Errorf("replay chunk: legacy state mapping is unreachable and the local cache cannot prove it: %w", localErr)
	}
	descriptor, err := mappingDescriptor(ctx, opts.Destination.Git, local, index)
	if err != nil {
		return err
	}
	if descriptor != prior {
		return fmt.Errorf("replay chunk: local mapping %#v does not match legacy state mapping %#v", descriptor, prior)
	}
	return nil
}

func chunkStart(ctx context.Context, opts ChunkOptions) (chunkPosition, error) {
	base, err := consumerChunkBase(opts)
	if err != nil {
		return chunkPosition{}, err
	}
	name := opts.Release.DestinationTag
	ref := state.ProgressNamespace + name
	for i := range opts.Discovery.State.Tracks {
		track := &opts.Discovery.State.Tracks[i]
		if track.Name != name {
			continue
		}
		if track.Ref != ref {
			return chunkPosition{}, fmt.Errorf("replay chunk: track %s uses ref %s, want %s", name, track.Ref, ref)
		}
		observed, ok := opts.Discovery.Observed[ref]
		if !ok || observed != track.Destination {
			return chunkPosition{}, fmt.Errorf("replay chunk: progress ref %s is %s, state records %s", ref, observed, track.Destination)
		}
		if err := opts.Destination.Git.FetchExact(ctx, opts.Destination.Remote, ref, observed, opts.Discovery.Format.HexLength()); err != nil {
			return chunkPosition{}, fmt.Errorf("replay chunk: fetch progress: %w", err)
		}
		trackCopy := *track
		return chunkPosition{
			source: track.Source, destination: track.Destination,
			historySource: base.source, historyTag: base.tag, track: &trackCopy,
		}, nil
	}
	if object, exists := opts.Discovery.Observed[ref]; exists {
		return chunkPosition{}, fmt.Errorf("replay chunk: untracked progress ref %s exists at %s", ref, object)
	}
	base.historySource, base.historyTag = base.source, base.tag
	return base, nil
}

func consumerChunkBase(opts ChunkOptions) (chunkPosition, error) {
	branchRef := "refs/heads/" + opts.Config.Destination.Branch
	var published *state.Published
	for i := range opts.Discovery.State.Published {
		entry := &opts.Discovery.State.Published[i]
		if entry.Ref == branchRef && entry.Kind == state.KindBranch {
			published = entry
			break
		}
	}

	position := chunkPosition{}
	cursorDestination := ""
	switch {
	case opts.Discovery.ControlPlaneBranch != nil:
		control := opts.Discovery.ControlPlaneBranch
		if published == nil {
			return chunkPosition{}, fmt.Errorf("replay chunk: control-plane branch %s has no generated state base", branchRef)
		}
		if control.Ref != branchRef || control.Base != published.Object || control.Source != published.Source {
			return chunkPosition{}, fmt.Errorf(
				"replay chunk: control-plane branch %#v does not match state branch %#v",
				*control, *published)
		}
		position.source, position.destination = published.Source, control.Object
		cursorDestination = published.Object
	case published != nil:
		position.source, position.destination = published.Source, published.Object
		cursorDestination = published.Object
	case opts.Discovery.AdoptedBranch != nil:
		position.source = opts.Discovery.AdoptedBranch.Source
		position.destination = opts.Discovery.AdoptedBranch.Object
		cursorDestination = opts.Discovery.AdoptedBranch.Object
	default:
		return chunkPosition{}, fmt.Errorf("replay chunk: state records no verified consumer branch %s", branchRef)
	}
	if observed := opts.Discovery.Observed[branchRef]; observed != position.destination {
		return chunkPosition{}, fmt.Errorf("replay chunk: consumer branch %s is %s, expected %s", branchRef, observed, position.destination)
	}
	for _, cursor := range opts.Discovery.State.Cursors {
		if cursor.Source == position.source && cursor.Destination == cursorDestination && strings.HasPrefix(cursor.Ref, "refs/tags/") {
			position.tag = strings.TrimPrefix(cursor.Ref, "refs/tags/")
			break
		}
	}
	if position.tag == "" {
		return chunkPosition{}, fmt.Errorf(
			"replay chunk: consumer position %s at generated base %s has no source release cursor",
			position.source, cursorDestination)
	}
	return position, nil
}

func chunkCandidates(ctx context.Context, git *gitcli.Runner, start chunkPosition, release string, size int) ([]string, int, error) {
	remaining, err := git.CommitLog(ctx, gitcli.CommitLogOptions{Include: []string{release}, Exclude: []string{start.source}})
	if err != nil {
		return nil, 0, fmt.Errorf("replay chunk: count source range: %w", err)
	}
	if len(remaining) == 0 {
		return nil, 0, errors.New("replay chunk: pending release has no commits after the recorded source position")
	}
	mainline, err := git.CommitLog(ctx, gitcli.CommitLogOptions{
		Include: []string{release}, Exclude: []string{start.source}, FirstParent: true,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("replay chunk: read source mainline: %w", err)
	}
	if len(mainline) == 0 {
		return nil, 0, errors.New("replay chunk: source mainline has no checkpoint candidate")
	}
	first := 0
	for i, candidate := range mainline {
		count, err := chunkRangeCount(ctx, git, start.source, candidate.SHA)
		if err != nil {
			return nil, 0, err
		}
		first = i
		if count >= size {
			break
		}
	}
	candidates := make([]string, 0, len(mainline)-first)
	for _, commit := range mainline[first:] {
		candidates = append(candidates, commit.SHA)
	}
	return candidates, len(remaining), nil
}

func chunkRangeCount(ctx context.Context, git *gitcli.Runner, anchor, head string) (int, error) {
	commits, err := git.CommitLog(ctx, gitcli.CommitLogOptions{Include: []string{head}, Exclude: []string{anchor}})
	if err != nil {
		return 0, fmt.Errorf("replay chunk: count range through %s: %w", head, err)
	}
	return len(commits), nil
}

func snapshotChunkMapping(ctx context.Context, git *gitcli.Runner, store *gomodmap.Store) ([]byte, state.Mapping, error) {
	data, index, err := store.Snapshot(ctx)
	if err != nil {
		return nil, state.Mapping{}, fmt.Errorf("replay chunk: snapshot mapping: %w", err)
	}
	descriptor, err := mappingDescriptor(ctx, git, data, index)
	if err != nil {
		return nil, state.Mapping{}, err
	}
	return data, descriptor, nil
}

func mappingDescriptor(ctx context.Context, git *gitcli.Runner, data []byte, index *gomodmap.Index) (state.Mapping, error) {
	if index == nil || index.Len() == 0 {
		return state.Mapping{}, errors.New("replay chunk: mapping index is empty")
	}
	object, err := git.WriteBlob(ctx, data)
	if err != nil {
		return state.Mapping{}, fmt.Errorf("replay chunk: write mapping blob: %w", err)
	}
	sum := sha256.Sum256(data)
	return state.Mapping{
		Digest:  "sha256:" + hex.EncodeToString(sum[:]),
		Object:  object,
		Entries: index.Len(),
	}, nil
}

func chunkDocument(ctx context.Context, opts ChunkOptions, start chunkPosition, dag *DAGResult, track state.Track, mapping state.Mapping) (state.Document, error) {
	next := opts.Discovery.State.Clone()
	next.Digest = ""
	next.Mapping = mapping
	next.Engine = state.Engine{Version: buildinfo.Version, Toolchain: opts.Config.Determinism.Toolchain}

	profile := dag.Replay.Epoch.ProfileHash
	if next.Epoch.Profile != profile {
		if start.track != nil {
			return state.Document{}, fmt.Errorf("replay chunk: active track profile %s does not match generated profile %s", next.Epoch.Profile, profile)
		}
		mainline, err := opts.SourceCache.Git().WithNoLazyFetch().CommitLog(ctx, gitcli.CommitLogOptions{
			Include: []string{track.Source}, Exclude: []string{start.source}, FirstParent: true,
		})
		if err != nil {
			return state.Document{}, fmt.Errorf("replay chunk: find new epoch source: %w", err)
		}
		if len(mainline) == 0 {
			return state.Document{}, errors.New("replay chunk: new epoch has no first-parent source commit after its anchor")
		}
		next.Epoch = state.Epoch{
			Profile:     profile,
			Source:      mainline[0].SHA,
			Destination: start.destination,
		}
	}

	replaced := false
	for i := range next.Tracks {
		if next.Tracks[i].Name == track.Name {
			next.Tracks[i] = track
			replaced = true
			break
		}
	}
	if !replaced {
		next.Tracks = append(next.Tracks, track)
	}
	merged, err := state.MergeWith(ctx, opts.Discovery.State, next,
		opts.SourceCache.Git().WithNoLazyFetch(), opts.Destination.Git)
	if err != nil {
		return state.Document{}, fmt.Errorf("replay chunk: %w", err)
	}
	return merged, nil
}
