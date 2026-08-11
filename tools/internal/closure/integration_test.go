package closure_test

import (
	"os"
	"testing"

	"github.com/enj/soapbox/tools/internal/closure"
)

// kubernetesSourceEnv names a directory holding a materialized Kubernetes
// worktree checked out at the profile's minimum release.
const kubernetesSourceEnv = "KUBERNETES_SOURCE_DIR"

// TestBuild_KubernetesRBAC asserts the real RBAC closure shape the plan
// commits to.
//
// The test is gated on an operator supplied worktree and skips otherwise.
// Cloning kubernetes/kubernetes during a normal go test would make the unit
// suite depend on the network, on several gigabytes of history, and on GitHub
// being reachable, none of which the invariants under test actually need: the
// synthetic fixtures in this package already cover the engine's behaviour. What
// this test adds is proof that the behaviour meets real upstream source, so it
// runs in the dry-run phase where a worktree already exists.
//
// Materialize one with a blobless clone and a sparse checkout, then:
//
//	KUBERNETES_SOURCE_DIR=/path/to/kubernetes go test ./internal/closure/
func TestBuild_KubernetesRBAC(t *testing.T) {
	dir := os.Getenv(kubernetesSourceEnv)
	if dir == "" {
		t.Skipf("set %s to a kubernetes worktree checked out at v1.36.1", kubernetesSourceEnv)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("%s=%q is not a directory: %v", kubernetesSourceEnv, dir, err)
	}

	// These values mirror soapbox.yaml. They are duplicated rather than loaded
	// from the profile because internal/closure stays independent of
	// internal/config, and because a test that read the profile would pass even
	// if the profile drifted away from the shape the plan approved.
	opts := closure.Options{
		Root:         dir,
		ImportPrefix: "k8s.io/kubernetes",
		Roots:        []string{"plugin/pkg/auth/authorizer/rbac"},
		PruneFiles: []string{
			"pkg/apis/rbac/v1/defaults.go",
			"pkg/apis/rbac/v1/helpers.go",
			"pkg/apis/rbac/v1/register.go",
			"pkg/apis/rbac/v1/zz_generated.conversion.go",
			"pkg/apis/rbac/v1/zz_generated.deepcopy.go",
			"pkg/apis/rbac/v1/zz_generated.defaults.go",
			"pkg/apis/rbac/v1/zz_generated.validations.go",
			"pkg/registry/rbac/validation/internal_version_adapter.go",
		},
		RequiredFiles: []string{
			"pkg/apis/rbac/v1/doc.go",
			"pkg/apis/rbac/v1/evaluation_helpers.go",
			"pkg/registry/rbac/validation/policy_compact.go",
			"pkg/registry/rbac/validation/rule.go",
			"plugin/pkg/auth/authorizer/rbac/rbac.go",
			"plugin/pkg/auth/authorizer/rbac/subject_locator.go",
		},
		DeniedImports: []string{"k8s.io/kubernetes/pkg/apis/rbac"},
		Limits: closure.Limits{
			MaxPackages:      8,
			MaxFiles:         40,
			MaxNonTestLines:  5000,
			MaxPackageGrowth: 2,
		},
	}

	result := build(t, opts)

	// The pre-prune closure is four packages, and pruning eight exact files
	// leaves three. pkg/apis/rbac disappears because the pruned files were its
	// only importers, not because any of its own files were removed.
	if got, want := result.Report.Observed.PrePrune.Packages, 4; got != want {
		t.Errorf("pre-prune packages = %d, want %d", got, want)
	}
	assertStrings(t, "post-prune packages", result.Report.Exact.Packages, []string{
		"k8s.io/kubernetes/pkg/apis/rbac/v1",
		"k8s.io/kubernetes/pkg/registry/rbac/validation",
		"k8s.io/kubernetes/plugin/pkg/auth/authorizer/rbac",
	})
	assertStrings(t, "removed files", result.RemovedFiles, opts.PruneFiles)
	if got, want := len(result.RemovedFiles), 8; got != want {
		t.Errorf("pruned files = %d, want %d", got, want)
	}

	// The exact deny rule matches the unversioned internal API package only.
	// Its /v1 helper subpackage is retained, keeping doc.go and
	// evaluation_helpers.go.
	assertStrings(t, "retained pkg/apis/rbac/v1 files", filesUnder(result, "pkg/apis/rbac/v1/"), []string{
		"pkg/apis/rbac/v1/doc.go",
		"pkg/apis/rbac/v1/evaluation_helpers.go",
	})

	for _, pkg := range result.Report.Exact.Packages {
		if pkg == "k8s.io/kubernetes/pkg/apis/rbac" {
			t.Errorf("packages still contain the denied internal API package")
		}
	}

	t.Logf("pre-prune: %+v", result.Report.Observed.PrePrune)
	t.Logf("post-prune: %+v", result.Report.Observed.PostPrune)
	t.Logf("external boundary packages: %d", len(result.Report.Exact.ExternalPackages))
}

// filesUnder lists the copy plan paths below a directory prefix.
func filesUnder(result *closure.Result, prefix string) []string {
	var out []string
	for _, file := range result.Report.Exact.Files {
		if len(file) > len(prefix) && file[:len(prefix)] == prefix {
			out = append(out, file)
		}
	}
	return out
}
