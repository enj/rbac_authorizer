package cli_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/cli"
	"github.com/enj/soapbox/tools/internal/config"
)

// TestRunGenerateUsage covers every command line the generate command refuses.
//
// None of these reads a profile, opens a cache, resolves a module, or touches a
// network, so each one has to fail identically whether or not any of those
// exist. The directory is deliberately empty to prove it.
func TestRunGenerateUsage(t *testing.T) {
	dir := t.TempDir()
	cacheRoot := t.TempDir()

	tests := []struct {
		name       string
		args       []string
		wantStderr string
	}{
		{
			name:       "tag and branch together",
			args:       []string{"generate", "-dir", dir, "-tag", "v1.36.1", "-branch", "master"},
			wantStderr: "only one may be given",
		},
		{
			name:       "offline with an explicit fetch",
			args:       []string{"generate", "-dir", dir, "-offline", "-fetch"},
			wantStderr: "refuses every network operation, so -fetch",
		},
		{
			name:       "offline with a reachable proxy",
			args:       []string{"generate", "-dir", dir, "-offline", "-proxy", "https://proxy.example"},
			wantStderr: "refuses every network operation, so -proxy https://proxy.example",
		},
		{
			name:       "unsupported format",
			args:       []string{"generate", "-dir", dir, "-format", "yaml"},
			wantStderr: `unsupported -format "yaml"`,
		},
		{
			// A generation removes its own scratch root and refuses to write
			// into an existing output tree, and every directory it owns has to
			// sit outside the profile directory, so there is no default it could
			// take without deleting or refusing something nobody named.
			name:       "no cache root",
			args:       []string{"generate", "-dir", dir},
			wantStderr: "-cache is required",
		},
		{
			name: "report inside generated output",
			args: []string{
				"generate", "-dir", dir, "-cache", cacheRoot,
				"-out", cacheRoot + "-module", "-report", filepath.Join(cacheRoot+"-module", "report.json"),
			},
			wantStderr: "must not be inside the generated output tree",
		},
		{
			name:       "unknown flag",
			args:       []string{"generate", "-nope"},
			wantStderr: "flag provided but not defined",
		},
		{
			name:       "operands",
			args:       []string{"generate", "extra"},
			wantStderr: "takes no arguments",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stdout, stderr, code := run(t.Context(), t, "", test.args...)
			if code != cli.ExitUsage {
				t.Fatalf("exit code = %d, want %d\nstdout: %s\nstderr: %s", code, cli.ExitUsage, stdout, stderr)
			}
			if !strings.Contains(stderr, test.wantStderr) {
				t.Fatalf("stderr %q does not contain %q", stderr, test.wantStderr)
			}
			if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
				t.Fatalf("a usage failure touched the working directory: %v %v", entries, err)
			}
		})
	}
}

// TestRunGenerateOfflineAcceptsAProxyThatAgreesWithIt proves the contradiction
// is between offline and a reachable proxy rather than between offline and the
// flag.
//
// An operator who spells out what -offline implies has asked for one thing
// twice, not for two incompatible things, so the run proceeds and fails on the
// profile that is not there.
func TestRunGenerateOfflineAcceptsAProxyThatAgreesWithIt(t *testing.T) {
	base := t.TempDir()
	_, stderr, code := run(t.Context(), t, "", "generate",
		"-dir", filepath.Join(base, "profile"), "-cache", filepath.Join(base, "cache"),
		"-offline", "-proxy", "off")
	if code != cli.ExitFailure {
		t.Fatalf("exit code = %d, want %d\nstderr: %s", code, cli.ExitFailure, stderr)
	}
	if !strings.Contains(stderr, "read profile") {
		t.Fatalf("stderr %q does not report the missing profile", stderr)
	}
}

// TestRunGenerateHelp keeps the rendered flags and the parsed flags in
// agreement, and keeps the command itself discoverable.
func TestRunGenerateHelp(t *testing.T) {
	stdout, stderr, code := run(t.Context(), t, "", "help", "generate")
	if code != cli.ExitOK {
		t.Fatalf("exit code = %d\nstderr: %s", code, stderr)
	}
	for _, flag := range []string{
		"-dir string", "-config string", "-cache string", "-work string", "-out string",
		"-report string", "-tag string", "-branch string", "-patch-branch string",
		"-source-remote string", "-fetch", "-offline", "-materialize", "-keep-worktree",
		"-format string", "-strict", "-proxy string", "-version-index string",
	} {
		if !strings.Contains(stdout, flag) {
			t.Errorf("generate usage does not document %s:\n%s", flag, stdout)
		}
	}

	listing, _, code := run(t.Context(), t, "", "help")
	if code != cli.ExitOK {
		t.Fatalf("exit code = %d", code)
	}
	if !strings.Contains(listing, "generate  compose the generated module") {
		t.Errorf("the top level usage does not list the generate command:\n%s", listing)
	}
}

// TestRunGenerateRefusesABranch proves a ref shape this engine does not
// implement is reported as a finding rather than as a bad command line or a
// broken engine.
//
// A branch needs the pseudo-version resolution path, which needs staging
// repository URLs nothing here verifies, so the refusal is the honest answer.
// It arrives before any subprocess runs, which is why the profile directory
// still holds nothing but the profile afterwards.
func TestRunGenerateRefusesABranch(t *testing.T) {
	dir, cache := generateRoots(t)

	_, stderr, code := run(t.Context(), t, dir, "generate", "-cache", cache, "-branch", "master", "-proxy", "off")
	if code != cli.ExitCheck {
		t.Fatalf("exit code = %d, want %d\nstderr: %s", code, cli.ExitCheck, stderr)
	}
	for _, want := range []string{"does not support the requested run", "is a branch"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr does not explain the refusal (%q):\n%s", want, stderr)
		}
	}
	assertOnlyTheProfile(t, dir)
	if _, err := os.Lstat(cache); !os.IsNotExist(err) {
		t.Errorf("a refused run shape created a cache: %v", err)
	}
}

// TestRunGenerateCancellation proves a cancelled generation reports the
// cancellation rather than a finding about the profile.
func TestRunGenerateCancellation(t *testing.T) {
	dir, cache := generateRoots(t)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, stderr, code := run(ctx, t, dir, "generate", "-cache", cache, "-proxy", "off")
	if code != cli.ExitCanceled {
		t.Fatalf("exit code = %d, want %d\nstderr: %s", code, cli.ExitCanceled, stderr)
	}
}

// TestRunGenerateReportsARuntimeFailure proves a source that cannot be read is
// a condition to repair rather than a finding to review.
//
// The report still has to be written, because a run that measured nothing is
// exactly the case where an operator needs the artifact to say so.
func TestRunGenerateReportsARuntimeFailure(t *testing.T) {
	dir, cache := generateRoots(t)
	reportPath := filepath.Join(t.TempDir(), "reports", "generate.json")

	_, stderr, code := run(t.Context(), t, dir,
		"generate", "-source-remote", "file://"+filepath.Join(t.TempDir(), "absent"),
		"-cache", cache, "-proxy", "off", "-report", reportPath)
	if code != cli.ExitFailure {
		t.Fatalf("exit code = %d, want %d\nstderr: %s", code, cli.ExitFailure, stderr)
	}

	failure := decodeGenerateReport(t, readFile(t, reportPath))
	if failure.Stage != "extract" {
		t.Errorf("failure stage = %q, want extract", failure.Stage)
	}
	if failure.Policy {
		t.Error("a source that could not be cloned was reported as a finding about the profile")
	}
}

// TestRunGenerateWritesAReportForAFinding drives the command through its real
// terminal surface against a real upstream repository.
//
// The engine has its own tests; what this adds is proof that the command line
// wires them together: that the flags resolve to the directories the generation
// uses, that a refusal is reviewable from an artifact rather than from a stderr
// line, that a run which produced no module prints nothing that could be read
// as one, and that the report carries nothing that would make two machines
// disagree.
func TestRunGenerateWritesAReportForAFinding(t *testing.T) {
	ctx := t.Context()
	up := newPlanUpstream(ctx, t)
	dir, cache := generateRoots(t)

	// A proxy nothing can reach, so a phase that resolved a module before the
	// refusal would fail naming this URL, and the report is checked for it
	// afterwards. The refusal arrives while extracting, which is before any
	// module is resolved at all.
	const proxy = "https://proxy.invalid/soapbox"
	reportPath := filepath.Join(t.TempDir(), "reports", "generate.json")
	out := filepath.Join(t.TempDir(), "module")

	stdout, stderr, code := run(ctx, t, dir,
		"generate", "-source-remote", up.url(), "-cache", cache, "-out", out,
		"-proxy", proxy, "-strict", "-format", "json", "-report", reportPath)
	if code != cli.ExitCheck {
		t.Fatalf("exit code = %d, want %d\nstdout: %s\nstderr: %s", code, cli.ExitCheck, stdout, stderr)
	}

	// The requested artifact is still written, because that is what a reviewer
	// acts on. Standard output stays empty: this run produced no module, and a
	// partial rendering there would read as one that did.
	if stdout != "" {
		t.Errorf("a refused generation wrote to standard output:\n%s", stdout)
	}
	written := readFile(t, reportPath)
	failure := decodeGenerateReport(t, written)
	if failure.Stage != "extract" {
		t.Errorf("failure stage = %q, want extract", failure.Stage)
	}
	if !failure.Policy {
		t.Error("a strict refusal was not reported as a finding about the profile")
	}
	if failure.Unsupported {
		t.Error("a strict refusal was reported as an unsupported run shape")
	}
	if failure.Message == "" {
		t.Error("the failure section carries no message")
	}

	// Determinism is what the report is for, so it may name nothing that
	// belongs to this machine rather than to the profile and the source commit.
	for _, secret := range []string{dir, out, proxy, filepath.Dir(reportPath)} {
		if strings.Contains(written, secret) {
			t.Errorf("the written report leaks %q", secret)
		}
	}

	// A generation writes its module only with -materialize, and a refused one
	// writes nothing at all: an output tree left behind is one an operator has
	// to know not to trust.
	if _, err := os.Lstat(out); !os.IsNotExist(err) {
		t.Errorf("the refused generation left an output tree behind: %v", err)
	}
}

// TestRunGenerateMaterializesNothingByDefault proves the default is the
// read-only one.
//
// The command computes and gates the same module either way, so a default that
// wrote would have every exploratory run produce a tree somebody has to clean
// up, and the flag that says "write it" would say nothing.
func TestRunGenerateMaterializesNothingByDefault(t *testing.T) {
	ctx := t.Context()
	up := newPlanUpstream(ctx, t)
	dir, cache := generateRoots(t)

	stdout, stderr, code := run(ctx, t, dir,
		"generate", "-source-remote", up.url(), "-cache", cache, "-proxy", "off")
	// The fixture upstream carries no module metadata, so the generation gets
	// as far as pinning the staging modules and stops there. What the command
	// contributes is that it got that far with the directories it resolved.
	if code != cli.ExitFailure {
		t.Fatalf("exit code = %d, want %d\nstdout: %s\nstderr: %s", code, cli.ExitFailure, stdout, stderr)
	}
	if stdout != "" {
		t.Errorf("a failed generation wrote to standard output:\n%s", stdout)
	}
	if !strings.Contains(stderr, "generate: staging") {
		t.Errorf("stderr does not name the phase that failed:\n%s", stderr)
	}
	if _, err := os.Lstat(cache + "-module"); !os.IsNotExist(err) {
		t.Errorf("a run without -materialize wrote the default output tree: %v", err)
	}

	// The engine owns one directory below the work root and removes it on the
	// way out, whether the run finished or refused, so no source tree it
	// materialized while working survives.
	if entries, err := os.ReadDir(cache + "-work"); err != nil || len(entries) != 0 {
		t.Errorf("the run left scratch state behind: %v %v", entries, err)
	}

	// The cache is the expensive part and it survives, so a second run reuses
	// the clone this one paid for.
	entries, err := os.ReadDir(cache)
	if err != nil {
		t.Fatalf("read the cache root: %v", err)
	}
	if len(entries) == 0 {
		t.Error("the run resolved its ref without leaving a reusable cache")
	}
}

// TestRunGenerateRefusesAConflictingLayout proves a layout the engine cannot
// run is reported as the command line problem it is.
//
// The rule belongs to the engine, which removes its scratch root and would take
// the cache, the repository, or its own output with it. Nothing about a profile,
// a cache, or a network decides the answer, so the run is refused before any of
// them is read: the profile directory here holds nothing at all, and the refusal
// is the same one an operator would get from a repository that does.
func TestRunGenerateRefusesAConflictingLayout(t *testing.T) {
	tests := []struct {
		name string
		args func(dir, cache string) []string
		want string
	}{
		{
			name: "the cache sits inside the profile directory",
			args: func(dir, _ string) []string { return []string{"-cache", filepath.Join(dir, "cache")} },
			want: "contains the source cache root",
		},
		{
			name: "the output tree is the cache root",
			args: func(_, cache string) []string { return []string{"-cache", cache, "-out", cache} },
			want: "the source cache root and the output tree are both",
		},
		{
			name: "the version index sits inside the scratch root",
			args: func(_, cache string) []string {
				return []string{"-cache", cache, "-version-index", filepath.Join(cache+"-work", "index.json")}
			},
			want: "contains the version index",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			base := t.TempDir()
			dir, cache := filepath.Join(base, "profile"), filepath.Join(base, "cache")
			args := append([]string{"generate", "-dir", dir, "-proxy", "off"}, test.args(dir, cache)...)
			stdout, stderr, code := run(t.Context(), t, "", args...)
			if code != cli.ExitUsage {
				t.Fatalf("exit code = %d, want %d\nstdout: %s\nstderr: %s", code, cli.ExitUsage, stdout, stderr)
			}
			if !strings.Contains(stderr, test.want) {
				t.Fatalf("stderr does not report that %q:\n%s", test.want, stderr)
			}
			if entries, err := os.ReadDir(base); err != nil || len(entries) != 0 {
				t.Fatalf("a refused layout created something: %v %v", entries, err)
			}
		})
	}
}

// TestRunGenerateMapsTheEngineLayoutRefusalToUsage covers the layouts the
// command does not decide for itself.
//
// The command checks the pairs it can name before it reads anything, and the
// engine checks every pair at its own boundary, including one this layer leaves
// alone: a version index may live inside the cache and may not swallow it. A
// layout only the engine rejects is still nothing but what the operator typed,
// so it exits as a usage problem rather than as an engine failure.
func TestRunGenerateMapsTheEngineLayoutRefusalToUsage(t *testing.T) {
	dir, cache := generateRoots(t)
	index := filepath.Join(filepath.Dir(cache), "staging-versions.json")

	stdout, stderr, code := run(t.Context(), t, dir,
		"generate", "-cache", cache, "-version-index", index, "-proxy", "off")
	if code != cli.ExitUsage {
		t.Fatalf("exit code = %d, want %d\nstdout: %s\nstderr: %s", code, cli.ExitUsage, stdout, stderr)
	}
	if !strings.Contains(stderr, "the generation directories conflict") {
		t.Fatalf("stderr does not carry the engine's refusal:\n%s", stderr)
	}
	if _, err := os.Lstat(cache); !os.IsNotExist(err) {
		t.Errorf("a refused layout created the cache: %v", err)
	}
}

// generateRoots lays out a profile directory and a cache root that contain
// neither one another nor anything else the run owns, which is the layout the
// engine requires and the command therefore has to be given.
func generateRoots(t *testing.T) (dir, cache string) {
	t.Helper()
	base := t.TempDir()
	dir = filepath.Join(base, "profile")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("create the profile directory: %v", err)
	}
	writeProfile(t, filepath.Join(dir, config.DefaultFileName), planProfile)
	return dir, filepath.Join(base, "cache")
}

// generateFailure is the part of the generation report these tests read.
type generateFailure struct {
	Stage       string `json:"stage"`
	Message     string `json:"message"`
	Policy      bool   `json:"policy"`
	Unsupported bool   `json:"unsupported"`
}

// decodeGenerateReport decodes one written report and requires it to carry a
// failure section.
func decodeGenerateReport(t *testing.T, written string) generateFailure {
	t.Helper()
	var decoded struct {
		Schema  int              `json:"schema"`
		Failure *generateFailure `json:"failure"`
	}
	if err := json.Unmarshal([]byte(written), &decoded); err != nil {
		t.Fatalf("the written report is not JSON: %v", err)
	}
	if decoded.Schema != 1 {
		t.Errorf("report schema = %d, want 1", decoded.Schema)
	}
	if decoded.Failure == nil {
		t.Fatalf("the report carries no failure section:\n%s", written)
	}
	return *decoded.Failure
}

// readFile reads one artifact a command was asked to write.
func readFile(t *testing.T, path string) string {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(contents)
}

// assertOnlyTheProfile proves a refusal created no directory of its own.
func assertOnlyTheProfile(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the profile directory: %v", err)
	}
	for _, entry := range entries {
		if entry.Name() != config.DefaultFileName {
			t.Errorf("the refusal created %s", entry.Name())
		}
	}
}
