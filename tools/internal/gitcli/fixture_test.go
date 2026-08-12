package gitcli_test

import (
	"context"
	"testing"

	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/testsupport"
)

// Fixture commit labels. Tests refer to commits by label so an assertion reads
// as a statement about the topology rather than about a hash.
const (
	base    = "base"    // the commit both release lines descend from
	mainOne = "mainOne" // mainline work after the split
	feature = "feature" // a side branch
	mergeC  = "merge"   // the merge of the side branch into the mainline
	release = "release" // work on the release branch
)

// Fixture ref names.
const (
	mainBranch     = "main"
	releaseBranch  = "release-1.36"
	annotatedTag   = "v1.36.1"
	lightweightTag = "v1.36.0"
)

// upstream is a real repository with the shapes source discovery has to cope
// with: a mainline, a merged side branch, a release branch that diverged from a
// shared base, an annotated release tag, and a lightweight tag.
//
//	base ── mainOne ── merge      (main)
//	  │        └── feature ─┘
//	  └── release                 (release-1.36, tagged v1.36.1)
type upstream struct {
	repo    *testsupport.Repo
	commits map[string]string
}

// newUpstream builds the fixture repository and serves it over file://, with
// partial clone filtering enabled so a blobless clone is actually exercised
// rather than silently downgraded to a full one.
func newUpstream(ctx context.Context, t *testing.T) *upstream {
	t.Helper()

	repo := testsupport.NewRepo(ctx, t, testsupport.Options{
		Branch:    mainBranch,
		UserName:  testUserName,
		UserEmail: testUserEmail,
	})
	// Without this the server refuses --filter and hands back every blob, so a
	// test asserting partial clone behaviour would pass against a full clone.
	repo.SetConfig(ctx, t, "uploadpack.allowFilter", "true")

	up := &upstream{repo: repo, commits: make(map[string]string)}
	up.commits[base] = repo.WriteAndCommit(ctx, t, "README.md", "base\n", "docs: add readme\n")
	up.commits[mainOne] = repo.WriteAndCommit(ctx, t,
		"plugin/pkg/auth/authorizer/rbac/rbac.go", "package rbac\n", "feat: add authorizer\n")

	// The side branch starts from the base commit, so the merge below has two
	// genuinely different parents.
	up.checkout(ctx, t, up.commits[base])
	up.commits[feature] = repo.WriteAndCommit(ctx, t,
		"pkg/registry/rbac/validation/rule.go", "package validation\n", "feat: add rule resolver\n")

	up.commits[mergeC] = up.merge(ctx, t, up.commits[mainOne], up.commits[feature])
	up.updateRef(ctx, t, "refs/heads/"+mainBranch, up.commits[mergeC])

	// The release branch diverges from the shared base, which is what makes the
	// common ancestor of the two lines interesting.
	up.checkout(ctx, t, up.commits[base])
	up.commits[release] = repo.WriteAndCommit(ctx, t, "release.md", "1.36\n", "docs: open 1.36\n")
	up.updateRef(ctx, t, "refs/heads/"+releaseBranch, up.commits[release])

	up.tag(ctx, t, gitcli.TagOptions{
		Name:    annotatedTag,
		Commit:  up.commits[release],
		Message: "Kubernetes v1.36.1\n",
		Tagger:  gitcli.Signature{Name: testUserName, Email: testUserEmail, Date: "2026-01-02T03:04:05Z"},
	})
	up.tag(ctx, t, gitcli.TagOptions{Name: lightweightTag, Commit: up.commits[base]})

	up.checkout(ctx, t, "refs/heads/"+mainBranch)
	return up
}

// url reports the file URL a clone reads the fixture from.
func (u *upstream) url() string { return "file://" + u.repo.Dir }

// sha reports the object name recorded for a fixture label.
func (u *upstream) sha(label string) string {
	return u.commits[label]
}

// checkout moves the fixture's work tree to a revision.
func (u *upstream) checkout(ctx context.Context, t *testing.T, revision string) {
	t.Helper()
	if err := u.repo.Git.CheckoutDetached(ctx, revision); err != nil {
		t.Fatalf("checkout %s: %v", revision, err)
	}
}

// merge records a merge commit whose tree is the mainline's, which models an
// upstream merge that took no content from the side branch.
func (u *upstream) merge(ctx context.Context, t *testing.T, first, second string) string {
	t.Helper()
	tree, err := u.repo.Git.ResolveTree(ctx, first)
	if err != nil {
		t.Fatalf("resolve tree: %v", err)
	}
	commit, err := u.repo.Git.WriteCommit(ctx, gitcli.CommitTreeOptions{
		Tree:      tree,
		Parents:   []string{first, second},
		Message:   "Merge rule resolver\n",
		Author:    gitcli.Signature{Name: testUserName, Email: testUserEmail, Date: testRawDate},
		Committer: gitcli.Signature{Name: testUserName, Email: testUserEmail, Date: testRawDate},
	})
	if err != nil {
		t.Fatalf("write merge commit: %v", err)
	}
	return commit
}

// updateRef points a fixture ref at a commit, creating it when it does not
// exist yet.
//
// The current value is read first because the typed runner only performs
// compare and swap updates: a blind write is exactly the local rewind the
// engine must not be able to perform by accident.
func (u *upstream) updateRef(ctx context.Context, t *testing.T, name, revision string) {
	t.Helper()
	exists, err := u.repo.Git.HasRef(ctx, name)
	if err != nil {
		t.Fatalf("probe %s: %v", name, err)
	}
	if !exists {
		if err := u.repo.Git.CreateRef(ctx, name, revision); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		return
	}
	current, err := u.repo.Git.ResolveCommit(ctx, name)
	if err != nil {
		t.Fatalf("resolve %s: %v", name, err)
	}
	if err := u.repo.Git.UpdateRef(ctx, name, revision, current); err != nil {
		t.Fatalf("update %s: %v", name, err)
	}
}

// tag creates a fixture tag.
func (u *upstream) tag(ctx context.Context, t *testing.T, opts gitcli.TagOptions) {
	t.Helper()
	if err := u.repo.Git.CreateTag(ctx, opts); err != nil {
		t.Fatalf("tag %s: %v", opts.Name, err)
	}
}

// commit records one more commit on the fixture's current branch.
func (u *upstream) commit(ctx context.Context, t *testing.T, relPath, contents, message string) string {
	t.Helper()
	return u.repo.WriteAndCommit(ctx, t, relPath, contents, message)
}

// newAnonymousRunner builds a runner that carries no caller supplied
// environment entry, which is what the source commands require. HOME is
// isolated through the process environment so it survives Anonymous.
func newAnonymousRunner(t *testing.T, dir string) *gitcli.Runner {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	runner, err := gitcli.New(t.Context(), gitcli.Options{
		Dir:     dir,
		Inherit: []string{"PATH", "HOME"},
	})
	if err != nil {
		t.Fatalf("create git runner: %v", err)
	}
	if !runner.IsAnonymous() {
		t.Fatal("runner built without Env entries must be anonymous")
	}
	return runner
}
