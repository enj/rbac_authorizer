package typeswap_test

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/enj/soapbox/tools/internal/typeswap"
)

// The fixtures here are real Go source trees written to a temporary directory
// and type checked with go/types, parsed with comments so the marker analysis
// reads the same generator directives it will read in production. Standard
// library imports resolve against the real standard library, so a conversion
// that reinterprets through unsafe.Pointer is the real unsafe.Pointer.

// stdImporter type checks the standard library from GOROOT source.
//
// The lock is required rather than defensive: the source importer marks a
// package as in progress while it reads it, so two parallel tests importing
// context at the same moment make the second see the first's marker and report
// an import cycle through a package that has none.
var stdImporter = sync.OnceValue(func() types.Importer {
	return &lockedImporter{importer: importer.ForCompiler(token.NewFileSet(), "source", nil)}
})

type lockedImporter struct {
	mu       sync.Mutex
	importer types.Importer
}

func (l *lockedImporter) Import(path string) (*types.Package, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.importer.Import(path)
}

// fixture is a type checked synthetic tree.
type fixture struct {
	dir      string
	fset     *token.FileSet
	packages map[string]*typeswap.Package
}

// newFixture writes the tree, type checks it, and returns the loaded packages.
//
// Files map an import path and file name, joined with a slash, onto source.
func newFixture(t *testing.T, files map[string]string) *fixture {
	t.Helper()

	dir := t.TempDir()
	byPackage := make(map[string][]string)
	for name, contents := range files {
		importPath, base := path.Split(name)
		importPath = strings.TrimSuffix(importPath, "/")
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatalf("create directory for %s: %v", name, err)
		}
		if err := os.WriteFile(full, []byte(contents), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		byPackage[importPath] = append(byPackage[importPath], base)
	}

	f := &fixture{dir: dir, fset: token.NewFileSet(), packages: map[string]*typeswap.Package{}}

	parsed := make(map[string][]*ast.File, len(byPackage))
	for importPath, names := range byPackage {
		slices.Sort(names)
		byPackage[importPath] = names
		for _, base := range names {
			full := filepath.Join(dir, filepath.FromSlash(importPath), base)
			// Comments are kept because the marker proof reads generator
			// directives. A loader that dropped them would make that proof
			// vacuous rather than failing.
			file, err := parser.ParseFile(f.fset, full, nil, parser.ParseComments)
			if err != nil {
				t.Fatalf("parse %s: %v", path.Join(importPath, base), err)
			}
			parsed[importPath] = append(parsed[importPath], file)
		}
	}

	for _, importPath := range topoOrder(t, parsed) {
		f.check(t, importPath, dir, byPackage[importPath], parsed[importPath])
	}
	return f
}

// check type checks one package and records it.
func (f *fixture) check(t *testing.T, importPath, dir string, goFiles []string, files []*ast.File) {
	t.Helper()

	info := &types.Info{
		Types: map[ast.Expr]types.TypeAndValue{},
		Defs:  map[*ast.Ident]types.Object{},
		Uses:  map[*ast.Ident]types.Object{},
	}
	var problems []string
	config := &types.Config{
		Importer: importerFunc(func(imported string) (*types.Package, error) {
			if known, ok := f.packages[imported]; ok {
				return known.Types, nil
			}
			return stdImporter().Import(imported)
		}),
		Error: func(err error) { problems = append(problems, err.Error()) },
	}
	checked, err := config.Check(importPath, f.fset, files, info)
	if checked == nil {
		t.Fatalf("type check %s: %v", importPath, err)
	}
	if len(problems) > 0 {
		t.Fatalf("type check %s:\n  %s", importPath, strings.Join(problems, "\n  "))
	}

	f.packages[importPath] = &typeswap.Package{
		ImportPath: importPath,
		Dir:        filepath.Join(dir, filepath.FromSlash(importPath)),
		Types:      checked,
		Syntax:     files,
		Info:       info,
		// The fixture has no cgo, so the compiled and source file lists are the
		// same. They are set separately anyway, because the alignment between
		// CompiledGoFiles and Syntax is a checked invariant.
		CompiledGoFiles: slices.Clone(goFiles),
		GoFiles:         slices.Clone(goFiles),
		Imports:         fileImports(files),
	}
}

// graph builds a graph over every loaded package with the given retained set.
func (f *fixture) graph(retained ...string) *typeswap.Graph {
	packages := make([]*typeswap.Package, 0, len(f.packages))
	for _, pkg := range f.packages {
		packages = append(packages, pkg)
	}
	slices.SortFunc(packages, func(a, b *typeswap.Package) int {
		return strings.Compare(a.ImportPath, b.ImportPath)
	})
	return &typeswap.Graph{Fset: f.fset, Packages: packages, Retained: slices.Clone(retained)}
}

// fileImports lists the import paths a package's files import, sorted.
func fileImports(files []*ast.File) []string {
	var imports []string
	for _, file := range files {
		for _, spec := range file.Imports {
			if spec.Path == nil {
				continue
			}
			unquoted, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				continue
			}
			imports = append(imports, unquoted)
		}
	}
	slices.Sort(imports)
	return slices.Compact(imports)
}

// topoOrder returns the fixture's packages in dependency order.
func topoOrder(t *testing.T, parsed map[string][]*ast.File) []string {
	t.Helper()

	remaining := make(map[string]bool, len(parsed))
	for importPath := range parsed {
		remaining[importPath] = true
	}

	var order []string
	for len(remaining) > 0 {
		ready := make([]string, 0, len(remaining))
		for importPath := range remaining {
			blocked := false
			for _, imported := range fileImports(parsed[importPath]) {
				if remaining[imported] && imported != importPath {
					blocked = true
					break
				}
			}
			if !blocked {
				ready = append(ready, importPath)
			}
		}
		if len(ready) == 0 {
			t.Fatal("fixture packages form an import cycle")
		}
		slices.Sort(ready)
		order = append(order, ready...)
		for _, importPath := range ready {
			delete(remaining, importPath)
		}
	}
	return order
}

// importerFunc adapts a function to types.Importer.
type importerFunc func(string) (*types.Package, error)

func (f importerFunc) Import(path string) (*types.Package, error) { return f(path) }
