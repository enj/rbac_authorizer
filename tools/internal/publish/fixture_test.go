package publish

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/enj/soapbox/tools/internal/gitcli"
)

// The destination layout every fixture publishes to. It matches the shape a
// generated repository is configured with: one consumer branch, release tags, a
// state branch that looks like a consumer branch and is not one, and a progress
// namespace outside anything a module proxy reads.
const (
	testIdentity       = "github.com/enj/rbac_authorizer"
	testBranch         = "refs/heads/main"
	testStateRef       = "refs/heads/soapbox-state"
	testProgressPrefix = "refs/soapbox/progress/"
	testProgressRef    = testProgressPrefix + "backfill"
)

// testSignature pins identity and date on every fixture commit.
//
// Object names have to be reproducible for the determinism tests to mean
// anything: two fixtures built in different temporary directories must produce
// the same commits, and a wall clock date would make them differ for a reason
// that has nothing to do with what is being published.
var testSignature = gitcli.Signature{
	Name:  "Soapbox Test",
	Email: "test@soapbox.invalid",
	Date:  "1700000000 +0000",
}

// destination is a real local repository and the real remote it publishes to.
type destination struct {
	tb     testing.TB
	ctx    context.Context
	git    *gitcli.Runner
	dir    string
	remote string
	pub    *Publisher
}

// newDestination builds a local repository, a bare remote, and a publisher
// bound to both.
//
// The remote is a real repository that real pushes reach; nothing here is
// stubbed. It is made bare by configuration rather than by git init --bare,
// because the typed Git boundary exposes no bare initialization and this
// package may not add one. Bareness is load bearing rather than cosmetic: a
// non-bare remote refuses a push to its checked out branch, which would make
// every branch test depend on which branch the fixture happened to create.
func newDestination(ctx context.Context, tb testing.TB, format gitcli.ObjectFormat) *destination {
	tb.Helper()
	if format == "" {
		format = gitcli.ObjectFormatSHA1
	}
	dir := tb.TempDir()
	local := newRepository(ctx, tb, dir, "work", format)

	remoteRoot := tb.TempDir()
	remote := newRepository(ctx, tb, remoteRoot, "main", format)
	if err := remote.SetConfigLocal(ctx, "core.bare", "true"); err != nil {
		tb.Fatalf("make the remote bare: %v", err)
	}
	remotePath := filepath.Join(remoteRoot, ".git")

	d := &destination{tb: tb, ctx: ctx, git: local, dir: dir, remote: remotePath}
	d.pub = d.publisher(Options{})
	return d
}

// publisher builds a publisher for this destination, filling in every option
// the caller left empty with the fixture default.
func (d *destination) publisher(opts Options) *Publisher {
	d.tb.Helper()
	if opts.Remote == "" {
		opts.Remote = d.remote
		opts.AllowLocalRemote = true
	}
	if opts.Identity == "" {
		opts.Identity = testIdentity
	}
	if opts.Lister == nil {
		opts.Lister = NewLocalRemote(d.git)
	}
	if opts.Namespaces == (Namespaces{}) {
		opts.Namespaces = Namespaces{StateRef: testStateRef, ProgressPrefix: testProgressPrefix}
	}
	pub, err := New(d.ctx, d.git, opts)
	if err != nil {
		d.tb.Fatalf("create publisher: %v", err)
	}
	return pub
}

// newRepository initializes one repository with an isolated environment.
func newRepository(ctx context.Context, tb testing.TB, dir, branch string, format gitcli.ObjectFormat) *gitcli.Runner {
	tb.Helper()
	// HOME travels as an isolation entry rather than an environment entry: it
	// decides where git looks for state and is not a secret, so seeding it into
	// the redactor would only make a temporary path unreadable in failures.
	git, err := gitcli.New(ctx, gitcli.Options{
		Dir:       dir,
		Inherit:   []string{"PATH"},
		Isolation: []string{"HOME=" + tb.TempDir()},
	})
	if err != nil {
		tb.Fatalf("create git runner: %v", err)
	}
	if err := git.InitRepositoryWithFormat(ctx, branch, format); err != nil {
		tb.Fatalf("initialize %s repository: %v", string(format), err)
	}
	return git
}

// commit records one commit on the current branch and reports its object name.
func (d *destination) commit(name, content, message string) string {
	d.tb.Helper()
	if err := os.WriteFile(filepath.Join(d.dir, name), []byte(content), 0o600); err != nil {
		d.tb.Fatalf("write %s: %v", name, err)
	}
	if err := d.git.AddPaths(d.ctx, name); err != nil {
		d.tb.Fatalf("stage %s: %v", name, err)
	}
	opts := gitcli.CommitOptions{Message: message, Author: testSignature, Committer: testSignature}
	if err := d.git.Commit(d.ctx, opts); err != nil {
		d.tb.Fatalf("commit %s: %v", message, err)
	}
	sha, err := d.git.ResolveCommit(d.ctx, "HEAD")
	if err != nil {
		d.tb.Fatalf("resolve HEAD: %v", err)
	}
	return sha
}

// rewind moves the working branch back to an earlier commit, which is how a
// fixture builds two histories that do not descend from one another.
func (d *destination) rewind(commit string) {
	d.tb.Helper()
	if err := d.git.ResetHard(d.ctx, commit); err != nil {
		d.tb.Fatalf("rewind to %s: %v", commit, err)
	}
}

// tag creates an annotated tag and reports the tag object's name.
func (d *destination) tag(name, commit, message string) string {
	d.tb.Helper()
	opts := gitcli.TagOptions{Name: name, Commit: commit, Message: message, Tagger: testSignature}
	if err := d.git.CreateTag(d.ctx, opts); err != nil {
		d.tb.Fatalf("create tag %s: %v", name, err)
	}
	refs, err := d.git.ListRefs(d.ctx, tagPrefix+name)
	if err != nil {
		d.tb.Fatalf("read tag %s: %v", name, err)
	}
	if len(refs) != 1 {
		d.tb.Fatalf("read tag %s: got %d refs, want 1", name, len(refs))
	}
	return refs[0].Target
}

// seed pushes objects straight to the remote, which is how a fixture arranges
// the state a plan will find there.
func (d *destination) seed(refspecs ...string) {
	d.tb.Helper()
	if err := d.git.Push(d.ctx, d.remote, refspecs...); err != nil {
		d.tb.Fatalf("seed the remote: %v", err)
	}
}

// remoteRefs reports what the remote holds now, by ref name.
func (d *destination) remoteRefs() map[string]string {
	d.tb.Helper()
	refs, err := NewLocalRemote(d.git).RemoteRefs(d.ctx, d.remote)
	if err != nil {
		d.tb.Fatalf("read remote refs: %v", err)
	}
	observed := make(map[string]string, len(refs))
	for _, ref := range refs {
		observed[ref.Name] = ref.Target
	}
	return observed
}

// requireRemote asserts the exact set of refs the remote holds.
func (d *destination) requireRemote(want map[string]string) {
	d.tb.Helper()
	observed := d.remoteRefs()
	for name, object := range want {
		if observed[name] != object {
			d.tb.Errorf("remote %s = %q, want %q", name, observed[name], object)
		}
	}
	for name := range observed {
		if _, ok := want[name]; !ok {
			d.tb.Errorf("remote holds unexpected ref %s at %s", name, observed[name])
		}
	}
}

// planUpdates plans and fails the test when planning does not succeed.
func (d *destination) planUpdates(updates ...Update) *Plan {
	d.tb.Helper()
	plan, err := d.pub.Plan(d.ctx, updates)
	if err != nil {
		d.tb.Fatalf("plan: %v", err)
	}
	return plan
}

// apply applies an approved plan and fails the test when it does not succeed.
func (d *destination) apply(plan *Plan, scope Scope) *Result {
	d.tb.Helper()
	result, err := d.pub.Apply(d.ctx, plan, ApplyOptions{Approval: plan.Hash(), Scope: scope})
	if err != nil {
		d.tb.Fatalf("apply %s: %v", string(scope), err)
	}
	return result
}

// branchUpdate builds a consumer branch update.
func branchUpdate(ref, object string) Update {
	return Update{Ref: ref, Kind: KindBranch, NewObject: object, Evidence: "replay:master"}
}

// tagUpdate builds a consumer tag update.
func tagUpdate(ref, object string) Update {
	return Update{Ref: ref, Kind: KindTag, NewObject: object, Evidence: "release:v0.36.1"}
}
