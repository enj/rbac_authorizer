package release_test

import (
	"context"
	"testing"

	"github.com/enj/soapbox/tools/internal/config"
	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/release"
	"github.com/enj/soapbox/tools/internal/relocate"
	"github.com/enj/soapbox/tools/internal/testsupport"
	"github.com/enj/soapbox/tools/internal/treebuild"
)

// The release every test projects, and the identities it is projected under.
//
// Every value is pinned rather than generated, because the whole property under
// test is that the same inputs produce the same object names. A date from a
// clock or a name from a temporary path would make a rerun comparison compare
// the fixture instead of the projection.
const (
	sourceTag      = "v1.36.1"
	destinationTag = "v0.36.1"
	// sourceCommit is 40 hexadecimal characters whichever algorithm the
	// destination repository uses, because it names a commit of the upstream
	// repository and upstream's hash algorithm is not the destination's to
	// choose. It is never resolved: this package only ever opens the destination
	// repository.
	sourceCommit = "9f0c1d2e3a4b5c6d7e8f90a1b2c3d4e5f6071829"
	sourceURL    = "https://github.com/kubernetes/kubernetes/releases/tag/v1.36.1"
	taggerName   = "Upstream Release Robot"
	taggerEmail  = "release@upstream.example"
	taggerDate   = "1700000900 +0000"
	botName      = "soapbox-bot"
	botEmail     = "bot@soapbox.example"
	authorName   = "Upstream Author"
	authorEmail  = "author@upstream.example"
)

// objectFormats are the hash algorithms a destination repository can be created
// under. sha256 is not hypothetical: it is chosen when the repository is created
// and it changes the length of every object name a release records.
var objectFormats = []gitcli.ObjectFormat{gitcli.ObjectFormatSHA1, gitcli.ObjectFormatSHA256}

// fixture is one destination repository holding the objects a release is
// described in terms of.
//
// One repository rather than two, because this package never opens the source
// one: everything it knows about the upstream release arrives as a value, so a
// projection that tried to read the source would have nowhere to read it from.
type fixture struct {
	t      *testing.T
	git    *gitcli.Runner
	format gitcli.ObjectFormat
	// replay is the destination commit the replay produced at the release, and
	// replayTree is the tree it records.
	replay     string
	replayTree string
	// projection is a tree differing from replayTree, which is what a release
	// whose dependencies moved from pseudo-versions to real ones produces.
	projection string
}

// newFixture builds the destination repository under the given hash algorithm.
func newFixture(ctx context.Context, t *testing.T, format gitcli.ObjectFormat) *fixture {
	t.Helper()
	f := &fixture{t: t, git: newRepo(ctx, t, format), format: format}
	f.replayTree = f.tree(ctx, "v0.0.0-20240101000000-abcdefabcdef")
	f.projection = f.tree(ctx, "v0.36.1")
	f.replay = f.commit(ctx, f.replayTree)
	return f
}

// newRepo creates one real repository whose objects use the named algorithm.
//
// The default format goes through the shared testsupport helper. sha256 does not
// because the hash algorithm is chosen when a repository is created and that
// helper does not express it, while the algorithm is exactly what a release must
// not depend on.
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

// tree writes a one file tree recording the dependency version the content
// names, which is the difference a release projection makes.
func (f *fixture) tree(ctx context.Context, version string) string {
	f.t.Helper()
	manifest, err := treebuild.WriteFileSet(ctx, f.git, relocate.FileSet{
		Files: []relocate.File{{
			Path:     "go.mod",
			Mode:     relocate.ModeRegular,
			Contents: []byte("module monis.app/kk/rbac_authorizer\n\nrequire monis.app/kk/api " + version + "\n"),
		}},
	})
	if err != nil {
		f.t.Fatalf("write tree for %q: %v", version, err)
	}
	return manifest.Tree
}

// blob writes one blob, which is an object of a type no release input may be.
func (f *fixture) blob(ctx context.Context, content string) string {
	f.t.Helper()
	blob, err := f.git.WriteBlob(ctx, []byte(content))
	if err != nil {
		f.t.Fatalf("write blob: %v", err)
	}
	return blob
}

// commit writes the destination commit the replay is standing on.
func (f *fixture) commit(ctx context.Context, tree string) string {
	f.t.Helper()
	commit, err := f.git.WriteCommit(ctx, gitcli.CommitTreeOptions{
		Tree:      tree,
		Message:   "feat: the replayed commit\n\nKubernetes-commit: " + sourceCommit + "\n",
		Author:    gitcli.Signature{Name: authorName, Email: authorEmail, Date: "1700000000 +0000"},
		Committer: gitcli.Signature{Name: botName, Email: botEmail, Date: "1700000500 +0000"},
	})
	if err != nil {
		f.t.Fatalf("write replay commit: %v", err)
	}
	return commit
}

// missing is an object name of the right length for this repository that the
// repository does not hold.
func (f *fixture) missing() string {
	name := make([]byte, len(f.replayTree))
	for i := range name {
		name[i] = 'a'
	}
	return string(name)
}

// options describes the release with a projection that changed nothing, which is
// the shape a test adjusts from.
func (f *fixture) options() release.Options {
	return release.Options{
		Policy: config.ReleasePolicyV1ToV0,
		Source: release.Source{
			Tag:    sourceTag,
			Commit: sourceCommit,
			Tagger: gitcli.Signature{Name: taggerName, Email: taggerEmail, Date: taggerDate},
			URL:    sourceURL,
		},
		Replay:     release.Replay{Commit: f.replay, Tree: f.replayTree},
		Projection: f.replayTree,
		Bot:        release.Identity{Name: botName, Email: botEmail},
	}
}

// project projects the release and requires success.
func (f *fixture) project(ctx context.Context, opts release.Options) *release.Result {
	f.t.Helper()
	result, err := release.Project(ctx, f.git, opts)
	if err != nil {
		f.t.Fatalf("project release: %v", err)
	}
	return result
}

// info reads one commit of the destination repository.
func (f *fixture) info(ctx context.Context, commit string) gitcli.Commit {
	f.t.Helper()
	read, err := f.git.CommitInfo(ctx, commit)
	if err != nil {
		f.t.Fatalf("read commit %s: %v", commit, err)
	}
	return read
}

// commitTree reports the tree a destination commit records.
func (f *fixture) commitTree(ctx context.Context, commit string) string {
	f.t.Helper()
	tree, err := f.git.ResolveTree(ctx, commit)
	if err != nil {
		f.t.Fatalf("read tree of %s: %v", commit, err)
	}
	return tree
}

// objectType reports the type of one object, and whether the repository holds it
// at all.
func (f *fixture) objectType(ctx context.Context, object string) (string, bool) {
	f.t.Helper()
	infos, err := f.git.ObjectInfoBatch(ctx, gitcli.ObjectInfoOptions{Revisions: []string{object}})
	if err != nil {
		f.t.Fatalf("probe object %s: %v", object, err)
	}
	if len(infos) != 1 {
		f.t.Fatalf("probe object %s: got %d records, want 1", object, len(infos))
	}
	if infos[0].Missing {
		return "", false
	}
	return infos[0].Type, true
}
