package relocate_test

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/relocate"
)

// newRelocatedSet builds a small relocated set to compose generated files into.
func newRelocatedSet(t *testing.T) relocate.FileSet {
	t.Helper()
	set, err := relocate.Build(t.Context(), relocate.Plan{Files: []relocate.PlanFile{
		regular("pkg/registry/rbac/validation", "rule.go", "package validation\n"),
		regular("plugin/pkg/auth/authorizer/rbac", "rbac.go", "package rbac\n"),
	}}, relocate.Options{InternalPrefix: internalPrefix})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return set
}

// generated builds a root file the engine produced rather than copied.
func generated(path, contents string) relocate.File {
	return relocate.File{Path: path, Mode: relocate.ModeRegular, Contents: []byte(contents), Generated: true}
}

// TestWithComposesGeneratedRootFiles proves the curated facade and the root
// provenance reach the tree through the same boundary as copied code, sorted
// into one set with it.
func TestWithComposesGeneratedRootFiles(t *testing.T) {
	t.Parallel()
	set := newRelocatedSet(t)
	composed, err := set.With(
		generated("zz_generated_assertions.go", "package rbacauthorizer\n"),
		generated("authorizer.go", "package rbacauthorizer\n"),
		relocate.File{Path: "LICENSE", Mode: relocate.ModeRegular, Contents: []byte("Apache License\n")},
		relocate.File{Path: "NOTICE", Mode: relocate.ModeRegular, Contents: []byte("notices\n")},
	)
	if err != nil {
		t.Fatalf("compose: %v", err)
	}

	want := []string{
		"LICENSE",
		"NOTICE",
		"authorizer.go",
		"internal/kk/pkg/registry/rbac/validation/rule.go",
		"internal/kk/plugin/pkg/auth/authorizer/rbac/rbac.go",
		"zz_generated_assertions.go",
	}
	if got := strings.Join(paths(composed), "\n"); got != strings.Join(want, "\n") {
		t.Errorf("composed paths are\n%s\nwant\n%s", got, strings.Join(want, "\n"))
	}
	// Lookup binary searches, so a set whose order was not restored would miss
	// files that are present.
	for _, name := range want {
		if _, ok := composed.Lookup(name); !ok {
			t.Errorf("composed set cannot look up %s", name)
		}
	}
	// The root files belong to no relocated package, so they must not create
	// one.
	for _, pkg := range composed.Packages {
		if pkg.Path == "" || pkg.Path == "." {
			t.Errorf("composition created a package record for the module root")
		}
	}
}

// TestWithLeavesTheReceiverAlone proves composition is not an append.
//
// A caller may compose several times from one relocated set, and a method that
// grew the receiver's array in place would let one composition become visible
// through an unrelated copy of the set, which is the kind of aliasing bug that
// only shows up once two callers exist.
func TestWithLeavesTheReceiverAlone(t *testing.T) {
	t.Parallel()
	set := newRelocatedSet(t)
	before := strings.Join(paths(set), "\n")

	first, err := set.With(generated("authorizer.go", "package rbacauthorizer\n"))
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	second, err := set.With(generated("doc.go", "package rbacauthorizer\n"))
	if err != nil {
		t.Fatalf("compose: %v", err)
	}

	if after := strings.Join(paths(set), "\n"); after != before {
		t.Errorf("composition changed the receiver:\n%s", after)
	}
	if _, ok := first.Lookup("doc.go"); ok {
		t.Errorf("the second composition is visible through the first")
	}
	if _, ok := second.Lookup("authorizer.go"); ok {
		t.Errorf("the first composition is visible through the second")
	}
}

// TestWithRefusesBrokenTrees proves the composition boundary enforces every
// invariant the copy boundary does.
//
// Each case is a tree that cannot be written, or one that would materialize
// differently on a case insensitive file system than the set describes, and
// every one of them has to be refused here rather than discovered halfway
// through writing a directory.
func TestWithRefusesBrokenTrees(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		files []relocate.File
		want  error
	}{
		{
			name: "generated file collides with a relocated one",
			files: []relocate.File{{
				Source:        "plugin/pkg/auth/authorizer/rbac/other.go",
				Path:          "internal/kk/plugin/pkg/auth/authorizer/rbac/rbac.go",
				Package:       "internal/kk/plugin/pkg/auth/authorizer/rbac",
				SourcePackage: "plugin/pkg/auth/authorizer/rbac",
				Mode:          relocate.ModeRegular,
				Contents:      []byte("package rbac\n"),
			}},
			want: relocate.ErrCollision,
		},
		{
			name: "two generated files differ only in case",
			files: []relocate.File{
				generated("authorizer.go", "package rbacauthorizer\n"),
				generated("Authorizer.go", "package rbacauthorizer\n"),
			},
			want: relocate.ErrCaseCollision,
		},
		{
			name:  "a generated file occupies a relocated directory",
			files: []relocate.File{generated("internal/kk/plugin", "not a directory\n")},
			want:  relocate.ErrOverlap,
		},
		{
			name:  "a generated file escapes the module root",
			files: []relocate.File{generated("../authorizer.go", "package rbacauthorizer\n")},
			want:  nil,
		},
		{
			name:  "a generated file has a mode Git cannot record",
			files: []relocate.File{{Path: "authorizer.go", Mode: relocate.Mode(9), Contents: []byte("x")}},
			want:  relocate.ErrUnsupportedMode,
		},
		{
			// A composed link is refused whatever it points at. The receiver
			// was built under a policy this method cannot see, so accepting one
			// would quietly grant a set relocated under SymlinkReject the link
			// rules it was built to refuse.
			name:  "a composed symbolic link pointing outside the set",
			files: []relocate.File{{Path: "authorizer.go", Mode: relocate.ModeSymlink, Contents: []byte("../elsewhere.go")}},
			want:  relocate.ErrSymlink,
		},
		{
			name: "a composed symbolic link resolving inside the set",
			files: []relocate.File{{
				Path:     "authorizer.go",
				Mode:     relocate.ModeSymlink,
				Contents: []byte("internal/kk/plugin/pkg/auth/authorizer/rbac/rbac.go"),
			}},
			want: relocate.ErrSymlink,
		},
		{
			// A Go file below the root that declares no package would compile
			// into the relocated package it sits in while appearing in no
			// package record, so nothing reading the set could account for it.
			name:  "an unclassified Go file inside a relocated package",
			files: []relocate.File{generated("internal/kk/plugin/pkg/auth/authorizer/rbac/helper.go", "package rbac\n")},
			want:  relocate.ErrUnclassified,
		},
		{
			name:  "an unclassified Go file in a new directory",
			files: []relocate.File{generated("support/helper.go", "package support\n")},
			want:  relocate.ErrUnclassified,
		},
		{
			name: "a generated file claims a package it does not live in",
			files: []relocate.File{{
				Path: "authorizer.go", Package: "internal/kk", Mode: relocate.ModeRegular,
				Contents: []byte("package rbacauthorizer\n"),
			}},
			want: relocate.ErrPackageBoundary,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := newRelocatedSet(t).With(test.files...)
			switch {
			case err == nil:
				t.Fatalf("compose accepted a tree that cannot be written")
			case test.want != nil && !errors.Is(err, test.want):
				t.Fatalf("compose error is %v, want %v", err, test.want)
			}
		})
	}
}

// TestWithEmptyAdditionRevalidates proves the guarantee of the result does not
// depend on how many files were added, which is what lets a caller compose in a
// loop without reasoning about the empty case.
func TestWithEmptyAdditionRevalidates(t *testing.T) {
	t.Parallel()
	composed, err := newRelocatedSet(t).With()
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	if len(composed.Files) != 2 {
		t.Errorf("composing nothing produced %d files, want the original 2", len(composed.Files))
	}

	// A set assembled by hand is checked exactly as one from Build is, because
	// FileSet is an exported type with exported fields and composition is a
	// write boundary.
	broken := relocate.FileSet{Files: []relocate.File{
		{Path: "b.go", Mode: relocate.ModeRegular},
		{Path: "a.go", Mode: relocate.ModeRegular},
	}}
	if _, err := broken.With(); err != nil {
		// Composition sorts before checking, so an unsorted hand assembled set
		// is repaired rather than refused; what must not happen is that it is
		// accepted while still unsorted.
		t.Fatalf("compose: %v", err)
	}
	repaired, err := broken.With()
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	if got := strings.Join(paths(repaired), ","); got != "a.go,b.go" {
		t.Errorf("composed paths are %s, want them sorted", got)
	}
}

// TestComposeClassifiesInTreeGoFiles proves the classification rule admits
// exactly what it should.
//
// A root file belongs to no relocated package and must not invent one. A Go
// file below the root must say which package it joins, and once it does it
// appears in the package records and so in everything that reads them. A file
// that is not Go is neither compiled nor part of a package, so it is carried
// without a record.
func TestComposeClassifiesInTreeGoFiles(t *testing.T) {
	t.Parallel()
	composed, err := newRelocatedSet(t).With(
		generated("authorizer.go", "package rbacauthorizer\n"),
		relocate.File{
			Source:        "plugin/pkg/auth/authorizer/rbac/zz_generated.go",
			Path:          "internal/kk/plugin/pkg/auth/authorizer/rbac/zz_generated.go",
			Package:       "internal/kk/plugin/pkg/auth/authorizer/rbac",
			SourcePackage: "plugin/pkg/auth/authorizer/rbac",
			Mode:          relocate.ModeRegular,
			Contents:      []byte("package rbac\n"),
			Generated:     true,
		},
		relocate.File{
			Path:     "internal/kk/plugin/pkg/auth/authorizer/rbac/LICENSE",
			Mode:     relocate.ModeRegular,
			Contents: []byte("Apache License\n"),
		},
	)
	if err != nil {
		t.Fatalf("compose: %v", err)
	}

	var rbac relocate.Package
	for _, pkg := range composed.Packages {
		if pkg.Path == "internal/kk/plugin/pkg/auth/authorizer/rbac" {
			rbac = pkg
		}
	}
	if !slices.Contains(rbac.Files, "internal/kk/plugin/pkg/auth/authorizer/rbac/zz_generated.go") {
		t.Errorf("a classified Go file is missing from its package record: %v", rbac.Files)
	}
	// The root file and the licence belong to no package, so neither may
	// appear in a record nor create one.
	for _, pkg := range composed.Packages {
		if slices.Contains(pkg.Files, "authorizer.go") {
			t.Errorf("a root file was recorded in package %s", pkg.Path)
		}
		if slices.Contains(pkg.Files, "internal/kk/plugin/pkg/auth/authorizer/rbac/LICENSE") {
			t.Errorf("a licence was recorded as package source in %s", pkg.Path)
		}
	}
}

// TestComposeKeepsTheSymlinkPolicy proves composition cannot widen the policy
// the receiver was relocated under.
//
// A set built under SymlinkReject holds no link, and the permissive check the
// combined set goes through would accept one that composition added. Refusing
// links among the added files closes that without constraining the receiver,
// and costs nothing: a generated facade, a licence, and a notice are all
// regular files.
func TestComposeKeepsTheSymlinkPolicy(t *testing.T) {
	t.Parallel()
	rejecting, err := relocate.Build(t.Context(), relocate.Plan{Files: []relocate.PlanFile{
		regular("plugin/pkg/auth/authorizer/rbac", "rbac.go", "package rbac\n"),
	}}, relocate.Options{InternalPrefix: internalPrefix, Symlinks: relocate.SymlinkReject})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	link := relocate.File{
		Path:     "authorizer.go",
		Mode:     relocate.ModeSymlink,
		Contents: []byte("internal/kk/plugin/pkg/auth/authorizer/rbac/rbac.go"),
	}
	if _, err := rejecting.With(link); !errors.Is(err, relocate.ErrSymlink) {
		t.Fatalf("compose error is %v, want ErrSymlink", err)
	}

	// A set relocated under the permissive policy keeps the links it was built
	// with, which composition must not disturb.
	permissive, err := relocate.Build(t.Context(), relocate.Plan{Files: []relocate.PlanFile{
		regular("plugin/pkg/auth/authorizer/rbac", "rbac.go", "package rbac\n"),
		{
			Path:     "plugin/pkg/auth/authorizer/rbac/link.go",
			Package:  "plugin/pkg/auth/authorizer/rbac",
			Mode:     relocate.ModeSymlink,
			Contents: []byte("rbac.go"),
		},
	}}, relocate.Options{InternalPrefix: internalPrefix, Symlinks: relocate.SymlinkInternal})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	composed, err := permissive.With(generated("authorizer.go", "package rbacauthorizer\n"))
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	if _, ok := composed.Lookup("internal/kk/plugin/pkg/auth/authorizer/rbac/link.go"); !ok {
		t.Errorf("composition dropped a link the receiver was built with")
	}
}

// TestComposeGroupsPackagesDeterministically proves the package records of a
// composed set do not depend on the order the files arrived in.
//
// Sorting package records by destination alone is not a total order, and
// SortFunc is not stable, so two records sharing a directory while naming
// different upstream ones could merge either way. The record that survives
// decides which upstream package the tree reports, and a tree that reported a
// different one between two runs over identical input would not be
// reproducible.
func TestComposeGroupsPackagesDeterministically(t *testing.T) {
	t.Parallel()
	file := func(source string) relocate.File {
		return relocate.File{
			Source:        source + "/rule.go",
			Path:          "internal/kk/pkg/registry/rbac/validation/" + strings.ReplaceAll(source, "/", "_") + ".go",
			Package:       "internal/kk/pkg/registry/rbac/validation",
			SourcePackage: source,
			Mode:          relocate.ModeRegular,
			Contents:      []byte("package validation\n"),
		}
	}
	forward, err := relocate.FileSet{}.With(file("a/one"), file("b/two"))
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	reverse, err := relocate.FileSet{}.With(file("b/two"), file("a/one"))
	if err != nil {
		t.Fatalf("compose: %v", err)
	}

	if len(forward.Packages) != 1 || len(reverse.Packages) != 1 {
		t.Fatalf("composition produced %d and %d package records, want one each",
			len(forward.Packages), len(reverse.Packages))
	}
	if forward.Packages[0].Source != reverse.Packages[0].Source {
		t.Errorf("the merged record names %q one way round and %q the other",
			forward.Packages[0].Source, reverse.Packages[0].Source)
	}
	if got := forward.Packages[0].Source; got != "a/one" {
		t.Errorf("the merged record names %q, want the lexicographically smallest upstream package", got)
	}
	if len(forward.Packages[0].Files) != 2 {
		t.Errorf("the merged record holds %d files, want both", len(forward.Packages[0].Files))
	}
}

// TestComposedSetMaterializes proves the whole point of composing rather than
// writing generated files separately: one atomic write produces the complete
// module tree.
func TestComposedSetMaterializes(t *testing.T) {
	t.Parallel()
	composed, err := newRelocatedSet(t).With(
		generated("authorizer.go", "package rbacauthorizer\n"),
		relocate.File{Path: "LICENSE", Mode: relocate.ModeRegular, Contents: []byte("Apache License\n")},
	)
	if err != nil {
		t.Fatalf("compose: %v", err)
	}

	destination := filepath.Join(t.TempDir(), "module")
	if err := relocate.Materialize(t.Context(), destination, composed); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	for name, want := range map[string]string{
		"authorizer.go": "package rbacauthorizer\n",
		"LICENSE":       "Apache License\n",
		"internal/kk/plugin/pkg/auth/authorizer/rbac/rbac.go": "package rbac\n",
	} {
		contents, err := os.ReadFile(filepath.Join(destination, filepath.FromSlash(name)))
		if err != nil {
			t.Errorf("read %s: %v", name, err)
			continue
		}
		if string(contents) != want {
			t.Errorf("%s holds %q, want %q", name, contents, want)
		}
	}

	// Materialization never merges into an existing tree, so a second write of
	// the composed set is refused rather than producing a half updated module.
	if err := relocate.Materialize(t.Context(), destination, composed); !errors.Is(err, relocate.ErrDestinationExists) {
		t.Errorf("second materialize error is %v, want ErrDestinationExists", err)
	}
}
