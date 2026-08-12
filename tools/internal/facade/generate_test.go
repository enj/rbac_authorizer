package facade_test

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"

	"github.com/enj/soapbox/tools/internal/facade"
	"github.com/enj/soapbox/tools/internal/relocate"
)

// TestGenerateRBACSurface pins the shape of the generated facade for the
// profile's own contract.
//
// The assertions are about what a declaration is rather than about exact bytes:
// an alias rather than a redeclaration, a function rather than a variable, a
// variadic parameter rather than the slice go/types records, and a facade name
// rather than an internal package qualifier in every published signature.
func TestGenerateRBACSurface(t *testing.T) {
	t.Parallel()
	dir := newRBACFixture(t)
	result := generate(t, dir, rbacSpec())
	source := fileNamed(t, result, "authorizer.go")

	for _, want := range []string{
		facade.GeneratedHeader,
		"package rbacauthorizer",
		// Types and interfaces are aliases, which is what preserves identity.
		"type RBACAuthorizer = rbac.RBACAuthorizer",
		"type RoleGetter = validation.RoleGetter",
		// The four adapters keep their upstream declaration under the explicit
		// names the profile gave them.
		"type RoleGetterFromLister = rbac.RoleGetter",
		"type RoleBindingListerFromLister = rbac.RoleBindingLister",
		"type ClusterRoleGetterFromLister = rbac.ClusterRoleGetter",
		"type ClusterRoleBindingListerFromLister = rbac.ClusterRoleBindingLister",
		// A function is a real declaration whose signature is spelled in facade
		// names, and whose body forwards to the relocated declaration.
		"func New(roles RoleGetter, roleBindings RoleBindingLister, clusterRoles ClusterRoleGetter, clusterRoleBindings ClusterRoleBindingLister) *RBACAuthorizer {",
		"return rbac.New(roles, roleBindings, clusterRoles, clusterRoleBindings)",
		// A variadic parameter stays variadic on both sides of the forward.
		"rules ...v1.PolicyRule) bool {",
		"return rbac.RulesAllow(requestAttributes, rules...)",
		// Constants are forwarded directly.
		"const PolicyRuleLimit = validation.PolicyRuleLimit",
	} {
		if !strings.Contains(source, want) {
			t.Errorf("generated facade does not contain %q\n%s", want, source)
		}
	}

	for _, unwanted := range []struct{ text, why string }{
		{"type RBACAuthorizer struct", "a redeclared type would not be the relocated type"},
		{"var New =", "an assignable function variable lets any consumer replace the published API"},
		{"func New(roles validation.RoleGetter", "a published signature must not spell an internal package"},
	} {
		if strings.Contains(source, unwanted.text) {
			t.Errorf("generated facade contains %q: %s\n%s", unwanted.text, unwanted.why, source)
		}
	}
	assertOnlyAliasesConstsAndFuncs(t, source)
}

// assertOnlyAliasesConstsAndFuncs proves the generated file declares nothing
// but aliases, constants, and functions.
//
// The check is structural rather than textual because the two failures it
// guards against are structural. A type declared rather than aliased is a
// distinct type that no relocated code produces and no consumer implementation
// satisfies, and a package level variable of any kind is state every importer
// of the module shares and any importer can change.
func assertOnlyAliasesConstsAndFuncs(t *testing.T, source string) {
	t.Helper()
	parsed, err := parser.ParseFile(token.NewFileSet(), "facade.go", source, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse generated facade: %v", err)
	}
	for _, decl := range parsed.Decls {
		general, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		switch general.Tok {
		case token.VAR:
			t.Errorf("generated facade declares a package level variable")
		case token.TYPE:
			for _, spec := range general.Specs {
				typed, ok := spec.(*ast.TypeSpec)
				if !ok {
					t.Fatalf("type declaration holds a %T", spec)
				}
				if !typed.Assign.IsValid() {
					t.Errorf("generated facade declares type %s instead of aliasing it", typed.Name.Name)
				}
			}
		}
	}
}

// TestGenerateKeepsExternalIdentity proves that a type belonging to another
// module is referenced through that module rather than redeclared or aliased
// into the generated one.
//
// This is the property that makes the extracted authorizer usable at all. A
// consumer builds a PolicyRule with the same API package everything else in the
// ecosystem uses, and a facade that published its own copy of the type would
// take a value nobody else can produce.
func TestGenerateKeepsExternalIdentity(t *testing.T) {
	t.Parallel()
	dir := newRBACFixture(t)
	result := generate(t, dir, rbacSpec())
	source := fileNamed(t, result, "authorizer.go")

	if !strings.Contains(source, `"example.com/api/rbac/v1"`) {
		t.Errorf("generated facade does not import the API module directly\n%s", source)
	}
	assertOnlyAliasesConstsAndFuncs(t, source)

	entry, ok := entryNamed(result.Manifest, "RulesAllow")
	if !ok {
		t.Fatalf("manifest has no RulesAllow entry")
	}
	if !strings.Contains(entry.Type, "example.com/api/rbac/v1.PolicyRule") {
		t.Errorf("manifest records RulesAllow as %q, which does not keep the API module's identity", entry.Type)
	}
}

// TestGenerateProducesCompilableModule writes the generated files into the
// fixture and type checks the whole module.
//
// Every other test in this file inspects what the generator decided. This one
// asks the Go type checker whether the decision was legal: that the aliases
// resolve, the import names do not collide, the parameter names do not capture
// an import, the forwarding calls match the signatures they forward to, and the
// emitted assertions hold.
func TestGenerateProducesCompilableModule(t *testing.T) {
	t.Parallel()
	dir := newRBACFixture(t)
	env := goEnvironment(t, dir)
	result, err := facade.Generate(t.Context(), facade.Options{Dir: dir, Env: env, Spec: rbacSpec()})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	writeGenerated(t, dir, result.Files)
	assertTypeChecks(t, dir, env)
}

// TestGenerateIsDeterministic proves two runs over one module produce identical
// bytes, which is what makes a regenerated release a real diff.
func TestGenerateIsDeterministic(t *testing.T) {
	t.Parallel()
	dir := newRBACFixture(t)
	first := generate(t, dir, rbacSpec())

	// The second run states the exports in reverse, because the order a profile
	// happens to list them in must not reach the output.
	spec := rbacSpec()
	slicesReverse(spec.Exports)
	slicesReverse(spec.Assertions)
	second := generate(t, dir, spec)

	if len(first.Files) != len(second.Files) {
		t.Fatalf("runs produced %d and %d files", len(first.Files), len(second.Files))
	}
	for i, file := range first.Files {
		if file.Path != second.Files[i].Path {
			t.Fatalf("file %d is %s and %s", i, file.Path, second.Files[i].Path)
		}
		if string(file.Contents) != string(second.Files[i].Contents) {
			t.Errorf("%s differs between runs\nfirst:\n%s\nsecond:\n%s", file.Path, file.Contents, second.Files[i].Contents)
		}
	}
	if !first.Manifest.Equal(second.Manifest) {
		t.Errorf("manifests differ between runs:\n%v", facade.Diff(first.Manifest, second.Manifest))
	}
}

// TestGenerateAssertions proves the interface assertions are both emitted and
// checked, and that the file exists even when a profile declares none.
func TestGenerateAssertions(t *testing.T) {
	t.Parallel()
	dir := newRBACFixture(t)

	result := generate(t, dir, rbacSpec())
	source := fileNamed(t, result, "zz_generated_assertions.go")
	for _, want := range []string{
		facade.GeneratedHeader,
		`"example.com/apiserver/pkg/authorization/authorizer"`,
		"_ authorizer.Authorizer = (*RBACAuthorizer)(nil)",
		"_ authorizer.RuleResolver = (*RBACAuthorizer)(nil)",
	} {
		// gofmt aligns the assignments of a var block, so the comparison is
		// against the collapsed text rather than the emitted columns.
		if !strings.Contains(collapseSpaces(source), want) {
			t.Errorf("assertions file does not contain %q\n%s", want, source)
		}
	}

	spec := rbacSpec()
	spec.Assertions = nil
	empty := fileNamed(t, generate(t, dir, spec), "zz_generated_assertions.go")
	if !strings.Contains(empty, "package rbacauthorizer") {
		t.Errorf("assertions file is not generated when no assertion is declared\n%s", empty)
	}
	if strings.Contains(empty, "var (") {
		t.Errorf("assertions file declares assertions that were not requested\n%s", empty)
	}
}

// TestGenerateRefusesBrokenAssertion proves the failure is reported here, in
// terms of the method that is missing, rather than left for the compiler to
// report from a generated file.
func TestGenerateRefusesBrokenAssertion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		method  string
		replace string
		want    string
	}{
		{
			name:    "missing method",
			method:  "func (r *RBACAuthorizer) Authorize(ctx context.Context, requestAttributes authorizer.Attributes) (authorizer.Decision, string, error) {\n\treturn authorizer.DecisionNoOpinion, \"\", nil\n}",
			replace: "",
			want:    "method Authorize is missing",
		},
		{
			name:    "wrong signature",
			method:  "func (r *RBACAuthorizer) Authorize(ctx context.Context, requestAttributes authorizer.Attributes) (authorizer.Decision, string, error) {\n\treturn authorizer.DecisionNoOpinion, \"\", nil\n}",
			replace: "func (r *RBACAuthorizer) Authorize(ctx context.Context, requestAttributes authorizer.Attributes) authorizer.Decision {\n\treturn authorizer.DecisionNoOpinion\n}",
			want:    "method Authorize has the wrong signature",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			files := maps.Clone(generatedModuleFiles)
			key := internalPrefix + "/plugin/pkg/auth/authorizer/rbac/rbac.go"
			source := files[key]
			if !strings.Contains(source, test.method) {
				t.Fatalf("fixture does not contain the method to replace")
			}
			files[key] = strings.Replace(source, test.method, test.replace, 1)

			dir := newFixture(t, files)
			_, err := facade.Generate(t.Context(), facade.Options{Dir: dir, Env: goEnvironment(t, dir), Spec: rbacSpec()})
			if !errors.Is(err, facade.ErrAssertion) {
				t.Fatalf("generate error is %v, want ErrAssertion", err)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Errorf("generate error %v does not say %q", err, test.want)
			}
		})
	}
}

// TestGenerateRefusesInternalLeak proves that a relocated type reachable from
// the public API without a facade alias stops the run, and that the message
// names the route that reached it.
func TestGenerateRefusesInternalLeak(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		declaration string
		drop        string
		wantTrail   string
	}{
		{
			name:        "function result",
			declaration: "func Describe() *Diagnostics { return nil }\n\ntype Diagnostics struct{ Reason string }\n",
			wantTrail:   "facade Describe -> result 0 reaches",
		},
		{
			name:        "exported struct field",
			declaration: "type Diagnostics struct{ Reason string }\n",
			drop:        "RBACAuthorizer",
			wantTrail:   "facade RBACAuthorizer -> field Diagnostics reaches",
		},
		{
			name:        "interface method parameter",
			declaration: "type Diagnostics struct{ Reason string }\n",
			drop:        "SubjectLocator",
			wantTrail:   "facade SubjectLocator -> method Diagnose -> parameter d reaches",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			files := maps.Clone(generatedModuleFiles)
			key := internalPrefix + "/plugin/pkg/auth/authorizer/rbac/rbac.go"
			source := files[key] + "\n" + test.declaration
			switch test.drop {
			case "RBACAuthorizer":
				source = strings.Replace(source,
					"type RBACAuthorizer struct {\n\tauthorizationRuleResolver validation.AuthorizationRuleResolver\n}",
					"type RBACAuthorizer struct {\n\tauthorizationRuleResolver validation.AuthorizationRuleResolver\n\tDiagnostics *Diagnostics\n}", 1)
			case "SubjectLocator":
				source = strings.Replace(source,
					"AllowedSubjects(ctx context.Context, attributes authorizer.Attributes) ([]rbacv1.Subject, error)",
					"AllowedSubjects(ctx context.Context, attributes authorizer.Attributes) ([]rbacv1.Subject, error)\n\tDiagnose(d *Diagnostics)", 1)
			}
			files[key] = source

			spec := rbacSpec()
			if test.drop == "" {
				spec.Exports = append(spec.Exports, facade.Export{
					Name: "Describe", Kind: facade.KindFunc, Source: rbacPackage + ".Describe",
				})
			}
			dir := newFixture(t, files)
			_, err := facade.Generate(t.Context(), facade.Options{Dir: dir, Env: goEnvironment(t, dir), Spec: spec})
			if !errors.Is(err, facade.ErrLeak) {
				t.Fatalf("generate error is %v, want ErrLeak", err)
			}
			if !strings.Contains(err.Error(), test.wantTrail) {
				t.Errorf("generate error %v does not name the route %q", err, test.wantTrail)
			}
			if !strings.Contains(err.Error(), "/plugin/pkg/auth/authorizer/rbac.Diagnostics") {
				t.Errorf("generate error %v does not name the leaked type", err)
			}
		})
	}
}

// TestGeneratePublishingLeakedTypeSucceeds proves the leak check is about the
// facade rather than about the type: adding an alias for the same type makes
// the same module generate.
func TestGeneratePublishingLeakedTypeSucceeds(t *testing.T) {
	t.Parallel()
	files := maps.Clone(generatedModuleFiles)
	key := internalPrefix + "/plugin/pkg/auth/authorizer/rbac/rbac.go"
	files[key] += "\nfunc Describe() *Diagnostics { return nil }\n\ntype Diagnostics struct{ Reason string }\n"

	spec := rbacSpec()
	spec.Exports = append(spec.Exports,
		facade.Export{Name: "Describe", Kind: facade.KindFunc, Source: rbacPackage + ".Describe"},
		facade.Export{Name: "Diagnostics", Kind: facade.KindType, Source: rbacPackage + ".Diagnostics"},
	)

	dir := newFixture(t, files)
	env := goEnvironment(t, dir)
	result, err := facade.Generate(t.Context(), facade.Options{Dir: dir, Env: env, Spec: spec})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	source := fileNamed(t, result, "authorizer.go")
	if !strings.Contains(source, "func Describe() *Diagnostics {") {
		t.Errorf("generated facade does not spell the published alias in the signature\n%s", source)
	}
	writeGenerated(t, dir, result.Files)
	assertTypeChecks(t, dir, env)
}

// TestGenerateRefusesMutableVar proves an exported variable never becomes part
// of the published API, whichever way it is asked for.
func TestGenerateRefusesMutableVar(t *testing.T) {
	t.Parallel()
	files := maps.Clone(generatedModuleFiles)
	key := internalPrefix + "/plugin/pkg/auth/authorizer/rbac/rbac.go"
	files[key] += "\nvar DefaultLimit = 10\n"

	for _, kind := range []facade.Kind{facade.KindVar, facade.KindConst} {
		spec := rbacSpec()
		spec.Exports = append(spec.Exports, facade.Export{
			Name: "DefaultLimit", Kind: kind, Source: rbacPackage + ".DefaultLimit",
		})
		dir := newFixture(t, files)
		_, err := facade.Generate(t.Context(), facade.Options{Dir: dir, Env: goEnvironment(t, dir), Spec: spec})
		switch {
		case kind == facade.KindVar && !errors.Is(err, facade.ErrMutableVar):
			t.Errorf("generate error for a declared var is %v, want ErrMutableVar", err)
		case kind == facade.KindConst && !errors.Is(err, facade.ErrKindMismatch):
			t.Errorf("generate error for a var declared as a const is %v, want ErrKindMismatch", err)
		}
	}
}

// TestGenerateRefusesUpstreamChanges covers the failures an upstream bump
// produces: a symbol that is gone, and one whose kind changed.
func TestGenerateRefusesUpstreamChanges(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(map[string]string)
		export facade.Export
		want   error
	}{
		{
			name:   "removed symbol",
			mutate: func(map[string]string) {},
			export: facade.Export{Name: "Gone", Kind: facade.KindFunc, Source: rbacPackage + ".Gone"},
			want:   facade.ErrMissingSymbol,
		},
		{
			name: "struct became an interface",
			mutate: func(files map[string]string) {
				key := internalPrefix + "/plugin/pkg/auth/authorizer/rbac/rbac.go"
				files[key] += "\ntype Evaluator struct{ Name string }\n"
			},
			export: facade.Export{Name: "Evaluator", Kind: facade.KindInterface, Source: rbacPackage + ".Evaluator"},
			want:   facade.ErrKindMismatch,
		},
		{
			name: "interface became a struct",
			mutate: func(files map[string]string) {
				key := internalPrefix + "/plugin/pkg/auth/authorizer/rbac/rbac.go"
				files[key] += "\ntype Evaluator interface{ Evaluate() bool }\n"
			},
			export: facade.Export{Name: "Evaluator", Kind: facade.KindType, Source: rbacPackage + ".Evaluator"},
			want:   facade.ErrKindMismatch,
		},
		{
			name: "function became a type",
			mutate: func(files map[string]string) {
				key := internalPrefix + "/plugin/pkg/auth/authorizer/rbac/rbac.go"
				files[key] += "\ntype Evaluate struct{}\n"
			},
			export: facade.Export{Name: "Evaluate", Kind: facade.KindFunc, Source: rbacPackage + ".Evaluate"},
			want:   facade.ErrKindMismatch,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			files := maps.Clone(generatedModuleFiles)
			test.mutate(files)
			spec := rbacSpec()
			spec.Exports = append(spec.Exports, test.export)

			dir := newFixture(t, files)
			_, err := facade.Generate(t.Context(), facade.Options{Dir: dir, Env: goEnvironment(t, dir), Spec: spec})
			if !errors.Is(err, test.want) {
				t.Fatalf("generate error is %v, want %v", err, test.want)
			}
		})
	}
}

// TestGenerateGenerics covers the two answers this package gives to a generic
// declaration: a generic type is aliased, because an alias forwards its
// parameters unchanged and preserves identity exactly; a generic function is
// refused, because a forwarding declaration is a different function whose type
// inference cannot be proved identical.
func TestGenerateGenerics(t *testing.T) {
	t.Parallel()
	files := maps.Clone(generatedModuleFiles)
	key := internalPrefix + "/plugin/pkg/auth/authorizer/rbac/rbac.go"
	files[key] += `
type Set[T comparable] struct {
	Members []T
}

func (s *Set[T]) Contains(member T) bool { return false }

func NewSet[T comparable](members ...T) *Set[T] { return &Set[T]{Members: members} }
`

	t.Run("generic type is aliased", func(t *testing.T) {
		t.Parallel()
		spec := rbacSpec()
		spec.Exports = append(spec.Exports, facade.Export{
			Name: "Set", Kind: facade.KindType, Source: rbacPackage + ".Set",
		})
		dir := newFixture(t, files)
		env := goEnvironment(t, dir)
		result, err := facade.Generate(t.Context(), facade.Options{Dir: dir, Env: env, Spec: spec})
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		source := fileNamed(t, result, "authorizer.go")
		if !strings.Contains(source, "type Set[T comparable] = rbac.Set[T]") {
			t.Errorf("generated facade does not alias the generic type\n%s", source)
		}
		writeGenerated(t, dir, result.Files)
		assertTypeChecks(t, dir, env)
	})

	t.Run("generic function is refused", func(t *testing.T) {
		t.Parallel()
		spec := rbacSpec()
		spec.Exports = append(spec.Exports, facade.Export{
			Name: "NewSet", Kind: facade.KindFunc, Source: rbacPackage + ".NewSet",
		})
		dir := newFixture(t, files)
		_, err := facade.Generate(t.Context(), facade.Options{Dir: dir, Env: goEnvironment(t, dir), Spec: spec})
		if !errors.Is(err, facade.ErrGeneric) {
			t.Fatalf("generate error is %v, want ErrGeneric", err)
		}
	})
}

// TestGenerateRenamesCapturingParameter proves a forwarding declaration never
// lets a parameter name capture something the declaration itself needs.
//
// A parameter is in scope across the whole signature, so an upstream parameter
// named after the package the body forwards to, or after a facade type the
// signature spells, would either fail to compile or resolve to the parameter.
func TestGenerateRenamesCapturingParameter(t *testing.T) {
	t.Parallel()
	files := maps.Clone(generatedModuleFiles)
	key := internalPrefix + "/plugin/pkg/auth/authorizer/rbac/rbac.go"
	files[key] += `
func Capture(rbac string, RBACAuthorizer int, _ bool, error string) string { return rbac }
`
	spec := rbacSpec()
	spec.Exports = append(spec.Exports, facade.Export{
		Name: "Capture", Kind: facade.KindFunc, Source: rbacPackage + ".Capture",
	})

	dir := newFixture(t, files)
	env := goEnvironment(t, dir)
	result, err := facade.Generate(t.Context(), facade.Options{Dir: dir, Env: env, Spec: spec})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	source := fileNamed(t, result, "authorizer.go")
	if !strings.Contains(source, "func Capture(a0 string, a1 int, a2 bool, a3 string) string {") {
		t.Errorf("generated facade does not rename capturing parameters\n%s", source)
	}
	writeGenerated(t, dir, result.Files)
	assertTypeChecks(t, dir, env)
}

// TestGenerateReturnsRelocatableFiles proves the generated files compose into a
// relocated file set through the same write boundary as copied files.
func TestGenerateReturnsRelocatableFiles(t *testing.T) {
	t.Parallel()
	dir := newRBACFixture(t)
	result := generate(t, dir, rbacSpec())

	set, err := relocate.FileSet{}.With(result.Files...)
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	for _, name := range []string{"authorizer.go", "zz_generated_assertions.go"} {
		file, ok := set.Lookup(name)
		switch {
		case !ok:
			t.Fatalf("composed set has no %s", name)
		case !file.Generated:
			t.Errorf("%s is not marked generated, though it carries the generated marker", name)
		case file.Mode != relocate.ModeRegular:
			t.Errorf("%s has mode %v, want a regular file", name, file.Mode)
		case file.Source != "":
			t.Errorf("%s claims upstream source %q, though it is generated", name, file.Source)
		}
	}
}

// TestLoadRefusesAmbientEnvironment proves the loader never falls back to the
// environment the process happens to have, which would make the published API
// depend on the shell the run started in.
func TestLoadRefusesAmbientEnvironment(t *testing.T) {
	t.Parallel()
	dir := newRBACFixture(t)
	for _, test := range []struct {
		name string
		opts facade.Options
	}{
		{"no environment", facade.Options{Dir: dir, Spec: rbacSpec()}},
		{"empty environment", facade.Options{Dir: dir, Env: []string{}, Spec: rbacSpec()}},
		{"relative directory", facade.Options{Dir: "gen", Env: []string{"HOME=/tmp"}, Spec: rbacSpec()}},
		{"no directory", facade.Options{Env: []string{"HOME=/tmp"}, Spec: rbacSpec()}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := facade.Generate(t.Context(), test.opts); !errors.Is(err, facade.ErrLoad) {
				t.Fatalf("generate error is %v, want ErrLoad", err)
			}
		})
	}
}

// writeGenerated writes the generated files into the module so the whole thing
// can be type checked as a consumer would see it.
func writeGenerated(t *testing.T, dir string, files []relocate.File) {
	t.Helper()
	for _, file := range files {
		path := filepath.Join(dir, filepath.FromSlash(file.Path))
		if err := os.WriteFile(path, file.Contents, 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
}

// assertTypeChecks loads the whole module and fails on any diagnostic.
//
// Type checking is the strongest statement available about generated Go that
// does not involve running it: everything the generator can get wrong, from an
// import alias that collides to a forwarding call whose arguments do not match,
// is a type error.
func assertTypeChecks(t *testing.T, dir string, env []string) {
	t.Helper()
	loaded, err := packages.Load(&packages.Config{
		Context: t.Context(),
		Mode:    packages.NeedName | packages.NeedTypes | packages.NeedImports | packages.NeedDeps | packages.NeedSyntax | packages.NeedTypesInfo,
		Dir:     dir,
		Env:     env,
	}, "./...")
	if err != nil {
		t.Fatalf("load generated module: %v", err)
	}
	if len(loaded) == 0 {
		t.Fatalf("load generated module matched no package")
	}
	packages.Visit(loaded, nil, func(pkg *packages.Package) {
		for _, problem := range pkg.Errors {
			t.Errorf("%s: %v", pkg.PkgPath, problem)
		}
	})
}

// entryNamed reports the manifest entry published under a name.
func entryNamed(manifest facade.Manifest, name string) (facade.Entry, bool) {
	for _, entry := range manifest.Entries {
		if entry.Name == name {
			return entry, true
		}
	}
	return facade.Entry{}, false
}

// collapseSpaces squashes runs of spaces so a comparison does not depend on the
// columns gofmt aligns a declaration block into.
func collapseSpaces(source string) string {
	for strings.Contains(source, "  ") {
		source = strings.ReplaceAll(source, "  ", " ")
	}
	return source
}

// slicesReverse reverses a slice in place, which the determinism test uses to
// prove the input order does not reach the output.
func slicesReverse[T any](values []T) {
	for i, j := 0, len(values)-1; i < j; i, j = i+1, j-1 {
		values[i], values[j] = values[j], values[i]
	}
}
