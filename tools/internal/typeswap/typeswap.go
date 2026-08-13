// Package typeswap proves that an internal Kubernetes API type can be replaced
// by its public counterpart, or that an unreachable internal package can be
// pruned without replacing any type at all.
//
// The policy this implements is prefer-external, and the load-bearing word is
// prove. Replacing k8s.io/kubernetes/pkg/apis/rbac with k8s.io/api/rbac/v1 is
// not a rename: the declarations have distinct Go identities and can differ in
// methods, serialization tags, and import-time behavior. A textual rewrite that
// got any of that wrong could compile while silently changing behavior.
//
// Reachability decides which proof is required. If retained code still names an
// internal symbol, the change is a real substitution and generator markers,
// conversion shape, method sets, recursive field identity, global effects, and
// the generated public API must all permit it. If no retained code names the
// internal package, no value changes type. In that dead-package case the
// internal package must be absent from the retained closure, retained code must
// already import the configured external package, generator markers must confirm
// the intended pair, global effects must be unobservable, and the public facade
// must be unchanged. Conversion, method-set, and field-identity analyses are
// explicitly reported as inapplicable rather than falsely claiming that two
// declarations which are never substituted are interchangeable.
//
// For RBAC the answer is the dead-package case. Retained code already uses
// k8s.io/api/rbac/v1, while Kubernetes' unversioned internal types intentionally
// omit public wire tags and carry helpers the public types do not. The internal
// package is pruned without rewriting a retained reference. Scheme registration
// removed by that pruning is still a real behavior change and is reported rather
// than omitted because it is harmless to this facade.
//
// The package is a pure analyzer over an already loaded, type checked graph. It
// never opens a second worktree and never reads a file the caller did not put
// in the graph, and it never imports the facade.
//
// The pre-prune and post-prune public API comparison is therefore not performed
// here. The facade owns what the generated public API is and already renders
// and diffs its own manifest, so a second manifest and a second differ in this
// package would be two implementations of one question that could disagree, and
// the disagreement would decide whether a module ships. The caller runs that
// comparison and passes the rendered differences in through the graph. What
// this package owns is the consequence: any difference at all blocks the
// substitution, because the whole argument for pruning is that the public API
// does not notice.
package typeswap

import (
	"context"
	"fmt"
	"slices"
)

// Analyzer proves one profile's type policy against loaded graphs.
//
// An Analyzer is immutable once constructed and safe for concurrent use.
type Analyzer struct {
	opts Options
}

// New validates the options and returns an Analyzer bound to them.
func New(ctx context.Context, opts Options) (*Analyzer, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("new type analyzer: %w", err)
	}
	normalized := opts.clone()
	normalized.normalize()
	if err := normalized.validate(); err != nil {
		return nil, err
	}
	return &Analyzer{opts: normalized}, nil
}

// Options returns a copy of the validated, normalized options.
func (a *Analyzer) Options() Options { return a.opts.clone() }

// Analyze runs every proof for every configured pair.
//
// Analyze does not fail because a pair was blocked. Being blocked is an
// answer, and the caller reads Result to learn which proof failed and on what
// evidence. It fails only when the graph cannot support an analysis: a pair
// whose packages are absent, or a graph that is not type checked.
func (a *Analyzer) Analyze(ctx context.Context, graph *Graph) (*Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("analyze type policy: %w", err)
	}
	if graph == nil {
		return nil, fmt.Errorf("analyze type policy: %w", ErrGraphMissing)
	}
	if err := graph.validate(); err != nil {
		return nil, err
	}

	reports := make([]PairReport, 0, len(a.opts.Pairs))
	for _, pair := range a.opts.Pairs {
		report, err := a.analyzePair(ctx, graph, pair)
		if err != nil {
			return nil, err
		}
		reports = append(reports, report)
	}
	slices.SortFunc(reports, func(x, y PairReport) int {
		return compareStrings(x.Internal, y.Internal)
	})
	return &Result{Schema: ReportSchema, Policy: a.opts.Policy, Pairs: reports}, nil
}

// analyzePair runs the reachability proof and every proof applicable to one pair.
func (a *Analyzer) analyzePair(ctx context.Context, graph *Graph, pair Pair) (PairReport, error) {
	if err := ctx.Err(); err != nil {
		return PairReport{}, fmt.Errorf("analyze pair %s: %w", pair.Internal, err)
	}

	internal, ok := graph.lookup(pair.Internal)
	if !ok {
		return PairReport{}, &PairError{Pair: pair, Err: fmt.Errorf("%w: internal package %s", ErrPackageMissing, pair.Internal)}
	}
	external, ok := graph.lookup(pair.External)
	if !ok {
		return PairReport{}, &PairError{Pair: pair, Err: fmt.Errorf("%w: external package %s", ErrPackageMissing, pair.External)}
	}

	report := PairReport{Internal: pair.Internal, External: pair.External}

	// The policy that keeps internal types needs no proof, because it changes
	// nothing. Recording it as an explicit action rather than as an absence
	// keeps a profile's decision visible in the report and in provenance.
	if a.opts.Policy == PolicyKeepInternal {
		report.Action = ActionKeepInternal
		report.Analyses = nil
		return report, nil
	}

	uses := retainedUses(graph, pair.Internal)
	report.Rewrites = uses.rewrites(pair.External)
	report.ExternalAlreadyUsed = externalImporters(graph, pair.External)

	// The effect inventory is walked once and shared. It parses every file of
	// the internal package, and running it twice made the report and the
	// analysis two independent walks that could in principle disagree.
	changes := inventoryEffects(graph, internal, uses)
	report.Analyses = []AnalysisReport{
		analyzeMarkers(graph, pair),
		analyzeReachability(graph, pair, uses, report.ExternalAlreadyUsed),
	}
	if len(report.Rewrites) == 0 {
		// Nothing retained names an internal symbol, so no declaration is
		// replaced. Conversion, method-set, and field identity are still named
		// in the report, but explicitly as inapplicable rather than as a false
		// claim that Kubernetes' internal and external declarations are equal.
		report.Analyses = append(report.Analyses,
			pruningOnlyAnalysis(AnalysisConversions, pair),
			pruningOnlyAnalysis(AnalysisMethodSets, pair),
			pruningOnlyAnalysis(AnalysisFieldIdentity, pair),
		)
	} else {
		report.Analyses = append(report.Analyses,
			analyzeConversions(graph, pair),
			analyzeMethodSets(graph, internal, external, uses),
			analyzeFieldIdentity(graph, internal, external),
		)
	}
	report.Analyses = append(report.Analyses, analyzeGlobalEffects(internal, changes))
	report.BehaviorChanges = changes
	report.PublicAPIDifferences = slices.Clone(graph.PublicAPIDifferences)

	for _, analysis := range report.Analyses {
		report.Evidence = append(report.Evidence, analysis.Evidence...)
		report.Blockers = append(report.Blockers, analysis.Blockers...)
	}
	// A public API difference blocks regardless of which proof passed. The
	// module is published under immutable tags, so a consumer's build breaking
	// is not something the proofs can outvote.
	for _, difference := range report.PublicAPIDifferences {
		report.Blockers = append(report.Blockers,
			"pruning changes the generated public API: "+difference)
	}
	slices.Sort(report.Evidence)
	slices.Sort(report.Blockers)
	report.Evidence = slices.Compact(report.Evidence)
	report.Blockers = slices.Compact(report.Blockers)

	report.Action = decideAction(report)
	return report, nil
}

// decideAction collapses the proofs into one action.
//
// The order is not arbitrary. A blocker decides the outcome regardless of
// anything else, because an unproved substitution is not something the rest of
// the analysis can outvote. With no blockers, the question is only whether any
// retained code still names an internal type: when nothing does, the internal
// package is dead and pruning it is the whole change, which is both simpler
// and safer than rewriting references that do not exist.
// pruningOnlyAnalysis records why a substitution proof is inapplicable without
// claiming the two packages are interchangeable. Kubernetes' internal API types
// may intentionally differ in serialization tags and helper methods from their
// public counterparts; those differences matter when a retained reference is
// rewritten and do not matter when the internal package is unreachable.
func pruningOnlyAnalysis(name string, pair Pair) AnalysisReport {
	subject := name
	switch name {
	case AnalysisConversions:
		subject = "generated conversion bodies"
	case AnalysisMethodSets:
		subject = "method-set identity"
	case AnalysisFieldIdentity:
		subject = "field and serialization identity"
	}
	return analysisReport(name, []string{
		"no retained reference to " + pair.Internal + " is rewritten, so " + subject +
			" is not required to prune the unreachable package; this makes no claim that the declarations are interchangeable",
	}, nil)
}

func decideAction(report PairReport) Action {
	switch {
	case len(report.Blockers) > 0:
		return ActionBlocked
	case len(report.Rewrites) == 0:
		return ActionPruneInternal
	default:
		return ActionRewriteReferences
	}
}
