package relocate_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/config"
	"github.com/enj/soapbox/tools/internal/relocate"
)

// treeEntries reports every path below root, relative and slash separated, so a
// test can assert on the complete materialized shape rather than on the files
// it remembered to look for.
func treeEntries(t *testing.T, root string) []string {
	t.Helper()
	var found []string
	err := filepath.WalkDir(root, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if !entry.IsDir() {
			found = append(found, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	slices.Sort(found)
	return found
}

// destPath resolves a slash separated destination path below a materialized
// tree.
func destPath(root, rel string) string {
	return filepath.Join(root, filepath.FromSlash(rel))
}

func TestMaterialize(t *testing.T) {
	t.Parallel()

	set, err := relocate.Build(t.Context(), relocate.Plan{Files: []relocate.PlanFile{
		regular("pkg/apis/rbac/v1", "doc.go", "package v1\n"),
		regular("plugin/pkg/auth/authorizer/rbac", "rbac.go", "package rbac\n"),
		{
			Path:     "hack/tool.sh",
			Package:  "hack",
			Mode:     relocate.ModeExecutable,
			Contents: []byte("#!/bin/sh\nexit 0\n"),
		},
	}}, relocate.Options{InternalPrefix: internalPrefix})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	destination := filepath.Join(t.TempDir(), "module")
	if err := relocate.Materialize(t.Context(), destination, set); err != nil {
		t.Fatalf("materialize: %v", err)
	}

	want := []string{
		"internal/kk/hack/tool.sh",
		"internal/kk/pkg/apis/rbac/v1/doc.go",
		"internal/kk/plugin/pkg/auth/authorizer/rbac/rbac.go",
	}
	if got := treeEntries(t, destination); !slices.Equal(got, want) {
		t.Errorf("materialized %v, want %v", got, want)
	}

	contents, err := os.ReadFile(destPath(destination, "internal/kk/pkg/apis/rbac/v1/doc.go"))
	if err != nil {
		t.Fatalf("read relocated file: %v", err)
	}
	if string(contents) != "package v1\n" {
		t.Errorf("relocated file holds %q", contents)
	}

	// Modes are set explicitly rather than left to the process umask, because an
	// executable that lost its bit is committed as a regular file and stays
	// broken for every consumer.
	for path, wantMode := range map[string]fs.FileMode{
		"internal/kk/hack/tool.sh":            0o755,
		"internal/kk/pkg/apis/rbac/v1/doc.go": 0o644,
	} {
		info, err := os.Lstat(destPath(destination, path))
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if got := info.Mode().Perm(); got != wantMode {
			t.Errorf("%s has mode %v, want %v", path, got, wantMode)
		}
	}
}

func TestMaterializeSymlink(t *testing.T) {
	t.Parallel()

	set, err := relocate.Build(t.Context(), relocate.Plan{Files: []relocate.PlanFile{
		regular("pkg/a", "real.go", "package a\n"),
		{
			Path:     "pkg/a/link.go",
			Package:  "pkg/a",
			Mode:     relocate.ModeSymlink,
			Contents: []byte("real.go"),
		},
	}}, relocate.Options{InternalPrefix: internalPrefix, Symlinks: relocate.SymlinkInternal})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	destination := filepath.Join(t.TempDir(), "module")
	if err := relocate.Materialize(t.Context(), destination, set); err != nil {
		t.Fatalf("materialize: %v", err)
	}

	link := destPath(destination, "internal/kk/pkg/a/link.go")
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("stat link: %v", err)
	}
	if info.Mode()&fs.ModeSymlink == 0 {
		t.Fatalf("link.go is %v, want a symbolic link", info.Mode())
	}
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("read link: %v", err)
	}
	if target != "real.go" {
		t.Errorf("link points at %q, want %q", target, "real.go")
	}
}

func TestMaterializeRejectsExistingDestination(t *testing.T) {
	t.Parallel()

	destination := filepath.Join(t.TempDir(), "module")
	if err := os.MkdirAll(destination, 0o750); err != nil {
		t.Fatalf("create destination: %v", err)
	}
	if err := os.WriteFile(filepath.Join(destination, "stale.go"), []byte("package stale\n"), 0o600); err != nil {
		t.Fatalf("write stale file: %v", err)
	}

	set, err := relocate.Build(t.Context(), relocate.Plan{Files: []relocate.PlanFile{
		regular("pkg/a", "a.go", "package a\n"),
	}}, relocate.Options{InternalPrefix: internalPrefix})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	err = relocate.Materialize(t.Context(), destination, set)
	if !errors.Is(err, relocate.ErrDestinationExists) {
		t.Fatalf("materialize error %v, want %v", err, relocate.ErrDestinationExists)
	}
	// A refused materialization must not have touched what was already there.
	if got := treeEntries(t, destination); !slices.Equal(got, []string{"stale.go"}) {
		t.Errorf("destination holds %v, want the untouched stale file", got)
	}
}

// assertNothingCreated fails when a materialization that did not complete left
// anything behind in the directory the destination would have appeared in. A
// leftover scratch tree is picked up by nothing, but it is written next to the
// destination, so a later run would find it there.
func assertNothingCreated(t *testing.T, parent, destination string) {
	t.Helper()
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("destination exists after a failed materialization: %v", err)
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatalf("read parent: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, len(entries))
		for i, entry := range entries {
			names[i] = entry.Name()
		}
		t.Errorf("parent holds %v, want nothing", names)
	}
}

// relocated builds a relocated file the way Build would, so a test set differs
// from a valid one only in the invariant it is written to break.
func relocated(pkg, name, contents string) relocate.File {
	return relocate.File{
		Source:        pkg + "/" + name,
		Path:          internalPrefix + "/" + pkg + "/" + name,
		Package:       internalPrefix + "/" + pkg,
		SourcePackage: pkg,
		Mode:          relocate.ModeRegular,
		Contents:      []byte(contents),
	}
}

// relocatedLink builds a relocated symbolic link the way Build would.
func relocatedLink(pkg, name, target string) relocate.File {
	file := relocated(pkg, name, target)
	file.Mode = relocate.ModeSymlink
	return file
}

// TestMaterializeRejectsInvalidSets covers the sets a caller can hand the write
// boundary that Build would never have produced.
//
// FileSet is an exported type with exported fields, so a set can be assembled
// by hand, decoded, or edited after Build returned it. Materialize therefore
// proves the invariants itself instead of assuming its input came from Build,
// and it does so before creating anything, which is why every case also asserts
// that no scratch tree and no destination were left behind.
func TestMaterializeRejectsInvalidSets(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		files []relocate.File
		want  error
		names []string
	}{
		{
			name: "path that escapes the destination root",
			files: []relocate.File{{
				Source: "pkg/a/a.go",
				Path:   "../outside.go",
				Mode:   relocate.ModeRegular,
			}},
			want: config.ErrPathTraversal,
		},
		{
			name: "absolute path",
			files: []relocate.File{{
				Source: "pkg/a/a.go",
				Path:   "/etc/passwd",
				Mode:   relocate.ModeRegular,
			}},
			want: config.ErrAbsolutePath,
		},
		{
			name: "windows drive letter path",
			files: []relocate.File{{
				Source: "pkg/a/a.go",
				Path:   "C:/windows/hosts",
				Mode:   relocate.ModeRegular,
			}},
			want: config.ErrAbsolutePath,
		},
		{
			name: "unsupported mode",
			files: []relocate.File{{
				Source: "pkg/a/a.go",
				Path:   internalPrefix + "/pkg/a/a.go",
				Mode:   relocate.Mode(9),
			}},
			want: relocate.ErrUnsupportedMode,
		},
		{
			name: "package that disagrees with the destination",
			files: []relocate.File{{
				Source:  "pkg/a/a.go",
				Path:    internalPrefix + "/pkg/a/a.go",
				Package: internalPrefix + "/pkg/b",
				Mode:    relocate.ModeRegular,
			}},
			want: relocate.ErrPackageBoundary,
		},
		{
			name: "source package that disagrees with the source",
			files: []relocate.File{{
				Source:        "pkg/a/a.go",
				Path:          internalPrefix + "/pkg/a/a.go",
				Package:       internalPrefix + "/pkg/a",
				SourcePackage: "pkg/b",
				Mode:          relocate.ModeRegular,
			}},
			want: relocate.ErrPackageBoundary,
		},
		{
			name: "two files on one destination",
			files: []relocate.File{
				{
					Source:        "pkg/a/dup.go",
					Path:          internalPrefix + "/pkg/a/dup.go",
					Package:       internalPrefix + "/pkg/a",
					SourcePackage: "pkg/a",
					Mode:          relocate.ModeRegular,
					Contents:      []byte("first"),
				},
				{
					Source:        "pkg/z/dup.go",
					Path:          internalPrefix + "/pkg/a/dup.go",
					Package:       internalPrefix + "/pkg/a",
					SourcePackage: "pkg/z",
					Mode:          relocate.ModeRegular,
					Contents:      []byte("second"),
				},
			},
			want:  relocate.ErrCollision,
			names: []string{"pkg/a/dup.go", "pkg/z/dup.go"},
		},
		{
			name: "destinations that differ only in case",
			files: []relocate.File{
				relocated("pkg/a", "File.go", "package a\n"),
				relocated("pkg/a", "file.go", "package a\n"),
			},
			want:  relocate.ErrCaseCollision,
			names: []string{"pkg/a/File.go", "pkg/a/file.go"},
		},
		{
			name: "files out of destination order",
			files: []relocate.File{
				relocated("pkg/b", "b.go", "package b\n"),
				relocated("pkg/a", "a.go", "package a\n"),
			},
			want: relocate.ErrUnsorted,
		},
		{
			name: "file that is a directory of another file",
			files: []relocate.File{
				relocated("pkg/a", "b", "a file named like a directory\n"),
				relocated("pkg/a/b", "c.go", "package b\n"),
			},
			want: relocate.ErrOverlap,
		},
		{
			name: "link that escapes the destination root",
			files: []relocate.File{
				relocatedLink("pkg/a", "x.go", "../../../../../etc/passwd"),
			},
			want: relocate.ErrSymlink,
		},
		{
			name: "link with an absolute target",
			files: []relocate.File{
				relocatedLink("pkg/a", "x.go", "/etc/passwd"),
			},
			want: relocate.ErrSymlink,
		},
		{
			name: "link to a target outside the set",
			files: []relocate.File{
				relocatedLink("pkg/a", "x.go", "absent.go"),
			},
			want: relocate.ErrSymlink,
		},
		{
			name: "link cycle",
			files: []relocate.File{
				relocatedLink("pkg/a", "x.go", "y.go"),
				relocatedLink("pkg/a", "y.go", "x.go"),
			},
			want: relocate.ErrSymlink,
		},
		{
			name: "link chain that ends in a cycle",
			files: []relocate.File{
				relocatedLink("pkg/a", "w.go", "x.go"),
				relocatedLink("pkg/a", "x.go", "y.go"),
				relocatedLink("pkg/a", "y.go", "x.go"),
			},
			want: relocate.ErrSymlink,
		},
		{
			// A chain can only end at a regular or executable file because a
			// mode the set cannot record is refused before any link is walked.
			name: "link to a target the set cannot record",
			files: []relocate.File{
				relocatedLink("pkg/a", "x.go", "y.go"),
				func() relocate.File {
					file := relocated("pkg/a", "y.go", "")
					file.Mode = relocate.Mode(9)
					return file
				}(),
			},
			want: relocate.ErrUnsupportedMode,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			parent := t.TempDir()
			destination := filepath.Join(parent, "module")

			err := relocate.Materialize(t.Context(), destination, relocate.FileSet{Files: test.files})
			if !errors.Is(err, test.want) {
				t.Fatalf("materialize error %v, want %v", err, test.want)
			}
			for _, name := range test.names {
				if !strings.Contains(err.Error(), name) {
					t.Errorf("error %v does not name %q", err, name)
				}
			}
			assertNothingCreated(t, parent, destination)
		})
	}
}

// TestMaterializeSymlinkChainResolves proves that a chain materialization
// accepts resolves on a real file system, so the rule that admits it is not
// merely internally consistent.
func TestMaterializeSymlinkChainResolves(t *testing.T) {
	t.Parallel()

	set, err := relocate.Build(t.Context(), relocate.Plan{Files: []relocate.PlanFile{
		regular("pkg/a", "real.go", "package a\n"),
		{
			Path:     "pkg/a/x.go",
			Package:  "pkg/a",
			Mode:     relocate.ModeSymlink,
			Contents: []byte("y.go"),
		},
		{
			Path:     "pkg/a/y.go",
			Package:  "pkg/a",
			Mode:     relocate.ModeSymlink,
			Contents: []byte("real.go"),
		},
	}}, relocate.Options{InternalPrefix: internalPrefix, Symlinks: relocate.SymlinkInternal})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	destination := filepath.Join(t.TempDir(), "module")
	if err := relocate.Materialize(t.Context(), destination, set); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	contents, err := os.ReadFile(destPath(destination, "internal/kk/pkg/a/x.go"))
	if err != nil {
		t.Fatalf("read through the chain: %v", err)
	}
	if string(contents) != "package a\n" {
		t.Errorf("chain resolved to %q", contents)
	}
}

func TestMaterializeHonoursCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	set, err := relocate.Build(t.Context(), relocate.Plan{Files: []relocate.PlanFile{
		regular("pkg/a", "a.go", "package a\n"),
	}}, relocate.Options{InternalPrefix: internalPrefix})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	parent := t.TempDir()
	destination := filepath.Join(parent, "module")
	if err := relocate.Materialize(ctx, destination, set); !errors.Is(err, context.Canceled) {
		t.Fatalf("materialize error %v, want %v", err, context.Canceled)
	}
	assertNothingCreated(t, parent, destination)
}

// TestMaterializeLeavesNothingBehindOnFailure drives a write failure partway
// through the tree.
//
// The set has to be one materialization accepts, because a refused set never
// reaches a disk at all, so the failure is a path element longer than the file
// system can hold. The first file is written before the second one fails, which
// is what makes this the case that exercises the scratch cleanup rather than
// the validation that runs before any scratch tree exists.
func TestMaterializeLeavesNothingBehindOnFailure(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("a", 300)
	if err := os.WriteFile(filepath.Join(t.TempDir(), long), nil, 0o600); err == nil {
		t.Skip("this file system accepts a 300 byte name, so no write can be made to fail")
	}

	parent := t.TempDir()
	destination := filepath.Join(parent, "module")
	set := relocate.FileSet{Files: []relocate.File{
		relocated("pkg/a", "a.go", "package a\n"),
		relocated("pkg/a", long+".go", "package a\n"),
	}}

	if err := relocate.Materialize(t.Context(), destination, set); err == nil {
		t.Fatal("materialize accepted a set it could not write")
	}
	assertNothingCreated(t, parent, destination)
}

func TestMaterializeCreatesMissingParents(t *testing.T) {
	t.Parallel()

	destination := filepath.Join(t.TempDir(), "nested", "deeper", "module")
	set, err := relocate.Build(t.Context(), relocate.Plan{Files: []relocate.PlanFile{
		regular("pkg/a", "a.go", "package a\n"),
	}}, relocate.Options{InternalPrefix: internalPrefix})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if err := relocate.Materialize(t.Context(), destination, set); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if got := treeEntries(t, destination); !slices.Equal(got, []string{"internal/kk/pkg/a/a.go"}) {
		t.Errorf("materialized %v", got)
	}
}

func TestMaterializeEmptySet(t *testing.T) {
	t.Parallel()

	destination := filepath.Join(t.TempDir(), "module")
	if err := relocate.Materialize(t.Context(), destination, relocate.FileSet{}); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	info, err := os.Lstat(destination)
	if err != nil {
		t.Fatalf("stat destination: %v", err)
	}
	if !info.IsDir() {
		t.Error("destination is not a directory")
	}
	if got := treeEntries(t, destination); len(got) != 0 {
		t.Errorf("materialized %v, want an empty tree", got)
	}
}
