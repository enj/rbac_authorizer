package rewrite_test

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/rewrite"
)

// update rewrites the golden files instead of comparing against them. A golden
// file is reviewed by reading the diff it produces, so regenerating one is a
// deliberate step rather than something a failing test does on its own.
var update = flag.Bool("update", false, "rewrite the golden files in testdata")

// goldenSources maps each golden fixture onto the upstream path it came from,
// because the modification notice and the provenance record name that path.
var goldenSources = map[string]string{
	"authorizer": "plugin/pkg/auth/authorizer/rbac/rbac.go",
	"doc":        "pkg/apis/rbac/v1/doc.go",
	"generated":  "pkg/apis/rbac/v1/zz_generated.deepcopy.go",
	"cgo":        "pkg/registry/rbac/validation/probe.go",
}

// prunedForGolden is the RBAC profile's prune outcome as the marker rules see
// it: the internal rbac API package is pruned, so a generator marker naming it
// as an input, and a deepcopy marker whose generated output went with it, both
// dangle.
func prunedForGolden(directive rewrite.Directive) bool {
	return slices.Contains([]string{
		"k8s.io/kubernetes/pkg/apis/rbac",
		"package",
		"TypeMeta",
	}, directive.Value)
}

// TestGolden runs each fixture through the complete Go transformation and
// compares the result byte for byte.
//
// The fixtures are upstream shaped on purpose: a license header, grouped
// imports mixing external and internal modules, generator markers, a build
// constraint, a generated file marker, a cgo preamble, and strings and comments
// that read like import paths. Between them they cover every construct the
// transformation has to preserve, and a golden comparison makes an unintended
// change to any of them visible as a diff rather than as a passing test.
func TestGolden(t *testing.T) {
	t.Parallel()

	inputs, err := filepath.Glob(filepath.Join("testdata", "golden", "*.input"))
	if err != nil {
		t.Fatalf("list golden inputs: %v", err)
	}
	if len(inputs) == 0 {
		t.Fatal("no golden inputs found")
	}

	for _, input := range inputs {
		name := strings.TrimSuffix(filepath.Base(input), ".input")
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			source, ok := goldenSources[name]
			if !ok {
				t.Fatalf("golden fixture %q has no upstream path", name)
			}
			contents, err := os.ReadFile(input)
			if err != nil {
				t.Fatalf("read %s: %v", input, err)
			}

			options := noticedOptions()
			options.Directives.Dangling = prunedForGolden
			file := rewrite.File{
				Path:       "internal/kk/" + source,
				SourcePath: source,
				Contents:   contents,
				Generated:  name == "generated",
			}

			result, err := rewrite.GoFile(t.Context(), file, options)
			if err != nil {
				t.Fatalf("rewrite: %v", err)
			}
			assertGofmt(t, result.Contents)

			golden := filepath.Join("testdata", "golden", name+".golden")
			if *update {
				if err := os.WriteFile(golden, result.Contents, 0o600); err != nil {
					t.Fatalf("write %s: %v", golden, err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("read %s: %v", golden, err)
			}
			if !bytes.Equal(result.Contents, want) {
				t.Errorf("rewrote %s to:\n%s\nwant:\n%s", input, result.Contents, want)
			}

			// A second pass over the golden output must change nothing, which
			// is what makes a replayed source commit produce the same tree.
			again := file
			again.Contents = want
			second, err := rewrite.GoFile(t.Context(), again, options)
			if err != nil {
				t.Fatalf("second rewrite: %v", err)
			}
			if second.Changed() {
				t.Errorf("a second pass changed %v", second.Changes)
			}
		})
	}
}
