package publish

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestApplyPublishesAndConfirms(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newDestination(ctx, t, "")
	h := d.buildHistory()

	plan := d.planUpdates(
		branchUpdate(testBranch, h.forward),
		tagUpdate(tagPrefix+"v0.36.1", h.tagBase),
	)
	result := d.apply(plan, ScopeConsumer)

	if !result.Verified {
		t.Error("apply did not confirm the destination after pushing")
	}
	wantPushed := []string{testBranch, tagPrefix + "v0.36.1"}
	if strings.Join(result.Pushed, ",") != strings.Join(wantPushed, ",") {
		t.Errorf("pushed %v, want %v", result.Pushed, wantPushed)
	}
	d.requireRemote(map[string]string{
		testBranch:            h.forward,
		tagPrefix + "v0.36.1": h.tagBase,
	})
}

func TestApplyRepublishesNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newDestination(ctx, t, "")
	h := d.buildHistory()
	d.seed(h.forward+":"+testBranch, h.tagBase+":"+tagPrefix+"v0.36.1")
	published := d.remoteRefs()

	plan := d.planUpdates(
		branchUpdate(testBranch, h.forward),
		tagUpdate(tagPrefix+"v0.36.1", h.tagBase),
	)
	result := d.apply(plan, ScopeConsumer)

	if len(result.Pushed) != 0 {
		t.Errorf("pushed %v, want nothing: a publication that already happened is not a second one", result.Pushed)
	}
	if len(result.NoOps) != 2 {
		t.Errorf("no-ops %v, want both refs", result.NoOps)
	}
	if !result.Verified {
		t.Error("a publication with nothing to do was not reported as verified")
	}
	d.requireRemote(published)
}

func TestApplyPartitionsConsumerAndProgressRefs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newDestination(ctx, t, "")
	h := d.buildHistory()

	plan := d.planUpdates(
		branchUpdate(testBranch, h.forward),
		tagUpdate(tagPrefix+"v0.36.1", h.tagBase),
		Update{Ref: testProgressRef, Kind: KindProgress, NewObject: h.base, Evidence: "backfill:chunk-1"},
		Update{Ref: testStateRef, Kind: KindState, NewObject: h.base, Evidence: "state:cursor"},
	)

	// A long backfill writes its bookkeeping between chunks, while consumer
	// refs stay where the last fully gated run left them.
	internal := d.apply(plan, ScopeNonConsumer)
	wantInternal := []string{testStateRef, testProgressRef}
	if strings.Join(internal.Pushed, ",") != strings.Join(wantInternal, ",") {
		t.Errorf("non-consumer push carried %v, want %v", internal.Pushed, wantInternal)
	}
	d.requireRemote(map[string]string{testStateRef: h.base, testProgressRef: h.base})

	consumer := d.apply(plan, ScopeConsumer)
	wantConsumer := []string{testBranch, tagPrefix + "v0.36.1"}
	if strings.Join(consumer.Pushed, ",") != strings.Join(wantConsumer, ",") {
		t.Errorf("consumer push carried %v, want %v", consumer.Pushed, wantConsumer)
	}
	d.requireRemote(map[string]string{
		testStateRef:          h.base,
		testProgressRef:       h.base,
		testBranch:            h.forward,
		tagPrefix + "v0.36.1": h.tagBase,
	})
}

func TestApplyReconciliationLeasesObservedRefsAtomically(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newDestination(ctx, t, "")
	h := d.buildHistory()
	tagRef := tagPrefix + "v0.36.1"

	initial := d.planUpdates(
		Update{Ref: testBranch, Kind: KindBranch, NewObject: h.base, ExpectAbsent: true, Evidence: "initial branch"},
		Update{Ref: tagRef, Kind: KindTag, NewObject: h.tagBase, ExpectAbsent: true, Evidence: "initial tag"},
		Update{Ref: testProgressRef, Kind: KindProgress, NewObject: h.base, ExpectAbsent: true, Evidence: "initial progress"},
		Update{Ref: testStateRef, Kind: KindState, NewObject: h.base, ExpectAbsent: true, Evidence: "initial state"},
	)
	d.apply(initial, ScopeNonConsumer)
	d.apply(initial, ScopeConsumer)

	reconcile := d.planUpdates(
		Update{Ref: testBranch, Kind: KindBranch, NewObject: h.base, ExpectedOld: h.base, Evidence: "observe branch"},
		Update{Ref: tagRef, Kind: KindTag, NewObject: h.tagBase, ExpectedOld: h.tagBase, Evidence: "observe tag"},
		Update{Ref: testProgressRef, Kind: KindProgress, NewObject: h.base, ExpectedOld: h.base, Evidence: "observe progress"},
		Update{Ref: testStateRef, Kind: KindState, NewObject: h.middle, ExpectedOld: h.base, Evidence: "advance state"},
	)
	result := d.apply(reconcile, ScopeReconcile)
	if len(result.NoOps) != 3 {
		t.Errorf("reconciliation no-ops = %v, want branch, tag, and progress", result.NoOps)
	}
	d.requireRemote(map[string]string{
		testBranch: h.base, tagRef: h.tagBase,
		testProgressRef: h.base, testStateRef: h.middle,
	})

	movingConsumer := d.planUpdates(
		Update{Ref: testBranch, Kind: KindBranch, NewObject: h.forward, ExpectedOld: h.base, Evidence: "move consumer"},
		Update{Ref: testStateRef, Kind: KindState, NewObject: h.forward, ExpectedOld: h.middle, Evidence: "advance state"},
	)
	if _, err := d.pub.Apply(ctx, movingConsumer, ApplyOptions{Approval: movingConsumer.Hash(), Scope: ScopeReconcile}); err == nil || !strings.Contains(err.Error(), "cannot move consumer ref") {
		t.Fatalf("reconciliation moving consumer = %v, want refusal", err)
	}
}

func TestApplyDryRunPublishesNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newDestination(ctx, t, "")
	h := d.buildHistory()
	plan := d.planUpdates(branchUpdate(testBranch, h.forward))

	result, err := d.pub.Apply(ctx, plan, ApplyOptions{Approval: plan.Hash(), Scope: ScopeConsumer, DryRun: true})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	switch {
	case !result.DryRun:
		t.Error("a dry run was not reported as one")
	case len(result.Pushed) != 0:
		t.Errorf("a dry run pushed %v", result.Pushed)
	case result.Verified:
		t.Error("a dry run claimed the destination was confirmed")
	}
	d.requireRemote(map[string]string{})
}

func TestApplyRefusesWithoutMatchingApproval(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		approval func(plan *Plan) string
		mutate   func(plan *Plan)
		wantErr  error
	}{
		{
			name:     "no approval",
			approval: func(*Plan) string { return "" },
			wantErr:  ErrApproval,
		},
		{
			name:     "an approval of something else",
			approval: func(*Plan) string { return "sha256:" + strings.Repeat("0", 64) },
			wantErr:  ErrApproval,
		},
		{
			name:     "an approval of the plan before it was edited",
			approval: func(plan *Plan) string { return plan.Hash() },
			// The approval still names the hash the manifest carries, and the
			// manifest no longer describes its own actions. Only recomputing
			// the hash catches this.
			mutate:  func(plan *Plan) { plan.Manifest.Actions[0].NewObject = strings.Repeat("1", 40) },
			wantErr: ErrManifestModified,
		},
		{
			name:     "an approval naming an edited hash",
			approval: func(*Plan) string { return "sha256:" + strings.Repeat("1", 64) },
			mutate:   func(plan *Plan) { plan.Manifest.Hash = "sha256:" + strings.Repeat("1", 64) },
			wantErr:  ErrManifestModified,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			d := newDestination(ctx, t, "")
			h := d.buildHistory()
			plan := d.planUpdates(branchUpdate(testBranch, h.forward))

			approval := test.approval(plan)
			if test.mutate != nil {
				test.mutate(plan)
			}
			_, err := d.pub.Apply(ctx, plan, ApplyOptions{Approval: approval, Scope: ScopeConsumer})
			if err == nil {
				t.Fatal("apply succeeded, want a refusal")
			}
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("apply error = %v, want %v", err, test.wantErr)
			}
			d.requireRemote(map[string]string{})
		})
	}
}

func TestApplyRefusesAPlanForAnotherDestination(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newDestination(ctx, t, "")
	h := d.buildHistory()

	other := d.publisher(Options{Identity: "github.com/enj/other_module"})
	plan, err := other.Plan(ctx, []Update{branchUpdate(testBranch, h.forward)})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	_, err = d.pub.Apply(ctx, plan, ApplyOptions{Approval: plan.Hash(), Scope: ScopeConsumer})
	if err == nil {
		t.Fatal("apply accepted a plan made for another destination")
	}
	if !errors.Is(err, ErrScopeMismatch) {
		t.Fatalf("apply error = %v, want %v", err, ErrScopeMismatch)
	}
	d.requireRemote(map[string]string{})
}

func TestApplyRequiresAnExplicitScope(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newDestination(ctx, t, "")
	h := d.buildHistory()
	plan := d.planUpdates(branchUpdate(testBranch, h.forward))

	_, err := d.pub.Apply(ctx, plan, ApplyOptions{Approval: plan.Hash()})
	if err == nil {
		t.Fatal("apply without a scope succeeded")
	}
	if !strings.Contains(err.Error(), "scope") {
		t.Fatalf("apply error = %v, want it to name the missing scope", err)
	}
	d.requireRemote(map[string]string{})
}

func TestApplyRefusesRemoteDrift(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// arrange sets up the destination and returns the plan to approve.
		arrange func(d *destination, h history) *Plan
		// drift moves the destination after the plan was made.
		drift func(d *destination, h history)
		want  string
	}{
		{
			name: "the branch advanced under the plan",
			arrange: func(d *destination, h history) *Plan {
				d.seed(h.base + ":" + testBranch)
				return d.planUpdates(branchUpdate(testBranch, h.forward))
			},
			// Another writer reached the planned commit first. The result is
			// the object the plan wanted, and the plan still described a
			// destination that no longer exists.
			drift: func(d *destination, h history) { d.seed(h.forward + ":" + testBranch) },
			want:  "now holds",
		},
		{
			name: "the branch appeared under a create",
			arrange: func(d *destination, h history) *Plan {
				return d.planUpdates(branchUpdate(testBranch, h.forward))
			},
			drift: func(d *destination, h history) { d.seed(h.divergent + ":" + testBranch) },
			want:  "it was absent and now holds",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			d := newDestination(ctx, t, "")
			h := d.buildHistory()

			plan := test.arrange(d, h)
			test.drift(d, h)
			published := d.remoteRefs()

			_, err := d.pub.Apply(ctx, plan, ApplyOptions{Approval: plan.Hash(), Scope: ScopeConsumer})
			if err == nil {
				t.Fatal("apply succeeded against a destination that moved")
			}
			if !errors.Is(err, ErrRemoteDrift) {
				t.Fatalf("apply error = %v, want %v", err, ErrRemoteDrift)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("apply error = %v, want it to state %q", err, test.want)
			}
			d.requireRemote(published)
		})
	}
}

func TestApplyLeavesNothingBehindWhenThePushIsRejected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newDestination(ctx, t, "")
	h := d.buildHistory()
	d.seed(h.base + ":" + testBranch)
	published := d.remoteRefs()

	// The destination cannot hold both refs/heads/main and a ref beneath it, and
	// nothing local can know that: the conflict lives in the destination's ref
	// store. The push is therefore rejected as a whole, which is exactly the
	// case the atomic push exists for.
	conflicting := branchUpdate(testBranch+"/next", h.forward)
	plan := d.planUpdates(conflicting, branchUpdate("refs/heads/other", h.forward))

	_, err := d.pub.Apply(ctx, plan, ApplyOptions{Approval: plan.Hash(), Scope: ScopeConsumer})
	if err == nil {
		t.Fatal("a rejected push was reported as a publication")
	}
	var pushErr *PushError
	if !errors.As(err, &pushErr) {
		t.Fatalf("apply error = %v, want a *PushError", err)
	}
	switch {
	case !pushErr.Verified:
		t.Error("the destination could not be read after the rejection")
	case len(pushErr.Applied) != 0:
		t.Errorf("applied %v, want nothing: the push is atomic", pushErr.Applied)
	case len(pushErr.Unapplied) != 2:
		t.Errorf("unapplied %v, want both refs", pushErr.Unapplied)
	case pushErr.Scope != ScopeConsumer:
		t.Errorf("scope = %q, want %q", string(pushErr.Scope), string(ScopeConsumer))
	}
	d.requireRemote(published)
}

func TestPublicationHonoursCancellation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newDestination(ctx, t, "")
	h := d.buildHistory()
	plan := d.planUpdates(branchUpdate(testBranch, h.forward))

	canceled, cancel := context.WithCancel(ctx)
	cancel()

	if _, err := d.pub.Plan(canceled, []Update{branchUpdate(testBranch, h.forward)}); !errors.Is(err, context.Canceled) {
		t.Errorf("plan error = %v, want %v", err, context.Canceled)
	}
	_, err := d.pub.Apply(canceled, plan, ApplyOptions{Approval: plan.Hash(), Scope: ScopeConsumer})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("apply error = %v, want %v", err, context.Canceled)
	}
	d.requireRemote(map[string]string{})
}

func TestApplyPublishesOnlyWhatTheManifestNames(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newDestination(ctx, t, "")
	h := d.buildHistory()
	plan := d.planUpdates(branchUpdate(testBranch, h.base))

	// The local branch moves on while the approval is pending. The push sends
	// object names rather than ref names, so the approved commit is published
	// and the later one is not.
	d.rewind(h.base)
	later := d.commit("later.txt", "later", "later")
	d.apply(plan, ScopeConsumer)

	d.requireRemote(map[string]string{testBranch: h.base})
	if later == h.base {
		t.Fatal("the fixture did not move the local branch")
	}
}
