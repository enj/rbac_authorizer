package typeswap

import (
	"errors"
	"fmt"
	"strings"
)

// Type policy failure sentinels.
//
// A blocked pair is not one of these. Being blocked is the expected outcome of
// a proof that does not hold, and it is reported as an action with blockers.
// These report the cases where no analysis is possible at all.
var (
	// ErrInvalidOptions reports a policy that cannot be analyzed as written.
	ErrInvalidOptions = errors.New("type policy is invalid")
	// ErrGraphMissing reports a nil graph.
	ErrGraphMissing = errors.New("type graph is required")
	// ErrPackageMissing reports a pair naming a package the graph does not
	// contain. An upstream move must fail the run rather than silently
	// analyzing nothing and reporting that nothing blocks the substitution.
	ErrPackageMissing = errors.New("paired package is not in the graph")
)

// OptionsError reports every structural problem found in one policy or graph.
type OptionsError struct {
	Problems []string
}

// Error renders each problem on its own line.
func (e *OptionsError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "invalid type policy: %d problems", len(e.Problems))
	for _, problem := range e.Problems {
		b.WriteString("\n  - ")
		b.WriteString(problem)
	}
	return b.String()
}

// Unwrap reports ErrInvalidOptions so a caller can classify every structural
// failure with one errors.Is.
func (e *OptionsError) Unwrap() error { return ErrInvalidOptions }

// PairError attributes a failure to one internal to external pairing.
type PairError struct {
	// Pair is the pairing the failure belongs to.
	Pair Pair
	// Err is the sentinel or underlying cause.
	Err error
}

// Error renders the pair scoped failure.
func (e *PairError) Error() string {
	return fmt.Sprintf("pair %s to %s: %v", e.Pair.Internal, e.Pair.External, e.Err)
}

// Unwrap exposes the cause so errors.Is can classify it.
func (e *PairError) Unwrap() error { return e.Err }
