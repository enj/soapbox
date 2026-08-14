package gitcli

import (
	"slices"
	"strings"
	"testing"
)

func TestGitHubTokenCredentialUsesHostScopedRuntimeConfig(t *testing.T) {
	const token = "ghs_internal_credential_test"
	credential, err := NewGitHubTokenCredential(token)
	if err != nil {
		t.Fatalf("credential: %v", err)
	}
	configured, err := credential.Apply(Options{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	want := []string{
		"GIT_CONFIG_COUNT=6",
		"GIT_CONFIG_KEY_0=credential.helper",
		"GIT_CONFIG_VALUE_0=",
		"GIT_CONFIG_KEY_1=http.extraHeader",
		"GIT_CONFIG_VALUE_1=",
		"GIT_CONFIG_KEY_2=http.https://github.com/.followRedirects",
		"GIT_CONFIG_VALUE_2=false",
		"GIT_CONFIG_KEY_3=http.https://github.com/.sslVerify",
		"GIT_CONFIG_VALUE_3=true",
		"GIT_CONFIG_KEY_4=http.https://github.com/.proxy",
		"GIT_CONFIG_VALUE_4=",
		"GIT_CONFIG_KEY_5=http.https://github.com/.extraHeader",
	}
	env := gitConfigEnv(configured.gitConfig)
	if len(env) != 13 {
		t.Fatalf("runtime config has %d entries, want 13", len(env))
	}
	for _, entry := range want {
		if !slices.Contains(env, entry) {
			t.Errorf("runtime config does not contain %q", entry)
		}
	}
	if !slices.ContainsFunc(env, func(entry string) bool {
		return strings.HasPrefix(entry, "GIT_CONFIG_VALUE_5=AUTHORIZATION: basic ")
	}) {
		t.Error("runtime config has no Basic authorization header")
	}
	if slices.ContainsFunc(env[:12], func(entry string) bool { return strings.Contains(entry, token) }) {
		t.Fatal("token appears outside the authorization value")
	}
}

func TestGitHubTokenCredentialOverridesRepositoryHTTPConfig(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	base, err := New(ctx, Options{Dir: dir, Inherit: []string{"PATH"}})
	if err != nil {
		t.Fatalf("base runner: %v", err)
	}
	if err := base.InitRepository(ctx, "main"); err != nil {
		t.Fatalf("init: %v", err)
	}
	for _, entry := range []struct {
		key   string
		value string
	}{
		{key: "http.https://github.com/.extraHeader", value: "Authorization: bearer repository-value"},
		{key: "http.https://github.com/.followRedirects", value: "true"},
		{key: "http.https://github.com/.proxy", value: "https://proxy.example.invalid"},
		{key: "http.https://github.com/.sslVerify", value: "false"},
	} {
		if err := base.SetConfigLocal(ctx, entry.key, entry.value); err != nil {
			t.Fatalf("set %s: %v", entry.key, err)
		}
	}

	credential, err := NewGitHubTokenCredential("ghs_repository_config_test")
	if err != nil {
		t.Fatalf("credential: %v", err)
	}
	opts, err := credential.Apply(Options{Dir: dir, Inherit: []string{"PATH"}})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	runner, err := New(ctx, opts)
	if err != nil {
		t.Fatalf("credentialed runner: %v", err)
	}
	out, err := runner.run(ctx, "config", "--get-urlmatch", "http", "https://github.com/enj/repository.git")
	if err != nil {
		t.Fatalf("effective HTTP config: %v", err)
	}
	for _, unwanted := range []string{"repository-value", "proxy.example.invalid"} {
		if strings.Contains(out, unwanted) {
			t.Fatalf("effective HTTP config retained repository value %q", unwanted)
		}
	}
	for _, want := range []string{
		"http.extraheader " + Placeholder,
		"http.followredirects false",
		"http.proxy ",
		"http.sslverify true",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("effective HTTP config does not contain %q", want)
		}
	}
}

func TestValidateGitConfigFailsClosed(t *testing.T) {
	for _, test := range []struct {
		name    string
		entries []gitConfig
	}{
		{name: "empty key", entries: []gitConfig{{value: "x"}}},
		{name: "duplicate key", entries: []gitConfig{{key: "a", value: "x"}, {key: "a", value: "y"}}},
		{name: "newline key", entries: []gitConfig{{key: "a\nb", value: "x"}}},
		{name: "newline value", entries: []gitConfig{{key: "a", value: "x\ny"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateGitConfig(test.entries); err == nil {
				t.Fatal("invalid runtime config was accepted")
			}
		})
	}
}
