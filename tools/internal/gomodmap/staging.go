package gomodmap

import (
	"context"
	"fmt"
	"strings"

	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/gitgraph"
)

// KubernetesCommitTrailer is the trailer every published staging commit carries
// to name the source commit it was generated from. It is the only link between
// the two histories: staging commits keep neither the source object name in
// their tree nor a shared parent with it.
const KubernetesCommitTrailer = "Kubernetes-commit"

// SourceMainline is the first-parent history of one source commit, newest
// first.
//
// It is built once per source commit and reused for every staging module,
// because the walk is the expensive part of a mapping and the answer does not
// depend on which module is being mapped.
//
// Only the first parent is followed. A staging repository is generated from the
// mainline of a release branch, so a commit that is only reachable through a
// merge's second parent was never published on its own, and treating it as a
// mapping candidate would name a staging commit that does not exist.
const kubernetesStagingPrefix = "k8s.io/"

// StagingRepository returns the canonical repository for a Kubernetes staging
// module. Staging modules are one-segment k8s.io paths published from sibling
// repositories in the kubernetes organization; refusing every other shape keeps
// exact-commit resolution from guessing where dependency history lives.
func StagingRepository(modulePath string) (string, error) {
	name, ok := strings.CutPrefix(modulePath, kubernetesStagingPrefix)
	if !ok || name == "" || strings.Contains(name, "/") {
		return "", fmt.Errorf("staging module %q must be a one-segment path below %s", modulePath, kubernetesStagingPrefix)
	}
	return "https://github.com/kubernetes/" + name + ".git", nil
}

type SourceMainline struct {
	// commits is the mainline, newest first, so a scan finds the closest
	// ancestor before any older one.
	commits []string
}

// MainlineOptions bounds one mainline walk.
type MainlineOptions struct {
	// Revision is the source commit to walk back from.
	Revision string
	// Anchor is the oldest source commit the mapping may inspect, inclusive.
	Anchor string
	// MaxCount bounds the walk. Zero means the whole history.
	//
	// A bound is a correctness risk rather than only a performance choice: a
	// mapping that finds nothing within the bound is indistinguishable from a
	// source commit that predates the staging repository, so a bounded walk that
	// comes up empty is reported as a failed mapping rather than as an absent
	// one.
	MaxCount int
}

// NewSourceMainline walks one release-bounded first-parent source history.
// The inclusive anchor prevents every exact generation from scanning the full
// six-figure Kubernetes history.
func NewSourceMainline(ctx context.Context, git *gitcli.Runner, opts MainlineOptions) (*SourceMainline, error) {
	if opts.Revision == "" {
		return nil, fmt.Errorf("source mainline: a revision is required")
	}
	revList := gitcli.RevListOptions{
		Include: []string{opts.Revision}, FirstParent: true, MaxCount: opts.MaxCount,
	}
	var anchor string
	if opts.Anchor != "" {
		var err error
		anchor, err = git.ResolveCommit(ctx, opts.Anchor)
		if err != nil {
			return nil, fmt.Errorf("source mainline anchor: %w", err)
		}
		revision, err := git.ResolveCommit(ctx, opts.Revision)
		if err != nil {
			return nil, fmt.Errorf("source mainline of %s: %w", opts.Revision, err)
		}
		descends, err := git.IsAncestor(ctx, anchor, revision)
		if err != nil {
			return nil, fmt.Errorf("source mainline of %s: anchor ancestry: %w", opts.Revision, err)
		}
		if !descends {
			return nil, fmt.Errorf("source mainline of %s: revision %s does not descend from anchor %s", opts.Revision, revision, anchor)
		}
		revList.Exclude = []string{anchor}
	}
	commits, err := git.CommitGraph(ctx, revList)
	if err != nil {
		return nil, fmt.Errorf("source mainline of %s: %w", opts.Revision, err)
	}
	if anchor != "" {
		commits = append([]gitcli.DAGCommit{{SHA: anchor}}, commits...)
	}
	if len(commits) == 0 {
		return nil, fmt.Errorf("source mainline of %s: no commits", opts.Revision)
	}

	// CommitGraph reports parents before children. The scan wants the opposite,
	// because the closest ancestor is the one a staging repository would have
	// published most recently.
	mainline := &SourceMainline{commits: make([]string, 0, len(commits))}
	seen := make(map[string]bool, len(commits))
	for i := len(commits) - 1; i >= 0; i-- {
		sha := strings.Clone(commits[i].SHA)
		if seen[sha] {
			return nil, fmt.Errorf("source mainline of %s: commit %s appears twice, which a first-parent walk does not produce", opts.Revision, sha)
		}
		seen[sha] = true
		mainline.commits = append(mainline.commits, sha)
	}
	if err := gitgraph.ValidateSHA(mainline.commits[0]); err != nil {
		return nil, fmt.Errorf("source mainline of %s: %w", opts.Revision, err)
	}
	return mainline, nil
}

// Len reports how many commits the mainline covers.
func (m *SourceMainline) Len() int { return len(m.commits) }

// Head reports the commit the walk started from.
func (m *SourceMainline) Head() string { return m.commits[0] }

// StagingIndex maps source commits onto the staging commits generated from
// them, for one staging repository.
type StagingIndex struct {
	// modulePath names the staging module, for error messages.
	modulePath string
	// bySource maps a source object name onto the staging commit claiming it.
	bySource map[string]string
}

// IndexOptions selects the staging history one index covers.
// StagingReleaseAnchorOptions identifies the adjacent staging release targets
// whose bounded publication history is about to be indexed.
type StagingReleaseAnchorOptions struct {
	ModulePath string
	Previous   string
	Current    string
}

// StagingReleaseAnchor derives the oldest staging commit an adjacent release
// walk may inspect. Kubernetes patch publishing usually advances linearly, but
// may tag one unclaimed dependency-update commit on a side spur before the next
// patch continues from its parent. Only that exact topology is accepted.
func StagingReleaseAnchor(ctx context.Context, git *gitcli.Runner, opts StagingReleaseAnchorOptions) (string, error) {
	if opts.ModulePath == "" {
		return "", fmt.Errorf("staging release anchor: a module path is required")
	}
	if opts.Previous == "" || opts.Current == "" {
		return "", fmt.Errorf("staging release anchor for %s: previous and current targets are required", opts.ModulePath)
	}
	previous, err := git.ResolveCommit(ctx, opts.Previous)
	if err != nil {
		return "", fmt.Errorf("staging release anchor for %s previous target: %w", opts.ModulePath, err)
	}
	current, err := git.ResolveCommit(ctx, opts.Current)
	if err != nil {
		return "", fmt.Errorf("staging release anchor for %s current target: %w", opts.ModulePath, err)
	}
	linear, err := git.IsAncestor(ctx, previous, current)
	if err != nil {
		return "", fmt.Errorf("staging release anchor for %s: ancestry: %w", opts.ModulePath, err)
	}
	if linear {
		return previous, nil
	}

	base, err := git.MergeBase(ctx, previous, current)
	if err != nil {
		return "", fmt.Errorf("staging release anchor for %s: divergent targets %s and %s: %w", opts.ModulePath, previous, current, err)
	}
	metadata, err := git.CommitInfo(ctx, previous)
	if err != nil {
		return "", fmt.Errorf("staging release anchor for %s previous target: %w", opts.ModulePath, err)
	}
	if len(metadata.Parents) != 1 || metadata.Parents[0] != base {
		return "", fmt.Errorf(
			"staging release anchor for %s: previous target %s must be one unmerged commit above unique base %s, parents are %v",
			opts.ModulePath, previous, base, metadata.Parents)
	}
	if claims := metadata.TrailerValues(KubernetesCommitTrailer); len(claims) != 0 {
		return "", fmt.Errorf(
			"staging release anchor for %s: divergent previous target %s carries %d %s trailers",
			opts.ModulePath, previous, len(claims), KubernetesCommitTrailer)
	}
	return base, nil
}

type IndexOptions struct {
	// ModulePath is the staging module the repository publishes, such as
	// k8s.io/api.
	ModulePath string
	// Revision is the staging branch tip to walk, such as a release tag.
	Revision string
	// Anchor is the oldest staging commit to inspect, inclusive.
	Anchor string
	// MaxCount bounds the walk. Zero means the whole bounded history.
	MaxCount int
}

// NewStagingIndex reads the source commit each release-bounded staging commit
// claims. Multiple Kubernetes-commit trailers are ambiguous and refused; a
// commit carrying no claim is unrelated publishing machinery and is skipped.
func NewStagingIndex(ctx context.Context, git *gitcli.Runner, opts IndexOptions) (*StagingIndex, error) {
	if opts.ModulePath == "" {
		return nil, fmt.Errorf("staging index: a module path is required")
	}
	if opts.Revision == "" {
		return nil, fmt.Errorf("staging index for %s: a revision is required", opts.ModulePath)
	}
	logOptions := gitcli.CommitLogOptions{
		Include: []string{opts.Revision}, FirstParent: true, MaxCount: opts.MaxCount,
	}
	var anchor *gitcli.Commit
	var revision string
	if opts.Anchor != "" {
		anchorCommit, err := git.ResolveCommit(ctx, opts.Anchor)
		if err != nil {
			return nil, fmt.Errorf("staging index for %s anchor: %w", opts.ModulePath, err)
		}
		revision, err = git.ResolveCommit(ctx, opts.Revision)
		if err != nil {
			return nil, fmt.Errorf("staging index for %s: %w", opts.ModulePath, err)
		}
		descends, err := git.IsAncestor(ctx, anchorCommit, revision)
		if err != nil {
			return nil, fmt.Errorf("staging index for %s: anchor ancestry: %w", opts.ModulePath, err)
		}
		if !descends {
			return nil, fmt.Errorf("staging index for %s: revision %s does not descend from anchor %s", opts.ModulePath, revision, anchorCommit)
		}
		metadata, err := git.CommitInfo(ctx, anchorCommit)
		if err != nil {
			return nil, fmt.Errorf("staging index for %s anchor: %w", opts.ModulePath, err)
		}
		anchor = &metadata
		logOptions.Exclude = []string{anchorCommit}
		// Read the complete bounded first-parent range so its connection to the
		// inclusive anchor can be proved before an observational MaxCount is applied.
		logOptions.MaxCount = 0
	}
	commits, err := git.CommitLog(ctx, logOptions)
	if err != nil {
		return nil, fmt.Errorf("staging index for %s: %w", opts.ModulePath, err)
	}
	if anchor != nil {
		if revision != anchor.SHA {
			if len(commits) == 0 || len(commits[0].Parents) == 0 || commits[0].Parents[0] != anchor.SHA {
				return nil, fmt.Errorf("staging index for %s: anchor %s is not the first-parent boundary of revision %s", opts.ModulePath, anchor.SHA, revision)
			}
		}
		if opts.MaxCount > 0 && len(commits) > opts.MaxCount {
			commits = commits[len(commits)-opts.MaxCount:]
		}
		commits = append([]gitcli.Commit{*anchor}, commits...)
	}

	index := &StagingIndex{
		modulePath: opts.ModulePath,
		bySource:   make(map[string]string, len(commits)),
	}
	// Parents come before children, so a later commit claiming an already claimed
	// source commit overwrites the earlier one. That is the right direction: the
	// newest staging commit for a source commit is the one whose tree the release
	// actually published.
	for _, commit := range commits {
		claims := commit.TrailerValues(KubernetesCommitTrailer)
		switch len(claims) {
		case 0:
			continue
		case 1:
		default:
			return nil, fmt.Errorf("staging index for %s: commit %s carries %d %s trailers", opts.ModulePath, commit.SHA, len(claims), KubernetesCommitTrailer)
		}
		source := strings.Clone(claims[0])
		if err := gitgraph.ValidateSHA(source); err != nil {
			return nil, fmt.Errorf("staging index for %s: %s trailer of commit %s: %w", opts.ModulePath, KubernetesCommitTrailer, commit.SHA, err)
		}
		staging := strings.Clone(commit.SHA)
		if err := gitgraph.ValidateSHA(staging); err != nil {
			return nil, fmt.Errorf("staging index for %s: %w", opts.ModulePath, err)
		}
		index.bySource[source] = staging
	}
	if len(index.bySource) == 0 {
		return nil, fmt.Errorf("staging index for %s: no commit under %s carries a %s trailer", opts.ModulePath, opts.Revision, KubernetesCommitTrailer)
	}
	return index, nil
}

// Len reports how many source commits the index claims.
func (i *StagingIndex) Len() int { return len(i.bySource) }

// CommitMapping is the staging commit a source commit maps onto.
type CommitMapping struct {
	// ModulePath is the staging module the commit belongs to.
	ModulePath string
	// Source is the source commit the mapping was asked about.
	Source string
	// Matched is the source commit the staging commit actually claims. It equals
	// Source when that commit changed this staging module, and is an ancestor of
	// it otherwise.
	Matched string
	// Staging is the staging commit to pin.
	Staging string
	// Distance is how many mainline commits separate Source from Matched. Zero
	// means the source commit produced a staging commit of its own.
	Distance int
}

// Collapsed reports whether the source commit produced no staging commit of its
// own and was mapped onto an ancestor.
func (m CommitMapping) Collapsed() bool { return m.Distance > 0 }

// Map reports the staging commit that carries a source commit's content.
//
// A source commit that changed nothing under a staging directory produces no
// staging commit at all, which is the normal case: most Kubernetes commits touch
// none of a given staging module. The content of the source commit is then
// whatever the most recent ancestor that did produce one published, so the walk
// moves back along the mainline until the index recognises a commit.
//
// Failing to find one is fatal rather than a signal to fall back to the oldest
// staging commit. An unmapped source commit means the source predates the
// staging repository, the walk was bounded too tightly, or the two histories are
// unrelated, and each of those would produce a dependency pin on code that has
// nothing to do with the commit being built.
func (i *StagingIndex) Map(mainline *SourceMainline) (CommitMapping, error) {
	for distance, source := range mainline.commits {
		staging, ok := i.bySource[source]
		if !ok {
			continue
		}
		return CommitMapping{
			ModulePath: i.modulePath,
			Source:     mainline.Head(),
			Matched:    source,
			Staging:    staging,
			Distance:   distance,
		}, nil
	}
	return CommitMapping{}, fmt.Errorf(
		"staging module %s: no commit claims %s or any of its %d first-parent ancestors",
		i.modulePath, mainline.Head(), mainline.Len()-1)
}
