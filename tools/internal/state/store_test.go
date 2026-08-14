package state_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/gomodmap"
	"github.com/enj/soapbox/tools/internal/state"
)

// storeOptions is the fixed call every store test makes, so a difference
// between two stored records comes from the document rather than from the call.
func storeOptions(doc state.Document, parents ...string) state.StoreOptions {
	return state.StoreOptions{
		Document:  doc,
		Parents:   parents,
		Author:    signature,
		Committer: signature,
	}
}

// TestStoreLoadRoundTrip proves a record written into a repository reads back
// as the record that was written, under both hash algorithms.
func TestStoreLoadRoundTrip(t *testing.T) {
	for _, format := range objectFormats {
		t.Run(string(format), func(t *testing.T) {
			ctx := t.Context()
			git := newRepo(ctx, t, format)
			doc := mustNew(t, base(format))

			record, err := state.Store(ctx, git, storeOptions(doc))
			if err != nil {
				t.Fatalf("store: %v", err)
			}
			if record.Format != format {
				t.Fatalf("record names format %s, want %s", record.Format, format)
			}
			for _, name := range []string{record.Blob, record.Tree, record.Commit} {
				if len(name) != format.HexLength() {
					t.Fatalf("object name %q is not a %s name", name, format)
				}
			}
			if record.Digest != doc.Digest {
				t.Fatalf("record digests to %s, want %s", record.Digest, doc.Digest)
			}

			// The record is reachable through the commit and through the tree,
			// because a resume may know either.
			for _, revision := range []string{record.Commit, record.Tree} {
				loaded, err := state.Load(ctx, git, revision)
				if err != nil {
					t.Fatalf("load from %s: %v", revision, err)
				}
				if loaded.Digest != doc.Digest {
					t.Fatalf("loaded digest %s from %s, want %s", loaded.Digest, revision, doc.Digest)
				}
				if !slices.Equal(loaded.Cursors, doc.Cursors) {
					t.Fatalf("loaded cursors %v, want %v", loaded.Cursors, doc.Cursors)
				}
				if !slices.Equal(loaded.Published, doc.Published) {
					t.Fatalf("loaded published %v, want %v", loaded.Published, doc.Published)
				}
			}
			if _, _, err := state.LoadMapping(ctx, git, record.Commit); !errors.Is(err, state.ErrMappingEvidenceMissing) {
				t.Fatalf("legacy mapping evidence = %v, want ErrMappingEvidenceMissing", err)
			}
		})
	}
}

func TestStoreMakesMappingEvidenceReachableAndLoadVerifiesIt(t *testing.T) {
	for _, format := range objectFormats {
		t.Run(string(format), func(t *testing.T) {
			ctx := t.Context()
			git := newRepo(ctx, t, format)
			index := gomodmap.NewIndex()
			if err := index.Put(gomodmap.Entry{
				Source: sha(format, "mapping-source"),
				Tag:    "v1.36.1",
				Modules: []gomodmap.ModuleVersion{{
					Path: "k8s.io/api", Version: "v0.36.1", Commit: sha(format, "mapping-module"),
				}},
			}); err != nil {
				t.Fatalf("build mapping index: %v", err)
			}
			mapping, err := gomodmap.Encode(index)
			if err != nil {
				t.Fatalf("encode mapping: %v", err)
			}
			object, err := git.WriteBlob(ctx, mapping)
			if err != nil {
				t.Fatalf("write mapping blob: %v", err)
			}
			sum := sha256.Sum256(mapping)
			raw := base(format)
			raw.Mapping = state.Mapping{
				Digest:  "sha256:" + hex.EncodeToString(sum[:]),
				Object:  object,
				Entries: index.Len(),
			}
			doc := mustNew(t, raw)
			opts := storeOptions(doc)
			opts.Mapping = mapping
			record, err := state.Store(ctx, git, opts)
			if err != nil {
				t.Fatalf("store mapping-backed state: %v", err)
			}
			if record.MappingBlob != object {
				t.Errorf("record mapping blob = %s, want %s", record.MappingBlob, object)
			}
			inspected, err := state.Inspect(ctx, git, record.Commit)
			if err != nil {
				t.Fatalf("inspect state: %v", err)
			}
			if inspected != record {
				t.Errorf("inspected record = %#v, want %#v", inspected, record)
			}
			entries, err := git.ListTree(ctx, record.Commit)
			if err != nil {
				t.Fatalf("list state tree: %v", err)
			}
			if !slices.ContainsFunc(entries, func(entry gitcli.TreeEntry) bool {
				return entry.Path == state.MappingFile && entry.Object == object
			}) {
				t.Fatalf("state tree does not make mapping %s reachable: %#v", object, entries)
			}
			loaded, err := state.Load(ctx, git, record.Commit)
			if err != nil {
				t.Fatalf("load mapping-backed state: %v", err)
			}
			if loaded.Mapping != doc.Mapping {
				t.Errorf("loaded mapping = %#v, want %#v", loaded.Mapping, doc.Mapping)
			}
			loadedBytes, loadedIndex, err := state.LoadMapping(ctx, git, record.Commit)
			if err != nil {
				t.Fatalf("load mapping evidence: %v", err)
			}
			if !slices.Equal(loadedBytes, mapping) || loadedIndex.Len() != index.Len() {
				t.Errorf("loaded mapping = %d bytes/%d entries, want %d/%d", len(loadedBytes), loadedIndex.Len(), len(mapping), index.Len())
			}

			remoteRoot := t.TempDir()
			remote, err := gitcli.New(ctx, gitcli.Options{Dir: remoteRoot, Inherit: []string{"PATH"}})
			if err != nil {
				t.Fatalf("create remote runner: %v", err)
			}
			if err := remote.InitRepositoryWithFormat(ctx, "main", format); err != nil {
				t.Fatalf("init remote: %v", err)
			}
			if err := remote.SetConfigLocal(ctx, "core.bare", "true"); err != nil {
				t.Fatalf("make remote bare: %v", err)
			}
			const stateRef = "refs/heads/soapbox-state"
			if err := git.CreateRef(ctx, stateRef, record.Commit); err != nil {
				t.Fatalf("create state ref: %v", err)
			}
			remotePath := filepath.Join(remoteRoot, ".git")
			if err := git.PushAtomic(ctx, remotePath, []gitcli.PushUpdate{{Ref: stateRef, New: record.Commit, ExpectAbsent: true}}); err != nil {
				t.Fatalf("push state record: %v", err)
			}
			fresh := newRepo(ctx, t, format)
			if err := fresh.FetchExact(ctx, remotePath, stateRef, record.Commit, format.HexLength()); err != nil {
				t.Fatalf("fetch exact state record: %v", err)
			}
			fetchedBytes, fetchedIndex, err := state.LoadMapping(ctx, fresh, record.Commit)
			if err != nil {
				t.Fatalf("load fetched mapping evidence: %v", err)
			}
			if !slices.Equal(fetchedBytes, mapping) || fetchedIndex.Len() != index.Len() {
				t.Errorf("fetched mapping = %d bytes/%d entries, want %d/%d", len(fetchedBytes), fetchedIndex.Len(), len(mapping), index.Len())
			}
		})
	}
}

// TestStoreIsDeterministic pins the property a resumable run rests on: the same
// record produces the same objects, in the same repository and in a fresh one.
//
// The second repository is the real assertion. Object names that only agreed
// within one repository would agree because the objects were already there,
// which says nothing about whether two machines doing the same work would
// produce the same record.
func TestStoreIsDeterministic(t *testing.T) {
	for _, format := range objectFormats {
		t.Run(string(format), func(t *testing.T) {
			ctx := t.Context()
			doc := mustNew(t, base(format))

			first := newRepo(ctx, t, format)
			once, err := state.Store(ctx, first, storeOptions(doc))
			if err != nil {
				t.Fatalf("store: %v", err)
			}
			twice, err := state.Store(ctx, first, storeOptions(doc))
			if err != nil {
				t.Fatalf("store again: %v", err)
			}
			if once != twice {
				t.Fatalf("storing the same record twice produced %+v and %+v", once, twice)
			}

			second := newRepo(ctx, t, format)
			elsewhere, err := state.Store(ctx, second, storeOptions(doc))
			if err != nil {
				t.Fatalf("store in a fresh repository: %v", err)
			}
			if elsewhere != once {
				t.Fatalf("a fresh repository produced %+v, want %+v", elsewhere, once)
			}
			if !slices.Equal(elsewhere.Report(), once.Report()) {
				t.Fatalf("reports differ:\n%s\nand\n%s",
					strings.Join(once.Report(), "\n"), strings.Join(elsewhere.Report(), "\n"))
			}

			// A different record must produce different objects, or the
			// comparison above would hold for records that are not the same.
			changed := base(format)
			changed.Mapping.Entries = doc.Mapping.Entries + 1
			other, err := state.Store(ctx, first, storeOptions(mustNew(t, changed)))
			if err != nil {
				t.Fatalf("store a changed record: %v", err)
			}
			if other.Commit == once.Commit || other.Blob == once.Blob {
				t.Fatalf("a changed record produced the same objects: %+v", other)
			}
		})
	}
}

// TestStoreCreatesNoRef is the boundary this package exists to hold.
//
// A record of work in progress must not be visible to a module consumer, and
// the only thing that makes an object visible is a ref pointing at it. Writing
// objects is therefore safe at any point in a run, and every decision about
// what a branch should point at stays with the gated publication step.
func TestStoreCreatesNoRef(t *testing.T) {
	for _, format := range objectFormats {
		t.Run(string(format), func(t *testing.T) {
			ctx := t.Context()
			git := newRepo(ctx, t, format)

			before, err := git.ListRefs(ctx)
			if err != nil {
				t.Fatalf("list refs: %v", err)
			}
			if len(before) != 0 {
				t.Fatalf("the fresh repository already holds %d refs", len(before))
			}

			first, err := state.Store(ctx, git, storeOptions(mustNew(t, base(format))))
			if err != nil {
				t.Fatalf("store: %v", err)
			}
			// A second record with the first as its parent is the shape a resume
			// produces, and is the call most likely to want a ref to advance.
			advanced := base(format)
			advanced.Mapping.Entries = 30
			if _, err := state.Store(ctx, git, storeOptions(mustNew(t, advanced), first.Commit)); err != nil {
				t.Fatalf("store a successor: %v", err)
			}

			after, err := git.ListRefs(ctx)
			if err != nil {
				t.Fatalf("list refs: %v", err)
			}
			if len(after) != 0 {
				t.Fatalf("storing created refs: %v", after)
			}
			head, err := git.HasHead(ctx)
			if err != nil {
				t.Fatalf("has head: %v", err)
			}
			if head {
				t.Fatalf("storing moved HEAD onto a commit")
			}
		})
	}
}

// TestStoreWritesAnUnsignedCommitWithoutProvenance pins the shape of the commit
// itself.
//
// Two absences are load bearing. Nothing is signed, because a generated commit
// has no signer and a signature would claim otherwise. Nothing carries a
// provenance trailer, because that trailer is how a resumed run rebuilds the
// source to destination mapping from published history, and a state commit is
// not the transform of any upstream commit.
func TestStoreWritesAnUnsignedCommitWithoutProvenance(t *testing.T) {
	ctx := t.Context()
	git := newRepo(ctx, t, gitcli.ObjectFormatSHA1)
	doc := mustNew(t, base(gitcli.ObjectFormatSHA1))

	record, err := state.Store(ctx, git, storeOptions(doc))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	info, err := git.CommitInfo(ctx, record.Commit)
	if err != nil {
		t.Fatalf("commit info: %v", err)
	}
	if len(info.Parents) != 0 {
		t.Fatalf("a first record has parents %v", info.Parents)
	}
	if info.SignatureStatus != "" && info.SignatureStatus != "N" {
		t.Fatalf("the commit carries signature status %q", info.SignatureStatus)
	}
	if len(info.Trailers) != 0 {
		t.Fatalf("the commit carries trailers %v", info.Trailers)
	}
	// CommitInfo renders the stored raw date in ISO form, so the assertion is
	// against the instant the caller supplied rather than against its spelling.
	// What matters is that it is the supplied one rather than the wall clock.
	const supplied = "2023-11-14T22:13:20Z"
	if info.AuthorDate != supplied || info.CommitterDate != supplied {
		t.Fatalf("dates are %q and %q, want %q", info.AuthorDate, info.CommitterDate, supplied)
	}
	if !strings.Contains(info.RawMessage, doc.Digest) {
		t.Fatalf("the message does not name the record's digest: %q", info.RawMessage)
	}

	// The parents a caller states are the parents the commit records, so a
	// chain of records can be walked without a ref ever having existed.
	successor := base(gitcli.ObjectFormatSHA1)
	successor.Mapping.Entries = 30
	next, err := state.Store(ctx, git, storeOptions(mustNew(t, successor), record.Commit))
	if err != nil {
		t.Fatalf("store a successor: %v", err)
	}
	chain, err := git.CommitInfo(ctx, next.Commit)
	if err != nil {
		t.Fatalf("commit info: %v", err)
	}
	if !slices.Equal(chain.Parents, []string{record.Commit}) {
		t.Fatalf("the successor has parents %v, want %v", chain.Parents, []string{record.Commit})
	}
}

// TestStoreRefusesAMismatchedObjectFormat checks the tie between the document
// and the repository it is being written into.
//
// A document names the hash algorithm every object name in it uses. Written
// into a repository using the other one, it would decode cleanly and describe
// commits that repository does not have, and a resume would transform the wrong
// history rather than fail.
func TestStoreRefusesAMismatchedObjectFormat(t *testing.T) {
	ctx := t.Context()
	git := newRepo(ctx, t, gitcli.ObjectFormatSHA256)
	doc := mustNew(t, base(gitcli.ObjectFormatSHA1))

	if _, err := state.Store(ctx, git, storeOptions(doc)); !errors.Is(err, state.ErrObjectFormat) {
		t.Fatalf("store: %v, want %v", err, state.ErrObjectFormat)
	}

	// The same refusal has to hold on the way in. A record written elsewhere
	// and fetched into this repository reaches Load rather than Store, so it is
	// planted here as an ordinary blob and read back.
	encoded, err := doc.Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	blob, err := git.WriteBlob(ctx, encoded)
	if err != nil {
		t.Fatalf("write blob: %v", err)
	}
	tree, err := git.WriteTree(ctx, []gitcli.TreeEntry{{
		Mode: gitcli.ModeRegular, Object: blob, Path: state.File,
	}})
	if err != nil {
		t.Fatalf("write tree: %v", err)
	}
	if _, err := state.Load(ctx, git, tree); !errors.Is(err, state.ErrObjectFormat) {
		t.Fatalf("load: %v, want %v", err, state.ErrObjectFormat)
	}

	// The document is otherwise sound, so the refusal above is about the
	// repository it was read from rather than about the record.
	matching := newRepo(ctx, t, gitcli.ObjectFormatSHA1)
	record, err := state.Store(ctx, matching, storeOptions(doc))
	if err != nil {
		t.Fatalf("store in a matching repository: %v", err)
	}
	if _, err := state.Load(ctx, matching, record.Commit); err != nil {
		t.Fatalf("load from a matching repository: %v", err)
	}
}

// TestStoreRefusesAnInvalidDocument checks that nothing unreadable reaches a
// blob. A record that cannot be decoded is a resume that cannot happen.
func TestStoreRefusesAnInvalidDocument(t *testing.T) {
	ctx := t.Context()
	git := newRepo(ctx, t, gitcli.ObjectFormatSHA1)

	doc := mustNew(t, base(gitcli.ObjectFormatSHA1))
	doc.Anchor.Ref = "v1.36.1"
	if _, err := state.Store(ctx, git, storeOptions(doc)); err == nil {
		t.Fatalf("store accepted a document validation refuses")
	}

	refs, err := git.ListRefs(ctx)
	if err != nil {
		t.Fatalf("list refs: %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("a refused store created refs: %v", refs)
	}
}

// TestStoreRefusesAnUnusableParent covers every parent that would produce a
// chain a resume cannot walk.
//
// The refusals are made here rather than left to git, because git's answers
// arrive too late to be useful. An abbreviation or a name from the other hash
// algorithm either resolves or does not, and a record whose parent is written in
// a notation the rest of the document does not use is wrong even when it
// resolves. A parent that is a tree, or one this repository does not hold,
// produces a commit whose earlier records cannot be read at all.
func TestStoreRefusesAnUnusableParent(t *testing.T) {
	ctx := t.Context()
	format := gitcli.ObjectFormatSHA1
	git := newRepo(ctx, t, format)

	planted, err := state.Store(ctx, git, storeOptions(mustNew(t, base(format))))
	if err != nil {
		t.Fatalf("store: %v", err)
	}

	tests := []struct {
		name    string
		parents []string
		want    error
		message string
	}{{
		name:    "an abbreviated commit",
		parents: []string{planted.Commit[:12]},
		want:    state.ErrObjectFormat,
	}, {
		name:    "a name from the other hash algorithm",
		parents: []string{sha(gitcli.ObjectFormatSHA256, "elsewhere")},
		want:    state.ErrObjectFormat,
	}, {
		name:    "a name that is not hexadecimal",
		parents: []string{strings.Repeat("z", format.HexLength())},
		want:    nil, message: "lower case hexadecimal",
	}, {
		name:    "an object this repository does not hold",
		parents: []string{strings.Repeat("b", format.HexLength())},
		want:    nil, message: "is not an object this repository holds",
	}, {
		name:    "a tree rather than a commit",
		parents: []string{planted.Tree},
		want:    nil, message: "is a tree, not a commit",
	}, {
		name:    "a blob rather than a commit",
		parents: []string{planted.Blob},
		want:    nil, message: "is a blob, not a commit",
	}, {
		name:    "a bad parent after a good one",
		parents: []string{planted.Commit, planted.Tree},
		want:    nil, message: "parent 1",
	}}

	successor := func(t *testing.T) state.Document {
		t.Helper()
		doc := base(format)
		doc.Mapping.Entries = 30
		return mustNew(t, doc)
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := state.Store(ctx, git, storeOptions(successor(t), test.parents...))
			if err == nil {
				t.Fatalf("store accepted the parent")
			}
			if test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("store: %v, want %v", err, test.want)
			}
			if test.message != "" && !strings.Contains(err.Error(), test.message) {
				t.Fatalf("store: %v, want a message holding %q", err, test.message)
			}
		})
	}

	// A real commit in this repository is accepted, so the refusals above are
	// about the parents rather than about the call.
	if _, err := state.Store(ctx, git, storeOptions(successor(t), planted.Commit)); err != nil {
		t.Fatalf("store with a real parent: %v", err)
	}
}

// TestLoadRefusesARevisionHoldingNoRecord checks that a commit which is not a
// state commit is reported rather than read as an empty one.
func TestLoadRefusesARevisionHoldingNoRecord(t *testing.T) {
	ctx := t.Context()
	g := newGraph(ctx, t, gitcli.ObjectFormatSHA1)

	if _, err := state.Load(ctx, g.git, g.source[0]); err == nil {
		t.Fatalf("load accepted a commit holding no record")
	}
}

// TestLoadRefusesATamperedRecord checks that the digest survives the round trip
// through a repository.
//
// The tampered document is written as an ordinary blob, which is what an
// operator editing the file and committing it would produce, so the refusal has
// to come from decoding rather than from anything Store did.
func TestLoadRefusesATamperedRecord(t *testing.T) {
	ctx := t.Context()
	git := newRepo(ctx, t, gitcli.ObjectFormatSHA1)
	doc := mustNew(t, base(gitcli.ObjectFormatSHA1))

	encoded, err := doc.Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	tampered := strings.Replace(string(encoded), "enj/rbac_authorizer", "enj/rbac_authorize2", 1)

	blob, err := git.WriteBlob(ctx, []byte(tampered))
	if err != nil {
		t.Fatalf("write blob: %v", err)
	}
	tree, err := git.WriteTree(ctx, []gitcli.TreeEntry{{
		Mode: gitcli.ModeRegular, Object: blob, Path: state.File,
	}})
	if err != nil {
		t.Fatalf("write tree: %v", err)
	}
	if _, err := state.Load(ctx, git, tree); !errors.Is(err, state.ErrDigest) {
		t.Fatalf("load: %v, want %v", err, state.ErrDigest)
	}
}

// TestStoreStopsOnACancelledContext checks that an abandoned run writes no
// objects.
func TestStoreStopsOnACancelledContext(t *testing.T) {
	git := newRepo(t.Context(), t, gitcli.ObjectFormatSHA1)
	doc := mustNew(t, base(gitcli.ObjectFormatSHA1))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := state.Store(ctx, git, storeOptions(doc)); !errors.Is(err, context.Canceled) {
		t.Fatalf("store: %v, want %v", err, context.Canceled)
	}
	if _, err := state.Load(ctx, git, "HEAD"); !errors.Is(err, context.Canceled) {
		t.Fatalf("load: %v, want %v", err, context.Canceled)
	}
}
