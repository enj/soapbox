package sync_test

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/config"
	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/source"
	"github.com/enj/soapbox/tools/internal/state"
	"github.com/enj/soapbox/tools/internal/sync"
	"github.com/enj/soapbox/tools/internal/testsupport"
)

// gitResults checks the typed results returned by Git fixture operations.
type gitResults struct {
	t *testing.T
}

func mustGit(t *testing.T) gitResults {
	return gitResults{t: t}
}

func (m gitResults) object(value string, err error) string {
	m.t.Helper()
	if err != nil {
		m.t.Fatalf("Git fixture operation: %v", err)
	}
	return value
}

func (m gitResults) refs(value []gitcli.Ref, err error) []gitcli.Ref {
	m.t.Helper()
	if err != nil {
		m.t.Fatalf("Git fixture operation: %v", err)
	}
	return value
}

func (m gitResults) format(value gitcli.ObjectFormat, err error) gitcli.ObjectFormat {
	m.t.Helper()
	if err != nil {
		m.t.Fatalf("Git fixture operation: %v", err)
	}
	return value
}

func (m gitResults) runner(value *gitcli.Runner, err error) *gitcli.Runner {
	m.t.Helper()
	if err != nil {
		m.t.Fatalf("Git fixture operation: %v", err)
	}
	return value
}

// mustDo fails the test if err is non-nil. Use for operations that return only
// an error.
func mustDo(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// --- source cache test helper ---

// newTestSource builds a source repository with annotated release tags and
// opens a source.Cache over it. Tags are created from a single commit on main.
// It enables uploadpack.allowFilter so the blobless clone audit passes.
func newTestSource(ctx context.Context, t *testing.T, tags []string) (*source.Cache, string) {
	t.Helper()

	repo := testsupport.NewRepo(ctx, t, testsupport.Options{
		Branch:    "main",
		UserName:  "Test",
		UserEmail: "test@test.com",
	})
	repo.SetConfig(ctx, t, "uploadpack.allowFilter", "true")

	commit := repo.WriteAndCommit(ctx, t, "main.go", "package main\n", "feat: initial\n")

	for _, tag := range tags {
		if err := repo.Git.CreateTag(ctx, gitcli.TagOptions{
			Name: tag, Commit: commit,
			Tagger: testSignature, Message: "Release " + tag + "\n",
		}); err != nil {
			t.Fatalf("create tag %s: %v", tag, err)
		}
	}

	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	git, err := gitcli.New(ctx, gitcli.Options{Dir: root, Inherit: []string{"PATH", "HOME"}})
	if err != nil {
		t.Fatalf("source runner: %v", err)
	}
	cache, err := source.Open(ctx, source.Options{
		Remote:       "file://" + repo.Dir,
		CacheRoot:    filepath.Join(root, "cache"),
		WorktreeRoot: filepath.Join(root, "worktrees"),
		Git:          git,
	})
	if err != nil {
		t.Fatalf("open source cache: %v", err)
	}
	return cache, commit
}

// --- destination test helper ---

// discoveryDest is a destination pair: local repo + bare remote.
type discoveryDest struct {
	localGit  *gitcli.Runner
	dir       string
	remoteDir string
	parent    string
}

func newDiscoveryDest(ctx context.Context, t *testing.T) *discoveryDest {
	t.Helper()
	destDir := t.TempDir()
	destGit := newRepository(ctx, t, destDir, testBranch)
	remoteRoot := t.TempDir()
	remoteGit := newRepository(ctx, t, remoteRoot, testBranch)
	if err := remoteGit.SetConfigLocal(ctx, "core.bare", "true"); err != nil {
		t.Fatalf("make remote bare: %v", err)
	}
	parent := writeControlPlane(ctx, t, destGit)
	return &discoveryDest{
		localGit:  destGit,
		dir:       destDir,
		remoteDir: filepath.Join(remoteRoot, ".git"),
		parent:    parent,
	}
}

func (d *discoveryDest) opts(cache *source.Cache, sourceCommit string) sync.DiscoverOptions {
	return sync.DiscoverOptions{
		Config:           discoveryConfig(sourceCommit),
		LocalGit:         d.localGit.Anonymous().WithNoLazyFetch(),
		Remote:           d.remoteDir,
		Identity:         testIdentity,
		AllowLocalRemote: true,
		SourceCache:      cache,
	}
}

func (d *discoveryDest) localRefs(ctx context.Context, t *testing.T) map[string]string {
	t.Helper()
	refs, err := d.localGit.ListRefs(ctx)
	if err != nil {
		t.Fatalf("list local refs: %v", err)
	}
	m := make(map[string]string, len(refs))
	for _, ref := range refs {
		m[ref.Name] = ref.Target
	}
	return m
}

func discoveryConfig(anchorCommit string) *config.Config {
	cfg := testConfig()
	cfg.Source.Refs.MinimumRelease = testSourceTag
	cfg.Source.Refs.AnchorCommit = anchorCommit
	cfg.Release.FirstTag = testReleaseTag
	return cfg
}

// publishedState builds a state document recording a branch and tag, stores it,
// and pushes everything to the remote. It returns the state commit, the
// destination commit (branch head), and the tag OID.
func (d *discoveryDest) publishedState(ctx context.Context, t *testing.T, sourceCommit string) (stateCommit, destCommit, tagOID string) {
	t.Helper()

	blob := mustGit(t).object(d.localGit.WriteBlob(ctx, []byte("published\n")))
	entries, listErr := d.localGit.ListTree(ctx, d.parent)
	if listErr != nil {
		t.Fatalf("read control-plane tree: %v", listErr)
	}
	entries = append(entries, gitcli.TreeEntry{Mode: gitcli.ModeRegular, Object: blob, Path: "p.go"})
	tree := mustGit(t).object(d.localGit.WriteTree(ctx, entries))
	dc := mustGit(t).object(d.localGit.WriteCommit(ctx, gitcli.CommitTreeOptions{
		Tree: tree, Parents: []string{d.parent},
		Author: testSignature, Committer: testSignature,
		Message: "published\n",
	}))
	mustDo(t, d.localGit.CreateTag(ctx, gitcli.TagOptions{
		Name: testReleaseTag, Commit: dc,
		Tagger: testSignature, Message: "Release " + testReleaseTag + "\n",
	}))
	tagRefs := mustGit(t).refs(d.localGit.ListRefs(ctx, "refs/tags/"+testReleaseTag))
	tOID := tagRefs[0].Target

	format := mustGit(t).format(d.localGit.ObjectFormat(ctx))
	doc := state.Document{
		Schema:       state.Schema,
		ObjectFormat: format,
		Destination:  state.Destination{Repository: testRepository, Module: testModulePath},
		Anchor:       state.Anchor{Source: sourceCommit, Ref: testSourceRef},
		Epoch: state.Epoch{
			Profile:     testProfileHash,
			Source:      sourceCommit,
			Destination: d.parent,
		},
		Cursors: []state.Cursor{{Ref: testSourceRef, Source: sourceCommit, Destination: dc}},
		Published: []state.Published{
			{Ref: testBranchRef, Kind: state.KindBranch, Object: dc, Source: sourceCommit},
			{Ref: "refs/tags/" + testReleaseTag, Kind: state.KindTag, Object: tOID, Source: sourceCommit},
		},
		Engine: state.Engine{Version: "test", Toolchain: "go1.26.5"},
	}
	doc, err := state.New(doc)
	if err != nil {
		t.Fatalf("build state: %v", err)
	}
	record, err := state.Store(ctx, d.localGit, state.StoreOptions{
		Document: doc, Author: testSignature, Committer: testSignature,
	})
	if err != nil {
		t.Fatalf("store state: %v", err)
	}
	mustDo(t, d.localGit.CreateRef(ctx, testStateRef, record.Commit))
	mustDo(t, d.localGit.PushAtomic(ctx, d.remoteDir, []gitcli.PushUpdate{
		{Ref: testBranchRef, New: dc, ExpectAbsent: true},
		{Ref: testStateRef, New: record.Commit, ExpectAbsent: true},
		{Ref: "refs/tags/" + testReleaseTag, New: tOID, ExpectAbsent: true},
	}))
	mustDo(t, d.localGit.UpdateRef(ctx, testBranchRef, dc, d.parent))
	return record.Commit, dc, tOID
}

func (d *discoveryDest) advanceBranch(ctx context.Context, t *testing.T, base, path, contents, message string) string {
	t.Helper()
	entries, err := d.localGit.ListTree(ctx, base)
	if err != nil {
		t.Fatalf("read branch base tree: %v", err)
	}
	object, err := d.localGit.WriteBlob(ctx, []byte(contents))
	if err != nil {
		t.Fatalf("write branch advance blob: %v", err)
	}
	found := false
	for i := range entries {
		if entries[i].Path == path {
			entries[i].Object = object
			found = true
			break
		}
	}
	if !found {
		entries = append(entries, gitcli.TreeEntry{Mode: gitcli.ModeRegular, Object: object, Path: path})
	}
	tree, err := d.localGit.WriteTree(ctx, entries)
	if err != nil {
		t.Fatalf("write branch advance tree: %v", err)
	}
	commit, err := d.localGit.WriteCommit(ctx, gitcli.CommitTreeOptions{
		Tree: tree, Parents: []string{base}, Author: testSignature, Committer: testSignature,
		Message: message,
	})
	if err != nil {
		t.Fatalf("write branch advance commit: %v", err)
	}
	if err := d.localGit.PushAtomic(ctx, d.remoteDir, []gitcli.PushUpdate{{
		Ref: testBranchRef, New: commit, ExpectedOld: base,
	}}); err != nil {
		t.Fatalf("push branch advance: %v", err)
	}
	if err := d.localGit.UpdateRef(ctx, testBranchRef, commit, base); err != nil {
		t.Fatalf("advance local branch: %v", err)
	}
	return commit
}

// --- Validation tests ---

func TestDiscoverRequiresProfile(t *testing.T) {
	_, err := sync.Discover(t.Context(), sync.DiscoverOptions{})
	if err == nil {
		t.Fatal("expected error for missing profile")
	}
	if !strings.Contains(err.Error(), "profile is required") {
		t.Fatalf("error %q does not mention profile", err)
	}
}

func TestDiscoverRequiresLocalGit(t *testing.T) {
	_, err := sync.Discover(t.Context(), sync.DiscoverOptions{
		Config: discoveryConfig(testSourceCommit),
	})
	if err == nil {
		t.Fatal("expected error for missing local git runner")
	}
	if !strings.Contains(err.Error(), "local destination git runner") {
		t.Fatalf("error %q does not mention local git runner", err)
	}
}

func TestDiscoverRequiresNoLazyFetch(t *testing.T) {
	ctx := t.Context()
	d := newDiscoveryDest(ctx, t)
	cache, sourceCommit := newTestSource(ctx, t, []string{testSourceTag})
	opts := d.opts(cache, sourceCommit)
	// Use anonymous but without no-lazy-fetch pin.
	opts.LocalGit = d.localGit.Anonymous()
	_, err := sync.Discover(ctx, opts)
	if err == nil {
		t.Fatal("expected error for runner without no-lazy-fetch")
	}
	if !strings.Contains(err.Error(), "promisor fetches") {
		t.Fatalf("error %q does not mention promisor fetches", err)
	}
}

func TestDiscoverRequiresSourceCache(t *testing.T) {
	ctx := t.Context()
	d := newDiscoveryDest(ctx, t)
	opts := d.opts(nil, testSourceCommit)
	opts.SourceCache = nil
	_, err := sync.Discover(ctx, opts)
	if err == nil {
		t.Fatal("expected error for missing source cache")
	}
	if !strings.Contains(err.Error(), "source cache") {
		t.Fatalf("error %q does not mention source cache", err)
	}
}

func TestDiscoverHonoursCancellation(t *testing.T) {
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := sync.Discover(cancelled, sync.DiscoverOptions{Config: discoveryConfig(testSourceCommit)})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("discover = %v, want cancellation", err)
	}
}

// --- Absent state ---

func TestDiscoverAbsentStateAllReleasesPending(t *testing.T) {
	ctx := t.Context()
	cache, sourceCommit := newTestSource(ctx, t, []string{testSourceTag})
	d := newDiscoveryDest(ctx, t)

	disc, err := sync.Discover(ctx, d.opts(cache, sourceCommit))
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if disc.StateCommit != "" {
		t.Errorf("state commit = %q, want empty", disc.StateCommit)
	}
	if len(disc.Pending) != 1 {
		t.Fatalf("pending = %d, want 1", len(disc.Pending))
	}
	if disc.Pending[0].DestinationTag != testReleaseTag {
		t.Errorf("pending tag = %q, want %q", disc.Pending[0].DestinationTag, testReleaseTag)
	}
}

// --- Fixed point with FETCH_HEAD/ref assertions ---

func TestDiscoverFixedPointNoOp(t *testing.T) {
	ctx := t.Context()
	cache, sourceCommit := newTestSource(ctx, t, []string{testSourceTag})
	d := newDiscoveryDest(ctx, t)
	sc, _, _ := d.publishedState(ctx, t, sourceCommit)

	localRefsBefore := d.localRefs(ctx, t)

	disc, err := sync.Discover(ctx, d.opts(cache, sourceCommit))
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if !disc.FixedPoint() {
		t.Errorf("expected fixed point, got %d pending", len(disc.Pending))
	}
	if disc.StateCommit != sc {
		t.Errorf("state commit = %q, want %q", disc.StateCommit, sc)
	}
	format := mustGit(t).format(d.localGit.ObjectFormat(ctx))
	if disc.Format != format {
		t.Errorf("format = %q, want %q", string(disc.Format), string(format))
	}

	// Assert FETCH_HEAD and consumer refs are unchanged.
	localRefsAfter := d.localRefs(ctx, t)
	for ref, before := range localRefsBefore {
		if after, ok := localRefsAfter[ref]; !ok {
			t.Errorf("local ref %s was deleted during discovery", ref)
		} else if after != before {
			t.Errorf("local ref %s moved from %s to %s during discovery", ref, before, after)
		}
	}
	if _, hasFetchHead := localRefsAfter["FETCH_HEAD"]; hasFetchHead {
		t.Error("discovery wrote FETCH_HEAD")
	}
	for ref := range localRefsAfter {
		if strings.HasPrefix(ref, "refs/soapbox/fetch/") {
			t.Errorf("discovery left temporary ref %s", ref)
		}
	}
}

// --- Tag immutability: tag moved (state vs remote disagree) ---

func TestDiscoverImmutableTagConflictFromState(t *testing.T) {
	ctx := t.Context()
	cache, sourceCommit := newTestSource(ctx, t, []string{testSourceTag})
	d := newDiscoveryDest(ctx, t)

	// Create the published commit and tag.
	blob := mustGit(t).object(d.localGit.WriteBlob(ctx, []byte("pub\n")))
	tree := mustGit(t).object(d.localGit.WriteTree(ctx, []gitcli.TreeEntry{
		{Mode: gitcli.ModeRegular, Object: blob, Path: "p.go"},
	}))
	destCommit := mustGit(t).object(d.localGit.WriteCommit(ctx, gitcli.CommitTreeOptions{
		Tree: tree, Parents: []string{d.parent},
		Author: testSignature, Committer: testSignature,
		Message: "published\n",
	}))
	mustDo(t, d.localGit.CreateTag(ctx, gitcli.TagOptions{
		Name: testReleaseTag, Commit: destCommit,
		Tagger: testSignature, Message: "Release\n",
	}))
	tagRefs := mustGit(t).refs(d.localGit.ListRefs(ctx, "refs/tags/"+testReleaseTag))
	remoteTagOID := tagRefs[0].Target

	// Build a second tag object at a different commit to use as the conflicting
	// state-recorded OID.
	blob2 := mustGit(t).object(d.localGit.WriteBlob(ctx, []byte("other\n")))
	tree2 := mustGit(t).object(d.localGit.WriteTree(ctx, []gitcli.TreeEntry{
		{Mode: gitcli.ModeRegular, Object: blob2, Path: "other.go"},
	}))
	otherCommit := mustGit(t).object(d.localGit.WriteCommit(ctx, gitcli.CommitTreeOptions{
		Tree: tree2, Parents: []string{destCommit},
		Author: testSignature, Committer: testSignature,
		Message: "other\n",
	}))
	mustDo(t, d.localGit.CreateTag(ctx, gitcli.TagOptions{
		Name: "conflict-helper", Commit: otherCommit,
		Tagger: testSignature, Message: "Conflict\n",
	}))
	helperRefs := mustGit(t).refs(d.localGit.ListRefs(ctx, "refs/tags/conflict-helper"))
	stateTagOID := helperRefs[0].Target

	// Build state recording the tag at stateTagOID (different from remoteTagOID).
	format := mustGit(t).format(d.localGit.ObjectFormat(ctx))
	doc := state.Document{
		Schema:       state.Schema,
		ObjectFormat: format,
		Destination:  state.Destination{Repository: testRepository, Module: testModulePath},
		Anchor:       state.Anchor{Source: sourceCommit, Ref: testSourceRef},
		Epoch: state.Epoch{
			Profile:     testProfileHash,
			Source:      sourceCommit,
			Destination: d.parent,
		},
		Cursors: []state.Cursor{{Ref: testSourceRef, Source: sourceCommit, Destination: destCommit}},
		Published: []state.Published{
			{Ref: testBranchRef, Kind: state.KindBranch, Object: destCommit, Source: sourceCommit},
			{Ref: "refs/tags/" + testReleaseTag, Kind: state.KindTag, Object: stateTagOID, Source: sourceCommit},
		},
		Engine: state.Engine{Version: "test", Toolchain: "go1.26.5"},
	}
	doc, err := state.New(doc)
	if err != nil {
		t.Fatalf("build state: %v", err)
	}
	record, err := state.Store(ctx, d.localGit, state.StoreOptions{
		Document: doc, Author: testSignature, Committer: testSignature,
	})
	if err != nil {
		t.Fatalf("store state: %v", err)
	}
	mustDo(t, d.localGit.CreateRef(ctx, testStateRef, record.Commit))

	mustDo(t, d.localGit.PushAtomic(ctx, d.remoteDir, []gitcli.PushUpdate{
		{Ref: testBranchRef, New: destCommit, ExpectAbsent: true},
		{Ref: testStateRef, New: record.Commit, ExpectAbsent: true},
		{Ref: "refs/tags/" + testReleaseTag, New: remoteTagOID, ExpectAbsent: true},
	}))
	mustDo(t, d.localGit.UpdateRef(ctx, testBranchRef, destCommit, d.parent))

	_, err = sync.Discover(ctx, d.opts(cache, sourceCommit))
	if err == nil {
		t.Fatal("expected error for tag conflict")
	}
	if !errors.Is(err, state.ErrTagMoved) {
		t.Fatalf("error = %v, want ErrTagMoved", err)
	}
}

// --- Tag immutability: state records tag but remote deleted it ---

func TestDiscoverDeletedPublishedTag(t *testing.T) {
	ctx := t.Context()
	cache, sourceCommit := newTestSource(ctx, t, []string{testSourceTag})
	d := newDiscoveryDest(ctx, t)

	// Create the tag locally but do NOT push it to the remote, simulating
	// a deletion after publication.
	blob := mustGit(t).object(d.localGit.WriteBlob(ctx, []byte("pub\n")))
	tree := mustGit(t).object(d.localGit.WriteTree(ctx, []gitcli.TreeEntry{
		{Mode: gitcli.ModeRegular, Object: blob, Path: "p.go"},
	}))
	destCommit := mustGit(t).object(d.localGit.WriteCommit(ctx, gitcli.CommitTreeOptions{
		Tree: tree, Parents: []string{d.parent},
		Author: testSignature, Committer: testSignature,
		Message: "published\n",
	}))
	mustDo(t, d.localGit.CreateTag(ctx, gitcli.TagOptions{
		Name: testReleaseTag, Commit: destCommit,
		Tagger: testSignature, Message: "Release\n",
	}))
	tagRefs := mustGit(t).refs(d.localGit.ListRefs(ctx, "refs/tags/"+testReleaseTag))
	tagOID := tagRefs[0].Target

	// Build state recording both branch and tag.
	format := mustGit(t).format(d.localGit.ObjectFormat(ctx))
	doc := state.Document{
		Schema:       state.Schema,
		ObjectFormat: format,
		Destination:  state.Destination{Repository: testRepository, Module: testModulePath},
		Anchor:       state.Anchor{Source: sourceCommit, Ref: testSourceRef},
		Epoch: state.Epoch{
			Profile:     testProfileHash,
			Source:      sourceCommit,
			Destination: d.parent,
		},
		Cursors: []state.Cursor{{Ref: testSourceRef, Source: sourceCommit, Destination: destCommit}},
		Published: []state.Published{
			{Ref: testBranchRef, Kind: state.KindBranch, Object: destCommit, Source: sourceCommit},
			{Ref: "refs/tags/" + testReleaseTag, Kind: state.KindTag, Object: tagOID, Source: sourceCommit},
		},
		Engine: state.Engine{Version: "test", Toolchain: "go1.26.5"},
	}
	doc, err := state.New(doc)
	if err != nil {
		t.Fatalf("build state: %v", err)
	}
	record, err := state.Store(ctx, d.localGit, state.StoreOptions{
		Document: doc, Author: testSignature, Committer: testSignature,
	})
	if err != nil {
		t.Fatalf("store state: %v", err)
	}
	mustDo(t, d.localGit.CreateRef(ctx, testStateRef, record.Commit))

	// Push branch and state but NOT the tag.
	mustDo(t, d.localGit.PushAtomic(ctx, d.remoteDir, []gitcli.PushUpdate{
		{Ref: testBranchRef, New: destCommit, ExpectAbsent: true},
		{Ref: testStateRef, New: record.Commit, ExpectAbsent: true},
	}))
	mustDo(t, d.localGit.UpdateRef(ctx, testBranchRef, destCommit, d.parent))

	_, err = sync.Discover(ctx, d.opts(cache, sourceCommit))
	if err == nil {
		t.Fatal("expected error for deleted published tag")
	}
	if !strings.Contains(err.Error(), "deleted after publication") {
		t.Fatalf("error %q does not explain deleted tag", err)
	}
}

// --- Tag immutability: untrusted remote tag (not in state, not firstTag) ---

func TestDiscoverUntrustedExtraRemoteTag(t *testing.T) {
	ctx := t.Context()
	cache, sourceCommit := newTestSource(ctx, t, []string{testSourceTag})
	d := newDiscoveryDest(ctx, t)
	d.publishedState(ctx, t, sourceCommit)

	remoteRunner := mustGit(t).runner(d.localGit.WithDir(d.remoteDir))
	blob := mustGit(t).object(remoteRunner.WriteBlob(ctx, []byte("extra\n")))
	tree := mustGit(t).object(remoteRunner.WriteTree(ctx, []gitcli.TreeEntry{
		{Mode: gitcli.ModeRegular, Object: blob, Path: "x.go"},
	}))
	commit := mustGit(t).object(remoteRunner.WriteCommit(ctx, gitcli.CommitTreeOptions{
		Tree: tree, Author: testSignature, Committer: testSignature,
		Message: "extra\n",
	}))
	mustDo(t, remoteRunner.CreateTag(ctx, gitcli.TagOptions{
		Name: "v0.99.0", Commit: commit,
		Tagger: testSignature, Message: "Extra\n",
	}))

	_, err := sync.Discover(ctx, d.opts(cache, sourceCommit))
	if err == nil {
		t.Fatal("expected error for untrusted extra remote tag")
	}
	if !strings.Contains(err.Error(), "not recorded in state") {
		t.Fatalf("error %q does not explain untrusted tag", err)
	}
}

// --- Branch drift ---

func TestDiscoverBranchDriftFromState(t *testing.T) {
	ctx := t.Context()
	cache, sourceCommit := newTestSource(ctx, t, []string{testSourceTag})
	d := newDiscoveryDest(ctx, t)
	d.publishedState(ctx, t, sourceCommit)

	remoteRunner := mustGit(t).runner(d.localGit.WithDir(d.remoteDir))
	blob := mustGit(t).object(remoteRunner.WriteBlob(ctx, []byte("drift\n")))
	tree := mustGit(t).object(remoteRunner.WriteTree(ctx, []gitcli.TreeEntry{
		{Mode: gitcli.ModeRegular, Object: blob, Path: "drift.go"},
	}))
	commit := mustGit(t).object(remoteRunner.WriteCommit(ctx, gitcli.CommitTreeOptions{
		Tree: tree, Author: testSignature, Committer: testSignature,
		Message: "drift\n",
	}))
	refs := mustGit(t).refs(remoteRunner.ListRefs(ctx, testBranchRef))
	if len(refs) > 0 {
		mustDo(t, remoteRunner.UpdateRef(ctx, testBranchRef, commit, refs[0].Target))
	}

	_, err := sync.Discover(ctx, d.opts(cache, sourceCommit))
	if err == nil {
		t.Fatal("expected error for branch drift")
	}
	// The HEAD mismatch fires before the state comparison because
	// requireLocalHEADMatches runs first.
	if !strings.Contains(err.Error(), "local HEAD is") {
		t.Fatalf("error %q does not explain branch drift", err)
	}
}

func TestDiscoverAcceptsControlPlaneFastForward(t *testing.T) {
	ctx := t.Context()
	cache, sourceCommit := newTestSource(ctx, t, []string{testSourceTag})
	d := newDiscoveryDest(ctx, t)
	_, base, _ := d.publishedState(ctx, t, sourceCommit)
	first := d.advanceBranch(ctx, t, base, "soapbox.yaml", "version: 2\n", "chore: update profile\n")
	head := d.advanceBranch(ctx, t, first, "tools/go.sum", "canonical sums\n", "fix: update checksums\n")

	discovery, err := sync.Discover(ctx, d.opts(cache, sourceCommit))
	if err != nil {
		t.Fatalf("discover control-plane fast-forward: %v", err)
	}
	control := discovery.ControlPlaneBranch
	if control == nil || control.Ref != testBranchRef || control.Base != base || control.Object != head || control.Source != sourceCommit {
		t.Fatalf("control-plane branch = %#v, want %s..%s from %s", control, base, head, sourceCommit)
	}
	if discovery.AdoptedBranch != nil {
		t.Errorf("control-plane fast-forward was treated as consumer adoption: %#v", discovery.AdoptedBranch)
	}
	if !discovery.FixedPoint() {
		t.Errorf("control-plane-only discovery is not a fixed point: %#v", discovery)
	}
}

func TestDiscoverRefusesControlPlaneGeneratedPath(t *testing.T) {
	ctx := t.Context()
	cache, sourceCommit := newTestSource(ctx, t, []string{testSourceTag})
	d := newDiscoveryDest(ctx, t)
	_, base, _ := d.publishedState(ctx, t, sourceCommit)
	d.advanceBranch(ctx, t, base, "go.mod", "module example.invalid/drift\n", "chore: alter generated module\n")

	_, err := sync.Discover(ctx, d.opts(cache, sourceCommit))
	if err == nil || !strings.Contains(err.Error(), "changes generated path go.mod") {
		t.Fatalf("discover generated-path drift = %v, want generated path refusal", err)
	}
}

func TestDiscoverRefusesControlPlaneSourceTrailer(t *testing.T) {
	ctx := t.Context()
	cache, sourceCommit := newTestSource(ctx, t, []string{testSourceTag})
	d := newDiscoveryDest(ctx, t)
	_, base, _ := d.publishedState(ctx, t, sourceCommit)
	d.advanceBranch(ctx, t, base, "soapbox.yaml", "version: 2\n",
		"chore: forge source mapping\n\nKubernetes-commit: "+sourceCommit+"\n")

	_, err := sync.Discover(ctx, d.opts(cache, sourceCommit))
	if err == nil || !strings.Contains(err.Error(), "carries source-provenance trailer") {
		t.Fatalf("discover control-plane trailer = %v, want provenance refusal", err)
	}
}

// --- State-commit override ---

func TestDiscoverStateCommitOverrideMismatch(t *testing.T) {
	ctx := t.Context()
	cache, sourceCommit := newTestSource(ctx, t, []string{testSourceTag})
	d := newDiscoveryDest(ctx, t)

	remoteRunner := mustGit(t).runner(d.localGit.WithDir(d.remoteDir))
	blob := mustGit(t).object(remoteRunner.WriteBlob(ctx, []byte("fake\n")))
	tree := mustGit(t).object(remoteRunner.WriteTree(ctx, []gitcli.TreeEntry{
		{Mode: gitcli.ModeRegular, Object: blob, Path: "state.json"},
	}))
	commit := mustGit(t).object(remoteRunner.WriteCommit(ctx, gitcli.CommitTreeOptions{
		Tree: tree, Author: testSignature, Committer: testSignature,
		Message: "state\n",
	}))
	mustDo(t, remoteRunner.CreateRef(ctx, testStateRef, commit))

	format := mustGit(t).format(d.localGit.ObjectFormat(ctx))
	wrongCommit := strings.Repeat("b", format.HexLength())

	opts := d.opts(cache, sourceCommit)
	opts.StateCommitOverride = wrongCommit
	_, err := sync.Discover(ctx, opts)
	if err == nil {
		t.Fatal("expected error for state-commit mismatch")
	}
	if !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("error %q does not explain mismatch", err)
	}
}

func TestDiscoverStateCommitOverrideAbsent(t *testing.T) {
	ctx := t.Context()
	cache, sourceCommit := newTestSource(ctx, t, []string{testSourceTag})
	d := newDiscoveryDest(ctx, t)

	format := mustGit(t).format(d.localGit.ObjectFormat(ctx))
	overrideCommit := strings.Repeat("a", format.HexLength())

	opts := d.opts(cache, sourceCommit)
	opts.StateCommitOverride = overrideCommit
	_, err := sync.Discover(ctx, opts)
	if err == nil {
		t.Fatal("expected error for override when state is absent")
	}
	if !strings.Contains(err.Error(), "does not advertise") {
		t.Fatalf("error %q does not explain absent state", err)
	}
}

// --- State validation against profile ---

func TestDiscoverStateRepositoryMismatch(t *testing.T) {
	ctx := t.Context()
	cache, sourceCommit := newTestSource(ctx, t, []string{testSourceTag})
	d := newDiscoveryDest(ctx, t)

	format := mustGit(t).format(d.localGit.ObjectFormat(ctx))
	doc := state.Document{
		Schema:       state.Schema,
		ObjectFormat: format,
		Destination:  state.Destination{Repository: "other/wrong", Module: testModulePath},
		Anchor:       state.Anchor{Source: sourceCommit, Ref: testSourceRef},
		Epoch: state.Epoch{
			Profile:     testProfileHash,
			Source:      sourceCommit,
			Destination: d.parent,
		},
		Cursors: []state.Cursor{{Ref: testSourceRef, Source: sourceCommit, Destination: d.parent}},
		Engine:  state.Engine{Version: "test", Toolchain: "go1.26.5"},
	}
	doc, err := state.New(doc)
	if err != nil {
		t.Fatalf("build state: %v", err)
	}
	record, err := state.Store(ctx, d.localGit, state.StoreOptions{
		Document: doc, Author: testSignature, Committer: testSignature,
	})
	if err != nil {
		t.Fatalf("store state: %v", err)
	}
	mustDo(t, d.localGit.CreateRef(ctx, testStateRef, record.Commit))
	mustDo(t, d.localGit.PushAtomic(ctx, d.remoteDir, []gitcli.PushUpdate{
		{Ref: testStateRef, New: record.Commit, ExpectAbsent: true},
	}))

	_, err = sync.Discover(ctx, d.opts(cache, sourceCommit))
	if err == nil {
		t.Fatal("expected error for repository mismatch")
	}
	if !strings.Contains(err.Error(), "state records repository") {
		t.Fatalf("error %q does not explain repository mismatch", err)
	}
}

func TestDiscoverStateModuleMismatch(t *testing.T) {
	ctx := t.Context()
	cache, sourceCommit := newTestSource(ctx, t, []string{testSourceTag})
	d := newDiscoveryDest(ctx, t)

	format := mustGit(t).format(d.localGit.ObjectFormat(ctx))
	doc := state.Document{
		Schema:       state.Schema,
		ObjectFormat: format,
		Destination:  state.Destination{Repository: testRepository, Module: "wrong.example/module"},
		Anchor:       state.Anchor{Source: sourceCommit, Ref: testSourceRef},
		Epoch: state.Epoch{
			Profile:     testProfileHash,
			Source:      sourceCommit,
			Destination: d.parent,
		},
		Cursors: []state.Cursor{{Ref: testSourceRef, Source: sourceCommit, Destination: d.parent}},
		Engine:  state.Engine{Version: "test", Toolchain: "go1.26.5"},
	}
	doc, err := state.New(doc)
	if err != nil {
		t.Fatalf("build state: %v", err)
	}
	record, err := state.Store(ctx, d.localGit, state.StoreOptions{
		Document: doc, Author: testSignature, Committer: testSignature,
	})
	if err != nil {
		t.Fatalf("store state: %v", err)
	}
	mustDo(t, d.localGit.CreateRef(ctx, testStateRef, record.Commit))
	mustDo(t, d.localGit.PushAtomic(ctx, d.remoteDir, []gitcli.PushUpdate{
		{Ref: testStateRef, New: record.Commit, ExpectAbsent: true},
	}))

	_, err = sync.Discover(ctx, d.opts(cache, sourceCommit))
	if err == nil {
		t.Fatal("expected error for module mismatch")
	}
	if !strings.Contains(err.Error(), "state records module") {
		t.Fatalf("error %q does not explain module mismatch", err)
	}
}

// --- Ordered pending ---

func TestDiscoverMultiplePendingOrderedBySemver(t *testing.T) {
	ctx := t.Context()
	cache, sourceCommit := newTestSource(ctx, t, []string{"v1.36.1", "v1.37.0", "v1.38.0"})
	d := newDiscoveryDest(ctx, t)

	disc, err := sync.Discover(ctx, d.opts(cache, sourceCommit))
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(disc.Pending) != 3 {
		t.Fatalf("pending = %d, want 3", len(disc.Pending))
	}
	wantTags := []string{"v0.36.1", "v0.37.0", "v0.38.0"}
	for i, want := range wantTags {
		if disc.Pending[i].DestinationTag != want {
			t.Errorf("pending[%d] = %q, want %q", i, disc.Pending[i].DestinationTag, want)
		}
	}
}

// --- Legacy firstTag adoption ---

func TestDiscoverAdoptsLegacyFirstTag(t *testing.T) {
	ctx := t.Context()
	cache, sourceCommit := newTestSource(ctx, t, []string{testSourceTag})
	d := newDiscoveryDest(ctx, t)

	cfg := discoveryConfig(sourceCommit)
	// Build a published commit with the correct provenance trailer while
	// preserving the setup-derived control plane.
	blob := mustGit(t).object(d.localGit.WriteBlob(ctx, []byte("published\n")))
	entries, listErr := d.localGit.ListTree(ctx, d.parent)
	if listErr != nil {
		t.Fatalf("read control-plane tree: %v", listErr)
	}
	entries = append(entries, gitcli.TreeEntry{Mode: gitcli.ModeRegular, Object: blob, Path: "p.go"})
	tree := mustGit(t).object(d.localGit.WriteTree(ctx, entries))
	commitMessage := "Release " + testReleaseTag + "\n\n" + cfg.Commit.TrailerKey + ": " + sourceCommit + "\n"
	destCommit := mustGit(t).object(d.localGit.WriteCommit(ctx, gitcli.CommitTreeOptions{
		Tree: tree, Parents: []string{d.parent},
		Author: testSignature, Committer: testSignature,
		Message: commitMessage,
	}))

	// The tag message must carry the release source metadata keys the adoption
	// verifies: Source-tag, Source-commit, Source-release.
	sourceURL := "https://github.com/kubernetes/kubernetes/releases/tag/" + testSourceTag
	tagMessage := testReleaseTag + "\n\n" +
		"Source-tag: " + testSourceTag + "\n" +
		"Source-commit: " + sourceCommit + "\n" +
		"Source-release: " + sourceURL + "\n"
	mustDo(t, d.localGit.CreateTag(ctx, gitcli.TagOptions{
		Name: testReleaseTag, Commit: destCommit,
		Tagger: testSignature, Message: tagMessage,
	}))
	tagRefs := mustGit(t).refs(d.localGit.ListRefs(ctx, "refs/tags/"+testReleaseTag))
	tagOID := tagRefs[0].Target

	// Build state recording the branch but NOT the tag (legacy scenario).
	format := mustGit(t).format(d.localGit.ObjectFormat(ctx))
	doc := state.Document{
		Schema:       state.Schema,
		ObjectFormat: format,
		Destination:  state.Destination{Repository: testRepository, Module: testModulePath},
		Anchor:       state.Anchor{Source: sourceCommit, Ref: testSourceRef},
		Epoch: state.Epoch{
			Profile:     testProfileHash,
			Source:      sourceCommit,
			Destination: d.parent,
		},
		Cursors: []state.Cursor{{Ref: testSourceRef, Source: sourceCommit, Destination: destCommit}},
		Published: []state.Published{
			{Ref: testBranchRef, Kind: state.KindBranch, Object: destCommit, Source: sourceCommit},
		},
		Engine: state.Engine{Version: "test", Toolchain: "go1.26.5"},
	}
	doc, err := state.New(doc)
	if err != nil {
		t.Fatalf("build state: %v", err)
	}
	record, err := state.Store(ctx, d.localGit, state.StoreOptions{
		Document: doc, Author: testSignature, Committer: testSignature,
	})
	if err != nil {
		t.Fatalf("store state: %v", err)
	}
	mustDo(t, d.localGit.CreateRef(ctx, testStateRef, record.Commit))

	mustDo(t, d.localGit.PushAtomic(ctx, d.remoteDir, []gitcli.PushUpdate{
		{Ref: testBranchRef, New: destCommit, ExpectAbsent: true},
		{Ref: testStateRef, New: record.Commit, ExpectAbsent: true},
		{Ref: "refs/tags/" + testReleaseTag, New: tagOID, ExpectAbsent: true},
	}))
	mustDo(t, d.localGit.UpdateRef(ctx, testBranchRef, destCommit, d.parent))
	controlHead := d.advanceBranch(ctx, t, destCommit, "soapbox.yaml", "version: 2\n", "chore: migrate control plane\n")

	discoverOpts := d.opts(cache, sourceCommit)
	discoverOpts.Config.Commit.Committer = config.Identity{
		Name: "Replacement Bot", Email: "replacement@example.test",
	}
	disc, err := sync.Discover(ctx, discoverOpts)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(disc.Pending) != 0 {
		t.Errorf("pending = %d, want 0", len(disc.Pending))
	}
	// FixedPoint is false because adoption needs a state-reconciliation plan.
	if disc.FixedPoint() {
		t.Error("FixedPoint should be false while Adopted is non-nil")
	}
	if disc.Adopted == nil {
		t.Fatal("expected an adopted tag")
	}
	if disc.Adopted.Tag != testReleaseTag {
		t.Errorf("adopted tag = %q, want %q", disc.Adopted.Tag, testReleaseTag)
	}
	if disc.Adopted.Object != tagOID {
		t.Errorf("adopted object = %q, want %q", disc.Adopted.Object, tagOID)
	}
	if disc.Adopted.Commit != destCommit {
		t.Errorf("adopted commit = %q, want %q", disc.Adopted.Commit, destCommit)
	}
	if disc.Adopted.Source != sourceCommit {
		t.Errorf("adopted source = %q, want %q", disc.Adopted.Source, sourceCommit)
	}
	// State HAS a Published branch, so no branch adoption is needed. The
	// operator-only fast-forward is retained separately as a graft point.
	if disc.AdoptedBranch != nil {
		t.Errorf("unexpected adopted branch %+v", disc.AdoptedBranch)
	}
	if disc.ControlPlaneBranch == nil || disc.ControlPlaneBranch.Base != destCommit || disc.ControlPlaneBranch.Object != controlHead {
		t.Errorf("control-plane branch = %#v, want %s..%s", disc.ControlPlaneBranch, destCommit, controlHead)
	}
	tags, err := cache.ListTags(ctx)
	if err != nil || len(tags) != 1 {
		t.Fatalf("list source release = %#v, %v", tags, err)
	}
	reconciliation, err := sync.PlanAdoptionReconciliation(ctx, sync.FinalizeOptions{
		Config: discoverOpts.Config, Discovery: disc, SourceCache: cache,
		Destination: sync.Destination{
			Git: discoverOpts.LocalGit, Remote: discoverOpts.Remote,
			Identity: discoverOpts.Identity, AllowLocalRemote: discoverOpts.AllowLocalRemote,
			Lister: discoverOpts.Lister,
		},
		Release: source.Release{Source: tags[0], DestinationTag: testReleaseTag},
	})
	if err != nil {
		t.Fatalf("plan legacy adoption reconciliation: %v", err)
	}
	if _, err := sync.ApplyReconciliation(ctx, reconciliation, reconciliation.Publish.Hash()); err != nil {
		t.Fatalf("apply legacy adoption reconciliation: %v", err)
	}
	reconciled, err := state.Load(ctx, d.localGit, reconciliation.State.Commit)
	if err != nil {
		t.Fatalf("load legacy reconciliation: %v", err)
	}
	if !slices.ContainsFunc(reconciled.Published, func(entry state.Published) bool {
		return entry.Ref == "refs/tags/"+testReleaseTag && entry.Object == tagOID
	}) {
		t.Errorf("legacy reconciliation did not record tag: %#v", reconciled.Published)
	}
	if !slices.ContainsFunc(reconciled.Published, func(entry state.Published) bool {
		return entry.Ref == testBranchRef && entry.Object == destCommit
	}) {
		t.Errorf("legacy reconciliation replaced generated branch image with control plane: %#v", reconciled.Published)
	}
	remoteRefs, err := d.localGit.RemoteRefs(ctx, d.remoteDir, format.HexLength())
	if err != nil {
		t.Fatalf("read reconciled refs: %v", err)
	}
	if !slices.ContainsFunc(remoteRefs, func(ref gitcli.Ref) bool {
		return ref.Name == testBranchRef && ref.Target == controlHead
	}) {
		t.Errorf("reconciliation moved control-plane branch: %#v", remoteRefs)
	}
}

func TestDiscoverRefusesNonFirstTagAdoption(t *testing.T) {
	ctx := t.Context()
	cache, sourceCommit := newTestSource(ctx, t, []string{testSourceTag, "v1.37.0"})
	d := newDiscoveryDest(ctx, t)

	d.publishedState(ctx, t, sourceCommit)

	remoteRunner := mustGit(t).runner(d.localGit.WithDir(d.remoteDir))
	blob := mustGit(t).object(remoteRunner.WriteBlob(ctx, []byte("rogue\n")))
	tree := mustGit(t).object(remoteRunner.WriteTree(ctx, []gitcli.TreeEntry{
		{Mode: gitcli.ModeRegular, Object: blob, Path: "rogue.go"},
	}))
	commit := mustGit(t).object(remoteRunner.WriteCommit(ctx, gitcli.CommitTreeOptions{
		Tree: tree, Author: testSignature, Committer: testSignature,
		Message: "rogue\n",
	}))
	mustDo(t, remoteRunner.CreateTag(ctx, gitcli.TagOptions{
		Name: "v0.37.0", Commit: commit,
		Tagger: testSignature, Message: "Rogue\n",
	}))

	_, err := sync.Discover(ctx, d.opts(cache, sourceCommit))
	if err == nil {
		t.Fatal("expected error for non-firstTag adoption")
	}
	if !strings.Contains(err.Error(), "not recorded in state") {
		t.Fatalf("error %q does not explain untracked tag refusal", err)
	}
}

// --- Additional precondition tests ---

func TestDiscoverHTTPSRemoteRequiresRemoteGit(t *testing.T) {
	ctx := t.Context()
	cache, sourceCommit := newTestSource(ctx, t, []string{testSourceTag})
	d := newDiscoveryDest(ctx, t)

	opts := d.opts(cache, sourceCommit)
	opts.Remote = "https://github.com/" + testRepository + ".git"
	opts.AllowLocalRemote = false
	opts.RemoteGit = nil
	_, err := sync.Discover(ctx, opts)
	if err == nil {
		t.Fatal("expected error for HTTPS remote without RemoteGit")
	}
	if !strings.Contains(err.Error(), "HTTPS remote requires a credentialed RemoteGit") {
		t.Fatalf("error %q does not explain missing RemoteGit", err)
	}
}

func TestDiscoverRefusesRootMismatch(t *testing.T) {
	ctx := t.Context()
	cache, sourceCommit := newTestSource(ctx, t, []string{testSourceTag})
	d := newDiscoveryDest(ctx, t)

	// Build a second repo for RemoteGit at a different root.
	otherDir := t.TempDir()
	otherGit := newRepository(ctx, t, otherDir, testBranch)

	opts := d.opts(cache, sourceCommit)
	opts.RemoteGit = otherGit
	_, err := sync.Discover(ctx, opts)
	if err == nil {
		t.Fatal("expected error for root mismatch")
	}
	if !strings.Contains(err.Error(), "share a repository root") {
		t.Fatalf("error %q does not explain root mismatch", err)
	}
}

func TestDiscoverRefusesAnchorCommitMissing(t *testing.T) {
	ctx := t.Context()
	cache, _ := newTestSource(ctx, t, []string{testSourceTag})
	d := newDiscoveryDest(ctx, t)

	opts := d.opts(cache, "")
	opts.Config.Source.Refs.AnchorCommit = ""
	_, err := sync.Discover(ctx, opts)
	if err == nil {
		t.Fatal("expected error for missing anchorCommit")
	}
	if !strings.Contains(err.Error(), "neither profile anchorCommit nor state provides a provable anchor") {
		t.Fatalf("error %q does not explain missing anchor", err)
	}
}

func TestDiscoverRefusesUnknownBranchOnRemote(t *testing.T) {
	ctx := t.Context()
	cache, sourceCommit := newTestSource(ctx, t, []string{testSourceTag})
	d := newDiscoveryDest(ctx, t)

	// Push an unexpected branch to the bare remote.
	blob := mustGit(t).object(d.localGit.WriteBlob(ctx, []byte("rogue\n")))
	tree := mustGit(t).object(d.localGit.WriteTree(ctx, []gitcli.TreeEntry{
		{Mode: gitcli.ModeRegular, Object: blob, Path: "rogue.go"},
	}))
	commit := mustGit(t).object(d.localGit.WriteCommit(ctx, gitcli.CommitTreeOptions{
		Tree: tree, Author: testSignature, Committer: testSignature,
		Message: "rogue branch\n",
	}))
	mustDo(t, d.localGit.CreateRef(ctx, "refs/heads/rogue", commit))
	mustDo(t, d.localGit.PushAtomic(ctx, d.remoteDir, []gitcli.PushUpdate{
		{Ref: "refs/heads/rogue", New: commit, ExpectAbsent: true},
	}))

	_, err := sync.Discover(ctx, d.opts(cache, sourceCommit))
	if err == nil {
		t.Fatal("expected error for unknown branch on remote")
	}
	if !strings.Contains(err.Error(), "outside the expected set") {
		t.Fatalf("error %q does not explain unknown branch", err)
	}
}

func TestDiscoverStateAnchorSourceMismatch(t *testing.T) {
	ctx := t.Context()
	cache, sourceCommit := newTestSource(ctx, t, []string{testSourceTag})
	d := newDiscoveryDest(ctx, t)

	// Build state with a different anchor source.
	format := mustGit(t).format(d.localGit.ObjectFormat(ctx))
	wrongAnchor := strings.Repeat("f", format.HexLength())
	doc := state.Document{
		Schema:       state.Schema,
		ObjectFormat: format,
		Destination:  state.Destination{Repository: testRepository, Module: testModulePath},
		Anchor:       state.Anchor{Source: wrongAnchor, Ref: testSourceRef},
		Epoch: state.Epoch{
			Profile:     testProfileHash,
			Source:      sourceCommit,
			Destination: d.parent,
		},
		Cursors: []state.Cursor{{Ref: testSourceRef, Source: sourceCommit, Destination: d.parent}},
		Engine:  state.Engine{Version: "test", Toolchain: "go1.26.5"},
	}
	doc, err := state.New(doc)
	if err != nil {
		t.Fatalf("build state: %v", err)
	}
	record, err := state.Store(ctx, d.localGit, state.StoreOptions{
		Document: doc, Author: testSignature, Committer: testSignature,
	})
	if err != nil {
		t.Fatalf("store state: %v", err)
	}
	mustDo(t, d.localGit.CreateRef(ctx, testStateRef, record.Commit))
	mustDo(t, d.localGit.PushAtomic(ctx, d.remoteDir, []gitcli.PushUpdate{
		{Ref: testStateRef, New: record.Commit, ExpectAbsent: true},
	}))

	_, err = sync.Discover(ctx, d.opts(cache, sourceCommit))
	if err == nil {
		t.Fatal("expected error for anchor source mismatch")
	}
	if !strings.Contains(err.Error(), "state anchor source") {
		t.Fatalf("error %q does not explain anchor mismatch", err)
	}
}

func TestDiscoverStateAnchorRefMismatch(t *testing.T) {
	ctx := t.Context()
	cache, sourceCommit := newTestSource(ctx, t, []string{testSourceTag})
	d := newDiscoveryDest(ctx, t)

	// Build state with the correct anchor source but wrong ref.
	format := mustGit(t).format(d.localGit.ObjectFormat(ctx))
	doc := state.Document{
		Schema:       state.Schema,
		ObjectFormat: format,
		Destination:  state.Destination{Repository: testRepository, Module: testModulePath},
		Anchor:       state.Anchor{Source: sourceCommit, Ref: "refs/tags/v1.99.0"},
		Epoch: state.Epoch{
			Profile:     testProfileHash,
			Source:      sourceCommit,
			Destination: d.parent,
		},
		Cursors: []state.Cursor{{Ref: testSourceRef, Source: sourceCommit, Destination: d.parent}},
		Engine:  state.Engine{Version: "test", Toolchain: "go1.26.5"},
	}
	doc, err := state.New(doc)
	if err != nil {
		t.Fatalf("build state: %v", err)
	}
	record, err := state.Store(ctx, d.localGit, state.StoreOptions{
		Document: doc, Author: testSignature, Committer: testSignature,
	})
	if err != nil {
		t.Fatalf("store state: %v", err)
	}
	mustDo(t, d.localGit.CreateRef(ctx, testStateRef, record.Commit))
	mustDo(t, d.localGit.PushAtomic(ctx, d.remoteDir, []gitcli.PushUpdate{
		{Ref: testStateRef, New: record.Commit, ExpectAbsent: true},
	}))

	_, err = sync.Discover(ctx, d.opts(cache, sourceCommit))
	if err == nil {
		t.Fatal("expected error for anchor ref mismatch")
	}
	if !strings.Contains(err.Error(), "state anchor ref") {
		t.Fatalf("error %q does not explain anchor ref mismatch", err)
	}
}

// --- Exact tag message adoption tests ---

// adoptionTestSetup builds a destination with state recording the branch but
// not the tag, and returns the pieces needed to create a tag with a custom
// message and run discovery.
func adoptionTestSetup(ctx context.Context, t *testing.T) (*discoveryDest, *source.Cache, string, string) {
	t.Helper()
	cache, sourceCommit := newTestSource(ctx, t, []string{testSourceTag})
	d := newDiscoveryDest(ctx, t)
	cfg := discoveryConfig(sourceCommit)

	blob := mustGit(t).object(d.localGit.WriteBlob(ctx, []byte("published\n")))
	tree := mustGit(t).object(d.localGit.WriteTree(ctx, []gitcli.TreeEntry{
		{Mode: gitcli.ModeRegular, Object: blob, Path: "p.go"},
	}))
	commitMessage := "Release " + testReleaseTag + "\n\n" + cfg.Commit.TrailerKey + ": " + sourceCommit + "\n"
	destCommit := mustGit(t).object(d.localGit.WriteCommit(ctx, gitcli.CommitTreeOptions{
		Tree: tree, Parents: []string{d.parent},
		Author: testSignature, Committer: testSignature,
		Message: commitMessage,
	}))

	// Build state recording the branch but not the tag.
	format := mustGit(t).format(d.localGit.ObjectFormat(ctx))
	doc := state.Document{
		Schema:       state.Schema,
		ObjectFormat: format,
		Destination:  state.Destination{Repository: testRepository, Module: testModulePath},
		Anchor:       state.Anchor{Source: sourceCommit, Ref: testSourceRef},
		Epoch: state.Epoch{
			Profile:     testProfileHash,
			Source:      sourceCommit,
			Destination: d.parent,
		},
		Cursors: []state.Cursor{{Ref: testSourceRef, Source: sourceCommit, Destination: destCommit}},
		Published: []state.Published{
			{Ref: testBranchRef, Kind: state.KindBranch, Object: destCommit, Source: sourceCommit},
		},
		Engine: state.Engine{Version: "test", Toolchain: "go1.26.5"},
	}
	doc, err := state.New(doc)
	if err != nil {
		t.Fatalf("build state: %v", err)
	}
	record, err := state.Store(ctx, d.localGit, state.StoreOptions{
		Document: doc, Author: testSignature, Committer: testSignature,
	})
	if err != nil {
		t.Fatalf("store state: %v", err)
	}
	mustDo(t, d.localGit.CreateRef(ctx, testStateRef, record.Commit))

	return d, cache, sourceCommit, destCommit
}

// finishAdoption creates the tag with the given message, pushes everything, and runs discovery.
func finishAdoption(ctx context.Context, t *testing.T, d *discoveryDest, cache *source.Cache, sourceCommit, destCommit, tagMessage string) error {
	t.Helper()

	mustDo(t, d.localGit.CreateTag(ctx, gitcli.TagOptions{
		Name: testReleaseTag, Commit: destCommit,
		Tagger: testSignature, Message: tagMessage,
	}))
	tagRefs := mustGit(t).refs(d.localGit.ListRefs(ctx, "refs/tags/"+testReleaseTag))
	tagOID := tagRefs[0].Target

	stateRefs := mustGit(t).refs(d.localGit.ListRefs(ctx, testStateRef))
	stateCommit := stateRefs[0].Target

	mustDo(t, d.localGit.PushAtomic(ctx, d.remoteDir, []gitcli.PushUpdate{
		{Ref: testBranchRef, New: destCommit, ExpectAbsent: true},
		{Ref: testStateRef, New: stateCommit, ExpectAbsent: true},
		{Ref: "refs/tags/" + testReleaseTag, New: tagOID, ExpectAbsent: true},
	}))
	mustDo(t, d.localGit.UpdateRef(ctx, testBranchRef, destCommit, d.parent))

	opts := d.opts(cache, sourceCommit)
	_, err := sync.Discover(ctx, opts)
	return err
}

func TestDiscoverAdoptionRefusesDuplicateSourceKey(t *testing.T) {
	ctx := t.Context()
	d, cache, sourceCommit, destCommit := adoptionTestSetup(ctx, t)

	sourceURL := "https://github.com/kubernetes/kubernetes/releases/tag/" + testSourceTag
	// Duplicate Source-tag line.
	tagMessage := testReleaseTag + "\n\n" +
		"Source-tag: " + testSourceTag + "\n" +
		"Source-tag: " + testSourceTag + "\n" +
		"Source-commit: " + sourceCommit + "\n" +
		"Source-release: " + sourceURL + "\n"

	err := finishAdoption(ctx, t, d, cache, sourceCommit, destCommit, tagMessage)
	if err == nil {
		t.Fatal("expected error for duplicate Source-tag key")
	}
	if !strings.Contains(err.Error(), "message does not match expected format") {
		t.Fatalf("error %q does not explain message mismatch", err)
	}
}

func TestDiscoverAdoptionRefusesExtraTrailer(t *testing.T) {
	ctx := t.Context()
	d, cache, sourceCommit, destCommit := adoptionTestSetup(ctx, t)

	sourceURL := "https://github.com/kubernetes/kubernetes/releases/tag/" + testSourceTag
	// Extra trailer key.
	tagMessage := testReleaseTag + "\n\n" +
		"Source-tag: " + testSourceTag + "\n" +
		"Source-commit: " + sourceCommit + "\n" +
		"Source-release: " + sourceURL + "\n" +
		"Extra-key: value\n"

	err := finishAdoption(ctx, t, d, cache, sourceCommit, destCommit, tagMessage)
	if err == nil {
		t.Fatal("expected error for extra trailer")
	}
	if !strings.Contains(err.Error(), "message does not match expected format") {
		t.Fatalf("error %q does not explain message mismatch", err)
	}
}

func TestDiscoverAdoptionRefusesWrongSourceTag(t *testing.T) {
	ctx := t.Context()
	d, cache, sourceCommit, destCommit := adoptionTestSetup(ctx, t)

	sourceURL := "https://github.com/kubernetes/kubernetes/releases/tag/" + testSourceTag
	// Source-tag value is wrong.
	tagMessage := testReleaseTag + "\n\n" +
		"Source-tag: v1.99.0\n" +
		"Source-commit: " + sourceCommit + "\n" +
		"Source-release: " + sourceURL + "\n"

	err := finishAdoption(ctx, t, d, cache, sourceCommit, destCommit, tagMessage)
	if err == nil {
		t.Fatal("expected error for wrong Source-tag value")
	}
	if !strings.Contains(err.Error(), "message does not match expected format") {
		t.Fatalf("error %q does not explain message mismatch", err)
	}
}

// --- Anchor resolution from state ---

func TestDiscoverResolvesAnchorFromState(t *testing.T) {
	ctx := t.Context()
	cache, sourceCommit := newTestSource(ctx, t, []string{testSourceTag})
	d := newDiscoveryDest(ctx, t)

	// Publish full state with branch and tag.
	sc, _, _ := d.publishedState(ctx, t, sourceCommit)

	// Run discovery with empty anchorCommit — state should provide it.
	opts := d.opts(cache, sourceCommit)
	opts.Config.Source.Refs.AnchorCommit = ""
	disc, err := sync.Discover(ctx, opts)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if disc.StateCommit != sc {
		t.Errorf("state commit = %q, want %q", disc.StateCommit, sc)
	}
	if disc.ResolvedAnchor == nil {
		t.Fatal("expected a resolved anchor")
	}
	if disc.ResolvedAnchor.Commit != sourceCommit {
		t.Errorf("resolved anchor commit = %q, want %q", disc.ResolvedAnchor.Commit, sourceCommit)
	}
	wantRef := "refs/tags/" + testSourceTag
	if disc.ResolvedAnchor.Ref != wantRef {
		t.Errorf("resolved anchor ref = %q, want %q", disc.ResolvedAnchor.Ref, wantRef)
	}
}

func TestDiscoverAdoptsConsumerRefsFromCompletedTrack(t *testing.T) {
	ctx := t.Context()
	const (
		oldSourceTag = "v1.36.1"
		newSourceTag = "v1.36.2"
		oldDestTag   = "v0.36.1"
		newDestTag   = "v0.36.2"
	)

	sourceRepo := testsupport.NewRepo(ctx, t, testsupport.Options{
		Branch: "main", UserName: "Test", UserEmail: "test@test.com",
	})
	sourceRepo.SetConfig(ctx, t, "uploadpack.allowFilter", "true")
	oldSource := sourceRepo.WriteAndCommit(ctx, t, "main.go", "package main\n", "feat: initial\n")
	if err := sourceRepo.Git.CreateTag(ctx, gitcli.TagOptions{
		Name: oldSourceTag, Commit: oldSource, Tagger: testSignature, Message: "Release " + oldSourceTag + "\n",
	}); err != nil {
		t.Fatalf("tag old source: %v", err)
	}
	newSource := sourceRepo.WriteAndCommit(ctx, t, "main.go", "package main\nconst Next = true\n", "feat: next\n")
	if err := sourceRepo.Git.CreateTag(ctx, gitcli.TagOptions{
		Name: newSourceTag, Commit: newSource, Tagger: testSignature, Message: "Release " + newSourceTag + "\n",
	}); err != nil {
		t.Fatalf("tag new source: %v", err)
	}
	root := t.TempDir()
	sourceGit, err := gitcli.New(ctx, gitcli.Options{Dir: root, Inherit: []string{"PATH"}})
	if err != nil {
		t.Fatalf("source runner: %v", err)
	}
	cache, err := source.Open(ctx, source.Options{
		Remote: "file://" + sourceRepo.Dir, CacheRoot: filepath.Join(root, "cache"),
		WorktreeRoot: filepath.Join(root, "worktrees"), Git: sourceGit,
	})
	if err != nil {
		t.Fatalf("open source cache: %v", err)
	}
	if err := cache.Fetch(ctx, source.Refs{AllTags: true}); err != nil {
		t.Fatalf("fetch source tags: %v", err)
	}

	d := newDiscoveryDest(ctx, t)
	cfg := discoveryConfig(oldSource)
	writeDestination := func(parent, sourceCommit, message string) string {
		blob, err := d.localGit.WriteBlob(ctx, []byte(message+"\n"))
		if err != nil {
			t.Fatalf("write destination blob: %v", err)
		}
		tree, err := d.localGit.WriteTree(ctx, []gitcli.TreeEntry{{Mode: gitcli.ModeRegular, Object: blob, Path: "p.go"}})
		if err != nil {
			t.Fatalf("write destination tree: %v", err)
		}
		commit, err := d.localGit.WriteCommit(ctx, gitcli.CommitTreeOptions{
			Tree: tree, Parents: []string{parent}, Author: testSignature, Committer: testSignature,
			Message: message + "\n\n" + cfg.Commit.TrailerKey + ": " + sourceCommit + "\n",
		})
		if err != nil {
			t.Fatalf("write destination commit: %v", err)
		}
		return commit
	}
	oldDest := writeDestination(d.parent, oldSource, "old release")
	newDest := writeDestination(oldDest, newSource, "new release")

	tagMessage := func(sourceTag, sourceCommit, destTag string) string {
		return destTag + "\n\n" +
			"Source-tag: " + sourceTag + "\n" +
			"Source-commit: " + sourceCommit + "\n" +
			"Source-release: https://github.com/kubernetes/kubernetes/releases/tag/" + sourceTag + "\n"
	}
	for _, tag := range []struct{ name, sourceTag, sourceCommit, target string }{
		{name: oldDestTag, sourceTag: oldSourceTag, sourceCommit: oldSource, target: oldDest},
		{name: newDestTag, sourceTag: newSourceTag, sourceCommit: newSource, target: newDest},
	} {
		if err := d.localGit.CreateTag(ctx, gitcli.TagOptions{
			Name: tag.name, Commit: tag.target, Tagger: testSignature,
			Message: tagMessage(tag.sourceTag, tag.sourceCommit, tag.name),
		}); err != nil {
			t.Fatalf("tag destination %s: %v", tag.name, err)
		}
	}
	tagRefs, err := d.localGit.ListRefs(ctx, "refs/tags/"+oldDestTag, "refs/tags/"+newDestTag)
	if err != nil {
		t.Fatalf("list destination tags: %v", err)
	}
	tagObjects := map[string]string{}
	for _, ref := range tagRefs {
		tagObjects[ref.Name] = ref.Target
	}

	format, err := d.localGit.ObjectFormat(ctx)
	if err != nil {
		t.Fatalf("object format: %v", err)
	}
	doc, err := state.New(state.Document{
		Schema: state.Schema, ObjectFormat: format,
		Destination: state.Destination{Repository: testRepository, Module: testModulePath},
		Anchor:      state.Anchor{Source: oldSource, Ref: "refs/tags/" + oldSourceTag},
		Epoch:       state.Epoch{Profile: testProfileHash, Source: oldSource, Destination: d.parent},
		Cursors:     []state.Cursor{{Ref: "refs/tags/" + oldSourceTag, Source: oldSource, Destination: oldDest}},
		Tracks: []state.Track{{
			Name: newDestTag, Ref: state.ProgressNamespace + newDestTag,
			Source: newSource, Destination: newDest, Done: 2, Total: 2,
		}},
		Published: []state.Published{
			{Ref: testBranchRef, Kind: state.KindBranch, Object: oldDest, Source: oldSource},
			{Ref: "refs/tags/" + oldDestTag, Kind: state.KindTag, Object: tagObjects["refs/tags/"+oldDestTag], Source: oldSource},
		},
		Engine: state.Engine{Version: "test", Toolchain: "go1.26.5"},
	})
	if err != nil {
		t.Fatalf("build checkpoint state: %v", err)
	}
	record, err := state.Store(ctx, d.localGit, state.StoreOptions{Document: doc, Author: testSignature, Committer: testSignature})
	if err != nil {
		t.Fatalf("store checkpoint state: %v", err)
	}
	if err := d.localGit.UpdateRef(ctx, testBranchRef, newDest, d.parent); err != nil {
		t.Fatalf("advance local branch: %v", err)
	}
	if err := d.localGit.CreateRef(ctx, testStateRef, record.Commit); err != nil {
		t.Fatalf("create state ref: %v", err)
	}
	if err := d.localGit.CreateRef(ctx, state.ProgressNamespace+newDestTag, newDest); err != nil {
		t.Fatalf("create progress ref: %v", err)
	}
	if err := d.localGit.PushAtomic(ctx, d.remoteDir, []gitcli.PushUpdate{
		{Ref: testBranchRef, New: newDest, ExpectAbsent: true},
		{Ref: testStateRef, New: record.Commit, ExpectAbsent: true},
		{Ref: state.ProgressNamespace + newDestTag, New: newDest, ExpectAbsent: true},
		{Ref: "refs/tags/" + oldDestTag, New: tagObjects["refs/tags/"+oldDestTag], ExpectAbsent: true},
		{Ref: "refs/tags/" + newDestTag, New: tagObjects["refs/tags/"+newDestTag], ExpectAbsent: true},
	}); err != nil {
		t.Fatalf("push crashed consumer state: %v", err)
	}

	result, err := sync.Discover(ctx, d.opts(cache, oldSource))
	if err != nil {
		t.Fatalf("discover tracked consumer adoption: %v", err)
	}
	if result.AdoptedBranch == nil || result.AdoptedBranch.Object != newDest || result.AdoptedBranch.Source != newSource {
		t.Errorf("adopted branch = %#v, want %s from %s", result.AdoptedBranch, newDest, newSource)
	}
	if result.Adopted == nil || result.Adopted.Ref != "refs/tags/"+newDestTag || result.Adopted.Object != tagObjects["refs/tags/"+newDestTag] {
		t.Errorf("adopted tag = %#v, want %s", result.Adopted, tagObjects["refs/tags/"+newDestTag])
	}
	if len(result.Pending) != 0 || result.FixedPoint() {
		t.Errorf("pending = %#v fixedPoint=%v, want adoption-only reconciliation", result.Pending, result.FixedPoint())
	}
	var newRevision source.Revision
	tags, err := cache.ListTags(ctx)
	if err != nil {
		t.Fatalf("list source tags: %v", err)
	}
	for _, revision := range tags {
		if revision.Name == newSourceTag {
			newRevision = revision
			break
		}
	}
	if newRevision.Commit == "" {
		t.Fatalf("source cache has no %s revision", newSourceTag)
	}
	discoverOpts := d.opts(cache, oldSource)
	reconciliation, err := sync.PlanAdoptionReconciliation(ctx, sync.FinalizeOptions{
		Config: discoverOpts.Config, Discovery: result, SourceCache: cache,
		Destination: sync.Destination{
			Git: discoverOpts.LocalGit, Remote: discoverOpts.Remote,
			Identity: discoverOpts.Identity, AllowLocalRemote: discoverOpts.AllowLocalRemote,
			Lister: discoverOpts.Lister,
		},
		Release: source.Release{Source: newRevision, DestinationTag: newDestTag},
	})
	if err != nil {
		t.Fatalf("plan adoption reconciliation: %v", err)
	}
	if _, err := sync.ApplyReconciliation(ctx, reconciliation, reconciliation.Publish.Hash()); err != nil {
		t.Fatalf("apply adoption reconciliation: %v", err)
	}
	remoteRefs, err := d.localGit.RemoteRefs(ctx, d.remoteDir, format.HexLength())
	if err != nil {
		t.Fatalf("list reconciled remote refs: %v", err)
	}
	refs := make(map[string]string, len(remoteRefs))
	for _, ref := range remoteRefs {
		refs[ref.Name] = ref.Target
	}
	if refs[testStateRef] != reconciliation.State.Commit {
		t.Errorf("reconciled remote state = %s, want %s", refs[testStateRef], reconciliation.State.Commit)
	}
	reconciled, err := state.Load(ctx, d.localGit, reconciliation.State.Commit)
	if err != nil {
		t.Fatalf("load reconciled state: %v", err)
	}
	if len(reconciled.Tracks) != 0 {
		t.Errorf("reconciled state retained tracks: %#v", reconciled.Tracks)
	}
	for _, want := range []string{testBranchRef, "refs/tags/" + newDestTag} {
		if !slices.ContainsFunc(reconciled.Published, func(entry state.Published) bool { return entry.Ref == want }) {
			t.Errorf("reconciled state does not publish %s: %#v", want, reconciled.Published)
		}
	}
	if err := d.localGit.UpdateRef(ctx, testStateRef, reconciliation.State.Commit, record.Commit); err != nil {
		t.Fatalf("advance local state ref: %v", err)
	}
	fixed, err := sync.Discover(ctx, d.opts(cache, oldSource))
	if err != nil {
		t.Fatalf("discover reconciled fixed point: %v", err)
	}
	if !fixed.FixedPoint() || len(fixed.Pending) != 0 {
		t.Errorf("reconciled discovery fixedPoint=%v pending=%#v adopted=%#v branch=%#v", fixed.FixedPoint(), fixed.Pending, fixed.Adopted, fixed.AdoptedBranch)
	}
}
