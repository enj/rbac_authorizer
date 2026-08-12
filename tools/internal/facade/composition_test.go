package facade_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/facade"
	"github.com/enj/soapbox/tools/internal/provenance"
	"github.com/enj/soapbox/tools/internal/relocate"
	"github.com/enj/soapbox/tools/internal/rewrite"
)

// TestGeneratedRootComposesIntoOneTree is the integration the two generators
// exist for.
//
// The curated facade and the root provenance are produced by different packages
// from different inputs, and they land in one module root beside relocated
// upstream code. Nothing checks that they fit together until they are composed:
// a path collision, a second package documentation comment, or a root file that
// shadows a relocated directory would each produce a module that fails
// somewhere far from the code that generated it.
func TestGeneratedRootComposesIntoOneTree(t *testing.T) {
	t.Parallel()
	dir := newRBACFixture(t)
	result := generate(t, dir, rbacSpec())

	roots := rootProvenance(result.Manifest)
	rootFiles, err := roots.Files()
	if err != nil {
		t.Fatalf("root provenance: %v", err)
	}

	relocated, err := relocate.Build(t.Context(), relocate.Plan{Files: []relocate.PlanFile{
		{
			Path: "plugin/pkg/auth/authorizer/rbac/rbac.go", Package: "plugin/pkg/auth/authorizer/rbac",
			Mode: relocate.ModeRegular, Contents: []byte("package rbac\n"),
		},
		{
			Path: "pkg/registry/rbac/validation/rule.go", Package: "pkg/registry/rbac/validation",
			Mode: relocate.ModeRegular, Contents: []byte("package validation\n"),
		},
	}}, relocate.Options{InternalPrefix: internalPrefix})
	if err != nil {
		t.Fatalf("relocate: %v", err)
	}

	composed, err := relocated.With(append(result.Files, rootFiles...)...)
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	// The root record and the composed tree have to agree. Validation can only
	// see the records it was handed, so this is the check that proves the
	// NOTICE describes the tree that is actually published.
	if err := roots.CrossCheck(composed); err != nil {
		t.Fatalf("cross check: %v", err)
	}

	want := []string{
		"LICENSE",
		"NOTICE",
		"README.md",
		"authorizer.go",
		"doc.go",
		"internal/kk/pkg/registry/rbac/validation/rule.go",
		"internal/kk/plugin/pkg/auth/authorizer/rbac/rbac.go",
		"zz_generated_assertions.go",
	}
	if got := strings.Join(pathsOf(composed), "\n"); got != strings.Join(want, "\n") {
		t.Fatalf("composed tree is\n%s\nwant\n%s", got, strings.Join(want, "\n"))
	}

	destination := filepath.Join(t.TempDir(), "module")
	if err := relocate.Materialize(t.Context(), destination, composed); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	assertOnePackageComment(t, destination, []string{"authorizer.go", "doc.go", "zz_generated_assertions.go"})
}

// assertOnePackageComment proves the root package has exactly one documentation
// comment and that every root file declares the same package.
//
// Two package comments is not a compile error; it is a package whose
// documentation is whichever file the tooling happened to read first, which is
// exactly the sort of thing a generator produces and nobody notices. The facade
// therefore carries the generated marker separated by a blank line, so the
// marker cannot become documentation, and doc.go is the only file that
// documents the package.
func assertOnePackageComment(t *testing.T, destination string, names []string) {
	t.Helper()
	documented := 0
	packageName := ""
	for _, name := range names {
		source, err := os.ReadFile(filepath.Join(destination, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), name, source, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		if !ast.IsGenerated(parsed) {
			t.Errorf("%s does not carry the generated file marker", name)
		}
		switch {
		case packageName == "":
			packageName = parsed.Name.Name
		case parsed.Name.Name != packageName:
			t.Errorf("%s declares package %s, but %s declares %s", name, parsed.Name.Name, names[0], packageName)
		}
		if parsed.Doc != nil {
			documented++
			if name != provenance.DocFileName {
				t.Errorf("%s carries a package documentation comment, which belongs in %s:\n%s",
					name, provenance.DocFileName, parsed.Doc.Text())
			}
		}
	}
	if documented != 1 {
		t.Errorf("the root package has %d documentation comments, want exactly one", documented)
	}
}

// rootProvenance builds root provenance that matches the fixture, including the
// published API taken from the facade manifest so the README cannot describe an
// API the module does not have.
func rootProvenance(manifest facade.Manifest) provenance.Options {
	const sha = "3f6c1ad2b1e0b3b2d34f9d31a4f8e7c6d5a49182"
	record := rewrite.NewPackageProvenance(
		internalPrefix+"/plugin/pkg/auth/authorizer/rbac",
		"plugin/pkg/auth/authorizer/rbac",
		rewrite.Options{SourceRepository: "https://github.com/kubernetes/kubernetes.git", SourceSHA: sha})
	record.AddFile(
		rewrite.File{
			Path:       internalPrefix + "/plugin/pkg/auth/authorizer/rbac/rbac.go",
			SourcePath: "plugin/pkg/auth/authorizer/rbac/rbac.go",
		},
		rewrite.Result{Changes: []rewrite.Change{{
			Kind: rewrite.ChangeImport,
			Path: internalPrefix + "/plugin/pkg/auth/authorizer/rbac/rbac.go",
			Line: 8,
		}}})
	validation := rewrite.NewPackageProvenance(
		internalPrefix+"/pkg/registry/rbac/validation",
		"pkg/registry/rbac/validation",
		rewrite.Options{SourceRepository: "https://github.com/kubernetes/kubernetes.git", SourceSHA: sha})
	validation.AddFile(
		rewrite.File{
			Path:       internalPrefix + "/pkg/registry/rbac/validation/rule.go",
			SourcePath: "pkg/registry/rbac/validation/rule.go",
		},
		rewrite.Result{Changes: []rewrite.Change{{
			Kind: rewrite.ChangeNotice,
			Path: internalPrefix + "/pkg/registry/rbac/validation/rule.go",
			Line: 1,
		}}})

	names := make([]string, len(manifest.Entries))
	for i, entry := range manifest.Entries {
		names[i] = entry.Name
	}
	return provenance.Options{
		Module:         manifest.Module,
		RootPackage:    manifest.Package,
		Repository:     "https://github.com/enj/rbac_authorizer",
		InternalPrefix: internalPrefix,
		Summary:        "RBAC authorization extracted from Kubernetes as an independently consumable Go module.",
		Source: provenance.Source{
			Repository: "https://github.com/kubernetes/kubernetes.git",
			Module:     sourcePrefix,
			Project:    "Kubernetes",
			SHA:        sha,
			Tag:        "v1.36.1",
			Packages:   []string{"plugin/pkg/auth/authorizer/rbac"},
		},
		License:        []byte("Apache License\nVersion 2.0, January 2004\n"),
		LicenseID:      provenance.Apache20,
		UpstreamNotice: []byte("Copyright 2014 The Kubernetes Authors.\n"),
		Packages:       []*rewrite.PackageProvenance{record, validation},
		PublicAPI:      names,
	}
}

// pathsOf reports the destination paths of a relocated set.
func pathsOf(set relocate.FileSet) []string {
	names := make([]string, len(set.Files))
	for i, file := range set.Files {
		names[i] = file.Path
	}
	return names
}
