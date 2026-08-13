package sync

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/enj/soapbox/tools/internal/config"
	"github.com/enj/soapbox/tools/internal/generate"
	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/testsupport"
)

// The upstream release this file builds. The names are local rather than
// shared with the package's external tests, because those live in sync_test and
// an internal test cannot see them.
const (
	upstreamTag = "v1.36.1"
	upstreamRef = "refs/tags/" + upstreamTag
)

// TestReadReleaseReportsWhatTheUpstreamTagRecords covers the half of a
// synchronization that opens the source repository.
//
// It is an internal test because the read is not a seam a caller reaches: Plan
// performs it against the clone the generation just used, so the only way to
// exercise it against a repository a test built is from inside the package.
// What it has to get right is that every value a released tag reproduces comes
// out of the objects rather than out of the clock or the environment.
func TestReadReleaseReportsWhatTheUpstreamTagRecords(t *testing.T) {
	ctx := t.Context()
	repo := testsupport.NewRepo(ctx, t, testsupport.Options{
		Branch:    "master",
		UserName:  "Upstream Author",
		UserEmail: "author@kubernetes.invalid",
	})

	const message = "Fix the authorizer\n\nA second paragraph the replay must preserve.\n"
	commit := repo.WriteAndCommit(ctx, t, "main.go", "package main\n", message)

	tagger := gitcli.Signature{
		Name:  "Kubernetes Release Robot",
		Email: "k8s-release-robot@users.noreply.github.com",
		Date:  "1700000000 +0000",
	}
	if err := repo.Git.CreateTag(ctx, gitcli.TagOptions{
		Name:    upstreamTag,
		Commit:  commit,
		Message: "Kubernetes " + upstreamTag + "\n",
		Tagger:  tagger,
	}); err != nil {
		t.Fatalf("create the release tag: %v", err)
	}

	cfg := &config.Config{Source: config.Source{
		Repository: "https://github.com/kubernetes/kubernetes.git",
	}}
	// The runner is rebased onto the repository the same way Plan rebases the
	// generation's runner onto its source cache.
	git, err := gitcli.New(ctx, gitcli.Options{Inherit: []string{"PATH"}})
	if err != nil {
		t.Fatalf("git runner: %v", err)
	}

	release, err := readRelease(ctx, git, repo.Dir, cfg, upstreamTag)
	if err != nil {
		t.Fatalf("read release: %v", err)
	}

	if release.Commit != commit {
		t.Errorf("commit = %q, want the tagged commit %q", release.Commit, commit)
	}
	if release.Ref != upstreamRef {
		t.Errorf("ref = %q, want %q", release.Ref, upstreamRef)
	}
	// The tagger's raw date is what the destination tag records, so a
	// regenerated tag is byte identical to the one the release's timing
	// produced. A date read from anywhere else would not reproduce.
	if release.Tagger != tagger {
		t.Errorf("tagger = %+v, want %+v", release.Tagger, tagger)
	}
	// The message travels whole. A replay that appended the subject again, or
	// that lost the second paragraph, would publish a commit claiming to be the
	// upstream one while saying something else.
	if release.Message != message {
		t.Errorf("message = %q, want %q", release.Message, message)
	}
	if release.Author.Name != "Upstream Author" || release.Author.Date == "" {
		t.Errorf("author = %+v, want the upstream author with a date", release.Author)
	}
	if release.CommitterDate == "" {
		t.Errorf("committer date is empty, want the upstream one")
	}
	const wantURL = "https://github.com/kubernetes/kubernetes/releases/tag/" + upstreamTag
	if release.URL != wantURL {
		t.Errorf("url = %q, want %q", release.URL, wantURL)
	}

	// The release the read produced has to be one Project accepts, or the two
	// halves of Plan agree only by accident.
	if err := validateRelease(release); err != nil {
		t.Errorf("the release this package read is one it refuses: %v", err)
	}
}

// TestSourceCacheDirNamesTheCloneTheExtractionBuilt is the test that catches a
// whole class of quiet wrongness.
//
// A generation reports the cache root, and the root is not a repository: it
// holds one bare clone per remote, under a name derived from that remote. A
// runner pointed at the root would let git discover whichever repository sits
// above it, which on a developer's machine is frequently the engine's own
// checkout. The release would then be read from the wrong repository and the
// published tag would claim an upstream commit that never produced the module.
//
// So the cache is built here exactly as the extraction builds it, and the
// release is read through the derivation this package uses.
func TestSourceCacheDirNamesTheCloneTheExtractionBuilt(t *testing.T) {
	ctx := t.Context()
	const remote = "https://github.com/kubernetes/kubernetes.git"
	cfg := &config.Config{Source: config.Source{Repository: remote}}
	opts := generate.Options{Config: cfg, CacheRoot: filepath.Join(t.TempDir(), "cache")}

	// The clone is created where the extraction would have created it, which is
	// the contract under test. It is built directly rather than through
	// source.Open because Open insists on a partial clone and a local path
	// remote cannot serve one; what matters here is the location, and the
	// location is a plain string derivation both packages have to agree on.
	cacheDir := sourceCacheDir(opts)
	if parent := filepath.Dir(cacheDir); parent != opts.CacheRoot {
		t.Fatalf("cache directory %q does not sit directly under the cache root %q", cacheDir, opts.CacheRoot)
	}
	if err := os.MkdirAll(cacheDir, 0o750); err != nil {
		t.Fatalf("create the cache directory: %v", err)
	}
	upstream := testsupport.NewRepo(ctx, t, testsupport.Options{
		Dir:       cacheDir,
		Branch:    "master",
		UserName:  "Upstream Author",
		UserEmail: "author@kubernetes.invalid",
	})
	commit := upstream.WriteAndCommit(ctx, t, "main.go", "package main\n", "Initial commit\n")
	if err := upstream.Git.CreateTag(ctx, gitcli.TagOptions{
		Name:    upstreamTag,
		Commit:  commit,
		Message: "Kubernetes " + upstreamTag + "\n",
		Tagger: gitcli.Signature{
			Name:  "Kubernetes Release Robot",
			Email: "k8s-release-robot@users.noreply.github.com",
			Date:  "1700000000 +0000",
		},
	}); err != nil {
		t.Fatalf("create the release tag: %v", err)
	}

	git, err := gitcli.New(ctx, gitcli.Options{Inherit: []string{"PATH"}})
	if err != nil {
		t.Fatalf("git runner: %v", err)
	}
	release, err := readRelease(ctx, git, cacheDir, cfg, upstreamTag)
	if err != nil {
		t.Fatalf("read the release out of the cache: %v", err)
	}
	if release.Commit != commit {
		t.Errorf("commit = %q, want the tagged commit %q", release.Commit, commit)
	}

	// The root is not a repository, and this is the assertion that keeps the
	// derivation honest: a version of this package that handed the root over
	// would fail here rather than quietly read whichever repository git
	// discovered above it.
	if _, err := readRelease(ctx, git, opts.CacheRoot, cfg, upstreamTag); err == nil {
		t.Errorf("reading the release from the cache root succeeded, want a failure: the root holds no repository")
	}

	// A source remote override selects a different cache, so a run against a
	// local mirror cannot read a release out of the clone of the real upstream.
	override := generate.Options{Config: cfg, CacheRoot: opts.CacheRoot, SourceRemote: t.TempDir()}
	if got, want := sourceCacheDir(override), cacheDir; got == want {
		t.Errorf("the cache directory is %q for both the profile remote and an override, want two caches", got)
	}
}

// TestReadReleaseRefusesASourceItCannotPublishAURLFor proves the release page
// is derived narrowly.
//
// The URL is written verbatim into a tag object that can never be taken back,
// so a source repository that is not an https URL produces a refusal rather
// than a guess. A local mirror is the common way to reach one, which is exactly
// why the profile's repository decides it and the run's -source-remote does not.
func TestReadReleaseRefusesASourceItCannotPublishAURLFor(t *testing.T) {
	for _, repository := range []string{
		filepath.Join(t.TempDir(), "mirror.git"),
		"git@github.com:kubernetes/kubernetes.git",
		"http://github.com/kubernetes/kubernetes.git",
	} {
		if _, err := releaseURL(repository, upstreamTag); !errors.Is(err, ErrUnsupported) {
			t.Errorf("releaseURL(%q) = %v, want an unsupported source", repository, err)
		}
	}
}
