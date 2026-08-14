package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/actionsctx"
	"github.com/enj/soapbox/tools/internal/config"
	"github.com/enj/soapbox/tools/internal/ghapi"
	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/testsupport"
)

// testToken stands in for a live workflow token in tests.
const testToken = "ghs_testauthtoken0000000000000000"

// validSHA is a well-formed 40-character lowercase hex SHA for tests.
const validSHA = "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"

// validActionsEnv returns an environment map that satisfies every actionsctx
// check for the destination repository enj/rbac_authorizer on main.
func validActionsEnv(sha string) map[string]string {
	return map[string]string{
		"GITHUB_ACTIONS":       "true",
		"GITHUB_REPOSITORY":    "enj/rbac_authorizer",
		"GITHUB_REF":           "refs/heads/main",
		"GITHUB_REF_PROTECTED": "true",
		"GITHUB_EVENT_NAME":    "schedule",
		"GITHUB_SHA":           sha,
	}
}

// mapLookup builds a LookupEnv that answers from a static map.
func mapLookup(m map[string]string) actionsctx.LookupEnv {
	return func(name string) (string, bool) {
		if m == nil {
			return "", false
		}
		v, ok := m[name]
		return v, ok
	}
}

// testProfileConfig returns a minimal Config that passes syncToken's needs.
func testProfileConfig() *config.Config {
	return &config.Config{
		Destination: config.Destination{
			Repository: "enj/rbac_authorizer",
			Branch:     "main",
		},
		Publication: config.Publication{
			Mode: config.PublicationModeAutomatic,
		},
	}
}

// --- syncToken tests ---

// TestSyncTokenUnattendedValidatesBeforeReadingToken verifies that -unattended
// validates the Actions context before looking up the token. When the context
// is invalid, the error names the context problem, not the missing token.
func TestSyncTokenUnattendedValidatesBeforeReadingToken(t *testing.T) {
	// Token is present but GITHUB_ACTIONS is not, so context validation fails
	// before the token is ever read.
	env := map[string]string{
		tokenEnvName: testToken,
	}
	fs, flags := syncFlagSet()
	_ = fs.Parse([]string{"-unattended"})
	usage := func(io.Writer) {}
	_, _, err := syncToken(flags, testProfileConfig(), usage, mapLookup(env))
	if err == nil {
		t.Fatal("expected context validation error")
	}
	if !strings.Contains(err.Error(), "GITHUB_ACTIONS") {
		t.Fatalf("error %q names the wrong problem; want GITHUB_ACTIONS", err)
	}
}

// TestSyncTokenUnattendedRequiresToken verifies that -unattended with a valid
// context but no token is a usage error.
func TestSyncTokenUnattendedRequiresToken(t *testing.T) {
	env := validActionsEnv(validSHA)
	// No token in env.
	fs, flags := syncFlagSet()
	_ = fs.Parse([]string{"-unattended"})
	usage := func(io.Writer) {}
	_, _, err := syncToken(flags, testProfileConfig(), usage, mapLookup(env))
	if err == nil {
		t.Fatal("expected error for missing token with -unattended")
	}
	if !strings.Contains(err.Error(), tokenEnvName) {
		t.Fatalf("error %q does not mention %s", err, tokenEnvName)
	}
}

// TestSyncTokenReturnsNilWithoutToken verifies the no-credential path.
func TestSyncTokenReturnsNilWithoutToken(t *testing.T) {
	fs, flags := syncFlagSet()
	_ = fs.Parse(nil)
	usage := func(io.Writer) {}
	token, ctx, err := syncToken(flags, testProfileConfig(), usage, mapLookup(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "" {
		t.Fatalf("token = %q, want empty", token)
	}
	if ctx != nil {
		t.Fatalf("context = %v, want nil", ctx)
	}
}

// TestSyncTokenValidatesContextWhenTokenPresent verifies that even without
// -unattended, a present token triggers context validation before the token
// value is returned.
func TestSyncTokenValidatesContextWhenTokenPresent(t *testing.T) {
	env := map[string]string{
		tokenEnvName: testToken,
		// No GITHUB_ACTIONS, so validation fails.
	}
	fs, flags := syncFlagSet()
	_ = fs.Parse(nil)
	usage := func(io.Writer) {}
	_, _, err := syncToken(flags, testProfileConfig(), usage, mapLookup(env))
	if err == nil {
		t.Fatal("expected context validation error when token present without GITHUB_ACTIONS")
	}
	if !strings.Contains(err.Error(), "GITHUB_ACTIONS") {
		t.Fatalf("error %q does not mention GITHUB_ACTIONS", err)
	}
}

// TestSyncTokenSucceedsInTrustedContext verifies the happy path.
func TestSyncTokenSucceedsInTrustedContext(t *testing.T) {
	env := validActionsEnv(validSHA)
	env[tokenEnvName] = testToken
	fs, flags := syncFlagSet()
	_ = fs.Parse(nil)
	usage := func(io.Writer) {}
	got, ctx, err := syncToken(flags, testProfileConfig(), usage, mapLookup(env))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != testToken {
		t.Fatalf("token = %q, want %q", got, testToken)
	}
	if ctx == nil {
		t.Fatal("context is nil, want non-nil")
	}
	if ctx.SHA != validSHA {
		t.Fatalf("SHA = %q, want %q", ctx.SHA, validSHA)
	}
}

// --- syncLookupEnv tests ---

// TestSyncLookupEnvHidesToken verifies that the LookupEnv returned by
// syncLookupEnv hides the token from generate and extract.
func TestSyncLookupEnvHidesWorkflowControl(t *testing.T) {
	base := mapLookup(map[string]string{
		tokenEnvName:    testToken,
		approvalEnvName: "sha256:" + strings.Repeat("a", 64),
		"PATH":          "/usr/bin",
	})
	lookup := syncLookupEnv(base)

	for _, name := range []string{tokenEnvName, approvalEnvName} {
		if value, ok := lookup(name); ok || value != "" {
			t.Fatalf("lookup(%q) = (%q, %v), want (\"\", false)", name, value, ok)
		}
	}
	// Other variables must pass through.
	if value, ok := lookup("PATH"); !ok || value != "/usr/bin" {
		t.Fatalf("lookup(PATH) = (%q, %v), want (\"/usr/bin\", true)", value, ok)
	}
}

// TestSyncLookupEnvAlwaysHidesTokenEvenWhenAbsent verifies that syncLookupEnv
// always returns a wrapper that blocks the token name, even when no token was
// present. A nil return would let generate fall back to os.LookupEnv and a
// later environment mutation could expose the credential.
func TestSyncLookupEnvAlwaysHidesTokenEvenWhenAbsent(t *testing.T) {
	base := mapLookup(map[string]string{
		"PATH": "/usr/bin",
	})
	lookup := syncLookupEnv(base)
	if lookup == nil {
		t.Fatal("syncLookupEnv must never return nil")
	}

	// Token name is hidden even though the base does not have it.
	if value, ok := lookup(tokenEnvName); ok || value != "" {
		t.Fatalf("lookup(%q) = (%q, %v), want (\"\", false)", tokenEnvName, value, ok)
	}

	// Other variables pass through.
	if value, ok := lookup("PATH"); !ok || value != "/usr/bin" {
		t.Fatalf("lookup(PATH) = (%q, %v), want (\"/usr/bin\", true)", value, ok)
	}
}

func TestCheckWorkflowSyncFlags(t *testing.T) {
	for _, flag := range []string{
		"local-remote", "remote", "identity",
		"state-commit", "tag", "branch", "source-remote", "patch-branch",
		"offline", "fetch",
	} {
		t.Run(flag, func(t *testing.T) {
			err := checkWorkflowSyncFlags(map[string]bool{flag: true})
			if err == nil || !strings.Contains(err.Error(), "-"+flag+" cannot be given") {
				t.Fatalf("check = %v, want a %s refusal", err, flag)
			}
		})
	}
	if err := checkWorkflowSyncFlags(map[string]bool{
		"cache": true, "destination": true, "dir": true,
		"apply": true, "approve": true,
	}); err != nil {
		t.Fatalf("generated paths or manual approval pair were refused: %v", err)
	}
}

func TestValidateWorkflowApproval(t *testing.T) {
	valid := "sha256:" + strings.Repeat("a", 64)
	if err := validateWorkflowApproval(valid); err != nil {
		t.Fatalf("valid approval: %v", err)
	}
	for _, approval := range []string{
		"", "sha256:abc", "sha256:" + strings.Repeat("A", 64),
		"sha512:" + strings.Repeat("a", 64), "sha256:" + strings.Repeat("g", 64),
	} {
		if err := validateWorkflowApproval(approval); err == nil {
			t.Errorf("approval %q was accepted", approval)
		}
	}
}

// --- verifySyncSHA tests ---

// TestVerifySyncSHAMatchesHead uses a real temporary Git repository built by
// testsupport to verify the SHA comparison with a no-lazy-fetch runner,
// matching how production runs the check before any credential is built.
func TestVerifySyncSHAMatchesHead(t *testing.T) {
	ctx := t.Context()
	repo := testsupport.NewRepo(ctx, t, testsupport.Options{
		UserName:  "Test",
		UserEmail: "test@test.com",
	})
	head := repo.Commit(ctx, t, "init", gitcli.CommitOptions{AllowEmpty: true})

	offline := repo.Git.WithNoLazyFetch()
	if !offline.IsNoLazyFetch() {
		t.Fatal("WithNoLazyFetch did not set the flag")
	}

	// Match: should succeed.
	if err := verifySyncSHA(ctx, offline, head); err != nil {
		t.Fatalf("unexpected error with matching SHA: %v", err)
	}

	// Mismatch: should fail.
	fakeSHA := "0000000000000000000000000000000000000000"
	if err := verifySyncSHA(ctx, offline, fakeSHA); err == nil {
		t.Fatal("expected error with mismatching SHA")
	} else if !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("error %q does not explain mismatch", err)
	}
}

// --- verifySyncCheckout tests ---

// TestVerifySyncCheckoutSameRepo verifies that a profile directory inside the
// destination repository is accepted.
func TestVerifySyncCheckoutSameRepo(t *testing.T) {
	ctx := t.Context()
	repo := testsupport.NewRepo(ctx, t, testsupport.Options{
		UserName:  "Test",
		UserEmail: "test@test.com",
	})
	repo.Commit(ctx, t, "init", gitcli.CommitOptions{AllowEmpty: true})

	offline := repo.Git.WithNoLazyFetch()

	// Profile directory is the repo root itself: should pass.
	if err := verifySyncCheckout(ctx, offline, repo.Dir); err != nil {
		t.Fatalf("unexpected error for same directory: %v", err)
	}

	// Profile directory is a subdirectory of the repo: should pass.
	sub := filepath.Join(repo.Dir, "subdir")
	if err := os.MkdirAll(sub, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := verifySyncCheckout(ctx, offline, sub); err != nil {
		t.Fatalf("unexpected error for subdirectory: %v", err)
	}
}

// TestVerifySyncCheckoutDifferentRepo verifies that a profile directory in a
// different repository is refused. This prevents -dir from supplying a
// profile that controls what the token-bearing destination publishes.
func TestVerifySyncCheckoutDifferentRepo(t *testing.T) {
	ctx := t.Context()
	dest := testsupport.NewRepo(ctx, t, testsupport.Options{
		UserName:  "Test",
		UserEmail: "test@test.com",
	})
	dest.Commit(ctx, t, "init", gitcli.CommitOptions{AllowEmpty: true})

	other := testsupport.NewRepo(ctx, t, testsupport.Options{
		UserName:  "Test",
		UserEmail: "test@test.com",
	})
	other.Commit(ctx, t, "init", gitcli.CommitOptions{AllowEmpty: true})

	offline := dest.Git.WithNoLazyFetch()
	err := verifySyncCheckout(ctx, offline, other.Dir)
	if err == nil {
		t.Fatal("expected error for different repository")
	}
	if !strings.Contains(err.Error(), "does not match destination repository root") {
		t.Fatalf("error %q does not explain the two-repo refusal", err)
	}
}

// TestVerifySyncCheckoutNestedRepo verifies that a nested Git repository
// inside the destination is refused. A subdirectory that is its own Git
// repository (e.g. a submodule or an independently initialized directory)
// has a different RepositoryRoot even though its path is beneath the
// destination root, so the containment-only check would wrongly accept it.
func TestVerifySyncCheckoutNestedRepo(t *testing.T) {
	ctx := t.Context()
	dest := testsupport.NewRepo(ctx, t, testsupport.Options{
		UserName:  "Test",
		UserEmail: "test@test.com",
	})
	dest.Commit(ctx, t, "init", gitcli.CommitOptions{AllowEmpty: true})

	// Create a nested Git repository inside the destination.
	nestedDir := filepath.Join(dest.Dir, "nested")
	if err := os.MkdirAll(nestedDir, 0o750); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}
	nested := testsupport.NewRepo(ctx, t, testsupport.Options{
		Dir:       nestedDir,
		UserName:  "Test",
		UserEmail: "test@test.com",
	})
	nested.Commit(ctx, t, "init", gitcli.CommitOptions{AllowEmpty: true})

	offline := dest.Git.WithNoLazyFetch()
	err := verifySyncCheckout(ctx, offline, nestedDir)
	if err == nil {
		t.Fatal("expected error for nested repository")
	}
	if !strings.Contains(err.Error(), "does not match destination repository root") {
		t.Fatalf("error %q does not explain the nested-repo refusal", err)
	}
}

// --- verifySyncWorkflowWithClient tests ---

// TestVerifySyncWorkflowSuccess verifies the happy path using a real httptest
// server that returns matching repository and workflow responses.
func TestVerifySyncWorkflowSuccess(t *testing.T) {
	stub := newGitHubStub(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/enj/rbac_authorizer":
			writeJSON(w, ghapi.Repository{DefaultBranch: "main"})
		case "/repos/enj/rbac_authorizer/actions/workflows/sync.yml":
			writeJSON(w, ghapi.Workflow{State: ghapi.WorkflowActive, Path: syncWorkflowPath})
		default:
			http.NotFound(w, r)
		}
	})
	client := stub.client(t)

	if err := verifySyncWorkflowWithClient(t.Context(), client, testProfileConfig()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify the bearer header was sent.
	for _, req := range stub.requests {
		auth := req.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			t.Errorf("request to %s has Authorization %q, want Bearer prefix", req.Path, auth)
		}
	}
}

// TestVerifySyncWorkflowDefaultBranchMismatch verifies that a repository whose
// default branch does not match the profile is refused.
func TestVerifySyncWorkflowDefaultBranchMismatch(t *testing.T) {
	stub := newGitHubStub(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/enj/rbac_authorizer":
			writeJSON(w, ghapi.Repository{DefaultBranch: "develop"})
		default:
			http.NotFound(w, r)
		}
	})
	client := stub.client(t)

	err := verifySyncWorkflowWithClient(t.Context(), client, testProfileConfig())
	if err == nil {
		t.Fatal("expected error for branch mismatch")
	}
	if !strings.Contains(err.Error(), "default branch") {
		t.Fatalf("error %q does not explain branch mismatch", err)
	}
}

// TestVerifySyncWorkflowDisabled verifies that a disabled workflow is refused.
func TestVerifySyncWorkflowDisabled(t *testing.T) {
	stub := newGitHubStub(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/enj/rbac_authorizer":
			writeJSON(w, ghapi.Repository{DefaultBranch: "main"})
		case "/repos/enj/rbac_authorizer/actions/workflows/sync.yml":
			writeJSON(w, ghapi.Workflow{State: ghapi.WorkflowDisabledInactivity, Path: syncWorkflowPath})
		default:
			http.NotFound(w, r)
		}
	})
	client := stub.client(t)

	err := verifySyncWorkflowWithClient(t.Context(), client, testProfileConfig())
	if err == nil {
		t.Fatal("expected error for disabled workflow")
	}
	if !strings.Contains(err.Error(), ghapi.WorkflowDisabledInactivity) {
		t.Fatalf("error %q does not name the disabled state", err)
	}
}

// TestVerifySyncWorkflowPathMismatch verifies that a workflow at an unexpected
// path is refused.
func TestVerifySyncWorkflowPathMismatch(t *testing.T) {
	stub := newGitHubStub(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/enj/rbac_authorizer":
			writeJSON(w, ghapi.Repository{DefaultBranch: "main"})
		case "/repos/enj/rbac_authorizer/actions/workflows/sync.yml":
			writeJSON(w, ghapi.Workflow{State: ghapi.WorkflowActive, Path: ".github/workflows/other/sync.yml"})
		default:
			http.NotFound(w, r)
		}
	})
	client := stub.client(t)

	err := verifySyncWorkflowWithClient(t.Context(), client, testProfileConfig())
	if err == nil {
		t.Fatal("expected error for path mismatch")
	}
	if !strings.Contains(err.Error(), "workflow path") {
		t.Fatalf("error %q does not explain path mismatch", err)
	}
}

// TestVerifySyncWorkflowBearerHeader verifies that the bearer token is sent
// as the Authorization header on every request.
func TestVerifySyncWorkflowBearerHeader(t *testing.T) {
	stub := newGitHubStub(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/enj/rbac_authorizer":
			writeJSON(w, ghapi.Repository{DefaultBranch: "main"})
		case "/repos/enj/rbac_authorizer/actions/workflows/sync.yml":
			writeJSON(w, ghapi.Workflow{State: ghapi.WorkflowActive, Path: syncWorkflowPath})
		default:
			http.NotFound(w, r)
		}
	})
	client := stub.client(t)
	_ = verifySyncWorkflowWithClient(t.Context(), client, testProfileConfig())

	if len(stub.requests) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(stub.requests))
	}
	for _, req := range stub.requests {
		auth := req.Header.Get("Authorization")
		if auth != "Bearer "+testToken {
			t.Errorf("request to %s: Authorization = %q, want %q", req.Path, auth, "Bearer "+testToken)
		}
	}
}

// --- test helpers ---

// githubStub is a recording httptest server for GitHub API tests.
type githubStub struct {
	server   *httptest.Server
	requests []recorded
}

// recorded is one request as the server received it.
type recorded struct {
	Path   string
	Header http.Header
}

// newGitHubStub starts a recording server.
func newGitHubStub(t *testing.T, handler http.HandlerFunc) *githubStub {
	t.Helper()
	stub := &githubStub{}
	stub.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stub.requests = append(stub.requests, recorded{
			Path:   r.URL.Path,
			Header: r.Header.Clone(),
		})
		handler(w, r)
	}))
	t.Cleanup(stub.server.Close)
	return stub
}

// client builds a ghapi.Client pointed at the stub server with a real
// StaticBearer authorizer.
func (s *githubStub) client(t *testing.T) *ghapi.Client {
	t.Helper()
	auth, err := ghapi.NewStaticBearer(testToken)
	if err != nil {
		t.Fatalf("NewStaticBearer: %v", err)
	}
	client, err := ghapi.New(ghapi.Config{
		Authorizer:             auth,
		BaseURL:                s.server.URL,
		AllowPlaintextLoopback: true,
	})
	if err != nil {
		t.Fatalf("ghapi.New: %v", err)
	}
	return client
}

// writeJSON encodes v as JSON into the response.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
