package cli_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/cli"
	"github.com/enj/soapbox/tools/internal/config"
	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/testsupport"
)

// TestRunPlanUsage covers every command line the plan command refuses.
//
// None of these reads a profile, opens a cache, or touches a network, so each
// one has to fail identically whether or not any of those exist. The directory
// is deliberately empty to prove it.
func TestRunPlanUsage(t *testing.T) {
	dir := t.TempDir()

	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStderr string
	}{
		{
			name:       "tag and branch together",
			args:       []string{"plan", "-dir", dir, "-tag", "v1.36.1", "-branch", "master"},
			wantCode:   cli.ExitUsage,
			wantStderr: "only one may be given",
		},
		{
			name:       "offline with an explicit fetch",
			args:       []string{"plan", "-dir", dir, "-offline", "-fetch"},
			wantCode:   cli.ExitUsage,
			wantStderr: "refuses every network operation",
		},
		{
			name:       "unsupported format",
			args:       []string{"plan", "-dir", dir, "-format", "yaml"},
			wantCode:   cli.ExitUsage,
			wantStderr: `unsupported -format "yaml"`,
		},
		{
			name:       "unknown flag",
			args:       []string{"plan", "-nope"},
			wantCode:   cli.ExitUsage,
			wantStderr: "flag provided but not defined",
		},
		{
			name:       "operands",
			args:       []string{"plan", "extra"},
			wantCode:   cli.ExitUsage,
			wantStderr: "takes no arguments",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stdout, stderr, code := run(t.Context(), t, "", test.args...)
			if code != test.wantCode {
				t.Fatalf("exit code = %d, want %d\nstdout: %s\nstderr: %s", code, test.wantCode, stdout, stderr)
			}
			if !strings.Contains(stderr, test.wantStderr) {
				t.Fatalf("stderr %q does not contain %q", stderr, test.wantStderr)
			}
			if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
				t.Fatalf("a usage failure touched the working directory: %v %v", entries, err)
			}
		})
	}
}

// TestRunPlanHelp keeps the rendered flags and the parsed flags in agreement.
func TestRunPlanHelp(t *testing.T) {
	stdout, stderr, code := run(t.Context(), t, "", "help", "plan")
	if code != cli.ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", code, stderr)
	}
	for _, flag := range []string{
		"-cache string", "-work string", "-out string", "-report string",
		"-tag string", "-branch string", "-patch-branch string", "-source-remote string",
		"-fetch", "-offline", "-materialize", "-keep-worktree", "-format string", "-strict",
	} {
		if !strings.Contains(stdout, flag) {
			t.Errorf("plan usage does not document %s:\n%s", flag, stdout)
		}
	}
}

// TestRunPlanEndToEnd drives the command through its real terminal surface
// against a real upstream repository.
//
// The engine has its own tests; what this adds is proof that the command line
// wires them together: that the flags resolve to the directories the plan uses,
// that both output formats render, that -report writes the same bytes the JSON
// format prints, and that the process exit code contract holds.
func TestRunPlanEndToEnd(t *testing.T) {
	ctx := t.Context()
	up := newPlanUpstream(ctx, t)
	dir := planProfileDir(t, up)

	t.Run("summary", func(t *testing.T) {
		reportPath := filepath.Join(t.TempDir(), "reports", "plan.json")
		stdout, stderr, code := run(ctx, t, dir,
			"plan", "-source-remote", up.url(), "-cache", ".cache", "-report", reportPath)
		if code != cli.ExitOK {
			t.Fatalf("exit code = %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
		}
		for _, want := range []string{
			"soapbox plan for tag v1.36.1",
			"closure       ",
			"manifest      sha256:",
			"computed only, pass -materialize",
		} {
			if !strings.Contains(stdout, want) {
				t.Errorf("summary does not contain %q:\n%s", want, stdout)
			}
		}

		written, err := os.ReadFile(reportPath)
		if err != nil {
			t.Fatalf("read the written report: %v", err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(written, &decoded); err != nil {
			t.Fatalf("the written report is not JSON: %v", err)
		}
		if decoded["schema"] != float64(1) {
			t.Errorf("report schema = %v, want 1", decoded["schema"])
		}
		if strings.Contains(string(written), dir) {
			t.Error("the written report leaks the profile directory")
		}
	})

	t.Run("json matches the written report", func(t *testing.T) {
		reportPath := filepath.Join(t.TempDir(), "plan.json")
		stdout, stderr, code := run(ctx, t, dir,
			"plan", "-source-remote", up.url(), "-cache", ".cache", "-format", "json", "-report", reportPath)
		if code != cli.ExitOK {
			t.Fatalf("exit code = %d\nstderr: %s", code, stderr)
		}
		written, err := os.ReadFile(reportPath)
		if err != nil {
			t.Fatalf("read the written report: %v", err)
		}
		if stdout != string(written) {
			t.Error("the printed report and the written report differ")
		}
	})

	t.Run("materialize", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "tree")
		_, stderr, code := run(ctx, t, dir,
			"plan", "-source-remote", up.url(), "-cache", ".cache", "-materialize", "-out", out)
		if code != cli.ExitOK {
			t.Fatalf("exit code = %d\nstderr: %s", code, stderr)
		}
		authorizer := filepath.Join(out, filepath.FromSlash(
			"internal/kk/plugin/pkg/auth/authorizer/rbac/rbac.go"))
		contents, err := os.ReadFile(authorizer)
		if err != nil {
			t.Fatalf("read the relocated authorizer: %v", err)
		}
		if !strings.Contains(string(contents), "monis.app/kk/fixture/internal/kk/pkg/registry/rbac/validation") {
			t.Errorf("the materialized file was not rewritten:\n%s", contents)
		}

		// A second run must refuse rather than merge into the tree the first one
		// wrote.
		_, stderr, code = run(ctx, t, dir,
			"plan", "-source-remote", up.url(), "-cache", ".cache", "-materialize", "-out", out)
		if code != cli.ExitFailure {
			t.Fatalf("exit code = %d, want %d\nstderr: %s", code, cli.ExitFailure, stderr)
		}
		if !strings.Contains(stderr, "destination already exists") {
			t.Errorf("stderr %q does not report the existing destination", stderr)
		}
	})

	t.Run("policy failure exits check", func(t *testing.T) {
		_, stderr, code := run(ctx, t, dir, "plan", "-source-remote", up.url(), "-cache", ".cache", "-tag", "v9.99.9")
		if code != cli.ExitCheck {
			t.Fatalf("exit code = %d, want %d\nstderr: %s", code, cli.ExitCheck, stderr)
		}
	})

	t.Run("offline without a cache exits failure", func(t *testing.T) {
		empty := planProfileDir(t, up)
		_, stderr, code := run(ctx, t, empty, "plan", "-source-remote", up.url(), "-cache", ".cache", "-offline")
		if code != cli.ExitFailure {
			t.Fatalf("exit code = %d, want %d\nstderr: %s", code, cli.ExitFailure, stderr)
		}
		if !strings.Contains(stderr, "offline run needs an existing cache") {
			t.Errorf("stderr %q does not explain the missing cache", stderr)
		}
	})
}

// TestRunPlanCancellation proves a cancelled plan reports the cancellation
// rather than a finding about the profile.
func TestRunPlanCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	up := newPlanUpstream(ctx, t)
	dir := planProfileDir(t, up)
	cancel()

	_, stderr, code := run(ctx, t, dir, "plan", "-source-remote", up.url(), "-cache", ".cache")
	if code != cli.ExitCanceled {
		t.Fatalf("exit code = %d, want %d\nstderr: %s", code, cli.ExitCanceled, stderr)
	}
}

// TestRunPlanOfflineReachesNoRemote proves an offline run performs no network
// I/O, including the fetch nobody asks for.
//
// Refusing to clone and refusing to fetch is not enough. The cache is blobless,
// so checking out a commit whose blobs never arrived makes git download them
// from the promisor remote on its own, and a run that did that would be reading
// upstream while claiming to be offline. The remote is moved out of the way for
// the offline run, so a checkout that reached for it would fail naming the
// transport rather than the object.
func TestRunPlanOfflineReachesNoRemote(t *testing.T) {
	ctx := t.Context()
	up := newPlanUpstream(ctx, t)
	dir := planProfileDir(t, up)
	cacheRoot := filepath.Join(dir, ".cache")

	// A first online run with the closure narrowed to one package, so the cache
	// holds the commits and trees of the whole repository and the blobs of that
	// package alone.
	narrow := strings.Replace(planProfile,
		"    - plugin/pkg/auth/authorizer/rbac\n", "    - pkg/registry/rbac/validation\n", 1)
	narrow = strings.Replace(narrow,
		"    - pkg/registry/rbac/validation/internal_version_adapter.go\n", "", 1)
	narrowDir := t.TempDir()
	writeProfile(t, filepath.Join(narrowDir, config.DefaultFileName), narrow)

	_, stderr, code := run(ctx, t, narrowDir, "plan", "-source-remote", up.url(), "-cache", cacheRoot)
	if code != cli.ExitOK {
		t.Fatalf("the priming run failed with %d\nstderr: %s", code, stderr)
	}

	// The remote is moved out of the way. Anything that now reaches for it
	// fails in a way that names the repository rather than an object.
	moved := up.repo.Dir + "-moved"
	if err := os.Rename(up.repo.Dir, moved); err != nil {
		t.Fatalf("move the upstream out of the way: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Rename(moved, up.repo.Dir); err != nil {
			t.Errorf("restore the upstream: %v", err)
		}
	})

	// The offline run needs a package whose blobs the priming run never
	// materialized, so a checkout that lazily fetched would have to reach the
	// remote that is no longer there.
	_, stderr, code = run(ctx, t, dir,
		"plan", "-source-remote", up.url(), "-cache", cacheRoot, "-offline")
	if code == cli.ExitOK {
		t.Fatalf("the offline run succeeded, so it either found every blob locally or fetched:\n%s", stderr)
	}
	// A fetch that was attempted and failed says so in git's own words. Neither
	// spelling may appear, because neither can happen without a transfer having
	// been started.
	for _, transport := range []string{
		"does not appear to be a git repository",
		"Could not read from remote repository",
		"remote helper",
		moved,
	} {
		if strings.Contains(stderr, transport) {
			t.Errorf("the offline run reached for the remote (%q):\n%s", transport, stderr)
		}
	}
	if !strings.Contains(stderr, "soapbox:") {
		t.Errorf("the offline run produced no diagnostic:\n%s", stderr)
	}
}

// TestRunPlanRefusesConflictingPaths proves an output tree that would sit where
// the run's own state lives is reported as the flag problem it is.
//
// Every directory involved is one the operator named or defaulted, and the
// answer does not depend on a profile, a cache, or a network existing, so it
// belongs with the other usage failures rather than with the failures a run
// discovers. The profile directory is deliberately left empty to prove it.
func TestRunPlanRefusesConflictingPaths(t *testing.T) {
	dir := t.TempDir()

	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "output is the cache root",
			args: []string{"-cache", "state", "-out", "state"},
			want: "source cache root",
		},
		{
			name: "output contains the work root",
			args: []string{"-out", "state", "-work", "state/work"},
			want: "work root",
		},
		{
			name: "output inside the materialized source root",
			args: []string{"-work", "state", "-out", "state/src/tree"},
			want: "materialized source root",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			args := append([]string{"plan", "-dir", dir}, test.args...)
			stdout, stderr, code := run(t.Context(), t, "", args...)
			if code != cli.ExitUsage {
				t.Fatalf("exit code = %d, want %d\nstdout: %s\nstderr: %s", code, cli.ExitUsage, stdout, stderr)
			}
			if !strings.Contains(stderr, test.want) {
				t.Fatalf("stderr does not name the %s:\n%s", test.want, stderr)
			}
			if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
				t.Fatalf("a refused option set created something: %v %v", entries, err)
			}
		})
	}
}

// planUpstream is a minimal real repository the plan command can extract from.
type planUpstream struct {
	repo *testsupport.Repo
}

func (u *planUpstream) url() string { return "file://" + u.repo.Dir }

// TestRunPlanWritesAReportForAFinding proves a refusal is reviewable from an
// artifact rather than from a stderr line.
//
// A finding CI cannot attach, diff, or key an issue on is a finding CI cannot
// act on, so the report is written before the exit code is decided.
func TestRunPlanWritesAReportForAFinding(t *testing.T) {
	ctx := t.Context()
	up := newPlanUpstream(ctx, t)

	tests := []struct {
		name    string
		profile string
		args    []string
		stage   string
	}{
		{
			// The profile pins a closure golden the directory does not hold, so
			// the plan produces a notice and -strict refuses it. The report is
			// complete: the run measured everything before the gate ran.
			name:    "strict refuses a notice",
			profile: planProfile,
			args:    []string{"-strict"},
			stage:   "plan strict",
		},
		{
			// A limit the closure passes refuses partway through, so the report
			// is partial and still has to be a report.
			name:    "closure passes a limit",
			profile: strings.Replace(planProfile, "    maxPackages: 8\n", "    maxPackages: 1\n", 1),
			stage:   "plan closure",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			writeProfile(t, filepath.Join(dir, config.DefaultFileName), test.profile)
			reportPath := filepath.Join(t.TempDir(), "reports", "plan.json")

			args := append([]string{
				"plan", "-source-remote", up.url(), "-cache", ".cache", "-report", reportPath,
			}, test.args...)
			_, stderr, code := run(ctx, t, dir, args...)
			if code != cli.ExitCheck {
				t.Fatalf("exit code = %d, want %d\nstderr: %s", code, cli.ExitCheck, stderr)
			}

			written, err := os.ReadFile(reportPath)
			if err != nil {
				t.Fatalf("the refusal wrote no report: %v", err)
			}
			var decoded struct {
				Schema  int `json:"schema"`
				Failure *struct {
					Stage   string `json:"stage"`
					Message string `json:"message"`
				} `json:"failure"`
			}
			if err := json.Unmarshal(written, &decoded); err != nil {
				t.Fatalf("the written report is not JSON: %v", err)
			}
			if decoded.Schema != 1 {
				t.Errorf("report schema = %d, want 1", decoded.Schema)
			}
			if decoded.Failure == nil {
				t.Fatalf("the report carries no failure section:\n%s", written)
			}
			if decoded.Failure.Stage != test.stage {
				t.Errorf("failure stage = %q, want %q", decoded.Failure.Stage, test.stage)
			}
			if decoded.Failure.Message == "" {
				t.Error("the failure section carries no message")
			}
			if strings.Contains(string(written), dir) {
				t.Error("the written report leaks the profile directory")
			}
		})
	}
}

// TestRunPlanKeepsTheCheckCodeWhenTheReportCannotBeWritten proves a second
// problem does not hide the first.
//
// A run that found a policy failure and could not write its report has two
// problems. The exit code still has to say a finding is waiting, because that is
// what a reviewer acts on, and the write failure still has to be printed,
// because otherwise the missing artifact is a mystery.
func TestRunPlanKeepsTheCheckCodeWhenTheReportCannotBeWritten(t *testing.T) {
	ctx := t.Context()
	up := newPlanUpstream(ctx, t)
	dir := planProfileDir(t, up)

	// A regular file where the report's parent directory belongs, so the write
	// fails for a reason that has nothing to do with the plan.
	blocked := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(blocked, []byte("not a directory\n"), 0o600); err != nil {
		t.Fatalf("create the blocking file: %v", err)
	}

	_, stderr, code := run(ctx, t, dir,
		"plan", "-source-remote", up.url(), "-cache", ".cache", "-strict",
		"-report", filepath.Join(blocked, "plan.json"))
	if code != cli.ExitCheck {
		t.Fatalf("exit code = %d, want %d\nstderr: %s", code, cli.ExitCheck, stderr)
	}
	for _, want := range []string{"plan strict", "write plan report"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr does not mention %q:\n%s", want, stderr)
		}
	}
}

// TestRunPlanPatchBranch covers the branch a tag run matches patch selectors
// against.
//
// A patch is authored against a line of development and a tag names no line, so
// the branch is derived from upstream's own convention when the profile tracks
// it and demanded from the operator when nothing answers. Matching a selector
// against a branch the maintainer did not choose applies patches that were never
// meant for the release being planned.
func TestRunPlanPatchBranch(t *testing.T) {
	ctx := t.Context()
	up := newPlanUpstream(ctx, t)

	tests := []struct {
		name     string
		branches string
		args     []string
		wantCode int
		want     string
	}{
		{
			name:     "derived from the tag",
			branches: "      - master\n      - release-1.36\n",
			wantCode: cli.ExitOK,
			want:     "release-1.36",
		},
		{
			name:     "one tracked branch is unambiguous",
			branches: "      - master\n",
			wantCode: cli.ExitOK,
			want:     "master",
		},
		{
			name:     "explicit flag wins",
			branches: "      - master\n      - release-1.36\n",
			args:     []string{"-patch-branch", "release-1.35"},
			wantCode: cli.ExitOK,
			want:     "release-1.35",
		},
		{
			// Several tracked branches and none of them derived from the tag.
			// There is no defensible default, so the operator decides.
			name:     "ambiguous without the flag",
			branches: "      - master\n      - release-1.35\n",
			wantCode: cli.ExitUsage,
			want:     "-patch-branch is required",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			profile := strings.Replace(planProfile, "      - master\n", test.branches, 1)
			profile = strings.Replace(profile, "patches: []\n", "patches:\n  - file: patches/noop.patch\n", 1)

			dir := t.TempDir()
			writeProfile(t, filepath.Join(dir, config.DefaultFileName), profile)
			writePlanPatch(t, dir)

			args := append([]string{"plan", "-source-remote", up.url(), "-cache", ".cache", "-format", "json"}, test.args...)
			stdout, stderr, code := run(ctx, t, dir, args...)
			if code != test.wantCode {
				t.Fatalf("exit code = %d, want %d\nstdout: %s\nstderr: %s", code, test.wantCode, stdout, stderr)
			}
			if test.wantCode != cli.ExitOK {
				if !strings.Contains(stderr, test.want) {
					t.Fatalf("stderr does not explain the ambiguity:\n%s", stderr)
				}
				return
			}

			var decoded struct {
				Patches struct {
					Branch   string   `json:"branch"`
					Selected []string `json:"selected"`
				} `json:"patches"`
			}
			if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
				t.Fatalf("the printed report is not JSON: %v", err)
			}
			if decoded.Patches.Branch != test.want {
				t.Errorf("patch branch = %q, want %q", decoded.Patches.Branch, test.want)
			}
			// The patch carries no branch selector, so it is selected whatever
			// branch was chosen. What the branch decides is what the report
			// records, which is the value under test.
			if len(decoded.Patches.Selected) != 1 {
				t.Errorf("selected patches = %v, want the one the profile carries", decoded.Patches.Selected)
			}
		})
	}

	t.Run("no patches needs no branch", func(t *testing.T) {
		dir := planProfileDir(t, up)
		stdout, stderr, code := run(ctx, t, dir,
			"plan", "-source-remote", up.url(), "-cache", ".cache", "-format", "json")
		if code != cli.ExitOK {
			t.Fatalf("exit code = %d\nstderr: %s", code, stderr)
		}
		var decoded struct {
			Patches struct {
				Branch string `json:"branch"`
			} `json:"patches"`
		}
		if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
			t.Fatalf("the printed report is not JSON: %v", err)
		}
		if decoded.Patches.Branch != "" {
			t.Errorf("a profile with no patches reported the branch %q", decoded.Patches.Branch)
		}
	})
}

// writePlanPatch stores the patch that makes the profile one carrying patches.
//
// It has no branch selector, so which branch the command derived is visible in
// the report rather than in whether the patch was selected.
func writePlanPatch(t *testing.T, dir string) {
	t.Helper()
	full := filepath.Join(dir, "patches", "noop.patch")
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		t.Fatalf("create patch directory: %v", err)
	}
	diff := `--- a/pkg/registry/rbac/validation/rule.go
+++ b/pkg/registry/rbac/validation/rule.go
@@ -3,4 +3,5 @@ package validation
 // RuleResolver resolves the rules a subject holds.
 type RuleResolver interface {
` + " \tRules() []string\n" + `+	Count() int
 }
`
	if err := os.WriteFile(full, []byte(diff), 0o600); err != nil {
		t.Fatalf("write patch: %v", err)
	}
}

// newPlanUpstream builds the smallest upstream that exercises the whole
// pipeline: one configured root, one discovered package, and one prune entry.
func newPlanUpstream(ctx context.Context, t *testing.T) *planUpstream {
	t.Helper()
	repo := testsupport.NewRepo(ctx, t, testsupport.Options{
		Branch:    "master",
		UserName:  "Soapbox Test",
		UserEmail: "test@example.com",
	})
	repo.SetConfig(ctx, t, "uploadpack.allowFilter", "true")

	files := map[string]string{
		"plugin/pkg/auth/authorizer/rbac/rbac.go": `package rbac

import rbacregistryvalidation "k8s.io/kubernetes/pkg/registry/rbac/validation"

// Authorizer answers authorization requests.
type Authorizer struct {
	resolver rbacregistryvalidation.RuleResolver
}
`,
		"pkg/registry/rbac/validation/rule.go": `package validation

// RuleResolver resolves the rules a subject holds.
type RuleResolver interface {
	Rules() []string
}
`,
		"pkg/registry/rbac/validation/internal_version_adapter.go": `package validation

// Adapt is the adapter the profile removes.
func Adapt() {}
`,
	}
	paths := make([]string, 0, len(files))
	for path, contents := range files {
		repo.WriteFile(t, path, contents)
		paths = append(paths, path)
	}
	commit := repo.Commit(ctx, t, "feat: add the authorizer\n", gitcli.CommitOptions{}, paths...)
	if err := repo.Git.CreateTag(ctx, gitcli.TagOptions{
		Name:    "v1.36.1",
		Commit:  commit,
		Message: "Kubernetes v1.36.1\n",
		Tagger:  gitcli.Signature{Name: "Soapbox Test", Email: "test@example.com", Date: "2026-01-02T03:04:05Z"},
	}); err != nil {
		t.Fatalf("create tag: %v", err)
	}
	return &planUpstream{repo: repo}
}

// planProfileDir writes a profile matching the plan fixture into a fresh
// directory.
func planProfileDir(t *testing.T, _ *planUpstream) string {
	t.Helper()
	dir := t.TempDir()
	writeProfile(t, filepath.Join(dir, config.DefaultFileName), planProfile)
	return dir
}

// planProfile is a complete profile for the command level fixture.
const planProfile = `version: 2
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
  module: monis.app/kk/fixture
  repository: enj/fixture
  remote: https://github.com/enj/fixture.git
  branch: main
  stateRef: refs/heads/soapbox-state
  progressRefPrefix: refs/soapbox/progress/
  rootPackage: fixture
  internalPrefix: internal/kk
  summary: the Kubernetes RBAC authorizer as an independently consumable Go module.
packages:
  roots:
    - plugin/pkg/auth/authorizer/rbac
  recursive: false
  assetGlobs: []
prune:
  files:
    - pkg/registry/rbac/validation/internal_version_adapter.go
  required:
    - pkg/registry/rbac/validation/rule.go
deny:
  imports:
    - k8s.io/kubernetes/pkg/apis/rbac
closure:
  includeTests: false
  limits:
    maxPackages: 8
    maxFiles: 40
    maxNonTestLines: 5000
    maxPackageGrowth: 2
  golden: testdata/closure/fixture.json
types:
  policy: prefer-external
  pairs: []
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
  overrides: []
patches: []
facade:
  package: fixture
  file: authorizer.go
  assertionsFile: zz_generated_assertions.go
  exports:
    - name: Authorizer
      kind: type
      source: k8s.io/kubernetes/plugin/pkg/auth/authorizer/rbac.Authorizer
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
  path: kk/fixture/index.html
  importPath: monis.app/kk/fixture
  repositoryURL: https://github.com/enj/fixture
  probeURL: https://monis.app/kk/fixture?go-get=1
publication:
  mode: automatic
compatibility:
  apiserver: external
determinism:
  toolchain: go1.26.5
  chunkSize: 200
`
