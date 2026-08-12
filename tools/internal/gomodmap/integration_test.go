package gomodmap_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/buildinfo"
	"github.com/enj/soapbox/tools/internal/gomodmap"
)

// kubernetesGoModPath is the real root go.mod of kubernetes/kubernetes v1.36.1,
// served verbatim by the module proxy as k8s.io/kubernetes@v1.36.1.mod.
//
// It is checked in because everything this parser insists on is an assumption
// about someone else's repository. Fixtures written from those assumptions can
// only confirm them; the release itself is the thing that can contradict them.
const kubernetesGoModPath = "testdata/kubernetes-v1.36.1.mod"

// The shape of that release, stated as numbers so a change to it fails here
// rather than downstream. Kubernetes stages 33 modules and requires 31 of them:
// sample-cli-plugin and sample-controller are published from the tree without
// the root module building against either.
const (
	kubernetesStagingModules  = 33
	kubernetesStagingRequired = 31
	kubernetesExternal        = 171
)

// TestParseRootModule_Kubernetes parses the real root go.mod of the release the
// profile extracts from.
func TestParseRootModule_Kubernetes(t *testing.T) {
	t.Parallel()

	root := parseKubernetesGoMod(t, kubernetesGoModPath)

	if root.Path != "k8s.io/kubernetes" {
		t.Errorf("module path = %q, want k8s.io/kubernetes", root.Path)
	}
	// The engine module declares the same language version as the release it
	// transforms, so the engine never compiles the extracted code under an older
	// language than upstream did.
	if root.Go != buildinfo.GoDirective {
		t.Errorf("go directive = %q, but the engine declares %q; they must not drift", root.Go, buildinfo.GoDirective)
	}
	// The release sets no toolchain of its own, which is why the generated module
	// falls back to the engine's pin rather than inheriting one.
	if root.Toolchain != "" {
		t.Errorf("toolchain directive = %q, want none", root.Toolchain)
	}
	// The godebug default is the one directive the generated module inherits, so
	// a release that stopped setting it would silently change how extracted code
	// behaves.
	if want := []gomodmap.Godebug{{Key: "default", Value: "go1.26"}}; len(root.Godebug) != 1 || root.Godebug[0] != want[0] {
		t.Errorf("godebug = %v, want %v", root.Godebug, want)
	}

	if len(root.Staging) != kubernetesStagingModules {
		t.Errorf("found %d staging modules, want %d", len(root.Staging), kubernetesStagingModules)
	}
	if len(root.External) != kubernetesExternal {
		t.Errorf("found %d external requirements, want %d", len(root.External), kubernetesExternal)
	}

	var required int
	for _, module := range root.Staging {
		if !strings.HasPrefix(module.Path, "k8s.io/") {
			t.Errorf("staging module %q is not under k8s.io/", module.Path)
		}
		if want := gomodmap.StagingDir + "/" + module.Path; module.Dir != want {
			t.Errorf("staging module %s is provided by %q, want %q", module.Path, module.Dir, want)
		}
		if module.Required {
			required++
		}
		// Kubernetes marks none of its own staging requirements indirect, because
		// the root module builds against them directly.
		if module.Indirect {
			t.Errorf("staging module %s is marked indirect", module.Path)
		}
	}
	if required != kubernetesStagingRequired {
		t.Errorf("%d staging modules are required, want %d", required, kubernetesStagingRequired)
	}

	// The two modules the RBAC profile reaches have to be among them, and both
	// are modules the root module really builds against.
	for _, want := range []string{"k8s.io/api", "k8s.io/apimachinery"} {
		module, ok := root.StagingModuleOf(want)
		if !ok {
			t.Errorf("staging module %s is missing", want)
			continue
		}
		if !module.Required {
			t.Errorf("staging module %s is not required by the root module", want)
		}
	}

	// These two are staged but deliberately not required. A parser insisting that
	// every replacement carries a requirement refuses the real release outright,
	// so this is the assertion that pins the shape rather than an edge case.
	for _, want := range []string{"k8s.io/sample-cli-plugin", "k8s.io/sample-controller"} {
		module, ok := root.StagingModuleOf(want)
		if !ok {
			t.Errorf("staging module %s is missing", want)
			continue
		}
		if module.Required {
			t.Errorf("staging module %s is now required by the root module, which is a different shape than the one modelled here", want)
		}
	}

	// Every external version has to be one the generated module could pin as is.
	// The release carries +incompatible versions and pseudo-versions among them,
	// so this is the check that the exactness rule was written against real
	// versions rather than against tidy ones.
	for _, requirement := range root.External {
		if err := gomodmap.ValidateExactVersion(requirement.Version); err != nil {
			t.Errorf("external requirement %s: %v", requirement.Path, err)
		}
	}
}

// kubernetesSourceEnv names a directory holding a materialized Kubernetes
// worktree checked out at the profile's minimum release. It is the same variable
// the closure integration test reads, so one export covers both.
const kubernetesSourceEnv = "KUBERNETES_SOURCE_DIR"

// TestParseRootModule_KubernetesWorktree proves the checked-in copy is still the
// file upstream ships.
//
// The copy above is what makes the parser's assumptions testable without a
// network or several gigabytes of history, and it is also the thing that can go
// stale. An operator who has the worktree gets that copy compared against it, so
// a release whose shape moved is caught as a stale fixture rather than as a
// passing test of the wrong bytes.
//
//	KUBERNETES_SOURCE_DIR=/path/to/kubernetes go test ./internal/gomodmap/
func TestParseRootModule_KubernetesWorktree(t *testing.T) {
	t.Parallel()

	dir := os.Getenv(kubernetesSourceEnv)
	if dir == "" {
		t.Skipf("set %s to a kubernetes worktree checked out at the profile's minimum release", kubernetesSourceEnv)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("%s=%q is not a directory: %v", kubernetesSourceEnv, dir, err)
	}

	checkedIn, err := os.ReadFile(kubernetesGoModPath)
	if err != nil {
		t.Fatalf("read %s: %v", kubernetesGoModPath, err)
	}
	path := filepath.Join(dir, gomodmap.RootModulePath)
	worktree, err := os.ReadFile(path) //nolint:gosec // the path is the operator's own worktree
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(checkedIn) != string(worktree) {
		t.Errorf("%s no longer matches %s; the release moved and the checked-in copy has to be refreshed with it", kubernetesGoModPath, path)
	}
	// Parsing it anyway is what turns a refreshed release into a parser failure
	// rather than only a diff.
	if _, err := gomodmap.ParseRootModule(path, worktree); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
}

// parseKubernetesGoMod reads and parses a checked-in Kubernetes root go.mod.
func parseKubernetesGoMod(t *testing.T, path string) *gomodmap.RootModule {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // the path is a checked-in test fixture
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	root, err := gomodmap.ParseRootModule(path, data)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return root
}
