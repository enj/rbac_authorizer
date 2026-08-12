// Package typeswap proves that an internal Kubernetes API type can be replaced
// by its public counterpart, or that the internal package can simply be pruned.
//
// The policy this implements is prefer-external, and the load bearing word is
// prove. Replacing k8s.io/kubernetes/pkg/apis/rbac with k8s.io/api/rbac/v1 is
// not a rename: the two declarations are different Go types, they are wire
// compatible only because generated conversions say so, and the internal
// package does things at import time that the public one does not. A textual
// rewrite that got any of that wrong would produce a module that compiles,
// passes its tests, and silently drops a field or stops registering a type.
//
// So substitution is treated as a proof obligation with five parts, and all
// five have to hold:
//
//  1. Markers. An upstream generator directive must name the pairing. The
//     engine does not decide that two packages describe the same API because
//     their types have the same names; upstream already recorded that decision
//     and the analysis reads it.
//  2. Conversion shape. The generated conversion functions must be field
//     preserving. A body that only assigns, casts, reinterprets through
//     unsafe.Pointer, or calls a nested conversion is mechanical. A body with a
//     loop that transforms, a conditional that drops a value, or a call to
//     anything else is hand written logic, and hand written logic means the two
//     types are not the same shape however similar they look.
//  3. Method sets. Every retained use of an internal type has to keep working,
//     which means the external type needs the same exported methods with the
//     same signatures.
//  4. Field identity. Recursively, the two types must agree on field names,
//     field order, field types, JSON tags, and protobuf tags. Order and tags
//     matter because these types are serialized: a reordered protobuf tag is a
//     wire incompatibility that no compiler will catch.
//  5. Global effects. Whatever the internal package does at import time, such
//     as registering into a scheme, either has to be unobservable through the
//     generated public API or has to be recorded as a documented behavior
//     change with a test.
//
// For RBAC the answer is the simplest of the three: no retained type is
// rewritten at all, because the retained code already uses k8s.io/api/rbac/v1.
// The internal package is pruned rather than substituted, and what the analysis
// proves is that pruning it is safe. The scheme registration that pruning
// removes is a real behavior change, so it is reported as one rather than
// omitted because it happens to be harmless here.
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

// analyzePair runs the five proofs for one pair and decides the action.
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

	// The effect inventory is walked once and shared. It parses every file of
	// the internal package, and running it twice made the report and the
	// analysis two independent walks that could in principle disagree.
	changes := inventoryEffects(graph, internal, uses)
	report.Analyses = []AnalysisReport{
		analyzeMarkers(graph, pair),
		analyzeConversions(graph, pair),
		analyzeMethodSets(graph, internal, external, uses),
		analyzeFieldIdentity(graph, internal, external),
		analyzeGlobalEffects(internal, changes),
	}
	report.BehaviorChanges = changes
	report.ExternalAlreadyUsed = externalImporters(graph, pair.External)
	report.PublicAPIDifferences = slices.Clone(graph.PublicAPIDifferences)

	for _, analysis := range report.Analyses {
		report.Evidence = append(report.Evidence, analysis.Evidence...)
		report.Blockers = append(report.Blockers, analysis.Blockers...)
	}
	// A public API difference blocks regardless of which proof passed. The
	// module is published under immutable tags, so a consumer's build breaking
	// is not something the five proofs can outvote.
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
