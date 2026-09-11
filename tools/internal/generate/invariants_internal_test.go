package generate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/closure"
	"github.com/enj/soapbox/tools/internal/config"
	"github.com/enj/soapbox/tools/internal/extract"
	"github.com/enj/soapbox/tools/internal/gomodmap"
	"github.com/enj/soapbox/tools/internal/modgen"
	"github.com/enj/soapbox/tools/internal/relocate"
	"github.com/enj/soapbox/tools/internal/typeswap"
)

func TestCloneConfigIsDeep(t *testing.T) {
	profile := filepath.Join("..", "..", "..", "soapbox.yaml")
	cfg, err := config.Load(t.Context(), profile)
	if err != nil {
		t.Fatalf("load profile: %v", err)
	}
	clone, err := cloneConfig(cfg)
	if err != nil {
		t.Fatalf("clone profile: %v", err)
	}

	originalRoot := cfg.Packages.Roots[0]
	clone.Packages.Roots[0] = "changed/by/clone"
	if cfg.Packages.Roots[0] != originalRoot {
		t.Fatal("mutating the clone changed the caller's package roots")
	}

	originalPrune := clone.Prune.Files[0]
	cfg.Prune.Files[0] = "changed/by/caller"
	if clone.Prune.Files[0] != originalPrune {
		t.Fatal("mutating the caller changed the cloned prune list")
	}
}

func TestCheckDisjointAllowsIndexOnlyInCache(t *testing.T) {
	root := t.TempDir()
	cache := namedDir{name: "source cache root", path: filepath.Join(root, "cache")}
	insideCache := namedDir{name: "version index", path: filepath.Join(cache.path, "staging-versions.json"), file: true}
	if err := checkDisjoint(cache, insideCache); err != nil {
		t.Fatalf("version index in cache root: %v", err)
	}

	profile := namedDir{name: "profile directory", path: filepath.Join(root, "profile")}
	insideProfile := namedDir{name: "version index", path: filepath.Join(profile.path, "staging-versions.json"), file: true}
	if err := checkDisjoint(profile, insideProfile); !errors.Is(err, ErrPathConflict) {
		t.Fatalf("version index in profile error = %v, want ErrPathConflict", err)
	}
}

func TestPrePruneConfigDropsPostPruneLimits(t *testing.T) {
	cfg := &config.Config{
		Prune: config.Prune{Files: []string{"x.go"}},
		Deny:  config.Deny{Imports: []string{"example.com/internal"}},
		Closure: config.Closure{
			Golden: "shape.json",
			Limits: config.ClosureLimits{MaxPackages: 3, MaxPackageGrowth: 2},
		},
	}
	baseline := prePruneConfig(cfg)
	if baseline.Closure.Golden != "" || baseline.Closure.Limits != (config.ClosureLimits{}) {
		t.Fatalf("baseline closure controls = %#v, want no golden or post-prune limits", baseline.Closure)
	}
	if len(baseline.Prune.Files) != 0 || len(baseline.Deny.Imports) != 0 {
		t.Fatalf("baseline retained prune/deny controls: %#v %#v", baseline.Prune, baseline.Deny)
	}
	if cfg.Closure.Limits.MaxPackageGrowth != 2 || len(cfg.Prune.Files) != 1 {
		t.Fatal("building the baseline mutated the caller's profile")
	}
}

func TestPrepareUpstreamModuleCopiesOnlyProductionClosure(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	packageDir := filepath.Join(source, "pkg", "app")
	if err := os.MkdirAll(packageDir, 0o750); err != nil {
		t.Fatalf("source package directory: %v", err)
	}
	files := map[string]string{
		"app.go":      "package app\n",
		"app_test.go": "package app\n",
		"extra.go":    "package app\n",
		"tool":        "fixture tool\n",
	}
	for name, contents := range files {
		if err := os.WriteFile(filepath.Join(packageDir, name), []byte(contents), 0o600); err != nil {
			t.Fatalf("source file %s: %v", name, err)
		}
	}
	if err := os.Chmod(filepath.Join(packageDir, "tool"), 0o751); err != nil {
		t.Fatalf("source tool mode: %v", err)
	}

	r := &run{
		paths: Paths{Work: filepath.Join(root, "work"), PreWorktree: source},
		pre: &extract.Result{Report: extract.Report{
			Closure: extract.ClosureReport{Report: closure.ClosureReport{Exact: closure.ExactShape{Files: []string{
				"pkg/app/app.go",
				"pkg/app/app_test.go",
				"pkg/app/tool",
			}}}},
		}},
	}
	dir, err := r.prepareUpstreamModule(t.Context())
	if err != nil {
		t.Fatalf("prepare upstream module: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "pkg", "app", "app.go"))
	if err != nil {
		t.Fatalf("read copied production file: %v", err)
	}
	if string(got) != files["app.go"] {
		t.Fatalf("copied production file = %q, want %q", got, files["app.go"])
	}
	for _, name := range []string{"app_test.go", "extra.go"} {
		if _, err := os.Stat(filepath.Join(dir, "pkg", "app", name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("excluded file %s stat error = %v, want not exist", name, err)
		}
	}
	info, err := os.Stat(filepath.Join(dir, "pkg", "app", "tool"))
	if err != nil {
		t.Fatalf("inspect copied tool: %v", err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o751); got != want {
		t.Fatalf("copied tool mode = %v, want %v", got, want)
	}
}

func TestCheckSubstitutionsRefusesUnappliedRewrites(t *testing.T) {
	err := checkSubstitutions(&typeswap.Result{Pairs: []typeswap.PairReport{{
		Internal: "k8s.io/kubernetes/pkg/apis/example",
		External: "k8s.io/api/example/v1",
		Action:   typeswap.ActionRewriteReferences,
		Rewrites: []typeswap.Rewrite{{
			Package:     "k8s.io/kubernetes/pkg/consumer",
			Symbol:      "k8s.io/kubernetes/pkg/apis/example.Value",
			Replacement: "k8s.io/api/example/v1.Value",
		}},
	}}})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("rewrite error = %v, want ErrUnsupported", err)
	}
	if !strings.Contains(err.Error(), "requires rewriting 1 retained references") {
		t.Fatalf("rewrite error does not explain the unapplied edit: %v", err)
	}
}

func TestResolveStagingStartsWithAnEmptyIndex(t *testing.T) {
	root, err := gomodmap.ParseRootModule("go.mod", []byte(`module k8s.io/kubernetes

go 1.26.0

require k8s.io/api v0.0.0

replace k8s.io/api => ./staging/src/k8s.io/api
`))
	if err != nil {
		t.Fatalf("parse root module: %v", err)
	}
	store := filepath.Join(t.TempDir(), "state", "versions.json")
	r := &run{opts: Options{StorePath: store, Offline: true}}
	_, _, err = r.resolveStaging(t.Context(), root, strings.Repeat("c", 40))
	if err == nil {
		t.Fatal("offline resolution with a cold index unexpectedly succeeded")
	}
	if errors.Is(err, gomodmap.ErrIndexMissing) {
		t.Fatalf("cold index was treated as corruption instead of an empty cache: %v", err)
	}
	if !strings.Contains(err.Error(), "holds no entry") {
		t.Fatalf("cold offline error = %v, want the missing commit explanation", err)
	}
}

func TestModuleMappingsDescribeOnlyKeptRequirements(t *testing.T) {
	root, err := gomodmap.ParseRootModule("go.mod", []byte(`module k8s.io/kubernetes

go 1.26.0

require (
	k8s.io/api v0.0.0
	k8s.io/apiserver v0.0.0
)

replace (
	k8s.io/api => ./staging/src/k8s.io/api
	k8s.io/apiserver => ./staging/src/k8s.io/apiserver
)
`))
	if err != nil {
		t.Fatalf("parse root module: %v", err)
	}
	r := &run{
		root: root,
		staging: []gomodmap.ModuleVersion{
			{Path: "k8s.io/api", Version: "v0.36.1", Commit: strings.Repeat("a", 40)},
			{Path: "k8s.io/apiserver", Version: "v0.36.1", Commit: strings.Repeat("b", 40)},
		},
		moduleReport: &modgen.Report{Kept: []gomodmap.Requirement{{Path: "k8s.io/api", Version: "v0.36.1"}}},
	}

	mappings, err := r.moduleMappings()
	if err != nil {
		t.Fatalf("module mappings: %v", err)
	}
	if len(mappings) != 1 || mappings[0].Module != "k8s.io/api" {
		t.Fatalf("module mappings = %#v, want only the kept k8s.io/api pin", mappings)
	}
}

func TestExecuteRecordsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r := &run{}
	if err := r.execute(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("execute error = %v, want context.Canceled", err)
	}
	if r.report.Failure == nil || r.report.Failure.Stage != stageExtract {
		t.Fatalf("cancellation failure = %#v, want extract stage", r.report.Failure)
	}
}

func TestRecordOutputCountsTheRootFacadePackage(t *testing.T) {
	report := Report{}
	report.recordOutput(relocate.FileSet{
		Files:    []relocate.File{{Path: "authorizer.go"}, {Path: "internal/kk/pkg/rbac/rbac.go", Package: "internal/kk/pkg/rbac"}},
		Packages: []relocate.Package{{Path: "internal/kk/pkg/rbac"}},
	}, false)
	if report.Output.Packages != 2 {
		t.Fatalf("output packages = %d, want relocated package plus root facade", report.Output.Packages)
	}
}

func TestReportJSONDoesNotEscapeEvidence(t *testing.T) {
	report := Report{Schema: ReportSchema, Failure: &FailureReport{Message: "<work> && <proxy>"}}
	data, err := report.JSON()
	if err != nil {
		t.Fatalf("report JSON: %v", err)
	}
	if !strings.Contains(string(data), "<work> && <proxy>") {
		t.Fatalf("report JSON escaped evidence: %s", data)
	}
}

func TestRecordExtractKeepsOnlyPostPruneNotices(t *testing.T) {
	report := Report{}
	report.recordExtract(
		&extract.Result{Report: extract.Report{Notices: []string{"baseline-only marker"}}},
		&extract.Result{Report: extract.Report{Notices: []string{"published-tree notice"}}},
	)
	if got, want := strings.Join(report.Notices, "\n"), "published-tree notice"; got != want {
		t.Fatalf("generation notices = %q, want %q", got, want)
	}
}

func TestReportScrubsLoaderEnvironment(t *testing.T) {
	root := t.TempDir()
	cache := filepath.Join(root, "go-cache")
	proxy := "file://" + filepath.Join(root, "proxy")
	report := Report{}
	report.init(Options{Config: &config.Config{Destination: config.Destination{Module: "example.com/generated"}, Facade: config.Facade{Package: "generated"}}})
	report.addLoaderEnvironment([]string{"GOCACHE=" + cache, "GOPROXY=" + proxy})
	report.fail(stageModule, errors.New("read "+proxy+" using "+cache))
	if strings.Contains(report.Failure.Message, root) || strings.Contains(report.Failure.Message, proxy) {
		t.Fatalf("failure leaked loader environment: %q", report.Failure.Message)
	}
}

func TestReportScrubsSourceRemote(t *testing.T) {
	root := t.TempDir()
	mirror := filepath.Join(root, "private", "mirror.git")
	remote := "file://" + mirror
	report := Report{}
	report.init(Options{
		Config: &config.Config{
			Destination: config.Destination{Module: "example.com/generated"},
			Facade:      config.Facade{Package: "generated"},
			Determinism: config.Determinism{Toolchain: "go1.26.5"},
		},
		WorkRoot:     filepath.Join(root, "work"),
		CacheRoot:    filepath.Join(root, "cache"),
		OutputRoot:   filepath.Join(root, "output"),
		ProfileDir:   filepath.Join(root, "profile"),
		StorePath:    filepath.Join(root, "cache", "versions.json"),
		SourceRemote: remote,
	})
	report.fail(stageExtract, errors.New("fetch "+remote+" from "+mirror))
	if strings.Contains(report.Failure.Message, remote) || strings.Contains(report.Failure.Message, mirror) {
		t.Fatalf("failure leaked source remote: %q", report.Failure.Message)
	}
	if !strings.Contains(report.Failure.Message, "<source-remote>") {
		t.Fatalf("failure did not mark the scrubbed source remote: %q", report.Failure.Message)
	}
}

func TestFormatComposedGoFiles(t *testing.T) {
	original := relocate.FileSet{Files: []relocate.File{
		{Path: "pkg/p.go", Mode: relocate.ModeRegular, Contents: []byte("package p\nvar X=1\n")},
		{Path: "README.md", Mode: relocate.ModeRegular, Contents: []byte("unchanged\n")},
	}}
	formatted, err := formatComposedGoFiles(original)
	if err != nil {
		t.Fatalf("format composed files: %v", err)
	}
	if got, want := string(formatted.Files[0].Contents), "package p\n\nvar X = 1\n"; got != want {
		t.Errorf("formatted Go file = %q, want %q", got, want)
	}
	if got := string(formatted.Files[1].Contents); got != "unchanged\n" {
		t.Errorf("formatted non-Go file = %q", got)
	}
	if got := string(original.Files[0].Contents); got != "package p\nvar X=1\n" {
		t.Errorf("formatting mutated input = %q", got)
	}
}

func TestFormatComposedGoFilesRejectsInvalidSource(t *testing.T) {
	_, err := formatComposedGoFiles(relocate.FileSet{Files: []relocate.File{{
		Path: "pkg/broken.go", Mode: relocate.ModeRegular, Contents: []byte("package"),
	}}})
	if err == nil || !strings.Contains(err.Error(), "pkg/broken.go") {
		t.Fatalf("format composed files error = %v, want path-specific syntax failure", err)
	}
}
