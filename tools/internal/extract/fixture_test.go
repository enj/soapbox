package extract_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/closure"
	"github.com/enj/soapbox/tools/internal/config"
	"github.com/enj/soapbox/tools/internal/extract"
	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/testsupport"
)

const (
	fixtureUserName  = "Soapbox Test"
	fixtureUserEmail = "test@example.com"

	fixtureBranch = "master"
	fixtureTag    = "v1.36.1"
)

// fixtureProfile is a complete profile with the RBAC shape.
//
// It is written out rather than derived from the repository's own soapbox.yaml,
// because a test that read the real profile would keep passing while the profile
// drifted away from the shape these assertions describe, and because the fixture
// has to name a source repository the validator accepts while the run reads from
// a local mirror through -source-remote.
const fixtureProfile = `version: 2
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
    - pkg/apis/rbac/v1/zz_generated.conversion.go
    - pkg/apis/rbac/v1/zz_generated.deepcopy.go
    - pkg/registry/rbac/validation/internal_version_adapter.go
  required:
    - pkg/apis/rbac/v1/doc.go
    - pkg/apis/rbac/v1/evaluation_helpers.go
    - plugin/pkg/auth/authorizer/rbac/rbac.go
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

// fixtureFiles is the upstream tree the plan extracts from.
//
// The shape mirrors the real RBAC profile closely enough that the invariants
// under test are the ones the plan commits to: the closure reaches four packages
// and pruning leaves three, the internal API package disappears because its only
// importer was pruned rather than because any of its own files were, and the
// package set nests, so pkg/apis/rbac and pkg/apis/rbac/v1 both have to
// materialize while the sibling subpackages of each stay out.
var fixtureFiles = map[string]string{
	"plugin/pkg/auth/authorizer/rbac/rbac.go": `package rbac

import (
	"fmt"

	rbacv1helpers "k8s.io/kubernetes/pkg/apis/rbac/v1"
	rbacregistryvalidation "k8s.io/kubernetes/pkg/registry/rbac/validation"
)

// Authorizer answers authorization requests.
type Authorizer struct {
	resolver rbacregistryvalidation.RuleResolver
}

// Describe renders the authorizer.
func (a *Authorizer) Describe() string {
	return fmt.Sprintf("%v allows=%t", a.resolver, rbacv1helpers.RuleAllows())
}
`,
	// A sibling subpackage of the configured root. Package granularity means it
	// never enters the closure, and the sparse pattern set means it is never
	// even materialized.
	"plugin/pkg/auth/authorizer/rbac/bootstrappolicy/policy.go": `package bootstrappolicy

// Policy is the bootstrap policy nobody asked for.
type Policy struct{}
`,
	// A companion build input rather than Go source. It is committed executable
	// so the plan has to carry a mode through the closure, the relocation, and
	// the manifest without rounding it off.
	"plugin/pkg/auth/authorizer/rbac/rbac_amd64.s": `// Placeholder assembly. The plan copies it; nothing here assembles it.
`,
	"pkg/registry/rbac/validation/rule.go": `package validation

import rbacv1 "k8s.io/api/rbac/v1"

// RuleResolver resolves the rules a subject holds.
type RuleResolver interface {
	Rules() []rbacv1.PolicyRule
}
`,
	// Pruned. It is the sole importer of the internal API package, which is why
	// pruning it makes that package disappear from the closure.
	"pkg/registry/rbac/validation/internal_version_adapter.go": `package validation

import rbacinternal "k8s.io/kubernetes/pkg/apis/rbac"

// Adapt returns an internal rule.
func Adapt() rbacinternal.PolicyRule {
	return rbacinternal.PolicyRule{}
}
`,
	"pkg/apis/rbac/types.go": `package rbac

// PolicyRule is the internal rule type.
type PolicyRule struct{}
`,
	// Retained rather than pruned, so the plan has to report a surviving file
	// as generated. Nothing else in the fixture carries the marker into the
	// output tree.
	"pkg/registry/rbac/validation/zz_generated.deepcopy.go": `// Code generated by deepcopy-gen. DO NOT EDIT.

package validation

// DeepCopyResolver copies nothing.
func DeepCopyResolver() {}
`,
	// A sibling subpackage of pkg/apis/rbac. Nothing imports it, and the nested
	// pattern set must keep it out even though its parent is materialized.
	"pkg/apis/rbac/v1beta1/types.go": `package v1beta1

// PolicyRule is the beta rule type.
type PolicyRule struct{}
`,
	"pkg/apis/rbac/v1/doc.go": `// +k8s:conversion-gen=k8s.io/kubernetes/pkg/apis/rbac
// +k8s:conversion-gen-external-types=k8s.io/api/rbac/v1
// +k8s:deepcopy-gen=package
// +groupName=rbac.authorization.k8s.io

package v1
`,
	"pkg/apis/rbac/v1/evaluation_helpers.go": `package v1

import rbacv1 "k8s.io/api/rbac/v1"

// RuleAllows reports whether any rule matches.
func RuleAllows() bool {
	return len([]rbacv1.PolicyRule{}) == 0
}
`,
	// Pruned, and carrying the generated file marker.
	"pkg/apis/rbac/v1/zz_generated.conversion.go": `//go:build !ignore_autogenerated

// Code generated by conversion-gen. DO NOT EDIT.

package v1

import rbacinternal "k8s.io/kubernetes/pkg/apis/rbac"

// Convert converts an external rule to an internal one.
func Convert() rbacinternal.PolicyRule {
	return rbacinternal.PolicyRule{}
}
`,
	// Pruned. Its removal is the evidence that strips the deepcopy-gen marker
	// from doc.go.
	"pkg/apis/rbac/v1/zz_generated.deepcopy.go": `// Code generated by deepcopy-gen. DO NOT EDIT.

package v1

// DeepCopyInto copies nothing.
func DeepCopyInto() {}
`,
}

// fixtureModes are the files the fixture commits with a mode other than the
// default. Git records only the executable bit for a blob, so this is the whole
// range a relocated tree can carry.
var fixtureModes = map[string]os.FileMode{
	"plugin/pkg/auth/authorizer/rbac/rbac_amd64.s": 0o755,
}

// upstream is a real repository serving the fixture over file://.
type upstream struct {
	repo   *testsupport.Repo
	commit string
}

// newUpstream builds the fixture repository.
//
// Object filtering is enabled because the cache is a blobless partial clone and
// source.Open proves the filter took effect: a server that ignored it would hand
// back every blob and the audit would refuse the cache it just created.
func newUpstream(ctx context.Context, t *testing.T) *upstream {
	t.Helper()
	return newUpstreamWith(ctx, t, nil)
}

// newUpstreamWith builds the fixture repository with some files replaced, which
// is how a test states the upstream shape it needs without disturbing the one
// every other test asserts against.
func newUpstreamWith(ctx context.Context, t *testing.T, overrides map[string]string) *upstream {
	t.Helper()

	repo := testsupport.NewRepo(ctx, t, testsupport.Options{
		Branch:    fixtureBranch,
		UserName:  fixtureUserName,
		UserEmail: fixtureUserEmail,
	})
	repo.SetConfig(ctx, t, "uploadpack.allowFilter", "true")

	files := make(map[string]string, len(fixtureFiles)+len(overrides))
	maps.Copy(files, fixtureFiles)
	maps.Copy(files, overrides)

	paths := make([]string, 0, len(files))
	for path, contents := range files {
		repo.WriteFile(t, path, contents)
		if mode, ok := fixtureModes[path]; ok {
			if err := os.Chmod(filepath.Join(repo.Dir, filepath.FromSlash(path)), mode); err != nil {
				t.Fatalf("set mode of %s: %v", path, err)
			}
		}
		paths = append(paths, path)
	}
	slices.Sort(paths)
	commit := repo.Commit(ctx, t, "feat: add the rbac authorizer\n", gitcli.CommitOptions{}, paths...)

	if err := repo.Git.CreateTag(ctx, gitcli.TagOptions{
		Name:    fixtureTag,
		Commit:  commit,
		Message: "Kubernetes " + fixtureTag + "\n",
		Tagger:  gitcli.Signature{Name: fixtureUserName, Email: fixtureUserEmail, Date: "2026-01-02T03:04:05Z"},
	}); err != nil {
		t.Fatalf("create tag: %v", err)
	}
	return &upstream{repo: repo, commit: commit}
}

func (u *upstream) url() string { return "file://" + u.repo.Dir }

// profileDir writes the fixture profile into a fresh directory and returns it.
func profileDir(t *testing.T, profile string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, config.DefaultFileName), []byte(profile), 0o600); err != nil {
		t.Fatalf("write profile: %v", err)
	}
	return dir
}

// loadProfile decodes a profile through the real validator, so a fixture that
// drifted out of the schema fails as a bad fixture rather than as a bad plan.
func loadProfile(t *testing.T, profile string) *config.Config {
	t.Helper()
	cfg, err := config.Decode([]byte(profile))
	if err != nil {
		t.Fatalf("decode fixture profile: %v", err)
	}
	return cfg
}

// planOptions builds a complete option set pointed at a fresh set of
// directories, which is what lets one test run the same plan twice in different
// places and compare the results.
func planOptions(ctx context.Context, t *testing.T, up *upstream, profile string) extract.Options {
	t.Helper()
	root := t.TempDir()
	git, err := gitcli.New(ctx, gitcli.Options{Inherit: []string{"PATH"}})
	if err != nil {
		t.Fatalf("create git runner: %v", err)
	}
	return extract.Options{
		Config:       loadProfile(t, profile),
		ProfileDir:   profileDir(t, profile),
		CacheRoot:    filepath.Join(root, "cache"),
		WorkRoot:     filepath.Join(root, "work"),
		OutputRoot:   filepath.Join(root, "tree"),
		Ref:          extract.Ref{Kind: extract.RefTag, Name: fixtureTag},
		PatchBranch:  fixtureBranch,
		SourceRemote: up.url(),
		Fetch:        true,
		Git:          git,
		// The fixture profile names the real environment variables, and a
		// developer machine or CI runner may legitimately hold them. Reporting
		// none keeps the credential check under test in the one test that
		// exercises it.
		LookupEnv: func(string) (string, bool) { return "", false },
	}
}

// mustPlan runs a plan that is expected to succeed.
func mustPlan(ctx context.Context, t *testing.T, opts extract.Options) *extract.Result {
	t.Helper()
	result, err := extract.Plan(ctx, opts)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	return result
}

// destinations lists the relocated destination paths of a result.
func destinations(result *extract.Result) []string {
	out := make([]string, 0, len(result.Files.Files))
	for _, file := range result.Files.Files {
		out = append(out, file.Path)
	}
	return out
}

// contentsOf reports one relocated file's final bytes.
func contentsOf(t *testing.T, result *extract.Result, destination string) string {
	t.Helper()
	file, ok := result.Files.Lookup(destination)
	if !ok {
		t.Fatalf("%q is not in the relocated set:\n  %s", destination, strings.Join(destinations(result), "\n  "))
	}
	return string(file.Contents)
}

// assertEqual compares two string lists and reports the difference.
func assertEqual(t *testing.T, what string, got, want []string) {
	t.Helper()
	if slices.Equal(got, want) {
		return
	}
	t.Errorf("%s:\n got %q\nwant %q", what, got, want)
}

// worktreePaths lists the repository relative files a materialized work tree
// holds, which is what proves the sparse pattern set did what it claims.
func worktreePaths(t *testing.T, dir string) []string {
	t.Helper()
	if dir == "" {
		t.Fatal("the work tree was removed, so its contents cannot be inspected")
	}
	return walkFiles(t, dir, func(name string) bool { return name == ".git" })
}

// treePaths lists the module relative files a materialized output tree holds.
func treePaths(t *testing.T, dir string) []string {
	t.Helper()
	return walkFiles(t, dir, func(string) bool { return false })
}

// walkFiles lists every regular file below dir as a sorted slash path.
func walkFiles(t *testing.T, dir string, skip func(string) bool) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if skip(entry.Name()) {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	slices.Sort(out)
	return out
}

// credentialedRunner builds a runner carrying a caller supplied environment
// entry, which is what a publishing runner looks like.
//
// The entry is a neutral name rather than one of gitcli's fixed, security
// relevant variables. Any entry at all makes the runner non-anonymous, which is
// the whole of what this fixture needs, and Plan refuses such a runner during
// validation without ever running a Git command. Naming GIT_ASKPASS instead
// would be a real override rather than a simulated credential: assembleEnv
// appends caller entries after fixedEnv, so the fixture would be undoing the
// neutralisation that makes a missing credential fail instead of prompt.
func credentialedRunner(ctx context.Context, t *testing.T) *gitcli.Runner {
	t.Helper()
	git, err := gitcli.New(ctx, gitcli.Options{
		Inherit: []string{"PATH"},
		Env:     []string{"SOAPBOX_TOKEN=fixture-credential"},
	})
	if err != nil {
		t.Fatalf("create git runner: %v", err)
	}
	if git.IsAnonymous() {
		t.Fatal("the fixture runner is supposed to look credentialed")
	}
	return git
}

// fixtureGolden is the closure golden path every fixture profile pins. The
// profile directory a test builds does not hold it unless the test writes one,
// so the default fixture exercises the absent case.
const fixtureGolden = "testdata/closure/fixture.json"

// writeGolden stores a closure report as the profile's pinned golden.
//
// The bytes come from a real run rather than from a checked-in literal, because
// a golden written by hand would drift from the closure the engine produces and
// the test would then be asserting on the drift.
func writeGolden(t *testing.T, profileDir string, report closure.ClosureReport) {
	t.Helper()
	encoded, err := report.JSON()
	if err != nil {
		t.Fatalf("encode golden: %v", err)
	}
	full := filepath.Join(profileDir, filepath.FromSlash(fixtureGolden))
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		t.Fatalf("create golden directory: %v", err)
	}
	if err := os.WriteFile(full, encoded, 0o600); err != nil {
		t.Fatalf("write golden: %v", err)
	}
}

// writeGoldenBytes stores exact bytes as the profile's pinned golden, which is
// how a test states a malformed one.
func writeGoldenBytes(t *testing.T, profileDir string, contents []byte) {
	t.Helper()
	full := filepath.Join(profileDir, filepath.FromSlash(fixtureGolden))
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		t.Fatalf("create golden directory: %v", err)
	}
	if err := os.WriteFile(full, contents, 0o600); err != nil {
		t.Fatalf("write golden: %v", err)
	}
}

// planFailure runs a plan that is expected to fail and returns both halves.
func planFailure(ctx context.Context, t *testing.T, opts extract.Options) (*extract.Result, error) {
	t.Helper()
	result, err := extract.Plan(ctx, opts)
	if err == nil {
		t.Fatal("the plan was expected to fail")
	}
	return result, err
}

// mustPolicy requires a failure the command line reports as a finding, raised
// by the phase that was supposed to refuse.
//
// The stage is part of the assertion rather than an extra check a caller may
// forget. A run that refused for the right reason at the wrong phase is a run
// whose gates fire in an order nobody chose, and the exit code alone cannot
// tell the two apart.
func mustPolicy(t *testing.T, err error, stage string) {
	t.Helper()
	var policy *extract.PolicyError
	if !errors.As(err, &policy) {
		t.Fatalf("error %v is not a policy failure", err)
	}
	if policy.Stage != stage {
		t.Fatalf("the %s stage refused, want %s: %v", policy.Stage, stage, err)
	}
}

// The fixture's embedding variant.
//
// It exists to separate the two things a .go file can be. samples embeds a
// directory holding a file named like Go source that is not Go source, and
// notes embeds every text file beside it, which is the pattern that would
// capture a provenance record written into the same directory.
//
// The package stands alone under a recursive root, because package granular
// sparse checkout materializes a package's own files and not its
// subdirectories, so an embedded directory only exists in the work tree of a
// recursive profile. The go tool never loads a package from testdata and
// neither does the closure, so the recursive root still expands to one package.
const (
	embedOnlyGo = `package only

import "embed"

//go:embed testdata
var samples embed.FS

//go:embed *.txt
var notes embed.FS

// Thing is the type the facade exports.
type Thing struct{}

// Assets reports the embedded sets so neither variable is unused.
func Assets() (embed.FS, embed.FS) { return samples, notes }
`

	// embeddedGoAsset is named like Go source and is not Go source. A plan that
	// parsed, relocated, formatted, or annotated it would either fail outright
	// or change bytes the published module is supposed to serve unchanged.
	embeddedGoAsset = `This file is named .go and is embedded data.

package k8s.io/kubernetes -- not a package clause, not parsable, not source.
import "k8s.io/kubernetes/pkg/apis/rbac" -- not an import either.
`

	// embeddedNote is the upstream text file the broad pattern exists to match,
	// so displacing the provenance record does not leave the pattern matching
	// nothing.
	embeddedNote = "upstream note, embedded by the broad pattern.\n"

	// embedPruned is the second file in the package, so the profile has
	// something to prune without pruning a package's last file.
	embedPruned = "package only\n\n// Pruned is removed before the closure settles.\ntype Pruned struct{}\n"
)

// embedProfile plans the standalone embedding package.
var embedProfile = strings.NewReplacer(
	"    - plugin/pkg/auth/authorizer/rbac\n", "    - pkg/only\n",
	"  recursive: false\n", "  recursive: true\n",
	"    - pkg/apis/rbac/v1/zz_generated.conversion.go\n", "    - pkg/only/pruned.go\n",
	"    - pkg/apis/rbac/v1/zz_generated.deepcopy.go\n", "",
	"    - pkg/registry/rbac/validation/internal_version_adapter.go\n", "",
	"    - pkg/apis/rbac/v1/doc.go\n", "    - pkg/only/only.go\n",
	"    - pkg/apis/rbac/v1/evaluation_helpers.go\n", "",
	"    - plugin/pkg/auth/authorizer/rbac/rbac.go\n", "",
	"    - name: Authorizer\n", "    - name: Thing\n",
	"      source: k8s.io/kubernetes/plugin/pkg/auth/authorizer/rbac.Authorizer\n",
	"      source: k8s.io/kubernetes/pkg/only.Thing\n",
).Replace(fixtureProfile)

// embedFiles is the standalone embedding package's upstream tree.
var embedFiles = map[string]string{
	"pkg/only/only.go":           embedOnlyGo,
	"pkg/only/pruned.go":         embedPruned,
	"pkg/only/notes.txt":         embeddedNote,
	"pkg/only/testdata/asset.go": embeddedGoAsset,
}

// embeddingUpstream builds the fixture with the embedding package in place.
func embeddingUpstream(ctx context.Context, t *testing.T) *upstream {
	t.Helper()
	return newUpstreamWith(ctx, t, embedFiles)
}

// narrowProfile plans one package of the fixture instead of the RBAC closure.
//
// It exists to prime a cache whose blobs cover part of the repository and not
// the rest, which is the state an offline run has to fail closed on rather than
// quietly complete by fetching.
var narrowProfile = strings.NewReplacer(
	"    - plugin/pkg/auth/authorizer/rbac\n", "    - pkg/apis/rbac/v1\n",
	"    - pkg/apis/rbac/v1/zz_generated.conversion.go\n", "",
	"    - pkg/registry/rbac/validation/internal_version_adapter.go\n", "",
	"    - plugin/pkg/auth/authorizer/rbac/rbac.go\n", "",
	"    - k8s.io/kubernetes/pkg/apis/rbac\n", "    - k8s.io/kubernetes/pkg/registry/rbac/validation\n",
	"    - name: Authorizer\n", "    - name: RuleAllows\n",
	"      kind: type\n", "      kind: func\n",
	"      source: k8s.io/kubernetes/plugin/pkg/auth/authorizer/rbac.Authorizer\n",
	"      source: k8s.io/kubernetes/pkg/apis/rbac/v1.RuleAllows\n",
).Replace(fixtureProfile)

// chainLength is how many packages the import chain fixture holds.
//
// It is comfortably past the fixed bound the widening loop used to carry, which
// is the whole point: a profile whose closure legitimately reaches this many
// packages must be refused by its own limits or not at all.
const chainLength = 70

// chainProfile plans a long import chain instead of the RBAC shape.
//
// Only the fields the closure reads differ from the RBAC fixture. The limits are
// generous because this profile exists to prove that the package ceiling, rather
// than a number the engine chose, is what bounds widening.
var chainProfile = strings.NewReplacer(
	"    - plugin/pkg/auth/authorizer/rbac\n", "    - pkg/chain/p00\n",
	"    - pkg/apis/rbac/v1/zz_generated.conversion.go\n", "    - pkg/chain/p00/pruned.go\n",
	"    - pkg/apis/rbac/v1/zz_generated.deepcopy.go\n", "",
	"    - pkg/registry/rbac/validation/internal_version_adapter.go\n", "",
	"    - pkg/apis/rbac/v1/doc.go\n", "    - pkg/chain/p00/p00.go\n",
	"    - pkg/apis/rbac/v1/evaluation_helpers.go\n", "",
	"    - plugin/pkg/auth/authorizer/rbac/rbac.go\n", "",
	"    maxPackages: 8\n", "    maxPackages: 200\n",
	"    maxFiles: 40\n", "    maxFiles: 400\n",
	"    maxPackageGrowth: 2\n", "    maxPackageGrowth: 200\n",
	"    - name: Authorizer\n", "    - name: Thing\n",
	"      source: k8s.io/kubernetes/plugin/pkg/auth/authorizer/rbac.Authorizer\n",
	"      source: k8s.io/kubernetes/pkg/chain/p00.Thing\n",
).Replace(fixtureProfile)

// chainFiles renders an import chain of the given length.
//
// Each package imports exactly the next one, so the closure discovers one
// package per widening round and the number of rounds is the chain's length.
func chainFiles(length int) map[string]string {
	files := make(map[string]string, length+1)
	for i := range length {
		name := chainPackage(i)
		var b strings.Builder
		fmt.Fprintf(&b, "package %s\n\n", name)
		if i+1 < length {
			fmt.Fprintf(&b, "import next %q\n\n", "k8s.io/kubernetes/pkg/chain/"+chainPackage(i+1))
			fmt.Fprintf(&b, "// Thing links to the next package in the chain.\ntype Thing struct{ Next next.Thing }\n")
		} else {
			fmt.Fprintf(&b, "// Thing ends the chain.\ntype Thing struct{}\n")
		}
		files["pkg/chain/"+name+"/"+name+".go"] = b.String()
	}
	// The root package carries a second file so the profile has something to
	// prune without pruning a package's last file.
	files["pkg/chain/p00/pruned.go"] = "package p00\n\n// Pruned is removed before the closure settles.\ntype Pruned struct{}\n"
	return files
}

// chainPackage renders one chain package's name.
func chainPackage(index int) string { return fmt.Sprintf("p%02d", index) }
