package setup

import (
	"strings"
	"testing"

	"golang.org/x/mod/module"
)

// TestParseEnginePin covers the one input that decides what code a derived
// repository will run.
//
// The refusals matter more than the acceptances. A version that resolves at
// build time rather than naming a release turns a repository whose whole purpose
// is byte-identical output into one whose engine can change under it, and every
// rejected spelling below is a way that could happen.
func TestParseEnginePin(t *testing.T) {
	tests := []struct {
		name        string
		value       string
		wantVersion string
		wantTag     string
		wantErr     string
	}{
		{name: "module version", value: "v1.4.2", wantVersion: "v1.4.2", wantTag: "tools/v1.4.2"},
		{name: "repository tag", value: "tools/v1.4.2", wantVersion: "v1.4.2", wantTag: "tools/v1.4.2"},
		{name: "surrounding space", value: "  tools/v0.1.0\n", wantVersion: "v0.1.0", wantTag: "tools/v0.1.0"},
		{name: "prerelease", value: "v1.0.0-rc.1", wantVersion: "v1.0.0-rc.1", wantTag: "tools/v1.0.0-rc.1"},

		{name: "empty", value: "", wantErr: "an engine version is required"},
		{name: "blank", value: "   ", wantErr: "an engine version is required"},
		{name: "a query", value: "latest", wantErr: "must name a released tag"},
		{name: "a branch", value: "main", wantErr: "must name a released tag"},
		{name: "a tagged query", value: "tools/latest", wantErr: "must name a released tag"},
		{name: "no v prefix", value: "1.4.2", wantErr: "must name a released tag"},
		{name: "not a version", value: "vNext", wantErr: "is not a semantic version"},
		{name: "major only", value: "v1", wantErr: "is not canonical"},
		{name: "minor only", value: "v1.4", wantErr: "is not canonical"},
		{name: "build metadata", value: "v1.4.2+dirty", wantErr: "is not canonical"},
		{
			name:    "pseudo-version",
			value:   "v0.0.0-20260101120000-abcdef123456",
			wantErr: "is a pseudo-version",
		},
		{
			// The engine module path carries no /v2 suffix, so no v2 release of it
			// can exist. Accepting one would write a requirement the go command
			// cannot resolve.
			name:    "a major the module path cannot carry",
			value:   "v2.0.0",
			wantErr: "invalid version",
		},
		{name: "doubled tag prefix", value: "tools/tools/v1.0.0", wantErr: "must name a released tag"},
		{name: "incompatible", value: "v2.0.0+incompatible", wantErr: "is not canonical"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pin, err := parseEnginePin(test.value)
			if test.wantErr != "" {
				if err == nil {
					t.Fatalf("parseEnginePin(%q) = %+v, want an error", test.value, pin)
				}
				if !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("parseEnginePin(%q) error = %v, want %q", test.value, err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseEnginePin(%q): %v", test.value, err)
			}
			if pin.version != test.wantVersion || pin.tag != test.wantTag {
				t.Errorf("parseEnginePin(%q) = %s / %s, want %s / %s", test.value, pin.version, pin.tag, test.wantVersion, test.wantTag)
			}
		})
	}
}

// TestToolsModulePath pins the nested module's identity.
func TestToolsModulePath(t *testing.T) {
	tests := []struct {
		root string
		want string
	}{
		{root: "monis.app/kk/rbac_authorizer", want: "monis.app/kk/rbac_authorizer/tools"},
		{root: "example.com/x", want: "example.com/x/tools"},
	}
	for _, test := range tests {
		if got := toolsModulePath(test.root); got != test.want {
			t.Errorf("toolsModulePath(%q) = %q, want %q", test.root, got, test.want)
		}
	}
}

func TestRenderGoModMatchesTidyRequirementGroups(t *testing.T) {
	t.Parallel()

	got, err := renderGoMod(
		"example.com/derived/tools",
		[]module.Version{{Path: EngineModulePath, Version: "v0.2.0"}},
		[]module.Version{
			{Path: "golang.org/x/mod", Version: "v0.39.0"},
			{Path: "gopkg.in/yaml.v3", Version: "v3.0.1"},
		},
	)
	if err != nil {
		t.Fatalf("render go.mod: %v", err)
	}
	want := `module example.com/derived/tools

go 1.26.0

toolchain go1.26.5

require github.com/enj/soapbox/tools v0.2.0

require (
	golang.org/x/mod v0.39.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)
`
	if string(got) != want {
		t.Errorf("go.mod =\n%s\nwant:\n%s", got, want)
	}
}

// TestPayloadAndTemplateSetsAreDisjoint is the invariant behind the allowlist.
//
// A path that was both retained and template owned would be decided by the order
// the classifier happens to check its rules in, which is exactly the kind of
// accident that turns a preserved file into a deleted one after an unrelated
// refactor.
func TestPayloadAndTemplateSetsAreDisjoint(t *testing.T) {
	for _, retained := range retainedFiles {
		if templateOwned(retained) {
			t.Errorf("%s is both retained and template owned", retained)
		}
	}
	for _, dir := range retainedDirs {
		if templateOwned(dir + "anything.txt") {
			t.Errorf("%s is both retained and template owned", dir)
		}
	}
	// A composed path may be template owned, which is what makes it a replace
	// rather than a create. It may never be retained, because a retained path is
	// one setup promises not to write.
	for _, composed := range []string{rootGoModPath, toolsGoModPath, toolsGoSumPath, toolsMainPath, ciWorkflowPath, syncWorkflowPath} {
		for _, retained := range retainedFiles {
			if composed == retained {
				t.Errorf("%s is both composed and retained", composed)
			}
		}
	}
	// Every marker survives long enough to be checked, and every one of them is
	// either composed over or removed, so no trace of the template's identity is
	// left in the derived repository.
	for _, marker := range templateMarkers {
		composed := marker == toolsMainPath
		if !composed && !templateOwned(marker) && marker != "soapbox.yaml" {
			t.Errorf("marker %s is neither composed nor removed, so a derived repository still looks like a template", marker)
		}
	}
}
