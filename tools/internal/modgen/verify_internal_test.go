package modgen

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"golang.org/x/mod/modfile"

	"github.com/enj/soapbox/tools/internal/gomodmap"
)

// parseModule parses go.mod text for a comparison test.
func parseModule(t *testing.T, text string) *modfile.File {
	t.Helper()
	file, err := modfile.Parse("go.mod", []byte(text), nil)
	if err != nil {
		t.Fatalf("parse go.mod: %v", err)
	}
	return file
}

// generatedModule is what this package writes before tidying.
const generatedModule = `module monis.app/kk/rbac_authorizer

go 1.26.0

toolchain go1.26.5

godebug default=go1.26

require (
	github.com/spf13/cobra v1.10.1
	k8s.io/api v0.36.1
	k8s.io/klog/v2 v2.130.1 // indirect
)
`

// TestCompare_Unchanged proves a module the go command agreed with reports every
// requirement as kept.
func TestCompare_Unchanged(t *testing.T) {
	t.Parallel()

	intended := parseModule(t, generatedModule)
	report, err := compare(intended, parseModule(t, generatedModule), false)
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	want := []string{"github.com/spf13/cobra", "k8s.io/api", "k8s.io/klog/v2"}
	got := make([]string, len(report.Kept))
	for i, requirement := range report.Kept {
		got[i] = requirement.Path
	}
	if !slices.Equal(got, want) {
		t.Errorf("kept = %v, want %v", got, want)
	}
	if len(report.Dropped) != 0 {
		t.Errorf("dropped = %v, want none", report.Dropped)
	}
	// A module the go command did not change reclassifies nothing, so the
	// indirect marking the source carried is also the one the report records.
	if len(report.Reclassified) != 0 {
		t.Errorf("reclassified = %v, want none", report.Reclassified)
	}
	for _, requirement := range report.Kept {
		if requirement.Path == "k8s.io/klog/v2" && !requirement.Indirect {
			t.Error("k8s.io/klog/v2 lost its indirect marking")
		}
	}
}

// TestCompare_Dropped proves requirements tidy removed are reported rather than
// treated as a failure. Extracting a few packages out of Kubernetes drops most
// of them, which is the normal outcome.
func TestCompare_Dropped(t *testing.T) {
	t.Parallel()

	tidied := `module monis.app/kk/rbac_authorizer

go 1.26.0

toolchain go1.26.5

godebug default=go1.26

require k8s.io/api v0.36.1
`
	report, err := compare(parseModule(t, generatedModule), parseModule(t, tidied), false)
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	want := []string{"github.com/spf13/cobra", "k8s.io/klog/v2"}
	if !slices.Equal(report.Dropped, want) {
		t.Errorf("dropped = %v, want %v", report.Dropped, want)
	}
	if len(report.Kept) != 1 || report.Kept[0].Path != "k8s.io/api" {
		t.Errorf("kept = %v, want only k8s.io/api", report.Kept)
	}
}

func TestCompare_AllowsAndReportsCompatibilityAdditions(t *testing.T) {
	t.Parallel()
	tidied := generatedModule + "\nrequire github.com/kr/text v0.2.0 // indirect\n"
	report, err := compare(parseModule(t, generatedModule), parseModule(t, tidied), true)
	if err != nil {
		t.Fatalf("compare with allowed additions: %v", err)
	}
	want := []gomodmap.Requirement{{Path: "github.com/kr/text", Version: "v0.2.0", Indirect: true}}
	if !slices.Equal(report.Added, want) {
		t.Errorf("added = %#v, want %#v", report.Added, want)
	}
	if !slices.Contains(report.Kept, want[0]) {
		t.Errorf("kept = %#v, want added requirement included", report.Kept)
	}
	if _, err := compare(parseModule(t, generatedModule), parseModule(t, tidied), false); !errors.Is(err, ErrModuleDrift) {
		t.Errorf("ordinary compare = %v, want ErrModuleDrift", err)
	}
}

// TestCompare_Reclassified proves a requirement tidying kept at the pinned
// version but marked differently is reported rather than refused or absorbed.
//
// The extracted module is a subset of the source, so both directions happen: a
// module the source imports from a package that was not extracted becomes
// indirect, and one the source only reached through a dependency becomes direct
// when an extracted package imports it. Neither changes which code is built, and
// the marking that ends up in the generated module is the go command's.
func TestCompare_Reclassified(t *testing.T) {
	t.Parallel()

	tidied := `module monis.app/kk/rbac_authorizer

go 1.26.0

toolchain go1.26.5

godebug default=go1.26

require (
	github.com/spf13/cobra v1.10.1 // indirect
	k8s.io/api v0.36.1
	k8s.io/klog/v2 v2.130.1
)
`
	report, err := compare(parseModule(t, generatedModule), parseModule(t, tidied), false)
	if err != nil {
		t.Fatalf("compare: %v", err)
	}

	want := []Reclassification{
		{Path: "github.com/spf13/cobra", Indirect: true},
		{Path: "k8s.io/klog/v2", Indirect: false},
	}
	if !slices.Equal(report.Reclassified, want) {
		t.Errorf("reclassified = %v, want %v", report.Reclassified, want)
	}
	if len(report.Kept) != 3 {
		t.Fatalf("kept = %v, want all three requirements", report.Kept)
	}
	// The recorded marking is the one the generated module now carries, not the
	// one the source module had.
	for _, requirement := range report.Kept {
		want := requirement.Path == "github.com/spf13/cobra"
		if requirement.Indirect != want {
			t.Errorf("kept %s indirect = %v, want %v", requirement.Path, requirement.Indirect, want)
		}
	}
	if len(report.Dropped) != 0 {
		t.Errorf("dropped = %v, want none", report.Dropped)
	}
}

// TestCompare_Floated proves a pin minimal version selection raised is refused.
// The raised version is what a consumer would actually build against, and the
// operator approved the pin rather than its successor.
func TestCompare_Floated(t *testing.T) {
	t.Parallel()

	tidied := strings.Replace(generatedModule, "k8s.io/api v0.36.1", "k8s.io/api v0.36.4", 1)
	_, err := compare(parseModule(t, generatedModule), parseModule(t, tidied), false)
	if !errors.Is(err, ErrPinFloated) {
		t.Fatalf("compare: error = %v, want ErrPinFloated", err)
	}
	// The message has to name what moved and where it went, or an operator
	// cannot act on it.
	for _, want := range []string{"k8s.io/api", "v0.36.1", "v0.36.4"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("compare: error = %v, want it to contain %q", err, want)
		}
	}
}

func TestCompare_Drift(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		tidied  string
		wantErr string
	}{
		{
			// A module the engine never wrote cannot have come from the source
			// commit, because the source lists every module its build resolves.
			name:    "requirement appeared",
			tidied:  generatedModule + "\nrequire github.com/pkg/errors v0.9.1\n",
			wantErr: "the go command added",
		},
		{
			// A replace directive would make the published module resolve to code
			// from somewhere its module path does not name.
			name:    "replace appeared",
			tidied:  generatedModule + "\nreplace k8s.io/api => ../api\n",
			wantErr: "must carry no replace directives",
		},
		{
			name:    "exclude appeared",
			tidied:  generatedModule + "\nexclude k8s.io/api v0.36.0\n",
			wantErr: "must carry no exclude directives",
		},
		{
			name:    "module path changed",
			tidied:  strings.Replace(generatedModule, "monis.app/kk/rbac_authorizer", "monis.app/kk/other", 1),
			wantErr: "module path became",
		},
		{
			name:    "go directive changed",
			tidied:  strings.Replace(generatedModule, "go 1.26.0", "go 1.27.0", 1),
			wantErr: "go directive became",
		},
		{
			name:    "toolchain changed",
			tidied:  strings.Replace(generatedModule, "toolchain go1.26.5", "toolchain go1.27.0", 1),
			wantErr: "toolchain directive became",
		},
		{
			// A different godebug default compiles the extracted code under
			// different semantics than upstream compiled it under.
			name:    "godebug changed",
			tidied:  strings.Replace(generatedModule, "godebug default=go1.26", "godebug default=go1.25", 1),
			wantErr: "godebug directives became",
		},
		{
			name:    "godebug removed",
			tidied:  strings.Replace(generatedModule, "godebug default=go1.26\n", "", 1),
			wantErr: "godebug directives became",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := compare(parseModule(t, generatedModule), parseModule(t, test.tidied), false)
			if !errors.Is(err, ErrModuleDrift) {
				t.Fatalf("compare: error = %v, want ErrModuleDrift", err)
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Errorf("compare: error = %v, want it to contain %q", err, test.wantErr)
			}
		})
	}
}

// TestCompare_FloatAndDropTogether proves a floated pin is reported even when
// other requirements were legitimately dropped in the same pass.
func TestCompare_FloatAndDropTogether(t *testing.T) {
	t.Parallel()

	tidied := `module monis.app/kk/rbac_authorizer

go 1.26.0

toolchain go1.26.5

godebug default=go1.26

require k8s.io/api v0.36.9
`
	_, err := compare(parseModule(t, generatedModule), parseModule(t, tidied), false)
	if !errors.Is(err, ErrPinFloated) {
		t.Errorf("compare: error = %v, want ErrPinFloated", err)
	}
}
