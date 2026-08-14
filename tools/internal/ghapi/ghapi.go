// Package ghapi is the typed GitHub REST boundary of the engine.
//
// Every request is assembled from a validated base URL and individually escaped
// path segments, so an owner, a repository name, or a workflow file name can
// never become part of the URL's structure. The credential is supplied by an
// Authorizer and set as one Authorization header on the outgoing request. It
// never travels in a URL, a query string, or an error, and a redirect that
// leaves the configured origin is refused rather than followed, because
// following one would offer the header to a host the engine did not choose.
//
// This package provides the typed HTTP boundary for authenticated GitHub API
// calls. The token is supplied by an Authorizer and set as one Authorization
// header on the outgoing request, so the base URL rules, the redirect refusal,
// the response size bound, and the typed errors all cover every endpoint.
package ghapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/enj/soapbox/tools/internal/buildinfo"
)

// DefaultBaseURL is the REST API root of github.com.
const DefaultBaseURL = "https://api.github.com"

// DefaultUserAgent identifies the engine and its exact version to GitHub, which
// asks every client to send one and answers an anonymous agent with a refusal.
const DefaultUserAgent = "soapbox/" + buildinfo.Version

// DefaultMaxResponseBytes bounds a decoded response body.
//
// Every response this package reads is a small JSON document, and the largest
// of them is one page of repositories. The bound exists so a destination that
// answers with an unbounded stream cannot exhaust memory before it is refused.
const DefaultMaxResponseBytes = 4 << 20

// DefaultHTTPTimeout bounds one REST call when the caller supplies no client.
//
// A client with no timeout waits forever on a destination that accepts the
// connection and then says nothing, which in a scheduled publishing run is a
// job that never ends rather than one that fails.
const DefaultHTTPTimeout = 30 * time.Second

// maxResponseBytesLimit is the largest bound this package accepts.
//
// The read is bounded at one byte past the limit so a body that exactly fills
// it stays distinguishable from one that overflows it, and that addition must
// not wrap. Anything at or above this is refused rather than silently turned
// into a negative limit, which io.LimitReader reads as no bytes at all.
const maxResponseBytesLimit = math.MaxInt64 - 1

// The fixed representation every request asks for. The API version is pinned so
// a future default cannot change the shape of a decoded field without a
// deliberate edit here.
const (
	acceptHeader     = "application/vnd.github+json"
	apiVersionHeader = "X-GitHub-Api-Version"
	apiVersion       = "2022-11-28"
)

// maxRedirects bounds a redirect chain that stays on the configured origin.
// GitHub redirects a renamed repository once; a chain longer than this is a
// loop rather than a rename.
const maxRedirects = 5

// Authorizer supplies the Authorization header value for one request.
//
// It is an interface rather than a token string so the client remains ignorant
// of how a credential is sourced and tests can fail a request before transport.
// The production GITHUB_TOKEN implementation is static for one workflow job.
type Authorizer interface {
	AuthorizationHeader(ctx context.Context) (string, error)
}

// AuthorizerFunc adapts a function to Authorizer.
type AuthorizerFunc func(ctx context.Context) (string, error)

// AuthorizationHeader calls f.
func (f AuthorizerFunc) AuthorizationHeader(ctx context.Context) (string, error) { return f(ctx) }

// Config describes one GitHub REST client.
type Config struct {
	// Authorizer supplies the credential presented on every request. It is
	// required because an anonymous client could silently read a public subset
	// instead of the authenticated repository view the caller intends.
	Authorizer Authorizer

	// BaseURL is the REST API root. It defaults to DefaultBaseURL and must be an
	// https URL with no user information, query, or fragment.
	BaseURL string

	// HTTPClient is the transport. It defaults to a client with a request
	// timeout. Whatever is supplied is copied before its redirect policy and
	// cookie jar are replaced, so the caller's client is never mutated.
	HTTPClient *http.Client

	// UserAgent defaults to DefaultUserAgent.
	UserAgent string

	// MaxResponseBytes defaults to DefaultMaxResponseBytes.
	MaxResponseBytes int64

	// Clock reports the current time. It defaults to time.Now and is injected so
	// a Retry-After deadline resolves to an exact delay in tests.
	Clock func() time.Time

	// AllowPlaintextLoopback permits an http base URL that names a loopback
	// address. It exists for httptest servers and nothing else: a plaintext
	// request to any routable host would put the Authorization header on the
	// wire in clear text, so the loopback restriction is enforced rather than
	// documented.
	AllowPlaintextLoopback bool
}

// Client is a typed GitHub REST client bound to one origin and one credential.
type Client struct {
	base       *url.URL
	http       *http.Client
	authorizer Authorizer
	userAgent  string
	maxBytes   int64
	clock      func() time.Time
}

// New builds a client from cfg.
func New(cfg Config) (*Client, error) {
	if cfg.Authorizer == nil {
		return nil, errors.New("github client: an authorizer is required")
	}
	raw := cfg.BaseURL
	if raw == "" {
		raw = DefaultBaseURL
	}
	base, err := parseBaseURL(raw, cfg.AllowPlaintextLoopback)
	if err != nil {
		return nil, fmt.Errorf("github client: %w", err)
	}
	maxBytes := cfg.MaxResponseBytes
	if maxBytes == 0 {
		maxBytes = DefaultMaxResponseBytes
	}
	if maxBytes < 0 {
		return nil, fmt.Errorf("github client: max response bytes %d must not be negative", maxBytes)
	}
	if maxBytes > maxResponseBytesLimit {
		return nil, fmt.Errorf("github client: max response bytes %d must not exceed %d", maxBytes, int64(maxResponseBytesLimit))
	}
	userAgent := cfg.UserAgent
	if userAgent == "" {
		userAgent = DefaultUserAgent
	}
	if strings.ContainsAny(userAgent, "\r\n") {
		return nil, errors.New("github client: the user agent must be one line")
	}
	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}
	return &Client{
		base:       base,
		http:       harden(cfg.HTTPClient, base),
		authorizer: cfg.Authorizer,
		userAgent:  userAgent,
		maxBytes:   maxBytes,
		clock:      clock,
	}, nil
}

// BaseURL reports the origin every request is built against.
func (c *Client) BaseURL() string { return c.base.String() }

// harden copies a client and replaces the two parts of it that decide where a
// credential can travel.
//
// The redirect policy refuses any hop that leaves the configured origin. Go
// already strips the Authorization header when a redirect changes host, but a
// stripped header turns an authorized read into an anonymous one, and an
// anonymous read of a public repository succeeds with the wrong answer rather
// than failing. The cookie jar is dropped because a jar is a second credential
// store this package never asked for. A client the caller supplied is copied
// first, so neither replacement is visible to whoever handed it over, and a
// client this package created for itself is bounded by a timeout.
func harden(client *http.Client, base *url.URL) *http.Client {
	hardened := &http.Client{Timeout: DefaultHTTPTimeout}
	if client != nil {
		*hardened = *client
	}
	hardened.Jar = nil
	origin := canonicalOrigin(base)
	hardened.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if canonicalOrigin(req.URL) != origin {
			return fmt.Errorf("%s: %w", req.URL.Redacted(), ErrRedirectRefused)
		}
		if len(via) >= maxRedirects {
			return fmt.Errorf("%d hops: %w", len(via), ErrTooManyRedirects)
		}
		return nil
	}
	return hardened
}

// canonicalOrigin renders scheme, host, and port in the one form two spellings
// of the same origin share.
//
// A host is case insensitive and an omitted port means the scheme's default, so
// comparing the raw Host fields would call https://API.GitHub.com:443 a
// different origin from https://api.github.com and refuse a redirect that never
// left. Getting this wrong in the other direction is what matters, though: the
// comparison is what keeps the Authorization header from reaching a host the
// engine did not choose, so it normalizes rather than loosens.
func canonicalOrigin(u *url.URL) string {
	scheme := strings.ToLower(u.Scheme)
	port := u.Port()
	if port == "" {
		switch scheme {
		case "https":
			port = "443"
		case "http":
			port = "80"
		}
	}
	return scheme + "://" + strings.ToLower(u.Hostname()) + ":" + port
}

// parseBaseURL validates a REST API root.
//
// The rules keep a credential and a surprise destination out of the one URL
// every request is built from. User information is refused because this client
// authorizes through a header and a URL that carries a password would put it in
// every log that records a request line. A query or a fragment is refused
// because both are silently dropped when path segments are joined, so accepting
// one would mean honouring a caller's intent nowhere.
func parseBaseURL(raw string, allowPlaintextLoopback bool) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("base URL is malformed: %w", redactURLError(err))
	}
	switch {
	case parsed.Opaque != "":
		return nil, fmt.Errorf("base URL %q must not be opaque", parsed.Redacted())
	case parsed.Host == "":
		return nil, fmt.Errorf("base URL %q must name a host", parsed.Redacted())
	case parsed.User != nil:
		return nil, fmt.Errorf("base URL %q must not embed credentials", parsed.Redacted())
	case parsed.RawQuery != "" || parsed.ForceQuery:
		return nil, fmt.Errorf("base URL %q must not carry a query", parsed.Redacted())
	case parsed.Fragment != "":
		return nil, fmt.Errorf("base URL %q must not carry a fragment", parsed.Redacted())
	}
	switch parsed.Scheme {
	case "https":
	case "http":
		if !allowPlaintextLoopback {
			return nil, fmt.Errorf("base URL %q must use https", parsed.Redacted())
		}
		if !isLoopback(parsed.Hostname()) {
			return nil, fmt.Errorf("base URL %q may only use http on a loopback address", parsed.Redacted())
		}
	default:
		return nil, fmt.Errorf("base URL %q must use https", parsed.Redacted())
	}
	// A base path is kept, because GitHub Enterprise Server serves the API under
	// /api/v3, but it is normalized first so the joined path of a request cannot
	// depend on whether the operator wrote a trailing slash.
	if parsed.Path != "" {
		cleaned := path.Clean("/" + strings.TrimSuffix(parsed.EscapedPath(), "/"))
		if cleaned == "/" {
			cleaned = ""
		}
		parsed.RawPath = ""
		parsed.Path = ""
		if cleaned != "" {
			resolved, err := url.Parse(cleaned)
			if err != nil {
				return nil, fmt.Errorf("base URL path is malformed: %w", redactURLError(err))
			}
			parsed.Path, parsed.RawPath = resolved.Path, resolved.RawPath
		}
		// Cleaning works on the escaped path, so a percent encoded traversal
		// survives it and only becomes one again when the path is decoded. The
		// decoded form is what is checked, because that is what a server
		// resolves the request against.
		for segment := range strings.SplitSeq(parsed.Path, "/") {
			if segment == ".." {
				return nil, fmt.Errorf("base URL %q must not traverse", parsed.Redacted())
			}
		}
	}
	return parsed, nil
}

// isLoopback reports whether a host names this machine.
func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}

// resolve builds the absolute URL of one request from escaped path segments.
//
// Each segment is escaped individually and checked first, so a caller supplied
// owner or repository name cannot introduce a path separator, walk up out of
// the API root, or append a query of its own.
func (c *Client) resolve(segments []string, query url.Values) (*url.URL, error) {
	if len(segments) == 0 {
		return nil, errors.New("a request needs a path")
	}
	escaped := make([]string, 0, len(segments))
	for _, segment := range segments {
		switch {
		case segment == "":
			return nil, errors.New("a path segment must not be empty")
		case segment == "." || segment == "..":
			return nil, fmt.Errorf("path segment %q must not traverse", segment)
		case strings.ContainsAny(segment, "/?#"):
			return nil, fmt.Errorf("path segment %q must not contain a URL separator", segment)
		case strings.ContainsFunc(segment, func(r rune) bool { return r < 0x20 || r == 0x7f }):
			return nil, errors.New("a path segment must not contain control characters")
		}
		escaped = append(escaped, url.PathEscape(segment))
	}
	// The path is assembled as text and parsed once rather than joined through
	// URL.JoinPath, which drops the leading slash when the base has no path of
	// its own and would leave the route recorded in an error disagreeing with
	// the route actually requested. Traversal is already refused above, so
	// there is nothing left for a cleaning pass to do.
	reference, err := url.Parse(c.base.EscapedPath() + "/" + strings.Join(escaped, "/"))
	if err != nil {
		return nil, fmt.Errorf("request path is malformed: %w", redactURLError(err))
	}
	target := *c.base
	target.Path, target.RawPath = reference.Path, reference.RawPath
	if query != nil {
		target.RawQuery = query.Encode()
	}
	return &target, nil
}

// request performs one REST call and decodes its JSON response into T.
//
// It is a package level generic function rather than a method because Go does
// not allow a method to introduce a type parameter, and the alternative of
// decoding into an any and asserting would move a compile time guarantee into
// the callers.
func request[T any](ctx context.Context, c *Client, method string, segments []string, query url.Values, body any) (T, error) {
	var decoded T
	if c == nil {
		return decoded, errors.New("github request: no client")
	}
	if err := ctx.Err(); err != nil {
		return decoded, fmt.Errorf("github %s: %w", method, err)
	}
	target, err := c.resolve(segments, query)
	if err != nil {
		return decoded, fmt.Errorf("github %s: %w", method, err)
	}
	// The path is what an error may name. The full URL is not, because a query
	// is caller supplied and an error is the one place a stray value would be
	// written down.
	route := target.EscapedPath()

	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return decoded, fmt.Errorf("github %s %s: encode request: %w", method, route, err)
		}
		payload = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, target.String(), payload)
	if err != nil {
		return decoded, fmt.Errorf("github %s %s: build request: %w", method, route, err)
	}
	credential, err := c.authorizer.AuthorizationHeader(ctx)
	if err != nil {
		return decoded, fmt.Errorf("github %s %s: %w", method, route, err)
	}
	if credential == "" {
		return decoded, fmt.Errorf("github %s %s: the authorizer returned no credential", method, route)
	}
	req.Header.Set("Authorization", credential)
	req.Header.Set("Accept", acceptHeader)
	req.Header.Set(apiVersionHeader, apiVersion)
	req.Header.Set("User-Agent", c.userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return decoded, fmt.Errorf("github %s %s: %w", method, route, redactURLError(err))
	}
	defer resp.Body.Close()

	// One byte past the bound is read so a body that exactly fills it is still
	// distinguishable from one that overflows it.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, c.maxBytes+1))
	if err != nil {
		return decoded, fmt.Errorf("github %s %s: read response: %w", method, route, redactURLError(err))
	}
	if int64(len(raw)) > c.maxBytes {
		return decoded, fmt.Errorf("github %s %s: %w of %d bytes", method, route, ErrResponseTooLarge, c.maxBytes)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return decoded, statusError(method, route, resp, raw, c.clock())
	}
	if resp.StatusCode == http.StatusNoContent || len(bytes.TrimSpace(raw)) == 0 {
		return decoded, nil
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		// The error carries the decoder's complaint but never the server-controlled
		// response bytes, which may contain private repository data.
		return decoded, fmt.Errorf("github %s %s: decode response: %w", method, route, err)
	}
	return decoded, nil
}
