package sync_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/config"
	"github.com/enj/soapbox/tools/internal/generate"
	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/relocate"
	"github.com/enj/soapbox/tools/internal/sync"
)

// The destination layout every fixture publishes to. It is the shape a
// generated repository is configured with: one consumer branch, release tags, a
// state branch that looks like a consumer branch and is not one, and a progress
// namespace outside anything a module proxy reads.
const (
	testRepository  = "enj/rbac_authorizer"
	testIdentity    = "github.com/" + testRepository
	testModulePath  = "monis.app/kk/rbac_authorizer"
	testBranch      = "main"
	testBranchRef   = "refs/heads/" + testBranch
	testStateRef    = "refs/heads/soapbox-state"
	testProgressRef = "refs/soapbox/progress/"
)

// The upstream release every fixture publishes, and what the v1-to-v0 release
// policy maps it onto.
const (
	testSourceTag  = "v1.36.1"
	testSourceRef  = "refs/tags/" + testSourceTag
	testReleaseTag = "v0.36.1"
)

// testProfileHash is a well formed output affecting profile digest.
//
// Its exact value is arbitrary and its shape is not: the replay refuses a hash
// that is not "sha256:" and sixty-four lower case hexadecimal characters,
// because two spellings of one digest would compare as two epochs.
const testProfileHash = "sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// testSourceCommit is the upstream commit the fixture release was cut from. It
// is a SHA-1 shaped name because every fixture repository is SHA-1, which is
// what git still initializes by default.
const testSourceCommit = "1111111111111111111111111111111111111111"

// testSignature pins identity and date on everything the fixture writes.
//
// Object names have to be reproducible for the determinism tests to mean
// anything: two synchronizations run in different temporary directories must
// produce the same commits, and a wall clock date would make them differ for a
// reason that has nothing to do with what is being published.
var testSignature = gitcli.Signature{
	Name:  "soapbox[bot]",
	Email: "soapbox[bot]@users.noreply.github.com",
	Date:  "1700000000 +0000",
}

// destination is a real local repository and the real bare remote it publishes
// to.
//
// Nothing here is stubbed. The remote is a repository that real pushes reach,
// made bare by configuration rather than by git init --bare because the typed
// Git boundary exposes no bare initialization. Bareness is load bearing: a non
// bare remote refuses a push to its checked out branch, which would make every
// branch test depend on which branch the fixture happened to create.
type destination struct {
	git    *gitcli.Runner
	dir    string
	remote string
	parent string
}

func newDestination(ctx context.Context, t *testing.T) *destination {
	t.Helper()
	dir := t.TempDir()
	local := newRepository(ctx, t, dir, testBranch)

	remoteRoot := t.TempDir()
	remote := newRepository(ctx, t, remoteRoot, testBranch)
	if err := remote.SetConfigLocal(ctx, "core.bare", "true"); err != nil {
		t.Fatalf("make the remote bare: %v", err)
	}
	parent := writeControlPlane(ctx, t, local)
	return &destination{git: local, dir: dir, remote: filepath.Join(remoteRoot, ".git"), parent: parent}
}

// writeControlPlane creates the setup-derived commit that every replay preserves.
// It is local but not published, matching a dry run performed before the initial
// control-plane push. Fixed content and identity make its object name independent
// of the temporary repository path.
func writeControlPlane(ctx context.Context, t *testing.T, git *gitcli.Runner) string {
	t.Helper()
	files := []struct{ path, contents string }{
		{".github/workflows/ci.yml", "name: ci\n"},
		{".github/workflows/sync.yml", "name: sync\n"},
		{".gitignore", "/bin/\n"},
		{"go.mod", "module " + testModulePath + "\n\ngo 1.26.0\n"},
		{"internal/kk/stale/stale.go", "package stale\n"},
		{"patches/index.yaml", "patches: []\n"},
		{"soapbox.yaml", "version: 1\n"},
		{"tools/cmd/soapbox/main.go", "package main\n"},
		{"tools/go.mod", "module " + testModulePath + "/tools\n\ngo 1.26.0\n"},
	}
	entries := make([]gitcli.TreeEntry, 0, len(files))
	for _, file := range files {
		object, err := git.WriteBlob(ctx, []byte(file.contents))
		if err != nil {
			t.Fatalf("write control-plane blob %s: %v", file.path, err)
		}
		entries = append(entries, gitcli.TreeEntry{Mode: gitcli.ModeRegular, Object: object, Path: file.path})
	}
	tree, err := git.WriteTree(ctx, entries)
	if err != nil {
		t.Fatalf("write control-plane tree: %v", err)
	}
	commit, err := git.WriteCommit(ctx, gitcli.CommitTreeOptions{
		Tree:      tree,
		Author:    testSignature,
		Committer: testSignature,
		Message:   "chore: set up derived repository\n",
	})
	if err != nil {
		t.Fatalf("write control-plane commit: %v", err)
	}
	if err := git.CreateRef(ctx, testBranchRef, commit); err != nil {
		t.Fatalf("create local control-plane branch: %v", err)
	}
	return commit
}

// newRepository initializes one repository with an isolated environment.
func newRepository(ctx context.Context, t *testing.T, dir, branch string) *gitcli.Runner {
	t.Helper()
	// HOME travels as an isolation entry rather than an environment entry: it
	// decides where git looks for state and is not a secret, so seeding it into
	// the redactor would only make a temporary path unreadable in failures.
	git, err := gitcli.New(ctx, gitcli.Options{
		Dir:       dir,
		Inherit:   []string{"PATH"},
		Isolation: []string{"HOME=" + t.TempDir()},
	})
	if err != nil {
		t.Fatalf("create git runner: %v", err)
	}
	if err := git.InitRepository(ctx, branch); err != nil {
		t.Fatalf("initialize repository: %v", err)
	}
	return git
}

// options assembles a complete set of project options against this
// destination, with every field the caller left empty filled by the fixture.
func (d *destination) options() sync.ProjectOptions {
	return sync.ProjectOptions{
		Config:  testConfig(),
		Module:  testModule(),
		Release: testRelease(),
		Destination: sync.Destination{
			Git:              d.git,
			Remote:           d.remote,
			Identity:         testIdentity,
			AllowLocalRemote: true,
		},
		BotDate: testSignature.Date,
	}
}

// publishControlPlane makes the setup-derived branch visible to the remote,
// matching the normal first synchronization after repository setup.
func (d *destination) publishControlPlane(ctx context.Context, t *testing.T) {
	t.Helper()
	if err := d.git.PushAtomic(ctx, d.remote, []gitcli.PushUpdate{{
		Ref:          testBranchRef,
		New:          d.parent,
		ExpectAbsent: true,
	}}); err != nil {
		t.Fatalf("publish control-plane branch: %v", err)
	}
}

// remoteRefs reports what the destination remote actually holds, as a map from
// ref name to object name.
//
// It reads the remote repository rather than the local one, because what a
// publication did is a fact about the remote and a local ref would be a memory
// of what this process intended.
func (d *destination) remoteRefs(ctx context.Context, t *testing.T) map[string]string {
	t.Helper()
	repository, err := d.git.WithDir(d.remote)
	if err != nil {
		t.Fatalf("open the remote: %v", err)
	}
	refs, err := repository.ListRefs(ctx)
	if err != nil {
		t.Fatalf("list remote refs: %v", err)
	}
	held := make(map[string]string, len(refs))
	for _, ref := range refs {
		held[ref.Name] = ref.Target
	}
	return held
}

// testConfig is the destination profile every fixture publishes under.
//
// It is a value rather than a decoded document because these tests exercise
// what a synchronization does with a validated profile, not whether the decoder
// accepts one: the decoder has its own tests, and routing every case here
// through five kilobytes of YAML would make a failure in the publication rules
// read as a failure to write a profile. The fields are exactly the ones this
// package consumes, spelled the way the decoder produces them, which is why the
// branch is a short name and the state ref is fully qualified.
func testConfig() *config.Config {
	return &config.Config{
		Source: config.Source{
			Repository: "https://github.com/kubernetes/kubernetes.git",
		},
		Destination: config.Destination{
			Module:            testModulePath,
			Repository:        testRepository,
			Remote:            "https://github.com/" + testRepository + ".git",
			Branch:            testBranch,
			StateRef:          testStateRef,
			ProgressRefPrefix: testProgressRef,
			InternalPrefix:    "internal/kk",
		},
		Facade:  config.Facade{File: "rbac.go", AssertionsFile: "zz_generated_assertions.go"},
		Release: config.Release{Policy: config.ReleasePolicyV1ToV0},
		Commit: config.Commit{
			Committer:  config.Identity{Name: testSignature.Name, Email: testSignature.Email},
			TrailerKey: "Kubernetes-commit",
		},
		Determinism: config.Determinism{Toolchain: "go1.26.5"},
	}
}

// testRelease is the upstream release every fixture publishes.
func testRelease() sync.Release {
	return sync.Release{
		Tag:    testSourceTag,
		Ref:    testSourceRef,
		Commit: testSourceCommit,
		Tagger: gitcli.Signature{
			Name:  "Kubernetes Release Robot",
			Email: "k8s-release-robot@users.noreply.github.com",
			Date:  "1700000000 +0000",
		},
		URL: "https://github.com/kubernetes/kubernetes/releases/tag/" + testSourceTag,
		Author: gitcli.Signature{
			Name:  "Upstream Author",
			Email: "author@kubernetes.invalid",
			Date:  "1699999999 +0000",
		},
		CommitterDate: "1699999999 +0000",
		Message:       "Release " + testSourceTag + "\n",
	}
}

// testModule is a small generated module and the report describing it.
//
// The files are real content written as real blobs, which is what the tree, the
// commit, and every object name derived from them depend on. They are small
// rather than a copy of a generated Kubernetes module because what these tests
// measure is what a synchronization does with a module, and a larger one would
// change only how long that takes.
func testModule() sync.Module {
	return sync.Module{
		Files: relocate.FileSet{
			Files: []relocate.File{
				file("LICENSE", "Apache License 2.0\n"),
				file("go.mod", "module "+testModulePath+"\n\ngo 1.26\n"),
				file("internal/kk/rbac/rbac.go", "package rbac\n\n// Authorize decides.\nfunc Authorize() bool { return true }\n"),
				file("rbac.go", "package rbacauthorizer\n\n// Authorize decides.\nfunc Authorize() bool { return true }\n"),
			},
			Packages: []relocate.Package{
				{Source: ".", Path: "."},
				{Source: "plugin/pkg/auth/authorizer/rbac", Path: "internal/kk/rbac"},
			},
		},
		Report: testReport(),
	}
}

func file(path, contents string) relocate.File {
	return relocate.File{
		Path:     path,
		Package:  filepath.ToSlash(filepath.Dir(path)),
		Mode:     relocate.ModeRegular,
		Contents: []byte(contents),
	}
}

// testReport is the generation report a synchronization summarizes.
//
// Only the fields the manifest reads are populated. The rest of a real report
// runs to thousands of lines answering a different question, and filling it in
// here would say that the manifest depends on more of it than it does.
func testReport() generate.Report {
	return generate.Report{
		Engine: generate.EngineReport{
			Toolchain:   "go1.26.5",
			ProfileHash: testProfileHash,
		},
		Source: generate.SourceReport{
			RefKind:    "tag",
			RefName:    testSourceTag,
			Commit:     testSourceCommit,
			ReleaseTag: testReleaseTag,
		},
		Extract: generate.ExtractReport{Post: generate.PassReport{
			PrunedFiles:   []string{"plugin/pkg/auth/authorizer/rbac/rbac_test.go"},
			DeniedImports: []string{"k8s.io/kubernetes/pkg/api/legacyscheme"},
		}},
		Dependencies: generate.DependencyReport{
			Policy: config.DependencyPolicyExternal,
			Totals: generate.TotalsReport{Candidates: 2, Refused: 2},
		},
		Provenance: generate.ProvenanceReport{
			LicenseID: "Apache-2.0",
			PublicAPI: []string{"Authorize", "New"},
			BehaviorChanges: []generate.BehaviorChangeReport{
				{Summary: "the authorizer no longer registers metrics", Cause: "prune"},
			},
		},
		Output: generate.OutputReport{
			Module:       testModulePath,
			Files:        4,
			Packages:     2,
			ManifestHash: "sha256:" + strings.Repeat("ab", 32),
		},
		Notices: []string{"the upstream commit carried no NOTICE"},
	}
}
