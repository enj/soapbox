package sync

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/config"
	"github.com/enj/soapbox/tools/internal/generate"
	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/relocate"
	"github.com/enj/soapbox/tools/internal/replay"
	"github.com/enj/soapbox/tools/internal/source"
	"github.com/enj/soapbox/tools/internal/testsupport"
)

const dagProfileHash = "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"

var dagSignature = gitcli.Signature{
	Name:  "Soapbox DAG Test",
	Email: "dag@example.com",
	Date:  "1700000000 +0000",
}

func TestRelevantDAGPrefiltersPathsAndPreservesMergeParents(t *testing.T) {
	ctx := t.Context()
	sourceRepo := testsupport.NewRepo(ctx, t, testsupport.Options{
		Branch: "main", UserName: dagSignature.Name, UserEmail: dagSignature.Email,
	})
	sourceRepo.SetConfig(ctx, t, "uploadpack.allowFilter", "true")
	sourceRepo.WriteFile(t, "pkg/root/value.go", "package root\nconst Value = 0\n")
	anchor := sourceRepo.Commit(ctx, t, "release anchor\n", gitcli.CommitOptions{}, "pkg/root/value.go")
	sourceRepo.WriteFile(t, "docs/readme.md", "irrelevant\n")
	docs := sourceRepo.Commit(ctx, t, "docs: irrelevant\n", gitcli.CommitOptions{}, "docs/readme.md")

	// Build a side commit directly so the worktree can continue down main while
	// the source graph retains a real two-parent merge.
	sideTree := dagTreeWithFile(ctx, t, sourceRepo.Git, docs, "pkg/root/value.go", "package root\nconst Value = 2\n")
	side, err := sourceRepo.Git.WriteCommit(ctx, gitcli.CommitTreeOptions{
		Tree: sideTree, Parents: []string{docs}, Message: "feat: side root change\n",
		Author: dagSignature, Committer: dagSignature,
	})
	if err != nil {
		t.Fatalf("write side commit: %v", err)
	}

	sourceRepo.WriteFile(t, "pkg/root/value.go", "package root\nconst Value = 1\n")
	main := sourceRepo.Commit(ctx, t, "feat: main root change\n", gitcli.CommitOptions{}, "pkg/root/value.go")
	mergeTree := dagTreeWithFile(ctx, t, sourceRepo.Git, main, "pkg/root/value.go", "package root\nconst Value = 3\n")
	merge, err := sourceRepo.Git.WriteCommit(ctx, gitcli.CommitTreeOptions{
		Tree: mergeTree, Parents: []string{main, side}, Message: "merge: preserve both relevant sides\n",
		Author: dagSignature, Committer: dagSignature,
	})
	if err != nil {
		t.Fatalf("write merge commit: %v", err)
	}
	if err := sourceRepo.Git.CreateTag(ctx, gitcli.TagOptions{
		Name: "v1.0.1", Commit: merge, Message: "release v1.0.1\n", Tagger: dagSignature,
	}); err != nil {
		t.Fatalf("tag release: %v", err)
	}

	cacheRoot := t.TempDir()
	anonymous, err := gitcli.New(ctx, gitcli.Options{Inherit: []string{"PATH"}})
	if err != nil {
		t.Fatalf("source runner: %v", err)
	}
	cache, err := source.Open(ctx, source.Options{
		Remote: "file://" + sourceRepo.Dir, CacheRoot: cacheRoot,
		WorktreeRoot: t.TempDir(), Git: anonymous,
	})
	if err != nil {
		t.Fatalf("open source cache: %v", err)
	}
	if err := cache.Fetch(ctx, source.Refs{Tags: []string{"v1.0.1"}}); err != nil {
		t.Fatalf("fetch release: %v", err)
	}
	resolved, err := cache.Resolve(ctx, source.Refs{Tags: []string{"v1.0.1"}})
	if err != nil || len(resolved) != 1 {
		t.Fatalf("resolve release = %#v, %v", resolved, err)
	}

	destination := testsupport.NewRepo(ctx, t, testsupport.Options{
		Branch: "main", UserName: dagSignature.Name, UserEmail: dagSignature.Email,
	})
	for path, contents := range map[string]string{
		".github/workflows/ci.yml":   "name: ci\n",
		".github/workflows/sync.yml": "name: sync\n",
		"go.mod":                     "module example.com/control\n",
		"soapbox.yaml":               "version: 2\n",
		"tools/cmd/soapbox/main.go":  "package main\n",
		"tools/go.mod":               "module example.com/control/tools\n",
	} {
		destination.WriteFile(t, path, contents)
	}
	parent := destination.Commit(ctx, t, "setup control plane\n", gitcli.CommitOptions{},
		".github/workflows/ci.yml", ".github/workflows/sync.yml", "go.mod", "soapbox.yaml",
		"tools/cmd/soapbox/main.go", "tools/go.mod")

	cfg := &config.Config{
		Source:      config.Source{Repository: "file://" + sourceRepo.Dir, ImportPrefix: "k8s.io/kubernetes"},
		Destination: config.Destination{InternalPrefix: "internal/kk"},
		Packages:    config.Packages{Roots: []string{"pkg/root"}},
		Release:     config.Release{Policy: config.ReleasePolicyV1ToV0},
		Commit: config.Commit{
			Committer:  config.Identity{Name: dagSignature.Name, Email: dagSignature.Email},
			TrailerKey: "Kubernetes-commit",
		},
	}
	r, commits, err := newDAGRun(ctx, DAGOptions{
		Config: cfg, SourceCache: cache, DestinationGit: destination.Git,
		Generate:     generate.Options{Config: cfg, CacheRoot: cacheRoot, SourceRemote: cache.Remote()},
		AnchorCommit: anchor, AnchorTag: "v1.0.0", MappedAnchor: true,
		EpochParent: parent,
		Release:     source.Release{Source: resolved[0], DestinationTag: "v0.0.1"},
	})
	if err != nil {
		t.Fatalf("new DAG run: %v", err)
	}
	if got := commits[len(commits)-1].Parents; !slices.Equal(got, []string{main, side}) {
		t.Fatalf("source merge parents = %v, want [%s %s]", got, main, side)
	}

	r.generated[anchor] = dagGenerated("anchor")
	r.addWatched(r.generated[anchor])
	r.generated[main] = dagGenerated("main")
	r.generated[side] = dagGenerated("side")
	r.generated[merge] = dagGenerated("merge")

	projected, err := replay.Run(ctx, destination.Git, replay.Options{
		Commits: commits, Anchor: anchor, Heads: []string{merge},
		Epoch:         replay.Epoch{ProfileHash: dagProfileHash, Parent: parent},
		Bot:           replay.Identity{Name: dagSignature.Name, Email: dagSignature.Email},
		ProvenanceKey: cfg.Commit.TrailerKey,
		Transform:     r.transform,
	})
	if err != nil {
		t.Fatalf("replay relevant DAG: %v", err)
	}
	if r.result.GeneratedCommits != 3 || r.result.PrefilteredCommits != 2 {
		t.Errorf("generated %d, prefiltered %d, want 3 and 2", r.result.GeneratedCommits, r.result.PrefilteredCommits)
	}

	records := make(map[string]replay.Record, len(projected.Records))
	for _, record := range projected.Records {
		records[record.Source] = record
	}
	if !records[anchor].Collapsed || !records[docs].Collapsed {
		t.Errorf("anchor/docs records were not collapsed: %#v %#v", records[anchor], records[docs])
	}
	mergeRecord := records[merge]
	if !mergeRecord.Merge || len(mergeRecord.MappedParents) != 2 {
		t.Fatalf("merge record = %#v, want two preserved destination parents", mergeRecord)
	}
	if len(projected.Heads) != 1 || projected.Heads[0].Destination != mergeRecord.Destination {
		t.Fatalf("replay heads = %#v, merge destination = %s", projected.Heads, mergeRecord.Destination)
	}
}

func dagTreeWithFile(ctx context.Context, t *testing.T, git *gitcli.Runner, parent, path string, contents string) string {
	t.Helper()
	tree, err := git.ResolveTree(ctx, parent)
	if err != nil {
		t.Fatalf("resolve parent tree: %v", err)
	}
	entries, err := git.ListTree(ctx, tree)
	if err != nil {
		t.Fatalf("list parent tree: %v", err)
	}
	blob, err := git.WriteBlob(ctx, []byte(contents))
	if err != nil {
		t.Fatalf("write source blob: %v", err)
	}
	replaced := false
	for i := range entries {
		if entries[i].Path == path {
			entries[i].Mode = gitcli.ModeRegular
			entries[i].Object = blob
			replaced = true
		}
	}
	if !replaced {
		entries = append(entries, gitcli.TreeEntry{Mode: gitcli.ModeRegular, Object: blob, Path: path})
	}
	tree, err = git.WriteTree(ctx, entries)
	if err != nil {
		t.Fatalf("write source tree: %v", err)
	}
	return tree
}

func dagGenerated(value string) *generate.Result {
	files := relocate.FileSet{Files: []relocate.File{
		{Path: "go.mod", Mode: relocate.ModeRegular, Contents: []byte("module example.com/generated\n")},
		{Path: "internal/kk/pkg/root/value.go", Mode: relocate.ModeRegular, Contents: []byte("package root\nconst Value = \"" + value + "\"\n")},
	}}
	return &generate.Result{
		Files: files,
		Report: generate.Report{
			Engine: generate.EngineReport{ProfileHash: dagProfileHash},
			Extract: generate.ExtractReport{Post: generate.PassReport{
				ClosurePackages: []string{"k8s.io/kubernetes/pkg/root"},
			}},
		},
	}
}

func TestInitialWatchedAndDynamicClosureExpansion(t *testing.T) {
	cfg := &config.Config{
		Source:       config.Source{ImportPrefix: "k8s.io/kubernetes"},
		Packages:     config.Packages{Roots: []string{"pkg/root"}},
		Dependencies: config.Dependencies{CopyPackages: []string{"staging/src/k8s.io/component-helpers/auth/rbac/validation"}},
	}
	r := dagRun{opts: DAGOptions{Config: cfg}, watched: initialWatched(cfg)}
	r.addWatched(&generate.Result{Report: generate.Report{
		Extract: generate.ExtractReport{Post: generate.PassReport{ClosurePackages: []string{
			"k8s.io/kubernetes/pkg/registry/rbac/validation",
			"k8s.io/api/rbac/v1",
		}}},
		Staging: generate.StagingReport{Modules: []generate.ModulePin{{
			Path: "k8s.io/api", Directory: "staging/src/k8s.io/api",
		}}},
	}})
	for _, path := range []string{
		"pkg/root/file.go",
		"pkg/registry/rbac/validation/rule.go",
		"staging/src/k8s.io/api/rbac/v1/types.go",
		"staging/src/k8s.io/component-helpers/auth/rbac/validation/policy.go",
		"go.mod",
	} {
		if !r.watches(path) {
			t.Errorf("watched closure does not cover %s", path)
		}
	}
	if r.watches("docs/readme.md") || r.watches("pkg/unrelated/file.go") {
		t.Errorf("watched closure unexpectedly covers an unrelated path: %v", r.watched)
	}
	paths := make([]string, 0, len(r.watched))
	for path := range r.watched {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	if strings.Join(paths, "\n") == "" {
		t.Fatal("watched closure is empty")
	}
}
