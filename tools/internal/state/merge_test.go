package state_test

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/relocate"
	"github.com/enj/soapbox/tools/internal/state"
	"github.com/enj/soapbox/tools/internal/treebuild"
)

// signature is the fixed identity every fixture commit is written with. The
// date is a constant so a fixture commit's object name depends only on its tree
// and its parents.
var signature = gitcli.Signature{Name: "Soapbox", Email: "soapbox@example.invalid", Date: "1700000000 +0000"}

// newRepo creates a real repository whose objects use the named hash algorithm.
//
// HOME is redirected through Isolation rather than Env so the runner stays
// anonymous: the redirection decides where git looks for state and carries no
// credential.
func newRepo(ctx context.Context, t *testing.T, format gitcli.ObjectFormat) *gitcli.Runner {
	t.Helper()
	git, err := gitcli.New(ctx, gitcli.Options{
		Dir:       t.TempDir(),
		Inherit:   []string{"PATH"},
		Isolation: []string{"HOME=" + t.TempDir()},
	})
	if err != nil {
		t.Fatalf("create git runner: %v", err)
	}
	if err := git.InitRepositoryWithFormat(ctx, "main", format); err != nil {
		t.Fatalf("init %s repository: %v", format, err)
	}
	return git
}

// fileSet builds a one file tree that differs per label and index, so every
// fixture commit records a tree of its own.
func fileSet(label string, index int) relocate.FileSet {
	return relocate.FileSet{Files: []relocate.File{{
		Path:     "fixture.txt",
		Mode:     relocate.ModeRegular,
		Contents: []byte(label + " " + strconv.Itoa(index) + "\n"),
	}}}
}

// graph is a real commit graph the merge rules are proved against.
//
// Source commits and destination commits are two disjoint chains rather than
// one, because that is what they are: upstream history and the history the
// engine builds from it live in different repositories and share no object. A
// single chain would let a fixture use one commit as both the image of a source
// and the source of another image, which a record can never mean.
//
// Each chain is linear with one commit diverging from its root, which is the
// smallest shape that separates the three answers ancestry can give: chain[i]
// reaches every earlier entry, the fork reaches only chain[0], and nothing on
// one chain reaches anything on the other.
type graph struct {
	git    *gitcli.Runner
	format gitcli.ObjectFormat
	source []string
	dest   []string
	// srcFork and destFork descend from their chain's root only, so each is a
	// real commit that a position part way along its chain cannot move to.
	srcFork  string
	destFork string
}

// chainLength is how many commits each chain holds.
//
// Every role in the record needs a source and destination pair of its own, plus
// a further pair to advance into, because the correspondence rule refuses a
// document in which two roles disagree about what a commit became. The indices
// are handed out in document order by graph.document.
const chainLength = 10

// newGraph builds the two chains and their forks.
//
// The commits are written through the same object seam the engine publishes
// with, so the fixture exercises real objects rather than names that merely look
// like them. No ref points at any of them, which is also how the state commits
// this package writes exist.
func newGraph(ctx context.Context, t *testing.T, format gitcli.ObjectFormat) *graph {
	t.Helper()
	git := newRepo(ctx, t, format)
	g := &graph{git: git, format: format}

	g.source = g.chain(ctx, t, "source", chainLength)
	g.dest = g.chain(ctx, t, "destination", chainLength)
	g.srcFork = g.commit(ctx, t, "source fork", 0, []string{g.source[0]})
	g.destFork = g.commit(ctx, t, "destination fork", 0, []string{g.dest[0]})
	return g
}

// chain writes one linear run of commits and reports them in order.
func (g *graph) chain(ctx context.Context, t *testing.T, label string, length int) []string {
	t.Helper()
	var parents, chain []string
	for i := range length {
		sha := g.commit(ctx, t, label, i, parents)
		chain = append(chain, sha)
		parents = []string{sha}
	}
	return chain
}

// commit writes one fixture commit with a tree of its own.
func (g *graph) commit(ctx context.Context, t *testing.T, label string, index int, parents []string) string {
	t.Helper()
	manifest, err := treebuild.WriteFileSet(ctx, g.git, fileSet(label, index))
	if err != nil {
		t.Fatalf("write %s %d tree: %v", label, index, err)
	}
	sha, err := treebuild.WriteSyntheticCommit(ctx, g.git, treebuild.SyntheticCommitOptions{
		Tree:      manifest.Tree,
		Parents:   parents,
		Author:    signature,
		Committer: signature,
		Message:   label + " " + strconv.Itoa(index) + "\n",
	})
	if err != nil {
		t.Fatalf("write %s %d commit: %v", label, index, err)
	}
	return sha
}

// document is the record the graph's commits describe, canonicalised and
// digested.
//
// Every role gets a pair of indices of its own, and the pair one higher is where
// that role advances to, so a test can move one position without another role's
// claim about the same commit contradicting it.
func (g *graph) document(t *testing.T) state.Document {
	t.Helper()
	doc := base(g.format)
	doc.Anchor.Source = g.source[0]
	doc.Epoch.Source = g.source[1]
	doc.Epoch.Destination = g.dest[1]
	doc.Cursors = []state.Cursor{{
		Ref: "refs/heads/master", Source: g.source[2], Destination: g.dest[2],
	}}
	doc.Tracks = []state.Track{{
		Name: "release-1-36", Ref: state.ProgressNamespace + "release-1-36",
		Source: g.source[4], Destination: g.dest[4], Done: 3, Total: 9,
	}}
	doc.Published = []state.Published{{
		Ref: "refs/heads/main", Kind: state.KindBranch, Source: g.source[6], Object: g.dest[6],
	}, {
		Ref: "refs/tags/v0.36.1", Kind: state.KindTag, Source: g.source[8], Object: g.dest[8],
	}}
	return mustNew(t, doc)
}

// TestMergeRules states every successor rule as the smallest change to a record
// a resume would propose.
//
// Each case starts from a document the graph's real commits describe and builds
// the next one from it, which is exactly the shape a run produces: take what was
// recorded, advance what moved, hand both to the gate.
func TestMergeRules(t *testing.T) {
	for _, format := range objectFormats {
		t.Run(string(format), func(t *testing.T) {
			ctx := t.Context()
			g := newGraph(ctx, t, format)

			tests := []struct {
				name    string
				mutate  func(*state.Document)
				want    error
				message string
			}{{
				name: "an ordinary advance",
				mutate: func(d *state.Document) {
					d.Cursors[0].Source = g.source[3]
					d.Cursors[0].Destination = g.dest[3]
					d.Tracks[0].Source = g.source[5]
					d.Tracks[0].Destination = g.dest[5]
					d.Tracks[0].Done = 5
					d.Published[0].Source = g.source[7]
					d.Published[0].Object = g.dest[7]
					d.Mapping.Entries = 20
					d.Mapping.Digest = digest("mapping two")
					d.Mapping.Object = sha(format, "mapping object two")
				},
			}, {
				name:   "a record that advanced nothing",
				mutate: func(d *state.Document) {},
			}, {
				name: "a new tracked ref",
				mutate: func(d *state.Document) {
					d.Cursors = append(d.Cursors, state.Cursor{
						Ref: "refs/heads/release-1.36", Source: g.source[9], Destination: g.dest[9],
					})
				},
			}, {
				name: "a cursor source that rewinds",
				mutate: func(d *state.Document) {
					d.Cursors[0].Source = g.source[0]
				},
				want: state.ErrRewind,
			}, {
				name: "a cursor source that moves to a divergent commit",
				mutate: func(d *state.Document) {
					d.Cursors[0].Source = g.srcFork
				},
				want: state.ErrRewind,
			}, {
				name: "a cursor destination that rewinds",
				mutate: func(d *state.Document) {
					d.Cursors[0].Destination = g.dest[0]
				},
				want: state.ErrRewind,
			}, {
				name: "a dropped cursor",
				mutate: func(d *state.Document) {
					d.Cursors = nil
				},
				want: state.ErrDropped,
			}, {
				name: "a moved anchor",
				mutate: func(d *state.Document) {
					d.Anchor.Source = g.source[1]
				},
				want: state.ErrImmutable,
			}, {
				name: "a different destination repository",
				mutate: func(d *state.Document) {
					d.Destination.Repository = "enj/other"
				},
				want: state.ErrImmutable,
			}, {
				name: "a graft moved under an unchanged profile",
				mutate: func(d *state.Document) {
					d.Epoch.Destination = g.dest[3]
				},
				want:    state.ErrImmutable,
				message: "epoch destination",
			}, {
				name: "an epoch start moved under an unchanged profile",
				mutate: func(d *state.Document) {
					d.Epoch.Source = g.source[3]
				},
				want:    state.ErrImmutable,
				message: "epoch source",
			}, {
				name: "a mapping index that shrank",
				mutate: func(d *state.Document) {
					d.Mapping.Entries = 11
				},
				want: state.ErrRewind,
			}, {
				name: "a mapping index rewritten rather than extended",
				mutate: func(d *state.Document) {
					d.Mapping.Digest = digest("mapping rewritten")
					d.Mapping.Object = sha(format, "mapping rewritten object")
				},
				want:    state.ErrImmutable,
				message: "still resolves 12 source commits",
			}, {
				name: "a mapping index whose blob started digesting to something new",
				mutate: func(d *state.Document) {
					// The blob is unchanged, so its content is unchanged, so its
					// digest cannot have moved. One of the two is wrong and the
					// record cannot say which.
					d.Mapping.Digest = digest("mapping rewritten")
					d.Mapping.Entries = 20
				},
				want:    state.ErrCorrespondence,
				message: "that blob digested to",
			}, {
				name: "a mapping index whose digest turned up in another blob",
				mutate: func(d *state.Document) {
					d.Mapping.Object = sha(format, "mapping elsewhere")
					d.Mapping.Entries = 20
				},
				want:    state.ErrCorrespondence,
				message: "that digest was stored in",
			}, {
				name: "a mapping index whose digest started resolving more commits",
				mutate: func(d *state.Document) {
					d.Mapping.Entries = 20
				},
				want:    state.ErrCorrespondence,
				message: "that digest resolved 12",
			}, {
				name: "a track that un-did work",
				mutate: func(d *state.Document) {
					d.Tracks[0].Done = 2
				},
				want: state.ErrRewind,
			}, {
				name: "a track that shrank its total",
				mutate: func(d *state.Document) {
					d.Tracks[0].Total = 8
				},
				want: state.ErrRewind,
			}, {
				name: "a track that grew its total",
				mutate: func(d *state.Document) {
					d.Tracks[0].Total = 40
				},
			}, {
				name: "an unfinished track that disappeared",
				mutate: func(d *state.Document) {
					d.Tracks = nil
				},
				want: state.ErrDropped,
			}, {
				name: "a track that finished",
				mutate: func(d *state.Document) {
					d.Tracks[0].Done = d.Tracks[0].Total
					d.Tracks[0].Source = g.source[5]
					d.Tracks[0].Destination = g.dest[5]
				},
			}, {
				name: "a track moved to another ref",
				mutate: func(d *state.Document) {
					d.Tracks[0].Name = "release-1-37"
					d.Tracks[0].Ref = state.ProgressNamespace + "release-1-37"
				},
				want:    state.ErrDropped,
				message: "release-1-36",
			}, {
				name: "a published branch that advances",
				mutate: func(d *state.Document) {
					d.Published[0].Source = g.source[7]
					d.Published[0].Object = g.dest[7]
				},
			}, {
				name: "a published branch that is not a fast forward",
				mutate: func(d *state.Document) {
					d.Published[0].Object = g.destFork
				},
				want: state.ErrRewind,
			}, {
				name: "a published branch that fast forwards over a rewound source",
				mutate: func(d *state.Document) {
					// The object moves forward, which on its own reads as an
					// ordinary advance. The source going backwards under it is
					// what this catches: the branch would keep claiming it was
					// published from history it no longer covers.
					d.Published[0].Source = g.source[0]
					d.Published[0].Object = g.dest[7]
				},
				want:    state.ErrRewind,
				message: "refs/heads/main source",
			}, {
				name: "a published branch that fast forwards onto a divergent source",
				mutate: func(d *state.Document) {
					d.Published[0].Source = g.srcFork
					d.Published[0].Object = g.dest[7]
				},
				want:    state.ErrRewind,
				message: "refs/heads/main source",
			}, {
				name: "a dropped published branch",
				mutate: func(d *state.Document) {
					d.Published = d.Published[1:]
				},
				want: state.ErrDropped,
			}, {
				name: "a tag that moved forward",
				mutate: func(d *state.Document) {
					d.Published[1].Object = g.dest[9]
				},
				want: state.ErrTagMoved,
			}, {
				name: "a tag republished from a later source",
				mutate: func(d *state.Document) {
					// The tag still points where it did, so nothing a consumer
					// resolves has changed, but the record would now name a
					// different upstream commit as what that tag contains.
					d.Published[1].Source = g.source[9]
				},
				want:    state.ErrTagMoved,
				message: "now published from",
			}, {
				name: "a tag that disappeared",
				mutate: func(d *state.Document) {
					d.Published = d.Published[:1]
				},
				want: state.ErrTagMoved,
			}, {
				name: "a tag re-labelled as a branch",
				mutate: func(d *state.Document) {
					d.Published[1].Kind = state.KindBranch
					d.Published[1].Ref = "refs/heads/v0.36.1"
				},
				want:    state.ErrTagMoved,
				message: "refs/tags/v0.36.1",
			}}

			for _, test := range tests {
				t.Run(test.name, func(t *testing.T) {
					prev := g.document(t)
					next := prev.Clone()
					next.Digest = ""
					test.mutate(&next)

					merged, err := state.Merge(ctx, prev, next, g.git)
					if test.want == nil && test.message == "" {
						if err != nil {
							t.Fatalf("merge: %v, want acceptance", err)
						}
						if err := merged.Validate(); err != nil {
							t.Fatalf("the accepted successor does not validate: %v", err)
						}
						return
					}
					if err == nil {
						t.Fatalf("merge accepted the successor")
					}
					if test.want != nil && !errors.Is(err, test.want) {
						t.Fatalf("merge: %v, want %v", err, test.want)
					}
					if test.message != "" && !strings.Contains(err.Error(), test.message) {
						t.Fatalf("merge: %v, want a message holding %q", err, test.message)
					}
				})
			}
		})
	}
}

// TestMergeAcrossAnEpoch covers the one case where destination history is
// allowed to move sideways.
//
// A profile change re-derives every transformed commit, so the destination
// commits in the next record are a new line rather than a continuation. What
// must not relax with it is the source side, which still only moves forward, or
// a published tag, which a consumer has already resolved.
func TestMergeAcrossAnEpoch(t *testing.T) {
	ctx := t.Context()
	g := newGraph(ctx, t, gitcli.ObjectFormatSHA1)
	prev := g.document(t)

	reGrafted := prev.Clone()
	reGrafted.Digest = ""
	reGrafted.Epoch.Profile = digest("profile two")
	reGrafted.Epoch.Source = g.source[3]
	reGrafted.Epoch.Destination = g.dest[3]
	// The destination is re-derived onto the fork, which the recorded
	// destination does not reach.
	reGrafted.Cursors[0].Source = g.source[3]
	reGrafted.Cursors[0].Destination = g.destFork
	if _, err := state.Merge(ctx, prev, reGrafted, g.git); err != nil {
		t.Fatalf("merge across an epoch: %v", err)
	}

	rewound := reGrafted.Clone()
	rewound.Cursors[0].Source = g.source[0]
	if _, err := state.Merge(ctx, prev, rewound, g.git); !errors.Is(err, state.ErrRewind) {
		t.Fatalf("merge with a rewound source across an epoch: %v, want %v", err, state.ErrRewind)
	}

	movedTag := reGrafted.Clone()
	movedTag.Published[1].Object = g.dest[9]
	if _, err := state.Merge(ctx, prev, movedTag, g.git); !errors.Is(err, state.ErrTagMoved) {
		t.Fatalf("merge with a moved tag across an epoch: %v, want %v", err, state.ErrTagMoved)
	}

	// A published branch's source is upstream history, which no profile change
	// re-derives, so it is still required to move forward across an epoch.
	rewoundBranch := reGrafted.Clone()
	rewoundBranch.Published[0].Source = g.source[0]
	if _, err := state.Merge(ctx, prev, rewoundBranch, g.git); !errors.Is(err, state.ErrRewind) {
		t.Fatalf("merge with a rewound branch source across an epoch: %v, want %v", err, state.ErrRewind)
	}

	slippedGraft := prev.Clone()
	slippedGraft.Digest = ""
	slippedGraft.Epoch.Profile = digest("profile two")
	slippedGraft.Epoch.Destination = g.destFork
	if _, err := state.Merge(ctx, prev, slippedGraft, g.git); !errors.Is(err, state.ErrRewind) {
		t.Fatalf("merge with a graft off the published line: %v, want %v", err, state.ErrRewind)
	}
}

type restrictedAncestry struct {
	inner   state.Ancestry
	allowed map[string]bool
}

func (a restrictedAncestry) IsAncestor(ctx context.Context, ancestor, descendant string) (bool, error) {
	if !a.allowed[ancestor] || !a.allowed[descendant] {
		return false, errors.New("ancestry question reached the wrong repository")
	}
	return a.inner.IsAncestor(ctx, ancestor, descendant)
}

func TestMergeWithUsesSeparateSourceAndDestinationGraphs(t *testing.T) {
	ctx := t.Context()
	g := newGraph(ctx, t, gitcli.ObjectFormatSHA1)
	prev := g.document(t)
	next := prev.Clone()
	next.Digest = ""
	next.Epoch.Profile = digest("separate graph profile")
	next.Epoch.Source = g.source[3]
	next.Epoch.Destination = g.dest[3]
	next.Cursors[0].Source = g.source[3]
	next.Cursors[0].Destination = g.destFork

	sourceAllowed := map[string]bool{g.srcFork: true}
	for _, commit := range g.source {
		sourceAllowed[commit] = true
	}
	destinationAllowed := map[string]bool{g.destFork: true}
	for _, commit := range g.dest {
		destinationAllowed[commit] = true
	}
	merged, err := state.MergeWith(ctx, prev, next,
		restrictedAncestry{inner: g.git, allowed: sourceAllowed},
		restrictedAncestry{inner: g.git, allowed: destinationAllowed})
	if err != nil {
		t.Fatalf("merge with separate graphs: %v", err)
	}
	if merged.Epoch != next.Epoch {
		t.Errorf("merged epoch = %#v, want %#v", merged.Epoch, next.Epoch)
	}
}

// TestMergeAcceptsClosingAFinishedTrack checks the one removal that is not a
// loss.
//
// Finishing a track is what deleting its progress ref means: the chunks landed,
// nothing unreachable is left behind, and the record should stop carrying it.
// An unfinished track's removal is covered by the rule table; this needs a
// previous record in which the track is already complete.
func TestMergeAcceptsClosingAFinishedTrack(t *testing.T) {
	ctx := t.Context()
	g := newGraph(ctx, t, gitcli.ObjectFormatSHA1)

	finished := g.document(t).Clone()
	finished.Digest = ""
	finished.Tracks[0].Done = finished.Tracks[0].Total
	prev := mustNew(t, finished)

	closed := prev.Clone()
	closed.Digest = ""
	closed.Tracks = nil
	merged, err := state.Merge(ctx, prev, closed, g.git)
	if err != nil {
		t.Fatalf("merge closing a finished track: %v", err)
	}
	if len(merged.Tracks) != 0 {
		t.Fatalf("the accepted successor still holds %d tracks", len(merged.Tracks))
	}
}

// TestMergeAcceptsAnAlreadyCanonicalSuccessor checks the other way a caller
// arrives: with a successor it has already run through New.
//
// A candidate that carries a digest is verified rather than re-digested, so a
// caller that built its next record properly is not made to strip the digest
// first, and one whose record was modified after New is still caught.
func TestMergeAcceptsAnAlreadyCanonicalSuccessor(t *testing.T) {
	ctx := t.Context()
	g := newGraph(ctx, t, gitcli.ObjectFormatSHA1)
	prev := g.document(t)

	built := prev.Clone()
	built.Digest = ""
	built.Cursors[0].Source = g.source[3]
	digested := mustNew(t, built)

	merged, err := state.Merge(ctx, prev, digested, g.git)
	if err != nil {
		t.Fatalf("merge a digested successor: %v", err)
	}
	if merged.Digest != digested.Digest {
		t.Fatalf("merge re-digested the successor to %s, want %s", merged.Digest, digested.Digest)
	}

	tampered := digested.Clone()
	tampered.Cursors[0].Destination = g.dest[3]
	if _, err := state.Merge(ctx, prev, tampered, g.git); !errors.Is(err, state.ErrDigest) {
		t.Fatalf("merge: %v, want %v", err, state.ErrDigest)
	}
}

// countingAncestry delegates every question to a real repository and records how
// many it was asked. It answers nothing itself, so the rules stay proved against
// real commits.
type countingAncestry struct {
	inner state.Ancestry
	calls int
}

func (c *countingAncestry) IsAncestor(ctx context.Context, ancestor, descendant string) (bool, error) {
	c.calls++
	return c.inner.IsAncestor(ctx, ancestor, descendant)
}

// TestMergeAsksOnlyAboutPositionsThatMoved checks the short circuit that keeps a
// resume cheap.
//
// A backfill merges a record after every chunk, and most positions are unchanged
// each time. Asking the repository about them would spend a subprocess per
// cursor per chunk to be told a commit is its own ancestor.
func TestMergeAsksOnlyAboutPositionsThatMoved(t *testing.T) {
	ctx := t.Context()
	g := newGraph(ctx, t, gitcli.ObjectFormatSHA1)
	prev := g.document(t)

	quiet := &countingAncestry{inner: g.git}
	unchanged := prev.Clone()
	unchanged.Digest = ""
	if _, err := state.Merge(ctx, prev, unchanged, quiet); err != nil {
		t.Fatalf("merge an unchanged record: %v", err)
	}
	if quiet.calls != 0 {
		t.Fatalf("an unchanged record asked the repository %d questions, want none", quiet.calls)
	}

	busy := &countingAncestry{inner: g.git}
	advanced := prev.Clone()
	advanced.Digest = ""
	advanced.Cursors[0].Source = g.source[3]
	if _, err := state.Merge(ctx, prev, advanced, busy); err != nil {
		t.Fatalf("merge an advanced record: %v", err)
	}
	if busy.calls != 1 {
		t.Fatalf("one advanced cursor asked %d questions, want 1", busy.calls)
	}
}

// TestMergeStopsOnACancelledContext checks that an abandoned run cannot still
// accept a successor.
//
// Without the check at the top, a merge that advanced nothing would ask the
// repository nothing, and so would never reach the cancellation the caller
// already signalled.
func TestMergeStopsOnACancelledContext(t *testing.T) {
	g := newGraph(t.Context(), t, gitcli.ObjectFormatSHA1)
	prev := g.document(t)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	next := prev.Clone()
	next.Digest = ""
	if _, err := state.Merge(ctx, prev, next, g.git); !errors.Is(err, context.Canceled) {
		t.Fatalf("merge: %v, want %v", err, context.Canceled)
	}
}

// TestMergeReportsAnUnanswerableQuestion checks that a repository which cannot
// answer is a refusal rather than a silent acceptance.
func TestMergeReportsAnUnanswerableQuestion(t *testing.T) {
	ctx := t.Context()
	g := newGraph(ctx, t, gitcli.ObjectFormatSHA1)
	prev := g.document(t)

	next := prev.Clone()
	next.Digest = ""
	// A well formed name for an object this repository does not hold. The
	// question is real, and git cannot answer it.
	next.Cursors[0].Source = strings.Repeat("b", g.format.HexLength())
	_, err := state.Merge(ctx, prev, next, g.git)
	if err == nil {
		t.Fatalf("merge accepted a move to an object the repository does not hold")
	}
	if !strings.Contains(err.Error(), "ancestry") {
		t.Fatalf("merge: %v, want a message naming the ancestry question", err)
	}
}

// TestMergeRefusesAnUnusableCall covers the two inputs a merge cannot proceed
// from: no ancestry checker, and a previous record that does not validate.
func TestMergeRefusesAnUnusableCall(t *testing.T) {
	ctx := t.Context()
	g := newGraph(ctx, t, gitcli.ObjectFormatSHA1)
	prev := g.document(t)

	next := prev.Clone()
	next.Digest = ""
	if _, err := state.Merge(ctx, prev, next, nil); err == nil {
		t.Fatalf("merge accepted a call with no ancestry checker")
	}

	tampered := prev.Clone()
	tampered.Destination.Module = "monis.app/kk/other"
	if _, err := state.Merge(ctx, tampered, next, g.git); !errors.Is(err, state.ErrDigest) {
		t.Fatalf("merge: %v, want %v", err, state.ErrDigest)
	}
}

// TestMergeReturnsAnIndependentDocument checks that the accepted successor is
// the caller's own, canonicalised and digested on the way through.
//
// A caller hands in a candidate it built by cloning and editing, and it commonly
// keeps editing afterwards. A result sharing that candidate's slices would let
// those later edits reach a record that has already been accepted.
func TestMergeReturnsAnIndependentDocument(t *testing.T) {
	ctx := t.Context()
	g := newGraph(ctx, t, gitcli.ObjectFormatSHA1)
	prev := g.document(t)

	next := prev.Clone()
	next.Digest = ""
	next.Cursors[0].Source = g.source[3]
	// The candidate arrives out of order, which is what a caller appending a
	// newly discovered ref produces.
	next.Cursors = append(next.Cursors, state.Cursor{
		Ref: "refs/heads/main", Source: g.source[9], Destination: g.dest[9],
	})

	merged, err := state.Merge(ctx, prev, next, g.git)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if merged.Digest == "" {
		t.Fatalf("the accepted successor carries no digest")
	}
	if err := merged.Validate(); err != nil {
		t.Fatalf("the accepted successor does not validate: %v", err)
	}
	if merged.Cursors[0].Ref != "refs/heads/main" {
		t.Fatalf("the accepted successor was not sorted: %v", merged.Cursors)
	}

	next.Cursors[0].Source = g.srcFork
	if merged.Cursors[1].Source == g.srcFork {
		t.Fatalf("the accepted successor shares its cursors with the candidate")
	}
	if err := merged.Validate(); err != nil {
		t.Fatalf("validate after the caller mutated its candidate: %v", err)
	}
}
