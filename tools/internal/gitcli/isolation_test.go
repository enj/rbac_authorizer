package gitcli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/testsupport"
)

// TestRunnerIgnoresBrokenAmbientConfiguration proves the isolation is real
// rather than configured only in tests: a global configuration file that git
// cannot parse would fail every command if it were read at all.
func TestRunnerIgnoresBrokenAmbientConfiguration(t *testing.T) {
	ctx := t.Context()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("this is not valid git configuration\n"), 0o600); err != nil {
		t.Fatalf("write global config: %v", err)
	}
	t.Setenv("HOME", home)

	// No Env override: the runner inherits the poisoned HOME the way production
	// would and must still neutralise the global file.
	dir := t.TempDir()
	runner, err := gitcli.New(ctx, gitcli.Options{Dir: dir})
	if err != nil {
		t.Fatalf("create runner: %v", err)
	}
	if _, err := runner.Version(ctx); err != nil {
		t.Fatalf("version with a broken global config: %v", err)
	}
	if err := runner.InitRepository(ctx, "main"); err != nil {
		t.Fatalf("init with a broken global config: %v", err)
	}
	if err := runner.SetConfigLocal(ctx, "user.name", "Soapbox Test"); err != nil {
		t.Fatalf("set identity: %v", err)
	}
	if err := runner.SetConfigLocal(ctx, "user.email", "test@example.com"); err != nil {
		t.Fatalf("set identity: %v", err)
	}
	if err := runner.Commit(ctx, gitcli.CommitOptions{Message: "chore: seed\n", AllowEmpty: true}); err != nil {
		t.Fatalf("commit with a broken global config: %v", err)
	}
}

// TestRunnerDoesNotRunRepositoryHooks proves that a hook checked into a
// repository, or inherited from ambient configuration, never executes.
func TestRunnerDoesNotRunRepositoryHooks(t *testing.T) {
	ctx := t.Context()
	repo := testsupport.NewRepo(ctx, t, testsupport.Options{UserName: testUserName, UserEmail: testUserEmail})

	// /bin/false is not a script: the hook is a bare interpreter reference that
	// always fails, so the commit can only succeed when hooks are disabled.
	if _, err := os.Stat("/bin/false"); err != nil {
		t.Skipf("/bin/false is unavailable: %v", err)
	}
	hooks := filepath.Join(repo.Dir, ".git", "hooks")
	if err := os.MkdirAll(hooks, 0o750); err != nil {
		t.Fatalf("create hooks directory: %v", err)
	}
	for _, name := range []string{"pre-commit", "commit-msg", "pre-push"} {
		if err := os.WriteFile(filepath.Join(hooks, name), []byte("#!/bin/false\n"), 0o700); err != nil {
			t.Fatalf("write %s hook: %v", name, err)
		}
	}

	repo.WriteFile(t, "a.txt", "a\n")
	repo.Commit(ctx, t, "feat: a\n", gitcli.CommitOptions{}, "a.txt")

	origin := testsupport.NewRepo(ctx, t, testsupport.Options{UserName: testUserName, UserEmail: testUserEmail})
	origin.WriteAndCommit(ctx, t, "seed.txt", "seed\n", "chore: seed\n")
	if err := repo.Git.Push(ctx, origin.Dir, "refs/heads/main:refs/heads/incoming"); err != nil {
		t.Fatalf("push with a failing pre-push hook present: %v", err)
	}
}

func TestRunnerRedactsValuesPassedThroughEnv(t *testing.T) {
	ctx := t.Context()
	const token = "ghs_ENVONLYTOKEN"
	dir := t.TempDir()

	// The token is only in Env, never in Secrets, so redaction must fail closed.
	runner, err := gitcli.New(ctx, gitcli.Options{
		Dir: dir,
		Env: []string{"SOAPBOX_TOKEN=" + token},
	})
	if err != nil {
		t.Fatalf("create runner: %v", err)
	}
	if err := runner.InitRepository(ctx, "main"); err != nil {
		t.Fatalf("init: %v", err)
	}

	_, resolveErr := runner.ResolveCommit(ctx, "refs/heads/"+token)
	if resolveErr == nil {
		t.Fatal("expected an error for an unknown revision")
	}
	if strings.Contains(resolveErr.Error(), token) {
		t.Fatalf("error %q leaks a token that was only passed through Env", resolveErr)
	}
	if !strings.Contains(resolveErr.Error(), gitcli.Placeholder) {
		t.Fatalf("error %q has no redaction placeholder", resolveErr)
	}
}

func TestIsRepositoryDistinguishesBenignAndFatalStates(t *testing.T) {
	ctx := t.Context()

	t.Run("empty directory is not a repository", func(t *testing.T) {
		runner := newRunner(t, t.TempDir())
		isRepo, err := runner.IsRepository(ctx)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if isRepo {
			t.Fatal("an empty directory reported as a repository")
		}
	})

	t.Run("broken git file is fatal", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("not a gitfile\n"), 0o600); err != nil {
			t.Fatalf("write .git: %v", err)
		}
		runner := newRunner(t, dir)
		if _, err := runner.IsRepository(ctx); err == nil {
			t.Fatal("a broken gitfile was reported as a plain missing repository")
		}
	})

	t.Run("dangling gitdir pointer is fatal", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(t.TempDir(), "absent")
		if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: "+target+"\n"), 0o600); err != nil {
			t.Fatalf("write .git: %v", err)
		}
		runner := newRunner(t, dir)
		if _, err := runner.IsRepository(ctx); err == nil {
			t.Fatal("a dangling gitdir pointer was reported as a plain missing repository")
		}
	})
}

func TestHasHeadDistinguishesMissingAndUnreadableHead(t *testing.T) {
	ctx := t.Context()

	t.Run("repository without commits", func(t *testing.T) {
		repo := testsupport.NewRepo(ctx, t, testsupport.Options{UserName: testUserName, UserEmail: testUserEmail})
		hasHead, err := repo.Git.HasHead(ctx)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if hasHead {
			t.Fatal("a repository without commits reported a HEAD commit")
		}
	})

	t.Run("head pointing at a missing object", func(t *testing.T) {
		repo := testsupport.NewRepo(ctx, t, testsupport.Options{UserName: testUserName, UserEmail: testUserEmail})
		repo.WriteAndCommit(ctx, t, "a.txt", "a\n", "feat: a\n")

		ref := filepath.Join(repo.Dir, ".git", "refs", "heads", "main")
		if err := os.WriteFile(ref, []byte(strings.Repeat("dead1234", 5)+"\n"), 0o600); err != nil {
			t.Fatalf("corrupt ref: %v", err)
		}
		if _, err := repo.Git.HasHead(ctx); err == nil {
			t.Fatal("a HEAD pointing at a missing object was reported as an absent HEAD")
		}
	})
}

func TestAddPathsStagesLiteralPathsOnly(t *testing.T) {
	ctx := t.Context()
	repo := testsupport.NewRepo(ctx, t, testsupport.Options{UserName: testUserName, UserEmail: testUserEmail})
	repo.WriteFile(t, "keep.txt", "keep\n")
	repo.WriteFile(t, "secret.txt", "secret\n")

	tests := []struct {
		name    string
		path    string
		wantErr string
	}{
		{name: "wildcard", path: "*.txt", wantErr: "must name one file"},
		{name: "character class", path: "[ks]*.txt", wantErr: "must name one file"},
		{name: "pathspec magic", path: ":(glob)**/*.txt", wantErr: "must not use pathspec magic"},
		{name: "top magic", path: ":/secret.txt", wantErr: "must not use pathspec magic"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := repo.Git.AddPaths(ctx, test.path)
			if err == nil {
				t.Fatalf("expected an error for %q", test.path)
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error %q does not contain %q", err, test.wantErr)
			}
		})
	}

	if err := repo.Git.AddPaths(ctx, "keep.txt"); err != nil {
		t.Fatalf("stage a literal path: %v", err)
	}
	repo.Commit(ctx, t, "feat: keep\n", gitcli.CommitOptions{})

	commit, err := repo.Git.CommitInfo(ctx, "HEAD")
	if err != nil {
		t.Fatalf("commit info: %v", err)
	}
	if commit.SHA == "" {
		t.Fatal("commit was not created")
	}
	// secret.txt must still be untracked because no pattern was ever expanded.
	if _, err := repo.Git.ResolveCommit(ctx, "HEAD"); err != nil {
		t.Fatalf("resolve HEAD: %v", err)
	}
}

func TestValidatePushRemote(t *testing.T) {
	tests := []struct {
		name    string
		remote  string
		wantErr string
	}{
		{name: "publish host", remote: "https://github.com/enj/rbac_authorizer.git"},
		{name: "absolute path", remote: "/tmp/soapbox-remote.git"},
		{name: "file url", remote: "file:///tmp/soapbox-remote.git"},
		{name: "named remote", remote: "origin", wantErr: "hides its target in configuration"},
		{name: "other https host", remote: "https://example.com/enj/x.git", wantErr: "must publish to github.com"},
		{name: "credentials", remote: "https://ghs_token@github.com/enj/x.git", wantErr: "must not embed credentials"},
		{name: "relative path", remote: "../other.git", wantErr: "must be an absolute path"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := gitcli.ValidatePushRemote(test.remote)
			switch {
			case test.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case test.wantErr != "" && err == nil:
				t.Fatalf("expected an error containing %q", test.wantErr)
			case test.wantErr != "" && !strings.Contains(err.Error(), test.wantErr):
				t.Fatalf("error %q does not contain %q", err, test.wantErr)
			}
		})
	}
}

func TestPushRejectsLocalRemoteRewrites(t *testing.T) {
	ctx := t.Context()
	repo := testsupport.NewRepo(ctx, t, testsupport.Options{UserName: testUserName, UserEmail: testUserEmail})
	repo.WriteAndCommit(ctx, t, "a.txt", "a\n", "feat: a\n")
	local := testsupport.NewRepo(ctx, t, testsupport.Options{UserName: testUserName, UserEmail: testUserEmail})

	tests := []struct {
		name  string
		key   string
		value string
		// remote is the push target. A file target is covered as well as the
		// publish host, because a rewrite redirects both by the same mechanism
		// and only the local one can be proved against a real server.
		remote string
	}{
		{
			name:   "insteadOf",
			key:    "url.https://attacker.example.com/.insteadOf",
			value:  "https://github.com/",
			remote: "https://github.com/enj/rbac_authorizer.git",
		},
		{
			name:   "pushInsteadOf",
			key:    "url.https://attacker.example.com/.pushInsteadOf",
			value:  "https://github.com/",
			remote: "https://github.com/enj/rbac_authorizer.git",
		},
		{
			name:   "pushurl",
			key:    "remote.origin.pushurl",
			value:  "https://attacker.example.com/enj/x.git",
			remote: "https://github.com/enj/rbac_authorizer.git",
		},
		{
			name:   "insteadOf on a file mirror",
			key:    "url.file:///attacker/.insteadOf",
			value:  "file://" + local.Dir,
			remote: local.Dir,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repo.SetConfig(ctx, t, test.key, test.value)
			t.Cleanup(func() {
				if err := repo.Git.UnsetConfigLocal(ctx, test.key); err != nil {
					t.Fatalf("clear %s: %v", test.key, err)
				}
			})

			err := repo.Git.Push(ctx, test.remote, "refs/heads/main:refs/heads/main")
			if err == nil {
				t.Fatal("expected the push to fail closed")
			}
			if !strings.Contains(err.Error(), "rewrites the remote") {
				t.Fatalf("error %q does not report a rewritten remote", err)
			}
		})
	}
}

// TestPushRejectsWorktreeScopedRewrites proves the gate is not fooled by the
// configuration scope a --local query cannot see.
//
// Enabling the worktree configuration extension gives a repository a second
// file whose keys outrank the local ones. A rewrite parked there redirects the
// transfer exactly like a local one and is invisible to git config --local, so
// a gate that only looked there would pass a push straight to the attacker.
func TestPushRejectsWorktreeScopedRewrites(t *testing.T) {
	ctx := t.Context()
	repo := testsupport.NewRepo(ctx, t, testsupport.Options{UserName: testUserName, UserEmail: testUserEmail})
	repo.WriteAndCommit(ctx, t, "a.txt", "a\n", "feat: a\n")

	repo.SetConfig(ctx, t, "extensions.worktreeConfig", "true")
	// The file is written directly because that is how a restored or tampered
	// repository would carry it: no git command has to have been run for the
	// key to take effect on the next transfer.
	worktreeConfig := filepath.Join(repo.Dir, ".git", "config.worktree")
	contents := "[url \"https://attacker.example.com/\"]\n\tinsteadOf = https://github.com/\n"
	if err := os.WriteFile(worktreeConfig, []byte(contents), 0o600); err != nil {
		t.Fatalf("write worktree scoped configuration: %v", err)
	}

	// The key really is invisible to the scope a naive gate would query.
	if _, found, err := repo.Git.ConfigLocal(ctx, "url.https://attacker.example.com/.insteadOf"); err != nil {
		t.Fatalf("read local configuration: %v", err)
	} else if found {
		t.Fatal("the fixture did not exercise the worktree scope")
	}

	err := repo.Git.Push(ctx, "https://github.com/enj/rbac_authorizer.git", "refs/heads/main:refs/heads/main")
	if err == nil {
		t.Fatal("expected the push to fail closed")
	}
	if !strings.Contains(err.Error(), "rewrites the remote") {
		t.Fatalf("error %q does not report a rewritten remote", err)
	}
}

// homeWithIdentity creates a HOME directory whose git configuration names an
// identity, and returns the directory.
//
// A repository that includes ~/inc.config is what makes HOME observable: git
// expands the tilde against HOME when it reads the include, so the identity on
// a commit says which HOME the subprocess actually had. The fixed environment
// sends global configuration to the null device, which is why the include is
// the mechanism rather than ~/.gitconfig.
func homeWithIdentity(t *testing.T, name string) string {
	t.Helper()
	home := t.TempDir()
	contents := "[user]\n\tname = " + name + "\n\temail = " + strings.ReplaceAll(strings.ToLower(name), " ", ".") + "@example.invalid\n"
	if err := os.WriteFile(filepath.Join(home, "inc.config"), []byte(contents), 0o600); err != nil {
		t.Fatalf("write identity configuration: %v", err)
	}
	return home
}

// commitIdentity initializes a repository that reads its identity from HOME,
// records one commit through git, and reports the author name git used.
func commitIdentity(t *testing.T, git *gitcli.Runner) string {
	t.Helper()
	ctx := t.Context()
	if err := git.InitRepository(ctx, "main"); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := git.SetConfigLocal(ctx, "include.path", "~/inc.config"); err != nil {
		t.Fatalf("set include: %v", err)
	}
	if err := git.Commit(ctx, gitcli.CommitOptions{Message: "chore: identity probe\n", AllowEmpty: true}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	head, err := git.ResolveCommit(ctx, "HEAD")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	commit, err := git.CommitInfo(ctx, head)
	if err != nil {
		t.Fatalf("commit info: %v", err)
	}
	return commit.AuthorName
}

// TestAnonymousPreservesIsolation proves that stripping credentials does not
// also strip the entries that decide where git looks for state.
//
// The two are different kinds of thing. A token grants access and must never
// reach the source host; an isolated HOME withholds access and must survive,
// because a run that redirected git away from operator configuration has to
// stay redirected when it reaches the network. Rebuilding the environment from
// the process would silently restore the operator's HOME at exactly the moment
// the run stopped being trusted.
func TestAnonymousPreservesIsolation(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()

	// The process HOME holds configuration this run must never read.
	t.Setenv("HOME", homeWithIdentity(t, "Operator Identity"))
	isolated := homeWithIdentity(t, "Isolated Identity")

	const token = "ghs_notarealtoken" //nolint:gosec // a fixture value, not a credential
	runner, err := gitcli.New(ctx, gitcli.Options{
		Dir:       dir,
		Inherit:   []string{"PATH", "HOME"},
		Isolation: []string{"HOME=" + isolated},
		Env:       []string{"SOAPBOX_TOKEN=" + token},
	})
	if err != nil {
		t.Fatalf("create runner: %v", err)
	}
	if runner.IsAnonymous() {
		t.Fatal("a runner carrying an Env entry must not be anonymous")
	}

	anonymous := runner.Anonymous()
	if !anonymous.IsAnonymous() {
		t.Fatal("Anonymous must produce an anonymous runner")
	}
	if got := commitIdentity(t, anonymous); got != "Isolated Identity" {
		t.Fatalf("author %q, want the isolated identity, so HOME survived credential stripping", got)
	}

	// The credential is gone, and its value is still scrubbed from output.
	if got := anonymous.Redactor().String("a line mentioning " + token); strings.Contains(got, token) {
		t.Fatal("the redactor forgot a credential the runner can no longer see")
	}
}

// TestAnonymousIgnoresLaterProcessEnvironmentChanges proves the inherited
// values are the ones read when the runner was built.
//
// Anonymous rebuilt the environment by reading the process again, so a value
// that changed in between would take effect at the moment credentials were
// stripped, which is the one moment a run must not change behaviour.
func TestAnonymousIgnoresLaterProcessEnvironmentChanges(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()

	t.Setenv("HOME", homeWithIdentity(t, "Constructed Identity"))
	runner, err := gitcli.New(ctx, gitcli.Options{
		Dir:     dir,
		Inherit: []string{"PATH", "HOME"},
		Env:     []string{"SOAPBOX_TOKEN=value"},
	})
	if err != nil {
		t.Fatalf("create runner: %v", err)
	}

	// The process moves on after the runner was built.
	t.Setenv("HOME", homeWithIdentity(t, "Later Identity"))

	if got := commitIdentity(t, runner.Anonymous()); got != "Constructed Identity" {
		t.Fatalf("author %q, want the value inherited when the runner was built", got)
	}
}

func TestRedactorAvoidsCopyingWhenNothingMatches(t *testing.T) {
	r := gitcli.NewRedactor("ghs_TOKEN")
	in := []byte("plain output with no secret")
	out := r.Bytes(in)
	if len(out) == 0 || &out[0] != &in[0] {
		t.Fatal("Bytes copied a slice that contains no secret")
	}

	empty := gitcli.NewRedactor()
	if got := empty.Bytes(in); &got[0] != &in[0] {
		t.Fatal("a redactor without secrets copied its input")
	}
	if got := empty.String("value"); got != "value" {
		t.Fatalf("String() = %q", got)
	}
}

func TestRedactWriterIsTransparentWithoutSecrets(t *testing.T) {
	var buf strings.Builder
	w := gitcli.NewRedactor().Writer(&buf)
	if _, err := w.Write([]byte("streamed")); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Nothing is held back, so the bytes are visible before Close.
	if got := buf.String(); got != "streamed" {
		t.Fatalf("buffered output = %q, want it written through immediately", got)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestCommitCanProduceSignedCommitsForFixtures(t *testing.T) {
	ctx := t.Context()
	key := testsupport.NewSigningKey(t)
	repo := testsupport.NewRepo(ctx, t, testsupport.Options{UserName: testUserName, UserEmail: testUserEmail})
	repo.EnableSSHSigning(ctx, t, key, testUserEmail)

	repo.WriteFile(t, "a.txt", "a\n")
	repo.Commit(ctx, t, "feat: a\n", gitcli.CommitOptions{Sign: true}, "a.txt")

	commit, err := repo.Git.CommitInfo(ctx, "HEAD")
	if err != nil {
		t.Fatalf("commit info: %v", err)
	}
	if commit.SignatureStatus != "G" {
		t.Fatalf("signature status = %q, want G", commit.SignatureStatus)
	}
	if commit.SignerKey != key.Fingerprint {
		t.Fatalf("signer key = %q, want %q", commit.SignerKey, key.Fingerprint)
	}

	unsigned := testsupport.NewRepo(ctx, t, testsupport.Options{UserName: testUserName, UserEmail: testUserEmail})
	unsigned.WriteAndCommit(ctx, t, "a.txt", "a\n", "feat: a\n")
	plain, err := unsigned.Git.CommitInfo(ctx, "HEAD")
	if err != nil {
		t.Fatalf("commit info: %v", err)
	}
	if plain.SignatureStatus != "N" {
		t.Fatalf("default commits must stay unsigned, got status %q", plain.SignatureStatus)
	}
}
