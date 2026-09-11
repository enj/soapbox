package gomodmap_test

import (
	"context"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/gomodmap"
	"github.com/enj/soapbox/tools/internal/testsupport"
)

const (
	fixtureUserName  = "Soapbox Test"
	fixtureUserEmail = "test@example.com"
)

// sourceFixture is a Kubernetes shaped source history.
//
//	s0 ── s1 ── s2 ── s3 ── s4   (main, first parent)
//	       └── side ────┘
//
// s3 is a merge whose second parent is side, so side is reachable from the tip
// but is not on the mainline. That is the shape that decides whether a mapping
// follows first parents or wanders onto work that was never published.
type sourceFixture struct {
	repo    *testsupport.Repo
	commits map[string]string
}

func TestStagingRepository(t *testing.T) {
	tests := []struct {
		module string
		want   string
	}{
		{module: "k8s.io/api", want: "https://github.com/kubernetes/api.git"},
		{module: "k8s.io/component-helpers", want: "https://github.com/kubernetes/component-helpers.git"},
		{module: ""},
		{module: "k8s.io"},
		{module: "example.com/api"},
		{module: "k8s.io/"},
		{module: "k8s.io/api/v2"},
	}
	for _, test := range tests {
		t.Run(test.module, func(t *testing.T) {
			got, err := gomodmap.StagingRepository(test.module)
			if test.want == "" {
				if err == nil {
					t.Fatalf("StagingRepository(%q) = %q, want an error", test.module, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("StagingRepository(%q): %v", test.module, err)
			}
			if got != test.want {
				t.Errorf("StagingRepository(%q) = %q, want %q", test.module, got, test.want)
			}
		})
	}
}

func writeStagingCommit(ctx context.Context, t *testing.T, repo *testsupport.Repo, parents []string, message string) string {
	t.Helper()
	var tree string
	var err error
	if len(parents) == 0 {
		blob, blobErr := repo.Git.WriteBlob(ctx, []byte(message))
		if blobErr != nil {
			t.Fatalf("write topology blob: %v", blobErr)
		}
		tree, err = repo.Git.WriteTree(ctx, []gitcli.TreeEntry{{
			Mode: gitcli.ModeRegular, Object: blob, Path: "topology.txt",
		}})
	} else {
		tree, err = repo.Git.ResolveTree(ctx, parents[0])
	}
	if err != nil {
		t.Fatalf("resolve topology tree: %v", err)
	}
	signature := gitcli.Signature{Name: fixtureUserName, Email: fixtureUserEmail, Date: fixtureDate}
	commit, err := repo.Git.WriteCommit(ctx, gitcli.CommitTreeOptions{
		Tree: tree, Parents: parents, Message: message,
		Author: signature, Committer: signature,
	})
	if err != nil {
		t.Fatalf("write topology commit: %v", err)
	}
	return commit
}

func writeStagingFileCommit(ctx context.Context, t *testing.T, repo *testsupport.Repo, parent, path, contents, message string) string {
	t.Helper()
	entries, err := repo.Git.ListTree(ctx, parent)
	if err != nil {
		t.Fatalf("read topology tree: %v", err)
	}
	blob, err := repo.Git.WriteBlob(ctx, []byte(contents))
	if err != nil {
		t.Fatalf("write topology file: %v", err)
	}
	replaced := false
	for i := range entries {
		if entries[i].Path == path {
			entries[i] = gitcli.TreeEntry{Mode: gitcli.ModeRegular, Object: blob, Path: path}
			replaced = true
			break
		}
	}
	if !replaced {
		entries = append(entries, gitcli.TreeEntry{Mode: gitcli.ModeRegular, Object: blob, Path: path})
	}
	tree, err := repo.Git.WriteTree(ctx, entries)
	if err != nil {
		t.Fatalf("write topology tree: %v", err)
	}
	signature := gitcli.Signature{Name: fixtureUserName, Email: fixtureUserEmail, Date: fixtureDate}
	commit, err := repo.Git.WriteCommit(ctx, gitcli.CommitTreeOptions{
		Tree: tree, Parents: []string{parent}, Message: message,
		Author: signature, Committer: signature,
	})
	if err != nil {
		t.Fatalf("write topology file commit: %v", err)
	}
	return commit
}

func newStagingTopology(ctx context.Context, t *testing.T) (*testsupport.Repo, string) {
	t.Helper()
	repo := testsupport.NewRepo(ctx, t, testsupport.Options{
		Branch: "master", UserName: fixtureUserName, UserEmail: fixtureUserEmail,
	})
	base := repo.WriteAndCommit(ctx, t, "base.go", "package staging\n", claim("publish base", strings.Repeat("a", 40)))
	return repo, base
}

func TestStagingReleaseAnchor(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	repo, base := newStagingTopology(ctx, t)
	linear := writeStagingCommit(ctx, t, repo, []string{base}, claim("publish linear", strings.Repeat("b", 40)))
	spur := writeStagingCommit(ctx, t, repo, []string{base}, "update dependencies for previous tag\n")
	current := writeStagingCommit(ctx, t, repo, []string{base}, claim("publish current", strings.Repeat("c", 40)))

	tests := []struct {
		name     string
		previous string
		current  string
		want     string
	}{
		{name: "same target", previous: base, current: base, want: base},
		{name: "linear targets", previous: base, current: linear, want: base},
		{name: "one unclaimed previous tag spur", previous: spur, current: current, want: base},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := gomodmap.StagingReleaseAnchor(ctx, repo.Git, gomodmap.StagingReleaseAnchorOptions{
				ModulePath: "k8s.io/api", Previous: test.previous, Current: test.current,
			})
			if err != nil {
				t.Fatalf("derive staging release anchor: %v", err)
			}
			if got != test.want {
				t.Errorf("anchor = %s, want %s", got, test.want)
			}
		})
	}
}

func TestStagingReleaseAnchorRejectsUnknownDivergence(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	tests := []struct {
		name    string
		arrange func(*testing.T, *testsupport.Repo, string) (string, string)
		want    string
	}{
		{
			name: "two commit spur",
			arrange: func(t *testing.T, repo *testsupport.Repo, base string) (string, string) {
				first := writeStagingCommit(ctx, t, repo, []string{base}, "first old spur\n")
				previous := writeStagingCommit(ctx, t, repo, []string{first}, "second old spur\n")
				current := writeStagingCommit(ctx, t, repo, []string{base}, claim("current", strings.Repeat("b", 40)))
				return previous, current
			},
			want: "must be one unmerged commit",
		},
		{
			name: "claimed spur",
			arrange: func(t *testing.T, repo *testsupport.Repo, base string) (string, string) {
				previous := writeStagingCommit(ctx, t, repo, []string{base}, claim("claimed old spur", strings.Repeat("b", 40)))
				current := writeStagingCommit(ctx, t, repo, []string{base}, claim("current", strings.Repeat("c", 40)))
				return previous, current
			},
			want: "carries 1 Kubernetes-commit trailers",
		},
		{
			name: "duplicate claims on spur",
			arrange: func(t *testing.T, repo *testsupport.Repo, base string) (string, string) {
				message := "claimed old spur\n\n" +
					gomodmap.KubernetesCommitTrailer + ": " + strings.Repeat("b", 40) + "\n" +
					gomodmap.KubernetesCommitTrailer + ": " + strings.Repeat("c", 40) + "\n"
				previous := writeStagingCommit(ctx, t, repo, []string{base}, message)
				current := writeStagingCommit(ctx, t, repo, []string{base}, claim("current", strings.Repeat("d", 40)))
				return previous, current
			},
			want: "carries 2 Kubernetes-commit trailers",
		},
		{
			name: "old side merge",
			arrange: func(t *testing.T, repo *testsupport.Repo, base string) (string, string) {
				orphan := writeStagingCommit(ctx, t, repo, nil, "unrelated root\n")
				previous := writeStagingCommit(ctx, t, repo, []string{base, orphan}, "old tag merge\n")
				current := writeStagingCommit(ctx, t, repo, []string{base}, claim("current", strings.Repeat("b", 40)))
				return previous, current
			},
			want: "parents are",
		},
		{
			name: "no common ancestor",
			arrange: func(t *testing.T, repo *testsupport.Repo, base string) (string, string) {
				previous := writeStagingCommit(ctx, t, repo, nil, "unrelated previous root\n")
				current := writeStagingCommit(ctx, t, repo, []string{base}, claim("current", strings.Repeat("b", 40)))
				return previous, current
			},
			want: "no common ancestor",
		},
		{
			name: "ambiguous merge base",
			arrange: func(t *testing.T, repo *testsupport.Repo, base string) (string, string) {
				left := writeStagingCommit(ctx, t, repo, []string{base}, "left\n")
				right := writeStagingCommit(ctx, t, repo, []string{base}, "right\n")
				previous := writeStagingCommit(ctx, t, repo, []string{left, right}, "merge left first\n")
				current := writeStagingCommit(ctx, t, repo, []string{right, left}, "merge right first\n")
				return previous, current
			},
			want: "more than one best common ancestor",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			repo, base := newStagingTopology(ctx, t)
			previous, current := test.arrange(t, repo, base)
			_, err := gomodmap.StagingReleaseAnchor(ctx, repo.Git, gomodmap.StagingReleaseAnchorOptions{
				ModulePath: "k8s.io/api", Previous: previous, Current: current,
			})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("derive staging release anchor error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestNewStagingIndexRejectsNonFirstParentAnchor(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	repo, base := newStagingTopology(ctx, t)
	mainline := writeStagingCommit(ctx, t, repo, []string{base}, claim("mainline", strings.Repeat("b", 40)))
	side := writeStagingCommit(ctx, t, repo, []string{base}, claim("side", strings.Repeat("c", 40)))
	head := writeStagingCommit(ctx, t, repo, []string{mainline, side}, claim("merge", strings.Repeat("d", 40)))

	_, err := gomodmap.NewStagingIndex(ctx, repo.Git, gomodmap.IndexOptions{
		ModulePath: "k8s.io/api", Revision: head, Anchor: side,
	})
	if err == nil || !strings.Contains(err.Error(), "not the first-parent boundary") {
		t.Fatalf("new staging index error = %v, want first-parent boundary refusal", err)
	}
}

func TestNewStagingIndexRejectsNegativeMaxCountWithAnchor(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	repo, anchor := newStagingTopology(ctx, t)
	current := writeStagingCommit(ctx, t, repo, []string{anchor}, claim("publish current", strings.Repeat("b", 40)))
	_, err := gomodmap.NewStagingIndex(ctx, repo.Git, gomodmap.IndexOptions{
		ModulePath: "k8s.io/api", Revision: current, Anchor: anchor, MaxCount: -1,
	})
	if err == nil || !strings.Contains(err.Error(), "must not be negative") {
		t.Fatalf("new staging index error = %v, want negative-bound refusal", err)
	}
}

func newSourceFixture(ctx context.Context, t *testing.T) *sourceFixture {
	t.Helper()

	repo := testsupport.NewRepo(ctx, t, testsupport.Options{
		Branch:    "main",
		UserName:  fixtureUserName,
		UserEmail: fixtureUserEmail,
	})
	fixture := &sourceFixture{repo: repo, commits: make(map[string]string)}

	fixture.commits["s0"] = repo.WriteAndCommit(ctx, t, "README.md", "base\n", "docs: add readme\n")
	fixture.commits["s1"] = repo.WriteAndCommit(ctx, t,
		"staging/src/k8s.io/api/types.go", "package api\n", "feat: add api types\n")
	fixture.commits["s2"] = repo.WriteAndCommit(ctx, t,
		"pkg/kubelet/kubelet.go", "package kubelet\n", "feat: add kubelet\n")

	// The side branch starts from s1 so the merge below has two real parents.
	fixture.checkout(ctx, t, fixture.commits["s1"])
	fixture.commits["side"] = repo.WriteAndCommit(ctx, t,
		"staging/src/k8s.io/api/side.go", "package api\n", "feat: side work\n")

	fixture.commits["s3"] = fixture.merge(ctx, t, fixture.commits["s2"], fixture.commits["side"])
	fixture.setBranch(ctx, t, fixture.commits["s3"])
	fixture.commits["s4"] = repo.WriteAndCommit(ctx, t,
		"staging/src/k8s.io/api/more.go", "package api\n", "feat: more api types\n")
	return fixture
}

func (f *sourceFixture) sha(t *testing.T, label string) string {
	t.Helper()
	sha, ok := f.commits[label]
	if !ok {
		t.Fatalf("unknown fixture commit %q", label)
	}
	return sha
}

func (f *sourceFixture) checkout(ctx context.Context, t *testing.T, revision string) {
	t.Helper()
	if err := f.repo.Git.CheckoutDetached(ctx, revision); err != nil {
		t.Fatalf("checkout %s: %v", revision, err)
	}
}

// fixtureDate is the date the merge fixture records, in git's raw form. Writing
// a commit object takes the date exactly as git stores it rather than in one of
// the friendlier formats git's date parser accepts, and a fixed value keeps the
// fixture's object names stable across runs.
const fixtureDate = "1700000000 +0000"

// merge writes a merge commit whose tree is the first parent's, which is enough
// to make the topology real without needing a content merge.
func (f *sourceFixture) merge(ctx context.Context, t *testing.T, first, second string) string {
	t.Helper()
	tree, err := f.repo.Git.ResolveTree(ctx, first)
	if err != nil {
		t.Fatalf("resolve tree: %v", err)
	}
	signature := gitcli.Signature{Name: fixtureUserName, Email: fixtureUserEmail, Date: fixtureDate}
	commit, err := f.repo.Git.WriteCommit(ctx, gitcli.CommitTreeOptions{
		Tree:      tree,
		Parents:   []string{first, second},
		Message:   "Merge side work\n",
		Author:    signature,
		Committer: signature,
	})
	if err != nil {
		t.Fatalf("write merge commit: %v", err)
	}
	return commit
}

// setBranch points main at a commit and checks it out, so later commits extend
// it.
func (f *sourceFixture) setBranch(ctx context.Context, t *testing.T, revision string) {
	t.Helper()
	name := "refs/heads/main"
	current, err := f.repo.Git.ResolveCommit(ctx, name)
	if err != nil {
		t.Fatalf("resolve %s: %v", name, err)
	}
	if err := f.repo.Git.UpdateRef(ctx, name, revision, current); err != nil {
		t.Fatalf("update %s: %v", name, err)
	}
	if err := f.repo.Git.ResetHard(ctx, revision); err != nil {
		t.Fatalf("reset to %s: %v", revision, err)
	}
	if err := f.repo.Git.CheckoutDetached(ctx, name); err != nil {
		t.Fatalf("checkout %s: %v", name, err)
	}
}

// claim renders a staging commit message that claims one source commit.
func claim(subject, source string) string {
	return subject + "\n\n" + gomodmap.KubernetesCommitTrailer + ": " + source + "\n"
}

// newStagingFixture builds a staging repository from complete commit messages,
// so a test can write a message git would parse differently from the usual
// shape rather than only well formed claims.
func newStagingFixture(ctx context.Context, t *testing.T, messages []string) *testsupport.Repo {
	t.Helper()

	repo := testsupport.NewRepo(ctx, t, testsupport.Options{
		Branch:    "master",
		UserName:  fixtureUserName,
		UserEmail: fixtureUserEmail,
	})
	for i, message := range messages {
		repo.WriteAndCommit(ctx, t, "file.go", strings.Repeat("x", i+1)+"\n", message)
	}
	return repo
}

func TestCarryForwardStagingMapping(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	source := newSourceFixture(ctx, t)
	mainline, err := gomodmap.NewSourceMainline(ctx, source.repo.Git, gomodmap.MainlineOptions{
		Revision: source.sha(t, "s4"), Anchor: source.sha(t, "s1"),
	})
	if err != nil {
		t.Fatalf("build source mainline: %v", err)
	}

	t.Run("divergent unclaimed release delta", func(t *testing.T) {
		repo, base := newStagingTopology(ctx, t)
		previous := writeStagingCommit(ctx, t, repo, []string{base}, "previous dependency update\n")
		current := writeStagingCommit(ctx, t, repo, []string{base}, "current dependency update\n")
		anchor, err := gomodmap.StagingReleaseAnchor(ctx, repo.Git, gomodmap.StagingReleaseAnchorOptions{
			ModulePath: "k8s.io/cri-streaming", Previous: previous, Current: current,
		})
		if err != nil {
			t.Fatalf("derive release anchor: %v", err)
		}
		index, err := gomodmap.NewStagingIndex(ctx, repo.Git, gomodmap.IndexOptions{
			ModulePath: "k8s.io/cri-streaming", Revision: current, Anchor: anchor, Previous: previous,
		})
		if err != nil {
			t.Fatalf("build staging index: %v", err)
		}
		mapping, err := mapStagingRelease(ctx, index, repo.Git, mainline, previous, current, "v0.36.2")
		if err != nil {
			t.Fatalf("map adjacent release: %v", err)
		}
		assertCarriedMapping(t, mapping, source, previous)
	})

	t.Run("same release target", func(t *testing.T) {
		repo, target := newStagingTopology(ctx, t)
		index, err := gomodmap.NewStagingIndex(ctx, repo.Git, gomodmap.IndexOptions{
			ModulePath: "k8s.io/streaming", Revision: target, Anchor: target, Previous: target,
		})
		if err != nil {
			t.Fatalf("build staging index: %v", err)
		}
		mapping, err := mapStagingRelease(ctx, index, repo.Git, mainline, target, target, "v0.36.2")
		if err != nil {
			t.Fatalf("map adjacent release: %v", err)
		}
		assertCarriedMapping(t, mapping, source, target)
	})

	t.Run("entirely unclaimed same target", func(t *testing.T) {
		repo := testsupport.NewRepo(ctx, t, testsupport.Options{
			Branch: "master", UserName: fixtureUserName, UserEmail: fixtureUserEmail,
		})
		target := repo.WriteAndCommit(ctx, t, "go.mod", "module k8s.io/api\n\ngo 1.26.0\n", "dependency update\n")
		index, err := gomodmap.NewStagingIndex(ctx, repo.Git, gomodmap.IndexOptions{
			ModulePath: "k8s.io/api", Revision: target, Anchor: target, Previous: target,
		})
		if err != nil {
			t.Fatalf("build unclaimed release index: %v", err)
		}
		mapping, err := mapStagingRelease(ctx, index, repo.Git, mainline, target, target, "v0.36.2")
		if err != nil {
			t.Fatalf("map unclaimed adjacent release: %v", err)
		}
		assertCarriedMapping(t, mapping, source, target)
	})
}

func assertCarriedMapping(t *testing.T, mapping gomodmap.CommitMapping, source *sourceFixture, target string) {
	t.Helper()
	if mapping.Source != source.sha(t, "s4") || mapping.Matched != source.sha(t, "s1") ||
		mapping.Staging != target || mapping.Version != "v0.36.2" || mapping.Distance != 3 || !mapping.Carried {
		t.Fatalf("carry-forward mapping = %#v", mapping)
	}
	if !mapping.Collapsed() {
		t.Error("carry-forward mapping should be collapsed onto the prior release boundary")
	}
}

func TestCarryForwardStagingMappingRejectsUnprovedCarry(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	source := newSourceFixture(ctx, t)
	mainline, err := gomodmap.NewSourceMainline(ctx, source.repo.Git, gomodmap.MainlineOptions{
		Revision: source.sha(t, "s4"), Anchor: source.sha(t, "s1"),
	})
	if err != nil {
		t.Fatalf("build source mainline: %v", err)
	}

	t.Run("new unrelated claim", func(t *testing.T) {
		repo, base := newStagingTopology(ctx, t)
		previous := writeStagingCommit(ctx, t, repo, []string{base}, "previous dependency update\n")
		current := writeStagingCommit(ctx, t, repo, []string{base}, claim("unrelated current claim", strings.Repeat("e", 40)))
		index := stagingReleaseIndex(ctx, t, repo, previous, current, 0)
		_, err := mapStagingRelease(ctx, index, repo.Git, mainline, previous, current, "v0.36.2")
		if err == nil || !strings.Contains(err.Error(), "no commit claims") {
			t.Fatalf("map release error = %v, want unmapped claim refusal", err)
		}
	})

	t.Run("sampled index", func(t *testing.T) {
		repo, base := newStagingTopology(ctx, t)
		previous := writeStagingCommit(ctx, t, repo, []string{base}, "previous dependency update\n")
		current := writeStagingCommit(ctx, t, repo, []string{base}, "current dependency update\n")
		index := stagingReleaseIndex(ctx, t, repo, previous, current, 1)
		_, err := mapStagingRelease(ctx, index, repo.Git, mainline, previous, current, "v0.36.2")
		if err == nil || !strings.Contains(err.Error(), "sampled index") {
			t.Fatalf("map release error = %v, want sampled-index refusal", err)
		}
	})

	t.Run("claim-free package source change", func(t *testing.T) {
		repo, base := newStagingTopology(ctx, t)
		previous := writeStagingCommit(ctx, t, repo, []string{base}, "previous dependency update\n")
		current := writeStagingFileCommit(ctx, t, repo, base, "types.go", "package api\n", "current unclaimed source change\n")
		index := stagingReleaseIndex(ctx, t, repo, previous, current, 0)
		_, err := mapStagingRelease(ctx, index, repo.Git, mainline, previous, current, "v0.36.2")
		if err == nil || !strings.Contains(err.Error(), "claim-free delta changes package source") {
			t.Fatalf("map release error = %v, want package-source refusal", err)
		}
	})

	t.Run("mismatched previous target", func(t *testing.T) {
		repo, base := newStagingTopology(ctx, t)
		previous := writeStagingCommit(ctx, t, repo, []string{base}, "previous dependency update\n")
		otherPrevious := writeStagingCommit(ctx, t, repo, []string{base}, "different previous dependency update\n")
		current := writeStagingCommit(ctx, t, repo, []string{base}, "current dependency update\n")
		index := stagingReleaseIndex(ctx, t, repo, previous, current, 0)
		_, err := mapStagingRelease(ctx, index, repo.Git, mainline, otherPrevious, current, "v0.36.2")
		if err == nil || !strings.Contains(err.Error(), "does not match indexed target") {
			t.Fatalf("map release error = %v, want previous-target mismatch", err)
		}
	})

	t.Run("mismatched current target", func(t *testing.T) {
		repo, target := newStagingTopology(ctx, t)
		index, err := gomodmap.NewStagingIndex(ctx, repo.Git, gomodmap.IndexOptions{
			ModulePath: "k8s.io/api", Revision: target, Anchor: target, Previous: target,
		})
		if err != nil {
			t.Fatalf("build staging index: %v", err)
		}
		_, err = mapStagingRelease(ctx, index, repo.Git, mainline, target, strings.Repeat("e", 40), "v0.36.2")
		if err == nil || !strings.Contains(err.Error(), "does not match indexed revision") {
			t.Fatalf("map release error = %v, want revision mismatch", err)
		}
	})

	t.Run("malformed targets", func(t *testing.T) {
		repo, target := newStagingTopology(ctx, t)
		index, err := gomodmap.NewStagingIndex(ctx, repo.Git, gomodmap.IndexOptions{
			ModulePath: "k8s.io/api", Revision: target, Anchor: target, Previous: target,
		})
		if err != nil {
			t.Fatalf("build staging index: %v", err)
		}
		for _, test := range []struct {
			name     string
			previous string
			current  string
			want     string
		}{
			{name: "previous", previous: "bad", current: target, want: "previous target"},
			{name: "current", previous: target, current: "bad", want: "current target"},
		} {
			t.Run(test.name, func(t *testing.T) {
				_, err := mapStagingRelease(ctx, index, repo.Git, mainline, test.previous, test.current, "v0.36.2")
				if err == nil || !strings.Contains(err.Error(), test.want) {
					t.Fatalf("map release error = %v, want %q", err, test.want)
				}
			})
		}
	})

	t.Run("invalid previous version", func(t *testing.T) {
		repo, base := newStagingTopology(ctx, t)
		previous := writeStagingCommit(ctx, t, repo, []string{base}, "previous dependency update\n")
		current := writeStagingCommit(ctx, t, repo, []string{base}, "current dependency update\n")
		index := stagingReleaseIndex(ctx, t, repo, previous, current, 0)
		_, err := mapStagingRelease(ctx, index, repo.Git, mainline, previous, current, "latest")
		if err == nil || !strings.Contains(err.Error(), "previous version") {
			t.Fatalf("map release error = %v, want invalid-version refusal", err)
		}
	})

	t.Run("missing mainline", func(t *testing.T) {
		repo, target := newStagingTopology(ctx, t)
		index, err := gomodmap.NewStagingIndex(ctx, repo.Git, gomodmap.IndexOptions{
			ModulePath: "k8s.io/api", Revision: target, Anchor: target, Previous: target,
		})
		if err != nil {
			t.Fatalf("build staging index: %v", err)
		}
		_, err = mapStagingRelease(ctx, index, repo.Git, nil, target, target, "v0.36.2")
		if err == nil || !strings.Contains(err.Error(), "source mainline") {
			t.Fatalf("map release error = %v, want missing-mainline refusal", err)
		}
	})
}

func stagingReleaseIndex(ctx context.Context, t *testing.T, repo *testsupport.Repo, previous, current string, maxCount int) *gomodmap.StagingIndex {
	t.Helper()
	anchor, err := gomodmap.StagingReleaseAnchor(ctx, repo.Git, gomodmap.StagingReleaseAnchorOptions{
		ModulePath: "k8s.io/api", Previous: previous, Current: current,
	})
	if err != nil {
		t.Fatalf("derive release anchor: %v", err)
	}
	index, err := gomodmap.NewStagingIndex(ctx, repo.Git, gomodmap.IndexOptions{
		ModulePath: "k8s.io/api", Revision: current, Anchor: anchor,
		Previous: previous, MaxCount: maxCount,
	})
	if err != nil {
		t.Fatalf("build staging index: %v", err)
	}
	return index
}

func mapStagingRelease(
	ctx context.Context,
	index *gomodmap.StagingIndex,
	git *gitcli.Runner,
	mainline *gomodmap.SourceMainline,
	previous, current, version string,
) (gomodmap.CommitMapping, error) {
	return index.MapRelease(ctx, git, mainline, gomodmap.StagingReleaseMappingOptions{
		Previous: previous, Current: current, PreviousVersion: version,
	})
}

func TestStagingIndex_MapReleaseMetadataCarry(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	source := newSourceFixture(ctx, t)
	mainline, err := gomodmap.NewSourceMainline(ctx, source.repo.Git, gomodmap.MainlineOptions{
		Revision: source.sha(t, "s4"), Anchor: source.sha(t, "s1"),
	})
	if err != nil {
		t.Fatalf("build source mainline: %v", err)
	}

	const previousGoMod = "module k8s.io/api\n\ngo 1.26.0\n\nrequire example.com/dependency v1.0.0\n"
	tests := []struct {
		name        string
		previous    string
		path        string
		contents    string
		wantCarried bool
	}{
		{
			name: "requirement upgrade and replacement",
			path: "go.mod",
			contents: "module k8s.io/api\n\ngo 1.26.0\n\nrequire example.com/dependency v1.1.0\n\n" +
				"replace example.com/dependency => ../dependency\n",
			wantCarried: true,
		},
		{
			name:     "flattened staging placeholder",
			previous: "module k8s.io/api\n\ngo 1.26.0\n\nrequire k8s.io/apimachinery v0.36.2\n",
			path:     "go.mod",
			contents: "module k8s.io/api\n\ngo 1.26.0\n\nrequire k8s.io/apimachinery v0.0.0\n\n" +
				"replace k8s.io/apimachinery => ../apimachinery\n",
			wantCarried: true,
		},
		{
			name:     "requirement downgrade",
			path:     "go.mod",
			contents: "module k8s.io/api\n\ngo 1.26.0\n\nrequire example.com/dependency v0.9.0\n",
		},
		{
			name:     "requirement removal",
			path:     "go.mod",
			contents: "module k8s.io/api\n\ngo 1.26.0\n",
		},
		{name: "package source", path: "types.go", contents: "package api\n"},
		{
			name:     "module language semantics",
			path:     "go.mod",
			contents: "module k8s.io/api\n\ngo 1.27.0\n\nrequire example.com/dependency v1.1.0\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repo, base := newStagingTopology(ctx, t)
			prior := test.previous
			if prior == "" {
				prior = previousGoMod
			}
			anchor := writeStagingFileCommit(
				ctx, t, repo, base, "go.mod", prior,
				claim("publish previous source", source.sha(t, "s1")),
			)
			previous := writeStagingCommit(ctx, t, repo, []string{anchor}, "previous dependency update\n")
			current := writeStagingFileCommit(
				ctx, t, repo, anchor, test.path, test.contents,
				claim("publish current source", source.sha(t, "s4")),
			)
			index := stagingReleaseIndex(ctx, t, repo, previous, current, 0)
			mapping, err := mapStagingRelease(ctx, index, repo.Git, mainline, previous, current, "v0.36.2")
			if err != nil {
				t.Fatalf("map release: %v", err)
			}
			if mapping.Carried != test.wantCarried {
				t.Fatalf("mapping carried = %t, want %t: %#v", mapping.Carried, test.wantCarried, mapping)
			}
			if test.wantCarried {
				if mapping.Staging != previous || mapping.Version != "v0.36.2" {
					t.Fatalf("carried mapping = %#v", mapping)
				}
			} else if mapping.Staging != current || mapping.Version != "" {
				t.Fatalf("source-changing mapping = %#v, want current commit", mapping)
			}
		})
	}
}

func TestStagingIndex_Map(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	source := newSourceFixture(ctx, t)

	// The staging repository publishes s0, s1, s4 and the side commit. It does
	// not publish s2 or s3, which changed nothing under the staging directory.
	staging := newStagingFixture(ctx, t, []string{
		claim("publish base", source.sha(t, "s0")),
		claim("publish api types", source.sha(t, "s1")),
		claim("publish side work", source.sha(t, "side")),
		"chore: no claim\n",
		claim("publish more api types", source.sha(t, "s4")),
	})

	index, err := gomodmap.NewStagingIndex(ctx, staging.Git, gomodmap.IndexOptions{
		ModulePath: "k8s.io/api",
		Revision:   "HEAD",
	})
	if err != nil {
		t.Fatalf("new staging index: %v", err)
	}
	if index.Len() != 4 {
		t.Errorf("index covers %d source commits, want 4", index.Len())
	}

	tests := []struct {
		name         string
		from         string
		wantMatched  string
		wantDistance int
	}{
		{
			name:         "commit with its own staging commit",
			from:         "s4",
			wantMatched:  "s4",
			wantDistance: 0,
		},
		{
			// s2 changed nothing under staging, so its content is whatever s1
			// published.
			name:         "collapsed onto the nearest ancestor",
			from:         "s2",
			wantMatched:  "s1",
			wantDistance: 1,
		},
		{
			// The merge's second parent is published, but it is not on the
			// mainline, so the mapping has to walk past it to s1.
			name:         "merge does not map onto its side parent",
			from:         "s3",
			wantMatched:  "s1",
			wantDistance: 2,
		},
		{
			name:         "root commit",
			from:         "s0",
			wantMatched:  "s0",
			wantDistance: 0,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			mainline, err := gomodmap.NewSourceMainline(ctx, source.repo.Git, gomodmap.MainlineOptions{
				Revision: source.sha(t, test.from),
			})
			if err != nil {
				t.Fatalf("new source mainline: %v", err)
			}
			mapping, err := index.Map(mainline)
			if err != nil {
				t.Fatalf("map %s: %v", test.from, err)
			}
			if got, want := mapping.Matched, source.sha(t, test.wantMatched); got != want {
				t.Errorf("matched %s, want %s (%s)", got, want, test.wantMatched)
			}
			if mapping.Distance != test.wantDistance {
				t.Errorf("distance = %d, want %d", mapping.Distance, test.wantDistance)
			}
			if got, want := mapping.Collapsed(), test.wantDistance > 0; got != want {
				t.Errorf("collapsed = %v, want %v", got, want)
			}
			if mapping.Source != source.sha(t, test.from) {
				t.Errorf("source = %s, want %s", mapping.Source, source.sha(t, test.from))
			}
			if mapping.ModulePath != "k8s.io/api" {
				t.Errorf("module path = %q, want k8s.io/api", mapping.ModulePath)
			}
		})
	}
}

// TestStagingIndex_Map_Unmapped proves an unmappable source commit is a failure
// rather than a silent fallback onto the oldest staging commit.
func TestStagingIndex_Map_Unmapped(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	source := newSourceFixture(ctx, t)
	other := newSourceFixture(ctx, t)
	// Two independently created fixtures can have identical commit objects when
	// Git records them in the same second. Extend the other history with content
	// the source does not have so the claimed commit is objectively unrelated.
	unrelated := other.repo.WriteAndCommit(ctx, t,
		"UNRELATED.md", "unrelated source\n", "test: diverge unrelated history\n")

	// The staging repository only knows commits from an unrelated history.
	staging := newStagingFixture(ctx, t, []string{
		claim("publish unrelated", unrelated),
	})
	index, err := gomodmap.NewStagingIndex(ctx, staging.Git, gomodmap.IndexOptions{
		ModulePath: "k8s.io/api",
		Revision:   "HEAD",
	})
	if err != nil {
		t.Fatalf("new staging index: %v", err)
	}

	mainline, err := gomodmap.NewSourceMainline(ctx, source.repo.Git, gomodmap.MainlineOptions{
		Revision: source.sha(t, "s4"),
	})
	if err != nil {
		t.Fatalf("new source mainline: %v", err)
	}
	_, err = index.Map(mainline)
	if err == nil {
		t.Fatal("map: got nil error, want an unmapped commit error")
	}
	if !strings.Contains(err.Error(), "no commit claims") {
		t.Errorf("map: error = %v, want it to report an unmapped commit", err)
	}
}

func TestNewStagingIndex_Rejects(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	source := newSourceFixture(ctx, t)

	tests := []struct {
		name     string
		messages []string
		wantErr  string
	}{
		{
			// Two claims mean the commit does not establish which source commit
			// produced it, so choosing either one would pin on a guess. Both
			// trailers are in the same paragraph, because git only reads the last
			// paragraph of a message as trailers at all.
			name: "two claims on one commit",
			messages: []string{
				"publish base\n\n" +
					gomodmap.KubernetesCommitTrailer + ": " + source.sha(t, "s0") + "\n" +
					gomodmap.KubernetesCommitTrailer + ": " + source.sha(t, "s1") + "\n",
			},
			wantErr: "carries 2 Kubernetes-commit trailers",
		},
		{
			name:     "claim is not an object name",
			messages: []string{claim("publish base", "not-a-sha")},
			wantErr:  "must be 40 or 64 hexadecimal characters",
		},
		{
			name:     "claim is abbreviated",
			messages: []string{claim("publish base", source.sha(t, "s0")[:12])},
			wantErr:  "must be 40 or 64 hexadecimal characters",
		},
		{
			name:     "no commit claims anything",
			messages: []string{"chore: no claim\n"},
			wantErr:  "no commit under HEAD carries a Kubernetes-commit trailer",
		},
		{
			// A trailer shaped line in the first paragraph is the subject, not a
			// claim, so a message like this establishes nothing.
			name:     "claim in the subject paragraph is not a trailer",
			messages: []string{gomodmap.KubernetesCommitTrailer + ": " + source.sha(t, "s0") + "\n"},
			wantErr:  "no commit under HEAD carries a Kubernetes-commit trailer",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			staging := newStagingFixture(ctx, t, test.messages)
			_, err := gomodmap.NewStagingIndex(ctx, staging.Git, gomodmap.IndexOptions{
				ModulePath: "k8s.io/api",
				Revision:   "HEAD",
			})
			if err == nil {
				t.Fatalf("new staging index: got nil error, want %q", test.wantErr)
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Errorf("new staging index: error = %v, want it to contain %q", err, test.wantErr)
			}
		})
	}
}

// TestNewStagingIndex_LatestClaimWins proves a source commit republished by a
// later staging commit maps onto the newer one, which is the tree the release
// actually shipped.
func TestNewStagingIndex_LatestClaimWins(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	source := newSourceFixture(ctx, t)
	staging := newStagingFixture(ctx, t, []string{
		claim("publish api types", source.sha(t, "s1")),
		claim("republish api types", source.sha(t, "s1")),
	})

	index, err := gomodmap.NewStagingIndex(ctx, staging.Git, gomodmap.IndexOptions{
		ModulePath: "k8s.io/api",
		Revision:   "HEAD",
	})
	if err != nil {
		t.Fatalf("new staging index: %v", err)
	}
	mainline, err := gomodmap.NewSourceMainline(ctx, source.repo.Git, gomodmap.MainlineOptions{
		Revision: source.sha(t, "s1"),
	})
	if err != nil {
		t.Fatalf("new source mainline: %v", err)
	}
	mapping, err := index.Map(mainline)
	if err != nil {
		t.Fatalf("map: %v", err)
	}

	head, err := staging.Git.ResolveCommit(ctx, "HEAD")
	if err != nil {
		t.Fatalf("resolve staging HEAD: %v", err)
	}
	if mapping.Staging != head {
		t.Errorf("staging commit = %s, want the newest claim %s", mapping.Staging, head)
	}
}

func TestNewSourceMainline(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	source := newSourceFixture(ctx, t)

	mainline, err := gomodmap.NewSourceMainline(ctx, source.repo.Git, gomodmap.MainlineOptions{
		Revision: source.sha(t, "s4"),
	})
	if err != nil {
		t.Fatalf("new source mainline: %v", err)
	}
	// s0, s1, s2, s3, s4 are on the mainline; side is not.
	if mainline.Len() != 5 {
		t.Errorf("mainline covers %d commits, want 5", mainline.Len())
	}
	if mainline.Head() != source.sha(t, "s4") {
		t.Errorf("head = %s, want %s", mainline.Head(), source.sha(t, "s4"))
	}
}

// TestNewSourceMainline_Bounded proves MaxCount bounds the walk from the tip
// rather than from the root.
func TestNewSourceMainline_Bounded(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	source := newSourceFixture(ctx, t)

	mainline, err := gomodmap.NewSourceMainline(ctx, source.repo.Git, gomodmap.MainlineOptions{
		Revision: source.sha(t, "s4"),
		MaxCount: 2,
	})
	if err != nil {
		t.Fatalf("new source mainline: %v", err)
	}
	if mainline.Len() != 2 {
		t.Fatalf("mainline covers %d commits, want 2", mainline.Len())
	}
	if mainline.Head() != source.sha(t, "s4") {
		t.Errorf("head = %s, want the tip %s", mainline.Head(), source.sha(t, "s4"))
	}
}

func TestNewSourceMainline_AnchorIsInclusive(t *testing.T) {
	ctx := t.Context()
	source := newSourceFixture(ctx, t)
	mainline, err := gomodmap.NewSourceMainline(ctx, source.repo.Git, gomodmap.MainlineOptions{
		Revision: source.sha(t, "s4"),
		Anchor:   source.sha(t, "s2"),
	})
	if err != nil {
		t.Fatalf("new bounded source mainline: %v", err)
	}
	if mainline.Len() != 3 {
		t.Errorf("mainline covers %d commits, want s4, s3, and inclusive s2", mainline.Len())
	}
	if mainline.Head() != source.sha(t, "s4") {
		t.Errorf("head = %s, want %s", mainline.Head(), source.sha(t, "s4"))
	}
}

func TestNewStagingIndex_AnchorIsInclusive(t *testing.T) {
	ctx := t.Context()
	source := newSourceFixture(ctx, t)
	staging := newStagingFixture(ctx, t, []string{
		claim("publish base", source.sha(t, "s0")),
		claim("publish anchor", source.sha(t, "s2")),
		claim("publish head", source.sha(t, "s4")),
	})
	commits, err := staging.Git.CommitLog(ctx, gitcli.CommitLogOptions{Include: []string{"HEAD"}})
	if err != nil {
		t.Fatalf("read staging commits: %v", err)
	}
	index, err := gomodmap.NewStagingIndex(ctx, staging.Git, gomodmap.IndexOptions{
		ModulePath: "k8s.io/api", Revision: commits[2].SHA, Anchor: commits[1].SHA,
	})
	if err != nil {
		t.Fatalf("new bounded staging index: %v", err)
	}
	if index.Len() != 2 {
		t.Errorf("staging index has %d claims, want anchor and head", index.Len())
	}
}

func TestNewSourceMainline_Rejects(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	source := newSourceFixture(ctx, t)

	if _, err := gomodmap.NewSourceMainline(ctx, source.repo.Git, gomodmap.MainlineOptions{}); err == nil {
		t.Error("new source mainline: got nil error, want a missing revision error")
	}
}
