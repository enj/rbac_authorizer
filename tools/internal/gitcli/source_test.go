package gitcli_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/gitcli"
)

func TestValidateSourceRemote(t *testing.T) {
	tests := []struct {
		name    string
		remote  string
		wantErr string
	}{
		{name: "public source", remote: "https://github.com/kubernetes/kubernetes.git"},
		{name: "absolute path", remote: "/srv/mirror/kubernetes.git"},
		{name: "file url", remote: "file:///srv/mirror/kubernetes.git"},
		{
			name:    "another host",
			remote:  "https://gitlab.example.com/kubernetes/kubernetes.git",
			wantErr: "must fetch source from github.com",
		},
		{
			// A lookalike host is the whole reason the allowlist exists.
			name:    "host suffix lookalike",
			remote:  "https://github.com.evil.example/kubernetes/kubernetes.git",
			wantErr: "must fetch source from github.com",
		},
		{
			name:    "embedded credentials",
			remote:  "https://token@github.com/kubernetes/kubernetes.git",
			wantErr: "must not embed credentials",
		},
		{
			name:    "named remote",
			remote:  "origin",
			wantErr: "hides its target in configuration",
		},
		{name: "ssh remote", remote: "git@github.com:kubernetes/kubernetes.git", wantErr: "must be a remote name"},
		{name: "unsupported scheme", remote: "http://github.com/kubernetes/kubernetes.git", wantErr: "must use https or file"},
		{name: "git protocol", remote: "git://github.com/kubernetes/kubernetes.git", wantErr: "must use https or file"},
		{name: "empty", remote: "", wantErr: "must not be empty"},
		{name: "flag like", remote: "--upload-pack=touch", wantErr: "must not start with a dash"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := gitcli.ValidateSourceRemote(test.remote)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error %q does not mention %q", err, test.wantErr)
			}
		})
	}
}

// TestValidateSourceRemoteNeverEchoesCredentials proves a rejected remote is
// reported without the secret it carried, because these messages reach logs and
// CI annotations.
func TestValidateSourceRemoteNeverEchoesCredentials(t *testing.T) {
	err := gitcli.ValidateSourceRemote("https://s3cr3t-token@evil.example/repo.git")
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "s3cr3t-token") {
		t.Fatalf("error %q echoes the credential", err)
	}
}

func TestValidateFetchRefspec(t *testing.T) {
	tests := []struct {
		name    string
		spec    string
		wantErr error
		message string
	}{
		{name: "branch", spec: "refs/heads/master:refs/heads/master"},
		{name: "tag", spec: "refs/tags/v1.36.1:refs/tags/v1.36.1"},
		{
			// A forced fetch would let an upstream history rewrite silently
			// replace what the engine already replayed from.
			name:    "forced",
			spec:    "+refs/heads/master:refs/heads/master",
			wantErr: gitcli.ErrForceRefspec,
		},
		{name: "delete", spec: ":refs/heads/master", wantErr: gitcli.ErrDeleteRefspec},
		{name: "flag like", spec: "-refs/heads/master:refs/heads/master", wantErr: gitcli.ErrFlagLikeArgument},
		{name: "wildcard source", spec: "refs/tags/*:refs/tags/*", message: "must not contain"},
		{name: "no destination", spec: "refs/heads/master:", message: "must name a destination ref"},
		{name: "no colon", spec: "refs/heads/master", message: "must be <source>:<destination>"},
		{name: "two colons", spec: "refs/heads/a:refs/heads/b:refs/heads/c", message: "exactly one colon"},
		{name: "whitespace", spec: "refs/heads/a refs/heads/b:refs/heads/c", message: "must not contain whitespace"},
		{name: "empty", spec: "", message: "must not be empty"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := gitcli.ValidateFetchRefspec(test.spec)
			if test.wantErr == nil && test.message == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected an error")
			}
			if test.wantErr != nil && !errors.Is(err, test.wantErr) {
				t.Fatalf("error %v is not %v", err, test.wantErr)
			}
			if test.message != "" && !strings.Contains(err.Error(), test.message) {
				t.Fatalf("error %q does not mention %q", err, test.message)
			}
		})
	}
}

// TestCloneSourceIsBlobless proves the cache really is a partial clone: git
// records the promisor remote and the filter it was created with, and a full
// clone would record neither.
func TestCloneSourceIsBlobless(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)
	root := t.TempDir()
	runner := newAnonymousRunner(t, root)

	dir := filepath.Join(root, "cache.git")
	if err := runner.CloneSource(ctx, gitcli.SourceCloneOptions{
		Remote:    up.url(),
		Directory: dir,
		Bare:      true,
	}); err != nil {
		t.Fatalf("clone: %v", err)
	}

	cache, err := runner.WithDir(dir)
	if err != nil {
		t.Fatalf("scope runner to the cache: %v", err)
	}
	bare, err := cache.IsBareRepository(ctx)
	if err != nil {
		t.Fatalf("bare probe: %v", err)
	}
	if !bare {
		t.Fatal("source cache must be bare")
	}
	for key, want := range map[string]string{
		"remote.origin.promisor":           "true",
		"remote.origin.partialclonefilter": gitcli.BloblessFilter,
	} {
		got, found, err := cache.ConfigLocal(ctx, key)
		if err != nil {
			t.Fatalf("read %s: %v", key, err)
		}
		if !found || got != want {
			t.Fatalf("%s = %q (found %v), want %q", key, got, found, want)
		}
	}

	// The whole history arrived even though its blobs did not.
	refs, err := cache.ListRefs(ctx, "refs/heads/*")
	if err != nil {
		t.Fatalf("list refs: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("cloned %d branches, want 2", len(refs))
	}
}

func TestCloneSourceRejectsCredentialedRunner(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)
	root := t.TempDir()

	// A runner carrying an environment entry may be carrying a credential, and
	// the source host must never see one.
	credentialed, err := gitcli.New(ctx, gitcli.Options{
		Dir:     root,
		Inherit: []string{"PATH"},
		Env:     []string{"GIT_CONFIG_COUNT=0"},
	})
	if err != nil {
		t.Fatalf("create runner: %v", err)
	}
	if credentialed.IsAnonymous() {
		t.Fatal("a runner with Env entries must not be anonymous")
	}

	cloneErr := credentialed.CloneSource(ctx, gitcli.SourceCloneOptions{
		Remote:    up.url(),
		Directory: filepath.Join(root, "cache.git"),
		Bare:      true,
	})
	if !errors.Is(cloneErr, gitcli.ErrCredentialedRunner) {
		t.Fatalf("error %v is not a credentialed runner refusal", cloneErr)
	}
	fetchErr := credentialed.FetchSource(ctx, gitcli.SourceFetchOptions{
		Remote:   up.url(),
		Refspecs: []string{"refs/heads/main:refs/heads/main"},
	})
	if !errors.Is(fetchErr, gitcli.ErrCredentialedRunner) {
		t.Fatalf("error %v is not a credentialed runner refusal", fetchErr)
	}

	// Stripping the entries is what makes the runner usable again.
	if err := credentialed.Anonymous().CloneSource(ctx, gitcli.SourceCloneOptions{
		Remote:    up.url(),
		Directory: filepath.Join(root, "anonymous.git"),
		Bare:      true,
	}); err != nil {
		t.Fatalf("clone with an anonymous runner: %v", err)
	}
}

func TestCloneSourceRejectsDisallowedRemotes(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	runner := newAnonymousRunner(t, root)

	tests := []struct {
		name   string
		remote string
		dir    string
		want   string
	}{
		{
			name:   "another host",
			remote: "https://evil.example/kubernetes/kubernetes.git",
			dir:    filepath.Join(root, "a.git"),
			want:   "must fetch source from github.com",
		},
		{
			name:   "relative directory",
			remote: "file:///srv/mirror.git",
			dir:    "relative/cache.git",
			want:   "must be absolute",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := runner.CloneSource(ctx, gitcli.SourceCloneOptions{
				Remote:    test.remote,
				Directory: test.dir,
				Bare:      true,
			})
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error %q does not mention %q", err, test.want)
			}
			if _, statErr := os.Stat(test.dir); statErr == nil {
				t.Fatal("a rejected clone must not create its directory")
			}
		})
	}
}

// TestFetchSourceRefusesRewrittenHistory is the fail-closed behaviour that
// protects published history: upstream rewound a branch, and because no refspec
// may be forced, the cache refuses the update instead of adopting it.
func TestFetchSourceRefusesRewrittenHistory(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)
	root := t.TempDir()
	runner := newAnonymousRunner(t, root)

	dir := filepath.Join(root, "cache.git")
	if err := runner.CloneSource(ctx, gitcli.SourceCloneOptions{Remote: up.url(), Directory: dir, Bare: true}); err != nil {
		t.Fatalf("clone: %v", err)
	}
	cache, err := runner.WithDir(dir)
	if err != nil {
		t.Fatalf("scope runner to the cache: %v", err)
	}
	spec := "refs/heads/" + mainBranch + ":refs/heads/" + mainBranch

	// A fast forward is accepted.
	added := up.commit(ctx, t, "extra.go", "package extra\n", "feat: add extra\n")
	up.updateRef(ctx, t, "refs/heads/"+mainBranch, added)
	if err := cache.FetchSource(ctx, gitcli.SourceFetchOptions{Remote: up.url(), Refspecs: []string{spec}}); err != nil {
		t.Fatalf("fast forward fetch: %v", err)
	}
	if got, err := cache.ResolveCommit(ctx, "refs/heads/"+mainBranch); err != nil || got != added {
		t.Fatalf("cached branch = %q (%v), want %q", got, err, added)
	}

	// Rewinding the branch upstream must not rewind the cache.
	up.updateRef(ctx, t, "refs/heads/"+mainBranch, up.sha(mainOne))
	err = cache.FetchSource(ctx, gitcli.SourceFetchOptions{Remote: up.url(), Refspecs: []string{spec}})
	if err == nil {
		t.Fatal("expected a rewritten branch to be refused")
	}
	if !strings.Contains(err.Error(), "non-fast-forward") && !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("error %q does not explain the rejection", err)
	}
	if got, resolveErr := cache.ResolveCommit(ctx, "refs/heads/"+mainBranch); resolveErr != nil || got != added {
		t.Fatalf("cache moved to %q (%v), want it to stay at %q", got, resolveErr, added)
	}
}

func TestFetchSourceTags(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)
	root := t.TempDir()
	runner := newAnonymousRunner(t, root)

	dir := filepath.Join(root, "cache.git")
	// Cloning without tags leaves the tag namespace empty, so the explicit
	// tag fetch below is what populates it.
	if err := runner.CloneSource(ctx, gitcli.SourceCloneOptions{Remote: up.url(), Directory: dir, Bare: true}); err != nil {
		t.Fatalf("clone: %v", err)
	}
	cache, err := runner.WithDir(dir)
	if err != nil {
		t.Fatalf("scope runner to the cache: %v", err)
	}

	newTag := "v1.36.2"
	up.tag(ctx, t, gitcli.TagOptions{
		Name:    newTag,
		Commit:  up.sha(release),
		Message: "Kubernetes v1.36.2\n",
		Tagger:  gitcli.Signature{Name: testUserName, Email: testUserEmail, Date: "2026-02-03T04:05:06Z"},
	})
	if err := cache.FetchSource(ctx, gitcli.SourceFetchOptions{
		Remote:   up.url(),
		Refspecs: []string{"refs/heads/" + releaseBranch + ":refs/heads/" + releaseBranch},
		Tags:     true,
	}); err != nil {
		t.Fatalf("fetch tags: %v", err)
	}

	present, err := cache.HasRef(ctx, "refs/tags/"+newTag)
	if err != nil {
		t.Fatalf("ref probe: %v", err)
	}
	if !present {
		t.Fatal("the new tag was not fetched")
	}
}

func TestFetchSourceRequiresRefspecs(t *testing.T) {
	ctx := t.Context()
	runner := newAnonymousRunner(t, t.TempDir())
	err := runner.FetchSource(ctx, gitcli.SourceFetchOptions{Remote: "https://github.com/kubernetes/kubernetes.git"})
	if err == nil || !strings.Contains(err.Error(), "at least one refspec") {
		t.Fatalf("error %v does not require a refspec", err)
	}
}

// TestAnonymousDropsCallerEnvironment proves the stripping is real rather than a
// flag: an entry that reached the runner through Options.Env no longer reaches
// the subprocess afterwards. GIT_AUTHOR_NAME stands in for a credential because
// its effect is directly observable in the resulting commit.
func TestAnonymousDropsCallerEnvironment(t *testing.T) {
	ctx := t.Context()
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()

	const injected = "Injected Identity"
	credentialed, err := gitcli.New(ctx, gitcli.Options{
		Dir:     dir,
		Inherit: []string{"PATH", "HOME"},
		Env:     []string{"GIT_AUTHOR_NAME=" + injected},
	})
	if err != nil {
		t.Fatalf("create runner: %v", err)
	}
	if err := credentialed.InitRepository(ctx, mainBranch); err != nil {
		t.Fatalf("init: %v", err)
	}
	for key, value := range map[string]string{"user.name": testUserName, "user.email": testUserEmail} {
		if err := credentialed.SetConfigLocal(ctx, key, value); err != nil {
			t.Fatalf("set %s: %v", key, err)
		}
	}

	if err := credentialed.Commit(ctx, gitcli.CommitOptions{Message: "chore: with env\n", AllowEmpty: true}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	withEnv, err := credentialed.ResolveCommit(ctx, "HEAD")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	anonymous := credentialed.Anonymous()
	if err := anonymous.Commit(ctx, gitcli.CommitOptions{Message: "chore: without env\n", AllowEmpty: true}); err != nil {
		t.Fatalf("anonymous commit: %v", err)
	}
	withoutEnv, err := anonymous.ResolveCommit(ctx, "HEAD")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	// The entry applied while it was present. The identity is read back exactly
	// as it was recorded, because a commit identity is replayed content rather
	// than diagnostics and is not passed through the redactor.
	before, err := anonymous.CommitInfo(ctx, withEnv)
	if err != nil {
		t.Fatalf("commit info: %v", err)
	}
	if before.AuthorName != injected {
		t.Fatalf("author %q, want %q", before.AuthorName, injected)
	}

	// The redactor still carries the value even though this runner can no longer
	// see the entry, which is what keeps it out of diagnostics after stripping.
	if anonymous.Redactor().String(injected) != gitcli.Placeholder {
		t.Fatal("Anonymous dropped the redactor along with the environment")
	}

	// After stripping, the identity comes from repository configuration again.
	after, err := anonymous.CommitInfo(ctx, withoutEnv)
	if err != nil {
		t.Fatalf("commit info: %v", err)
	}
	if after.AuthorName != testUserName {
		t.Fatalf("author %q, want %q", after.AuthorName, testUserName)
	}
	if strings.Contains(after.AuthorName, injected) {
		t.Fatal("the caller supplied entry still reaches the subprocess")
	}
}

func TestListRefsAndHasRef(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)
	git := up.repo.Git

	refs, err := git.ListRefs(ctx, "refs/tags/*")
	if err != nil {
		t.Fatalf("list refs: %v", err)
	}
	byName := make(map[string]gitcli.Ref, len(refs))
	for _, ref := range refs {
		byName[ref.Name] = ref
	}

	annotated, ok := byName["refs/tags/"+annotatedTag]
	if !ok {
		t.Fatalf("annotated tag is missing from %v", refs)
	}
	if !annotated.Annotated() {
		t.Fatal("an annotated tag must report a tag object")
	}
	if annotated.Target == annotated.Commit {
		t.Fatal("an annotated tag's object differs from the commit it peels to")
	}
	if annotated.Commit != up.sha(release) {
		t.Fatalf("annotated tag peels to %q, want %q", annotated.Commit, up.sha(release))
	}

	light, ok := byName["refs/tags/"+lightweightTag]
	if !ok {
		t.Fatalf("lightweight tag is missing from %v", refs)
	}
	if light.Annotated() {
		t.Fatal("a lightweight tag must not report a tag object")
	}
	if light.Commit != up.sha(base) || light.Target != up.sha(base) {
		t.Fatalf("lightweight tag = %q/%q, want %q", light.Target, light.Commit, up.sha(base))
	}

	// Ref listing is ordered by name, so a report built from it is stable.
	names := make([]string, 0, len(refs))
	for _, ref := range refs {
		names = append(names, ref.Name)
	}
	if !isSorted(names) {
		t.Fatalf("refs %v are not ordered", names)
	}

	for name, want := range map[string]bool{
		"refs/heads/" + mainBranch: true,
		"refs/heads/absent":        false,
	} {
		got, err := git.HasRef(ctx, name)
		if err != nil {
			t.Fatalf("ref probe %s: %v", name, err)
		}
		if got != want {
			t.Fatalf("HasRef(%q) = %v, want %v", name, got, want)
		}
	}
	if _, err := git.HasRef(ctx, "not-hierarchical"); err == nil {
		t.Fatal("expected an error for a ref name that is not fully qualified")
	}
}

func TestIsBareRepositoryOutsideRepository(t *testing.T) {
	ctx := t.Context()
	runner := newAnonymousRunner(t, t.TempDir())
	bare, err := runner.IsBareRepository(ctx)
	if err != nil {
		t.Fatalf("bare probe outside a repository: %v", err)
	}
	if bare {
		t.Fatal("a plain directory is not a bare repository")
	}
}

// isSorted reports whether values are in non-decreasing order.
func isSorted(values []string) bool {
	for i := 1; i < len(values); i++ {
		if values[i-1] > values[i] {
			return false
		}
	}
	return true
}
