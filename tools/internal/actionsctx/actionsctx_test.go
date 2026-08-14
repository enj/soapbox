package actionsctx_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/actionsctx"
)

// validSHA is a well-formed 40-character lowercase hex SHA for tests.
const validSHA = "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"

// validEnv returns the full set of environment variables that pass validation.
func validEnv() map[string]string {
	return map[string]string{
		"GITHUB_ACTIONS":       "true",
		"GITHUB_REPOSITORY":    "enj/rbac_authorizer",
		"GITHUB_REF":           "refs/heads/main",
		"GITHUB_REF_PROTECTED": "true",
		"GITHUB_EVENT_NAME":    "schedule",
		"GITHUB_SHA":           validSHA,
	}
}

// validOpts returns options that match validEnv.
func validOpts(env map[string]string) actionsctx.Options {
	return actionsctx.Options{
		LookupEnv:     envMap(env),
		Repository:    "enj/rbac_authorizer",
		DefaultBranch: "main",
	}
}

func TestValidateRequiresExpectedRepository(t *testing.T) {
	env := validEnv()
	_, err := actionsctx.Validate(actionsctx.Options{
		LookupEnv:     envMap(env),
		DefaultBranch: "main",
	})
	if !errors.Is(err, actionsctx.ErrUntrustedContext) {
		t.Fatalf("got %v, want ErrUntrustedContext", err)
	}
	if !strings.Contains(err.Error(), "expected repository must not be empty") {
		t.Fatalf("error %q does not explain empty repository", err)
	}
}

func TestValidateRequiresExpectedDefaultBranch(t *testing.T) {
	env := validEnv()
	_, err := actionsctx.Validate(actionsctx.Options{
		LookupEnv:  envMap(env),
		Repository: "enj/rbac_authorizer",
	})
	if !errors.Is(err, actionsctx.ErrUntrustedContext) {
		t.Fatalf("got %v, want ErrUntrustedContext", err)
	}
	if !strings.Contains(err.Error(), "expected default branch must not be empty") {
		t.Fatalf("error %q does not explain empty branch", err)
	}
}

func TestValidateRequiresGitHubActions(t *testing.T) {
	_, err := actionsctx.Validate(actionsctx.Options{
		LookupEnv:     envMap(nil),
		Repository:    "enj/rbac_authorizer",
		DefaultBranch: "main",
	})
	if !errors.Is(err, actionsctx.ErrUntrustedContext) {
		t.Fatalf("got %v, want ErrUntrustedContext", err)
	}
	if !strings.Contains(err.Error(), "GITHUB_ACTIONS") {
		t.Fatalf("error %q does not mention GITHUB_ACTIONS", err)
	}
}

func TestValidateRequiresRepository(t *testing.T) {
	_, err := actionsctx.Validate(actionsctx.Options{
		LookupEnv: envMap(map[string]string{
			"GITHUB_ACTIONS": "true",
		}),
		Repository:    "enj/rbac_authorizer",
		DefaultBranch: "main",
	})
	if !errors.Is(err, actionsctx.ErrUntrustedContext) {
		t.Fatalf("got %v, want ErrUntrustedContext", err)
	}
	if !strings.Contains(err.Error(), "GITHUB_REPOSITORY") {
		t.Fatalf("error %q does not mention GITHUB_REPOSITORY", err)
	}
}

func TestValidateRefusesRepositoryMismatch(t *testing.T) {
	env := validEnv()
	env["GITHUB_REPOSITORY"] = "other/repo"
	_, err := actionsctx.Validate(validOpts(env))
	if !errors.Is(err, actionsctx.ErrUntrustedContext) {
		t.Fatalf("got %v, want ErrUntrustedContext", err)
	}
	if !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("error %q does not explain mismatch", err)
	}
}

func TestValidateRequiresRef(t *testing.T) {
	env := validEnv()
	delete(env, "GITHUB_REF")
	_, err := actionsctx.Validate(validOpts(env))
	if !errors.Is(err, actionsctx.ErrUntrustedContext) {
		t.Fatalf("got %v, want ErrUntrustedContext", err)
	}
	if !strings.Contains(err.Error(), "GITHUB_REF is not set") {
		t.Fatalf("error %q does not mention GITHUB_REF", err)
	}
}

func TestValidateRefusesBranchMismatch(t *testing.T) {
	env := validEnv()
	env["GITHUB_REF"] = "refs/heads/feature"
	_, err := actionsctx.Validate(validOpts(env))
	if !errors.Is(err, actionsctx.ErrUntrustedContext) {
		t.Fatalf("got %v, want ErrUntrustedContext", err)
	}
	if !strings.Contains(err.Error(), "default branch") {
		t.Fatalf("error %q does not explain branch mismatch", err)
	}
}

func TestValidateRefusesNonBranchRef(t *testing.T) {
	env := validEnv()
	env["GITHUB_REF"] = "refs/tags/v1.0.0"
	_, err := actionsctx.Validate(validOpts(env))
	if !errors.Is(err, actionsctx.ErrUntrustedContext) {
		t.Fatalf("got %v, want ErrUntrustedContext", err)
	}
	if !strings.Contains(err.Error(), "not a branch ref") {
		t.Fatalf("error %q does not explain non-branch ref", err)
	}
}

func TestValidateRequiresProtectedRef(t *testing.T) {
	env := validEnv()
	delete(env, "GITHUB_REF_PROTECTED")
	_, err := actionsctx.Validate(validOpts(env))
	if !errors.Is(err, actionsctx.ErrUntrustedContext) {
		t.Fatalf("got %v, want ErrUntrustedContext", err)
	}
	if !strings.Contains(err.Error(), "GITHUB_REF_PROTECTED") {
		t.Fatalf("error %q does not mention GITHUB_REF_PROTECTED", err)
	}
}

func TestValidateRefusesUnprotectedRef(t *testing.T) {
	env := validEnv()
	env["GITHUB_REF_PROTECTED"] = "false"
	_, err := actionsctx.Validate(validOpts(env))
	if !errors.Is(err, actionsctx.ErrUntrustedContext) {
		t.Fatalf("got %v, want ErrUntrustedContext", err)
	}
	if !strings.Contains(err.Error(), "GITHUB_REF_PROTECTED is not \"true\"") {
		t.Fatalf("error %q does not explain unprotected ref", err)
	}
}

func TestValidateRefusesUntrustedEvent(t *testing.T) {
	for _, event := range []string{"push", "pull_request", "pull_request_target"} {
		t.Run(event, func(t *testing.T) {
			env := validEnv()
			env["GITHUB_EVENT_NAME"] = event
			_, err := actionsctx.Validate(validOpts(env))
			if !errors.Is(err, actionsctx.ErrUntrustedContext) {
				t.Fatalf("got %v, want ErrUntrustedContext", err)
			}
			if !strings.Contains(err.Error(), "not a trusted trigger") {
				t.Fatalf("error %q does not explain untrusted event", err)
			}
		})
	}
}

func TestValidateRequiresSHA(t *testing.T) {
	env := validEnv()
	delete(env, "GITHUB_SHA")
	_, err := actionsctx.Validate(validOpts(env))
	if !errors.Is(err, actionsctx.ErrUntrustedContext) {
		t.Fatalf("got %v, want ErrUntrustedContext", err)
	}
	if !strings.Contains(err.Error(), "GITHUB_SHA is not set") {
		t.Fatalf("error %q does not mention GITHUB_SHA", err)
	}
}

func TestValidateRefusesMalformedSHA(t *testing.T) {
	tests := []struct {
		name string
		sha  string
		want string
	}{{
		name: "too short",
		sha:  "abc123",
		want: "40 hexadecimal characters",
	}, {
		name: "uppercase",
		sha:  "A1B2C3D4E5F6A1B2C3D4E5F6A1B2C3D4E5F6A1B2",
		want: "lowercase hexadecimal",
	}, {
		name: "non-hex character",
		sha:  "g1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2",
		want: "lowercase hexadecimal",
	}, {
		name: "too long",
		sha:  "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2aa",
		want: "40 hexadecimal characters",
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := validEnv()
			env["GITHUB_SHA"] = tt.sha
			_, err := actionsctx.Validate(validOpts(env))
			if !errors.Is(err, actionsctx.ErrUntrustedContext) {
				t.Fatalf("got %v, want ErrUntrustedContext", err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %q does not contain %q", err, tt.want)
			}
		})
	}
}

func TestValidateAcceptsTrustedSchedule(t *testing.T) {
	env := validEnv()
	ctx, err := actionsctx.Validate(validOpts(env))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ctx.Repository != "enj/rbac_authorizer" {
		t.Errorf("Repository = %q", ctx.Repository)
	}
	if ctx.DefaultBranch != "main" {
		t.Errorf("DefaultBranch = %q", ctx.DefaultBranch)
	}
	if ctx.SHA != validSHA {
		t.Errorf("SHA = %q", ctx.SHA)
	}
	if ctx.EventName != "schedule" {
		t.Errorf("EventName = %q", ctx.EventName)
	}
}

func TestValidateAcceptsTrustedWorkflowDispatch(t *testing.T) {
	env := validEnv()
	env["GITHUB_EVENT_NAME"] = "workflow_dispatch"
	ctx, err := actionsctx.Validate(validOpts(env))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ctx.EventName != "workflow_dispatch" {
		t.Errorf("EventName = %q", ctx.EventName)
	}
}

// envMap builds a LookupEnv that answers from a static map.
func envMap(m map[string]string) actionsctx.LookupEnv {
	return func(name string) (string, bool) {
		v, ok := m[name]
		return v, ok
	}
}
