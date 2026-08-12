package gocli_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/gocli"
)

// offline keeps every test that resolves modules away from the network. A test
// that reached proxy.golang.org would be measuring the internet rather than this
// package.
const offline = gocli.ProxyOff

// newModule writes a minimal module and returns a runner scoped to it.
func newModule(t *testing.T, goMod, source, proxy string) (*gocli.Runner, string) {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), goMod)
	if source != "" {
		writeFile(t, filepath.Join(dir, "z.go"), source)
	}
	return newRunner(t, dir, proxy), dir
}

// newRunner builds a runner over dir under the given proxy policy.
func newRunner(t *testing.T, dir, proxy string) *gocli.Runner {
	t.Helper()
	return buildRunner(t, gocli.Options{Dir: dir, Proxy: proxy})
}

// newSecretRunner builds a runner carrying one credential through Env.
func newSecretRunner(t *testing.T, dir, secret, proxy string) *gocli.Runner {
	t.Helper()
	return buildRunner(t, gocli.Options{
		Dir:   dir,
		Proxy: proxy,
		Env:   []string{"SOAPBOX_TOKEN=" + secret},
	})
}

// buildRunner fills in the isolation every test needs and creates the runner.
func buildRunner(t *testing.T, opts gocli.Options) *gocli.Runner {
	t.Helper()
	runner, err := gocli.New(t.Context(), isolatedOptions(t, opts))
	if err != nil {
		t.Fatalf("create go runner: %v", err)
	}
	return runner
}

// isolatedOptions fills in the isolation every test needs. The caches the test
// process already has are reused, so a test neither compiles from cold nor
// writes into the developer's real ones.
//
// A caller that states its own Inherit keeps it, which is how a test can put a
// variable in the process environment, inherit it on purpose, and still prove
// that the typed option is what decides.
func isolatedOptions(t *testing.T, opts gocli.Options) gocli.Options {
	t.Helper()
	isolation := []string{"HOME=" + newIsolatedHome(t)}
	for _, name := range []string{"GOCACHE", "GOMODCACHE", "GOPATH", "GOTMPDIR", "TMPDIR"} {
		if value, ok := os.LookupEnv(name); ok {
			isolation = append(isolation, name+"="+value)
		}
	}
	if opts.Inherit == nil {
		opts.Inherit = []string{"PATH"}
	}
	opts.Isolation = append(isolation, opts.Isolation...)
	return opts
}

// newIsolatedHome returns a throwaway HOME with telemetry turned off.
//
// The go command derives its telemetry directory from HOME, and in a fresh one
// it writes counter files as it exits. That races the temporary directory
// cleanup, so the mode file is written first: the isolation is the point of the
// directory, and a test that leaves a background writer behind is not isolated.
func newIsolatedHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	writeFile(t, filepath.Join(home, ".config", "go", "telemetry", "mode"), "off\n")
	return home
}

// writeFile writes one fixture file.
func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("create directory for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

const testGoMod = "module example.com/z\n\ngo 1.26.0\n"

func TestNewResolvesTheBinary(t *testing.T) {
	runner := newRunner(t, t.TempDir(), offline)
	if !filepath.IsAbs(runner.Binary()) {
		t.Fatalf("binary %q is not absolute", runner.Binary())
	}
}

func TestNewRejectsBadOptions(t *testing.T) {
	tests := []struct {
		name string
		opts gocli.Options
		want string
	}{
		{
			name: "missing binary",
			opts: gocli.Options{Binary: "soapbox-go-does-not-exist"},
			want: "go executable lookup",
		},
		{
			name: "missing directory",
			opts: gocli.Options{Dir: filepath.Join(t.TempDir(), "absent")},
			want: "go working directory",
		},
		{
			name: "environment entry without a value",
			opts: gocli.Options{Env: []string{"GOFLAGS"}},
			want: "must be KEY=VALUE",
		},
		{
			name: "isolation entry without a name",
			opts: gocli.Options{Isolation: []string{"=value"}},
			want: "must name a variable",
		},
		{
			name: "environment entry with a null byte",
			opts: gocli.Options{Env: []string{"GOFLAGS=a\x00b"}},
			want: "null byte",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner, err := gocli.New(t.Context(), test.opts)
			if err == nil {
				t.Fatalf("options were accepted, runner dir %q", runner.Dir())
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error %q does not mention %q", err, test.want)
			}
		})
	}
}

func TestNewRejectsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := gocli.New(ctx, gocli.Options{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want %v", err, context.Canceled)
	}
}

func TestWithDir(t *testing.T) {
	base := newRunner(t, t.TempDir(), offline)
	dir := t.TempDir()
	file := filepath.Join(dir, "file.txt")
	writeFile(t, file, "x")

	scoped, err := base.WithDir(dir)
	if err != nil {
		t.Fatalf("with dir: %v", err)
	}
	if scoped.Dir() != dir {
		t.Fatalf("dir = %q, want %q", scoped.Dir(), dir)
	}
	if base.Dir() == dir {
		t.Fatal("WithDir mutated the runner it was called on")
	}

	tests := []struct {
		name string
		dir  string
	}{
		{name: "relative", dir: "relative/path"},
		{name: "absent", dir: filepath.Join(dir, "absent")},
		{name: "not a directory", dir: file},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := base.WithDir(test.dir); err == nil {
				t.Fatalf("directory %q was accepted", test.dir)
			}
		})
	}
}

// TestFixedEnvironmentNeutralisesHostileAmbientState is the reason this package
// exists. Every variable below silently changes which modules are trusted, where
// they come from, or which toolchain answers, and each one is set in the process
// environment and named in Inherit so that inheriting it is not what is being
// tested: what is being tested is that the fixed entries still win.
func TestFixedEnvironmentNeutralisesHostileAmbientState(t *testing.T) {
	hostileEnvFile := filepath.Join(t.TempDir(), "hostile-env")
	writeFile(t, hostileEnvFile, "GOFLAGS=-mod=mod\nGOPRIVATE=hostile.example\nGONOPROXY=hostile.example\n")

	hostile := map[string]string{
		"GOENV":               hostileEnvFile,
		"GOFLAGS":             "-mod=mod -tags=hostile",
		"GOWORK":              filepath.Join(t.TempDir(), "hostile.work"),
		"GOPRIVATE":           "hostile.example",
		"GONOPROXY":           "hostile.example",
		"GONOSUMDB":           "hostile.example",
		"GOINSECURE":          "hostile.example",
		"GOTOOLCHAIN":         "go1.99.0",
		"GOVCS":               "*:all",
		"GOAUTH":              "netrc",
		"GOPROXY":             "https://hostile.example,direct",
		"NETRC":               filepath.Join(t.TempDir(), "hostile-netrc"),
		"GIT_CONFIG_GLOBAL":   filepath.Join(t.TempDir(), "hostile-gitconfig"),
		"GIT_CONFIG_NOSYSTEM": "0",
		"GIT_TERMINAL_PROMPT": "1",
		"GIT_ASKPASS":         filepath.Join(t.TempDir(), "hostile-askpass"),
	}
	inherit := []string{"PATH", "HOME"}
	for name, value := range hostile {
		t.Setenv(name, value)
		inherit = append(inherit, name)
	}

	// A HOME carrying the files the go command and git read on their own. It is
	// the one the runner is pointed at, so anything read from it is read on
	// purpose rather than by escaping isolation.
	home := newIsolatedHome(t)
	writeFile(t, filepath.Join(home, ".netrc"), "machine proxy.golang.org login hostile password hostile\n")
	writeFile(t, filepath.Join(home, ".gitconfig"),
		"[url \"https://hostile.example/\"]\n\tinsteadOf = https://\n[credential]\n\thelper = /tmp/hostile-helper\n")

	runner, err := gocli.New(t.Context(), gocli.Options{
		Dir:       t.TempDir(),
		Inherit:   inherit,
		Isolation: []string{"HOME=" + home},
	})
	if err != nil {
		t.Fatalf("create go runner: %v", err)
	}

	want := map[string]string{
		// An empty exemption list means no module is exempt from the proxy or
		// the checksum database, which is the safe direction for all four.
		"GOFLAGS":    "",
		"GOPRIVATE":  "",
		"GONOPROXY":  "",
		"GOINSECURE": "",
		// A workspace above the module directory must not replace the module.
		"GOWORK": "off",
		// A go directive must never silently download a different toolchain.
		"GOTOOLCHAIN": "local",
		// A module fetch must never run a version control tool, and must never
		// go looking for the operator's stored credentials.
		"GOVCS":  "*:off",
		"GOAUTH": "off",
		// The proxy is a typed option, so an ambient one is ignored entirely.
		"GOPROXY": gocli.DefaultProxy,
		// The checksum database is left at its default rather than weakened.
		"GOSUMDB": "sum.golang.org",
	}
	names := make([]string, 0, len(want))
	for name := range want {
		names = append(names, name)
	}
	values, err := runner.Env(t.Context(), names...)
	if err != nil {
		t.Fatalf("go env: %v", err)
	}
	for name, expected := range want {
		if values[name] != expected {
			t.Errorf("%s = %q, want %q", name, values[name], expected)
		}
	}
}

// TestIsolationCannotOverrideFixedPolicy proves the fixed entries are a policy
// rather than a default.
//
// Isolation exists to redirect where the go command keeps state. If the same
// field could also name GOFLAGS, GOVCS, or GOTOOLCHAIN, then every guarantee the
// fixed entries make would hold only until a caller redirected a cache and
// reached one variable too far.
func TestIsolationCannotOverrideFixedPolicy(t *testing.T) {
	for _, entry := range []string{
		"GOFLAGS=-mod=mod",
		"GOWORK=/tmp/hostile.work",
		"GOPRIVATE=hostile.example",
		"GONOPROXY=hostile.example",
		"GONOSUMDB=hostile.example",
		"GOTOOLCHAIN=go1.99.0",
		"GOVCS=*:all",
		"GOAUTH=netrc",
		"GOENV=/tmp/hostile-env",
		"NETRC=/tmp/hostile-netrc",
		"GIT_CONFIG_GLOBAL=/tmp/hostile-gitconfig",
		"GIT_TERMINAL_PROMPT=1",
		"GOPROXY=https://hostile.example",
		"GOSUMDB=off",
	} {
		t.Run(entry, func(t *testing.T) {
			_, err := gocli.New(t.Context(), gocli.Options{
				Dir:       t.TempDir(),
				Inherit:   []string{"PATH"},
				Isolation: []string{entry},
			})
			if err == nil {
				t.Fatal("a policy variable was accepted as an isolation entry")
			}
			if !strings.Contains(err.Error(), "state or cache variable") {
				t.Fatalf("error %q does not explain what Isolation is for", err)
			}
		})
	}
}

// TestEnvCannotOverrideFixedPolicy covers the other caller supplied field. Env
// carries credentials, not policy, so it may not reach the fixed variables or
// the proxy either.
func TestEnvCannotOverrideFixedPolicy(t *testing.T) {
	tests := []struct {
		entry string
		want  string
	}{
		{entry: "GOFLAGS=-mod=mod", want: "fixed by this package"},
		{entry: "GOTOOLCHAIN=go1.99.0", want: "fixed by this package"},
		{entry: "GOVCS=*:all", want: "fixed by this package"},
		{entry: "GOAUTH=netrc", want: "fixed by this package"},
		{entry: "GIT_ASKPASS=/tmp/askpass", want: "fixed by this package"},
		{entry: "GOPROXY=https://hostile.example", want: "must be set through the Proxy option"},
	}
	for _, test := range tests {
		t.Run(test.entry, func(t *testing.T) {
			_, err := gocli.New(t.Context(), gocli.Options{
				Dir:     t.TempDir(),
				Inherit: []string{"PATH"},
				Env:     []string{test.entry},
			})
			if err == nil {
				t.Fatal("a policy variable was accepted as an environment entry")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error %q does not mention %q", err, test.want)
			}
		})
	}
}

// TestIsolationRedirectsState proves the field still does the job it is for.
func TestIsolationRedirectsState(t *testing.T) {
	cache := t.TempDir()
	runner := buildRunner(t, gocli.Options{
		Dir:       t.TempDir(),
		Proxy:     offline,
		Isolation: []string{"GOCACHE=" + cache},
	})
	values, err := runner.Env(t.Context(), "GOCACHE", "GOFLAGS")
	if err != nil {
		t.Fatalf("go env: %v", err)
	}
	if values["GOCACHE"] != cache {
		t.Fatalf("GOCACHE = %q, want %q", values["GOCACHE"], cache)
	}
	if values["GOFLAGS"] != "" {
		t.Fatalf("GOFLAGS = %q, want the fixed empty value", values["GOFLAGS"])
	}
}

// TestProxyPolicy pins the module download policy to a typed option, so a
// silent fallback to fetching from version control cannot be configured by
// accident.
func TestProxyPolicy(t *testing.T) {
	t.Run("default has no direct fallback", func(t *testing.T) {
		runner := buildRunner(t, gocli.Options{Dir: t.TempDir()})
		values, err := runner.Env(t.Context(), "GOPROXY")
		if err != nil {
			t.Fatalf("go env: %v", err)
		}
		if values["GOPROXY"] != gocli.DefaultProxy {
			t.Fatalf("GOPROXY = %q, want %q", values["GOPROXY"], gocli.DefaultProxy)
		}
		if strings.Contains(values["GOPROXY"], "direct") {
			t.Fatal("the default proxy still falls back to a version control fetch")
		}
	})

	t.Run("caller may state one", func(t *testing.T) {
		runner := buildRunner(t, gocli.Options{Dir: t.TempDir(), Proxy: offline})
		values, err := runner.Env(t.Context(), "GOPROXY")
		if err != nil {
			t.Fatalf("go env: %v", err)
		}
		if values["GOPROXY"] != gocli.ProxyOff {
			t.Fatalf("GOPROXY = %q, want %q", values["GOPROXY"], gocli.ProxyOff)
		}
	})

	t.Run("cleartext is refused", func(t *testing.T) {
		_, err := gocli.New(t.Context(), gocli.Options{
			Dir:     t.TempDir(),
			Inherit: []string{"PATH"},
			Proxy:   "http://proxy.invalid",
		})
		if err == nil {
			t.Fatal("a cleartext proxy was accepted")
		}
	})

	t.Run("an embedded password is redacted", func(t *testing.T) {
		const password = "s3cret-proxy-password"
		runner := buildRunner(t, gocli.Options{
			Dir:   t.TempDir(),
			Proxy: "https://user:" + password + "@proxy.example",
		})
		if runner.Redactor().String(password) != gitcli.Placeholder {
			t.Fatal("a password embedded in the proxy URL was not seeded into the redactor")
		}
	})
}

// TestProxyIsDecidedByTheOptionAlone pins the input to the proxy policy.
//
// The ambient GOPROXY here is one validation rejects, and it is named in Inherit
// so that reaching it would be easy. If the option ever stops being the only
// input, every offline test in this package starts failing at construction with
// "proxy must be off, direct, ..." — a message about the machine the tests ran
// on rather than about the code. That is a failure worth catching here, once,
// with the reason attached.
//
// The accepted forms are pinned alongside the refused ones because the two are
// one decision. A form quietly leaving the accepted set would strand a caller,
// and a form quietly joining it is how cleartext or a version control fallback
// gets back in.
func TestProxyIsDecidedByTheOptionAlone(t *testing.T) {
	t.Setenv("GOPROXY", "http://hostile.example")

	tests := []struct {
		name  string
		proxy string
		// want is the value the subprocess must report. Empty means the option
		// is refused before any subprocess starts.
		want string
	}{
		{name: "empty falls back to the default", want: gocli.DefaultProxy},
		{name: "off", proxy: gocli.ProxyOff, want: gocli.ProxyOff},
		{name: "the default stated explicitly", proxy: gocli.DefaultProxy, want: gocli.DefaultProxy},
		{name: "direct", proxy: "direct", want: "direct"},
		{name: "a list", proxy: gocli.DefaultProxy + ",direct", want: gocli.DefaultProxy + ",direct"},
		{name: "a file mirror", proxy: "file:///var/cache/goproxy", want: "file:///var/cache/goproxy"},
		{name: "cleartext", proxy: "http://proxy.invalid"},
		{name: "one cleartext entry in a list", proxy: gocli.DefaultProxy + ",http://proxy.invalid"},
		{name: "a trailing empty entry", proxy: gocli.DefaultProxy + ","},
		{name: "a bare host", proxy: "proxy.example"},
		{name: "a null byte", proxy: gocli.ProxyOff + "\x00"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner, err := gocli.New(t.Context(), isolatedOptions(t, gocli.Options{
				Dir:     t.TempDir(),
				Proxy:   test.proxy,
				Inherit: []string{"PATH", "HOME", "GOPROXY"},
			}))
			if test.want == "" {
				if err == nil {
					t.Fatalf("proxy %q was accepted", test.proxy)
				}
				return
			}
			if err != nil {
				t.Fatalf("create go runner: %v", err)
			}
			values, err := runner.Env(t.Context(), "GOPROXY")
			if err != nil {
				t.Fatalf("go env: %v", err)
			}
			if values["GOPROXY"] != test.want {
				t.Fatalf("GOPROXY = %q, want %q", values["GOPROXY"], test.want)
			}
		})
	}
}

// TestRejectedOptionsNeverEchoTheirValue covers the one window where a
// credential can reach a log through this package.
//
// Options validation runs before the redactor that would mask a secret exists,
// so an error that quoted what it rejected would print the token in clear. Both
// fields that can carry one are checked: a malformed Env entry, where the whole
// entry is the value, and a proxy URL, which is a normal place for a password to
// live. The variable name is fair game in the first case, because the caller
// chose it and it is not the secret.
func TestRejectedOptionsNeverEchoTheirValue(t *testing.T) {
	const secret = "s3cret-should-never-be-printed"

	tests := []struct {
		name string
		opts gocli.Options
	}{
		{
			name: "an environment entry with no separator is all value",
			opts: gocli.Options{Env: []string{secret}},
		},
		{
			name: "an environment entry with a null byte",
			opts: gocli.Options{Env: []string{"SOAPBOX_TOKEN=" + secret + "\x00"}},
		},
		{
			name: "a proxy the policy refuses",
			opts: gocli.Options{Proxy: "http://user:" + secret + "@proxy.invalid"},
		},
		{
			name: "a proxy with a null byte",
			opts: gocli.Options{Proxy: "https://user:" + secret + "@proxy.example\x00"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.opts.Dir = t.TempDir()
			test.opts.Inherit = []string{"PATH"}
			runner, err := gocli.New(t.Context(), test.opts)
			if err == nil {
				t.Fatalf("hostile options were accepted, runner dir %q", runner.Dir())
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("the rejection echoed the secret: %v", err)
			}
		})
	}
}

// TestSecretsNeverReachOutput proves an environment value is redacted out of a
// captured diagnostic stream without being listed in Secrets.
func TestSecretsNeverReachOutput(t *testing.T) {
	const secret = "s3cret-proxy-token"
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), testGoMod)
	// The import names a module that cannot be resolved offline, so the go
	// command quotes the path back, which is how the secret reaches the stream.
	writeFile(t, filepath.Join(dir, "z.go"), "package z\n\nimport _ \"example.com/"+secret+"/pkg\"\n")
	runner := newSecretRunner(t, dir, secret, offline)

	err := runner.Tidy(t.Context(), gocli.TidyOptions{})
	if err == nil {
		t.Fatal("expected a failure for an unresolvable import")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaked the secret: %v", err)
	}

	var execErr *gocli.ExecError
	if !errors.As(err, &execErr) {
		t.Fatalf("error %v is not an ExecError", err)
	}
	if strings.Contains(execErr.Stderr, secret) {
		t.Fatalf("captured stderr leaked the secret: %q", execErr.Stderr)
	}
	// The placeholder proves the value was replaced rather than merely absent.
	if !strings.Contains(execErr.Stderr, gitcli.Placeholder) {
		t.Fatalf("captured stderr %q does not show the redaction placeholder", execErr.Stderr)
	}
	if execErr.ExitCode != 1 {
		t.Fatalf("exit code = %d, want 1", execErr.ExitCode)
	}
	if got := gocli.ExitCodeOf(err); got != 1 {
		t.Fatalf("ExitCodeOf = %d, want 1", got)
	}
}

// TestSecretsAreRedactedFromStandardOutput covers the other stream, where a
// secret can arrive through an argument the caller built.
func TestSecretsAreRedactedFromStandardOutput(t *testing.T) {
	const secret = "s3cret-module-token"
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), testGoMod)
	runner := newSecretRunner(t, dir, secret, offline)

	modules, err := runner.ListModules(t.Context(), "example.com/"+secret+"@v1.0.0")
	if err != nil {
		t.Fatalf("list modules: %v", err)
	}
	if len(modules) != 1 {
		t.Fatalf("got %d modules, want 1", len(modules))
	}
	if strings.Contains(modules[0].Path, secret) {
		t.Fatalf("standard output leaked the secret: %q", modules[0].Path)
	}
	if !strings.Contains(modules[0].Path, gitcli.Placeholder) {
		t.Fatalf("module path %q does not show the redaction placeholder", modules[0].Path)
	}
}

func TestRunHonoursCancellation(t *testing.T) {
	runner, _ := newModule(t, testGoMod, "package z\n", offline)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := runner.Env(ctx, "GOCACHE"); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want %v", err, context.Canceled)
	}
}

// TestModuleCommandsRequireTheirOwnModule proves the go command's upward search
// for go.mod cannot hand a command a module the caller never named. The child
// directory has none, and the parent does, so a command that walked upwards
// would succeed against the wrong module instead of failing.
func TestModuleCommandsRequireTheirOwnModule(t *testing.T) {
	parent := t.TempDir()
	writeFile(t, filepath.Join(parent, "go.mod"), testGoMod)
	child := filepath.Join(parent, "child")
	if err := os.MkdirAll(child, 0o750); err != nil {
		t.Fatalf("create child directory: %v", err)
	}

	ctx := t.Context()
	scoped := newRunner(t, child, offline)
	rootless := newRunner(t, "", offline)

	tests := []struct {
		name string
		call func(*gocli.Runner) error
	}{
		{
			name: "list modules",
			call: func(r *gocli.Runner) error {
				_, err := r.ListModules(ctx, "example.com/z")
				return err
			},
		},
		{
			name: "list packages",
			call: func(r *gocli.Runner) error {
				_, err := r.ListPackages(ctx, gocli.PackageListOptions{Patterns: []string{"./..."}})
				return err
			},
		},
		{
			name: "module graph",
			call: func(r *gocli.Runner) error {
				_, err := r.ModuleGraph(ctx)
				return err
			},
		},
		{
			name: "tidy",
			call: func(r *gocli.Runner) error { return r.Tidy(ctx, gocli.TidyOptions{}) },
		},
		{
			name: "download",
			call: func(r *gocli.Runner) error {
				_, err := r.Download(ctx)
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.call(scoped); err == nil {
				t.Fatal("a directory without go.mod was accepted")
			}
			if err := test.call(rootless); err == nil {
				t.Fatal("a runner without a directory was accepted")
			} else if !strings.Contains(err.Error(), "module directory is required") {
				t.Fatalf("error %q does not name the missing directory", err)
			}
		})
	}
}
