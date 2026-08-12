package deppolicy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Action is the decision for one candidate.
type Action string

const (
	// ActionExternal keeps the dependency external. It is the default and the
	// outcome of any failed gate.
	ActionExternal Action = "external"
	// ActionCopy admits the candidate into the generated module.
	ActionCopy Action = "copy"
)

// Gate kinds, as reported.
const (
	// KindCorrectness marks a gate no override can relax.
	KindCorrectness = "correctness"
	// KindCost marks a gate an override can relax with a justification, an
	// approver, and an expiry.
	KindCost = "cost"
)

// Result is the complete, deterministic outcome of one dependency decision.
type Result struct {
	// Policy is the configured default action.
	Policy string
	// Candidates reports every candidate the graph offered, sorted by import
	// path, whether or not the profile proposed it. Reporting the unproposed
	// ones is what lets the report answer "why not copy this" without an
	// operator having to propose it and read a failure.
	Candidates []CandidateReport
	// Copy lists the staging paths the decision approves, sorted. For a profile
	// whose answer is external, it is empty, and that emptiness is the decision
	// rather than the absence of one.
	Copy []string
	// Totals summarize the decision.
	Totals Totals
}

// CandidateReport is one candidate's complete record.
type CandidateReport struct {
	// ImportPath is the package's import path.
	ImportPath string
	// StagingPath is its upstream staging/src relative path.
	StagingPath string
	// Module is the module providing it.
	Module string
	// Proposed reports whether the profile asked to copy it.
	Proposed bool
	// Action is the decision.
	Action Action
	// Gates are every gate's verdict, in a fixed order: correctness first,
	// then cost.
	Gates []GateReport
	// Score is every measurement, including for a candidate a correctness gate
	// already refused.
	Score Score
}

// GateReport is one gate's verdict for one candidate.
type GateReport struct {
	// Name is the gate's profile name.
	Name string
	// Kind is KindCorrectness or KindCost.
	Kind string
	// Passed is the verdict.
	Passed bool
	// Measured is what was measured, in Unit.
	Measured int64
	// Limit is the ceiling the measurement was compared against. It is absent
	// for a correctness gate, whose only acceptable measurement is zero.
	Limit int64
	// Unit names what Measured counts.
	Unit string
	// Minimum reports that Limit is a floor rather than a ceiling, so the gate
	// passes when Measured is at least Limit. Without it a reader would have to
	// know which gates invert, and a benefit gate reading "9 exceeds limit 1"
	// would say the opposite of what it means.
	Minimum bool
	// Unmeasured reports that the caller supplied no measurement for this
	// gate. An unmeasured gate never passes and no override can relax it,
	// because an override weighs a known cost against a justification and there
	// is no known cost to weigh.
	Unmeasured bool
	// Evidence explains the verdict, sorted. It is populated for a failure and
	// for a permitted borderline case, so a passing gate that nearly did not
	// pass still says why.
	Evidence []string
	// Override records the relaxation applied, if any. A correctness gate can
	// never carry one.
	Override *OverrideRecord
}

// OverrideRecord is the promise that relaxed a cost gate.
type OverrideRecord struct {
	// Justification records why the cost is acceptable.
	Justification string
	// Approver records who accepted it.
	Approver string
	// ExpiresAfterMinor is the Kubernetes minor after which it stops being
	// believed.
	ExpiresAfterMinor int
}

// Totals summarize one decision.
type Totals struct {
	// Candidates is how many candidates were judged.
	Candidates int
	// Copied is how many the decision admits.
	Copied int
	// Refused is how many it refuses.
	Refused int
	// CopiedLines is how many lines the admitted candidates would add.
	CopiedLines int
	// ModulesRemoved is how many modules would actually leave the build.
	ModulesRemoved int
}

// resolveOverrides indexes the overrides and refuses the expired ones.
//
// An expired override fails the run rather than reverting to the unrelaxed
// gate. Reverting would be the friendlier behaviour and the wrong one: it turns
// a promise nobody renewed into a policy change nobody reviewed, and the module
// ships under an immutable tag.
func (d *Decider) resolveOverrides(graph *Graph) (map[string]Override, error) {
	known := make(map[string]bool, len(graph.Candidates))
	for _, candidate := range graph.Candidates {
		known[candidate.StagingPath] = true
	}

	resolved := make(map[string]Override, len(d.opts.Overrides))
	for _, override := range d.opts.Overrides {
		// An override is good through the minor it names and expires once the
		// source moves past it, so v1.38 still applies while transforming
		// v1.38. Expiring it at equality would silently shorten every promise
		// by one release.
		if override.ExpiresAfterMinor < d.opts.SourceMinor {
			return nil, &OverrideError{
				StagingPath:       override.StagingPath,
				Gate:              override.Gate,
				Approver:          override.Approver,
				ExpiresAfterMinor: override.ExpiresAfterMinor,
				SourceMinor:       d.opts.SourceMinor,
				Err:               ErrOverrideExpired,
			}
		}
		if !known[override.StagingPath] {
			return nil, &CandidateError{StagingPath: override.StagingPath, Err: ErrOverrideUnused}
		}
		resolved[overrideKey(override.StagingPath, override.Gate)] = override
	}
	return resolved, nil
}

// overrideKey identifies one override.
func overrideKey(stagingPath, gate string) string { return stagingPath + "\x00" + gate }

// interopGateReport renders the interoperability verdict.
//
// An incomplete walk refuses just as a finding does. The depth bound cutting a
// path short means the types beyond it were never examined, and on a
// correctness gate an unexamined path cannot count as a clean one.
func interopGateReport(findings []interopFinding, exhausted []string) GateReport {
	report := GateReport{Name: GateInteroperability, Kind: KindCorrectness, Unit: "boundary types"}
	for _, finding := range findings {
		report.Measured++
		report.Evidence = append(report.Evidence, renderInteropFinding(finding))
	}
	for _, path := range exhausted {
		report.Measured++
		report.Evidence = append(report.Evidence,
			"blocks: the type walk hit its depth bound at "+path+
				"; what lies beyond was never examined, so the walk cannot be reported as clean")
	}
	report.Passed = report.Measured == 0
	slices.Sort(report.Evidence)
	return report
}

// renderInteropFinding writes one finding as a line an operator can act on.
func renderInteropFinding(finding interopFinding) string {
	line := fmt.Sprintf("blocks: %s (%s) reached by %s", finding.Type, finding.Shape, finding.Path)
	if finding.Detail != "" {
		line += "; " + finding.Detail
	}
	return line
}

// globalStateGateReport renders the global state verdict.
func globalStateGateReport(findings []stateFinding) GateReport {
	report := GateReport{
		Name:     GateGlobalState,
		Kind:     KindCorrectness,
		Unit:     "state findings",
		Measured: int64(len(findings)),
		Passed:   len(findings) == 0,
	}
	for _, finding := range findings {
		line := fmt.Sprintf("%s: %s", finding.Kind, finding.Symbol)
		if finding.Position != "" {
			line += " at " + finding.Position
		}
		report.Evidence = append(report.Evidence, line+"; "+finding.Reason)
	}
	slices.Sort(report.Evidence)
	return report
}

// diamondGateReport renders the diamond verdict.
func diamondGateReport(findings []diamondFinding) GateReport {
	report := GateReport{
		Name:     GateDiamond,
		Kind:     KindCorrectness,
		Unit:     "retained reachers",
		Measured: int64(len(findings)),
		Passed:   len(findings) == 0,
	}
	for _, finding := range findings {
		line := finding.Reason
		if finding.Importer != "" {
			line = finding.Importer + " imports " + finding.Candidate + "; " + finding.Reason
		}
		report.Evidence = append(report.Evidence, line)
	}
	slices.Sort(report.Evidence)
	return report
}

// closureGateReport renders the closure completeness verdict.
//
// An incomplete closure is a correctness failure rather than a cost: the
// relocated copy would import a package that did not move with it, so the
// generated module would not compile. No justification makes that acceptable,
// which is why this gate carries no override.
func closureGateReport(score Score) GateReport {
	report := GateReport{
		Name:     GateClosureComplete,
		Kind:     KindCorrectness,
		Unit:     "unsatisfied imports",
		Measured: int64(len(score.ClosureGaps)),
		Passed:   len(score.ClosureGaps) == 0,
	}
	for _, gap := range score.ClosureGaps {
		report.Evidence = append(report.Evidence,
			"still imports "+gap+" from its own module, which is not proposed for copying")
	}
	slices.Sort(report.Evidence)
	return report
}

// costGateReports renders every cost gate.
//
// The measurements are those of the whole accepted copy rather than of this one
// candidate, because that is what a ceiling describes. A profile that allows a
// thousand copied lines means a thousand lines in the generated module, not a
// thousand lines per package five times over. The same report is therefore
// attached to every candidate, and it says so.
func (d *Decider) costGateReports(stagingPath string, score Score, total aggregate, overrides map[string]Override) []GateReport {
	ceilings := d.opts.Gates.Cost

	reports := []GateReport{
		{Name: GateCopiedPackages, Unit: "packages across the accepted copy",
			Measured: int64(total.packages), Limit: int64(ceilings.MaxCopiedPackages)},
		{Name: GateCopiedLines, Unit: "lines across the accepted copy",
			Measured: int64(total.lines), Limit: int64(ceilings.MaxCopiedLines)},
		{Name: GateGeneratedFiles, Unit: "generated files across the accepted copy",
			Measured: int64(total.generated), Limit: int64(ceilings.MaxGeneratedFiles)},
		{Name: GateDistinctLicenses, Unit: "distinct verified licences across the accepted copy",
			Measured: int64(len(total.licenses)), Limit: int64(ceilings.MaxDistinctLicenses),
			Unmeasured: !total.licensesVerified},
		{Name: GateModuleZipBytes, Unit: "zip bytes across the accepted copy",
			Measured: total.zipBytes, Limit: ceilings.MaxModuleZipBytes,
			Unmeasured: !total.zipKnown},
		{Name: GateUpstreamCadence, Unit: "releases in the source minor",
			Measured: int64(total.cadence), Limit: int64(ceilings.MaxReleasesPerMinor),
			Unmeasured: !total.cadenceKnown},
		{Name: GateSecurityCritical, Unit: "security critical packages",
			Measured: int64(total.security)},
		{Name: GateNativeCode, Unit: "native inputs",
			Measured: int64(total.native)},
	}
	reports = append(reports, d.leverageGateReport(total))

	for i := range reports {
		if reports[i].Kind == "" {
			reports[i].Kind = KindCost
		}
		if reports[i].Minimum {
			continue
		}
		// An unmeasured cost is never a passing cost. Treating an absent
		// measurement as zero is the one arithmetic that turns missing evidence
		// into approval, which is exactly backwards for a module published
		// under an immutable tag.
		reports[i].Passed = !reports[i].Unmeasured && reports[i].Measured <= reports[i].Limit
		reports[i].Evidence = costEvidence(reports[i].Name, score)
		if reports[i].Unmeasured {
			reports[i].Evidence = append(reports[i].Evidence,
				"the caller supplied no measurement for this gate, so it is refused rather than scored as zero")
			slices.Sort(reports[i].Evidence)
		}
	}
	for i := range reports {
		override, ok := overrides[overrideKey(stagingPath, reports[i].Name)]
		if !ok || reports[i].Unmeasured {
			// An override cannot relax a gate that was never measured, because
			// an override weighs a known cost against a justification and there
			// is nothing to weigh.
			continue
		}
		// An override relaxes the ceiling completely rather than raising it to a
		// new number. A number would be a second ceiling nobody reviewed, while
		// a recorded promise with an approver and an expiry is reviewable and
		// does expire.
		reports[i].Passed = true
		reports[i].Override = &OverrideRecord{
			Justification:     override.Justification,
			Approver:          override.Approver,
			ExpiresAfterMinor: override.ExpiresAfterMinor,
		}
	}
	return reports
}

// leverageGateReport renders the benefit side of the ledger.
//
// This is the gate that asks what the copy is actually for. Copying is only
// justified by removing something from the consumer's build, and the usual
// outcome of copying some packages of a module that stays for the others is
// that nothing leaves at all: the consumer downloads the same module, compiles
// the same packages, and now also compiles the copy. A profile states the
// benefit it expects, and a copy that does not deliver it is refused however
// cheap it looks.
func (d *Decider) leverageGateReport(total aggregate) GateReport {
	ceilings := d.opts.Gates.Cost
	report := GateReport{
		Name:     GateMinimumLeverage,
		Kind:     KindCost,
		Unit:     "modules removed from the consumer build",
		Minimum:  true,
		Measured: int64(total.modulesRemoved),
		Limit:    int64(ceilings.MinModulesRemoved),
	}
	report.Passed = report.Measured >= report.Limit &&
		int64(total.packagesRemoved) >= int64(ceilings.MinPackagesRemoved) &&
		int64(total.linesRemoved) >= int64(ceilings.MinLinesRemoved)
	report.Evidence = []string{fmt.Sprintf(
		"the copy would remove %d modules, %d compiled packages, and %d compiled lines from the consumer build",
		total.modulesRemoved, total.packagesRemoved, total.linesRemoved)}
	if !report.Passed {
		report.Evidence = append(report.Evidence, fmt.Sprintf(
			"the profile requires at least %d modules, %d packages, and %d lines removed, so this copy buys too little to be worth owning",
			ceilings.MinModulesRemoved, ceilings.MinPackagesRemoved, ceilings.MinLinesRemoved))
	}
	slices.Sort(report.Evidence)
	return report
}

// costEvidence explains a cost measurement where the number alone is not
// enough to act on.
func costEvidence(gate string, score Score) []string {
	var evidence []string
	switch gate {
	case GateDistinctLicenses:
		evidence = slices.Clone(score.LicenseIdentifiers)
	case GateSecurityCritical:
		if score.SecurityCriticalSegment != "" {
			evidence = []string{"path segment " + score.SecurityCriticalSegment +
				" means owning a copy also means owning its CVE response, which reaches consumers only when this engine republishes"}
		}
	case GateNativeCode:
		if score.Cgo {
			evidence = append(evidence, `the package imports "C", so the generated module would require a cgo toolchain`)
		}
	}
	slices.Sort(evidence)
	return evidence
}

// boolCount renders a boolean as a measurement.
func boolCount(set bool) int64 {
	if set {
		return 1
	}
	return 0
}

// totalScore summarizes the decision.
func totalScore(candidates []CandidateReport) Totals {
	totals := Totals{Candidates: len(candidates)}
	removed := make(map[string]bool)
	for _, candidate := range candidates {
		if candidate.Action == ActionCopy && candidate.Proposed {
			totals.Copied++
			totals.CopiedLines += candidate.Score.CopiedLines
			for _, module := range candidate.Score.ModulesRemoved {
				removed[module] = true
			}
			continue
		}
		totals.Refused++
	}
	totals.ModulesRemoved = len(removed)
	return totals
}

// FailedGates returns the names of the gates that refused one candidate,
// sorted.
//
// It exists so a caller, a test, or a provenance record can state the reason
// for a refusal without walking the report structure and rediscovering the
// convention that a false Passed is the refusal.
func (c CandidateReport) FailedGates() []string {
	var failed []string
	for _, gate := range c.Gates {
		if !gate.Passed {
			failed = append(failed, gate.Name)
		}
	}
	slices.Sort(failed)
	return failed
}

// Candidate returns the report for one import path.
func (r *Result) Candidate(importPath string) (CandidateReport, bool) {
	for _, candidate := range r.Candidates {
		if candidate.ImportPath == importPath {
			return candidate, true
		}
	}
	return CandidateReport{}, false
}

// JSON renders the result as deterministic, indented JSON.
//
// The encoding is stable because every slice in the structure was sorted when
// it was built, so the bytes are a fixture a test can pin and a provenance
// record can carry. HTML escaping is off because a Go type name containing an
// angle bracket should read as itself rather than as an entity.
func (r *Result) JSON() ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(r); err != nil {
		return nil, fmt.Errorf("encode dependency policy result: %w", err)
	}
	return buffer.Bytes(), nil
}

// String renders a short human summary, used in logs and failure messages.
func (r *Result) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "dependency policy %s: %d candidates, %d copied, %d refused",
		r.Policy, r.Totals.Candidates, r.Totals.Copied, r.Totals.Refused)
	for _, candidate := range r.Candidates {
		failed := candidate.FailedGates()
		if len(failed) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n  - %s refused by %s", candidate.ImportPath, strings.Join(failed, ", "))
	}
	return b.String()
}
