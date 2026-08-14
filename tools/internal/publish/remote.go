package publish

import (
	"context"
	"fmt"
	"net/url"
	"path"
	"path/filepath"
	"strings"

	"github.com/enj/soapbox/tools/internal/gitcli"
)

// RemoteRefLister reads the refs a destination currently advertises.
//
// It is an interface rather than a method on the Git runner because the runner
// has no remote ref listing yet. Planning cannot proceed without one: what a
// push would do is a fact about the remote, and every alternative source for
// that fact is worse. A local tracking ref is a memory of a past fetch, and a
// fetch would download objects to answer a question about names.
//
// An implementation must not embed credentials in the remote it is handed, must
// not fetch objects, and must report the refs exactly as the remote holds them,
// with an annotated tag reported as the tag object rather than the commit it
// peels to.
type RemoteRefLister interface {
	RemoteRefs(ctx context.Context, remote string) ([]gitcli.Ref, error)
}

// LocalRemote lists the refs of a destination that lives on this machine, by
// reading the repository directly through the typed Git boundary.
//
// It covers the local bare mirrors that dry runs and tests publish to, where
// the remote's refs and the repository's refs are the same refs. A network
// destination is refused rather than approximated, because the only honest way
// to read one is git ls-remote and this engine does not expose it yet.
type LocalRemote struct {
	git *gitcli.Runner
}

// NewLocalRemote builds a lister backed by one Git runner. The runner supplies
// only the controlled environment; the repository it reads is the destination
// named in each call.
func NewLocalRemote(git *gitcli.Runner) *LocalRemote { return &LocalRemote{git: git} }

// RemoteRefs reports the refs of a local destination.
func (l *LocalRemote) RemoteRefs(ctx context.Context, remote string) ([]gitcli.Ref, error) {
	if l == nil || l.git == nil {
		return nil, fmt.Errorf("local remote refs: no git runner")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("local remote refs: %w", err)
	}
	dir, err := localRemotePath(remote)
	if err != nil {
		return nil, fmt.Errorf("local remote refs: %w", err)
	}
	repository, err := l.git.WithDir(dir)
	if err != nil {
		return nil, fmt.Errorf("local remote refs: %w", err)
	}
	refs, err := repository.ListRefs(ctx)
	if err != nil {
		return nil, fmt.Errorf("local remote refs: %w", err)
	}
	return refs, nil
}

// isLocalRemote reports whether a remote names a repository on this machine.
func isLocalRemote(remote string) (bool, error) {
	if err := gitcli.ValidateRemote(remote); err != nil {
		return false, err
	}
	if !strings.Contains(remote, "://") {
		return filepath.IsAbs(remote), nil
	}
	parsed, err := url.Parse(remote)
	if err != nil {
		return false, fmt.Errorf("remote is malformed: %w", err)
	}
	return parsed.Scheme == "file", nil
}

// localRemotePath reports the repository directory a local remote names.
//
// The remote is held to the same rule a push target is: an absolute path or a
// URL, never a name or a relative path, because both of those resolve against
// state this package cannot see. A file URL is then required to name no host,
// or the local one. A host bearing file URL is a path on another machine as far
// as some tools are concerned, and reading it as a local directory would
// silently answer a question about one repository with the contents of another.
func localRemotePath(remote string) (string, error) {
	if err := gitcli.ValidatePushRemote(remote); err != nil {
		return "", err
	}
	local, err := isLocalRemote(remote)
	if err != nil {
		return "", err
	}
	if !local {
		return "", fmt.Errorf("%q: %w", redact(remote), ErrRemoteRefsUnsupported)
	}
	if !strings.Contains(remote, "://") {
		return remote, nil
	}
	parsed, err := url.Parse(remote)
	if err != nil {
		return "", fmt.Errorf("remote is malformed: %w", err)
	}
	if host := parsed.Host; host != "" && host != "localhost" {
		return "", fmt.Errorf("file remote %q must not name a host", redact(remote))
	}
	if !filepath.IsAbs(parsed.Path) {
		return "", fmt.Errorf("file remote %q must name an absolute path", redact(remote))
	}
	return parsed.Path, nil
}

// canonicalIdentity reports the destination recorded in a manifest.
//
// The identity is a repository name, never a location. An https destination has
// one already, host and path with any git suffix dropped, so it is derived and
// a stated value is checked against it rather than trusted. A local destination
// has no such name, so the caller states one and the path never appears: a
// manifest is compared byte for byte across runs and machines, and a temporary
// directory would make two identical publications look different.
func canonicalIdentity(remote, stated string, local bool) (string, error) {
	if local {
		if stated == "" {
			return "", fmt.Errorf("a local destination requires a stated identity, such as %s/owner/name", gitcli.PublishHost)
		}
		return validateIdentity(stated)
	}
	parsed, err := url.Parse(remote)
	if err != nil {
		return "", fmt.Errorf("remote is malformed: %w", err)
	}
	derived, err := validateIdentity(parsed.Host + strings.TrimSuffix(parsed.Path, ".git"))
	if err != nil {
		return "", err
	}
	if stated != "" && stated != derived {
		return "", fmt.Errorf("stated identity %q does not name remote %q", stated, derived)
	}
	return derived, nil
}

// validateIdentity checks a canonical repository name.
//
// The rules keep a location out of a name. No scheme, no user information, no
// absolute path, no whitespace, and no traversal, so an identity cannot become
// a filesystem path by accident and cannot carry a credential at all. It is
// also required to be already normalized rather than normalized here, because a
// manifest is compared byte for byte and two spellings of one repository would
// otherwise be two manifests.
//
// A rejected identity is never echoed as written. It is caller supplied, and
// the one way it can be wrong that matters is by carrying a token.
func validateIdentity(identity string) (string, error) {
	switch {
	case identity == "":
		return "", fmt.Errorf("identity %q must name a repository", identity)
	case strings.Contains(identity, "://"), strings.Contains(identity, "@"), strings.Contains(identity, ":"):
		return "", fmt.Errorf("identity %q must be a repository name such as %s/owner/name", redact(identity), gitcli.PublishHost)
	case filepath.IsAbs(identity):
		return "", fmt.Errorf("identity %q must not be a path", redact(identity))
	case strings.ContainsAny(identity, " \t\n\r"):
		return "", fmt.Errorf("identity %q must not contain whitespace", redact(identity))
	case path.Clean(identity) != identity:
		return "", fmt.Errorf("identity %q must be already normalized", redact(identity))
	}
	return identity, nil
}

// redactedUser replaces user information in a rendered remote.
const redactedUser = "redacted"

// redact renders a remote for a message without any user information, so a
// destination that was handed a credential by mistake cannot echo it into an
// error a caller logs.
func redact(remote string) string {
	parsed, err := url.Parse(remote)
	if err != nil || !strings.Contains(remote, "://") {
		if at := strings.LastIndex(remote, "@"); at >= 0 {
			return redactedUser + "@" + remote[at+1:]
		}
		return remote
	}
	if parsed.User != nil {
		parsed.User = url.User(redactedUser)
	}
	return parsed.String()
}

// HTTPSRemote lists the refs of a destination over the network using git
// ls-remote through the typed Git boundary.
//
// It covers the production case: a github.com HTTPS destination whose
// credentials travel through the Git runner's package-owned runtime
// configuration. The runner must carry a GitHubTokenCredential; the remote URL
// must never embed credentials.
type HTTPSRemote struct {
	git *gitcli.Runner
}

// NewHTTPSRemote builds a lister for network destinations.
func NewHTTPSRemote(git *gitcli.Runner) *HTTPSRemote { return &HTTPSRemote{git: git} }

// RemoteRefs reports the refs a network destination advertises.
func (h *HTTPSRemote) RemoteRefs(ctx context.Context, remote string) ([]gitcli.Ref, error) {
	if h == nil || h.git == nil {
		return nil, fmt.Errorf("https remote refs: no git runner")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("https remote refs: %w", err)
	}
	local, err := isLocalRemote(remote)
	if err != nil {
		return nil, fmt.Errorf("https remote refs: %w", err)
	}
	if local {
		return nil, fmt.Errorf("https remote refs: %q is a local destination, use LocalRemote", redact(remote))
	}
	format, err := h.git.ObjectFormat(ctx)
	if err != nil {
		return nil, fmt.Errorf("https remote refs: %w", err)
	}
	refs, err := h.git.RemoteRefs(ctx, remote, format.HexLength())
	if err != nil {
		return nil, fmt.Errorf("https remote refs: %w", err)
	}
	return refs, nil
}

// remoteRefs reads the destination and indexes it by ref name.
//
// Every advertised object name is checked against the destination's hash
// algorithm. A remote in the other algorithm is not a remote whose values are
// simply unfamiliar: comparing its names with local ones would report every ref
// as changed, and a plan built on that would try to create refs that already
// exist.
func (p *Publisher) remoteRefs(ctx context.Context) (map[string]string, error) {
	refs, err := p.lister.RemoteRefs(ctx, p.remote)
	if err != nil {
		return nil, fmt.Errorf("read %s refs: %w", p.identity, err)
	}
	observed := make(map[string]string, len(refs))
	for _, ref := range refs {
		// A remote advertises more than refs. HEAD is a symbolic pointer, and
		// an annotated tag is advertised a second time in peeled form, as
		// refs/tags/v1^{} naming the commit inside it. Neither is a ref this
		// package could publish to, and the peeled entry would contradict the
		// real one for the same name, so both are skipped rather than parsed.
		if !strings.HasPrefix(ref.Name, "refs/") || strings.HasSuffix(ref.Name, "^{}") {
			continue
		}
		if err := gitcli.ValidateRefName(ref.Name); err != nil {
			return nil, fmt.Errorf("read %s refs: %w", p.identity, err)
		}
		if err := p.validateObjectName("remote object", ref.Target); err != nil {
			return nil, fmt.Errorf("read %s refs: %q: %w", p.identity, ref.Name, err)
		}
		if previous, ok := observed[ref.Name]; ok && previous != ref.Target {
			return nil, fmt.Errorf("read %s refs: %q was advertised as both %s and %s", p.identity, ref.Name, previous, ref.Target)
		}
		observed[ref.Name] = ref.Target
	}
	return observed, nil
}
