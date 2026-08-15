package gitcli_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/testsupport"
)

// objectRepo is an initialized repository with no commits, which is all the
// object plumbing needs: blobs, trees, and tag objects exist independently of
// any ref.
func objectRepo(ctx context.Context, t *testing.T) *testsupport.Repo {
	t.Helper()
	return testsupport.NewRepo(ctx, t, testsupport.Options{
		Branch:    mainBranch,
		UserName:  testUserName,
		UserEmail: testUserEmail,
	})
}

// writeBlob writes content and fails the test if it cannot.
func writeBlob(ctx context.Context, t *testing.T, repo *testsupport.Repo, content string) string {
	t.Helper()
	name, err := repo.Git.WriteBlob(ctx, []byte(content))
	if err != nil {
		t.Fatalf("write blob: %v", err)
	}
	return name
}

func TestWriteBlobPreservesBytes(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	repo := objectRepo(ctx, t)

	tests := []struct {
		name    string
		content []byte
	}{
		{name: "empty", content: []byte{}},
		{name: "nil is the empty blob", content: nil},
		{name: "text", content: []byte("package rbac\n")},
		{name: "null bytes", content: []byte{0x00, 0x01, 0x00, 0xff}},
		{name: "invalid utf8", content: []byte{0xc3, 0x28, 0xa0, 0xa1}},
		{name: "no trailing newline", content: []byte("no newline")},
		{name: "carriage returns", content: []byte("a\r\nb\r\n")},
		{name: "lone carriage return", content: []byte("a\rb")},
		{name: "leading dash", content: []byte("--not-an-option\n")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			name, err := repo.Git.WriteBlob(ctx, test.content)
			if err != nil {
				t.Fatalf("write blob: %v", err)
			}
			got, err := repo.Git.ReadBlob(ctx, gitcli.BlobOptions{Revision: name})
			if err != nil {
				t.Fatalf("read blob back: %v", err)
			}
			want := test.content
			if want == nil {
				want = []byte{}
			}
			if !bytes.Equal(got, want) {
				t.Errorf("round tripped %q, want %q", got, want)
			}
		})
	}
}

func TestWriteBlobIsContentAddressed(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	repo := objectRepo(ctx, t)

	// The same content written twice is one object, which is what lets the
	// builder skip work it has already done.
	first := writeBlob(ctx, t, repo, "identical\n")
	second := writeBlob(ctx, t, repo, "identical\n")
	if first != second {
		t.Errorf("same content produced %s and %s, want one object name", first, second)
	}
	if other := writeBlob(ctx, t, repo, "different\n"); other == first {
		t.Error("different content produced the same object name")
	}
}

// treeFixture is a set of entries covering the modes and path shapes a
// generated tree has to record.
func treeFixture(ctx context.Context, t *testing.T, repo *testsupport.Repo) []gitcli.TreeEntry {
	t.Helper()
	regular := writeBlob(ctx, t, repo, "package rbac\n")
	script := writeBlob(ctx, t, repo, "#!/bin/sh\n")
	target := writeBlob(ctx, t, repo, "../regular.go")
	return []gitcli.TreeEntry{
		{Mode: gitcli.ModeRegular, Object: regular, Path: "regular.go"},
		{Mode: gitcli.ModeExecutable, Object: script, Path: "hack/run.sh"},
		{Mode: gitcli.ModeSymlink, Object: target, Path: "vendor/link.go"},
		{Mode: gitcli.ModeRegular, Object: regular, Path: "a space/two words.go"},
		{Mode: gitcli.ModeRegular, Object: regular, Path: "quote\"d.go"},
		{Mode: gitcli.ModeRegular, Object: regular, Path: "tab\there.go"},
		{Mode: gitcli.ModeRegular, Object: regular, Path: "unicode/é日本.go"},
		{Mode: gitcli.ModeRegular, Object: regular, Path: "deep/nested/pkg/doc.go"},
	}
}

func TestWriteTreeRoundTripsEveryPathShape(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	repo := objectRepo(ctx, t)
	entries := treeFixture(ctx, t, repo)

	tree, err := repo.Git.WriteTree(ctx, entries)
	if err != nil {
		t.Fatalf("write tree: %v", err)
	}
	got, err := repo.Git.ListTree(ctx, tree)
	if err != nil {
		t.Fatalf("list tree: %v", err)
	}

	want := slices.Clone(entries)
	slices.SortFunc(want, func(a, b gitcli.TreeEntry) int { return strings.Compare(a.Path, b.Path) })
	if len(got) != len(want) {
		t.Fatalf("listed %d entries, want %d", len(got), len(want))
	}
	for i, entry := range want {
		if got[i] != entry {
			t.Errorf("entry %d is %+v, want %+v", i, got[i], entry)
		}
	}
}

func TestWriteTreeIgnoresInputOrder(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	repo := objectRepo(ctx, t)
	entries := treeFixture(ctx, t, repo)

	forward, err := repo.Git.WriteTree(ctx, entries)
	if err != nil {
		t.Fatalf("write tree: %v", err)
	}

	// The reversed input describes the same tree, so it has to produce the same
	// object. Determinism that depended on the caller's ordering would not be
	// determinism at all.
	reversed := slices.Clone(entries)
	slices.Reverse(reversed)
	backward, err := repo.Git.WriteTree(ctx, reversed)
	if err != nil {
		t.Fatalf("write reversed tree: %v", err)
	}
	if forward != backward {
		t.Errorf("reversed input produced %s, want %s", backward, forward)
	}

	// The caller's slice is an input, not scratch space.
	if reversed[0].Path == entries[0].Path {
		t.Error("WriteTree sorted the caller's slice in place")
	}
}

func TestWriteTreeLeavesTheRepositoryIndexAlone(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	repo := objectRepo(ctx, t)

	// A file staged before the build has to still be staged after it, and the
	// tree must not have picked it up.
	repo.WriteFile(t, "staged.txt", "staged\n")
	if err := repo.Git.AddPaths(ctx, "staged.txt"); err != nil {
		t.Fatalf("stage file: %v", err)
	}
	before, err := repo.Git.StatusPorcelainZ(ctx)
	if err != nil {
		t.Fatalf("read status: %v", err)
	}

	blob := writeBlob(ctx, t, repo, "generated\n")
	tree, err := repo.Git.WriteTree(ctx, []gitcli.TreeEntry{
		{Mode: gitcli.ModeRegular, Object: blob, Path: "generated.go"},
	})
	if err != nil {
		t.Fatalf("write tree: %v", err)
	}

	after, err := repo.Git.StatusPorcelainZ(ctx)
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	if before != after {
		t.Errorf("status changed from %q to %q", before, after)
	}
	listed, err := repo.Git.ListTree(ctx, tree)
	if err != nil {
		t.Fatalf("list tree: %v", err)
	}
	if len(listed) != 1 || listed[0].Path != "generated.go" {
		t.Errorf("tree holds %+v, want only generated.go", listed)
	}
}

func TestWriteTreeRejectsBadEntries(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	repo := objectRepo(ctx, t)
	blob := writeBlob(ctx, t, repo, "content\n")

	tests := []struct {
		name    string
		entries []gitcli.TreeEntry
		want    error
	}{
		{
			name:    "no entries",
			entries: nil,
		},
		{
			name: "duplicate path",
			entries: []gitcli.TreeEntry{
				{Mode: gitcli.ModeRegular, Object: blob, Path: "same.go"},
				{Mode: gitcli.ModeExecutable, Object: blob, Path: "same.go"},
			},
			want: gitcli.ErrDuplicateTreeEntry,
		},
		{
			name: "gitlink mode",
			entries: []gitcli.TreeEntry{
				{Mode: gitcli.FileMode("160000"), Object: blob, Path: "submodule"},
			},
			want: gitcli.ErrUnsupportedFileMode,
		},
		{
			name: "tree mode",
			entries: []gitcli.TreeEntry{
				{Mode: gitcli.FileMode("040000"), Object: blob, Path: "dir"},
			},
			want: gitcli.ErrUnsupportedFileMode,
		},
		{
			name: "empty mode",
			entries: []gitcli.TreeEntry{
				{Object: blob, Path: "file.go"},
			},
			want: gitcli.ErrUnsupportedFileMode,
		},
		{
			name: "abbreviated object name",
			entries: []gitcli.TreeEntry{
				{Mode: gitcli.ModeRegular, Object: blob[:8], Path: "file.go"},
			},
		},
		{
			name: "absolute path",
			entries: []gitcli.TreeEntry{
				{Mode: gitcli.ModeRegular, Object: blob, Path: "/etc/passwd"},
			},
		},
		{
			name: "parent traversal",
			entries: []gitcli.TreeEntry{
				{Mode: gitcli.ModeRegular, Object: blob, Path: "../escape.go"},
			},
		},
		{
			name: "path with a null byte",
			entries: []gitcli.TreeEntry{
				{Mode: gitcli.ModeRegular, Object: blob, Path: "a\x00b.go"},
			},
		},
		{
			name: "path with a newline",
			entries: []gitcli.TreeEntry{
				{Mode: gitcli.ModeRegular, Object: blob, Path: "a\nb.go"},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := repo.Git.WriteTree(ctx, test.entries)
			if err == nil {
				t.Fatal("expected an error")
			}
			if test.want != nil && !errors.Is(err, test.want) {
				t.Errorf("error %v, want %v", err, test.want)
			}
		})
	}
}

func TestWriteTreeRejectsAPathThatIsAlsoADirectory(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	repo := objectRepo(ctx, t)
	blob := writeBlob(ctx, t, repo, "content\n")

	// Git does not refuse this pair. update-index takes both and write-tree
	// returns a tree that silently holds one of them, so a module carrying such
	// a pair would publish with a file missing and nothing to indicate it.
	tests := []struct {
		name  string
		paths []string
	}{
		{name: "adjacent", paths: []string{"pkg", "pkg/doc.go"}},
		// Sorting does not put the clashing pair next to each other here,
		// because "!" precedes "/", so an adjacency check would miss it.
		{name: "separated by a sibling", paths: []string{"pkg", "pkg!x.go", "pkg/doc.go"}},
		{name: "deep", paths: []string{"a/b", "a/b/c/d.go"}},
		{name: "directory listed first", paths: []string{"a/b/c.go", "a/b"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			entries := make([]gitcli.TreeEntry, 0, len(test.paths))
			for _, path := range test.paths {
				entries = append(entries, gitcli.TreeEntry{Mode: gitcli.ModeRegular, Object: blob, Path: path})
			}
			_, err := repo.Git.WriteTree(ctx, entries)
			if !errors.Is(err, gitcli.ErrTreePathConflict) {
				t.Errorf("error %v, want %v", err, gitcli.ErrTreePathConflict)
			}
		})
	}
}

// TestWriteTreeRejectsANonBlobObject covers the clash git resolves by writing a
// tree nobody can read back. write-tree checks that an object exists but not
// that it is the type its mode claims, so a tree or a commit staged under a file
// mode produces a well formed tree whose entry inflates to another object's
// bytes, and ls-tree reports it as the blob it is not.
func TestWriteTreeRejectsANonBlobObject(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	repo := objectRepo(ctx, t)
	blob := writeBlob(ctx, t, repo, "content\n")
	tree, err := repo.Git.WriteTree(ctx, []gitcli.TreeEntry{
		{Mode: gitcli.ModeRegular, Object: blob, Path: "inner.go"},
	})
	if err != nil {
		t.Fatalf("write tree: %v", err)
	}
	commit := taggedCommit(ctx, t, repo)

	tests := []struct {
		name   string
		object string
		want   error
	}{
		{name: "tree under a file mode", object: tree, want: gitcli.ErrNotABlob},
		{name: "commit under a file mode", object: commit, want: gitcli.ErrNotABlob},
		{name: "absent object", object: strings.Repeat("a", len(blob)), want: gitcli.ErrObjectNotFound},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := repo.Git.WriteTree(ctx, []gitcli.TreeEntry{
				{Mode: gitcli.ModeRegular, Object: test.object, Path: "file.go"},
			})
			if !errors.Is(err, test.want) {
				t.Errorf("error %v, want %v", err, test.want)
			}
		})
	}
}

// TestWriteTreeRejectsGitOwnDirectory covers the third way git turns a bad tree
// into a good looking one. update-index prints "Ignoring path" for a component
// naming git's own directory, exits zero, and write-tree then reports a tree
// that is simply missing the file.
func TestWriteTreeRejectsGitOwnDirectory(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	repo := objectRepo(ctx, t)
	blob := writeBlob(ctx, t, repo, "content\n")

	refused := []string{
		".git/config",
		".GIT/config",
		".Git/HEAD",
		"pkg/.git/config",
		"pkg/.git/hooks/pre-commit",
		".git./config",
		".git /config",
		"git~1/config",
		"GIT~1/config",
		".git",
	}
	for _, path := range refused {
		t.Run("refuses "+path, func(t *testing.T) {
			t.Parallel()
			_, err := repo.Git.WriteTree(ctx, []gitcli.TreeEntry{
				{Mode: gitcli.ModeRegular, Object: blob, Path: "keep.go"},
				{Mode: gitcli.ModeRegular, Object: blob, Path: path},
			})
			if !errors.Is(err, gitcli.ErrReservedTreePath) {
				t.Errorf("error %v, want %v", err, gitcli.ErrReservedTreePath)
			}
		})
	}

	// The names that merely begin the same way are ordinary files a generated
	// module really does carry, and git records every one of them.
	t.Run("records the neighbours", func(t *testing.T) {
		t.Parallel()
		kept := []string{".gitignore", ".gitattributes", ".gitmodules", ".github/workflows/ci.yaml", "git~10/doc.go", "pkg/gitutil/git.go"}
		entries := make([]gitcli.TreeEntry, 0, len(kept))
		for _, path := range kept {
			entries = append(entries, gitcli.TreeEntry{Mode: gitcli.ModeRegular, Object: blob, Path: path})
		}
		tree, err := repo.Git.WriteTree(ctx, entries)
		if err != nil {
			t.Fatalf("write tree: %v", err)
		}
		listed, err := repo.Git.ListTree(ctx, tree)
		if err != nil {
			t.Fatalf("list tree: %v", err)
		}
		if len(listed) != len(kept) {
			t.Fatalf("tree holds %d entries, want %d", len(listed), len(kept))
		}
	})
}

// TestWriteTreeFailsOnADeclinedEntry pins the net under the named rules.
//
// With core.protectHFS set, git also refuses spellings of ".git" that hide a
// zero width character inside the name, which this package does not model and
// deliberately does not try to. The call still has to fail, because git's way
// of refusing is to exit zero having dropped the entry.
func TestWriteTreeFailsOnADeclinedEntry(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	repo := objectRepo(ctx, t)
	blob := writeBlob(ctx, t, repo, "content\n")
	repo.SetConfig(ctx, t, "core.protectHFS", "true")

	// A zero width non joiner sits between the g and the i. HFS+ ignores it, so
	// the name opens the real .git directory there.
	sneaky := ".g" + string(rune(0x200c)) + "it/config"
	_, err := repo.Git.WriteTree(ctx, []gitcli.TreeEntry{
		{Mode: gitcli.ModeRegular, Object: blob, Path: "keep.go"},
		{Mode: gitcli.ModeRegular, Object: blob, Path: sneaky},
	})
	if !errors.Is(err, gitcli.ErrTreeEntryDropped) {
		t.Errorf("error %v, want %v", err, gitcli.ErrTreeEntryDropped)
	}
}

func TestWriteTreeCleansUpItsIndexOnCancellation(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	repo := objectRepo(ctx, t)
	blob := writeBlob(ctx, t, repo, "content\n")

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err := repo.Git.WriteTree(cancelled, []gitcli.TreeEntry{
		{Mode: gitcli.ModeRegular, Object: blob, Path: "file.go"},
	})
	if err == nil {
		t.Fatal("expected a cancelled build to fail")
	}

	// The temporary index lives under the test's own temporary directory tree
	// only by convention, so the durable assertion is that the repository is
	// still usable and holds no index of its own.
	if _, err := repo.Git.WriteTree(ctx, []gitcli.TreeEntry{
		{Mode: gitcli.ModeRegular, Object: blob, Path: "file.go"},
	}); err != nil {
		t.Fatalf("write tree after cancellation: %v", err)
	}
}

func TestObjectFormatMatchesTheRepository(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	tests := []struct {
		name   string
		format gitcli.ObjectFormat
		length int
	}{
		{name: "sha1", format: gitcli.ObjectFormatSHA1, length: 40},
		{name: "sha256", format: gitcli.ObjectFormatSHA256, length: 64},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			// HOME travels as an isolation entry rather than through t.Setenv,
			// which would forbid running these in parallel, and rather than
			// through Env, which would seed the redactor with the path.
			runner, err := gitcli.New(ctx, gitcli.Options{
				Dir:       t.TempDir(),
				Inherit:   []string{"PATH"},
				Isolation: []string{"HOME=" + t.TempDir()},
			})
			if err != nil {
				t.Fatalf("create git runner: %v", err)
			}
			if err := runner.InitRepositoryWithFormat(ctx, mainBranch, test.format); err != nil {
				t.Fatalf("init %s repository: %v", test.format, err)
			}
			got, err := runner.ObjectFormat(ctx)
			if err != nil {
				t.Fatalf("read object format: %v", err)
			}
			if got != test.format {
				t.Errorf("format %q, want %q", got, test.format)
			}
			if got.HexLength() != test.length {
				t.Errorf("hex length %d, want %d", got.HexLength(), test.length)
			}

			// The whole write path has to work under both algorithms, and the
			// object names it produces have to be the length the format promises.
			blob, err := runner.WriteBlob(ctx, []byte("package rbac\n"))
			if err != nil {
				t.Fatalf("write blob: %v", err)
			}
			tree, err := runner.WriteTree(ctx, []gitcli.TreeEntry{
				{Mode: gitcli.ModeRegular, Object: blob, Path: "doc.go"},
			})
			if err != nil {
				t.Fatalf("write tree: %v", err)
			}
			for name, object := range map[string]string{"blob": blob, "tree": tree} {
				if len(object) != test.length {
					t.Errorf("%s name %q is %d characters, want %d", name, object, len(object), test.length)
				}
			}
			empty, err := runner.EmptyTree(ctx)
			if err != nil {
				t.Fatalf("read empty tree: %v", err)
			}
			if len(empty) != test.length {
				t.Errorf("empty tree %q is %d characters, want %d", empty, len(empty), test.length)
			}
		})
	}
}

// taggedCommit builds one commit to hang tag objects off.
func taggedCommit(ctx context.Context, t *testing.T, repo *testsupport.Repo) string {
	t.Helper()
	blob := writeBlob(ctx, t, repo, "package rbac\n")
	tree, err := repo.Git.WriteTree(ctx, []gitcli.TreeEntry{
		{Mode: gitcli.ModeRegular, Object: blob, Path: "doc.go"},
	})
	if err != nil {
		t.Fatalf("write tree: %v", err)
	}
	signature := gitcli.Signature{Name: testUserName, Email: testUserEmail, Date: testRawDate}
	commit, err := repo.Git.WriteCommit(ctx, gitcli.CommitTreeOptions{
		Tree:      tree,
		Message:   "feat: add rbac\n",
		Author:    signature,
		Committer: signature,
	})
	if err != nil {
		t.Fatalf("write commit: %v", err)
	}
	return commit
}

func TestWriteTagObjectCreatesNoRef(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	repo := objectRepo(ctx, t)
	commit := taggedCommit(ctx, t, repo)

	before, err := repo.Git.ListRefs(ctx)
	if err != nil {
		t.Fatalf("list refs: %v", err)
	}

	object, err := repo.Git.WriteTagObject(ctx, gitcli.TagObjectOptions{
		Object:  commit,
		Type:    "commit",
		Name:    "v0.36.1",
		Message: "Kubernetes v0.36.1\n",
		Tagger:  gitcli.Signature{Name: testUserName, Email: testUserEmail, Date: "1700000000 +0530"},
	})
	if err != nil {
		t.Fatalf("write tag object: %v", err)
	}

	// Learning a release tag's object name must not be the same act as
	// publishing it, so a dry run leaves the ref namespace untouched.
	after, err := repo.Git.ListRefs(ctx)
	if err != nil {
		t.Fatalf("list refs: %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("refs went from %d to %d, want no change", len(before), len(after))
	}

	// The same tag written twice is the same object, which is what makes a
	// regenerated release comparable to a published one.
	again, err := repo.Git.WriteTagObject(ctx, gitcli.TagObjectOptions{
		Object:  commit,
		Type:    "commit",
		Name:    "v0.36.1",
		Message: "Kubernetes v0.36.1\n",
		Tagger:  gitcli.Signature{Name: testUserName, Email: testUserEmail, Date: "1700000000 +0530"},
	})
	if err != nil {
		t.Fatalf("rewrite tag object: %v", err)
	}
	if again != object {
		t.Errorf("rewrote as %s, want %s", again, object)
	}
}

func TestWriteTagObjectRejectsBadOptions(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	repo := objectRepo(ctx, t)
	commit := taggedCommit(ctx, t, repo)

	valid := gitcli.TagObjectOptions{
		Object:  commit,
		Type:    "commit",
		Name:    "v0.36.1",
		Message: "release\n",
		Tagger:  gitcli.Signature{Name: testUserName, Email: testUserEmail, Date: "1700000000 +0530"},
	}
	tests := []struct {
		name   string
		mutate func(*gitcli.TagObjectOptions)
	}{
		{name: "abbreviated object", mutate: func(o *gitcli.TagObjectOptions) { o.Object = commit[:8] }},
		{name: "unknown type", mutate: func(o *gitcli.TagObjectOptions) { o.Type = "widget" }},
		{name: "wrong type for the object", mutate: func(o *gitcli.TagObjectOptions) { o.Type = "blob" }},
		{name: "qualified ref name", mutate: func(o *gitcli.TagObjectOptions) { o.Name = "refs/tags/v1" }},
		{name: "empty message", mutate: func(o *gitcli.TagObjectOptions) { o.Message = "" }},
		{name: "no tagger name", mutate: func(o *gitcli.TagObjectOptions) { o.Tagger.Name = "" }},
		{name: "no tagger email", mutate: func(o *gitcli.TagObjectOptions) { o.Tagger.Email = "" }},
		{
			name:   "tagger name with a bracket",
			mutate: func(o *gitcli.TagObjectOptions) { o.Tagger.Name = "A <sneaky@example.com> B" },
		},
		{
			name:   "tagger name with a line break",
			mutate: func(o *gitcli.TagObjectOptions) { o.Tagger.Name = "A\ntagger B <b@example.com> 1 +0000" },
		},
		{name: "iso date", mutate: func(o *gitcli.TagObjectOptions) { o.Tagger.Date = "2026-01-02T03:04:05Z" }},
		{name: "no zone offset", mutate: func(o *gitcli.TagObjectOptions) { o.Tagger.Date = "1700000000" }},
		{name: "short zone offset", mutate: func(o *gitcli.TagObjectOptions) { o.Tagger.Date = "1700000000 +53" }},
		{name: "unsigned zone offset", mutate: func(o *gitcli.TagObjectOptions) { o.Tagger.Date = "1700000000 0530" }},
		{name: "empty date", mutate: func(o *gitcli.TagObjectOptions) { o.Tagger.Date = "" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			opts := valid
			test.mutate(&opts)
			if _, err := repo.Git.WriteTagObject(ctx, opts); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

// tagMessages are the message shapes git's default cleanup would rewrite. Each
// one is a real upstream release note shape, and each has to survive verbatim.
var tagMessages = []struct {
	name    string
	message string
}{
	{name: "plain", message: "Kubernetes v0.36.1\n"},
	{name: "comment line", message: "Release\n# not a comment, part of the note\nDone\n"},
	{name: "leading comment", message: "# heading\nbody\n"},
	{name: "trailing whitespace", message: "Release   \nnotes\t\n"},
	{name: "trailing blank lines", message: "Release\n\n\n"},
	{name: "no trailing newline", message: "Release"},
	{name: "carriage returns", message: "Release\r\nnotes\r\n"},
	{name: "unicode", message: "Versión 0.36.1 — é日本\n"},
	{name: "leading dash", message: "--force is not an option here\n"},
}

func TestTagInfoReadsAnnotatedTagsVerbatim(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	for _, test := range tagMessages {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			repo := objectRepo(ctx, t)
			commit := taggedCommit(ctx, t, repo)
			tagger := gitcli.Signature{Name: testUserName, Email: testUserEmail, Date: "1700000000 +0530"}

			if err := repo.Git.CreateTag(ctx, gitcli.TagOptions{
				Name:    "v0.36.1",
				Commit:  commit,
				Message: test.message,
				Tagger:  tagger,
			}); err != nil {
				t.Fatalf("create tag: %v", err)
			}
			info, err := repo.Git.TagInfo(ctx, "v0.36.1")
			if err != nil {
				t.Fatalf("read tag: %v", err)
			}
			if !info.Annotated {
				t.Error("tag is not annotated")
			}
			if info.Message != test.message {
				t.Errorf("message %q, want %q", info.Message, test.message)
			}
			if info.Target != commit {
				t.Errorf("target %s, want %s", info.Target, commit)
			}
			if info.Object == commit {
				t.Error("annotated tag reports the commit as its own object")
			}
			if info.Tagger != tagger {
				t.Errorf("tagger %+v, want %+v", info.Tagger, tagger)
			}
		})
	}
}

func TestCreateTagAndWriteTagObjectAgree(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	// The two ways of producing a tag have to produce the same bytes. If they
	// did not, a run could not compare a tag it built against one a previous
	// release published.
	for _, test := range tagMessages {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			repo := objectRepo(ctx, t)
			commit := taggedCommit(ctx, t, repo)
			tagger := gitcli.Signature{Name: testUserName, Email: testUserEmail, Date: "1700000000 +0530"}

			written, err := repo.Git.WriteTagObject(ctx, gitcli.TagObjectOptions{
				Object:  commit,
				Type:    "commit",
				Name:    "v0.36.1",
				Message: test.message,
				Tagger:  tagger,
			})
			if err != nil {
				t.Fatalf("write tag object: %v", err)
			}
			if err := repo.Git.CreateTag(ctx, gitcli.TagOptions{
				Name:    "v0.36.1",
				Commit:  commit,
				Message: test.message,
				Tagger:  tagger,
			}); err != nil {
				t.Fatalf("create tag: %v", err)
			}
			info, err := repo.Git.TagInfo(ctx, "v0.36.1")
			if err != nil {
				t.Fatalf("read tag: %v", err)
			}
			if info.Object != written {
				t.Errorf("CreateTag produced %s, WriteTagObject produced %s", info.Object, written)
			}
		})
	}
}

func TestTagInfoReadsLightweightTags(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	repo := objectRepo(ctx, t)
	commit := taggedCommit(ctx, t, repo)

	if err := repo.Git.CreateTag(ctx, gitcli.TagOptions{Name: "v0.36.0", Commit: commit}); err != nil {
		t.Fatalf("create tag: %v", err)
	}
	info, err := repo.Git.TagInfo(ctx, "v0.36.0")
	if err != nil {
		t.Fatalf("read tag: %v", err)
	}
	switch {
	case info.Annotated:
		t.Error("lightweight tag reported as annotated")
	case info.Object != commit:
		t.Errorf("object %s, want %s", info.Object, commit)
	case info.Target != commit:
		t.Errorf("target %s, want %s", info.Target, commit)
	case info.Message != "":
		// A lightweight tag has nowhere to record a message, so reporting the
		// commit's would invent one the tag does not carry.
		t.Errorf("message %q, want none", info.Message)
	case info.Tagger != (gitcli.Signature{}):
		t.Errorf("tagger %+v, want none", info.Tagger)
	}
}

func TestTagInfoReportsAMissingTag(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	repo := objectRepo(ctx, t)

	_, err := repo.Git.TagInfo(ctx, "v9.99.99")
	if !errors.Is(err, gitcli.ErrTagNotFound) {
		t.Errorf("error %v, want %v", err, gitcli.ErrTagNotFound)
	}
}

func TestTagInfoPreservesRawTaggerDates(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	// A tag object records the zone the tagger was in, and a regenerated tag has
	// to record the same one. Every timestamp here is past the lower bound
	// git's date parser imposes, which
	// TestWriteTagObjectOutlivesTheDateParser covers on its own.
	dates := []string{
		"1700000000 +0000",
		"1700000000 +0530",
		"1700000000 -0800",
		"1700000000 +1400",
		"100000000 +0000",
	}
	for _, date := range dates {
		t.Run(date, func(t *testing.T) {
			t.Parallel()
			repo := objectRepo(ctx, t)
			commit := taggedCommit(ctx, t, repo)
			tagger := gitcli.Signature{Name: testUserName, Email: testUserEmail, Date: date}

			if err := repo.Git.CreateTag(ctx, gitcli.TagOptions{
				Name:    "v0.36.1",
				Commit:  commit,
				Message: "release\n",
				Tagger:  tagger,
			}); err != nil {
				t.Fatalf("create tag: %v", err)
			}
			info, err := repo.Git.TagInfo(ctx, "v0.36.1")
			if err != nil {
				t.Fatalf("read tag: %v", err)
			}
			if info.Tagger.Date != date {
				t.Errorf("date %q, want %q", info.Tagger.Date, date)
			}
		})
	}
}

func TestWriteTagObjectOutlivesTheDateParser(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	// The two tag paths are not equally faithful at the bottom of the range.
	// CreateTag hands the date to git's parser, which only reads a bare number
	// as a timestamp once it is large enough not to look like a date component,
	// and refuses everything below that. WriteTagObject assembles the object's
	// bytes, so it can reproduce an upstream tag carrying any timestamp. An
	// early tag is therefore replayable only through the object.
	tests := []struct {
		name      string
		date      string
		parseable bool
	}{
		{name: "epoch", date: "0 +0000"},
		{name: "one second in", date: "1 +0000"},
		{name: "just below the bound", date: "99999999 +0000"},
		{name: "the lowest parseable timestamp", date: "100000000 +0000", parseable: true},
		{name: "present day", date: "1700000000 +0530", parseable: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			repo := objectRepo(ctx, t)
			commit := taggedCommit(ctx, t, repo)
			tagger := gitcli.Signature{Name: testUserName, Email: testUserEmail, Date: test.date}

			// The object path has to work for every one of these.
			object, err := repo.Git.WriteTagObject(ctx, gitcli.TagObjectOptions{
				Object:  commit,
				Type:    "commit",
				Name:    "v0.0.1",
				Message: "release\n",
				Tagger:  tagger,
			})
			if err != nil {
				t.Fatalf("write tag object dated %q: %v", test.date, err)
			}
			if !isObjectNameForTest(object) {
				t.Fatalf("object name %q is not an object name", object)
			}

			err = repo.Git.CreateTag(ctx, gitcli.TagOptions{
				Name:    "v0.0.1",
				Commit:  commit,
				Message: "release\n",
				Tagger:  tagger,
			})
			if test.parseable != (err == nil) {
				t.Fatalf("CreateTag dated %q returned %v, want parseable=%v", test.date, err, test.parseable)
			}
			if !test.parseable {
				return
			}
			// Where both paths work they have to agree, or a replayed release
			// would depend on which one produced it.
			info, err := repo.Git.TagInfo(ctx, "v0.0.1")
			if err != nil {
				t.Fatalf("read tag: %v", err)
			}
			if info.Object != object {
				t.Errorf("CreateTag produced %s, WriteTagObject produced %s", info.Object, object)
			}
		})
	}
}

// isObjectNameForTest mirrors the package's own check, so a test can assert an
// object name without reaching into unexported code.
func isObjectNameForTest(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	return strings.Trim(value, "0123456789abcdef") == ""
}

func TestListTreeRejectsNonTrees(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	repo := objectRepo(ctx, t)
	blob := writeBlob(ctx, t, repo, "package rbac\n")

	if _, err := repo.Git.ListTree(ctx, blob); err == nil {
		t.Error("expected listing a blob as a tree to fail")
	}
}

// TestWriteCommitRequiresExactObjectNames pins the inputs that decide what a
// replayed commit records.
//
// A revision git would resolve makes the written commit depend on the state of
// the repository at the moment it ran rather than on what the caller described,
// and an identity git completes from the environment or the clock attributes the
// commit to whoever ran the engine, at whatever time they ran it. Every one of
// those is silent and every one of them changes the object name, so this is the
// plumbing writer's contract rather than the friendlier one Commit offers.
func TestWriteCommitRequiresExactObjectNames(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	repo := objectRepo(ctx, t)
	commit := taggedCommit(ctx, t, repo)
	tree, err := repo.Git.ResolveTree(ctx, commit)
	if err != nil {
		t.Fatalf("read the commit tree: %v", err)
	}
	if err := repo.Git.CreateRef(ctx, "refs/heads/topic", commit); err != nil {
		t.Fatalf("create ref: %v", err)
	}
	whole := gitcli.Signature{Name: testUserName, Email: testUserEmail, Date: testRawDate}

	tests := []struct {
		name string
		opts gitcli.CommitTreeOptions
	}{
		{name: "tree named by a branch", opts: gitcli.CommitTreeOptions{Tree: "refs/heads/topic", Author: whole, Committer: whole}},
		{name: "tree named by HEAD", opts: gitcli.CommitTreeOptions{Tree: "HEAD^{tree}", Author: whole, Committer: whole}},
		{name: "abbreviated tree", opts: gitcli.CommitTreeOptions{Tree: tree[:8], Author: whole, Committer: whole}},
		{name: "empty tree name", opts: gitcli.CommitTreeOptions{Author: whole, Committer: whole}},
		{name: "parent named by a branch", opts: gitcli.CommitTreeOptions{Tree: tree, Parents: []string{"refs/heads/topic"}, Author: whole, Committer: whole}},
		{name: "abbreviated parent", opts: gitcli.CommitTreeOptions{Tree: tree, Parents: []string{commit[:8]}, Author: whole, Committer: whole}},
		// Each identity case carries whatever the case is not about, so a
		// failure names one cause rather than several at once.
		{name: "author without a name", opts: gitcli.CommitTreeOptions{Tree: tree, Author: gitcli.Signature{Email: testUserEmail, Date: testRawDate}, Committer: whole}},
		{name: "author without an address", opts: gitcli.CommitTreeOptions{Tree: tree, Author: gitcli.Signature{Name: testUserName, Date: testRawDate}, Committer: whole}},
		{name: "committer without a name", opts: gitcli.CommitTreeOptions{Tree: tree, Author: whole, Committer: gitcli.Signature{Email: testUserEmail, Date: testRawDate}}},
		{name: "committer without an address", opts: gitcli.CommitTreeOptions{Tree: tree, Author: whole, Committer: gitcli.Signature{Name: testUserName, Date: testRawDate}}},
		// An absent date makes commit-tree record the wall clock, and a friendly
		// one asks git to interpret it, so the same fields would produce a
		// different object name depending on when and where the run happened.
		{name: "author without a date", opts: gitcli.CommitTreeOptions{Tree: tree, Author: gitcli.Signature{Name: testUserName, Email: testUserEmail}, Committer: whole}},
		{name: "committer without a date", opts: gitcli.CommitTreeOptions{Tree: tree, Author: whole, Committer: gitcli.Signature{Name: testUserName, Email: testUserEmail}}},
		{
			name: "author date in RFC 3339",
			opts: gitcli.CommitTreeOptions{Tree: tree, Author: gitcli.Signature{Name: testUserName, Email: testUserEmail, Date: "2026-01-02T03:04:05+05:30"}, Committer: whole},
		},
		{
			name: "committer date git would have to interpret",
			opts: gitcli.CommitTreeOptions{Tree: tree, Author: whole, Committer: gitcli.Signature{Name: testUserName, Email: testUserEmail, Date: "yesterday"}},
		},
		{
			name: "author date without a zone offset",
			opts: gitcli.CommitTreeOptions{Tree: tree, Author: gitcli.Signature{Name: testUserName, Email: testUserEmail, Date: "1700000000"}, Committer: whole},
		},
		{
			name: "author name carrying an angle bracket",
			opts: gitcli.CommitTreeOptions{Tree: tree, Author: gitcli.Signature{Name: "A <evil@example.com>", Email: testUserEmail}, Committer: whole},
		},
		{
			name: "committer address carrying a line break",
			opts: gitcli.CommitTreeOptions{Tree: tree, Author: whole, Committer: gitcli.Signature{Name: testUserName, Email: "a@example.com\nx"}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			opts := test.opts
			opts.Message = "feat: replayed\n"
			if _, err := repo.Git.WriteCommit(ctx, opts); err == nil {
				t.Error("expected an error")
			}
		})
	}

	t.Run("a complete request is written", func(t *testing.T) {
		t.Parallel()
		written, err := repo.Git.WriteCommit(ctx, gitcli.CommitTreeOptions{
			Tree:      tree,
			Parents:   []string{commit},
			Message:   "feat: replayed\n",
			Author:    whole,
			Committer: whole,
		})
		if err != nil {
			t.Fatalf("write commit: %v", err)
		}
		if !isObjectNameForTest(written) {
			t.Errorf("wrote %q, want an object name", written)
		}
	})
}

// TestTagInfoDoesNotMatchASlashDescendant covers a for-each-ref pattern rule
// that reads like an exact match and is not one.
//
// A pattern without a glob matches a ref completely or from the beginning up to
// a slash, so "refs/tags/v1" also names "refs/tags/v1/beta". A publisher asks
// TagInfo whether a release already exists, and an answer describing a different
// tag would tell it to leave the release uncreated.
func TestTagInfoDoesNotMatchASlashDescendant(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	repo := objectRepo(ctx, t)
	commit := taggedCommit(ctx, t, repo)

	if err := repo.Git.CreateTag(ctx, gitcli.TagOptions{Name: "v1/beta", Commit: commit}); err != nil {
		t.Fatalf("create the descendant tag: %v", err)
	}
	if _, err := repo.Git.TagInfo(ctx, "v1"); !errors.Is(err, gitcli.ErrTagNotFound) {
		t.Errorf("error %v, want %v", err, gitcli.ErrTagNotFound)
	}
	// The descendant itself is still readable under its own whole name.
	info, err := repo.Git.TagInfo(ctx, "v1/beta")
	if err != nil {
		t.Fatalf("read the descendant tag: %v", err)
	}
	if info.Target != commit {
		t.Errorf("target %s, want %s", info.Target, commit)
	}

	// Once the exact ref exists it is the one reported. It can never also have a
	// slash descendant: git's ref store refuses to hold refs/tags/v2 and
	// refs/tags/v2/rc1 at once, which is exactly why the dangerous case is the
	// one above, where only the descendant exists and the name asked about does
	// not.
	if err := repo.Git.CreateTag(ctx, gitcli.TagOptions{Name: "v2", Commit: commit}); err != nil {
		t.Fatalf("create tag: %v", err)
	}
	info, err = repo.Git.TagInfo(ctx, "v2")
	if err != nil {
		t.Fatalf("read tag: %v", err)
	}
	if info.Name != "v2" || info.Target != commit {
		t.Errorf("tag info = %+v, want the tag named v2 at %s", info, commit)
	}
}

// TestListTreeStopsAtTheOutputLimit pins that reading upstream content unredacted
// is not the same as reading it unbounded. The bytes a repository holds must not
// decide how much memory this process allocates.
func TestListTreeStopsAtTheOutputLimit(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	repo := objectRepo(ctx, t)
	blob := writeBlob(ctx, t, repo, "content\n")

	entries := make([]gitcli.TreeEntry, 0, 64)
	for i := range 64 {
		entries = append(entries, gitcli.TreeEntry{
			Mode:   gitcli.ModeRegular,
			Object: blob,
			Path:   fmt.Sprintf("pkg/file%02d.go", i),
		})
	}
	tree, err := repo.Git.WriteTree(ctx, entries)
	if err != nil {
		t.Fatalf("write tree: %v", err)
	}

	limited, err := gitcli.New(ctx, gitcli.Options{Dir: repo.Dir, OutputLimit: 128})
	if err != nil {
		t.Fatalf("create a limited runner: %v", err)
	}
	if _, err := limited.ListTree(ctx, tree); err == nil {
		t.Fatal("expected the listing to be refused for passing the limit")
	} else if !strings.Contains(err.Error(), "limit") {
		t.Errorf("error %v does not mention the limit", err)
	}

	// The same listing succeeds when the limit accommodates it, so the refusal
	// above is the limit and not the tree.
	listed, err := repo.Git.ListTree(ctx, tree)
	if err != nil {
		t.Fatalf("list tree: %v", err)
	}
	if len(listed) != len(entries) {
		t.Errorf("listed %d entries, want %d", len(listed), len(entries))
	}
}

// TestObjectInfoBatchReportsAmbiguity covers the answer that must not be read as
// "absent". A caller deciding what it still has to write would otherwise write a
// second object under a name that already means two things.
func TestObjectInfoBatchReportsAmbiguity(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	repo := objectRepo(ctx, t)

	// Two objects sharing a four character prefix. Git refuses to guess between
	// them, which is the condition under test; the search is bounded so a run
	// that cannot find a pair reports that rather than hanging.
	seen := make(map[string]string, 4096)
	var prefix string
	for i := range 4096 {
		name := writeBlob(ctx, t, repo, fmt.Sprintf("ambiguity fixture %d\n", i))
		if other, clash := seen[name[:4]]; clash && other != name {
			prefix = name[:4]
			break
		}
		seen[name[:4]] = name
	}
	if prefix == "" {
		t.Skip("no four character prefix collision was produced")
	}

	_, err := repo.Git.ObjectInfoBatch(ctx, gitcli.ObjectInfoOptions{Revisions: []string{prefix}})
	if !errors.Is(err, gitcli.ErrObjectAmbiguous) {
		t.Errorf("error %v, want %v", err, gitcli.ErrObjectAmbiguous)
	}
}
