package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/enj/soapbox/tools/internal/config"
	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/publish"
)

const WriteVerificationSchema = 1

// WriteVerificationOptions describes a trusted no-op destination write probe.
type WriteVerificationOptions struct {
	Config      *config.Config
	LocalGit    *gitcli.Runner
	Destination Destination
}

// WriteVerificationResult is the stable report of a leased no-op branch push.
type WriteVerificationResult struct {
	Schema        int    `json:"schema"`
	Ref           string `json:"ref"`
	Object        string `json:"object"`
	WriteVerified bool   `json:"writeVerified"`
}

func (r WriteVerificationResult) JSON() ([]byte, error) {
	encoded, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("write verification report: %w", err)
	}
	return append(encoded, '\n'), nil
}

func (r WriteVerificationResult) Text() string {
	return fmt.Sprintf("soapbox write verification\n  ref            %s\n  object         %s\n  write verified %t\n", r.Ref, r.Object, r.WriteVerified)
}

// VerifyWriteAccess proves a trusted destination credential can reach
// receive-pack without discovering or publishing a source release. The branch
// is pushed to itself with a compare-and-swap lease; every destination ref must
// remain unchanged.
func VerifyWriteAccess(ctx context.Context, opts WriteVerificationOptions) (*WriteVerificationResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("write verification: %w", err)
	}
	switch {
	case opts.Config == nil:
		return nil, errors.New("write verification: a profile is required")
	case opts.LocalGit == nil:
		return nil, errors.New("write verification: an anonymous local Git runner is required")
	case opts.Destination.Git == nil:
		return nil, errors.New("write verification: a destination Git runner is required")
	case !opts.LocalGit.IsAnonymous():
		return nil, errors.New("write verification: the local Git runner must be anonymous")
	case !opts.LocalGit.IsNoLazyFetch():
		return nil, errors.New("write verification: the local Git runner must refuse promisor fetches")
	}
	format, err := opts.LocalGit.ObjectFormat(ctx)
	if err != nil {
		return nil, fmt.Errorf("write verification: object format: %w", err)
	}
	publisher, lister, err := publisherForDestination(ctx, opts.Destination.Git, opts.Destination, opts.Config, format)
	if err != nil {
		return nil, fmt.Errorf("write verification: %w", err)
	}
	refs, err := lister.RemoteRefs(ctx, opts.Destination.Remote)
	if err != nil {
		return nil, fmt.Errorf("write verification: read destination refs: %w", err)
	}
	observed, err := indexDestinationRefs(refs, opts.Config, format)
	if err != nil {
		return nil, fmt.Errorf("write verification: %w", err)
	}
	branchRef := "refs/heads/" + opts.Config.Destination.Branch
	object := observed[branchRef]
	if object == "" {
		return nil, fmt.Errorf("write verification: destination branch %s is absent", branchRef)
	}
	if err := requireLocalHEADMatches(ctx, opts.LocalGit, branchRef, object); err != nil {
		return nil, fmt.Errorf("write verification: %w", err)
	}
	if err := verifyObservedWrite(ctx, publisher, branchRef, object); err != nil {
		return nil, err
	}
	return &WriteVerificationResult{
		Schema: WriteVerificationSchema, Ref: branchRef, Object: object, WriteVerified: true,
	}, nil
}

func verifyObservedWrite(ctx context.Context, publisher *publish.Publisher, ref, object string) error {
	plan, err := publisher.Plan(ctx, []publish.Update{{
		Ref: ref, Kind: publish.KindBranch,
		NewObject: object, ExpectedOld: object,
		Evidence: "trusted no-op write verification",
	}})
	if err != nil {
		return fmt.Errorf("write verification: %w", err)
	}
	result, err := publisher.Apply(ctx, plan, publish.ApplyOptions{
		Approval: plan.Manifest.Hash,
		Scope:    publish.ScopeReconcile,
	})
	if err != nil {
		return fmt.Errorf("write verification: %w", err)
	}
	if !result.Verified || len(result.Pushed) != 1 || result.Pushed[0] != ref {
		return fmt.Errorf("write verification: push result verified=%t refs=%v", result.Verified, result.Pushed)
	}
	return nil
}
