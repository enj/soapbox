package generate_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/extract"
	"github.com/enj/soapbox/tools/internal/generate"
	"github.com/enj/soapbox/tools/internal/gitcli"
)

// TestGenerateRefusesUnusableOptions covers everything a generation can decide
// before it starts a subprocess.
//
// Each row is a way the run could otherwise do damage or produce an answer
// nobody could trust: a relative directory makes the result depend on where the
// process was started, a nested directory makes the scratch cleanup delete
// something the operator owns, a credentialed runner puts a publishing secret on
// a path to the source host, and a branch ref asks for a resolution this engine
// cannot perform honestly.
func TestGenerateRefusesUnusableOptions(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(t *testing.T, opts *generate.Options)
		wantErr string
	}{
		{
			name:    "no profile",
			mutate:  func(_ *testing.T, opts *generate.Options) { opts.Config = nil },
			wantErr: "a profile is required",
		},
		{
			name:    "no git runner",
			mutate:  func(_ *testing.T, opts *generate.Options) { opts.Git = nil },
			wantErr: "a Git runner is required",
		},
		{
			name:    "no go runner",
			mutate:  func(_ *testing.T, opts *generate.Options) { opts.Go = nil },
			wantErr: "a Go runner is required",
		},
		{
			name: "credentialed git runner",
			mutate: func(t *testing.T, opts *generate.Options) {
				git, err := gitcli.New(t.Context(), gitcli.Options{
					Inherit: []string{"PATH"},
					Env:     []string{"GIT_TOKEN=secret"},
				})
				if err != nil {
					t.Fatalf("git runner: %v", err)
				}
				opts.Git = git
			},
			wantErr: "the Git runner carries caller supplied environment entries",
		},
		{
			name:    "relative cache root",
			mutate:  func(_ *testing.T, opts *generate.Options) { opts.CacheRoot = "cache" },
			wantErr: `the source cache root "cache" must be absolute`,
		},
		{
			name:    "unclean work root",
			mutate:  func(_ *testing.T, opts *generate.Options) { opts.WorkRoot += "/../work" },
			wantErr: "must be a clean path",
		},
		{
			name:    "missing version index",
			mutate:  func(_ *testing.T, opts *generate.Options) { opts.StorePath = "" },
			wantErr: "the version index is required",
		},
		{
			name: "work root inside the cache",
			mutate: func(_ *testing.T, opts *generate.Options) {
				opts.WorkRoot = filepath.Join(opts.CacheRoot, "work")
			},
			wantErr: "contains the work root",
		},
		{
			name: "output tree is the cache",
			mutate: func(_ *testing.T, opts *generate.Options) {
				opts.OutputRoot = opts.CacheRoot
			},
			wantErr: "are both",
		},
		{
			name: "output tree already exists",
			mutate: func(t *testing.T, opts *generate.Options) {
				if err := os.MkdirAll(opts.OutputRoot, 0o750); err != nil {
					t.Fatalf("create output tree: %v", err)
				}
			},
			wantErr: "output tree",
		},
		{
			name: "output tree contains the profile",
			mutate: func(_ *testing.T, opts *generate.Options) {
				opts.OutputRoot = filepath.Dir(opts.ProfileDir)
			},
			wantErr: "contains the profile directory",
		},
		{
			name: "branch ref",
			mutate: func(_ *testing.T, opts *generate.Options) {
				opts.Ref = extract.Ref{Kind: extract.RefBranch, Name: "master"}
			},
			wantErr: "only a release tag or an engine-selected exact commit can be generated",
		},
		{
			name: "unknown ref kind",
			mutate: func(_ *testing.T, opts *generate.Options) {
				opts.Ref = extract.Ref{Kind: "snapshot", Name: "abc"}
			},
			wantErr: `ref kind "snapshot" must be tag or commit`,
		},
		{
			name: "malformed exact commit",
			mutate: func(_ *testing.T, opts *generate.Options) {
				opts.Ref = extract.Ref{Kind: extract.RefCommit, Name: "abc"}
				opts.Fetch = false
			},
			wantErr: "exact source commit",
		},
		{
			name: "exact commit without release context",
			mutate: func(_ *testing.T, opts *generate.Options) {
				opts.Ref = extract.Ref{Kind: extract.RefCommit, Name: strings.Repeat("a", 40)}
				opts.Fetch = false
			},
			wantErr: "requires its bounding release context",
		},
		{
			name: "exact commit without history anchor",
			mutate: func(_ *testing.T, opts *generate.Options) {
				opts.Ref = extract.Ref{Kind: extract.RefCommit, Name: strings.Repeat("a", 40)}
				opts.ReleaseContext = "v1.36.1"
				opts.Fetch = false
			},
			wantErr: "requires its published history anchor",
		},
		{
			name: "exact commit fetch",
			mutate: func(_ *testing.T, opts *generate.Options) {
				opts.Ref = extract.Ref{Kind: extract.RefCommit, Name: strings.Repeat("a", 40)}
				opts.ReleaseContext = "v1.36.1"
				opts.HistoryAnchor = strings.Repeat("a", 40)
				opts.HistoryAnchorRelease = "v1.36.1"
				opts.Fetch = true
			},
			wantErr: "cannot be fetched by object name",
		},
		{
			name: "no ref name",
			mutate: func(_ *testing.T, opts *generate.Options) {
				opts.Ref = extract.Ref{Kind: extract.RefTag}
			},
			wantErr: "a ref name is required",
		},
		{
			name: "publishing credential in the environment",
			mutate: func(_ *testing.T, opts *generate.Options) {
				opts.LookupEnv = func(name string) (string, bool) {
					if name == "SOAPBOX_GITHUB_TOKEN" {
						return "fake-credential-for-test", true
					}
					return "", false
				}
			},
			wantErr: "SOAPBOX_GITHUB_TOKEN is set",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			opts := baseOptions(t)
			test.mutate(t, &opts)

			result, err := generate.Generate(t.Context(), opts)
			if err == nil {
				t.Fatalf("generate: got nil error, want %q", test.wantErr)
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Errorf("generate: error = %v, want it to contain %q", err, test.wantErr)
			}
			// Nothing was measured, so there is nothing worth reporting. A
			// result here would be a report describing a run that never
			// started.
			if result != nil {
				t.Errorf("generate: got a result for an unusable option set, want none")
			}
		})
	}
}

// TestGenerateRefusesBranchAsUnsupported proves the branch refusal is
// distinguishable from a bad profile.
//
// It matters because the three answers are acted on differently. A branch is not
// a finding about the profile and not a broken engine: it is a shape this engine
// does not implement, and a caller has to be able to tell that apart from a
// profile CI should ask a human to review.
func TestGenerateRefusesBranchAsUnsupported(t *testing.T) {
	opts := baseOptions(t)
	opts.Ref = extract.Ref{Kind: extract.RefBranch, Name: "master"}

	_, err := generate.Generate(t.Context(), opts)
	if err == nil {
		t.Fatal("generate: got nil error, want a refusal")
	}
	if !errors.Is(err, generate.ErrUnsupported) {
		t.Errorf("generate: error = %v, want it to be ErrUnsupported", err)
	}
	// The refusal is a policy failure, because the engine worked and the answer
	// is no: the ref the profile was pointed at is one this engine will not
	// produce a module from. It is distinguished from an unacceptable profile by
	// ErrUnsupported rather than by the error kind.
	var policy *generate.PolicyError
	if !errors.As(err, &policy) {
		t.Fatalf("generate: error = %v, want a policy failure", err)
	}
	if policy.Stage != "options" {
		t.Errorf("policy stage = %s, want options", policy.Stage)
	}
}

// TestGenerateReportsRuntimeFailuresAsRuntime is the other half of the CLI
// contract.
//
// A caller acts on the two answers differently: a policy refusal means read the
// profile, and a runtime failure means retry or repair the environment. A run
// that could not reach its source, could not run its toolchain, or could not
// write its output has found nothing at all about the profile, and reporting it
// as a refusal would send a reviewer looking for a problem that is not there.
func TestGenerateReportsRuntimeFailuresAsRuntime(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(t *testing.T, opts *generate.Options)
		wantErr string
	}{
		{
			// The source host is unreachable, which is a transport condition.
			name: "unreachable source remote",
			mutate: func(t *testing.T, opts *generate.Options) {
				opts.SourceRemote = "file://" + filepath.Join(t.TempDir(), "absent")
				opts.Fetch = true
			},
			wantErr: "extract",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			opts := baseOptions(t)
			test.mutate(t, &opts)

			_, err := generate.Generate(t.Context(), opts)
			if err == nil {
				t.Fatalf("generate: got nil error, want a runtime failure")
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Errorf("generate: error = %v, want it to contain %q", err, test.wantErr)
			}
			var policy *generate.PolicyError
			if errors.As(err, &policy) {
				t.Errorf("generate: error = %v, want a runtime failure rather than a refusal at stage %s", err, policy.Stage)
			}
		})
	}
}
