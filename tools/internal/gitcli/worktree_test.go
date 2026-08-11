package gitcli_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/testsupport"
)

// nestedUpstream is a repository whose package directory has a subpackage, which
// is the shape that tells package granular materialization apart from recursive
// directory copying.
func nestedUpstream(t *testing.T) (*testsupport.Repo, string) {
	t.Helper()
	ctx := t.Context()

	repo := testsupport.NewRepo(ctx, t, testsupport.Options{
		Branch:    mainBranch,
		UserName:  testUserName,
		UserEmail: testUserEmail,
	})
	repo.SetConfig(ctx, t, "uploadpack.allowFilter", "true")
	repo.WriteFile(t, "pkg/apis/rbac/v1/doc.go", "package v1\n")
	repo.WriteFile(t, "pkg/apis/rbac/v1/helpers.go", "package v1\n")
	repo.WriteFile(t, "pkg/apis/rbac/v1/nested/deep.go", "package nested\n")
	repo.WriteFile(t, "plugin/pkg/auth/authorizer/rbac/rbac.go", "package rbac\n")
	repo.WriteFile(t, "unrelated/other.go", "package other\n")
	commit := repo.Commit(ctx, t, "feat: seed packages\n", gitcli.CommitOptions{},
		"pkg/apis/rbac/v1/doc.go",
		"pkg/apis/rbac/v1/helpers.go",
		"pkg/apis/rbac/v1/nested/deep.go",
		"plugin/pkg/auth/authorizer/rbac/rbac.go",
		"unrelated/other.go",
	)
	return repo, commit
}

// bareCacheOf clones a fixture into a bare blobless cache, which is what work
// trees are materialized from in production.
func bareCacheOf(t *testing.T, remote string) *gitcli.Runner {
	t.Helper()
	root := t.TempDir()
	runner := newAnonymousRunner(t, root)
	dir := filepath.Join(root, "cache.git")
	if err := runner.CloneSource(t.Context(), gitcli.SourceCloneOptions{
		Remote:    "file://" + remote,
		Directory: dir,
		Bare:      true,
	}); err != nil {
		t.Fatalf("clone cache: %v", err)
	}
	cache, err := runner.WithDir(dir)
	if err != nil {
		t.Fatalf("scope runner to the cache: %v", err)
	}
	return cache
}

// materializedPaths lists the repository relative files present in a work tree.
func materializedPaths(t *testing.T, root string) []string {
	t.Helper()
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == ".git" {
			return nil
		}
		paths = append(paths, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("walk work tree: %v", err)
	}
	slices.Sort(paths)
	return paths
}

func TestSparseWorktreeMaterialization(t *testing.T) {
	ctx := t.Context()
	repo, commit := nestedUpstream(t)
	cache := bareCacheOf(t, repo.Dir)
	worktreeRoot := t.TempDir()

	tests := []struct {
		name     string
		patterns []string
		cone     bool
		want     []string
	}{
		{
			// Package granularity: the directory's own files and nothing below
			// it. Without the negative pattern the subpackage comes along.
			name:     "package granular",
			patterns: []string{"/pkg/apis/rbac/v1/*", "!/pkg/apis/rbac/v1/*/"},
			want:     []string{"pkg/apis/rbac/v1/doc.go", "pkg/apis/rbac/v1/helpers.go"},
		},
		{
			name:     "recursive directory",
			patterns: []string{"/pkg/apis/rbac/v1/"},
			want: []string{
				"pkg/apis/rbac/v1/doc.go",
				"pkg/apis/rbac/v1/helpers.go",
				"pkg/apis/rbac/v1/nested/deep.go",
			},
		},
		{
			name:     "two roots",
			patterns: []string{"/pkg/apis/rbac/v1/*", "!/pkg/apis/rbac/v1/*/", "/plugin/pkg/auth/authorizer/rbac/*", "!/plugin/pkg/auth/authorizer/rbac/*/"},
			want: []string{
				"pkg/apis/rbac/v1/doc.go",
				"pkg/apis/rbac/v1/helpers.go",
				"plugin/pkg/auth/authorizer/rbac/rbac.go",
			},
		},
		{
			// Cone mode always includes subdirectories, which is why it cannot
			// express package granularity.
			name:     "cone mode includes subdirectories",
			patterns: []string{"pkg/apis/rbac/v1"},
			cone:     true,
			want: []string{
				"pkg/apis/rbac/v1/doc.go",
				"pkg/apis/rbac/v1/helpers.go",
				"pkg/apis/rbac/v1/nested/deep.go",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := filepath.Join(worktreeRoot, strings.ReplaceAll(test.name, " ", "-"))
			if err := cache.AddWorktree(ctx, gitcli.WorktreeOptions{
				Path:       dir,
				Commit:     commit,
				NoCheckout: true,
			}); err != nil {
				t.Fatalf("add worktree: %v", err)
			}
			t.Cleanup(func() {
				// The subtest context is already cancelled by the time cleanup
				// runs, and cleanup still has to reach git.
				if err := cache.RemoveWorktree(context.WithoutCancel(ctx), dir); err != nil {
					t.Errorf("remove worktree: %v", err)
				}
			})

			worktree, err := cache.WithDir(dir)
			if err != nil {
				t.Fatalf("scope runner to the work tree: %v", err)
			}
			// Nothing is materialized until the patterns are installed, which is
			// what keeps a blobless clone from fetching every blob in the tree.
			if paths := materializedPaths(t, dir); len(paths) != 0 {
				t.Fatalf("work tree created with files %v", paths)
			}
			if err := worktree.SetSparseCheckout(ctx, gitcli.SparseOptions{Cone: test.cone, Patterns: test.patterns}); err != nil {
				t.Fatalf("set sparse checkout: %v", err)
			}
			if err := worktree.CheckoutDetached(ctx, commit); err != nil {
				t.Fatalf("checkout: %v", err)
			}

			if got := materializedPaths(t, dir); !slices.Equal(got, test.want) {
				t.Fatalf("materialized %v, want %v", got, test.want)
			}

			// Cone mode rewrites the patterns it was given, so the round trip is
			// only asserted for the pattern form the engine relies on.
			if !test.cone {
				installed, err := worktree.SparseCheckoutPatterns(ctx)
				if err != nil {
					t.Fatalf("read sparse patterns: %v", err)
				}
				if !slices.Equal(installed, test.patterns) {
					t.Fatalf("installed patterns %v, want %v", installed, test.patterns)
				}
			}
		})
	}
}

// TestWorktreeCheckoutIsDetached proves a materialization cannot move a ref in
// the shared cache.
func TestWorktreeCheckoutIsDetached(t *testing.T) {
	ctx := t.Context()
	repo, commit := nestedUpstream(t)
	cache := bareCacheOf(t, repo.Dir)
	dir := filepath.Join(t.TempDir(), "wt")

	if err := cache.AddWorktree(ctx, gitcli.WorktreeOptions{Path: dir, Commit: commit, NoCheckout: true}); err != nil {
		t.Fatalf("add worktree: %v", err)
	}

	worktrees, err := cache.ListWorktrees(ctx)
	if err != nil {
		t.Fatalf("list worktrees: %v", err)
	}
	var found bool
	for _, worktree := range worktrees {
		if worktree.Bare {
			continue
		}
		found = true
		if !worktree.Detached {
			t.Fatalf("work tree %q is on branch %q", worktree.Path, worktree.Branch)
		}
		if worktree.Head != commit {
			t.Fatalf("work tree head %q, want %q", worktree.Head, commit)
		}
	}
	if !found {
		t.Fatalf("no work tree was registered in %v", worktrees)
	}
}

// TestRemoveWorktreeIsIdempotent covers cleanup after a failure: a deferred
// removal must not turn into a second error that hides the first.
func TestRemoveWorktreeIsIdempotent(t *testing.T) {
	ctx := t.Context()
	repo, commit := nestedUpstream(t)
	cache := bareCacheOf(t, repo.Dir)
	dir := filepath.Join(t.TempDir(), "wt")

	if err := cache.AddWorktree(ctx, gitcli.WorktreeOptions{Path: dir, Commit: commit}); err != nil {
		t.Fatalf("add worktree: %v", err)
	}
	if err := cache.RemoveWorktree(ctx, dir); err != nil {
		t.Fatalf("remove worktree: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("work tree directory survived removal: %v", err)
	}
	if err := cache.RemoveWorktree(ctx, dir); err != nil {
		t.Fatalf("second removal: %v", err)
	}
	if err := cache.RemoveWorktree(ctx, filepath.Join(t.TempDir(), "never-created")); err != nil {
		t.Fatalf("removing an unregistered work tree: %v", err)
	}
	if err := cache.PruneWorktrees(ctx); err != nil {
		t.Fatalf("prune: %v", err)
	}
}

func TestWorktreeAndSparseValidation(t *testing.T) {
	ctx := t.Context()
	repo, commit := nestedUpstream(t)
	cache := bareCacheOf(t, repo.Dir)

	tests := []struct {
		name string
		run  func() error
		want string
	}{
		{
			name: "relative worktree path",
			run: func() error {
				return cache.AddWorktree(ctx, gitcli.WorktreeOptions{Path: "relative/wt", Commit: commit})
			},
			want: "must be absolute",
		},
		{
			name: "flag like worktree path",
			run: func() error {
				return cache.AddWorktree(ctx, gitcli.WorktreeOptions{Path: "--force", Commit: commit})
			},
			want: "must not start with a dash",
		},
		{
			name: "no sparse pattern",
			run:  func() error { return cache.SetSparseCheckout(ctx, gitcli.SparseOptions{}) },
			want: "at least one pattern",
		},
		{
			name: "flag like sparse pattern",
			run: func() error {
				return cache.SetSparseCheckout(ctx, gitcli.SparseOptions{Patterns: []string{"--stdin"}})
			},
			want: "must not start with a dash",
		},
		{
			// The pattern file is line based, so a pattern carrying a newline
			// would silently become two patterns.
			name: "pattern with a line break",
			run: func() error {
				return cache.SetSparseCheckout(ctx, gitcli.SparseOptions{Patterns: []string{"/pkg/*\n/etc/*"}})
			},
			want: "must not contain a line break",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.run()
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error %q does not mention %q", err, test.want)
			}
		})
	}
}

func TestStatusAndDiff(t *testing.T) {
	ctx := t.Context()
	repo := testsupport.NewRepo(ctx, t, testsupport.Options{
		UserName:  testUserName,
		UserEmail: testUserEmail,
	})
	repo.WriteAndCommit(ctx, t, "rule.go", "package validation\n", "feat: add rule\n")
	repo.WriteFile(t, "rule.go", "package validation\n\nconst Changed = true\n")
	repo.WriteFile(t, "extra.go", "package validation\n")

	entries, err := repo.Git.Status(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	byPath := make(map[string]gitcli.StatusEntry, len(entries))
	for _, entry := range entries {
		byPath[entry.Path] = entry
	}
	if got := byPath["rule.go"].Code; got != " M" {
		t.Fatalf("rule.go status %q, want a modification", got)
	}
	if got := byPath["extra.go"].Code; got != "??" {
		t.Fatalf("extra.go status %q, want untracked", got)
	}
	if byPath["rule.go"].Conflicted() {
		t.Fatal("a modified file is not a conflict")
	}

	diff, err := repo.Git.Diff(ctx, gitcli.DiffOptions{})
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if !strings.Contains(diff, "+const Changed = true") {
		t.Fatalf("diff does not carry the change:\n%s", diff)
	}
	// Limiting to a path that did not change proves the pathspec is applied
	// literally rather than as a pattern.
	scoped, err := repo.Git.Diff(ctx, gitcli.DiffOptions{Paths: []string{"extra.go"}})
	if err != nil {
		t.Fatalf("scoped diff: %v", err)
	}
	if scoped != "" {
		t.Fatalf("scoped diff is not empty:\n%s", scoped)
	}
	if _, err := repo.Git.Diff(ctx, gitcli.DiffOptions{Paths: []string{"*.go"}}); err == nil {
		t.Fatal("expected an error for a pattern pathspec")
	}
}

func TestApplyPatch(t *testing.T) {
	ctx := t.Context()
	repo := testsupport.NewRepo(ctx, t, testsupport.Options{
		UserName:  testUserName,
		UserEmail: testUserEmail,
	})
	repo.WriteAndCommit(ctx, t, "rule.go", "package validation\n\nfunc Rule() {}\n", "feat: add rule\n")

	patch := []byte(`diff --git a/rule.go b/rule.go
--- a/rule.go
+++ b/rule.go
@@ -1,3 +1,3 @@
 package validation

-func Rule() {}
+func Rule() bool { return true }
`)

	// A check never touches the work tree.
	if err := repo.Git.ApplyPatch(ctx, gitcli.ApplyOptions{Patch: patch, Check: true}); err != nil {
		t.Fatalf("check patch: %v", err)
	}
	contents, err := os.ReadFile(filepath.Join(repo.Dir, "rule.go"))
	if err != nil {
		t.Fatalf("read rule.go: %v", err)
	}
	if strings.Contains(string(contents), "return true") {
		t.Fatal("a checked patch must not be applied")
	}

	if err := repo.Git.ApplyPatch(ctx, gitcli.ApplyOptions{Patch: patch, ThreeWay: true, Index: true}); err != nil {
		t.Fatalf("apply patch: %v", err)
	}
	contents, err = os.ReadFile(filepath.Join(repo.Dir, "rule.go"))
	if err != nil {
		t.Fatalf("read rule.go: %v", err)
	}
	if !strings.Contains(string(contents), "return true") {
		t.Fatalf("patch was not applied:\n%s", contents)
	}

	if err := repo.Git.ApplyPatch(ctx, gitcli.ApplyOptions{}); err == nil {
		t.Fatal("expected an error for an empty patch")
	}
	if err := repo.Git.ApplyPatch(ctx, gitcli.ApplyOptions{Patch: patch, Strip: -1}); err == nil {
		t.Fatal("expected an error for a negative strip count")
	}
}

// TestApplyPatchConflictIsReportable covers the failure the conflict runbook is
// written for: a three way application that cannot merge must fail, leave the
// markers behind, and let the run capture them.
func TestApplyPatchConflictIsReportable(t *testing.T) {
	ctx := t.Context()
	repo := testsupport.NewRepo(ctx, t, testsupport.Options{
		UserName:  testUserName,
		UserEmail: testUserEmail,
	})
	repo.WriteAndCommit(ctx, t, "rule.go", "package validation\n\nfunc Rule() {}\n", "feat: add rule\n")

	// The patch was written against a line the upstream file no longer has, and
	// the blob it names is not in this repository, so no merge base exists.
	patch := []byte(`diff --git a/rule.go b/rule.go
index 1111111111111111111111111111111111111111..2222222222222222222222222222222222222222 100644
--- a/rule.go
+++ b/rule.go
@@ -1,3 +1,3 @@
 package validation

-func Rule() int { return 7 }
+func Rule() int { return 8 }
`)

	err := repo.Git.ApplyPatch(ctx, gitcli.ApplyOptions{Patch: patch, ThreeWay: true, Index: true})
	if err == nil {
		t.Fatal("expected the patch to fail")
	}
	if !strings.Contains(err.Error(), "git apply") {
		t.Fatalf("error %q is not a patch failure", err)
	}

	// The run still has to be able to describe what happened.
	if _, err := repo.Git.Status(ctx); err != nil {
		t.Fatalf("status after a failed patch: %v", err)
	}
	if _, err := repo.Git.Diff(ctx, gitcli.DiffOptions{}); err != nil {
		t.Fatalf("diff after a failed patch: %v", err)
	}
}

// TestApplyThreeWayIndexAppliesBothSemantics proves the narrow entry point is
// not merely a rename: the same patch that a strict apply rejects after upstream
// drift succeeds here, and it reaches both the work tree and the index.
func TestApplyThreeWayIndexAppliesBothSemantics(t *testing.T) {
	ctx := t.Context()
	repo := testsupport.NewRepo(ctx, t, testsupport.Options{
		UserName:  testUserName,
		UserEmail: testUserEmail,
	})
	const original = "package validation\n\nfunc Rule() {}\n"
	repo.WriteAndCommit(ctx, t, "rule.go", original, "feat: add rule\n")

	// The patch is captured from git itself, which is where a maintained patch
	// series comes from. Its index line names the pre-image blob, and that blob
	// is what gives a three way apply a base to merge from.
	repo.WriteFile(t, "rule.go", "package validation\n\nfunc Rule() bool { return true }\n")
	patch, err := repo.Git.DiffWorkTree(ctx)
	if err != nil {
		t.Fatalf("capture patch: %v", err)
	}
	if !strings.Contains(patch, "index ") {
		t.Fatalf("captured patch has no index line:\n%s", patch)
	}
	if err := repo.Git.ResetHard(ctx, "HEAD"); err != nil {
		t.Fatalf("reset: %v", err)
	}

	// Upstream drift rewrites a context line the hunk depends on.
	repo.WriteAndCommit(ctx, t, "rule.go",
		"package validation // drifted\n\nfunc Rule() {}\n", "refactor: annotate package\n")

	// A strict apply cannot reconcile that, which is the whole reason the patch
	// phase never offers one.
	if err := repo.Git.ApplyPatch(ctx, gitcli.ApplyOptions{Patch: []byte(patch)}); err == nil {
		t.Fatal("expected a strict apply to reject the drifted context")
	}

	if err := repo.Git.ApplyThreeWayIndex(ctx, []byte(patch)); err != nil {
		t.Fatalf("apply three way: %v", err)
	}
	contents, err := os.ReadFile(filepath.Join(repo.Dir, "rule.go"))
	if err != nil {
		t.Fatalf("read rule.go: %v", err)
	}
	if !strings.Contains(string(contents), "return true") {
		t.Fatalf("the patch did not reach the work tree:\n%s", contents)
	}
	if !strings.Contains(string(contents), "// drifted") {
		t.Fatalf("the three way apply discarded the drifted context:\n%s", contents)
	}

	// The index was updated too, so the change is already staged for the tree
	// building that follows.
	staged, err := repo.Git.Diff(ctx, gitcli.DiffOptions{Staged: true})
	if err != nil {
		t.Fatalf("staged diff: %v", err)
	}
	if !strings.Contains(staged, "+func Rule() bool { return true }") {
		t.Fatalf("the patch did not reach the index:\n%s", staged)
	}

	if err := repo.Git.ApplyThreeWayIndex(ctx, nil); err == nil {
		t.Fatal("expected an error for an empty patch")
	}
}

// TestStatusPorcelainZAndDiffWorkTree covers the raw forms a conflict report
// records, and pins the record shape a caller parses.
func TestStatusPorcelainZAndDiffWorkTree(t *testing.T) {
	ctx := t.Context()
	repo := testsupport.NewRepo(ctx, t, testsupport.Options{
		UserName:  testUserName,
		UserEmail: testUserEmail,
	})
	repo.WriteAndCommit(ctx, t, "rule.go", "package validation\n", "feat: add rule\n")

	clean, err := repo.Git.StatusPorcelainZ(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if clean != "" {
		t.Fatalf("a clean work tree reported %q", clean)
	}
	emptyDiff, err := repo.Git.DiffWorkTree(ctx)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if emptyDiff != "" {
		t.Fatalf("a clean work tree reported a diff:\n%s", emptyDiff)
	}

	repo.WriteFile(t, "rule.go", "package validation\n\nconst Changed = true\n")
	repo.WriteFile(t, "extra.go", "package validation\n")

	raw, err := repo.Git.StatusPorcelainZ(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	// Records are null terminated: two characters of code, a space, then exactly
	// one path. The parsed form has to agree with the raw form it is built from.
	records := strings.Split(strings.TrimSuffix(raw, "\x00"), "\x00")
	slices.Sort(records)
	if want := []string{" M rule.go", "?? extra.go"}; !slices.Equal(records, want) {
		t.Fatalf("records %q, want %q", records, want)
	}
	entries, err := repo.Git.Status(ctx)
	if err != nil {
		t.Fatalf("parsed status: %v", err)
	}
	if len(entries) != len(records) {
		t.Fatalf("parsed %d entries from %d records", len(entries), len(records))
	}

	diff, err := repo.Git.DiffWorkTree(ctx)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if !strings.Contains(diff, "+const Changed = true") {
		t.Fatalf("diff does not carry the change:\n%s", diff)
	}
}

func TestResetHardAndClean(t *testing.T) {
	ctx := t.Context()
	repo := testsupport.NewRepo(ctx, t, testsupport.Options{
		UserName:  testUserName,
		UserEmail: testUserEmail,
	})
	repo.WriteFile(t, ".gitignore", "ignored/\n")
	first := repo.Commit(ctx, t, "chore: ignore generated output\n", gitcli.CommitOptions{}, ".gitignore")
	repo.WriteAndCommit(ctx, t, "rule.go", "package validation\n", "feat: add rule\n")

	repo.WriteFile(t, "rule.go", "package validation\n\nbroken\n")
	repo.WriteFile(t, "untracked.go", "package validation\n")
	repo.WriteFile(t, "ignored/output.txt", "generated\n")

	if err := repo.Git.ResetHard(ctx, "HEAD"); err != nil {
		t.Fatalf("reset: %v", err)
	}
	contents, err := os.ReadFile(filepath.Join(repo.Dir, "rule.go"))
	if err != nil {
		t.Fatalf("read rule.go: %v", err)
	}
	if strings.Contains(string(contents), "broken") {
		t.Fatal("reset did not restore the tracked file")
	}

	if err := repo.Git.Clean(ctx); err != nil {
		t.Fatalf("clean: %v", err)
	}
	// Ignored output is removed too, because a materialized work tree must hold
	// exactly what the run put there.
	for _, path := range []string{"untracked.go", "ignored/output.txt"} {
		if _, err := os.Stat(filepath.Join(repo.Dir, filepath.FromSlash(path))); !os.IsNotExist(err) {
			t.Fatalf("%s survived the clean: %v", path, err)
		}
	}

	if err := repo.Git.ResetHard(ctx, first); err != nil {
		t.Fatalf("reset to the first commit: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo.Dir, "rule.go")); !os.IsNotExist(err) {
		t.Fatalf("rule.go survived a reset to before it existed: %v", err)
	}
}
