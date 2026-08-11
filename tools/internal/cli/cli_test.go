package cli_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/cli"
	"github.com/enj/soapbox/tools/internal/config"
)

func TestRunDispatch(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{
			name:       "no command",
			args:       nil,
			wantCode:   cli.ExitUsage,
			wantStderr: "no command given",
		},
		{
			name:       "unknown command",
			args:       []string{"publish"},
			wantCode:   cli.ExitUsage,
			wantStderr: `unknown command "publish"`,
		},
		{
			name:       "flag before the command",
			args:       []string{"-dir", "."},
			wantCode:   cli.ExitUsage,
			wantStderr: "flags follow the command name",
		},
		{
			name:       "top level help flag",
			args:       []string{"--help"},
			wantCode:   cli.ExitOK,
			wantStdout: "soapbox <command> [flags]",
		},
		{
			name:       "help command",
			args:       []string{"help"},
			wantCode:   cli.ExitOK,
			wantStdout: "validate  decode and validate",
		},
		{
			name:       "help for a command",
			args:       []string{"help", "doctor"},
			wantCode:   cli.ExitOK,
			wantStdout: "-dir string",
		},
		{
			name:       "help for an unknown command",
			args:       []string{"help", "publish"},
			wantCode:   cli.ExitUsage,
			wantStderr: `unknown command "publish"`,
		},
		{
			name:       "help with too many arguments",
			args:       []string{"help", "doctor", "validate"},
			wantCode:   cli.ExitUsage,
			wantStderr: "at most one command name",
		},
		{
			name:       "version",
			args:       []string{"version"},
			wantCode:   cli.ExitOK,
			wantStdout: "soapbox " + cli.Version,
		},
		{
			name:       "version rejects operands",
			args:       []string{"version", "extra"},
			wantCode:   cli.ExitUsage,
			wantStderr: "takes no arguments",
		},
		{
			name:       "unknown flag",
			args:       []string{"validate", "-nope"},
			wantCode:   cli.ExitUsage,
			wantStderr: "flag provided but not defined",
		},
		{
			name:       "command help flag",
			args:       []string{"validate", "-h"},
			wantCode:   cli.ExitOK,
			wantStdout: "-config string",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stdout, stderr, code := run(t.Context(), t, "", test.args...)
			if code != test.wantCode {
				t.Fatalf("exit code = %d, want %d\nstdout: %s\nstderr: %s", code, test.wantCode, stdout, stderr)
			}
			if test.wantStdout != "" && !strings.Contains(stdout, test.wantStdout) {
				t.Fatalf("stdout %q does not contain %q", stdout, test.wantStdout)
			}
			if test.wantStderr != "" && !strings.Contains(stderr, test.wantStderr) {
				t.Fatalf("stderr %q does not contain %q", stderr, test.wantStderr)
			}
		})
	}
}

func TestRunVersionReportsTheGoRuntime(t *testing.T) {
	stdout, _, code := run(t.Context(), t, "", "version")
	if code != cli.ExitOK {
		t.Fatalf("exit code = %d", code)
	}
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("version output = %q, want two lines", stdout)
	}
	if lines[0] != "soapbox "+cli.Version {
		t.Fatalf("first line = %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], "go1.") {
		t.Fatalf("second line = %q, want a Go runtime version", lines[1])
	}
}

func TestRunValidate(t *testing.T) {
	profile := repositoryProfile(t)
	dir := t.TempDir()
	writeProfile(t, filepath.Join(dir, config.DefaultFileName), profile)
	writeProfile(t, filepath.Join(dir, "invalid.yaml"), strings.Replace(profile, "chunkSize: 200", "chunkSize: 0", 1))
	writeProfile(t, filepath.Join(dir, "unknown.yaml"), profile+"extra: true\n")
	writeProfile(t, filepath.Join(dir, "multi.yaml"), profile+"---\n"+profile)

	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{
			name:       "valid profile",
			args:       []string{"validate", "-dir", dir},
			wantCode:   cli.ExitOK,
			wantStdout: "is a valid soapbox profile",
		},
		{
			name:       "summary reports the release mapping",
			args:       []string{"validate", "-dir", dir},
			wantCode:   cli.ExitOK,
			wantStdout: "v1.36.1 maps to v0.36.1 under v1-to-v0",
		},
		{
			name:       "invalid profile",
			args:       []string{"validate", "-dir", dir, "-config", "invalid.yaml"},
			wantCode:   cli.ExitCheck,
			wantStderr: "determinism.chunkSize",
		},
		{
			name:       "unknown field is a profile problem, not a runtime failure",
			args:       []string{"validate", "-dir", dir, "-config", "unknown.yaml"},
			wantCode:   cli.ExitCheck,
			wantStderr: "field extra not found",
		},
		{
			name:       "multiple documents",
			args:       []string{"validate", "-dir", dir, "-config", "multi.yaml"},
			wantCode:   cli.ExitCheck,
			wantStderr: "multiple YAML documents",
		},
		{
			name:       "unsupported format is rejected before the profile is read",
			args:       []string{"validate", "-dir", dir, "-config", "absent.yaml", "-format", "json"},
			wantCode:   cli.ExitUsage,
			wantStderr: `unsupported -format "json"`,
		},
		{
			name:       "missing profile",
			args:       []string{"validate", "-dir", dir, "-config", "absent.yaml"},
			wantCode:   cli.ExitFailure,
			wantStderr: "read profile",
		},
		{
			name:       "unsupported format",
			args:       []string{"validate", "-dir", dir, "-format", "json"},
			wantCode:   cli.ExitUsage,
			wantStderr: `unsupported -format "json"`,
		},
		{
			name:       "canonical format",
			args:       []string{"validate", "-dir", dir, "-format", "canonical"},
			wantCode:   cli.ExitOK,
			wantStdout: "importPrefix: k8s.io/kubernetes",
		},
		{
			name:       "profile format keeps the relocation layout",
			args:       []string{"validate", "-dir", dir, "-format", "profile"},
			wantCode:   cli.ExitOK,
			wantStdout: "destinationInternalPrefix: internal/kk",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stdout, stderr, code := run(t.Context(), t, "", test.args...)
			if code != test.wantCode {
				t.Fatalf("exit code = %d, want %d\nstdout: %s\nstderr: %s", code, test.wantCode, stdout, stderr)
			}
			if test.wantStdout != "" && !strings.Contains(stdout, test.wantStdout) {
				t.Fatalf("stdout %q does not contain %q", stdout, test.wantStdout)
			}
			if test.wantStderr != "" && !strings.Contains(stderr, test.wantStderr) {
				t.Fatalf("stderr %q does not contain %q", stderr, test.wantStderr)
			}
		})
	}
}

func TestRunValidateResolvesRelativePathsAgainstTheEnvironment(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, filepath.Join(dir, config.DefaultFileName), repositoryProfile(t))

	stdout, stderr, code := run(t.Context(), t, dir, "validate")
	if code != cli.ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "is a valid soapbox profile") {
		t.Fatalf("stdout = %q", stdout)
	}

	profileBytes, _, code := run(t.Context(), t, dir, "validate", "-format", "profile")
	if code != cli.ExitOK {
		t.Fatalf("exit code = %d", code)
	}
	if strings.Contains(profileBytes, "maxNonTestLines") || strings.Contains(profileBytes, "chunkSize") {
		t.Fatalf("profile output still carries operational fields:\n%s", profileBytes)
	}
	if _, err := config.Decode([]byte(profileBytes)); err == nil {
		t.Fatal("profile bytes are not a complete profile and must not validate")
	}
}

func TestRunDoctor(t *testing.T) {
	dir := t.TempDir()
	stdout, stderr, code := run(t.Context(), t, "", "doctor", "-dir", dir)
	if code != cli.ExitOK && code != cli.ExitCheck {
		t.Fatalf("exit code = %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "git.binary") || !strings.Contains(stdout, "checks:") {
		t.Fatalf("doctor output = %q", stdout)
	}
	if code == cli.ExitCheck && !strings.Contains(stderr, "required checks failed") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestRunCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, stderr, code := run(ctx, t, "", "doctor", "-dir", t.TempDir())
	if code != cli.ExitCanceled {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, cli.ExitCanceled, stderr)
	}
}

func TestRunToleratesNilWriters(t *testing.T) {
	if code := cli.Run(t.Context(), cli.Env{}, []string{"version"}); code != cli.ExitOK {
		t.Fatalf("exit code = %d", code)
	}
	if code := cli.Run(t.Context(), cli.Env{}, nil); code != cli.ExitUsage {
		t.Fatalf("exit code = %d", code)
	}
}

// TestRunPrintsUsageExactlyOnce pins the output contract of the flag layer:
// a help request goes to standard output once, and a bad flag prints one error
// with that command's flags and no second, top level usage block.
func TestRunPrintsUsageExactlyOnce(t *testing.T) {
	t.Run("command help goes to stdout once", func(t *testing.T) {
		stdout, stderr, code := run(t.Context(), t, "", "validate", "-h")
		if code != cli.ExitOK {
			t.Fatalf("exit code = %d", code)
		}
		if stderr != "" {
			t.Fatalf("help wrote to stderr: %q", stderr)
		}
		if got := strings.Count(stdout, "-config string"); got != 1 {
			t.Fatalf("flag documented %d times:\n%s", got, stdout)
		}
		if got := strings.Count(stdout, "soapbox validate:"); got != 1 {
			t.Fatalf("command summary printed %d times:\n%s", got, stdout)
		}
		if strings.Contains(stdout, "usage:\n  soapbox <command>") {
			t.Fatalf("command help printed the top level usage:\n%s", stdout)
		}
	})

	t.Run("unknown command flag prints one scoped error", func(t *testing.T) {
		stdout, stderr, code := run(t.Context(), t, "", "validate", "-nope")
		if code != cli.ExitUsage {
			t.Fatalf("exit code = %d", code)
		}
		if stdout != "" {
			t.Fatalf("failure wrote to stdout: %q", stdout)
		}
		if got := strings.Count(stderr, "flag provided but not defined"); got != 1 {
			t.Fatalf("error printed %d times:\n%s", got, stderr)
		}
		if got := strings.Count(stderr, "-config string"); got != 1 {
			t.Fatalf("command flags printed %d times:\n%s", got, stderr)
		}
		if strings.Contains(stderr, "usage:\n  soapbox <command>") {
			t.Fatalf("command failure printed the top level usage:\n%s", stderr)
		}
	})

	t.Run("unknown command prints the top level usage once", func(t *testing.T) {
		_, stderr, code := run(t.Context(), t, "", "publish")
		if code != cli.ExitUsage {
			t.Fatalf("exit code = %d", code)
		}
		if got := strings.Count(stderr, "usage:\n  soapbox <command>"); got != 1 {
			t.Fatalf("top level usage printed %d times:\n%s", got, stderr)
		}
	})

	t.Run("operands are rejected with the command usage", func(t *testing.T) {
		_, stderr, code := run(t.Context(), t, "", "doctor", "extra")
		if code != cli.ExitUsage {
			t.Fatalf("exit code = %d", code)
		}
		if got := strings.Count(stderr, "-dir string"); got != 1 {
			t.Fatalf("command flags printed %d times:\n%s", got, stderr)
		}
	})

	t.Run("version has no flags to print", func(t *testing.T) {
		stdout, stderr, code := run(t.Context(), t, "", "help", "version")
		if code != cli.ExitOK {
			t.Fatalf("exit code = %d (stderr %q)", code, stderr)
		}
		if strings.Contains(stdout, "flags:") {
			t.Fatalf("version help printed an empty flag block:\n%s", stdout)
		}
	})
}

// run executes one command line and captures both streams.
func run(ctx context.Context, t *testing.T, dir string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errBuf bytes.Buffer
	code = cli.Run(ctx, cli.Env{Stdout: &out, Stderr: &errBuf, Dir: dir}, args)
	return out.String(), errBuf.String(), code
}

// repositoryProfile returns the shipped RBAC profile, which keeps the command
// tests aligned with the profile the repository actually publishes.
func repositoryProfile(t *testing.T) string {
	t.Helper()
	path := filepath.Join("..", "..", "..", config.DefaultFileName)
	contents, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		t.Skipf("%s is not present in this checkout", path)
	}
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(contents)
}

func writeProfile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
