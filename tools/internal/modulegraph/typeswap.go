package modulegraph

import (
	"context"
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/tools/go/packages"

	"github.com/enj/soapbox/tools/internal/typeswap"
)

// Relabel reads a relocated module's import paths as the upstream paths a
// profile spells.
//
// It exists because the two halves of a type policy are written in different
// vocabularies. A profile names k8s.io/kubernetes/pkg/apis/rbac, which is the
// path a reader can look up upstream, while the module actually on disk has
// relocated that package to monis.app/kk/rbac_authorizer/internal/kk/pkg/apis/rbac.
// Relabelling maps the second back onto the first so a pairing stays spelled the
// way an operator wrote it. External paths are never touched: k8s.io/api/rbac/v1
// is a real dependency of the generated module and keeps the identity it
// actually has.
//
// The zero value performs no relabelling, which is the right setting for a load
// whose import paths are already the ones the profile spells.
type Relabel struct {
	// ModulePath is the generated module path, such as
	// monis.app/kk/rbac_authorizer.
	ModulePath string
	// InternalPrefix is the module relative directory relocated packages live
	// below, such as internal/kk.
	InternalPrefix string
	// SourcePrefix is the upstream module path they were relocated from, such
	// as k8s.io/kubernetes.
	SourcePrefix string
}

// configured reports whether any relabelling was asked for.
func (r Relabel) configured() bool {
	return r.ModulePath != "" || r.InternalPrefix != "" || r.SourcePrefix != ""
}

// root reports the import path the relocated packages sit at.
func (r Relabel) root() string { return path.Join(r.ModulePath, r.InternalPrefix) }

// validate rejects a partially stated relabel.
//
// A half configured relabel is worse than none: it would map some paths and
// leave others, producing a graph in two vocabularies at once, which is the
// state the proof below exists to prevent.
func (r Relabel) validate(p *problems) {
	if !r.configured() {
		return
	}
	if r.ModulePath == "" {
		p.addf("relabel: a generated module path is required to recognise a relocated package")
	}
	if r.SourcePrefix == "" {
		p.addf("relabel: an upstream source prefix is required to name a relocated package")
	}
	if strings.HasPrefix(r.InternalPrefix, "/") || strings.HasPrefix(r.InternalPrefix, "../") {
		p.addf("relabel: internal prefix %q must be a module relative directory", r.InternalPrefix)
	}
}

// apply reports the path an import path is known by after relabelling, and
// whether it was relabelled.
//
// The prefix test is an exact match on a path boundary. A module named
// monis.app/kk/rbac_authorizer_extra shares a textual prefix with
// monis.app/kk/rbac_authorizer and must not be relabelled, which a plain string
// prefix test would get wrong.
func (r Relabel) apply(importPath string) (string, bool) {
	if !r.configured() {
		return importPath, false
	}
	root := r.root()
	switch {
	case importPath == root:
		return r.SourcePrefix, true
	case strings.HasPrefix(importPath, root+"/"):
		return r.SourcePrefix + strings.TrimPrefix(importPath, root), true
	default:
		return importPath, false
	}
}

// TypeswapSpec describes the type policy graph to build.
type TypeswapSpec struct {
	// Packages are the import paths to include, spelled the way Relabel
	// leaves them. Empty means every loaded package outside the standard
	// library, which is the usual case: both sides of every pair and every
	// package that could retain a use have to be present, and naming them
	// individually is a list that goes stale silently.
	Packages []string
	// Retained are the import paths that survive pruning, spelled the way
	// Relabel leaves them.
	Retained []string
	// PrunedFiles are the repository relative files the profile removes.
	PrunedFiles []string
	// PublicAPIDifferences are the rendered differences between the generated
	// module's public API before and after the change. This package does not
	// compute them; the facade owns what the public API is.
	PublicAPIDifferences []string
	// Relabel maps relocated import paths back onto the upstream paths the
	// profile spells. The zero value relabels nothing.
	Relabel Relabel
}

// Typeswap adapts the loaded graph into a type policy graph.
//
// The adaptation is a field copy plus one proof. The proof is the reason this
// is not inline at the call site: typeswap identifies a package two ways, and
// they have to agree. It looks a pair up by ImportPath, and it decides whether
// retained code still uses an internal type by comparing the package path
// carried on the type checked object. Relabelling the first without the second
// produces a graph where the pair resolves and every use of it is invisible,
// and the analysis then reports that nothing references the internal package
// and pruning it is safe. That is a false pass in the one direction that
// deletes code a consumer depends on.
//
// A type checked package path cannot be rewritten. It is fixed when the loader
// creates the package, and rebuilding the type graph to change it would produce
// new objects, which would break the object identity the equivalence proof
// compares. So the relabel is proven rather than propagated: every package in
// the returned graph must carry a type identity equal to the path it is filed
// under, and a relabel that would break that is refused with the paths that
// disagree. An operator reading that refusal learns the load has to be taken
// against a tree whose packages already carry the upstream identity, which is
// something they can act on, rather than reading a report that proved nothing.
func (g *Graph) Typeswap(ctx context.Context, spec TypeswapSpec) (*typeswap.Graph, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("adapt type policy graph: %w", err)
	}
	const action = "adapt type policy graph"

	var p problems
	spec.Relabel.validate(&p)
	if err := p.err(g.redactor, action, ErrOptions); err != nil {
		return nil, err
	}

	display, err := g.displayIndex(action, spec.Relabel)
	if err != nil {
		return nil, err
	}

	selected, err := g.selectTypeswapPackages(action, spec, display)
	if err != nil {
		return nil, err
	}

	graph := &typeswap.Graph{
		Fset:                 g.fset,
		Packages:             selected,
		Retained:             slices.Clone(spec.Retained),
		PrunedFiles:          slices.Clone(spec.PrunedFiles),
		PublicAPIDifferences: slices.Clone(spec.PublicAPIDifferences),
	}
	slices.Sort(graph.Retained)
	graph.Retained = slices.Compact(graph.Retained)

	loaded := make(map[string]bool, len(selected))
	for _, pkg := range selected {
		loaded[pkg.ImportPath] = true
	}
	for _, retained := range graph.Retained {
		if !loaded[retained] {
			// typeswap refuses this too. It is caught here as well so the
			// message names the vocabulary problem an operator actually hit,
			// which after a relabel is almost always a path spelled in the
			// other one.
			p.addf("retained package %q is not in the graph, so its use of an internal type cannot be checked", retained)
		}
	}
	if err := p.err(g.redactor, action, ErrPackageMissing); err != nil {
		return nil, err
	}
	return graph, nil
}

// selectTypeswapPackages resolves the requested packages and copies them.
func (g *Graph) selectTypeswapPackages(action string, spec TypeswapSpec, display map[string]*packages.Package) ([]*typeswap.Package, error) {
	var p problems
	var chosen []string
	if len(spec.Packages) > 0 {
		chosen = slices.Clone(spec.Packages)
		slices.Sort(chosen)
		chosen = slices.Compact(chosen)
		for _, importPath := range chosen {
			if _, ok := display[importPath]; !ok {
				p.addf("package %q is not in the module graph", importPath)
			}
		}
		if err := p.err(g.redactor, action, ErrPackageMissing); err != nil {
			return nil, err
		}
	} else {
		// The standard library is excluded rather than carried. It can never be
		// a pair, can never be retained because it is never part of the
		// generated module, and carries no upstream generator directive, so
		// including it would only make the marker scan read GOROOT.
		for importPath := range display {
			if !isStandard(importPath) {
				chosen = append(chosen, importPath)
			}
		}
		slices.Sort(chosen)
	}

	selected := make([]*typeswap.Package, 0, len(chosen))
	for _, importPath := range chosen {
		pkg := display[importPath]
		selected = append(selected, &typeswap.Package{
			ImportPath: importPath,
			Dir:        packageDir(pkg),
			Types:      pkg.Types,
			Syntax:     slices.Clone(pkg.Syntax),
			Info:       pkg.TypesInfo,
			// The compiled file names keep the loader's order and are not
			// deduplicated. A marker's file is taken by indexing this list with
			// a syntax index, so reordering it would attribute a directive to
			// the wrong file.
			CompiledGoFiles: orderedBaseNames(pkg.CompiledGoFiles),
			GoFiles:         orderedBaseNames(pkg.GoFiles),
			CgoFiles:        cgoOriginals(pkg),
			Imports:         relabelAll(importPaths(pkg), spec.Relabel),
		})
	}
	return selected, nil
}

// displayIndex maps every loaded package onto the path it is known by after
// relabelling, proving that the result is one consistent vocabulary.
//
// Two things are refused. A collision means two loaded packages would be filed
// under one name, so a lookup could not say which one a policy judged. A
// disagreement between the filed name and the type checked package path means
// the graph would resolve a package by one identity and its objects by another,
// which is the false pass this proof exists to prevent.
func (g *Graph) displayIndex(action string, relabel Relabel) (map[string]*packages.Package, error) {
	var p problems
	display := make(map[string]*packages.Package, len(g.ordered))
	origin := make(map[string]string, len(g.ordered))
	for _, pkg := range g.ordered {
		name, relabelled := relabel.apply(pkg.PkgPath)
		if previous, ok := origin[name]; ok {
			p.addf("packages %q and %q are both known as %q, so a lookup cannot say which one a policy would judge",
				previous, pkg.PkgPath, name)
			continue
		}
		origin[name] = pkg.PkgPath
		display[name] = pkg
		if pkg.Types == nil {
			// Reported by the load, which runs before any adapter. Skipping it
			// here keeps one missing type checker result from also producing a
			// misleading relabel problem.
			continue
		}
		if identity := pkg.Types.Path(); identity != name {
			detail := fmt.Sprintf("package %q is filed as %q but its type identity is %q", pkg.PkgPath, name, identity)
			if relabelled {
				detail += "; a retained use is resolved through the object's package path, so this graph would report that nothing uses the internal package and that pruning it is safe"
			}
			p.addf("%s", detail)
		}
	}
	if err := p.err(g.redactor, action, ErrRelabelUnproven); err != nil {
		return nil, err
	}
	return display, nil
}

// relabelAll applies a relabel to every path in a list, sorted.
func relabelAll(paths []string, relabel Relabel) []string {
	if !relabel.configured() {
		return paths
	}
	out := make([]string, 0, len(paths))
	for _, importPath := range paths {
		name, _ := relabel.apply(importPath)
		out = append(out, name)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// orderedBaseNames reports the base name of each path, preserving order and
// duplicates so an index into the result still means what it meant.
//
// The names come from the loader, which reports operating system paths, so they
// are split as such. The relabelling above works on import paths and uses slash
// semantics; the two are different alphabets and are not interchangeable.
func orderedBaseNames(paths []string) []string {
	names := make([]string, 0, len(paths))
	for _, file := range paths {
		names = append(names, filepath.Base(file))
	}
	return names
}
