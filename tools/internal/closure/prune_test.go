package closure_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/enj/soapbox/tools/internal/closure"
)

// TestBuild_PruneIdempotence proves a Builder may be run repeatedly over the
// same worktree.
//
// The extraction pipeline applies patches and rebuilds until the closure stops
// changing, so a second pass necessarily sees an already pruned tree. The
// builder must not read that as an upstream rename, and it must produce exactly
// the same closure both times.
func TestBuild_PruneIdempotence(t *testing.T) {
	root := writeTree(t, rbacShape())
	ctx := context.Background()
	builder, err := closure.New(ctx, rbacOptions(root))
	if err != nil {
		t.Fatalf("new builder: %v", err)
	}

	first, err := builder.Build(ctx)
	if err != nil {
		t.Fatalf("first build: %v", err)
	}
	second, err := builder.Build(ctx)
	if err != nil {
		t.Fatalf("second build: %v", err)
	}

	if len(second.RemovedFiles) != 0 {
		t.Errorf("second pass removed %q, want nothing left to remove", second.RemovedFiles)
	}
	assertStrings(t, "packages", second.Report.Exact.Packages, first.Report.Exact.Packages)
	assertStrings(t, "files", second.Report.Exact.Files, first.Report.Exact.Files)

	firstJSON, err := first.Report.JSON()
	if err != nil {
		t.Fatalf("encode first report: %v", err)
	}
	secondJSON, err := second.Report.JSON()
	if err != nil {
		t.Fatalf("encode second report: %v", err)
	}
	if string(firstJSON) != string(secondJSON) {
		t.Errorf("report changed between passes:\nfirst:\n%s\nsecond:\n%s", firstJSON, secondJSON)
	}
}

// TestBuild_PruneIsReasserted proves pruning is asserted on every pass rather
// than applied once. A patch is entitled to reintroduce a file the profile
// prunes, and the next pass must remove it again.
func TestBuild_PruneIsReasserted(t *testing.T) {
	root := writeTree(t, rbacShape())
	ctx := context.Background()
	builder, err := closure.New(ctx, rbacOptions(root))
	if err != nil {
		t.Fatalf("new builder: %v", err)
	}
	if _, err := builder.Build(ctx); err != nil {
		t.Fatalf("first build: %v", err)
	}

	// Stand in for a patch that restores the adapter.
	restored := filepath.Join(root, "pkg", "validation", "adapter.go")
	if err := os.WriteFile(restored, []byte(source("validation", `"`+testPrefix+`/pkg/apis/rbac"`)), 0o600); err != nil {
		t.Fatalf("restore adapter: %v", err)
	}

	second, err := builder.Build(ctx)
	if err != nil {
		t.Fatalf("second build: %v", err)
	}
	assertStrings(t, "removed files", second.RemovedFiles, []string{"pkg/validation/adapter.go"})
	if _, err := os.Lstat(restored); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("adapter still present after reassertion, lstat err = %v", err)
	}
}

func TestBuild_PruneFailures(t *testing.T) {
	tests := []struct {
		name  string
		files tree
		// link adds pkg/app/link.go as a symbolic link, which a tree map of file
		// contents cannot express.
		link bool
		opts func(root string) closure.Options
		want error
	}{
		{
			name:  "target upstream no longer has",
			files: tree{"pkg/app/app.go": source("app")},
			opts: func(root string) closure.Options {
				return closure.Options{
					Root: root, ImportPrefix: testPrefix, Roots: []string{"pkg/app"},
					PruneFiles: []string{"pkg/app/renamed.go"},
				}
			},
			want: closure.ErrPruneMissing,
		},
		{
			name:  "target names a directory",
			files: tree{"pkg/app/app.go": source("app"), "pkg/app/sub/sub.go": source("sub")},
			opts: func(root string) closure.Options {
				return closure.Options{
					Root: root, ImportPrefix: testPrefix, Roots: []string{"pkg/app"},
					PruneFiles: []string{"pkg/app/sub"},
				}
			},
			want: closure.ErrPruneMissing,
		},
		{
			name: "target in a package the closure never reached",
			files: tree{
				"pkg/app/app.go":       source("app"),
				"pkg/unrelated/one.go": source("unrelated"),
				"pkg/unrelated/two.go": source("unrelated"),
			},
			opts: func(root string) closure.Options {
				return closure.Options{
					Root: root, ImportPrefix: testPrefix, Roots: []string{"pkg/app"},
					PruneFiles: []string{"pkg/unrelated/one.go"},
				}
			},
			want: closure.ErrPruneOutsideClosure,
		},
		{
			name: "target is not a build input",
			files: tree{
				"pkg/app/app.go": source("app"),
				"pkg/app/OWNERS": "approvers: []\n",
			},
			opts: func(root string) closure.Options {
				return closure.Options{
					Root: root, ImportPrefix: testPrefix, Roots: []string{"pkg/app"},
					PruneFiles: []string{"pkg/app/OWNERS"},
				}
			},
			// The directory is in the closure and the file is not, so the error
			// says which of the two is the problem.
			want: closure.ErrPruneNotMaterialized,
		},
		{
			name: "test file excluded by the build",
			files: tree{
				"pkg/app/app.go":      source("app"),
				"pkg/app/app_test.go": source("app"),
			},
			opts: func(root string) closure.Options {
				return closure.Options{
					Root: root, ImportPrefix: testPrefix, Roots: []string{"pkg/app"},
					PruneFiles: []string{"pkg/app/app_test.go"},
				}
			},
			want: closure.ErrPruneExcludedTest,
		},
		{
			name: "last file of a test-only package",
			files: tree{
				"pkg/app/app.go":        source("app"),
				"pkg/only/only_test.go": source("only"),
			},
			opts: func(root string) closure.Options {
				return closure.Options{
					Root: root, ImportPrefix: testPrefix,
					Roots:        []string{"pkg/app", "pkg/only"},
					IncludeTests: true,
					PruneFiles:   []string{"pkg/only/only_test.go"},
				}
			},
			// The package holds nothing but tests, so its last test file is its
			// last Go file. Reporting this as a package that suddenly has no Go
			// files would name the directory instead of the entry that emptied it.
			want: closure.ErrPruneLastFile,
		},
		{
			name:  "target is a symbolic link",
			files: tree{"pkg/app/app.go": source("app"), "pkg/app/real.go": source("app")},
			link:  true,
			opts: func(root string) closure.Options {
				return closure.Options{
					Root: root, ImportPrefix: testPrefix, Roots: []string{"pkg/app"},
					PruneFiles: []string{"pkg/app/link.go"},
				}
			},
			want: closure.ErrUnsafeSymlink,
		},
		{
			name:  "removing a root package's last file",
			files: tree{"pkg/app/app.go": source("app")},
			opts: func(root string) closure.Options {
				return closure.Options{
					Root: root, ImportPrefix: testPrefix, Roots: []string{"pkg/app"},
					PruneFiles: []string{"pkg/app/app.go"},
				}
			},
			want: closure.ErrPruneLastFile,
		},
		{
			name: "removing every file of a discovered package",
			files: tree{
				"pkg/app/app.go": source("app", `"`+testPrefix+`/pkg/lib"`),
				"pkg/lib/one.go": source("lib"),
				"pkg/lib/two.go": source("lib"),
			},
			opts: func(root string) closure.Options {
				return closure.Options{
					Root: root, ImportPrefix: testPrefix, Roots: []string{"pkg/app"},
					PruneFiles: []string{"pkg/lib/one.go", "pkg/lib/two.go"},
				}
			},
			want: closure.ErrPruneLastFile,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := writeTree(t, test.files)
			if test.link {
				symlink(t, root, "pkg/app/link.go", "real.go")
			}
			err := buildError(t, test.opts(root))
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}

// TestBuild_PruneValidatesBeforeRemoving proves a failed prune pass leaves the
// worktree untouched. A run that half pruned a tree before failing would need a
// fresh checkout to retry, and the pipeline retries after fixing the profile.
func TestBuild_PruneValidatesBeforeRemoving(t *testing.T) {
	root := writeTree(t, tree{
		"pkg/app/app.go":   source("app"),
		"pkg/app/extra.go": source("app"),
	})
	err := buildError(t, closure.Options{
		Root: root, ImportPrefix: testPrefix, Roots: []string{"pkg/app"},
		// The first entry is valid; the second never existed.
		PruneFiles: []string{"pkg/app/extra.go", "pkg/app/renamed.go"},
	})
	if !errors.Is(err, closure.ErrPruneMissing) {
		t.Fatalf("error = %v, want ErrPruneMissing", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "pkg", "app", "extra.go")); err != nil {
		t.Errorf("valid prune target was removed despite the failure: %v", err)
	}
}

// TestBuild_PruneIncludedTest proves that a _test.go file is prunable exactly
// when the build carries tests, which is the other half of the excluded test
// error.
func TestBuild_PruneIncludedTest(t *testing.T) {
	root := writeTree(t, tree{
		"pkg/app/app.go":      source("app"),
		"pkg/app/app_test.go": source("app"),
	})
	result := build(t, closure.Options{
		Root: root, ImportPrefix: testPrefix, Roots: []string{"pkg/app"},
		IncludeTests: true,
		PruneFiles:   []string{"pkg/app/app_test.go"},
	})
	assertStrings(t, "removed files", result.RemovedFiles, []string{"pkg/app/app_test.go"})
	assertStrings(t, "files", result.Report.Exact.Files, []string{"pkg/app/app.go"})
}

// TestBuild_PrunedFileIsNotResurrectedAsAnAsset proves an asset glob is answered
// against the tree the build produced, not the tree it started from.
//
// Asset globs and prune entries can name the same file, because a build input is
// still a file an operator may glob. Resolving the globs once, before pruning,
// would put a deleted path back into the copy plan: the report would count a
// file that is not there and the copier would fail on it long after the closure
// claimed to be proved.
func TestBuild_PrunedFileIsNotResurrectedAsAnAsset(t *testing.T) {
	files := tree{
		"pkg/app/app.go": source("app"),
		"pkg/app/gen.go": source("app"),
	}

	t.Run("surviving matches are kept and pruned ones are not", func(t *testing.T) {
		root := writeTree(t, files)
		result := build(t, closure.Options{
			Root: root, ImportPrefix: testPrefix, Roots: []string{"pkg/app"},
			AssetGlobs: []string{"pkg/app/*.go"},
			PruneFiles: []string{"pkg/app/gen.go"},
		})
		assertStrings(t, "files", result.Report.Exact.Files, []string{"pkg/app/app.go"})
		if got, want := result.Report.Observed.PostPrune.Files, 1; got != want {
			t.Errorf("post-prune files = %d, want %d", got, want)
		}
		// The pre-prune plan counted both, so the difference is what pruning
		// actually removed rather than what the stale match claimed survived.
		if got, want := result.Report.Observed.Growth.FilesRemoved, 1; got != want {
			t.Errorf("files removed = %d, want %d", got, want)
		}
	})

	t.Run("glob left with nothing fails closed", func(t *testing.T) {
		root := writeTree(t, files)
		err := buildError(t, closure.Options{
			Root: root, ImportPrefix: testPrefix, Roots: []string{"pkg/app"},
			AssetGlobs: []string{"pkg/app/gen.go"},
			PruneFiles: []string{"pkg/app/gen.go"},
		})
		// A profile that prunes the only file its asset glob names contradicts
		// itself, and the contradiction is reported rather than resolved by
		// copying a file that no longer exists.
		if !errors.Is(err, closure.ErrPatternNoMatch) {
			t.Fatalf("error = %v, want ErrPatternNoMatch", err)
		}
	})
}

// TestBuild_BaselineSurvivesFailedPass proves the pre-prune measurement names
// the tree upstream produced rather than the tree the last pass left behind.
//
// A failed pass still prunes: the profile is asserted before the closure is
// judged, so a run that fails on a denied import has already mutated the
// worktree. If the baseline were recorded only on success, the corrected pass
// would measure an already pruned tree and report that pruning removed nothing,
// turning the report that exists to prove the reduction into evidence against
// it.
func TestBuild_BaselineSurvivesFailedPass(t *testing.T) {
	root := writeTree(t, rbacShape())
	ctx := context.Background()
	opts := rbacOptions(root)
	// Only the helper is pruned, so the adapter keeps importing the denied
	// package and the first pass fails after pruning.
	opts.PruneFiles = []string{"pkg/apis/rbac/v1/helpers.go"}
	builder, err := closure.New(ctx, opts)
	if err != nil {
		t.Fatalf("new builder: %v", err)
	}

	if _, err := builder.Build(ctx); !errors.Is(err, closure.ErrImportDenied) {
		t.Fatalf("first build error = %v, want ErrImportDenied", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "pkg", "apis", "rbac", "v1", "helpers.go")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixture precondition: the failed pass should still have pruned, lstat err = %v", err)
	}

	// Stand in for the patch the pipeline applies between passes.
	adapter := filepath.Join(root, "pkg", "validation", "adapter.go")
	if err := os.WriteFile(adapter, []byte(source("validation")), 0o600); err != nil {
		t.Fatalf("patch adapter: %v", err)
	}

	result, err := builder.Build(ctx)
	if err != nil {
		t.Fatalf("second build: %v", err)
	}
	// Four packages and seven Go files is the shape of the tree as written, not
	// the shape the second pass found.
	if got, want := result.Report.Observed.PrePrune.Packages, 4; got != want {
		t.Errorf("pre-prune packages = %d, want %d", got, want)
	}
	if got, want := result.Report.Observed.PrePrune.GoFiles, 7; got != want {
		t.Errorf("pre-prune go files = %d, want %d", got, want)
	}
	if got, want := result.Report.Observed.Growth.PackagesRemoved, 1; got != want {
		t.Errorf("packages removed = %d, want %d", got, want)
	}
	if got, want := result.Report.Observed.Growth.GoFilesRemoved, 2; got != want {
		t.Errorf("go files removed = %d, want %d", got, want)
	}
}

// TestBuild_FreshBuilderOverPrunedTree proves the restart contract.
//
// Nothing on disk records that soapbox removed a file, so a Builder that did not
// do the removing cannot tell a completed prune from an upstream rename. It
// fails, naming the entry, rather than treating an absent file as proof of the
// removal it was asked to assert. A caller resuming after a crash rematerializes
// the worktree instead.
func TestBuild_FreshBuilderOverPrunedTree(t *testing.T) {
	root := writeTree(t, rbacShape())

	first := build(t, rbacOptions(root))
	if len(first.RemovedFiles) == 0 {
		t.Fatalf("fixture precondition: the first build should have pruned something")
	}

	err := buildError(t, rbacOptions(root))
	if !errors.Is(err, closure.ErrPruneMissing) {
		t.Fatalf("error = %v, want ErrPruneMissing", err)
	}
	var fileErr *closure.FileError
	if !errors.As(err, &fileErr) {
		t.Fatalf("error = %v, want *closure.FileError naming the entry", err)
	}
	if !slices.Contains(rbacOptions(root).PruneFiles, fileErr.File) {
		t.Errorf("file = %q, want one of the configured prune entries", fileErr.File)
	}

	// Rematerializing is what makes the retry work, and it works because the
	// builder state and the tree state agree again.
	build(t, rbacOptions(writeTree(t, rbacShape())))
}

func TestBuild_RequiredFiles(t *testing.T) {
	files := tree{
		"pkg/app/app.go":   source("app", `"`+testPrefix+`/pkg/lib"`),
		"pkg/app/extra.go": source("app", `"`+testPrefix+`/pkg/gone"`),
		"pkg/lib/lib.go":   source("lib"),
		"pkg/gone/gone.go": source("gone"),
	}

	t.Run("retained file passes", func(t *testing.T) {
		root := writeTree(t, files)
		build(t, closure.Options{
			Root: root, ImportPrefix: testPrefix, Roots: []string{"pkg/app"},
			RequiredFiles: []string{"pkg/app/app.go", "pkg/lib/lib.go"},
		})
	})

	t.Run("upstream rename fails closed", func(t *testing.T) {
		root := writeTree(t, files)
		err := buildError(t, closure.Options{
			Root: root, ImportPrefix: testPrefix, Roots: []string{"pkg/app"},
			RequiredFiles: []string{"pkg/lib/renamed.go"},
		})
		if !errors.Is(err, closure.ErrRequiredMissing) {
			t.Fatalf("error = %v, want ErrRequiredMissing", err)
		}
		var fileErr *closure.FileError
		if !errors.As(err, &fileErr) || fileErr.File != "pkg/lib/renamed.go" {
			t.Fatalf("error = %v, want *closure.FileError for pkg/lib/renamed.go", err)
		}
	})

	// Presence on disk is not retention. pkg/gone still exists in the worktree,
	// but nothing in the post-prune closure imports it, so the generated module
	// would not contain the required file.
	t.Run("file whose package left the closure fails closed", func(t *testing.T) {
		root := writeTree(t, files)
		err := buildError(t, closure.Options{
			Root: root, ImportPrefix: testPrefix, Roots: []string{"pkg/app"},
			PruneFiles:    []string{"pkg/app/extra.go"},
			RequiredFiles: []string{"pkg/gone/gone.go"},
		})
		if !errors.Is(err, closure.ErrRequiredMissing) {
			t.Fatalf("error = %v, want ErrRequiredMissing", err)
		}
		if _, statErr := os.Lstat(filepath.Join(root, "pkg", "gone", "gone.go")); statErr != nil {
			t.Fatalf("fixture precondition: pkg/gone/gone.go should still be on disk: %v", statErr)
		}
	})
}
