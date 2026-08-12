package deppolicy_test

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

	"github.com/enj/soapbox/tools/internal/deppolicy"
)

// The fixtures in this file are real Go source trees written to a temporary
// directory and type checked with go/types. Nothing is stubbed: the policy
// reads the same *types.Package, *ast.File, and on disk bytes it will read in
// production, and the cost measurements count lines in files that exist.
//
// Standard library imports resolve through a shared source importer rather than
// through hand written stand ins, so a fixture that says context.WithValue
// means the real context.WithValue and the deny registry is exercised against
// the real object it will match at run time.

// stdImporter type checks the standard library from GOROOT source.
//
// It is created once and shared because it caches, and because type checking
// context and its dependencies from source is the slowest thing these tests do.
// The lock is required, not defensive: the source importer marks a package as
// in progress while it reads it, so two parallel tests importing context at the
// same moment make the second one see the first one's marker and report an
// import cycle through a package that has none.
var stdImporter = sync.OnceValue(func() types.Importer {
	return &lockedImporter{importer: importer.ForCompiler(token.NewFileSet(), "source", nil)}
})

// lockedImporter serializes access to an importer that caches.
type lockedImporter struct {
	mu       sync.Mutex
	importer types.Importer
}

func (l *lockedImporter) Import(path string) (*types.Package, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.importer.Import(path)
}

// fixture is a type checked synthetic module tree.
type fixture struct {
	dir      string
	fset     *token.FileSet
	packages map[string]*deppolicy.Package
}

// fixtureSource describes one synthetic tree.
type fixtureSource struct {
	// Modules are the module paths in the tree, longest match wins when
	// attributing a package to a module.
	Modules []string
	// Files maps an import path and file name, joined with a slash, onto the
	// file's source. "k8s.io/apiserver/pkg/authorization/authorizer/interfaces.go"
	// declares a file of package
	// k8s.io/apiserver/pkg/authorization/authorizer.
	Files map[string]string
	// Extra maps the same kind of key onto a non-Go build input, used to make a
	// candidate carry native code.
	Extra map[string]string
	// ModuleFiles maps a module path and file name onto a file at the module
	// root, used for licences.
	ModuleFiles map[string]string
}

// newFixture writes the tree, type checks it, and returns the loaded packages.
func newFixture(t *testing.T, source fixtureSource) *fixture {
	t.Helper()

	dir := t.TempDir()
	byPackage := make(map[string][]string)
	for name, contents := range source.Files {
		importPath, base := path.Split(name)
		importPath = strings.TrimSuffix(importPath, "/")
		writeFixtureFile(t, dir, name, contents)
		byPackage[importPath] = append(byPackage[importPath], base)
	}
	for name, contents := range source.Extra {
		writeFixtureFile(t, dir, name, contents)
	}
	for name, contents := range source.ModuleFiles {
		writeFixtureFile(t, dir, name, contents)
	}

	f := &fixture{dir: dir, fset: token.NewFileSet(), packages: map[string]*deppolicy.Package{}}

	parsed := make(map[string][]*ast.File, len(byPackage))
	for importPath, files := range byPackage {
		slices.Sort(files)
		byPackage[importPath] = files
		for _, base := range files {
			full := filepath.Join(dir, filepath.FromSlash(importPath), base)
			file, err := parser.ParseFile(f.fset, full, nil, parser.ParseComments)
			if err != nil {
				t.Fatalf("parse %s: %v", path.Join(importPath, base), err)
			}
			parsed[importPath] = append(parsed[importPath], file)
		}
	}

	for _, importPath := range topoOrder(t, parsed) {
		f.check(t, source.Modules, importPath, dir, byPackage[importPath], parsed[importPath])
	}
	return f
}

// writeFixtureFile writes one file, creating its parent directories.
func writeFixtureFile(t *testing.T, dir, name, contents string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		t.Fatalf("create directory for %s: %v", name, err)
	}
	if err := os.WriteFile(full, []byte(contents), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// check type checks one package and records it.
func (f *fixture) check(t *testing.T, modules []string, importPath, dir string, goFiles []string, files []*ast.File) {
	t.Helper()

	info := &types.Info{
		Types: map[ast.Expr]types.TypeAndValue{},
		Defs:  map[*ast.Ident]types.Object{},
		Uses:  map[*ast.Ident]types.Object{},
	}
	var checkErrors []string
	config := &types.Config{
		Importer: importerFunc(func(importPath string) (*types.Package, error) {
			if known, ok := f.packages[importPath]; ok {
				return known.Types, nil
			}
			return stdImporter().Import(importPath)
		}),
		// Every type checker diagnostic is collected and fails the test. A
		// fixture that does not type check would silently leave Info.Uses
		// incomplete, and an analysis that reads Uses would then find nothing
		// and report a clean package. Swallowing these would make a broken
		// fixture look like a passing policy.
		Error: func(err error) { checkErrors = append(checkErrors, err.Error()) },
	}
	checked, err := config.Check(importPath, f.fset, files, info)
	if checked == nil {
		t.Fatalf("type check %s: %v", importPath, err)
	}
	if len(checkErrors) > 0 {
		t.Fatalf("type check %s:\n  %s", importPath, strings.Join(checkErrors, "\n  "))
	}

	pkgDir := filepath.Join(dir, filepath.FromSlash(importPath))
	var extra []string
	if entries, err := os.ReadDir(pkgDir); err == nil {
		for _, entry := range entries {
			if !entry.IsDir() && !strings.HasSuffix(entry.Name(), ".go") {
				extra = append(extra, entry.Name())
			}
		}
	}
	slices.Sort(extra)

	f.packages[importPath] = &deppolicy.Package{
		ImportPath: importPath,
		Module:     moduleOf(modules, importPath),
		Dir:        pkgDir,
		Types:      checked,
		Syntax:     files,
		Info:       info,
		GoFiles:    slices.Clone(goFiles),
		OtherFiles: extra,
		Imports:    fileImports(files),
	}
}

// pkg returns one loaded package.
func (f *fixture) pkg(t *testing.T, importPath string) *deppolicy.Package {
	t.Helper()
	loaded, ok := f.packages[importPath]
	if !ok {
		t.Fatalf("fixture has no package %s", importPath)
	}
	return loaded
}

// candidate returns one loaded package as a staging candidate.
func (f *fixture) candidate(t *testing.T, importPath string) deppolicy.Candidate {
	t.Helper()
	return deppolicy.Candidate{
		StagingPath: "staging/src/" + importPath,
		Package:     f.pkg(t, importPath),
	}
}

// build renders the fixture's packages as a resolved consumer build.
func (f *fixture) build() []deppolicy.BuildPackage {
	build := make([]deppolicy.BuildPackage, 0, len(f.packages))
	for _, loaded := range f.packages {
		build = append(build, deppolicy.BuildPackage{
			ImportPath: loaded.ImportPath,
			Module:     loaded.Module,
			Imports:    slices.Clone(loaded.Imports),
		})
	}
	slices.SortFunc(build, func(a, b deppolicy.BuildPackage) int {
		return strings.Compare(a.ImportPath, b.ImportPath)
	})
	return build
}

// moduleOf attributes a package to the longest module path that contains it.
func moduleOf(modules []string, importPath string) string {
	best := ""
	for _, module := range modules {
		if importPath != module && !strings.HasPrefix(importPath, module+"/") {
			continue
		}
		if len(module) > len(best) {
			best = module
		}
	}
	return best
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
//
// Type checking a package needs its imports checked first, and a fixture is
// written as a map, so the order has to be derived rather than assumed.
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
			t.Fatalf("fixture packages form an import cycle: %v", slices.Sorted(maps(remaining)))
		}
		slices.Sort(ready)
		order = append(order, ready...)
		for _, importPath := range ready {
			delete(remaining, importPath)
		}
	}
	return order
}

// maps yields a map's keys, so the cycle message can be sorted.
func maps(set map[string]bool) func(func(string) bool) {
	return func(yield func(string) bool) {
		for key := range set {
			if !yield(key) {
				return
			}
		}
	}
}

// importerFunc adapts a function to types.Importer.
type importerFunc func(string) (*types.Package, error)

func (f importerFunc) Import(path string) (*types.Package, error) { return f(path) }
