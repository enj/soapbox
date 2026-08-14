package sync_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/extract"
	"github.com/enj/soapbox/tools/internal/generate"
	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/publish"
	"github.com/enj/soapbox/tools/internal/sync"
)

// TestProjectPlansTheFirstReleaseWithoutTouchingTheRemote is the shape every
// other test here varies: a destination that has never been published to, one
// release, and a plan that says exactly what publishing it would do.
func TestProjectPlansTheFirstReleaseWithoutTouchingTheRemote(t *testing.T) {
	ctx := t.Context()
	dest := newDestination(ctx, t)

	result, err := sync.Project(ctx, dest.options())
	if err != nil {
		t.Fatalf("project: %v", err)
	}

	if got := result.Manifest.Source.ReleaseTag; got != testReleaseTag {
		t.Errorf("release tag = %q, want %q", got, testReleaseTag)
	}
	if result.Manifest.Objects.Tree == "" || result.Manifest.Objects.Commit == "" {
		t.Errorf("objects = %+v, want a tree and a commit", result.Manifest.Objects)
	}
	// The release projection tree is the replayed tree for a single release
	// epoch, so the tag names the replayed commit and no projection commit
	// exists. A commit here would mean the engine published an empty change.
	if got := result.Manifest.Objects.ProjectionCommit; got != "" {
		t.Errorf("projection commit = %q, want none: the release tree is the replayed tree", got)
	}
	if got, want := result.Manifest.Objects.TagTarget, result.Manifest.Objects.Commit; got != want {
		t.Errorf("tag target = %q, want the replayed commit %q", got, want)
	}
	commit, err := dest.git.CommitInfo(ctx, result.Manifest.Objects.Commit)
	if err != nil {
		t.Fatalf("read replay commit: %v", err)
	}
	if len(commit.Parents) != 1 || commit.Parents[0] != dest.parent {
		t.Errorf("replay parents = %v, want the setup-derived commit %s", commit.Parents, dest.parent)
	}
	entries, err := dest.git.ListTree(ctx, result.Manifest.Objects.Tree)
	if err != nil {
		t.Fatalf("read composed tree: %v", err)
	}
	paths := make(map[string]bool, len(entries))
	for _, entry := range entries {
		paths[entry.Path] = true
	}
	for _, preserved := range []string{".github/workflows/ci.yml", ".github/workflows/sync.yml", ".gitignore", "patches/index.yaml", "soapbox.yaml", "tools/cmd/soapbox/main.go", "tools/go.mod"} {
		if !paths[preserved] {
			t.Errorf("composed tree dropped control-plane path %s", preserved)
		}
	}
	if paths["internal/kk/stale/stale.go"] {
		t.Error("composed tree retained a stale generated file from the parent")
	}
	if got := result.Document.Epoch.Destination; got != dest.parent {
		t.Errorf("state epoch parent = %q, want setup-derived commit %q", got, dest.parent)
	}

	// Three refs, each created, and every one of them still absent.
	want := map[string]publish.Effect{
		testStateRef:                  publish.EffectCreate,
		testBranchRef:                 publish.EffectCreate,
		"refs/tags/" + testReleaseTag: publish.EffectCreate,
	}
	got := make(map[string]publish.Effect)
	for _, action := range result.Manifest.Publish.Actions {
		got[action.Ref] = action.Effect
		if action.OldObject != "" {
			t.Errorf("action %s reports old object %q, want none on a fresh destination", action.Ref, action.OldObject)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("planned refs = %v, want %v", got, want)
	}
	for ref, effect := range want {
		if got[ref] != effect {
			t.Errorf("ref %s effect = %q, want %q", ref, got[ref], effect)
		}
	}

	if refs := dest.remoteRefs(ctx, t); len(refs) != 0 {
		t.Errorf("the remote holds %v after planning, want nothing: a plan publishes nothing", refs)
	}
}

func TestProjectRefusesWithoutASetupDerivedParent(t *testing.T) {
	ctx := t.Context()
	localRoot := t.TempDir()
	local := newRepository(ctx, t, localRoot, testBranch)
	remoteRoot := t.TempDir()
	remote := newRepository(ctx, t, remoteRoot, testBranch)
	if err := remote.SetConfigLocal(ctx, "core.bare", "true"); err != nil {
		t.Fatalf("make the remote bare: %v", err)
	}
	dest := &destination{git: local, dir: localRoot, remote: filepath.Join(remoteRoot, ".git")}

	if _, err := sync.Project(ctx, dest.options()); !errors.Is(err, sync.ErrUnsupported) {
		t.Fatalf("project = %v, want a missing control-plane refusal", err)
	}
	if refs := dest.remoteRefs(ctx, t); len(refs) != 0 {
		t.Errorf("the remote holds %v after the refusal, want nothing", refs)
	}
}

func TestProjectRefusesAnUnrelatedParent(t *testing.T) {
	ctx := t.Context()
	localRoot := t.TempDir()
	local := newRepository(ctx, t, localRoot, testBranch)
	object, err := local.WriteBlob(ctx, []byte("not a soapbox repository\n"))
	if err != nil {
		t.Fatalf("write unrelated blob: %v", err)
	}
	tree, err := local.WriteTree(ctx, []gitcli.TreeEntry{{Mode: gitcli.ModeRegular, Object: object, Path: "README.md"}})
	if err != nil {
		t.Fatalf("write unrelated tree: %v", err)
	}
	commit, err := local.WriteCommit(ctx, gitcli.CommitTreeOptions{
		Tree: tree, Author: testSignature, Committer: testSignature, Message: "chore: unrelated\n",
	})
	if err != nil {
		t.Fatalf("write unrelated commit: %v", err)
	}
	if err := local.CreateRef(ctx, testBranchRef, commit); err != nil {
		t.Fatalf("create unrelated HEAD: %v", err)
	}
	remoteRoot := t.TempDir()
	remote := newRepository(ctx, t, remoteRoot, testBranch)
	if err := remote.SetConfigLocal(ctx, "core.bare", "true"); err != nil {
		t.Fatalf("make the remote bare: %v", err)
	}
	dest := &destination{git: local, dir: localRoot, remote: filepath.Join(remoteRoot, ".git")}

	if _, err := sync.Project(ctx, dest.options()); !errors.Is(err, sync.ErrUnsupported) {
		t.Fatalf("project = %v, want an unrelated control-plane refusal", err)
	}
}

func TestProjectFastForwardsAPublishedControlPlane(t *testing.T) {
	ctx := t.Context()
	dest := newDestination(ctx, t)
	dest.publishControlPlane(ctx, t)

	result, err := sync.Project(ctx, dest.options())
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	for _, action := range result.Manifest.Publish.Actions {
		if action.Ref != testBranchRef {
			continue
		}
		if action.Effect != publish.EffectFastForward || action.OldObject != dest.parent {
			t.Errorf("branch action = %+v, want a fast-forward from setup commit %s", action, dest.parent)
		}
		return
	}
	t.Fatal("publication plan has no consumer branch action")
}

// TestPlanIsDeterministicAcrossRoots proves the manifest describes the work
// rather than the machine.
//
// Two synchronizations of one release into two different temporary directories
// have to produce the same hash. It is the property the whole approval flow
// rests on: an operator approves a hash computed on one machine and the
// publication that quotes it runs on another.
func TestPlanIsDeterministicAcrossRoots(t *testing.T) {
	ctx := t.Context()

	first, err := sync.Project(ctx, newDestination(ctx, t).options())
	if err != nil {
		t.Fatalf("first project: %v", err)
	}
	second, err := sync.Project(ctx, newDestination(ctx, t).options())
	if err != nil {
		t.Fatalf("second project: %v", err)
	}

	if first.Manifest.Hash != second.Manifest.Hash {
		t.Errorf("manifest hash differs across roots:\n first  %s\n second %s",
			first.Manifest.Hash, second.Manifest.Hash)
	}
	firstJSON, err := first.Manifest.JSON()
	if err != nil {
		t.Fatalf("encode first manifest: %v", err)
	}
	secondJSON, err := second.Manifest.JSON()
	if err != nil {
		t.Fatalf("encode second manifest: %v", err)
	}
	if string(firstJSON) != string(secondJSON) {
		t.Errorf("manifest bytes differ across roots:\n%s\n%s", firstJSON, secondJSON)
	}
}

// TestManifestCarriesNoPathOrSecret checks the artifact an approval quotes.
//
// The manifest is attached to a review and kept as the record of what was
// authorized, so a temporary directory from the machine that produced it would
// both break the comparison above and leak into that record.
func TestManifestCarriesNoPathOrSecret(t *testing.T) {
	ctx := t.Context()
	dest := newDestination(ctx, t)

	result, err := sync.Project(ctx, dest.options())
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	encoded, err := result.Manifest.JSON()
	if err != nil {
		t.Fatalf("encode manifest: %v", err)
	}

	rendered := string(encoded) + result.Manifest.Text()
	for _, forbidden := range []struct{ what, value string }{
		{"the local repository", dest.dir},
		{"the destination remote path", dest.remote},
	} {
		if forbidden.value == "" {
			continue
		}
		if strings.Contains(rendered, forbidden.value) {
			t.Errorf("the manifest names %s (%q)", forbidden.what, forbidden.value)
		}
	}
	// The canonical destination is a repository rather than a location, which is
	// what makes the manifest comparable between a local dry run and the real
	// publication it rehearses.
	if got := result.Manifest.Publish.Remote; got != testIdentity {
		t.Errorf("manifest remote = %q, want the canonical identity %q", got, testIdentity)
	}
}

// TestManifestHashCoversEveryField proves the approval hash is worth quoting.
//
// An approval names one hash, so every field a reviewer reads has to be inside
// it. A field that could change without changing the hash is a field an
// approval does not actually cover.
func TestManifestHashCoversEveryField(t *testing.T) {
	ctx := t.Context()

	result, err := sync.Project(ctx, newDestination(ctx, t).options())
	if err != nil {
		t.Fatalf("project: %v", err)
	}

	for _, tc := range []struct {
		name   string
		mutate func(*sync.Manifest)
	}{
		{"the upstream commit", func(m *sync.Manifest) { m.Source.Commit = strings.Repeat("9", 40) }},
		{"the profile hash", func(m *sync.Manifest) { m.Engine.ProfileHash = "sha256:" + strings.Repeat("f", 64) }},
		{"the generated tree", func(m *sync.Manifest) { m.Objects.Tree = strings.Repeat("9", 40) }},
		{"the state commit", func(m *sync.Manifest) { m.Objects.StateCommit = strings.Repeat("9", 40) }},
		{"the module manifest hash", func(m *sync.Manifest) { m.Module.ManifestHash = "sha256:" + strings.Repeat("c", 64) }},
		{"a behavior change", func(m *sync.Manifest) { m.Module.BehaviorChanges = nil }},
		{"the dependency decision", func(m *sync.Manifest) { m.Module.Dependencies.Copied = 7 }},
		{"a planned ref", func(m *sync.Manifest) { m.Publish.Actions[0].Ref = "refs/heads/elsewhere" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			modified := result.Manifest
			modified.Publish.Actions = append([]publish.Action(nil), modified.Publish.Actions...)
			tc.mutate(&modified)
			if err := modified.Verify(); err == nil {
				t.Fatalf("changing %s left the manifest verifying, so the hash does not cover it", tc.name)
			} else if !errors.Is(err, sync.ErrManifestModified) && !errors.Is(err, publish.ErrManifestModified) {
				t.Fatalf("changing %s reported %v, want a modified manifest", tc.name, err)
			}
		})
	}
}

// TestProjectHonoursCancellation proves a cancelled run writes nothing.
func TestProjectHonoursCancellation(t *testing.T) {
	ctx := t.Context()
	dest := newDestination(ctx, t)

	cancelled, cancel := context.WithCancel(ctx)
	cancel()

	if _, err := sync.Project(cancelled, dest.options()); !errors.Is(err, context.Canceled) {
		t.Fatalf("project = %v, want a cancellation", err)
	}
	if refs := dest.remoteRefs(ctx, t); len(refs) != 0 {
		t.Errorf("the remote holds %v after a cancelled run, want nothing", refs)
	}
}

// TestProjectRefusesWithoutADestinationRemote proves publication is off by
// default rather than merely unlikely.
func TestProjectRefusesWithoutADestinationRemote(t *testing.T) {
	ctx := t.Context()
	dest := newDestination(ctx, t)

	opts := dest.options()
	opts.Destination.Remote = ""
	if _, err := sync.Project(ctx, opts); !errors.Is(err, sync.ErrPublicationDisabled) {
		t.Fatalf("project = %v, want a disabled publication", err)
	}

	// A local remote is reachable only because the caller said so. Without the
	// option a mistyped configuration would publish into a directory.
	opts = dest.options()
	opts.Destination.AllowLocalRemote = false
	if _, err := sync.Project(ctx, opts); !errors.Is(err, publish.ErrLocalRemoteNotAllowed) {
		t.Fatalf("project = %v, want a refused local remote", err)
	}
}

// TestPlanRefusesARunShapeItCannotPublish covers the guards Plan applies before
// it starts a generation.
//
// They run first because a generation is minutes of work against a clone of
// upstream, and a run that could never have published its result should not
// spend them. A branch is the case that matters: it is a well formed request
// this engine cannot serve, because a release is what gets published and a
// branch names none.
func TestPlanRefusesARunShapeItCannotPublish(t *testing.T) {
	ctx := t.Context()
	dest := newDestination(ctx, t)

	base := func() sync.Options {
		return sync.Options{
			Generate: generate.Options{
				Config: testConfig(),
				Ref:    extract.Ref{Kind: extract.RefTag, Name: testSourceTag},
			},
			Destination: dest.options().Destination,
		}
	}

	for _, tc := range []struct {
		name   string
		mutate func(*sync.Options)
		want   error
	}{{
		name:   "a branch instead of a release",
		mutate: func(o *sync.Options) { o.Generate.Ref = extract.Ref{Kind: extract.RefBranch, Name: "master"} },
		want:   sync.ErrUnsupported,
	}, {
		name:   "no profile",
		mutate: func(o *sync.Options) { o.Generate.Config = nil },
		want:   nil,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			opts := base()
			tc.mutate(&opts)
			_, err := sync.Plan(ctx, opts)
			if err == nil {
				t.Fatalf("plan succeeded, want a refusal")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("plan = %v, want %v", err, tc.want)
			}
			if refs := dest.remoteRefs(ctx, t); len(refs) != 0 {
				t.Errorf("the remote holds %v after a refused plan, want nothing", refs)
			}
		})
	}
}

// TestProjectRecordsBranchAndTagThenFixedPoint proves that after publication,
// a second synchronization records both the branch and the tag in Published,
// and a third synchronization with the same state is a fixed-point no-op
// (digest unchanged, state commit reused).
func TestProjectRecordsBranchAndTagThenFixedPoint(t *testing.T) {
	ctx := t.Context()
	dest := newDestination(ctx, t)

	// First sync: no published refs yet.
	first, err := sync.Project(ctx, dest.options())
	if err != nil {
		t.Fatalf("first project: %v", err)
	}

	// The first sync records no Published entries because the destination was
	// empty when it was read.
	if len(first.Document.Published) != 0 {
		t.Errorf("first sync published = %d entries, want 0", len(first.Document.Published))
	}

	// Simulate publication: push the consumer branch, tag, and state.
	head := first.Replay.Heads[0].Destination
	tagRef := "refs/tags/" + first.Release.Tag
	if err := dest.git.PushAtomic(ctx, dest.remote, []gitcli.PushUpdate{
		{Ref: testBranchRef, New: head, ExpectAbsent: true},
		{Ref: tagRef, New: first.Release.Object, ExpectAbsent: true},
		{Ref: testStateRef, New: first.State.Commit, ExpectAbsent: true},
	}); err != nil {
		t.Fatalf("simulate first publication: %v", err)
	}
	if err := dest.git.UpdateRef(ctx, testBranchRef, head, dest.parent); err != nil {
		t.Fatalf("advance local branch: %v", err)
	}

	// Second sync: sees the published branch and tag.
	opts := dest.options()
	opts.StateCommit = first.State.Commit
	second, err := sync.Project(ctx, opts)
	if err != nil {
		t.Fatalf("second project: %v", err)
	}

	// The second sync records both branch and tag in Published.
	wantKinds := map[string]bool{testBranchRef: false, tagRef: false}
	for _, pub := range second.Document.Published {
		if _, expected := wantKinds[pub.Ref]; expected {
			wantKinds[pub.Ref] = true
		}
	}
	for ref, found := range wantKinds {
		if !found {
			t.Errorf("second sync did not record Published entry for %s", ref)
		}
	}

	// Simulate publication of the second state.
	if err := dest.git.PushAtomic(ctx, dest.remote, []gitcli.PushUpdate{
		{Ref: testStateRef, New: second.State.Commit, ExpectedOld: first.State.Commit},
	}); err != nil {
		t.Fatalf("simulate second publication: %v", err)
	}

	// Third sync: same destination, same release — fixed-point no-op.
	opts = dest.options()
	opts.StateCommit = second.State.Commit
	third, err := sync.Project(ctx, opts)
	if err != nil {
		t.Fatalf("third project: %v", err)
	}

	// The digest is unchanged: the third run sees the same Published entries
	// the second recorded.
	if second.Document.Digest != third.Document.Digest {
		t.Errorf("digest changed between second and third run: %s vs %s",
			second.Document.Digest, third.Document.Digest)
	}
	// The state commit is reused rather than rewritten.
	if second.State.Commit != third.State.Commit {
		t.Errorf("state commit changed between second and third run: %s vs %s",
			second.State.Commit, third.State.Commit)
	}
}
