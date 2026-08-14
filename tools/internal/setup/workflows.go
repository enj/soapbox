package setup

import (
	"fmt"
	"regexp"
	"strings"
)

// pinnedAction is one third party action and the exact commit it is pinned to.
//
// A tag is not a pin. GitHub tags move, and an action that moved is arbitrary
// code running with whatever permissions the job holds, so the SHA is the
// contract and the tag beside it is a comment for the reader. Both are recorded
// here rather than resolved at generation time, because resolving a tag over the
// network would make the generated workflow depend on when setup ran.
type pinnedAction struct {
	// Name is the owner/repository of the action.
	Name string
	// SHA is the full forty character commit the workflow pins.
	SHA string
	// Tag is the release that commit was, recorded only as a comment.
	Tag string
}

// Ref renders the pinned reference a workflow step uses.
func (a pinnedAction) Ref() string {
	return fmt.Sprintf("%s@%s # %s", a.Name, a.SHA, a.Tag)
}

// The actions the generated workflows run. Both SHAs were read from the
// repositories they belong to at the tags named beside them.
var (
	actionCheckout = pinnedAction{Name: "actions/checkout", SHA: "3d3c42e5aac5ba805825da76410c181273ba90b1", Tag: "v7.0.1"}
	actionSetupGo  = pinnedAction{Name: "actions/setup-go", SHA: "b7ad1dad31e06c5925ef5d2fc7ad053ef454303e", Tag: "v7.0.0"}
)

// syncSchedule is the cron expression the publishing workflow runs on.
//
// The minute is deliberately not zero. Every repository that asks for a nightly
// run lands on the hour, so the platform sheds load exactly there, and a
// scheduled workflow that is dropped is a sync that silently did not happen.
const syncSchedule = "37 4 * * *"

// syncConcurrency is the group the publishing workflow serialises on. It is a
// constant rather than an expression because there is one publishing pipeline
// per repository, and a group that varied by ref would let two runs publish at
// once.
const syncConcurrency = "soapbox-sync"

// SyncBuildCommand builds the engine shim without any token in the
// environment, so the Go build and module-download subprocess never sees the
// publishing credential.
const SyncBuildCommand = "go build -o ${{ runner.temp }}/soapbox ./cmd/soapbox"

// syncRunCommand is the pre-built binary invocation the sync step runs. Only
// this step receives SOAPBOX_GITHUB_TOKEN.
const syncRunCommand = "${{ runner.temp }}/soapbox sync -dir .. -destination .. -cache ${{ runner.temp }}/soapbox-cache"

// SyncCommand is the full engine invocation documented for the sync workflow.
// It is the run command; the build step is separate and carries no token.
//
// Every decision about what is published is the engine's. The workflow
// contributes a checkout, a toolchain, the job-scoped GITHUB_TOKEN, and the
// two directories the engine may not choose for itself. The build step
// produces the binary without credentials; only the run step receives the
// token.
//
// The base invocation carries no -apply. Automatic mode adds -unattended,
// which is the approved self-apply path for scheduled runs.
//
// The flags named here are asserted against the ones the sync command actually
// defines, in the command layer that owns both, so the two cannot drift apart
// silently.
const SyncCommand = syncRunCommand

// VerifyCommand is the read-only engine invocation the CI workflow runs.
const VerifyCommand = "go run ./cmd/soapbox validate -dir .."

// shaPattern matches a full commit object name, which is the only pin a
// workflow may name.
var shaPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// branchPattern matches the branch names the generated workflows may name. It is
// narrower than what Git accepts on purpose: this value is interpolated into a
// YAML scalar and into a workflow expression, and a branch name that needed
// quoting to survive either one would be a branch name that could change what the
// workflow means.
var branchPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

// goVersionPattern matches the toolchain version the setup-go action installs.
var goVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+(\.[0-9]+)?$`)

// workflowInputs are the profile derived values the workflows interpolate.
type workflowInputs struct {
	branch    string
	goVersion string
	mode      string
}

// checkWorkflowInputs refuses any value that could change what the generated
// YAML means.
//
// Everything interpolated into a workflow comes from the profile, which is
// checked in, reviewed, and validated. That still is not a reason to interpolate
// it unchecked: a workflow is the one generated artifact that runs with
// credentials, so a value reaching it is checked against what it is allowed to
// be rather than against what would be obviously wrong.
func (w workflowInputs) check() error {
	if !branchPattern.MatchString(w.branch) {
		return fmt.Errorf("workflow: branch %q is not a plain branch name", w.branch)
	}
	if !goVersionPattern.MatchString(w.goVersion) {
		return fmt.Errorf("workflow: Go version %q is not a release version", w.goVersion)
	}
	for _, action := range []pinnedAction{actionCheckout, actionSetupGo} {
		if !shaPattern.MatchString(action.SHA) {
			return fmt.Errorf("workflow: %s is not pinned to a full commit", action.Name)
		}
	}
	return nil
}

// goVersionOf turns a pinned toolchain such as go1.26.5 into the version the
// setup-go action installs.
func goVersionOf(toolchain string) string {
	return strings.TrimPrefix(toolchain, "go")
}

// composeCIWorkflow renders the verification workflow.
//
// It never receives write credentials. Pull request code runs here, so a token
// this job held would be a token any contributor could reach, and the checkout
// is told not to persist one at all so a later step cannot use it by accident.
func composeCIWorkflow(in workflowInputs) []byte {
	var b strings.Builder
	b.WriteString(`# Generated by soapbox setup. Read-only verification.
#
# This workflow runs on pull request code and therefore holds no credential: no
# write token is available to it, its token may only read, and the checkout keeps
# no token in the work tree for a later step to find.
name: ci

on:
  push:
    branches:
      - '`)
	b.WriteString(in.branch)
	b.WriteString(`'
  pull_request:
    branches:
      - '`)
	b.WriteString(in.branch)
	b.WriteString(`'

permissions: {}

concurrency:
  group: ci-${{ github.ref }}
  cancel-in-progress: true

jobs:
  verify:
    runs-on: ubuntu-latest
    timeout-minutes: 30
    permissions:
      contents: read
    steps:
      - name: Check out the repository
        uses: `)
	b.WriteString(actionCheckout.Ref())
	b.WriteString(`
        with:
          persist-credentials: false
      - name: Set up Go
        uses: `)
	b.WriteString(actionSetupGo.Ref())
	b.WriteString(`
        with:
          go-version: '`)
	b.WriteString(in.goVersion)
	b.WriteString(`'
          check-latest: false
      - name: Build
        run: go build ./...
      - name: Vet
        run: go vet ./...
      - name: Test
        run: go test ./...
      - name: Build the engine shim
        working-directory: tools
        run: go build ./...
      - name: Vet the engine shim
        working-directory: tools
        run: go vet ./...
      - name: Test the engine shim
        working-directory: tools
        run: go test ./...
      - name: Validate the profile
        working-directory: `)
	b.WriteString(toolsDirName)
	b.WriteString(`
        run: `)
	b.WriteString(VerifyCommand)
	b.WriteString("\n")
	return []byte(b.String())
}

// composeSyncWorkflow renders the publishing workflow.
//
// Five properties are load bearing and each is visible in the rendered YAML.
// Publishing runs only from the protected default branch, so a fork or a topic
// branch cannot reach the credentials. There is no pull_request_target trigger,
// which is the one trigger that would hand a fork's code the repository's own
// secrets. One non-cancelling concurrency group means a long backfill is never
// killed midway by the next scheduled run. The job contains exactly one Go
// invocation, so every decision about what gets published is the engine's and is
// reviewable in Go rather than spread across workflow steps.
//
// The workflow uses the built-in GITHUB_TOKEN with contents:write permission.
// The token is passed as SOAPBOX_GITHUB_TOKEN so the engine can authenticate
// pushes without requiring a separate GitHub App installation.
func composeSyncWorkflow(in workflowInputs) []byte {
	var b strings.Builder
	b.WriteString(`# Generated by soapbox setup. Publishing.
#
# This is the only workflow that writes to the repository. It runs on a schedule
# or an explicitly authorized manual dispatch, never on pull request code, and
# the job refuses to run from any ref but the protected default branch. All of
# the maintained logic is one Go invocation; the workflow supplies a checkout, a
# toolchain, and the GITHUB_TOKEN for authenticated pushes.
`)
	if in.mode == "manual" {
		b.WriteString(`#
# An empty approval input is plan-only. After reviewing its exact hash, an
# operator may dispatch again with that hash; it travels in the environment,
# never in the command line. Automatic mode is the only self-approval path.
`)
	} else {
		b.WriteString(`#
# The invocation runs unattended: a scheduled run publishes without further
# human intervention once the profile has been approved.
`)
	}
	b.WriteString(`name: sync

on:
  schedule:
    - cron: '`)
	b.WriteString(syncSchedule)
	b.WriteString("'\n")
	if in.mode == "manual" {
		b.WriteString(`  workflow_dispatch:
    inputs:
      approve:
        description: Exact reconciliation plan hash to apply; empty plans only
        required: false
        type: string
`)
	} else {
		b.WriteString(`  workflow_dispatch:
    inputs:
      verify-write:
        description: Verify GITHUB_TOKEN write access without processing releases
        required: false
        default: false
        type: boolean
`)
	}
	b.WriteString(`
permissions: {}

concurrency:
  group: `)
	b.WriteString(syncConcurrency)
	b.WriteString(`
  cancel-in-progress: false

jobs:
  sync:
    timeout-minutes: 180
    if: github.ref == 'refs/heads/`)
	b.WriteString(in.branch)
	b.WriteString(`' && github.ref_protected
    runs-on: ubuntu-latest
    permissions:
      contents: write
      actions: read
    steps:
      - name: Check out the repository
        uses: `)
	b.WriteString(actionCheckout.Ref())
	b.WriteString(`
        with:
          fetch-depth: 0
          persist-credentials: false
      - name: Set up Go
        uses: `)
	b.WriteString(actionSetupGo.Ref())
	b.WriteString(`
        with:
          go-version: '`)
	b.WriteString(in.goVersion)
	b.WriteString(`'
          check-latest: false
      - name: Build the engine shim
        working-directory: `)
	b.WriteString(toolsDirName)
	b.WriteString(`
        run: `)
	b.WriteString(SyncBuildCommand)
	if in.mode == "manual" {
		b.WriteString(`
      - name: Synchronize with upstream
        working-directory: `)
		b.WriteString(toolsDirName)
		b.WriteString(`
        env:
          SOAPBOX_GITHUB_TOKEN: ${{ github.token }}
          SOAPBOX_APPROVAL: ${{ inputs.approve }}
        run: `)
		b.WriteString(syncRunCommand)
		b.WriteString("\n")
	} else {
		b.WriteString(`
      - name: Verify write access
        if: inputs.verify-write
        working-directory: `)
		b.WriteString(toolsDirName)
		b.WriteString(`
        env:
          SOAPBOX_GITHUB_TOKEN: ${{ github.token }}
        run: `)
		b.WriteString(syncRunCommand)
		b.WriteString(` -verify-write
      - name: Synchronize with upstream
        if: ${{ !inputs.verify-write }}
        working-directory: `)
		b.WriteString(toolsDirName)
		b.WriteString(`
        env:
          SOAPBOX_GITHUB_TOKEN: ${{ github.token }}
        run: `)
		b.WriteString(syncRunCommand)
		b.WriteString(" -unattended\n")
	}
	return []byte(b.String())
}
