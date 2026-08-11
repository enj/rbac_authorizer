package patchset_test

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/config"
	"github.com/enj/soapbox/tools/internal/patchset"
)

// writePatchFile writes one patch file below a repository root.
func writePatchFile(t *testing.T, root, rel, contents string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		t.Fatalf("create directory for %s: %v", rel, err)
	}
	if err := os.WriteFile(full, []byte(contents), 0o600); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

func TestLoad(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writePatchFile(t, root, "patches/0001-export.patch", "--- a/x\n+++ b/x\n")
	writePatchFile(t, root, "patches/0002-adapt.patch", "--- a/y\n+++ b/y\n")

	got, err := patchset.Load(t.Context(), root, []config.Patch{
		{File: "patches/0002-adapt.patch", Since: "abc", Branches: []string{"master"}},
		{File: "patches/0001-export.patch", Until: "def"},
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	// Configured order is authoring order, so the loader must not sort.
	want := []string{"patches/0002-adapt.patch", "patches/0001-export.patch"}
	if names := ids(got); !slices.Equal(names, want) {
		t.Fatalf("loaded %v, want %v", names, want)
	}
	if string(got[0].Diff) != "--- a/y\n+++ b/y\n" {
		t.Errorf("first patch carries %q", got[0].Diff)
	}
	if got[0].Since != "abc" || !slices.Equal(got[0].Branches, []string{"master"}) {
		t.Errorf("first patch lost its selectors: %+v", got[0])
	}
	if got[1].Until != "def" {
		t.Errorf("second patch lost its until selector: %+v", got[1])
	}
}

func TestLoadEmptySeries(t *testing.T) {
	t.Parallel()

	got, err := patchset.Load(t.Context(), t.TempDir(), nil)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got != nil {
		t.Errorf("loaded %v, want nothing", ids(got))
	}
}

func TestLoadRejects(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writePatchFile(t, root, "patches/blank.patch", "   \n\n")
	writePatchFile(t, root, "patches/real.patch", "--- a/x\n+++ b/x\n")
	writePatchFile(t, root, "outside.patch", "--- a/x\n+++ b/x\n")

	// A link that resolves outside the repository is the interesting traversal:
	// the configured path is clean and relative, so only a containment check
	// performed by the read itself can catch it.
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "hostile.patch"), []byte("--- a/x\n"), 0o600); err != nil {
		t.Fatalf("write hostile patch: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "patches", "link")); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	tests := []struct {
		name string
		file string
		want error
		// wantText matches failures that carry no sentinel, which is how the
		// operating system level containment check reports an escape.
		wantText string
	}{
		{name: "missing file", file: "patches/absent.patch", want: os.ErrNotExist},
		{name: "blank diff", file: "patches/blank.patch", want: patchset.ErrEmptyPatch},
		{name: "parent traversal", file: "../outside.patch", want: config.ErrPathTraversal},
		{name: "absolute path", file: "/etc/passwd", want: config.ErrAbsolutePath},
		{name: "symlink escape", file: "patches/link/hostile.patch", wantText: "escapes"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := patchset.Load(t.Context(), root, []config.Patch{{File: test.file}})
			if err == nil {
				t.Fatalf("load accepted %q", test.file)
			}
			if test.want != nil && !errors.Is(err, test.want) {
				t.Errorf("load error %v, want %v", err, test.want)
			}
			if test.wantText != "" && !strings.Contains(err.Error(), test.wantText) {
				t.Errorf("load error %v, want it to mention %q", err, test.wantText)
			}
		})
	}
}

func TestLoadRejectsDuplicateFiles(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writePatchFile(t, root, "patches/0001-a.patch", "--- a/x\n+++ b/x\n")

	_, err := patchset.Load(t.Context(), root, []config.Patch{
		{File: "patches/0001-a.patch"},
		{File: "patches/0001-a.patch"},
	})
	if !errors.Is(err, patchset.ErrDuplicatePatch) {
		t.Errorf("load error %v, want %v", err, patchset.ErrDuplicatePatch)
	}
}
