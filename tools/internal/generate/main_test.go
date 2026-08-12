package generate_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// sharedModuleCache and sharedBuildCache are the Go caches every end-to-end test
// resolves and compiles through.
//
// They are the package's own rather than the developer's, and each for its own
// reason.
//
// The module cache is isolated for correctness. The fixture publishes modules at
// fixed versions through a fixture proxy, and a module cache is keyed by path and
// version alone: a developer's cache that already held one of those versions from
// an earlier shape of the fixture would serve that instead, and every test would
// then be checking a tree nobody wrote.
//
// The build cache is isolated for reliability. These tests drive the go command
// dozens of times, and a build cache shared with whatever else is running in the
// same checkout is one another process can leave a half written entry in. The
// symptom is a load that fails with an empty cache entry, which looks exactly
// like a broken fixture and is not one.
//
// Both are shared across the package rather than per test, because compiling the
// same tiny fixture for every test is pure cost.
var (
	sharedModuleCache string
	sharedBuildCache  string
)

// TestMain owns the shared caches for the whole package.
//
// The removal is its own function rather than a defer because os.Exit does not
// run defers, and a module cache left behind is a directory the go command made
// read-only that nothing else will clean up.
func TestMain(m *testing.M) {
	root, err := os.MkdirTemp("", "soapbox-generate")
	if err != nil {
		fmt.Fprintln(os.Stderr, "generate tests: cache root:", err)
		os.Exit(1)
	}
	sharedModuleCache = filepath.Join(root, "mod")
	sharedBuildCache = filepath.Join(root, "build")

	code := m.Run()

	if err := removeAllForced(root); err != nil {
		fmt.Fprintln(os.Stderr, "generate tests: cache cleanup:", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

// moduleCache reports the shared module cache, failing a test that somehow ran
// without TestMain having prepared one.
func moduleCache(t *testing.T) string {
	t.Helper()
	if sharedModuleCache == "" {
		t.Fatal("module cache: TestMain did not prepare one")
	}
	return sharedModuleCache
}

// buildCache reports the shared build cache.
func buildCache(t *testing.T) string {
	t.Helper()
	if sharedBuildCache == "" {
		t.Fatal("build cache: TestMain did not prepare one")
	}
	return sharedBuildCache
}
