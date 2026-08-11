package closure_test

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/closure"
)

// goldenPath is the checked-in report for the synthetic RBAC shaped fixture.
const goldenPath = "testdata/rbac-shape.json"

// TestReport_Golden compares the encoded report with a checked-in golden.
//
// The golden is the point of the report: a release is allowed to depend on the
// exact package set, file set, and boundary imports, so any change to them must
// show up as a reviewable diff rather than as a quietly different module. Set
// SOAPBOX_UPDATE_GOLDEN=1 to rewrite it after an intended change.
func TestReport_Golden(t *testing.T) {
	root := writeTree(t, rbacShape())
	result := build(t, rbacOptions(root))

	got, err := result.Report.JSON()
	if err != nil {
		t.Fatalf("encode report: %v", err)
	}
	if os.Getenv("SOAPBOX_UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o750); err != nil {
			t.Fatalf("create testdata directory: %v", err)
		}
		if err := os.WriteFile(goldenPath, got, 0o600); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("updated %s", goldenPath)
		return
	}

	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("report does not match %s\ngot:\n%s\nwant:\n%s", goldenPath, got, want)
	}
}

// TestReport_DeterministicAcrossRoots proves the encoding depends on the closure
// alone. Two identical trees at different absolute paths, scanned through
// different map and directory orderings, must encode to the same bytes, because
// the replay phase compares generated output across machines and runs.
func TestReport_DeterministicAcrossRoots(t *testing.T) {
	first := build(t, rbacOptions(writeTree(t, rbacShape())))
	second := build(t, rbacOptions(writeTree(t, rbacShape())))

	firstJSON, err := first.Report.JSON()
	if err != nil {
		t.Fatalf("encode first report: %v", err)
	}
	secondJSON, err := second.Report.JSON()
	if err != nil {
		t.Fatalf("encode second report: %v", err)
	}
	if string(firstJSON) != string(secondJSON) {
		t.Errorf("reports differ across roots:\n%s\n%s", firstJSON, secondJSON)
	}
}

// TestReport_ListsAreSorted proves every list the report carries is ordered, so
// a golden never churns on filesystem or map iteration order.
func TestReport_ListsAreSorted(t *testing.T) {
	root := writeTree(t, rbacShape())
	result := build(t, rbacOptions(root))

	exact := result.Report.Exact
	lists := map[string][]string{
		"roots":            exact.Roots,
		"packages":         exact.Packages,
		"files":            exact.Files,
		"externalPackages": exact.ExternalPackages,
		"standardPackages": exact.StandardPackages,
		"prunedFiles":      exact.PrunedFiles,
		"deniedImports":    exact.DeniedImports,
		"copyPlan":         planPaths(result.CopyPlan),
		"removedFiles":     result.RemovedFiles,
	}
	for label, values := range lists {
		if !slices.IsSorted(values) {
			t.Errorf("%s is not sorted: %q", label, values)
		}
	}

	for _, pkg := range result.Packages {
		if !slices.IsSorted(pkg.GoFiles) {
			t.Errorf("%s go files are not sorted: %q", pkg.ImportPath, pkg.GoFiles)
		}
		if !slices.IsSorted(pkg.Imports) {
			t.Errorf("%s imports are not sorted: %q", pkg.ImportPath, pkg.Imports)
		}
	}
	if !slices.IsSortedFunc(result.Packages, func(a, b closure.Package) int {
		return strings.Compare(a.ImportPath, b.ImportPath)
	}) {
		t.Errorf("packages are not sorted by import path")
	}
}

// TestReport_EmptyListsEncodeAsArrays proves an empty closure detail encodes as
// [] rather than null, so a golden stays diffable when a profile stops using a
// feature.
func TestReport_EmptyListsEncodeAsArrays(t *testing.T) {
	root := writeTree(t, tree{"pkg/app/app.go": source("app")})
	result := build(t, closure.Options{
		Root: root, ImportPrefix: testPrefix, Roots: []string{"pkg/app"},
	})

	encoded, err := result.Report.JSON()
	if err != nil {
		t.Fatalf("encode report: %v", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	var exact map[string]json.RawMessage
	if err := json.Unmarshal(decoded["exact"], &exact); err != nil {
		t.Fatalf("decode exact shape: %v", err)
	}
	for _, field := range []string{"externalPackages", "standardPackages", "prunedFiles", "deniedImports"} {
		if got := string(exact[field]); got != "[]" {
			t.Errorf("%s = %s, want []", field, got)
		}
	}
}

// TestCopyEntry_JSONRoundTrip proves a copy entry's mode survives encoding in a
// form a reviewer can read. An fs.FileMode encodes as a decimal by default,
// which makes 0755 appear as 493 in a golden nobody can check by eye.
func TestCopyEntry_JSONRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		mode fs.FileMode
		want string
	}{
		{name: "regular", mode: 0o644, want: "0644"},
		{name: "executable", mode: 0o755, want: "0755"},
		{name: "restricted", mode: 0o600, want: "0600"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			entry := closure.CopyEntry{Path: "pkg/app/app.go", Kind: closure.KindGo, Mode: test.mode}
			encoded, err := json.Marshal(entry)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var raw struct {
				Mode string `json:"mode"`
			}
			if err := json.Unmarshal(encoded, &raw); err != nil {
				t.Fatalf("unmarshal probe: %v", err)
			}
			if raw.Mode != test.want {
				t.Errorf("mode = %q, want %q", raw.Mode, test.want)
			}

			var round closure.CopyEntry
			if err := json.Unmarshal(encoded, &round); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if round != entry {
				t.Errorf("round trip = %+v, want %+v", round, entry)
			}
		})
	}
}

// TestCopyPlan_PreservesModes proves the plan carries the permission bits the
// copier has to reproduce.
func TestCopyPlan_PreservesModes(t *testing.T) {
	root := writeTree(t, tree{
		"pkg/app/app.go":      source("app"),
		"pkg/app/gen.sh.tmpl": "#!/bin/sh\n",
	})
	executable := filepath.Join(root, "pkg", "app", "gen.sh.tmpl")
	if err := os.Chmod(executable, 0o755); err != nil {
		t.Fatalf("chmod fixture: %v", err)
	}

	result := build(t, closure.Options{
		Root: root, ImportPrefix: testPrefix, Roots: []string{"pkg/app"},
		AssetGlobs: []string{"pkg/app/*.tmpl"},
	})
	for _, entry := range result.CopyPlan {
		switch entry.Path {
		case "pkg/app/gen.sh.tmpl":
			if entry.Mode.Perm() != 0o755 {
				t.Errorf("asset mode = %v, want 0755", entry.Mode.Perm())
			}
		case "pkg/app/app.go":
			if entry.Mode.Perm() != 0o600 {
				t.Errorf("go file mode = %v, want 0600", entry.Mode.Perm())
			}
		}
	}
}
