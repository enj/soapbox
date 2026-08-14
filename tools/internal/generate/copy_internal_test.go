package generate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/config"
	"github.com/enj/soapbox/tools/internal/deppolicy"
	"github.com/enj/soapbox/tools/internal/gomodmap"
	"github.com/enj/soapbox/tools/internal/modgen"
	"github.com/enj/soapbox/tools/internal/relocate"
)

// testRewriteConfig returns the minimal config fields rewriteForStagingModule
// reads: Destination.Module, Destination.InternalPrefix, Source.Repository.
func testRewriteConfig() *config.Config {
	return &config.Config{
		Source: config.Source{
			Repository: "https://github.com/kubernetes/kubernetes.git",
		},
		Destination: config.Destination{
			Module:         "example.com/gen",
			InternalPrefix: "internal/kk",
		},
	}
}

func TestReadCandidatePackageRefusesIgnoredFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package p\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	pkg := &deppolicy.Package{
		ImportPath:   "example.com/p",
		Dir:          dir,
		GoFiles:      []string{"a.go"},
		IgnoredFiles: []string{"a_linux.go"},
	}
	_, err := readCandidatePackage(context.Background(), "staging/src/example.com/p", pkg)
	if err == nil {
		t.Fatal("expected error for ignored files, got nil")
	}
	if !errors.Is(err, ErrCopyNotPortable) {
		t.Errorf("error = %v, want it to wrap ErrCopyNotPortable", err)
	}
}

func TestReadCandidatePackageRefusesNoDir(t *testing.T) {
	pkg := &deppolicy.Package{
		ImportPath: "example.com/p",
		Dir:        "",
		GoFiles:    []string{"a.go"},
	}
	_, err := readCandidatePackage(context.Background(), "staging/src/example.com/p", pkg)
	if err == nil || !strings.Contains(err.Error(), "no resolved directory") {
		t.Errorf("error = %v, want it to mention no resolved directory", err)
	}
}

func TestReadCandidatePackageRefusesNoFiles(t *testing.T) {
	pkg := &deppolicy.Package{
		ImportPath: "example.com/p",
		Dir:        t.TempDir(),
	}
	_, err := readCandidatePackage(context.Background(), "staging/src/example.com/p", pkg)
	if err == nil || !strings.Contains(err.Error(), "no files") {
		t.Errorf("error = %v, want it to mention no files", err)
	}
}

func TestReadCandidatePackageReadsFromDir(t *testing.T) {
	dir := t.TempDir()
	contents := []byte("package validation\n")
	if err := os.WriteFile(filepath.Join(dir, "policy.go"), contents, 0o644); err != nil {
		t.Fatal(err)
	}

	pkg := &deppolicy.Package{
		ImportPath: "k8s.io/component-helpers/auth/rbac/validation",
		Dir:        dir,
		GoFiles:    []string{"policy.go"},
	}
	files, err := readCandidatePackage(context.Background(), "staging/src/k8s.io/component-helpers/auth/rbac/validation", pkg)
	if err != nil {
		t.Fatalf("readCandidatePackage: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("got %d files, want 1", len(files))
	}
	file := files[0]
	if file.Path != "staging/src/k8s.io/component-helpers/auth/rbac/validation/policy.go" {
		t.Errorf("path = %s, want staging path prefix", file.Path)
	}
	if file.Package != "staging/src/k8s.io/component-helpers/auth/rbac/validation" {
		t.Errorf("package = %s, want staging path", file.Package)
	}
	if string(file.Contents) != string(contents) {
		t.Errorf("contents = %q, want %q", file.Contents, contents)
	}
	if file.Mode != relocate.ModeRegular {
		t.Errorf("mode = %v, want regular", file.Mode)
	}
}

func TestMaterializeCopiesRefusesDuplicateStagingPath(t *testing.T) {
	graph := &deppolicy.Graph{
		Candidates: []deppolicy.Candidate{
			{StagingPath: "staging/src/example.com/dup"},
			{StagingPath: "staging/src/example.com/dup"},
		},
	}
	result := &deppolicy.Result{
		Copy: []string{"staging/src/example.com/dup"},
	}

	r := &run{}
	_, err := r.materializeCopies(context.Background(), result, graph)
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("error = %v, want duplicate staging path refusal", err)
	}
}

func TestMaterializeCopiesRefusesNilPackage(t *testing.T) {
	graph := &deppolicy.Graph{
		Candidates: []deppolicy.Candidate{
			{StagingPath: "staging/src/example.com/m/pkg", Package: nil},
		},
	}
	result := &deppolicy.Result{
		Copy: []string{"staging/src/example.com/m/pkg"},
	}
	r := &run{
		copyEvidence: map[string]copyEvidence{
			"example.com/m": {
				StagingDir: "staging/src/example.com/m",
				ModulePath: "example.com/m",
			},
		},
	}
	_, err := r.materializeCopies(context.Background(), result, graph)
	if err == nil || !strings.Contains(err.Error(), "no loaded package") {
		t.Errorf("error = %v, want nil package refusal", err)
	}
}

func TestRewriteForStagingModuleCopiedFileGetsNotice(t *testing.T) {
	// A file that imports the staging module should get a notice when
	// noNotice=false (the copy path).
	src := `package validation

import "soapbox.test/helpers/auth"

var _ = auth.Check
`
	set := relocate.FileSet{
		Files: []relocate.File{{
			Path:     "internal/kk/staging/src/soapbox.test/helpers/auth/rbac/check.go",
			Source:   "staging/src/soapbox.test/helpers/auth/rbac/check.go",
			Package:  "internal/kk/staging/src/soapbox.test/helpers/auth/rbac",
			Mode:     relocate.ModeRegular,
			Contents: []byte(src),
		}},
	}

	cfg := testRewriteConfig()
	resultSet, changes, err := rewriteForStagingModule(context.Background(), set, cfg,
		stagingModuleInfo{modulePath: "soapbox.test/helpers", dir: "staging/src/soapbox.test/helpers"}, false)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	contents := string(resultSet.Files[0].Contents)
	if !strings.Contains(contents, "modified by soapbox") {
		t.Error("copied file with changed imports should receive a modification notice")
	}
	// The rewrite must record the import change in the changes map.
	filePath := set.Files[0].Path
	if len(changes[filePath]) == 0 {
		t.Error("copied file with changed imports should have recorded changes for provenance")
	}
}

func TestRewriteForStagingModuleRetainedFileNoSecondNotice(t *testing.T) {
	// A file that already carries an extraction notice and imports the
	// staging module should NOT get a second notice when noNotice=true
	// (the retained path).
	src := `// This file was modified by soapbox and is not the upstream original.
// Imports under k8s.io/kubernetes were rewritten to example.com/gen/internal/kk.
package validation

import "soapbox.test/helpers/auth"

var _ = auth.Check
`
	set := relocate.FileSet{
		Files: []relocate.File{{
			Path:     "internal/kk/pkg/registry/rbac/validation/rule.go",
			Source:   "pkg/registry/rbac/validation/rule.go",
			Package:  "internal/kk/pkg/registry/rbac/validation",
			Mode:     relocate.ModeRegular,
			Contents: []byte(src),
		}},
	}

	cfg := testRewriteConfig()
	resultSet, changes, err := rewriteForStagingModule(context.Background(), set, cfg,
		stagingModuleInfo{modulePath: "soapbox.test/helpers", dir: "staging/src/soapbox.test/helpers"}, true)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	contents := string(resultSet.Files[0].Contents)
	count := strings.Count(contents, "modified by soapbox")
	if count != 1 {
		t.Errorf("retained file has %d modification notices, want exactly 1 (the original)", count)
	}
	// The retained file's import WAS rewritten, so the change must be recorded
	// even though no notice was added.
	filePath := set.Files[0].Path
	if len(changes[filePath]) == 0 {
		t.Error("retained file with changed imports should have recorded changes for provenance")
	}
}

func TestRewriteForStagingModuleUnchangedFileNoNotice(t *testing.T) {
	// A file that does NOT import the staging module should get no notice
	// regardless of the noNotice setting, because rewrite.GoFile produces
	// zero edits and returns the original bytes unchanged.
	src := `package validation

import "fmt"

var _ = fmt.Println
`
	set := relocate.FileSet{
		Files: []relocate.File{{
			Path:     "internal/kk/pkg/validation/rule.go",
			Source:   "pkg/validation/rule.go",
			Package:  "internal/kk/pkg/validation",
			Mode:     relocate.ModeRegular,
			Contents: []byte(src),
		}},
	}

	cfg := testRewriteConfig()
	resultSet, changes, err := rewriteForStagingModule(context.Background(), set, cfg,
		stagingModuleInfo{modulePath: "soapbox.test/helpers", dir: "staging/src/soapbox.test/helpers"}, false)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if string(resultSet.Files[0].Contents) != src {
		t.Error("file with no matching imports should be returned byte-for-byte unchanged")
	}
	if len(changes) != 0 {
		t.Errorf("file with no matching imports should produce no changes, got %d files with changes", len(changes))
	}
}

func TestRefreshModuleReportAbsentGoSumIsEmpty(t *testing.T) {
	dir := t.TempDir()
	goMod := []byte("module example.com/m\n\ngo 1.26.0\n")
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), goMod, 0o644); err != nil {
		t.Fatal(err)
	}
	// No go.sum — the module has no external deps.

	r := &run{
		paths:        Paths{PostModule: dir},
		moduleReport: &modgen.Report{},
		report:       Report{Module: ModuleReport{BaselineGoModHash: "baseline"}},
	}
	if err := r.refreshModuleReport(context.Background()); err != nil {
		t.Fatalf("refreshModuleReport: %v", err)
	}
	if !strings.Contains(string(r.moduleReport.GoMod), "example.com/m") {
		t.Error("go.mod was not refreshed")
	}
	if len(r.moduleReport.GoSum) != 0 {
		t.Errorf("go.sum = %q, want empty for absent file", r.moduleReport.GoSum)
	}
	if got, want := r.report.Module.GoModHash, contentDigest(goMod); got != want {
		t.Errorf("report go.mod hash = %q, want %q", got, want)
	}
	if r.report.Module.GoSumHash != "" {
		t.Errorf("report go.sum hash = %q, want empty", r.report.Module.GoSumHash)
	}
	if r.report.Module.BaselineGoModHash != "baseline" {
		t.Errorf("baseline hash = %q, want preserved", r.report.Module.BaselineGoModHash)
	}
}

func TestReconcileRefreshedRequirementsReportsCopyRemoval(t *testing.T) {
	previous := &modgen.Report{
		Kept: []gomodmap.Requirement{
			{Path: "example.com/added", Version: "v1.0.0", Indirect: true},
			{Path: "example.com/copied", Version: "v1.0.0"},
			{Path: "example.com/direct", Version: "v1.0.0"},
			{Path: "example.com/reclassified", Version: "v1.0.0"},
		},
		Added:        []gomodmap.Requirement{{Path: "example.com/added", Version: "v1.0.0", Indirect: true}},
		Dropped:      []string{"example.com/already-dropped"},
		Reclassified: []modgen.Reclassification{{Path: "example.com/reclassified", Indirect: false}},
	}
	kept := []gomodmap.Requirement{
		{Path: "example.com/added", Version: "v1.0.0", Indirect: true},
		{Path: "example.com/direct", Version: "v1.0.0", Indirect: true},
		{Path: "example.com/reclassified", Version: "v1.0.0", Indirect: true},
	}

	got, err := reconcileRefreshedRequirements(previous, kept)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !slices.Equal(got.Kept, kept) {
		t.Errorf("kept = %#v, want %#v", got.Kept, kept)
	}
	wantDropped := []string{"example.com/already-dropped", "example.com/copied"}
	if !slices.Equal(got.Dropped, wantDropped) {
		t.Errorf("dropped = %v, want %v", got.Dropped, wantDropped)
	}
	if !slices.Equal(got.Added, kept[:1]) {
		t.Errorf("added = %#v, want %#v", got.Added, kept[:1])
	}
	wantReclassified := []modgen.Reclassification{{Path: "example.com/direct", Indirect: true}}
	if !slices.Equal(got.Reclassified, wantReclassified) {
		t.Errorf("reclassified = %#v, want %#v", got.Reclassified, wantReclassified)
	}
}

func TestReconcileRefreshedRequirementsRefusesDrift(t *testing.T) {
	previous := &modgen.Report{Kept: []gomodmap.Requirement{{Path: "example.com/pinned", Version: "v1.0.0"}}}
	tests := []struct {
		name    string
		kept    []gomodmap.Requirement
		wantErr error
	}{
		{
			name:    "added requirement",
			kept:    []gomodmap.Requirement{{Path: "example.com/new", Version: "v1.0.0"}, {Path: "example.com/pinned", Version: "v1.0.0"}},
			wantErr: modgen.ErrModuleDrift,
		},
		{
			name:    "changed pin",
			kept:    []gomodmap.Requirement{{Path: "example.com/pinned", Version: "v1.1.0"}},
			wantErr: modgen.ErrPinFloated,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := reconcileRefreshedRequirements(previous, test.kept)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("reconcile error = %v, want %v", err, test.wantErr)
			}
		})
	}
}

func TestRefreshModuleReportGoSumReadErrorIsNotSwallowed(t *testing.T) {
	dir := t.TempDir()
	goMod := []byte("module example.com/m\n\ngo 1.26.0\n")
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), goMod, 0o644); err != nil {
		t.Fatal(err)
	}
	// Create go.sum as a directory so reading it fails with a non-ErrNotExist
	// error (EISDIR on Linux, "is a directory" on all platforms).
	if err := os.Mkdir(filepath.Join(dir, "go.sum"), 0o750); err != nil {
		t.Fatal(err)
	}

	r := &run{
		paths:        Paths{PostModule: dir},
		moduleReport: &modgen.Report{},
	}
	err := r.refreshModuleReport(context.Background())
	if err == nil {
		t.Fatal("expected an error when go.sum is a directory, got nil")
	}
	if !strings.Contains(err.Error(), "go.sum") {
		t.Errorf("error = %v, want it to mention go.sum", err)
	}
}

func TestValidateBaseNames(t *testing.T) {
	tests := []struct {
		name    string
		names   []string
		wantErr string
	}{
		{name: "valid single", names: []string{"a.go"}},
		{name: "valid multiple", names: []string{"a.go", "b.go", "c.s"}},
		{name: "empty name", names: []string{"a.go", ""}, wantErr: "empty name"},
		{name: "slash", names: []string{"sub/a.go"}, wantErr: "path separator"},
		{name: "dot traversal", names: []string{".."}, wantErr: "traversal"},
		{name: "dot", names: []string{"."}, wantErr: "traversal"},
		{name: "hidden file", names: []string{".hidden"}, wantErr: "starts with a dot"},
		{name: "dash prefix", names: []string{"-flag"}, wantErr: "starts with a dash"},
		{name: "duplicate", names: []string{"a.go", "b.go", "a.go"}, wantErr: "duplicate"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateBaseNames(tt.names)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("want error containing %q, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
