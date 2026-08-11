package rewrite_test

import (
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/rewrite"
)

// buildProvenance assembles one package record from a rewritten package. The
// files are added in an order no closure would produce, so the rendering has to
// impose the order itself.
func buildProvenance(t *testing.T) *rewrite.PackageProvenance {
	t.Helper()

	options := noticedOptions()
	options.Directives.Dangling = prunedTargets("package")

	record := rewrite.NewPackageProvenance("internal/kk/pkg/apis/rbac/v1", "pkg/apis/rbac/v1", options)
	for _, file := range []rewrite.File{
		goFile("pkg/apis/rbac/v1/evaluation_helpers.go",
			"package v1\n\nimport \"k8s.io/kubernetes/pkg/apis/rbac\"\n\nvar _ = rbac.PolicyRule{}\n"),
		goFile("pkg/apis/rbac/v1/doc.go",
			"// +k8s:deepcopy-gen=package\n// +groupName=rbac.authorization.k8s.io\n\npackage v1\n"),
		goFile("pkg/apis/rbac/v1/untouched.go", "package v1\n\nimport \"k8s.io/api/rbac/v1\"\n\nvar _ = v1.PolicyRule{}\n"),
	} {
		result, err := rewrite.GoFile(t.Context(), file, options)
		if err != nil {
			t.Fatalf("rewrite %s: %v", file.Path, err)
		}
		record.AddFile(file, result)
	}
	record.AddPruned("pkg/apis/rbac/v1/register.go")
	record.AddPruned("pkg/apis/rbac/v1/defaults.go", "pkg/apis/rbac/v1/register.go")
	record.AddPatches("patches/0002-adapt.patch", "patches/0001-export.patch")
	return record
}

func TestPackageProvenanceRender(t *testing.T) {
	t.Parallel()

	got := buildProvenance(t).Render()
	want := `soapbox package provenance
package: internal/kk/pkg/apis/rbac/v1
upstream package: pkg/apis/rbac/v1
upstream repository: https://github.com/kubernetes/kubernetes.git
upstream commit: 0123456789abcdef0123456789abcdef01234567

files:
  internal/kk/pkg/apis/rbac/v1/doc.go
    upstream: pkg/apis/rbac/v1/doc.go
    1 marker-removal - // +k8s:deepcopy-gen=package
    4 notice + // This file was modified by soapbox and is not the upstream original. ...
  internal/kk/pkg/apis/rbac/v1/evaluation_helpers.go
    upstream: pkg/apis/rbac/v1/evaluation_helpers.go
    1 notice + // This file was modified by soapbox and is not the upstream original. ...
    3 import k8s.io/kubernetes/pkg/apis/rbac -> monis.app/kk/rbac_authorizer/internal/kk/pkg/apis/rbac
  internal/kk/pkg/apis/rbac/v1/untouched.go
    upstream: pkg/apis/rbac/v1/untouched.go
    unchanged

pruned:
  pkg/apis/rbac/v1/defaults.go
  pkg/apis/rbac/v1/register.go

patches:
  patches/0002-adapt.patch
  patches/0001-export.patch
`
	if got != want {
		t.Errorf("rendered:\n%s\nwant:\n%s", got, want)
	}
}

// TestPackageProvenanceIsDeterministic asserts a stable rendering. The record
// is committed into the generated module, so an unstable order would show up as
// a diff in every release even when nothing changed.
func TestPackageProvenanceIsDeterministic(t *testing.T) {
	t.Parallel()

	first := buildProvenance(t).Render()
	for range 3 {
		if got := buildProvenance(t).Render(); got != first {
			t.Fatalf("rendered:\n%s\nwant:\n%s", got, first)
		}
	}
}

// TestPackageProvenanceRecordsWhatChanged asserts the record answers the three
// questions it exists for: where a file came from, what the engine changed, and
// what was removed on the way.
func TestPackageProvenanceRecordsWhatChanged(t *testing.T) {
	t.Parallel()

	rendered := buildProvenance(t).Render()
	for _, want := range []string{
		"upstream commit: 0123456789abcdef0123456789abcdef01234567",
		"upstream: pkg/apis/rbac/v1/doc.go",
		"marker-removal - // +k8s:deepcopy-gen=package",
		"import k8s.io/kubernetes/pkg/apis/rbac -> monis.app/kk/rbac_authorizer/internal/kk/pkg/apis/rbac",
		"pkg/apis/rbac/v1/register.go",
		"patches/0001-export.patch",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("record does not mention %q:\n%s", want, rendered)
		}
	}
	// A marker the profile keeps must not be reported as removed.
	if strings.Contains(rendered, "groupName") {
		t.Errorf("record reports a change to a kept marker:\n%s", rendered)
	}
}

func TestPackageProvenanceEmpty(t *testing.T) {
	t.Parallel()

	record := rewrite.NewPackageProvenance("internal/kk/pkg/a", "pkg/a", rewrite.Options{})
	got := record.Render()
	want := `soapbox package provenance
package: internal/kk/pkg/a
upstream package: pkg/a

files:
  (none)

pruned:
  (none)

patches:
  (none)
`
	if got != want {
		t.Errorf("rendered:\n%s\nwant:\n%s", got, want)
	}
}

func TestProvenanceFileName(t *testing.T) {
	t.Parallel()

	if rewrite.ProvenanceFileName != "SOAPBOX_PROVENANCE.txt" {
		t.Errorf("provenance file name is %q", rewrite.ProvenanceFileName)
	}
	if strings.HasSuffix(rewrite.ProvenanceFileName, ".go") {
		t.Error("the provenance record would be compiled into the module")
	}
}
