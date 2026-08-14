package replay_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/replay"
)

// objectFormats are the hash algorithms a destination repository can be created
// under. sha256 is not hypothetical: it is chosen when the repository is created
// and it changes the length of every object name the replay handles.
var objectFormats = []gitcli.ObjectFormat{gitcli.ObjectFormatSHA1, gitcli.ObjectFormatSHA256}

// TestLinearHistory replays a chain in which one commit changed nothing.
//
// This is the ordinary case and the one the whole design exists for: upstream
// commits that touch nothing the extraction keeps must not appear in the
// published history, and the commits around them must still form a chain.
func TestLinearHistory(t *testing.T) {
	t.Parallel()
	for _, format := range objectFormats {
		t.Run(string(format), func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			f := newFixture(ctx, t, format)
			f.add(ctx, "a", "first")
			f.add(ctx, "b", "first", "a")
			f.add(ctx, "c", "second", "b")

			result := f.run(ctx, f.options(f.newProjection("").transform))

			if result.Written != 2 || result.Collapsed != 1 {
				t.Fatalf("wrote %d and collapsed %d commits, want 2 and 1", result.Written, result.Collapsed)
			}
			first, third := f.destination(result, "a"), f.destination(result, "c")
			if got := f.destination(result, "b"); got != first {
				t.Fatalf("the unchanged commit mapped to %s, want the commit before it, %s", got, first)
			}
			if parents := f.parents(ctx, first); len(parents) != 0 {
				t.Fatalf("the first commit has parents %v, want none", parents)
			}
			if parents := f.parents(ctx, third); !slices.Equal(parents, []string{first}) {
				t.Fatalf("the last commit has parents %v, want [%s]", parents, first)
			}
			if got, want := f.commitTree(ctx, third), f.tree(ctx, "second"); got != want {
				t.Fatalf("the last commit records tree %s, want %s", got, want)
			}

			collapsed := f.record(result, "b")
			if !collapsed.Collapsed || collapsed.Merge || collapsed.Changed {
				t.Fatalf("the unchanged commit was recorded as %+v", collapsed)
			}
			if !slices.Equal(collapsed.MappedParents, []string{first}) {
				t.Fatalf("the unchanged commit mapped parents %v, want [%s]", collapsed.MappedParents, first)
			}
			if want := []replay.Head{{Source: f.sha("c"), Destination: third}}; !slices.Equal(result.Heads, want) {
				t.Fatalf("heads are %v, want %v", result.Heads, want)
			}
		})
	}
}

// TestCommitShape checks what a replayed commit records.
//
// The shape is the contract with the published history: the upstream author
// keeps the change, the bot owns the act of recording it, the upstream committer
// date keeps the object name reproducible, the message is preserved with exactly
// one provenance trailer, and nothing is signed because nothing signed it.
func TestCommitShape(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newFixture(ctx, t, gitcli.ObjectFormatSHA1)
	f.add(ctx, "a", "first")

	result := f.run(ctx, f.options(f.newProjection("").transform))
	source := info(ctx, t, f.source, f.sha("a"))
	written := info(ctx, t, f.dest, f.destination(result, "a"))

	if written.AuthorIdentity() != source.AuthorIdentity() || written.AuthorDate != source.AuthorDate {
		t.Fatalf("author is %s at %s, want %s at %s",
			written.AuthorIdentity(), written.AuthorDate, source.AuthorIdentity(), source.AuthorDate)
	}
	if want := gitcli.Identity(botName, botEmail); written.CommitterIdentity() != want {
		t.Fatalf("committer is %s, want %s", written.CommitterIdentity(), want)
	}
	if written.CommitterDate != source.CommitterDate {
		t.Fatalf("committer date is %s, want the upstream %s", written.CommitterDate, source.CommitterDate)
	}
	if !strings.HasPrefix(written.RawMessage, source.RawMessage) {
		t.Fatalf("message is %q, want it to begin with the upstream %q", written.RawMessage, source.RawMessage)
	}
	if values := written.TrailerValues(provenanceKey); !slices.Equal(values, []string{f.sha("a")}) {
		t.Fatalf("provenance trailers are %v, want exactly [%s]", values, f.sha("a"))
	}
	// N is git's verdict for a commit that carries no signature at all.
	if written.SignatureStatus != "N" {
		t.Fatalf("signature status is %q, want an unsigned commit", written.SignatureStatus)
	}
}

// TestAnchorBoundsReplay replays from an anchor and leaves everything below it
// alone, which is what makes the transformed history start at a stated commit
// rather than at whatever the clone happened to contain.
func TestAnchorBoundsReplay(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newFixture(ctx, t, gitcli.ObjectFormatSHA1)
	f.add(ctx, "old", "ancient")
	f.add(ctx, "anchor", "first", "old")
	f.add(ctx, "tip", "second", "anchor")

	opts := f.options(f.newProjection("").transform)
	opts.Anchor = f.sha("anchor")
	result := f.run(ctx, opts)

	if len(result.Records) != 2 {
		t.Fatalf("replayed %d commits, want the anchor and the tip", len(result.Records))
	}
	if _, mapped := result.Mapping.Destination(f.sha("old")); mapped {
		t.Fatal("the commit below the anchor was replayed")
	}
	base := f.destination(result, "anchor")
	if parents := f.parents(ctx, base); len(parents) != 0 {
		t.Fatalf("the anchor commit has parents %v, want none: its source parent is below the boundary", parents)
	}
	if parents := f.parents(ctx, f.destination(result, "tip")); !slices.Equal(parents, []string{base}) {
		t.Fatalf("the tip has parents %v, want [%s]", parents, base)
	}
}

// TestHeadOutsideAnchor refuses a head whose history never reaches the anchor.
//
// A newly tracked branch that does not descend from the recorded anchor would
// publish history sharing no root with what was already published, so the run
// stops rather than inventing a relationship.
func TestHeadOutsideAnchor(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newFixture(ctx, t, gitcli.ObjectFormatSHA1)
	f.add(ctx, "anchor", "first")
	f.add(ctx, "unrelated", "other")

	opts := f.options(f.newProjection("").transform)
	opts.Anchor = f.sha("anchor")
	opts.Heads = []string{f.sha("unrelated")}

	_, err := replay.Run(ctx, f.dest, opts)
	if err == nil || !strings.Contains(err.Error(), "does not descend from anchor") {
		t.Fatalf("replay of an unrelated head returned %v, want a refusal", err)
	}
}

// TestMergePreserved keeps a merge whose side carries generated history, even
// though the merge itself produced the tree its first parent already records.
//
// Collapsing it would erase where that side joined the mainline, and the side's
// commits would then be reachable from nothing.
func TestMergePreserved(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		// order is the source parent order of the merge.
		order []string
		// sameTreeAsFirstParent reports the shape the rule is really about: a
		// merge that recorded exactly what its first parent already held and is
		// kept anyway, because the side it merged is generated history.
		sameTreeAsFirstParent bool
	}{
		{name: "mainline first", order: []string{"b", "s"}, sameTreeAsFirstParent: true},
		{name: "side first", order: []string{"s", "b"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			f := newFixture(ctx, t, gitcli.ObjectFormatSHA1)
			f.add(ctx, "a", "first")
			f.add(ctx, "b", "mainline", "a")
			f.add(ctx, "s", "side", "a")
			f.add(ctx, "m", "mainline", test.order...)

			result := f.run(ctx, f.options(f.newProjection("").transform))

			merge := f.record(result, "m")
			if !merge.Merge || merge.Collapsed {
				t.Fatalf("the merge was recorded as %+v", merge)
			}
			want := []string{f.destination(result, test.order[0]), f.destination(result, test.order[1])}
			if parents := f.parents(ctx, merge.Destination); !slices.Equal(parents, want) {
				t.Fatalf("the merge has parents %v, want %v in source parent order", parents, want)
			}
			if got, want := f.commitTree(ctx, merge.Destination), f.tree(ctx, "mainline"); got != want {
				t.Fatalf("the merge records tree %s, want %s", got, want)
			}
			if same := f.commitTree(ctx, merge.Destination) == f.commitTree(ctx, want[0]); same != test.sameTreeAsFirstParent {
				t.Fatalf("the merge records its first parent's tree: %t, want %t", same, test.sameTreeAsFirstParent)
			}
			if result.Written != 4 {
				t.Fatalf("wrote %d commits, want the three commits and the merge", result.Written)
			}
		})
	}
}

// TestMergeCollapsedWhenSideAddsNothing drops a merge whose side generated
// nothing, because a merge of a commit that its first parent already contains is
// a merge with nothing to merge.
func TestMergeCollapsedWhenSideAddsNothing(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newFixture(ctx, t, gitcli.ObjectFormatSHA1)
	f.add(ctx, "a", "first")
	f.add(ctx, "b", "mainline", "a")
	f.add(ctx, "s", "first", "a")
	f.add(ctx, "m", "mainline", "b", "s")

	result := f.run(ctx, f.options(f.newProjection("").transform))

	if result.Written != 2 || result.Collapsed != 2 {
		t.Fatalf("wrote %d and collapsed %d commits, want 2 and 2", result.Written, result.Collapsed)
	}
	mainline := f.destination(result, "b")
	if got := f.destination(result, "s"); got != f.destination(result, "a") {
		t.Fatalf("the side commit mapped to %s, want the commit it branched from", f.name(got))
	}
	merge := f.record(result, "m")
	if !merge.Collapsed || merge.Merge {
		t.Fatalf("the merge was recorded as %+v, want a collapse", merge)
	}
	if merge.Destination != mainline {
		t.Fatalf("the merge mapped to %s, want the mainline commit %s", merge.Destination, mainline)
	}
	if !slices.Equal(merge.MappedParents, []string{mainline}) {
		t.Fatalf("the merge kept parents %v, want only the mainline commit", merge.MappedParents)
	}
}

// TestMappedParentsDeduped collapses a merge's parents onto one destination
// commit when both sides produced nothing of their own.
func TestMappedParentsDeduped(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newFixture(ctx, t, gitcli.ObjectFormatSHA1)
	f.add(ctx, "a", "first")
	f.add(ctx, "x", "first", "a")
	f.add(ctx, "y", "first", "a")
	f.add(ctx, "m", "second", "x", "y")

	result := f.run(ctx, f.options(f.newProjection("").transform))

	base := f.destination(result, "a")
	merge := f.record(result, "m")
	if merge.Merge || merge.Collapsed {
		t.Fatalf("the merge was recorded as %+v, want an ordinary commit", merge)
	}
	if !slices.Equal(merge.MappedParents, []string{base}) {
		t.Fatalf("the merge kept parents %v, want the one commit both sides mapped to", merge.MappedParents)
	}
	if parents := f.parents(ctx, merge.Destination); !slices.Equal(parents, []string{base}) {
		t.Fatalf("the written commit has parents %v, want [%s]", parents, base)
	}
}

// TestRootWithoutContent leaves a source commit that generated nothing unmapped,
// so the first commit that does generate something becomes the root.
func TestRootWithoutContent(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newFixture(ctx, t, gitcli.ObjectFormatSHA1)
	f.add(ctx, "empty", "")
	f.add(ctx, "first", "content", "empty")

	result := f.run(ctx, f.options(f.newProjection("").transform))

	empty := f.record(result, "empty")
	if !empty.Collapsed || empty.Destination != "" {
		t.Fatalf("the empty commit was recorded as %+v, want a collapse onto nothing", empty)
	}
	if _, mapped := result.Mapping.Destination(f.sha("empty")); mapped {
		t.Fatal("a commit that generated nothing was mapped")
	}
	root := f.destination(result, "first")
	if parents := f.parents(ctx, root); len(parents) != 0 {
		t.Fatalf("the first generated commit has parents %v, want none", parents)
	}
}

func TestForcedUnchangedCommitPreservesReleaseBoundary(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newFixture(ctx, t, gitcli.ObjectFormatSHA1)
	f.add(ctx, "base", "content")
	f.add(ctx, "release", "content", "base")

	projection := f.newProjection("")
	opts := f.options(func(ctx context.Context, source replay.Commit) (replay.Transformed, error) {
		transformed, err := projection.transform(ctx, source)
		if err != nil {
			return replay.Transformed{}, err
		}
		transformed.Force = source.SHA == f.sha("release")
		return transformed, nil
	})
	result := f.run(ctx, opts)
	releaseRecord := f.record(result, "release")
	if releaseRecord.Collapsed || releaseRecord.Destination == "" {
		t.Fatalf("forced release record = %+v, want a written commit", releaseRecord)
	}
	if releaseRecord.Tree != f.record(result, "base").Tree {
		t.Errorf("forced release tree = %s, base tree = %s", releaseRecord.Tree, f.record(result, "base").Tree)
	}
	if parents := f.parents(ctx, releaseRecord.Destination); !slices.Equal(parents, []string{f.destination(result, "base")}) {
		t.Errorf("forced release parents = %v, want base destination", parents)
	}
}

// TestEpochParent extends published history under a second profile epoch.
//
// The second run re-transforms a commit the first run already published. It has
// to recognise that the content is what the epoch parent already records and
// collapse onto it, because writing it again would republish history that other
// people already have.
func TestEpochParent(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newFixture(ctx, t, gitcli.ObjectFormatSHA1)
	f.add(ctx, "a", "first")
	f.add(ctx, "b", "second", "a")

	first := f.options(f.newProjection("").transform)
	first.Heads = []string{f.sha("a")}
	published := f.destination(f.run(ctx, first), "a")

	second := f.options(f.newProjection("first").transform)
	second.Epoch = replay.Epoch{ProfileHash: profileHash, Parent: published}
	result := f.run(ctx, second)

	if got := f.destination(result, "a"); got != published {
		t.Fatalf("the already published commit mapped to %s, want the epoch parent %s", got, published)
	}
	if result.Written != 1 || result.Collapsed != 1 {
		t.Fatalf("wrote %d and collapsed %d commits, want 1 and 1", result.Written, result.Collapsed)
	}
	extended := f.destination(result, "b")
	if parents := f.parents(ctx, extended); !slices.Equal(parents, []string{published}) {
		t.Fatalf("the new commit has parents %v, want [%s]", parents, published)
	}
}

// TestEpochParentAdoptsUnmappedCommit attaches a commit the mapping placed
// nowhere to the epoch parent, so a new epoch continues the published history
// instead of starting a second root beside it.
func TestEpochParentAdoptsUnmappedCommit(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newFixture(ctx, t, gitcli.ObjectFormatSHA1)
	f.add(ctx, "a", "first")
	f.add(ctx, "b", "second", "a")

	first := f.options(f.newProjection("").transform)
	first.Heads = []string{f.sha("a")}
	published := f.destination(f.run(ctx, first), "a")

	// The second run replays only the later commit, so its source parent is
	// outside the run entirely and nothing maps it.
	second := f.options(f.newProjection("first").transform)
	second.Epoch = replay.Epoch{ProfileHash: profileHash, Parent: published}
	second.Anchor = f.sha("b")
	result := f.run(ctx, second)

	if parents := f.parents(ctx, f.destination(result, "b")); !slices.Equal(parents, []string{published}) {
		t.Fatalf("the commit has parents %v, want the epoch parent [%s]", parents, published)
	}
}

// TestRerunIsIdentical replays one history twice into repositories at different
// paths and requires the same objects.
//
// This is the property publication rests on. An append only history can only be
// resumed if regenerating a commit produces the commit that is already there, so
// nothing in a run may depend on a path, a clock, or an iteration order.
func TestRerunIsIdentical(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	build := func() *replay.Result {
		f := newFixture(ctx, t, gitcli.ObjectFormatSHA1)
		f.add(ctx, "a", "first")
		f.add(ctx, "b", "first", "a")
		f.add(ctx, "c", "mainline", "b")
		f.add(ctx, "s", "side", "b")
		f.add(ctx, "m", "mainline", "c", "s")
		return f.run(ctx, f.options(f.newProjection("").transform))
	}

	first, second := build(), build()
	if !slices.Equal(first.Report(), second.Report()) {
		t.Fatalf("the two runs report differently:\n%s\n\n%s",
			strings.Join(first.Report(), "\n"), strings.Join(second.Report(), "\n"))
	}
	if !slices.Equal(first.Heads, second.Heads) {
		t.Fatalf("the two runs produced heads %v and %v", first.Heads, second.Heads)
	}
	for i, record := range first.Records {
		if other := second.Records[i]; record.Destination != other.Destination || record.Tree != other.Tree {
			t.Fatalf("record %d differs: %+v and %+v", i, record, other)
		}
	}
}

// TestTransformRefusals covers every way a transform can report something the
// run must not commit.
//
// A transform is caller code, and what it reports decides what is published
// under a provenance trailer that claims a source commit produced it. Every one
// of these would publish a commit that lies about its own content.
func TestTransformRefusals(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		// mutate adjusts a valid result into the one under test.
		mutate func(f *fixture, source replay.Commit, transformed *replay.Transformed)
		want   error
	}{
		{
			name: "a result for another commit",
			mutate: func(f *fixture, _ replay.Commit, transformed *replay.Transformed) {
				transformed.Source = f.sha("a")
			},
			want: replay.ErrTransformSource,
		},
		{
			name: "a tree that is not an object name",
			mutate: func(_ *fixture, _ replay.Commit, transformed *replay.Transformed) {
				transformed.Tree = "HEAD^{tree}"
			},
		},
		{
			name: "an object that is not a tree",
			mutate: func(f *fixture, _ replay.Commit, transformed *replay.Transformed) {
				transformed.Tree = f.sha("a")
			},
			want: replay.ErrTransformTree,
		},
		{
			name: "a tree the repository does not hold",
			mutate: func(_ *fixture, _ replay.Commit, transformed *replay.Transformed) {
				transformed.Tree = strings.Repeat("ab", len(transformed.Tree)/2)
			},
			want: replay.ErrTransformTree,
		},
		{
			name: "a change that produced the baseline tree",
			mutate: func(_ *fixture, _ replay.Commit, transformed *replay.Transformed) {
				transformed.Changed = true
			},
			want: replay.ErrTransformChange,
		},
		{
			name: "no change beside a tree that differs",
			mutate: func(f *fixture, source replay.Commit, transformed *replay.Transformed) {
				transformed.Tree = f.tree(t.Context(), "something else")
				transformed.Changed = false
			},
			want: replay.ErrTransformChange,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			f := newFixture(ctx, t, gitcli.ObjectFormatSHA1)
			f.add(ctx, "a", "first")
			// The second commit changes nothing, so a valid result for it is the
			// baseline tree with no change and every mutation above turns it into
			// exactly one refusal.
			f.add(ctx, "b", "first", "a")

			projection := f.newProjection("")
			opts := f.options(func(ctx context.Context, source replay.Commit) (replay.Transformed, error) {
				transformed, err := projection.transform(ctx, source)
				if err != nil || source.SHA != f.sha("b") {
					return transformed, err
				}
				test.mutate(f, source, &transformed)
				return transformed, nil
			})

			result, err := replay.Run(ctx, f.dest, opts)
			if err == nil {
				t.Fatal("the result was accepted")
			}
			if test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("replay failed with %v, want %v", err, test.want)
			}
			if result == nil || result.Written != 1 {
				t.Fatalf("the failed run reported %+v, want the one commit written before it", result)
			}
		})
	}
}

// TestTransformError reports the failure and how far the run got.
//
// The commits already written are real objects. Nothing points at them, so they
// cost only disk, but a caller deciding what to do next is entitled to know they
// exist and which source commits they came from.
func TestTransformError(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newFixture(ctx, t, gitcli.ObjectFormatSHA1)
	f.add(ctx, "a", "first")
	f.add(ctx, "b", "second", "a")
	f.add(ctx, "c", "third", "b")

	failure := errors.New("the closure could not be resolved")
	projection := f.newProjection("")
	projection.hook = func(call int, _ replay.Commit) error {
		if call == 3 {
			return failure
		}
		return nil
	}

	result, err := replay.Run(ctx, f.dest, f.options(projection.transform))
	if !errors.Is(err, failure) {
		t.Fatalf("replay failed with %v, want the transform's error", err)
	}
	if result.Written != 2 || len(result.Records) != 2 {
		t.Fatalf("the failed run wrote %d commits and reported %d records, want 2 and 2",
			result.Written, len(result.Records))
	}
	if result.Records[1].Source != f.sha("b") {
		t.Fatalf("the last record is for %s, want the commit before the failure", f.name(result.Records[1].Source))
	}
}

// TestCancellation stops between commits and reports what was already written.
func TestCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	f := newFixture(ctx, t, gitcli.ObjectFormatSHA1)
	f.add(ctx, "a", "first")
	f.add(ctx, "b", "second", "a")
	f.add(ctx, "c", "third", "b")

	projection := f.newProjection("")
	projection.hook = func(call int, _ replay.Commit) error {
		if call == 3 {
			cancel()
		}
		return nil
	}

	result, err := replay.Run(ctx, f.dest, f.options(projection.transform))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("replay failed with %v, want a cancellation", err)
	}
	if result.Written != 2 {
		t.Fatalf("the cancelled run wrote %d commits, want the 2 completed before the cancellation", result.Written)
	}
}

// TestRecordsDoNotAliasTransformResults copies what a transform reported.
//
// A transform that reuses a buffer between calls is ordinary Go, and a result
// that held on to it would report the last commit's evidence for every commit.
func TestRecordsDoNotAliasTransformResults(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newFixture(ctx, t, gitcli.ObjectFormatSHA1)
	f.add(ctx, "a", "first")
	f.add(ctx, "b", "second", "a")

	reused := make([]string, 1)
	projection := f.newProjection("")
	opts := f.options(func(ctx context.Context, source replay.Commit) (replay.Transformed, error) {
		transformed, err := projection.transform(ctx, source)
		if err != nil {
			return transformed, err
		}
		reused[0] = "evidence for " + source.SHA
		transformed.Evidence = reused
		return transformed, nil
	})

	result := f.run(ctx, opts)
	for _, record := range result.Records {
		if want := []string{"evidence for " + record.Source}; !slices.Equal(record.Evidence, want) {
			t.Fatalf("record for %s carries evidence %v, want %v", f.name(record.Source), record.Evidence, want)
		}
	}
}

// TestOptionRefusals covers the inputs a run must refuse before it does any work.
//
// Each of these is checked up front rather than at the commit that would trip
// over it, because a transform is the expensive part of a replay and a run that
// performed thousands of them before reporting an unusable bot identity would be
// reporting the problem far from where it was introduced.
func TestOptionRefusals(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		// mutate turns valid options into the ones under test.
		mutate func(f *fixture, opts *replay.Options)
		want   error
		// message is a fragment of the expected refusal.
		message string
	}{
		{
			name:    "no transform",
			mutate:  func(_ *fixture, opts *replay.Options) { opts.Transform = nil },
			message: "a transform is required",
		},
		{
			name:    "no commits",
			mutate:  func(_ *fixture, opts *replay.Options) { opts.Commits = nil },
			message: "at least one source commit is required",
		},
		{
			name:    "no profile hash",
			mutate:  func(_ *fixture, opts *replay.Options) { opts.Epoch.ProfileHash = "" },
			want:    replay.ErrProfileHash,
			message: "a profile hash is required",
		},
		{
			name: "a profile hash of another algorithm",
			mutate: func(_ *fixture, opts *replay.Options) {
				opts.Epoch.ProfileHash = "sha1:" + strings.Repeat("ab", 20)
			},
			want:    replay.ErrProfileHash,
			message: `must begin with "sha256:"`,
		},
		{
			name: "a profile hash that is not a digest",
			mutate: func(_ *fixture, opts *replay.Options) {
				opts.Epoch.ProfileHash = "sha256:not-a-digest"
			},
			want:    replay.ErrProfileHash,
			message: "want 64",
		},
		{
			name:    "no bot email",
			mutate:  func(_ *fixture, opts *replay.Options) { opts.Bot.Email = "" },
			message: "a bot name and email are required",
		},
		{
			name:    "no provenance key",
			mutate:  func(_ *fixture, opts *replay.Options) { opts.ProvenanceKey = "" },
			message: "a trailer key is required",
		},
		{
			name:    "a provenance key git would not read",
			mutate:  func(_ *fixture, opts *replay.Options) { opts.ProvenanceKey = "Kubernetes_commit" },
			message: "letters, digits, and dashes",
		},
		{
			name: "an epoch parent that is not a commit",
			mutate: func(f *fixture, opts *replay.Options) {
				opts.Epoch.Parent = f.tree(t.Context(), "first")
			},
			message: "want a commit",
		},
		{
			name: "an abbreviated epoch parent",
			mutate: func(f *fixture, opts *replay.Options) {
				opts.Epoch.Parent = f.sha("a")[:12]
			},
			message: "hexadecimal characters",
		},
		{
			name: "a source commit without an author",
			mutate: func(_ *fixture, opts *replay.Options) {
				opts.Commits = slices.Clone(opts.Commits)
				opts.Commits[0].Author.Email = ""
			},
			message: "an author name and email are required",
		},
		{
			name: "a source date git did not store",
			mutate: func(_ *fixture, opts *replay.Options) {
				opts.Commits = slices.Clone(opts.Commits)
				opts.Commits[0].Author.Date = "2026-08-12T10:00:00Z"
			},
			message: "author date",
		},
		{
			name: "a source commit without a message",
			mutate: func(_ *fixture, opts *replay.Options) {
				opts.Commits = slices.Clone(opts.Commits)
				opts.Commits[0].Message = "  \n"
			},
			message: "a message is required",
		},
		{
			name: "a duplicated source commit",
			mutate: func(_ *fixture, opts *replay.Options) {
				opts.Commits = append(slices.Clone(opts.Commits), opts.Commits[0])
			},
			message: "appears more than once",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			f := newFixture(ctx, t, gitcli.ObjectFormatSHA1)
			f.add(ctx, "a", "first")
			f.add(ctx, "b", "second", "a")

			opts := f.options(f.newProjection("").transform)
			test.mutate(f, &opts)

			result, err := replay.Run(ctx, f.dest, opts)
			if err == nil {
				t.Fatal("the options were accepted")
			}
			if result != nil {
				t.Fatalf("a refused run reported %+v, want no result at all", result)
			}
			if test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("replay failed with %v, want %v", err, test.want)
			}
			if !strings.Contains(err.Error(), test.message) {
				t.Fatalf("replay failed with %v, want a refusal mentioning %q", err, test.message)
			}
		})
	}
}

// TestBranchesShareTheirBase replays two branches that never merged.
//
// A destination repository tracks several release branches, and they diverge
// from the common transformed anchor rather than from one another, so each head
// has to land on its own commit while the history they share stays one chain.
func TestBranchesShareTheirBase(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newFixture(ctx, t, gitcli.ObjectFormatSHA1)
	f.add(ctx, "base", "first")
	f.add(ctx, "release", "release content", "base")
	f.add(ctx, "main", "main content", "base")

	result := f.run(ctx, f.options(f.newProjection("").transform))

	base := f.destination(result, "base")
	for _, name := range []string{"release", "main"} {
		if parents := f.parents(ctx, f.destination(result, name)); !slices.Equal(parents, []string{base}) {
			t.Fatalf("branch %s has parents %v, want the shared base [%s]", name, parents, base)
		}
	}
	// Heads keep the order the graph reports, which is the order the commits
	// were handed over, so a report of a run over several branches is stable.
	want := []replay.Head{
		{Source: f.sha("release"), Destination: f.destination(result, "release")},
		{Source: f.sha("main"), Destination: f.destination(result, "main")},
	}
	if !slices.Equal(result.Heads, want) {
		t.Fatalf("heads are %v, want %v", result.Heads, want)
	}
}

// TestReplayCreatesNoRef checks that a run writes objects and nothing else.
//
// Deciding what history should look like and deciding to publish it are separate
// gates, and only the second one can destroy anything. A replay that moved a ref
// would have made the second decision on its own, so the destination repository
// must hold exactly the refs it held before.
func TestReplayCreatesNoRef(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newFixture(ctx, t, gitcli.ObjectFormatSHA1)
	f.add(ctx, "a", "first")
	f.add(ctx, "b", "second", "a")

	before, err := f.dest.ListRefs(ctx)
	if err != nil {
		t.Fatalf("list refs: %v", err)
	}
	f.run(ctx, f.options(f.newProjection("").transform))
	after, err := f.dest.ListRefs(ctx)
	if err != nil {
		t.Fatalf("list refs: %v", err)
	}
	if !slices.Equal(before, after) {
		t.Fatalf("the destination refs changed from %v to %v", before, after)
	}
}

// TestReportIsDeterministic renders every outcome a record can hold.
func TestReportIsDeterministic(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newFixture(ctx, t, gitcli.ObjectFormatSHA1)
	f.add(ctx, "a", "first")
	f.add(ctx, "b", "first", "a")
	f.add(ctx, "c", "mainline", "b")
	f.add(ctx, "s", "side", "b")
	f.add(ctx, "m", "mainline", "c", "s")

	result := f.run(ctx, f.options(f.newProjection("").transform))
	report := result.Report()

	if want := "profile " + profileHash; report[0] != want {
		t.Fatalf("the first line is %q, want %q", report[0], want)
	}
	if want := "parent none"; report[1] != want {
		t.Fatalf("the second line is %q, want %q", report[1], want)
	}
	if want := "written 4 collapsed 1"; report[2] != want {
		t.Fatalf("the third line is %q, want %q", report[2], want)
	}
	kinds := make([]string, 0, len(result.Records))
	for _, line := range report[4:] {
		kinds = append(kinds, strings.Fields(line)[0])
	}
	if want := []string{"commit", "collapse", "commit", "commit", "merge"}; !slices.Equal(kinds, want) {
		t.Fatalf("the report describes %v, want %v", kinds, want)
	}
}
