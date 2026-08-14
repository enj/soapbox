package config_test

import (
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/config"
)

// v1Profile is a complete, valid schema v1 profile with the old githubApp
// section. This is what a derived repository that has not yet been upgraded
// looks like.
const v1Profile = `version: 1
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
    maxPackageGrowth: 2
  golden: testdata/closure/rbac-v1.36.1.json
types:
  policy: prefer-external
  pairs:
    - internal: k8s.io/kubernetes/pkg/apis/rbac
      external: k8s.io/api/rbac/v1
dependencies:
  policy: external
  copyPackages: []
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
    - name: RBACAuthorizer
      kind: type
      source: k8s.io/kubernetes/plugin/pkg/auth/authorizer/rbac.RBACAuthorizer
  aliases:
    - name: RoleGetterFromLister
      kind: type
      source: k8s.io/kubernetes/plugin/pkg/auth/authorizer/rbac.RoleGetter
  interfaceAssertions:
    - type: RBACAuthorizer
      pointer: true
      interface: k8s.io/apiserver/pkg/authorization/authorizer.Authorizer
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
githubApp:
  appIDEnv: SOAPBOX_GITHUB_APP_ID
  installationIDEnv: SOAPBOX_GITHUB_INSTALLATION_ID
  privateKeyEnv: SOAPBOX_GITHUB_APP_PRIVATE_KEY
  apiBaseURL: https://api.github.com
determinism:
  toolchain: go1.26.5
  chunkSize: 200
`

func TestMigrateV1ToV2(t *testing.T) {
	migrated, err := config.MigrateV1ToV2([]byte(v1Profile))
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// The migrated bytes must decode as a valid v2 profile.
	cfg, err := config.Decode(migrated)
	if err != nil {
		t.Fatalf("decode migrated: %v", err)
	}
	if cfg.Version != config.SchemaVersion {
		t.Fatalf("version = %d, want %d", cfg.Version, config.SchemaVersion)
	}

	// The migration must set defaults for new fields.
	if cfg.Publication.Mode != config.PublicationModeManual {
		t.Errorf("publication.mode = %q, want %q", cfg.Publication.Mode, config.PublicationModeManual)
	}
	if cfg.Compatibility.Apiserver != config.CompatibilityApiserverExternal {
		t.Errorf("compatibility.apiserver = %q, want %q", cfg.Compatibility.Apiserver, config.CompatibilityApiserverExternal)
	}

	// The githubApp section must be gone.
	if strings.Contains(string(migrated), "githubApp") {
		t.Error("migrated profile still contains githubApp")
	}
	if strings.Contains(string(migrated), "appIDEnv") {
		t.Error("migrated profile still contains appIDEnv")
	}

	// forbiddenModules must be present.
	if strings.Contains(string(migrated), "forbiddenModules") {
		// Good — present in the output.
	} else {
		t.Error("migrated profile does not contain forbiddenModules")
	}

	// Existing fields must survive.
	if cfg.Source.ImportPrefix != "k8s.io/kubernetes" {
		t.Errorf("source.importPrefix = %q", cfg.Source.ImportPrefix)
	}
	if cfg.Determinism.Toolchain != "go1.26.5" {
		t.Errorf("determinism.toolchain = %q", cfg.Determinism.Toolchain)
	}
}

func TestMigrateV1ToV2RefusesV2(t *testing.T) {
	_, err := config.MigrateV1ToV2([]byte(baseProfile))
	if err == nil {
		t.Fatal("expected error migrating a v2 profile")
	}
	// A v2 profile has fields the v1 struct does not, so the strict decoder
	// rejects it before the version check runs.
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %v, want mention of unknown field", err)
	}
}

func TestDecodeWithMigrationV1(t *testing.T) {
	cfg, migrated, err := config.DecodeWithMigration([]byte(v1Profile))
	if err != nil {
		t.Fatalf("decode with migration: %v", err)
	}
	if cfg.Version != config.SchemaVersion {
		t.Fatalf("version = %d, want %d", cfg.Version, config.SchemaVersion)
	}
	if cfg.Publication.Mode != config.PublicationModeManual {
		t.Errorf("publication.mode = %q", cfg.Publication.Mode)
	}
	if strings.Contains(string(migrated), "githubApp") {
		t.Error("migrated bytes still contain githubApp")
	}
}

func TestDecodeWithMigrationV2(t *testing.T) {
	cfg, _, err := config.DecodeWithMigration([]byte(baseProfile))
	if err != nil {
		t.Fatalf("decode with migration: %v", err)
	}
	if cfg.Version != config.SchemaVersion {
		t.Fatalf("version = %d, want %d", cfg.Version, config.SchemaVersion)
	}
}

func TestMigrateV1ToV2Rejections(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(string) string
		wantErr string
	}{
		{
			name: "unknown field in v1 profile",
			mutate: func(s string) string {
				return s + "unknownField: true\n"
			},
			wantErr: "not found",
		},
		{
			name: "missing githubApp section",
			mutate: func(s string) string {
				// Remove the githubApp block entirely.
				lines := strings.Split(s, "\n")
				var out []string
				skip := false
				for _, line := range lines {
					if strings.HasPrefix(line, "githubApp:") {
						skip = true
						continue
					}
					if skip && (strings.HasPrefix(line, "  ") || line == "") {
						continue
					}
					skip = false
					out = append(out, line)
				}
				return strings.Join(out, "\n")
			},
			wantErr: "githubApp",
		},
		{
			name: "duplicate key",
			mutate: func(s string) string {
				return "version: 1\n" + s
			},
			wantErr: "already defined",
		},
		{
			name: "missing App API URL",
			mutate: func(s string) string {
				return strings.Replace(s, "  apiBaseURL: https://api.github.com\n", "", 1)
			},
			wantErr: "apiBaseURL",
		},
		{
			name: "invalid App environment name",
			mutate: func(s string) string {
				return strings.Replace(s, "SOAPBOX_GITHUB_APP_ID", "not-valid", 1)
			},
			wantErr: "githubApp.appIDEnv",
		},
		{
			name: "duplicate App environment name",
			mutate: func(s string) string {
				return strings.Replace(s, "SOAPBOX_GITHUB_INSTALLATION_ID", "SOAPBOX_GITHUB_APP_ID", 1)
			},
			wantErr: "distinct",
		},
		{
			name: "unapproved App API host",
			mutate: func(s string) string {
				return strings.Replace(s, "https://api.github.com", "https://example.invalid", 1)
			},
			wantErr: "githubApp.apiBaseURL",
		},
		{
			name: "invalid shared profile field",
			mutate: func(s string) string {
				return strings.Replace(s, "license: Apache-2.0", "license: LicenseRef-Unknown", 1)
			},
			wantErr: "source.license",
		},
		{
			name: "version 0",
			mutate: func(s string) string {
				return strings.Replace(s, "version: 1", "version: 0", 1)
			},
			wantErr: "version",
		},
		{
			name: "empty document",
			mutate: func(_ string) string {
				return ""
			},
			wantErr: "empty",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := test.mutate(v1Profile)
			_, err := config.MigrateV1ToV2([]byte(input))
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Errorf("error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestMigratedProfileRoundTrips(t *testing.T) {
	migrated, err := config.MigrateV1ToV2([]byte(v1Profile))
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	cfg, err := config.Decode(migrated)
	if err != nil {
		t.Fatalf("decode migrated: %v", err)
	}
	canonical, err := cfg.Canonical()
	if err != nil {
		t.Fatalf("canonical: %v", err)
	}
	reloaded, err := config.Decode(canonical)
	if err != nil {
		t.Fatalf("decode canonical: %v", err)
	}
	again, err := reloaded.Canonical()
	if err != nil {
		t.Fatalf("canonical of reloaded: %v", err)
	}
	if string(canonical) != string(again) {
		t.Fatal("migrated profile does not round trip through the strict schema")
	}
}
