package sync_test

import (
	"maps"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/sync"
)

func TestVerifyWriteAccessLeasesWithoutMovingRefs(t *testing.T) {
	ctx := t.Context()
	destination := newDestination(ctx, t)
	destination.publishControlPlane(ctx, t)
	before := destination.remoteRefs(ctx, t)
	opts := destination.options()

	result, err := sync.VerifyWriteAccess(ctx, sync.WriteVerificationOptions{
		Config:      opts.Config,
		LocalGit:    destination.git.Anonymous().WithNoLazyFetch(),
		Destination: opts.Destination,
	})
	if err != nil {
		t.Fatalf("verify write access: %v", err)
	}
	if !result.WriteVerified || result.Ref != testBranchRef || result.Object != destination.parent {
		t.Errorf("result = %#v, want verified %s at %s", result, testBranchRef, destination.parent)
	}
	after := destination.remoteRefs(ctx, t)
	if !maps.Equal(before, after) {
		t.Errorf("write verification moved refs: before %#v, after %#v", before, after)
	}
	encoded, err := result.JSON()
	if err != nil {
		t.Fatalf("render result: %v", err)
	}
	if !strings.Contains(string(encoded), `"writeVerified": true`) || !strings.Contains(result.Text(), "write verified true") {
		t.Errorf("result renderings omit proof:\n%s\n%s", encoded, result.Text())
	}
}
