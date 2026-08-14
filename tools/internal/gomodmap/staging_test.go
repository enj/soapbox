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

	// The staging repository only knows commits from an unrelated history.
	staging := newStagingFixture(ctx, t, []string{
		claim("publish base", other.sha(t, "s0")),
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
