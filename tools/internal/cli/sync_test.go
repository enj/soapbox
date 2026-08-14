package cli_test

import (
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/cli"
)

// TestSyncDecidesUsageBeforeReadingTheProfile is the property the whole command
// line surface is arranged around.
//
// Every case here runs in a directory holding no profile at all. A command line
// that cannot work has to fail the same way whether or not a profile, a cache,
// or a network happens to be there, so a missing profile must never be what the
// operator is told about first when what they typed was already contradictory.
func TestSyncDecidesUsageBeforeReadingTheProfile(t *testing.T) {
	// The cache and destination are named so that the flags being tested are the
	// only thing missing. Neither directory has to exist: nothing is read.
	const cache = "-cache=/tmp/soapbox-cache-that-is-never-read"
	const destination = "-destination=/tmp/soapbox-destination-that-is-never-read"

	tests := []struct {
		name       string
		args       []string
		wantStderr string
	}{{
		name:       "no destination repository",
		args:       []string{"sync", cache},
		wantStderr: "-destination is required",
	}, {
		name:       "no source cache",
		args:       []string{"sync", destination},
		wantStderr: "-cache is required",
	}, {
		name:       "publishing without an approval",
		args:       []string{"sync", cache, destination, "-apply"},
		wantStderr: "must be given with -approve",
	}, {
		name:       "an approval without publishing",
		args:       []string{"sync", cache, destination, "-approve=sha256:whatever"},
		wantStderr: "-apply must also be given",
	}, {
		name:       "a branch instead of a release",
		args:       []string{"sync", cache, destination, "-branch=master"},
		wantStderr: "a synchronization publishes a release",
	}, {
		name:       "two refs",
		args:       []string{"sync", cache, destination, "-tag=v1.36.1", "-branch=master"},
		wantStderr: "only one may be given",
	}, {
		name:       "an unsupported output format",
		args:       []string{"sync", cache, destination, "-format=yaml"},
		wantStderr: `unsupported -format "yaml"`,
	}, {
		name:       "offline contradicted by a fetch",
		args:       []string{"sync", cache, destination, "-offline", "-fetch"},
		wantStderr: "-fetch cannot also be requested",
	}, {
		name:       "offline contradicted by a proxy",
		args:       []string{"sync", cache, destination, "-offline", "-proxy=https://proxy.golang.org"},
		wantStderr: "cannot also be requested",
	}, {
		name:       "unattended contradicts apply",
		args:       []string{"sync", cache, destination, "-unattended", "-apply"},
		wantStderr: "-unattended self-approves, so -apply cannot also be given",
	}, {
		name:       "unattended contradicts approve",
		args:       []string{"sync", cache, destination, "-unattended", "-approve=sha256:whatever"},
		wantStderr: "-unattended self-approves, so -approve cannot also be given",
	}, {
		name:       "unattended contradicts local-remote",
		args:       []string{"sync", cache, destination, "-unattended", "-local-remote"},
		wantStderr: "-unattended publishes over HTTPS, so -local-remote cannot also be given",
	}, {
		name:       "unattended contradicts remote override",
		args:       []string{"sync", cache, destination, "-unattended", "-remote=https://example.com/x.git"},
		wantStderr: "-unattended derives the remote from the profile, so -remote cannot also be given",
	}, {
		name:       "unattended contradicts identity override",
		args:       []string{"sync", cache, destination, "-unattended", "-identity=github.com/other/repo"},
		wantStderr: "-unattended derives the identity from the profile, so -identity cannot also be given",
	}, {
		name:       "unattended contradicts state-commit override",
		args:       []string{"sync", cache, destination, "-unattended", "-state-commit=abc123"},
		wantStderr: "-unattended discovers state from the destination, so -state-commit cannot also be given",
	}, {
		name:       "unattended contradicts tag override",
		args:       []string{"sync", cache, destination, "-unattended", "-tag=v1.36.1"},
		wantStderr: "-unattended discovers releases from the source, so -tag cannot also be given",
	}, {
		name:       "unattended contradicts source-remote override",
		args:       []string{"sync", cache, destination, "-unattended", "-source-remote=https://example.com/k.git"},
		wantStderr: "-unattended derives the source from the profile, so -source-remote cannot also be given",
	}, {
		name:       "write verification contradicts unattended",
		args:       []string{"sync", cache, destination, "-verify-write", "-unattended"},
		wantStderr: "-verify-write is a no-op probe",
	}, {
		name:       "write verification contradicts override",
		args:       []string{"sync", cache, destination, "-verify-write", "-tag=v1.36.1"},
		wantStderr: "-verify-write derives every input",
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdout, stderr, code := run(t.Context(), t, t.TempDir(), tt.args...)
			if code != cli.ExitUsage {
				t.Fatalf("exit code = %d, want %d\nstdout: %s\nstderr: %s",
					code, cli.ExitUsage, stdout, stderr)
			}
			if !strings.Contains(stderr, tt.wantStderr) {
				t.Fatalf("stderr %q does not contain %q", stderr, tt.wantStderr)
			}
			// A usage failure prints the failing command's own flags, so an
			// operator sees what sync takes rather than the top level command list.
			if !strings.Contains(stderr, "-approve string") {
				t.Errorf("stderr %q does not print the sync usage block", stderr)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want nothing: a refused command line produces no artifact", stdout)
			}
		})
	}
}

// TestSyncIsDispatchableAndDocumented checks that the command is reachable and
// that its help names the flags a publication turns on.
func TestSyncIsDispatchableAndDocumented(t *testing.T) {
	stdout, stderr, code := run(t.Context(), t, t.TempDir(), "help", "sync")
	if code != cli.ExitOK {
		t.Fatalf("exit code = %d, want %d\nstderr: %s", code, cli.ExitOK, stderr)
	}
	for _, flag := range []string{
		"-destination string", "-remote string", "-identity string",
		"-local-remote", "-state-commit string", "-apply", "-approve string",
		"-unattended", "-verify-write",
	} {
		if !strings.Contains(stdout, flag) {
			t.Errorf("sync help does not document %q", flag)
		}
	}

	listing, _, code := run(t.Context(), t, t.TempDir(), "help")
	if code != cli.ExitOK {
		t.Fatalf("help exit code = %d", code)
	}
	if !strings.Contains(listing, "sync") {
		t.Errorf("the command list does not name sync:\n%s", listing)
	}
}
