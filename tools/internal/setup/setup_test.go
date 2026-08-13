package setup_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/mod/modfile"

	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/setup"
)

// TestPlanClassifiesTemplate pins the whole allowlist.
//
// The four sets are asserted exactly rather than by sampling, because the
// allowlist is the security boundary: a path that silently moved from preserved
// to deleted is precisely the regression this package exists to prevent, and a
// test that only checked the paths it expected to find would not see it.
func TestPlanClassifiesTemplate(t *testing.T) {
	ctx := t.Context()
	root, git := newTemplate(ctx, t, map[string]string{
		"Makefile":                 "all:\n\techo hi\n",
		"internal/kk/rbac/rbac.go": "package rbac\n",
		"authorizer.go":            "package rbacauthorizer\n",
	})
	result := plan(ctx, t, newOptions(ctx, t, root, git))

	tests := []struct {
		name string
		got  []string
		want []string
	}{
		{
			name: "created",
			got:  actionPaths(result, setup.ActionCreate),
			want: []string{".github/workflows/sync.yml", "go.mod"},
		},
		{
			name: "replaced",
			got:  actionPaths(result, setup.ActionReplace),
			want: []string{".github/workflows/ci.yml", "tools/cmd/soapbox/main.go", "tools/go.mod"},
		},
		{
			name: "deleted",
			got:  actionPaths(result, setup.ActionDelete),
			want: []string{
				".claude/settings.json",
				".github/workflows/template-selftest.yml",
				".golangci.yml",
				".serena/project.yml",
				"CLAUDE.md",
				"docs/setup.md",
				"plans/goal.md",
				"plans/implementation.md",
				"tools/go.sum",
				"tools/internal/cli/cli.go",
				"tools/internal/config/config.go",
				"tools/soapbox.go",
				"tools/soapbox_test.go",
			},
		},
		{
			name: "kept",
			got:  result.Report.Kept,
			want: []string{
				".gitattributes",
				".gitignore",
				"LICENSE",
				"NOTICE",
				"README.md",
				"authorizer.go",
				"internal/kk/rbac/rbac.go",
				"patches/README.md",
				"patches/index.yaml",
				"soapbox.yaml",
			},
		},
		{
			name: "preserved but unrecognised",
			got:  result.Report.Ignored,
			want: []string{"Makefile"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if !slices.Equal(test.got, test.want) {
				t.Errorf("%s paths:\n got %q\nwant %q", test.name, test.got, test.want)
			}
		})
	}

	if result.Applied {
		t.Error("a plan reported itself as applied")
	}
	assertUnchanged(ctx, t, git)
}

// TestApplyTransformsRepository runs the transformation and inspects the result
// on disk rather than in the manifest.
func TestApplyTransformsRepository(t *testing.T) {
	ctx := t.Context()
	root, git := newTemplate(ctx, t, map[string]string{"Makefile": "all:\n"})
	opts := newOptions(ctx, t, root, git)
	planned := plan(ctx, t, opts)

	result, err := setup.Apply(ctx, opts, planned.Report.Hash)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !result.Applied {
		t.Fatal("apply did not report itself as applied")
	}

	for _, present := range []string{
		"go.mod",
		"tools/go.mod",
		"tools/cmd/soapbox/main.go",
		".github/workflows/ci.yml",
		".github/workflows/sync.yml",
		// Retained and preserved paths survive a transformation they were never
		// part of.
		"soapbox.yaml",
		"patches/index.yaml",
		"LICENSE",
		"Makefile",
	} {
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(present))); err != nil {
			t.Errorf("%s should exist after setup: %v", present, err)
		}
	}
	for _, absent := range []string{
		"CLAUDE.md",
		"plans",
		".claude",
		".serena",
		"docs",
		"tools/soapbox.go",
		"tools/soapbox_test.go",
		"tools/go.sum",
		"tools/internal",
		".github/workflows/template-selftest.yml",
	} {
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(absent))); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s should be gone after setup, got %v", absent, err)
		}
	}
	// The directory that held a preserved file is still there, which is what
	// distinguishes pruning empty directories from removing directories.
	if _, err := os.Lstat(filepath.Join(root, "tools", "cmd", "soapbox")); err != nil {
		t.Errorf("tools/cmd/soapbox should survive because setup composes a file in it: %v", err)
	}
}

// TestApplyIsRefusedTwice proves the transformation is not idempotent by
// design. A repository that already has a root module is no longer a template,
// and composing a second root module over the first would overwrite whatever the
// first generation put there.
func TestApplyIsRefusedTwice(t *testing.T) {
	ctx := t.Context()
	root, git := newTemplate(ctx, t, nil)
	opts := newOptions(ctx, t, root, git)
	planned := plan(ctx, t, opts)

	if _, err := setup.Apply(ctx, opts, planned.Report.Hash); err != nil {
		t.Fatalf("apply: %v", err)
	}
	commit(ctx, t, git, "chore: derived repository")

	_, err := setup.Plan(ctx, opts)
	if !errors.Is(err, setup.ErrNotTemplate) {
		t.Fatalf("second plan error = %v, want ErrNotTemplate", err)
	}
	assertPolicy(t, err)
}

// TestApplyRequiresTheCurrentManifest covers the approval contract.
func TestApplyRequiresTheCurrentManifest(t *testing.T) {
	ctx := t.Context()
	root, git := newTemplate(ctx, t, nil)
	opts := newOptions(ctx, t, root, git)
	planned := plan(ctx, t, opts)

	tests := []struct {
		name    string
		approve string
	}{
		{name: "empty", approve: ""},
		{name: "blank", approve: "   "},
		{name: "another hash", approve: strings.Repeat("0", 64)},
		{name: "truncated", approve: planned.Report.Hash[:16]},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := setup.Apply(ctx, opts, test.approve)
			if !errors.Is(err, setup.ErrApproval) {
				t.Fatalf("apply error = %v, want ErrApproval", err)
			}
			assertPolicy(t, err)
			if result != nil && result.Applied {
				t.Error("a refused apply reported itself as applied")
			}
			assertUnchanged(ctx, t, git)
		})
	}

	// The same hash spelled in the other case is the same hash. Nothing else is.
	if _, err := setup.Apply(ctx, opts, strings.ToUpper(planned.Report.Hash)); err != nil {
		t.Fatalf("apply with an upper case approval: %v", err)
	}
}

// TestApplyRefusesAStaleApproval proves the hash is compared against a freshly
// measured repository rather than against the plan that produced it. A tree that
// changed after the operator read the manifest is a tree they did not approve.
func TestApplyRefusesAStaleApproval(t *testing.T) {
	ctx := t.Context()
	root, git := newTemplate(ctx, t, nil)
	opts := newOptions(ctx, t, root, git)
	stale := plan(ctx, t, opts).Report.Hash

	if err := os.WriteFile(filepath.Join(root, "docs", "extra.md"), []byte("# extra\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	commit(ctx, t, git, "docs: add a page")

	fresh := plan(ctx, t, opts).Report.Hash
	if fresh == stale {
		t.Fatal("adding a template owned file did not change the manifest")
	}
	if _, err := setup.Apply(ctx, opts, stale); !errors.Is(err, setup.ErrApproval) {
		t.Fatalf("apply error = %v, want ErrApproval", err)
	}
}

// TestManifestIsIndependentOfLocation is the determinism property the approval
// hash rests on: the same template produces the same manifest from two different
// directories, so two operators can compare hashes.
func TestManifestIsIndependentOfLocation(t *testing.T) {
	ctx := t.Context()

	first := plan(ctx, t, mustOptions(ctx, t))
	second := plan(ctx, t, mustOptions(ctx, t))

	if first.Report.Hash != second.Report.Hash {
		t.Errorf("manifest hash differs between roots:\n %s\n %s", first.Report.Hash, second.Report.Hash)
	}
	firstJSON, err := first.Report.JSON()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	secondJSON, err := second.Report.JSON()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if string(firstJSON) != string(secondJSON) {
		t.Error("manifests differ between roots")
	}
}

// TestManifestNamesNoAbsolutePath keeps the report comparable. A manifest that
// mentioned where it ran would differ between two machines that agreed about
// everything that matters.
func TestManifestNamesNoAbsolutePath(t *testing.T) {
	ctx := t.Context()
	root, git := newTemplate(ctx, t, nil)
	result := plan(ctx, t, newOptions(ctx, t, root, git))

	encoded, err := result.Report.JSON()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	for _, rendering := range []string{string(encoded), result.Summary()} {
		if strings.Contains(rendering, root) {
			t.Error("a rendering names the repository directory")
		}
		if strings.Contains(rendering, string(filepath.Separator)+"tmp") {
			t.Error("a rendering names a temporary directory")
		}
	}
}

// TestPlanRefusals covers every repository shape setup will not transform.
func TestPlanRefusals(t *testing.T) {
	tests := []struct {
		name    string
		extra   map[string]string
		arrange func(ctx context.Context, tb testing.TB, root string, git *gitcli.Runner)
		want    error
	}{
		{
			name:    "uncommitted change",
			arrange: func(_ context.Context, tb testing.TB, root string, _ *gitcli.Runner) { touch(tb, root, "CLAUDE.md") },
			want:    setup.ErrDirty,
		},
		{
			name: "untracked file",
			arrange: func(_ context.Context, tb testing.TB, root string, _ *gitcli.Runner) {
				touch(tb, root, "scratch.txt")
			},
			want: setup.ErrDirty,
		},
		{
			name: "payload path the template does not own",
			// A sync workflow in a template checkout means this is not the tree
			// setup thinks it is, so the composed one is not written over it.
			extra: map[string]string{".github/workflows/sync.yml": "name: someone else's sync\n"},
			want:  setup.ErrUnknownOverwrite,
		},
		{
			name:  "paths differing only by case",
			extra: map[string]string{"GO.MOD": "not a module\n"},
			want:  setup.ErrCaseCollision,
		},
		{
			name: "tracked symbolic link",
			arrange: func(ctx context.Context, tb testing.TB, root string, git *gitcli.Runner) {
				if err := os.Symlink("README.md", filepath.Join(root, "LINK.md")); err != nil {
					tb.Fatalf("symlink: %v", err)
				}
				commit(ctx, tb, git, "chore: add a link")
			},
			want: setup.ErrSymlink,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := t.Context()
			root, git := newTemplate(ctx, t, test.extra)
			opts := newOptions(ctx, t, root, git)
			if test.arrange != nil {
				test.arrange(ctx, t, root, git)
			}
			_, err := setup.Plan(ctx, opts)
			if !errors.Is(err, test.want) {
				t.Fatalf("plan error = %v, want %v", err, test.want)
			}
			assertPolicy(t, err)
		})
	}
}

func TestPlanRefusesReservedProfileShapes(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*setup.Options)
		want   string
	}{
		{
			name:   "toolchain differs from engine",
			mutate: func(opts *setup.Options) { opts.Config.Determinism.Toolchain = "go1.26.4" },
			want:   "must match engine toolchain",
		},
		{
			name:   "internal prefix occupies engine shim",
			mutate: func(opts *setup.Options) { opts.Config.Destination.InternalPrefix = "tools/internal" },
			want:   "reserved for the derived repository's engine shim",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := t.Context()
			root, git := newTemplate(ctx, t, nil)
			opts := newOptions(ctx, t, root, git)
			test.mutate(&opts)
			_, err := setup.Plan(ctx, opts)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("plan error = %v, want %q", err, test.want)
			}
		})
	}
}

// TestPlanRefusesANonTemplate covers the marker itself, one missing path at a
// time, because a marker that only fired when everything was missing would
// accept most of the repositories it exists to refuse.
func TestPlanRefusesANonTemplate(t *testing.T) {
	for _, marker := range []string{
		"soapbox.yaml",
		"plans/implementation.md",
		"tools/soapbox.go",
		"tools/internal/cli/cli.go",
		"tools/cmd/soapbox/main.go",
	} {
		t.Run(marker, func(t *testing.T) {
			ctx := t.Context()
			root, git := newTemplate(ctx, t, nil)
			opts := newOptions(ctx, t, root, git)

			if err := os.Remove(filepath.Join(root, filepath.FromSlash(marker))); err != nil {
				t.Fatalf("remove: %v", err)
			}
			commit(ctx, t, git, "chore: remove a marker")

			_, err := setup.Plan(ctx, opts)
			if !errors.Is(err, setup.ErrNotTemplate) {
				t.Fatalf("plan error = %v, want ErrNotTemplate", err)
			}
			if !strings.Contains(err.Error(), marker) {
				t.Errorf("error %q does not name %s", err, marker)
			}
		})
	}
}

// TestPlanRefusesASubdirectory keeps the transformation whole. Setup rewrites a
// repository, and a root that is one directory inside it would delete and
// compose against a tree it only partly measured.
func TestPlanRefusesASubdirectory(t *testing.T) {
	ctx := t.Context()
	root, git := newTemplate(ctx, t, nil)
	opts := newOptions(ctx, t, root, git)
	opts.Root = filepath.Join(root, "tools")

	if _, err := setup.Plan(ctx, opts); err == nil {
		t.Fatal("a subdirectory root was accepted")
	} else if !strings.Contains(err.Error(), "rather than a subdirectory") {
		t.Fatalf("plan error = %v, want a subdirectory refusal", err)
	}
}

// TestCancellationLeavesTheRepositoryAlone covers the context contract on both
// entry points.
func TestCancellationLeavesTheRepositoryAlone(t *testing.T) {
	ctx := t.Context()
	root, git := newTemplate(ctx, t, nil)
	opts := newOptions(ctx, t, root, git)
	planned := plan(ctx, t, opts)

	canceled, cancel := context.WithCancel(ctx)
	cancel()

	if _, err := setup.Plan(canceled, opts); !errors.Is(err, context.Canceled) {
		t.Errorf("plan error = %v, want context.Canceled", err)
	}
	if _, err := setup.Apply(canceled, opts, planned.Report.Hash); !errors.Is(err, context.Canceled) {
		t.Errorf("apply error = %v, want context.Canceled", err)
	}
	assertUnchanged(ctx, t, git)
}

// TestGeneratedModulesPinTheEngine parses what setup wrote with the same module
// file parser the go command uses, so the assertion is about what the toolchain
// will read rather than about the text.
func TestGeneratedModulesPinTheEngine(t *testing.T) {
	ctx := t.Context()
	root, git := newTemplate(ctx, t, nil)
	opts := newOptions(ctx, t, root, git)
	opts.EngineSum = engineSumFor("v1.4.2")
	planned := plan(ctx, t, opts)
	if _, err := setup.Apply(ctx, opts, planned.Report.Hash); err != nil {
		t.Fatalf("apply: %v", err)
	}

	rootMod := parseModule(t, filepath.Join(root, "go.mod"))
	if got := rootMod.Module.Mod.Path; got != "monis.app/kk/rbac_authorizer" {
		t.Errorf("root module = %q", got)
	}
	if len(rootMod.Require) != 0 {
		t.Errorf("root module requires %d modules, want none until a generation decides them", len(rootMod.Require))
	}
	if rootMod.Toolchain == nil || rootMod.Toolchain.Name != "go1.26.5" {
		t.Errorf("root toolchain = %+v", rootMod.Toolchain)
	}

	toolsMod := parseModule(t, filepath.Join(root, "tools", "go.mod"))
	if got := toolsMod.Module.Mod.Path; got != "monis.app/kk/rbac_authorizer/tools" {
		t.Errorf("tools module = %q", got)
	}
	if len(toolsMod.Require) != 1 {
		t.Fatalf("tools module requires %d modules, want exactly the engine", len(toolsMod.Require))
	}
	if got := toolsMod.Require[0].Mod; got.Path != setup.EngineModulePath || got.Version != "v1.4.2" {
		t.Errorf("engine requirement = %s %s, want %s v1.4.2", got.Path, got.Version, setup.EngineModulePath)
	}

	// The shim compiles against the engine's public entry point and nothing else,
	// which is what keeps tool dependencies out of the library's module graph.
	main := readFile(t, filepath.Join(root, "tools", "cmd", "soapbox", "main.go"))
	if !strings.Contains(main, `soapbox "`+setup.EngineModulePath+`"`) {
		t.Error("the shim does not import the engine entry point")
	}
	if strings.Contains(main, "/internal/") {
		t.Error("the shim reaches into the engine's internal packages")
	}

	sum := readFile(t, filepath.Join(root, "tools", "go.sum"))
	if !strings.Contains(sum, setup.EngineModulePath+" v1.4.2 h1:") {
		t.Errorf("tools/go.sum does not cover the pinned release:\n%s", sum)
	}
}

// TestEngineChecksums covers the one input setup cannot derive.
func TestEngineChecksums(t *testing.T) {
	tests := []struct {
		name    string
		sum     []byte
		wantSum bool
		wantErr string
	}{
		{
			name:    "absent",
			wantSum: false,
		},
		{
			name:    "covering the pin",
			sum:     engineSumFor("v1.4.2"),
			wantSum: true,
		},
		{
			name:    "covering another release",
			sum:     engineSumFor("v1.4.1"),
			wantErr: "do not cover the pinned release",
		},
		{
			name:    "missing the module hash",
			sum:     []byte(setup.EngineModulePath + " v1.4.2/go.mod h1:AAAA=\n"),
			wantErr: "do not cover the pinned release",
		},
		{
			name:    "malformed",
			sum:     []byte("github.com/enj/soapbox/tools v1.4.2\n"),
			wantErr: "is not a go.sum line",
		},
		{
			name:    "unhashed",
			sum:     []byte(setup.EngineModulePath + " v1.4.2 sha256:AAAA=\n"),
			wantErr: "does not carry an h1 hash",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := t.Context()
			root, git := newTemplate(ctx, t, nil)
			opts := newOptions(ctx, t, root, git)
			opts.EngineSum = test.sum

			result, err := setup.Plan(ctx, opts)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("plan error = %v, want %q", err, test.wantErr)
				}
				assertPolicy(t, err)
				return
			}
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			written := slices.ContainsFunc(result.Report.Actions, func(a setup.Action) bool {
				return a.Path == "tools/go.sum" && a.Kind != setup.ActionDelete
			})
			if written != test.wantSum {
				t.Errorf("tools/go.sum written = %t, want %t", written, test.wantSum)
			}
			// The template's own engine checksums never survive into a derived
			// repository: either they are replaced by ones covering the pin, or
			// they are removed with the rest of the engine.
			removed := slices.Contains(actionPaths(result, setup.ActionDelete), "tools/go.sum")
			if removed == test.wantSum {
				t.Errorf("tools/go.sum removed = %t, want %t", removed, !test.wantSum)
			}
			if result.Report.Engine.Sum != test.wantSum {
				t.Errorf("report records checksums = %t, want %t", result.Report.Engine.Sum, test.wantSum)
			}
			// An absent go.sum is reported rather than left for the first failing
			// build to explain.
			hasNotice := slices.ContainsFunc(result.Report.Notices, func(n string) bool {
				return strings.Contains(n, "tools/go.sum was not written")
			})
			if hasNotice == test.wantSum {
				t.Errorf("notice about the missing checksums = %t, want %t", hasNotice, !test.wantSum)
			}
		})
	}
}

// TestSetupTouchesNoGitState is the local-only contract. Setup transforms a work
// tree; it does not commit, tag, branch, or learn about a remote.
func TestSetupTouchesNoGitState(t *testing.T) {
	ctx := t.Context()
	root, git := newTemplate(ctx, t, nil)
	opts := newOptions(ctx, t, root, git)

	before := refState(ctx, t, git)
	planned := plan(ctx, t, opts)
	if _, err := setup.Apply(ctx, opts, planned.Report.Hash); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if after := refState(ctx, t, git); after != before {
		t.Errorf("refs changed:\nbefore %q\nafter  %q", before, after)
	}

	// A transformation that wrote through git rather than through the file system
	// would leave the change staged. Everything setup did is in the work tree,
	// which is what lets the operator review it with git before committing.
	entries, err := git.Status(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("setup left no work tree changes at all")
	}
	for _, entry := range entries {
		if entry.Code[0] != ' ' && entry.Code != "??" {
			t.Errorf("setup staged %s (%q), which the operator did not ask it to", entry.Path, entry.Code)
		}
	}
	if _, err := os.Lstat(filepath.Join(root, ".git")); err != nil {
		t.Errorf(".git should be untouched: %v", err)
	}

	// No staging directory survives beside the repository.
	siblings, err := os.ReadDir(filepath.Dir(root))
	if err != nil {
		t.Fatalf("read parent: %v", err)
	}
	for _, sibling := range siblings {
		if strings.HasPrefix(sibling.Name(), ".soapbox-setup-") {
			t.Errorf("a staging directory survived: %s", sibling.Name())
		}
	}
}

// commit records every work tree change under the fixture identity.
func commit(ctx context.Context, tb testing.TB, git *gitcli.Runner, message string) {
	tb.Helper()
	if err := git.AddPaths(ctx, "."); err != nil {
		tb.Fatalf("stage: %v", err)
	}
	if err := git.Commit(ctx, gitcli.CommitOptions{Message: message}); err != nil {
		tb.Fatalf("commit: %v", err)
	}
}

// touch writes a file without committing it, which is how the tests produce a
// work tree setup refuses.
func touch(tb testing.TB, root, name string) {
	tb.Helper()
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(name)), []byte("changed\n"), 0o600); err != nil {
		tb.Fatalf("write %s: %v", name, err)
	}
}

// mustOptions builds a fresh template in a fresh directory.
func mustOptions(ctx context.Context, tb testing.TB) setup.Options {
	tb.Helper()
	root, git := newTemplate(ctx, tb, nil)
	return newOptions(ctx, tb, root, git)
}

// assertUnchanged proves the repository was not written to.
func assertUnchanged(ctx context.Context, tb testing.TB, git *gitcli.Runner) {
	tb.Helper()
	entries, err := git.Status(ctx)
	if err != nil {
		tb.Fatalf("status: %v", err)
	}
	if len(entries) > 0 {
		tb.Errorf("the repository changed: %v", entries)
	}
}

// assertPolicy proves a refusal is one the operator can act on rather than a
// runtime failure, which is what decides the process exit code.
func assertPolicy(tb testing.TB, err error) {
	tb.Helper()
	var policy *setup.PolicyError
	if !errors.As(err, &policy) {
		tb.Errorf("error %v is not a PolicyError", err)
	}
}

// refState renders every ref the repository holds.
func refState(ctx context.Context, tb testing.TB, git *gitcli.Runner) string {
	tb.Helper()
	refs, err := git.ListRefs(ctx)
	if err != nil {
		tb.Fatalf("list refs: %v", err)
	}
	rendered := make([]string, 0, len(refs))
	for _, ref := range refs {
		rendered = append(rendered, ref.Name+" "+ref.Target)
	}
	slices.Sort(rendered)
	return strings.Join(rendered, "\n")
}

// parseModule reads a generated go.mod through the module file parser.
func parseModule(tb testing.TB, path string) *modfile.File {
	tb.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // the path is a test temporary directory
	if err != nil {
		tb.Fatalf("read %s: %v", path, err)
	}
	parsed, err := modfile.Parse(path, data, nil)
	if err != nil {
		tb.Fatalf("parse %s: %v", path, err)
	}
	return parsed
}

// readFile reads one generated file.
func readFile(tb testing.TB, path string) string {
	tb.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // the path is a test temporary directory
	if err != nil {
		tb.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}
