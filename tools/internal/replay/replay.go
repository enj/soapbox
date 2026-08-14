// Package replay maps a source commit graph onto deterministic destination
// commits.
//
// A run is a function of what it is handed: the source commits with their parent
// edges, one profile epoch, one bot identity, and a transform that answers what
// tree each source commit produces. Nothing is read from a clock, an
// environment, or a ref, so two runs over the same inputs write the same object
// names. That property is what the published history rests on, because a
// destination commit is only append only if regenerating it produces the commit
// that is already there.
//
// The package writes objects and nothing else. It creates no ref, moves no
// branch, pushes nothing, and keeps no state on disk, so the commits it writes
// are unreachable until a later phase points a ref at them. Deciding what
// history should look like and deciding to publish it are separate gates, and
// only the second one can destroy anything.
//
// The transform decides content and this package decides shape. Nothing here
// looks inside a tree beyond checking that the name it was given is one, and the
// provenance trailer key, the bot identity, and the profile hash are all
// parameters, so the transformation carries no Kubernetes specific behaviour.
package replay

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/gitgraph"
	"github.com/enj/soapbox/tools/internal/treebuild"
)

// Replay sentinels. Callers use errors.Is to distinguish the refusal.
var (
	// ErrTransformSource reports a result that describes a commit other than the
	// one the transform was handed.
	ErrTransformSource = errors.New("transform reported a different source commit")
	// ErrTransformTree reports a result naming an object that is missing from
	// the destination repository or is not a tree.
	ErrTransformTree = errors.New("transform did not report a tree of the destination repository")
	// ErrTransformChange reports a result whose changed flag contradicts the
	// tree it produced.
	ErrTransformChange = errors.New("transform contradicts the tree it produced")
	// ErrProfileHash reports an epoch that does not carry one canonical profile
	// hash.
	ErrProfileHash = errors.New("profile hash is not one canonical value")
)

// Identity is a name and address with no date.
//
// The engine's bot is a fixed identity, and every date a replayed commit records
// comes from upstream, so an identity carrying one here would be offering a
// value this package must never use: a generated date is exactly what stops a
// rerun from reproducing an object name.
type Identity struct {
	// Name is the identity's display name.
	Name string
	// Email is the identity's address.
	Email string
}

// Commit is one source commit and the metadata a replayed commit preserves.
//
// The metadata travels with the graph rather than being read here, because this
// package never opens the source repository: it writes into the destination one.
// That is what makes the transformation testable as a function, and what stops
// the shape of published history from depending on which clone of upstream a run
// happened to have.
type Commit struct {
	// SHA is the source commit object name.
	SHA string
	// Parents are the source parent object names in git's order, so Parents[0]
	// is the first parent and defines the mainline. A root commit has none.
	Parents []string
	// Author is the upstream author carrying the upstream raw author date. It is
	// preserved exactly, because the person who wrote the change is still the
	// author of the commit that replays it.
	Author gitcli.Signature
	// CommitterDate is the upstream raw committer date. A replayed commit
	// records the engine's bot as its committer, because the engine is what
	// recorded that particular commit, and this date, because a rerun has to
	// produce the same object name rather than one that moves with the clock.
	CommitterDate string
	// Message is the complete upstream commit message. It is replayed as written
	// and extended with exactly one provenance trailer.
	Message string
}

// Transformed is what a transform reports for one source commit.
type Transformed struct {
	// Source is the commit this result describes. The transform echoes it so
	// that a result computed for a different commit, which is what a cache keyed
	// by the wrong thing hands back, is refused rather than committed under a
	// provenance trailer that would then be a false claim.
	Source string
	// Tree is the destination tree object name the transform produced. The
	// object must already exist in the destination repository.
	Tree string
	// Changed reports content differing from what the destination parent already
	// records. It is checked against the trees rather than believed.
	Changed bool
	// Force records an otherwise unchanged commit. Release boundaries use it so
	// each immutable destination tag targets a commit carrying the exact source
	// release provenance even when generated module bytes did not change.
	Force bool
	// Evidence records why, for the run's report. It is carried through
	// unchanged and never affects the shape of the history.
	Evidence []string
}

// Transform produces the destination tree of one source commit.
//
// It is called exactly once per replayed commit, in replay order, and it is
// where every content decision lives: pruning, patching, relocation, rewriting,
// and generation. The tree it names must already be written, because this
// package commits the name it is given rather than building content of its own.
type Transform func(ctx context.Context, source Commit) (Transformed, error)

// Epoch is one profile epoch: the profile a run generates under and the
// destination commit its history extends.
//
// A run is exactly one epoch, which is why the profile hash is a single value
// here rather than something the transform reports per commit. A control plane
// change that alters generated bytes starts a new epoch, and a new epoch is a
// second run carrying the new hash and the previous destination head as Parent.
// Published history is then extended rather than regenerated, which is the only
// treatment of a profile change that leaves already released tags standing.
type Epoch struct {
	// ProfileHash is the digest of the output affecting profile, in the form
	// "sha256:" followed by 64 lower case hexadecimal characters, which is what
	// the engine's plan reports. It is recorded in the result so a later run can
	// tell whether the profile moved, and the exact form is required because two
	// spellings of one digest would compare as two epochs.
	ProfileHash string
	// Parent is the destination commit this epoch's first generated commit
	// attaches to. Empty starts the history at a root, which is what the first
	// epoch of a destination repository does.
	Parent string
}

// Options configures one replay run.
type Options struct {
	// Commits are the source commits in the caller's traversal order, normally
	// git's own reverse topological order. The order is the tie-break that makes
	// the replay sequence reproducible when several commits become ready at
	// once, so it must not depend on map iteration or on a directory listing.
	Commits []Commit
	// Anchor bounds the replay below. The anchor is replayed as the base of the
	// transformed history and its proper ancestors are left out, so a run states
	// which commit the destination history is rooted at rather than inferring
	// one. Empty replays everything the heads reach.
	Anchor string
	// Heads are the source commits to replay up to. Empty means every head of
	// the graph.
	Heads []string
	// Epoch is the profile epoch this run generates under.
	Epoch Epoch
	// Bot is the committer identity of every commit the run writes.
	Bot Identity
	// ProvenanceKey is the trailer key the source commit is recorded under, such
	// as Kubernetes-commit. It is what a later run reads to rebuild the mapping
	// from the published history, so a run without one is refused.
	ProvenanceKey string
	// Transform produces the destination tree of each source commit.
	Transform Transform
}

// Run replays the source graph into the destination repository and reports what
// it produced.
//
// The shape of the result is decided by these rules, and only by these rules:
//
//   - Traversal is topological, parents strictly before children, with the
//     caller's commit order as the tie-break. Source parent order is preserved,
//     so the first parent of a replayed commit is the descendant of the first
//     parent of the source commit.
//
//   - A source commit's destination parents are its source parents resolved to
//     the nearest ancestor that produced a commit, deduplicated by first
//     occurrence, with any parent that another parent already contains removed.
//     A source parent left below the anchor contributes nothing, which is how
//     the first replayed commit ends up attached to the epoch parent or to
//     nothing at all.
//
//   - The baseline of a commit is the tree its destination parent records, or
//     the empty tree when it has none. A transformed tree equal to its baseline
//     changed nothing, so no commit is written: the source commit maps onto that
//     destination parent, and everything descending from it attaches there
//     instead. This is what keeps an upstream commit that touched nothing the
//     extraction keeps out of the published history.
//
//   - A commit with more than one distinct destination parent is written even
//     when its tree equals its first parent's, because the side it merges
//     carries generated history the first parent does not and dropping the merge
//     would erase where that history joined. A merge whose sides all resolve to
//     one destination commit is not a merge here, and it collapses or is written
//     as an ordinary commit exactly like any other.
//
//   - Changed is checked rather than believed, wherever the check is meaningful:
//     every commit with at most one destination parent, which is every commit
//     whose baseline is a single known tree. A transform that reports a change
//     while producing the baseline tree, or reports none while producing
//     something else, is refused. The trees are the authority, because they are
//     what gets published.
//
// The written commits preserve the upstream author, author date, and message,
// record the bot as committer with the upstream committer date, carry exactly
// one provenance trailer naming the source commit, and are unsigned.
//
// A failed run reports both the error and the records completed before it, so a
// caller can see how far the run got and which objects exist. Nothing was
// referenced, so those objects are unreachable and cost nothing but disk. A run
// that fails validation reports no result at all, because nothing was attempted.
func Run(ctx context.Context, git *gitcli.Runner, opts Options) (*Result, error) {
	r, err := newRun(ctx, git, opts)
	if err != nil {
		return nil, fmt.Errorf("replay: %w", err)
	}
	return r.execute(ctx)
}

// run is the state of one replay.
type run struct {
	git  *gitcli.Runner
	opts Options
	// source is the graph the replay traverses, and metadata is the per commit
	// detail the graph deliberately does not carry.
	source   *gitgraph.Graph
	metadata map[string]Commit
	// mapping records which destination commit each source commit produced,
	// including the source commits that produced no commit of their own and map
	// onto an ancestor's.
	mapping *gitgraph.Mapping
	// destination holds the commits this run wrote, plus the epoch parent as a
	// root, and graph is the cached graph over them.
	destination []gitgraph.Commit
	graph       *gitgraph.Graph
	// trees is the tree each destination commit records, which is what a
	// baseline comparison needs and what makes it free rather than a read.
	trees     map[string]string
	emptyTree string
	result    *Result
}

// provenanceProbe is the message and value the provenance key is checked with.
//
// The key is validated by asking the writer that will use it rather than by a
// second copy of git's token rule here, because two answers to "is this a
// trailer key" is how a run ends up publishing a commit whose provenance nothing
// can read back. The probe runs before any transform, since a transform is
// expensive and an unusable key is a refusal the caller should have immediately.
const provenanceProbe = "provenance probe"

// newRun validates the options and prepares the run.
//
// Everything that can be judged without calling the transform is judged here.
// A transform is the expensive part of a replay, and a run that performed
// thousands of them before discovering that its bot identity was incomplete
// would be reporting a problem far from where it was introduced.
func newRun(ctx context.Context, git *gitcli.Runner, opts Options) (*run, error) {
	if git == nil {
		return nil, errors.New("a destination git runner is required")
	}
	if opts.Transform == nil {
		return nil, errors.New("a transform is required")
	}
	if len(opts.Commits) == 0 {
		return nil, errors.New("at least one source commit is required")
	}
	if err := validateProfileHash(opts.Epoch.ProfileHash); err != nil {
		return nil, err
	}
	if err := validateIdentity(opts.Bot); err != nil {
		return nil, err
	}
	if _, err := treebuild.ProvenanceMessage(provenanceProbe, opts.ProvenanceKey, provenanceProbe); err != nil {
		return nil, fmt.Errorf("provenance key %q: %w", opts.ProvenanceKey, err)
	}

	nodes := make([]gitgraph.Commit, 0, len(opts.Commits))
	metadata := make(map[string]Commit, len(opts.Commits))
	for i, commit := range opts.Commits {
		if err := validateCommit(commit); err != nil {
			return nil, fmt.Errorf("source commit %d: %w", i, err)
		}
		node := commit
		node.Parents = slices.Clone(commit.Parents)
		metadata[node.SHA] = node
		nodes = append(nodes, gitgraph.Commit{SHA: node.SHA, Parents: node.Parents})
	}
	source, err := gitgraph.New(nodes)
	if err != nil {
		return nil, err
	}

	r := &run{
		git:      git,
		opts:     opts,
		source:   source,
		metadata: metadata,
		mapping:  gitgraph.NewMapping(),
		trees:    make(map[string]string, len(opts.Commits)),
	}
	if err := r.prepareEpoch(ctx); err != nil {
		return nil, err
	}
	return r, nil
}

// prepareEpoch resolves what the epoch's history extends.
//
// The epoch parent enters the destination graph as a root. Its real ancestry is
// unknown to this run, and recording it as a root is the honest shape: no commit
// this run writes can be an ancestor of a commit that already existed, so the
// only thing the missing ancestry could hide is a relationship that cannot
// arise. Its tree is read once, because it is the baseline the first commit of
// the epoch is compared against.
func (r *run) prepareEpoch(ctx context.Context) error {
	emptyTree, err := r.git.EmptyTree(ctx)
	if err != nil {
		return fmt.Errorf("empty tree: %w", err)
	}
	r.emptyTree = emptyTree

	parent := r.opts.Epoch.Parent
	if parent == "" {
		return nil
	}
	if err := gitgraph.ValidateSHA(parent); err != nil {
		return fmt.Errorf("epoch parent: %w", err)
	}
	if err := r.requireType(ctx, parent, "commit"); err != nil {
		return fmt.Errorf("epoch parent: %w", err)
	}
	tree, err := r.git.ResolveTree(ctx, parent)
	if err != nil {
		return fmt.Errorf("epoch parent tree: %w", err)
	}
	r.trees[parent] = tree
	r.destination = append(r.destination, gitgraph.Commit{SHA: parent})
	return nil
}

// execute replays every selected commit.
//
// The result is built as the run proceeds and is returned with a failure as well
// as with success, so a caller that hit an error still learns exactly which
// commits were written before it.
func (r *run) execute(ctx context.Context) (*Result, error) {
	order, heads, err := r.order()
	if err != nil {
		return nil, fmt.Errorf("replay: %w", err)
	}
	r.result = &Result{
		Epoch:   r.opts.Epoch,
		Records: make([]Record, 0, len(order)),
		Mapping: r.mapping,
	}

	for _, sha := range order {
		if err := ctx.Err(); err != nil {
			return r.result, fmt.Errorf("replay at %s: %w", sha, err)
		}
		if err := r.step(ctx, sha); err != nil {
			return r.result, fmt.Errorf("replay %s: %w", sha, err)
		}
	}

	r.result.Heads = make([]Head, 0, len(heads))
	for _, head := range heads {
		destination, _ := r.mapping.Destination(head)
		r.result.Heads = append(r.result.Heads, Head{Source: head, Destination: destination})
	}
	return r.result, nil
}

// order reports the commits to replay and the heads they were selected for.
func (r *run) order() (order, heads []string, err error) {
	heads = slices.Clone(r.opts.Heads)
	if len(heads) == 0 {
		heads = r.source.Heads()
	}
	for _, head := range heads {
		if err := gitgraph.ValidateSHA(head); err != nil {
			return nil, nil, fmt.Errorf("head: %w", err)
		}
	}

	if r.opts.Anchor != "" {
		if err := gitgraph.ValidateSHA(r.opts.Anchor); err != nil {
			return nil, nil, fmt.Errorf("anchor: %w", err)
		}
		// Range validates that every head descends from the anchor, which is the
		// refusal a newly tracked branch with unrelated history has to hit.
		order, err = r.source.Range(r.opts.Anchor, heads)
		if err != nil {
			return nil, nil, err
		}
		return order, heads, nil
	}

	// Without an anchor the replay covers whatever the heads reach, which for
	// the default heads is the whole graph.
	reachable := make(map[string]bool, r.source.Len())
	for _, head := range heads {
		ancestors, err := r.source.Ancestors(head)
		if err != nil {
			return nil, nil, err
		}
		maps.Copy(reachable, ancestors)
	}
	for _, sha := range r.source.TopologicalOrder() {
		if reachable[sha] {
			order = append(order, sha)
		}
	}
	return order, heads, nil
}

// step replays one source commit.
func (r *run) step(ctx context.Context, sha string) error {
	source := r.metadata[sha]
	transformed, err := r.transform(ctx, source)
	if err != nil {
		return err
	}
	parents, err := r.parents(sha)
	if err != nil {
		return err
	}

	record := Record{
		Source:        sha,
		SourceParents: slices.Clone(source.Parents),
		MappedParents: parents,
		Tree:          transformed.Tree,
		Changed:       transformed.Changed,
		Evidence:      slices.Clone(transformed.Evidence),
	}

	// More than one destination parent is a merge whichever tree it records, so
	// the baseline is not consulted: there is no single parent for a tree to be
	// unchanged against, and the side that survived deduplication is generated
	// history that only this commit joins.
	if len(parents) < 2 {
		collapsed, err := r.collapse(sha, parents, transformed)
		if err != nil {
			return err
		}
		if collapsed {
			record.Collapsed = true
			record.Destination, _ = r.mapping.Destination(sha)
			r.result.Collapsed++
			r.result.Records = append(r.result.Records, record)
			return nil
		}
	}

	destination, err := r.write(ctx, source, transformed.Tree, parents)
	if err != nil {
		return err
	}
	record.Destination = destination
	record.Merge = len(parents) > 1
	r.result.Written++
	r.result.Records = append(r.result.Records, record)
	return nil
}

// transform calls the transform and checks what it reported.
func (r *run) transform(ctx context.Context, source Commit) (Transformed, error) {
	transformed, err := r.opts.Transform(ctx, source)
	if err != nil {
		return Transformed{}, fmt.Errorf("transform: %w", err)
	}
	// A transform is the long part of a step, so cancellation is honoured again
	// the moment it returns. Stopping here rather than at the top of the next
	// step is what lets a cancelled run end without having probed or written
	// anything for work it was told to abandon.
	if err := ctx.Err(); err != nil {
		return Transformed{}, err
	}
	if transformed.Source != source.SHA {
		return Transformed{}, fmt.Errorf("%w: %q", ErrTransformSource, transformed.Source)
	}
	if err := gitgraph.ValidateSHA(transformed.Tree); err != nil {
		return Transformed{}, fmt.Errorf("transform tree: %w", err)
	}
	if err := r.requireType(ctx, transformed.Tree, "tree"); err != nil {
		return Transformed{}, fmt.Errorf("%w: %w", ErrTransformTree, err)
	}
	return transformed, nil
}

// parents reports the destination parents of a source commit.
//
// Resolving and deduplicating by first occurrence needs only the source graph.
// Removing a parent that another parent already contains needs the destination
// graph, and it can only remove something when at least two parents remain, so
// the destination graph is built for the merges a history preserves rather than
// once per replayed commit.
func (r *run) parents(sha string) ([]string, error) {
	mapped, err := r.source.MappedParents(sha, r.mapping, nil)
	if err != nil {
		return nil, err
	}
	if len(mapped) > 1 {
		graph, err := r.destinationGraph()
		if err != nil {
			return nil, err
		}
		mapped, err = graph.DedupeParents(mapped)
		if err != nil {
			return nil, fmt.Errorf("destination parents: %w", err)
		}
	}
	// A commit the mapping placed nowhere attaches to the commit the epoch
	// extends, which is what makes a new epoch a continuation of the published
	// history rather than a second root beside it.
	if len(mapped) == 0 && r.opts.Epoch.Parent != "" {
		mapped = []string{r.opts.Epoch.Parent}
	}
	return mapped, nil
}

// collapse decides whether a commit is written, and records the mapping when it
// is not.
//
// It is only meaningful for a commit with at most one destination parent, which
// is the only case where a single baseline tree exists to compare against.
func (r *run) collapse(sha string, parents []string, transformed Transformed) (bool, error) {
	baseline := r.emptyTree
	if len(parents) == 1 {
		known, ok := r.trees[parents[0]]
		if !ok {
			return false, fmt.Errorf("destination parent %s has no recorded tree", parents[0])
		}
		baseline = known
	}

	unchanged := transformed.Tree == baseline
	switch {
	case unchanged && transformed.Changed:
		return false, fmt.Errorf("%w: reported a change, but tree %s is already what the baseline records",
			ErrTransformChange, transformed.Tree)
	case !unchanged && !transformed.Changed:
		return false, fmt.Errorf("%w: reported no change, but tree %s is not the baseline %s",
			ErrTransformChange, transformed.Tree, baseline)
	case transformed.Force:
		return false, nil
	case !unchanged:
		return false, nil
	}

	// An unchanged commit with no destination parent generated nothing at all,
	// so it maps to nothing. Its descendants look further up for somewhere to
	// attach, and the first one that generates something becomes a root.
	if len(parents) == 0 {
		return true, nil
	}
	if err := r.mapping.Set(sha, parents[0]); err != nil {
		return false, err
	}
	return true, nil
}

// write records one destination commit.
func (r *run) write(ctx context.Context, source Commit, tree string, parents []string) (string, error) {
	destination, err := treebuild.WriteCommit(ctx, r.git, treebuild.CommitOptions{
		Tree:    tree,
		Parents: parents,
		Author:  source.Author,
		Committer: gitcli.Signature{
			Name:  r.opts.Bot.Name,
			Email: r.opts.Bot.Email,
			Date:  source.CommitterDate,
		},
		Message:       source.Message,
		ProvenanceKey: r.opts.ProvenanceKey,
		Source:        source.SHA,
	})
	if err != nil {
		return "", err
	}
	if err := r.mapping.Set(source.SHA, destination); err != nil {
		return "", err
	}
	r.trees[destination] = tree
	r.destination = append(r.destination, gitgraph.Commit{SHA: destination, Parents: slices.Clone(parents)})
	r.graph = nil
	return destination, nil
}

// destinationGraph returns a graph over the commits written so far, rebuilding
// it only when something was written since it was last asked for.
func (r *run) destinationGraph() (*gitgraph.Graph, error) {
	if r.graph != nil {
		return r.graph, nil
	}
	graph, err := gitgraph.New(r.destination)
	if err != nil {
		return nil, fmt.Errorf("destination graph: %w", err)
	}
	r.graph = graph
	return graph, nil
}

// requireType checks that an object exists in the destination repository with
// the expected type.
//
// The probe answers from the local object store, because whether this repository
// already holds an object is a question about this repository. A tree name that
// is really a blob would otherwise reach commit-tree, and a collapsed commit
// never reaches commit-tree at all, so without this check a transform could map
// a source commit onto an object that is not a tree and nothing would notice.
func (r *run) requireType(ctx context.Context, object, want string) error {
	infos, err := r.git.ObjectInfoBatch(ctx, gitcli.ObjectInfoOptions{Revisions: []string{object}})
	if err != nil {
		return err
	}
	if len(infos) != 1 {
		return fmt.Errorf("object %s: got %d records, want 1", object, len(infos))
	}
	switch info := infos[0]; {
	case info.Missing:
		return fmt.Errorf("object %s is missing", object)
	case info.Type != want:
		return fmt.Errorf("object %s is a %s, want a %s", object, info.Type, want)
	}
	return nil
}

// validateCommit checks the metadata a replayed commit is built from.
//
// The raw dates are checked with gitcli's own rule, because a date this package
// judged differently would be a second answer to a question gitcli already
// answers when it writes the commit. The identity fields are only checked for
// presence, which is deliberately weaker than the check gitcli performs: this
// exists so a run fails before performing thousands of transforms, not so that
// there are two authorities on what an identity is.
func validateCommit(commit Commit) error {
	if err := gitgraph.ValidateSHA(commit.SHA); err != nil {
		return err
	}
	if commit.Author.Name == "" || commit.Author.Email == "" {
		return fmt.Errorf("commit %s: an author name and email are required", commit.SHA)
	}
	if err := gitcli.ValidateRawDate(commit.Author.Date); err != nil {
		return fmt.Errorf("commit %s: author date: %w", commit.SHA, err)
	}
	if err := gitcli.ValidateRawDate(commit.CommitterDate); err != nil {
		return fmt.Errorf("commit %s: committer date: %w", commit.SHA, err)
	}
	if strings.TrimSpace(commit.Message) == "" {
		return fmt.Errorf("commit %s: a message is required", commit.SHA)
	}
	return nil
}

// validateIdentity checks the bot identity every written commit records.
func validateIdentity(bot Identity) error {
	if bot.Name == "" || bot.Email == "" {
		return errors.New("a bot name and email are required")
	}
	return nil
}

// The canonical form of a profile hash: the algorithm it names, and the length
// of the hex digest that follows.
//
// The form is stated rather than inferred, because the engine has one producer
// of these digests and it emits exactly this: extract renders "sha256:" followed
// by the hex encoding of a sha256 sum. Anything looser would let a value that is
// not a digest at all be recorded as the identity of an epoch. If a second
// algorithm is ever produced, the two have to be reconciled deliberately, and a
// refusal here is how that gets noticed rather than published.
const (
	profileHashPrefix = "sha256:"
	profileHashDigits = 64
)

// validateProfileHash refuses an epoch that does not name one canonical profile
// digest.
//
// The hash is an identity rather than a description: a later run compares it to
// decide whether the profile moved and therefore whether history may be extended
// or a new epoch has to start. Two spellings of one digest would answer that
// question with a spurious yes, so the canonical form is required here rather
// than normalized silently, and a value that is not a digest of the stated
// algorithm is refused rather than carried into a report as though it were one.
func validateProfileHash(hash string) error {
	if hash == "" {
		return fmt.Errorf("%w: a profile hash is required", ErrProfileHash)
	}
	digest, ok := strings.CutPrefix(hash, profileHashPrefix)
	if !ok {
		return fmt.Errorf("%w: %q must begin with %q", ErrProfileHash, hash, profileHashPrefix)
	}
	if len(digest) != profileHashDigits {
		return fmt.Errorf("%w: %q carries %d digest characters, want %d",
			ErrProfileHash, hash, len(digest), profileHashDigits)
	}
	for _, r := range digest {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f':
		default:
			return fmt.Errorf("%w: %q must be lower case hexadecimal", ErrProfileHash, hash)
		}
	}
	return nil
}
