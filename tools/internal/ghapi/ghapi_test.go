package ghapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/enj/soapbox/tools/internal/ghapi"
)

// testToken stands in for a live workflow token. No URL, header other than
// Authorization, or error message this package produces may contain it.
const testToken = "ghs_notarealworkflowtoken000000000"

// recorded is one request as the server received it.
type recorded struct {
	Method   string
	Path     string
	RawQuery string
	Header   http.Header
	Body     []byte
}

// api is a GitHub stand-in that records every request it answers.
type api struct {
	server *httptest.Server

	mu       sync.Mutex
	requests []recorded
}

// newAPI starts a recording server that answers with handler.
func newAPI(t *testing.T, handler http.HandlerFunc) *api {
	t.Helper()
	stub := &api{}
	stub.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		stub.mu.Lock()
		stub.requests = append(stub.requests, recorded{
			Method:   r.Method,
			Path:     r.URL.EscapedPath(),
			RawQuery: r.URL.RawQuery,
			Header:   r.Header.Clone(),
			Body:     body,
		})
		stub.mu.Unlock()
		handler(w, r)
	}))
	t.Cleanup(stub.server.Close)
	return stub
}

// recordedRequests reports every request the server answered.
func (a *api) recordedRequests() []recorded {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]recorded(nil), a.requests...)
}

// only reports the single request the server was expected to answer.
func (a *api) only(t *testing.T) recorded {
	t.Helper()
	got := a.recordedRequests()
	if len(got) != 1 {
		t.Fatalf("server answered %d requests, want 1", len(got))
	}
	return got[0]
}

// client builds a client pointed at the stand-in.
func (a *api) client(t *testing.T, adjust func(*ghapi.Config)) *ghapi.Client {
	t.Helper()
	cfg := ghapi.Config{
		Authorizer: ghapi.AuthorizerFunc(func(context.Context) (string, error) {
			return "Bearer " + testToken, nil
		}),
		BaseURL:                a.server.URL,
		AllowPlaintextLoopback: true,
	}
	if adjust != nil {
		adjust(&cfg)
	}
	client, err := ghapi.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client
}

// writeJSON answers with a status and a JSON body.
func writeJSON(t *testing.T, w http.ResponseWriter, status int, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Errorf("write response: %v", err)
	}
}

func TestNewRequiresAnAuthorizer(t *testing.T) {
	t.Parallel()

	if _, err := ghapi.New(ghapi.Config{}); err == nil {
		t.Fatal("New with no authorizer succeeded, want an error")
	}
}

// TestNewBoundsTheResponseLimit covers the arithmetic behind the bound. The read
// is taken one byte past the limit so a body that exactly fills it stays
// distinguishable from one that overflows it, and that addition must not wrap:
// io.LimitReader reads a negative limit as no bytes at all, which would turn
// every response into an empty one.
func TestNewBoundsTheResponseLimit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		limit           int64
		wantErrContains string
	}{
		{name: "the default is used for zero", limit: 0},
		{name: "a modest bound is accepted", limit: 1024},
		{name: "the largest safe bound is accepted", limit: math.MaxInt64 - 1},
		{name: "a negative bound is refused", limit: -1, wantErrContains: "must not be negative"},
		{name: "the maximum int64 would overflow the read", limit: math.MaxInt64, wantErrContains: "must not exceed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := ghapi.New(ghapi.Config{
				Authorizer:       ghapi.AuthorizerFunc(func(context.Context) (string, error) { return "Bearer x", nil }),
				MaxResponseBytes: test.limit,
			})
			if test.wantErrContains == "" {
				if err != nil {
					t.Fatalf("New with limit %d: %v", test.limit, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("New with limit %d succeeded, want an error", test.limit)
			}
			if !strings.Contains(err.Error(), test.wantErrContains) {
				t.Fatalf("error %q does not contain %q", err, test.wantErrContains)
			}
		})
	}
}

// TestLabelsThatTheQueryCannotExpressAreRefused covers the one place a caller's
// value is joined into a list rather than escaped into a field of its own.
// GitHub's labels parameter is comma separated, so a label holding a comma
// would arrive as two labels and the listing would answer a different question
// than the one asked, quietly and with a plausible looking result.
func TestLabelsThatTheQueryCannotExpressAreRefused(t *testing.T) {
	t.Parallel()

	stub := newAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the server was reached with an unrepresentable label")
		writeJSON(t, w, http.StatusOK, []map[string]any{})
	})
	client := stub.client(t, nil)

	tests := []struct {
		name            string
		label           string
		wantErrContains string
	}{
		{name: "a comma", label: "needs,triage", wantErrContains: "must not contain a comma"},
		{name: "a leading space", label: " soapbox", wantErrContains: "surrounded by whitespace"},
		{name: "a trailing space", label: "soapbox ", wantErrContains: "surrounded by whitespace"},
		{name: "an empty label", label: "", wantErrContains: "must not be empty"},
		{name: "a newline", label: "soapbox\nsync", wantErrContains: "control characters"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := client.ListOpenIssues(t.Context(), "enj", "rbac_authorizer", []string{test.label})
			if err == nil {
				t.Fatalf("label %q was accepted, want an error", test.label)
			}
			if !strings.Contains(err.Error(), test.wantErrContains) {
				t.Fatalf("error %q does not contain %q", err, test.wantErrContains)
			}
		})
	}
	if requests := stub.recordedRequests(); len(requests) != 0 {
		t.Fatalf("server answered %d requests, want none", len(requests))
	}
}

// TestThrottlingIsRecognisedWithoutAJSONBody covers the refusal shape that has
// no message to read: an edge throttle can arrive as HTML or as nothing at all,
// and the Retry-After header is then the only evidence that the request was
// throttled rather than denied.
func TestThrottlingIsRecognisedWithoutAJSONBody(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		status     int
		retryAfter string
		body       string
		wantDelay  time.Duration
	}{
		{
			name:       "an html body with a date deadline",
			status:     http.StatusForbidden,
			retryAfter: now.Add(90 * time.Second).Format(http.TimeFormat),
			body:       "<html><body>Forbidden</body></html>",
			wantDelay:  90 * time.Second,
		},
		{
			name:       "an empty body with a date deadline",
			status:     http.StatusForbidden,
			retryAfter: now.Add(2 * time.Minute).Format(http.TimeFormat),
			wantDelay:  2 * time.Minute,
		},
		{
			name:       "a deadline already past asks for an immediate retry",
			status:     http.StatusForbidden,
			retryAfter: now.Add(-time.Hour).Format(http.TimeFormat),
			body:       "<html>nope</html>",
		},
		{
			name:       "too many requests with a date deadline",
			status:     http.StatusTooManyRequests,
			retryAfter: now.Add(30 * time.Second).Format(http.TimeFormat),
			wantDelay:  30 * time.Second,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			stub := newAPI(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", test.retryAfter)
				w.Header().Set("Content-Type", "text/html")
				w.WriteHeader(test.status)
				if test.body != "" {
					if _, err := io.WriteString(w, test.body); err != nil {
						t.Errorf("write body: %v", err)
					}
				}
			})
			client := stub.client(t, func(cfg *ghapi.Config) {
				cfg.Clock = func() time.Time { return now }
			})

			_, err := client.Repository(t.Context(), "enj", "rbac_authorizer")
			if !errors.Is(err, ghapi.ErrRateLimited) {
				t.Fatalf("error = %v, want ErrRateLimited", err)
			}
			if errors.Is(err, ghapi.ErrForbidden) {
				t.Fatal("a throttle must not also read as a plain refusal")
			}
			var failure *ghapi.StatusError
			if !errors.As(err, &failure) {
				t.Fatalf("error %q is not a *StatusError", err)
			}
			delay, ok := failure.RetryAfter()
			if !ok {
				t.Fatal("no retry delay was reported")
			}
			if delay != test.wantDelay {
				t.Fatalf("retry after = %s, want %s", delay, test.wantDelay)
			}
			if strings.Contains(err.Error(), testToken) {
				t.Fatal("the refusal carries the credential")
			}
		})
	}
}

func TestRequestCarriesTheFixedHeadersAndNoCredentialInTheURL(t *testing.T) {
	t.Parallel()

	stub := newAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, ghapi.Repository{FullName: "enj/rbac_authorizer", DefaultBranch: "main"})
	})
	client := stub.client(t, nil)

	if _, err := client.Repository(t.Context(), "enj", "rbac_authorizer"); err != nil {
		t.Fatalf("Repository: %v", err)
	}

	got := stub.only(t)
	if got.Method != http.MethodGet {
		t.Errorf("method = %q, want GET", got.Method)
	}
	if got.Path != "/repos/enj/rbac_authorizer" {
		t.Errorf("path = %q, want /repos/enj/rbac_authorizer", got.Path)
	}
	if got.RawQuery != "" {
		t.Errorf("query = %q, want none", got.RawQuery)
	}
	fixed := map[string]string{
		"Authorization":        "Bearer " + testToken,
		"Accept":               "application/vnd.github+json",
		"X-Github-Api-Version": "2022-11-28",
		"User-Agent":           ghapi.DefaultUserAgent,
	}
	for name, want := range fixed {
		if value := got.Header.Get(name); value != want {
			t.Errorf("header %s = %q, want %q", name, value, want)
		}
	}
	// The credential belongs in exactly one header and nowhere else.
	for name, values := range got.Header {
		if name == "Authorization" {
			continue
		}
		for _, value := range values {
			if strings.Contains(value, testToken) {
				t.Errorf("header %s carries the credential", name)
			}
		}
	}
	if strings.Contains(got.Path+got.RawQuery, testToken) {
		t.Error("the request line carries the credential")
	}
}

func TestRepositoryReadsTheDefaultBranch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		body            any
		want            string
		wantErrContains string
	}{
		{name: "a branch is reported", body: map[string]any{"default_branch": "master"}, want: "master"},
		{name: "no branch is refused", body: map[string]any{"full_name": "enj/x"}, wantErrContains: "no default branch"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			stub := newAPI(t, func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, http.StatusOK, test.body)
			})
			got, err := stub.client(t, nil).DefaultBranch(t.Context(), "enj", "rbac_authorizer")
			if test.wantErrContains != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErrContains) {
					t.Fatalf("error = %v, want one containing %q", err, test.wantErrContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("DefaultBranch: %v", err)
			}
			if got != test.want {
				t.Fatalf("default branch = %q, want %q", got, test.want)
			}
		})
	}
}

func TestWorkflowReportsWhetherItIsStillEnabled(t *testing.T) {
	t.Parallel()

	tests := []struct {
		state string
		want  bool
	}{
		{state: ghapi.WorkflowActive, want: true},
		{state: ghapi.WorkflowDisabledManually},
		{state: ghapi.WorkflowDisabledInactivity},
		{state: ghapi.WorkflowDisabledFork},
	}
	for _, test := range tests {
		t.Run(test.state, func(t *testing.T) {
			t.Parallel()

			stub := newAPI(t, func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, http.StatusOK, ghapi.Workflow{ID: 7, Name: "sync", Path: ".github/workflows/sync.yml", State: test.state})
			})
			workflow, err := stub.client(t, nil).Workflow(t.Context(), "enj", "rbac_authorizer", "sync.yml")
			if err != nil {
				t.Fatalf("Workflow: %v", err)
			}
			if workflow.Enabled() != test.want {
				t.Fatalf("Enabled() = %t for state %q, want %t", workflow.Enabled(), test.state, test.want)
			}
			if path := stub.only(t).Path; path != "/repos/enj/rbac_authorizer/actions/workflows/sync.yml" {
				t.Fatalf("path = %q", path)
			}
		})
	}
}

func TestListOpenIssuesDropsPullRequestsAndPaginates(t *testing.T) {
	t.Parallel()

	// The first page is full, so a second is requested; the last entry of the
	// first page is a pull request and must not survive.
	first := make([]map[string]any, 0, 100)
	for i := 1; i < 100; i++ {
		first = append(first, map[string]any{"number": i, "title": fmt.Sprintf("issue %d", i)})
	}
	first = append(first, map[string]any{"number": 100, "title": "a pull request", "pull_request": map[string]any{"html_url": "https://example.invalid/pr"}})

	stub := newAPI(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("page") {
		case "1":
			writeJSON(t, w, http.StatusOK, first)
		default:
			writeJSON(t, w, http.StatusOK, []map[string]any{{"number": 101, "title": "issue 101"}})
		}
	})

	issues, err := stub.client(t, nil).ListOpenIssues(t.Context(), "enj", "rbac_authorizer", []string{"soapbox", "sync"})
	if err != nil {
		t.Fatalf("ListOpenIssues: %v", err)
	}
	if len(issues) != 100 {
		t.Fatalf("got %d issues, want 100 with the pull request dropped", len(issues))
	}
	for _, issue := range issues {
		if issue.PullRequest != nil {
			t.Fatalf("issue %d is a pull request", issue.Number)
		}
	}
	requests := stub.recordedRequests()
	if len(requests) != 2 {
		t.Fatalf("made %d requests, want 2", len(requests))
	}
	if want := "labels=soapbox%2Csync&page=1&per_page=100&state=open"; requests[0].RawQuery != want {
		t.Fatalf("first query = %q, want %q", requests[0].RawQuery, want)
	}
}

func TestIssueWritesSendTheirBodies(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		call       func(*ghapi.Client) error
		wantMethod string
		wantPath   string
		wantBody   string
	}{
		{
			name: "create",
			call: func(c *ghapi.Client) error {
				_, err := c.CreateIssue(t.Context(), "enj", "rbac_authorizer", ghapi.IssueRequest{
					Title: "sync failed", Body: "details", Labels: []string{"soapbox"},
				})
				return err
			},
			wantMethod: http.MethodPost,
			wantPath:   "/repos/enj/rbac_authorizer/issues",
			wantBody:   `{"title":"sync failed","body":"details","labels":["soapbox"]}`,
		},
		{
			name: "update",
			call: func(c *ghapi.Client) error {
				_, err := c.UpdateIssue(t.Context(), "enj", "rbac_authorizer", 12, ghapi.IssueUpdate{State: "closed"})
				return err
			},
			wantMethod: http.MethodPatch,
			wantPath:   "/repos/enj/rbac_authorizer/issues/12",
			wantBody:   `{"state":"closed"}`,
		},
		{
			name: "comment",
			call: func(c *ghapi.Client) error {
				_, err := c.CreateIssueComment(t.Context(), "enj", "rbac_authorizer", 12, "another failure")
				return err
			},
			wantMethod: http.MethodPost,
			wantPath:   "/repos/enj/rbac_authorizer/issues/12/comments",
			wantBody:   `{"body":"another failure"}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			stub := newAPI(t, func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, http.StatusCreated, map[string]any{"number": 12, "id": 34})
			})
			if err := test.call(stub.client(t, nil)); err != nil {
				t.Fatalf("%s: %v", test.name, err)
			}
			got := stub.only(t)
			if got.Method != test.wantMethod {
				t.Errorf("method = %q, want %q", got.Method, test.wantMethod)
			}
			if got.Path != test.wantPath {
				t.Errorf("path = %q, want %q", got.Path, test.wantPath)
			}
			if body := strings.TrimSpace(string(got.Body)); body != test.wantBody {
				t.Errorf("body = %s, want %s", body, test.wantBody)
			}
		})
	}
}

func TestCallerSuppliedNamesAreValidatedBeforeAnyRequest(t *testing.T) {
	t.Parallel()

	stub := newAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the server was reached with an invalid name")
		writeJSON(t, w, http.StatusOK, map[string]any{})
	})
	client := stub.client(t, nil)

	tests := []struct {
		name  string
		call  func() error
		field string
	}{
		{
			name:  "an owner with a separator",
			call:  func() error { _, err := client.Repository(t.Context(), "enj/evil", "r"); return err },
			field: "owner",
		},
		{
			name:  "a repository that traverses",
			call:  func() error { _, err := client.Repository(t.Context(), "enj", ".."); return err },
			field: "repository",
		},
		{
			name:  "an empty owner",
			call:  func() error { _, err := client.Repository(t.Context(), "", "r"); return err },
			field: "owner",
		},
		{
			name:  "a workflow file with a slash",
			call:  func() error { _, err := client.Workflow(t.Context(), "enj", "r", "a/b.yml"); return err },
			field: "workflow file",
		},
		{
			name: "a negative issue number",
			call: func() error {
				_, err := client.UpdateIssue(t.Context(), "enj", "r", -1, ghapi.IssueUpdate{State: "closed"})
				return err
			},
			field: "issue number",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.call()
			if err == nil {
				t.Fatalf("%s was accepted, want an error", test.name)
			}
			if !strings.Contains(err.Error(), test.field) {
				t.Fatalf("error %q does not name %q", err, test.field)
			}
		})
	}
	if requests := stub.recordedRequests(); len(requests) != 0 {
		t.Fatalf("server answered %d requests, want none", len(requests))
	}
}

func TestStatusRefusalsAreTyped(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		status          int
		header          map[string]string
		body            any
		wantIs          error
		wantNotIs       []error
		wantMsgContains string
		wantRetryAfter  time.Duration
	}{
		{
			name:            "a bad credential is unauthorized",
			status:          http.StatusUnauthorized,
			body:            map[string]string{"message": "Bad credentials"},
			wantIs:          ghapi.ErrUnauthorized,
			wantMsgContains: "Bad credentials",
		},
		{
			name:            "a missing permission is forbidden",
			status:          http.StatusForbidden,
			body:            map[string]string{"message": "Resource not accessible by integration"},
			wantIs:          ghapi.ErrForbidden,
			wantNotIs:       []error{ghapi.ErrRateLimited},
			wantMsgContains: "not accessible by integration",
		},
		{
			name:      "an uninstalled app is not found",
			status:    http.StatusNotFound,
			body:      map[string]string{"message": "Not Found"},
			wantIs:    ghapi.ErrNotFound,
			wantNotIs: []error{ghapi.ErrForbidden},
		},
		{
			name:      "a primary rate limit is a throttle",
			status:    http.StatusForbidden,
			header:    map[string]string{"X-RateLimit-Limit": "5000", "X-RateLimit-Remaining": "0", "X-RateLimit-Reset": "1786000000"},
			body:      map[string]string{"message": "API rate limit exceeded"},
			wantIs:    ghapi.ErrRateLimited,
			wantNotIs: []error{ghapi.ErrForbidden},
		},
		{
			name:           "a secondary rate limit carries a retry delay",
			status:         http.StatusTooManyRequests,
			header:         map[string]string{"Retry-After": "60"},
			body:           map[string]string{"message": "You have exceeded a secondary rate limit"},
			wantIs:         ghapi.ErrRateLimited,
			wantRetryAfter: time.Minute,
		},
		{
			name:   "an unparseable body still reports the status",
			status: http.StatusBadGateway,
			body:   "<html>gateway</html>",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			stub := newAPI(t, func(w http.ResponseWriter, _ *http.Request) {
				for name, value := range test.header {
					w.Header().Set(name, value)
				}
				w.WriteHeader(test.status)
				if text, ok := test.body.(string); ok {
					if _, err := io.WriteString(w, text); err != nil {
						t.Errorf("write body: %v", err)
					}
					return
				}
				if err := json.NewEncoder(w).Encode(test.body); err != nil {
					t.Errorf("write body: %v", err)
				}
			})

			_, err := stub.client(t, nil).Repository(t.Context(), "enj", "rbac_authorizer")
			if err == nil {
				t.Fatal("the refusal was reported as success")
			}
			if test.wantIs != nil && !errors.Is(err, test.wantIs) {
				t.Errorf("error %q is not %v", err, test.wantIs)
			}
			for _, not := range test.wantNotIs {
				if errors.Is(err, not) {
					t.Errorf("error %q must not be %v", err, not)
				}
			}
			if test.wantMsgContains != "" && !strings.Contains(err.Error(), test.wantMsgContains) {
				t.Errorf("error %q does not contain %q", err, test.wantMsgContains)
			}

			var failure *ghapi.StatusError
			if !errors.As(err, &failure) {
				t.Fatalf("error %q is not a *StatusError", err)
			}
			if failure.Status != test.status {
				t.Errorf("status = %d, want %d", failure.Status, test.status)
			}
			if failure.Path != "/repos/enj/rbac_authorizer" {
				t.Errorf("path = %q", failure.Path)
			}
			delay, ok := failure.RetryAfter()
			if test.wantRetryAfter != 0 {
				if !ok || delay != test.wantRetryAfter {
					t.Errorf("retry after = %s (%t), want %s", delay, ok, test.wantRetryAfter)
				}
			} else if ok {
				t.Errorf("retry after = %s, want none", delay)
			}
			if strings.Contains(err.Error(), testToken) {
				t.Error("the refusal carries the credential")
			}
		})
	}
}

func TestRateLimitCountersAreReported(t *testing.T) {
	t.Parallel()

	stub := newAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Used", "5000")
		w.Header().Set("X-RateLimit-Reset", "1786000000")
		w.Header().Set("X-RateLimit-Resource", "core")
		writeJSON(t, w, http.StatusForbidden, map[string]string{"message": "API rate limit exceeded"})
	})

	_, err := stub.client(t, nil).Repository(t.Context(), "enj", "rbac_authorizer")
	var failure *ghapi.StatusError
	if !errors.As(err, &failure) {
		t.Fatalf("error %v is not a *StatusError", err)
	}
	if failure.RateLimit == nil {
		t.Fatal("no rate limit metadata was reported")
	}
	want := ghapi.RateLimit{
		Limit: 5000, Remaining: 0, Used: 5000, Resource: "core",
		Reset: time.Unix(1786000000, 0).UTC(),
	}
	if *failure.RateLimit != want {
		t.Fatalf("rate limit = %+v, want %+v", *failure.RateLimit, want)
	}
}

func TestCrossOriginRedirectIsRefused(t *testing.T) {
	t.Parallel()

	var elsewhereCalls int
	elsewhere := newAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		elsewhereCalls++
		writeJSON(t, w, http.StatusOK, map[string]any{"full_name": "attacker/repo"})
	})
	stub := newAPI(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.server.URL+"/repos/enj/rbac_authorizer", http.StatusMovedPermanently)
	})

	_, err := stub.client(t, nil).Repository(t.Context(), "enj", "rbac_authorizer")
	if !errors.Is(err, ghapi.ErrRedirectRefused) {
		t.Fatalf("error = %v, want ErrRedirectRefused", err)
	}
	if elsewhereCalls != 0 {
		t.Fatalf("the other origin was reached %d times", elsewhereCalls)
	}
	if len(elsewhere.recordedRequests()) != 0 {
		t.Fatal("the other origin recorded a request")
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("error %q carries the credential", err)
	}
}

func TestSameOriginRedirectIsFollowedWithTheCredential(t *testing.T) {
	t.Parallel()

	stub := newAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/old_name") {
			http.Redirect(w, r, "/repos/enj/rbac_authorizer", http.StatusMovedPermanently)
			return
		}
		writeJSON(t, w, http.StatusOK, map[string]any{"full_name": "enj/rbac_authorizer", "default_branch": "main"})
	})

	repository, err := stub.client(t, nil).Repository(t.Context(), "enj", "old_name")
	if err != nil {
		t.Fatalf("Repository: %v", err)
	}
	if repository.FullName != "enj/rbac_authorizer" {
		t.Fatalf("full name = %q, want the renamed repository", repository.FullName)
	}
	requests := stub.recordedRequests()
	if len(requests) != 2 {
		t.Fatalf("made %d requests, want 2", len(requests))
	}
	if got := requests[1].Header.Get("Authorization"); got != "Bearer "+testToken {
		t.Fatalf("the followed request presented %q", got)
	}
}

func TestResponseBoundIsEnforced(t *testing.T) {
	t.Parallel()

	stub := newAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, map[string]any{"default_branch": strings.Repeat("b", 4096)})
	})
	client := stub.client(t, func(cfg *ghapi.Config) { cfg.MaxResponseBytes = 256 })

	_, err := client.Repository(t.Context(), "enj", "rbac_authorizer")
	if !errors.Is(err, ghapi.ErrResponseTooLarge) {
		t.Fatalf("error = %v, want ErrResponseTooLarge", err)
	}
	if !strings.Contains(err.Error(), "256") {
		t.Fatalf("error %q does not report the bound", err)
	}
}

func TestCancellationIsReported(t *testing.T) {
	t.Parallel()

	t.Run("before the request", func(t *testing.T) {
		t.Parallel()

		stub := newAPI(t, func(w http.ResponseWriter, _ *http.Request) {
			t.Error("the server was reached with a cancelled context")
			writeJSON(t, w, http.StatusOK, map[string]any{})
		})
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		_, err := stub.client(t, nil).Repository(ctx, "enj", "rbac_authorizer")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
		if requests := stub.recordedRequests(); len(requests) != 0 {
			t.Fatalf("server answered %d requests, want none", len(requests))
		}
	})

	t.Run("during the request", func(t *testing.T) {
		t.Parallel()

		release := make(chan struct{})
		ctx, cancel := context.WithCancel(t.Context())
		stub := newAPI(t, func(w http.ResponseWriter, _ *http.Request) {
			cancel()
			<-release
			writeJSON(t, w, http.StatusOK, map[string]any{})
		})
		defer close(release)

		_, err := stub.client(t, nil).Repository(ctx, "enj", "rbac_authorizer")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
		if strings.Contains(err.Error(), stub.server.URL) {
			t.Fatalf("error %q echoes the request URL", err)
		}
	})
}

func TestAuthorizerFailureStopsTheRequest(t *testing.T) {
	t.Parallel()

	stub := newAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the server was reached without a credential")
		writeJSON(t, w, http.StatusOK, map[string]any{})
	})
	refusal := errors.New("no workflow token")
	client := stub.client(t, func(cfg *ghapi.Config) {
		cfg.Authorizer = ghapi.AuthorizerFunc(func(context.Context) (string, error) { return "", refusal })
	})

	_, err := client.Repository(t.Context(), "enj", "rbac_authorizer")
	if !errors.Is(err, refusal) {
		t.Fatalf("error = %v, want the authorizer's refusal", err)
	}
	if requests := stub.recordedRequests(); len(requests) != 0 {
		t.Fatalf("server answered %d requests, want none", len(requests))
	}
}
