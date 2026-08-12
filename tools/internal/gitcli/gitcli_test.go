package gitcli_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/testsupport"
)

const (
	testUserName  = "Soapbox Test"
	testUserEmail = "test@example.com"
)

func TestRunnerVersion(t *testing.T) {
	ctx := t.Context()
	runner := newRunner(t, t.TempDir())

	version, err := runner.Version(ctx)
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if !version.AtLeast(gitcli.Version{Major: 2}) {
		t.Fatalf("git version %v is implausible", version)
	}
	if !filepath.IsAbs(runner.Binary()) {
		t.Fatalf("binary %q is not absolute", runner.Binary())
	}
}

func TestNewRejectsMissingDirectory(t *testing.T) {
	_, err := gitcli.New(t.Context(), gitcli.Options{Dir: filepath.Join(t.TempDir(), "absent")})
	if err == nil {
		t.Fatal("expected an error for a missing working directory")
	}
	if !strings.Contains(err.Error(), "git working directory") {
		t.Fatalf("error %q is not a working directory error", err)
	}
}

func TestNewRejectsMissingBinary(t *testing.T) {
	_, err := gitcli.New(t.Context(), gitcli.Options{Binary: "soapbox-git-does-not-exist"})
	if err == nil {
		t.Fatal("expected an error for a missing git executable")
	}
	if !strings.Contains(err.Error(), "git executable lookup") {
		t.Fatalf("error %v is not an executable lookup failure", err)
	}
}

func TestRunnerCanceledContext(t *testing.T) {
	runner := newRunner(t, t.TempDir())

	tests := []struct {
		name    string
		context func(t *testing.T) context.Context
		want    error
	}{
		{
			name: "canceled",
			context: func(t *testing.T) context.Context {
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				return ctx
			},
			want: context.Canceled,
		},
		{
			name: "expired deadline",
			context: func(t *testing.T) context.Context {
				ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Hour))
				t.Cleanup(cancel)
				return ctx
			},
			want: context.DeadlineExceeded,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := runner.Version(test.context(t))
			if !errors.Is(err, test.want) {
				t.Fatalf("error %v is not %v", err, test.want)
			}
		})
	}
}

func TestRunnerRepositoryLifecycle(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()

	runner := newRunner(t, dir)
	isRepo, err := runner.IsRepository(ctx)
	if err != nil {
		t.Fatalf("probe empty directory: %v", err)
	}
	if isRepo {
		t.Fatal("an empty directory reported as a repository")
	}

	repo := testsupport.NewRepo(ctx, t, testsupport.Options{
		Dir:       dir,
		Branch:    "main",
		UserName:  testUserName,
		UserEmail: testUserEmail,
	})

	isRepo, err = repo.Git.IsRepository(ctx)
	if err != nil || !isRepo {
		t.Fatalf("IsRepository() = %t, %v", isRepo, err)
	}
	hasHead, err := repo.Git.HasHead(ctx)
	if err != nil {
		t.Fatalf("HasHead on empty repository: %v", err)
	}
	if hasHead {
		t.Fatal("a repository without commits reported a HEAD commit")
	}

	sha := repo.WriteAndCommit(ctx, t, "README.md", "soapbox\n", "docs: add readme\n")
	hasHead, err = repo.Git.HasHead(ctx)
	if err != nil || !hasHead {
		t.Fatalf("HasHead after the first commit = %t, %v", hasHead, err)
	}
	resolved, err := repo.Git.ResolveCommit(ctx, "HEAD")
	if err != nil {
		t.Fatalf("resolve HEAD: %v", err)
	}
	if resolved != sha {
		t.Fatalf("ResolveCommit(HEAD) = %q, want %q", resolved, sha)
	}
	root, err := repo.Git.RepositoryRoot(ctx)
	if err != nil {
		t.Fatalf("repository root: %v", err)
	}
	if filepath.Base(root) != filepath.Base(dir) {
		t.Fatalf("repository root %q does not match %q", root, dir)
	}
}

func TestRunnerResolveCommitRejectsUnknownRevision(t *testing.T) {
	ctx := t.Context()
	repo := testsupport.NewRepo(ctx, t, testsupport.Options{UserName: testUserName, UserEmail: testUserEmail})
	repo.WriteAndCommit(ctx, t, "a.txt", "a\n", "feat: a\n")

	tests := []struct {
		name     string
		revision string
		wantErr  string
	}{
		{name: "unknown ref", revision: "refs/heads/absent", wantErr: "git revision"},
		{name: "option like", revision: "--all", wantErr: "must not start with a dash"},
		{name: "empty", revision: "", wantErr: "must not be empty"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := repo.Git.ResolveCommit(ctx, test.revision)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error %q does not contain %q", err, test.wantErr)
			}
		})
	}

	_, err := repo.Git.ResolveCommit(ctx, "refs/heads/absent")
	if code := gitcli.ExitCodeOf(err); code == 0 {
		t.Fatalf("expected a non zero git exit code, got %d in %v", code, err)
	}
}

func TestRunnerConfigLocal(t *testing.T) {
	ctx := t.Context()
	repo := testsupport.NewRepo(ctx, t, testsupport.Options{UserName: testUserName, UserEmail: testUserEmail})

	value, found, err := repo.Git.ConfigLocal(ctx, "user.name")
	if err != nil || !found || value != testUserName {
		t.Fatalf("ConfigLocal(user.name) = %q, %t, %v", value, found, err)
	}

	value, found, err = repo.Git.ConfigLocal(ctx, "soapbox.absent")
	if err != nil {
		t.Fatalf("missing key returned an error: %v", err)
	}
	if found || value != "" {
		t.Fatalf("ConfigLocal(soapbox.absent) = %q, %t", value, found)
	}

	if err := repo.Git.SetConfigLocal(ctx, "soapbox.value", "-dangerous"); err == nil {
		t.Fatal("expected option like config values to be rejected")
	}
}

func TestRunnerCommitInfo(t *testing.T) {
	ctx := t.Context()
	repo := testsupport.NewRepo(ctx, t, testsupport.Options{UserName: testUserName, UserEmail: testUserEmail})

	message := "feat(rbac): add authorizer\n\nBody line.\n\nKubernetes-commit: 1234567890abcdef\nSigned-off-by: Monis Khan <mok@microsoft.com>\n"
	repo.WriteFile(t, "authorizer.go", "package rbacauthorizer\n")
	sha := repo.Commit(ctx, t, message, gitcli.CommitOptions{
		Author:    gitcli.Signature{Name: "Upstream Author", Email: "upstream@example.com", Date: "2026-01-02T03:04:05+00:00"},
		Committer: gitcli.Signature{Name: "soapbox[bot]", Email: "bot@example.com", Date: "2026-01-02T03:04:05+00:00"},
	}, "authorizer.go")

	commit, err := repo.Git.CommitInfo(ctx, "HEAD")
	if err != nil {
		t.Fatalf("commit info: %v", err)
	}
	if commit.SHA != sha {
		t.Fatalf("SHA = %q, want %q", commit.SHA, sha)
	}
	if got, want := commit.AuthorIdentity(), "Upstream Author <upstream@example.com>"; got != want {
		t.Fatalf("author = %q, want %q", got, want)
	}
	if got, want := commit.CommitterIdentity(), "soapbox[bot] <bot@example.com>"; got != want {
		t.Fatalf("committer = %q, want %q", got, want)
	}
	if commit.Subject != "feat(rbac): add authorizer" {
		t.Fatalf("subject = %q", commit.Subject)
	}
	if commit.SignatureStatus != "N" {
		t.Fatalf("signature status = %q, want N for an unsigned commit", commit.SignatureStatus)
	}
	if got := commit.TrailerValues("Kubernetes-commit"); len(got) != 1 || got[0] != "1234567890abcdef" {
		t.Fatalf("Kubernetes-commit trailers = %v", got)
	}
	if got := commit.TrailerValues("signed-off-by"); len(got) != 1 || got[0] != "Monis Khan <mok@microsoft.com>" {
		t.Fatalf("Signed-off-by trailers = %v", got)
	}
	if !strings.Contains(commit.RawMessage, "Body line.") {
		t.Fatalf("raw message = %q", commit.RawMessage)
	}
	if !strings.HasPrefix(commit.RawMessage, commit.Subject) {
		t.Fatalf("raw message %q does not start with the subject %q", commit.RawMessage, commit.Subject)
	}
	if !strings.HasPrefix(commit.AuthorDate, "2026-01-02T03:04:05") {
		t.Fatalf("author date = %q", commit.AuthorDate)
	}
}

func TestRunnerDropsAmbientEnvironment(t *testing.T) {
	ctx := t.Context()
	t.Setenv("GIT_AUTHOR_NAME", "Ambient Identity")
	t.Setenv("GIT_AUTHOR_EMAIL", "ambient@example.com")

	repo := testsupport.NewRepo(ctx, t, testsupport.Options{UserName: testUserName, UserEmail: testUserEmail})
	repo.WriteAndCommit(ctx, t, "a.txt", "a\n", "feat: a\n")

	commit, err := repo.Git.CommitInfo(ctx, "HEAD")
	if err != nil {
		t.Fatalf("commit info: %v", err)
	}
	if got, want := commit.AuthorIdentity(), gitcli.Identity(testUserName, testUserEmail); got != want {
		t.Fatalf("author = %q, want %q, ambient environment leaked into the subprocess", got, want)
	}
}

func TestRunnerRedactsFailureOutput(t *testing.T) {
	ctx := t.Context()
	const secret = "ghs_SUPERSECRETTOKEN"
	repo := testsupport.NewRepo(ctx, t, testsupport.Options{
		UserName:  testUserName,
		UserEmail: testUserEmail,
		Secrets:   []string{secret},
	})

	_, err := repo.Git.ResolveCommit(ctx, "refs/heads/"+secret)
	if err == nil {
		t.Fatal("expected an error for an unknown revision")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error %q leaks the secret", err)
	}
	if !strings.Contains(err.Error(), gitcli.Placeholder) {
		t.Fatalf("error %q has no redaction placeholder", err)
	}
}

func TestRunnerPush(t *testing.T) {
	ctx := t.Context()
	origin := testsupport.NewRepo(ctx, t, testsupport.Options{UserName: testUserName, UserEmail: testUserEmail})
	origin.WriteAndCommit(ctx, t, "seed.txt", "seed\n", "chore: seed\n")

	source := testsupport.NewRepo(ctx, t, testsupport.Options{UserName: testUserName, UserEmail: testUserEmail})
	source.WriteAndCommit(ctx, t, "a.txt", "a\n", "feat: a\n")

	if err := source.Git.Push(ctx, origin.Dir, "refs/heads/main:refs/heads/incoming"); err != nil {
		t.Fatalf("push: %v", err)
	}
	if _, err := origin.Git.ResolveCommit(ctx, "refs/heads/incoming"); err != nil {
		t.Fatalf("pushed ref is missing: %v", err)
	}

	t.Run("force refspec is rejected before git runs", func(t *testing.T) {
		err := source.Git.Push(ctx, origin.Dir, "+refs/heads/main:refs/heads/incoming")
		if !errors.Is(err, gitcli.ErrForceRefspec) {
			t.Fatalf("error %v is not %v", err, gitcli.ErrForceRefspec)
		}
	})

	t.Run("delete refspec is rejected before git runs", func(t *testing.T) {
		err := source.Git.Push(ctx, origin.Dir, ":refs/heads/incoming")
		if !errors.Is(err, gitcli.ErrDeleteRefspec) {
			t.Fatalf("error %v is not %v", err, gitcli.ErrDeleteRefspec)
		}
	})

	t.Run("credentials in the remote are rejected", func(t *testing.T) {
		const token = "ghs_token"
		err := source.Git.Push(ctx, "https://"+token+"@github.com/enj/x.git", "refs/heads/main:refs/heads/main")
		if err == nil || !strings.Contains(err.Error(), "must not embed credentials") {
			t.Fatalf("error %v does not reject embedded credentials", err)
		}
		if strings.Contains(err.Error(), token) {
			t.Fatalf("error %q echoes the credential", err)
		}
	})

	t.Run("no refspec is rejected", func(t *testing.T) {
		if err := source.Git.Push(ctx, origin.Dir); err == nil {
			t.Fatal("expected an error when no refspec is given")
		}
	})

	t.Run("non fast forward is rejected by git", func(t *testing.T) {
		unrelated := testsupport.NewRepo(ctx, t, testsupport.Options{UserName: testUserName, UserEmail: testUserEmail})
		unrelated.WriteAndCommit(ctx, t, "b.txt", "b\n", "feat: b\n")

		err := unrelated.Git.Push(ctx, origin.Dir, "refs/heads/main:refs/heads/incoming")
		if err == nil {
			t.Fatal("expected git to reject a non fast forward update")
		}
		if !strings.Contains(strings.ToLower(err.Error()), "rejected") &&
			!strings.Contains(strings.ToLower(err.Error()), "fetch first") &&
			!strings.Contains(strings.ToLower(err.Error()), "non-fast-forward") {
			t.Fatalf("error %q does not look like a rejected push", err)
		}
	})
}

func TestRunnerWithDir(t *testing.T) {
	ctx := t.Context()
	first := t.TempDir()
	second := t.TempDir()

	runner := newRunner(t, first)
	moved, err := runner.WithDir(second)
	if err != nil {
		t.Fatalf("move runner: %v", err)
	}
	if runner.Dir() != first {
		t.Fatalf("original runner directory changed to %q", runner.Dir())
	}
	if moved.Dir() != second {
		t.Fatalf("moved runner directory = %q, want %q", moved.Dir(), second)
	}
	if _, err := moved.IsRepository(ctx); err != nil {
		t.Fatalf("probe moved runner: %v", err)
	}
}

// TestRunnerWithDirValidatesTheDirectory proves a runner cannot be pointed at a
// path that does not exist or is not a directory. Without the check the first
// command would silently run in the process working directory, which is
// whatever directory the operator happened to start the engine from.
func TestRunnerWithDirValidatesTheDirectory(t *testing.T) {
	root := t.TempDir()
	runner := newRunner(t, root)

	file := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	tests := []struct {
		name string
		dir  string
	}{
		{name: "missing", dir: filepath.Join(root, "absent")},
		{name: "not a directory", dir: file},
		{name: "relative", dir: "relative/path"},
		{name: "empty", dir: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := runner.WithDir(test.dir); err == nil {
				t.Fatalf("WithDir(%q) must not accept the directory", test.dir)
			}
		})
	}
}

// TestRunnerWithDirDoesNotDiscoverAncestorRepositories proves a runner scoped to
// an exact directory never acts on a repository above it. A cache directory that
// was emptied would otherwise hand every later command whatever repository
// happens to contain it.
func TestRunnerWithDirDoesNotDiscoverAncestorRepositories(t *testing.T) {
	ctx := t.Context()
	ancestor := testsupport.NewRepo(ctx, t, testsupport.Options{UserName: testUserName, UserEmail: testUserEmail})
	ancestor.WriteAndCommit(ctx, t, "a.txt", "a\n", "feat: a\n")

	inside := filepath.Join(ancestor.Dir, "not-a-cache")
	if err := os.MkdirAll(inside, 0o750); err != nil {
		t.Fatalf("create directory: %v", err)
	}

	scoped, err := newRunner(t, ancestor.Dir).WithDir(inside)
	if err != nil {
		t.Fatalf("scope runner: %v", err)
	}
	repository, err := scoped.IsRepository(ctx)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if repository {
		t.Fatal("a directory that holds no repository must not report the repository above it")
	}
}

func TestRunnerAddPathsValidation(t *testing.T) {
	ctx := t.Context()
	repo := testsupport.NewRepo(ctx, t, testsupport.Options{UserName: testUserName, UserEmail: testUserEmail})

	if err := repo.Git.AddPaths(ctx); err == nil {
		t.Fatal("expected an error when no path is given")
	}
	if err := repo.Git.AddPaths(ctx, "--all"); err == nil {
		t.Fatal("expected option like paths to be rejected")
	}
	if err := repo.Git.Commit(ctx, gitcli.CommitOptions{}); err == nil {
		t.Fatal("expected an error when no message is given")
	}
}

// newRunner builds a runner with an isolated environment for dir.
func newRunner(t *testing.T, dir string) *gitcli.Runner {
	t.Helper()
	runner, err := gitcli.New(t.Context(), gitcli.Options{
		Dir:     dir,
		Inherit: []string{"PATH"},
		Env:     []string{"HOME=" + t.TempDir()},
	})
	if err != nil {
		t.Fatalf("create git runner: %v", err)
	}
	return runner
}

// TestMalformedEnvironmentEntryIsNotEchoed proves a rejected entry cannot leak
// the value it carries.
//
// An entry with no separator is entirely value, Env is where credentials
// arrive, and this validation runs in New, before the redactor that would have
// masked it exists. Quoting the entry there would be the one path by which a
// token reaches a log through this package.
//
// Each case also pins how the failing entry is identified, because a refusal
// the operator cannot act on is its own defect. Until a name is known that can
// only be the position; once one is known it is safe to print, since the caller
// chose it and it is not the secret.
func TestMalformedEnvironmentEntryIsNotEchoed(t *testing.T) {
	const secret = "ghs_averyprivatetoken"

	tests := []struct {
		name string
		opts gitcli.Options
		// want is the fragment that identifies the offending entry.
		want string
	}{
		{
			name: "environment entry",
			opts: gitcli.Options{Env: []string{"GIT_CONFIG_COUNT=0", secret}},
			want: "environment entry 1 must be KEY=VALUE",
		},
		{
			name: "isolation entry",
			opts: gitcli.Options{Isolation: []string{"HOME=/tmp", secret}},
			want: "isolation entry 1 must be KEY=VALUE",
		},
		{
			name: "environment entry with no name",
			opts: gitcli.Options{Env: []string{"GIT_CONFIG_COUNT=0", "=" + secret}},
			want: "environment entry 1 must name a variable",
		},
		{
			name: "environment entry with a null byte",
			opts: gitcli.Options{Env: []string{"SOAPBOX_TOKEN=" + secret + "\x00"}},
			want: `"SOAPBOX_TOKEN" must not contain a null byte`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := gitcli.New(t.Context(), test.opts)
			if err == nil {
				t.Fatal("a malformed entry was accepted")
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("the rejection echoed the value: %v", err)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error %q does not identify the entry as %q", err, test.want)
			}
		})
	}
}
