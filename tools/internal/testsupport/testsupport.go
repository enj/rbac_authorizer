// Package testsupport builds real temporary Git repositories for engine tests.
//
// Tests never mock the Git command line. They create throwaway repositories
// under a test temporary directory with global and system configuration
// disabled, so a run can neither read nor change the developer's real state.
package testsupport

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/enj/soapbox/tools/internal/gitcli"
)

// TB is the subset of testing.TB the helpers need. Depending on the interface
// instead of testing.TB keeps the testing package out of non test builds.
type TB interface {
	Helper()
	Fatalf(format string, args ...any)
	TempDir() string
}

// Options configures a temporary repository.
type Options struct {
	// Dir is the repository directory. Empty means a fresh temporary directory.
	Dir string
	// Branch is the initial branch name. Empty means main.
	Branch string
	// UserName and UserEmail seed the repository local identity. Empty values
	// are not written, which models a repository that is not configured yet.
	UserName  string
	UserEmail string
	// Secrets seed the runner redactor.
	Secrets []string
}

// Repo is a temporary repository and the runner that drives it.
type Repo struct {
	Dir  string
	Home string
	Git  *gitcli.Runner
}

// NewRepo creates an initialized repository with isolated configuration.
func NewRepo(ctx context.Context, tb TB, opts Options) *Repo {
	tb.Helper()

	dir := opts.Dir
	if dir == "" {
		dir = tb.TempDir()
	}
	home := tb.TempDir()
	branch := opts.Branch
	if branch == "" {
		branch = "main"
	}

	// The runner already neutralises global and system configuration for every
	// subprocess, so a temporary HOME only guards tools that read it directly.
	git, err := gitcli.New(ctx, gitcli.Options{
		Dir:     dir,
		Inherit: []string{"PATH"},
		Env:     []string{"HOME=" + home},
		Secrets: opts.Secrets,
	})
	if err != nil {
		tb.Fatalf("create git runner: %v", err)
	}
	if err := git.InitRepository(ctx, branch); err != nil {
		tb.Fatalf("init repository: %v", err)
	}

	repo := &Repo{Dir: dir, Home: home, Git: git}
	if opts.UserName != "" {
		repo.SetConfig(ctx, tb, "user.name", opts.UserName)
	}
	if opts.UserEmail != "" {
		repo.SetConfig(ctx, tb, "user.email", opts.UserEmail)
	}
	return repo
}

// SetConfig writes one repository local configuration value.
func (r *Repo) SetConfig(ctx context.Context, tb TB, key, value string) {
	tb.Helper()
	if err := r.Git.SetConfigLocal(ctx, key, value); err != nil {
		tb.Fatalf("set %s: %v", key, err)
	}
}

// WriteFile writes a repository relative file, creating parent directories.
func (r *Repo) WriteFile(tb TB, relPath, contents string) {
	tb.Helper()
	full := filepath.Join(r.Dir, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		tb.Fatalf("create directory for %s: %v", relPath, err)
	}
	if err := os.WriteFile(full, []byte(contents), 0o600); err != nil {
		tb.Fatalf("write %s: %v", relPath, err)
	}
}

// Commit stages the named paths and records a commit. Author and committer
// default to the repository identity when the signature is empty, and the
// commit is unsigned unless the options ask for a signature.
func (r *Repo) Commit(ctx context.Context, tb TB, message string, opts gitcli.CommitOptions, paths ...string) string {
	tb.Helper()
	if len(paths) > 0 {
		if err := r.Git.AddPaths(ctx, paths...); err != nil {
			tb.Fatalf("stage %s: %v", strings.Join(paths, " "), err)
		}
	}
	opts.Message = message
	if err := r.Git.Commit(ctx, opts); err != nil {
		tb.Fatalf("commit: %v", err)
	}
	sha, err := r.Git.ResolveCommit(ctx, "HEAD")
	if err != nil {
		tb.Fatalf("resolve HEAD: %v", err)
	}
	return sha
}

// WriteAndCommit writes one file and commits it.
func (r *Repo) WriteAndCommit(ctx context.Context, tb TB, relPath, contents, message string) string {
	tb.Helper()
	r.WriteFile(tb, relPath, contents)
	return r.Commit(ctx, tb, message, gitcli.CommitOptions{}, relPath)
}
