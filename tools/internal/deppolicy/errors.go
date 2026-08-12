package deppolicy

import (
	"errors"
	"fmt"
	"strings"
)

// Dependency policy failure sentinels.
//
// A refused candidate is not one of these. Refusal is the expected outcome and
// is reported as a decision, so a caller reads Result to learn what was
// refused and why. These sentinels report the cases where no decision can be
// made at all: a malformed profile, a graph that cannot be judged, an override
// that expired, or a candidate directory that could not be measured.
var (
	// ErrInvalidOptions reports a policy that cannot be evaluated as written.
	ErrInvalidOptions = errors.New("dependency policy is invalid")
	// ErrGraphMissing reports a nil graph.
	ErrGraphMissing = errors.New("dependency graph is required")
	// ErrStagingPathMalformed reports a staging path that is not a clean
	// staging/src relative path.
	ErrStagingPathMalformed = errors.New("staging path is malformed")
	// ErrProposalUnknown reports a proposal naming a package the graph does not
	// contain. An upstream rename must fail the run rather than quietly
	// reducing an approved copy to no copy.
	ErrProposalUnknown = errors.New("proposed staging package is not in the graph")
	// ErrOverrideExpired reports an override whose Kubernetes minor expiry has
	// passed. The run fails rather than reverting to the unrelaxed gate,
	// because reverting would turn a forgotten promise into a silent policy
	// change nobody reviewed.
	ErrOverrideExpired = errors.New("cost gate override expired")
	// ErrOverrideUnused reports an override for a candidate the graph does not
	// contain. An override is a dated promise about a specific package, so one
	// that no longer applies is stale configuration, not a harmless leftover.
	ErrOverrideUnused = errors.New("cost gate override applies to no candidate")
	// ErrIdentityMalformed reports an identity requirement that is not a
	// fully qualified type name.
	ErrIdentityMalformed = errors.New("required type identity is malformed")
	// ErrIdentityUnknown reports an identity requirement naming a package the
	// graph does not contain. A requirement that matches nothing silently
	// removes the diamond evidence it exists to provide.
	ErrIdentityUnknown = errors.New("required type identity names a package that is not in the graph")
	// ErrMeasureFailed reports a candidate whose files could not be read. A
	// cost that cannot be measured is never treated as zero: an unreadable
	// candidate is refused rather than admitted on missing evidence.
	ErrMeasureFailed = errors.New("candidate cost could not be measured")
)

// OptionsError reports every structural problem found in one policy or graph.
//
// Reporting all of them at once is deliberate. An operator editing a profile
// fixes what they are shown, and a validator that stops at the first problem
// turns one edit into several rounds.
type OptionsError struct {
	Problems []string
}

// Error renders each problem on its own line.
func (e *OptionsError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "invalid dependency policy: %d problems", len(e.Problems))
	for _, problem := range e.Problems {
		b.WriteString("\n  - ")
		b.WriteString(problem)
	}
	return b.String()
}

// Unwrap reports ErrInvalidOptions so a caller can classify every structural
// failure with one errors.Is.
func (e *OptionsError) Unwrap() error { return ErrInvalidOptions }

// CandidateError attributes a failure to one candidate staging package.
type CandidateError struct {
	// StagingPath is the candidate the failure belongs to.
	StagingPath string
	// Err is the sentinel or underlying cause.
	Err error
}

// Error renders the candidate scoped failure.
func (e *CandidateError) Error() string {
	return fmt.Sprintf("candidate %s: %v", e.StagingPath, e.Err)
}

// Unwrap exposes the cause so errors.Is can classify it.
func (e *CandidateError) Unwrap() error { return e.Err }

// OverrideError attributes a failure to one cost gate override.
//
// The expiry and the source minor are both carried because the useful question
// on reading this error is not "did it expire" but "by how much", which decides
// whether the answer is to renew the promise or to abandon it.
type OverrideError struct {
	// StagingPath is the candidate the override names.
	StagingPath string
	// Gate is the cost gate it relaxes.
	Gate string
	// Approver recorded the promise, and is named so the run tells an operator
	// who to ask rather than only what expired.
	Approver string
	// ExpiresAfterMinor is the last Kubernetes minor the override applied to.
	ExpiresAfterMinor int
	// SourceMinor is the minor series being transformed.
	SourceMinor int
	// Err is the sentinel cause.
	Err error
}

// Error renders the override scoped failure.
func (e *OverrideError) Error() string {
	return fmt.Sprintf("override %s gate %s approved by %s was good through v1.%d, source is v1.%d: %v",
		e.StagingPath, e.Gate, e.Approver, e.ExpiresAfterMinor, e.SourceMinor, e.Err)
}

// Unwrap exposes the cause so errors.Is can classify it.
func (e *OverrideError) Unwrap() error { return e.Err }

// IdentityError attributes a failure to one required type identity.
type IdentityError struct {
	// Type is the fully qualified type name as configured.
	Type string
	// Err is the sentinel cause.
	Err error
}

// Error renders the identity scoped failure.
func (e *IdentityError) Error() string {
	return fmt.Sprintf("required type identity %s: %v", e.Type, e.Err)
}

// Unwrap exposes the cause so errors.Is can classify it.
func (e *IdentityError) Unwrap() error { return e.Err }
