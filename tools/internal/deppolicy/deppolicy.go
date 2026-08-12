// Package deppolicy decides whether a staging package may be copied into the
// generated module or must stay an external module dependency.
//
// The default answer is external. A staging module being large is not a reason
// to copy it, and neither is a small compiled subset: the question this package
// answers is not "how much of the dependency do we use" but "what breaks for a
// consumer if we own this code instead of importing it". Copying is allowed
// only for pure leaf utilities whose relocation is invisible to everyone.
//
// Three correctness gates decide that, and none of them can be overridden.
//
// Interoperability. A copied package's types get new identities. A consumer
// that already holds a k8s.io/apiserver/pkg/authorization/authorizer.Attributes
// cannot pass it to a function that now wants the relocated copy of the same
// declaration: to the compiler those are unrelated types. So a candidate may
// not own any type that crosses the generated public boundary. Interfaces are
// the one shape that can survive relocation, because a caller satisfies an
// interface structurally, but only when no candidate-owned type appears
// anywhere in the transitive method set and nothing requires the interface's
// real identity.
//
// Global state. Relocation duplicates package-level state instead of sharing
// it. A copied context key type is a different key, so a value the real
// package stored is invisible to the copy and reads as absent rather than as
// an error. A copied feature gate, scheme, or metrics registry registers into
// a second registry that nothing consults. These failures are silent at
// compile time and at run time, which is why the scan is deliberately
// conservative and refuses on suspicion rather than on proof.
//
// Closure completeness. A copy that leaves one of its own module's packages
// behind would import something that did not move with it, so the generated
// module would not compile. That is not expensive, it is broken, which is why
// it sits with the correctness gates and carries no override.
//
// Diamond. Copying only helps if the original leaves the build. When a
// retained external package still imports the candidate, the consumer compiles
// both the copy and the original, pays for both, and gets two incompatible
// types for one declaration. That is strictly worse than not copying.
//
// Everything else is cost, and cost is scored rather than judged. Copied lines,
// generated files, distinct licences, module zip bytes, native code, security
// critical paths, upstream cadence, and the modules and lines a copy would
// remove are all measured and recorded for every candidate, including
// candidates the correctness gates already refused. An operator can relax a cost
// gate with a justification, an approver, and a Kubernetes minor through which
// the relaxation holds. An expired override fails the run rather than reverting
// to the unrelaxed gate. No override, expired or not, touches a correctness
// gate.
//
// A cost that was never measured is not a cost of zero. Module zip size,
// upstream cadence, and licence identity are facts only the toolchain, Git, and
// a reader of the licence text can supply, so they arrive with a flag saying
// whether they were measured at all, and an unmeasured one refuses its gate.
// No override can relax such a gate either: an override weighs a known cost
// against a justification, and there is nothing to weigh. Licence identity in
// particular is supplied rather than inferred, because a file named LICENSE
// proves only that a file is named LICENSE.
//
// The package is a pure analyzer. It is handed an already loaded, type checked
// Graph and reads only the candidate directories named in it, so it never
// shells out, never resolves a module, and never consults an ambient
// environment. Loading belongs to the caller, which owns the go/packages
// configuration and the explicit environment that goes with it.
//
// That makes the completeness of the Graph a correctness property rather than a
// convenience, so it is validated rather than assumed. A candidate without
// syntax or type information would be scanned for global state, found to have
// none, and approved; an empty build graph would give the diamond gate nothing
// to find and pass every candidate. Both are refused, because on these gates
// the absence of evidence is never evidence of absence.
package deppolicy

import (
	"context"
	"fmt"
	"slices"
)

// Decider evaluates one profile's dependency policy against loaded graphs.
//
// A Decider is immutable once constructed and is safe for concurrent use. It
// holds the validated policy, not the code under analysis, so the same Decider
// can judge the graph of every ref a run walks.
type Decider struct {
	opts Options
}

// New validates the options and returns a Decider bound to them.
//
// Validation is complete and reported at once, because an operator editing a
// profile is better served by every problem than by the first one. Nothing is
// read from disk here: a Decider is cheap, and the module it will judge may not
// be generated yet.
func New(ctx context.Context, opts Options) (*Decider, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("new dependency decider: %w", err)
	}
	normalized := opts.clone()
	normalized.normalize()
	if err := normalized.validate(); err != nil {
		return nil, err
	}
	return &Decider{opts: normalized}, nil
}

// Options returns a copy of the validated, normalized options.
func (d *Decider) Options() Options { return d.opts.clone() }

// Decide evaluates every candidate in the graph and returns the decision.
//
// The order is deliberate. Correctness gates run first and run for every
// candidate, so the report explains a refusal in terms of what would actually
// break rather than in terms of a size limit that happened to trip first. Cost
// is then measured for every candidate, including refused ones, because an
// operator arguing for an override needs the numbers behind the refusal and
// because the fixture that pins this decision is only meaningful if the
// measurements are real.
//
// Decide never fails because a candidate was refused. A refusal is a decision,
// and the caller reads Result.Copy to learn what survived. It fails only when
// the graph cannot support a decision at all, or when the profile itself is
// inconsistent: a proposal naming a package the graph does not contain, or an
// override that expired.
func (d *Decider) Decide(ctx context.Context, graph *Graph) (*Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("decide dependency policy: %w", err)
	}
	if graph == nil {
		return nil, fmt.Errorf("decide dependency policy: %w", ErrGraphMissing)
	}
	if err := graph.validate(); err != nil {
		return nil, err
	}
	if err := d.checkProposals(graph); err != nil {
		return nil, err
	}
	if err := d.checkIdentityRequired(graph); err != nil {
		return nil, err
	}
	overrides, err := d.resolveOverrides(graph)
	if err != nil {
		return nil, err
	}

	owned := ownedPackages(graph.Candidates)

	interop, exhausted := d.evaluateInterop(graph, owned)
	global, err := d.evaluateGlobalState(ctx, graph)
	if err != nil {
		return nil, err
	}
	diamond := d.evaluateDiamond(graph, owned)
	costs, err := d.measureCost(ctx, graph)
	if err != nil {
		return nil, err
	}

	// The decision is two passes because cost is a property of the whole copy
	// rather than of one package in it. Copied lines, generated files, and
	// distinct licences all accumulate: five packages of two hundred lines each
	// are a thousand lines the generated module now owns, and scoring each
	// candidate against the ceiling separately would admit all five while a
	// profile that stated one ceiling of a thousand meant exactly this.
	//
	// So the first pass settles correctness, which is genuinely per candidate,
	// and the second measures the accepted set as one thing.
	correctness := make([]CandidateReport, 0, len(graph.Candidates))
	for _, candidate := range graph.Candidates {
		path := candidate.Package.ImportPath
		report := CandidateReport{
			ImportPath:  path,
			StagingPath: candidate.StagingPath,
			Module:      candidate.Package.Module,
			Proposed:    slices.Contains(d.opts.Proposals, candidate.StagingPath),
			Score:       costs[path],
		}
		report.Gates = []GateReport{
			interopGateReport(interop[path], exhausted),
			globalStateGateReport(global[path]),
			diamondGateReport(diamond[path]),
			closureGateReport(costs[path]),
		}
		correctness = append(correctness, report)
	}

	accepted := acceptedScores(correctness)
	candidates := make([]CandidateReport, 0, len(correctness))
	for _, report := range correctness {
		report.Gates = append(report.Gates,
			d.costGateReports(report.StagingPath, report.Score, accepted, overrides)...)
		report.Action = actionFor(report.Gates)
		candidates = append(candidates, report)
	}
	slices.SortFunc(candidates, func(a, b CandidateReport) int {
		return compareStrings(a.ImportPath, b.ImportPath)
	})

	result := &Result{
		Policy:     d.opts.Policy,
		Candidates: candidates,
		Copy:       copiedStagingPaths(candidates),
	}
	result.Totals = totalScore(candidates)
	return result, nil
}

// acceptedScores aggregates the measurements of the candidates that cleared
// every correctness gate and that the profile actually proposed.
//
// Only those count. A candidate the interoperability gate already refused is
// not going to be copied, so charging its lines against the ceiling would let
// one refused package hide a second one that would otherwise have fit.
func acceptedScores(reports []CandidateReport) aggregate {
	// The "known" flags start true and are narrowed by each accepted candidate,
	// because they mean "every accepted candidate supplied this" and an empty
	// accepted set has nothing missing.
	total := aggregate{
		licenses:         map[string]bool{},
		zipKnown:         true,
		cadenceKnown:     true,
		licensesVerified: true,
	}
	for _, report := range reports {
		if !report.Proposed {
			continue
		}
		refused := false
		for _, gate := range report.Gates {
			if !gate.Passed {
				refused = true
				break
			}
		}
		if refused {
			continue
		}
		total.packages++
		total.lines += report.Score.CopiedLines
		total.generated += report.Score.GeneratedFiles
		total.native += report.Score.NativeFiles + int(boolCount(report.Score.Cgo))
		total.security += int(boolCount(report.Score.SecurityCriticalSegment != ""))
		total.zipBytes += report.Score.ModuleZipBytes
		total.zipKnown = total.zipKnown && report.Score.ZipBytesKnown
		total.cadence = max(total.cadence, report.Score.ReleasesPerMinor)
		total.cadenceKnown = total.cadenceKnown && report.Score.CadenceKnown
		total.licensesVerified = total.licensesVerified && report.Score.LicensesVerified
		for _, identifier := range report.Score.LicenseIdentifiers {
			total.licenses[identifier] = true
		}
		total.modulesRemoved = max(total.modulesRemoved, len(report.Score.ModulesRemoved))
		total.packagesRemoved = max(total.packagesRemoved, report.Score.PackagesRemoved)
		total.linesRemoved = max(total.linesRemoved, report.Score.LinesRemoved)
	}
	return total
}

// aggregate is the accepted copy measured as one thing.
type aggregate struct {
	packages         int
	lines            int
	generated        int
	native           int
	security         int
	zipBytes         int64
	zipKnown         bool
	cadence          int
	cadenceKnown     bool
	licenses         map[string]bool
	licensesVerified bool
	modulesRemoved   int
	packagesRemoved  int
	linesRemoved     int
}

// checkProposals refuses a profile that proposes a package the graph does not
// contain. Silently ignoring the proposal would let an upstream rename turn an
// approved copy into no copy at all, and the module ships under an immutable
// tag.
func (d *Decider) checkProposals(graph *Graph) error {
	known := make(map[string]bool, len(graph.Candidates))
	for _, candidate := range graph.Candidates {
		known[candidate.StagingPath] = true
	}
	for _, proposal := range d.opts.Proposals {
		if !known[proposal] {
			return &CandidateError{StagingPath: proposal, Err: ErrProposalUnknown}
		}
	}
	return nil
}

// checkIdentityRequired refuses an identity requirement naming a package the
// graph does not contain.
//
// These entries come from the facade's interface assertions and are what pins
// an upstream package in place for the diamond gate. A typo, or an upstream
// move, would silently name nothing: the requirement would match no candidate,
// the diamond finding it should have produced would not appear, and a copy
// would be approved precisely because the evidence against it was misspelled.
func (d *Decider) checkIdentityRequired(graph *Graph) error {
	if len(d.opts.IdentityRequired) == 0 {
		return nil
	}
	known := make(map[string]bool, len(graph.Candidates)+len(graph.Build))
	for _, candidate := range graph.Candidates {
		known[candidate.Package.ImportPath] = true
	}
	for _, pkg := range graph.Build {
		known[pkg.ImportPath] = true
	}
	for _, required := range d.opts.IdentityRequired {
		pkgPath, _, ok := splitQualifiedType(required)
		if !ok {
			return &IdentityError{Type: required, Err: ErrIdentityMalformed}
		}
		if !known[pkgPath] {
			return &IdentityError{Type: required, Err: ErrIdentityUnknown}
		}
	}
	return nil
}

// ownedPackages indexes the import paths a copy would take ownership of.
func ownedPackages(candidates []Candidate) map[string]bool {
	owned := make(map[string]bool, len(candidates))
	for _, candidate := range candidates {
		owned[candidate.Package.ImportPath] = true
	}
	return owned
}

// actionFor collapses a candidate's gate reports into one action.
//
// A single failed gate is decisive. There is no weighing of a passed
// correctness gate against a failed one, and no arithmetic that lets a large
// benefit outvote a broken type identity.
func actionFor(gates []GateReport) Action {
	for _, gate := range gates {
		if !gate.Passed {
			return ActionExternal
		}
	}
	return ActionCopy
}

// copiedStagingPaths lists the staging paths the decision approves, sorted.
//
// The result is the copy plan a later phase relocates, so it is expressed in
// staging paths rather than import paths: copied files keep their complete
// upstream relative path below the internal prefix, which is what preserves
// nested Go internal visibility.
func copiedStagingPaths(candidates []CandidateReport) []string {
	var copied []string
	for _, candidate := range candidates {
		if candidate.Action == ActionCopy && candidate.Proposed {
			copied = append(copied, candidate.StagingPath)
		}
	}
	slices.Sort(copied)
	return copied
}
