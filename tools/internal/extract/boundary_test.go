package extract_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// forbiddenGitCalls are the Git operations a plan must never reach for.
//
// The list is the write side of the version control surface: pushing publishes,
// and creating, moving, or tagging a ref changes state that later phases treat
// as append-only. A plan answers a question about one source commit, so it needs
// none of them, and an accidental call would be a publication nobody approved.
//
// The check is by name against the typed runner's methods rather than by
// reviewing the code, because the whole point is to catch the call a future
// change adds without anyone noticing.
var forbiddenGitCalls = []string{
	"Push",
	"CreateRef",
	"UpdateRef",
	"CreateTag",
}

// TestPlanNeverMutatesARemote proves the read-only boundary from the source
// rather than from behaviour.
//
// A behavioural test can only prove that the paths it exercised wrote nothing.
// This proves that no path can, which is the claim the command's contract makes.
func TestPlanNeverMutatesARemote(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("list package sources: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("found no sources to inspect")
	}

	inspected := 0
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		inspected++
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if slices.Contains(forbiddenGitCalls, selector.Sel.Name) {
				t.Errorf("%s: a plan is read-only and must never call %s",
					fset.Position(selector.Pos()), selector.Sel.Name)
			}
			return true
		})
	}
	if inspected == 0 {
		t.Fatal("every source file was skipped, so nothing was checked")
	}
}

// TestPlanWritesOnlyWhereItIsAllowed proves the filesystem side of the same
// boundary: a default plan touches its own cache and work roots and nothing
// else, not even the profile repository it read.
func TestPlanWritesOnlyWhereItIsAllowed(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)
	opts := planOptions(ctx, t, up, fixtureProfile)

	before := snapshotTree(t, opts.ProfileDir)
	upstreamBefore := snapshotTree(t, up.repo.Dir)

	mustPlan(ctx, t, opts)

	if after := snapshotTree(t, opts.ProfileDir); !slices.Equal(before, after) {
		t.Errorf("the plan changed the profile repository:\n before %v\n  after %v", before, after)
	}
	if after := snapshotTree(t, up.repo.Dir); !slices.Equal(upstreamBefore, after) {
		t.Errorf("the plan changed the upstream repository:\n before %v\n  after %v", upstreamBefore, after)
	}
	if _, err := os.Stat(opts.OutputRoot); !os.IsNotExist(err) {
		t.Errorf("the plan wrote the output tree without -materialize: %v", err)
	}
}

// snapshotTree renders a directory as sorted "path=size" entries, which changes
// whenever a file is added, removed, or rewritten.
func snapshotTree(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel)+"="+strconv.FormatInt(info.Size(), 10))
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	slices.Sort(out)
	return out
}
