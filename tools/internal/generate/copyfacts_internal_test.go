package generate

import (
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/gocli"
	"github.com/enj/soapbox/tools/internal/relocate"
)

func TestCountMinorCadence(t *testing.T) {
	tests := []struct {
		name        string
		results     []gocli.ModuleWithVersions
		modulePath  string
		version     string
		sourceMinor int
		want        int
		wantErr     string
	}{
		{
			name:    "no records",
			results: nil,
			wantErr: "no records",
		},
		{
			name: "duplicate records",
			results: []gocli.ModuleWithVersions{
				{Path: "m", Versions: []string{"v0.36.1"}},
				{Path: "m", Versions: []string{"v0.36.1"}},
			},
			modulePath: "m",
			version:    "v0.36.1",
			wantErr:    "returned 2 records",
		},
		{
			name: "record has error",
			results: []gocli.ModuleWithVersions{
				{Path: "m", Error: &gocli.ModuleError{Err: "lookup failed"}},
			},
			modulePath: "m",
			version:    "v0.36.1",
			wantErr:    "lookup failed",
		},
		{
			name: "wrong module path",
			results: []gocli.ModuleWithVersions{
				{Path: "other", Versions: []string{"v0.36.1"}},
			},
			modulePath: "m",
			version:    "v0.36.1",
			wantErr:    "returned other, want m",
		},
		{
			name: "resolved version missing from list",
			results: []gocli.ModuleWithVersions{
				{Path: "m", Versions: []string{"v0.36.0"}},
			},
			modulePath:  "m",
			version:     "v0.36.1",
			sourceMinor: 36,
			wantErr:     "does not contain resolved version",
		},
		{
			name: "pseudo-version uses tagged release cadence",
			results: []gocli.ModuleWithVersions{
				{Path: "m", Versions: []string{"v0.35.0", "v0.36.0", "v0.36.1"}},
			},
			modulePath:  "m",
			version:     "v0.0.0-20260102030405-333333333333",
			sourceMinor: 36,
			want:        2,
		},
		{
			name: "malformed version in list",
			results: []gocli.ModuleWithVersions{
				{Path: "m", Versions: []string{"v0.36.1", "not-a-version"}},
			},
			modulePath:  "m",
			version:     "v0.36.1",
			sourceMinor: 36,
			wantErr:     "malformed version",
		},
		{
			name: "counts exact minor only",
			results: []gocli.ModuleWithVersions{
				{Path: "m", Versions: []string{
					"v0.35.0", "v0.35.1",
					"v0.36.0", "v0.36.1", "v0.36.2",
					"v0.37.0",
				}},
			},
			modulePath:  "m",
			version:     "v0.36.1",
			sourceMinor: 36,
			want:        3,
		},
		{
			name: "prerelease counted when minor matches",
			results: []gocli.ModuleWithVersions{
				{Path: "m", Versions: []string{
					"v0.36.0-alpha.1", "v0.36.0-rc.1", "v0.36.0", "v0.36.1",
				}},
			},
			modulePath:  "m",
			version:     "v0.36.1",
			sourceMinor: 36,
			want:        4,
		},
		{
			name: "retracted version counted",
			results: []gocli.ModuleWithVersions{
				{Path: "m", Versions: []string{
					"v0.36.0", "v0.36.1", "v0.36.2",
				}},
			},
			modulePath:  "m",
			version:     "v0.36.1",
			sourceMinor: 36,
			want:        3,
		},
		{
			name: "zero cadence when no minor matches",
			results: []gocli.ModuleWithVersions{
				{Path: "m", Versions: []string{"v0.35.0", "v0.35.1", "v0.37.0"}},
			},
			modulePath:  "m",
			version:     "v0.35.0",
			sourceMinor: 36,
			want:        0,
		},
		{
			name: "single version",
			results: []gocli.ModuleWithVersions{
				{Path: "m", Versions: []string{"v0.36.1"}},
			},
			modulePath:  "m",
			version:     "v0.36.1",
			sourceMinor: 36,
			want:        1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := countMinorCadence(tt.results, tt.modulePath, tt.version, tt.sourceMinor)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("got %d, want error containing %q", got, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("cadence = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestCountMinorCadenceIgnoresReleasesAfterCutoff(t *testing.T) {
	results := []gocli.ModuleWithVersions{{
		Path: "k8s.io/component-helpers",
		Versions: []string{
			"v0.36.0-alpha.0", "v0.36.0", "v0.36.1", "v0.36.2", "v0.36.3",
		},
	}}
	got, err := countMinorCadence(results, "k8s.io/component-helpers", "v0.36.1", 36, "v0.36.1")
	if err != nil {
		t.Fatalf("count cadence: %v", err)
	}
	if got != 3 {
		t.Errorf("cadence = %d, want alpha, v0.36.0, and v0.36.1 only", got)
	}
}

func TestRelativeStagingPackages(t *testing.T) {
	tests := []struct {
		name      string
		staging   []string
		moduleDir string
		want      []string
		wantErr   string
	}{
		{
			name:      "package equals module dir",
			staging:   []string{"staging/src/k8s.io/helpers"},
			moduleDir: "staging/src/k8s.io/helpers",
			want:      []string{"."},
		},
		{
			name:      "package is child of module dir",
			staging:   []string{"staging/src/k8s.io/helpers/text/policy"},
			moduleDir: "staging/src/k8s.io/helpers",
			want:      []string{"text/policy"},
		},
		{
			name: "multiple children",
			staging: []string{
				"staging/src/k8s.io/helpers/text/policy",
				"staging/src/k8s.io/helpers/text/format",
			},
			moduleDir: "staging/src/k8s.io/helpers",
			want:      []string{"text/policy", "text/format"},
		},
		{
			name:      "package outside module dir",
			staging:   []string{"staging/src/k8s.io/other/pkg"},
			moduleDir: "staging/src/k8s.io/helpers",
			wantErr:   "not inside module directory",
		},
		{
			name: "one inside one outside",
			staging: []string{
				"staging/src/k8s.io/helpers/text/policy",
				"staging/src/k8s.io/other/pkg",
			},
			moduleDir: "staging/src/k8s.io/helpers",
			wantErr:   "not inside module directory",
		},
		{
			name:      "prefix overlap not a child",
			staging:   []string{"staging/src/k8s.io/helpers-extra/pkg"},
			moduleDir: "staging/src/k8s.io/helpers",
			wantErr:   "not inside module directory",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := relativeStagingPackages(tt.staging, tt.moduleDir)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("got %v, want error containing %q", got, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("result[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestValidateStagingOrigin(t *testing.T) {
	const (
		modulePath  = "soapbox.test/component-helpers"
		version     = "v0.36.1"
		commit      = "3333333333333333333333333333333333333333"
		sourceRepo  = "https://github.com/kubernetes/kubernetes.git"
		canonURL    = "https://github.com/kubernetes/component-helpers"
		expectedRef = "refs/tags/v0.36.1"
	)

	valid := &gocli.ModuleOrigin{
		VCS:  "git",
		URL:  canonURL,
		Hash: commit,
		Ref:  expectedRef,
	}

	tests := []struct {
		name    string
		origin  *gocli.ModuleOrigin
		wantErr string
	}{
		{
			name:    "nil origin",
			origin:  nil,
			wantErr: "no Origin",
		},
		{
			name:    "wrong VCS",
			origin:  &gocli.ModuleOrigin{VCS: "hg", URL: canonURL, Hash: commit, Ref: expectedRef},
			wantErr: `VCS is "hg"`,
		},
		{
			name:    "empty hash",
			origin:  &gocli.ModuleOrigin{VCS: "git", URL: canonURL, Hash: "", Ref: expectedRef},
			wantErr: "no commit hash",
		},
		{
			name:    "hash mismatch",
			origin:  &gocli.ModuleOrigin{VCS: "git", URL: canonURL, Hash: "0000000000000000000000000000000000000000", Ref: expectedRef},
			wantErr: "does not match pinned",
		},
		{
			name:    "wrong ref",
			origin:  &gocli.ModuleOrigin{VCS: "git", URL: canonURL, Hash: commit, Ref: "refs/tags/v0.35.0"},
			wantErr: "Ref is",
		},
		{
			name:    "empty ref",
			origin:  &gocli.ModuleOrigin{VCS: "git", URL: canonURL, Hash: commit, Ref: ""},
			wantErr: "Ref is",
		},
		{
			name:    "non-empty subdir",
			origin:  &gocli.ModuleOrigin{VCS: "git", URL: canonURL, Hash: commit, Ref: expectedRef, Subdir: "staging/src/helpers"},
			wantErr: "Subdir is",
		},
		{
			name:    "wrong URL host",
			origin:  &gocli.ModuleOrigin{VCS: "git", URL: "https://evil.example.com/component-helpers", Hash: commit, Ref: expectedRef},
			wantErr: "does not match expected canonical",
		},
		{
			name:    "wrong URL basename",
			origin:  &gocli.ModuleOrigin{VCS: "git", URL: "https://github.com/kubernetes/other-module", Hash: commit, Ref: expectedRef},
			wantErr: "does not match expected canonical",
		},
		{
			name:   "valid origin",
			origin: valid,
		},
		{
			name:   "valid with .git suffix on URL",
			origin: &gocli.ModuleOrigin{VCS: "git", URL: canonURL + ".git", Hash: commit, Ref: expectedRef},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			url, hash, err := validateStagingOrigin(tt.origin, modulePath, version, commit, sourceRepo)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("got url=%q hash=%q, want error containing %q", url, hash, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if url == "" || hash == "" {
				t.Errorf("got url=%q hash=%q, want both non-empty", url, hash)
			}
		})
	}

	// Edge case: sourceRepo with no path separator.
	t.Run("malformed source repo", func(t *testing.T) {
		_, _, err := validateStagingOrigin(valid, modulePath, version, commit, "noslash")
		if err == nil || !strings.Contains(err.Error(), "no path separator") {
			t.Fatalf("error = %v, want it to mention no path separator", err)
		}
	})

	// Edge case: sourceRepo without .git suffix.
	t.Run("source repo without .git", func(t *testing.T) {
		url, hash, err := validateStagingOrigin(valid, modulePath, version, commit, "https://github.com/kubernetes/kubernetes")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if url == "" || hash == "" {
			t.Errorf("got url=%q hash=%q, want both non-empty", url, hash)
		}
	})
}

func TestValidateStagingOriginPseudoVersion(t *testing.T) {
	const (
		pseudo = "v0.0.0-20260102030405-333333333333"
		hash   = "3333333333333333333333333333333333333333"
		url    = "https://github.com/kubernetes/component-helpers"
	)
	tests := []struct {
		name string
		ref  string
		want string
	}{
		{name: "exact hash without discovery ref"},
		{name: "branch discovery ref", ref: "refs/heads/master"},
		{name: "contradictory tag ref", ref: "refs/tags/" + pseudo, want: "want empty or refs/heads/*"},
		{name: "malformed branch ref", ref: "refs/heads/bad ref", want: "must not contain control characters or spaces"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			origin := &gocli.ModuleOrigin{VCS: "git", URL: url, Hash: hash, Ref: test.ref}
			gotURL, gotHash, err := validateStagingOrigin(origin, "soapbox.test/component-helpers", pseudo, hash, "https://github.com/kubernetes/kubernetes.git")
			if test.want != "" {
				if err == nil {
					t.Fatal("expected an error")
				}
				if !strings.Contains(err.Error(), test.want) {
					t.Fatalf("error %q does not contain %q", err, test.want)
				}
				return
			}
			if err != nil {
				t.Fatalf("validate pseudo-version origin: %v", err)
			}
			if gotURL != url || gotHash != hash {
				t.Errorf("validated origin = %q %q, want %q %q", gotURL, gotHash, url, hash)
			}
		})
	}
}

func TestEvidenceForPackage(t *testing.T) {
	evidence := map[string]copyEvidence{
		"soapbox.test/component-helpers": {
			StagingDir:        "staging/src/soapbox.test/component-helpers",
			ModulePath:        "soapbox.test/component-helpers",
			DestinationPrefix: "internal/kk/staging/src/soapbox.test/component-helpers",
		},
	}

	tests := []struct {
		name       string
		pkg        relocate.Package
		evidence   map[string]copyEvidence
		wantModule string
		wantErr    string
	}{
		{
			name: "exact match",
			pkg: relocate.Package{
				Source: "staging/src/soapbox.test/component-helpers",
				Path:   "internal/kk/staging/src/soapbox.test/component-helpers",
			},
			evidence:   evidence,
			wantModule: "soapbox.test/component-helpers",
		},
		{
			name: "child match",
			pkg: relocate.Package{
				Source: "staging/src/soapbox.test/component-helpers/text/policy",
				Path:   "internal/kk/staging/src/soapbox.test/component-helpers/text/policy",
			},
			evidence:   evidence,
			wantModule: "soapbox.test/component-helpers",
		},
		{
			name: "no match",
			pkg: relocate.Package{
				Source: "staging/src/soapbox.test/other-module/pkg",
				Path:   "internal/kk/staging/src/soapbox.test/other-module/pkg",
			},
			evidence: evidence,
			wantErr:  "no measured evidence",
		},
		{
			name: "empty evidence",
			pkg: relocate.Package{
				Source: "staging/src/soapbox.test/component-helpers/text/policy",
				Path:   "internal/kk/staging/src/soapbox.test/component-helpers/text/policy",
			},
			evidence: map[string]copyEvidence{},
			wantErr:  "no measured evidence",
		},
		{
			name: "ambiguous match",
			pkg: relocate.Package{
				Source: "staging/src/soapbox.test/component-helpers/text/policy",
				Path:   "internal/kk/staging/src/soapbox.test/component-helpers/text/policy",
			},
			evidence: map[string]copyEvidence{
				"soapbox.test/component-helpers": {
					StagingDir:        "staging/src/soapbox.test/component-helpers",
					ModulePath:        "soapbox.test/component-helpers",
					DestinationPrefix: "internal/kk/staging/src/soapbox.test/component-helpers",
				},
				"soapbox.test/component-helpers/text": {
					StagingDir:        "staging/src/soapbox.test/component-helpers/text",
					ModulePath:        "soapbox.test/component-helpers/text",
					DestinationPrefix: "internal/kk/staging/src/soapbox.test/component-helpers/text",
				},
			},
			wantErr: "ambiguous evidence",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev, err := evidenceForPackage(tt.evidence, tt.pkg)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("got %+v, want error containing %q", ev, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if ev.ModulePath != tt.wantModule {
				t.Errorf("evidence module = %q, want %q", ev.ModulePath, tt.wantModule)
			}
		})
	}
}
