package gitcli

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
)

// RemoteRefs reads the refs a remote repository advertises without fetching any
// objects.
//
// The command is git ls-remote --refs, which asks the remote for its ref
// advertisement and reports each ref as a tab-separated OID/name pair. No
// objects are downloaded, and no local state is modified.
//
// The runner must carry a GitHubTokenCredential for an authenticated remote,
// or be anonymous for a public one. The package translates that credential into
// GIT_CONFIG_COUNT/KEY/VALUE entries with a host-scoped extraHeader; it never
// appears in a URL or an argument vector.
//
// The remote is validated by ValidatePushRemote: it must be an absolute path, a
// file URL, or an https URL on the publish host. A named remote is refused
// because its target lives in configuration.
//
// Results are deterministically sorted by ref name. Duplicate refs (same name,
// different OID) are refused. Peeled entries (^{}) and HEAD are excluded. Every
// returned name is validated by ValidateRefName and every OID is checked against
// the runner's expected hex length from the caller.
func (r *Runner) RemoteRefs(ctx context.Context, remote string, hexLength int) ([]Ref, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("git ls-remote: %w", err)
	}
	if err := ValidatePushRemote(remote); err != nil {
		return nil, fmt.Errorf("git ls-remote: %w", r.redactor.Error(err))
	}
	if hexLength != 40 && hexLength != 64 {
		return nil, fmt.Errorf("git ls-remote: hex length %d must be 40 or 64", hexLength)
	}
	if err := r.assertNoRemoteRewrites(ctx); err != nil {
		return nil, fmt.Errorf("git ls-remote: %w", r.redactor.Error(err))
	}
	safeRemote := r.redactor.String(redactRemote(remote))

	// --refs suppresses the peeled entries (^{}) and the HEAD symref, so the
	// output is one ref per line in the form "<oid>\t<refname>\n".
	out, err := r.run(ctx, "ls-remote", "--refs", "--end-of-options", remote)
	if err != nil {
		return nil, fmt.Errorf("git ls-remote %q: %w", safeRemote, r.redactor.Error(err))
	}

	var refs []Ref
	seen := make(map[string]string)
	for line := range strings.SplitSeq(out, "\n") {
		if line == "" {
			continue
		}
		oid, name, ok := strings.Cut(line, "\t")
		if !ok {
			return nil, fmt.Errorf("git ls-remote %q: malformed line %q", safeRemote, r.redactor.String(line))
		}
		safeName := r.redactor.String(name)
		if err := ValidateRefName(name); err != nil {
			return nil, fmt.Errorf("git ls-remote %q: %w", safeRemote, r.redactor.Error(err))
		}
		if err := validateHex(oid, hexLength); err != nil {
			return nil, fmt.Errorf("git ls-remote %q: %q: %w", safeRemote, safeName, r.redactor.Error(err))
		}
		if prev, dup := seen[name]; dup {
			if prev != oid {
				return nil, fmt.Errorf("git ls-remote %q: %q advertised as both %s and %s", safeRemote, safeName, r.redactor.String(prev), r.redactor.String(oid))
			}
			continue
		}
		seen[name] = oid
		refs = append(refs, Ref{Name: name, Target: oid})
	}

	// Sort deterministically by ref name so callers observe stable ordering
	// regardless of the remote's advertisement order.
	slices.SortFunc(refs, func(a, b Ref) int {
		return strings.Compare(a.Name, b.Name)
	})

	return refs, nil
}

// FetchExact downloads exactly one advertised object from a remote without
// moving any local ref and without writing FETCH_HEAD.
//
// The refspec fetches the remote ref into a namespaced ref under
// refs/soapbox/fetch/ that no consumer ref can collide with. After the fetch
// the temporary ref is deleted, so no persistent state remains. The object is
// retained in the object store.
//
// The caller must supply the exact OID the remote advertised (from RemoteRefs).
// After the fetch, the downloaded object is verified to match that OID. This
// guarantees that the remote cannot substitute a different object than what was
// advertised.
//
// The runner must carry the same credentials that were used for RemoteRefs.
func (r *Runner) FetchExact(ctx context.Context, remote, remoteRef, expectedOID string, hexLength int) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("git fetch exact: %w", err)
	}
	if err := ValidatePushRemote(remote); err != nil {
		return fmt.Errorf("git fetch exact: %w", r.redactor.Error(err))
	}
	if err := ValidateRefName(remoteRef); err != nil {
		return fmt.Errorf("git fetch exact: remote ref: %w", r.redactor.Error(err))
	}
	if hexLength != 40 && hexLength != 64 {
		return fmt.Errorf("git fetch exact: hex length %d must be 40 or 64", hexLength)
	}
	if err := validateHex(expectedOID, hexLength); err != nil {
		return fmt.Errorf("git fetch exact: expected OID: %w", r.redactor.Error(err))
	}
	if err := r.assertNoRemoteRewrites(ctx); err != nil {
		return fmt.Errorf("git fetch exact: %w", r.redactor.Error(err))
	}
	safeRemote := r.redactor.String(redactRemote(remote))
	safeRef := r.redactor.String(remoteRef)
	safeExpected := r.redactor.String(expectedOID)

	// Include the advertised object in the private namespace so a stale ref left
	// by an interrupted fetch for a different advertisement cannot block this
	// fetch or be overwritten by it.
	tmpRef := "refs/soapbox/fetch/" + expectedOID

	// Fetch exactly the named remote ref into the temporary ref.
	// --no-write-fetch-head keeps FETCH_HEAD clean.
	// --no-tags prevents auto-following tags.
	refspec := remoteRef + ":" + tmpRef
	_, err := r.run(ctx, "fetch", "--no-write-fetch-head", "--no-tags",
		"--end-of-options", remote, refspec)
	if err != nil {
		return fmt.Errorf("git fetch exact from %q ref %q: %w", safeRemote, safeRef, r.redactor.Error(err))
	}

	cleanup := func() error {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, err := r.run(cleanupCtx, "update-ref", "-d", "--end-of-options", tmpRef)
		return err
	}
	needsCleanup := true
	defer func() {
		if needsCleanup {
			_ = cleanup()
		}
	}()

	// Verify the fetched object matches the advertised OID.
	out, err := r.run(ctx, "rev-parse", "--verify", "--end-of-options", tmpRef)
	if err != nil {
		return fmt.Errorf("git fetch exact: verify tmp ref: %w", r.redactor.Error(err))
	}
	got := strings.TrimSpace(out)
	if got != expectedOID {
		return fmt.Errorf("git fetch exact: fetched object %s does not match advertised %s for %q", r.redactor.String(got), safeExpected, safeRef)
	}

	// Delete the temporary ref — the object remains in the store.
	if err := cleanup(); err != nil {
		return fmt.Errorf("git fetch exact: delete tmp ref: %w", r.redactor.Error(err))
	}
	needsCleanup = false
	return nil
}

// validateHex checks that s is exactly hexLength lowercase hex characters.
func validateHex(s string, hexLength int) error {
	if len(s) != hexLength {
		return fmt.Errorf("object %q must be a %d-character hex name", s, hexLength)
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return fmt.Errorf("object %q must be lowercase hexadecimal", s)
		}
	}
	return nil
}
