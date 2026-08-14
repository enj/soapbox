package gitcli_test

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/gitcli"
)

func TestGitHubTokenCredentialConfiguresHostScopedEnvironment(t *testing.T) {
	const token = "ghs_repository_scoped_test_token"
	credential, err := gitcli.NewGitHubTokenCredential(token)
	if err != nil {
		t.Fatalf("credential: %v", err)
	}
	base := gitcli.Options{Dir: t.TempDir(), Env: []string{"EXAMPLE=value"}, Secrets: []string{"existing-secret"}}
	configured, err := credential.Apply(base)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !slices.Equal(configured.Env, []string{"EXAMPLE=value"}) {
		t.Errorf("caller environment changed to %v", configured.Env)
	}
	if slices.ContainsFunc(configured.Env, func(entry string) bool { return strings.Contains(entry, token) }) {
		t.Fatal("raw token appears in the Git environment")
	}
	if !slices.Contains(configured.Secrets, token) {
		t.Fatal("raw token was not seeded into the redactor secrets")
	}
	if len(base.Env) != 1 || len(base.Secrets) != 1 {
		t.Fatal("applying the credential mutated the caller's options")
	}

	runner, err := gitcli.New(t.Context(), configured)
	if err != nil {
		t.Fatalf("runner: %v", err)
	}
	err = runner.CloneSource(t.Context(), gitcli.SourceCloneOptions{
		Remote: "https://github.com/kubernetes/kubernetes.git", Directory: t.TempDir(), Bare: true,
	})
	if !errors.Is(err, gitcli.ErrCredentialedRunner) {
		t.Fatalf("source clone error = %v, want ErrCredentialedRunner", err)
	}
}

func TestGitHubTokenCredentialFormattingOmitsToken(t *testing.T) {
	t.Parallel()
	const token = "ghs_formatting_secret"
	credential, err := gitcli.NewGitHubTokenCredential(token)
	if err != nil {
		t.Fatalf("credential: %v", err)
	}
	for _, format := range []string{"%v", "%+v", "%#v"} {
		rendered := fmt.Sprintf(format, credential)
		if strings.Contains(rendered, token) {
			t.Fatalf("format %q leaked token in %q", format, rendered)
		}
	}
}

func TestGitHubTokenCredentialRefusesInvalidInputs(t *testing.T) {
	for _, test := range []struct {
		name  string
		token string
	}{
		{name: "empty"},
		{name: "space", token: "token with space"},
		{name: "newline", token: "token\nvalue"},
		{name: "unicode whitespace", token: "token value"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := gitcli.NewGitHubTokenCredential(test.token); err == nil {
				t.Fatal("invalid token was accepted")
			}
		})
	}

	credential, err := gitcli.NewGitHubTokenCredential("valid_token")
	if err != nil {
		t.Fatalf("credential: %v", err)
	}
	for _, entry := range []string{
		"GIT_CONFIG_COUNT=0",
		"GIT_CONFIG_KEY_7=credential.helper",
		"GIT_CONFIG_VALUE_3=value",
		"GIT_CONFIG_PARAMETERS='http.sslVerify=false'",
		"GIT_CONFIG_GLOBAL=/tmp/config",
		"GIT_TRACE_CURL=/tmp/trace",
		"GIT_SSL_NO_VERIFY=1",
		"GIT_ASKPASS=/tmp/helper",
		"https_proxy=https://proxy.example.invalid",
	} {
		t.Run(entry, func(t *testing.T) {
			if _, err := credential.Apply(gitcli.Options{Env: []string{entry}}); err == nil {
				t.Fatal("conflicting transport environment was accepted")
			}
		})
	}
	for _, test := range []struct {
		name string
		opts gitcli.Options
	}{
		{name: "inherited config", opts: gitcli.Options{Inherit: []string{"PATH", "GIT_CONFIG_GLOBAL"}}},
		{name: "isolation config", opts: gitcli.Options{Isolation: []string{"GIT_CONFIG_SYSTEM=/tmp/config"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := credential.Apply(test.opts); err == nil {
				t.Fatal("conflicting transport environment was accepted")
			}
		})
	}
}
