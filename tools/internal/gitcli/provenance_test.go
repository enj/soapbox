package gitcli_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/testsupport"
)

// evilUpstream is a copy of the fixture upstream with one extra commit on its
// mainline, which is the repository an attacker would like a source command to
// reach instead of the configured one.
//
// It is a clone rather than an unrelated repository on purpose: its history
// fast forwards the cache, so a redirected fetch succeeds and the cache silently
// adopts the attacker's commit. An unrelated history would be refused by the
// non fast forward rule and would prove only that some other safeguard caught
// it. Cloning also keeps this fixture at the same real Git boundary as the code
// under test instead of depending on filesystem-copy directory enumeration.
func evilUpstream(t *testing.T, up *upstream) (dir, commit string) {
	t.Helper()
	ctx := t.Context()

	root := t.TempDir()
	dir = filepath.Join(root, "attacker")
	runner := newAnonymousRunner(t, root)
	if err := runner.CloneSource(ctx, gitcli.SourceCloneOptions{
		Remote: up.url(), Directory: dir,
	}); err != nil {
		t.Fatalf("clone attacker upstream: %v", err)
	}
	git, err := runner.WithDir(dir)
	if err != nil {
		t.Fatalf("scope runner to the attacker repository: %v", err)
	}

	head, err := git.ResolveCommit(ctx, "refs/heads/"+mainBranch)
	if err != nil {
		t.Fatalf("resolve attacker head: %v", err)
	}
	tree, err := git.ResolveTree(ctx, head)
	if err != nil {
		t.Fatalf("resolve tree: %v", err)
	}
	identity := gitcli.Signature{Name: testUserName, Email: testUserEmail, Date: testRawDate}
	commit, err = git.WriteCommit(ctx, gitcli.CommitTreeOptions{
		Tree:      tree,
		Parents:   []string{head},
		Message:   "feat: attacker controlled change\n",
		Author:    identity,
		Committer: identity,
	})
	if err != nil {
		t.Fatalf("write attacker commit: %v", err)
	}
	if err := git.UpdateRef(ctx, "refs/heads/"+mainBranch, commit, head); err != nil {
		t.Fatalf("move attacker branch: %v", err)
	}
	return dir, commit
}

// poison writes a repository configuration file directly, which is how a
// restored or tampered cache carries a key: no git command has to have been run
// for it to take effect on the next transfer.
func poison(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestFetchSourceRejectsRewrittenRemote is the fail-closed behaviour that
// protects which repository the engine transforms.
//
// The cache is the one piece of engine state that survives between runs, so an
// attacker who can write to a build cache directory can leave a URL rewrite in
// it. Git applies the rewrite to the explicit remote on the command line, fetches
// the attacker's history, and reports success while every log line still names
// the configured upstream. Both configuration scopes a repository owns are
// covered, because the per work tree one is invisible to a --local query and
// outranks the local file.
func TestFetchSourceRejectsRewrittenRemote(t *testing.T) {
	ctx := t.Context()

	tests := []struct {
		name string
		// file is the name of the configuration file below the cache directory.
		file string
		// enableWorktreeConfig turns on the extension that makes the per work
		// tree file take effect and outrank the local one.
		enableWorktreeConfig bool
	}{
		{name: "local scope", file: "config"},
		{name: "worktree scope", file: "config.worktree", enableWorktreeConfig: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			up := newUpstream(ctx, t)
			evilDir, evilCommit := evilUpstream(t, up)
			root := t.TempDir()
			runner := newAnonymousRunner(t, root)

			dir := filepath.Join(root, "cache.git")
			if err := runner.CloneSource(ctx, gitcli.SourceCloneOptions{
				Remote: up.url(), Directory: dir, Bare: true,
			}); err != nil {
				t.Fatalf("clone: %v", err)
			}
			cache, err := runner.WithDir(dir)
			if err != nil {
				t.Fatalf("scope runner to the cache: %v", err)
			}
			cached, err := cache.ResolveCommit(ctx, "refs/heads/"+mainBranch)
			if err != nil {
				t.Fatalf("resolve cached branch: %v", err)
			}

			if test.enableWorktreeConfig {
				if err := cache.SetConfigLocal(ctx, "extensions.worktreeConfig", "true"); err != nil {
					t.Fatalf("enable worktree configuration: %v", err)
				}
			}
			// The rewrite names the configured remote exactly, which is the form
			// git applies to a URL given on the command line.
			rewrite := "[url \"file://" + evilDir + "\"]\n\tinsteadOf = " + up.url() + "\n"
			existing := ""
			if test.file == "config" {
				contents, err := os.ReadFile(filepath.Join(dir, "config"))
				if err != nil {
					t.Fatalf("read cache configuration: %v", err)
				}
				existing = string(contents)
			}
			poison(t, filepath.Join(dir, test.file), existing+rewrite)

			spec := "refs/heads/" + mainBranch + ":refs/heads/" + mainBranch
			err = cache.FetchSource(ctx, gitcli.SourceFetchOptions{
				Remote: up.url(), Refspecs: []string{spec},
			})
			if err == nil {
				t.Fatal("a fetch whose remote can be rewritten must fail closed")
			}
			if !strings.Contains(err.Error(), "rewrites the remote") {
				t.Fatalf("error %q does not report a rewritten remote", err)
			}
			// The attacker's history fast forwards the cache, so nothing but the
			// gate stands between this fetch and the cache adopting it.
			got, err := cache.ResolveCommit(ctx, "refs/heads/"+mainBranch)
			if err != nil {
				t.Fatalf("resolve cached branch: %v", err)
			}
			switch got {
			case evilCommit:
				t.Fatal("the cache adopted the attacker's commit")
			case cached:
			default:
				t.Fatalf("cached branch moved to %q, want it to stay at %q", got, cached)
			}
		})
	}
}

// TestCloneSourceRejectsRewrittenRemote covers the same gate for the clone that
// creates the cache, where the rewrite lives in the repository the clone happens
// to run inside.
func TestCloneSourceRejectsRewrittenRemote(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)

	host := testsupport.NewRepo(ctx, t, testsupport.Options{UserName: testUserName, UserEmail: testUserEmail})
	host.SetConfig(ctx, t, "url.https://attacker.example.com/mirror.git.insteadOf", up.url())

	runner := newAnonymousRunner(t, host.Dir)
	err := runner.CloneSource(ctx, gitcli.SourceCloneOptions{
		Remote:    up.url(),
		Directory: filepath.Join(host.Dir, "cache.git"),
		Bare:      true,
	})
	if err == nil {
		t.Fatal("a clone run where a rewrite applies must fail closed")
	}
	if !strings.Contains(err.Error(), "rewrites the remote") {
		t.Fatalf("error %q does not report a rewritten remote", err)
	}
}

// TestLazyFetchRejectsRewrittenRemote covers the transfer that has no remote on
// its command line at all.
//
// A blobless cache downloads a missing blob from its promisor remote on demand,
// so a rewrite redirects that download exactly like an explicit fetch, and the
// objects it returns end up in the transformed history.
func TestLazyFetchRejectsRewrittenRemote(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)
	root := t.TempDir()
	runner := newAnonymousRunner(t, root)

	dir := filepath.Join(root, "cache.git")
	if err := runner.CloneSource(ctx, gitcli.SourceCloneOptions{
		Remote: up.url(), Directory: dir, Bare: true,
	}); err != nil {
		t.Fatalf("clone: %v", err)
	}
	cache, err := runner.WithDir(dir)
	if err != nil {
		t.Fatalf("scope runner to the cache: %v", err)
	}
	if err := cache.SetConfigLocal(ctx, "url.https://attacker.example.com/mirror.git.insteadOf", up.url()); err != nil {
		t.Fatalf("write rewrite: %v", err)
	}

	_, err = cache.ObjectInfoBatch(ctx, gitcli.ObjectInfoOptions{
		Revisions:      []string{up.sha(mainOne)},
		AllowLazyFetch: true,
	})
	if err == nil {
		t.Fatal("a lazy fetch whose promisor remote can be rewritten must fail closed")
	}
	if !strings.Contains(err.Error(), "rewrites the remote") {
		t.Fatalf("error %q does not report a rewritten remote", err)
	}

	// The same probe without lazy fetching answers from the object store, so it
	// is unaffected by a rewrite it can never reach.
	if _, err := cache.ObjectInfoBatch(ctx, gitcli.ObjectInfoOptions{
		Revisions: []string{up.sha(mainOne)},
	}); err != nil {
		t.Fatalf("local probe must not be gated: %v", err)
	}
}

// TestCloneSourceFailsWhenServerIgnoresFilter proves the engine notices a
// partial clone that silently became a complete one.
//
// A server that does not advertise object filtering makes git print a warning,
// exit zero, and record the promisor remote and filter anyway. Configuration
// therefore cannot distinguish a blobless clone from a full one, and a cache
// that quietly holds every blob of a repository the size of Kubernetes is not
// the cache the engine asked for.
func TestCloneSourceFailsWhenServerIgnoresFilter(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)
	// The fixture enables filtering, so it has to be switched off to model a
	// server that does not support it.
	up.repo.SetConfig(ctx, t, "uploadpack.allowFilter", "false")

	root := t.TempDir()
	runner := newAnonymousRunner(t, root)
	dir := filepath.Join(root, "cache.git")

	err := runner.CloneSource(ctx, gitcli.SourceCloneOptions{
		Remote: up.url(), Directory: dir, Bare: true,
	})
	if !errors.Is(err, gitcli.ErrFilterIgnored) {
		t.Fatalf("clone error = %v, want %v", err, gitcli.ErrFilterIgnored)
	}
}

// TestFetchSourceFailsWhenServerIgnoresFilter covers the same degradation on
// the path that updates an existing cache.
func TestFetchSourceFailsWhenServerIgnoresFilter(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)
	root := t.TempDir()
	runner := newAnonymousRunner(t, root)

	dir := filepath.Join(root, "cache.git")
	if err := runner.CloneSource(ctx, gitcli.SourceCloneOptions{
		Remote: up.url(), Directory: dir, Bare: true,
	}); err != nil {
		t.Fatalf("clone: %v", err)
	}
	cache, err := runner.WithDir(dir)
	if err != nil {
		t.Fatalf("scope runner to the cache: %v", err)
	}

	up.repo.SetConfig(ctx, t, "uploadpack.allowFilter", "false")
	added := up.commit(ctx, t, "extra.go", "package extra\n", "feat: add extra\n")
	up.updateRef(ctx, t, "refs/heads/"+mainBranch, added)

	err = cache.FetchSource(ctx, gitcli.SourceFetchOptions{
		Remote:   up.url(),
		Refspecs: []string{"refs/heads/" + mainBranch + ":refs/heads/" + mainBranch},
	})
	if !errors.Is(err, gitcli.ErrFilterIgnored) {
		t.Fatalf("fetch error = %v, want %v", err, gitcli.ErrFilterIgnored)
	}
}

// TestPartialCloneStatusOf proves the probe distinguishes a repository that
// omits its blobs from one that holds them, which is the difference
// configuration cannot express.
func TestPartialCloneStatusOf(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)
	root := t.TempDir()
	runner := newAnonymousRunner(t, root)

	blobless := filepath.Join(root, "blobless.git")
	if err := runner.CloneSource(ctx, gitcli.SourceCloneOptions{
		Remote: up.url(), Directory: blobless, Bare: true,
	}); err != nil {
		t.Fatalf("blobless clone: %v", err)
	}
	partial, err := runner.WithDir(blobless)
	if err != nil {
		t.Fatalf("scope runner: %v", err)
	}
	status, err := partial.PartialCloneStatusOf(ctx, "refs/heads/"+mainBranch)
	if err != nil {
		t.Fatalf("partial clone probe: %v", err)
	}
	if status != gitcli.PartialCloneConfirmed {
		t.Fatalf("blobless clone status = %v, want confirmed", status)
	}

	// A repository holding every blob is what a degraded clone leaves behind.
	// The fixture repository itself is exactly that.
	status, err = up.repo.Git.PartialCloneStatusOf(ctx, "refs/heads/"+mainBranch)
	if err != nil {
		t.Fatalf("partial clone probe: %v", err)
	}
	if status != gitcli.PartialCloneFull {
		t.Fatalf("complete repository status = %v, want full", status)
	}
}

// TestIsBareRepositoryAtDoesNotDiscoverAncestors proves a cache probe describes
// the path it was asked about.
//
// Git's repository discovery walks upwards, so probing a directory that is not
// a repository reports on whichever repository contains it. A cache directory
// that was emptied, or a path below a bare repository, would then present
// itself as a usable cache, and the engine would fetch into and materialize
// from a repository nobody named.
func TestIsBareRepositoryAtDoesNotDiscoverAncestors(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)
	root := t.TempDir()
	runner := newAnonymousRunner(t, root)

	bare := filepath.Join(root, "cache.git")
	if err := runner.CloneSource(ctx, gitcli.SourceCloneOptions{
		Remote: up.url(), Directory: bare, Bare: true,
	}); err != nil {
		t.Fatalf("clone: %v", err)
	}

	inside := filepath.Join(bare, "not-a-cache")
	if err := os.MkdirAll(inside, 0o750); err != nil {
		t.Fatalf("create directory: %v", err)
	}

	if got, err := runner.IsBareRepositoryAt(ctx, bare); err != nil || !got {
		t.Fatalf("bare cache probe = %v (%v), want true", got, err)
	}
	if got, err := runner.IsBareRepositoryAt(ctx, inside); err != nil || got {
		t.Fatalf("directory inside a bare repository = %v (%v), want false", got, err)
	}
	if got, err := runner.IsBareRepositoryAt(ctx, up.repo.Dir); err != nil || got {
		t.Fatalf("work tree probe = %v (%v), want false", got, err)
	}
	if _, err := runner.IsBareRepositoryAt(ctx, "relative/path"); err == nil {
		t.Fatal("a relative path must be rejected")
	}
}

// TestConfigKeysReportsEveryRepositoryScope proves the audit surface sees the
// scope a --local listing cannot.
func TestConfigKeysReportsEveryRepositoryScope(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)
	root := t.TempDir()
	runner := newAnonymousRunner(t, root)

	dir := filepath.Join(root, "cache.git")
	if err := runner.CloneSource(ctx, gitcli.SourceCloneOptions{
		Remote: up.url(), Directory: dir, Bare: true,
	}); err != nil {
		t.Fatalf("clone: %v", err)
	}
	cache, err := runner.WithDir(dir)
	if err != nil {
		t.Fatalf("scope runner to the cache: %v", err)
	}
	if err := cache.SetConfigLocal(ctx, "extensions.worktreeConfig", "true"); err != nil {
		t.Fatalf("enable worktree configuration: %v", err)
	}
	poison(t, filepath.Join(dir, "config.worktree"), "[core]\n\tsshCommand = /bin/false\n")

	entries, err := cache.ConfigKeys(ctx)
	if err != nil {
		t.Fatalf("config keys: %v", err)
	}
	var scopes []string
	found := false
	for _, entry := range entries {
		scopes = append(scopes, entry.Scope+":"+entry.Key)
		if entry.Scope == "worktree" && strings.EqualFold(entry.Key, "core.sshCommand") {
			found = true
		}
		if entry.Scope != "local" && entry.Scope != "worktree" {
			t.Errorf("listing reports scope %q, which the repository does not own", entry.Scope)
		}
	}
	if !found {
		t.Fatalf("worktree scoped key is missing from %v", scopes)
	}

	// The effective read has to agree with the scope that actually wins.
	if value, ok, err := cache.ConfigEffective(ctx, "core.sshCommand"); err != nil || !ok || value != "/bin/false" {
		t.Fatalf("effective core.sshCommand = %q (found %v, err %v)", value, ok, err)
	}
}
