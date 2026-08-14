package cli_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/cli"
	"github.com/enj/soapbox/tools/internal/config"
	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/setup"
	"github.com/enj/soapbox/tools/internal/testsupport"
	"github.com/enj/soapbox/tools/internal/upgrade"
)

// TestRunUpgradeUsage covers every command line the upgrade command refuses.
func TestRunUpgradeUsage(t *testing.T) {
	dir := t.TempDir()

	tests := []struct {
		name       string
		args       []string
		wantStderr string
	}{
		{
			name:       "no engine version",
			args:       []string{"upgrade", "-dir", dir, "-engine-mod", "/fake/go.mod", "-engine-sum", "/fake/go.sum"},
			wantStderr: "-engine-version",
		},
		{
			name:       "no engine mod",
			args:       []string{"upgrade", "-dir", dir, "-engine-version", "v1.5.0", "-engine-sum", "/fake/go.sum"},
			wantStderr: "-engine-mod",
		},
		{
			name:       "no engine sum",
			args:       []string{"upgrade", "-dir", dir, "-engine-version", "v1.5.0", "-engine-mod", "/fake/go.mod"},
			wantStderr: "-engine-sum",
		},
		{
			name:       "unsupported format",
			args:       []string{"upgrade", "-dir", dir, "-engine-version", "v1.5.0", "-engine-mod", "/fake/go.mod", "-engine-sum", "/fake/go.sum", "-format", "yaml"},
			wantStderr: `unsupported -format "yaml"`,
		},
		{
			name:       "apply without approve",
			args:       []string{"upgrade", "-dir", dir, "-engine-version", "v1.5.0", "-engine-mod", "/fake/go.mod", "-engine-sum", "/fake/go.sum", "-apply"},
			wantStderr: "-approve",
		},
		{
			name:       "approve without apply",
			args:       []string{"upgrade", "-dir", dir, "-engine-version", "v1.5.0", "-engine-mod", "/fake/go.mod", "-engine-sum", "/fake/go.sum", "-approve", "abc"},
			wantStderr: "meaningless without -apply",
		},
		{
			name:       "unexpected operand",
			args:       []string{"upgrade", "-dir", dir, "-engine-version", "v1.5.0", "-engine-mod", "/fake/go.mod", "-engine-sum", "/fake/go.sum", "extra"},
			wantStderr: "takes no arguments",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stderr bytes.Buffer
			code := cli.Run(t.Context(), cli.Env{Stderr: &stderr, Dir: dir}, test.args)
			if code != cli.ExitUsage {
				t.Errorf("exit code = %d, want %d; stderr: %s", code, cli.ExitUsage, stderr.String())
			}
			if !strings.Contains(stderr.String(), test.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), test.wantStderr)
			}
		})
	}
}

// TestRunUpgradeHelp verifies the help output lists the required flags.
func TestRunUpgradeHelp(t *testing.T) {
	var stdout bytes.Buffer
	code := cli.Run(t.Context(), cli.Env{Stdout: &stdout}, []string{"help", "upgrade"})
	if code != cli.ExitOK {
		t.Fatalf("exit code = %d", code)
	}
	for _, flag := range []string{"-engine-version", "-engine-mod", "-engine-sum", "-apply", "-approve", "-report", "-config"} {
		if !strings.Contains(stdout.String(), flag) {
			t.Errorf("help does not describe %s:\n%s", flag, stdout.String())
		}
	}
}

// TestRunUpgradeDryRunThenApply exercises the full CLI contract: a dry run
// reports changes without writing, and the hash it produced lets a second run
// apply.
func TestRunUpgradeDryRunThenApply(t *testing.T) {
	ctx := t.Context()
	root, engineModPath, engineSumPath := buildDerivedForCLI(ctx, t)
	reportPath := filepath.Join(t.TempDir(), "manifest.json")

	// Dry run.
	var stdout, stderr bytes.Buffer
	code := cli.Run(ctx, cli.Env{Stdout: &stdout, Stderr: &stderr, Dir: root}, []string{
		"upgrade",
		"-engine-version", "tools/v1.5.0",
		"-engine-mod", engineModPath,
		"-engine-sum", engineSumPath,
		"-format", "json",
		"-report", reportPath,
	})
	if code != cli.ExitOK {
		t.Fatalf("dry run exit code = %d, stderr: %s", code, stderr.String())
	}

	// Parse the JSON manifest.
	var manifest upgrade.Report
	if err := json.Unmarshal(stdout.Bytes(), &manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if manifest.Hash == "" {
		t.Fatal("manifest has no hash")
	}

	// The report file must have been written.
	if _, err := os.Stat(reportPath); err != nil {
		t.Fatalf("report not written: %v", err)
	}

	// Root go.mod must NOT be in the actions — upgrade does not own it.
	for _, action := range manifest.Actions {
		if action.Path == "go.mod" {
			t.Error("manifest includes root go.mod, which upgrade must not overwrite")
		}
	}

	// Wrong hash is a finding.
	t.Run("wrong hash", func(t *testing.T) {
		var stderr bytes.Buffer
		code := cli.Run(ctx, cli.Env{Stderr: &stderr, Dir: root}, []string{
			"upgrade",
			"-engine-version", "tools/v1.5.0",
			"-engine-mod", engineModPath,
			"-engine-sum", engineSumPath,
			"-apply", "-approve", strings.Repeat("0", 64),
		})
		if code != cli.ExitCheck {
			t.Fatalf("exit code = %d, want %d; stderr: %s", code, cli.ExitCheck, stderr.String())
		}
	})

	// Correct hash applies.
	t.Run("correct hash applies", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := cli.Run(ctx, cli.Env{Stdout: &stdout, Stderr: &stderr, Dir: root}, []string{
			"upgrade",
			"-engine-version", "tools/v1.5.0",
			"-engine-mod", engineModPath,
			"-engine-sum", engineSumPath,
			"-apply", "-approve", manifest.Hash,
		})
		if code != cli.ExitOK {
			t.Fatalf("apply exit code = %d, stderr: %s", code, stderr.String())
		}
		// Verify the shim go.mod was written.
		shimMod, err := os.ReadFile(filepath.Join(root, "tools", "go.mod"))
		if err != nil {
			t.Fatalf("read tools/go.mod: %v", err)
		}
		if !strings.Contains(string(shimMod), "v1.5.0") {
			t.Errorf("tools/go.mod does not pin v1.5.0:\n%s", shimMod)
		}
	})
}

// buildDerivedForCLI creates a template, runs setup via CLI, commits the
// result, and writes the engine go.mod and go.sum to temp files for use as
// -engine-mod and -engine-sum flags. Returns the repo root and the two paths.
func buildDerivedForCLI(ctx context.Context, tb testing.TB) (root, engineModPath, engineSumPath string) {
	tb.Helper()

	repo := testsupport.NewRepo(ctx, tb, testsupport.Options{
		UserName:  "Upgrade CLI Test",
		UserEmail: "test@example.invalid",
	})
	for path, contents := range map[string]string{
		config.DefaultFileName:      planProfile,
		"plans/implementation.md":   "# plan\n",
		"tools/soapbox.go":          "package soapbox\n",
		"tools/internal/cli/cli.go": "package cli\n",
		"tools/cmd/soapbox/main.go": "package main\n",
		"tools/go.mod":              engineGoModContent,
		"CLAUDE.md":                 "# instructions\n",
		"README.md":                 "# fixture\n",
	} {
		repo.WriteFile(tb, path, contents)
	}
	repo.Commit(ctx, tb, "chore: template", gitcli.CommitOptions{}, ".")

	root, err := filepath.EvalSymlinks(repo.Dir)
	if err != nil {
		tb.Fatalf("resolve: %v", err)
	}

	// Run setup to create the derived repo.
	var stdout, stderr bytes.Buffer
	code := cli.Run(ctx, cli.Env{Stdout: &stdout, Stderr: &stderr, Dir: root},
		[]string{"setup", "-engine-version", "tools/v0.3.0"})
	if code != cli.ExitOK {
		tb.Fatalf("setup dry run exit %d, stderr: %s", code, stderr.String())
	}

	// Parse hash from manifest.
	var manifest struct{ Hash string }
	// The output is summary format; find the hash in it.
	for _, line := range strings.Split(stdout.String(), "\n") {
		if strings.Contains(line, "manifest") && !strings.Contains(line, "nothing") {
			fields := strings.Fields(line)
			manifest.Hash = fields[len(fields)-1]
		}
	}
	if manifest.Hash == "" {
		tb.Fatalf("could not find hash in setup output:\n%s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = cli.Run(ctx, cli.Env{Stdout: &stdout, Stderr: &stderr, Dir: root},
		[]string{"setup", "-engine-version", "tools/v0.3.0", "-apply", "-approve", manifest.Hash})
	if code != cli.ExitOK {
		tb.Fatalf("setup apply exit %d, stderr: %s", code, stderr.String())
	}

	repo.Commit(ctx, tb, "chore: setup", gitcli.CommitOptions{}, ".")

	// Write engine go.mod and go.sum to temp files for -engine-mod/-engine-sum.
	tmpDir := tb.TempDir()
	engineModPath = filepath.Join(tmpDir, "engine-go.mod")
	if err := os.WriteFile(engineModPath, []byte(engineGoModContent), 0o644); err != nil {
		tb.Fatalf("write engine go.mod: %v", err)
	}
	engineSumPath = filepath.Join(tmpDir, "engine-go.sum")
	if err := os.WriteFile(engineSumPath, []byte(engineGoSumContent), 0o644); err != nil {
		tb.Fatalf("write engine go.sum: %v", err)
	}
	return root, engineModPath, engineSumPath
}

// commitAll stages and commits all changes in the repo at the given root.
func commitAll(ctx context.Context, tb testing.TB, root string) {
	tb.Helper()
	git, err := gitcli.New(ctx, gitcli.Options{Dir: root})
	if err != nil {
		tb.Fatalf("git runner: %v", err)
	}
	if err := git.AddPaths(ctx, "."); err != nil {
		tb.Fatalf("stage: %v", err)
	}
	if err := git.Commit(ctx, gitcli.CommitOptions{Message: "chore: update"}); err != nil {
		tb.Fatalf("commit: %v", err)
	}
}

// engineGoModContent is the engine's go.mod. It declares the engine module
// path — not the derived shim path.
const engineGoModContent = `module github.com/enj/soapbox/tools

go 1.26.0

require (
	golang.org/x/mod v0.39.0
	golang.org/x/tools v0.48.0
	gopkg.in/yaml.v3 v3.0.1
)

require golang.org/x/sync v0.22.0 // indirect
`

// engineGoSumContent covers the engine and its requirements.
var engineGoSumContent = strings.Join([]string{
	setup.EngineModulePath + " v1.5.0 h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
	setup.EngineModulePath + " v1.5.0/go.mod h1:BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB=",
	"golang.org/x/mod v0.39.0 h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
	"golang.org/x/mod v0.39.0/go.mod h1:BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB=",
	"golang.org/x/sync v0.22.0 h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
	"golang.org/x/sync v0.22.0/go.mod h1:BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB=",
	"golang.org/x/tools v0.48.0 h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
	"golang.org/x/tools v0.48.0/go.mod h1:BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB=",
	"gopkg.in/yaml.v3 v3.0.1 h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
	"gopkg.in/yaml.v3 v3.0.1/go.mod h1:BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB=",
}, "\n") + "\n"

// TestRunUpgradeV1ApplyThenFixedPoint applies a v1 migration, verifies the
// profile is v2 with no githubApp, then replans and proves zero actions.
func TestRunUpgradeV1ApplyThenFixedPoint(t *testing.T) {
	ctx := t.Context()
	root, engineModPath, engineSumPath := buildDerivedForCLI(ctx, t)

	// Overwrite profile with v1 schema.
	v1 := strings.Replace(planProfile, "version: 2\n", "version: 1\n", 1)
	v1 = strings.Replace(v1, "publication:\n  mode: automatic\ncompatibility:\n  apiserver: external\n",
		"githubApp:\n  appIDEnv: SOAPBOX_APP_ID\n  installationIDEnv: SOAPBOX_INSTALL_ID\n  privateKeyEnv: SOAPBOX_KEY\n  apiBaseURL: https://api.github.com\n", 1)
	v1 = strings.Replace(v1, "  forbiddenModules: []\n", "", 1)
	if err := os.WriteFile(filepath.Join(root, config.DefaultFileName), []byte(v1), 0o644); err != nil {
		t.Fatalf("write v1 profile: %v", err)
	}
	commitAll(ctx, t, root)

	// Plan.
	var stdout bytes.Buffer
	code := cli.Run(ctx, cli.Env{Stdout: &stdout, Dir: root}, []string{
		"upgrade", "-engine-version", "tools/v1.5.0",
		"-engine-mod", engineModPath, "-engine-sum", engineSumPath,
		"-format", "json",
	})
	if code != cli.ExitOK {
		t.Fatalf("plan exit %d, output: %s", code, stdout.String())
	}
	var manifest upgrade.Report
	if err := json.Unmarshal(stdout.Bytes(), &manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}

	// soapbox.yaml must be in the manifest.
	yamlInManifest := false
	for _, a := range manifest.Actions {
		if a.Path == config.DefaultFileName {
			yamlInManifest = true
		}
	}
	if !yamlInManifest {
		t.Fatal("manifest does not include soapbox.yaml for v1 migration")
	}

	// Apply.
	stdout.Reset()
	code = cli.Run(ctx, cli.Env{Stdout: &stdout, Dir: root}, []string{
		"upgrade", "-engine-version", "tools/v1.5.0",
		"-engine-mod", engineModPath, "-engine-sum", engineSumPath,
		"-apply", "-approve", manifest.Hash,
	})
	if code != cli.ExitOK {
		t.Fatalf("apply exit %d, output: %s", code, stdout.String())
	}

	// Verify the written profile is v2 with no githubApp.
	profileData, err := os.ReadFile(filepath.Join(root, config.DefaultFileName))
	if err != nil {
		t.Fatalf("read profile: %v", err)
	}
	if strings.Contains(string(profileData), "githubApp") {
		t.Error("applied profile still contains githubApp")
	}
	cfg, err := config.Decode(profileData)
	if err != nil {
		t.Fatalf("decode applied profile: %v", err)
	}
	if cfg.Version != config.SchemaVersion {
		t.Errorf("applied profile version = %d, want %d", cfg.Version, config.SchemaVersion)
	}
	if cfg.Publication.Mode != config.PublicationModeManual {
		t.Errorf("publication.mode = %q, want %q", cfg.Publication.Mode, config.PublicationModeManual)
	}

	// Verify the exact digest matches the manifest.
	for _, a := range manifest.Actions {
		if a.Path != config.DefaultFileName {
			continue
		}
		got := digestBytes(profileData)
		if got != a.Digest {
			t.Errorf("applied soapbox.yaml digest %s, manifest approved %s", got, a.Digest)
		}
	}

	// Commit and replan — should show zero actions (fixed point).
	commitAll(ctx, t, root)
	stdout.Reset()
	code = cli.Run(ctx, cli.Env{Stdout: &stdout, Dir: root}, []string{
		"upgrade", "-engine-version", "tools/v1.5.0",
		"-engine-mod", engineModPath, "-engine-sum", engineSumPath,
		"-format", "json",
	})
	if code != cli.ExitOK {
		t.Fatalf("replan exit %d, output: %s", code, stdout.String())
	}
	var replan upgrade.Report
	if err := json.Unmarshal(stdout.Bytes(), &replan); err != nil {
		t.Fatalf("decode replan: %v", err)
	}
	if len(replan.Actions) != 0 {
		t.Errorf("replan has %d actions, want 0 (fixed point)", len(replan.Actions))
	}
}

func TestRunUpgradeTargetConfigInstallsPolicyProfile(t *testing.T) {
	ctx := t.Context()
	root, engineModPath, engineSumPath := buildDerivedForCLI(ctx, t)

	target := strings.Replace(planProfile,
		"  forbiddenModules: []\n",
		"  forbiddenModules:\n    - k8s.io/apiserver\n",
		1,
	)
	target = strings.Replace(target,
		"compatibility:\n  apiserver: external\n",
		"compatibility:\n  apiserver: local\n",
		1,
	)
	targetPath := filepath.Join(t.TempDir(), "target-soapbox.yaml")
	if err := os.WriteFile(targetPath, []byte(target), 0o644); err != nil {
		t.Fatalf("write target profile: %v", err)
	}

	args := []string{
		"upgrade", "-engine-version", "tools/v1.5.0",
		"-engine-mod", engineModPath, "-engine-sum", engineSumPath,
		"-target-config", targetPath, "-format", "json",
	}
	var stdout, stderr bytes.Buffer
	code := cli.Run(ctx, cli.Env{Stdout: &stdout, Stderr: &stderr, Dir: root}, args)
	if code != cli.ExitOK {
		t.Fatalf("plan exit %d, stderr: %s", code, stderr.String())
	}
	var manifest upgrade.Report
	if err := json.Unmarshal(stdout.Bytes(), &manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if !slices.ContainsFunc(manifest.Actions, func(action upgrade.Action) bool {
		return action.Path == config.DefaultFileName && action.Kind == upgrade.ActionUpdate
	}) {
		t.Fatalf("actions = %#v, want target profile update", manifest.Actions)
	}

	applyArgs := append(slices.Clone(args[:len(args)-2]), "-apply", "-approve", manifest.Hash)
	stdout.Reset()
	stderr.Reset()
	code = cli.Run(ctx, cli.Env{Stdout: &stdout, Stderr: &stderr, Dir: root}, applyArgs)
	if code != cli.ExitOK {
		t.Fatalf("apply exit %d, stderr: %s", code, stderr.String())
	}
	got, err := os.ReadFile(filepath.Join(root, config.DefaultFileName))
	if err != nil {
		t.Fatalf("read applied profile: %v", err)
	}
	if !bytes.Equal(got, []byte(target)) {
		t.Fatal("applied profile bytes differ from the manifest-selected target")
	}
	cfg, err := config.Decode(got)
	if err != nil {
		t.Fatalf("decode applied profile: %v", err)
	}
	if cfg.Compatibility.Apiserver != config.CompatibilityApiserverLocal {
		t.Errorf("compatibility = %q, want local", cfg.Compatibility.Apiserver)
	}
}

func TestRunUpgradeTargetConfigRefusesRetarget(t *testing.T) {
	ctx := t.Context()
	root, engineModPath, engineSumPath := buildDerivedForCLI(ctx, t)
	target := strings.Replace(planProfile, "  branch: main\n", "  branch: other\n", 1)
	targetPath := filepath.Join(t.TempDir(), "retarget.yaml")
	if err := os.WriteFile(targetPath, []byte(target), 0o644); err != nil {
		t.Fatalf("write target profile: %v", err)
	}

	var stderr bytes.Buffer
	code := cli.Run(ctx, cli.Env{Stderr: &stderr, Dir: root}, []string{
		"upgrade", "-engine-version", "tools/v1.5.0",
		"-engine-mod", engineModPath, "-engine-sum", engineSumPath,
		"-target-config", targetPath,
	})
	if code != cli.ExitCheck {
		t.Fatalf("exit = %d, want %d; stderr: %s", code, cli.ExitCheck, stderr.String())
	}
	if !strings.Contains(stderr.String(), "immutable profile field destination.branch") {
		t.Errorf("stderr = %q, want immutable destination branch refusal", stderr.String())
	}
}

// TestRunUpgradeRootGoModSurvivesApply verifies root go.mod with generated
// requirements is byte-identical after apply.
func TestRunUpgradeRootGoModSurvivesApply(t *testing.T) {
	ctx := t.Context()
	root, engineModPath, engineSumPath := buildDerivedForCLI(ctx, t)

	// Add generated requirements to root go.mod.
	goModPath := filepath.Join(root, "go.mod")
	original, err := os.ReadFile(goModPath)
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	expanded := string(original) + "\nrequire k8s.io/api v0.36.1\n"
	if err := os.WriteFile(goModPath, []byte(expanded), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	commitAll(ctx, t, root)

	// Plan.
	var stdout bytes.Buffer
	code := cli.Run(ctx, cli.Env{Stdout: &stdout, Dir: root}, []string{
		"upgrade", "-engine-version", "tools/v1.5.0",
		"-engine-mod", engineModPath, "-engine-sum", engineSumPath,
		"-format", "json",
	})
	if code != cli.ExitOK {
		t.Fatalf("plan exit %d", code)
	}
	var manifest upgrade.Report
	if err := json.Unmarshal(stdout.Bytes(), &manifest); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Apply.
	stdout.Reset()
	code = cli.Run(ctx, cli.Env{Stdout: &stdout, Dir: root}, []string{
		"upgrade", "-engine-version", "tools/v1.5.0",
		"-engine-mod", engineModPath, "-engine-sum", engineSumPath,
		"-apply", "-approve", manifest.Hash,
	})
	if code != cli.ExitOK {
		t.Fatalf("apply exit %d", code)
	}

	// Root go.mod must be byte-identical.
	after, err := os.ReadFile(goModPath)
	if err != nil {
		t.Fatalf("read go.mod after apply: %v", err)
	}
	if string(after) != expanded {
		t.Errorf("root go.mod changed after apply:\nbefore: %s\nafter: %s", expanded, after)
	}
}

// TestRunUpgradeRefusesDowngrade proves a target version older than the
// current pin is refused.
func TestRunUpgradeRefusesDowngrade(t *testing.T) {
	ctx := t.Context()
	root, engineModPath, _ := buildDerivedForCLI(ctx, t)

	// Current engine is v0.3.0. Asking for v0.2.0 must fail.
	// We need a go.sum for v0.2.0 too.
	tmpDir := t.TempDir()
	oldSumPath := filepath.Join(tmpDir, "old-go.sum")
	oldSum := strings.ReplaceAll(engineGoSumContent, "v1.5.0", "v0.2.0")
	if err := os.WriteFile(oldSumPath, []byte(oldSum), 0o644); err != nil {
		t.Fatalf("write old sum: %v", err)
	}

	var stderr bytes.Buffer
	code := cli.Run(ctx, cli.Env{Stderr: &stderr, Dir: root}, []string{
		"upgrade", "-engine-version", "tools/v0.2.0",
		"-engine-mod", engineModPath, "-engine-sum", oldSumPath,
	})
	if code != cli.ExitCheck {
		t.Fatalf("exit code = %d, want %d; stderr: %s", code, cli.ExitCheck, stderr.String())
	}
	if !strings.Contains(stderr.String(), "downgrade") {
		t.Errorf("stderr = %q, want mention of downgrade", stderr.String())
	}
}

// TestRunUpgradeRefusesUnrelatedRepo proves a repo with the wrong root module
// is refused as not derived.
func TestRunUpgradeRefusesUnrelatedRepo(t *testing.T) {
	ctx := t.Context()

	repo := testsupport.NewRepo(ctx, t, testsupport.Options{
		UserName:  "Upgrade Test",
		UserEmail: "test@example.invalid",
	})
	// Create a repo with go.mod and soapbox.yaml but wrong module path.
	repo.WriteFile(t, "go.mod", "module example.com/wrong\n\ngo 1.26.0\n")
	repo.WriteFile(t, config.DefaultFileName, planProfile)
	repo.WriteFile(t, "tools/go.mod", "module example.com/wrong/tools\n\ngo 1.26.0\n")
	repo.Commit(ctx, t, "chore: init", gitcli.CommitOptions{}, ".")

	root, err := filepath.EvalSymlinks(repo.Dir)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	tmpDir := t.TempDir()
	engineModPath := filepath.Join(tmpDir, "engine.mod")
	if err := os.WriteFile(engineModPath, []byte(engineGoModContent), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	engineSumPath := filepath.Join(tmpDir, "engine.sum")
	if err := os.WriteFile(engineSumPath, []byte(engineGoSumContent), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	var stderr bytes.Buffer
	code := cli.Run(ctx, cli.Env{Stderr: &stderr, Dir: root}, []string{
		"upgrade", "-engine-version", "tools/v1.5.0",
		"-engine-mod", engineModPath, "-engine-sum", engineSumPath,
	})
	if code != cli.ExitCheck {
		t.Fatalf("exit code = %d, want %d; stderr: %s", code, cli.ExitCheck, stderr.String())
	}
	if !strings.Contains(stderr.String(), "not a derived") {
		t.Errorf("stderr = %q, want mention of 'not a derived'", stderr.String())
	}
}

// digestBytes renders the sha256 digest of contents in the manifest format.
func digestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}
