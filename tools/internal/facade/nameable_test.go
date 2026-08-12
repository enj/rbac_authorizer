package facade_test

import (
	"errors"
	"maps"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/facade"
)

// rbacFile is the fixture file the nameability tests extend.
const rbacFile = internalPrefix + "/plugin/pkg/auth/authorizer/rbac/rbac.go"

// withRBACDeclarations returns the fixture with extra declarations appended to
// the relocated rbac package.
func withRBACDeclarations(source string) map[string]string {
	files := maps.Clone(generatedModuleFiles)
	files[rbacFile] += source
	return files
}

// TestGenerateResolvesThroughInternalAliases proves an alias declared inside the
// generated module is not a leak.
//
// An alias declares no type: what a consumer has to be able to name is what it
// denotes. An internal alias to a dependency's type, to a basic type, or to an
// unnamed type therefore publishes nothing unnameable, and reporting one as a
// leak would refuse a facade that is perfectly usable. The generated file has to
// agree, so the signature is checked to spell the target rather than the
// internal alias whose name no consumer can write.
func TestGenerateResolvesThroughInternalAliases(t *testing.T) {
	t.Parallel()
	files := withRBACDeclarations(`
// These three alias a dependency's named type, a predeclared type, and an
// unnamed type. None of them is a type of its own.
type Decision = authorizer.Decision

type Count = int

type Names = []string

func Summarize() (Decision, Count, Names) { return authorizer.DecisionNoOpinion, 0, nil }
`)
	spec := rbacSpec()
	spec.Exports = append(spec.Exports, facade.Export{
		Name: "Summarize", Kind: facade.KindFunc, Source: rbacPackage + ".Summarize",
	})

	dir := newFixture(t, files)
	env := goEnvironment(t, dir)
	result, err := facade.Generate(t.Context(), facade.Options{Dir: dir, Env: env, Spec: spec})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	source := fileNamed(t, result, "authorizer.go")
	if !strings.Contains(source, "func Summarize() (authorizer.Decision, int, []string) {") {
		t.Errorf("generated facade does not resolve through the internal aliases\n%s", source)
	}
	writeGenerated(t, dir, result.Files)
	assertTypeChecks(t, dir, env)
}

// TestGenerateRefusesDependencyInternalType proves the nameability rule is about
// the consumer rather than about this module's own boundary.
//
// A dependency that exposes one of its own internal types in its public API is
// an upstream mistake, but republishing that type here makes it this module's
// mistake: a consumer can receive the value and cannot declare, implement, or
// accept it, exactly as if it had come from this module's internal tree.
func TestGenerateRefusesDependencyInternalType(t *testing.T) {
	t.Parallel()
	files := withRBACDeclarations(`
// Extender embeds a dependency interface whose method set reaches that
// dependency's own internal package. Publishing it would ask a consumer to
// implement a method they cannot write.
type Extender interface {
	authorizer.Extended
}
`)
	spec := rbacSpec()
	spec.Exports = append(spec.Exports, facade.Export{
		Name: "Extender", Kind: facade.KindInterface, Source: rbacPackage + ".Extender",
	})

	dir := newFixture(t, files)
	_, err := facade.Generate(t.Context(), facade.Options{Dir: dir, Env: goEnvironment(t, dir), Spec: spec})
	if !errors.Is(err, facade.ErrLeak) {
		t.Fatalf("generate error is %v, want ErrLeak", err)
	}
	for _, want := range []string{
		"facade Extender -> method Handle -> result 0",
		"example.com/apiserver/pkg/internal/private.Handle",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("generate error %v does not say %q", err, want)
		}
	}
}

// TestGenerateKeepsDependencyAliasOverInternalType proves the rule is about what
// a consumer can write rather than about where a type happens to live.
//
// A dependency that exports an alias over one of its own internal types has
// given every importer a legal spelling for it. Resolving through that alias
// would produce a reference the generated module is not even permitted to
// import, so the alias is kept as written and nothing is refused.
func TestGenerateKeepsDependencyAliasOverInternalType(t *testing.T) {
	t.Parallel()
	files := withRBACDeclarations(`
func Described() *authorizer.Handle { return nil }
`)
	spec := rbacSpec()
	spec.Exports = append(spec.Exports, facade.Export{
		Name: "Described", Kind: facade.KindFunc, Source: rbacPackage + ".Described",
	})

	dir := newFixture(t, files)
	env := goEnvironment(t, dir)
	result, err := facade.Generate(t.Context(), facade.Options{Dir: dir, Env: env, Spec: spec})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	source := fileNamed(t, result, "authorizer.go")
	if !strings.Contains(source, "func Described() *authorizer.Handle {") {
		t.Errorf("generated facade does not keep the dependency's own alias\n%s", source)
	}
	if strings.Contains(source, "private.Handle") {
		t.Errorf("generated facade resolved through a dependency alias to a package it cannot import\n%s", source)
	}
	writeGenerated(t, dir, result.Files)
	assertTypeChecks(t, dir, env)
}

// TestGenerateRefusesUnnameableConstraint proves a published generic type's
// parameter list is part of what a consumer has to be able to write.
//
// The generated alias repeats the constraint verbatim, so a constraint that
// names an unpublished internal type produces a declaration a consumer can see
// and cannot satisfy.
func TestGenerateRefusesUnnameableConstraint(t *testing.T) {
	t.Parallel()
	files := withRBACDeclarations(`
type Key interface{ ~string }

type Table[T Key] struct {
	Rows []T
}
`)
	spec := rbacSpec()
	spec.Exports = append(spec.Exports, facade.Export{
		Name: "Table", Kind: facade.KindType, Source: rbacPackage + ".Table",
	})

	dir := newFixture(t, files)
	_, err := facade.Generate(t.Context(), facade.Options{Dir: dir, Env: goEnvironment(t, dir), Spec: spec})
	if !errors.Is(err, facade.ErrLeak) {
		t.Fatalf("generate error is %v, want ErrLeak", err)
	}
	if !strings.Contains(err.Error(), "facade Table -> constraint of T reaches") {
		t.Errorf("generate error %v does not name the constraint", err)
	}

	// Publishing the constraint makes the same module generate, which is what
	// proves the check is about the facade rather than about the type.
	spec.Exports = append(spec.Exports, facade.Export{
		Name: "Key", Kind: facade.KindInterface, Source: rbacPackage + ".Key",
	})
	dir = newFixture(t, files)
	env := goEnvironment(t, dir)
	result, err := facade.Generate(t.Context(), facade.Options{Dir: dir, Env: env, Spec: spec})
	if err != nil {
		t.Fatalf("generate with the constraint published: %v", err)
	}
	if source := fileNamed(t, result, "authorizer.go"); !strings.Contains(source, "type Table[T Key] = rbac.Table[T]") {
		t.Errorf("generated facade does not spell the published constraint\n%s", source)
	}
	writeGenerated(t, dir, result.Files)
	assertTypeChecks(t, dir, env)
}

// TestGenerateRefusesUnrepresentableType proves the generator will not emit a
// type it cannot reproduce identically.
//
// An unnamed struct or interface is spelled structurally, and an unexported
// member is qualified by the package that declared it. The same characters
// written in the generated package would declare a different type, so a facade
// that emitted them would publish a signature that looks like the upstream one
// and accepts nothing the upstream one accepts.
func TestGenerateRefusesUnrepresentableType(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		declaration string
		export      facade.Export
		want        string
	}{
		{
			name:        "unnamed struct with an unexported field",
			declaration: "\nfunc Anonymous() struct{ hidden int } { return struct{ hidden int }{} }\n",
			export:      facade.Export{Name: "Anonymous", Kind: facade.KindFunc, Source: rbacPackage + ".Anonymous"},
			want:        "unexported field hidden",
		},
		{
			name:        "unnamed interface with an unexported method",
			declaration: "\nfunc Sealed() interface{ seal() } { return nil }\n",
			export:      facade.Export{Name: "Sealed", Kind: facade.KindFunc, Source: rbacPackage + ".Sealed"},
			want:        "unexported method seal",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			spec := rbacSpec()
			spec.Exports = append(spec.Exports, test.export)

			dir := newFixture(t, withRBACDeclarations(test.declaration))
			_, err := facade.Generate(t.Context(), facade.Options{Dir: dir, Env: goEnvironment(t, dir), Spec: spec})
			if !errors.Is(err, facade.ErrUnrepresentable) {
				t.Fatalf("generate error is %v, want ErrUnrepresentable", err)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Errorf("generate error %v does not say %q", err, test.want)
			}
			if !strings.Contains(err.Error(), rbacPackage[len(sourcePrefix)+1:]) {
				t.Errorf("generate error %v does not name the package that owns the member", err)
			}
		})
	}
}
