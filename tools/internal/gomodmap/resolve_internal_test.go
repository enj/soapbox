package gomodmap

import (
	"errors"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/gocli"
)

// Object names used as resolution inputs. They are literals because the checks
// under test care only that a value is a well formed object name.
const (
	commitA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	commitB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// pseudoNaming renders a pseudo-version naming the given commit, the way the go
// command would.
func pseudoNaming(commit string) string {
	return "v0.0.0-20260101000000-" + commit[:12]
}

// resolvedAt renders a module the go command resolved from one commit.
func resolvedAt(version, hash string) gocli.Module {
	resolved := gocli.Module{Path: "k8s.io/api", Version: version}
	if hash != "" {
		resolved.Origin = &gocli.ModuleOrigin{VCS: "git", URL: "https://github.com/kubernetes/api", Hash: hash}
	}
	return resolved
}

// TestAssertNamesCommit is the check that makes asking the toolchain for a
// version safer than computing one here.
func TestAssertNamesCommit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		resolved gocli.Module
		commit   string
		wantErr  string
	}{
		{
			name:     "pseudo-version names the requested commit",
			resolved: resolvedAt(pseudoNaming(commitA), commitA),
			commit:   commitA,
		},
		{
			// The failure the whole resolution path exists to catch: a version
			// that is perfectly well formed and describes different code. The
			// origin is the gate, so that is what reports it.
			name:     "pseudo-version names another commit",
			resolved: resolvedAt(pseudoNaming(commitB), commitB),
			commit:   commitA,
			wantErr:  "was resolved from",
		},
		{
			// The origin agrees with the request but the version string does not.
			// The version is what gets written into the generated go.mod, so a
			// disagreement between the two has to fail even though the gate passed.
			name:     "origin agrees but the version names another commit",
			resolved: resolvedAt(pseudoNaming(commitB), commitA),
			commit:   commitA,
			wantErr:  "names revision",
		},
		{
			// The version string and the origin disagree, so the pin that would be
			// published names a different commit than the one it was resolved
			// from.
			name:     "origin disagrees with the version",
			resolved: resolvedAt(pseudoNaming(commitA), commitB),
			commit:   commitA,
			wantErr:  "was resolved from",
		},
		{
			// A pin this engine cannot tie back to a commit is one nothing can
			// reproduce, and publication is append-only.
			name:     "no version control origin",
			resolved: resolvedAt(pseudoNaming(commitA), ""),
			commit:   commitA,
			wantErr:  "reported no version control origin",
		},
		{
			name: "origin carries no revision",
			resolved: gocli.Module{
				Path:    "k8s.io/api",
				Version: pseudoNaming(commitA),
				Origin:  &gocli.ModuleOrigin{VCS: "git"},
			},
			commit:  commitA,
			wantErr: "reported an origin with no revision",
		},
		{
			// A mapped staging commit can be exactly the commit a staging
			// repository tagged, and the go command then answers a commit query
			// with the tag. That names the requested commit, so refusing it would
			// fail intermediate mapping for repositories that tag often.
			name:     "release tag naming the requested commit",
			resolved: resolvedAt("v0.36.1", commitA),
			commit:   commitA,
		},
		{
			// The same answer for a different commit is still refused, because the
			// gate is the origin rather than the shape of the version.
			name:     "release tag naming another commit",
			resolved: resolvedAt("v0.36.1", commitB),
			commit:   commitA,
			wantErr:  "was resolved from",
		},
		{
			name:     "version is empty",
			resolved: resolvedAt("", commitA),
			commit:   commitA,
			wantErr:  "a version is required",
		},
		{
			name:     "version is not a version at all",
			resolved: resolvedAt("latest", commitA),
			commit:   commitA,
			wantErr:  "is not a semantic version",
		},
		{
			// The placeholder names no published module, so it can never be a pin
			// even if an origin agreed with it.
			name:     "version is the staging placeholder",
			resolved: resolvedAt(StagingVersion, commitA),
			commit:   commitA,
			wantErr:  "is the source placeholder",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := assertNamesCommit(test.resolved, test.commit)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("assert names commit: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("assert names commit: got nil error, want %q", test.wantErr)
			}
			if !errors.Is(err, ErrVersionMismatch) {
				t.Errorf("assert names commit: error = %v, want ErrVersionMismatch", err)
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Errorf("assert names commit: error = %v, want it to contain %q", err, test.wantErr)
			}
		})
	}
}

// TestAssertNamesTag proves a release pin is checked against the reference it
// was actually resolved through, not only against the version it reported.
func TestAssertNamesTag(t *testing.T) {
	t.Parallel()

	const wantOriginErr = "rather than root Git tag refs/tags/v0.36.1"
	tests := []struct {
		name    string
		origin  *gocli.ModuleOrigin
		wantErr string
	}{
		{
			name:   "resolved through the release tag",
			origin: &gocli.ModuleOrigin{VCS: "git", Hash: commitA, Ref: "refs/tags/v0.36.1"},
		},
		{
			name:    "resolved through a prefixed release tag",
			origin:  &gocli.ModuleOrigin{VCS: "git", Hash: commitA, Ref: "refs/tags/staging/v0.36.1"},
			wantErr: wantOriginErr,
		},
		{
			name:    "resolved through a branch",
			origin:  &gocli.ModuleOrigin{VCS: "git", Hash: commitA, Ref: "refs/heads/release-1.36"},
			wantErr: wantOriginErr,
		},
		{
			name:    "resolved through another tag",
			origin:  &gocli.ModuleOrigin{VCS: "git", Hash: commitA, Ref: "refs/tags/v0.36.2"},
			wantErr: wantOriginErr,
		},
		{
			name:    "resolved from a module subdirectory",
			origin:  &gocli.ModuleOrigin{VCS: "git", Hash: commitA, Subdir: "staging", Ref: "refs/tags/v0.36.1"},
			wantErr: wantOriginErr,
		},
		{
			name:    "resolved from another VCS",
			origin:  &gocli.ModuleOrigin{VCS: "hg", Hash: commitA, Ref: "refs/tags/v0.36.1"},
			wantErr: wantOriginErr,
		},
		{
			name:    "no reference at all",
			origin:  &gocli.ModuleOrigin{VCS: "git", Hash: commitA},
			wantErr: wantOriginErr,
		},
		{
			name:    "reference is not under refs/tags",
			origin:  &gocli.ModuleOrigin{VCS: "git", Hash: commitA, Ref: "v0.36.1"},
			wantErr: wantOriginErr,
		},
		{
			name:    "no origin",
			wantErr: "reported no version control origin",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			resolved := gocli.Module{Path: "k8s.io/api", Version: "v0.36.1", Origin: test.origin}
			commit, err := assertNamesTag(resolved, "v0.36.1")
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("assert names tag: %v", err)
				}
				if commit != commitA {
					t.Errorf("assert names tag returned commit %q, want %q", commit, commitA)
				}
				return
			}
			if err == nil {
				t.Fatalf("assert names tag: got nil error, want %q", test.wantErr)
			}
			if !errors.Is(err, ErrVersionMismatch) {
				t.Errorf("assert names tag: error = %v, want ErrVersionMismatch", err)
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Errorf("assert names tag: error = %v, want it to contain %q", err, test.wantErr)
			}
		})
	}
}

func TestIndexResolved(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		paths   []string
		modules []gocli.Module
		wantErr error
		message string
	}{
		{
			name:  "every module resolved",
			paths: []string{"k8s.io/api", "k8s.io/apimachinery"},
			modules: []gocli.Module{
				{Path: "k8s.io/api", Version: "v0.36.1"},
				{Path: "k8s.io/apimachinery", Version: "v0.36.1"},
			},
		},
		{
			// The go command reports a query it could not resolve in the module's
			// own Error field rather than by failing, so it has to be read.
			name:  "module reported an error",
			paths: []string{"k8s.io/api"},
			modules: []gocli.Module{
				{Path: "k8s.io/api", Error: &gocli.ModuleError{Err: "unknown revision v0.36.1"}},
			},
			wantErr: ErrUnresolvedModule,
			message: "unknown revision",
		},
		{
			name:    "module missing from the response",
			paths:   []string{"k8s.io/api", "k8s.io/apimachinery"},
			modules: []gocli.Module{{Path: "k8s.io/api", Version: "v0.36.1"}},
			wantErr: ErrUnresolvedModule,
			message: "k8s.io/apimachinery is missing",
		},
		{
			name:  "module resolved twice",
			paths: []string{"k8s.io/api"},
			modules: []gocli.Module{
				{Path: "k8s.io/api", Version: "v0.36.1"},
				{Path: "k8s.io/api", Version: "v0.36.2"},
			},
			wantErr: ErrUnresolvedModule,
			message: "resolved to both",
		},
		{
			name:    "module resolved to no version",
			paths:   []string{"k8s.io/api"},
			modules: []gocli.Module{{Path: "k8s.io/api"}},
			wantErr: ErrUnresolvedModule,
			message: "resolved to no version",
		},
		{
			name:    "version is not canonical",
			paths:   []string{"k8s.io/api"},
			modules: []gocli.Module{{Path: "k8s.io/api", Version: "v0.36.1+meta"}},
			wantErr: ErrVersionMismatch,
			message: "non canonical",
		},
		{
			// Extra records are the normal shape of a batched response and must
			// not be mistaken for a failure.
			name:  "response carries an unrequested module",
			paths: []string{"k8s.io/api"},
			modules: []gocli.Module{
				{Path: "k8s.io/api", Version: "v0.36.1"},
				{Path: "k8s.io/klog/v2", Version: "v2.130.1"},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			found, err := indexResolved(test.paths, test.modules)
			if test.wantErr == nil {
				if err != nil {
					t.Fatalf("index resolved: %v", err)
				}
				for _, modulePath := range test.paths {
					if _, ok := found[modulePath]; !ok {
						t.Errorf("index resolved: %s is missing from the index", modulePath)
					}
				}
				return
			}
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("index resolved: error = %v, want %v", err, test.wantErr)
			}
			if test.message != "" && !strings.Contains(err.Error(), test.message) {
				t.Errorf("index resolved: error = %v, want it to contain %q", err, test.message)
			}
		})
	}
}

func TestUniqueSorted(t *testing.T) {
	t.Parallel()

	sorted, err := uniqueSorted([]string{"k8s.io/apimachinery", "k8s.io/api"})
	if err != nil {
		t.Fatalf("unique sorted: %v", err)
	}
	if len(sorted) != 2 || sorted[0] != "k8s.io/api" || sorted[1] != "k8s.io/apimachinery" {
		t.Errorf("unique sorted = %v, want them sorted by path", sorted)
	}

	tests := []struct {
		name    string
		paths   []string
		wantErr string
	}{
		{
			name:    "no modules",
			paths:   nil,
			wantErr: "at least one staging module is required",
		},
		{
			name:    "module listed twice",
			paths:   []string{"k8s.io/api", "k8s.io/api"},
			wantErr: "listed twice",
		},
		{
			name:    "empty module path",
			paths:   []string{""},
			wantErr: "a staging module path is required",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := uniqueSorted(test.paths)
			if err == nil {
				t.Fatalf("unique sorted: got nil error, want %q", test.wantErr)
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Errorf("unique sorted: error = %v, want it to contain %q", err, test.wantErr)
			}
		})
	}
}
