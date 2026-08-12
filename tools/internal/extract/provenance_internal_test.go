package extract

import (
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/rewrite"
)

// TestResultCopiesPackageProvenance proves two results built from one run share
// no writable evidence.
//
// A run produces a result more than once: a phase returns one, and a refusal
// discovered afterwards renders another from the same state. The records are
// pointers into a graph the run assembled, so handing the same pointers to both
// would let a caller that sorts, trims, or annotates one result silently rewrite
// the other, and the one that reaches an operator is not always the one that was
// edited. The copy is what makes that impossible, and this test reaches every
// level of the graph a caller can write through.
func TestResultCopiesPackageProvenance(t *testing.T) {
	r := &run{records: []*rewrite.PackageProvenance{samplePackageProvenance()}}

	first := r.result()
	second := r.result()
	want := samplePackageProvenance().Render()
	if got := second.Provenance[0].Render(); got != want {
		t.Fatalf("the exposed record does not match what the run holds:\n--- want ---\n%s\n--- got ---\n%s", want, got)
	}

	victim := first.Provenance[0]
	victim.Package = "internal/tampered"
	victim.SourcePackage = "pkg/tampered"
	victim.SourceSHA = strings.Repeat("0", 40)
	victim.Files[0].Path = "internal/tampered/rule.go"
	victim.Files[0].Changes[0].To = "tampered"
	victim.Files = append(victim.Files, rewrite.FileProvenance{Path: "internal/tampered/extra.go"})
	victim.Pruned = append(victim.Pruned, "pkg/invented/file.go")
	victim.Patches = append(victim.Patches, "invented.patch")

	if got := second.Provenance[0].Render(); got != want {
		t.Errorf("editing one result changed another built from the same run:\n--- want ---\n%s\n--- got ---\n%s", want, got)
	}
	if got := r.records[0].Render(); got != want {
		t.Errorf("editing a result changed the run's own record:\n--- want ---\n%s\n--- got ---\n%s", want, got)
	}
	if first.Provenance[0] == second.Provenance[0] {
		t.Error("two results share one record pointer")
	}
}

// TestResultWithoutProvenanceExposesNone proves a refusal that stopped before
// relocation reports no evidence rather than an empty claim about a tree it
// never built.
func TestResultWithoutProvenanceExposesNone(t *testing.T) {
	r := &run{}
	if got := r.result().Provenance; got != nil {
		t.Errorf("a run that recorded nothing exposed %d records", len(got))
	}
}

// TestClonePackageProvenanceSkipsNilRecords proves the copy cannot turn a
// missing record into one a caller would dereference.
func TestClonePackageProvenanceSkipsNilRecords(t *testing.T) {
	records := []*rewrite.PackageProvenance{nil, samplePackageProvenance(), nil}
	cloned := clonePackageProvenance(records)
	if len(cloned) != 1 {
		t.Fatalf("clone produced %d records, want 1", len(cloned))
	}
	if cloned[0] == nil {
		t.Fatal("clone produced a nil record")
	}
}

// samplePackageProvenance builds a record carrying every field the copy has to
// reach, so a shallow copy shows up as a changed rendering rather than as a
// field nobody asserted on.
func samplePackageProvenance() *rewrite.PackageProvenance {
	record := rewrite.NewPackageProvenance("internal/kk/pkg/registry/rbac/validation", "pkg/registry/rbac/validation",
		rewrite.Options{
			SourceRepository: "https://github.com/kubernetes/kubernetes.git",
			SourceSHA:        strings.Repeat("a", 40),
		})
	record.AddFile(
		rewrite.File{
			Path:       "internal/kk/pkg/registry/rbac/validation/rule.go",
			SourcePath: "pkg/registry/rbac/validation/rule.go",
		},
		rewrite.Result{Changes: []rewrite.Change{{
			Kind: rewrite.ChangeImport,
			Path: "internal/kk/pkg/registry/rbac/validation/rule.go",
			Line: 21,
			From: "k8s.io/kubernetes/pkg/apis/rbac",
			To:   "k8s.io/api/rbac/v1",
		}}},
	)
	record.AddPruned("pkg/registry/rbac/validation/internal_version_adapter.go")
	record.AddPatches("0001-example.patch")
	return record
}
