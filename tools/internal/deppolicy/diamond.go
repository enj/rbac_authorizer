package deppolicy

import (
	"slices"
	"strings"
)

// diamondFinding is one reason a copied package would still be reachable in its
// original form.
type diamondFinding struct {
	// Candidate is the import path that would be relocated.
	Candidate string
	// Importer is the retained package that still reaches the original, or the
	// empty string when the original is pinned by a required type identity
	// rather than by an import.
	Importer string
	// ImporterModule is the module providing the importer.
	ImporterModule string
	// Reason explains why the original cannot leave the build.
	Reason string
}

// evaluateDiamond runs the diamond gate for every candidate.
//
// Copying is only worth anything if the original leaves the consumer's build.
// When something retained still reaches the original, the consumer compiles
// both copies of the same declaration, downloads the module anyway, and now has
// two incompatible types where there was one. That is strictly worse than
// leaving the dependency alone, which is why this gate is a refusal rather than
// a cost.
//
// Two things pin an original in place. A retained package that imports it,
// including a package of the very module being copied from, since copying one
// package of a module does not remove the module. And a type whose real
// identity the generated module requires, because the facade asserting that it
// implements the upstream authorizer.Authorizer is exactly a statement that the
// upstream package must still be there.
func (d *Decider) evaluateDiamond(graph *Graph, owned map[string]bool) map[string][]diamondFinding {
	byCandidate := make(map[string][]diamondFinding, len(graph.Candidates))
	if len(owned) == 0 {
		return byCandidate
	}

	build := slices.Clone(graph.Build)
	slices.SortFunc(build, func(a, b BuildPackage) int {
		return compareStrings(a.ImportPath, b.ImportPath)
	})
	for _, pkg := range build {
		// The generated module's own packages are rewritten to import the
		// relocated copy, so they never keep the original alive.
		if pkg.Module == d.opts.ModulePath {
			continue
		}
		// A copied package importing another copied package is not a diamond:
		// both move together and the relocated pair still refers to itself.
		if owned[pkg.ImportPath] {
			continue
		}
		imports := slices.Clone(pkg.Imports)
		slices.Sort(imports)
		for _, imported := range imports {
			if !owned[imported] {
				continue
			}
			byCandidate[imported] = append(byCandidate[imported], diamondFinding{
				Candidate:      imported,
				Importer:       pkg.ImportPath,
				ImporterModule: pkg.Module,
				Reason:         "the retained package still imports the original, so the consumer build contains both the copy and the package it was copied from",
			})
		}
	}

	for _, required := range d.opts.IdentityRequired {
		pkgPath, typeName, ok := splitQualifiedType(required)
		if !ok || !owned[pkgPath] {
			continue
		}
		byCandidate[pkgPath] = append(byCandidate[pkgPath], diamondFinding{
			Candidate: pkgPath,
			Reason:    "the generated module must satisfy " + typeName + " from this package, so the original cannot leave the build",
		})
	}

	for candidate := range byCandidate {
		slices.SortFunc(byCandidate[candidate], func(a, b diamondFinding) int {
			if c := compareStrings(a.Importer, b.Importer); c != 0 {
				return c
			}
			return compareStrings(a.Reason, b.Reason)
		})
	}
	return byCandidate
}

// splitQualifiedType splits k8s.io/apiserver/pkg/authorization/authorizer.Authorizer
// into its package path and type name.
//
// The last dot separates them because a Go identifier cannot contain one, while
// the path before it can and usually does.
func splitQualifiedType(qualified string) (string, string, bool) {
	index := strings.LastIndex(qualified, ".")
	if index <= 0 || index == len(qualified)-1 {
		return "", "", false
	}
	pkgPath, name := qualified[:index], qualified[index+1:]
	if strings.Contains(name, "/") {
		return "", "", false
	}
	return pkgPath, name, true
}

// modulesRemoved reports which modules would leave the consumer build if every
// candidate were copied.
//
// This is the benefit side of the ledger and it is measured rather than
// asserted. Copying six packages of a module that stays in the build for a
// seventh removes nothing at all, and the report should say so in numbers
// rather than leave an operator to infer it from a diamond finding.
//
// The retained import set is built once. The obvious implementation asks, for
// each package, whether any retained package imports it, which walks the whole
// build graph once per package and is quadratic in a graph whose realistic size
// is a few thousand packages with tens of thousands of edges.
func modulesRemoved(graph *Graph, modulePath string, owned map[string]bool) []string {
	// Every import made by a package that survives the copy. A package in this
	// set is still reachable, so its module cannot leave.
	retainedImports := make(map[string]bool, len(graph.Build))
	for _, pkg := range graph.Build {
		if owned[pkg.ImportPath] || pkg.Module == modulePath {
			// A copied package moves with the copy, and the generated module's
			// own imports of a candidate are rewritten to the relocated path.
			// Neither keeps an original reachable.
			continue
		}
		for _, imported := range pkg.Imports {
			retainedImports[imported] = true
		}
	}

	stillNeeded := make(map[string]bool)
	candidateModules := make(map[string]bool)
	for _, pkg := range graph.Build {
		if pkg.Module == "" || pkg.Module == modulePath {
			continue
		}
		if owned[pkg.ImportPath] {
			candidateModules[pkg.Module] = true
			continue
		}
		if retainedImports[pkg.ImportPath] {
			stillNeeded[pkg.Module] = true
		}
	}

	removed := make([]string, 0, len(candidateModules))
	for module := range candidateModules {
		if !stillNeeded[module] {
			removed = append(removed, module)
		}
	}
	slices.Sort(removed)
	return removed
}
