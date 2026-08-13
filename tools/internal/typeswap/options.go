package typeswap

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// Policy values. Unknown values are rejected rather than ignored, so a profile
// written for a newer engine cannot silently lose meaning.
const (
	// PolicyPreferExternal replaces internal API types with their public
	// counterparts, or prunes the internal package when nothing retained uses
	// it. Every replacement carries a proof.
	PolicyPreferExternal = "prefer-external"
	// PolicyKeepInternal relocates the internal types unchanged.
	PolicyKeepInternal = "keep-internal"
)

// Analysis names, in the order they are reported.
const (
	AnalysisMarkers       = "markers"
	AnalysisReachability  = "reachability"
	AnalysisConversions   = "conversions"
	AnalysisMethodSets    = "methodSets"
	AnalysisFieldIdentity = "fieldIdentity"
	AnalysisGlobalEffects = "globalEffects"
)

// Options configures one type policy analysis.
//
// The struct is deliberately independent of internal/config, for the same
// reason the dependency policy's is: a profile is one way to describe the
// decision, and the fixtures describe it another way.
type Options struct {
	// Policy is PolicyPreferExternal or PolicyKeepInternal.
	Policy string
	// Pairs are the internal to external API package pairings to prove.
	Pairs []Pair
}

// Pair is one internal to external API package pairing.
type Pair struct {
	// Internal is the unversioned internal API package, for example
	// k8s.io/kubernetes/pkg/apis/rbac.
	Internal string
	// External is the published API package, for example k8s.io/api/rbac/v1.
	External string
}

// Graph is the loaded, type checked view of the packages an analysis reads.
//
// The caller loads it. That is what keeps this package free of go/packages and
// of any second worktree: the internal package, the external package, and every
// retained package are all already type checked by the time they arrive.
type Graph struct {
	// Fset positions every file in Packages, so evidence can be located.
	Fset *token.FileSet
	// Packages are every loaded package: both sides of each pair and every
	// package that survives pruning.
	Packages []*Package
	// Retained lists the import paths that survive pruning. It is what
	// separates "some package references this type" from "a package the
	// generated module will actually contain references this type", and the
	// second is the only one that decides anything.
	Retained []string
	// PrunedFiles are the repository relative files the profile removes.
	//
	// They are reported alongside a behavior change so an operator can see
	// which removal caused it, and they scope retained use analysis to the
	// files that survive. Paths are normalised before comparison, so a profile
	// written with "./pkg/apis/rbac/v1/register.go" or a backslash separator
	// matches the same file as the plain slash form rather than silently
	// matching nothing.
	PrunedFiles []string
	// PublicAPIDifferences are the differences between the generated module's
	// public API before and after the change, rendered by the caller.
	//
	// This package deliberately does not compute them. The facade owns what the
	// public API is and already renders and diffs its own manifest, so a second
	// manifest and a second differ here would be two implementations of one
	// question that could disagree. The caller runs facade.Diff and passes the
	// rendered result; a non empty list blocks the substitution, because the
	// whole argument for pruning is that the public API does not notice.
	PublicAPIDifferences []string
}

// Package is one loaded, type checked package.
//
// The shape mirrors what a go/packages load produces, so the adapter at the
// caller's boundary is a field copy.
type Package struct {
	// ImportPath is the package's import path.
	ImportPath string
	// Dir is the directory holding the package's files.
	Dir string
	// Types is the type checked package.
	Types *types.Package
	// Syntax are the parsed files, in the loader's order. They must be parsed
	// with comments, because the marker analysis reads generator directives
	// and a loader that dropped comments would make that proof vacuous.
	Syntax []*ast.File
	// Info carries type information for Syntax.
	Info *types.Info
	// CompiledGoFiles are the files Syntax was parsed from, in the same order.
	//
	// These are the compiled files, not the package's source files. For a cgo
	// package the two differ: go/packages reports the translated output in
	// CompiledGoFiles and the originals in GoFiles, and Syntax is parsed from
	// the former. Naming a marker's file from GoFiles would therefore attribute
	// a directive to the wrong file, or index out of range, whenever the two
	// lists have different lengths.
	CompiledGoFiles []string
	// GoFiles are the package's original Go source files. They are what cgo
	// detection reads, because the translated output no longer imports "C".
	GoFiles []string
	// CgoFiles are the original files that use cgo, as the loader reported
	// them. A non empty list is the authoritative cgo signal.
	CgoFiles []string
	// Imports are the import paths this package imports.
	Imports []string
}

// clone returns a deep copy.
func (o Options) clone() Options {
	out := o
	out.Pairs = slices.Clone(o.Pairs)
	return out
}

// normalize sorts the pairs so two equal option sets produce byte identical
// reports.
func (o *Options) normalize() {
	o.Policy = strings.TrimSpace(o.Policy)
	slices.SortFunc(o.Pairs, func(a, b Pair) int {
		if c := compareStrings(a.Internal, b.Internal); c != 0 {
			return c
		}
		return compareStrings(a.External, b.External)
	})
}

// validate reports every structural problem in one pass.
func (o *Options) validate() error {
	var problems []string
	addf := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	switch o.Policy {
	case PolicyPreferExternal, PolicyKeepInternal:
	default:
		addf("policy: unsupported value %q, want one of %s, %s", o.Policy, PolicyPreferExternal, PolicyKeepInternal)
	}

	seen := make([]string, 0, len(o.Pairs))
	for _, pair := range o.Pairs {
		seen = append(seen, pair.Internal)
		switch {
		case strings.TrimSpace(pair.Internal) == "":
			addf("pairs: an internal package path is required")
		case strings.TrimSpace(pair.External) == "":
			addf("pairs: %q needs an external package path", pair.Internal)
		case pair.Internal == pair.External:
			addf("pairs: %q is paired with itself", pair.Internal)
		}
	}
	for _, duplicate := range duplicates(seen) {
		addf("pairs: duplicate internal package %q", duplicate)
	}

	if len(problems) > 0 {
		return &OptionsError{Problems: problems}
	}
	return nil
}

// validate rejects a graph that cannot support an analysis.
func (g *Graph) validate() error {
	var problems []string
	addf := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	if g.Fset == nil {
		addf("fset: a shared file set is required so evidence can carry positions")
	}
	if len(g.Packages) == 0 {
		addf("packages: at least one loaded package is required")
	}
	seen := make(map[string]bool, len(g.Packages))
	for i, pkg := range g.Packages {
		switch {
		case pkg == nil:
			addf("packages[%d]: package is nil", i)
		case pkg.Types == nil:
			addf("packages[%d]: package %q is not type checked", i, pkg.ImportPath)
		case seen[pkg.ImportPath]:
			addf("packages[%d]: duplicate package %q", i, pkg.ImportPath)
		case len(pkg.CompiledGoFiles) != len(pkg.Syntax):
			// Marker evidence names the file a directive sits in, and the name
			// is taken by index. A list that does not line up with Syntax would
			// silently attribute a directive to the wrong file, so the mismatch
			// is refused rather than indexed around.
			addf("packages[%d]: package %q has %d compiled files and %d parsed files, which cannot be aligned",
				i, pkg.ImportPath, len(pkg.CompiledGoFiles), len(pkg.Syntax))
		default:
			seen[pkg.ImportPath] = true
		}
	}
	for _, retained := range g.Retained {
		if !seen[retained] {
			addf("retained: package %q is not loaded, so its use of internal types cannot be checked", retained)
		}
	}

	if len(problems) > 0 {
		return &OptionsError{Problems: problems}
	}
	return nil
}

// lookup returns one loaded package.
func (g *Graph) lookup(importPath string) (*Package, bool) {
	for _, pkg := range g.Packages {
		if pkg.ImportPath == importPath {
			return pkg, true
		}
	}
	return nil, false
}

// retainedPackages returns the loaded packages that survive pruning, sorted.
//
// A package that is loaded but not retained is deliberately excluded: it is
// about to be pruned, so what it references cannot block anything.
func (g *Graph) retainedPackages() []*Package {
	var kept []*Package
	for _, importPath := range g.Retained {
		if pkg, ok := g.lookup(importPath); ok {
			kept = append(kept, pkg)
		}
	}
	slices.SortFunc(kept, func(a, b *Package) int {
		return compareStrings(a.ImportPath, b.ImportPath)
	})
	return kept
}

// position renders a source position, or the empty string.
func (g *Graph) position(pos token.Pos) string {
	if g.Fset == nil || !pos.IsValid() {
		return ""
	}
	return g.Fset.Position(pos).String()
}

// isPruned reports whether a position lies in a file the profile removes.
//
// Pruning is per file while loading is per package, so a package can survive
// while one of its files does not. pkg/apis/rbac/v1 is exactly that case: its
// helpers are retained and its generated conversion file is pruned. Without
// this check the conversions' references to the internal types would count as
// retained uses, and the analysis would conclude that retained code depends on
// a package the profile is about to delete.
//
// The match is a path suffix because the pruned list is repository relative
// while a position is absolute, and a repository relative path is unique within
// the tree it came from.
func (g *Graph) isPruned(position string) bool {
	if position == "" || len(g.PrunedFiles) == 0 {
		return false
	}
	file := position
	if index := strings.LastIndex(file, ":"); index > 0 {
		// Trim the line and column, which a token.Position appends.
		file = file[:index]
		if index = strings.LastIndex(file, ":"); index > 0 {
			file = file[:index]
		}
	}
	file = path.Clean(filepath.ToSlash(file))
	for _, pruned := range g.PrunedFiles {
		pruned = normalizePruned(pruned)
		if pruned == "" {
			continue
		}
		if file == pruned || strings.HasSuffix(file, "/"+pruned) {
			return true
		}
	}
	return false
}

// normalizePruned reduces a configured prune entry to a clean relative slash
// path.
//
// A prune list is written by hand, so it arrives with whatever separators and
// leading dots the author used. Comparing those verbatim against a position's
// path makes a correctly configured prune silently match nothing, and a prune
// that matches nothing means the file it names is treated as retained, which is
// the direction that turns a removed conversion into a retained dependency.
func normalizePruned(entry string) string {
	cleaned := path.Clean(filepath.ToSlash(strings.TrimSpace(entry)))
	cleaned = strings.TrimPrefix(cleaned, "./")
	if cleaned == "." || cleaned == "/" {
		return ""
	}
	return strings.TrimPrefix(cleaned, "/")
}

// PrunedGeneratedOutputs returns the generator kinds whose output the profile
// removes, sorted.
//
// A directive is dangling not only when the package it names is pruned but also
// when the file it used to generate is. Removing zz_generated.deepcopy.go while
// leaving `+k8s:deepcopy-gen=package` in place records a generator run that can
// no longer be reproduced, and rewrite's DefaultRules removes those markers for
// exactly that reason. The inventory has to see the same thing rewrite does, or
// provenance and the tree disagree.
func (g *Graph) PrunedGeneratedOutputs() []string {
	var kinds []string
	for _, entry := range g.PrunedFiles {
		base := path.Base(normalizePruned(entry))
		for marker, prefix := range generatedOutputs {
			if strings.HasPrefix(base, prefix) {
				kinds = append(kinds, marker)
			}
		}
	}
	slices.Sort(kinds)
	return slices.Compact(kinds)
}

// generatedOutputs maps a generator marker onto the file name prefix its output
// carries upstream.
var generatedOutputs = map[string]string{
	MarkerDeepCopyGen:   "zz_generated.deepcopy",
	MarkerDefaulterGen:  "zz_generated.defaults",
	MarkerConversionGen: "zz_generated.conversion",
	MarkerOpenAPIGen:    "zz_generated.openapi",
	MarkerProtobufGen:   "generated.pb",
}

// duplicates returns each value appearing more than once, sorted.
func duplicates(values []string) []string {
	counts := make(map[string]int, len(values))
	for _, value := range values {
		counts[value]++
	}
	var repeated []string
	for value, count := range counts {
		if count > 1 {
			repeated = append(repeated, value)
		}
	}
	slices.Sort(repeated)
	return repeated
}

// compareStrings orders two strings, so every sort here reads the same way.
func compareStrings(a, b string) int { return strings.Compare(a, b) }
