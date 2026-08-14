package ghapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Refusals a caller can branch on with errors.Is.
var (
	// ErrUnauthorized reports that GitHub rejected the credential itself.
	ErrUnauthorized = errors.New("github rejected the credential")

	// ErrForbidden reports that the credential is valid but lacks the required
	// repository or workflow permission.
	ErrForbidden = errors.New("github refused the request")

	// ErrNotFound reports that GitHub has no visible such resource. A resource
	// hidden from the credential can be answered this way rather than with a
	// refusal, so a caller cannot tell the two apart from the status alone.
	ErrNotFound = errors.New("github reports no such resource")

	// ErrRateLimited reports a primary or secondary rate limit. The error also
	// carries the retry metadata GitHub returned.
	ErrRateLimited = errors.New("github rate limited the request")

	// ErrResponseTooLarge reports a response body past the configured bound.
	ErrResponseTooLarge = errors.New("github response exceeds the response limit")

	// ErrRedirectRefused reports a redirect that left the configured origin.
	ErrRedirectRefused = errors.New("github redirected the request off its origin")

	// ErrTooManyRedirects reports a redirect chain that never settled.
	ErrTooManyRedirects = errors.New("github redirected the request too many times")
)

// RateLimit is the retry metadata GitHub attaches to a throttled response.
type RateLimit struct {
	// Limit, Remaining, and Used are the primary rate limit counters. They are
	// zero when GitHub did not report them.
	Limit     int
	Remaining int
	Used      int

	// Resource names the budget the request was charged against, such as core
	// or search.
	Resource string

	// Reset is when the primary budget refills. It is zero when unreported.
	Reset time.Time

	// RetryAfter is how long GitHub asked the client to wait. Secondary limits
	// report this and no counters, so it is the only field a caller can rely on
	// for an abuse refusal. It is clamped at zero for a deadline that has
	// already passed, which means retry now rather than never.
	RetryAfter time.Duration

	// RetryAfterSet reports whether GitHub sent a Retry-After header at all.
	// It is separate from RetryAfter because a delay of zero is a real answer:
	// a header naming a moment in the past asks for an immediate retry, and a
	// missing header asks for nothing.
	RetryAfterSet bool
}

// StatusError reports a GitHub response the client refused to treat as success.
//
// It carries the request's method and escaped path rather than its URL, so a
// caller that logs it cannot write down a query string, and it carries only
// GitHub's own message and documentation link out of the body. Response bytes
// never reach an error.
type StatusError struct {
	// Method and Path locate the request.
	Method string
	Path   string

	// Status is the HTTP status code.
	Status int

	// Message and DocumentationURL are GitHub's own explanation, empty when the
	// body was not an error envelope.
	Message          string
	DocumentationURL string

	// RateLimit is the retry metadata, nil when GitHub reported none.
	RateLimit *RateLimit

	// limited records whether this status is a rate limit rather than a plain
	// refusal, which cannot be read off the code alone: GitHub answers a
	// primary limit with 403 and a secondary one with either 403 or 429.
	limited bool
}

// Error renders the refusal without the response body.
func (e *StatusError) Error() string {
	message := e.Message
	if message == "" {
		message = http.StatusText(e.Status)
	}
	rendered := fmt.Sprintf("github %s %s: %d %s", e.Method, e.Path, e.Status, message)
	if delay, ok := e.RetryAfter(); ok {
		rendered += fmt.Sprintf(" (retry after %s)", delay)
	}
	return rendered
}

// Is maps a status onto the package refusals.
func (e *StatusError) Is(target error) bool {
	switch target {
	case ErrRateLimited:
		return e.limited
	case ErrUnauthorized:
		return e.Status == http.StatusUnauthorized
	case ErrForbidden:
		return e.Status == http.StatusForbidden && !e.limited
	case ErrNotFound:
		return e.Status == http.StatusNotFound
	default:
		return false
	}
}

// RetryAfter reports how long GitHub asked the client to wait, and whether it
// asked at all. A zero delay with ok true means retry now.
func (e *StatusError) RetryAfter() (time.Duration, bool) {
	if e.RateLimit == nil || !e.RateLimit.RetryAfterSet {
		return 0, false
	}
	return e.RateLimit.RetryAfter, true
}

// errorEnvelope is the body GitHub returns with a refusal.
type errorEnvelope struct {
	Message          string `json:"message"`
	DocumentationURL string `json:"documentation_url"`
}

// statusError builds the typed refusal for a non-success response.
//
// now is the instant the response arrived, and it is passed in rather than
// read here because a Retry-After deadline is absolute: turning it into a delay
// needs a clock, and the client's clock is injected so tests are exact.
func statusError(method, route string, resp *http.Response, raw []byte, now time.Time) error {
	failure := &StatusError{Method: method, Path: route, Status: resp.StatusCode}
	var envelope errorEnvelope
	// A refusal body that does not parse is not itself an error worth reporting
	// over the status that prompted it, so the message is simply left empty.
	if err := json.Unmarshal(raw, &envelope); err == nil {
		failure.Message = oneLine(envelope.Message)
		failure.DocumentationURL = oneLine(envelope.DocumentationURL)
	}
	failure.RateLimit = parseRateLimit(resp.Header, now)
	failure.limited = isRateLimited(resp.StatusCode, failure)
	return failure
}

// isRateLimited reports whether a refusal is a throttle rather than a denial.
//
// A 429 always is. A 403 is one only when GitHub says so, either by reporting
// an exhausted budget, by asking for a retry delay, or in its message, because
// the same code is how a missing installation permission is refused and
// retrying that forever would never succeed.
//
// The header evidence is checked before the message because a throttled
// response does not always carry a JSON body: an edge refusal can arrive as
// HTML or as nothing at all, and a Retry-After header is GitHub asking for a
// retry whatever the body says.
func isRateLimited(status int, failure *StatusError) bool {
	if status == http.StatusTooManyRequests {
		return true
	}
	if status != http.StatusForbidden {
		return false
	}
	if limit := failure.RateLimit; limit != nil && (limit.RetryAfterSet || (limit.Limit > 0 && limit.Remaining == 0)) {
		return true
	}
	message := strings.ToLower(failure.Message)
	return strings.Contains(message, "rate limit") || strings.Contains(message, "abuse")
}

// parseRateLimit reads the retry metadata out of a response's headers.
func parseRateLimit(header http.Header, now time.Time) *RateLimit {
	limit := &RateLimit{
		Limit:     headerInt(header, "X-RateLimit-Limit"),
		Remaining: headerInt(header, "X-RateLimit-Remaining"),
		Used:      headerInt(header, "X-RateLimit-Used"),
		Resource:  oneLine(header.Get("X-RateLimit-Resource")),
	}
	if reset := headerInt(header, "X-RateLimit-Reset"); reset > 0 {
		limit.Reset = time.Unix(int64(reset), 0).UTC()
	}
	limit.RetryAfter, limit.RetryAfterSet = retryAfter(header.Get("Retry-After"), now)
	if *limit == (RateLimit{}) {
		return nil
	}
	return limit
}

// headerInt reads a non-negative integer header, reporting zero when it is
// absent or unparseable.
func headerInt(header http.Header, name string) int {
	value, err := strconv.Atoi(strings.TrimSpace(header.Get(name)))
	if err != nil || value < 0 {
		return 0
	}
	return value
}

// retryAfter reads a Retry-After header in either of the two forms RFC 9110
// allows, reporting the delay and whether the header was there to be read.
//
// The date form is absolute, so it is resolved against the instant the response
// arrived. A deadline that has already passed yields a zero delay rather than a
// negative one: GitHub is asking for an immediate retry, not for none, and the
// caller still needs to know it was throttled.
func retryAfter(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds <= 0 {
			return 0, true
		}
		return time.Duration(seconds) * time.Second, true
	}
	deadline, err := http.ParseTime(value)
	if err != nil {
		// A header this package cannot read is reported as absent rather than
		// as zero delay, because guessing a delay would have it acted on.
		return 0, false
	}
	if delay := deadline.Sub(now); delay > 0 {
		return delay, true
	}
	return 0, true
}

// oneLine keeps a value GitHub supplied from spanning lines, so nothing this
// package renders can forge a second log record.
func oneLine(value string) string {
	value = strings.TrimSpace(value)
	if !strings.ContainsAny(value, "\r\n") {
		return value
	}
	return strings.Join(strings.FieldsFunc(value, func(r rune) bool { return r == '\r' || r == '\n' }), " ")
}

// redactURLError drops the URL that net/http embeds in a transport error.
//
// The wrapped cause is kept, so errors.Is still reaches the redirect refusals
// and context cancellation, but the request line does not survive into a
// message the caller may log next to a route it already knows.
func redactURLError(err error) error {
	var failure *url.Error
	if !errors.As(err, &failure) {
		return err
	}
	if failure.Op == "" {
		return failure.Err
	}
	return fmt.Errorf("%s: %w", strings.ToLower(failure.Op), failure.Err)
}
