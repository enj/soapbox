// Package actionsctx validates that the process is running inside a trusted
// GitHub Actions workflow before accepting a publishing credential.
//
// The validation is fail-closed: every required property must be present and
// correct, and any missing or contradictory value refuses the run. This
// protects against a credential being accepted in a context that was not the
// one the operator approved, such as a pull_request trigger, a fork, a
// non-default branch, or a manual invocation that happens to set the token.
package actionsctx

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// Context is the validated GitHub Actions environment.
//
// Every field has already passed fail-closed validation when this value exists.
// The zero value is never returned by Validate.
type Context struct {
	// Repository is the owner/name slug (GITHUB_REPOSITORY).
	Repository string
	// DefaultBranch is the repository's configured default branch
	// (derived from GITHUB_REF on the default branch).
	DefaultBranch string
	// SHA is the commit the workflow is running against (GITHUB_SHA),
	// validated as exactly 40 lowercase hexadecimal characters.
	SHA string
	// EventName is the trigger event (GITHUB_EVENT_NAME).
	EventName string
}

// LookupEnv reads one environment variable. It matches os.LookupEnv.
type LookupEnv func(string) (string, bool)

// Options configures the validation.
type Options struct {
	// LookupEnv reads the environment. A nil value uses os.LookupEnv.
	LookupEnv LookupEnv
	// Repository is the expected owner/name slug from the profile. It must
	// not be empty: skipping the repository check would accept a credential
	// in a repository the operator never configured.
	Repository string
	// DefaultBranch is the expected default branch from the profile. It must
	// not be empty: skipping the branch check would accept a credential on a
	// branch the operator never configured.
	DefaultBranch string
}

// trustedEvents are the only trigger events that may carry a publishing
// credential. A pull_request or push event is never trusted because the code
// it runs is determined by the contributor, not the repository owner.
var trustedEvents = []string{"schedule", "workflow_dispatch"}

// ErrUntrustedContext reports that the Actions environment does not satisfy the
// requirements for accepting a publishing credential.
var ErrUntrustedContext = errors.New("untrusted workflow context")

// Validate checks the GitHub Actions environment for the properties a
// publishing credential requires.
//
// The checks are ordered so the most fundamental requirement (running inside
// Actions at all) is checked first, and the least likely misconfiguration
// (SHA format) is checked last, so an operator sees the most helpful
// message first.
func Validate(opts Options) (*Context, error) {
	lookup := opts.LookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}

	// 0. The caller must state what it expects. Empty expectations would skip
	// checks and silently accept a credential in any repository or on any
	// branch, which is the opposite of fail-closed.
	if opts.Repository == "" {
		return nil, fmt.Errorf("%w: expected repository must not be empty", ErrUntrustedContext)
	}
	if opts.DefaultBranch == "" {
		return nil, fmt.Errorf("%w: expected default branch must not be empty", ErrUntrustedContext)
	}

	// 1. Running inside GitHub Actions at all.
	if value, ok := lookup("GITHUB_ACTIONS"); !ok || value != "true" {
		return nil, fmt.Errorf("%w: GITHUB_ACTIONS is not \"true\"", ErrUntrustedContext)
	}

	// 2. Repository identity matches the configured destination.
	repo := envOrEmpty(lookup, "GITHUB_REPOSITORY")
	if repo == "" {
		return nil, fmt.Errorf("%w: GITHUB_REPOSITORY is not set", ErrUntrustedContext)
	}
	if repo != opts.Repository {
		return nil, fmt.Errorf("%w: GITHUB_REPOSITORY %q does not match configured repository %q",
			ErrUntrustedContext, repo, opts.Repository)
	}

	// 3. Running on the default branch. GITHUB_REF is refs/heads/<branch> for
	// branch events and something else for tag/PR events, so the ref must
	// both start with refs/heads/ and match the configured default branch.
	ref := envOrEmpty(lookup, "GITHUB_REF")
	if ref == "" {
		return nil, fmt.Errorf("%w: GITHUB_REF is not set", ErrUntrustedContext)
	}
	branch, ok := strings.CutPrefix(ref, "refs/heads/")
	if !ok {
		return nil, fmt.Errorf("%w: GITHUB_REF %q is not a branch ref", ErrUntrustedContext, ref)
	}
	if branch != opts.DefaultBranch {
		return nil, fmt.Errorf("%w: GITHUB_REF branch %q does not match configured default branch %q",
			ErrUntrustedContext, branch, opts.DefaultBranch)
	}

	// 4. Protected ref. GitHub sets GITHUB_REF_PROTECTED to "true" when the
	// ref is a protected branch. A publishing credential must not be accepted
	// on an unprotected branch, because an unprotected branch can be force-
	// pushed by any collaborator.
	if value, ok := lookup("GITHUB_REF_PROTECTED"); !ok || value != "true" {
		return nil, fmt.Errorf("%w: GITHUB_REF_PROTECTED is not \"true\"", ErrUntrustedContext)
	}

	// 5. Trusted trigger event.
	event := envOrEmpty(lookup, "GITHUB_EVENT_NAME")
	if event == "" {
		return nil, fmt.Errorf("%w: GITHUB_EVENT_NAME is not set", ErrUntrustedContext)
	}
	trusted := false
	for _, allowed := range trustedEvents {
		if event == allowed {
			trusted = true
			break
		}
	}
	if !trusted {
		return nil, fmt.Errorf("%w: GITHUB_EVENT_NAME %q is not a trusted trigger (want %s)",
			ErrUntrustedContext, event, strings.Join(trustedEvents, " or "))
	}

	// 6. SHA must be present and well-formed. GitHub provides a 40-character
	// lowercase hex SHA. The caller compares it to the destination HEAD after
	// this function returns; here we only validate the format so a malformed
	// value does not reach downstream code.
	sha := envOrEmpty(lookup, "GITHUB_SHA")
	if sha == "" {
		return nil, fmt.Errorf("%w: GITHUB_SHA is not set", ErrUntrustedContext)
	}
	if err := validateHexSHA(sha); err != nil {
		return nil, fmt.Errorf("%w: GITHUB_SHA: %w", ErrUntrustedContext, err)
	}

	return &Context{
		Repository:    repo,
		DefaultBranch: branch,
		SHA:           sha,
		EventName:     event,
	}, nil
}

// validateHexSHA checks that value is exactly 40 lowercase hexadecimal
// characters, which is the format GitHub uses for GITHUB_SHA.
func validateHexSHA(value string) error {
	if len(value) != 40 {
		return fmt.Errorf("%q must be 40 hexadecimal characters, got %d", value, len(value))
	}
	for _, r := range value {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f':
		default:
			return fmt.Errorf("%q must be lowercase hexadecimal", value)
		}
	}
	return nil
}

func envOrEmpty(lookup LookupEnv, name string) string {
	value, _ := lookup(name)
	return value
}
