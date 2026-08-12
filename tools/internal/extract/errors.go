package extract

import (
	"context"
	"errors"
	"fmt"
)

// PolicyError reports a plan that ran correctly and found the profile, its
// patches, or the content they select unacceptable.
//
// It exists so the command line can separate the three answers an operator acts
// on differently. A policy failure means the engine worked and the answer is no:
// a prune target upstream renamed, a denied import that came back, a patch that
// no longer applies, a closure past its limit, a file the pinned gofmt would
// reformat. A runtime failure means the engine could not answer at all, and a
// usage failure means the command line was wrong. Only the first is a finding
// about the profile, so only the first exits with the check code that CI reads
// as "review this" rather than "the tool is broken".
type PolicyError struct {
	// Stage names the phase that refused, such as anchor, closure, or patch.
	Stage string
	// Err is the underlying failure.
	Err error
}

// Error renders the stage scoped failure.
func (e *PolicyError) Error() string {
	return fmt.Sprintf("%s: %v", e.Stage, e.Err)
}

// Unwrap exposes the cause so errors.Is and errors.As reach the typed errors the
// lower packages return, such as *closure.LimitError and *patchset.ConflictError.
func (e *PolicyError) Unwrap() error { return e.Err }

// policyf reports a policy failure the engine states itself rather than one it
// received from a lower package.
func policyf(stage, format string, args ...any) error {
	return &PolicyError{Stage: stage, Err: fmt.Errorf(format, args...)}
}

// policyIf wraps a cause as a policy failure only when it really is one.
//
// Cancellation is never a finding about the profile, and neither is a
// filesystem or Git failure, so the caller passes the predicate that recognises
// the failures its phase can attribute to content. Everything else travels on
// unchanged and is reported as a runtime failure.
func policyIf(stage string, err error, content func(error) bool) error {
	switch {
	case err == nil:
		return nil
	case canceled(err):
		return err
	case content(err):
		return &PolicyError{Stage: stage, Err: err}
	default:
		return err
	}
}

// contentPolicy wraps every cause a phase produces as a policy failure except a
// cancellation.
//
// It is for the phases whose failures are content findings as a class rather
// than by sentinel: a patch that no longer applies and a relocation a profile
// made impossible are both answers about the profile. Cancellation still has to
// be excluded, because a context that ended mid-phase surfaces as whatever
// failure the interrupted step reported and says nothing about the content.
func contentPolicy(stage string, err error) error {
	return policyIf(stage, err, func(error) bool { return true })
}

// canceled reports a cause that is, or wraps, the end of a context.
//
// Every classification in this package consults it rather than testing the two
// sentinels inline, because the failure a cancellation arrives as is rarely the
// sentinel itself: patch application reports it as a *patchset.ConflictError at
// the cancel stage, and a subprocess reports it as an exit status. Both unwrap
// to the sentinel, and both must leave the command with the cancellation exit
// code rather than the one CI reads as a finding to review.
func canceled(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// isPolicy reports a failure that is a finding about the profile.
//
// A cancellation is never one, even when a phase wrapped it before this package
// could classify it, so it is excluded here as well as at every wrapping site.
// The two checks are deliberate duplication: this one decides whether a partial
// report is produced at all, and getting it wrong would attach a finding to a
// run that was simply stopped.
func isPolicy(err error) bool {
	var policy *PolicyError
	return !canceled(err) && errors.As(err, &policy)
}

// policyStage reports the stage a policy failure names, empty when the failure
// is not one.
func policyStage(err error) string {
	var policy *PolicyError
	if errors.As(err, &policy) {
		return policy.Stage
	}
	return ""
}
