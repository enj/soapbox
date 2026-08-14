package ghapi_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/ghapi"
)

func TestStaticBearerReturnsHeader(t *testing.T) {
	t.Parallel()
	auth, err := ghapi.NewStaticBearer("ghs_abc123")
	if err != nil {
		t.Fatalf("NewStaticBearer: %v", err)
	}
	got, err := auth.AuthorizationHeader(context.Background())
	if err != nil {
		t.Fatalf("AuthorizationHeader: %v", err)
	}
	if got != "Bearer ghs_abc123" {
		t.Errorf("header = %q, want %q", got, "Bearer ghs_abc123")
	}
}

func TestStaticBearerRefusesEmpty(t *testing.T) {
	t.Parallel()
	_, err := ghapi.NewStaticBearer("")
	if err == nil {
		t.Fatal("empty token was accepted")
	}
}

func TestStaticBearerRefusesControlCharacters(t *testing.T) {
	t.Parallel()
	for _, token := range []string{"ghs_abc\n123", "ghs\x00abc", "ghs\r123"} {
		_, err := ghapi.NewStaticBearer(token)
		if err == nil {
			t.Fatalf("token %q with control char was accepted", token)
		}
		if !strings.Contains(err.Error(), "control") {
			t.Errorf("error = %v, want mention of control characters", err)
		}
	}
}

func TestStaticBearerRefusesWhitespace(t *testing.T) {
	t.Parallel()
	for _, token := range []string{"ghs abc123", "ghs abc123"} {
		_, err := ghapi.NewStaticBearer(token)
		if err == nil {
			t.Fatalf("token %q with whitespace was accepted", token)
		}
		if !strings.Contains(err.Error(), "whitespace") {
			t.Errorf("error = %v, want mention of whitespace", err)
		}
	}
}

func TestStaticBearerImplementsAuthorizer(t *testing.T) {
	t.Parallel()
	auth, err := ghapi.NewStaticBearer("ghs_test")
	if err != nil {
		t.Fatalf("NewStaticBearer: %v", err)
	}
	var _ ghapi.Authorizer = auth
}

func TestStaticBearerFormattingOmitsToken(t *testing.T) {
	t.Parallel()
	const token = "ghs_formatting_secret"
	auth, err := ghapi.NewStaticBearer(token)
	if err != nil {
		t.Fatalf("NewStaticBearer: %v", err)
	}
	for _, format := range []string{"%v", "%+v", "%#v"} {
		rendered := fmt.Sprintf(format, auth)
		if strings.Contains(rendered, token) {
			t.Fatalf("format %q leaked token in %q", format, rendered)
		}
	}
}

func TestStaticBearerRefusesNilReceiver(t *testing.T) {
	t.Parallel()
	var auth *ghapi.StaticBearer
	if _, err := auth.AuthorizationHeader(t.Context()); err == nil {
		t.Fatal("nil static bearer returned a credential")
	}
}
