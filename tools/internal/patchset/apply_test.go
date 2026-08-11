package patchset_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/patchset"
)

const (
	baseContents     = "line1\nline2\nline3\n"
	upstreamContents = "line1\nupstream\nline3\n"
	patchedContents  = "line1\npatched\nline3\n"
)

// createPatch adds a new file, the shape a patch that exports an internal
// helper into its own file takes.
func createPatch(id, path, contents string) patchset.Patch {
	lines := strings.Count(contents, "\n")
	diff := fmt.Sprintf(""+
		"diff --git a/%[1]s b/%[1]s\n"+
		"new file mode 100644\n"+
		"--- /dev/null\n"+
		"+++ b/%[1]s\n"+
		"@@ -0,0 +1,%[2]d @@\n", path, lines)
	for line := range strings.SplitSeq(strings.TrimSuffix(contents, "\n"), "\n") {
		diff += "+" + line + "\n"
	}
	return patchset.Patch{ID: id, Diff: []byte(diff)}
}

// replacePatch rewrites the middle line of a three line file and records the
// preimage blob, which is what lets Git fall back to a three way merge when the
// file has moved upstream.
func replacePatch(id, path, from, to string) patchset.Patch {
	diff := fmt.Sprintf(""+
		"diff --git a/%[1]s b/%[1]s\n"+
		"index %[2]s..%[3]s 100644\n"+
		"--- a/%[1]s\n"+
		"+++ b/%[1]s\n"+
		"@@ -1,3 +1,3 @@\n"+
		" line1\n"+
		"-%[4]s\n"+
		"+%[5]s\n"+
		" line3\n",
		path, blobSHA(from), blobSHA(to),
		strings.Split(strings.TrimSuffix(from, "\n"), "\n")[1],
		strings.Split(strings.TrimSuffix(to, "\n"), "\n")[1])
	return patchset.Patch{ID: id, Diff: []byte(diff)}
}

// readFile reports the work tree contents of a repository relative path.
func readFile(t *testing.T, repo *gitRepo, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repo.Dir, filepath.FromSlash(rel)))
	if err != nil {
		return ""
	}
	return string(data)
}

func TestApplyAppliesSeriesInOrder(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	repo := newGitRepo(ctx, t)
	repo.WriteAndCommit(ctx, t, "f.txt", baseContents, "base")

	result, err := patchset.Apply(ctx, repo.Git, patchset.Request{
		SourceRef: "refs/heads/master",
		SourceSHA: "0123456789abcdef0123456789abcdef01234567",
		Patches: []patchset.Patch{
			createPatch("patches/0001-add.patch", "added.txt", "first\n"),
			replacePatch("patches/0002-replace.patch", "f.txt", baseContents, patchedContents),
		},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}

	want := []string{"patches/0001-add.patch", "patches/0002-replace.patch"}
	if got := result.PatchIDs(); !slices.Equal(got, want) {
		t.Errorf("applied %v, want %v", got, want)
	}
	if got := readFile(t, repo, "added.txt"); got != "first\n" {
		t.Errorf("added.txt is %q, want %q", got, "first\n")
	}
	if got := readFile(t, repo, "f.txt"); got != patchedContents {
		t.Errorf("f.txt is %q, want %q", got, patchedContents)
	}

	// --index semantics: every change is recorded in the index, so the work tree
	// column is clean and nothing is left untracked. Without it the pruning and
	// tree building steps that follow would not see the patched files.
	entries := repo.status(ctx, t)
	if len(entries) != 2 {
		t.Fatalf("status reported %d entries, want 2: %v", len(entries), entries)
	}
	for _, entry := range entries {
		if entry.Code[0] == ' ' || entry.Code[0] == '?' {
			t.Errorf("entry %q is not staged, so the apply did not use --index", entry.Code+" "+entry.Path)
		}
		if entry.Code[1] != ' ' {
			t.Errorf("entry %q has unstaged residue", entry.Code+" "+entry.Path)
		}
	}
}

// TestApplyIsAllOrNothing asserts that a failure anywhere in the series leaves
// the tree exactly as the caller handed it over. A half patched tree matches no
// profile and would be published as if it did.
func TestApplyIsAllOrNothing(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	repo := newGitRepo(ctx, t)
	repo.WriteAndCommit(ctx, t, "f.txt", upstreamContents, "upstream")

	_, err := patchset.Apply(ctx, repo.Git, patchset.Request{
		SourceRef: "refs/heads/master",
		SourceSHA: "0123456789abcdef0123456789abcdef01234567",
		Patches: []patchset.Patch{
			createPatch("patches/0001-add.patch", "added.txt", "first\n"),
			// The preimage blob was never committed here, so Git can neither
			// apply the hunk directly nor fall back to a three way merge.
			replacePatch("patches/0002-replace.patch", "f.txt", baseContents, patchedContents),
		},
	})

	var conflict *patchset.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("apply error %v, want a *patchset.ConflictError", err)
	}
	if conflict.PatchID != "patches/0002-replace.patch" || conflict.PatchIndex != 1 {
		t.Errorf("conflict names patch %q at index %d, want the second patch", conflict.PatchID, conflict.PatchIndex)
	}
	if conflict.Stage != patchset.StageApply {
		t.Errorf("conflict stage %q, want %q", conflict.Stage, patchset.StageApply)
	}
	if got := readFile(t, repo, "added.txt"); got != "" {
		t.Errorf("added.txt survived the rollback with %q", got)
	}
	if got := readFile(t, repo, "f.txt"); got != upstreamContents {
		t.Errorf("f.txt is %q, want the unpatched %q", got, upstreamContents)
	}

	if entries := repo.status(ctx, t); len(entries) != 0 {
		t.Errorf("work tree is not clean after rollback: %v", entries)
	}
}

// TestApplyReportsThreeWayConflict drives a real three way merge: the patch
// edits the same line upstream moved, and the preimage blob is present, so Git
// merges instead of refusing and leaves markers a maintainer can resolve.
func TestApplyReportsThreeWayConflict(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	repo := newGitRepo(ctx, t)
	repo.WriteAndCommit(ctx, t, "f.txt", baseContents, "base")
	repo.WriteAndCommit(ctx, t, "f.txt", upstreamContents, "upstream moved the line")

	_, err := patchset.Apply(ctx, repo.Git, patchset.Request{
		SourceRef: "refs/heads/release-1.36",
		SourceSHA: "89abcdef0123456789abcdef0123456789abcdef",
		Patches:   []patchset.Patch{replacePatch("patches/0001-replace.patch", "f.txt", baseContents, patchedContents)},
	})

	var conflict *patchset.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("apply error %v, want a *patchset.ConflictError", err)
	}
	if want := []string{"f.txt"}; !slices.Equal(conflict.ConflictedPaths, want) {
		t.Errorf("conflicted paths %v, want %v", conflict.ConflictedPaths, want)
	}
	if !strings.Contains(conflict.Diff, "<<<<<<<") || !strings.Contains(conflict.Diff, ">>>>>>>") {
		t.Errorf("diff carries no conflict markers:\n%s", conflict.Diff)
	}
	if !strings.Contains(conflict.Diff, "patched") || !strings.Contains(conflict.Diff, "upstream") {
		t.Errorf("diff does not show both sides of the conflict:\n%s", conflict.Diff)
	}
	if len(conflict.Status) == 0 {
		t.Error("conflict carries no status")
	}

	report := conflict.Report()
	for _, want := range []string{"refs/heads/release-1.36", "89abcdef0123456789abcdef0123456789abcdef", "patches/0001-replace.patch", "f.txt", "<<<<<<<"} {
		if !strings.Contains(report, want) {
			t.Errorf("report does not mention %q:\n%s", want, report)
		}
	}

	// The rollback must clear the unmerged index entries as well as the work
	// tree, otherwise the next ref transaction starts from a conflicted index.
	if got := readFile(t, repo, "f.txt"); got != upstreamContents {
		t.Errorf("f.txt is %q, want the unpatched %q", got, upstreamContents)
	}
	if entries := repo.status(ctx, t); len(entries) != 0 {
		t.Errorf("work tree is not clean after rollback: %v", entries)
	}
}

func TestApplyReassertsPruneAfterEveryPatch(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	repo := newGitRepo(ctx, t)
	repo.WriteAndCommit(ctx, t, "f.txt", baseContents, "base")

	var seen []string
	result, err := patchset.Apply(ctx, repo.Git, patchset.Request{
		SourceRef: "refs/heads/master",
		SourceSHA: "0123456789abcdef0123456789abcdef01234567",
		Patches: []patchset.Patch{
			createPatch("patches/0001-add.patch", "added.txt", "first\n"),
			createPatch("patches/0002-add.patch", "second.txt", "second\n"),
		},
		ReassertPrune: func(_ context.Context, patch patchset.Patch) error {
			seen = append(seen, patch.ID)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if want := []string{"patches/0001-add.patch", "patches/0002-add.patch"}; !slices.Equal(seen, want) {
		t.Errorf("prune reasserted after %v, want after %v", seen, want)
	}
	if got := result.PatchIDs(); !slices.Equal(got, seen) {
		t.Errorf("applied %v, want %v", got, seen)
	}
}

// TestApplyRollsBackWhenPruneRejects covers the profile bug the plan calls out:
// a patch that reintroduces a pruned file. Pruning is the caller's concern, so
// it is reported through the callback, but the transaction must fail exactly as
// a conflict does.
func TestApplyRollsBackWhenPruneRejects(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	repo := newGitRepo(ctx, t)
	repo.WriteAndCommit(ctx, t, "f.txt", baseContents, "base")

	pruned := errors.New("patch reintroduced pruned file pkg/apis/rbac/v1/register.go")
	_, err := patchset.Apply(ctx, repo.Git, patchset.Request{
		SourceRef: "refs/heads/master",
		SourceSHA: "0123456789abcdef0123456789abcdef01234567",
		Patches:   []patchset.Patch{createPatch("patches/0001-add.patch", "added.txt", "first\n")},
		ReassertPrune: func(context.Context, patchset.Patch) error {
			return pruned
		},
	})

	var conflict *patchset.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("apply error %v, want a *patchset.ConflictError", err)
	}
	if conflict.Stage != patchset.StagePrune {
		t.Errorf("conflict stage %q, want %q", conflict.Stage, patchset.StagePrune)
	}
	if !errors.Is(err, pruned) {
		t.Errorf("conflict does not wrap the prune failure: %v", err)
	}
	if got := readFile(t, repo, "added.txt"); got != "" {
		t.Errorf("added.txt survived the rollback with %q", got)
	}
}

func TestApplyEmptySeriesIsANoOp(t *testing.T) {
	t.Parallel()

	result, err := patchset.Apply(t.Context(), newDAG(), patchset.Request{
		SourceRef: "refs/heads/master",
		SourceSHA: "0123456789abcdef0123456789abcdef01234567",
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(result.Applied) != 0 {
		t.Errorf("applied %v, want nothing", result.PatchIDs())
	}
}

func TestApplyRejects(t *testing.T) {
	t.Parallel()

	usable := []patchset.Patch{patch("patches/0001-a.patch", "", "")}
	tests := []struct {
		name    string
		request patchset.Request
		want    error
	}{
		{
			name:    "no source ref",
			request: patchset.Request{SourceSHA: "0123456789abcdef", Patches: usable},
			want:    patchset.ErrIncompleteRequest,
		},
		{
			name:    "no source sha",
			request: patchset.Request{SourceRef: "refs/heads/master", Patches: usable},
			want:    patchset.ErrIncompleteRequest,
		},
		{
			name: "duplicate identifier",
			request: patchset.Request{
				SourceRef: "refs/heads/master",
				SourceSHA: "0123456789abcdef",
				Patches:   []patchset.Patch{usable[0], usable[0]},
			},
			want: patchset.ErrDuplicatePatch,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := patchset.Apply(t.Context(), newDAG(), test.request)
			if !errors.Is(err, test.want) {
				t.Errorf("apply error %v, want %v", err, test.want)
			}
		})
	}
}

func TestApplyRejectsNilGit(t *testing.T) {
	t.Parallel()

	_, err := patchset.Apply(t.Context(), nil, patchset.Request{SourceRef: "refs/heads/master", SourceSHA: "abc"})
	if !errors.Is(err, patchset.ErrNoGit) {
		t.Errorf("apply error %v, want %v", err, patchset.ErrNoGit)
	}
}

// brokenGit is a binding that reports a clean tree to the precondition query
// and then fails every later operation.
//
// A real repository cannot be made to fail status, diff, and reset on demand,
// and the behaviour under test is what a conflict report does when the Git it
// was handed stops working part way through a pass. The precondition is
// answered honestly so the pass reaches the code the test is about.
type brokenGit struct {
	// statuses counts Status calls. The first answers the clean HEAD
	// precondition; every later one belongs to evidence collection and fails.
	statuses int
}

func (g *brokenGit) IsAncestor(context.Context, string, string) (bool, error) {
	return false, errUnsupported
}
func (g *brokenGit) ApplyPatch(context.Context, gitcli.ApplyOptions) error { return errUnsupported }
func (g *brokenGit) Status(context.Context) ([]gitcli.StatusEntry, error) {
	g.statuses++
	if g.statuses == 1 {
		return nil, nil
	}
	return nil, errUnsupported
}
func (g *brokenGit) Diff(context.Context, gitcli.DiffOptions) (string, error) {
	return "", errUnsupported
}
func (g *brokenGit) ResetHard(context.Context, string) error { return errUnsupported }

// TestApplyJoinsEvidenceFailures asserts that a binding which cannot collect
// evidence or roll back still reports the original failure and reports the
// additional problems rather than swallowing either.
func TestApplyJoinsEvidenceFailures(t *testing.T) {
	t.Parallel()

	_, err := patchset.Apply(t.Context(), &brokenGit{}, patchset.Request{
		SourceRef: "refs/heads/master",
		SourceSHA: "0123456789abcdef0123456789abcdef01234567",
		Patches:   []patchset.Patch{patch("patches/0001-a.patch", "", "")},
	})

	var conflict *patchset.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("apply error %v, want a *patchset.ConflictError", err)
	}
	if !errors.Is(err, errUnsupported) {
		t.Errorf("conflict does not wrap the underlying failure: %v", err)
	}
	for _, want := range []string{"collect status", "collect work tree diff", "roll back"} {
		if !strings.Contains(conflict.Err.Error(), want) {
			t.Errorf("joined error does not mention %q: %v", want, conflict.Err)
		}
	}
}

func TestApplyHonoursCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := patchset.Apply(ctx, newDAG(), patchset.Request{
		SourceRef: "refs/heads/master",
		SourceSHA: "0123456789abcdef0123456789abcdef01234567",
		Patches:   []patchset.Patch{patch("patches/0001-a.patch", "", "")},
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("apply error %v, want %v", err, context.Canceled)
	}
}

// TestApplyRollsBackWhenCancelledMidSeries covers cancellation arriving between
// two patches of a series.
//
// The first patch has applied and is staged, so returning at that point would
// hand back exactly the partially patched tree the pass promises never to
// produce. The stop has to leave through the rollback like any other failure,
// and the returned error has to carry both the cancellation and the report.
func TestApplyRollsBackWhenCancelledMidSeries(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	repo := newGitRepo(ctx, t)
	repo.WriteAndCommit(ctx, t, "f.txt", baseContents, "base")

	passCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	_, err := patchset.Apply(passCtx, repo.Git, patchset.Request{
		SourceRef: "refs/heads/master",
		SourceSHA: "0123456789abcdef0123456789abcdef01234567",
		Patches: []patchset.Patch{
			createPatch("patches/0001-add.patch", "added.txt", "first\n"),
			createPatch("patches/0002-add.patch", "second.txt", "second\n"),
		},
		// Cancelling from the reassertion is how a real run stops: the caller's
		// context ends while the pass is between patches, with the first patch
		// already staged.
		ReassertPrune: func(context.Context, patchset.Patch) error {
			cancel()
			return nil
		},
	})

	var conflict *patchset.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("apply error %v, want a *patchset.ConflictError", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("conflict does not wrap the cancellation: %v", err)
	}
	if conflict.Stage != patchset.StageCancel {
		t.Errorf("conflict stage %q, want %q", conflict.Stage, patchset.StageCancel)
	}
	if conflict.PatchID != "patches/0002-add.patch" || conflict.PatchIndex != 1 {
		t.Errorf("conflict names patch %q at index %d, want the second patch", conflict.PatchID, conflict.PatchIndex)
	}

	// The rollback is the point of the test: the first patch must be gone even
	// though the context that would have driven the rollback was cancelled.
	if got := readFile(t, repo, "added.txt"); got != "" {
		t.Errorf("added.txt survived the rollback with %q", got)
	}
	if entries := repo.status(ctx, t); len(entries) != 0 {
		t.Errorf("work tree is not clean after rollback: %v", entries)
	}
}

// TestApplyRollsBackUnderACancelledContext asserts that evidence collection and
// the rollback run detached from the caller's context.
//
// The reassertion cancels and then rejects, so the conflict path is entered
// with a context that is already done. If the rollback inherited it, every Git
// call would fail immediately, the report would carry no status or diff, and
// the patched tree would be left behind.
func TestApplyRollsBackUnderACancelledContext(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	repo := newGitRepo(ctx, t)
	repo.WriteAndCommit(ctx, t, "f.txt", baseContents, "base")

	passCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	pruned := errors.New("patch reintroduced a pruned file")
	_, err := patchset.Apply(passCtx, repo.Git, patchset.Request{
		SourceRef: "refs/heads/master",
		SourceSHA: "0123456789abcdef0123456789abcdef01234567",
		Patches:   []patchset.Patch{createPatch("patches/0001-add.patch", "added.txt", "first\n")},
		ReassertPrune: func(context.Context, patchset.Patch) error {
			cancel()
			return pruned
		},
	})

	var conflict *patchset.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("apply error %v, want a *patchset.ConflictError", err)
	}
	if !errors.Is(err, pruned) {
		t.Errorf("conflict does not wrap the prune failure: %v", err)
	}
	// Evidence survived, which is only possible on a detached context.
	if len(conflict.Status) == 0 {
		t.Error("conflict carries no status, so evidence collection inherited the cancellation")
	}
	if conflict.Diff == "" {
		t.Error("conflict carries no diff, so evidence collection inherited the cancellation")
	}
	if got := readFile(t, repo, "added.txt"); got != "" {
		t.Errorf("added.txt survived the rollback with %q", got)
	}
	if entries := repo.status(ctx, t); len(entries) != 0 {
		t.Errorf("work tree is not clean after rollback: %v", entries)
	}
}

// TestApplyRejectsDirtyWorkTree covers the precondition Apply documents. The
// rollback is a hard reset to HEAD, so a caller whose pre-patch state is not
// committed would have it destroyed by the very step that is supposed to
// restore it. The pass has to refuse before it touches anything.
func TestApplyRejectsDirtyWorkTree(t *testing.T) {
	t.Parallel()

	const modified = "line1\nlocal edit\nline3\n"
	tests := []struct {
		name string
		// dirty makes the work tree diverge from HEAD.
		dirty func(ctx context.Context, t *testing.T, repo *gitRepo)
		// survivor is a path whose contents must be untouched afterwards, and
		// the contents it must still hold.
		path string
		want string
	}{
		{
			name: "modified tracked file",
			dirty: func(_ context.Context, t *testing.T, repo *gitRepo) {
				repo.WriteFile(t, "f.txt", modified)
			},
			path: "f.txt",
			want: modified,
		},
		{
			name: "untracked file",
			dirty: func(_ context.Context, t *testing.T, repo *gitRepo) {
				repo.WriteFile(t, "materialized.txt", "not committed\n")
			},
			path: "materialized.txt",
			want: "not committed\n",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			ctx := t.Context()
			repo := newGitRepo(ctx, t)
			repo.WriteAndCommit(ctx, t, "f.txt", baseContents, "base")
			test.dirty(ctx, t, repo)

			_, err := patchset.Apply(ctx, repo.Git, patchset.Request{
				SourceRef: "refs/heads/master",
				SourceSHA: "0123456789abcdef0123456789abcdef01234567",
				Patches:   []patchset.Patch{createPatch("patches/0001-add.patch", "added.txt", "first\n")},
			})
			if !errors.Is(err, patchset.ErrDirtyWorkTree) {
				t.Fatalf("apply error %v, want %v", err, patchset.ErrDirtyWorkTree)
			}
			// Refusing has to mean refusing: the caller's uncommitted content is
			// still exactly where it was, and no patch was applied.
			if got := readFile(t, repo, test.path); got != test.want {
				t.Errorf("%s is %q, want the caller's %q", test.path, got, test.want)
			}
			if got := readFile(t, repo, "added.txt"); got != "" {
				t.Errorf("the refused pass applied a patch anyway: added.txt is %q", got)
			}
		})
	}
}

// TestApplyEmptySeriesSkipsThePrecondition asserts that a source commit with no
// patches passes through a dirty tree untouched. An empty series changes
// nothing and must not impose a requirement it has no use for.
func TestApplyEmptySeriesSkipsThePrecondition(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	repo := newGitRepo(ctx, t)
	repo.WriteAndCommit(ctx, t, "f.txt", baseContents, "base")
	repo.WriteFile(t, "materialized.txt", "not committed\n")

	result, err := patchset.Apply(ctx, repo.Git, patchset.Request{
		SourceRef: "refs/heads/master",
		SourceSHA: "0123456789abcdef0123456789abcdef01234567",
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(result.Applied) != 0 {
		t.Errorf("applied %v, want nothing", result.PatchIDs())
	}
	if got := readFile(t, repo, "materialized.txt"); got != "not committed\n" {
		t.Errorf("materialized.txt is %q, want it untouched", got)
	}
}
