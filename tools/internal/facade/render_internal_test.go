package facade

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"strings"
	"testing"
)

// corpusSource declares one of every type form a public signature can take.
//
// The @ stands in for a backtick so the source can be a raw string literal and
// still carry a struct tag, which is one of the forms whose spelling has to
// survive rendering.
const corpusSource = `
package corpus

import "unsafe"

type Basic int

type PointerToNamed *Basic

type SliceOfPointer []*Basic

type ArrayOfSlice [3][]Basic

type MapOfPointer map[string]*Basic

type SendOnly chan<- Basic

type ReceiveOnly <-chan Basic

type Bidirectional chan Basic

type ChannelOfReceiveOnly chan (<-chan Basic)

type Variadic func(first int, rest ...string) (int, error)

type NoResult func()

type NamedResults func() (count int, err error)

type Tagged struct {
	Exported int @json:"exported"@
	Pointer  *Basic
	Bidirectional
}

type Sealed struct {
	hidden int
}

type SealedInterface interface {
	seal()
}

type Contract interface {
	Do(value int) error
	Emit(values ...Basic) (chan<- Basic, bool)
}

type Embedding interface {
	Contract
	Extra() [2]*Basic
}

type EmptyStruct struct{}

type EmptyInterface interface{}

type AnyAlias = any

type FuncAlias = func(Basic) error

type Pair[K comparable, V any] struct {
	Key   K
	Value V
}

type StringPair Pair[string, *Basic]

type PairHolder struct {
	Pair Pair[string, *Basic]
}

type Constrained[T interface{ ~int | ~string }] struct {
	Value T
}

type Nested struct {
	Inner    struct{ X int }
	Callback func(chan<- Basic, map[Basic][]string) (*Basic, error)
}

type Unsafe struct {
	Pointer unsafe.Pointer
}
`

// TestRendererMatchesGoTypes proves the type printer spells every form exactly
// as go/types does.
//
// The printer exists only because the facade renames what it publishes, so
// everything it does apart from that rename has to be go/types' own spelling.
// Comparing against types.TypeString over a corpus is the only way to say that
// without restating the language's grammar in assertions: a divergence in
// variadic parameters, channel direction parenthesisation, struct tags, or
// embedded interfaces would otherwise surface as a generated module that does
// not compile.
func TestRendererMatchesGoTypes(t *testing.T) {
	t.Parallel()
	scope := checkCorpus(t)
	table := newImportTable(nil)
	render := &renderer{aliases: map[*types.TypeName]string{}, qualify: table.qualify, mode: renderSource}

	// The recording pass has to see the same types the comparison does, because
	// an import name is chosen over the whole referenced set.
	for _, subject := range corpusTypes(scope) {
		if _, err := render.typ(subject.typ); err != nil {
			t.Fatalf("record %s: %v", subject.name, err)
		}
	}
	table.assign()
	qualifier := func(pkg *types.Package) string { return table.qualify(pkg) }

	for _, subject := range corpusTypes(scope) {
		got, err := render.typ(subject.typ)
		if err != nil {
			t.Errorf("render %s: %v", subject.name, err)
			continue
		}
		if want := types.TypeString(subject.typ, qualifier); got != want {
			t.Errorf("render %s = %q, go/types spells it %q", subject.name, got, want)
		}
	}
}

// TestRendererUsesFacadeNames proves the one thing the printer does differently
// from go/types: a published type is spelled by its facade name wherever it
// appears, however deeply it is nested.
func TestRendererUsesFacadeNames(t *testing.T) {
	t.Parallel()
	scope := checkCorpus(t)
	basic, ok := scope.Lookup("Basic").(*types.TypeName)
	if !ok {
		t.Fatalf("corpus does not declare Basic")
	}

	table := newImportTable(nil)
	render := &renderer{aliases: map[*types.TypeName]string{basic: "PublishedBasic"}, qualify: table.qualify, mode: renderSource}
	tests := []struct {
		name string
		want string
	}{
		{"SliceOfPointer", "[]*PublishedBasic"},
		{"MapOfPointer", "map[string]*PublishedBasic"},
		{"ChannelOfReceiveOnly", "chan (<-chan PublishedBasic)"},
		{"PairHolder", "struct{Pair corpus.Pair[string, *PublishedBasic]}"},
	}
	for _, test := range tests {
		if _, err := render.typ(scope.Lookup(test.name).Type().Underlying()); err != nil {
			t.Fatalf("record %s: %v", test.name, err)
		}
	}
	table.assign()

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			object := scope.Lookup(test.name)
			if object == nil {
				t.Fatalf("corpus does not declare %s", test.name)
			}
			got, err := render.typ(object.Type().Underlying())
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if got != test.want {
				t.Errorf("render = %q, want %q", got, test.want)
			}
		})
	}
}

// TestRendererRefusesUnreproducibleMembers pins the one place the two render
// modes have to disagree.
//
// Source has to compile in the generated package, where an unexported field or
// method of another package cannot be written at all: the same characters would
// declare a different type. A manifest only has to compare, and the member is a
// real part of what the compared type is, so it is recorded rather than
// refused.
func TestRendererRefusesUnreproducibleMembers(t *testing.T) {
	t.Parallel()
	scope := checkCorpus(t)
	tests := []struct {
		name string
		want string
	}{
		{name: "Sealed", want: "struct{hidden int}"},
		{name: "SealedInterface", want: "interface{seal()}"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			subject := scope.Lookup(test.name).Type().Underlying()

			source := &renderer{aliases: map[*types.TypeName]string{}, qualify: qualifyByPath, mode: renderSource}
			if _, err := source.typ(subject); !errors.Is(err, ErrUnrepresentable) {
				t.Errorf("source render error is %v, want ErrUnrepresentable", err)
			}

			manifest := &renderer{aliases: map[*types.TypeName]string{}, qualify: qualifyByPath, mode: renderManifest}
			got, err := manifest.typ(subject)
			if err != nil {
				t.Fatalf("manifest render: %v", err)
			}
			if got != test.want {
				t.Errorf("manifest render = %q, want %q", got, test.want)
			}
		})
	}
}

// TestRendererDropsNamesForTheManifest pins the other difference: a manifest
// compares API identity, which does not include parameter names.
func TestRendererDropsNamesForTheManifest(t *testing.T) {
	t.Parallel()
	scope := checkCorpus(t)
	subject := scope.Lookup("Variadic").Type().Underlying()

	for _, test := range []struct {
		mode renderMode
		want string
	}{
		{mode: renderSource, want: "func(first int, rest ...string) (int, error)"},
		{mode: renderManifest, want: "func(int, ...string) (int, error)"},
	} {
		render := &renderer{aliases: map[*types.TypeName]string{}, qualify: qualifyByPath, mode: test.mode}
		got, err := render.typ(subject)
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		if got != test.want {
			t.Errorf("render = %q, want %q", got, test.want)
		}
	}
}

// qualifyByPath names a package by its import path, which is how a manifest
// spells one.
func qualifyByPath(pkg *types.Package) string { return pkg.Path() }

// TestImportTableAssignsDeterministicNames covers the cases where a package
// cannot keep its own name.
//
// Every one of them is a real shape in this domain. Two relocated API versions
// are both named v1, an upstream package can be named the same as a facade
// export, and a path element can be something that is not an identifier at all.
func TestImportTableAssignsDeterministicNames(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		reserved []string
		packages [][2]string
		want     map[string]string
	}{
		{
			name:     "distinct names are kept",
			packages: [][2]string{{"k8s.io/api/rbac/v1", "v1"}, {"example.com/authorizer", "authorizer"}},
			want:     map[string]string{"k8s.io/api/rbac/v1": "v1", "example.com/authorizer": "authorizer"},
		},
		{
			name:     "colliding names grow leftwards through the path",
			packages: [][2]string{{"k8s.io/api/rbac/v1", "v1"}, {"k8s.io/api/core/v1", "v1"}},
			// The paths sort with core before rbac, so core keeps the bare name
			// and rbac takes the qualified one, on every run.
			want: map[string]string{"k8s.io/api/core/v1": "v1", "k8s.io/api/rbac/v1": "rbac_v1"},
		},
		{
			name:     "a facade name is never shadowed",
			reserved: []string{"validation"},
			packages: [][2]string{{"example.com/pkg/validation", "validation"}},
			want:     map[string]string{"example.com/pkg/validation": "pkg_validation"},
		},
		{
			name:     "a predeclared identifier is never shadowed",
			packages: [][2]string{{"example.com/pkg/error", "error"}},
			want:     map[string]string{"example.com/pkg/error": "pkg_error"},
		},
		{
			name:     "a path element that is not an identifier is sanitised",
			packages: [][2]string{{"example.com/aaa/cmp", "cmp"}, {"example.com/go-cmp/cmp", "cmp"}},
			// Paths are assigned in sorted order, so aaa keeps the bare name and
			// go-cmp takes a qualified one with its hyphen sanitised.
			want: map[string]string{"example.com/aaa/cmp": "cmp", "example.com/go-cmp/cmp": "go_cmp_cmp"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			table := newImportTable(test.reserved)
			for _, entry := range test.packages {
				table.qualify(types.NewPackage(entry[0], entry[1]))
			}
			table.assign()
			for path, want := range test.want {
				if got := table.local[path]; got != want {
					t.Errorf("local name of %s is %q, want %q", path, got, want)
				}
			}
			// A local name serves exactly one package, or the generated file
			// resolves a type against the wrong one.
			seen := make(map[string]string, len(table.local))
			for path, name := range table.local {
				if other, taken := seen[name]; taken {
					t.Errorf("%s and %s both take the local name %q", other, path, name)
				}
				seen[name] = path
			}
		})
	}
}

// TestCommentBlockWraps proves the generated prose wrapping depends on nothing
// but the text, which is what keeps a regenerated release from differing by a
// reflow nobody asked for.
func TestCommentBlockWraps(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		text string
		want string
	}{
		{name: "empty", text: "", want: ""},
		{name: "short", text: "One short line.", want: "// One short line.\n"},
		{
			name: "wrapped at the comment width",
			text: strings.Repeat("word ", 30),
			want: "// word word word word word word word word word word word word word word word\n" +
				"// word word word word word word word word word word word word word word word\n",
		},
		{
			name: "runs of whitespace collapse",
			text: "  a\t\tb \n c  ",
			want: "// a b c\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			rendered := commentBlock(test.text)
			if rendered != test.want {
				t.Errorf("commentBlock = %q, want %q", rendered, test.want)
			}
			for _, line := range strings.Split(strings.TrimSuffix(rendered, "\n"), "\n") {
				if len(line) > commentWidth {
					t.Errorf("line %q is %d columns, wider than %d", line, len(line), commentWidth)
				}
			}
		})
	}
}

// corpusSubject is one type from the corpus, with the name to report it under.
type corpusSubject struct {
	name string
	typ  types.Type
}

// unreproducible names the corpus declarations that exist to be refused.
//
// They carry unexported members, so source rendering refuses them by design and
// they have no place in a comparison against go/types, which has no such rule.
// Listing them here rather than skipping anything that fails keeps the
// exemption from quietly growing to cover a real divergence.
var unreproducible = map[string]bool{"Sealed": true, "SealedInterface": true}

// corpusTypes reports every declared type and its underlying type, which is
// what exercises the structural forms rather than only the named references to
// them.
func corpusTypes(scope *types.Scope) []corpusSubject {
	var subjects []corpusSubject
	for _, name := range scope.Names() {
		if unreproducible[name] {
			continue
		}
		object := scope.Lookup(name)
		subjects = append(subjects,
			corpusSubject{name: name, typ: object.Type()},
			corpusSubject{name: name + " underlying", typ: object.Type().Underlying()},
		)
	}
	return subjects
}

// checkCorpus type checks the corpus and reports its package scope.
//
// The importer resolves unsafe and refuses everything else, so the corpus can
// exercise unsafe.Pointer without the test depending on whether compiled export
// data for any other package happens to exist on this machine.
func checkCorpus(t *testing.T) *types.Scope {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "corpus.go", strings.ReplaceAll(corpusSource, "@", "`"), parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse corpus: %v", err)
	}
	config := types.Config{Importer: unsafeOnlyImporter{}}
	pkg, err := config.Check("corpus", fset, []*ast.File{file}, nil)
	if err != nil {
		t.Fatalf("type check corpus: %v", err)
	}
	return pkg.Scope()
}

// unsafeOnlyImporter resolves the one import the corpus has.
type unsafeOnlyImporter struct{}

// Import reports the unsafe package and refuses anything else, which keeps a
// corpus edit that reaches for a dependency from silently type checking against
// whatever export data is lying around.
func (unsafeOnlyImporter) Import(path string) (*types.Package, error) {
	if path == "unsafe" {
		return types.Unsafe, nil
	}
	return nil, fmt.Errorf("the corpus may not import %s", path)
}
