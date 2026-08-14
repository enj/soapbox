package setup_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/config"
	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/setup"
	"github.com/enj/soapbox/tools/internal/testsupport"
)

// engineRelease is the immutable engine release the fixtures pin.
const engineRelease = "tools/v1.4.2"

// templateFiles is the tracked content of a soapbox template checkout.
//
// It is a miniature of the real repository rather than a copy of it: every path
// setup classifies is represented, including one of each kind it must delete,
// keep, compose over, and preserve. Reading the real repository instead would
// make these tests fail whenever the real repository changed for reasons that
// have nothing to do with the transformation.
var templateFiles = map[string]string{
	// Markers. The template is recognisable by these and by the root go.mod it
	// does not have.
	config.DefaultFileName:      fixtureProfile,
	"plans/implementation.md":   "# implementation plan\n",
	"tools/soapbox.go":          "package soapbox\n",
	"tools/internal/cli/cli.go": "package cli\n",
	"tools/cmd/soapbox/main.go": "package main\n\nfunc main() {}\n",

	// Template owned, removed by setup.
	"CLAUDE.md":             "# project instructions\n",
	".golangci.yml":         "version: \"2\"\n",
	".claude/settings.json": "{}\n",
	".serena/project.yml":   "name: soapbox\n",
	"docs/setup.md":         "# setup\n",
	"plans/goal.md":         "# goal\n",
	"tools/soapbox_test.go": "package soapbox\n",
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

	// Retained, untouched by setup.
	"LICENSE":            "Apache License 2.0\n",
	"NOTICE":             "notice\n",
	"README.md":          "# soapbox\n",
	".gitignore":         "/bin/\n",
	".gitattributes":     "* text=auto eol=lf\n",
	"patches/index.yaml": "patches: []\n",
	"patches/README.md":  "# patches\n",
}

// newTemplate builds a committed template checkout and the runner that drives
// it.
func newTemplate(ctx context.Context, tb testing.TB, extra map[string]string) (string, *gitcli.Runner) {
	tb.Helper()

	repo := testsupport.NewRepo(ctx, tb, testsupport.Options{
		UserName:  "Soapbox Test",
		UserEmail: "test@example.invalid",
	})
	for path, contents := range templateFiles {
		repo.WriteFile(tb, path, contents)
	}
	for path, contents := range extra {
		repo.WriteFile(tb, path, contents)
	}
	repo.Commit(ctx, tb, "chore: template", gitcli.CommitOptions{}, ".")

	// The root is resolved because a temporary directory may itself be reached
	// through a symbolic link, which git reports resolved and the caller does not.
	root, err := filepath.EvalSymlinks(repo.Dir)
	if err != nil {
		tb.Fatalf("resolve repository: %v", err)
	}
	return root, repo.Git
}

// newOptions loads the fixture profile and builds the options one setup runs
// under.
func newOptions(ctx context.Context, tb testing.TB, root string, git *gitcli.Runner) setup.Options {
	tb.Helper()

	cfg, err := config.Load(ctx, filepath.Join(root, config.DefaultFileName))
	if err != nil {
		tb.Fatalf("load profile: %v", err)
	}
	engineMod, err := os.ReadFile(filepath.Join(root, "tools", "go.mod"))
	if err != nil {
		tb.Fatalf("read engine go.mod: %v", err)
	}
	return setup.Options{
		Root:          root,
		Config:        cfg,
		EngineVersion: engineRelease,
		EngineMod:     engineMod,
		Git:           git,
	}
}

// plan is the dry run every test starts from.
func plan(ctx context.Context, tb testing.TB, opts setup.Options) *setup.Result {
	tb.Helper()

	result, err := setup.Plan(ctx, opts)
	if err != nil {
		tb.Fatalf("plan: %v", err)
	}
	return result
}

// actionPaths lists the manifest paths of one kind.
func actionPaths(result *setup.Result, kind string) []string {
	var paths []string
	for _, action := range result.Report.Actions {
		if action.Kind == kind {
			paths = append(paths, action.Path)
		}
	}
	return paths
}

// engineSumFor renders a well formed go.sum covering one engine release. The
// hashes are not real checksums and no test asks the toolchain to verify them;
// what is under test is that setup refuses content that does not cover the pin
// and writes content that does.
func engineSumFor(version string) []byte {
	modules := []string{
		setup.EngineModulePath + " " + version,
		"golang.org/x/mod v0.39.0",
		"golang.org/x/sync v0.22.0",
		"golang.org/x/tools v0.48.0",
		"gopkg.in/yaml.v3 v3.0.1",
	}
	var lines []string
	for _, module := range modules {
		lines = append(lines,
			module+" h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
			module+"/go.mod h1:BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB=",
		)
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

// fixtureProfile is a complete, valid extraction profile. Setup reads only the
// destination module, the branch, the facade file names, the internal prefix,
// the toolchain, and the publication mode from it, but the profile has to
// validate as a whole for the command to reach setup at all.
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
