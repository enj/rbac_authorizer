package typeswap

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Action is the decision for one pairing.
type Action string

const (
	// ActionPruneInternal removes the internal package because nothing
	// retained references it. This is the RBAC outcome and the safest of the
	// three: no retained type is rewritten, so no rewrite can be wrong.
	ActionPruneInternal Action = "prune-internal"
	// ActionRewriteReferences replaces retained references to internal types
	// with the external ones. It is admitted only when all five proofs hold.
	ActionRewriteReferences Action = "rewrite-references"
	// ActionBlocked reports a substitution that could not be proved. The
	// blockers say which proof failed.
	ActionBlocked Action = "blocked"
	// ActionKeepInternal records a profile that chose not to substitute.
	ActionKeepInternal Action = "keep-internal"
)

// Behavior change kinds, as reported.
const (
	// ChangeSchemeRegistration marks an import time registration that pruning
	// removes.
	ChangeSchemeRegistration = "schemeRegistration"
	// ChangeGlobalMutation marks any other import time mutation of package
	// level state.
	ChangeGlobalMutation = "globalMutation"
	// ChangeConversion marks a generated conversion that no longer exists.
	ChangeConversion = "conversion"
)

// ReportSchema is the version of the encoded report's shape.
//
// The report is written into provenance and compared as a fixture, so a field
// rename is a wire change even when it is only a tidy up. Every field therefore
// carries an explicit JSON tag rather than relying on its Go name, and this
// constant moves when the shape does, so a consumer can tell an intended change
// from a silent one.
const ReportSchema = 1

// Result is the complete, deterministic outcome of one type policy analysis.
type Result struct {
	// Schema is ReportSchema at the time of encoding.
	Schema int `json:"schema"`
	// Policy is the configured policy.
	Policy string `json:"policy"`
	// Pairs reports every pairing, sorted by internal package path.
	Pairs []PairReport `json:"pairs"`
}

// PairReport is one pairing's complete record.
type PairReport struct {
	// Internal and External are the paired package paths.
	Internal string `json:"internal"`
	External string `json:"external"`
	// Action is the decision.
	Action Action `json:"action"`
	// Analyses are the five proofs, in a fixed order.
	Analyses []AnalysisReport `json:"analyses"`
	// Evidence is every proof's supporting evidence, sorted and deduplicated.
	Evidence []string `json:"evidence"`
	// Blockers is every proof's blocking finding, sorted and deduplicated. It
	// is empty for any action other than blocked.
	Blockers []string `json:"blockers"`
	// Rewrites are the retained references that a substitution would have to
	// change. For RBAC it is empty, and that emptiness is what makes the
	// action prune-internal rather than rewrite-references.
	Rewrites []Rewrite `json:"rewrites"`
	// BehaviorChanges are the import time effects the change removes. They are
	// reported even when they are harmless, because a change nobody wrote down
	// is a change nobody can test.
	BehaviorChanges []BehaviorChange `json:"behaviorChanges"`
	// PublicAPIDifferences are the caller supplied differences between the
	// pre-prune and post-prune public API. For RBAC it is empty, and that
	// emptiness is what proves the change is invisible rather than what
	// assumes it.
	PublicAPIDifferences []string `json:"publicApiDifferences"`
	// ExternalAlreadyUsed lists the retained packages that already import the
	// external package, sorted. For RBAC this is the proof that the
	// substitution has in effect already happened upstream and the internal
	// package is simply dead.
	ExternalAlreadyUsed []string `json:"externalAlreadyUsed"`
}

// AnalysisReport is one proof's verdict.
type AnalysisReport struct {
	// Name is the proof's name.
	Name string `json:"name"`
	// Passed is the verdict. A proof with blockers never passes.
	Passed bool `json:"passed"`
	// Evidence supports the verdict, sorted.
	Evidence []string `json:"evidence"`
	// Blockers are the findings that refuse it, sorted.
	Blockers []string `json:"blockers"`
}

// Rewrite is one retained reference a substitution would have to change.
type Rewrite struct {
	// Package is the retained package holding the reference.
	Package string `json:"package"`
	// Symbol is the internal symbol referenced.
	Symbol string `json:"symbol"`
	// Replacement is the external symbol that would replace it.
	Replacement string `json:"replacement"`
	// Position locates the reference.
	Position string `json:"position"`
}

// BehaviorChange is one import time effect the change removes.
type BehaviorChange struct {
	// Kind classifies the change.
	Kind string `json:"kind"`
	// Symbol names what performed the effect.
	Symbol string `json:"symbol"`
	// Detail explains what stops happening.
	Detail string `json:"detail"`
	// Position locates it.
	Position string `json:"position"`
	// Observable reports whether the effect can be reached through the
	// generated public API. An observable effect is a blocker; an
	// unobservable one is a documented change that still needs a test.
	Observable bool `json:"observable"`
}

// analysisReport builds one proof's verdict from its findings.
func analysisReport(name string, evidence, blockers []string) AnalysisReport {
	slices.Sort(evidence)
	slices.Sort(blockers)
	return AnalysisReport{
		Name:     name,
		Passed:   len(blockers) == 0,
		Evidence: slices.Compact(evidence),
		Blockers: slices.Compact(blockers),
	}
}

// Pair returns the report for one internal package path.
func (r *Result) Pair(internal string) (PairReport, bool) {
	for _, pair := range r.Pairs {
		if pair.Internal == internal {
			return pair, true
		}
	}
	return PairReport{}, false
}

// Analysis returns one proof's verdict.
func (p PairReport) Analysis(name string) (AnalysisReport, bool) {
	for _, analysis := range p.Analyses {
		if analysis.Name == name {
			return analysis, true
		}
	}
	return AnalysisReport{}, false
}

// FailedAnalyses returns the proofs that did not hold, sorted.
func (p PairReport) FailedAnalyses() []string {
	var failed []string
	for _, analysis := range p.Analyses {
		if !analysis.Passed {
			failed = append(failed, analysis.Name)
		}
	}
	slices.Sort(failed)
	return failed
}

// JSON renders the result as deterministic, indented JSON.
//
// Every slice was sorted when it was built, so the bytes are a fixture a test
// can pin and provenance can carry.
func (r *Result) JSON() ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(r); err != nil {
		return nil, fmt.Errorf("encode type policy result: %w", err)
	}
	return buffer.Bytes(), nil
}

// String renders a short human summary.
func (r *Result) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "type policy %s: %d pairs", r.Policy, len(r.Pairs))
	for _, pair := range r.Pairs {
		fmt.Fprintf(&b, "\n  - %s -> %s: %s", pair.Internal, pair.External, pair.Action)
		for _, blocker := range pair.Blockers {
			fmt.Fprintf(&b, "\n      blocked: %s", blocker)
		}
	}
	return b.String()
}
