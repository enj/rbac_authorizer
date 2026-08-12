package facade

import (
	"context"
	"fmt"
	"go/types"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/tools/go/packages"
)

// loadMode is exactly the information the generator reads.
//
// Every bit is load bearing. Names and modules identify a package and tell an
// internal one from a dependency; files and syntax anchor a diagnostic to a
// real position; types and type information are what resolves a symbol and
// walks a signature; imports and dependencies extend all of that to the
// transitive graph, which is where a leaked type or an unimplemented interface
// actually lives. Asking for less would make some check silently vacuous rather
// than fail, because go/packages leaves unrequested fields nil.
const loadMode = packages.NeedName |
	packages.NeedFiles |
	packages.NeedSyntax |
	packages.NeedTypes |
	packages.NeedTypesInfo |
	packages.NeedImports |
	packages.NeedDeps |
	packages.NeedModule

// maxReportedLoadErrors bounds how many type checker diagnostics one failure
// reports. A module that does not compile produces cascades, and the first few
// in a deterministic order are what identifies the cause.
const maxReportedLoadErrors = 10

// loaded is the type checked view of the generated module.
type loaded struct {
	// byPath maps an import path onto the loaded package, including every
	// transitive dependency, which is where the interfaces an assertion names
	// and the types a signature reaches are found.
	byPath map[string]*packages.Package
}

// lookup reports the package loaded at an import path.
func (l loaded) lookup(pkgPath string) (*packages.Package, bool) {
	pkg, ok := l.byPath[pkgPath]
	return pkg, ok
}

// scopeObject resolves one exported symbol of one loaded package.
func (l loaded) scopeObject(pkgPath, name string) (types.Object, error) {
	pkg, ok := l.lookup(pkgPath)
	if !ok {
		return nil, fmt.Errorf("package %s: %w", pkgPath, ErrMissingSymbol)
	}
	if pkg.Types == nil {
		return nil, fmt.Errorf("package %s carries no type information: %w", pkgPath, ErrLoad)
	}
	object := pkg.Types.Scope().Lookup(name)
	if object == nil || !object.Exported() {
		return nil, fmt.Errorf("%s.%s: %w", pkgPath, name, ErrMissingSymbol)
	}
	return object, nil
}

// load type checks the generated module and everything the requested patterns
// reach.
//
// The environment is supplied by the caller in full and is never inherited. A
// go/packages load runs the go command, so an ambient environment would let
// GOFLAGS, GOPRIVATE, a module cache, or a proxy from whoever happened to start
// the process decide which code was type checked, and the facade is generated
// from exactly that code. An engine that produces a different public API
// depending on the shell it ran in is not reproducible, so a caller that
// supplies nothing is refused rather than defaulted.
func load(ctx context.Context, dir string, env []string, patterns []string) (loaded, error) {
	switch {
	case dir == "":
		return loaded{}, fmt.Errorf("%w: a module directory is required", ErrLoad)
	case !filepath.IsAbs(dir):
		return loaded{}, fmt.Errorf("%w: module directory %q must be absolute", ErrLoad, dir)
	case len(env) == 0:
		return loaded{}, fmt.Errorf("%w: an explicit environment is required, because an inherited one would let the ambient GOFLAGS, module cache, and proxy decide which code the public API is generated from", ErrLoad)
	case len(patterns) == 0:
		return loaded{}, fmt.Errorf("%w: at least one package pattern is required", ErrLoad)
	}

	config := &packages.Config{
		Context: ctx,
		Mode:    loadMode,
		Dir:     dir,
		// The environment is passed through verbatim. Appending to it here would
		// hide a knob from the caller that decides what gets type checked.
		Env: slices.Clone(env),
		// Test variants would bring a package's test only declarations into
		// scope, and the facade publishes the package, not its tests.
		Tests: false,
	}
	roots, err := packages.Load(config, patterns...)
	if err != nil {
		return loaded{}, fmt.Errorf("load %s: %w", strings.Join(patterns, " "), err)
	}
	byPath := make(map[string]*packages.Package)
	var problems []string
	// Visit walks the roots and every dependency in a deterministic order, so a
	// module with several broken packages reports the same diagnostics on every
	// run.
	packages.Visit(roots, nil, func(pkg *packages.Package) {
		if pkg.PkgPath != "" {
			byPath[pkg.PkgPath] = pkg
		}
		for _, problem := range pkg.Errors {
			problems = append(problems, describeLoadError(pkg, problem))
		}
	})
	if len(problems) > 0 {
		return loaded{}, fmt.Errorf("load %s: %w:\n%s", strings.Join(patterns, " "), ErrLoad, indentLines(clampErrors(problems)))
	}
	// A pattern that matched nothing is a silent success in go/packages: it
	// returns no package and no error. That would generate a facade with a
	// missing symbol diagnostic pointing at a package that was never asked for,
	// so the empty match is reported as what it is.
	if len(byPath) == 0 {
		return loaded{}, fmt.Errorf("load %s: %w: no package matched", strings.Join(patterns, " "), ErrLoad)
	}
	return loaded{byPath: byPath}, nil
}

// describeLoadError renders one type checker diagnostic with the package that
// produced it, because a diagnostic from a dependency is otherwise
// indistinguishable from one in the module being generated.
func describeLoadError(pkg *packages.Package, problem packages.Error) string {
	position := problem.Pos
	if position == "" {
		position = pkg.PkgPath
	}
	return position + ": " + problem.Msg
}

// clampErrors keeps the first few diagnostics and says how many were dropped.
func clampErrors(problems []string) []string {
	if len(problems) <= maxReportedLoadErrors {
		return problems
	}
	kept := slices.Clone(problems[:maxReportedLoadErrors])
	return append(kept, fmt.Sprintf("(and %d more)", len(problems)-maxReportedLoadErrors))
}

// indentLines indents a diagnostic block so it reads as detail of the error it
// is wrapped in rather than as several unrelated errors.
func indentLines(lines []string) string {
	var b strings.Builder
	for i, line := range lines {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString("  " + line)
	}
	return b.String()
}
