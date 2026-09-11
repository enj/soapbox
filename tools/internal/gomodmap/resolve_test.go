package gomodmap_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/config"
	"github.com/enj/soapbox/tools/internal/gocli"
	"github.com/enj/soapbox/tools/internal/gomodmap"
)

// scratchRunner returns a go runner for the checks that are refused before any
// module is resolved. It cannot reach the network, so a check that wrongly
// reached the toolchain would fail rather than quietly resolve something.
func scratchRunner(t *testing.T) *gocli.Runner {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module soapbox.test/scratch\n\ngo 1.26.0\n"), 0o600); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	runner, err := gocli.New(t.Context(), gocli.Options{
		Dir:     dir,
		Inherit: []string{"PATH"},
		Proxy:   gocli.ProxyOff,
	})
	if err != nil {
		t.Fatalf("create go runner: %v", err)
	}
	return runner
}

// runnerInModule returns an offline runner rooted in a module with the given
// go.mod, or in a directory with none when the text is empty.
func runnerInModule(t *testing.T, goMod string) *gocli.Runner {
	t.Helper()
	dir := t.TempDir()
	if goMod != "" {
		if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0o600); err != nil {
			t.Fatalf("write go.mod: %v", err)
		}
	}
	runner, err := gocli.New(t.Context(), gocli.Options{
		Dir:     dir,
		Inherit: []string{"PATH"},
		Proxy:   gocli.ProxyOff,
	})
	if err != nil {
		t.Fatalf("create go runner: %v", err)
	}
	return runner
}

// TestResolveVersions_RejectsResolverModule proves the resolver refuses to run
// anywhere but an isolated scratch module.
//
// go list -m answers in the context of a main module, so the module the runner
// sits in decides part of the answer. A replacement resolves a version out of a
// directory and an exclude quietly changes which version a query selects, and
// neither is visible in the response as anything other than a different version.
// The refusal has to happen before the query rather than be read back out of it.
func TestResolveVersions_RejectsResolverModule(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		goMod   string
		wantErr string
	}{
		{
			name:    "no module at all",
			goMod:   "",
			wantErr: "resolver module",
		},
		{
			name:    "module carries a replacement",
			goMod:   "module soapbox.test/scratch\n\ngo 1.26.0\n\nreplace k8s.io/api => ./api\n",
			wantErr: "replace directives",
		},
		{
			name:    "module carries an exclude",
			goMod:   "module soapbox.test/scratch\n\ngo 1.26.0\n\nexclude k8s.io/api v0.36.0\n",
			wantErr: "exclude directives",
		},
		{
			// A resolver sitting in one of the modules being resolved answers for
			// it out of its own working tree rather than from the proxy.
			name:    "module is one of the modules being resolved",
			goMod:   "module k8s.io/api\n\ngo 1.26.0\n",
			wantErr: "is itself k8s.io/api",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			runner := runnerInModule(t, test.goMod)

			_, releaseErr := gomodmap.ResolveReleaseVersions(
				t.Context(), runner, config.ReleasePolicyV1ToV0, "v1.36.1", []string{"k8s.io/api"},
			)
			if releaseErr == nil {
				t.Fatalf("resolve release versions: got nil error, want %q", test.wantErr)
			}
			if !strings.Contains(releaseErr.Error(), test.wantErr) {
				t.Errorf("resolve release versions: error = %v, want it to contain %q", releaseErr, test.wantErr)
			}

			// Both entry points funnel through the same check, so both have to
			// refuse the same runner.
			_, commitErr := gomodmap.ResolveCommitVersions(t.Context(), runner, []gomodmap.CommitMapping{
				{ModulePath: "k8s.io/api", Source: sourceA, Staging: stagingA},
			})
			if commitErr == nil {
				t.Fatalf("resolve commit versions: got nil error, want %q", test.wantErr)
			}
			if !strings.Contains(commitErr.Error(), test.wantErr) {
				t.Errorf("resolve commit versions: error = %v, want it to contain %q", commitErr, test.wantErr)
			}
		})
	}
}

func TestResolveReleaseVersions_Rejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		policy  string
		tag     string
		paths   []string
		wantErr string
	}{
		{
			name:    "unsupported release policy",
			policy:  "v1-to-v9",
			tag:     "v1.36.1",
			paths:   []string{"k8s.io/api"},
			wantErr: "unsupported release policy",
		},
		{
			name:    "source tag is not a v1 release",
			policy:  config.ReleasePolicyV1ToV0,
			tag:     "v2.0.0",
			paths:   []string{"k8s.io/api"},
			wantErr: "requires a v1 source tag",
		},
		{
			name:    "source tag is not a version",
			policy:  config.ReleasePolicyV1ToV0,
			tag:     "release-1.36",
			paths:   []string{"k8s.io/api"},
			wantErr: "must start with v",
		},
		{
			name:    "no staging modules",
			policy:  config.ReleasePolicyV1ToV0,
			tag:     "v1.36.1",
			paths:   nil,
			wantErr: "at least one staging module is required",
		},
		{
			name:    "staging module listed twice",
			policy:  config.ReleasePolicyV1ToV0,
			tag:     "v1.36.1",
			paths:   []string{"k8s.io/api", "k8s.io/api"},
			wantErr: "listed twice",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := gomodmap.ResolveReleaseVersions(t.Context(), scratchRunner(t), test.policy, test.tag, test.paths)
			if err == nil {
				t.Fatalf("resolve release versions: got nil error, want %q", test.wantErr)
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Errorf("resolve release versions: error = %v, want it to contain %q", err, test.wantErr)
			}
		})
	}
}

func TestResolveCommitVersions_Rejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		mappings []gomodmap.CommitMapping
		wantErr  string
	}{
		{
			name:     "no mapped modules",
			mappings: nil,
			wantErr:  "at least one mapped module is required",
		},
		{
			name: "module mapped twice",
			mappings: []gomodmap.CommitMapping{
				{ModulePath: "k8s.io/api", Source: sourceA, Staging: stagingA},
				{ModulePath: "k8s.io/api", Source: sourceA, Staging: stagingB},
			},
			wantErr: "is mapped twice",
		},
		{
			name:     "mapping has no staging commit",
			mappings: []gomodmap.CommitMapping{{ModulePath: "k8s.io/api", Source: sourceA}},
			wantErr:  "has no mapped commit",
		},
		{
			name: "carried mapping has invalid version",
			mappings: []gomodmap.CommitMapping{{
				ModulePath: "k8s.io/api", Source: sourceA, Staging: stagingA, Version: "latest", Carried: true,
			}},
			wantErr: "carried version",
		},
		{
			name: "carried mapping has no version",
			mappings: []gomodmap.CommitMapping{{
				ModulePath: "k8s.io/api", Source: sourceA, Staging: stagingA, Carried: true,
			}},
			wantErr: "carried without an exact previous version",
		},
		{
			name: "ordinary mapping has a carried version",
			mappings: []gomodmap.CommitMapping{{
				ModulePath: "k8s.io/api", Source: sourceA, Staging: stagingA, Version: "v0.36.2",
			}},
			wantErr: "without a carried mapping",
		},
		{
			// The returned versions carry no source of their own, so a set drawn
			// from two source commits would be cached under whichever one the
			// caller happened to record it against.
			name: "mappings disagree about the source",
			mappings: []gomodmap.CommitMapping{
				{ModulePath: "k8s.io/api", Source: sourceA, Staging: stagingA},
				{ModulePath: "k8s.io/apimachinery", Source: sourceB, Staging: stagingB},
			},
			wantErr: "is mapped from source",
		},
		{
			name:     "mapping has no source",
			mappings: []gomodmap.CommitMapping{{ModulePath: "k8s.io/api", Staging: stagingA}},
			wantErr:  "source commit",
		},
		{
			// The source commit becomes the key an entry is cached under, so a
			// name no object store could hold is refused before anything resolves.
			name:     "source is not an object name",
			mappings: []gomodmap.CommitMapping{{ModulePath: "k8s.io/api", Source: "abc", Staging: stagingA}},
			wantErr:  "must be 40 or 64 hexadecimal characters",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := gomodmap.ResolveCommitVersions(t.Context(), scratchRunner(t), test.mappings)
			if err == nil {
				t.Fatalf("resolve commit versions: got nil error, want %q", test.wantErr)
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Errorf("resolve commit versions: error = %v, want it to contain %q", err, test.wantErr)
			}
		})
	}
}

// goNetworkEnv opts a run into the tests that resolve real modules.
const goNetworkEnv = "SOAPBOX_GO_NETWORK_TESTS"

// networkRunner returns a go runner that may reach the module proxy.
//
// The cache locations are carried over from the process environment because a
// run under a read-only default GOPATH would otherwise fail before reaching the
// proxy, and because reusing the operator's module cache is what keeps repeated
// runs of these tests from re-downloading the same modules.
func networkRunner(t *testing.T) *gocli.Runner {
	t.Helper()
	if os.Getenv(goNetworkEnv) == "" {
		t.Skipf("set %s=1 to run the tests that resolve real modules", goNetworkEnv)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module soapbox.test/scratch\n\ngo 1.26.0\n"), 0o600); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	var isolation []string
	for _, name := range []string{"GOPATH", "GOMODCACHE", "GOCACHE"} {
		if value := os.Getenv(name); value != "" {
			isolation = append(isolation, name+"="+value)
		}
	}
	runner, err := gocli.New(t.Context(), gocli.Options{
		Dir:       dir,
		Inherit:   []string{"PATH", "HOME"},
		Isolation: isolation,
	})
	if err != nil {
		t.Fatalf("create go runner: %v", err)
	}
	return runner
}

// TestResolveReleaseVersions_Real resolves a real published module through the
// real go command.
//
// The verdict logic is covered offline by the internal tests, which construct
// responses directly. What this adds is proof that the query this package builds
// is one the go command actually answers, and that the answer arrives in the
// shape the indexing expects. It is gated because it reaches the module proxy.
//
// golang.org/x/mod stands in for a staging module because it is versioned the
// same way they are, with v0 tags, so the v1-to-v0 mapping has something real to
// resolve against: source tag v1.39.0 maps onto v0.39.0.
//
//	SOAPBOX_GO_NETWORK_TESTS=1 go test ./internal/gomodmap/
func TestResolveReleaseVersions_Real(t *testing.T) {
	t.Parallel()

	versions, err := gomodmap.ResolveReleaseVersions(
		t.Context(), networkRunner(t), config.ReleasePolicyV1ToV0, "v1.39.0", []string{"golang.org/x/mod"},
	)
	if err != nil {
		t.Fatalf("resolve release versions: %v", err)
	}
	if len(versions) != 1 {
		t.Fatalf("versions = %v, want 1", versions)
	}
	if versions[0].Path != "golang.org/x/mod" || versions[0].Version != "v0.39.0" {
		t.Errorf("resolved %v, want golang.org/x/mod at v0.39.0", versions[0])
	}
	// A release pin records the commit its tag resolved to. The tag is immutable
	// to this engine but not to the repository that published it, so the commit
	// is the only evidence a later run has that the tag still names the same code.
	if versions[0].Commit != modTagCommit {
		t.Errorf("resolved staging commit %q, want %q", versions[0].Commit, modTagCommit)
	}
}

// TestResolveReleaseVersions_RealMissingTag proves a tag this engine computed
// but that was never published fails loudly rather than becoming a pin nothing
// can resolve.
func TestResolveReleaseVersions_RealMissingTag(t *testing.T) {
	t.Parallel()

	// x/mod has no v0.9999.0, so the mapped tag names nothing.
	_, err := gomodmap.ResolveReleaseVersions(
		t.Context(), networkRunner(t), config.ReleasePolicyV1ToV0, "v1.9999.0", []string{"golang.org/x/mod"},
	)
	if err == nil {
		t.Fatal("resolve release versions: got nil error, want an unresolved module")
	}
}

// Two real commits, one tagged and one not, and what the go command answers for
// each. Both are immutable, so the expected answers cannot drift.
//
// kube-openapi is the untagged case and is not a stand-in: it is a real
// Kubernetes dependency that has never been tagged, so every version of it is a
// pseudo-version. x/mod is the tagged case, where a commit query is answered
// with the tag that names it rather than with a pseudo-version.
const (
	openAPIModule  = "k8s.io/kube-openapi"
	openAPICommit  = "d427ff9ee9ad05f5da435abbb7c5929cb713ac56"
	openAPIVersion = "v0.0.0-20260721132016-d427ff9ee9ad"

	modModule     = "golang.org/x/mod"
	modTagCommit  = "13be9020bbbfae457b59b82c999f8c309cb21ffc"
	modTagVersion = "v0.39.0"
)

// TestResolveCommitVersions_Real resolves real commits through the real go
// command, in both shapes an intermediate mapping can produce.
//
// The untagged commit is the ordinary case and yields a pseudo-version. The
// tagged one is the case that makes the form of the answer a bad gate: a mapped
// staging commit can be exactly the commit a staging repository tagged, and the
// go command then answers with the tag. Refusing that would fail intermediate
// mapping for every staging repository that tags often. What is checked in both
// is the origin hash, which is the go command's own record of the revision it
// resolved.
func TestResolveCommitVersions_Real(t *testing.T) {
	t.Parallel()

	runner := networkRunner(t)
	versions, err := gomodmap.ResolveCommitVersions(t.Context(), runner, []gomodmap.CommitMapping{
		{ModulePath: openAPIModule, Source: sourceA, Staging: openAPICommit},
		{ModulePath: modModule, Source: sourceA, Staging: modTagCommit},
	})
	if err != nil {
		t.Fatalf("resolve commit versions: %v", err)
	}

	want := []gomodmap.ModuleVersion{
		{Path: modModule, Version: modTagVersion, Commit: modTagCommit},
		{Path: openAPIModule, Version: openAPIVersion, Commit: openAPICommit},
	}
	if len(versions) != len(want) {
		t.Fatalf("versions = %v, want %v", versions, want)
	}
	for i, resolved := range versions {
		if resolved != want[i] {
			t.Errorf("versions[%d] = %v, want %v", i, resolved, want[i])
		}
	}
}

func TestResolveCommitVersions_RealCarriedTag(t *testing.T) {
	t.Parallel()

	versions, err := gomodmap.ResolveCommitVersions(t.Context(), networkRunner(t), []gomodmap.CommitMapping{{
		ModulePath: modModule, Source: sourceA, Staging: modTagCommit,
		Version: modTagVersion, Carried: true,
	}})
	if err != nil {
		t.Fatalf("resolve carried version: %v", err)
	}
	want := gomodmap.ModuleVersion{Path: modModule, Version: modTagVersion, Commit: modTagCommit}
	if len(versions) != 1 || versions[0] != want {
		t.Fatalf("carried versions = %v, want [%v]", versions, want)
	}
}

// TestResolveCommitVersions_RealWrongCommit proves a mapping onto a commit the
// module never had is refused rather than resolved to whatever the proxy holds.
func TestResolveCommitVersions_RealWrongCommit(t *testing.T) {
	t.Parallel()

	// A well formed object name that belongs to a different repository entirely.
	_, err := gomodmap.ResolveCommitVersions(t.Context(), networkRunner(t), []gomodmap.CommitMapping{
		{ModulePath: openAPIModule, Source: sourceA, Staging: modTagCommit},
	})
	if err == nil {
		t.Fatal("resolve commit versions: got nil error, want the commit to be refused")
	}
}
