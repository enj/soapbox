package ghapi

import (
	"context"
	"fmt"
	"unicode"
)

// StaticBearer is an Authorizer that presents a fixed bearer token.
//
// It is the production credential for GITHUB_TOKEN workflows: the token is
// minted per-job by Actions and does not expire within the job's lifetime, so
// it needs no renewal and a static holder is the honest representation.
//
// The token is validated at construction: empty, whitespace-bearing, and
// control-character-bearing values are refused so a misconfigured caller fails
// immediately rather than after a network round trip with a malformed header.
type StaticBearer struct {
	header string
}

// String renders the authorizer without exposing the bearer token.
func (*StaticBearer) String() string { return "static bearer credential" }

// GoString renders the authorizer safely for the %#v format.
func (*StaticBearer) GoString() string { return "ghapi.StaticBearer{}" }

// NewStaticBearer builds an authorizer from a raw token value.
//
// The token must not be empty, must not contain control characters (including
// newlines), and must not contain whitespace. These are the characters that
// would either break the HTTP header or indicate a configuration error (such as
// passing the full "Bearer xxx" string instead of just the token).
func NewStaticBearer(token string) (*StaticBearer, error) {
	if err := validateToken(token); err != nil {
		return nil, fmt.Errorf("static bearer: %w", err)
	}
	return &StaticBearer{header: "Bearer " + token}, nil
}

// AuthorizationHeader returns the bearer header value.
func (s *StaticBearer) AuthorizationHeader(_ context.Context) (string, error) {
	if s == nil || s.header == "" {
		return "", fmt.Errorf("static bearer: no credential")
	}
	return s.header, nil
}

// validateToken checks a token value for use in headers and environment.
func validateToken(token string) error {
	if token == "" {
		return fmt.Errorf("token must not be empty")
	}
	for _, r := range token {
		switch {
		case unicode.IsControl(r):
			return fmt.Errorf("token must not contain control characters")
		case unicode.IsSpace(r):
			return fmt.Errorf("token must not contain whitespace")
		}
	}
	return nil
}
