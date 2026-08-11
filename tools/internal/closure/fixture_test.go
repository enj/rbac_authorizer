package closure_test

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/closure"
)

// testPrefix stands in for k8s.io/kubernetes. A synthetic prefix keeps the
// fixtures readable and proves the engine never special cases Kubernetes.
const testPrefix = "example.com/src"

// tree is a synthetic worktree: repository relative slash paths to contents.
type tree map[string]string

// writeTree materializes a tree under a fresh temporary directory and returns
// its root.
//
// Fixtures are real directories rather than an in-memory filesystem because the
// behaviour under test includes symbolic link refusal, file mode preservation,
// and containment enforced by os.Root, none of which a fake would exercise.
func writeTree(t *testing.T, files tree) string {
	t.Helper()
	root := t.TempDir()
	for _, rel := range slices.Sorted(maps.Keys(files)) {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatalf("create directory for %s: %v", rel, err)
		}
		if err := os.WriteFile(full, []byte(files[rel]), 0o600); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	return root
}

// symlink adds a symbolic link to an already materialized tree.
func symlink(t *testing.T, root, rel, target string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		t.Fatalf("create directory for %s: %v", rel, err)
	}
	if err := os.Symlink(target, full); err != nil {
		t.Fatalf("link %s -> %s: %v", rel, target, err)
	}
}

// rbacShape is a synthetic tree with the same topology as the Kubernetes RBAC
// authorizer profile: one configured root, a four package pre-prune closure, an
// internal API package reachable only through files the profile prunes, and a
// retained /v1 helper subpackage that the exact deny rule must not match.
//
// Keeping the shape rather than the content means the unit tests assert the
// same invariants the real profile depends on without cloning Kubernetes.
func rbacShape() tree {
	return tree{
		"plugin/rbac/rbac.go": source("rbac",
			`"fmt"`,
			`"k8s.io/api/rbac/v1"`,
			`"`+testPrefix+`/pkg/validation"`,
			`"`+testPrefix+`/pkg/apis/rbac/v1"`,
		),
		"plugin/rbac/subject_locator.go": source("rbac", `"`+testPrefix+`/pkg/validation"`),
		// Repository metadata that the generated module must never inherit.
		"plugin/rbac/OWNERS":      "approvers:\n  - someone\n",
		"plugin/rbac/BUILD.bazel": "go_library()\n",
		// Upstream keeps bootstrappolicy below the configured root rather than
		// beside it. Package granularity has to exclude it on the strength of
		// nothing importing it, not on the strength of it living elsewhere.
		"plugin/rbac/bootstrappolicy/policy.go": source("bootstrappolicy"),
		"plugin/sibling/sibling.go":             source("sibling"),

		"pkg/validation/rule.go":    source("validation", `"`+testPrefix+`/pkg/apis/rbac/v1"`),
		"pkg/validation/adapter.go": source("validation", `"`+testPrefix+`/pkg/apis/rbac"`),

		"pkg/apis/rbac/types.go":      source("rbac"),
		"pkg/apis/rbac/v1/doc.go":     source("v1"),
		"pkg/apis/rbac/v1/helpers.go": source("v1", `"`+testPrefix+`/pkg/apis/rbac"`),
	}
}

// rbacOptions returns options for rbacShape with the pruning and deny rules the
// real profile uses.
func rbacOptions(root string) closure.Options {
	return closure.Options{
		Root:         root,
		ImportPrefix: testPrefix,
		Roots:        []string{"plugin/rbac"},
		PruneFiles: []string{
			"pkg/apis/rbac/v1/helpers.go",
			"pkg/validation/adapter.go",
		},
		RequiredFiles: []string{
			"pkg/apis/rbac/v1/doc.go",
			"plugin/rbac/rbac.go",
		},
		DeniedImports: []string{testPrefix + "/pkg/apis/rbac"},
	}
}

// source renders a minimal Go file with the given package name and imports.
func source(pkg string, imports ...string) string {
	var b strings.Builder
	b.WriteString("package " + pkg + "\n")
	if len(imports) > 0 {
		b.WriteString("\nimport (\n")
		for _, imp := range imports {
			b.WriteString("\t" + imp + "\n")
		}
		b.WriteString(")\n")
	}
	return b.String()
}

// build runs one closure build and fails the test if it does not succeed.
func build(t *testing.T, opts closure.Options) *closure.Result {
	t.Helper()
	ctx := context.Background()
	builder, err := closure.New(ctx, opts)
	if err != nil {
		t.Fatalf("new builder: %v", err)
	}
	result, err := builder.Build(ctx)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return result
}

// buildError runs one closure build and fails the test if it succeeds.
func buildError(t *testing.T, opts closure.Options) error {
	t.Helper()
	ctx := context.Background()
	builder, err := closure.New(ctx, opts)
	if err != nil {
		return err
	}
	result, err := builder.Build(ctx)
	if err == nil {
		t.Fatalf("build succeeded, want failure; got %d packages", len(result.Packages))
	}
	return err
}

// assertStrings compares two ordered string lists.
func assertStrings(t *testing.T, label string, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s =\n  %q\nwant\n  %q", label, got, want)
	}
}

// planPaths lists a copy plan's paths.
func planPaths(plan []closure.CopyEntry) []string {
	out := make([]string, 0, len(plan))
	for _, entry := range plan {
		out = append(out, entry.Path)
	}
	return out
}

// planKind reports the kind recorded for one path.
func planKind(plan []closure.CopyEntry, path string) closure.CopyKind {
	for _, entry := range plan {
		if entry.Path == path {
			return entry.Kind
		}
	}
	return ""
}
