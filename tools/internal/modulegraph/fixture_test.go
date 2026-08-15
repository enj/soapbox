package modulegraph_test

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/gocli"
	"github.com/enj/soapbox/tools/internal/modulegraph"
)

// The fixtures in this file are real Go modules written to a temporary
// directory and loaded by the real go command through the real gocli runner.
// Nothing is stubbed. That matters more here than in most packages: everything
// this package asserts is a claim about what go/packages actually returns, so a
// fake loader would only prove that the fake agrees with the assertions.
//
// The tree is offline by construction. The second module is reached through a
// replace directive pointing at a sibling directory, so the load resolves a
// real cross module dependency with GOPROXY=off and no go.sum.
//
// One tree is built and loaded once for the whole package rather than once per
// test. That is not only for speed. Every load shells out to the go command,
// and a few dozen parallel loads of separate copies of the same replaced module
// contend in the shared build cache: entries are written and evicted under one
// another, and a load intermittently fails reading a srcfiles list. Sharing one
// load removes the contention at its source. It also puts the Graph's
// documented immutability under test, because every parallel test now adapts
// the same one.

// The fixture's import paths. The generated module is spelled the way a
// transformed module is: relocated upstream packages sit below an internal
// prefix, and a genuinely external dependency keeps its own path.
const (
	generatedModule = "example.test/generated"
	internalPrefix  = "internal/kk"
	sourcePrefix    = "k8s.io/kubernetes"

	facadePkg    = generatedModule
	internalPkg  = generatedModule + "/" + internalPrefix + "/pkg/apis/rbac"
	publishedPkg = "k8s.io/utilpkg/rbacv1"
	externalPkg  = "k8s.io/utilpkg/text"

	utilModule  = "k8s.io/utilpkg"
	stagingText = "staging/src/k8s.io/utilpkg/text"
)

// fixtureFiles is the tree every test starts from, keyed by path relative to
// the fixture root. The generated module and the module it depends on are
// siblings so the replace directive can be a relative path.
var fixtureFiles = map[string]string{
	"generated/go.mod": `module ` + generatedModule + `

go 1.26.0

require ` + utilModule + ` v0.0.0

replace ` + utilModule + ` => ../utilpkg
`,
	"generated/facade.go": `// Package generated is the fixture's public boundary.
package generated

import (
	"k8s.io/utilpkg/rbacv1"
	"k8s.io/utilpkg/text"

	"example.test/generated/internal/kk/pkg/apis/rbac"
)

// Role is the boundary's view of the internal API type.
type Role = rbac.Role

// PublishedRole is the boundary's view of the published API type.
type PublishedRole = rbacv1.Role

// Normalize is forwarded from the external helper.
func Normalize(in string) string { return text.Normalize(in) }
`,
	"generated/internal/kk/pkg/apis/rbac/types.go": `// +k8s:deepcopy-gen=package
// +groupName=rbac.authorization.k8s.io

// Package rbac holds the internal API types.
package rbac

// Role is an internal API type.
type Role struct {
	Name string ` + "`json:\"name\" protobuf:\"bytes,1,opt,name=name\"`" + `
}
`,
	"utilpkg/go.mod": `module ` + utilModule + `

go 1.26.0
`,
	"utilpkg/LICENSE": "Apache License 2.0\n",
	// The published package records the pairing the way upstream does: one file
	// naming both the internal package it converts from and the published
	// package the conversions target. The type policy reads that decision
	// rather than inferring one, so the fixture has to carry it for an end to
	// end adaptation to mean anything.
	"utilpkg/rbacv1/doc.go": `// +k8s:conversion-gen=` + internalPkg + `
// +k8s:conversion-gen-external-types=` + publishedPkg + `
// +groupName=rbac.authorization.k8s.io

// Package rbacv1 is the published API package.
package rbacv1
`,
	"utilpkg/rbacv1/types.go": `package rbacv1

// Role is the published API type.
type Role struct {
	Name string ` + "`json:\"name\" protobuf:\"bytes,1,opt,name=name\"`" + `
}
`,
	"utilpkg/text/text.go": `// Package text is a pure leaf utility.
package text

import "strings"

// Normalize lowers a verb.
func Normalize(in string) string {
	if in == "" {
		return "*"
	}
	return strings.ToLower(in)
}
`,
}

// fixture is one written module tree and the loader environment for it.
type fixture struct {
	// root holds both modules.
	root string
	// dir is the generated module, which every load runs in.
	dir string
	// env is the loader environment gocli produced for it.
	env []string
	// redactor is the runner's own, seeded with the proxy credential that the
	// same runner put into env. Pairing them is the point: a test that passed
	// an empty redactor would prove only that nothing was scrubbed.
	redactor *gitcli.Redactor
}

// The shared tree and the one load taken over it.
var (
	sharedOnce  sync.Once
	sharedFix   *fixture
	sharedGraph *modulegraph.Graph
	sharedErr   error
)

// TestMain removes the shared tree once every test has finished with it.
func TestMain(m *testing.M) {
	code := m.Run()
	if sharedFix != nil {
		os.RemoveAll(sharedFix.root)
	}
	os.Exit(code) //nolint:forbidigo // TestMain must return the test result to the process
}

// shared returns the tree every test that does not need its own uses, and the
// single graph loaded from it.
func shared(t *testing.T) (*fixture, *modulegraph.Graph) {
	t.Helper()
	sharedOnce.Do(func() { sharedFix, sharedGraph, sharedErr = buildShared() })
	if sharedErr != nil {
		t.Fatalf("build the shared fixture: %v", sharedErr)
	}
	return sharedFix, sharedGraph
}

// sharedOptions returns a load specification for the shared tree, which a test
// mutates to state exactly the one thing it is about.
func sharedOptions(t *testing.T) modulegraph.Options {
	t.Helper()
	f, _ := shared(t)
	return modulegraph.Options{Dir: f.dir, Env: f.env, Patterns: []string{"./..."}, Redactor: f.redactor}
}

// buildShared writes the tree and takes the one load over it.
//
// It takes no *testing.T because it runs under a sync.Once that several
// parallel tests may enter, and a T belonging to whichever test happened to win
// that race is not the T a later failure belongs to. Errors are returned and
// reported against the calling test's own T instead.
func buildShared() (*fixture, *modulegraph.Graph, error) {
	root, err := os.MkdirTemp("", "modulegraph-fixture-")
	if err != nil {
		return nil, nil, err
	}
	for name, contents := range fixtureFiles {
		if err := writeFixtureFile(filepath.Join(root, filepath.FromSlash(name)), contents); err != nil {
			return nil, nil, err
		}
	}
	dir := filepath.Join(root, "generated")

	ctx := context.Background()
	env, redactor, err := buildLoaderEnv(ctx, root, dir, gocli.ProxyOff, adapterSentinel)
	if err != nil {
		return nil, nil, err
	}
	graph, err := modulegraph.Load(ctx, modulegraph.Options{
		Dir:      dir,
		Env:      env,
		Patterns: []string{"./..."},
		Redactor: redactor,
	})
	if err != nil {
		return nil, nil, err
	}
	return &fixture{root: root, dir: dir, env: env, redactor: redactor}, graph, nil
}

// newFixture writes a tree of its own, for the few tests that need one the
// shared tree cannot express, such as a package that does not compile.
func newFixture(t *testing.T, extra map[string]string) *fixture {
	t.Helper()

	root := t.TempDir()
	for name, contents := range fixtureFiles {
		mustWrite(t, filepath.Join(root, filepath.FromSlash(name)), contents)
	}
	for name, contents := range extra {
		mustWrite(t, filepath.Join(root, filepath.FromSlash(name)), contents)
	}
	dir := filepath.Join(root, "generated")

	env, redactor, err := buildLoaderEnv(t.Context(), root, dir, gocli.ProxyOff)
	if err != nil {
		t.Fatalf("loader environment: %v", err)
	}
	return &fixture{root: root, dir: dir, env: env, redactor: redactor}
}

// options returns a load specification for a tree built by newFixture.
func (f *fixture) options() modulegraph.Options {
	return modulegraph.Options{Dir: f.dir, Env: f.env, Patterns: []string{"./..."}, Redactor: f.redactor}
}

// buildLoaderEnv builds the environment through the gocli runner, which is the
// integration this package is meant to have: the loader runs under the same
// isolation, proxy, and fixed policy as every other Go subprocess in the engine.
//
// The caches the test process already has are reused, so a test neither
// compiles the standard library from cold nor writes into the developer's real
// caches. The proxy is off, which the fixture can satisfy because its only
// cross module edge is a replace to a directory.
func buildLoaderEnv(ctx context.Context, root, dir, proxy string, secrets ...string) ([]string, *gitcli.Redactor, error) {
	home, err := isolatedHome(root)
	if err != nil {
		return nil, nil, err
	}
	isolation := []string{"HOME=" + home}
	for _, name := range []string{"GOCACHE", "GOMODCACHE", "GOPATH", "GOTMPDIR", "TMPDIR"} {
		if value, ok := os.LookupEnv(name); ok {
			isolation = append(isolation, name+"="+value)
		}
	}
	runner, err := gocli.New(ctx, gocli.Options{
		Dir:       dir,
		Proxy:     proxy,
		Inherit:   []string{"PATH"},
		Isolation: isolation,
		Secrets:   secrets,
	})
	if err != nil {
		return nil, nil, err
	}
	env, err := runner.LoaderEnv(ctx)
	if err != nil {
		return nil, nil, err
	}
	return env, runner.Redactor(), nil
}

// isolatedHome returns a throwaway HOME with telemetry turned off.
//
// The go command derives its telemetry directory from HOME and writes counter
// files as it exits. That races the temporary directory cleanup, so the mode
// file is written first.
func isolatedHome(root string) (string, error) {
	home := filepath.Join(root, "home")
	return home, writeFixtureFile(filepath.Join(home, ".config", "go", "telemetry", "mode"), "off\n")
}

// writeFixtureFile writes one fixture file, creating its parent directories.
func writeFixtureFile(path, contents string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(contents), 0o600)
}

// mustWrite writes one fixture file or fails the test.
func mustWrite(t *testing.T, path, contents string) {
	t.Helper()
	if err := writeFixtureFile(path, contents); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
