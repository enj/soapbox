package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/enj/soapbox/tools/internal/config"
	"github.com/enj/soapbox/tools/internal/extract"
	"github.com/enj/soapbox/tools/internal/generate"
	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/publish"
	"github.com/enj/soapbox/tools/internal/source"
)

const ReconcileSchema = 1

// ReconcileOptions composes all already-validated runtime boundaries of one
// trusted workflow. Source acquisition remains anonymous; destination reads and
// writes use a separate runner.
type ReconcileOptions struct {
	Config      *config.Config
	SourceCache *source.Cache
	LocalGit    *gitcli.Runner
	RemoteGit   *gitcli.Runner
	Destination Destination
	Generate    generate.Options

	StateCommitOverride string
	// Apply and Approval execute exactly one manual reconciliation plan. Automatic
	// ignores them and self-approves each in-memory plan under trusted policy.
	Apply     bool
	Approval  string
	Automatic bool
	Budget    WorkflowBudget
}

// ReconcileAction is one deterministic plan or apply decision.
type ReconcileAction struct {
	Kind           string `json:"kind"`
	SourceTag      string `json:"sourceTag,omitempty"`
	DestinationTag string `json:"destinationTag,omitempty"`
	PlanHash       string `json:"planHash,omitempty"`
	StateCommit    string `json:"stateCommit,omitempty"`
	Progress       string `json:"progress,omitempty"`
	Done           int    `json:"done,omitempty"`
	Total          int    `json:"total,omitempty"`
	Applied        bool   `json:"applied"`
}

// ReconcileResult is the machine-stable report for one workflow invocation.
type ReconcileResult struct {
	Schema             int  `json:"schema"`
	FixedPoint         bool `json:"fixedPoint"`
	BudgetExhausted    bool `json:"budgetExhausted"`
	NeedsConfiguration bool `json:"needsConfiguration"`
	// WriteVerified reports that an automatic fixed-point run completed a
	// leased no-op push through the destination credential. No ref moved; the
	// receive-pack handshake proves the token still has write access.
	WriteVerified bool              `json:"writeVerified"`
	Actions       []ReconcileAction `json:"actions"`
}

func (r ReconcileResult) JSON() ([]byte, error) {
	encoded, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("reconciliation report: %w", err)
	}
	return append(encoded, '\n'), nil
}

func (r ReconcileResult) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "soapbox reconciliation: %d action(s)\n", len(r.Actions))
	for _, action := range r.Actions {
		status := "planned"
		if action.Applied {
			status = "applied"
		}
		fmt.Fprintf(&b, "  %-24s %s", action.Kind, status)
		if action.DestinationTag != "" {
			fmt.Fprintf(&b, " %s -> %s", action.SourceTag, action.DestinationTag)
		}
		if action.Progress != "" {
			fmt.Fprintf(&b, " %s %d/%d", action.Progress, action.Done, action.Total)
		}
		if action.PlanHash != "" {
			fmt.Fprintf(&b, " %s", action.PlanHash)
		}
		b.WriteByte('\n')
	}
	switch {
	case r.FixedPoint:
		b.WriteString("  fixed point: destination and state agree\n")
	case r.BudgetExhausted:
		b.WriteString("  checkpoint: workflow budget reserved for clean exit\n")
	case r.NeedsConfiguration:
		b.WriteString("  configuration: persist the resolved source anchor before automatic publication\n")
	}
	if r.WriteVerified {
		b.WriteString("  write access: leased no-op push verified\n")
	}
	return b.String()
}

// Reconcile runs checkpoint steps until the pending release is complete, the
// workflow budget reserves a clean exit, or a consumer release is published.
// It publishes at most one consumer release per invocation; the next workflow
// starts from the newly reconciled branch and processes the next release.
func Reconcile(ctx context.Context, opts ReconcileOptions) (*ReconcileResult, error) {
	if err := checkReconcileOptions(opts); err != nil {
		return nil, err
	}
	if opts.Generate.Fetch && !opts.Generate.Offline {
		if err := opts.SourceCache.Fetch(ctx, source.Refs{AllTags: true}); err != nil {
			return nil, fmt.Errorf("reconciliation: fetch source releases: %w", err)
		}
	}
	report := &ReconcileResult{Schema: ReconcileSchema, Actions: []ReconcileAction{}}
	override := opts.StateCommitOverride
	seenState := map[string]bool{}

	for {
		if err := ctx.Err(); err != nil {
			return report, fmt.Errorf("reconciliation: %w", err)
		}
		discovery, err := discoverForReconcile(ctx, opts, override)
		if err != nil {
			return report, err
		}
		override = ""
		if discovery.StateCommit != "" {
			if seenState[discovery.StateCommit] {
				return report, fmt.Errorf("reconciliation: state %s was observed twice without reaching a terminal result", discovery.StateCommit)
			}
			seenState[discovery.StateCommit] = true
		}

		if discovery.Adopted != nil {
			rel, err := adoptedRelease(ctx, opts.Config, opts.SourceCache, discovery)
			if err != nil {
				return report, err
			}
			plan, err := PlanAdoptionReconciliation(ctx, FinalizeOptions{
				Config: opts.Config, Discovery: discovery, SourceCache: opts.SourceCache,
				Destination: opts.Destination, Release: rel,
			})
			if err != nil {
				return report, err
			}
			action := ReconcileAction{
				Kind: "adoption-reconciliation", SourceTag: rel.Source.Name,
				DestinationTag: rel.DestinationTag, PlanHash: plan.Publish.Hash(),
				StateCommit: plan.State.Commit,
			}
			if !opts.Automatic && !opts.Apply {
				report.Actions = append(report.Actions, action)
				return report, nil
			}
			approval := plan.Publish.Hash()
			if !opts.Automatic {
				approval = opts.Approval
			}
			if _, err := ApplyReconciliation(ctx, plan, approval); err != nil {
				return report, err
			}
			action.Applied = true
			report.Actions = append(report.Actions, action)
			if !opts.Automatic {
				return report, nil
			}
			continue
		}

		if discovery.FixedPoint() {
			report.FixedPoint = true
			if opts.Automatic {
				if err := verifyFixedPointWrite(ctx, opts, discovery); err != nil {
					return report, err
				}
				report.WriteVerified = true
			}
			return report, nil
		}
		if len(discovery.Pending) == 0 {
			if discovery.ResolvedAnchor != nil {
				report.NeedsConfiguration = true
				return report, nil
			}
			return report, errors.New("reconciliation: discovery is not at a fixed point and found no pending release or adoption")
		}

		rel := discovery.Pending[0].Source
		if discovery.State.Schema == 0 {
			initialGenerate := opts.Generate
			initialGenerate.Config = opts.Config
			initialGenerate.Ref = extract.Ref{Kind: extract.RefTag, Name: rel.Source.Name}
			initialGenerate.ReleaseContext = ""
			initialGenerate.HistoryAnchor = ""
			initialGenerate.HistoryAnchorRelease = ""
			initialGenerate.StagingSources = nil
			initial, err := Plan(ctx, Options{
				Generate: initialGenerate, Destination: opts.Destination,
			})
			if err != nil {
				return report, fmt.Errorf("reconciliation: initial release: %w", err)
			}
			action := ReconcileAction{
				Kind: "initial-release", SourceTag: rel.Source.Name,
				DestinationTag: rel.DestinationTag, PlanHash: initial.Manifest.Hash,
				StateCommit: initial.State.Commit,
			}
			if !opts.Automatic && !opts.Apply {
				report.Actions = append(report.Actions, action)
				return report, nil
			}
			approval := initial.Manifest.Hash
			if !opts.Automatic {
				approval = opts.Approval
			}
			if _, err := Apply(ctx, initial, ApplyOptions{Approval: approval}); err != nil {
				return report, err
			}
			action.Applied = true
			report.Actions = append(report.Actions, action)
			return report, nil
		}
		if track := completedTrackForRelease(discovery.State, rel); track != nil {
			plan, err := PlanFinal(ctx, FinalizeOptions{
				Config: opts.Config, Discovery: discovery, SourceCache: opts.SourceCache,
				Destination: opts.Destination, Generate: opts.Generate, Release: rel,
			})
			if err != nil {
				return report, err
			}
			action := ReconcileAction{
				Kind: "release", SourceTag: rel.Source.Name,
				DestinationTag: rel.DestinationTag, PlanHash: plan.Manifest.Hash,
				StateCommit: discovery.StateCommit,
			}
			if !opts.Automatic && !opts.Apply {
				report.Actions = append(report.Actions, action)
				return report, nil
			}
			if opts.Automatic {
				applied, err := ApplyTrusted(ctx, plan)
				if err != nil {
					return report, err
				}
				action.StateCommit = applied.State.Commit
			} else {
				if _, err := Apply(ctx, plan, ApplyOptions{Approval: opts.Approval}); err != nil {
					return report, err
				}
			}
			action.Applied = true
			report.Actions = append(report.Actions, action)
			// A consumer push changes the checkout ref a subsequent discovery must
			// start from. Stop after one release; the next workflow checks out that
			// exact reconciled head and handles any later pending release.
			report.FixedPoint = opts.Automatic && len(discovery.Pending) == 1 && discovery.ResolvedAnchor == nil
			return report, nil
		}

		chunk, err := PlanChunk(ctx, ChunkOptions{
			Config: opts.Config, Discovery: discovery, SourceCache: opts.SourceCache,
			Destination: opts.Destination, Generate: opts.Generate,
			Release: rel, Budget: opts.Budget,
		})
		if errors.Is(err, ErrWorkflowBudget) {
			report.BudgetExhausted = true
			return report, nil
		}
		if err != nil {
			return report, err
		}
		action := ReconcileAction{
			Kind: "checkpoint", SourceTag: rel.Source.Name,
			DestinationTag: rel.DestinationTag, PlanHash: chunk.Publish.Hash(),
			StateCommit: chunk.State.Commit, Progress: chunk.Track.Ref,
			Done: chunk.Track.Done, Total: chunk.Track.Total,
		}
		if !opts.Automatic && !opts.Apply {
			report.Actions = append(report.Actions, action)
			return report, nil
		}
		approval := chunk.Publish.Hash()
		if !opts.Automatic {
			approval = opts.Approval
		}
		if _, err := ApplyCheckpoint(ctx, chunk, approval, false); err != nil {
			return report, err
		}
		action.Applied = true
		report.Actions = append(report.Actions, action)
		if !opts.Automatic {
			return report, nil
		}
	}
}

// verifyFixedPointWrite performs an atomic, leased no-op push of the consumer
// branch through the trusted destination runner. The ref cannot move because
// NewObject and ExpectedOld are identical; reaching receive-pack proves the
// job-scoped credential still has write access before an external credential is
// decommissioned or a later release needs a real push.
func verifyFixedPointWrite(ctx context.Context, opts ReconcileOptions, discovery *Discovery) error {
	result, err := VerifyWriteAccess(ctx, WriteVerificationOptions{
		Config: opts.Config, LocalGit: opts.LocalGit, Destination: opts.Destination,
	})
	if err != nil {
		return fmt.Errorf("reconciliation: fixed-point write verification: %w", err)
	}
	if observed := discovery.Observed[result.Ref]; observed != result.Object {
		return fmt.Errorf(
			"reconciliation: fixed-point write verification observed %s at %s, verification used %s",
			result.Ref, observed, result.Object)
	}
	return nil
}

func checkReconcileOptions(opts ReconcileOptions) error {
	switch {
	case opts.Config == nil:
		return errors.New("reconciliation: a profile is required")
	case opts.SourceCache == nil:
		return errors.New("reconciliation: a source cache is required")
	case opts.LocalGit == nil || !opts.LocalGit.IsAnonymous() || !opts.LocalGit.IsNoLazyFetch():
		return errors.New("reconciliation: local destination Git must be anonymous with lazy fetching disabled")
	case opts.Destination.Git == nil:
		return errors.New("reconciliation: a destination Git runner is required")
	case opts.Generate.Fetch && opts.Generate.Offline:
		return errors.New("reconciliation: source fetch and offline mode cannot both be requested")
	case opts.Automatic && (opts.Apply || opts.Approval != ""):
		return errors.New("reconciliation: automatic mode self-approves, so manual apply and approval must be empty")
	case opts.Apply && opts.Approval == "":
		return errors.New("reconciliation: manual apply requires an approval hash")
	case !opts.Apply && opts.Approval != "":
		return errors.New("reconciliation: an approval hash requires manual apply")
	case opts.Automatic && opts.Config.Publication.Mode != config.PublicationModeAutomatic:
		return fmt.Errorf("reconciliation: automatic apply requires publication mode %q", config.PublicationModeAutomatic)
	}
	return nil
}

func discoverForReconcile(ctx context.Context, opts ReconcileOptions, override string) (*Discovery, error) {
	lister := opts.Destination.Lister
	if lister == nil && opts.RemoteGit != nil {
		lister = publish.NewHTTPSRemote(opts.RemoteGit)
	}
	return Discover(ctx, DiscoverOptions{
		Config: opts.Config, LocalGit: opts.LocalGit, RemoteGit: opts.RemoteGit,
		Remote: opts.Destination.Remote, Identity: opts.Destination.Identity,
		AllowLocalRemote: opts.Destination.AllowLocalRemote, Lister: lister,
		SourceCache: opts.SourceCache, StateCommitOverride: override,
	})
}

func adoptedRelease(ctx context.Context, cfg *config.Config, cache *source.Cache, discovery *Discovery) (source.Release, error) {
	anchor := cfg.Source.Refs.AnchorCommit
	if anchor == "" {
		anchor = discovery.State.Anchor.Source
	}
	releases, err := cache.DiscoverReleases(ctx, source.ReleaseOptions{
		Minimum:            cfg.Source.Refs.MinimumRelease,
		IncludePrereleases: cfg.Source.Refs.IncludePrereleases,
		Policy:             cfg.Release.Policy, Anchor: anchor,
	})
	if err != nil {
		return source.Release{}, fmt.Errorf("reconciliation: discover adopted release: %w", err)
	}
	for _, rel := range releases {
		if rel.DestinationTag == discovery.Adopted.Tag && rel.Source.Commit == discovery.Adopted.Source {
			return rel, nil
		}
	}
	return source.Release{}, fmt.Errorf("reconciliation: adopted tag %s from %s is not a discovered source release", discovery.Adopted.Tag, discovery.Adopted.Source)
}
