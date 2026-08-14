package publish

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/gitcli"
)

// testSecret stands in for a credential that reached a remote URL by
// mistake. Nothing this package renders may echo it.
const testSecret = "ghs_notarealtokenvalue00000"

func TestNewValidatesTheDestination(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		remote      string
		identity    string
		allowLocal  bool
		namespaces  Namespaces
		wantErr     error
		wantMessage string
		wantHidden  string
	}{
		{
			name:        "a credential in the remote is refused",
			remote:      "https://soapbox:" + testSecret + "@github.com/enj/rbac_authorizer",
			wantMessage: "must not embed credentials",
			wantHidden:  testSecret,
		},
		{
			name:        "another host is refused",
			remote:      "https://example.invalid/enj/rbac_authorizer",
			wantMessage: "must publish to github.com",
		},
		{
			name:        "an ssh remote is refused",
			remote:      "git@github.com:enj/rbac_authorizer.git",
			wantMessage: "must be a remote name, an absolute path, or an https URL",
		},
		{
			name:        "a named remote is refused",
			remote:      "origin",
			wantMessage: "hides its target in configuration",
		},
		{
			name:    "a local remote needs an explicit option",
			remote:  "/tmp/soapbox-destination",
			wantErr: ErrLocalRemoteNotAllowed,
		},
		{
			name:        "a local remote needs a stated identity",
			remote:      "/tmp/soapbox-destination",
			allowLocal:  true,
			wantMessage: "requires a stated identity",
		},
		{
			name:        "a stated identity must name the remote",
			remote:      "https://github.com/enj/rbac_authorizer",
			identity:    "github.com/enj/other_module",
			wantMessage: "does not name remote",
		},
		{
			name:        "an identity that is a path is refused",
			remote:      "/tmp/soapbox-destination",
			allowLocal:  true,
			identity:    "/srv/mirrors/rbac_authorizer",
			wantMessage: "must not be a path",
		},
		{
			name:        "an identity carrying a credential is refused",
			remote:      "/tmp/soapbox-destination",
			allowLocal:  true,
			identity:    "soapbox:" + testSecret + "@github.com/enj/rbac_authorizer",
			wantMessage: "must be a repository name",
			wantHidden:  testSecret,
		},
		{
			name:        "a state ref outside refs/heads is refused",
			remote:      "https://github.com/enj/rbac_authorizer",
			namespaces:  Namespaces{StateRef: "refs/soapbox/state", ProgressPrefix: testProgressPrefix},
			wantMessage: "must live under refs/heads/",
		},
		{
			name:        "a progress namespace that shadows branches is refused",
			remote:      "https://github.com/enj/rbac_authorizer",
			namespaces:  Namespaces{StateRef: testStateRef, ProgressPrefix: "refs/heads/progress/"},
			wantMessage: "must not shadow branches or tags",
		},
		{
			name:        "a progress namespace without a trailing slash is refused",
			remote:      "https://github.com/enj/rbac_authorizer",
			namespaces:  Namespaces{StateRef: testStateRef, ProgressPrefix: "refs/soapbox/progress"},
			wantMessage: "must end with a slash",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			d := newDestination(ctx, t, "")

			opts := Options{
				Remote:           test.remote,
				Identity:         test.identity,
				AllowLocalRemote: test.allowLocal,
				Namespaces:       test.namespaces,
				Lister:           NewLocalRemote(d.git),
			}
			if opts.Namespaces == (Namespaces{}) {
				opts.Namespaces = Namespaces{StateRef: testStateRef, ProgressPrefix: testProgressPrefix}
			}
			_, err := New(ctx, d.git, opts)
			if err == nil {
				t.Fatal("the destination was accepted, want a refusal")
			}
			if test.wantErr != nil && !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want %v", err, test.wantErr)
			}
			if test.wantMessage != "" && !strings.Contains(err.Error(), test.wantMessage) {
				t.Fatalf("error = %v, want it to state %q", err, test.wantMessage)
			}
			if test.wantHidden != "" && strings.Contains(err.Error(), test.wantHidden) {
				t.Fatalf("error echoed a credential: %v", err)
			}
		})
	}
}

func TestNewDerivesTheIdentityFromAnHTTPSRemote(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newDestination(ctx, t, "")

	for _, remote := range []string{
		"https://github.com/enj/rbac_authorizer",
		"https://github.com/enj/rbac_authorizer.git",
	} {
		pub, err := New(ctx, d.git, Options{
			Remote:     remote,
			Lister:     NewLocalRemote(d.git),
			Namespaces: Namespaces{StateRef: testStateRef, ProgressPrefix: testProgressPrefix},
		})
		if err != nil {
			t.Fatalf("accept %s: %v", remote, err)
		}
		if pub.Identity() != testIdentity {
			t.Errorf("identity of %s = %q, want %q", remote, pub.Identity(), testIdentity)
		}
	}
}

func TestNetworkDestinationsFailClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newDestination(ctx, t, "")
	h := d.buildHistory()

	pub, err := New(ctx, d.git, Options{
		Remote:     "https://github.com/enj/rbac_authorizer",
		Lister:     NewLocalRemote(d.git),
		Namespaces: Namespaces{StateRef: testStateRef, ProgressPrefix: testProgressPrefix},
	})
	if err != nil {
		t.Fatalf("create publisher: %v", err)
	}

	// The refs of a network destination cannot be read without a remote ref
	// listing the typed Git boundary does not expose. Planning says so instead
	// of assuming the destination is empty, which would turn every update into
	// a create and every existing tag into a collision nobody saw coming.
	_, err = pub.Plan(ctx, []Update{branchUpdate(testBranch, h.forward)})
	if !errors.Is(err, ErrRemoteRefsUnsupported) {
		t.Fatalf("plan error = %v, want %v", err, ErrRemoteRefsUnsupported)
	}
}

func TestLocalRemoteReadsRealRepositories(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newDestination(ctx, t, "")
	h := d.buildHistory()
	d.seed(h.base+":"+testBranch, h.tagBase+":"+tagPrefix+"v0.36.1")

	lister := NewLocalRemote(d.git)
	for _, remote := range []string{d.remote, "file://" + d.remote} {
		refs, err := lister.RemoteRefs(ctx, remote)
		if err != nil {
			t.Fatalf("read %s: %v", remote, err)
		}
		observed := make(map[string]string, len(refs))
		for _, ref := range refs {
			observed[ref.Name] = ref.Target
		}
		if observed[testBranch] != h.base {
			t.Errorf("%s: branch = %q, want %q", remote, observed[testBranch], h.base)
		}
		// An annotated tag has to be reported as the tag object rather than the
		// commit it peels to, because that is the object the ref holds and the
		// object an immutability check compares.
		if observed[tagPrefix+"v0.36.1"] != h.tagBase {
			t.Errorf("%s: tag = %q, want the tag object %q", remote, observed[tagPrefix+"v0.36.1"], h.tagBase)
		}
	}
}

func TestLocalRemoteRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newDestination(ctx, t, "")
	lister := NewLocalRemote(d.git)

	tests := []struct {
		name        string
		remote      string
		wantErr     error
		wantMessage string
		wantHidden  string
	}{
		{
			name:    "an https remote",
			remote:  "https://github.com/enj/rbac_authorizer",
			wantErr: ErrRemoteRefsUnsupported,
		},
		{
			name:       "an https remote carrying a credential",
			remote:     "https://soapbox:" + testSecret + "@github.com/enj/rbac_authorizer",
			wantHidden: testSecret,
		},
		{
			name:        "a file URL naming a host",
			remote:      "file://elsewhere/srv/rbac_authorizer.git",
			wantMessage: "must not name a host",
		},
		{
			name:        "a relative path",
			remote:      "../rbac_authorizer.git",
			wantMessage: "must be an absolute path or an https URL",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := lister.RemoteRefs(ctx, test.remote)
			if err == nil {
				t.Fatal("the remote was read, want a refusal")
			}
			if test.wantErr != nil && !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want %v", err, test.wantErr)
			}
			if test.wantMessage != "" && !strings.Contains(err.Error(), test.wantMessage) {
				t.Fatalf("error = %v, want it to state %q", err, test.wantMessage)
			}
			if test.wantHidden != "" && strings.Contains(err.Error(), test.wantHidden) {
				t.Fatalf("error echoed a credential: %v", err)
			}
		})
	}
}

func TestValidateIdentityRejectsLocations(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		identity string
		want     string
		wantErr  bool
	}{
		{name: "a repository name", identity: testIdentity, want: testIdentity},
		{name: "a leading slash", identity: "/" + testIdentity, wantErr: true},
		{name: "a trailing slash", identity: testIdentity + "/", wantErr: true},
		{name: "empty", identity: "", wantErr: true},
		{name: "only slashes", identity: "///", wantErr: true},
		{name: "a URL", identity: "https://" + testIdentity, wantErr: true},
		{name: "user information", identity: "soapbox@" + testIdentity, wantErr: true},
		{name: "a port", identity: "github.com:443/enj/rbac_authorizer", wantErr: true},
		{name: "traversal", identity: "github.com/enj/../other", wantErr: true},
		{name: "a current directory component", identity: "github.com/./enj", wantErr: true},
		{name: "whitespace", identity: "github.com/enj/rbac authorizer", wantErr: true},
		{name: "an absolute path", identity: "/srv/git/rbac_authorizer", wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := validateIdentity(test.identity)
			if test.wantErr {
				if err == nil {
					t.Fatalf("identity %q was accepted as %q", test.identity, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("identity %q: %v", test.identity, err)
			}
			if got != test.want {
				t.Errorf("identity %q = %q, want %q", test.identity, got, test.want)
			}
		})
	}
}

func TestRedactRemovesUserInformation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		remote string
		want   string
	}{
		{
			name:   "an https URL carrying a token",
			remote: "https://soapbox:" + testSecret + "@github.com/enj/rbac_authorizer",
			want:   "https://redacted@github.com/enj/rbac_authorizer",
		},
		{
			name:   "an https URL carrying a bare user",
			remote: "https://" + testSecret + "@github.com/enj/rbac_authorizer",
			want:   "https://redacted@github.com/enj/rbac_authorizer",
		},
		{
			name:   "an scp form remote",
			remote: testSecret + "@github.com:enj/rbac_authorizer.git",
			want:   "redacted@github.com:enj/rbac_authorizer.git",
		},
		{
			name:   "an identity carrying a token",
			remote: "soapbox:" + testSecret + "@github.com/enj/rbac_authorizer",
			want:   "redacted@github.com/enj/rbac_authorizer",
		},
		{
			name:   "a remote with nothing to hide",
			remote: "https://github.com/enj/rbac_authorizer",
			want:   "https://github.com/enj/rbac_authorizer",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := redact(test.remote)
			if got != test.want {
				t.Errorf("redact(%q) = %q, want %q", test.remote, got, test.want)
			}
			if strings.Contains(got, testSecret) {
				t.Errorf("redact(%q) kept the credential", test.remote)
			}
		})
	}
}

func TestNewRequiresItsCollaborators(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newDestination(ctx, t, "")

	if _, err := New(ctx, nil, Options{Remote: d.remote, AllowLocalRemote: true, Identity: testIdentity, Lister: NewLocalRemote(d.git)}); err == nil {
		t.Error("a publisher was built without a git runner")
	}
	if _, err := New(ctx, d.git, Options{Remote: d.remote, AllowLocalRemote: true, Identity: testIdentity}); err == nil {
		t.Error("a publisher was built without a remote ref lister")
	}
}

func TestNewRefusesAContradictedObjectFormat(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newDestination(ctx, t, "")

	_, err := New(ctx, d.git, Options{
		Remote:           d.remote,
		AllowLocalRemote: true,
		Identity:         testIdentity,
		Lister:           NewLocalRemote(d.git),
		Namespaces:       Namespaces{StateRef: testStateRef, ProgressPrefix: testProgressPrefix},
		ObjectFormat:     gitcli.ObjectFormatSHA256,
	})
	if err == nil {
		t.Fatal("a sha256 destination was accepted for a sha1 repository")
	}
	if !strings.Contains(err.Error(), "does not match the local repository format") {
		t.Fatalf("error = %v, want it to name the format mismatch", err)
	}
}
