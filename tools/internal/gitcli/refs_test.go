package gitcli_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/testsupport"
)

// TestChangedPathsFromEmptyTree pins what an empty from means.
//
// It has to mean "compare against nothing" for every commit shape, because the
// caller that passes it is asking what a commit's tree contains. Asking git for
// that with --root does not: git compares a root commit against nothing, an
// ordinary commit against its parent, and a merge against nothing at all,
// emitting no output. The last case is the dangerous one, because an empty
// result reads as "this commit changed nothing" rather than as a question git
// declined to answer.
func TestChangedPathsFromEmptyTree(t *testing.T) {
	ctx := t.Context()
	repo := testsupport.NewRepo(ctx, t, testsupport.Options{UserName: testUserName, UserEmail: testUserEmail})

	repo.WriteFile(t, "root-one.txt", "one\n")
	repo.WriteFile(t, "dir/root-two.txt", "two\n")
	root := repo.Commit(ctx, t, "feat: root\n", gitcli.CommitOptions{}, "root-one.txt", "dir/root-two.txt")
	child := repo.WriteAndCommit(t.Context(), t, "child.txt", "child\n", "feat: child\n")

	if err := repo.Git.CheckoutDetached(ctx, root); err != nil {
		t.Fatalf("checkout: %v", err)
	}
	side := repo.WriteAndCommit(ctx, t, "side.txt", "side\n", "feat: side\n")

	tree, err := repo.Git.ResolveTree(ctx, child)
	if err != nil {
		t.Fatalf("resolve tree: %v", err)
	}
	identity := gitcli.Signature{Name: testUserName, Email: testUserEmail, Date: testRawDate}
	merge, err := repo.Git.WriteCommit(ctx, gitcli.CommitTreeOptions{
		Tree:      tree,
		Parents:   []string{child, side},
		Message:   "Merge side\n",
		Author:    identity,
		Committer: identity,
	})
	if err != nil {
		t.Fatalf("write merge: %v", err)
	}

	tests := []struct {
		name     string
		revision string
		want     []string
	}{
		{
			name:     "root commit",
			revision: root,
			want:     []string{"dir/root-two.txt", "root-one.txt"},
		},
		{
			// With --root git would answer for the parent instead and report
			// only child.txt.
			name:     "ordinary commit",
			revision: child,
			want:     []string{"child.txt", "dir/root-two.txt", "root-one.txt"},
		},
		{
			// With --root git would emit nothing at all, which reads as a merge
			// that introduced no paths.
			name:     "merge commit",
			revision: merge,
			want:     []string{"child.txt", "dir/root-two.txt", "root-one.txt"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := repo.Git.ChangedPaths(ctx, "", test.revision)
			if err != nil {
				t.Fatalf("changed paths: %v", err)
			}
			slices.Sort(got)
			if !slices.Equal(got, test.want) {
				t.Fatalf("changed paths = %v, want the whole tree %v", got, test.want)
			}
		})
	}

	// An explicit parent still answers the question it was asked.
	got, err := repo.Git.ChangedPaths(ctx, child, merge)
	if err != nil {
		t.Fatalf("changed paths: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("merge against its first parent changed %v, want nothing", got)
	}
}

// TestEmptyTreeMatchesTheRepositoryHashAlgorithm proves the name used for the
// comparison is the one this repository would resolve, not a constant that
// happens to be right for SHA-1.
func TestEmptyTreeMatchesTheRepositoryHashAlgorithm(t *testing.T) {
	ctx := t.Context()
	repo := testsupport.NewRepo(ctx, t, testsupport.Options{UserName: testUserName, UserEmail: testUserEmail})
	repo.WriteAndCommit(ctx, t, "a.txt", "a\n", "feat: a\n")

	empty, err := repo.Git.EmptyTree(ctx)
	if err != nil {
		t.Fatalf("empty tree: %v", err)
	}
	resolved, err := repo.Git.ResolveTree(ctx, empty)
	if err != nil {
		t.Fatalf("git does not know the empty tree %q: %v", empty, err)
	}
	if resolved != empty {
		t.Fatalf("empty tree resolved to %q, want %q", resolved, empty)
	}
}

// TestUpdateRefIsCompareAndSwap proves a local ref cannot be moved by a caller
// that is reasoning about a stale value.
//
// A blind update is how published history gets rewound: a concurrent run, a
// resumed run holding an old value, or a local rewrite between reading a ref
// and writing it would all be applied silently, and the ref the engine
// publishes from would no longer be the one it reasoned about.
func TestUpdateRefIsCompareAndSwap(t *testing.T) {
	ctx := t.Context()
	repo := testsupport.NewRepo(ctx, t, testsupport.Options{UserName: testUserName, UserEmail: testUserEmail})
	first := repo.WriteAndCommit(ctx, t, "a.txt", "a\n", "feat: a\n")
	second := repo.WriteAndCommit(ctx, t, "b.txt", "b\n", "feat: b\n")

	const ref = "refs/heads/published"
	if err := repo.Git.CreateRef(ctx, ref, first); err != nil {
		t.Fatalf("create ref: %v", err)
	}
	// Creating a ref that exists is refused rather than silently moving it.
	if err := repo.Git.CreateRef(ctx, ref, second); err == nil {
		t.Fatal("creating an existing ref must fail")
	}
	if got, err := repo.Git.ResolveCommit(ctx, ref); err != nil || got != first {
		t.Fatalf("ref = %q (%v), want it to stay at %q", got, err, first)
	}

	// A stale expected value is refused, which is the case a concurrent run or
	// a local rewind produces.
	if err := repo.Git.UpdateRef(ctx, ref, second, second); err == nil {
		t.Fatal("an update against a stale expected value must fail")
	}
	if got, err := repo.Git.ResolveCommit(ctx, ref); err != nil || got != first {
		t.Fatalf("ref = %q (%v), want it to stay at %q", got, err, first)
	}

	// The matching expected value is accepted.
	if err := repo.Git.UpdateRef(ctx, ref, second, first); err != nil {
		t.Fatalf("update against the current value: %v", err)
	}
	if got, err := repo.Git.ResolveCommit(ctx, ref); err != nil || got != second {
		t.Fatalf("ref = %q (%v), want %q", got, err, second)
	}

	// A rewind is an update like any other and is refused unless the caller
	// proves it knows where the ref currently is.
	if err := repo.Git.UpdateRef(ctx, ref, first, first); err == nil {
		t.Fatal("a rewind against a stale expected value must fail")
	}
	if err := repo.Git.UpdateRef(ctx, ref, first, second); err != nil {
		t.Fatalf("deliberate rewind against the current value: %v", err)
	}

	// There is no way to ask for a blind write.
	if err := repo.Git.UpdateRef(ctx, ref, second, ""); err == nil {
		t.Fatal("an empty expected value must be refused rather than meaning no check")
	} else if !strings.Contains(err.Error(), "expected object name") {
		t.Fatalf("error %q does not explain that an expected value is required", err)
	}
	if err := repo.Git.UpdateRef(ctx, ref, second, "--force"); err == nil {
		t.Fatal("an option like expected value must be rejected")
	}
}

// TestUpdateRefRejectsConcurrentRewind proves the comparison is git's, taken
// under the ref lock, rather than a read followed by a hopeful write.
func TestUpdateRefRejectsConcurrentRewind(t *testing.T) {
	ctx := t.Context()
	repo := testsupport.NewRepo(ctx, t, testsupport.Options{UserName: testUserName, UserEmail: testUserEmail})
	first := repo.WriteAndCommit(ctx, t, "a.txt", "a\n", "feat: a\n")
	second := repo.WriteAndCommit(ctx, t, "b.txt", "b\n", "feat: b\n")
	third := repo.WriteAndCommit(ctx, t, "c.txt", "c\n", "feat: c\n")

	const ref = "refs/heads/published"
	if err := repo.Git.CreateRef(ctx, ref, second); err != nil {
		t.Fatalf("create ref: %v", err)
	}

	// Another writer rewinds the ref after this run read it.
	if err := repo.Git.UpdateRef(ctx, ref, first, second); err != nil {
		t.Fatalf("simulate the other writer: %v", err)
	}
	err := repo.Git.UpdateRef(ctx, ref, third, second)
	if err == nil {
		t.Fatal("an update that would overwrite another writer must fail")
	}
	if got, resolveErr := repo.Git.ResolveCommit(ctx, ref); resolveErr != nil || got != first {
		t.Fatalf("ref = %q (%v), want the other writer's value %q", got, resolveErr, first)
	}
	if errors.Is(err, gitcli.ErrFlagLikeArgument) {
		t.Fatalf("error %v is a validation failure, not the ref lock verdict", err)
	}
}
