package setup_test

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/enj/soapbox/tools/internal/config"
	"github.com/enj/soapbox/tools/internal/setup"
)

// pinnedUses matches an action reference pinned to a full commit. The release
// that commit was is a YAML comment beside it, which the decoder strips, so the
// comment is asserted separately against the file text.
var pinnedUses = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+@[0-9a-f]{40}$`)

// pinnedUsesLine matches the whole rendered line, the pin and the release a
// reader needs to know which one it is.
var pinnedUsesLine = regexp.MustCompile(`^ *(- )?uses: [A-Za-z0-9._-]+/[A-Za-z0-9._-]+@[0-9a-f]{40} # v[0-9]+\.[0-9]+\.[0-9]+$`)

// workflow is the shape of a generated workflow, decoded rather than matched.
//
// Decoding is what makes these assertions mean anything. A test that grepped for
// "contents: read" would pass on a workflow where that string appeared in a
// comment, in the wrong job, or beside a second permissions block that overrode
// it, and every one of those is the bug this file exists to catch.
type workflow struct {
	Name string `yaml:"name"`
	// The trigger key is quoted because YAML 1.1 resolves a bare "on" to a
	// boolean, and the generated file is read back by GitHub rather than by this
	// decoder.
	On          map[string]any         `yaml:"on"`
	Permissions map[string]string      `yaml:"permissions"`
	Concurrency concurrency            `yaml:"concurrency"`
	Jobs        map[string]workflowJob `yaml:"jobs"`
}

type concurrency struct {
	Group            string `yaml:"group"`
	CancelInProgress bool   `yaml:"cancel-in-progress"`
}

type workflowJob struct {
	If             string            `yaml:"if"`
	RunsOn         string            `yaml:"runs-on"`
	TimeoutMinutes int               `yaml:"timeout-minutes"`
	Permissions    map[string]string `yaml:"permissions"`
	Steps          []workflowStep    `yaml:"steps"`
}

type workflowStep struct {
	Name             string            `yaml:"name"`
	If               string            `yaml:"if"`
	Uses             string            `yaml:"uses"`
	Run              string            `yaml:"run"`
	With             map[string]any    `yaml:"with"`
	Env              map[string]string `yaml:"env"`
	WorkingDirectory string            `yaml:"working-directory"`
}

// TestGeneratedWorkflowsAreLeastPrivilege inspects what setup actually wrote.
func TestGeneratedWorkflowsAreLeastPrivilege(t *testing.T) {
	ctx := t.Context()
	root, git := newTemplate(ctx, t, nil)
	opts := newOptions(ctx, t, root, git)
	planned := plan(ctx, t, opts)
	if _, err := setup.Apply(ctx, opts, planned.Report.Hash); err != nil {
		t.Fatalf("apply: %v", err)
	}

	ci := decodeWorkflow(t, filepath.Join(root, ".github", "workflows", "ci.yml"))
	sync := decodeWorkflow(t, filepath.Join(root, ".github", "workflows", "sync.yml"))

	t.Run("every job has a bounded runtime", func(t *testing.T) {
		for name, flow := range map[string]workflow{"ci": ci, "sync": sync} {
			for jobName, job := range flow.Jobs {
				if job.TimeoutMinutes <= 0 {
					t.Errorf("%s job %s has no timeout", name, jobName)
				}
			}
		}
	})

	t.Run("no workflow inherits a token by default", func(t *testing.T) {
		for name, flow := range map[string]workflow{"ci": ci, "sync": sync} {
			if len(flow.Permissions) != 0 {
				t.Errorf("%s grants %v at the top level, want none", name, flow.Permissions)
			}
		}
	})

	t.Run("no workflow runs fork code with repository secrets", func(t *testing.T) {
		for name, flow := range map[string]workflow{"ci": ci, "sync": sync} {
			if _, ok := flow.On["pull_request_target"]; ok {
				t.Errorf("%s uses pull_request_target", name)
			}
		}
	})

	t.Run("every action is pinned to a commit", func(t *testing.T) {
		for name, flow := range map[string]workflow{"ci": ci, "sync": sync} {
			for _, job := range flow.Jobs {
				for _, step := range job.Steps {
					if step.Uses == "" {
						continue
					}
					if !pinnedUses.MatchString(step.Uses) {
						t.Errorf("%s step %q uses %q, which is not a full commit pin", name, step.Name, step.Uses)
					}
				}
			}
		}
		// A bare SHA is unreadable, so every pin carries the release it was. The
		// comment is not the contract, but a pin nobody can identify is a pin
		// nobody will ever update.
		for _, name := range []string{"ci.yml", "sync.yml"} {
			for _, line := range strings.Split(readFile(t, filepath.Join(root, ".github", "workflows", name)), "\n") {
				if !strings.Contains(line, "uses:") {
					continue
				}
				if !pinnedUsesLine.MatchString(line) {
					t.Errorf("%s line %q does not name the release its pin was", name, line)
				}
			}
		}
	})

	t.Run("ci holds no credential", func(t *testing.T) {
		job, ok := ci.Jobs["verify"]
		if !ok {
			t.Fatalf("ci has jobs %v, want verify", keysOf(ci.Jobs))
		}
		if got := job.Permissions; len(got) != 1 || got["contents"] != "read" {
			t.Errorf("ci verify permissions = %v, want only contents: read", got)
		}
		for _, step := range job.Steps {
			if len(step.Env) != 0 {
				t.Errorf("ci step %q exports %v, and a verification job needs no secret", step.Name, step.Env)
			}
		}
		if strings.Contains(readFile(t, filepath.Join(root, ".github", "workflows", "ci.yml")), "secrets.") {
			t.Error("ci names a secret")
		}
		if got := checkoutWith(t, job, "persist-credentials"); got != false {
			t.Errorf("ci checkout persist-credentials = %v, want false", got)
		}
	})

	t.Run("sync runs unattended and only from the protected default branch", func(t *testing.T) {
		dispatch, ok := sync.On["workflow_dispatch"].(map[string]any)
		if !ok {
			t.Fatalf("sync workflow_dispatch = %#v, want input mapping", sync.On["workflow_dispatch"])
		}
		inputs, ok := dispatch["inputs"].(map[string]any)
		if !ok {
			t.Fatalf("sync dispatch inputs = %#v", dispatch["inputs"])
		}
		verify, ok := inputs["verify-write"].(map[string]any)
		if !ok || verify["type"] != "boolean" || verify["required"] != false || verify["default"] != false {
			t.Fatalf("verify-write input = %#v, want optional false boolean", inputs["verify-write"])
		}
		if _, ok := sync.On["pull_request"]; ok {
			t.Error("sync runs on pull requests")
		}
		if want := "github.ref == 'refs/heads/main' && github.ref_protected"; sync.Jobs["sync"].If != want {
			t.Errorf("sync guard = %q, want %q", sync.Jobs["sync"].If, want)
		}
	})

	t.Run("sync checkout fetches full history while CI stays shallow", func(t *testing.T) {
		syncJob := sync.Jobs["sync"]
		syncDepth := checkoutWith(t, syncJob, "fetch-depth")
		if syncDepth != 0 {
			t.Errorf("sync checkout fetch-depth = %v, want 0", syncDepth)
		}
		if got := checkoutWith(t, syncJob, "persist-credentials"); got != false {
			t.Errorf("sync checkout persist-credentials = %v, want false", got)
		}
		ciJob := ci.Jobs["verify"]
		if got := checkoutWith(t, ciJob, "persist-credentials"); got != false {
			t.Errorf("ci checkout persist-credentials = %v, want false", got)
		}
	})

	t.Run("sync is scheduled off the hour", func(t *testing.T) {
		schedule, ok := sync.On["schedule"].([]any)
		if !ok || len(schedule) != 1 {
			t.Fatalf("sync schedule = %v, want exactly one entry", sync.On["schedule"])
		}
		entry, ok := schedule[0].(map[string]any)
		if !ok {
			t.Fatalf("sync schedule entry = %v, want a cron mapping", schedule[0])
		}
		cron, ok := entry["cron"].(string)
		if !ok {
			t.Fatalf("sync schedule entry = %v, want a cron expression", entry)
		}
		minute, _, ok := strings.Cut(cron, " ")
		if !ok {
			t.Fatalf("cron %q has no minute field", cron)
		}
		value, err := strconv.Atoi(minute)
		if err != nil {
			t.Fatalf("cron minute %q: %v", minute, err)
		}
		// The busy minutes are the ones every other repository asks for. A run
		// scheduled there is the one most likely to be dropped under load.
		if value == 0 || value == 30 {
			t.Errorf("cron minute = %d, want a minute other repositories do not crowd", value)
		}
	})

	t.Run("sync serialises without cancelling", func(t *testing.T) {
		if sync.Concurrency.CancelInProgress {
			t.Error("sync cancels a run in progress, which would kill a backfill midway")
		}
		if sync.Concurrency.Group == "" || strings.Contains(sync.Concurrency.Group, "${{") {
			t.Errorf("sync concurrency group = %q, want one fixed group per repository", sync.Concurrency.Group)
		}
	})

	t.Run("sync writes with the workflow token", func(t *testing.T) {
		job := sync.Jobs["sync"]
		if job.Permissions["contents"] != "write" {
			t.Errorf("sync permissions = %v, want contents: write", job.Permissions)
		}
		if job.Permissions["actions"] != "read" {
			t.Errorf("sync permissions = %v, want actions: read", job.Permissions)
		}
		if len(job.Permissions) != 2 {
			t.Errorf("sync permissions = %v, want exactly contents and actions", job.Permissions)
		}
	})

	t.Run("sync exports the GITHUB_TOKEN only to execution steps", func(t *testing.T) {
		var exported []map[string]string
		for _, step := range sync.Jobs["sync"].Steps {
			if len(step.Env) > 0 {
				exported = append(exported, step.Env)
			}
		}
		if len(exported) != 2 {
			t.Fatalf("%d sync steps export environment variables, want verification and synchronization", len(exported))
		}
		want := map[string]string{"SOAPBOX_GITHUB_TOKEN": "${{ github.token }}"}
		for i, env := range exported {
			if len(env) != len(want) || env["SOAPBOX_GITHUB_TOKEN"] != want["SOAPBOX_GITHUB_TOKEN"] {
				t.Errorf("execution step %d exports %v, want %v", i, env, want)
			}
		}
	})

	t.Run("sync builds without token and selects one token execution", func(t *testing.T) {
		var commands []string
		var tokenSteps []workflowStep
		for _, step := range sync.Jobs["sync"].Steps {
			if step.Run != "" {
				commands = append(commands, step.Run)
				if len(step.Env) > 0 {
					tokenSteps = append(tokenSteps, step)
				}
			}
		}
		if len(commands) != 3 {
			t.Fatalf("sync runs %d commands, want build + write verification + synchronization: %q", len(commands), commands)
		}
		if !strings.HasPrefix(commands[0], "go build ") {
			t.Errorf("first command = %q, want go build", commands[0])
		}
		if len(tokenSteps) != 2 {
			t.Fatalf("expected verification and synchronization token steps, got %d", len(tokenSteps))
		}
		if !strings.Contains(commands[1], "-verify-write") || tokenSteps[0].If != "inputs.verify-write" {
			t.Errorf("write verification step = %#v", tokenSteps[0])
		}
		if !strings.Contains(commands[2], "-unattended") || tokenSteps[1].If != "${{ !inputs.verify-write }}" {
			t.Errorf("automatic synchronization step = %#v", tokenSteps[1])
		}
		for _, step := range tokenSteps {
			if strings.HasPrefix(step.Run, "go build") {
				t.Error("the build step should not receive the token")
			}
		}
		for _, cmd := range commands {
			if strings.ContainsAny(cmd, "|&;<>()") {
				t.Errorf("command %q composes shell rather than running one program", cmd)
			}
		}
	})
}

// checkoutWith reads one input of the checkout step.
func checkoutWith(tb testing.TB, job workflowJob, key string) any {
	tb.Helper()
	for _, step := range job.Steps {
		if !strings.HasPrefix(step.Uses, "actions/checkout@") {
			continue
		}
		value, ok := step.With[key]
		if !ok {
			tb.Fatalf("checkout step has no %s input", key)
		}
		return value
	}
	tb.Fatal("no checkout step")
	return nil
}

// decodeWorkflow reads one generated workflow as YAML.
func decodeWorkflow(tb testing.TB, path string) workflow {
	tb.Helper()
	var flow workflow
	if err := yaml.Unmarshal([]byte(readFile(tb, path)), &flow); err != nil {
		tb.Fatalf("decode %s: %v", path, err)
	}
	if flow.Name == "" {
		tb.Fatalf("%s decoded with no name, so the document did not parse as a workflow", path)
	}
	if len(flow.On) == 0 {
		tb.Fatalf("%s decoded with no triggers", path)
	}
	if len(flow.Jobs) == 0 {
		tb.Fatalf("%s decoded with no jobs", path)
	}
	return flow
}

// keysOf lists a map's keys for a failure message.
func keysOf[V any](m map[string]V) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	return names
}

// TestManualModeWorkflow covers a profile with publication.mode=manual.
func TestManualModeWorkflow(t *testing.T) {
	ctx := t.Context()

	// Build a template with a manual-mode profile.
	manualProfile := strings.Replace(fixtureProfile, "mode: automatic", "mode: manual", 1)
	root, git := newTemplate(ctx, t, map[string]string{
		config.DefaultFileName: manualProfile,
	})
	opts := newOptions(ctx, t, root, git)
	planned := plan(ctx, t, opts)
	if _, err := setup.Apply(ctx, opts, planned.Report.Hash); err != nil {
		t.Fatalf("apply: %v", err)
	}

	sync := decodeWorkflow(t, filepath.Join(root, ".github", "workflows", "sync.yml"))

	t.Run("manual mode does not pass -unattended", func(t *testing.T) {
		var commands []string
		for _, step := range sync.Jobs["sync"].Steps {
			if step.Run != "" {
				commands = append(commands, step.Run)
			}
		}
		if len(commands) != 2 {
			t.Fatalf("sync runs %d commands, want exactly two (build + run): %q", len(commands), commands)
		}
		// The run step (second command) must not have -unattended or -apply.
		runCmd := commands[1]
		if strings.Contains(runCmd, "-unattended") {
			t.Error("the manual mode sync workflow passes -unattended")
		}
		if strings.Contains(runCmd, "-apply") {
			t.Error("the manual mode sync workflow publishes without an approval")
		}
	})

	t.Run("manual mode accepts an environment-only approval input", func(t *testing.T) {
		dispatch, ok := sync.On["workflow_dispatch"].(map[string]any)
		if !ok {
			t.Fatalf("workflow_dispatch = %#v, want a mapping", sync.On["workflow_dispatch"])
		}
		inputs, ok := dispatch["inputs"].(map[string]any)
		if !ok {
			t.Fatalf("workflow_dispatch inputs = %#v", dispatch["inputs"])
		}
		approve, ok := inputs["approve"].(map[string]any)
		if !ok || approve["type"] != "string" || approve["required"] != false {
			t.Fatalf("approve input = %#v, want an optional string", inputs["approve"])
		}
		raw := readFile(t, filepath.Join(root, ".github", "workflows", "sync.yml"))
		if strings.Contains(raw, "-approve") || strings.Contains(raw, "${{ inputs.approve }}'") {
			t.Fatal("manual approval is interpolated into the command line")
		}
	})

	t.Run("manual mode exports token and approval", func(t *testing.T) {
		var exported map[string]string
		for _, step := range sync.Jobs["sync"].Steps {
			if len(step.Env) > 0 {
				exported = step.Env
			}
		}
		want := map[string]string{
			"SOAPBOX_GITHUB_TOKEN": "${{ github.token }}",
			"SOAPBOX_APPROVAL":     "${{ inputs.approve }}",
		}
		if len(exported) != len(want) {
			t.Fatalf("sync exports %v, want %v", exported, want)
		}
		for name, value := range want {
			if exported[name] != value {
				t.Errorf("sync exports %s = %q, want %q", name, exported[name], value)
			}
		}
	})

	t.Run("manual mode has contents:write and actions:read", func(t *testing.T) {
		job := sync.Jobs["sync"]
		if job.Permissions["contents"] != "write" {
			t.Errorf("sync permissions = %v, want contents: write", job.Permissions)
		}
		if job.Permissions["actions"] != "read" {
			t.Errorf("sync permissions = %v, want actions: read", job.Permissions)
		}
		if len(job.Permissions) != 2 {
			t.Errorf("sync permissions = %v, want exactly contents and actions", job.Permissions)
		}
	})

	t.Run("manual mode has the ref_protected guard", func(t *testing.T) {
		if want := "github.ref == 'refs/heads/main' && github.ref_protected"; sync.Jobs["sync"].If != want {
			t.Errorf("sync guard = %q, want %q", sync.Jobs["sync"].If, want)
		}
	})

	t.Run("manual mode pins every action", func(t *testing.T) {
		for _, job := range sync.Jobs {
			for _, step := range job.Steps {
				if step.Uses == "" {
					continue
				}
				if !pinnedUses.MatchString(step.Uses) {
					t.Errorf("step %q uses %q, which is not a full commit pin", step.Name, step.Uses)
				}
			}
		}
	})

	t.Run("manual mode has no id-token permission", func(t *testing.T) {
		job := sync.Jobs["sync"]
		if _, ok := job.Permissions["id-token"]; ok {
			t.Error("sync job has id-token permission")
		}
		if len(sync.Permissions) != 0 {
			t.Errorf("sync grants %v at the top level, want none", sync.Permissions)
		}
	})

	t.Run("manual mode has no App secrets", func(t *testing.T) {
		raw := readFile(t, filepath.Join(root, ".github", "workflows", "sync.yml"))
		if strings.Contains(raw, "secrets.") {
			t.Error("manual mode sync names a secret")
		}
	})
}
