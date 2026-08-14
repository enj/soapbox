package config_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/config"
)

func TestDecodeAcceptsTheBaseProfile(t *testing.T) {
	cfg, err := config.Decode([]byte(baseProfile))
	if err != nil {
		t.Fatalf("decode base profile: %v", err)
	}
	if cfg.Version != config.SchemaVersion {
		t.Fatalf("version = %d, want %d", cfg.Version, config.SchemaVersion)
	}
	if cfg.Source.ImportPrefix != "k8s.io/kubernetes" {
		t.Fatalf("import prefix = %q", cfg.Source.ImportPrefix)
	}
}

func TestDecodeRejectsMalformedDocuments(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty", in: "", want: "document is empty"},
		{name: "only comments", in: "# nothing here\n", want: "document is empty"},
		{name: "not a mapping", in: "- a\n- b\n", want: "cannot unmarshal"},
		{name: "unterminated quote", in: "version: \"1\ndestination:\n", want: "found unexpected end of stream"},
		{name: "tab indentation", in: "version: 1\nsource:\n\trepository: x\n", want: "found character that cannot start any token"},
		{name: "duplicate key", in: "version: 1\nversion: 1\n", want: `mapping key "version" already defined`},
		{
			name: "multiple documents",
			in:   baseProfile + "---\n" + baseProfile,
			want: "multiple YAML documents are not supported",
		},
		{
			name: "trailing document separator with content",
			in:   baseProfile + "---\nversion: 1\n",
			want: "multiple YAML documents are not supported",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := config.Decode([]byte(test.in))
			if err == nil {
				t.Fatalf("expected an error containing %q", test.want)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error does not contain %q:\n%v", test.want, err)
			}
		})
	}
}

func TestLoadReportsFileProblems(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()

	if _, err := config.Load(ctx, filepath.Join(dir, "absent.yaml")); err == nil {
		t.Fatal("expected an error for a missing profile")
	} else if !strings.Contains(err.Error(), "read profile") {
		t.Fatalf("error %q is not a read failure", err)
	}

	path := filepath.Join(dir, config.DefaultFileName)
	if err := os.WriteFile(path, []byte(baseProfile), 0o600); err != nil {
		t.Fatalf("write profile: %v", err)
	}
	if _, err := config.Load(ctx, path); err != nil {
		t.Fatalf("load profile: %v", err)
	}

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := config.Load(canceled, path); !errors.Is(err, context.Canceled) {
		t.Fatalf("error %v is not context.Canceled", err)
	}
}

func TestCanonicalRoundTripsAndIsIdempotent(t *testing.T) {
	cfg, err := config.Decode([]byte(baseProfile))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	canonical, err := cfg.Canonical()
	if err != nil {
		t.Fatalf("canonical: %v", err)
	}
	reloaded, err := config.Decode(canonical)
	if err != nil {
		t.Fatalf("decode canonical bytes: %v", err)
	}
	again, err := reloaded.Canonical()
	if err != nil {
		t.Fatalf("canonical of reloaded profile: %v", err)
	}
	if string(canonical) != string(again) {
		t.Fatalf("canonical encoding is not idempotent:\nfirst:\n%s\nsecond:\n%s", canonical, again)
	}
}

func TestNormalizationIsOrderIndependent(t *testing.T) {
	// The same profile with reordered set-like collections and a differently
	// cased host must produce identical canonical bytes, because the replay
	// profile hash is computed from them.
	reordered := mutate(t, baseProfile, []mutation{
		{
			old: "  files:\n    - pkg/apis/rbac/v1/register.go\n",
			new: "  files:\n    - pkg/apis/rbac/v1/register.go\n    - pkg/apis/rbac/v1/defaults.go\n",
		},
		{
			old: "      - master\n",
			new: "      - release-1.36\n      - master\n",
		},
	})
	sorted := mutate(t, baseProfile, []mutation{
		{
			old: "  files:\n    - pkg/apis/rbac/v1/register.go\n",
			new: "  files:\n    - pkg/apis/rbac/v1/defaults.go\n    - pkg/apis/rbac/v1/register.go\n",
		},
		{
			old: "      - master\n",
			new: "      - master\n      - release-1.36\n",
		},
		{
			old: "repository: https://github.com/kubernetes/kubernetes.git",
			new: "repository: https://GitHub.com/kubernetes/kubernetes.git",
		},
	})

	first := canonicalOf(t, reordered)
	second := canonicalOf(t, sorted)
	if first != second {
		t.Fatalf("normalization is order dependent:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

func TestProfileBytesExcludeObservationalGates(t *testing.T) {
	base, err := config.Decode([]byte(baseProfile))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	relaxed, err := config.Decode([]byte(mutate(t, baseProfile, []mutation{
		{old: "maxNonTestLines: 5000", new: "maxNonTestLines: 9000"},
		{old: "golden: testdata/closure/rbac-v1.36.1.json", new: "golden: testdata/closure/other.json"},
	})))
	if err != nil {
		t.Fatalf("decode relaxed profile: %v", err)
	}

	baseProfileBytes, err := base.ProfileBytes()
	if err != nil {
		t.Fatalf("profile bytes: %v", err)
	}
	relaxedProfileBytes, err := relaxed.ProfileBytes()
	if err != nil {
		t.Fatalf("profile bytes: %v", err)
	}
	if string(baseProfileBytes) != string(relaxedProfileBytes) {
		t.Fatal("observational closure limits changed the replay profile identity")
	}
	if strings.Contains(string(baseProfileBytes), "rbac-v1.36.1.json") {
		t.Fatalf("profile bytes still carry the closure golden:\n%s", baseProfileBytes)
	}

	// An output affecting change must still change the profile identity.
	renamed, err := config.Decode([]byte(mutate(t, baseProfile, []mutation{
		{old: "internalPrefix: internal/kk", new: "internalPrefix: internal/upstream"},
	})))
	if err != nil {
		t.Fatalf("decode renamed profile: %v", err)
	}
	renamedProfileBytes, err := renamed.ProfileBytes()
	if err != nil {
		t.Fatalf("profile bytes: %v", err)
	}
	if string(baseProfileBytes) == string(renamedProfileBytes) {
		t.Fatal("relocation prefix did not change the replay profile identity")
	}
}

func TestRepositoryProfileRoundTrips(t *testing.T) {
	path := filepath.Join("..", "..", "..", config.DefaultFileName)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		t.Skipf("%s is not present in this checkout", path)
	}
	cfg, err := config.Load(t.Context(), path)
	if err != nil {
		t.Fatalf("load repository profile: %v", err)
	}

	// The shipped RBAC profile is the reference example, so its invariants are
	// asserted here rather than described only in prose.
	if got, want := len(cfg.Prune.Files), 8; got != want {
		t.Fatalf("prune files = %d, want %d", got, want)
	}
	if got, want := cfg.Deny.Imports, []string{"k8s.io/kubernetes/pkg/apis/rbac"}; len(got) != 1 || got[0] != want[0] {
		t.Fatalf("deny imports = %v, want %v", got, want)
	}
	if len(cfg.Dependencies.CopyPackages) != 1 {
		t.Fatalf("copy packages = %v, want one", cfg.Dependencies.CopyPackages)
	}
	if cfg.Dependencies.CopyPackages[0] != "staging/src/k8s.io/component-helpers/auth/rbac/validation" {
		t.Fatalf("copy package = %q", cfg.Dependencies.CopyPackages[0])
	}
	if cfg.Dependencies.Policy != config.DependencyPolicyCopyApproved {
		t.Fatalf("dependency policy = %q", cfg.Dependencies.Policy)
	}
	wantForbidden := []string{"k8s.io/apiserver", "k8s.io/component-helpers"}
	if !slices.Equal(cfg.Dependencies.ForbiddenModules, wantForbidden) {
		t.Fatalf("forbidden modules = %v, want %v", cfg.Dependencies.ForbiddenModules, wantForbidden)
	}
	if cfg.Compatibility.Apiserver != config.CompatibilityApiserverLocal {
		t.Fatalf("apiserver compatibility = %q, want local", cfg.Compatibility.Apiserver)
	}
	if cfg.Types.Policy != config.TypePolicyPreferExternal {
		t.Fatalf("type policy = %q", cfg.Types.Policy)
	}
	if cfg.Release.Policy != config.ReleasePolicyV1ToV0 || cfg.Release.FirstTag != "v0.36.1" {
		t.Fatalf("release = %+v", cfg.Release)
	}
	if cfg.Source.Refs.MinimumRelease != "v1.36.1" {
		t.Fatalf("minimum release = %q", cfg.Source.Refs.MinimumRelease)
	}
	if cfg.Determinism.Toolchain != "go1.26.5" {
		t.Fatalf("toolchain = %q", cfg.Determinism.Toolchain)
	}
	if len(cfg.Facade.Aliases) != 4 {
		t.Fatalf("facade aliases = %d, want the four lister backed adapters", len(cfg.Facade.Aliases))
	}

	canonical, err := cfg.Canonical()
	if err != nil {
		t.Fatalf("canonical: %v", err)
	}
	reloaded, err := config.Decode(canonical)
	if err != nil {
		t.Fatalf("decode canonical repository profile: %v", err)
	}
	again, err := reloaded.Canonical()
	if err != nil {
		t.Fatalf("canonical of reloaded repository profile: %v", err)
	}
	if string(canonical) != string(again) {
		t.Fatal("the repository profile does not round trip through the strict schema")
	}
}

// canonicalOf decodes a profile and returns its canonical encoding.
func canonicalOf(t *testing.T, profile string) string {
	t.Helper()
	cfg, err := config.Decode([]byte(profile))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	canonical, err := cfg.Canonical()
	if err != nil {
		t.Fatalf("canonical: %v", err)
	}
	return string(canonical)
}
