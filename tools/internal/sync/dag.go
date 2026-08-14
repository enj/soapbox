package sync

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/enj/soapbox/tools/internal/config"
	"github.com/enj/soapbox/tools/internal/extract"
	"github.com/enj/soapbox/tools/internal/generate"
	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/replay"
	"github.com/enj/soapbox/tools/internal/source"
)

// DAGOptions describes one release-bounded relevant-DAG replay. The source
// release tag has already been fetched and discovered; this phase reads only
// exact commits from that cache and writes unreachable objects to the local
// destination repository.
type DAGOptions struct {
	Config         *config.Config
	SourceCache    *source.Cache
	DestinationGit *gitcli.Runner
	Generate       generate.Options

	// AnchorCommit bounds the range below. MappedAnchor maps it onto
	// EpochParent without regenerating a destination commit. AnchorTag seeds the
	// watched closure for a published release anchor; an intermediate progress
	// anchor leaves it empty and is generated as an exact commit for analysis.
	AnchorCommit string
	AnchorTag    string
	// HistoryAnchorCommit and HistoryAnchorTag keep intermediate dependency
	// mapping bounded by the last published release across progress checkpoints.
	// Empty adopts AnchorCommit and AnchorTag.
	HistoryAnchorCommit string
	HistoryAnchorTag    string
	MappedAnchor        bool
	EpochParent         string

	// HeadCommit optionally stops before the release head for a checkpoint. Empty
	// means the complete pending release.
	HeadCommit string
	// Release is the pending source release bounding every checkpoint.
	Release source.Release
}

// DAGResult is the locally projected relevant history. ReleaseGeneration is the
// exact generated module used at the release head and is ready for release
// projection; no ref has moved.
type DAGResult struct {
	Replay             *replay.Result
	HeadCommit         string
	HeadGeneration     *generate.Result
	ReleaseGeneration  *generate.Result
	WatchedPaths       []string
	GeneratedCommits   int
	PrefilteredCommits int
}

type dagRun struct {
	opts       DAGOptions
	sourceGit  *gitcli.Runner
	parentTree string
	watched    map[string]bool
	generated  map[string]*generate.Result
	trees      map[string]string
	result     DAGResult
}

// ReplayDAG transforms the source DAG from AnchorCommit through Release.
func ReplayDAG(ctx context.Context, opts DAGOptions) (*DAGResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("source DAG replay: %w", err)
	}
	r, commits, err := newDAGRun(ctx, opts)
	if err != nil {
		return nil, err
	}
	if err := r.seedWatched(ctx); err != nil {
		return nil, err
	}

	profileHash := r.generated[opts.AnchorCommit].Report.Engine.ProfileHash
	head := dagHead(opts)
	result, err := replay.Run(ctx, opts.DestinationGit, replay.Options{
		Commits: commits,
		Anchor:  opts.AnchorCommit,
		Heads:   []string{head},
		Epoch: replay.Epoch{
			ProfileHash: profileHash,
			Parent:      opts.EpochParent,
		},
		Bot: replay.Identity{
			Name:  opts.Config.Commit.Committer.Name,
			Email: opts.Config.Commit.Committer.Email,
		},
		ProvenanceKey: opts.Config.Commit.TrailerKey,
		Transform:     r.transform,
	})
	if err != nil {
		return nil, fmt.Errorf("source DAG replay: %w", err)
	}
	r.result.Replay = result
	r.result.HeadCommit = head
	r.result.HeadGeneration = r.generated[head]
	if head == opts.Release.Source.Commit {
		r.result.ReleaseGeneration = r.generated[head]
		if r.result.ReleaseGeneration == nil {
			return nil, errors.New("source DAG replay: release head produced no generation")
		}
	}
	r.result.WatchedPaths = make([]string, 0, len(r.watched))
	for watched := range r.watched {
		r.result.WatchedPaths = append(r.result.WatchedPaths, watched)
	}
	slices.Sort(r.result.WatchedPaths)
	return &r.result, nil
}

func dagHead(opts DAGOptions) string {
	if opts.HeadCommit != "" {
		return opts.HeadCommit
	}
	return opts.Release.Source.Commit
}

func newDAGRun(ctx context.Context, opts DAGOptions) (*dagRun, []replay.Commit, error) {
	switch {
	case opts.Config == nil:
		return nil, nil, errors.New("source DAG replay: a profile is required")
	case opts.SourceCache == nil:
		return nil, nil, errors.New("source DAG replay: a source cache is required")
	case opts.DestinationGit == nil:
		return nil, nil, errors.New("source DAG replay: a destination Git runner is required")
	case opts.AnchorCommit == "":
		return nil, nil, errors.New("source DAG replay: an anchor commit is required")
	case opts.EpochParent == "":
		return nil, nil, errors.New("source DAG replay: an epoch parent is required")
	case opts.Release.Source.Kind != source.KindTag || opts.Release.Source.Name == "" || opts.Release.Source.Commit == "":
		return nil, nil, errors.New("source DAG replay: the range head must be a resolved source release tag")
	case !opts.Release.Source.Annotated:
		return nil, nil, fmt.Errorf("source DAG replay: release %s is not annotated", opts.Release.Source.Name)
	case opts.Release.DestinationTag == "":
		return nil, nil, errors.New("source DAG replay: the destination release tag is required")
	}
	mappedTag, err := config.MapReleaseTag(opts.Config.Release.Policy, opts.Release.Source.Name)
	if err != nil {
		return nil, nil, fmt.Errorf("source DAG replay: map release tag: %w", err)
	}
	if mappedTag != opts.Release.DestinationTag {
		return nil, nil, fmt.Errorf("source DAG replay: release %s maps to %s, discovery supplied %s", opts.Release.Source.Name, mappedTag, opts.Release.DestinationTag)
	}

	effectiveRemote := opts.Generate.SourceRemote
	if effectiveRemote == "" {
		effectiveRemote = opts.Config.Source.Repository
	}
	if effectiveRemote != opts.SourceCache.Remote() {
		return nil, nil, fmt.Errorf("source DAG replay: generation source %q does not match opened cache %q", effectiveRemote, opts.SourceCache.Remote())
	}
	if filepath.Clean(opts.Generate.CacheRoot) != filepath.Clean(filepath.Dir(opts.SourceCache.Path())) {
		return nil, nil, fmt.Errorf("source DAG replay: generation cache root %q does not own source cache %q", opts.Generate.CacheRoot, opts.SourceCache.Path())
	}

	sourceGit := opts.SourceCache.Git().WithNoLazyFetch()
	head := dagHead(opts)
	seenCommits := map[string]bool{}
	for _, commit := range []string{opts.AnchorCommit, head, opts.Release.Source.Commit} {
		if seenCommits[commit] {
			continue
		}
		seenCommits[commit] = true
		if _, err := opts.SourceCache.ResolveCommit(ctx, commit); err != nil {
			return nil, nil, fmt.Errorf("source DAG replay: %w", err)
		}
	}
	descends, err := sourceGit.IsAncestor(ctx, opts.AnchorCommit, head)
	if err != nil {
		return nil, nil, fmt.Errorf("source DAG replay: verify checkpoint ancestry: %w", err)
	}
	if !descends {
		return nil, nil, fmt.Errorf("source DAG replay: checkpoint %s does not descend from anchor %s", head, opts.AnchorCommit)
	}
	bounded, err := sourceGit.IsAncestor(ctx, head, opts.Release.Source.Commit)
	if err != nil {
		return nil, nil, fmt.Errorf("source DAG replay: verify release bound: %w", err)
	}
	if !bounded {
		return nil, nil, fmt.Errorf("source DAG replay: checkpoint %s is not an ancestor of release %s at %s", head, opts.Release.Source.Name, opts.Release.Source.Commit)
	}

	anchor, err := sourceGit.CommitInfo(ctx, opts.AnchorCommit)
	if err != nil {
		return nil, nil, fmt.Errorf("source DAG replay: read anchor: %w", err)
	}
	logged, err := sourceGit.CommitLog(ctx, gitcli.CommitLogOptions{
		Include: []string{head},
		Exclude: []string{opts.AnchorCommit},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("source DAG replay: read source range: %w", err)
	}
	commits := make([]replay.Commit, 0, len(logged)+1)
	commits = append(commits, replayCommit(anchor))
	for _, commit := range logged {
		commits = append(commits, replayCommit(commit))
	}

	parentTree, err := opts.DestinationGit.ResolveTree(ctx, opts.EpochParent)
	if err != nil {
		return nil, nil, fmt.Errorf("source DAG replay: resolve epoch parent tree: %w", err)
	}
	r := &dagRun{
		opts:       opts,
		sourceGit:  sourceGit,
		parentTree: parentTree,
		watched:    initialWatched(opts.Config),
		generated:  make(map[string]*generate.Result, len(commits)),
		trees:      make(map[string]string, len(commits)),
	}
	return r, commits, nil
}

func replayCommit(commit gitcli.Commit) replay.Commit {
	return replay.Commit{
		SHA:     commit.SHA,
		Parents: slices.Clone(commit.Parents),
		Author: gitcli.Signature{
			Name:  commit.AuthorName,
			Email: commit.AuthorEmail,
			Date:  commit.AuthorDateRaw,
		},
		CommitterDate: commit.CommitterDateRaw,
		Message:       commit.RawMessage,
	}
}

func (r *dagRun) seedWatched(ctx context.Context) error {
	ref := r.opts.Release.Source.Name
	release := r.opts.AnchorCommit == r.opts.Release.Source.Commit
	if r.opts.AnchorTag != "" {
		ref = r.opts.AnchorTag
		release = true
	}
	generated, err := r.generate(ctx, r.opts.AnchorCommit, ref, release)
	if err != nil {
		return fmt.Errorf("source DAG replay: seed watched closure at %s: %w", r.opts.AnchorCommit, err)
	}
	r.generated[r.opts.AnchorCommit] = generated
	r.addWatched(generated)
	return nil
}

func (r *dagRun) transform(ctx context.Context, commit replay.Commit) (replay.Transformed, error) {
	baseline := r.parentTree
	if len(commit.Parents) > 0 {
		if tree, ok := r.trees[commit.Parents[0]]; ok {
			baseline = tree
		}
	}

	if commit.SHA == r.opts.AnchorCommit && r.opts.MappedAnchor {
		r.trees[commit.SHA] = baseline
		r.result.PrefilteredCommits++
		return replay.Transformed{
			Source:   commit.SHA,
			Tree:     baseline,
			Changed:  false,
			Evidence: []string{"published range anchor"},
		}, nil
	}

	relevant, paths, err := r.relevant(ctx, commit)
	if err != nil {
		return replay.Transformed{}, err
	}
	if !relevant {
		r.trees[commit.SHA] = baseline
		r.result.PrefilteredCommits++
		return replay.Transformed{
			Source:   commit.SHA,
			Tree:     baseline,
			Changed:  false,
			Evidence: []string{"no watched source path changed"},
		}, nil
	}

	generated := r.generated[commit.SHA]
	if generated == nil {
		isRelease := commit.SHA == r.opts.Release.Source.Commit
		ref := r.opts.Release.Source.Name
		generated, err = r.generate(ctx, commit.SHA, ref, isRelease)
		if err != nil {
			return replay.Transformed{}, err
		}
		r.generated[commit.SHA] = generated
	}
	if generated.Report.Engine.ProfileHash != r.generated[r.opts.AnchorCommit].Report.Engine.ProfileHash {
		return replay.Transformed{}, fmt.Errorf("commit %s generated profile %s, anchor generated %s", commit.SHA, generated.Report.Engine.ProfileHash, r.generated[r.opts.AnchorCommit].Report.Engine.ProfileHash)
	}
	manifest, err := composeModuleTree(ctx, r.opts.DestinationGit, r.opts.Config, r.opts.EpochParent, generated.Files)
	if err != nil {
		return replay.Transformed{}, err
	}
	r.trees[commit.SHA] = manifest.Tree
	r.addWatched(generated)
	r.result.GeneratedCommits++
	return replay.Transformed{
		Source:   commit.SHA,
		Tree:     manifest.Tree,
		Changed:  manifest.Tree != baseline,
		Force:    commit.SHA == r.opts.Release.Source.Commit,
		Evidence: append([]string{"watched source path changed"}, paths...),
	}, nil
}

func (r *dagRun) generate(ctx context.Context, commit, tag string, release bool) (*generate.Result, error) {
	opts := r.opts.Generate
	opts.Config = r.opts.Config
	opts.Fetch = false
	opts.Materialize = false
	if release {
		opts.Ref = extract.Ref{Kind: extract.RefTag, Name: tag}
		opts.ReleaseContext = ""
		opts.HistoryAnchor = ""
		opts.HistoryAnchorRelease = ""
		opts.StagingSources = nil
	} else {
		opts.Ref = extract.Ref{Kind: extract.RefCommit, Name: commit}
		opts.ReleaseContext = r.opts.Release.Source.Name
		opts.HistoryAnchor = r.opts.HistoryAnchorCommit
		if opts.HistoryAnchor == "" {
			opts.HistoryAnchor = r.opts.AnchorCommit
		}
		opts.HistoryAnchorRelease = r.opts.HistoryAnchorTag
		if opts.HistoryAnchorRelease == "" {
			opts.HistoryAnchorRelease = r.opts.AnchorTag
		}
	}
	result, err := generate.Generate(ctx, opts)
	if err != nil {
		return result, err
	}
	if result.Report.Source.Commit != commit {
		return nil, fmt.Errorf("generation selected source %s, want %s", result.Report.Source.Commit, commit)
	}
	return result, nil
}

func (r *dagRun) relevant(ctx context.Context, commit replay.Commit) (bool, []string, error) {
	if commit.SHA == r.opts.Release.Source.Commit || commit.SHA == r.opts.AnchorCommit && !r.opts.MappedAnchor {
		return true, []string{"release boundary"}, nil
	}
	parent := ""
	if len(commit.Parents) > 0 {
		parent = commit.Parents[0]
	}
	paths, err := r.sourceGit.ChangedPaths(ctx, parent, commit.SHA)
	if err != nil {
		return false, nil, fmt.Errorf("changed paths of %s: %w", commit.SHA, err)
	}
	var matched []string
	for _, changed := range paths {
		if r.watches(changed) {
			matched = append(matched, changed)
		}
	}
	return len(matched) > 0, matched, nil
}

func initialWatched(cfg *config.Config) map[string]bool {
	watched := map[string]bool{
		"go.mod":  true,
		"go.sum":  true,
		"LICENSE": true,
		"NOTICE":  true,
		"PATENTS": true,
	}
	for _, root := range cfg.Packages.Roots {
		watched[strings.TrimSuffix(root, "/")] = true
	}
	for _, copied := range cfg.Dependencies.CopyPackages {
		watched[strings.TrimSuffix(copied, "/")] = true
	}
	return watched
}

func (r *dagRun) addWatched(result *generate.Result) {
	for _, pkg := range result.Report.Extract.Post.ClosurePackages {
		if rel, ok := strings.CutPrefix(pkg, r.opts.Config.Source.ImportPrefix+"/"); ok {
			r.watched[rel] = true
			continue
		}
		for _, staged := range result.Report.Staging.Modules {
			if pkg == staged.Path {
				r.watched[staged.Directory] = true
				break
			}
			if rel, ok := strings.CutPrefix(pkg, staged.Path+"/"); ok {
				r.watched[staged.Directory+"/"+rel] = true
				break
			}
		}
	}
}

func (r *dagRun) watches(path string) bool {
	if r.watched[path] {
		return true
	}
	for watched := range r.watched {
		if strings.HasPrefix(path, watched+"/") {
			return true
		}
	}
	return false
}
