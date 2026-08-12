package typeswap

import (
	"go/types"
	"slices"
)

// usage is one retained reference to a symbol of the internal package.
type usage struct {
	// Package is the retained package holding the reference.
	Package string
	// Symbol is the internal symbol's name.
	Symbol string
	// Kind is what the symbol is: type, func, var, or const.
	Kind string
	// Position locates the reference.
	Position string
}

// usageSet is every retained reference to one internal package, sorted.
type usageSet struct {
	// Internal is the package being referenced.
	Internal string
	// Usages are the references, sorted by package, symbol, then position.
	Usages []usage
	// Symbols are the distinct internal symbol names referenced, sorted. This
	// is the set a substitution would have to find counterparts for.
	Symbols []string
	// Packages are the distinct retained packages that reference them, sorted.
	Packages []string
}

// retainedUses collects every reference a retained package makes to the
// internal package.
//
// Only retained packages count, and within them only retained files. A package
// about to be pruned may reference the internal types freely, because neither
// it nor the reference will exist in the generated module, and treating those
// references as obstacles would make every profile that prunes an adapter look
// impossible. The same holds one level down: pkg/apis/rbac/v1 survives for its
// helpers while its generated conversion file is pruned, so the conversions'
// references to internal types must not count either. That is what the graph's
// retained set and pruned file list are for.
//
// Only package scope symbols count. A struct field named Verbs resolves to an
// object whose package is the internal one, but it is not a symbol a
// substitution could name or replace, because the field moves with its type.
// Counting fields would make every conversion function read as a wall of
// retained references to symbols the published package does not declare.
func retainedUses(graph *Graph, internal string) usageSet {
	set := usageSet{Internal: internal}
	for _, pkg := range graph.retainedPackages() {
		if pkg.ImportPath == internal || pkg.Info == nil {
			continue
		}
		for ident, object := range pkg.Info.Uses {
			if object == nil || object.Pkg() == nil || object.Pkg().Path() != internal {
				continue
			}
			if object.Parent() == nil || object.Parent() != object.Pkg().Scope() {
				continue
			}
			position := graph.position(ident.Pos())
			if graph.isPruned(position) {
				continue
			}
			set.Usages = append(set.Usages, usage{
				Package:  pkg.ImportPath,
				Symbol:   object.Name(),
				Kind:     objectKind(object),
				Position: position,
			})
		}
	}

	slices.SortFunc(set.Usages, func(a, b usage) int {
		if c := compareStrings(a.Package, b.Package); c != 0 {
			return c
		}
		if c := compareStrings(a.Symbol, b.Symbol); c != 0 {
			return c
		}
		return compareStrings(a.Position, b.Position)
	})
	for _, use := range set.Usages {
		set.Symbols = append(set.Symbols, use.Symbol)
		set.Packages = append(set.Packages, use.Package)
	}
	slices.Sort(set.Symbols)
	slices.Sort(set.Packages)
	set.Symbols = slices.Compact(set.Symbols)
	set.Packages = slices.Compact(set.Packages)
	return set
}

// rewrites renders the retained references as the edits a substitution would
// have to make.
func (u usageSet) rewrites(external string) []Rewrite {
	rewrites := make([]Rewrite, 0, len(u.Usages))
	for _, use := range u.Usages {
		rewrites = append(rewrites, Rewrite{
			Package:     use.Package,
			Symbol:      u.Internal + "." + use.Symbol,
			Replacement: external + "." + use.Symbol,
			Position:    use.Position,
		})
	}
	return rewrites
}

// uses reports whether a retained package references one internal symbol.
func (u usageSet) uses(symbol string) bool { return slices.Contains(u.Symbols, symbol) }

// objectKind names what a resolved object is, for the report.
func objectKind(object types.Object) string {
	switch object.(type) {
	case *types.TypeName:
		return "type"
	case *types.Func:
		return "func"
	case *types.Var:
		return "var"
	case *types.Const:
		return "const"
	default:
		return "symbol"
	}
}

// externalImporters lists the retained packages that already import the
// external package, sorted.
//
// For RBAC this is the load bearing observation. The retained code was already
// written against k8s.io/api/rbac/v1 upstream, so the substitution this policy
// would perform has in effect already happened and the internal package is
// simply dead code. Recording which packages prove that keeps the conclusion
// checkable rather than asserted.
func externalImporters(graph *Graph, external string) []string {
	var importers []string
	for _, pkg := range graph.retainedPackages() {
		if slices.Contains(pkg.Imports, external) {
			importers = append(importers, pkg.ImportPath)
		}
	}
	slices.Sort(importers)
	return slices.Compact(importers)
}

// exportedNames returns a package's exported scope names, sorted.
func exportedNames(pkg *Package) []string {
	if pkg == nil || pkg.Types == nil {
		return nil
	}
	var names []string
	scope := pkg.Types.Scope()
	for _, name := range scope.Names() {
		if object := scope.Lookup(name); object != nil && object.Exported() {
			names = append(names, name)
		}
	}
	return names
}

// lookupType resolves one exported named type in a package.
func lookupType(pkg *Package, name string) (*types.Named, bool) {
	if pkg == nil || pkg.Types == nil {
		return nil, false
	}
	object := pkg.Types.Scope().Lookup(name)
	typeName, ok := object.(*types.TypeName)
	if !ok || !typeName.Exported() {
		return nil, false
	}
	named, ok := types.Unalias(typeName.Type()).(*types.Named)
	return named, ok
}

// substituting renders a type with the internal package spelled as the
// external one.
//
// This is what makes the two sides comparable at all. The types are different
// by construction, since they live in different packages, so comparing them
// directly would report every field as a mismatch. Rewriting only the package
// qualifier leaves every real difference, in field order, element type, or
// tag, exactly where it was.
func substituting(internal, external string) types.Qualifier {
	return func(pkg *types.Package) string {
		if pkg == nil {
			return ""
		}
		if pkg.Path() == internal {
			return external
		}
		return pkg.Path()
	}
}

// typeString renders a type through a qualifier.
func typeString(typ types.Type, qualifier types.Qualifier) string {
	return types.TypeString(typ, qualifier)
}
