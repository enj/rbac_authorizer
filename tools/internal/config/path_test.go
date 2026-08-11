package config_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/config"
)

func TestValidateRelPath(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		wantErr error
		wantMsg string
	}{
		{name: "nested file", path: "pkg/apis/rbac/v1/doc.go"},
		{name: "single element", path: "soapbox.yaml"},
		{name: "empty", path: "", wantMsg: "must not be empty"},
		{name: "current directory", path: ".", wantMsg: "must name a file or directory"},
		{name: "absolute", path: "/etc/shadow", wantErr: config.ErrAbsolutePath},
		{name: "windows drive", path: `C:/Windows/System32`, wantErr: config.ErrAbsolutePath},
		{name: "parent", path: "..", wantErr: config.ErrPathTraversal},
		{name: "leading parent", path: "../secrets", wantErr: config.ErrPathTraversal},
		{name: "embedded parent", path: "pkg/../../secrets", wantErr: config.ErrPathTraversal},
		{name: "interior parent", path: "pkg/apis/../rbac", wantErr: config.ErrPathTraversal},
		{name: "double slash", path: "pkg//apis", wantErr: config.ErrPathNotClean},
		{name: "trailing slash", path: "pkg/apis/", wantErr: config.ErrPathNotClean},
		{name: "dot element", path: "./pkg", wantErr: config.ErrPathNotClean},
		{name: "backslash", path: `pkg\apis`, wantMsg: "must use forward slashes"},
		{name: "null byte", path: "pkg\x00apis", wantMsg: "null byte"},
		{name: "control character", path: "pkg\napis", wantMsg: "control characters"},
		{name: "leading space", path: " pkg/apis", wantMsg: "leading or trailing space"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := config.ValidateRelPath(test.path)
			switch {
			case test.wantErr == nil && test.wantMsg == "":
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			case err == nil:
				t.Fatal("expected an error")
			case test.wantErr != nil && !errors.Is(err, test.wantErr):
				t.Fatalf("error %v is not %v", err, test.wantErr)
			case test.wantMsg != "" && !strings.Contains(err.Error(), test.wantMsg):
				t.Fatalf("error %q does not contain %q", err, test.wantMsg)
			}
		})
	}
}

func TestValidatePackagePath(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		wantErr string
	}{
		{name: "kubernetes package", path: "plugin/pkg/auth/authorizer/rbac"},
		{name: "versioned package", path: "pkg/apis/rbac/v1"},
		{name: "hidden element", path: "pkg/.hidden/rbac", wantErr: "must not start with"},
		{name: "space", path: "pkg/rbac authorizer", wantErr: "unsupported character"},
		{name: "colon", path: "pkg/rbac:v1", wantErr: "unsupported character"},
		{name: "parent", path: "pkg/../rbac", wantErr: "traverse"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := config.ValidatePackagePath(test.path)
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

func TestSafeJoin(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	outside := t.TempDir()

	mkdir(t, filepath.Join(root, "pkg", "apis"))
	writeFile(t, filepath.Join(root, "pkg", "apis", "doc.go"), "package apis\n")
	writeFile(t, filepath.Join(outside, "secret.txt"), "token\n")
	symlink(t, outside, filepath.Join(root, "escape"))
	symlink(t, filepath.Join(outside, "secret.txt"), filepath.Join(root, "secret-link"))
	symlink(t, filepath.Join(root, "pkg"), filepath.Join(root, "inside-link"))

	tests := []struct {
		name    string
		rel     string
		wantErr error
		wantMsg string
	}{
		{name: "existing file", rel: "pkg/apis/doc.go"},
		{name: "missing file below an existing directory", rel: "pkg/apis/new.go"},
		{name: "missing directory chain", rel: "generated/deep/tree/file.go"},
		{name: "symbolic link inside the root", rel: "inside-link/apis/doc.go"},
		{name: "symbolic link to a directory outside the root", rel: "escape", wantErr: config.ErrPathEscape},
		{name: "file through a symbolic link outside the root", rel: "escape/secret.txt", wantErr: config.ErrPathEscape},
		{name: "symbolic link to a file outside the root", rel: "secret-link", wantErr: config.ErrPathEscape},
		{name: "parent traversal", rel: "../secret.txt", wantErr: config.ErrPathTraversal},
		{name: "absolute path", rel: "/etc/shadow", wantErr: config.ErrAbsolutePath},
		{name: "empty", rel: "", wantMsg: "must not be empty"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := config.SafeJoin(ctx, root, test.rel)
			switch {
			case test.wantErr == nil && test.wantMsg == "":
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if !strings.HasPrefix(got, resolve(t, root)) {
					t.Fatalf("resolved path %q is outside %q", got, root)
				}
			case err == nil:
				t.Fatalf("expected an error, got %q", got)
			case test.wantErr != nil && !errors.Is(err, test.wantErr):
				t.Fatalf("error %v is not %v", err, test.wantErr)
			case test.wantMsg != "" && !strings.Contains(err.Error(), test.wantMsg):
				t.Fatalf("error %q does not contain %q", err, test.wantMsg)
			}
		})
	}

	t.Run("dangling symbolic links are refused", func(t *testing.T) {
		danglingRoot := t.TempDir()
		outside := t.TempDir()
		symlink(t, filepath.Join(outside, "absent.txt"), filepath.Join(danglingRoot, "absolute-dangling"))
		symlink(t, "../"+filepath.Base(outside)+"/absent.txt", filepath.Join(danglingRoot, "relative-dangling"))
		symlink(t, "absent-sibling.txt", filepath.Join(danglingRoot, "inside-dangling"))

		for _, name := range []string{"absolute-dangling", "relative-dangling", "inside-dangling"} {
			t.Run(name, func(t *testing.T) {
				got, err := config.SafeJoin(ctx, danglingRoot, name)
				if err == nil {
					t.Fatalf("dangling link resolved to %q instead of failing", got)
				}
				if !errors.Is(err, config.ErrDanglingSymlink) {
					t.Fatalf("error %v is not %v", err, config.ErrDanglingSymlink)
				}
			})
		}

		t.Run("path below a dangling link", func(t *testing.T) {
			if _, err := config.SafeJoin(ctx, danglingRoot, "absolute-dangling/child.txt"); !errors.Is(err, config.ErrDanglingSymlink) {
				t.Fatalf("error %v is not %v", err, config.ErrDanglingSymlink)
			}
		})
	})

	t.Run("canceled context", func(t *testing.T) {
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := config.SafeJoin(canceled, root, "pkg/apis/doc.go"); !errors.Is(err, context.Canceled) {
			t.Fatalf("error %v is not context.Canceled", err)
		}
	})

	t.Run("missing root", func(t *testing.T) {
		if _, err := config.SafeJoin(ctx, filepath.Join(root, "absent"), "file.go"); err == nil {
			t.Fatal("expected an error for a missing root")
		}
	})
}

func TestValidateGlob(t *testing.T) {
	tests := []struct {
		name    string
		glob    string
		wantErr string
	}{
		{name: "asset pattern", glob: "pkg/generated/*.json"},
		{name: "nested pattern", glob: "pkg/*/testdata/*.yaml"},
		{name: "unterminated class", glob: "pkg/[a-z.go", wantErr: "malformed pattern"},
		{name: "recursive globstar", glob: "pkg/**/testdata/*.json", wantErr: "recursive ** syntax"},
		{name: "trailing globstar", glob: "pkg/**", wantErr: "recursive ** syntax"},
		{name: "absolute", glob: "/pkg/*.go", wantErr: "must be relative"},
		{name: "traversal", glob: "../*.go", wantErr: "traverse"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := config.ValidateGlob(test.glob)
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

func mkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("link %s to %s: %v", link, target, err)
	}
}

func resolve(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("resolve %s: %v", path, err)
	}
	return resolved
}
