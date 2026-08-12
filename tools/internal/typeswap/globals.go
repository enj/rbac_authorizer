package typeswap

import (
	"go/ast"
	"go/token"
	"slices"

	"github.com/enj/soapbox/tools/internal/gostate"
)

// schemeSymbols are the names that mean "this package registers itself into a
// process global scheme at import time".
//
// They are matched by name because the Kubernetes API packages all declare
// their own, so there is no single upstream package to match against. That is
// conservative in the right direction: a package that declares its own
// SchemeBuilder is doing the thing this inventory exists to record.
var schemeSymbols = []string{
	"AddToScheme",
	"SchemeBuilder",
	"SchemeGroupVersion",
	"localSchemeBuilder",
}

// analyzeGlobalEffects inventories what stops happening when the internal
// package goes away.
//
// An API package does work at import time. It registers its types into a
// scheme, installs defaulting functions, and publishes a group version. None of
// that is visible in a type signature, so a substitution proved correct on
// types alone can still change what a program does.
//
// The test applied here is observability. An effect that no retained package
// can reach through the generated public API is a real change and is recorded
// as one, so it can be documented and tested rather than discovered. An effect
// that retained code does reach is a blocker, because removing it would change
// behaviour that the module's own API exposes.
func analyzeGlobalEffects(internal *Package, changes []BehaviorChange) AnalysisReport {
	var evidence, blockers []string
	for _, change := range changes {
		line := change.Kind + ": " + change.Symbol
		if change.Position != "" {
			line += " at " + change.Position
		}
		line += "; " + change.Detail
		if change.Observable {
			blockers = append(blockers, line+
				"; retained code reaches this symbol, so removing it would change behaviour the generated API exposes")
			continue
		}
		evidence = append(evidence, line+"; no retained package reaches it, so the change is unobservable through the generated API and is documented rather than blocking")
	}

	if len(changes) == 0 {
		evidence = append(evidence, internal.ImportPath+" performs no import time registration or global mutation")
	}
	return analysisReport(AnalysisGlobalEffects, evidence, blockers)
}

// inventoryEffects collects every import time effect the internal package has,
// sorted.
func inventoryEffects(graph *Graph, internal *Package, uses usageSet) []BehaviorChange {
	var changes []BehaviorChange

	scope := internal.Types.Scope()
	for _, name := range scope.Names() {
		object := scope.Lookup(name)
		if object == nil {
			continue
		}
		switch {
		case slices.Contains(schemeSymbols, name):
			changes = append(changes, BehaviorChange{
				Kind:       ChangeSchemeRegistration,
				Symbol:     internal.ImportPath + "." + name,
				Detail:     "the package registers its types into a scheme at import time, and pruning it means those types are no longer registered",
				Position:   graph.position(object.Pos()),
				Observable: uses.uses(name),
			})
		default:
			reason, isState := gostate.ExportedGlobal(object)
			if !isState {
				continue
			}
			changes = append(changes, BehaviorChange{
				Kind:       ChangeGlobalMutation,
				Symbol:     internal.ImportPath + "." + name,
				Detail:     reason,
				Position:   graph.position(object.Pos()),
				Observable: uses.uses(name),
			})
		}
	}

	for _, file := range internal.Syntax {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Name == nil || fn.Name.Name != "init" {
				continue
			}
			changes = append(changes, BehaviorChange{
				Kind:     ChangeSchemeRegistration,
				Symbol:   internal.ImportPath + ".init",
				Detail:   "an init function runs at import time, and pruning the package stops it running",
				Position: graph.position(fn.Pos()),
				// An init function has no name a retained package can
				// reference, so observability is decided by whether retained
				// code depends on what the init does. Merely importing the
				// package for one of its types is not such a dependency:
				// rewriting that reference to the published type is the whole
				// point of the policy, and treating the incidental import as a
				// blocker would refuse every substitution an API package could
				// ever have. Naming a scheme symbol is a real dependency, and
				// that is what this tests for.
				Observable: usesSchemeSymbol(uses),
			})
		}
	}

	changes = append(changes, initializerEffects(graph, internal, uses)...)

	// A generated conversion that disappears with the package is a change in
	// its own right: code that relied on the scheme being able to convert
	// between the internal and external shapes no longer can.
	for _, marker := range danglingMarkers(graph, internal.ImportPath) {
		changes = append(changes, BehaviorChange{
			Kind:     ChangeConversion,
			Symbol:   "+" + marker.Key + "=" + marker.Value,
			Detail:   "a retained file still carries a generator directive naming the pruned package, so the directive is stripped and recorded in provenance",
			Position: marker.Position,
		})
	}

	slices.SortFunc(changes, func(a, b BehaviorChange) int {
		if c := compareStrings(a.Kind, b.Kind); c != 0 {
			return c
		}
		if c := compareStrings(a.Symbol, b.Symbol); c != 0 {
			return c
		}
		return compareStrings(a.Position, b.Position)
	})
	return changes
}

// initializerEffects reports package level variable initializers that call
// something.
//
// `var _ = registerTypes()` and `var SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)`
// both run at import time and both register, but neither is an init function
// and neither is visible from a scope name alone: the first declares a blank
// identifier that never appears in the scope, and the second looks like an
// ordinary variable until its initializer is read. Scanning only scope names
// and init declarations misses both, which is exactly the registration an API
// package performs.
//
// A call into another package is reported. A call to a function the package
// declares itself is reported too, conservatively, because whether it registers
// anything cannot be decided without following it, and the whole point of this
// inventory is to be complete rather than clever.
func initializerEffects(graph *Graph, internal *Package, uses usageSet) []BehaviorChange {
	if internal.Info == nil {
		return nil
	}
	var changes []BehaviorChange
	for _, file := range internal.Syntax {
		for _, decl := range file.Decls {
			general, ok := decl.(*ast.GenDecl)
			if !ok || general.Tok != token.VAR {
				continue
			}
			for _, spec := range general.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, expression := range value.Values {
					called := initializerCall(internal, expression)
					if called == "" {
						continue
					}
					name := "_"
					if i < len(value.Names) {
						name = value.Names[i].Name
					}
					changes = append(changes, BehaviorChange{
						Kind:   ChangeGlobalMutation,
						Symbol: internal.ImportPath + "." + name,
						Detail: "the package level initializer calls " + called +
							" at import time, and pruning the package stops that call happening",
						Position:   graph.position(value.Pos()),
						Observable: usesSchemeSymbol(uses) || uses.uses(name),
					})
				}
			}
		}
	}
	return changes
}

// initializerCall returns the first function an initializer expression calls,
// or the empty string when it calls nothing.
func initializerCall(pkg *Package, expression ast.Expr) string {
	var found string
	ast.Inspect(expression, func(node ast.Node) bool {
		if found != "" {
			return false
		}
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		// A type conversion is not a call: T(x) computes nothing at import time
		// beyond the value it converts.
		if tv, ok := pkg.Info.Types[call.Fun]; ok && tv.IsType() {
			return true
		}
		object := calledObject(pkg.Info, call)
		if object == nil {
			found = "an unresolved function"
			return false
		}
		if object.Pkg() == nil {
			// A builtin such as make or append allocates and does not register.
			return true
		}
		found = object.Pkg().Path() + "." + object.Name()
		return false
	})
	return found
}

// usesSchemeSymbol reports whether retained code names one of the scheme
// symbols, which is what makes an import time registration something the
// retained code actually depends on rather than something it merely triggers.
func usesSchemeSymbol(uses usageSet) bool {
	for _, symbol := range schemeSymbols {
		if uses.uses(symbol) {
			return true
		}
	}
	return false
}

// ObservableChanges returns the behavior changes retained code can reach.
func (p PairReport) ObservableChanges() []BehaviorChange {
	var observable []BehaviorChange
	for _, change := range p.BehaviorChanges {
		if change.Observable {
			observable = append(observable, change)
		}
	}
	return observable
}

// DocumentedChanges returns the behavior changes that need documenting and a
// test but do not block the substitution.
func (p PairReport) DocumentedChanges() []BehaviorChange {
	var documented []BehaviorChange
	for _, change := range p.BehaviorChanges {
		if !change.Observable {
			documented = append(documented, change)
		}
	}
	return documented
}

// ChangeSummary renders the documented changes as one line per change, which is
// the shape provenance records.
func (p PairReport) ChangeSummary() []string {
	var lines []string
	for _, change := range p.BehaviorChanges {
		lines = append(lines, change.Kind+" "+change.Symbol+": "+change.Detail)
	}
	slices.Sort(lines)
	return slices.Compact(lines)
}
