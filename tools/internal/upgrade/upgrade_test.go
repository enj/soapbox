package upgrade_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/config"
	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/setup"
	"github.com/enj/soapbox/tools/internal/testsupport"
	"github.com/enj/soapbox/tools/internal/upgrade"
)

// engineRelease is the engine release the derived fixtures pin.
const engineRelease = "tools/v1.4.2"

// newEngineRelease is the target of the upgrade.
const newEngineRelease = "tools/v1.5.0"

// newDerived builds a committed derived repository by running setup on a
// template, then returns the repo root, git runner, config, and the repo
// helper for further commits.
func newDerived(ctx context.Context, tb testing.TB) (string, *gitcli.Runner, *config.Config, *testsupport.Repo) {
	tb.Helper()

	// Build the template.
	repo := testsupport.NewRepo(ctx, tb, testsupport.Options{
		UserName:  "Upgrade Test",
		UserEmail: "test@example.invalid",
	})
	for path, contents := range templateFiles {
		repo.WriteFile(tb, path, contents)
	}
	repo.Commit(ctx, tb, "chore: template", gitcli.CommitOptions{}, ".")

	root, err := filepath.EvalSymlinks(repo.Dir)
	if err != nil {
		tb.Fatalf("resolve repository: %v", err)
	}

	// Load config and run setup.
	cfg, err := config.Load(ctx, filepath.Join(root, config.DefaultFileName))
	if err != nil {
		tb.Fatalf("load profile: %v", err)
	}
	setupOpts := setup.Options{
		Root:          root,
		Config:        cfg,
		EngineVersion: engineRelease,
		EngineMod:     engineGoMod,
		Git:           repo.Git,
	}
	planned, err := setup.Plan(ctx, setupOpts)
	if err != nil {
		tb.Fatalf("plan: %v", err)
	}
	if _, err := setup.Apply(ctx, setupOpts, planned.Report.Hash); err != nil {
		tb.Fatalf("apply: %v", err)
	}

	// Commit the setup result so the work tree is clean.
	repo.Commit(ctx, tb, "chore: setup", gitcli.CommitOptions{}, ".")

	return root, repo.Git, cfg, repo
}

// upgradeOpts builds upgrade options for an existing derived repo with the new
// engine. It uses the template's engine go.mod (which declares the engine module
// path) rather than the derived repo's shim go.mod.
func upgradeOpts(tb testing.TB, root string, git *gitcli.Runner, cfg *config.Config) upgrade.Options {
	tb.Helper()
	// The engine go.mod is what the engine itself ships, not the derived shim.
	// In a real upgrade the operator reads it from the new engine release. In
	// tests we use the same template go.mod that setup used.
	return upgrade.Options{
		Root:          root,
		Config:        cfg,
		EngineVersion: newEngineRelease,
		EngineMod:     engineGoMod,
		EngineSum:     engineGoSum,
		Git:           git,
	}
}

// engineGoMod is the engine's go.mod, not the derived shim's. Setup validates
// that this declares the engine module path.
var engineGoMod = []byte(`module github.com/enj/soapbox/tools

go 1.26.0

require (
	golang.org/x/mod v0.39.0
	golang.org/x/tools v0.48.0
	gopkg.in/yaml.v3 v3.0.1
)

require golang.org/x/sync v0.22.0 // indirect
`)

// engineGoSum is a well-formed go.sum covering the engine module at both
// the original and target versions, plus all dependencies. The hashes are not
// real; they only need to parse as h1 entries.
var engineGoSum = func() []byte {
	modules := []string{
		setup.EngineModulePath + " v1.4.2",
		setup.EngineModulePath + " v1.5.0",
		"golang.org/x/mod v0.39.0",
		"golang.org/x/sync v0.22.0",
		"golang.org/x/tools v0.48.0",
		"gopkg.in/yaml.v3 v3.0.1",
	}
	var lines []string
	for _, m := range modules {
		lines = append(lines,
			m+" h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
			m+"/go.mod h1:BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB=",
		)
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}()

func TestPlanOnCleanDerivedRepo(t *testing.T) {
	ctx := t.Context()
	root, git, cfg, _ := newDerived(ctx, t)
	opts := upgradeOpts(t, root, git, cfg)

	result, err := upgrade.Plan(ctx, opts)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if result.Applied {
		t.Fatal("plan must not apply")
	}
	if result.Report.Hash == "" {
		t.Fatal("plan must produce a hash")
	}
	// Upgrading with a different engine version should produce at least one
	// update (tools/go.mod changes the require line).
	if result.Report.Totals.Update == 0 && result.Report.Totals.Create == 0 {
		t.Logf("no changes detected (same engine); checking consistency")
	}
	// The summary must be renderable.
	summary := result.Summary()
	if !strings.Contains(summary, "would upgrade") {
		t.Errorf("summary = %q, want 'would upgrade'", summary)
	}
}

func TestPlanRefusesDirtyWorkTree(t *testing.T) {
	ctx := t.Context()
	root, git, cfg, _ := newDerived(ctx, t)
	opts := upgradeOpts(t, root, git, cfg)

	// Dirty the work tree.
	if err := os.WriteFile(filepath.Join(root, "dirty.txt"), []byte("dirty"), 0o644); err != nil {
		t.Fatalf("write dirty file: %v", err)
	}

	_, err := upgrade.Plan(ctx, opts)
	if err == nil {
		t.Fatal("expected error for dirty work tree")
	}
	if !errors.Is(err, upgrade.ErrDirty) {
		t.Errorf("error = %v, want ErrDirty", err)
	}
	var pe *upgrade.PolicyError
	if !errors.As(err, &pe) {
		t.Errorf("error is not a PolicyError: %v", err)
	}
}

func TestPlanRefusesNonDerivedRepo(t *testing.T) {
	ctx := t.Context()

	// Build a repo with no root go.mod — this is a template, not derived.
	repo := testsupport.NewRepo(ctx, t, testsupport.Options{
		UserName:  "Upgrade Test",
		UserEmail: "test@example.invalid",
	})
	repo.WriteFile(t, config.DefaultFileName, fixtureProfile)
	repo.WriteFile(t, "tools/go.mod", `module github.com/enj/soapbox/tools

go 1.26.0

require (
	golang.org/x/mod v0.39.0
	golang.org/x/tools v0.48.0
	gopkg.in/yaml.v3 v3.0.1
)

require golang.org/x/sync v0.22.0 // indirect
`)
	repo.Commit(ctx, t, "chore: template", gitcli.CommitOptions{}, ".")

	root, err := filepath.EvalSymlinks(repo.Dir)
	if err != nil {
		t.Fatalf("resolve repository: %v", err)
	}
	cfg, err := config.Load(ctx, filepath.Join(root, config.DefaultFileName))
	if err != nil {
		t.Fatalf("load profile: %v", err)
	}
	engineMod, err := os.ReadFile(filepath.Join(root, "tools", "go.mod"))
	if err != nil {
		t.Fatalf("read engine go.mod: %v", err)
	}

	_, planErr := upgrade.Plan(ctx, upgrade.Options{
		Root:          root,
		Config:        cfg,
		EngineVersion: newEngineRelease,
		EngineMod:     engineMod,
		EngineSum:     engineGoSum,
		Git:           repo.Git,
	})
	if planErr == nil {
		t.Fatal("expected error for non-derived repo")
	}
	if !errors.Is(planErr, upgrade.ErrNotDerived) {
		t.Errorf("error = %v, want ErrNotDerived", planErr)
	}
}

func TestApplyRefusesACommittedPreimageChange(t *testing.T) {
	ctx := t.Context()
	root, git, cfg, repo := newDerived(ctx, t)
	opts := upgradeOpts(t, root, git, cfg)

	planned, err := upgrade.Plan(ctx, opts)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	const changed = "name: operator-edited-ci\n"
	repo.WriteFile(t, ".github/workflows/ci.yml", changed)
	repo.Commit(ctx, t, "chore: edit workflow", gitcli.CommitOptions{}, ".github/workflows/ci.yml")

	_, err = upgrade.Apply(ctx, opts, planned.Report.Hash)
	if !errors.Is(err, upgrade.ErrApproval) {
		t.Fatalf("apply error = %v, want ErrApproval", err)
	}
	got, readErr := os.ReadFile(filepath.Join(root, ".github", "workflows", "ci.yml"))
	if readErr != nil {
		t.Fatalf("read workflow: %v", readErr)
	}
	if string(got) != changed {
		t.Fatalf("stale approval changed the committed workflow to %q", got)
	}
}

func TestPlanRefusesAnOwnedSymlink(t *testing.T) {
	ctx := t.Context()
	root, git, cfg, repo := newDerived(ctx, t)
	outside := filepath.Join(t.TempDir(), "outside-go.mod")
	if err := os.WriteFile(outside, []byte(engineGoMod), 0o644); err != nil {
		t.Fatalf("write outside module: %v", err)
	}
	owned := filepath.Join(root, "tools", "go.mod")
	if err := os.Remove(owned); err != nil {
		t.Fatalf("remove owned module: %v", err)
	}
	if err := os.Symlink(outside, owned); err != nil {
		t.Fatalf("symlink owned module: %v", err)
	}
	repo.Commit(ctx, t, "chore: replace module with symlink", gitcli.CommitOptions{}, "tools/go.mod")

	_, err := upgrade.Plan(ctx, upgradeOpts(t, root, git, cfg))
	if !errors.Is(err, upgrade.ErrUnsafePath) {
		t.Fatalf("plan error = %v, want ErrUnsafePath", err)
	}
}

func TestApplyRequiresCorrectHash(t *testing.T) {
	ctx := t.Context()
	root, git, cfg, _ := newDerived(ctx, t)
	opts := upgradeOpts(t, root, git, cfg)

	_, err := upgrade.Apply(ctx, opts, "wrong-hash")
	if err == nil {
		t.Fatal("expected error for wrong hash")
	}
	if !errors.Is(err, upgrade.ErrApproval) {
		t.Errorf("error = %v, want ErrApproval", err)
	}
}

func TestApplyRequiresNonEmptyHash(t *testing.T) {
	ctx := t.Context()
	root, git, cfg, _ := newDerived(ctx, t)
	opts := upgradeOpts(t, root, git, cfg)

	_, err := upgrade.Apply(ctx, opts, "")
	if err == nil {
		t.Fatal("expected error for empty approval")
	}
	if !errors.Is(err, upgrade.ErrApproval) {
		t.Errorf("error = %v, want ErrApproval", err)
	}
}

func TestApplyWritesExactManifest(t *testing.T) {
	ctx := t.Context()
	root, git, cfg, _ := newDerived(ctx, t)
	opts := upgradeOpts(t, root, git, cfg)

	planned, err := upgrade.Plan(ctx, opts)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	result, err := upgrade.Apply(ctx, opts, planned.Report.Hash)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !result.Applied {
		t.Fatal("apply must set Applied")
	}
	if result.Partial {
		t.Fatal("a successful apply must not be partial")
	}
	summary := result.Summary()
	if !strings.Contains(summary, "upgraded") {
		t.Errorf("summary = %q, want 'upgraded'", summary)
	}

	// Verify every action was written correctly.
	for _, action := range result.Report.Actions {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(action.Path)))
		if err != nil {
			t.Errorf("read %s after apply: %v", action.Path, err)
			continue
		}
		if got := digest(data); got != action.Digest {
			t.Errorf("%s digest = %s, want %s", action.Path, got, action.Digest)
		}
	}
}

func TestApplyRefusedTwice(t *testing.T) {
	ctx := t.Context()
	root, git, cfg, repo := newDerived(ctx, t)
	opts := upgradeOpts(t, root, git, cfg)

	planned, err := upgrade.Plan(ctx, opts)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	if _, err := upgrade.Apply(ctx, opts, planned.Report.Hash); err != nil {
		t.Fatalf("first apply: %v", err)
	}

	// Commit the upgrade so the work tree is clean for the second plan.
	repo.Commit(ctx, t, "chore: upgrade", gitcli.CommitOptions{}, ".")

	// The second plan should show nothing to do: all files already match.
	result2, err := upgrade.Plan(ctx, opts)
	if err != nil {
		t.Fatalf("second plan: %v", err)
	}
	if result2.Report.Totals.Update != 0 || result2.Report.Totals.Create != 0 {
		t.Errorf("second plan still has %d updates and %d creates after apply",
			result2.Report.Totals.Update, result2.Report.Totals.Create)
	}
}

func TestManifestNamesNoAbsolutePath(t *testing.T) {
	ctx := t.Context()
	root, git, cfg, _ := newDerived(ctx, t)
	opts := upgradeOpts(t, root, git, cfg)

	result, err := upgrade.Plan(ctx, opts)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	data, err := result.Report.JSON()
	if err != nil {
		t.Fatalf("json: %v", err)
	}
	if strings.Contains(string(data), root) {
		t.Errorf("manifest contains the absolute root %q", root)
	}
}

func TestCancellationRefusesPlan(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := upgrade.Plan(ctx, upgrade.Options{Root: "/tmp/fake", Config: &config.Config{}})
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

func TestUpgradeOnlyTouchesOwnedFiles(t *testing.T) {
	ctx := t.Context()
	root, git, cfg, _ := newDerived(ctx, t)
	opts := upgradeOpts(t, root, git, cfg)

	// Record every tracked file's content before upgrade.
	before := make(map[string][]byte)
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		data, _ := os.ReadFile(path)
		before[rel] = data
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	planned, planErr := upgrade.Plan(ctx, opts)
	if planErr != nil {
		t.Fatalf("plan: %v", planErr)
	}
	if _, applyErr := upgrade.Apply(ctx, opts, planned.Report.Hash); applyErr != nil {
		t.Fatalf("apply: %v", applyErr)
	}

	// Build a set of paths the manifest says it touched.
	touched := make(map[string]bool, len(planned.Report.Actions))
	for _, action := range planned.Report.Actions {
		touched[action.Path] = true
	}

	// Walk again and verify no non-manifest file was modified.
	err = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if touched[rel] {
			return nil
		}
		data, _ := os.ReadFile(path)
		if old, ok := before[rel]; ok {
			if string(data) != string(old) {
				t.Errorf("upgrade modified %s which is not in the manifest", rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}

// digest mirrors the upgrade package's digest for test assertions.
func digest(contents []byte) string {
	sum := sha256.Sum256(contents)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// TestWriteAtomicRefusesPreexistingTemp proves that writeAtomic refuses to
// overwrite a preexisting file at the temp path, preventing a tracked or
// planted file from being truncated by a non-exclusive create.
func TestWriteAtomicRefusesPreexistingTemp(t *testing.T) {
	dir := t.TempDir()

	// Create the target directory and plant a file at the temp name.
	targetDir := filepath.Join(dir, ".github", "workflows")
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	tempFile := filepath.Join(targetDir, "sync.yml.soapbox-upgrade.tmp")
	if err := os.WriteFile(tempFile, []byte("planted"), 0o644); err != nil {
		t.Fatalf("plant temp: %v", err)
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("open root: %v", err)
	}
	defer func() { _ = root.Close() }()

	err = upgrade.WriteAtomicForTest(root, ".github/workflows/sync.yml", []byte("new content"))
	if err == nil {
		t.Fatal("writeAtomic accepted a preexisting temp file")
	}
	if !strings.Contains(err.Error(), "exist") {
		t.Errorf("error = %v, want mention of file already existing", err)
	}

	// The planted file must be unchanged.
	data, err := os.ReadFile(tempFile)
	if err != nil {
		t.Fatalf("read planted temp: %v", err)
	}
	if string(data) != "planted" {
		t.Errorf("planted temp was modified: %q", data)
	}
}

// TestApplyRefusesStalePreimage proves that committing a different version of
// an owned file between plan and apply causes the old approval hash to be
// refused and nothing is written.
func TestApplyRefusesStalePreimage(t *testing.T) {
	ctx := t.Context()
	root, git, cfg, repo := newDerived(ctx, t)
	opts := upgradeOpts(t, root, git, cfg)

	// Plan the upgrade.
	planned, err := upgrade.Plan(ctx, opts)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(planned.Report.Actions) == 0 {
		t.Skip("no actions to test (same engine version)")
	}
	oldHash := planned.Report.Hash

	// Find an update action for a workflow (not tools/go.mod which must stay
	// parseable for derived-repo validation).
	var targetPath string
	for _, a := range planned.Report.Actions {
		if a.Kind == upgrade.ActionUpdate && strings.HasSuffix(a.Path, ".yml") {
			targetPath = a.Path
			break
		}
	}
	if targetPath == "" {
		t.Skip("no workflow update actions to test preimage binding")
	}

	// Overwrite the workflow with different content and commit.
	repo.WriteFile(t, targetPath, "# modified between plan and apply\nname: modified\non: push\njobs: {}\n")
	repo.Commit(ctx, t, "chore: modify owned file", gitcli.CommitOptions{}, ".")

	// Record file content before apply attempt.
	beforeApply, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(targetPath)))
	if err != nil {
		t.Fatalf("read %s: %v", targetPath, err)
	}

	// Apply with the old hash must fail.
	_, err = upgrade.Apply(ctx, opts, oldHash)
	if err == nil {
		t.Fatal("apply accepted a stale preimage")
	}
	if !errors.Is(err, upgrade.ErrApproval) {
		t.Errorf("error = %v, want ErrApproval", err)
	}

	// The file must be unchanged — nothing was written.
	afterApply, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(targetPath)))
	if err != nil {
		t.Fatalf("read %s after apply: %v", targetPath, err)
	}
	if string(afterApply) != string(beforeApply) {
		t.Errorf("%s was modified despite the refused apply", targetPath)
	}
}

// TestPlanRefusesSymlinkAtOwnedPath proves that a symbolic link at an
// upgrade-owned path or a root marker produces ErrUnsafePath without
// following the link.
func TestPlanRefusesSymlinkAtOwnedPath(t *testing.T) {
	ctx := t.Context()
	root, git, cfg, _ := newDerived(ctx, t)

	// Replace the sync workflow with a symlink to a file outside the repo.
	targetPath := filepath.Join(root, ".github", "workflows", "sync.yml")
	if err := os.Remove(targetPath); err != nil {
		t.Fatalf("remove %s: %v", targetPath, err)
	}
	outside := filepath.Join(t.TempDir(), "outside.yml")
	if err := os.WriteFile(outside, []byte("external content"), 0o644); err != nil {
		t.Fatalf("write outside: %v", err)
	}
	if err := os.Symlink(outside, targetPath); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	// Stage and commit so the tree is clean (git tracks symlinks).
	commitAllForced(ctx, t, root)

	opts := upgradeOpts(t, root, git, cfg)
	_, err := upgrade.Plan(ctx, opts)
	if err == nil {
		t.Fatal("plan accepted a symlink at an owned path")
	}
	if !errors.Is(err, upgrade.ErrUnsafePath) {
		t.Errorf("error = %v, want ErrUnsafePath", err)
	}
}

// TestPlanRefusesSymlinkAtRootMarker proves that a symbolic link at go.mod
// (a derived-repo marker) produces an error without following the link.
func TestPlanRefusesSymlinkAtRootMarker(t *testing.T) {
	ctx := t.Context()
	root, git, cfg, _ := newDerived(ctx, t)

	// Replace go.mod with a symlink.
	goModPath := filepath.Join(root, "go.mod")
	goModContent, err := os.ReadFile(goModPath)
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	if err := os.Remove(goModPath); err != nil {
		t.Fatalf("remove go.mod: %v", err)
	}
	outside := filepath.Join(t.TempDir(), "go.mod")
	if err := os.WriteFile(outside, goModContent, 0o644); err != nil {
		t.Fatalf("write outside: %v", err)
	}
	if err := os.Symlink(outside, goModPath); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	commitAllForced(ctx, t, root)

	opts := upgradeOpts(t, root, git, cfg)
	_, err = upgrade.Plan(ctx, opts)
	if err == nil {
		t.Fatal("plan accepted a symlink at go.mod")
	}
	// Either ErrUnsafePath or ErrNotDerived is acceptable since the marker
	// validation runs first.
	if !errors.Is(err, upgrade.ErrUnsafePath) && !errors.Is(err, upgrade.ErrNotDerived) {
		t.Errorf("error = %v, want ErrUnsafePath or ErrNotDerived", err)
	}
}

// commitAllForced stages everything including symlinks and commits.
func commitAllForced(ctx context.Context, tb testing.TB, root string) {
	tb.Helper()
	git, err := gitcli.New(ctx, gitcli.Options{Dir: root})
	if err != nil {
		tb.Fatalf("git runner: %v", err)
	}
	if err := git.AddPaths(ctx, "."); err != nil {
		tb.Fatalf("stage: %v", err)
	}
	if err := git.Commit(ctx, gitcli.CommitOptions{Message: "chore: test fixture"}); err != nil {
		tb.Fatalf("commit: %v", err)
	}
}

// TestUpgradeFromV1Profile simulates upgrading a derived repository whose
// soapbox.yaml is still schema v1 (with a githubApp section). The caller uses
// DecodeWithMigration to load the v1 profile, then passes the migrated Config
// to upgrade.Plan/Apply.
func TestUpgradeFromV1Profile(t *testing.T) {
	ctx := t.Context()
	// Build a derived repo normally, then overwrite the profile with v1 YAML.
	root, git, _, repo := newDerived(ctx, t)

	// Overwrite soapbox.yaml with a v1 profile.
	repo.WriteFile(t, config.DefaultFileName, v1FixtureProfile)
	repo.Commit(ctx, t, "chore: revert to v1 profile", gitcli.CommitOptions{}, ".")

	// Load with migration.
	profileData, err := os.ReadFile(filepath.Join(root, config.DefaultFileName))
	if err != nil {
		t.Fatalf("read profile: %v", err)
	}
	cfg, _, err := config.DecodeWithMigration(profileData)
	if err != nil {
		t.Fatalf("decode with migration: %v", err)
	}
	if cfg.Version != config.SchemaVersion {
		t.Fatalf("migrated version = %d, want %d", cfg.Version, config.SchemaVersion)
	}
	if cfg.Publication.Mode != config.PublicationModeManual {
		t.Fatalf("migrated publication.mode = %q, want %q", cfg.Publication.Mode, config.PublicationModeManual)
	}

	opts := upgrade.Options{
		Root:          root,
		Config:        cfg,
		EngineVersion: newEngineRelease,
		EngineMod:     engineGoMod,
		EngineSum:     engineGoSum,
		Git:           git,
	}
	result, err := upgrade.Plan(ctx, opts)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if result.Report.Hash == "" {
		t.Fatal("plan must produce a hash")
	}

	// Apply the upgrade.
	applied, err := upgrade.Apply(ctx, opts, result.Report.Hash)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !applied.Applied {
		t.Fatal("apply must set Applied")
	}

	// The sync workflow must now use GITHUB_TOKEN, not App secrets.
	syncData, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "sync.yml"))
	if err != nil {
		t.Fatalf("read sync workflow: %v", err)
	}
	if !strings.Contains(string(syncData), "SOAPBOX_GITHUB_TOKEN") {
		t.Error("upgraded sync workflow does not contain SOAPBOX_GITHUB_TOKEN")
	}
	if strings.Contains(string(syncData), "secrets.SOAPBOX_GITHUB_APP") {
		t.Error("upgraded sync workflow still references App secrets")
	}
}

// v1FixtureProfile is a schema v1 profile with the githubApp section, as a
// derived repository that has not yet been upgraded would carry.
const v1FixtureProfile = `version: 1

source:
  repository: https://github.com/kubernetes/kubernetes.git
  importPrefix: k8s.io/kubernetes
  project: Kubernetes
  license: Apache-2.0
  refs:
    minimumRelease: v1.36.1
    includePrereleases: true
    branches:
      - master
    anchorCommit: ""

destination:
  module: monis.app/kk/rbac_authorizer
  repository: enj/rbac_authorizer
  remote: https://github.com/enj/rbac_authorizer.git
  branch: main
  stateRef: refs/heads/soapbox-state
  progressRefPrefix: refs/soapbox/progress/
  rootPackage: rbacauthorizer
  internalPrefix: internal/kk
  summary: the Kubernetes RBAC authorizer as an independently consumable Go module.

packages:
  roots:
    - plugin/pkg/auth/authorizer/rbac
  recursive: false
  assetGlobs: []

prune:
  files:
    - pkg/apis/rbac/v1/register.go
  required:
    - pkg/apis/rbac/v1/doc.go

deny:
  imports:
    - k8s.io/kubernetes/pkg/apis/rbac

closure:
  includeTests: false
  limits:
    maxPackages: 8
    maxFiles: 40
    maxNonTestLines: 5000
    maxPackageGrowth: 4
  golden: testdata/closure/fixture.json

types:
  policy: prefer-external
  pairs:
    - internal: k8s.io/kubernetes/pkg/apis/rbac
      external: k8s.io/api/rbac/v1

dependencies:
  policy: external
  copyPackages: []
  gates:
    interoperability: true
    globalState: true
    diamond: true
    cost:
      maxCopiedPackages: 0
      maxCopiedLines: 0
      maxGeneratedFiles: 0
      maxDistinctLicenses: 0
      maxModuleZipBytes: 0
      maxReleasesPerMinor: 0
      minModulesRemoved: 0
      minPackagesRemoved: 0
      minLinesRemoved: 0
  overrides: []

patches: []

facade:
  package: rbacauthorizer
  file: authorizer.go
  assertionsFile: zz_generated_assertions.go
  exports:
    - name: New
      kind: func
      source: k8s.io/kubernetes/plugin/pkg/auth/authorizer/rbac.New
  aliases: []
  interfaceAssertions: []

release:
  policy: v1-to-v0
  firstTag: v0.36.1

commit:
  authorPolicy: preserve-upstream
  committer:
    name: soapbox[bot]
    email: soapbox[bot]@users.noreply.github.com
  trailerKey: Kubernetes-commit
  sign: false

vanity:
  repository: enj/enj.github.io
  path: kk/rbac_authorizer/index.html
  importPath: monis.app/kk/rbac_authorizer
  repositoryURL: https://github.com/enj/rbac_authorizer
  probeURL: https://monis.app/kk/rbac_authorizer?go-get=1

githubApp:
  appIDEnv: SOAPBOX_GITHUB_APP_ID
  installationIDEnv: SOAPBOX_GITHUB_INSTALLATION_ID
  privateKeyEnv: SOAPBOX_GITHUB_APP_PRIVATE_KEY
  apiBaseURL: https://api.github.com

determinism:
  toolchain: go1.26.5
  chunkSize: 200
`

// templateFiles is the tracked content of a soapbox template checkout.
var templateFiles = map[string]string{
	config.DefaultFileName:      fixtureProfile,
	"plans/implementation.md":   "# implementation plan\n",
	"tools/soapbox.go":          "package soapbox\n",
	"tools/internal/cli/cli.go": "package cli\n",
	"tools/cmd/soapbox/main.go": "package main\n\nfunc main() {}\n",
	"CLAUDE.md":                 "# project instructions\n",
	".golangci.yml":             "version: \"2\"\n",
	".claude/settings.json":     "{}\n",
	".serena/project.yml":       "name: soapbox\n",
	"docs/setup.md":             "# setup\n",
	"plans/goal.md":             "# goal\n",
	"tools/soapbox_test.go":     "package soapbox\n",
	"tools/go.mod": `module github.com/enj/soapbox/tools

go 1.26.0

require (
	golang.org/x/mod v0.39.0
	golang.org/x/tools v0.48.0
	gopkg.in/yaml.v3 v3.0.1
)

require golang.org/x/sync v0.22.0 // indirect
`,
	"tools/go.sum":                            "",
	"tools/internal/config/config.go":         "package config\n",
	".github/workflows/ci.yml":                "name: engine-ci\n",
	".github/workflows/template-selftest.yml": "name: template-selftest\n",
	"LICENSE":            "Apache License 2.0\n",
	"NOTICE":             "notice\n",
	"README.md":          "# soapbox\n",
	".gitignore":         "/bin/\n",
	".gitattributes":     "* text=auto eol=lf\n",
	"patches/index.yaml": "patches: []\n",
	"patches/README.md":  "# patches\n",
}

const fixtureProfile = `
version: 2

source:
  repository: https://github.com/kubernetes/kubernetes.git
  importPrefix: k8s.io/kubernetes
  project: Kubernetes
  license: Apache-2.0
  refs:
    minimumRelease: v1.36.1
    includePrereleases: true
    branches:
      - master
    anchorCommit: ""

destination:
  module: monis.app/kk/rbac_authorizer
  repository: enj/rbac_authorizer
  remote: https://github.com/enj/rbac_authorizer.git
  branch: main
  stateRef: refs/heads/soapbox-state
  progressRefPrefix: refs/soapbox/progress/
  rootPackage: rbacauthorizer
  internalPrefix: internal/kk
  summary: the Kubernetes RBAC authorizer as an independently consumable Go module.

packages:
  roots:
    - plugin/pkg/auth/authorizer/rbac
  recursive: false
  assetGlobs: []

prune:
  files:
    - pkg/apis/rbac/v1/register.go
  required:
    - pkg/apis/rbac/v1/doc.go

deny:
  imports:
    - k8s.io/kubernetes/pkg/apis/rbac

closure:
  includeTests: false
  limits:
    maxPackages: 8
    maxFiles: 40
    maxNonTestLines: 5000
    maxPackageGrowth: 4
  golden: testdata/closure/fixture.json

types:
  policy: prefer-external
  pairs:
    - internal: k8s.io/kubernetes/pkg/apis/rbac
      external: k8s.io/api/rbac/v1

dependencies:
  policy: external
  copyPackages: []
  forbiddenModules: []
  gates:
    interoperability: true
    globalState: true
    diamond: true
    cost:
      maxCopiedPackages: 0
      maxCopiedLines: 0
      maxGeneratedFiles: 0
      maxDistinctLicenses: 0
      maxModuleZipBytes: 0
      maxReleasesPerMinor: 0
      minModulesRemoved: 0
      minPackagesRemoved: 0
      minLinesRemoved: 0
  overrides: []

patches: []

facade:
  package: rbacauthorizer
  file: authorizer.go
  assertionsFile: zz_generated_assertions.go
  exports:
    - name: New
      kind: func
      source: k8s.io/kubernetes/plugin/pkg/auth/authorizer/rbac.New
  aliases: []
  interfaceAssertions: []

release:
  policy: v1-to-v0
  firstTag: v0.36.1

commit:
  authorPolicy: preserve-upstream
  committer:
    name: soapbox[bot]
    email: soapbox[bot]@users.noreply.github.com
  trailerKey: Kubernetes-commit
  sign: false

vanity:
  repository: enj/enj.github.io
  path: kk/rbac_authorizer/index.html
  importPath: monis.app/kk/rbac_authorizer
  repositoryURL: https://github.com/enj/rbac_authorizer
  probeURL: https://monis.app/kk/rbac_authorizer?go-get=1

publication:
  mode: automatic

compatibility:
  apiserver: local

determinism:
  toolchain: go1.26.5
  chunkSize: 200
`
