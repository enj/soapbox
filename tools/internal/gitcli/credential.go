package gitcli

import (
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

const githubHTTPConfigPrefix = "http.https://" + PublishHost + "/."

type gitConfig struct {
	key   string
	value string
}

// GitHubTokenCredential configures Git's HTTPS transport for one repository-
// scoped GITHUB_TOKEN without putting the credential in argv or a remote URL.
// It resets ambient credential helpers and headers, requires TLS verification,
// and disables proxies and redirects for the github.com credential scope.
//
// Its fields are deliberately private. A caller can apply it to Runner options,
// but cannot accidentally format token-bearing configuration into a report.
type GitHubTokenCredential struct {
	config  []gitConfig
	secrets []string
}

// String renders the credential without exposing any token representation.
func (*GitHubTokenCredential) String() string { return "GitHub token credential" }

// GoString renders the credential safely for the %#v format.
func (*GitHubTokenCredential) GoString() string { return "gitcli.GitHubTokenCredential{}" }

// NewGitHubTokenCredential builds the host-scoped Basic authorization GitHub
// expects for x-access-token credentials.
func NewGitHubTokenCredential(token string) (*GitHubTokenCredential, error) {
	if token == "" {
		return nil, errors.New("github token credential: token must not be empty")
	}
	for _, r := range token {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return nil, errors.New("github token credential: token must not contain whitespace or control characters")
		}
	}

	basic := "x-access-token:" + token
	encoded := base64.StdEncoding.EncodeToString([]byte(basic))
	header := "AUTHORIZATION: basic " + encoded
	return &GitHubTokenCredential{
		config: []gitConfig{
			{key: "credential.helper", value: ""},
			{key: "http.extraHeader", value: ""},
			{key: githubHTTPConfigPrefix + "followRedirects", value: "false"},
			{key: githubHTTPConfigPrefix + "sslVerify", value: "true"},
			{key: githubHTTPConfigPrefix + "proxy", value: ""},
			{key: githubHTTPConfigPrefix + "extraHeader", value: header},
		},
		secrets: []string{token, basic, encoded, "Basic " + encoded, "basic " + encoded, header},
	}, nil
}

// Apply returns runner options carrying the credential. Existing environment
// config is refused instead of renumbered: composing two GIT_CONFIG_COUNT sets
// incorrectly can drop one silently, which could either lose authentication or
// send a credential under a caller-controlled key.
func (c *GitHubTokenCredential) Apply(base Options) (Options, error) {
	if c == nil || len(c.config) == 0 || len(c.secrets) == 0 {
		return Options{}, errors.New("github token credential: no credential")
	}
	if len(base.gitConfig) != 0 {
		return Options{}, errors.New("github token credential: runner options already carry package-owned Git config")
	}
	for i, name := range base.Inherit {
		if conflictsWithCredentialRuntime(name) {
			return Options{}, fmt.Errorf("github token credential: inherited environment name %d conflicts with credential-owned transport", i)
		}
	}
	for _, set := range []struct {
		kind    string
		entries []string
	}{
		{kind: "isolation", entries: base.Isolation},
		{kind: "environment", entries: base.Env},
	} {
		for i, entry := range set.entries {
			name, _, ok := strings.Cut(entry, "=")
			if !ok {
				return Options{}, fmt.Errorf("github token credential: %s entry %d is malformed", set.kind, i)
			}
			if conflictsWithCredentialRuntime(name) {
				return Options{}, fmt.Errorf("github token credential: %s entry %d conflicts with credential-owned transport", set.kind, i)
			}
		}
	}
	configured := base
	configured.Env = slices.Clone(base.Env)
	configured.Secrets = append(slices.Clone(base.Secrets), c.secrets...)
	configured.gitConfig = slices.Clone(c.config)
	return configured, nil
}

func conflictsWithCredentialRuntime(name string) bool {
	name = strings.ToUpper(name)
	if name == "GIT_CONFIG" || strings.HasPrefix(name, "GIT_CONFIG_") {
		return true
	}
	if name == "GIT_CURL_VERBOSE" || strings.HasPrefix(name, "GIT_TRACE") || strings.HasPrefix(name, "GIT_SSL_") {
		return true
	}
	switch name {
	case "GIT_ASKPASS", "SSH_ASKPASS", "GIT_TERMINAL_PROMPT", "GIT_EXEC_PATH",
		"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY",
		"SSL_CERT_FILE", "SSL_CERT_DIR", "CURL_CA_BUNDLE":
		return true
	default:
		return false
	}
}

func validateGitConfig(entries []gitConfig) error {
	seen := make(map[string]bool, len(entries))
	for i, entry := range entries {
		if entry.key == "" {
			return fmt.Errorf("git config entry %d has no key", i)
		}
		for _, value := range []string{entry.key, entry.value} {
			if strings.ContainsRune(value, '\x00') || strings.ContainsAny(value, "\r\n") {
				return fmt.Errorf("git config entry %d contains a control character", i)
			}
		}
		if seen[entry.key] {
			return fmt.Errorf("git config key %q is configured twice", entry.key)
		}
		seen[entry.key] = true
	}
	return nil
}

func gitConfigEnv(entries []gitConfig) []string {
	if len(entries) == 0 {
		return nil
	}
	env := make([]string, 0, 1+2*len(entries))
	env = append(env, "GIT_CONFIG_COUNT="+strconv.Itoa(len(entries)))
	for i, entry := range entries {
		env = append(env,
			"GIT_CONFIG_KEY_"+strconv.Itoa(i)+"="+entry.key,
			"GIT_CONFIG_VALUE_"+strconv.Itoa(i)+"="+entry.value,
		)
	}
	return env
}
