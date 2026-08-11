package closure_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/closure"
)

func TestBuild_RBACShape(t *testing.T) {
	root := writeTree(t, rbacShape())
	result := build(t, rbacOptions(root))

	exact := result.Report.Exact
	assertStrings(t, "packages", exact.Packages, []string{
		testPrefix + "/pkg/apis/rbac/v1",
		testPrefix + "/pkg/validation",
		testPrefix + "/plugin/rbac",
	})
	assertStrings(t, "files", exact.Files, []string{
		"pkg/apis/rbac/v1/doc.go",
		"pkg/validation/rule.go",
		"plugin/rbac/rbac.go",
		"plugin/rbac/subject_locator.go",
	})
	assertStrings(t, "externalPackages", exact.ExternalPackages, []string{"k8s.io/api/rbac/v1"})
	assertStrings(t, "standardPackages", exact.StandardPackages, []string{"fmt"})
	assertStrings(t, "removedFiles", result.RemovedFiles, []string{
		"pkg/apis/rbac/v1/helpers.go",
		"pkg/validation/adapter.go",
	})

	// The pre-prune closure is the four package shape; the internal API package
	// leaves only because pruning removed its sole importers.
	if got, want := result.Report.Observed.PrePrune.Packages, 4; got != want {
		t.Errorf("pre-prune packages = %d, want %d", got, want)
	}
	if got, want := result.Report.Observed.PostPrune.Packages, 3; got != want {
		t.Errorf("post-prune packages = %d, want %d", got, want)
	}
	if got, want := result.Report.Observed.Growth.PackagesBeyondRoots, 2; got != want {
		t.Errorf("packages beyond roots = %d, want %d", got, want)
	}
	if got, want := result.Report.Observed.Growth.PackagesRemoved, 1; got != want {
		t.Errorf("packages removed = %d, want %d", got, want)
	}

	// A subpackage below the root and a sibling beside it are both excluded by
	// package granularity, and repository metadata is never copied.
	for _, unwanted := range []string{
		"plugin/rbac/bootstrappolicy/policy.go",
		"plugin/sibling/sibling.go",
		"plugin/rbac/OWNERS",
		"plugin/rbac/BUILD.bazel",
	} {
		if kind := planKind(result.CopyPlan, unwanted); kind != "" {
			t.Errorf("copy plan contains %s as %q, want it excluded", unwanted, kind)
		}
	}
}

// TestBuild_DenyIsExact proves the deny rule matches one import path and not its
// subpackages. The RBAC profile denies the unversioned internal API package
// while retaining its /v1 helper subpackage, so a prefix match here would
// silently delete a package the generated module needs.
func TestBuild_DenyIsExact(t *testing.T) {
	root := writeTree(t, rbacShape())
	result := build(t, rbacOptions(root))

	if !containsString(result.Report.Exact.Packages, testPrefix+"/pkg/apis/rbac/v1") {
		t.Fatalf("packages = %q, want the retained /v1 subpackage of the denied import", result.Report.Exact.Packages)
	}
}

func TestBuild_DeniedImportReentry(t *testing.T) {
	root := writeTree(t, rbacShape())
	opts := rbacOptions(root)
	// Leaving the adapter in place is exactly what a bad patch would do.
	opts.PruneFiles = []string{"pkg/apis/rbac/v1/helpers.go"}

	err := buildError(t, opts)
	if !errors.Is(err, closure.ErrImportDenied) {
		t.Fatalf("error = %v, want ErrImportDenied", err)
	}
	var importErr *closure.ImportError
	if !errors.As(err, &importErr) {
		t.Fatalf("error = %v, want *closure.ImportError", err)
	}
	if importErr.File != "pkg/validation/adapter.go" {
		t.Errorf("introducing file = %q, want pkg/validation/adapter.go", importErr.File)
	}
	if importErr.Import != testPrefix+"/pkg/apis/rbac" {
		t.Errorf("import = %q, want %s/pkg/apis/rbac", importErr.Import, testPrefix)
	}
}

// TestBuild_PlatformTaggedImports proves the closure is the portable union.
// A build constraint or a GOOS/GOARCH filename suffix hides a file from the
// machine running the engine, but the generated module still has to compile on
// that platform, so the import must be followed anyway.
func TestBuild_PlatformTaggedImports(t *testing.T) {
	root := writeTree(t, tree{
		"pkg/portable/portable.go":          source("portable"),
		"pkg/portable/plat_linux.go":        source("portable", `_ "`+testPrefix+`/pkg/onlylinux"`),
		"pkg/portable/plat_windows.go":      source("portable", `_ "`+testPrefix+`/pkg/onlywindows"`),
		"pkg/portable/plat_darwin_arm64.go": source("portable", `_ "`+testPrefix+`/pkg/onlydarwin"`),
		"pkg/portable/tagged.go":            "//go:build never_ever\n\n" + source("portable", `_ "`+testPrefix+`/pkg/onlytagged"`),
		// A leading underscore hides a file from every build, including ours.
		"pkg/portable/_scratch.go": source("portable", `_ "`+testPrefix+`/pkg/never"`),

		"pkg/onlylinux/a.go":   source("onlylinux"),
		"pkg/onlywindows/a.go": source("onlywindows"),
		"pkg/onlydarwin/a.go":  source("onlydarwin"),
		"pkg/onlytagged/a.go":  source("onlytagged"),
	})

	result := build(t, closure.Options{
		Root:         root,
		ImportPrefix: testPrefix,
		Roots:        []string{"pkg/portable"},
	})
	assertStrings(t, "packages", result.Report.Exact.Packages, []string{
		testPrefix + "/pkg/onlydarwin",
		testPrefix + "/pkg/onlylinux",
		testPrefix + "/pkg/onlytagged",
		testPrefix + "/pkg/onlywindows",
		testPrefix + "/pkg/portable",
	})
}

func TestBuild_ImportCycle(t *testing.T) {
	root := writeTree(t, tree{
		"pkg/a/a.go": source("a", `"`+testPrefix+`/pkg/b"`),
		"pkg/b/b.go": source("b", `"`+testPrefix+`/pkg/c"`),
		// c imports a, closing the cycle. A traversal that did not remember
		// visited packages would not terminate here.
		"pkg/c/c.go": source("c", `"`+testPrefix+`/pkg/a"`),
	})

	result := build(t, closure.Options{
		Root:         root,
		ImportPrefix: testPrefix,
		Roots:        []string{"pkg/a"},
	})
	assertStrings(t, "packages", result.Report.Exact.Packages, []string{
		testPrefix + "/pkg/a",
		testPrefix + "/pkg/b",
		testPrefix + "/pkg/c",
	})
}

// TestBuild_ImportAliases proves an alias, a blank import, and a dot import all
// pull their package into the closure. Each compiles the imported package, so
// treating any of them as decorative would drop a real dependency.
func TestBuild_ImportAliases(t *testing.T) {
	root := writeTree(t, tree{
		"pkg/root/root.go": source("root",
			`aliased "`+testPrefix+`/pkg/aliased"`,
			`_ "`+testPrefix+`/pkg/blank"`,
			`. "`+testPrefix+`/pkg/dot"`,
		),
		"pkg/aliased/a.go": source("aliased"),
		"pkg/blank/a.go":   source("blank"),
		"pkg/dot/a.go":     source("dot"),
	})

	result := build(t, closure.Options{
		Root:         root,
		ImportPrefix: testPrefix,
		Roots:        []string{"pkg/root"},
	})
	assertStrings(t, "packages", result.Report.Exact.Packages, []string{
		testPrefix + "/pkg/aliased",
		testPrefix + "/pkg/blank",
		testPrefix + "/pkg/dot",
		testPrefix + "/pkg/root",
	})
}

// TestBuild_NativeCompanions proves the copy plan carries the non-Go inputs a
// portable build needs. A relocated package that lost its assembly or its cgo
// header would fail to build only on the platform that uses it.
func TestBuild_NativeCompanions(t *testing.T) {
	root := writeTree(t, tree{
		"pkg/native/native.go":     "package native\n\n/*\n#include \"shim.h\"\n*/\nimport \"C\"\n",
		"pkg/native/shim.h":        "void shim(void);\n",
		"pkg/native/shim.c":        "#include \"shim.h\"\nvoid shim(void) {}\n",
		"pkg/native/asm_amd64.s":   "TEXT ·zero(SB),0,$0\n\tRET\n",
		"pkg/native/asm_arm64.S":   "// preprocessed assembly\n",
		"pkg/native/prebuilt.syso": "\x00\x00",
		"pkg/native/OWNERS":        "approvers: []\n",
		"pkg/native/README.md":     "# native\n",
		"pkg/native/_ignored.c":    "int ignored;\n",
	})

	result := build(t, closure.Options{
		Root:         root,
		ImportPrefix: testPrefix,
		Roots:        []string{"pkg/native"},
	})
	assertStrings(t, "files", result.Report.Exact.Files, []string{
		"pkg/native/asm_amd64.s",
		"pkg/native/asm_arm64.S",
		"pkg/native/native.go",
		"pkg/native/prebuilt.syso",
		"pkg/native/shim.c",
		"pkg/native/shim.h",
	})

	kinds := map[string]closure.CopyKind{
		"pkg/native/native.go":     closure.KindGo,
		"pkg/native/shim.c":        closure.KindNative,
		"pkg/native/shim.h":        closure.KindHeader,
		"pkg/native/asm_amd64.s":   closure.KindAssembly,
		"pkg/native/asm_arm64.S":   closure.KindAssembly,
		"pkg/native/prebuilt.syso": closure.KindObject,
	}
	for path, want := range kinds {
		if got := planKind(result.CopyPlan, path); got != want {
			t.Errorf("kind of %s = %q, want %q", path, got, want)
		}
	}
}

func TestBuild_Embeds(t *testing.T) {
	root := writeTree(t, tree{
		"pkg/embeds/embeds.go": "package embeds\n\nimport \"embed\"\n\n" +
			"//go:embed data.txt\nvar single string\n\n" +
			"//go:embed \"quoted name.txt\" `raw.txt`\nvar many embed.FS\n\n" +
			"//go:embed tmpl/*.tmpl\nvar globbed embed.FS\n\n" +
			"//go:embed assets\nvar dir embed.FS\n\n" +
			"//go:embed all:hidden\nvar hiddenDir embed.FS\n",
		"pkg/embeds/data.txt":        "data\n",
		"pkg/embeds/quoted name.txt": "quoted\n",
		"pkg/embeds/raw.txt":         "raw\n",
		"pkg/embeds/tmpl/a.tmpl":     "a\n",
		"pkg/embeds/tmpl/b.tmpl":     "b\n",
		"pkg/embeds/tmpl/notes.md":   "not a template\n",
		"pkg/embeds/assets/nested/x": "x\n",
		"pkg/embeds/assets/.hidden":  "skipped without all:\n",
		"pkg/embeds/hidden/.dotfile": "kept by all:\n",
		"pkg/embeds/hidden/plain":    "kept\n",
		// A comment that merely mentions the directive, and one attached to a
		// function rather than a var, must not demand files.
		"pkg/embeds/decoy.go": "package embeds\n\n// This file explains //go:embed nothing.txt usage.\n\n//go:embed also-nothing.txt\nfunc decoy() {}\n",
	})

	result := build(t, closure.Options{
		Root:         root,
		ImportPrefix: testPrefix,
		Roots:        []string{"pkg/embeds"},
	})
	assertStrings(t, "files", result.Report.Exact.Files, []string{
		"pkg/embeds/assets/nested/x",
		"pkg/embeds/data.txt",
		"pkg/embeds/decoy.go",
		"pkg/embeds/embeds.go",
		"pkg/embeds/hidden/.dotfile",
		"pkg/embeds/hidden/plain",
		"pkg/embeds/quoted name.txt",
		"pkg/embeds/raw.txt",
		"pkg/embeds/tmpl/a.tmpl",
		"pkg/embeds/tmpl/b.tmpl",
	})
	if got := planKind(result.CopyPlan, "pkg/embeds/data.txt"); got != closure.KindEmbed {
		t.Errorf("kind of data.txt = %q, want %q", got, closure.KindEmbed)
	}
}

func TestBuild_EmbedFailures(t *testing.T) {
	tests := []struct {
		name      string
		directive string
		want      error
	}{
		{
			name:      "no match fails closed",
			directive: "//go:embed absent.txt\nvar v string\n",
			want:      closure.ErrPatternNoMatch,
		},
		{
			name:      "recursive pattern is refused",
			directive: "//go:embed data/**/x.txt\nvar v string\n",
			want:      closure.ErrRecursivePattern,
		},
		{
			name:      "escaping pattern is refused",
			directive: "//go:embed ../outside.txt\nvar v string\n",
			want:      closure.ErrPatternMalformed,
		},
		{
			name:      "unterminated quote is refused",
			directive: "//go:embed \"unterminated\nvar v string\n",
			want:      closure.ErrPatternMalformed,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := writeTree(t, tree{
				"outside.txt":           "outside\n",
				"pkg/embeds/data/x.txt": "x\n",
				"pkg/embeds/embeds.go":  "package embeds\n\nimport \"embed\"\n\nvar _ embed.FS\n\n" + test.directive,
			})
			err := buildError(t, closure.Options{
				Root:         root,
				ImportPrefix: testPrefix,
				Roots:        []string{"pkg/embeds"},
			})
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
			var fileErr *closure.FileError
			if !errors.As(err, &fileErr) {
				t.Fatalf("error = %v, want *closure.FileError naming the directive's file", err)
			}
			if fileErr.File != "pkg/embeds/embeds.go" {
				t.Errorf("file = %q, want pkg/embeds/embeds.go", fileErr.File)
			}
		})
	}
}

// TestBuild_EmbedHiddenNames proves the hidden name rule applies where the go
// tool applies it and nowhere else.
//
// The go tool documents the difference exactly: "image/*" embeds image/.tempfile
// while "image" does not. Only walking a matched directory skips names beginning
// with a dot or an underscore, and all: turns that off. What the pattern itself
// matched is always honoured, so filtering it would drop files a package really
// does embed and the generated module would fail to compile.
func TestBuild_EmbedHiddenNames(t *testing.T) {
	root := writeTree(t, tree{
		"pkg/app/app.go": "package app\n\nimport \"embed\"\n\n" +
			"//go:embed glob/*.txt\nvar globbed embed.FS\n\n" +
			"//go:embed tree\nvar walked embed.FS\n",
		"pkg/app/glob/.dotfile.txt": "matched outright\n",
		"pkg/app/glob/_under.txt":   "matched outright\n",
		"pkg/app/glob/plain.txt":    "matched outright\n",
		"pkg/app/tree/.dotfile.txt": "skipped by the walk\n",
		"pkg/app/tree/_under.txt":   "skipped by the walk\n",
		"pkg/app/tree/plain.txt":    "kept by the walk\n",
	})

	result := build(t, closure.Options{
		Root: root, ImportPrefix: testPrefix, Roots: []string{"pkg/app"},
	})
	assertStrings(t, "files", result.Report.Exact.Files, []string{
		"pkg/app/app.go",
		"pkg/app/glob/.dotfile.txt",
		"pkg/app/glob/_under.txt",
		"pkg/app/glob/plain.txt",
		"pkg/app/tree/plain.txt",
	})
}

// TestBuild_EmbedModuleBoundaries proves embedding stops where the source module
// stops.
//
// A directory holding its own go.mod is a different module. Copying its files
// into the generated module would nest one module inside another, where the
// nested files stop being built at all, so the go tool refuses to embed across
// the boundary and the closure has to refuse in the same two places: it skips a
// nested module found while walking a directory, and it fails when a pattern
// reaches into one directly.
func TestBuild_EmbedModuleBoundaries(t *testing.T) {
	t.Run("directory walk stops at the boundary", func(t *testing.T) {
		root := writeTree(t, tree{
			"pkg/app/app.go": "package app\n\nimport \"embed\"\n\n" +
				"//go:embed assets\nvar assets embed.FS\n",
			"pkg/app/assets/keep.txt":          "kept\n",
			"pkg/app/assets/inner/go.mod":      "module example.com/inner\n\ngo 1.26.0\n",
			"pkg/app/assets/inner/skipped.txt": "belongs to another module\n",
		})
		result := build(t, closure.Options{
			Root: root, ImportPrefix: testPrefix, Roots: []string{"pkg/app"},
		})
		assertStrings(t, "files", result.Report.Exact.Files, []string{
			"pkg/app/app.go",
			"pkg/app/assets/keep.txt",
		})
	})

	t.Run("pattern reaching into a nested module fails closed", func(t *testing.T) {
		root := writeTree(t, tree{
			"pkg/app/app.go": "package app\n\nimport \"embed\"\n\n" +
				"//go:embed inner/data.txt\nvar data string\n",
			"pkg/app/inner/go.mod":   "module example.com/inner\n\ngo 1.26.0\n",
			"pkg/app/inner/data.txt": "belongs to another module\n",
		})
		err := buildError(t, closure.Options{
			Root: root, ImportPrefix: testPrefix, Roots: []string{"pkg/app"},
		})
		if !errors.Is(err, closure.ErrEmbedNestedModule) {
			t.Fatalf("error = %v, want ErrEmbedNestedModule", err)
		}
	})
}

// TestBuild_EmbedVersionControlNames proves a version control directory is never
// carried, which is the guard that keeps honouring hidden matches from turning a
// pattern like data/* into a copy of a repository's internals. The go tool
// treats these names as not existing for embedding, so a walk skips one and a
// pattern that names one outright fails.
func TestBuild_EmbedVersionControlNames(t *testing.T) {
	files := func(directive string) tree {
		return tree{
			"pkg/app/app.go": "package app\n\nimport \"embed\"\n\n" +
				directive + "\nvar data embed.FS\n",
			"pkg/app/data/keep.txt":    "kept\n",
			"pkg/app/data/.git/HEAD":   "ref: refs/heads/main\n",
			"pkg/app/data/.git/config": "[core]\n",
		}
	}

	t.Run("walk skips it even under all:", func(t *testing.T) {
		root := writeTree(t, files("//go:embed all:data"))
		result := build(t, closure.Options{
			Root: root, ImportPrefix: testPrefix, Roots: []string{"pkg/app"},
		})
		assertStrings(t, "files", result.Report.Exact.Files, []string{
			"pkg/app/app.go",
			"pkg/app/data/keep.txt",
		})
	})

	t.Run("pattern matching it fails closed", func(t *testing.T) {
		root := writeTree(t, files("//go:embed data/*"))
		err := buildError(t, closure.Options{
			Root: root, ImportPrefix: testPrefix, Roots: []string{"pkg/app"},
		})
		if !errors.Is(err, closure.ErrEmbedBadName) {
			t.Fatalf("error = %v, want ErrEmbedBadName", err)
		}
	})
}

func TestBuild_Assets(t *testing.T) {
	files := tree{
		"pkg/app/app.go":              source("app"),
		"pkg/app/data/policy.yaml":    "policy\n",
		"pkg/app/data/schema.yaml":    "schema\n",
		"pkg/app/data/notes.txt":      "notes\n",
		"pkg/app/data/deep/more.yaml": "deep\n",
	}

	t.Run("glob selects files in one directory level", func(t *testing.T) {
		root := writeTree(t, files)
		result := build(t, closure.Options{
			Root:         root,
			ImportPrefix: testPrefix,
			Roots:        []string{"pkg/app"},
			AssetGlobs:   []string{"pkg/app/data/*.yaml"},
		})
		// A star never crosses a slash, so the nested file is not selected and
		// the directory itself is not expanded.
		assertStrings(t, "files", result.Report.Exact.Files, []string{
			"pkg/app/app.go",
			"pkg/app/data/policy.yaml",
			"pkg/app/data/schema.yaml",
		})
		if got := planKind(result.CopyPlan, "pkg/app/data/policy.yaml"); got != closure.KindAsset {
			t.Errorf("kind = %q, want %q", got, closure.KindAsset)
		}
	})

	t.Run("deeper level needs its own glob", func(t *testing.T) {
		root := writeTree(t, files)
		result := build(t, closure.Options{
			Root:         root,
			ImportPrefix: testPrefix,
			Roots:        []string{"pkg/app"},
			AssetGlobs:   []string{"pkg/app/data/*.yaml", "pkg/app/data/*/*.yaml"},
		})
		assertStrings(t, "files", result.Report.Exact.Files, []string{
			"pkg/app/app.go",
			"pkg/app/data/deep/more.yaml",
			"pkg/app/data/policy.yaml",
			"pkg/app/data/schema.yaml",
		})
	})

	t.Run("recursive syntax is refused rather than reinterpreted", func(t *testing.T) {
		root := writeTree(t, files)
		err := buildError(t, closure.Options{
			Root:         root,
			ImportPrefix: testPrefix,
			Roots:        []string{"pkg/app"},
			AssetGlobs:   []string{"pkg/app/**/*.yaml"},
		})
		if !errors.Is(err, closure.ErrRecursivePattern) {
			t.Fatalf("error = %v, want ErrRecursivePattern", err)
		}
	})

	t.Run("glob matching nothing fails closed", func(t *testing.T) {
		root := writeTree(t, files)
		err := buildError(t, closure.Options{
			Root:         root,
			ImportPrefix: testPrefix,
			Roots:        []string{"pkg/app"},
			AssetGlobs:   []string{"pkg/app/data/*.json"},
		})
		if !errors.Is(err, closure.ErrPatternNoMatch) {
			t.Fatalf("error = %v, want ErrPatternNoMatch", err)
		}
	})
}

func TestBuild_RecursiveRoots(t *testing.T) {
	files := tree{
		"pkg/app/app.go":               source("app"),
		"pkg/app/sub/sub.go":           source("sub"),
		"pkg/app/sub/deeper/deeper.go": source("deeper"),
		"pkg/app/testdata/fixture.go":  "package fixture\n\nimport \"broken\n",
		"pkg/app/vendor/v/v.go":        source("v"),
		"pkg/app/docs/README.md":       "# docs\n",
	}

	t.Run("off excludes unimported subpackages", func(t *testing.T) {
		root := writeTree(t, files)
		result := build(t, closure.Options{
			Root:         root,
			ImportPrefix: testPrefix,
			Roots:        []string{"pkg/app"},
		})
		assertStrings(t, "packages", result.Report.Exact.Packages, []string{testPrefix + "/pkg/app"})
	})

	t.Run("on includes every subdirectory holding Go files", func(t *testing.T) {
		root := writeTree(t, files)
		result := build(t, closure.Options{
			Root:         root,
			ImportPrefix: testPrefix,
			Roots:        []string{"pkg/app"},
			Recursive:    true,
		})
		// testdata and vendor are skipped exactly as the go tool skips them,
		// which is also why the deliberately unparsable fixture is harmless.
		assertStrings(t, "packages", result.Report.Exact.Packages, []string{
			testPrefix + "/pkg/app",
			testPrefix + "/pkg/app/sub",
			testPrefix + "/pkg/app/sub/deeper",
		})
		if got, want := result.Report.Observed.Growth.RootPackages, 3; got != want {
			t.Errorf("root packages = %d, want %d", got, want)
		}
		if got, want := result.Report.Observed.Growth.PackagesBeyondRoots, 0; got != want {
			t.Errorf("packages beyond roots = %d, want %d", got, want)
		}
	})
}

// TestBuild_RecursionDoesNotFollowDiscoveredPackages proves recursion applies to
// configured roots only. An import names one package; letting it drag in that
// package's subtree would quietly copy code nothing references.
func TestBuild_RecursionDoesNotFollowDiscoveredPackages(t *testing.T) {
	root := writeTree(t, tree{
		"pkg/app/app.go":         source("app", `"`+testPrefix+`/pkg/lib"`),
		"pkg/lib/lib.go":         source("lib"),
		"pkg/lib/extra/extra.go": source("extra"),
	})

	result := build(t, closure.Options{
		Root:         root,
		ImportPrefix: testPrefix,
		Roots:        []string{"pkg/app"},
		Recursive:    true,
	})
	assertStrings(t, "packages", result.Report.Exact.Packages, []string{
		testPrefix + "/pkg/app",
		testPrefix + "/pkg/lib",
	})
}

func TestBuild_IncludeTests(t *testing.T) {
	files := tree{
		"pkg/app/app.go":         source("app"),
		"pkg/app/app_test.go":    source("app", `"`+testPrefix+`/pkg/testonly"`),
		"pkg/app/ext_test.go":    source("app_test", `"`+testPrefix+`/pkg/testonly"`),
		"pkg/testonly/helper.go": source("testonly"),
	}

	t.Run("off leaves test-only dependencies out", func(t *testing.T) {
		root := writeTree(t, files)
		result := build(t, closure.Options{
			Root:         root,
			ImportPrefix: testPrefix,
			Roots:        []string{"pkg/app"},
		})
		assertStrings(t, "packages", result.Report.Exact.Packages, []string{testPrefix + "/pkg/app"})
		assertStrings(t, "files", result.Report.Exact.Files, []string{"pkg/app/app.go"})
	})

	t.Run("on pulls them in and accepts the external test package", func(t *testing.T) {
		root := writeTree(t, files)
		result := build(t, closure.Options{
			Root:         root,
			ImportPrefix: testPrefix,
			Roots:        []string{"pkg/app"},
			IncludeTests: true,
		})
		assertStrings(t, "packages", result.Report.Exact.Packages, []string{
			testPrefix + "/pkg/app",
			testPrefix + "/pkg/testonly",
		})
		if got, want := len(result.Packages[0].TestFiles), 2; got != want {
			t.Errorf("test files = %d, want %d", got, want)
		}
	})
}

func TestBuild_Symlinks(t *testing.T) {
	tests := []struct {
		name    string
		link    string
		target  string
		options func(root string) closure.Options
	}{
		{
			name:   "escaping link in a package directory",
			link:   "pkg/app/escape.go",
			target: "../../../../etc/passwd",
		},
		{
			name:   "link that stays inside the tree",
			link:   "pkg/app/alias.go",
			target: "app.go",
		},
		{
			name:   "linked directory under a recursive root",
			link:   "pkg/app/mirror",
			target: "../other",
			options: func(root string) closure.Options {
				return closure.Options{
					Root:         root,
					ImportPrefix: testPrefix,
					Roots:        []string{"pkg/app"},
					Recursive:    true,
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := writeTree(t, tree{
				"pkg/app/app.go":     source("app"),
				"pkg/other/other.go": source("other"),
			})
			symlink(t, root, test.link, test.target)

			opts := closure.Options{Root: root, ImportPrefix: testPrefix, Roots: []string{"pkg/app"}}
			if test.options != nil {
				opts = test.options(root)
			}
			err := buildError(t, opts)
			if !errors.Is(err, closure.ErrUnsafeSymlink) {
				t.Fatalf("error = %v, want ErrUnsafeSymlink", err)
			}
		})
	}
}

// TestBuild_SymlinkedPathComponent proves a link is refused wherever it sits in
// a package's path, not only at the end of it.
//
// An lstat answers about the last element alone: by the time the kernel reports
// on pkg/app, it has already resolved pkg. A link there stays inside the tree,
// so containment never notices it, but it makes one directory reachable under
// two names, and a closure that followed it would copy the same package twice
// under two import paths, each declaring the same Go package. That module cannot
// be built, so every element of a package root and of a followed import path is
// checked.
func TestBuild_SymlinkedPathComponent(t *testing.T) {
	tests := []struct {
		name    string
		link    string
		imports []string
		roots   []string
	}{
		{
			// The root is configured through the link, so the same directory is
			// the package at real/app and the package at pkg/app.
			name:    "in a configured package root",
			link:    "pkg",
			imports: []string{`"` + testPrefix + `/real/lib"`},
			roots:   []string{"pkg/app"},
		},
		{
			// The root imports one library twice, once by its real path and once
			// through the link. Following the link would put the same directory in
			// the closure under two import paths, both declaring package lib.
			name:    "in a followed import path",
			link:    "alias",
			imports: []string{`"` + testPrefix + `/real/lib"`, `alias "` + testPrefix + `/alias/lib"`},
			roots:   []string{"real/app"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := writeTree(t, tree{
				"real/app/app.go": source("app", test.imports...),
				"real/lib/lib.go": source("lib"),
			})
			symlink(t, root, test.link, "real")

			err := buildError(t, closure.Options{
				Root: root, ImportPrefix: testPrefix, Roots: test.roots,
			})
			if !errors.Is(err, closure.ErrUnsafeSymlink) {
				t.Fatalf("error = %v, want ErrUnsafeSymlink", err)
			}
			var fileErr *closure.FileError
			if !errors.As(err, &fileErr) {
				t.Fatalf("error = %v, want *closure.FileError", err)
			}
			if fileErr.File != test.link {
				t.Errorf("file = %q, want the linked element %q", fileErr.File, test.link)
			}
		})
	}
}

// TestBuild_RootEscapeIsContained proves os.Root containment holds even when the
// worktree root itself is reached through a link, which is how a temporary
// directory on macOS presents itself.
func TestBuild_RootEscapeIsContained(t *testing.T) {
	root := writeTree(t, tree{
		"pkg/app/app.go": source("app", `"`+testPrefix+`/pkg/lib"`),
		"pkg/lib/lib.go": source("lib"),
	})
	result := build(t, closure.Options{
		Root:         root,
		ImportPrefix: testPrefix,
		Roots:        []string{"pkg/app"},
	})
	for _, file := range result.Report.Exact.Files {
		if len(file) > 0 && (file[0] == '/' || file[0] == '.') {
			t.Errorf("file %q is not repository relative", file)
		}
	}
}

func TestBuild_MissingRoot(t *testing.T) {
	tests := []struct {
		name string
		root string
		want error
	}{
		{name: "absent directory", root: "pkg/absent", want: closure.ErrRootMissing},
		{name: "path names a file", root: "pkg/app/app.go", want: closure.ErrRootMissing},
		{name: "directory without Go files", root: "pkg/docs", want: closure.ErrPackageMissing},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := writeTree(t, tree{
				"pkg/app/app.go":     source("app"),
				"pkg/docs/README.md": "# docs\n",
			})
			err := buildError(t, closure.Options{
				Root:         root,
				ImportPrefix: testPrefix,
				Roots:        []string{test.root},
			})
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}

// TestBuild_MissingImportedPackage proves a dangling internal import is reported
// against the file that wrote it, because that is the edge an operator can cut.
func TestBuild_MissingImportedPackage(t *testing.T) {
	root := writeTree(t, tree{
		"pkg/app/app.go": source("app", `"`+testPrefix+`/pkg/absent"`),
	})
	err := buildError(t, closure.Options{
		Root:         root,
		ImportPrefix: testPrefix,
		Roots:        []string{"pkg/app"},
	})
	if !errors.Is(err, closure.ErrPackageMissing) {
		t.Fatalf("error = %v, want ErrPackageMissing", err)
	}
	var importErr *closure.ImportError
	if !errors.As(err, &importErr) {
		t.Fatalf("error = %v, want *closure.ImportError", err)
	}
	if importErr.File != "pkg/app/app.go" || importErr.Import != testPrefix+"/pkg/absent" {
		t.Errorf("error located at %q importing %q, want pkg/app/app.go importing %s/pkg/absent",
			importErr.File, importErr.Import, testPrefix)
	}
}

func TestBuild_MalformedPackage(t *testing.T) {
	t.Run("unparsable file", func(t *testing.T) {
		root := writeTree(t, tree{"pkg/app/app.go": "package app\n\nimport \"unterminated\n"})
		err := buildError(t, closure.Options{
			Root:         root,
			ImportPrefix: testPrefix,
			Roots:        []string{"pkg/app"},
		})
		if !errors.Is(err, closure.ErrPackageMalformed) {
			t.Fatalf("error = %v, want ErrPackageMalformed", err)
		}
		var fileErr *closure.FileError
		if !errors.As(err, &fileErr) || fileErr.File != "pkg/app/app.go" {
			t.Fatalf("error = %v, want *closure.FileError for pkg/app/app.go", err)
		}
	})

	t.Run("disagreeing package names", func(t *testing.T) {
		root := writeTree(t, tree{
			"pkg/app/app.go": source("app"),
			"pkg/app/gen.go": "//go:build ignore\n\n" + source("main"),
		})
		err := buildError(t, closure.Options{
			Root:         root,
			ImportPrefix: testPrefix,
			Roots:        []string{"pkg/app"},
		})
		if !errors.Is(err, closure.ErrPackageMalformed) {
			t.Fatalf("error = %v, want ErrPackageMalformed", err)
		}
		var pkgErr *closure.PackageError
		if !errors.As(err, &pkgErr) || pkgErr.Dir != "pkg/app" {
			t.Fatalf("error = %v, want *closure.PackageError for pkg/app", err)
		}
	})

	// The file that disagrees is the file to prune, so the error has to name it
	// however the directory happens to sort. Taking the first file as the
	// authority would make a stray generator that sorts early rename the package
	// and would then report the disagreement against an ordinary file that is
	// doing nothing wrong.
	t.Run("the majority names the package and every outlier is reported", func(t *testing.T) {
		root := writeTree(t, tree{
			"pkg/app/aaa_gen.go":    "//go:build ignore\n\n" + source("main"),
			"pkg/app/bbb.go":        source("app"),
			"pkg/app/ccc.go":        source("app"),
			"pkg/app/zzz_helper.go": "//go:build ignore\n\n" + source("helper"),
		})
		err := buildError(t, closure.Options{
			Root:         root,
			ImportPrefix: testPrefix,
			Roots:        []string{"pkg/app"},
		})
		if !errors.Is(err, closure.ErrPackageMalformed) {
			t.Fatalf("error = %v, want ErrPackageMalformed", err)
		}
		message := err.Error()
		for _, want := range []string{`the package is "app"`, "pkg/app/aaa_gen.go", "pkg/app/zzz_helper.go"} {
			if !strings.Contains(message, want) {
				t.Errorf("error %q does not mention %q", message, want)
			}
		}
		for _, innocent := range []string{"pkg/app/bbb.go", "pkg/app/ccc.go"} {
			if strings.Contains(message, innocent) {
				t.Errorf("error %q blames %s, which agrees with the majority", message, innocent)
			}
		}
	})

	// A stray generator declaring package main is exactly what pruning exists
	// for, so the pre-prune pass must tolerate it long enough to remove it.
	t.Run("pruning resolves the disagreement", func(t *testing.T) {
		root := writeTree(t, tree{
			"pkg/app/app.go": source("app"),
			"pkg/app/gen.go": "//go:build ignore\n\n" + source("main"),
		})
		result := build(t, closure.Options{
			Root:         root,
			ImportPrefix: testPrefix,
			Roots:        []string{"pkg/app"},
			PruneFiles:   []string{"pkg/app/gen.go"},
		})
		assertStrings(t, "files", result.Report.Exact.Files, []string{"pkg/app/app.go"})
	})

	// Pruning is also the remedy for a file upstream left unparsable, and the
	// remedy has to survive the measurement that runs before it. The tolerance is
	// scoped to the exact entry the profile names: any other malformed file still
	// fails, and it fails in the pass that can still say which file it was.
	t.Run("pruning a malformed file", func(t *testing.T) {
		root := writeTree(t, tree{
			"pkg/app/app.go":    source("app"),
			"pkg/app/broken.go": "package app\n\nfunc (\n",
		})
		result := build(t, closure.Options{
			Root:         root,
			ImportPrefix: testPrefix,
			Roots:        []string{"pkg/app"},
			PruneFiles:   []string{"pkg/app/broken.go"},
		})
		assertStrings(t, "files", result.Report.Exact.Files, []string{"pkg/app/app.go"})
		// The measurement still counted the file it could not parse, because the
		// three lines it removed are three lines upstream really had.
		if got, want := result.Report.Observed.Growth.NonTestLinesRemoved, 3; got != want {
			t.Errorf("non-test lines removed = %d, want %d", got, want)
		}
	})

	t.Run("a malformed file that is not pruned still fails", func(t *testing.T) {
		root := writeTree(t, tree{
			"pkg/app/app.go":     source("app"),
			"pkg/app/broken.go":  "package app\n\nfunc (\n",
			"pkg/app/removed.go": source("app"),
		})
		err := buildError(t, closure.Options{
			Root:         root,
			ImportPrefix: testPrefix,
			Roots:        []string{"pkg/app"},
			PruneFiles:   []string{"pkg/app/removed.go"},
		})
		if !errors.Is(err, closure.ErrPackageMalformed) {
			t.Fatalf("error = %v, want ErrPackageMalformed", err)
		}
		var fileErr *closure.FileError
		if !errors.As(err, &fileErr) || fileErr.File != "pkg/app/broken.go" {
			t.Fatalf("error = %v, want *closure.FileError for pkg/app/broken.go", err)
		}
	})
}

func TestBuild_Limits(t *testing.T) {
	files := tree{
		"pkg/app/app.go":   source("app", `"`+testPrefix+`/pkg/lib"`),
		"pkg/lib/lib.go":   source("lib", `"`+testPrefix+`/pkg/deep"`),
		"pkg/deep/deep.go": source("deep"),
	}
	tests := []struct {
		name   string
		limits closure.Limits
		want   string
	}{
		{name: "packages", limits: closure.Limits{MaxPackages: 2}, want: "limits.maxPackages"},
		{name: "files", limits: closure.Limits{MaxFiles: 2}, want: "limits.maxFiles"},
		{name: "non-test lines", limits: closure.Limits{MaxNonTestLines: 5}, want: "limits.maxNonTestLines"},
		{name: "package growth", limits: closure.Limits{MaxPackageGrowth: 1}, want: "limits.maxPackageGrowth"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := writeTree(t, files)
			err := buildError(t, closure.Options{
				Root:         root,
				ImportPrefix: testPrefix,
				Roots:        []string{"pkg/app"},
				Limits:       test.limits,
			})
			if !errors.Is(err, closure.ErrLimitExceeded) {
				t.Fatalf("error = %v, want ErrLimitExceeded", err)
			}
			var limitErr *closure.LimitError
			if !errors.As(err, &limitErr) {
				t.Fatalf("error = %v, want *closure.LimitError", err)
			}
			if limitErr.Name != test.want {
				t.Errorf("limit = %q, want %q", limitErr.Name, test.want)
			}
		})
	}

	t.Run("generous limits pass", func(t *testing.T) {
		root := writeTree(t, files)
		build(t, closure.Options{
			Root:         root,
			ImportPrefix: testPrefix,
			Roots:        []string{"pkg/app"},
			Limits: closure.Limits{
				MaxPackages: 8, MaxFiles: 40, MaxNonTestLines: 5000, MaxPackageGrowth: 2,
			},
		})
	})
}

func TestNew_InvalidOptions(t *testing.T) {
	tests := []struct {
		name string
		opts closure.Options
		want string
	}{
		{
			name: "missing root",
			opts: closure.Options{ImportPrefix: testPrefix, Roots: []string{"pkg/app"}},
			want: "materialized worktree path is required",
		},
		{
			name: "missing import prefix",
			opts: closure.Options{Root: "/tmp", Roots: []string{"pkg/app"}},
			want: "import path must not be empty",
		},
		{
			name: "no roots",
			opts: closure.Options{Root: "/tmp", ImportPrefix: testPrefix},
			want: "at least one package root is required",
		},
		{
			name: "absolute root",
			opts: closure.Options{Root: "/tmp", ImportPrefix: testPrefix, Roots: []string{"/pkg/app"}},
			want: "path must be relative",
		},
		{
			name: "traversing root",
			opts: closure.Options{Root: "/tmp", ImportPrefix: testPrefix, Roots: []string{"../escape"}},
			want: "must not traverse parent directories",
		},
		{
			name: "duplicate prune entry",
			opts: closure.Options{
				Root: "/tmp", ImportPrefix: testPrefix, Roots: []string{"pkg/app"},
				PruneFiles: []string{"pkg/app/a.go", "pkg/app/a.go"},
			},
			want: "duplicate entry",
		},
		{
			name: "prune and required conflict",
			opts: closure.Options{
				Root: "/tmp", ImportPrefix: testPrefix, Roots: []string{"pkg/app"},
				PruneFiles:    []string{"pkg/app/a.go"},
				RequiredFiles: []string{"pkg/app/a.go"},
			},
			want: "also a required file",
		},
		{
			name: "recursive asset glob",
			opts: closure.Options{
				Root: "/tmp", ImportPrefix: testPrefix, Roots: []string{"pkg/app"},
				AssetGlobs: []string{"pkg/**/x.yaml"},
			},
			want: "recursive ** pattern",
		},
		{
			name: "negative limit",
			opts: closure.Options{
				Root: "/tmp", ImportPrefix: testPrefix, Roots: []string{"pkg/app"},
				Limits: closure.Limits{MaxPackages: -1},
			},
			want: "must not be negative",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := closure.New(context.Background(), test.opts)
			if err == nil {
				t.Fatalf("New succeeded, want failure")
			}
			var optErr *closure.OptionsError
			if !errors.As(err, &optErr) {
				t.Fatalf("error = %v, want *closure.OptionsError", err)
			}
			if !containsSubstring(optErr.Problems, test.want) {
				t.Errorf("problems = %q, want one containing %q", optErr.Problems, test.want)
			}
		})
	}
}

// TestNew_OptionsAreCopied proves a Builder cannot be reconfigured by mutating
// the slices it was constructed with, which matters because the pipeline holds
// one Builder across several patch passes.
func TestNew_OptionsAreCopied(t *testing.T) {
	root := writeTree(t, rbacShape())
	opts := rbacOptions(root)
	builder, err := closure.New(context.Background(), opts)
	if err != nil {
		t.Fatalf("new builder: %v", err)
	}

	opts.PruneFiles[0] = "pkg/apis/rbac/types.go"
	opts.DeniedImports[0] = "fmt"

	got := builder.Options()
	assertStrings(t, "prune files", got.PruneFiles, []string{
		"pkg/apis/rbac/v1/helpers.go",
		"pkg/validation/adapter.go",
	})
	assertStrings(t, "denied imports", got.DeniedImports, []string{testPrefix + "/pkg/apis/rbac"})
}

func TestBuild_ContextCancelled(t *testing.T) {
	root := writeTree(t, rbacShape())
	builder, err := closure.New(context.Background(), rbacOptions(root))
	if err != nil {
		t.Fatalf("new builder: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := builder.Build(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

// TestBuild_EmbedInPrunedPackageIsTolerated proves the pre-prune measurement
// cannot veto the contract.
//
// A go:embed directive in a package that pruning is about to remove is resolved
// only while measuring what upstream contained. Failing the run there would be
// unfixable: the profile's remedy is pruning, and pruning has not happened yet
// when the measurement runs.
func TestBuild_EmbedInPrunedPackageIsTolerated(t *testing.T) {
	root := writeTree(t, tree{
		"pkg/app/app.go":   source("app", `"`+testPrefix+`/pkg/lib"`),
		"pkg/app/extra.go": source("app", `"`+testPrefix+`/pkg/gone"`),
		"pkg/lib/lib.go":   source("lib"),
		"pkg/gone/gone.go": "package gone\n\nimport \"embed\"\n\n//go:embed absent.txt\nvar missing embed.FS\n",
	})

	result := build(t, closure.Options{
		Root: root, ImportPrefix: testPrefix, Roots: []string{"pkg/app"},
		PruneFiles: []string{"pkg/app/extra.go"},
	})
	assertStrings(t, "packages", result.Report.Exact.Packages, []string{
		testPrefix + "/pkg/app",
		testPrefix + "/pkg/lib",
	})

	// The same directive in a package that survives still fails, because that
	// package's files really are copied.
	err := buildError(t, closure.Options{
		Root: root, ImportPrefix: testPrefix, Roots: []string{"pkg/gone"},
	})
	if !errors.Is(err, closure.ErrPatternNoMatch) {
		t.Fatalf("error = %v, want ErrPatternNoMatch", err)
	}
}

// TestBuild_TolerantPassStillRefusesSymlinks proves the pre-prune measurement
// absorbs a failure in content and nothing else.
//
// Tolerating an unresolvable pattern is safe: it describes content, content is
// what pruning changes, and every package that survives is resolved again by the
// post-prune pass. A symbolic link describes the tree the engine was handed. No
// prune entry makes it acceptable, and absorbing it would make the one pass that
// touched the path the one pass that stayed quiet about it.
func TestBuild_TolerantPassStillRefusesSymlinks(t *testing.T) {
	root := writeTree(t, tree{
		"pkg/app/app.go":   source("app", `"`+testPrefix+`/pkg/lib"`),
		"pkg/app/extra.go": source("app", `"`+testPrefix+`/pkg/gone"`),
		"pkg/lib/lib.go":   source("lib"),
		// The link sits in a subdirectory, where only embed resolution looks. A
		// link among the package's own files never reaches the copy plan, because
		// scanning the directory refuses it first.
		"pkg/gone/gone.go":       "package gone\n\nimport \"embed\"\n\n//go:embed data/linked.txt\nvar linked embed.FS\n",
		"pkg/gone/data/real.txt": "real\n",
	})
	symlink(t, root, "pkg/gone/data/linked.txt", "real.txt")

	// Pruning the sole importer would drop pkg/gone from the closure entirely,
	// which is exactly the case an unresolvable pattern is forgiven in.
	err := buildError(t, closure.Options{
		Root: root, ImportPrefix: testPrefix, Roots: []string{"pkg/app"},
		PruneFiles: []string{"pkg/app/extra.go"},
	})
	if !errors.Is(err, closure.ErrUnsafeSymlink) {
		t.Fatalf("error = %v, want ErrUnsafeSymlink", err)
	}
	var fileErr *closure.FileError
	if !errors.As(err, &fileErr) || fileErr.File != "pkg/gone/data/linked.txt" {
		t.Fatalf("error = %v, want *closure.FileError for pkg/gone/data/linked.txt", err)
	}
}

func containsString(values []string, want string) bool {
	return slices.Contains(values, want)
}

func containsSubstring(values []string, want string) bool {
	return slices.ContainsFunc(values, func(v string) bool { return strings.Contains(v, want) })
}
