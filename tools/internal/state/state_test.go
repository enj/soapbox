package state_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/state"
)

// objectFormats are the hash algorithms every property is proved under.
//
// sha256 is not hypothetical. It is a repository creation time choice that
// cannot be changed afterwards, so a width rule that only ever ran under sha1
// would be wrong in exactly the repository nobody tested.
var objectFormats = []gitcli.ObjectFormat{gitcli.ObjectFormatSHA1, gitcli.ObjectFormatSHA256}

// sha renders a distinct object name of the width a format uses, so a fixture
// can name many commits without any of them being a literal nobody can tell
// apart from another.
func sha(format gitcli.ObjectFormat, seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])[:format.HexLength()]
}

// digest renders a canonical content digest.
func digest(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// base is a fully populated document with every list occupied, so a mutation
// applied to it exercises the rule it names rather than an empty list.
func base(format gitcli.ObjectFormat) state.Document {
	return state.Document{
		Schema:       state.Schema,
		ObjectFormat: format,
		Destination: state.Destination{
			Repository: "enj/rbac_authorizer",
			Module:     "monis.app/kk/rbac_authorizer",
		},
		Anchor: state.Anchor{
			Source: sha(format, "anchor"),
			Ref:    "refs/tags/v1.36.1",
		},
		Epoch: state.Epoch{
			Profile:     digest("profile one"),
			Source:      sha(format, "epoch source"),
			Destination: sha(format, "epoch destination"),
		},
		Cursors: []state.Cursor{{
			Ref:         "refs/heads/master",
			Source:      sha(format, "master source"),
			Destination: sha(format, "master destination"),
		}},
		Mapping: state.Mapping{
			Digest:  digest("mapping one"),
			Object:  sha(format, "mapping object"),
			Entries: 12,
		},
		Tracks: []state.Track{{
			Name:        "release-1-36",
			Ref:         state.ProgressNamespace + "release-1-36",
			Source:      sha(format, "track source"),
			Destination: sha(format, "track destination"),
			Done:        3,
			Total:       9,
		}},
		Published: []state.Published{{
			Ref:    "refs/heads/main",
			Kind:   state.KindBranch,
			Object: sha(format, "main object"),
			Source: sha(format, "main source"),
		}},
		Engine: state.Engine{Version: "v0.1.0", Toolchain: "go1.26.5"},
	}
}

// mustNew builds the canonical form of a document a test expects to be valid.
func mustNew(t *testing.T, doc state.Document) state.Document {
	t.Helper()
	built, err := state.New(doc)
	if err != nil {
		t.Fatalf("new document: %v", err)
	}
	return built
}

// TestNewAcceptsAFullyPopulatedDocument is the fixture's own check. Every
// refusal below is stated as a mutation of this document, so a fixture that was
// quietly invalid would make each of those tests pass for the wrong reason.
func TestNewAcceptsAFullyPopulatedDocument(t *testing.T) {
	for _, format := range objectFormats {
		t.Run(string(format), func(t *testing.T) {
			doc := mustNew(t, base(format))
			if err := doc.Validate(); err != nil {
				t.Fatalf("validate: %v", err)
			}
			if !strings.HasPrefix(doc.Digest, "sha256:") || len(doc.Digest) != len("sha256:")+64 {
				t.Fatalf("digest %q is not the canonical form", doc.Digest)
			}
		})
	}
}

// TestNewAcceptsAFirstRun covers the shape a repository has before it has done
// anything: an epoch with nothing to graft onto, no cursors, an index that
// resolves nothing, a track that has produced nothing, and nothing published.
//
// It is the one document the engine has to be able to write before it can write
// any other, so a rule that only made sense once work existed would block the
// very first run.
func TestNewAcceptsAFirstRun(t *testing.T) {
	doc := base(gitcli.ObjectFormatSHA1)
	doc.Epoch.Destination = ""
	doc.Cursors = nil
	doc.Mapping = state.Mapping{}
	doc.Tracks = []state.Track{{Name: "backfill", Ref: "refs/soapbox/progress/backfill", Total: 400}}
	doc.Published = nil

	built := mustNew(t, doc)
	if err := built.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

// TestValidateRefusals states every rule as the smallest change to a valid
// document that breaks it.
//
// The mutation is applied after the document has been canonicalised and
// digested, which is what a hand edited record on disk looks like. Content is
// checked before the digest, so each case reports the rule it broke rather than
// the digest it invalidated on the way.
func TestValidateRefusals(t *testing.T) {
	for _, format := range objectFormats {
		t.Run(string(format), func(t *testing.T) {
			other := gitcli.ObjectFormatSHA256
			if format == gitcli.ObjectFormatSHA256 {
				other = gitcli.ObjectFormatSHA1
			}

			tests := []struct {
				name    string
				mutate  func(*state.Document)
				want    error
				message string
			}{{
				name:   "future schema",
				mutate: func(d *state.Document) { d.Schema = state.Schema + 1 },
				want:   state.ErrSchema,
			}, {
				name:   "unknown object format",
				mutate: func(d *state.Document) { d.ObjectFormat = "sha512" },
				want:   state.ErrObjectFormat,
			}, {
				name:   "object name from the other algorithm",
				mutate: func(d *state.Document) { d.Anchor.Source = sha(other, "anchor") },
				want:   state.ErrObjectFormat,
			}, {
				name:   "object name that is not hexadecimal",
				mutate: func(d *state.Document) { d.Anchor.Source = strings.Repeat("z", format.HexLength()) },
				want:   nil, message: "lower case hexadecimal",
			}, {
				name:   "object name in upper case",
				mutate: func(d *state.Document) { d.Anchor.Source = strings.ToUpper(d.Anchor.Source) },
				want:   nil, message: "lower case hexadecimal",
			}, {
				name:   "repository as a url",
				mutate: func(d *state.Document) { d.Destination.Repository = "https://github.com/enj/rbac.git" },
				want:   state.ErrLocation,
			}, {
				name:   "repository as an scp remote",
				mutate: func(d *state.Document) { d.Destination.Repository = "git@github.com:enj/rbac.git" },
				want:   state.ErrLocation,
			}, {
				name:   "repository as an absolute path",
				mutate: func(d *state.Document) { d.Destination.Repository = "/srv/git/enj/rbac" },
				want:   state.ErrLocation,
			}, {
				name:   "repository as a windows path",
				mutate: func(d *state.Document) { d.Destination.Repository = `C:/git/enj/rbac` },
				want:   state.ErrLocation,
			}, {
				name:   "repository with a backslash",
				mutate: func(d *state.Document) { d.Destination.Repository = `enj\rbac` },
				want:   state.ErrLocation,
			}, {
				name:   "repository without a name",
				mutate: func(d *state.Document) { d.Destination.Repository = "enj" },
				want:   nil, message: "owner and a name",
			}, {
				name:   "repository with a third component",
				mutate: func(d *state.Document) { d.Destination.Repository = "github.com/enj/rbac" },
				want:   nil, message: "owner and a name",
			}, {
				name:   "module that is a bare name",
				mutate: func(d *state.Document) { d.Destination.Module = "rbac" },
				want:   nil, message: "module path",
			}, {
				name:   "module carrying a secret",
				mutate: func(d *state.Document) { d.Destination.Module = "x-access-token:ghs_secret@monis.app/kk" },
				want:   state.ErrLocation,
			}, {
				name:   "value with a line break",
				mutate: func(d *state.Document) { d.Engine.Version = "v0.1.0\nSigned-off-by: nobody" },
				want:   nil, message: "whitespace or a control character",
			}, {
				name:   "anchor ref that is not fully qualified",
				mutate: func(d *state.Document) { d.Anchor.Ref = "v1.36.1" },
				want:   nil, message: "fully qualified",
			}, {
				name:   "anchor ref that is not hierarchical",
				mutate: func(d *state.Document) { d.Anchor.Ref = "refs" },
				want:   nil, message: "fully qualified",
			}, {
				name:   "anchor ref git would not accept",
				mutate: func(d *state.Document) { d.Anchor.Ref = "refs/tags/v1..36" },
				want:   nil, message: "anchor ref",
			}, {
				name:   "epoch graft that is not an object name",
				mutate: func(d *state.Document) { d.Epoch.Destination = "not-an-object" },
				want:   state.ErrObjectFormat,
			}, {
				name:   "profile digest without its prefix",
				mutate: func(d *state.Document) { d.Epoch.Profile = strings.TrimPrefix(d.Epoch.Profile, "sha256:") },
				want:   nil, message: `must begin with "sha256:"`,
			}, {
				name:   "profile digest of the wrong length",
				mutate: func(d *state.Document) { d.Epoch.Profile = "sha256:" + strings.Repeat("a", 63) },
				want:   nil, message: "63 digest characters",
			}, {
				name:   "profile digest in upper case",
				mutate: func(d *state.Document) { d.Epoch.Profile = "sha256:" + strings.Repeat("A", 64) },
				want:   nil, message: "lower case hexadecimal",
			}, {
				name: "duplicate cursor ref",
				mutate: func(d *state.Document) {
					d.Cursors = append(d.Cursors, d.Cursors[0])
				},
				want: state.ErrDuplicate,
			}, {
				name: "unsorted cursors",
				mutate: func(d *state.Document) {
					d.Cursors = []state.Cursor{{
						Ref: "refs/heads/master", Source: sha(format, "b"), Destination: sha(format, "bb"),
					}, {
						Ref: "refs/heads/main", Source: sha(format, "a"), Destination: sha(format, "aa"),
					}}
				},
				want: state.ErrUnsorted,
			}, {
				name: "one source mapped onto two destinations",
				mutate: func(d *state.Document) {
					d.Cursors = []state.Cursor{{
						Ref: "refs/heads/main", Source: sha(format, "s"), Destination: sha(format, "d1"),
					}, {
						Ref: "refs/heads/master", Source: sha(format, "s"), Destination: sha(format, "d2"),
					}}
				},
				want: state.ErrCorrespondence,
			}, {
				name: "one destination claimed by two sources",
				mutate: func(d *state.Document) {
					d.Cursors = []state.Cursor{{
						Ref: "refs/heads/main", Source: sha(format, "s1"), Destination: sha(format, "d"),
					}, {
						Ref: "refs/heads/master", Source: sha(format, "s2"), Destination: sha(format, "d"),
					}}
				},
				want: state.ErrCorrespondence,
			}, {
				name: "a cursor and a published branch that disagree about one destination",
				mutate: func(d *state.Document) {
					d.Published[0].Object = d.Cursors[0].Destination
				},
				want:    state.ErrCorrespondence,
				message: "came from",
			}, {
				name: "a track and a cursor that disagree about one source",
				mutate: func(d *state.Document) {
					d.Tracks[0].Source = d.Cursors[0].Source
				},
				want:    state.ErrCorrespondence,
				message: "track release-1-36",
			}, {
				name:   "mapping index with entries but no blob",
				mutate: func(d *state.Document) { d.Mapping.Object = "" },
				want:   nil, message: "mapping object is required",
			}, {
				name:   "mapping index with a blob but no entries",
				mutate: func(d *state.Document) { d.Mapping.Entries = 0 },
				want:   nil, message: "resolves no source commit",
			}, {
				name:   "mapping index with a negative count",
				mutate: func(d *state.Document) { d.Mapping.Entries = -1 },
				want:   nil, message: "must not be negative",
			}, {
				name:   "track ref that does not carry the track name",
				mutate: func(d *state.Document) { d.Tracks[0].Ref = state.ProgressNamespace + "other" },
				want:   state.ErrNamespace,
			}, {
				name:   "track held on a consumer branch",
				mutate: func(d *state.Document) { d.Tracks[0].Ref = state.BranchNamespace + "release-1-36" },
				want:   state.ErrNamespace,
			}, {
				name:   "track held on a consumer tag",
				mutate: func(d *state.Document) { d.Tracks[0].Ref = state.TagNamespace + "release-1-36" },
				want:   state.ErrNamespace,
			}, {
				name:   "track held outside every known namespace",
				mutate: func(d *state.Document) { d.Tracks[0].Ref = "refs/soapbox/release-1-36" },
				want:   state.ErrNamespace,
			}, {
				name:   "track that has done more than it must",
				mutate: func(d *state.Document) { d.Tracks[0].Done = d.Tracks[0].Total + 1 },
				want:   nil, message: "has done 10 of 9 commits",
			}, {
				name:   "track that has done a negative number of commits",
				mutate: func(d *state.Document) { d.Tracks[0].Done = -1 },
				want:   nil, message: "has done -1 commits",
			}, {
				name:   "track covering nothing",
				mutate: func(d *state.Document) { d.Tracks[0].Done, d.Tracks[0].Total = 0, 0 },
				want:   nil, message: "at least one commit",
			}, {
				name: "track that has done nothing but points somewhere",
				mutate: func(d *state.Document) {
					d.Tracks[0].Done = 0
				},
				want: nil, message: "has done no commits but records",
			}, {
				name: "track name spanning two ref components",
				mutate: func(d *state.Document) {
					// Confining the name to one component is what makes two
					// tracks unable to reach one ref, so a name carrying a
					// separator is refused rather than accommodated.
					d.Tracks[0].Name = "release/1-36"
					d.Tracks[0].Ref = state.ProgressNamespace + "release/1-36"
				},
				want: nil, message: "one ref component",
			}, {
				name:   "published entry with an unknown kind",
				mutate: func(d *state.Document) { d.Published[0].Kind = "release" },
				want:   nil, message: `want "branch" or "tag"`,
			}, {
				name:   "tag recorded outside the tag namespace",
				mutate: func(d *state.Document) { d.Published[0].Kind = state.KindTag },
				want:   state.ErrNamespace, message: `must live under "refs/tags/"`,
			}, {
				name: "branch recorded in the tag namespace",
				mutate: func(d *state.Document) {
					d.Published[0].Ref = "refs/tags/v0.36.1"
				},
				want: state.ErrNamespace, message: `must live under "refs/heads/"`,
			}, {
				name: "duplicate published ref",
				mutate: func(d *state.Document) {
					d.Published = append(d.Published, d.Published[0])
				},
				want: state.ErrDuplicate,
			}, {
				name:   "published entry that names no source",
				mutate: func(d *state.Document) { d.Published[0].Source = "" },
				want:   nil, message: "published source is required",
			}, {
				name:   "toolchain that is not a go toolchain",
				mutate: func(d *state.Document) { d.Engine.Toolchain = "1.26.5" },
				want:   nil, message: "Go toolchain name",
			}, {
				name:   "empty engine version",
				mutate: func(d *state.Document) { d.Engine.Version = "" },
				want:   nil, message: "engine version is required",
			}, {
				name:   "content changed after the document was digested",
				mutate: func(d *state.Document) { d.Destination.Module = "monis.app/kk/other" },
				want:   state.ErrDigest,
			}}

			for _, test := range tests {
				t.Run(test.name, func(t *testing.T) {
					doc := mustNew(t, base(format))
					test.mutate(&doc)
					err := doc.Validate()

					if test.want == nil && test.message == "" {
						if err != nil {
							t.Fatalf("validate: %v, want acceptance", err)
						}
						return
					}
					if err == nil {
						t.Fatalf("validate accepted the document")
					}
					if test.want != nil && !errors.Is(err, test.want) {
						t.Fatalf("validate: %v, want %v", err, test.want)
					}
					if test.message != "" && !strings.Contains(err.Error(), test.message) {
						t.Fatalf("validate: %v, want a message holding %q", err, test.message)
					}
				})
			}
		})
	}
}

// TestNewAcceptsTwoRefsOnOneCommit guards the correspondence rule from being
// read as uniqueness.
//
// A release branch and the tag cut from it sit on one commit routinely, so both
// cursors legitimately carry the same source and the same destination. The rule
// is about a document claiming one source became two different commits, and a
// stricter reading would refuse an ordinary repository.
func TestNewAcceptsTwoRefsOnOneCommit(t *testing.T) {
	format := gitcli.ObjectFormatSHA1
	doc := base(format)
	doc.Cursors = []state.Cursor{{
		Ref: "refs/heads/main", Source: sha(format, "s"), Destination: sha(format, "d"),
	}, {
		Ref: "refs/heads/release-1.36", Source: sha(format, "s"), Destination: sha(format, "d"),
	}}
	if err := mustNew(t, doc).Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

// TestNewAcceptsATrackThatHasClaimedNothing checks that the correspondence rule
// leaves an unstarted track alone.
//
// A track with no work done has produced no commit, so it makes no claim about
// what a source became and cannot contradict the cursor that shares its
// commits. Including it would refuse the ordinary shape of a backfill that has
// been planned but not begun.
func TestNewAcceptsATrackThatHasClaimedNothing(t *testing.T) {
	format := gitcli.ObjectFormatSHA256
	doc := base(format)
	doc.Tracks[0].Done = 0
	doc.Tracks[0].Source = ""
	doc.Tracks[0].Destination = ""
	if err := mustNew(t, doc).Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

// TestNamespacesCannotCollide proves that a backfill's progress can never be
// recorded on a ref a module consumer reads.
//
// The guarantee is structural rather than a check somewhere: a track's ref is
// exactly the progress namespace followed by its name, a published ref must be
// under the namespace its kind names, and the three namespaces share no prefix.
// This states that last part, because everything else rests on it and it is the
// one part a later edit could quietly break.
func TestNamespacesCannotCollide(t *testing.T) {
	namespaces := []string{state.ProgressNamespace, state.BranchNamespace, state.TagNamespace}
	for i, outer := range namespaces {
		if !strings.HasPrefix(outer, "refs/") || !strings.HasSuffix(outer, "/") {
			t.Fatalf("namespace %q is not a fully qualified ref prefix", outer)
		}
		for j, inner := range namespaces {
			if i == j {
				continue
			}
			if strings.HasPrefix(outer, inner) {
				t.Fatalf("namespace %q lies inside %q, so a progress ref could be a consumer ref", outer, inner)
			}
		}
	}
}

// TestAbsentAndEmptyListsAreOneRecord proves that a list nobody touched and a
// list explicitly emptied are the same state.
//
// They say the same thing, so they have to render, digest, and store the same
// way. Left apart they would be two blobs with two object names for one record,
// and a resume that decides whether anything changed by comparing names would
// report a change on the run that happened to build its document the other way.
func TestAbsentAndEmptyListsAreOneRecord(t *testing.T) {
	for _, format := range objectFormats {
		t.Run(string(format), func(t *testing.T) {
			absent := base(format)
			absent.Cursors = nil
			absent.Tracks = nil
			absent.Published = nil

			empty := base(format)
			empty.Cursors = []state.Cursor{}
			empty.Tracks = []state.Track{}
			empty.Published = []state.Published{}

			fromAbsent := mustNew(t, absent)
			fromEmpty := mustNew(t, empty)
			if fromAbsent.Digest != fromEmpty.Digest {
				t.Fatalf("an absent list digested to %s and an empty one to %s",
					fromAbsent.Digest, fromEmpty.Digest)
			}

			absentBytes, err := fromAbsent.Encode()
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			emptyBytes, err := fromEmpty.Encode()
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if !bytes.Equal(absentBytes, emptyBytes) {
				t.Fatalf("renderings differ:\n%s\nand\n%s", absentBytes, emptyBytes)
			}
			if bytes.Contains(absentBytes, []byte("null")) {
				t.Fatalf("a list rendered as null:\n%s", absentBytes)
			}

			// The lists are present on the way out too, so a caller ranging over
			// them does not have to distinguish the two cases either.
			for name, length := range map[string]int{
				"cursors":   len(fromAbsent.Cursors),
				"tracks":    len(fromAbsent.Tracks),
				"published": len(fromAbsent.Published),
			} {
				if length != 0 {
					t.Fatalf("%s holds %d entries, want none", name, length)
				}
			}
			if fromAbsent.Cursors == nil || fromAbsent.Tracks == nil || fromAbsent.Published == nil {
				t.Fatalf("a list came back absent rather than empty")
			}

			// Encoding a document a caller never ran through New has to agree
			// with the canonical one, or the digest it carries would describe
			// bytes nobody can reproduce.
			direct, err := fromEmpty.Clone().Encode()
			if err != nil {
				t.Fatalf("encode a clone: %v", err)
			}
			if !bytes.Equal(direct, emptyBytes) {
				t.Fatalf("a cloned document rendered differently:\n%s\nand\n%s", direct, emptyBytes)
			}

			// The same has to hold for a document whose lists are absent at the
			// moment it is rendered, which is what a caller assembling one by
			// hand and never cloning it produces. Its digest was computed over
			// the same normalisation, so it still verifies.
			raw := fromEmpty
			raw.Cursors, raw.Tracks, raw.Published = nil, nil, nil
			if err := raw.Validate(); err != nil {
				t.Fatalf("validate a document with absent lists: %v", err)
			}
			rawBytes, err := raw.Encode()
			if err != nil {
				t.Fatalf("encode a document with absent lists: %v", err)
			}
			if !bytes.Equal(rawBytes, emptyBytes) {
				t.Fatalf("absent lists rendered differently:\n%s\nand\n%s", rawBytes, emptyBytes)
			}
		})
	}
}

// TestNewCanonicalisesOrder proves the digest describes the work rather than the
// order the work was discovered in.
//
// Nothing in the engine fixes the order a repository answers a ref query in, so
// a record whose identity depended on it would make two identical runs on two
// machines disagree about whether anything had changed.
func TestNewCanonicalisesOrder(t *testing.T) {
	format := gitcli.ObjectFormatSHA256
	sorted := base(format)
	sorted.Cursors = []state.Cursor{
		{Ref: "refs/heads/main", Source: sha(format, "a"), Destination: sha(format, "aa")},
		{Ref: "refs/heads/master", Source: sha(format, "b"), Destination: sha(format, "bb")},
		{Ref: "refs/heads/release-1.36", Source: sha(format, "c"), Destination: sha(format, "cc")},
	}
	sorted.Tracks = []state.Track{
		{Name: "alpha", Ref: "refs/soapbox/progress/alpha", Total: 5},
		{Name: "beta", Ref: "refs/soapbox/progress/beta", Total: 7},
	}
	sorted.Published = []state.Published{
		{Ref: "refs/heads/main", Kind: state.KindBranch, Object: sha(format, "m"), Source: sha(format, "ms")},
		{Ref: "refs/tags/v0.36.1", Kind: state.KindTag, Object: sha(format, "t"), Source: sha(format, "ts")},
	}

	shuffled := sorted.Clone()
	slices.Reverse(shuffled.Cursors)
	slices.Reverse(shuffled.Tracks)
	slices.Reverse(shuffled.Published)

	forward := mustNew(t, sorted)
	backward := mustNew(t, shuffled)
	if forward.Digest != backward.Digest {
		t.Fatalf("reordered input digested to %s, want %s", backward.Digest, forward.Digest)
	}
	if !slices.Equal(forward.Cursors, backward.Cursors) {
		t.Fatalf("cursors differ: %v and %v", forward.Cursors, backward.Cursors)
	}
	if !slices.Equal(forward.Tracks, backward.Tracks) {
		t.Fatalf("tracks differ: %v and %v", forward.Tracks, backward.Tracks)
	}
	if !slices.Equal(forward.Published, backward.Published) {
		t.Fatalf("published differ: %v and %v", forward.Published, backward.Published)
	}
}

// TestNewRefusesADigestedDocument checks the one thing that would turn the
// digest from a tamper check into decoration: re-digesting a record that
// already carries one would report every modification as fine.
func TestNewRefusesADigestedDocument(t *testing.T) {
	doc := mustNew(t, base(gitcli.ObjectFormatSHA1))
	doc.Destination.Module = "monis.app/kk/other"
	if _, err := state.New(doc); !errors.Is(err, state.ErrDigest) {
		t.Fatalf("new: %v, want %v", err, state.ErrDigest)
	}
}

// TestDocumentsAreDefensivelyCopied proves a document cannot be changed through
// a slice its producer or its consumer still holds.
//
// Both directions matter. A caller that kept the slice it passed in could
// change a validated record afterwards, and a caller handed the record's own
// slice could change the copy the producer is still using.
func TestDocumentsAreDefensivelyCopied(t *testing.T) {
	format := gitcli.ObjectFormatSHA1
	input := base(format)
	cursors := slices.Clone(input.Cursors)
	input.Cursors = cursors

	doc := mustNew(t, input)
	digestBefore := doc.Digest

	// The producer's slice is no longer connected to the document.
	cursors[0].Destination = sha(format, "hijacked")
	if doc.Cursors[0].Destination == cursors[0].Destination {
		t.Fatalf("the document shares its cursors with the caller's slice")
	}
	if err := doc.Validate(); err != nil {
		t.Fatalf("validate after the caller mutated its own slice: %v", err)
	}

	// The consumer's copy is no longer connected either.
	clone := doc.Clone()
	clone.Cursors[0].Source = sha(format, "hijacked source")
	clone.Tracks[0].Done = 9
	clone.Published[0].Object = sha(format, "hijacked object")
	if doc.Cursors[0].Source == clone.Cursors[0].Source {
		t.Fatalf("the clone shares its cursors with the document")
	}
	if doc.Tracks[0].Done == clone.Tracks[0].Done {
		t.Fatalf("the clone shares its tracks with the document")
	}
	if doc.Published[0].Object == clone.Published[0].Object {
		t.Fatalf("the clone shares its published refs with the document")
	}
	if doc.Digest != digestBefore {
		t.Fatalf("digest changed to %s, want %s", doc.Digest, digestBefore)
	}
	if err := doc.Validate(); err != nil {
		t.Fatalf("validate after mutating a clone: %v", err)
	}
}

// TestDocumentHoldsOnlyTheNormalisedLists walks the document type and refuses
// any list beyond the three that are normalised by name.
//
// Normalisation is written out field by field rather than derived by reflection,
// which is the right trade for code on the digest path but means a list added
// somewhere new would silently render as null on one run and as an empty array
// on another. This is the tripwire for that: adding a list is fine, and this
// test says so at the point where the normalisation has to learn about it.
func TestDocumentHoldsOnlyTheNormalisedLists(t *testing.T) {
	normalised := map[string]bool{"Cursors": true, "Tracks": true, "Published": true}

	var walk func(typ reflect.Type, path string)
	walk = func(typ reflect.Type, path string) {
		if typ.Kind() != reflect.Struct {
			return
		}
		for i := range typ.NumField() {
			field := typ.Field(i)
			where := path + "." + field.Name
			switch field.Type.Kind() {
			case reflect.Slice, reflect.Array, reflect.Map, reflect.Pointer:
				if !normalised[field.Name] || path != "Document" {
					t.Fatalf("%s is a %s, which encode does not normalise, so one record could render two ways",
						where, field.Type.Kind())
				}
				walk(field.Type.Elem(), where)
			default:
				walk(field.Type, where)
			}
		}
	}
	walk(reflect.TypeFor[state.Document](), "Document")
}

// TestDocumentRecordsNoTime walks the whole document type and refuses any field
// that could hold a time.
//
// This is a rule about the schema rather than about one value, so it is checked
// against the type rather than against a fixture. A record carrying a timestamp
// would make two runs that did identical work produce different bytes, different
// digests, and different object names, which is the one property everything else
// here is built to provide.
func TestDocumentRecordsNoTime(t *testing.T) {
	temporal := []string{"time", "date", "stamp", "clock", "instant", "epochseconds"}

	var walk func(typ reflect.Type, path string)
	walk = func(typ reflect.Type, path string) {
		for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice || typ.Kind() == reflect.Array {
			typ = typ.Elem()
		}
		if typ.PkgPath() == "time" {
			t.Fatalf("%s is a %s.%s, and the record must hold no time", path, typ.PkgPath(), typ.Name())
		}
		if typ.Kind() != reflect.Struct {
			return
		}
		for i := range typ.NumField() {
			field := typ.Field(i)
			name := strings.ToLower(field.Name)
			// "Epoch" here names the profile generation, not a Unix epoch, so it
			// is the only word that has to be allowed through by name.
			if name != "epoch" {
				for _, word := range temporal {
					if strings.Contains(name, word) {
						t.Fatalf("%s.%s names a time, and the record must hold none", path, field.Name)
					}
				}
			}
			walk(field.Type, path+"."+field.Name)
		}
	}
	walk(reflect.TypeFor[state.Document](), "Document")
}

// TestPublishedTagMayShareSourceWithCursor proves that a KindTag Published
// entry may carry the same Source as a cursor while naming its annotated tag
// object as Object. Before the KindTag exclusion in images(), this was a
// correspondence conflict because one source mapped to two different
// destination objects (the branch commit and the tag object).
func TestPublishedTagMayShareSourceWithCursor(t *testing.T) {
	for _, format := range objectFormats {
		t.Run(string(format), func(t *testing.T) {
			doc := base(format)
			sharedSource := sha(format, "shared-source")
			branchCommit := sha(format, "branch-commit")
			tagObject := sha(format, "tag-object")

			doc.Anchor.Source = sharedSource
			doc.Epoch.Source = sharedSource
			doc.Cursors = []state.Cursor{{
				Ref:         "refs/heads/master",
				Source:      sharedSource,
				Destination: branchCommit,
			}}
			doc.Published = []state.Published{{
				Ref:    "refs/heads/main",
				Kind:   state.KindBranch,
				Source: sharedSource,
				Object: branchCommit,
			}, {
				Ref:    "refs/tags/v0.36.1",
				Kind:   state.KindTag,
				Source: sharedSource,
				Object: tagObject,
			}}
			canonical, err := state.New(doc)
			if err != nil {
				t.Fatalf("state.New: %v", err)
			}
			if err := canonical.Validate(); err != nil {
				t.Fatalf("validate: %v", err)
			}
		})
	}
}

func TestPublishedTagsRequireOneToOneCorrespondence(t *testing.T) {
	for _, format := range objectFormats {
		t.Run(string(format), func(t *testing.T) {
			tests := []struct {
				name string
				tags []state.Published
			}{
				{
					name: "one source cannot name two tag objects",
					tags: []state.Published{
						{Ref: "refs/tags/v0.36.1", Kind: state.KindTag, Source: sha(format, "source"), Object: sha(format, "tag-one")},
						{Ref: "refs/tags/v0.36.2", Kind: state.KindTag, Source: sha(format, "source"), Object: sha(format, "tag-two")},
					},
				},
				{
					name: "one tag object cannot name two sources",
					tags: []state.Published{
						{Ref: "refs/tags/v0.36.1", Kind: state.KindTag, Source: sha(format, "source-one"), Object: sha(format, "tag")},
						{Ref: "refs/tags/v0.36.2", Kind: state.KindTag, Source: sha(format, "source-two"), Object: sha(format, "tag")},
					},
				},
			}
			for _, test := range tests {
				t.Run(test.name, func(t *testing.T) {
					doc := base(format)
					doc.Published = test.tags
					_, err := state.New(doc)
					if !errors.Is(err, state.ErrCorrespondence) {
						t.Fatalf("state.New = %v, want correspondence error", err)
					}
				})
			}
		})
	}
}
