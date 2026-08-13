package sync

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"

	"github.com/enj/soapbox/tools/internal/publish"
)

// ApplyOptions configures one execution of an approved synchronization.
type ApplyOptions struct {
	// Approval is the synchronization manifest hash the operator approved after
	// reading it. It is required, and it is the only way this package learns
	// that publication was authorized: the hash covers the module, the objects,
	// and every ref that would move, so an approval that still matches is an
	// approval of exactly this synchronization.
	Approval string
	// DryRun performs every validation and every remote read and stops before
	// the pushes. It is how a run proves an approved publication would succeed
	// without performing it.
	DryRun bool
}

// Outcome is what one scoped push actually did.
//
// It exists rather than reusing publish.Result because a failed push produces
// no publish.Result at all: publish reports the failure as a PushError that
// carries what the destination holds now. Both shapes have to reach the caller
// as one type, or a caller reading the result would see a nil half and conclude
// that nothing happened, when what actually happened is the question.
type Outcome struct {
	// Scope is the half of the plan this push carried.
	Scope publish.Scope
	// Attempted names every ref the push carried, populated on a failure where
	// the distinction between attempted and applied is the whole answer.
	Attempted []string
	// Pushed names the refs the destination now holds at the planned object.
	Pushed []string
	// Unapplied names the refs it does not, populated on a failure.
	Unapplied []string
	// NoOps names the refs that already held the planned object.
	NoOps []string
	// DryRun reports that nothing was pushed because the caller rehearsed.
	DryRun bool
	// Verified reports that the destination was read afterwards and its contents
	// are known. When it is false the lists above are unknown rather than empty.
	Verified bool
	// Failed reports a push that did not complete. It is separate from the lists
	// because a push can fail having applied everything, which is what a
	// connection dropping after the remote committed looks like.
	Failed bool
}

// ApplyResult reports what one Apply did.
//
// The two halves are reported separately because they are two pushes, and the
// order between them is load bearing rather than incidental. A caller that
// needs to know whether a failure left the release published reads Consumer: a
// nil Consumer after a non-nil NonConsumer says the bookkeeping landed and the
// release was never attempted, and a Consumer with Failed set says it was
// attempted and says which of its refs are now live.
type ApplyResult struct {
	// NonConsumer is the state and progress push: engine bookkeeping no module
	// consumer and no module proxy can see.
	NonConsumer *Outcome
	// Consumer is the branch and tag push: what a module user resolves.
	Consumer *Outcome
	// DryRun reports that nothing was pushed because the caller asked for a
	// rehearsal.
	DryRun bool
}

// Apply publishes an approved synchronization.
//
// The approval is checked against the manifest's own hash, and the manifest is
// checked against its own contents first. Checking the manifest before the
// approval is what makes the check mean anything: an approval compared against
// a hash field that was edited alongside the actions it covers would match a
// manifest nobody read.
//
// The non-consumer half goes first, and the two halves are separate pushes.
// That order is the whole reason the scopes exist. The state record says where
// the engine got to; the branch and the tag are what a consumer and the module
// proxy resolve. Publishing the release first and failing before the record
// lands leaves a destination whose published history the next run cannot
// account for, whereas publishing the record first and failing before the
// release leaves a record of work whose refs simply have not appeared yet,
// which is a resumable state rather than a corrupt one.
//
// A failure in the consumer half is returned with the non-consumer result
// still attached, because "the bookkeeping is published and the release is not"
// is precisely what the operator needs to be told.
func Apply(ctx context.Context, result *Result, opts ApplyOptions) (*ApplyResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("synchronization apply: %w", err)
	}
	if result == nil {
		return nil, fmt.Errorf("synchronization apply: a plan is required")
	}
	if result.publisher == nil || result.Publish == nil {
		return nil, fmt.Errorf("%w: this plan was not built against a destination", ErrPublicationDisabled)
	}
	if err := result.Manifest.Verify(); err != nil {
		return nil, fmt.Errorf("synchronization apply: %w", err)
	}
	if err := checkApproval(opts.Approval, result.Manifest.Hash); err != nil {
		return nil, err
	}

	applied := &ApplyResult{DryRun: opts.DryRun}
	// The publication is approved by the synchronization hash, and the
	// publication manifest's own hash is what publish.Apply checks. It is read
	// off the plan rather than passed through from the caller because the
	// synchronization hash covers it: a plan whose publication half had been
	// swapped would already have failed Verify above.
	publication := publish.ApplyOptions{Approval: result.Publish.Hash(), DryRun: opts.DryRun}

	publication.Scope = publish.ScopeNonConsumer
	nonConsumer, err := result.publisher.Apply(ctx, result.Publish, publication)
	applied.NonConsumer = outcome(publish.ScopeNonConsumer, nonConsumer, err, opts.DryRun)
	if err != nil {
		return applied, fmt.Errorf("synchronization apply: %w", err)
	}

	publication.Scope = publish.ScopeConsumer
	consumer, err := result.publisher.Apply(ctx, result.Publish, publication)
	applied.Consumer = outcome(publish.ScopeConsumer, consumer, err, opts.DryRun)
	if err != nil {
		return applied, fmt.Errorf("synchronization apply: %w", err)
	}
	return applied, nil
}

// outcome reports what one half of a publication actually did.
//
// A failed push is the case this exists for. Git's push is atomic, so a
// rejected update should leave every ref untouched, but a connection that drops
// after the remote committed the transaction fails locally and succeeded
// remotely. publish reads the destination again and states what is really there
// inside a PushError, and it returns no result alongside it. A caller handed
// only the error would be told the half failed and nothing about which refs are
// now published, which is the first question an operator has.
//
// So the error is unwrapped into the same shape a success produces, carrying
// the destination read publish already performed and marked unverified when
// that read could not be made. A failure that carries no PushError, such as a
// refused approval or a drifted remote caught before the push, attempted
// nothing and reports nothing.
func outcome(scope publish.Scope, result *publish.Result, err error, dryRun bool) *Outcome {
	if err == nil {
		if result == nil {
			return nil
		}
		return &Outcome{
			Scope:     result.Scope,
			Attempted: refsOf(result.Actions),
			Pushed:    result.Pushed,
			NoOps:     result.NoOps,
			DryRun:    result.DryRun,
			Verified:  result.Verified,
		}
	}
	var pushErr *publish.PushError
	if !errors.As(err, &pushErr) {
		return nil
	}
	return &Outcome{
		Scope:     scopeOr(pushErr.Scope, scope),
		Attempted: pushErr.Attempted,
		Pushed:    pushErr.Applied,
		Unapplied: pushErr.Unapplied,
		DryRun:    dryRun,
		Verified:  pushErr.Verified,
		Failed:    true,
	}
}

// refsOf names the refs a set of actions covers, in manifest order.
func refsOf(actions []publish.Action) []string {
	refs := make([]string, 0, len(actions))
	for _, action := range actions {
		refs = append(refs, action.Ref)
	}
	return refs
}

// scopeOr prefers the scope the failure reported, falling back to the one that
// was asked for when the failure named none.
func scopeOr(reported, asked publish.Scope) publish.Scope {
	if reported != "" {
		return reported
	}
	return asked
}

// checkApproval compares an approval against the manifest hash it must name.
//
// The comparison is constant time. The value is a digest of a public artifact
// rather than a secret, so the property being protected is not confidentiality:
// it is that this check has exactly one outcome and no timing dependent
// shortcut that a future reader might be tempted to optimize into an early
// return.
func checkApproval(approval, hash string) error {
	if approval == "" {
		return fmt.Errorf("%w: no approval was given, and the manifest hashes to %s", ErrApproval, hash)
	}
	if subtle.ConstantTimeCompare([]byte(approval), []byte(hash)) != 1 {
		return fmt.Errorf("%w: it names %s and the manifest hashes to %s", ErrApproval, approval, hash)
	}
	return nil
}
