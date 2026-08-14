package config

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
)

// gitHost is the only remote host the engine talks to. Source and destination
// URLs are allowlisted so a profile cannot redirect authenticated pushes or
// unauthenticated clones to an attacker controlled host.
const gitHost = "github.com"

// redactedURL replaces user information, or a whole URL that cannot be rendered
// safely, in every diagnostic. It carries no characters that URL encoding would
// escape, so a redacted URL stays readable.
const redactedURL = "redacted"

// urlRule constrains one configured URL field.
type urlRule struct {
	allowedHosts []string
	query        string
	suffix       string
}

// validateURL checks scheme, credentials, host, port, query, and fragment.
// Every message renders the URL through safeURL, because a value that carries a
// token must never reach a log, a report, or a CI annotation.
func validateURL(raw string, rule urlRule) error {
	if raw == "" {
		return errors.New("URL must not be empty")
	}
	if strings.TrimSpace(raw) != raw {
		return errors.New("URL must not have leading or trailing space")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("URL %q is malformed: %s", safeURL(raw), urlErrorReason(err))
	}
	if parsed.Scheme != "https" {
		return fmt.Errorf("URL %q must use https", safeURL(raw))
	}
	if parsed.User != nil {
		return fmt.Errorf("URL %q must not embed credentials", safeURL(raw))
	}
	if parsed.Port() != "" {
		return fmt.Errorf("URL %q must not set an explicit port", safeURL(raw))
	}
	if parsed.Fragment != "" || parsed.RawFragment != "" {
		return fmt.Errorf("URL %q must not carry a fragment", safeURL(raw))
	}
	host := parsed.Hostname()
	if host == "" {
		return fmt.Errorf("URL %q must name a host", safeURL(raw))
	}
	if !slices.Contains(rule.allowedHosts, host) {
		return fmt.Errorf("URL %q uses host %q which is not one of %s", safeURL(raw), host, strings.Join(rule.allowedHosts, ", "))
	}
	if parsed.RawQuery != rule.query {
		if rule.query == "" {
			return fmt.Errorf("URL %q must not carry a query", safeURL(raw))
		}
		return fmt.Errorf("URL %q must carry the query %q", safeURL(raw), rule.query)
	}
	// Both spellings are inspected so a percent encoded parent element cannot
	// slip past the raw comparison.
	for _, path := range []string{parsed.EscapedPath(), parsed.Path} {
		for _, elem := range strings.Split(path, "/") {
			if elem == ".." {
				return fmt.Errorf("URL %q must not traverse parent directories", safeURL(raw))
			}
		}
	}
	if rule.suffix != "" && !strings.HasSuffix(parsed.EscapedPath(), rule.suffix) {
		return fmt.Errorf("URL %q must end with %q", safeURL(raw), rule.suffix)
	}
	return nil
}

// safeURL renders a URL without user information. An unparseable value is
// replaced entirely because it cannot be inspected safely.
func safeURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return redactedURL
	}
	if parsed.User == nil {
		return raw
	}
	parsed.User = url.User(redactedURL)
	return parsed.String()
}

// urlErrorReason reports why a URL failed to parse without echoing the URL,
// which url.Error embeds in its own message.
func urlErrorReason(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		return urlErr.Err.Error()
	}
	return "unparseable"
}

// normalizeURLHost lower cases the scheme and host of a URL so canonical profile
// bytes do not depend on how the host was typed. The transformation is textual
// and touches nothing else, so validation still sees the operator's bytes,
// including any leading or trailing space.
func normalizeURLHost(raw string) string {
	scheme := strings.Index(raw, "://")
	if scheme < 0 {
		return raw
	}
	authorityStart := scheme + len("://")
	authorityEnd := len(raw)
	if i := strings.IndexAny(raw[authorityStart:], "/?#"); i >= 0 {
		authorityEnd = authorityStart + i
	}
	authority := raw[authorityStart:authorityEnd]
	// User information keeps its case; only the host is case insensitive.
	if at := strings.LastIndex(authority, "@"); at >= 0 {
		authority = authority[:at+1] + strings.ToLower(authority[at+1:])
	} else {
		authority = strings.ToLower(authority)
	}
	return strings.ToLower(raw[:scheme]) + "://" + authority + raw[authorityEnd:]
}

// moduleDomain returns the domain element of a module path.
func moduleDomain(modulePath string) string {
	domain, _, _ := strings.Cut(modulePath, "/")
	return domain
}
