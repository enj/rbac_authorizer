package modgen_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/buildinfo"
	"github.com/enj/soapbox/tools/internal/gocli"
	"github.com/enj/soapbox/tools/internal/gomodmap"
	"github.com/enj/soapbox/tools/internal/modgen"
)

// offlineRunner returns a go runner that cannot reach the network, for the
// checks that must fail before any module is resolved.
func offlineRunner(t *testing.T, dir string) *gocli.Runner {
	t.Helper()
	runner, err := gocli.New(t.Context(), gocli.Options{
		Dir:     dir,
		Inherit: []string{"PATH"},
		Proxy:   gocli.ProxyOff,
	})
	if err != nil {
		t.Fatalf("create go runner: %v", err)
	}
	return runner
}

// generatedGoMod renders a module file for the verification tests.
func generatedGoMod(t *testing.T, requirements ...gomodmap.Requirement) []byte {
	t.Helper()
	data, err := modgen.Generate(modgen.Options{
		ModulePath: generatedModulePath,
		Go:         "1.26.0",
		Require:    requirements,
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	return data
}

// TestVerify_Rejects covers the states that are refused before the go command
// is ever asked to resolve anything.
func TestVerify_Rejects(t *testing.T) {
	t.Parallel()

	goMod := generatedGoMod(t, gomodmap.Requirement{Path: "k8s.io/api", Version: "v0.36.1"})

	tests := []struct {
		name    string
		setup   func(t *testing.T) string
		goMod   []byte
		wantErr string
	}{
		{
			// A relative directory would resolve against whatever directory the
			// run happened to start in.
			name:    "relative directory",
			setup:   func(*testing.T) string { return "scratch" },
			goMod:   goMod,
			wantErr: "must be absolute",
		},
		{
			name: "directory does not exist",
			setup: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "missing")
			},
			goMod:   goMod,
			wantErr: "is not usable",
		},
		{
			// A path that exists and is not a directory is reported as itself
			// rather than folded in with a stat failure, which would leave the
			// message wrapping a nil error.
			name: "directory is a file",
			setup: func(t *testing.T) string {
				path := filepath.Join(t.TempDir(), "file")
				if err := os.WriteFile(path, []byte("not a directory\n"), 0o600); err != nil {
					t.Fatalf("write file: %v", err)
				}
				return path
			},
			goMod:   goMod,
			wantErr: "is not a directory",
		},
		{
			// The scratch module is what this pass decides the contents of, so a
			// module file that is already there is a reused directory rather than
			// a generated one.
			name: "module file already exists",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				if err := os.WriteFile(filepath.Join(dir, "go.mod"), goMod, 0o600); err != nil {
					t.Fatalf("write go.mod: %v", err)
				}
				return dir
			},
			goMod:   goMod,
			wantErr: "the scratch module must be generated rather than reused",
		},
		{
			// go.sum is refused for the same reason as go.mod and one more: this
			// pass reads it back into the report, so a checksum file the caller
			// left behind would be published as though tidying had produced it,
			// and the cleanup on failure would delete a file the pass never wrote.
			name: "checksum file already exists",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				if err := os.WriteFile(filepath.Join(dir, "go.sum"), []byte("example.com/x v1.0.0 h1:deadbeef=\n"), 0o600); err != nil {
					t.Fatalf("write go.sum: %v", err)
				}
				return dir
			},
			goMod:   goMod,
			wantErr: "the scratch module must be generated rather than reused",
		},
		{
			name:    "generated module does not parse",
			setup:   func(t *testing.T) string { return t.TempDir() },
			goMod:   []byte("this is not a go.mod\n"),
			wantErr: "generated go.mod",
		},
		{
			// The pass is handed bytes rather than the options that produced them,
			// so a module file that did not come from Generate is caught here. It
			// is tidied by the engine's own go command, and the tidied bytes are
			// what gets published, so one naming another release would be formatted
			// by one toolchain while claiming another.
			name:    "generated module names another toolchain",
			setup:   func(t *testing.T) string { return t.TempDir() },
			goMod:   []byte("module " + generatedModulePath + "\n\ngo 1.26.0\n\ntoolchain go1.25.0\n"),
			wantErr: "is not the engine's pinned",
		},
		{
			// A toolchain may be absent only when the go directive already implies
			// the pinned release, which is the form the go command itself leaves
			// behind.
			name:    "generated module implies an older toolchain",
			setup:   func(t *testing.T) string { return t.TempDir() },
			goMod:   []byte("module " + generatedModulePath + "\n\ngo 1.26.0\n"),
			wantErr: "does not imply the engine's pinned",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			dir := test.setup(t)
			_, err := modgen.Verify(t.Context(), offlineRunner(t, t.TempDir()), modgen.VerifyOptions{
				Dir:   dir,
				GoMod: test.goMod,
			})
			if err == nil {
				t.Fatalf("verify: got nil error, want %q", test.wantErr)
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Errorf("verify: error = %v, want it to contain %q", err, test.wantErr)
			}
		})
	}
}

func TestVerify_ContextCancelled(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := modgen.Verify(ctx, offlineRunner(t, dir), modgen.VerifyOptions{
		Dir:   dir,
		GoMod: generatedGoMod(t, gomodmap.Requirement{Path: "k8s.io/api", Version: "v0.36.1"}),
	})
	if err == nil {
		t.Fatal("verify: got nil error, want a cancellation error")
	}
}

// TestVerify_LeavesNothingOnFailure proves a refused verification does not leave
// a half installed module behind for the next pass to trip over.
//
// Both sides of the write matter. A refusal before the module file is installed
// must not create one, and a refusal after it is installed has to take it back
// out: the pass refuses a directory that already holds a go.mod, so a failure
// that left one there would turn the retry into a complaint about a reused
// scratch module rather than a repeat of the real error.
func TestVerify_LeavesNothingOnFailure(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		goMod func(t *testing.T) []byte
		setup func(t *testing.T, dir string)
	}{
		{
			name:  "refused before the module file is written",
			goMod: func(*testing.T) []byte { return []byte("this is not a go.mod\n") },
			setup: func(*testing.T, string) {},
		},
		{
			// The module is well formed and names a module that does not exist, so
			// the failure lands in the go command after the file is on disk.
			name: "refused after the module file is written",
			goMod: func(t *testing.T) []byte {
				return generatedGoMod(t, gomodmap.Requirement{Path: "soapbox.test/absent", Version: "v1.0.0"})
			},
			setup: func(t *testing.T, dir string) {
				source := "package rbac\n\nimport _ \"soapbox.test/absent\"\n"
				if err := os.WriteFile(filepath.Join(dir, "rbac.go"), []byte(source), 0o600); err != nil {
					t.Fatalf("write source: %v", err)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			test.setup(t, dir)

			_, err := modgen.Verify(t.Context(), offlineRunner(t, dir), modgen.VerifyOptions{
				Dir:   dir,
				GoMod: test.goMod(t),
			})
			if err == nil {
				t.Fatal("verify: got nil error, want the pass to be refused")
			}
			for _, name := range []string{"go.mod", "go.sum"} {
				if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
					t.Errorf("verify left %s behind despite refusing the module", name)
				}
			}
		})
	}
}

// TestVerify_KeepsCallerChecksums proves the refusal of a pre-existing go.sum
// happens before anything is written, so the caller's file is still there
// afterwards.
//
// The precondition and the cleanup are the same list of names. Were the check
// ever to move after the module file is installed, the cleanup on the way out
// would delete a checksum file this pass did not create, which is the one
// outcome refusing the directory exists to prevent.
func TestVerify_KeepsCallerChecksums(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	const checksums = "example.com/x v1.0.0 h1:deadbeef=\n"
	sumPath := filepath.Join(dir, "go.sum")
	if err := os.WriteFile(sumPath, []byte(checksums), 0o600); err != nil {
		t.Fatalf("write go.sum: %v", err)
	}

	_, err := modgen.Verify(t.Context(), offlineRunner(t, dir), modgen.VerifyOptions{
		Dir:   dir,
		GoMod: generatedGoMod(t, gomodmap.Requirement{Path: "k8s.io/api", Version: "v0.36.1"}),
	})
	if err == nil {
		t.Fatal("verify: got nil error, want a reused scratch module to be refused")
	}

	kept, err := os.ReadFile(sumPath) //nolint:gosec // the path is this test's own temporary file
	if err != nil {
		t.Fatalf("read go.sum: %v", err)
	}
	if string(kept) != checksums {
		t.Errorf("go.sum = %q, want the caller's %q left untouched", kept, checksums)
	}
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
		t.Error("verify wrote go.mod into a directory it refused")
	}
}

// TestVerify_AcceptsImpliedToolchain proves a generated module whose go
// directive already selects the pinned release verifies without carrying a
// toolchain directive.
//
// Generate omits the directive in that case because the go command removes it,
// so a verification that insisted on seeing one would refuse every module
// generated from a source whose own go directive is the pinned patch release.
// The tidied file must not grow one either, which is what would otherwise be
// reported as drift.
func TestVerify_AcceptsImpliedToolchain(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	source := "package rbac\n\nimport \"strings\"\n\n// Name is the profile name.\nvar Name = strings.ToLower(\"RBAC\")\n"
	if err := os.WriteFile(filepath.Join(dir, "rbac.go"), []byte(source), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}

	goMod := []byte("module " + generatedModulePath + "\n\ngo " + strings.TrimPrefix(buildinfo.Toolchain, "go") + "\n")
	report, err := modgen.Verify(t.Context(), offlineRunner(t, dir), modgen.VerifyOptions{Dir: dir, GoMod: goMod})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if strings.Contains(string(report.GoMod), "toolchain") {
		t.Errorf("tidied go.mod grew a toolchain directive:\n%s", report.GoMod)
	}
}

// TestVerify_UsesTheScratchModule proves the pass tidies the directory it was
// given rather than the one the runner was built for.
//
// A runner made for some other directory would otherwise tidy whichever module
// contains it, which for an engine run started inside a checkout is the engine's
// own module. The report would then describe a module this pass never generated,
// and the real module would be rewritten on the way.
//
// The runner's own directory holds a module the go command cannot tidy offline,
// which is what makes the two behaviours tell apart: tidying the wrong directory
// fails outright rather than quietly succeeding on a module that happened to
// need no changes.
func TestVerify_UsesTheScratchModule(t *testing.T) {
	t.Parallel()

	elsewhere := t.TempDir()
	const bystander = "module soapbox.test/bystander\n\ngo 1.26.0\n\nrequire soapbox.test/absent v1.0.0\n"
	bystanderPath := filepath.Join(elsewhere, "go.mod")
	if err := os.WriteFile(bystanderPath, []byte(bystander), 0o600); err != nil {
		t.Fatalf("write bystander go.mod: %v", err)
	}
	bystanderSource := "package bystander\n\nimport _ \"soapbox.test/absent\"\n"
	if err := os.WriteFile(filepath.Join(elsewhere, "bystander.go"), []byte(bystanderSource), 0o600); err != nil {
		t.Fatalf("write bystander source: %v", err)
	}

	// The scratch module imports nothing outside the standard library, so the
	// offline runner can tidy it without reaching a proxy.
	dir := t.TempDir()
	source := "package rbac\n\nimport \"strings\"\n\n// Name is the profile name.\nvar Name = strings.ToLower(\"RBAC\")\n"
	if err := os.WriteFile(filepath.Join(dir, "rbac.go"), []byte(source), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	goMod := []byte("module " + generatedModulePath + "\n\ngo 1.26.0\n\ntoolchain " + buildinfo.Toolchain + "\n")

	report, err := modgen.Verify(t.Context(), offlineRunner(t, elsewhere), modgen.VerifyOptions{Dir: dir, GoMod: goMod})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !strings.Contains(string(report.GoMod), "module "+generatedModulePath) {
		t.Errorf("report describes:\n%s\nwant the scratch module %s", report.GoMod, generatedModulePath)
	}
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		t.Errorf("the scratch module was not written: %v", err)
	}

	untouched, err := os.ReadFile(bystanderPath) //nolint:gosec // the path is this test's own temporary file
	if err != nil {
		t.Fatalf("read bystander go.mod: %v", err)
	}
	if string(untouched) != bystander {
		t.Errorf("the runner's own module was rewritten:\n%s\nwant:\n%s", untouched, bystander)
	}
}

// goNetworkEnv opts a run into the tests that resolve real modules.
const goNetworkEnv = "SOAPBOX_GO_NETWORK_TESTS"

// cacheIsolation points the go command at the caches this run may write to.
//
// The go command derives them from HOME when they are not named, and a run whose
// HOME is read-only or too small to hold a module fails for a reason that has
// nothing to do with what is under test. Values already in the environment are
// honoured so an operator can aim a run at a filesystem with room on it.
func cacheIsolation(t *testing.T) []string {
	t.Helper()
	var isolation []string
	for _, name := range []string{"GOPATH", "GOMODCACHE", "GOCACHE"} {
		if value := os.Getenv(name); value != "" {
			isolation = append(isolation, name+"="+value)
		}
	}
	return isolation
}

// TestVerify_RealTidy runs the whole pass against the real Go toolchain and a
// real published module.
//
// It is gated because it resolves a module through the proxy, and the unit
// suite must not depend on the network being reachable: the comparison logic
// this pass is built on is covered offline by the compare tests. What this adds
// is proof that a generated module file survives a real go mod tidy unchanged,
// which no synthetic fixture can establish.
//
//	SOAPBOX_GO_NETWORK_TESTS=1 go test ./internal/modgen/
func TestVerify_RealTidy(t *testing.T) {
	t.Parallel()

	if os.Getenv(goNetworkEnv) == "" {
		t.Skipf("set %s=1 to run the tests that resolve real modules", goNetworkEnv)
	}

	dir := t.TempDir()
	// One file importing one package of one real module, so tidying has a real
	// import graph to reconcile rather than an empty one.
	source := `package rbac

import "golang.org/x/mod/semver"

// Newer reports whether a is a newer version than b.
func Newer(a, b string) bool { return semver.Compare(a, b) > 0 }
`
	if err := os.WriteFile(filepath.Join(dir, "rbac.go"), []byte(source), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}

	// The version has to match the one the engine's own go.mod requires, so the
	// module is already in the cache this run uses.
	goMod := generatedGoMod(t,
		gomodmap.Requirement{Path: "golang.org/x/mod", Version: "v0.39.0"},
	)

	runner, err := gocli.New(t.Context(), gocli.Options{
		Dir:       dir,
		Inherit:   []string{"PATH", "HOME"},
		Isolation: cacheIsolation(t),
	})
	if err != nil {
		t.Fatalf("create go runner: %v", err)
	}

	report, err := modgen.Verify(t.Context(), runner, modgen.VerifyOptions{Dir: dir, GoMod: goMod})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if len(report.Kept) != 1 || report.Kept[0].Path != "golang.org/x/mod" {
		t.Errorf("kept = %v, want golang.org/x/mod", report.Kept)
	}
	if report.Kept[0].Version != "v0.39.0" {
		t.Errorf("kept version = %q, want the pin v0.39.0", report.Kept[0].Version)
	}
	if len(report.GoSum) == 0 {
		t.Error("go.sum is empty for a module that requires a published dependency")
	}
	if !strings.Contains(string(report.GoMod), "golang.org/x/mod v0.39.0") {
		t.Errorf("tidied go.mod lost the pin:\n%s", report.GoMod)
	}
}

// TestVerify_RealTidyDropsUnused proves tidying removes a requirement nothing
// imports and that the pass reports it rather than failing, which is the normal
// outcome of extracting a few packages out of a large module.
func TestVerify_RealTidyDropsUnused(t *testing.T) {
	t.Parallel()

	if os.Getenv(goNetworkEnv) == "" {
		t.Skipf("set %s=1 to run the tests that resolve real modules", goNetworkEnv)
	}

	dir := t.TempDir()
	source := "package rbac\n\n// Nothing here imports anything outside the standard library.\nconst Name = \"rbac\"\n"
	if err := os.WriteFile(filepath.Join(dir, "rbac.go"), []byte(source), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}

	goMod := generatedGoMod(t,
		gomodmap.Requirement{Path: "golang.org/x/mod", Version: "v0.39.0"},
	)
	runner, err := gocli.New(t.Context(), gocli.Options{
		Dir:       dir,
		Inherit:   []string{"PATH", "HOME"},
		Isolation: cacheIsolation(t),
	})
	if err != nil {
		t.Fatalf("create go runner: %v", err)
	}

	report, err := modgen.Verify(t.Context(), runner, modgen.VerifyOptions{Dir: dir, GoMod: goMod})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if len(report.Kept) != 0 {
		t.Errorf("kept = %v, want none", report.Kept)
	}
	if len(report.Dropped) != 1 || report.Dropped[0] != "golang.org/x/mod" {
		t.Errorf("dropped = %v, want golang.org/x/mod", report.Dropped)
	}
}
