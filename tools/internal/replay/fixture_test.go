package replay_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/relocate"
	"github.com/enj/soapbox/tools/internal/replay"
	"github.com/enj/soapbox/tools/internal/testsupport"
	"github.com/enj/soapbox/tools/internal/treebuild"
)

// The identities and the trailer key every test replays under.
const (
	provenanceKey = "Kubernetes-commit"
	authorName    = "Upstream Author"
	authorEmail   = "author@upstream.example"
	upstreamName  = "Upstream Committer"
	upstreamEmail = "committer@upstream.example"
	botName       = "soapbox-bot"
	botEmail      = "bot@soapbox.example"
	profileHash   = "sha256:0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0"
)

// bot is the committer identity every replayed commit records.
var bot = replay.Identity{Name: botName, Email: botEmail}

// fixture is one replay scenario: a real upstream repository whose commits are
// replayed, and a real destination repository they are written into.
//
// Two repositories rather than one, because that is the real arrangement and it
// is the only way to notice a replay that accidentally reads the source. The
// package under test is only ever handed the destination runner.
type fixture struct {
	t      *testing.T
	source *gitcli.Runner
	dest   *gitcli.Runner
	// commits are the source commits in creation order, which is the order a
	// caller hands them over in.
	commits []replay.Commit
	// named resolves a test's readable commit name to its object name, so a
	// scenario reads as a graph rather than as a list of hashes.
	named map[string]string
	// content is the destination content each source commit projects to. Two
	// commits with the same content produce the same tree, which is what an
	// upstream commit that touched nothing the extraction keeps looks like.
	content map[string]string
	// trees caches the destination tree each content string was written as.
	trees map[string]string
}

// newFixture creates both repositories under the given hash algorithm.
//
// The default format goes through the shared testsupport helper. sha256 does not
// because the hash algorithm is chosen when a repository is created and that
// helper does not express it, while the algorithm is exactly what this package
// must not depend on.
func newFixture(ctx context.Context, t *testing.T, format gitcli.ObjectFormat) *fixture {
	t.Helper()
	return &fixture{
		t:       t,
		source:  newRepo(ctx, t, format),
		dest:    newRepo(ctx, t, format),
		named:   make(map[string]string),
		content: make(map[string]string),
		trees:   make(map[string]string),
	}
}

// newRepo creates one real repository whose objects use the named algorithm.
func newRepo(ctx context.Context, t *testing.T, format gitcli.ObjectFormat) *gitcli.Runner {
	t.Helper()
	if format == gitcli.ObjectFormatSHA1 {
		return testsupport.NewRepo(ctx, t, testsupport.Options{}).Git
	}
	git, err := gitcli.New(ctx, gitcli.Options{
		Dir:       t.TempDir(),
		Inherit:   []string{"PATH"},
		Isolation: []string{"HOME=" + t.TempDir()},
	})
	if err != nil {
		t.Fatalf("create git runner: %v", err)
	}
	if err := git.InitRepositoryWithFormat(ctx, "main", format); err != nil {
		t.Fatalf("init %s repository: %v", format, err)
	}
	return git
}

// add writes one upstream commit and records what it projects to.
//
// The dates are pinned to the commit's position rather than taken from a clock,
// so the source object names are the same on every run and a rerun comparison
// is comparing the replay rather than the fixture.
func (f *fixture) add(ctx context.Context, name, content string, parents ...string) string {
	f.t.Helper()

	index := len(f.commits)
	tree := f.writeTree(ctx, f.source, "upstream/"+name)
	author := gitcli.Signature{
		Name:  authorName,
		Email: authorEmail,
		Date:  fmt.Sprintf("%d +0000", 1700000000+index),
	}
	committer := gitcli.Signature{
		Name:  upstreamName,
		Email: upstreamEmail,
		Date:  fmt.Sprintf("%d +0200", 1700000500+index),
	}
	message := "feat: " + name + "\n\nthe body of " + name + "\n"

	shas := make([]string, 0, len(parents))
	for _, parent := range parents {
		shas = append(shas, f.sha(parent))
	}
	sha, err := f.source.WriteCommit(ctx, gitcli.CommitTreeOptions{
		Tree:      tree,
		Parents:   shas,
		Message:   message,
		Author:    author,
		Committer: committer,
	})
	if err != nil {
		f.t.Fatalf("write upstream commit %s: %v", name, err)
	}

	f.named[name] = sha
	f.content[sha] = content
	f.commits = append(f.commits, replay.Commit{
		SHA:           sha,
		Parents:       shas,
		Author:        author,
		CommitterDate: committer.Date,
		Message:       message,
	})
	return sha
}

// sha resolves a commit name the scenario used.
func (f *fixture) sha(name string) string {
	f.t.Helper()
	sha, ok := f.named[name]
	if !ok {
		f.t.Fatalf("unknown commit %q", name)
	}
	return sha
}

// name reports the readable name of an object name, for failure messages.
func (f *fixture) name(sha string) string {
	for name, candidate := range f.named {
		if candidate == sha {
			return name
		}
	}
	return sha
}

// tree reports the destination tree one content string is written as, writing it
// on first use. Empty content is the empty tree, which is what a commit that
// generated nothing produces.
func (f *fixture) tree(ctx context.Context, content string) string {
	f.t.Helper()
	if content == "" {
		empty, err := f.dest.EmptyTree(ctx)
		if err != nil {
			f.t.Fatalf("empty tree: %v", err)
		}
		return empty
	}
	if tree, ok := f.trees[content]; ok {
		return tree
	}
	tree := f.writeTree(ctx, f.dest, content)
	f.trees[content] = tree
	return tree
}

// writeTree writes a one file tree holding the given content.
func (f *fixture) writeTree(ctx context.Context, git *gitcli.Runner, content string) string {
	f.t.Helper()
	manifest, err := treebuild.WriteFileSet(ctx, git, relocate.FileSet{
		Files: []relocate.File{{
			Path:     "internal/kk/pkg/apis/rbac/types.go",
			Mode:     relocate.ModeRegular,
			Contents: []byte("package rbac\n\n// " + content + "\n"),
		}},
	})
	if err != nil {
		f.t.Fatalf("write tree for %q: %v", content, err)
	}
	return manifest.Tree
}

// projection is a transform that projects each source commit onto the content
// the fixture assigned it.
//
// It reports a change by comparing that content with the content of the last
// commit it saw on the same first parent line, which is how a real transform
// answers the question: it knows what it produced for the commit before, not
// what the destination repository holds. base is what the destination already
// holds where the replay starts, which is the epoch parent's content when a run
// extends published history and nothing at all when it starts one.
type projection struct {
	fixture *fixture
	base    string
	seen    map[string]string
	// calls counts transforms performed, and hook runs before each result is
	// returned so a test can cancel or fail at an exact point in the traversal.
	calls int
	hook  func(call int, source replay.Commit) error
}

// newProjection returns a transform over the fixture's content assignment.
func (f *fixture) newProjection(base string) *projection {
	return &projection{fixture: f, base: base, seen: make(map[string]string)}
}

// transform is the [replay.Transform] the tests replay with.
func (p *projection) transform(ctx context.Context, source replay.Commit) (replay.Transformed, error) {
	p.calls++
	content := p.fixture.content[source.SHA]
	baseline := p.base
	if len(source.Parents) > 0 {
		if previous, ok := p.seen[source.Parents[0]]; ok {
			baseline = previous
		}
	}
	p.seen[source.SHA] = content
	transformed := replay.Transformed{
		Source:   source.SHA,
		Tree:     p.fixture.tree(ctx, content),
		Changed:  content != baseline,
		Evidence: []string{"content " + content},
	}

	// The hook runs last so that a hook which cancels the context models a
	// transform interrupted after its work rather than one that could not do it.
	if p.hook != nil {
		if err := p.hook(p.calls, source); err != nil {
			return replay.Transformed{}, err
		}
	}
	return transformed, nil
}

// options returns replay options over the whole fixture with the given
// transform, ready for a test to adjust.
func (f *fixture) options(transform replay.Transform) replay.Options {
	return replay.Options{
		Commits:       f.commits,
		Epoch:         replay.Epoch{ProfileHash: profileHash},
		Bot:           bot,
		ProvenanceKey: provenanceKey,
		Transform:     transform,
	}
}

// run replays the whole fixture and requires success.
func (f *fixture) run(ctx context.Context, opts replay.Options) *replay.Result {
	f.t.Helper()
	result, err := replay.Run(ctx, f.dest, opts)
	if err != nil {
		f.t.Fatalf("replay: %v", err)
	}
	return result
}

// destination reports the destination commit a source name produced, and fails
// when the source commit produced nothing.
func (f *fixture) destination(result *replay.Result, name string) string {
	f.t.Helper()
	destination, ok := result.Mapping.Destination(f.sha(name))
	if !ok {
		f.t.Fatalf("commit %s is not mapped", name)
	}
	return destination
}

// record reports the record of one source name.
func (f *fixture) record(result *replay.Result, name string) replay.Record {
	f.t.Helper()
	sha := f.sha(name)
	for _, record := range result.Records {
		if record.Source == sha {
			return record
		}
	}
	f.t.Fatalf("commit %s has no record", name)
	return replay.Record{}
}

// parents reports the parents of a destination commit.
func (f *fixture) parents(ctx context.Context, commit string) []string {
	f.t.Helper()
	parents, err := f.dest.CommitParents(ctx, commit)
	if err != nil {
		f.t.Fatalf("read parents of %s: %v", commit, err)
	}
	return parents
}

// commitTree reports the tree a destination commit records.
func (f *fixture) commitTree(ctx context.Context, commit string) string {
	f.t.Helper()
	tree, err := f.dest.ResolveTree(ctx, commit)
	if err != nil {
		f.t.Fatalf("read tree of %s: %v", commit, err)
	}
	return tree
}

// info reads one commit from a repository.
func info(ctx context.Context, t *testing.T, git *gitcli.Runner, commit string) gitcli.Commit {
	t.Helper()
	read, err := git.CommitInfo(ctx, commit)
	if err != nil {
		t.Fatalf("read commit %s: %v", commit, err)
	}
	return read
}
