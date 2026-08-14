package publish

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/enj/soapbox/tools/internal/gitcli"
)

// Scope selects which half of a plan one push carries.
//
// Ordinary publication keeps the two halves separate. Consumer refs move only
// after every gate above this package has passed, while progress and state refs
// are written between backfill chunks. The one exception is post-publication
// reconciliation: state advances in an atomic push that also sends unchanged
// consumer/progress refspecs solely to lease the observations state records.
type Scope string

// The publication scopes.
const (
	// ScopeConsumer carries the branches and tags a module user can see.
	ScopeConsumer Scope = "consumer"
	// ScopeNonConsumer carries progress and state refs, which no consumer and
	// no module proxy reads.
	ScopeNonConsumer Scope = "non-consumer"
	// ScopeReconcile atomically advances state while leasing no-op consumer and
	// progress observations. It never permits a consumer ref to move.
	ScopeReconcile Scope = "reconcile"
)

// covers reports whether an action belongs to this scope.
func (s Scope) covers(action Action) bool {
	switch s {
	case ScopeConsumer:
		return action.Consumer
	case ScopeNonConsumer:
		return !action.Consumer
	case ScopeReconcile:
		return true
	default:
		return false
	}
}

// valid reports whether s is a supported publication scope.
func (s Scope) valid() bool {
	return s == ScopeConsumer || s == ScopeNonConsumer || s == ScopeReconcile
}

// ApplyOptions configures one execution of an approved plan.
type ApplyOptions struct {
	// Approval is the manifest hash the caller approved after its own gates
	// passed. It is required, and it is the only way this package learns that
	// publication was authorized: the hash covers every action, so an approval
	// that still matches is an approval of exactly these refs, these old
	// objects, and these new ones.
	Approval string
	// Scope selects the half of the plan to push. It is required rather than
	// defaulted, because the safe default and the useful default are opposites:
	// a caller that forgot to say would either never publish or would publish a
	// release it meant to defer.
	Scope Scope
	// DryRun performs every validation and every remote read, and stops before
	// the push. It is how a run proves a publication would succeed without
	// performing it.
	DryRun bool
}

// Result reports what one Apply did.
type Result struct {
	// Scope is the half of the plan this apply carried.
	Scope Scope
	// Actions are the plan's actions in that scope, in manifest order.
	Actions []Action
	// Pushed names the refs the push carried, sorted by ref.
	Pushed []string
	// NoOps names the refs that already held the planned object, sorted by ref.
	NoOps []string
	// DryRun reports that nothing was pushed because the caller asked for a
	// rehearsal.
	DryRun bool
	// Verified reports that the destination was read after the push and holds
	// every planned object. A push that git accepted is not yet a publication
	// that happened, so the two are reported separately.
	Verified bool
}

// PushError reports a push that failed, and what the destination holds now.
//
// Git's push is atomic, so a rejected update should leave every ref untouched.
// "Should" is doing work in that sentence: a connection that drops after the
// remote committed the transaction fails locally and succeeds remotely. Rather
// than report an outcome it cannot know, this reads the destination again and
// states what is actually there.
type PushError struct {
	// Scope is the half of the plan that was being pushed.
	Scope Scope
	// Attempted names every ref in the push.
	Attempted []string
	// Applied names the refs the destination now holds at the planned object.
	Applied []string
	// Unapplied names the refs it does not.
	Unapplied []string
	// Verified reports whether the destination could be read after the failure.
	// When it is false the two lists above are empty and unknown rather than
	// empty and confirmed.
	Verified bool
	// Err is the underlying failure.
	Err error
}

// Error renders the failure and what is known about its effect.
func (e *PushError) Error() string {
	outcome := "the destination could not be read afterwards"
	switch {
	case !e.Verified:
	case len(e.Applied) == 0:
		outcome = "no ref changed"
	default:
		outcome = fmt.Sprintf("%s already changed", strings.Join(e.Applied, ", "))
	}
	return fmt.Sprintf("push %s refs %s: %s: %v", string(e.Scope), strings.Join(e.Attempted, ", "), outcome, e.Err)
}

// Unwrap exposes the underlying failure.
func (e *PushError) Unwrap() error { return e.Err }

// Apply executes an approved plan with one atomic push.
//
// Every check the plan already made is made again here, against a fresh read of
// the destination. That is not defensive duplication: a plan is a statement
// about a remote at the moment it was read, an approval takes human time, and
// publishing an approved plan onto a remote that has moved would publish
// something nobody approved. A destination that drifted stops publication and
// says which ref moved.
//
// The push sends object names rather than local ref names, so what reaches the
// destination is exactly what the manifest describes even if a local branch
// moved while the approval was pending.
func (p *Publisher) Apply(ctx context.Context, plan *Plan, opts ApplyOptions) (*Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("publication apply: %w", err)
	}
	if plan == nil {
		return nil, errors.New("publication apply: a plan is required")
	}
	if !opts.Scope.valid() {
		return nil, fmt.Errorf("publication apply: scope %q must be %s, %s, or %s", string(opts.Scope), string(ScopeConsumer), string(ScopeNonConsumer), string(ScopeReconcile))
	}
	if err := plan.Manifest.Verify(); err != nil {
		return nil, fmt.Errorf("publication apply: %w", err)
	}
	if opts.Approval == "" {
		return nil, fmt.Errorf("publication apply: %w: no approval was given", ErrApproval)
	}
	if opts.Approval != plan.Manifest.Hash {
		return nil, fmt.Errorf("publication apply: %w: it approves %s and the plan is %s", ErrApproval, opts.Approval, plan.Manifest.Hash)
	}
	if plan.Manifest.Remote != p.identity {
		return nil, fmt.Errorf("publication apply: %w: it names %s and this publisher targets %s", ErrScopeMismatch, plan.Manifest.Remote, p.identity)
	}
	if plan.Manifest.ObjectFormat != string(p.format) {
		return nil, fmt.Errorf("publication apply: %w: it names object format %s and this publisher reads %s", ErrScopeMismatch, plan.Manifest.ObjectFormat, string(p.format))
	}
	if opts.Scope == ScopeReconcile {
		for _, action := range plan.Manifest.Actions {
			if action.Consumer && action.Effect != EffectNoOp {
				return nil, fmt.Errorf("publication apply: reconciliation cannot move consumer ref %s with effect %s", action.Ref, action.Effect)
			}
		}
	}

	result := &Result{Scope: opts.Scope, Actions: plan.Actions(opts.Scope), DryRun: opts.DryRun}
	if len(result.Actions) == 0 {
		return result, nil
	}
	observed, err := p.remoteRefs(ctx)
	if err != nil {
		return nil, fmt.Errorf("publication apply: %w", err)
	}
	if err := p.checkDrift(result.Actions, observed); err != nil {
		return nil, fmt.Errorf("publication apply: %w", err)
	}

	var pending []Action
	for _, action := range result.Actions {
		if action.Effect == EffectNoOp {
			result.NoOps = append(result.NoOps, action.Ref)
			if opts.Scope != ScopeReconcile {
				continue
			}
		}
		pending = append(pending, action)
	}
	// Ordinary scopes omit no-op refs. Reconciliation deliberately sends them
	// with leases in the same atomic push as state, so state can never record a
	// consumer or progress observation that changed between the read and push.
	if len(pending) == 0 {
		// Everything was already published. Nothing is sent, and the absence of
		// a push is the correct outcome rather than a skipped one.
		result.Verified = true
		return result, nil
	}
	if err := p.recheck(ctx, pending); err != nil {
		return nil, fmt.Errorf("publication apply: %w", err)
	}
	if opts.DryRun {
		return result, nil
	}

	for _, action := range pending {
		result.Pushed = append(result.Pushed, action.Ref)
	}
	if err := p.push(ctx, opts.Scope, pending); err != nil {
		return nil, fmt.Errorf("publication apply: %w", err)
	}
	result.Verified = true
	return result, nil
}

// push performs the atomic push and confirms the destination afterwards.
//
// Every update carries the value the manifest recorded, so the destination
// performs a compare and swap rather than a fast forward check. That is what
// makes the read above load bearing rather than advisory: a fast forward check
// would accept a push straight over a commit that appeared after the read, and
// the approval named the value that was read. The comparison has to happen
// where the ref is locked, not here.
func (p *Publisher) push(ctx context.Context, scope Scope, pending []Action) error {
	updates := make([]gitcli.PushUpdate, 0, len(pending))
	for _, action := range pending {
		updates = append(updates, gitcli.PushUpdate{
			Ref:          action.Ref,
			New:          action.NewObject,
			ExpectedOld:  action.OldObject,
			ExpectAbsent: action.OldObject == "",
		})
	}
	pushErr := p.git.PushAtomic(ctx, p.remote, updates)
	// The destination is read after the push either way. On failure it is the
	// only honest source for what happened; on success it is the difference
	// between git reporting that it sent the refs and the destination holding
	// them.
	after, readErr := p.remoteRefs(ctx)
	if pushErr != nil {
		failure := &PushError{Scope: scope, Attempted: refNames(pending), Verified: readErr == nil, Err: pushErr}
		if readErr == nil {
			failure.Applied, failure.Unapplied = partitionApplied(pending, after)
		}
		return failure
	}
	if readErr != nil {
		return fmt.Errorf("confirm published refs: %w", readErr)
	}
	if _, unapplied := partitionApplied(pending, after); len(unapplied) > 0 {
		return fmt.Errorf("push reported success and %s does not hold the published object", strings.Join(unapplied, ", "))
	}
	return nil
}

// checkDrift compares a fresh read of the destination with what the plan
// recorded, ref by ref.
func (p *Publisher) checkDrift(actions []Action, observed map[string]string) error {
	for _, action := range actions {
		current, present := observed[action.Ref]
		switch {
		case !present && action.OldObject != "":
			return fmt.Errorf("%q: %w: it held %s and now has no such ref", action.Ref, ErrRemoteDrift, action.OldObject)
		case present && action.OldObject == "":
			return fmt.Errorf("%q: %w: it was absent and now holds %s", action.Ref, ErrRemoteDrift, current)
		case present && current != action.OldObject:
			return fmt.Errorf("%q: %w: it held %s and now holds %s", action.Ref, ErrRemoteDrift, action.OldObject, current)
		}
	}
	return nil
}

// recheck proves again, immediately before the push, that the local repository
// can still support every pending action.
//
// The objects are the same objects the plan named, so this cannot disagree with
// the plan about history. What it can catch is the repository losing them: a
// prune, a re-created cache, or a different repository than the one that was
// planned against would each leave a plan that reads correctly and a push that
// sends nothing.
func (p *Publisher) recheck(ctx context.Context, pending []Action) error {
	var revisions []string
	for _, action := range pending {
		revisions = append(revisions, action.NewObject)
		if action.OldObject != "" {
			revisions = append(revisions, action.OldObject)
		}
	}
	infos, err := p.git.ObjectInfoBatch(ctx, gitcli.ObjectInfoOptions{Revisions: revisions})
	if err != nil {
		return fmt.Errorf("describe publication objects: %w", err)
	}
	if len(infos) != len(revisions) {
		return fmt.Errorf("describe publication objects: got %d records, want %d", len(infos), len(revisions))
	}
	objects := make(map[string]gitcli.ObjectInfo, len(revisions))
	for i, info := range infos {
		objects[revisions[i]] = info
	}
	for _, action := range pending {
		if err := p.requireObject(action.NewObject, objects, wantType(action.Kind)); err != nil {
			return fmt.Errorf("%q: new object: %w", action.Ref, err)
		}
		if action.Effect != EffectFastForward {
			continue
		}
		if err := p.requireObject(action.OldObject, objects, ""); err != nil {
			return fmt.Errorf("%q: the remote value cannot be proved an ancestor: %w", action.Ref, err)
		}
		forward, err := p.git.IsAncestor(ctx, action.OldObject, action.NewObject)
		if err != nil {
			return fmt.Errorf("%q: %w", action.Ref, err)
		}
		if !forward {
			return fmt.Errorf("%q: %w: %s does not descend from %s", action.Ref, ErrNonFastForward, action.NewObject, action.OldObject)
		}
	}
	return nil
}

// partitionApplied splits pending actions by whether the destination holds the
// planned object.
func partitionApplied(pending []Action, observed map[string]string) (applied, unapplied []string) {
	for _, action := range pending {
		if observed[action.Ref] == action.NewObject {
			applied = append(applied, action.Ref)
			continue
		}
		unapplied = append(unapplied, action.Ref)
	}
	return applied, unapplied
}

// refNames lists the destination refs of actions in order.
func refNames(actions []Action) []string {
	names := make([]string, 0, len(actions))
	for _, action := range actions {
		names = append(names, action.Ref)
	}
	return names
}
