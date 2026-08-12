package generate_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/config"
	"github.com/enj/soapbox/tools/internal/extract"
	"github.com/enj/soapbox/tools/internal/generate"
	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/gocli"
)

// fixtureTag is the upstream release every test generates from, and
// fixtureStagingTag is what the profile's v1-to-v0 release policy maps it onto.
const (
	fixtureTag        = "v1.36.1"
	fixtureStagingTag = "v0.36.1"
	fixtureBranch     = "master"
)

// loadProfile decodes the fixture profile through the real decoder.
//
// Running it through config.Decode rather than building a Config literal is what
// keeps the fixture honest: a profile this package could not actually decode
// would otherwise pass every test here while failing the moment an operator
// wrote it down.
func loadProfile(t *testing.T, repository string) *config.Config {
	t.Helper()
	if repository == "" {
		repository = "https://github.com/kubernetes/kubernetes.git"
	}
	source := strings.ReplaceAll(fixtureProfile, "REPOSITORY", repository)
	cfg, err := config.Decode([]byte(source))
	if err != nil {
		t.Fatalf("decode fixture profile: %v", err)
	}
	return cfg
}

// profileDir writes the profile to a directory of its own.
//
// The profile directory is a real directory rather than a reused temporary root
// because the engine checks it for containment against every other directory it
// owns, and a shared root would make that check pass or fail for the wrong
// reason.
func profileDir(t *testing.T, cfg *config.Config) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "profile")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("profile directory: %v", err)
	}
	data, err := cfg.Canonical()
	if err != nil {
		t.Fatalf("canonical profile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, config.DefaultFileName), data, 0o600); err != nil {
		t.Fatalf("profile file: %v", err)
	}
	return dir
}

// anonymousGit builds the Git runner a generation requires.
//
// It carries no caller supplied environment entry, which is what makes it
// anonymous: a generation talks to the public source host and to nothing else,
// and the engine refuses a runner that could carry a publishing credential
// there.
func anonymousGit(t *testing.T) *gitcli.Runner {
	t.Helper()
	git, err := gitcli.New(t.Context(), gitcli.Options{Inherit: []string{"PATH"}})
	if err != nil {
		t.Fatalf("git runner: %v", err)
	}
	return git
}

// offlineGo builds a Go runner that can resolve nothing.
//
// It is the right runner for every test that refuses before a module is ever
// resolved, because a run that reached the toolchain anyway would be caught by
// the proxy being off rather than passing quietly.
func offlineGo(t *testing.T, dir string) *gocli.Runner {
	t.Helper()
	state := filepath.Join(dir, "go-state")
	for _, path := range []string{filepath.Join(state, "home"), filepath.Join(state, "tmp")} {
		if err := os.MkdirAll(path, 0o750); err != nil {
			t.Fatalf("Go isolation directory: %v", err)
		}
	}
	runner, err := gocli.New(t.Context(), gocli.Options{
		Dir:     dir,
		Inherit: []string{"PATH"},
		Isolation: []string{
			"HOME=" + filepath.Join(state, "home"),
			"GOPATH=" + filepath.Join(state, "gopath"),
			"GOMODCACHE=" + filepath.Join(state, "mod"),
			"GOCACHE=" + filepath.Join(state, "build"),
			"GOTMPDIR=" + filepath.Join(state, "tmp"),
		},
		Proxy: gocli.ProxyOff,
	})
	if err != nil {
		t.Fatalf("go runner: %v", err)
	}
	return runner
}

// roots are the absolute directories one generation is given.
type roots struct {
	cache   string
	work    string
	output  string
	store   string
	profile string
}

// newRoots lays out the directories a generation owns below one temporary root.
//
// They are siblings rather than nested, because the engine refuses a layout
// where any one of them contains another: the run removes its own scratch root,
// so a work root holding the cache would delete a clone the operator paid for.
func newRoots(t *testing.T, cfg *config.Config) roots {
	t.Helper()
	base := t.TempDir()
	return roots{
		cache:   filepath.Join(base, "cache"),
		work:    filepath.Join(base, "work"),
		output:  filepath.Join(base, "tree"),
		store:   filepath.Join(base, "index", "versions.json"),
		profile: profileDir(t, cfg),
	}
}

// options assembles a complete set of generation options.
//
// LookupEnv is stubbed to report nothing so a developer whose shell happens to
// hold one of the profile's credential variables does not get a different
// result than CI does.
func (r roots) options(cfg *config.Config, git *gitcli.Runner, goRunner *gocli.Runner) generate.Options {
	return generate.Options{
		Config:      cfg,
		ProfileDir:  r.profile,
		CacheRoot:   r.cache,
		WorkRoot:    r.work,
		OutputRoot:  r.output,
		StorePath:   r.store,
		Ref:         extract.Ref{Kind: extract.RefTag, Name: fixtureTag},
		PatchBranch: fixtureBranch,
		Git:         git,
		Go:          goRunner,
		LookupEnv:   func(string) (string, bool) { return "", false },
	}
}

// baseOptions is the common case: a valid profile, sibling directories, an
// anonymous Git runner, and a Go runner that cannot resolve anything.
func baseOptions(t *testing.T) generate.Options {
	t.Helper()
	cfg := loadProfile(t, "")
	dirs := newRoots(t, cfg)
	return dirs.options(cfg, anonymousGit(t), offlineGo(t, t.TempDir()))
}

// generateFailure runs a generation that is expected to refuse.
func generateFailure(ctx context.Context, t *testing.T, opts generate.Options) (*generate.Result, error) {
	t.Helper()
	result, err := generate.Generate(ctx, opts)
	if err == nil {
		t.Fatalf("generate: got nil error, want a refusal")
	}
	return result, err
}
