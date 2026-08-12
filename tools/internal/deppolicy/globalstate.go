package deppolicy

import (
	"context"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"slices"

	"github.com/enj/soapbox/tools/internal/gostate"
)

// stateFinding is one piece of package level state that does not survive
// relocation.
type stateFinding struct {
	// Candidate is the import path of the package holding the state.
	Candidate string
	// Kind classifies the finding, using the stable kind constants.
	Kind string
	// Symbol names what was found: a called registration, a declared type, or
	// a package scope variable.
	Symbol string
	// Reason explains what breaks, in terms of the consequence.
	Reason string
	// Position locates the finding in the source.
	Position string
}

// evaluateGlobalState runs the global state gate for every candidate.
//
// Relocation duplicates package level state rather than sharing it, and every
// failure that follows is silent. A copied context key is a different key, so a
// value the real package stored reads as absent, which callers treat as "no
// request info" rather than as an error. A copied feature gate keeps its
// defaults while the operator believes a flag changed it. A copied metrics
// registration lands in a registry nothing scrapes. None of these fail to
// compile and none of them log anything, so the scan refuses on suspicion. A
// package that is a genuinely pure leaf utility, which is the only kind of
// package this policy would ever copy, has none of these shapes at all.
func (d *Decider) evaluateGlobalState(ctx context.Context, graph *Graph) (map[string][]stateFinding, error) {
	byCandidate := make(map[string][]stateFinding, len(graph.Candidates))
	for _, candidate := range graph.Candidates {
		// A cancelled scan returns the cancellation, never the findings it got
		// as far as collecting. A partial map would be indistinguishable from a
		// complete one that found nothing, and the caller would read a
		// cancelled run as a passed gate.
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("scan global state: %w", err)
		}
		pkg := candidate.Package
		findings := scanGlobalState(graph.Fset, pkg)
		slices.SortFunc(findings, compareStateFindings)
		byCandidate[pkg.ImportPath] = findings
	}
	return byCandidate, nil
}

// scanGlobalState collects every global state finding in one package.
func scanGlobalState(fset *token.FileSet, pkg *Package) []stateFinding {
	var findings []stateFinding
	at := func(pos token.Pos) string {
		if fset == nil || !pos.IsValid() {
			return ""
		}
		return fset.Position(pos).String()
	}

	// A denied path fires on the package itself. These are the packages that
	// hold the state other packages mutate, so the split that relocation
	// creates is invisible in their own source and no call scan would find it.
	if rule, ok := deniedPath(pkg.ImportPath); ok {
		findings = append(findings, stateFinding{
			Candidate: pkg.ImportPath,
			Kind:      rule.Kind,
			Symbol:    pkg.ImportPath,
			Reason:    rule.Reason,
		})
	}

	findings = append(findings, scanDeniedReferences(pkg, at)...)
	findings = append(findings, scanSingletons(pkg, at)...)
	findings = append(findings, scanInitFunctions(pkg, at)...)
	findings = append(findings, scanContextKeys(pkg, at)...)
	return findings
}

// scanDeniedReferences reports every use of a symbol in the deny registry.
//
// The scan reads resolved objects rather than source text, so a locally defined
// function named MustRegister does not match, a dot imported real one still
// does, and an import alias changes nothing. Uses are considered rather than
// calls alone because some of these symbols are variables: touching
// utilfeature.DefaultFeatureGate at all is the finding, whether it is read,
// written, or passed along.
func scanDeniedReferences(pkg *Package, at func(token.Pos) string) []stateFinding {
	if pkg.Info == nil {
		return nil
	}
	var findings []stateFinding
	seen := make(map[string]bool)
	for ident, object := range pkg.Info.Uses {
		if object == nil || object.Pkg() == nil {
			continue
		}
		rule, ok := deniedCall(object.Pkg().Path(), object.Name())
		if !ok {
			continue
		}
		symbol := object.Pkg().Path() + "." + object.Name()
		position := at(ident.Pos())
		// One finding per symbol per position keeps a loop that calls the same
		// registration twice from producing two identical lines.
		key := symbol + "@" + position
		if seen[key] {
			continue
		}
		seen[key] = true
		findings = append(findings, stateFinding{
			Candidate: pkg.ImportPath,
			Kind:      rule.Kind,
			Symbol:    symbol,
			Reason:    rule.Reason,
			Position:  position,
		})
	}
	return findings
}

// scanSingletons reports exported package scope variables.
//
// Every exported variable is shared state, whatever its type. A map or a slice
// can be mutated in place, but a string, an int, and a bool can all be
// reassigned by any importer, so the distinction the scan used to draw between
// "mutable" and "basic" types was not a distinction about mutability at all: it
// only asked whether the value could be changed without rebinding it. Both
// change state, and relocation splits both, so both are reported.
//
// Sentinel errors are the one documented exception. An exported error created
// at its declaration is used for comparison rather than as state, and including
// them would bury the real findings under every package's ErrNotFound. The
// exception is narrow on purpose: it recognises the error interface exactly,
// not "anything that looks constant".
func scanSingletons(pkg *Package, at func(token.Pos) string) []stateFinding {
	if pkg.Types == nil {
		return nil
	}
	var findings []stateFinding
	scope := pkg.Types.Scope()
	for _, name := range scope.Names() {
		variable, ok := scope.Lookup(name).(*types.Var)
		if !ok {
			continue
		}
		reason, isState := gostate.ExportedGlobal(variable)
		if !isState {
			continue
		}
		findings = append(findings, stateFinding{
			Candidate: pkg.ImportPath,
			Kind:      kindSingleton,
			Symbol:    pkg.ImportPath + "." + name,
			Reason:    reason,
			Position:  at(variable.Pos()),
		})
	}
	return findings
}

// scanInitFunctions reports every init function in a candidate.
//
// The bar for copying is a pure leaf utility, and a pure leaf utility has no
// reason to run code at import time. Rather than guess which init bodies are
// harmless, the scan reports all of them and distinguishes the two cases in the
// reason, so an operator sees whether the init reaches into another package or
// only arranges the package's own state.
func scanInitFunctions(pkg *Package, at func(token.Pos) string) []stateFinding {
	var findings []stateFinding
	for _, file := range pkg.Syntax {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Name == nil || fn.Name.Name != "init" {
				continue
			}
			reason := "an init function runs at import time, and relocation makes it run against the generated module's copy of the package state rather than the real one"
			if foreign := foreignCall(pkg, fn); foreign != "" {
				reason = "an init function calls " + foreign + " at import time, which registers into state the relocated copy does not share"
			}
			findings = append(findings, stateFinding{
				Candidate: pkg.ImportPath,
				Kind:      kindInit,
				Symbol:    pkg.ImportPath + ".init",
				Reason:    reason,
				Position:  at(fn.Pos()),
			})
		}
	}
	return findings
}

// foreignCall returns the first call in a function body that leaves the
// package, in source order, or the empty string.
func foreignCall(pkg *Package, fn *ast.FuncDecl) string {
	if pkg.Info == nil || fn.Body == nil {
		return ""
	}
	var found string
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		if found != "" {
			return false
		}
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		object := calledObject(pkg.Info, call)
		if object == nil || object.Pkg() == nil || object.Pkg().Path() == pkg.ImportPath {
			return true
		}
		found = object.Pkg().Path() + "." + object.Name()
		return false
	})
	return found
}

// scanContextKeys reports unexported types used as context keys.
//
// A context value is keyed by the dynamic type of the key, so the key's
// identity is the type's identity. Relocating the type produces a key that
// matches nothing the real package stored, and the read returns the zero value
// with ok false, which every caller in this tree treats as "the value was not
// set" rather than as a failure. That is the exact shape of a bug that ships.
func scanContextKeys(pkg *Package, at func(token.Pos) string) []stateFinding {
	if pkg.Info == nil {
		return nil
	}
	var findings []stateFinding
	seen := make(map[string]bool)
	for _, file := range pkg.Syntax {
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			object := calledObject(pkg.Info, call)
			if object == nil || object.Pkg() == nil ||
				object.Pkg().Path() != "context" || object.Name() != "WithValue" {
				return true
			}
			if len(call.Args) < 2 {
				return true
			}
			keyType := pkg.Info.TypeOf(call.Args[1])
			named, isNamed := types.Unalias(keyType).(*types.Named)
			if !isNamed || named.Obj() == nil || named.Obj().Pkg() == nil {
				return true
			}
			// Only a key the candidate itself declares is a relocation
			// problem. A key owned by a package that stays external keeps its
			// identity, so storing through it still works.
			if named.Obj().Pkg().Path() != pkg.ImportPath || named.Obj().Exported() {
				return true
			}
			symbol := pkg.ImportPath + "." + named.Obj().Name()
			if seen[symbol] {
				return true
			}
			seen[symbol] = true
			findings = append(findings, stateFinding{
				Candidate: pkg.ImportPath,
				Kind:      kindContextKey,
				Symbol:    symbol,
				Reason:    "a context value is keyed by the key's type identity, so a relocated unexported key type reads back as absent rather than failing",
				Position:  at(named.Obj().Pos()),
			})
			return true
		})
	}
	return findings
}

// calledObject resolves the function or method a call expression invokes.
func calledObject(info *types.Info, call *ast.CallExpr) types.Object {
	switch fun := ast.Unparen(call.Fun).(type) {
	case *ast.Ident:
		return info.Uses[fun]
	case *ast.SelectorExpr:
		return info.Uses[fun.Sel]
	case *ast.IndexExpr:
		// A generic instantiation, for example Register[T](x).
		return calledObject(info, &ast.CallExpr{Fun: fun.X})
	case *ast.IndexListExpr:
		return calledObject(info, &ast.CallExpr{Fun: fun.X})
	default:
		return nil
	}
}

// compareStateFindings orders findings so a report is byte stable.
func compareStateFindings(a, b stateFinding) int {
	if c := compareStrings(a.Kind, b.Kind); c != 0 {
		return c
	}
	if c := compareStrings(a.Symbol, b.Symbol); c != 0 {
		return c
	}
	return compareStrings(a.Position, b.Position)
}
