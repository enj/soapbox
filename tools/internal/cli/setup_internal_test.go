package cli

import (
	"slices"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/setup"
)

// TestGeneratedWorkflowCommandsNameRealFlags ties the workflows setup writes to
// the commands they invoke.
//
// The setup package composes those command lines and this package defines the
// flags they name, so nothing else in the build connects the two. Without this
// test, renaming or removing a flag would leave a generated workflow that parses
// as YAML, passes review, and fails on the first scheduled run in a repository
// nobody is watching.
func TestGeneratedWorkflowCommandsNameRealFlags(t *testing.T) {
	syncFlags, _ := syncFlagSet()
	validateFlags, _ := validateFlagSet()

	tests := []struct {
		name    string
		command string
		verb    string
		flags   func(string) bool
	}{
		{
			name:    "sync",
			command: setup.SyncCommand,
			verb:    "sync",
			flags:   func(flag string) bool { return syncFlags.Lookup(flag) != nil },
		},
		{
			name:    "validate",
			command: setup.VerifyCommand,
			verb:    "validate",
			flags:   func(flag string) bool { return validateFlags.Lookup(flag) != nil },
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fields := strings.Fields(test.command)
			// Find the verb in the command. For go-run commands the verb is
			// after the module path; for pre-built binaries the verb follows
			// the binary path (which may contain ${{ }} expressions that
			// split into multiple fields).
			verbIndex := -1
			for i, f := range fields {
				if f == test.verb {
					verbIndex = i
					break
				}
			}
			if verbIndex < 0 {
				t.Fatalf("command %q does not contain verb %q", test.command, test.verb)
			}
			named := 0
			for _, field := range fields[verbIndex+1:] {
				if !strings.HasPrefix(field, "-") {
					continue
				}
				named++
				flag := strings.TrimPrefix(field, "-")
				if name, _, ok := strings.Cut(flag, "="); ok {
					flag = name
				}
				if !test.flags(flag) {
					t.Errorf("command %q names -%s, which soapbox %s does not define", test.command, flag, test.verb)
				}
			}
			if named == 0 {
				t.Errorf("command %q names no flag, so this test proves nothing", test.command)
			}
		})
	}
}

// TestSyncBaseCommandCarriesNoApproval asserts the base sync invocation embeds
// no explicit approval flag. Automatic mode adds -unattended at generation
// time; the base command itself must not carry a stale or hardcoded approval.
func TestSyncBaseCommandCarriesNoApproval(t *testing.T) {
	fields := strings.Fields(setup.SyncCommand)
	for _, forbidden := range []string{"-apply", "-approve"} {
		if slices.Contains(fields, forbidden) {
			t.Errorf("the base sync command names %s", forbidden)
		}
	}
}
