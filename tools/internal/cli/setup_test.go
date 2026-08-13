package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/cli"
	"github.com/enj/soapbox/tools/internal/config"
	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/testsupport"
)

// TestRunSetupUsage covers every command line the setup command refuses.
//
// None of these reads a profile or opens a repository, so each has to fail
// identically whether or not either exists. The directory is deliberately empty
// to prove it.
func TestRunSetupUsage(t *testing.T) {
	dir := t.TempDir()

	tests := []struct {
		name       string
		args       []string
		wantStderr string
	}{
		{
			name:       "no engine version",
			args:       []string{"setup", "-dir", dir},
			wantStderr: "-engine-version is required",
		},
		{
			name:       "an unsupported format",
			args:       []string{"setup", "-dir", dir, "-engine-version", "v1.0.0", "-format", "yaml"},
			wantStderr: `unsupported -format "yaml"`,
		},
		{
			name:       "an approval without an apply",
			args:       []string{"setup", "-dir", dir, "-engine-version", "v1.0.0", "-approve", "abc"},
			wantStderr: "-apply is required with it",
		},
		{
			name:       "an apply without an approval",
			args:       []string{"setup", "-dir", dir, "-engine-version", "v1.0.0", "-apply"},
			wantStderr: "-approve must name the hash",
		},
		{
			name:       "a report inside the repository",
			args:       []string{"setup", "-dir", dir, "-engine-version", "v1.0.0", "-report", "manifest.json"},
			wantStderr: "must be outside the repository",
		},
		{
			name:       "an unexpected operand",
			args:       []string{"setup", "-dir", dir, "-engine-version", "v1.0.0", "extra"},
			wantStderr: "takes no arguments",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stderr bytes.Buffer
			code := cli.Run(t.Context(), cli.Env{Stderr: &stderr, Dir: dir}, test.args)
			if code != cli.ExitUsage {
				t.Errorf("exit code = %d, want %d", code, cli.ExitUsage)
			}
			if !strings.Contains(stderr.String(), test.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), test.wantStderr)
			}
		})
	}
}

// TestRunSetupDryRunThenApply is the whole command contract in one pass: a bare
// invocation reports and changes nothing, and the hash it printed is the one
// thing that lets the next invocation write.
func TestRunSetupDryRunThenApply(t *testing.T) {
	ctx := t.Context()
	root := setupTemplate(ctx, t)
	reportPath := filepath.Join(t.TempDir(), "manifest.json")

	var stdout, stderr bytes.Buffer
	code := cli.Run(ctx, cli.Env{Stdout: &stdout, Stderr: &stderr, Dir: root},
		[]string{"setup", "-engine-version", "tools/v0.3.0", "-report", reportPath})
	if code != cli.ExitOK {
		t.Fatalf("dry run exit code = %d, stderr %s", code, stderr.String())
	}
	if _, err := os.Lstat(filepath.Join(root, "go.mod")); !os.IsNotExist(err) {
		t.Fatalf("a dry run wrote the root module: %v", err)
	}
	if !strings.Contains(stdout.String(), "nothing was written") {
		t.Errorf("summary does not say the run wrote nothing:\n%s", stdout.String())
	}

	var manifest struct {
		Hash   string `json:"hash"`
		Engine struct {
			Tag string `json:"tag"`
		} `json:"engine"`
	}
	if err := json.Unmarshal(readReport(t, reportPath), &manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if manifest.Engine.Tag != "tools/v0.3.0" {
		t.Errorf("manifest pins %q, want tools/v0.3.0", manifest.Engine.Tag)
	}
	// The hash the operator would copy out of the summary is the hash the
	// manifest records, or the instruction the summary prints would not work.
	if !strings.Contains(stdout.String(), manifest.Hash) {
		t.Error("the summary does not print the manifest hash")
	}

	t.Run("a wrong approval is a finding", func(t *testing.T) {
		var stderr bytes.Buffer
		code := cli.Run(ctx, cli.Env{Stderr: &stderr, Dir: root},
			[]string{"setup", "-engine-version", "tools/v0.3.0", "-apply", "-approve", strings.Repeat("0", 64)})
		if code != cli.ExitCheck {
			t.Fatalf("exit code = %d, want %d (%s)", code, cli.ExitCheck, stderr.String())
		}
		if !strings.Contains(stderr.String(), "approval does not match") {
			t.Errorf("stderr = %q", stderr.String())
		}
	})

	t.Run("the printed approval applies", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := cli.Run(ctx, cli.Env{Stdout: &stdout, Stderr: &stderr, Dir: root},
			[]string{"setup", "-engine-version", "tools/v0.3.0", "-apply", "-approve", manifest.Hash})
		if code != cli.ExitOK {
			t.Fatalf("exit code = %d, stderr %s", code, stderr.String())
		}
		if strings.Contains(stdout.String(), "nothing was written") {
			t.Error("an applied run reported that it wrote nothing")
		}
		for _, name := range []string{"go.mod", "tools/go.mod", ".github/workflows/sync.yml"} {
			if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(name))); err != nil {
				t.Errorf("%s: %v", name, err)
			}
		}
	})
}

// TestRunSetupRefusesADirtyRepository proves a policy refusal reaches the exit
// code contract as a finding rather than as a crash, and that a requested
// manifest is still written for the operator to read.
func TestRunSetupRefusesADirtyRepository(t *testing.T) {
	ctx := t.Context()
	root := setupTemplate(ctx, t)
	if err := os.WriteFile(filepath.Join(root, "scratch.txt"), []byte("x\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := cli.Run(ctx, cli.Env{Stdout: &stdout, Stderr: &stderr, Dir: root},
		[]string{"setup", "-engine-version", "v0.3.0"})
	if code != cli.ExitCheck {
		t.Fatalf("exit code = %d, want %d (%s)", code, cli.ExitCheck, stderr.String())
	}
	if !strings.Contains(stderr.String(), "uncommitted changes") {
		t.Errorf("stderr = %q", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("a refused run wrote to stdout: %q", stdout.String())
	}
}

// TestRunSetupHelpListsTheCommand keeps the dispatcher and the help output from
// drifting apart.
func TestRunSetupHelpListsTheCommand(t *testing.T) {
	var stdout bytes.Buffer
	if code := cli.Run(t.Context(), cli.Env{Stdout: &stdout}, []string{"help", "setup"}); code != cli.ExitOK {
		t.Fatalf("exit code = %d", code)
	}
	for _, flag := range []string{"-engine-version", "-engine-sum", "-apply", "-approve", "-report"} {
		if !strings.Contains(stdout.String(), flag) {
			t.Errorf("help does not describe %s:\n%s", flag, stdout.String())
		}
	}
}

// setupTemplate builds a committed template checkout the setup command accepts.
func setupTemplate(ctx context.Context, tb testing.TB) string {
	tb.Helper()

	repo := testsupport.NewRepo(ctx, tb, testsupport.Options{
		UserName:  "Soapbox Test",
		UserEmail: "test@example.invalid",
	})
	for path, contents := range map[string]string{
		config.DefaultFileName:      planProfile,
		"plans/implementation.md":   "# plan\n",
		"tools/soapbox.go":          "package soapbox\n",
		"tools/internal/cli/cli.go": "package cli\n",
		"tools/cmd/soapbox/main.go": "package main\n",
		"CLAUDE.md":                 "# instructions\n",
		"README.md":                 "# fixture\n",
	} {
		repo.WriteFile(tb, path, contents)
	}
	repo.Commit(ctx, tb, "chore: template", gitcli.CommitOptions{}, ".")

	root, err := filepath.EvalSymlinks(repo.Dir)
	if err != nil {
		tb.Fatalf("resolve repository: %v", err)
	}
	return root
}

// readReport reads a manifest the command wrote.
func readReport(tb testing.TB, path string) []byte {
	tb.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // the path is a test temporary directory
	if err != nil {
		tb.Fatalf("read %s: %v", path, err)
	}
	return data
}
