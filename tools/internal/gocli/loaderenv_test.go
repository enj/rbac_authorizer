package gocli_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/gocli"
)

// envMap collapses a KEY=VALUE list the way a process does, with the last entry
// for a name winning. The tests below assert on the collapsed view because that
// is what the go command actually sees, and asserting on entry order instead
// would pass for an environment whose fixed entries had been overridden.
func envMap(entries []string) map[string]string {
	values := make(map[string]string, len(entries))
	for _, entry := range entries {
		if name, value, ok := strings.Cut(entry, "="); ok {
			values[name] = value
		}
	}
	return values
}

func TestLoaderEnvStatesEveryLocation(t *testing.T) {
	cache := t.TempDir()
	runner := buildRunner(t, gocli.Options{
		Dir:       t.TempDir(),
		Proxy:     offline,
		Isolation: []string{"GOCACHE=" + cache},
	})

	env, err := runner.LoaderEnv(t.Context())
	if err != nil {
		t.Fatalf("loader environment: %v", err)
	}
	values := envMap(env)

	if values["GOCACHE"] != cache {
		t.Fatalf("GOCACHE = %q, want the isolated %q", values["GOCACHE"], cache)
	}
	// A load that is not told where these are resolves them from HOME, which is
	// the ambient state the explicit environment exists to replace.
	for _, name := range []string{"GOROOT", "GOMODCACHE", "GOPATH"} {
		if values[name] == "" {
			t.Fatalf("%s is missing, so a load would resolve it from ambient state", name)
		}
	}

	for name, want := range map[string]string{
		"GOFLAGS":     "",
		"GOWORK":      "off",
		"GOENV":       "off",
		"GOTOOLCHAIN": "local",
		"LC_ALL":      "C",
		"GOPROXY":     offline,
	} {
		got, ok := values[name]
		if !ok {
			t.Fatalf("%s is missing, so the ambient value would decide", name)
		}
		if got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
}

// TestLoaderEnvOmitsCredentials pins the one difference between the loader
// environment and the environment the runner's own commands use. Env is this
// package's credential channel, and a load needs no credential of its own.
func TestLoaderEnvOmitsCredentials(t *testing.T) {
	const secret = "loader-token-value"
	runner := buildRunner(t, gocli.Options{
		Dir:   t.TempDir(),
		Proxy: offline,
		Env:   []string{"SOAPBOX_TOKEN=" + secret},
	})

	env, err := runner.LoaderEnv(t.Context())
	if err != nil {
		t.Fatalf("loader environment: %v", err)
	}
	for _, entry := range env {
		if strings.Contains(entry, secret) {
			t.Fatalf("loader environment carries a credential in %q", entry)
		}
	}
	if _, ok := envMap(env)["SOAPBOX_TOKEN"]; ok {
		t.Fatal("loader environment carries the Env entry that holds the credential")
	}
}

// TestLoaderEnvOverridesInheritedPolicy proves the fixed entries win over an
// inherited one. Ordering is what makes them fixed, so a change that appended
// the inherited snapshot last would still pass every other test here.
func TestLoaderEnvOverridesInheritedPolicy(t *testing.T) {
	t.Setenv("GOFLAGS", "-mod=mod")
	t.Setenv("GOTOOLCHAIN", "go1.0.0")
	runner := buildRunner(t, gocli.Options{
		Dir:     t.TempDir(),
		Proxy:   offline,
		Inherit: []string{"PATH", "GOFLAGS", "GOTOOLCHAIN"},
	})

	env, err := runner.LoaderEnv(t.Context())
	if err != nil {
		t.Fatalf("loader environment: %v", err)
	}
	values := envMap(env)
	if values["GOFLAGS"] != "" {
		t.Fatalf("GOFLAGS = %q, want the inherited -mod=mod to be overridden", values["GOFLAGS"])
	}
	if values["GOTOOLCHAIN"] != "local" {
		t.Fatalf("GOTOOLCHAIN = %q, want local", values["GOTOOLCHAIN"])
	}
}

func TestLoaderEnvStatesTheTemporaryDirectory(t *testing.T) {
	tmp := t.TempDir()
	runner := buildRunner(t, gocli.Options{
		Dir:       t.TempDir(),
		Proxy:     offline,
		Isolation: []string{"GOTMPDIR=" + tmp},
	})

	env, err := runner.LoaderEnv(t.Context())
	if err != nil {
		t.Fatalf("loader environment: %v", err)
	}
	if got := envMap(env)["GOTMPDIR"]; got != tmp {
		t.Fatalf("GOTMPDIR = %q, want %q", got, tmp)
	}
}

// TestLoaderEnvNeverWritesAnEmptyTemporaryDirectory covers the one variable an
// empty value is an answer for. An empty GOTMPDIR means the system temporary
// directory; writing the entry out would state an empty path instead.
func TestLoaderEnvNeverWritesAnEmptyTemporaryDirectory(t *testing.T) {
	runner := buildRunner(t, gocli.Options{Dir: t.TempDir(), Proxy: offline})

	env, err := runner.LoaderEnv(t.Context())
	if err != nil {
		t.Fatalf("loader environment: %v", err)
	}
	if slices.Contains(env, "GOTMPDIR=") {
		t.Fatal("an empty GOTMPDIR was written out, which states a path rather than a default")
	}
}

// TestLoaderEnvReturnsAFreshSlice proves a caller cannot reach back into the
// runner. The loader is handed this slice and keeps it for the length of a
// load, so a second caller mutating it would change what the first one loaded.
func TestLoaderEnvReturnsAFreshSlice(t *testing.T) {
	runner := buildRunner(t, gocli.Options{Dir: t.TempDir(), Proxy: offline})

	first, err := runner.LoaderEnv(t.Context())
	if err != nil {
		t.Fatalf("loader environment: %v", err)
	}
	for i := range first {
		first[i] = "MUTATED=1"
	}
	second, err := runner.LoaderEnv(t.Context())
	if err != nil {
		t.Fatalf("second loader environment: %v", err)
	}
	if slices.Contains(second, "MUTATED=1") {
		t.Fatal("mutating one loader environment changed the next one")
	}
	if envMap(second)["GOTOOLCHAIN"] != "local" {
		t.Fatal("the second loader environment lost its fixed entries")
	}
}
