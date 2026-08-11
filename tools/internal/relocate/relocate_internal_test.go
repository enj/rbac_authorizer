package relocate

import (
	"path"
	"testing"
)

// TestFoldPath covers the key two destination paths are compared by.
//
// Path validation keeps a destination to ASCII today, so the Unicode rows are
// not reachable through Build. They are covered because the key is defined by
// simple case folding rather than by ASCII lowering, and that definition is
// what keeps the rule right if the accepted character set ever widens: the file
// system a consumer checks the module out on folds the whole of Unicode, not
// the ASCII part of it.
func TestFoldPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		left  string
		right string
		same  bool
	}{
		{name: "identical", left: "pkg/a/doc.go", right: "pkg/a/doc.go", same: true},
		{name: "file name case", left: "pkg/a/File.go", right: "pkg/a/file.go", same: true},
		{name: "directory case", left: "pkg/A/doc.go", right: "pkg/a/doc.go", same: true},
		{name: "extension case", left: "pkg/a/doc.GO", right: "pkg/a/doc.go", same: true},
		// U+212A KELVIN SIGN folds together with both ASCII k and ASCII K.
		{name: "kelvin sign", left: "pkg/a/Kelvin.go", right: "pkg/a/kelvin.go", same: true},
		// U+017F LATIN SMALL LETTER LONG S folds together with ASCII s.
		{name: "long s", left: "pkg/a/ſum.go", right: "pkg/a/sum.go", same: true},
		{name: "different names", left: "pkg/a/doc.go", right: "pkg/a/dock.go"},
		{name: "different directories", left: "pkg/a/doc.go", right: "pkg/b/doc.go"},
		// Folding must not reach across a separator and turn two paths into one.
		{name: "separator against a letter", left: "pkg/a/doc.go", right: "pkg/adoc.go"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if same := foldPath(test.left) == foldPath(test.right); same != test.same {
				t.Errorf("%q and %q fold to %q and %q, want same = %v",
					test.left, test.right, foldPath(test.left), foldPath(test.right), test.same)
			}
		})
	}
}

// TestFoldPathPreservesStructure pins the property checkOverlap relies on: the
// folded form of a parent directory is the parent directory of the folded form,
// so walking the ancestors of a folded path finds the same directories walking
// the ancestors of the original one would.
func TestFoldPathPreservesStructure(t *testing.T) {
	t.Parallel()

	for _, p := range []string{
		"internal/kk/pkg/apis/rbac/v1/doc.go",
		"internal/kk/pkg/A/B/C.go",
		"internal/kk/plugin/pkg/auth/authorizer/rbac/RBAC.go",
	} {
		for dir := p; dir != "."; dir = path.Dir(dir) {
			if got, want := path.Dir(foldPath(dir)), foldPath(path.Dir(dir)); got != want {
				t.Errorf("folded parent of %q is %q, want %q", dir, got, want)
			}
		}
	}
}
