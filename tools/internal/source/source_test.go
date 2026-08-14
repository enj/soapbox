package source_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/gitgraph"
	"github.com/enj/soapbox/tools/internal/source"
	"github.com/enj/soapbox/tools/internal/testsupport"
)

const (
	testUserName  = "Soapbox Test"
	testUserEmail = "test@example.com"

	mainBranch    = "main"
	releaseBranch = "release-1.36"
	releaseTag    = "v1.36.1"
)

// upstream is a real repository with the topology source discovery has to cope
// with: a mainline that merges a side branch, a release branch that diverged
// from a shared base, and an annotated release tag.
//
//	base ── mainOne ── merge      (main)
//	  │        └── feature ─┘
//	  └── release                 (release-1.36, tagged v1.36.1)
type upstream struct {
	repo                                   *testsupport.Repo
	base, mainOne, feature, merge, release string
}

// newUpstream builds the fixture and enables partial clone filtering, without
// which the server hands back every blob and a blobless cache would silently be
// a full one.
func newUpstream(ctx context.Context, t *testing.T) *upstream {
	t.Helper()

	repo := testsupport.NewRepo(ctx, t, testsupport.Options{
		Branch:    mainBranch,
		UserName:  testUserName,
		UserEmail: testUserEmail,
	})
	repo.SetConfig(ctx, t, "uploadpack.allowFilter", "true")

	up := &upstream{repo: repo}
	up.base = repo.WriteAndCommit(ctx, t, "README.md", "base\n", "docs: add readme\n")
	repo.WriteFile(t, "pkg/apis/rbac/types.go", "package rbac\n")
	repo.WriteFile(t, "pkg/apis/rbac/v1/doc.go", "package v1\n")
	repo.WriteFile(t, "pkg/apis/rbac/v1/helpers.go", "package v1\n")
	repo.WriteFile(t, "pkg/apis/rbac/v1/nested/deep.go", "package nested\n")
	repo.WriteFile(t, "pkg/apis/rbac/v1beta1/types.go", "package v1beta1\n")
	repo.WriteFile(t, "plugin/pkg/auth/authorizer/rbac/rbac.go", "package rbac\n")
	up.mainOne = repo.Commit(ctx, t, "feat: add authorizer\n", gitcli.CommitOptions{},
		"pkg/apis/rbac/types.go",
		"pkg/apis/rbac/v1/doc.go",
		"pkg/apis/rbac/v1/helpers.go",
		"pkg/apis/rbac/v1/nested/deep.go",
		"pkg/apis/rbac/v1beta1/types.go",
		"plugin/pkg/auth/authorizer/rbac/rbac.go",
	)

	up.checkout(ctx, t, up.base)
	up.feature = repo.WriteAndCommit(ctx, t,
		"pkg/registry/rbac/validation/rule.go", "package validation\n", "feat: add rule resolver\n")

	tree, err := repo.Git.ResolveTree(ctx, up.mainOne)
	if err != nil {
		t.Fatalf("resolve tree: %v", err)
	}
	up.merge, err = repo.Git.WriteCommit(ctx, gitcli.CommitTreeOptions{
		Tree:      tree,
		Parents:   []string{up.mainOne, up.feature},
		Message:   "Merge rule resolver\n",
		Author:    gitcli.Signature{Name: testUserName, Email: testUserEmail, Date: "1700000000 +0000"},
		Committer: gitcli.Signature{Name: testUserName, Email: testUserEmail, Date: "1700000000 +0000"},
	})
	if err != nil {
		t.Fatalf("write merge commit: %v", err)
	}
	up.updateRef(ctx, t, "refs/heads/"+mainBranch, up.merge)

	up.checkout(ctx, t, up.base)
	up.release = repo.WriteAndCommit(ctx, t, "release.md", "1.36\n", "docs: open 1.36\n")
	up.updateRef(ctx, t, "refs/heads/"+releaseBranch, up.release)
	if err := repo.Git.CreateTag(ctx, gitcli.TagOptions{
		Name:    releaseTag,
		Commit:  up.release,
		Message: "Kubernetes v1.36.1\n",
		Tagger:  gitcli.Signature{Name: testUserName, Email: testUserEmail, Date: "2026-01-02T03:04:05Z"},
	}); err != nil {
		t.Fatalf("create tag: %v", err)
	}
	up.checkout(ctx, t, "refs/heads/"+mainBranch)
	return up
}

func (u *upstream) url() string { return "file://" + u.repo.Dir }

func (u *upstream) checkout(ctx context.Context, t *testing.T, revision string) {
	t.Helper()
	if err := u.repo.Git.CheckoutDetached(ctx, revision); err != nil {
		t.Fatalf("checkout %s: %v", revision, err)
	}
}

// updateRef points a fixture ref at a commit, creating it when it does not
// exist yet.
//
// The current value is read first because the typed runner only performs
// compare and swap updates: a blind write is exactly the local rewind the
// engine must not be able to perform by accident.
func (u *upstream) updateRef(ctx context.Context, t *testing.T, name, revision string) {
	t.Helper()
	exists, err := u.repo.Git.HasRef(ctx, name)
	if err != nil {
		t.Fatalf("probe %s: %v", name, err)
	}
	if !exists {
		if err := u.repo.Git.CreateRef(ctx, name, revision); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		return
	}
	current, err := u.repo.Git.ResolveCommit(ctx, name)
	if err != nil {
		t.Fatalf("resolve %s: %v", name, err)
	}
	if err := u.repo.Git.UpdateRef(ctx, name, revision, current); err != nil {
		t.Fatalf("update %s: %v", name, err)
	}
}

// runner builds a runner whose HOME is isolated through the process
// environment, so the isolation survives the credential stripping that source
// commands perform.
func runner(t *testing.T, dir string) *gitcli.Runner {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	git, err := gitcli.New(t.Context(), gitcli.Options{Dir: dir, Inherit: []string{"PATH", "HOME"}})
	if err != nil {
		t.Fatalf("create git runner: %v", err)
	}
	return git
}

// openCache opens a cache with fresh roots under the test temporary directory.
func openCache(ctx context.Context, t *testing.T, up *upstream) *source.Cache {
	t.Helper()
	root := t.TempDir()
	cache, err := source.Open(ctx, source.Options{
		Remote:       up.url(),
		CacheRoot:    filepath.Join(root, "cache"),
		WorktreeRoot: filepath.Join(root, "worktrees"),
		Git:          runner(t, root),
	})
	if err != nil {
		t.Fatalf("open cache: %v", err)
	}
	return cache
}

// allRefs names every ref the fixture tracks.
var allRefs = source.Refs{Branches: []string{mainBranch, releaseBranch}, Tags: []string{releaseTag}}

func TestOpenClonesThenReuses(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)
	root := t.TempDir()
	opts := source.Options{
		Remote:       up.url(),
		CacheRoot:    filepath.Join(root, "cache"),
		WorktreeRoot: filepath.Join(root, "worktrees"),
		Git:          runner(t, root),
	}

	first, err := source.Open(ctx, opts)
	if err != nil {
		t.Fatalf("open cache: %v", err)
	}
	if !first.Created() {
		t.Fatal("the first open must clone")
	}
	if !first.Git().IsAnonymous() {
		t.Fatal("cache commands must run anonymously")
	}

	// The cache is the expensive part of a run, so a second open reuses it.
	second, err := source.Open(ctx, opts)
	if err != nil {
		t.Fatalf("reopen cache: %v", err)
	}
	if second.Created() {
		t.Fatal("the second open must reuse the existing cache")
	}
	if second.Path() != first.Path() {
		t.Fatalf("cache path %q, want %q", second.Path(), first.Path())
	}
	if !strings.HasPrefix(second.Path(), filepath.Join(root, "cache")) {
		t.Fatalf("cache %q is outside its root", second.Path())
	}
	if second.Remote() != up.url() {
		t.Fatalf("remote %q, want %q", second.Remote(), up.url())
	}

	// A different remote gets its own cache directory rather than colliding
	// with the first one under the same root.
	other := newUpstream(ctx, t)
	otherOpts := opts
	otherOpts.Remote = other.url()
	otherCache, err := source.Open(ctx, otherOpts)
	if err != nil {
		t.Fatalf("open second cache: %v", err)
	}
	if otherCache.Path() == first.Path() {
		t.Fatalf("two remotes share the cache directory %q", otherCache.Path())
	}
	if !strings.HasPrefix(otherCache.Path(), filepath.Join(root, "cache")) {
		t.Fatalf("cache %q is outside its root", otherCache.Path())
	}
}

// TestOpenStripsCredentials proves a runner carrying a credential is made
// anonymous before it can reach the source host.
func TestOpenStripsCredentials(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)
	root := t.TempDir()

	t.Setenv("HOME", t.TempDir())
	credentialed, err := gitcli.New(ctx, gitcli.Options{
		Dir:     root,
		Inherit: []string{"PATH", "HOME"},
		Env:     []string{"SOAPBOX_TOKEN=super-secret-value"},
	})
	if err != nil {
		t.Fatalf("create runner: %v", err)
	}
	if credentialed.IsAnonymous() {
		t.Fatal("a runner with Env entries must not be anonymous")
	}

	cache, err := source.Open(ctx, source.Options{
		Remote:       up.url(),
		CacheRoot:    filepath.Join(root, "cache"),
		WorktreeRoot: filepath.Join(root, "worktrees"),
		Git:          credentialed,
	})
	if err != nil {
		t.Fatalf("open cache: %v", err)
	}
	if !cache.Git().IsAnonymous() {
		t.Fatal("the cache runner still carries caller supplied environment entries")
	}
}

func TestOpenRejectsUnusableCacheDirectory(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)

	tests := []struct {
		name    string
		prepare func(t *testing.T, cacheRoot, name string)
		want    string
	}{
		{
			name: "existing non repository",
			prepare: func(t *testing.T, cacheRoot, name string) {
				t.Helper()
				if err := os.MkdirAll(filepath.Join(cacheRoot, name), 0o750); err != nil {
					t.Fatalf("create directory: %v", err)
				}
			},
			want: "is not a bare repository",
		},
		{
			name: "existing file",
			prepare: func(t *testing.T, cacheRoot, name string) {
				t.Helper()
				if err := os.MkdirAll(cacheRoot, 0o750); err != nil {
					t.Fatalf("create root: %v", err)
				}
				if err := os.WriteFile(filepath.Join(cacheRoot, name), []byte("not a cache\n"), 0o600); err != nil {
					t.Fatalf("write file: %v", err)
				}
			},
			want: "is not a directory",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			cacheRoot := filepath.Join(root, "cache")
			const name = "cache.git"
			test.prepare(t, cacheRoot, name)

			_, err := source.Open(ctx, source.Options{
				Remote:       up.url(),
				CacheRoot:    cacheRoot,
				WorktreeRoot: filepath.Join(root, "worktrees"),
				Directory:    name,
				Git:          runner(t, root),
			})
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error %q does not mention %q", err, test.want)
			}
		})
	}
}

func TestOpenValidation(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	git := runner(t, root)

	tests := []struct {
		name string
		opts source.Options
		want string
	}{
		{
			name: "disallowed host",
			opts: source.Options{
				Remote:       "https://evil.example/kubernetes/kubernetes.git",
				CacheRoot:    filepath.Join(root, "c"),
				WorktreeRoot: filepath.Join(root, "w"),
				Git:          git,
			},
			want: "must fetch source from github.com",
		},
		{
			name: "no runner",
			opts: source.Options{
				Remote:       "https://github.com/kubernetes/kubernetes.git",
				CacheRoot:    filepath.Join(root, "c"),
				WorktreeRoot: filepath.Join(root, "w"),
			},
			want: "a git runner is required",
		},
		{
			name: "no cache root",
			opts: source.Options{
				Remote:       "https://github.com/kubernetes/kubernetes.git",
				WorktreeRoot: filepath.Join(root, "w"),
				Git:          git,
			},
			want: "a cache root is required",
		},
		{
			name: "no work tree root",
			opts: source.Options{
				Remote:    "https://github.com/kubernetes/kubernetes.git",
				CacheRoot: filepath.Join(root, "c"),
				Git:       git,
			},
			want: "a work tree root is required",
		},
		{
			// A cache directory that escapes its root would put the clone
			// somewhere the caller never agreed to write.
			name: "directory traversal",
			opts: source.Options{
				Remote:       "https://github.com/kubernetes/kubernetes.git",
				CacheRoot:    filepath.Join(root, "c"),
				WorktreeRoot: filepath.Join(root, "w"),
				Directory:    "../escape.git",
				Git:          git,
			},
			want: "traverse parent directories",
		},
		{
			name: "nested directory",
			opts: source.Options{
				Remote:       "https://github.com/kubernetes/kubernetes.git",
				CacheRoot:    filepath.Join(root, "c"),
				WorktreeRoot: filepath.Join(root, "w"),
				Directory:    "nested/cache.git",
				Git:          git,
			},
			want: "must be a single element",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := source.Open(ctx, test.opts)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error %q does not mention %q", err, test.want)
			}
		})
	}
}

func TestResolve(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)
	cache := openCache(ctx, t, up)

	if err := cache.Fetch(ctx, allRefs); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	revisions, err := cache.Resolve(ctx, allRefs)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(revisions) != 3 {
		t.Fatalf("resolved %d refs, want 3", len(revisions))
	}

	byName := make(map[string]source.Revision, len(revisions))
	for _, revision := range revisions {
		byName[revision.Name] = revision
	}
	main := byName[mainBranch]
	if main.Kind != source.KindBranch || main.Ref != "refs/heads/"+mainBranch {
		t.Fatalf("main resolved to %+v", main)
	}
	if main.Commit != up.merge {
		t.Fatalf("main commit %q, want %q", main.Commit, up.merge)
	}
	if main.Annotated {
		t.Fatal("a branch is not an annotated tag")
	}

	tag := byName[releaseTag]
	if tag.Kind != source.KindTag || tag.Ref != "refs/tags/"+releaseTag {
		t.Fatalf("tag resolved to %+v", tag)
	}
	// A release tag is annotated, so its own object differs from the commit it
	// peels to, and the generated release copies its tagger date.
	if !tag.Annotated {
		t.Fatal("the release tag must be annotated")
	}
	if tag.Commit != up.release {
		t.Fatalf("tag commit %q, want %q", tag.Commit, up.release)
	}
	if tag.Object == tag.Commit {
		t.Fatal("an annotated tag's object must differ from its commit")
	}
}

func TestResolveCommitUsesOnlyAnAlreadyFetchedObject(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)
	cache := openCache(ctx, t, up)
	if err := cache.Fetch(ctx, allRefs); err != nil {
		t.Fatalf("fetch refs: %v", err)
	}

	revision, err := cache.ResolveCommit(ctx, up.release)
	if err != nil {
		t.Fatalf("resolve exact commit: %v", err)
	}
	if revision.Kind != source.KindCommit || revision.Name != up.release ||
		revision.Ref != up.release || revision.Object != up.release || revision.Commit != up.release {
		t.Fatalf("revision = %#v, want exact commit %s", revision, up.release)
	}
	if revision.Annotated {
		t.Fatal("an exact commit was reported as an annotated tag")
	}

	blob, err := cache.Git().WriteBlob(ctx, []byte("not a commit"))
	if err != nil {
		t.Fatalf("write blob: %v", err)
	}
	tests := []struct {
		name   string
		commit string
		want   string
	}{
		{name: "malformed object name", commit: "abc123", want: "40 or 64 hexadecimal"},
		{name: "missing object", commit: strings.Repeat("f", 40), want: "missing from the cache"},
		{name: "wrong object type", commit: blob, want: "blob, not a commit"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := cache.ResolveCommit(ctx, test.commit)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error %q does not contain %q", err, test.want)
			}
		})
	}
}

func TestResolveRejectsUnusableRefs(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)
	cache := openCache(ctx, t, up)
	if err := cache.Fetch(ctx, allRefs); err != nil {
		t.Fatalf("fetch: %v", err)
	}

	tests := []struct {
		name    string
		refs    source.Refs
		prepare func(t *testing.T)
		want    string
	}{
		{
			// A profile naming a branch upstream deleted must stop the run
			// rather than replay a partial set of refs.
			name: "missing branch",
			refs: source.Refs{Branches: []string{"release-1.35"}},
			want: `branch "release-1.35" is missing from the cache`,
		},
		{
			name: "missing tag",
			refs: source.Refs{Tags: []string{"v9.99.0"}},
			want: `tag "v9.99.0" is missing from the cache`,
		},
		{
			// A ref pointing at an object the cache never received is corrupt,
			// and lazy fetching stays off so it is reported rather than fetched.
			name: "corrupt ref",
			refs: source.Refs{Branches: []string{"corrupt"}},
			prepare: func(t *testing.T) {
				t.Helper()
				const absent = "0123456789012345678901234567890123456789\n"
				path := filepath.Join(cache.Path(), "refs", "heads", "corrupt")
				if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
					t.Fatalf("create ref directory: %v", err)
				}
				if err := os.WriteFile(path, []byte(absent), 0o600); err != nil {
					t.Fatalf("write ref: %v", err)
				}
				t.Cleanup(func() {
					if err := os.Remove(path); err != nil {
						t.Errorf("remove ref: %v", err)
					}
				})
			},
			want: "corrupt",
		},
		{name: "flag like branch", refs: source.Refs{Branches: []string{"--all"}}, want: "must not start with a dash"},
		{name: "qualified branch", refs: source.Refs{Branches: []string{"refs/heads/main"}}, want: "must be a short name"},
		{name: "qualified tag", refs: source.Refs{Tags: []string{"refs/tags/v1.36.1"}}, want: "must be a short name"},
		{name: "empty tag", refs: source.Refs{Tags: []string{""}}, want: "must not be empty"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.prepare != nil {
				test.prepare(t)
			}
			_, err := cache.Resolve(ctx, test.refs)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error %q does not mention %q", err, test.want)
			}
		})
	}
}

func TestListBranchesAndTags(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)

	// A branch name with a slash is listed like any other. A trailing star in
	// the ref pattern would not match it, and the missing branch would look like
	// a branch upstream had deleted.
	up.updateRef(ctx, t, "refs/heads/feature/nested", up.base)
	cache := openCache(ctx, t, up)

	branches, err := cache.ListBranches(ctx)
	if err != nil {
		t.Fatalf("list branches: %v", err)
	}
	names := make([]string, 0, len(branches))
	for _, branch := range branches {
		names = append(names, branch.Name)
		if branch.Kind != source.KindBranch {
			t.Fatalf("branch %q has kind %q", branch.Name, branch.Kind)
		}
	}
	if want := []string{"feature/nested", mainBranch, releaseBranch}; !slices.Equal(names, want) {
		t.Fatalf("branches %v, want %v", names, want)
	}

	if err := cache.Fetch(ctx, source.Refs{AllTags: true}); err != nil {
		t.Fatalf("fetch tags: %v", err)
	}
	tags, err := cache.ListTags(ctx)
	if err != nil {
		t.Fatalf("list tags: %v", err)
	}
	if len(tags) != 1 || tags[0].Name != releaseTag || tags[0].Commit != up.release {
		t.Fatalf("tags %+v, want the release tag", tags)
	}
}

func TestFetchRequiresRefs(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)
	cache := openCache(ctx, t, up)

	err := cache.Fetch(ctx, source.Refs{})
	if err == nil || !strings.Contains(err.Error(), "at least one branch or tag") {
		t.Fatalf("error %v does not require a ref", err)
	}
}

func TestAnchor(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)
	cache := openCache(ctx, t, up)

	anchor, err := cache.Anchor(ctx, []string{"refs/heads/" + mainBranch, "refs/heads/" + releaseBranch})
	if err != nil {
		t.Fatalf("anchor: %v", err)
	}
	if anchor != up.base {
		t.Fatalf("anchor %q, want the base commit %q", anchor, up.base)
	}

	single, err := cache.Anchor(ctx, []string{"refs/heads/" + releaseBranch})
	if err != nil {
		t.Fatalf("single revision anchor: %v", err)
	}
	if single != up.release {
		t.Fatalf("anchor %q, want %q", single, up.release)
	}
	if _, err := cache.Anchor(ctx, nil); err == nil {
		t.Fatal("expected an error without revisions")
	}
}

// TestGraphBoundedByAnchor is the seam between the Git command line and the pure
// graph algorithms: real commit metadata is read from a real repository and then
// answered against with gitgraph.
func TestGraphBoundedByAnchor(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)
	cache := openCache(ctx, t, up)
	heads := []string{"refs/heads/" + mainBranch, "refs/heads/" + releaseBranch}

	graph, err := cache.Graph(ctx, source.GraphOptions{Heads: heads, Anchor: up.base})
	if err != nil {
		t.Fatalf("graph: %v", err)
	}
	if graph.Len() != 5 {
		t.Fatalf("graph holds %d commits, want 5", graph.Len())
	}

	// The anchor is present as the root of the transformed history, and the
	// history before it is not.
	if roots := graph.Roots(); !slices.Equal(roots, []string{up.base}) {
		t.Fatalf("roots %v, want the anchor %q", roots, up.base)
	}
	if err := graph.ValidateAnchor(up.base, []string{up.merge, up.release}); err != nil {
		t.Fatalf("validate anchor: %v", err)
	}

	selected, err := graph.Range(up.base, []string{up.merge, up.release})
	if err != nil {
		t.Fatalf("range: %v", err)
	}
	if len(selected) != 5 || selected[0] != up.base {
		t.Fatalf("range %v does not start at the anchor", selected)
	}
	position := make(map[string]int, len(selected))
	for i, sha := range selected {
		position[sha] = i
	}
	for _, pair := range [][2]string{
		{up.base, up.mainOne},
		{up.base, up.feature},
		{up.mainOne, up.merge},
		{up.feature, up.merge},
		{up.base, up.release},
	} {
		if position[pair[0]] >= position[pair[1]] {
			t.Fatalf("commit %s is not ordered before its child %s", pair[0], pair[1])
		}
	}

	// The merge kept both parents in git's order, which is what lets replay tell
	// the mainline from the side branch.
	parents, err := graph.Parents(up.merge)
	if err != nil {
		t.Fatalf("parents: %v", err)
	}
	if want := []string{up.mainOne, up.feature}; !slices.Equal(parents, want) {
		t.Fatalf("merge parents %v, want %v", parents, want)
	}
	line, err := graph.FirstParentLine(up.merge)
	if err != nil {
		t.Fatalf("first parent line: %v", err)
	}
	if want := []string{up.merge, up.mainOne, up.base}; !slices.Equal(line, want) {
		t.Fatalf("mainline %v, want %v", line, want)
	}

	// The anchor computed from the graph agrees with the one git computed.
	anchor, err := graph.CommonAnchor([]string{up.merge, up.release})
	if err != nil {
		t.Fatalf("common anchor: %v", err)
	}
	if anchor != up.base {
		t.Fatalf("common anchor %q, want %q", anchor, up.base)
	}
}

func TestGraphWithoutAnchorWalksToTheRoot(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)
	cache := openCache(ctx, t, up)

	graph, err := cache.Graph(ctx, source.GraphOptions{Heads: []string{"refs/heads/" + mainBranch}})
	if err != nil {
		t.Fatalf("graph: %v", err)
	}
	if graph.Len() != 4 {
		t.Fatalf("graph holds %d commits, want 4", graph.Len())
	}

	firstParent, err := cache.Graph(ctx, source.GraphOptions{
		Heads:       []string{"refs/heads/" + mainBranch},
		FirstParent: true,
	})
	if err != nil {
		t.Fatalf("first parent graph: %v", err)
	}
	if want := []string{up.base, up.mainOne, up.merge}; !slices.Equal(firstParent.TopologicalOrder(), want) {
		t.Fatalf("mainline %v, want %v", firstParent.TopologicalOrder(), want)
	}

	if _, err := cache.Graph(ctx, source.GraphOptions{}); err == nil {
		t.Fatal("expected an error without a head")
	}
	if _, err := cache.Graph(ctx, source.GraphOptions{
		Heads:  []string{"refs/heads/" + mainBranch},
		Anchor: "refs/heads/absent",
	}); err == nil {
		t.Fatal("expected an error for an unresolvable anchor")
	}
}

// TestGraphIsDeterministic runs the same discovery twice against the same
// repository, because published history has to be reproducible.
func TestGraphIsDeterministic(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)
	cache := openCache(ctx, t, up)
	opts := source.GraphOptions{
		Heads:  []string{"refs/heads/" + mainBranch, "refs/heads/" + releaseBranch},
		Anchor: up.base,
	}

	first, err := cache.Graph(ctx, opts)
	if err != nil {
		t.Fatalf("graph: %v", err)
	}
	want := first.TopologicalOrder()
	for range 8 {
		next, err := cache.Graph(ctx, opts)
		if err != nil {
			t.Fatalf("graph: %v", err)
		}
		if got := next.TopologicalOrder(); !slices.Equal(got, want) {
			t.Fatalf("order %v, want %v", got, want)
		}
	}
}

func TestAddWorktreeMaterializesPackages(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)
	cache := openCache(ctx, t, up)

	patterns, err := source.SparsePatterns([]string{"pkg/apis/rbac/v1", "plugin/pkg/auth/authorizer/rbac"}, false)
	if err != nil {
		t.Fatalf("sparse patterns: %v", err)
	}
	worktree, err := cache.AddWorktree(ctx, source.WorktreeOptions{Commit: up.mainOne, Patterns: patterns})
	if err != nil {
		t.Fatalf("add worktree: %v", err)
	}

	if worktree.Commit() != up.mainOne {
		t.Fatalf("materialized %q, want %q", worktree.Commit(), up.mainOne)
	}
	if !strings.HasPrefix(worktree.Path(), cache.WorktreeRoot()) {
		t.Fatalf("work tree %q is outside its root %q", worktree.Path(), cache.WorktreeRoot())
	}

	// Package granularity: the roots' own files, without the subpackage and
	// without anything the profile did not name.
	want := []string{
		"pkg/apis/rbac/v1/doc.go",
		"pkg/apis/rbac/v1/helpers.go",
		"plugin/pkg/auth/authorizer/rbac/rbac.go",
	}
	if got := materializedPaths(t, worktree.Path()); !slices.Equal(got, want) {
		t.Fatalf("materialized %v, want %v", got, want)
	}

	// Removal is idempotent so a deferred cleanup after a failure cannot mask
	// the error that caused it.
	if err := worktree.Remove(ctx); err != nil {
		t.Fatalf("remove worktree: %v", err)
	}
	if _, err := os.Stat(worktree.Path()); !os.IsNotExist(err) {
		t.Fatalf("work tree survived removal: %v", err)
	}
	if err := worktree.Remove(ctx); err != nil {
		t.Fatalf("second removal: %v", err)
	}
}

// TestAddWorktreeMaterializesNestedPackages proves the pattern set against real
// git rather than against the rendering in TestSparsePatterns.
//
// The claim is not obvious. An ancestor root keeps the exclusion that hides its
// subdirectories, so the only reason the nested root's files arrive is that git
// reads the pattern file with gitignore semantics and lets a later include undo
// an earlier exclusion for the paths it names. Nothing but a real checkout can
// confirm that, and the alternative rendering, which drops the ancestor's
// exclusion instead, passes an exact-output test while silently materializing
// every sibling subpackage.
func TestAddWorktreeMaterializesNestedPackages(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)
	cache := openCache(ctx, t, up)

	patterns, err := source.SparsePatterns([]string{"pkg/apis/rbac", "pkg/apis/rbac/v1"}, false)
	if err != nil {
		t.Fatalf("sparse patterns: %v", err)
	}
	worktree, err := cache.AddWorktree(ctx, source.WorktreeOptions{Commit: up.mainOne, Patterns: patterns})
	if err != nil {
		t.Fatalf("add worktree: %v", err)
	}
	t.Cleanup(func() {
		if err := worktree.Remove(context.WithoutCancel(ctx)); err != nil {
			t.Errorf("remove worktree: %v", err)
		}
	})

	// The ancestor's own files and the nested root's own files, and nothing
	// else: not v1beta1, which is a sibling subpackage of the ancestor, and not
	// v1/nested, which is a subpackage of the leaf root.
	want := []string{
		"pkg/apis/rbac/types.go",
		"pkg/apis/rbac/v1/doc.go",
		"pkg/apis/rbac/v1/helpers.go",
	}
	if got := materializedPaths(t, worktree.Path()); !slices.Equal(got, want) {
		t.Fatalf("materialized %v, want %v", got, want)
	}
}

// TestWorktreeSetPatternsWidensAndRestores covers the extraction pipeline's
// widening loop: a work tree materialized for one package grows to hold a
// second, and the tree it ends up with is the one upstream produced rather than
// whatever an earlier pass left behind.
func TestWorktreeSetPatternsWidensAndRestores(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)
	cache := openCache(ctx, t, up)

	narrow, err := source.SparsePatterns([]string{"pkg/apis/rbac/v1"}, false)
	if err != nil {
		t.Fatalf("sparse patterns: %v", err)
	}
	worktree, err := cache.AddWorktree(ctx, source.WorktreeOptions{Commit: up.mainOne, Patterns: narrow})
	if err != nil {
		t.Fatalf("add worktree: %v", err)
	}
	t.Cleanup(func() {
		if err := worktree.Remove(context.WithoutCancel(ctx)); err != nil {
			t.Errorf("remove worktree: %v", err)
		}
	})

	// A removal stands in for the pruning an earlier closure pass performs. The
	// widened tree has to hold it again, because the pre-prune measurement only
	// means what it says over the tree as upstream produced it.
	if err := os.Remove(filepath.Join(worktree.Path(), "pkg", "apis", "rbac", "v1", "helpers.go")); err != nil {
		t.Fatalf("remove file: %v", err)
	}

	wide, err := source.SparsePatterns([]string{"pkg/apis/rbac/v1", "plugin/pkg/auth/authorizer/rbac"}, false)
	if err != nil {
		t.Fatalf("sparse patterns: %v", err)
	}
	if err := worktree.SetPatterns(ctx, wide); err != nil {
		t.Fatalf("set patterns: %v", err)
	}

	want := []string{
		"pkg/apis/rbac/v1/doc.go",
		"pkg/apis/rbac/v1/helpers.go",
		"plugin/pkg/auth/authorizer/rbac/rbac.go",
	}
	if got := materializedPaths(t, worktree.Path()); !slices.Equal(got, want) {
		t.Fatalf("materialized %v, want %v", got, want)
	}

	// Widening a work tree must not move anything in the shared cache.
	head, err := cache.Git().ResolveCommit(ctx, "refs/heads/"+mainBranch)
	if err != nil {
		t.Fatalf("resolve cached branch: %v", err)
	}
	if head != up.merge {
		t.Fatalf("cached branch moved to %s, want %s", head, up.merge)
	}
}

func TestAddWorktreeVariants(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)
	cache := openCache(ctx, t, up)

	t.Run("named work tree", func(t *testing.T) {
		worktree, err := cache.AddWorktree(ctx, source.WorktreeOptions{Commit: up.base, Name: "explicit"})
		if err != nil {
			t.Fatalf("add worktree: %v", err)
		}
		t.Cleanup(func() {
			if err := worktree.Remove(context.WithoutCancel(ctx)); err != nil {
				t.Errorf("remove worktree: %v", err)
			}
		})
		if filepath.Base(worktree.Path()) != "explicit" {
			t.Fatalf("work tree %q does not use the given name", worktree.Path())
		}
		// Without patterns the whole tree is materialized.
		if got := materializedPaths(t, worktree.Path()); !slices.Equal(got, []string{"README.md"}) {
			t.Fatalf("materialized %v, want the whole tree", got)
		}
	})

	t.Run("rejects a name that escapes the root", func(t *testing.T) {
		if _, err := cache.AddWorktree(ctx, source.WorktreeOptions{Commit: up.base, Name: "../escape"}); err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("rejects an unknown commit", func(t *testing.T) {
		if _, err := cache.AddWorktree(ctx, source.WorktreeOptions{Commit: "refs/heads/absent"}); err == nil {
			t.Fatal("expected an error")
		}
	})

	t.Run("derived names are unique per call", func(t *testing.T) {
		// Two materializations of one commit are the ordinary case when an
		// operator compares profiles or CI runs a matrix. A name derived from
		// the commit alone would make the second fail on the registration the
		// first owns, or hand it a tree the first is pruning files out of.
		first, err := cache.AddWorktree(ctx, source.WorktreeOptions{Commit: up.base})
		if err != nil {
			t.Fatalf("add the first worktree: %v", err)
		}
		t.Cleanup(func() {
			if err := first.Remove(context.WithoutCancel(ctx)); err != nil {
				t.Errorf("remove the first worktree: %v", err)
			}
		})
		second, err := cache.AddWorktree(ctx, source.WorktreeOptions{Commit: up.base})
		if err != nil {
			t.Fatalf("add the second worktree over the same commit: %v", err)
		}
		t.Cleanup(func() {
			if err := second.Remove(context.WithoutCancel(ctx)); err != nil {
				t.Errorf("remove the second worktree: %v", err)
			}
		})

		if first.Path() == second.Path() {
			t.Fatalf("both work trees are %s", first.Path())
		}
		if first.Commit() != second.Commit() {
			t.Fatalf("the two work trees hold %s and %s", first.Commit(), second.Commit())
		}
		// Removing one leaves the other intact and usable.
		if err := second.Remove(ctx); err != nil {
			t.Fatalf("remove the second worktree: %v", err)
		}
		if got := materializedPaths(t, first.Path()); len(got) == 0 {
			t.Error("removing one work tree emptied the other")
		}
	})
}

// TestAddWorktreeNoLazyFetch proves the materialization can refuse to reach the
// promisor remote.
//
// A blobless cache holds every commit and tree and none of the blobs, so a
// checkout is precisely the operation that reaches for them. Refusing to fetch
// says nothing about it, because no fetch call is involved: git downloads what
// the checkout needs on its own. The refusal therefore has to live where the
// checkout does.
func TestAddWorktreeNoLazyFetch(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)

	patterns, err := source.SparsePatterns([]string{"pkg/apis/rbac/v1"}, false)
	if err != nil {
		t.Fatalf("sparse patterns: %v", err)
	}

	t.Run("refused", func(t *testing.T) {
		cache := openCache(ctx, t, up)
		_, err := cache.AddWorktree(ctx, source.WorktreeOptions{
			Commit:      up.mainOne,
			Patterns:    patterns,
			NoLazyFetch: true,
		})
		if err == nil {
			t.Fatal("a blobless cache holds no blobs, so the checkout had to fetch or fail")
		}
		if !strings.Contains(err.Error(), "lazy fetching disabled") {
			t.Fatalf("error %v does not show the refusal", err)
		}
		// The failure is local. Nothing may name the remote, because reaching it
		// is the thing that did not happen.
		if strings.Contains(err.Error(), up.repo.Dir) {
			t.Errorf("the refusal reached for the remote: %v", err)
		}
	})

	t.Run("permitted", func(t *testing.T) {
		// The same materialization without the refusal is the ordinary case a
		// blobless cache exists for, and it has to keep working.
		cache := openCache(ctx, t, up)
		worktree, err := cache.AddWorktree(ctx, source.WorktreeOptions{
			Commit:   up.mainOne,
			Patterns: patterns,
		})
		if err != nil {
			t.Fatalf("add worktree: %v", err)
		}
		t.Cleanup(func() {
			if err := worktree.Remove(context.WithoutCancel(ctx)); err != nil {
				t.Errorf("remove worktree: %v", err)
			}
		})
		if got := materializedPaths(t, worktree.Path()); len(got) == 0 {
			t.Error("the permitted materialization produced no files")
		}
	})
}

// TestWorktreeSetPatternsKeepsTheMatchingMode proves a pattern change does not
// silently switch a tree between cone and pattern matching.
//
// The two modes select different paths: cone matching always includes a matched
// directory's subdirectories, which is exactly what package granularity refuses.
// A caller that asked for one and got the other on its next widening round would
// materialize a tree it never asked for.
func TestWorktreeSetPatternsKeepsTheMatchingMode(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)
	cache := openCache(ctx, t, up)

	worktree, err := cache.AddWorktree(ctx, source.WorktreeOptions{
		Commit:   up.mainOne,
		Cone:     true,
		Patterns: []string{"pkg"},
	})
	if err != nil {
		t.Fatalf("add worktree: %v", err)
	}
	t.Cleanup(func() {
		if err := worktree.Remove(context.WithoutCancel(ctx)); err != nil {
			t.Errorf("remove worktree: %v", err)
		}
	})

	if err := worktree.SetPatterns(ctx, []string{"pkg"}); err != nil {
		t.Fatalf("set patterns: %v", err)
	}
	// Cone mode records the directory it was given; pattern mode would have
	// rewritten the set into the gitignore style patterns git derives.
	patterns, err := worktree.Git().SparseCheckoutPatterns(ctx)
	if err != nil {
		t.Fatalf("read patterns: %v", err)
	}
	if !slices.Contains(patterns, "pkg") {
		t.Errorf("the cone pattern set became %v, so the matching mode changed", patterns)
	}
}

// TestOpenAuditsDynamicPromisorValues proves a per-remote promisor entry is
// checked for what it says rather than only for whose it is.
//
// A filtered fetch that names a URL rather than a configured remote makes git
// record the filter under a section keyed by that URL. The key name proves only
// who the section is about: promisor may be false and the filter may name
// something other than the one the cache is built on, and either would leave the
// cache downloading history the profile never asked for or refusing to fetch the
// blobs a materialization needs.
func TestOpenAuditsDynamicPromisorValues(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)

	tests := []struct {
		name  string
		key   string
		value string
		want  string
	}{
		{
			name:  "promisor turned off",
			key:   "promisor",
			value: "false",
			want:  `not the required "true"`,
		},
		{
			name:  "filter widened",
			key:   "partialclonefilter",
			value: "blob:limit=1g",
			want:  `not the required "blob:none"`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			opts := source.Options{
				Remote:       up.url(),
				CacheRoot:    filepath.Join(root, "cache"),
				WorktreeRoot: filepath.Join(root, "worktrees"),
				Git:          runner(t, root),
			}
			cache, err := source.Open(ctx, opts)
			if err != nil {
				t.Fatalf("open cache: %v", err)
			}

			// The key is the one a filtered fetch against a URL writes, and it
			// names the configured remote, so the name check accepts it.
			key := "remote." + up.url() + "." + test.key
			if err := cache.Git().SetConfigLocal(ctx, key, test.value); err != nil {
				t.Fatalf("write %s: %v", key, err)
			}

			_, err = source.Open(ctx, opts)
			if err == nil {
				t.Fatalf("reopening accepted %s=%s", key, test.value)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error %v does not report the rejected value", err)
			}
		})
	}

	t.Run("the values a filtered fetch really writes are accepted", func(t *testing.T) {
		root := t.TempDir()
		opts := source.Options{
			Remote:       up.url(),
			CacheRoot:    filepath.Join(root, "cache"),
			WorktreeRoot: filepath.Join(root, "worktrees"),
			Git:          runner(t, root),
		}
		cache, err := source.Open(ctx, opts)
		if err != nil {
			t.Fatalf("open cache: %v", err)
		}
		for key, value := range map[string]string{
			"remote." + up.url() + ".promisor":           "true",
			"remote." + up.url() + ".partialclonefilter": "blob:none",
		} {
			if err := cache.Git().SetConfigLocal(ctx, key, value); err != nil {
				t.Fatalf("write %s: %v", key, err)
			}
		}
		if _, err := source.Open(ctx, opts); err != nil {
			t.Fatalf("reopening refused the configuration a filtered fetch writes: %v", err)
		}
	})
}

func TestSparsePatterns(t *testing.T) {
	tests := []struct {
		name      string
		roots     []string
		recursive bool
		want      []string
		wantErr   string
	}{
		{
			name:  "package granular",
			roots: []string{"pkg/apis/rbac/v1"},
			want:  []string{"/pkg/apis/rbac/v1/*", "!/pkg/apis/rbac/v1/*/"},
		},
		{
			name:      "recursive",
			roots:     []string{"pkg/apis/rbac/v1"},
			recursive: true,
			want:      []string{"/pkg/apis/rbac/v1/"},
		},
		{
			// Order is not the caller's to choose: an ancestor's subdirectory
			// exclusion has to precede a nested root's include, so every set is
			// sorted and the sorted form is what the pattern file records.
			name:  "several roots are sorted",
			roots: []string{"plugin/pkg/auth/authorizer/rbac", "pkg/registry/rbac/validation"},
			want: []string{
				"/pkg/registry/rbac/validation/*",
				"!/pkg/registry/rbac/validation/*/",
				"/plugin/pkg/auth/authorizer/rbac/*",
				"!/plugin/pkg/auth/authorizer/rbac/*/",
			},
		},
		{
			// The ancestor keeps its subdirectory exclusion, so the sibling
			// subpackages it would otherwise pull in stay out, and the nested
			// root's include follows that exclusion and undoes it for exactly
			// the directory the closure asked for.
			name:  "nested roots",
			roots: []string{"pkg/apis/rbac/v1", "pkg/apis/rbac"},
			want: []string{
				"/pkg/apis/rbac/*",
				"!/pkg/apis/rbac/*/",
				"/pkg/apis/rbac/v1/*",
				"!/pkg/apis/rbac/v1/*/",
			},
		},
		{
			name:  "duplicate roots collapse",
			roots: []string{"pkg/apis/rbac", "pkg/apis/rbac"},
			want:  []string{"/pkg/apis/rbac/*", "!/pkg/apis/rbac/*/"},
		},
		{
			name:      "nested roots are fine when recursive",
			roots:     []string{"pkg/apis/rbac", "pkg/apis/rbac/v1"},
			recursive: true,
			want:      []string{"/pkg/apis/rbac/", "/pkg/apis/rbac/v1/"},
		},
		{name: "no roots", wantErr: "at least one package root"},
		{name: "absolute root", roots: []string{"/pkg/apis"}, wantErr: "path must be relative"},
		{name: "traversal", roots: []string{"../etc"}, wantErr: "traverse parent directories"},
		{name: "glob root", roots: []string{"pkg/*"}, wantErr: "unsupported character"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := source.SparsePatterns(test.roots, test.recursive)
			if test.wantErr != "" {
				if err == nil {
					t.Fatal("expected an error")
				}
				if !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("error %q does not mention %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("sparse patterns: %v", err)
			}
			if !slices.Equal(got, test.want) {
				t.Fatalf("patterns %v, want %v", got, test.want)
			}
		})
	}
}

func TestCacheOperationsHonourCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	up := newUpstream(ctx, t)
	cache := openCache(ctx, t, up)
	cancel()

	if err := cache.Fetch(ctx, allRefs); !errors.Is(err, context.Canceled) {
		t.Fatalf("fetch error %v is not a cancellation", err)
	}
	if _, err := cache.Resolve(ctx, allRefs); !errors.Is(err, context.Canceled) {
		t.Fatalf("resolve error %v is not a cancellation", err)
	}
	if _, err := cache.Graph(ctx, source.GraphOptions{Heads: []string{"refs/heads/" + mainBranch}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("graph error %v is not a cancellation", err)
	}
	if _, err := cache.AddWorktree(ctx, source.WorktreeOptions{Commit: up.base}); !errors.Is(err, context.Canceled) {
		t.Fatalf("worktree error %v is not a cancellation", err)
	}
}

// TestMappingAcrossRealHistory checks the provenance round trip the replay phase
// depends on: destination commits carry a trailer naming the source commit they
// were produced from, and the mapping is rebuilt from those trailers alone.
func TestMappingAcrossRealHistory(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)
	cache := openCache(ctx, t, up)

	graph, err := cache.Graph(ctx, source.GraphOptions{Heads: []string{"refs/heads/" + mainBranch}})
	if err != nil {
		t.Fatalf("graph: %v", err)
	}

	// Only two source commits produced destination commits; the others changed
	// nothing the extraction keeps.
	destination := []gitgraph.Commit{
		{SHA: strings.Repeat("a", 40), Message: "docs: add readme\n\nKubernetes-commit: " + up.base + "\n"},
		{SHA: strings.Repeat("b", 40), Message: "feat: add authorizer\n\nKubernetes-commit: " + up.mainOne + "\n"},
	}
	mapping, err := gitgraph.MappingFromTrailers(destination, "Kubernetes-commit")
	if err != nil {
		t.Fatalf("mapping from trailers: %v", err)
	}
	if mapping.Len() != 2 {
		t.Fatalf("mapped %d commits, want 2", mapping.Len())
	}

	// The merge's parents both resolve into the destination, and the side branch
	// collapses onto the anchor because nothing on it was materialized.
	parents, err := graph.MappedParents(up.merge, mapping, nil)
	if err != nil {
		t.Fatalf("mapped parents: %v", err)
	}
	want := []string{strings.Repeat("b", 40), strings.Repeat("a", 40)}
	if !slices.Equal(parents, want) {
		t.Fatalf("mapped parents %v, want %v", parents, want)
	}
}

// materializedPaths lists the repository relative files present in a work tree.
func materializedPaths(t *testing.T, root string) []string {
	t.Helper()
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == ".git" {
			return nil
		}
		paths = append(paths, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("walk work tree: %v", err)
	}
	slices.Sort(paths)
	return paths
}
