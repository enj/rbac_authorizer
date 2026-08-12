package gitcli_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/testsupport"
)

// binaryBlob is content no line oriented reader would survive: null bytes, a
// bare carriage return, invalid UTF-8, and no trailing newline. ReadBlob has to
// return it byte for byte, because the engine hashes what it reads.
var binaryBlob = []byte{0x00, 'g', 'o', 0x00, 0x0d, 0xff, 0xfe, '\n', 0x00}

func TestReadBlobReturnsExactBytes(t *testing.T) {
	ctx := t.Context()
	repo := testsupport.NewRepo(ctx, t, testsupport.Options{UserName: testUserName, UserEmail: testUserEmail})

	tests := []struct {
		name    string
		path    string
		content string
	}{
		{name: "text", path: "go.mod", content: "module example.com/z\n\ngo 1.26.0\n"},
		{name: "empty", path: "empty.txt", content: ""},
		{name: "binary", path: "binary.bin", content: string(binaryBlob)},
		{name: "no trailing newline", path: "bare.txt", content: "no newline here"},
		{name: "crlf preserved", path: "crlf.txt", content: "one\r\ntwo\r\n"},
		{name: "nested path", path: "a/b/c/deep.txt", content: "deep\n"},
	}
	for _, test := range tests {
		repo.WriteFile(t, test.path, test.content)
	}
	paths := make([]string, 0, len(tests))
	for _, test := range tests {
		paths = append(paths, test.path)
	}
	head := repo.Commit(ctx, t, "feat: add fixtures\n", gitcli.CommitOptions{}, paths...)

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			content, err := repo.Git.ReadBlob(ctx, gitcli.BlobOptions{Revision: head, Path: test.path})
			if err != nil {
				t.Fatalf("read blob: %v", err)
			}
			if !bytes.Equal(content, []byte(test.content)) {
				t.Fatalf("content = %q, want %q", content, test.content)
			}
		})
	}
}

// TestReadBlobReadsByObjectName covers the form where the revision already names
// the blob, which is how a caller reads an object it discovered through a
// batched probe rather than through a path.
func TestReadBlobReadsByObjectName(t *testing.T) {
	ctx := t.Context()
	repo := testsupport.NewRepo(ctx, t, testsupport.Options{UserName: testUserName, UserEmail: testUserEmail})
	head := repo.WriteAndCommit(ctx, t, "go.mod", "module example.com/z\n", "feat: add module\n")

	infos, err := repo.Git.ObjectInfoBatch(ctx, gitcli.ObjectInfoOptions{Revisions: []string{head + ":go.mod"}})
	if err != nil {
		t.Fatalf("object info: %v", err)
	}
	if len(infos) != 1 || infos[0].Type != "blob" {
		t.Fatalf("object info = %+v, want one blob", infos)
	}

	content, err := repo.Git.ReadBlob(ctx, gitcli.BlobOptions{Revision: infos[0].Name})
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	if got := string(content); got != "module example.com/z\n" {
		t.Fatalf("content = %q", got)
	}
}

func TestReadBlobLimit(t *testing.T) {
	ctx := t.Context()
	repo := testsupport.NewRepo(ctx, t, testsupport.Options{UserName: testUserName, UserEmail: testUserEmail})
	// The content is large enough that a reader which measured after buffering
	// would have already allocated it.
	large := strings.Repeat("k8s.io/kubernetes\n", 4096)
	head := repo.WriteAndCommit(ctx, t, "large.txt", large, "feat: add large file\n")

	tests := []struct {
		name  string
		limit int64
		want  error
	}{
		{name: "exactly at the limit", limit: int64(len(large))},
		{name: "one byte short", limit: int64(len(large)) - 1, want: gitcli.ErrBlobTooLarge},
		{name: "far below", limit: 1, want: gitcli.ErrBlobTooLarge},
		{name: "zero uses the default", limit: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			content, err := repo.Git.ReadBlob(ctx, gitcli.BlobOptions{
				Revision: head,
				Path:     "large.txt",
				Limit:    test.limit,
			})
			if test.want != nil {
				if !errors.Is(err, test.want) {
					t.Fatalf("error = %v, want %v", err, test.want)
				}
				if content != nil {
					t.Fatalf("refused read returned %d bytes", len(content))
				}
				// The refusal has to say what it refused, or an operator cannot
				// tell a limit that is too low from a file that is too big.
				if !strings.Contains(err.Error(), fmt.Sprint(len(large))) {
					t.Fatalf("error %q does not report the blob size", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("read blob: %v", err)
			}
			if string(content) != large {
				t.Fatalf("content is %d bytes, want %d", len(content), len(large))
			}
		})
	}
}

func TestReadBlobRejectsNegativeLimit(t *testing.T) {
	ctx := t.Context()
	repo := testsupport.NewRepo(ctx, t, testsupport.Options{UserName: testUserName, UserEmail: testUserEmail})
	head := repo.WriteAndCommit(ctx, t, "go.mod", "module example.com/z\n", "feat: add module\n")

	if _, err := repo.Git.ReadBlob(ctx, gitcli.BlobOptions{Revision: head, Path: "go.mod", Limit: -1}); err == nil {
		t.Fatal("expected an error for a negative limit")
	}
}

func TestReadBlobVerdicts(t *testing.T) {
	ctx := t.Context()
	repo := testsupport.NewRepo(ctx, t, testsupport.Options{UserName: testUserName, UserEmail: testUserEmail})
	repo.WriteFile(t, "sub/file.txt", "content\n")
	head := repo.Commit(ctx, t, "feat: add file\n", gitcli.CommitOptions{}, "sub/file.txt")

	tests := []struct {
		name     string
		revision string
		path     string
		want     error
	}{
		{name: "absent path", revision: head, path: "absent.txt", want: gitcli.ErrObjectNotFound},
		{name: "absent revision", revision: strings.Repeat("0", len(head)), path: "sub/file.txt", want: gitcli.ErrObjectNotFound},
		// A missing object is reported by echoing the request back, so a long
		// path has to stay inside the bound the response header is read under.
		{name: "long absent path", revision: head, path: strings.Repeat("deep/", 600) + "file.txt", want: gitcli.ErrObjectNotFound},
		{name: "directory is not a blob", revision: head, path: "sub", want: gitcli.ErrNotABlob},
		{name: "commit is not a blob", revision: head, want: gitcli.ErrNotABlob},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			content, err := repo.Git.ReadBlob(ctx, gitcli.BlobOptions{Revision: test.revision, Path: test.path})
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
			if content != nil {
				t.Fatalf("failed read returned %d bytes", len(content))
			}
		})
	}
}

// TestReadBlobRejectsHostileArguments proves a revision or path cannot reach
// git as an option, as a different path, or as pathspec magic. None of these
// may resolve to anything, and none may be reported as a plain missing object,
// because that would mean the value was accepted and merely did not exist.
func TestReadBlobRejectsHostileArguments(t *testing.T) {
	ctx := t.Context()
	repo := testsupport.NewRepo(ctx, t, testsupport.Options{UserName: testUserName, UserEmail: testUserEmail})
	head := repo.WriteAndCommit(ctx, t, "secret.txt", "content\n", "feat: add file\n")

	tests := []struct {
		name     string
		revision string
		path     string
	}{
		{name: "option revision", revision: "--help", path: "secret.txt"},
		{name: "option path", revision: head, path: "--output=/tmp/x"},
		{name: "revision carries its own path", revision: head + ":secret.txt", path: "secret.txt"},
		{name: "revision carries index magic", revision: ":0", path: "secret.txt"},
		{name: "revision range", revision: head + "..HEAD", path: "secret.txt"},
		{name: "negated revision", revision: "^" + head, path: "secret.txt"},
		{name: "message search path", revision: head, path: ":/secret"},
		{name: "absolute path", revision: head, path: "/etc/passwd"},
		{name: "parent traversal", revision: head, path: "../../etc/passwd"},
		{name: "current directory", revision: head, path: "./secret.txt"},
		{name: "empty component", revision: head, path: "sub//file.txt"},
		{name: "directory path", revision: head, path: "sub/"},
		{name: "null in path", revision: head, path: "secret.txt\x00.go"},
		{name: "null in revision", revision: head + "\x00", path: "secret.txt"},
		{name: "newline in path", revision: head, path: "secret.txt\nHEAD:secret.txt"},
		{name: "newline in revision", revision: head + "\nHEAD", path: "secret.txt"},
		{name: "empty revision", revision: "", path: "secret.txt"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			content, err := repo.Git.ReadBlob(ctx, gitcli.BlobOptions{Revision: test.revision, Path: test.path})
			if err == nil {
				t.Fatalf("hostile argument was accepted and returned %q", content)
			}
			if errors.Is(err, gitcli.ErrObjectNotFound) {
				t.Fatalf("hostile argument reached git and was merely missing: %v", err)
			}
			if content != nil {
				t.Fatalf("rejected read returned %d bytes", len(content))
			}
		})
	}
}

// TestReadBlobDoesNotFetchLazily proves the default answers from the local
// object store. The clone is blobless, so the blob is genuinely absent, and a
// read that reached the promisor remote would succeed instead of reporting it.
func TestReadBlobDoesNotFetchLazily(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)
	root := t.TempDir()
	runner := newAnonymousRunner(t, root)

	dir := filepath.Join(root, "cache.git")
	if err := runner.CloneSource(ctx, gitcli.SourceCloneOptions{
		Remote:    up.url(),
		Directory: dir,
		Filter:    gitcli.BloblessFilter,
		Bare:      true,
	}); err != nil {
		t.Fatalf("clone: %v", err)
	}
	cache, err := runner.WithDir(dir)
	if err != nil {
		t.Fatalf("scope runner to the cache: %v", err)
	}

	head := up.sha(mainOne)
	if _, err := cache.ReadBlob(ctx, gitcli.BlobOptions{
		Revision: head,
		Path:     "plugin/pkg/auth/authorizer/rbac/rbac.go",
	}); !errors.Is(err, gitcli.ErrObjectNotFound) {
		t.Fatalf("error = %v, want %v", err, gitcli.ErrObjectNotFound)
	}

	// The same read with the fetch allowed proves the blob was reachable all
	// along, so the refusal above was the policy and not a broken fixture.
	content, err := cache.ReadBlob(ctx, gitcli.BlobOptions{
		Revision:       head,
		Path:           "plugin/pkg/auth/authorizer/rbac/rbac.go",
		AllowLazyFetch: true,
	})
	if err != nil {
		t.Fatalf("read blob with lazy fetch: %v", err)
	}
	if got := string(content); got != "package rbac\n" {
		t.Fatalf("content = %q", got)
	}
}

// TestReadBlobLazyFetchRefusesRewrittenRemote proves a lazy fetch is gated on
// the repository's own configuration exactly as an explicit transfer is: a
// rewrite must not be able to decide where the bytes come from.
func TestReadBlobLazyFetchRefusesRewrittenRemote(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)
	root := t.TempDir()
	runner := newAnonymousRunner(t, root)

	dir := filepath.Join(root, "cache.git")
	if err := runner.CloneSource(ctx, gitcli.SourceCloneOptions{
		Remote:    up.url(),
		Directory: dir,
		Filter:    gitcli.BloblessFilter,
		Bare:      true,
	}); err != nil {
		t.Fatalf("clone: %v", err)
	}
	cache, err := runner.WithDir(dir)
	if err != nil {
		t.Fatalf("scope runner to the cache: %v", err)
	}
	if err := cache.SetConfigLocal(ctx, "url.https://example.invalid/.insteadOf", "file://"); err != nil {
		t.Fatalf("set rewrite: %v", err)
	}

	_, err = cache.ReadBlob(ctx, gitcli.BlobOptions{
		Revision:       up.sha(mainOne),
		Path:           "plugin/pkg/auth/authorizer/rbac/rbac.go",
		AllowLazyFetch: true,
	})
	if err == nil {
		t.Fatal("expected a refusal for a repository that rewrites its remote")
	}
	if !strings.Contains(err.Error(), "rewrites the remote") {
		t.Fatalf("error %q is not a rewrite refusal", err)
	}
}

func TestReadBlobHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	repo := testsupport.NewRepo(t.Context(), t, testsupport.Options{UserName: testUserName, UserEmail: testUserEmail})
	head := repo.WriteAndCommit(t.Context(), t, "go.mod", "module example.com/z\n", "feat: add module\n")
	cancel()

	_, err := repo.Git.ReadBlob(ctx, gitcli.BlobOptions{Revision: head, Path: "go.mod"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want %v", err, context.Canceled)
	}
}

// TestReadBlobKeepsSecretBytes proves blob content is not passed through the
// redactor. A file that happens to contain the same bytes as a credential is
// still the file the engine has to parse and hash, and rewriting it would
// silently corrupt the output.
func TestReadBlobKeepsSecretBytes(t *testing.T) {
	ctx := t.Context()
	const secret = "ghs_averyprivatetoken"
	repo := testsupport.NewRepo(ctx, t, testsupport.Options{
		UserName:  testUserName,
		UserEmail: testUserEmail,
		Secrets:   []string{secret},
	})
	content := "module example.com/" + secret + "\n"
	head := repo.WriteAndCommit(ctx, t, "go.mod", content, "feat: add module\n")

	got, err := repo.Git.ReadBlob(ctx, gitcli.BlobOptions{Revision: head, Path: "go.mod"})
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	if string(got) != content {
		t.Fatalf("content = %q, want %q", got, content)
	}
	// The redactor is still armed for everything that is diagnostics rather
	// than content.
	if repo.Git.Redactor().String(secret) != gitcli.Placeholder {
		t.Fatal("the runner redactor was not seeded with the secret")
	}
}

// TestLazyFetchRequiresAnAnonymousRunner proves permitting a lazy fetch is
// permitting a fetch. The promisor remote is the public upstream, so a runner
// that might carry a credential must be refused before the transfer, exactly as
// CloneSource and FetchSource refuse one.
func TestLazyFetchRequiresAnAnonymousRunner(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)
	root := t.TempDir()
	anonymous := newAnonymousRunner(t, root)

	dir := filepath.Join(root, "cache.git")
	if err := anonymous.CloneSource(ctx, gitcli.SourceCloneOptions{
		Remote:    up.url(),
		Directory: dir,
		Filter:    gitcli.BloblessFilter,
		Bare:      true,
	}); err != nil {
		t.Fatalf("clone: %v", err)
	}

	// A runner carrying an environment entry may be carrying a credential.
	credentialed, err := gitcli.New(ctx, gitcli.Options{
		Dir:     dir,
		Inherit: []string{"PATH", "HOME"},
		Env:     []string{"SOAPBOX_TOKEN=ghs_lazyfetchtoken"},
	})
	if err != nil {
		t.Fatalf("create runner: %v", err)
	}
	if credentialed.IsAnonymous() {
		t.Fatal("a runner built with Env must not be anonymous")
	}

	head := up.sha(mainOne)
	const sourcePath = "plugin/pkg/auth/authorizer/rbac/rbac.go"

	t.Run("read blob", func(t *testing.T) {
		_, err := credentialed.ReadBlob(ctx, gitcli.BlobOptions{
			Revision:       head,
			Path:           sourcePath,
			AllowLazyFetch: true,
		})
		if !errors.Is(err, gitcli.ErrCredentialedRunner) {
			t.Fatalf("error = %v, want %v", err, gitcli.ErrCredentialedRunner)
		}
	})

	t.Run("object info", func(t *testing.T) {
		_, err := credentialed.ObjectInfoBatch(ctx, gitcli.ObjectInfoOptions{
			Revisions:      []string{head + ":" + sourcePath},
			AllowLazyFetch: true,
		})
		if !errors.Is(err, gitcli.ErrCredentialedRunner) {
			t.Fatalf("error = %v, want %v", err, gitcli.ErrCredentialedRunner)
		}
	})

	// Without the fetch there is no transfer to gate, so a credentialed runner
	// may still answer from the objects it already holds.
	t.Run("local reads stay allowed", func(t *testing.T) {
		infos, err := credentialed.ObjectInfoBatch(ctx, gitcli.ObjectInfoOptions{
			Revisions: []string{head},
		})
		if err != nil {
			t.Fatalf("local object info: %v", err)
		}
		if len(infos) != 1 || infos[0].Type != "commit" {
			t.Fatalf("object info = %+v, want one commit", infos)
		}
	})

	// Anonymous() strips the entries, which is how a credentialed run reaches
	// the source host at all.
	t.Run("anonymous copy is allowed", func(t *testing.T) {
		content, err := credentialed.Anonymous().ReadBlob(ctx, gitcli.BlobOptions{
			Revision:       head,
			Path:           sourcePath,
			AllowLazyFetch: true,
		})
		if err != nil {
			t.Fatalf("read blob: %v", err)
		}
		if got := string(content); got != "package rbac\n" {
			t.Fatalf("content = %q", got)
		}
	})
}

// TestOutputLimitIsEnforced proves an upstream repository cannot decide how much
// memory a capture spends. The blob path has its own bound; this covers every
// other command.
func TestOutputLimitIsEnforced(t *testing.T) {
	ctx := t.Context()
	repo := testsupport.NewRepo(ctx, t, testsupport.Options{UserName: testUserName, UserEmail: testUserEmail})
	repo.WriteAndCommit(ctx, t, "a.txt", "a\n", "feat: a\n")

	bounded, err := gitcli.New(ctx, gitcli.Options{
		Dir:         repo.Dir,
		Inherit:     []string{"PATH"},
		Env:         []string{"HOME=" + t.TempDir()},
		OutputLimit: 8,
	})
	if err != nil {
		t.Fatalf("create git runner: %v", err)
	}
	sha, err := bounded.ResolveCommit(ctx, "HEAD")
	if err == nil {
		t.Fatalf("an oversized response was accepted: %q", sha)
	}
	if !strings.Contains(err.Error(), "past the 8 byte limit") {
		t.Fatalf("error %q does not report the limit", err)
	}

	if _, err := gitcli.New(ctx, gitcli.Options{OutputLimit: -1}); err == nil {
		t.Fatal("a negative output limit was accepted")
	}
}
