package generate

import (
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/extract"
	"github.com/enj/soapbox/tools/internal/relocate"
)

func TestImportBelongsToModule(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		importPath string
		modulePath string
		want       bool
	}{
		{
			name:       "exact match",
			importPath: "k8s.io/component-helpers",
			modulePath: "k8s.io/component-helpers",
			want:       true,
		},
		{
			name:       "subpackage",
			importPath: "k8s.io/component-helpers/auth/rbac/validation",
			modulePath: "k8s.io/component-helpers",
			want:       true,
		},
		{
			name:       "different module with shared prefix",
			importPath: "k8s.io/component-helpersX/auth",
			modulePath: "k8s.io/component-helpers",
			want:       false,
		},
		{
			name:       "prefix without slash boundary",
			importPath: "k8s.io/component-helpers-extra/foo",
			modulePath: "k8s.io/component-helpers",
			want:       false,
		},
		{
			name:       "completely different",
			importPath: "k8s.io/api/rbac/v1",
			modulePath: "k8s.io/component-helpers",
			want:       false,
		},
		{
			name:       "module is prefix of different domain",
			importPath: "k8s.io/apimachinery/pkg/runtime",
			modulePath: "k8s.io/api",
			want:       false,
		},
		{
			name:       "module subpackage match",
			importPath: "k8s.io/api/rbac/v1",
			modulePath: "k8s.io/api",
			want:       true,
		},
		{
			name:       "empty import path",
			importPath: "",
			modulePath: "k8s.io/component-helpers",
			want:       false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := importBelongsToModule(tt.importPath, tt.modulePath)
			if got != tt.want {
				t.Errorf("importBelongsToModule(%q, %q) = %v, want %v",
					tt.importPath, tt.modulePath, got, tt.want)
			}
		})
	}
}

func TestCheckForbiddenGoSum(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		goSum     string
		forbidden map[string]bool
		wantErr   string // substring; empty means no error
	}{
		{
			name:      "empty go.sum",
			goSum:     "",
			forbidden: map[string]bool{"k8s.io/component-helpers": true},
		},
		{
			name:      "no forbidden module present",
			goSum:     "k8s.io/api v0.36.1 h1:abc=\nk8s.io/api v0.36.1/go.mod h1:def=\n",
			forbidden: map[string]bool{"k8s.io/component-helpers": true},
		},
		{
			name:      "forbidden module in go.sum",
			goSum:     "k8s.io/api v0.36.1 h1:abc=\nk8s.io/component-helpers v0.36.1 h1:xyz=\n",
			forbidden: map[string]bool{"k8s.io/component-helpers": true},
			wantErr:   "go.sum contains an entry for forbidden module k8s.io/component-helpers",
		},
		{
			name:      "similar prefix not matched",
			goSum:     "k8s.io/component-helpersX v0.36.1 h1:abc=\n",
			forbidden: map[string]bool{"k8s.io/component-helpers": true},
		},
		{
			name:      "go.mod hash line also caught",
			goSum:     "k8s.io/component-helpers v0.36.1/go.mod h1:xyz=\n",
			forbidden: map[string]bool{"k8s.io/component-helpers": true},
			wantErr:   "go.sum contains an entry for forbidden module k8s.io/component-helpers",
		},
		{
			name:      "malformed line refused",
			goSum:     "k8s.io/component-helpers\n",
			forbidden: map[string]bool{"k8s.io/component-helpers": true},
			wantErr:   "has 1 fields, want 3",
		},
		{
			name:      "oversized line refused",
			goSum:     strings.Repeat("x", 70<<10) + "\nk8s.io/component-helpers v0.36.1 h1:xyz=\n",
			forbidden: map[string]bool{"k8s.io/component-helpers": true},
			wantErr:   "scan go.sum",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := checkForbiddenGoSum([]byte(tt.goSum), tt.forbidden)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestCheckForbiddenGoSumExactMatch(t *testing.T) {
	t.Parallel()

	// go.sum line with k8s.io/component-helpers-extra should NOT match
	// k8s.io/component-helpers. This proves the check is exact-match, not prefix.
	goSum := []byte("k8s.io/component-helpers-extra v0.36.1 h1:abc=\n")
	forbidden := map[string]bool{"k8s.io/component-helpers": true}
	if err := checkForbiddenGoSum(goSum, forbidden); err != nil {
		t.Fatalf("exact-match violation: prefix match should not trigger: %v", err)
	}
}

func TestCheckForbiddenImportsDetectsSubpackage(t *testing.T) {
	t.Parallel()

	files := []relocate.File{
		{
			Path: "internal/kk/pkg/registry/rbac/validation/rule.go",
			Contents: []byte(`package validation

import "k8s.io/component-helpers/auth/rbac/validation"

func F() { _ = validation.RuleAllows }
`),
		},
	}

	forbidden := map[string]bool{"k8s.io/component-helpers": true}
	r := &run{
		post:      &extract.Result{Files: relocate.FileSet{Files: files}},
		copyFiles: relocate.FileSet{},
	}
	err := r.checkForbiddenImports(forbidden)
	if err == nil {
		t.Fatal("expected error for import of forbidden module subpackage")
	}
	want := "belongs to forbidden module k8s.io/component-helpers"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to contain %q", err.Error(), want)
	}
}

func TestCheckForbiddenImportsExactModuleBoundary(t *testing.T) {
	t.Parallel()

	// An import of k8s.io/component-helpersX should NOT be caught by
	// a forbidden rule on k8s.io/component-helpers.
	files := []relocate.File{
		{
			Path: "internal/kk/pkg/foo/bar.go",
			Contents: []byte(`package foo

import "k8s.io/component-helpersX/auth/rbac/validation"

func F() { _ = validation.RuleAllows }
`),
		},
	}

	forbidden := map[string]bool{"k8s.io/component-helpers": true}
	r := &run{
		post:      &extract.Result{Files: relocate.FileSet{Files: files}},
		copyFiles: relocate.FileSet{},
	}
	if err := r.checkForbiddenImports(forbidden); err != nil {
		t.Fatalf("exact boundary violation: %v", err)
	}
}

func TestCheckForbiddenImportsSkipsNonGoFiles(t *testing.T) {
	t.Parallel()

	files := []relocate.File{
		{
			Path:     "internal/kk/pkg/registry/rbac/validation/README.md",
			Contents: []byte("# This mentions k8s.io/component-helpers but is not Go\n"),
		},
	}

	forbidden := map[string]bool{"k8s.io/component-helpers": true}
	r := &run{
		post:      &extract.Result{Files: relocate.FileSet{Files: files}},
		copyFiles: relocate.FileSet{},
	}
	if err := r.checkForbiddenImports(forbidden); err != nil {
		t.Fatalf("non-Go file should not be scanned: %v", err)
	}
}

func TestCheckForbiddenImportsRefusesUnparseableGoFile(t *testing.T) {
	t.Parallel()

	files := []relocate.File{{
		Path:     "internal/kk/pkg/registry/rbac/validation/broken.go",
		Contents: []byte("package validation\n\nimport \"k8s.io/component-helpers/auth/rbac/validation\n"),
	}}
	r := &run{
		post:      &extract.Result{Files: relocate.FileSet{Files: files}},
		copyFiles: relocate.FileSet{},
	}
	err := r.checkForbiddenImports(map[string]bool{"k8s.io/component-helpers": true})
	if err == nil {
		t.Fatal("expected unparseable Go file to fail the forbidden import scan")
	}
	for _, want := range []string{"parse imports", "broken.go"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err.Error(), want)
		}
	}
}

func TestCheckForbiddenImportsCopyFiles(t *testing.T) {
	t.Parallel()

	// Forbidden import in copy files (not just retained files) must be caught.
	copyFiles := []relocate.File{
		{
			Path: "internal/kk/staging/copy.go",
			Contents: []byte(`package copy

import "k8s.io/component-helpers/other"

func G() { _ = other.X }
`),
		},
	}

	forbidden := map[string]bool{"k8s.io/component-helpers": true}
	r := &run{
		post:      &extract.Result{},
		copyFiles: relocate.FileSet{Files: copyFiles},
	}
	err := r.checkForbiddenImports(forbidden)
	if err == nil {
		t.Fatal("expected error for forbidden import in copy files")
	}
}
