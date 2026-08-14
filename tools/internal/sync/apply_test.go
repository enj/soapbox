package sync_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/publish"
	"github.com/enj/soapbox/tools/internal/state"
	"github.com/enj/soapbox/tools/internal/sync"
	"github.com/enj/soapbox/tools/internal/treebuild"
)

// TestApplyPublishesBookkeepingBeforeTheRelease is the successful publication,
// and it checks the order the two pushes happen in as well as their result.
//
// The order is the reason the scopes exist. The state record says where the
// engine got to; the branch and the tag are what a consumer and the module
// proxy resolve. A destination that has the release without the record is one
// the next run cannot account for.
func TestApplyPublishesBookkeepingBeforeTheRelease(t *testing.T) {
	ctx := t.Context()
	dest := newDestination(ctx, t)

	result, err := sync.Project(ctx, dest.options())
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	applied, err := sync.Apply(ctx, result, sync.ApplyOptions{Approval: result.Manifest.Hash})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}

	if applied.NonConsumer == nil || applied.Consumer == nil {
		t.Fatalf("apply reported %+v, want both halves", applied)
	}
	if got, want := applied.NonConsumer.Pushed, []string{testStateRef}; !equal(got, want) {
		t.Errorf("non-consumer pushed %v, want %v", got, want)
	}
	if got, want := applied.Consumer.Pushed, []string{testBranchRef, "refs/tags/" + testReleaseTag}; !equal(got, want) {
		t.Errorf("consumer pushed %v, want %v", got, want)
	}

	refs := dest.remoteRefs(ctx, t)
	for ref, want := range map[string]string{
		testStateRef:                  result.Manifest.Objects.StateCommit,
		testBranchRef:                 result.Manifest.Objects.Commit,
		"refs/tags/" + testReleaseTag: result.Manifest.Objects.Tag,
	} {
		if refs[ref] != want {
			t.Errorf("remote %s = %q, want %q", ref, refs[ref], want)
		}
	}
}

// TestApplyRefusesAnApprovalThatNamesSomethingElse is the gate the whole
// package exists to hold.
func TestApplyRefusesAnApprovalThatNamesSomethingElse(t *testing.T) {
	ctx := t.Context()

	for _, tc := range []struct {
		name     string
		approval func(hash string) string
	}{
		{"no approval at all", func(string) string { return "" }},
		{"a hash of something else", func(string) string { return "sha256:" + strings.Repeat("0", 64) }},
		{"the right hash with a character changed", func(hash string) string {
			return hash[:len(hash)-1] + map[bool]string{true: "0", false: "1"}[strings.HasSuffix(hash, "1")]
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dest := newDestination(ctx, t)
			result, err := sync.Project(ctx, dest.options())
			if err != nil {
				t.Fatalf("project: %v", err)
			}
			if _, err := sync.Apply(ctx, result, sync.ApplyOptions{
				Approval: tc.approval(result.Manifest.Hash),
			}); !errors.Is(err, sync.ErrApproval) {
				t.Fatalf("apply = %v, want a refused approval", err)
			}
			if refs := dest.remoteRefs(ctx, t); len(refs) != 0 {
				t.Errorf("the remote holds %v after a refused approval, want nothing", refs)
			}
		})
	}
}

// TestApplyRefusesAManifestEditedAfterApproval proves the approval is checked
// against the manifest's contents rather than against a field beside them.
//
// An approval compared only against the hash field would match a manifest whose
// actions were rewritten and whose hash was rewritten to suit, which is an
// approval of nothing.
func TestApplyRefusesAManifestEditedAfterApproval(t *testing.T) {
	ctx := t.Context()
	dest := newDestination(ctx, t)

	result, err := sync.Project(ctx, dest.options())
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	result.Manifest.Source.Commit = strings.Repeat("9", 40)

	if _, err := sync.Apply(ctx, result, sync.ApplyOptions{
		Approval: result.Manifest.Hash,
	}); !errors.Is(err, sync.ErrManifestModified) {
		t.Fatalf("apply = %v, want a modified manifest", err)
	}
	if refs := dest.remoteRefs(ctx, t); len(refs) != 0 {
		t.Errorf("the remote holds %v after a modified manifest, want nothing", refs)
	}
}

// TestApplyDryRunPushesNothing proves a rehearsal reads the destination and
// stops.
func TestApplyDryRunPushesNothing(t *testing.T) {
	ctx := t.Context()
	dest := newDestination(ctx, t)

	result, err := sync.Project(ctx, dest.options())
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	applied, err := sync.Apply(ctx, result, sync.ApplyOptions{
		Approval: result.Manifest.Hash,
		DryRun:   true,
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !applied.DryRun {
		t.Errorf("apply reported DryRun = false, want true")
	}
	if refs := dest.remoteRefs(ctx, t); len(refs) != 0 {
		t.Errorf("the remote holds %v after a rehearsal, want nothing", refs)
	}
}

// TestRerunningACompletedSynchronizationReachesAFixedPoint is what a scheduled
// workflow does: run again, find nothing upstream moved, and stop.
//
// It takes two reruns to reach silence, and the reason is worth stating. The
// state record reports the destination refs the run OBSERVED, so the first run
// records nothing published, because at the moment it read the destination
// nothing was. The first rerun observes the branch it published and records
// that, which is new information and legitimately advances the state ref by one
// commit. The second rerun observes the same thing, produces a byte identical
// record, and moves nothing at all.
//
// The alternative would be to record the refs the run intends to publish. That
// reads as a fixed point one run sooner and is a lie: the record would be
// pushed by the non-consumer half before the consumer half had run, and a
// consumer push that then failed would leave a published record asserting that
// a release landed when it had not.
func TestRerunningACompletedSynchronizationReachesAFixedPoint(t *testing.T) {
	ctx := t.Context()
	dest := newDestination(ctx, t)

	first, err := sync.Project(ctx, dest.options())
	if err != nil {
		t.Fatalf("first project: %v", err)
	}
	if _, err := sync.Apply(ctx, first, sync.ApplyOptions{Approval: first.Manifest.Hash}); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	// A first synchronization observed an empty destination, so its record
	// claims nothing was published. That is the property the ordering rests on.
	if published := first.Document.Published; len(published) != 0 {
		t.Errorf("the first record claims %v is published, want nothing: it observed an empty destination", published)
	}

	// The first rerun resumes from the published record, as a scheduled job does,
	// and records both consumer refs it now observes.
	second := dest.rerun(ctx, t, first.Manifest.Objects.StateCommit)
	consumerRefs := map[string]bool{testBranchRef: true, "refs/tags/" + testReleaseTag: true}
	for _, action := range second.Manifest.Publish.Actions {
		if consumerRefs[action.Ref] && action.Effect != publish.EffectNoOp {
			t.Errorf("consumer ref %s effect = %q on a rerun, want %q",
				action.Ref, action.Effect, publish.EffectNoOp)
		}
	}
	if got := len(second.Document.Published); got != 2 {
		t.Errorf("the rerun records %d published refs, want 2: it observed the branch and tag it published", got)
	}
	applied, err := sync.Apply(ctx, second, sync.ApplyOptions{Approval: second.Manifest.Hash})
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if len(applied.Consumer.Pushed) != 0 {
		t.Errorf("a rerun pushed consumer refs %v, want none", applied.Consumer.Pushed)
	}

	// The second rerun observes exactly what the first one recorded, so it
	// produces the same record and moves nothing.
	before := dest.remoteRefs(ctx, t)
	third := dest.rerun(ctx, t, second.Manifest.Objects.StateCommit)
	for _, action := range third.Manifest.Publish.Actions {
		if action.Effect != publish.EffectNoOp {
			t.Errorf("ref %s effect = %q at the fixed point, want %q",
				action.Ref, action.Effect, publish.EffectNoOp)
		}
	}
	if third.Manifest.Objects.StateCommit != second.Manifest.Objects.StateCommit {
		t.Errorf("the record moved at the fixed point: %s then %s",
			second.Manifest.Objects.StateCommit, third.Manifest.Objects.StateCommit)
	}
	applied, err = sync.Apply(ctx, third, sync.ApplyOptions{Approval: third.Manifest.Hash})
	if err != nil {
		t.Fatalf("third apply: %v", err)
	}
	if len(applied.NonConsumer.Pushed) != 0 || len(applied.Consumer.Pushed) != 0 {
		t.Errorf("the fixed point pushed %v and %v, want nothing",
			applied.NonConsumer.Pushed, applied.Consumer.Pushed)
	}
	if after := dest.remoteRefs(ctx, t); !sameRefs(before, after) {
		t.Errorf("the remote moved at the fixed point:\n before %v\n after  %v", before, after)
	}
}

// rerun plans one more synchronization resuming from a published record.
func (d *destination) rerun(ctx context.Context, t *testing.T, stateCommit string) *sync.Result {
	t.Helper()
	opts := d.options()
	opts.StateCommit = stateCommit
	result, err := sync.Project(ctx, opts)
	if err != nil {
		t.Fatalf("rerun from %s: %v", stateCommit, err)
	}
	return result
}

// TestProjectRefusesAReleaseTagTheDestinationAlreadyHolds proves a published tag
// never moves.
//
// A consumer and the module proxy have already resolved that tag. Republishing
// it under different content is the one failure an append only publisher exists
// to make impossible, and it is caught while planning rather than while pushing.
func TestProjectRefusesAReleaseTagTheDestinationAlreadyHolds(t *testing.T) {
	ctx := t.Context()
	dest := newDestination(ctx, t)
	dest.seedRemoteRef(ctx, t, "refs/tags/"+testReleaseTag)

	_, err := sync.Project(ctx, dest.options())
	if !errors.Is(err, publish.ErrTagMoved) {
		t.Fatalf("project = %v, want a refused tag move", err)
	}
}

// TestApplyRefusesADestinationThatMovedAfterPlanning proves an approval is not
// a licence to publish onto a repository nobody looked at.
//
// A plan is a statement about a remote at the moment it was read, and approval
// takes human time. Publishing an approved plan onto a remote that has moved
// would publish something nobody approved.
func TestApplyRefusesADestinationThatMovedAfterPlanning(t *testing.T) {
	ctx := t.Context()
	dest := newDestination(ctx, t)

	result, err := sync.Project(ctx, dest.options())
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	// The branch appears between the plan and the apply, which is exactly the
	// window an approval opens.
	dest.seedRemoteRef(ctx, t, testBranchRef)

	applied, err := sync.Apply(ctx, result, sync.ApplyOptions{Approval: result.Manifest.Hash})
	if !errors.Is(err, publish.ErrRemoteDrift) {
		t.Fatalf("apply = %v, want a refused drift", err)
	}
	// The bookkeeping half went first and had no reason to fail, so the record
	// is published and the release is not. That is the resumable outcome the
	// ordering was chosen for, and the result says so rather than reporting the
	// whole apply as though nothing happened.
	if applied == nil || applied.NonConsumer == nil {
		t.Fatalf("apply reported %+v, want the non-consumer half", applied)
	}
	if applied.Consumer != nil {
		t.Errorf("apply reported a consumer result %+v, want none", applied.Consumer)
	}
	refs := dest.remoteRefs(ctx, t)
	if refs[testStateRef] != result.Manifest.Objects.StateCommit {
		t.Errorf("remote %s = %q, want the published record %q",
			testStateRef, refs[testStateRef], result.Manifest.Objects.StateCommit)
	}
	if _, ok := refs["refs/tags/"+testReleaseTag]; ok {
		t.Errorf("the release tag was published despite the drift")
	}

	// This is the assertion the ordering has to earn. The record is now
	// published and the release is not, so the record must not claim otherwise.
	// A record listing the consumer branch as published would be read by the
	// next run as proof that a release landed, and no later gate could catch it,
	// because the record is internally consistent and correctly signed for.
	stored, err := state.Load(ctx, dest.git, result.Manifest.Objects.StateCommit)
	if err != nil {
		t.Fatalf("load the published record: %v", err)
	}
	for _, published := range stored.Published {
		t.Errorf("the published record claims %s is at %s, and the consumer push never happened",
			published.Ref, published.Object)
	}
}

// TestProjectRefusesAResumeItCannotHonestlyPerform proves the engine fails
// closed on the work it has not implemented.
//
// Publishing a second release means transforming the commits between the
// recorded anchor and this one and attaching them to published history. This
// engine transforms one release, so it says so rather than producing a history
// with a hole in it.
func TestProjectRefusesAResumeItCannotHonestlyPerform(t *testing.T) {
	ctx := t.Context()
	dest := newDestination(ctx, t)

	first, err := sync.Project(ctx, dest.options())
	if err != nil {
		t.Fatalf("project: %v", err)
	}

	for _, tc := range []struct {
		name   string
		mutate func(*sync.ProjectOptions)
	}{{
		name: "a different upstream release",
		mutate: func(o *sync.ProjectOptions) {
			o.Release.Tag = "v1.36.2"
			o.Release.Ref = "refs/tags/v1.36.2"
			o.Release.Commit = strings.Repeat("2", 40)
			// The report has to describe the same release, or the run is refused
			// for describing two of them before it ever reaches the resume.
			report := o.Module.Report
			report.Source.RefName = "v1.36.2"
			report.Source.Commit = o.Release.Commit
			report.Source.ReleaseTag = "v0.36.2"
			o.Module.Report = report
		},
	}, {
		name: "a profile that moved",
		mutate: func(o *sync.ProjectOptions) {
			report := o.Module.Report
			report.Engine.ProfileHash = "sha256:" + strings.Repeat("e", 64)
			o.Module.Report = report
		},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			opts := dest.options()
			opts.StateCommit = first.Manifest.Objects.StateCommit
			tc.mutate(&opts)
			if _, err := sync.Project(ctx, opts); !errors.Is(err, sync.ErrUnsupported) {
				t.Fatalf("project = %v, want an unsupported resume", err)
			}
		})
	}
}

// seedRemoteRef points one destination ref at a commit this engine did not
// write, which is how a test stands in for a repository somebody else touched.
func (d *destination) seedRemoteRef(ctx context.Context, t *testing.T, ref string) {
	t.Helper()
	remote, err := d.git.WithDir(d.remote)
	if err != nil {
		t.Fatalf("open the remote: %v", err)
	}
	empty, err := remote.EmptyTree(ctx)
	if err != nil {
		t.Fatalf("empty tree: %v", err)
	}
	signature := gitcli.Signature{Name: "Somebody Else", Email: "else@invalid", Date: "1600000000 +0000"}
	commit, err := treebuild.WriteSyntheticCommit(ctx, remote, treebuild.SyntheticCommitOptions{
		Tree:      empty,
		Author:    signature,
		Committer: signature,
		Message:   "a commit this engine did not write\n",
	})
	if err != nil {
		t.Fatalf("write a foreign commit: %v", err)
	}
	if err := remote.CreateRef(ctx, ref, commit); err != nil {
		t.Fatalf("create %s: %v", ref, err)
	}
}

func equal(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func sameRefs(before, after map[string]string) bool {
	if len(before) != len(after) {
		return false
	}
	for ref, object := range before {
		if after[ref] != object {
			return false
		}
	}
	return true
}
