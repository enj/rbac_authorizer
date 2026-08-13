package typeswap_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/enj/soapbox/tools/internal/typeswap"
)

// The cases here are the ones an earlier version of this package got wrong.
// Each produced a passing proof on code that should have been refused, or hung
// instead of answering.

// analyzeFiles runs the analysis over a fixture and returns the RBAC pairing.
func analyzeFiles(t *testing.T, files map[string]string) typeswap.PairReport {
	t.Helper()
	ctx := context.Background()

	f := newFixture(t, files)
	analyzer, err := typeswap.New(ctx, typeswap.Options{
		Policy: typeswap.PolicyPreferExternal,
		Pairs:  []typeswap.Pair{rbacPair},
	})
	if err != nil {
		t.Fatalf("new analyzer: %v", err)
	}
	graph := f.graph(rbacRetained...)
	graph.PrunedFiles = rbacPruned

	result, err := analyzer.Analyze(ctx, graph)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	report, ok := result.Pair(internalRBAC)
	if !ok {
		t.Fatalf("result has no pair for %s", internalRBAC)
	}
	return report
}

// TestConversionHelperCallIsNotMechanical is the bug that mattered most.
//
// `out.Kind = sanitize(in.Kind)` and `out.Kind = string(in.Kind)` parse
// identically: a call whose callee is a bare identifier with one argument. The
// classifier used to accept both by shape, which meant any hand written
// transformation passed as a field preserving copy as long as it was spelled
// as a single call. Only the type checker can tell a conversion from a call,
// so that is what decides now.
func TestConversionHelperCallIsNotMechanical(t *testing.T) {
	t.Parallel()

	files := mutate(internalRBACV1+"/zz_generated.conversion.go",
		"	out.Kind = in.Kind",
		"	out.Kind = sanitize(in.Kind)")
	files[internalRBACV1+"/helpers.go"] = `package v1

import "strings"

// sanitize is hand written logic that a conversion must not contain.
func sanitize(in string) string { return strings.TrimSpace(in) }
`

	report := analyzeFiles(t, withRetainedUse(files))
	if report.Action != typeswap.ActionBlocked {
		t.Fatalf("action = %q, want %q", report.Action, typeswap.ActionBlocked)
	}
	analysis, _ := report.Analysis(typeswap.AnalysisConversions)
	blockers := strings.Join(analysis.Blockers, "\n")
	if !strings.Contains(blockers, "sanitize") {
		t.Errorf("conversion blockers do not name the helper call:\n%s", blockers)
	}
	if !strings.Contains(blockers, "hand written logic rather than a conversion") {
		t.Errorf("conversion blockers do not explain the refusal:\n%s", blockers)
	}
}

// TestConversionCastIsStillMechanical is the other half of that boundary. A
// real type conversion must keep passing, or the proof would refuse every
// generated file.
func TestConversionCastIsStillMechanical(t *testing.T) {
	t.Parallel()

	files := mutate(internalRBACV1+"/zz_generated.conversion.go",
		"	out.Kind = in.Kind",
		"	out.Kind = string(in.Kind)")

	report := analyzeFiles(t, withRetainedUse(files))
	analysis, _ := report.Analysis(typeswap.AnalysisConversions)
	if !analysis.Passed {
		t.Fatalf("a real cast was refused: %v", analysis.Blockers)
	}
}

// TestContainerShapeMismatchIsRefused covers two shapes that used to compare
// equal because both mention the same named type.
//
// []PolicyRule and map[string]PolicyRule are different wire formats and
// different Go APIs. So are PolicyRule and *PolicyRule. Comparing the qualified
// name of whatever the container held reported all of them as identical.
func TestContainerShapeMismatchIsRefused(t *testing.T) {
	t.Parallel()

	externalTypes := externalRBAC + "/types.go"
	tests := []struct {
		name     string
		old      string
		swap     string
		mentions string
	}{
		{
			name:     "slice becomes a map",
			old:      "	Rules []PolicyRule `json:\"rules\" protobuf:\"bytes,2,rep,name=rules\"`",
			swap:     "	Rules map[string]PolicyRule `json:\"rules\" protobuf:\"bytes,2,rep,name=rules\"`",
			mentions: "Role.Rules is []k8s.io/api/rbac/v1.PolicyRule internally and map[string]k8s.io/api/rbac/v1.PolicyRule externally",
		},
		{
			name:     "value becomes a pointer",
			old:      "	Rules []PolicyRule `json:\"rules\" protobuf:\"bytes,2,rep,name=rules\"`",
			swap:     "	Rules []*PolicyRule `json:\"rules\" protobuf:\"bytes,2,rep,name=rules\"`",
			mentions: "Role.Rules is k8s.io/api/rbac/v1.PolicyRule internally and *k8s.io/api/rbac/v1.PolicyRule externally",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			report := analyzeFiles(t, withRetainedUse(mutate(externalTypes, test.old, test.swap)))
			if report.Action != typeswap.ActionBlocked {
				t.Fatalf("action = %q, want %q", report.Action, typeswap.ActionBlocked)
			}
			analysis, _ := report.Analysis(typeswap.AnalysisFieldIdentity)
			if analysis.Passed {
				t.Fatal("field identity passed on a container shape mismatch")
			}
			blockers := strings.Join(analysis.Blockers, "\n")
			if !strings.Contains(blockers, test.mentions) {
				t.Errorf("field identity blockers do not mention %q:\n%s", test.mentions, blockers)
			}
		})
	}
}

// TestRecursiveTypesTerminate covers a hang rather than a wrong answer.
//
// A struct with several fields of its own type reaches the same pair of types
// by many distinct paths. Keying the visited set by a rendered path revisited
// the pair once per path, which is exponential in the number of self
// referencing fields; the reproduction took over forty seconds. Keying by the
// pair of types makes it linear.
func TestRecursiveTypesTerminate(t *testing.T) {
	t.Parallel()

	recursive := `
// Node is self referential through several fields at once.
type Node struct {
	Name   string ` + "`json:\"name\" protobuf:\"bytes,1,opt,name=name\"`" + `
	Left   *Node  ` + "`json:\"left,omitempty\" protobuf:\"bytes,2,opt,name=left\"`" + `
	Right  *Node  ` + "`json:\"right,omitempty\" protobuf:\"bytes,3,opt,name=right\"`" + `
	Parent *Node  ` + "`json:\"parent,omitempty\" protobuf:\"bytes,4,opt,name=parent\"`" + `
	Peers  []Node ` + "`json:\"peers,omitempty\" protobuf:\"bytes,5,rep,name=peers\"`" + `
	Cycle  *Ring  ` + "`json:\"cycle,omitempty\" protobuf:\"bytes,6,opt,name=cycle\"`" + `
}

// Ring is mutually recursive with Node.
type Ring struct {
	Head *Node ` + "`json:\"head,omitempty\" protobuf:\"bytes,1,opt,name=head\"`" + `
	Next *Ring ` + "`json:\"next,omitempty\" protobuf:\"bytes,2,opt,name=next\"`" + `
}
`
	files := mutate(externalRBAC+"/types.go", "// Role is a namespaced set of rules.", recursive+"\n// Role is a namespaced set of rules.")
	files[internalRBAC+"/types.go"] = strings.Replace(files[internalRBAC+"/types.go"],
		"// Role is a namespaced set of rules.", recursive+"\n// Role is a namespaced set of rules.", 1)

	// The fixture and the assertions stay on the test goroutine, because
	// t.Fatalf may only be called from it. Only Analyze runs in the background,
	// and it is given a deadline so an exponential regression fails the test
	// instead of hanging the package until the global timeout.
	ctx := context.Background()
	f := newFixture(t, withRetainedUse(files))
	analyzer, err := typeswap.New(ctx, typeswap.Options{
		Policy: typeswap.PolicyPreferExternal,
		Pairs:  []typeswap.Pair{rbacPair},
	})
	if err != nil {
		t.Fatalf("new analyzer: %v", err)
	}
	graph := f.graph(rbacRetained...)
	graph.PrunedFiles = rbacPruned

	type outcome struct {
		result *typeswap.Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := analyzer.Analyze(ctx, graph)
		done <- outcome{result: result, err: err}
	}()

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("analyze: %v", got.err)
		}
		report, ok := got.result.Pair(internalRBAC)
		if !ok {
			t.Fatalf("result has no pair for %s", internalRBAC)
		}
		// The shapes match, so the recursion is not merely terminating, it is
		// reaching the right answer.
		analysis, _ := report.Analysis(typeswap.AnalysisFieldIdentity)
		if !analysis.Passed {
			t.Errorf("recursive types compared unequal: %v", analysis.Blockers)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("comparing self referential and mutually recursive types did not finish")
	}
}

// TestInitializerCallIsInventoried covers a registration the scan used to miss
// entirely.
//
// `var _ = registerTypes()` runs at import time and registers, but it declares
// a blank identifier that never appears in the package scope and it is not an
// init function, so a scan that reads only scope names and init declarations
// finds nothing at all.
func TestInitializerCallIsInventoried(t *testing.T) {
	t.Parallel()

	files := mutate(internalRBAC+"/register.go",
		"func init() {\n\tSchemeBuilder = append(SchemeBuilder, AddToScheme)\n}",
		`var _ = registerTypes()

func registerTypes() error {
	SchemeBuilder = append(SchemeBuilder, AddToScheme)
	return nil
}`)

	report := analyzeFiles(t, files)
	summary := strings.Join(report.ChangeSummary(), "\n")
	if !strings.Contains(summary, "registerTypes") {
		t.Errorf("behavior changes do not inventory the package level initializer:\n%s", summary)
	}
}

// TestMarkerHalvesMustShareAPackage proves a pairing is not assembled from two
// unrelated packages.
//
// Tracking two independent booleans accepted a tree where some package named
// the internal side and a different package named the external side. That pairs
// nothing: upstream records a pairing by writing both directives in one file.
func TestMarkerHalvesMustShareAPackage(t *testing.T) {
	t.Parallel()

	files := mutate(internalRBACV1+"/doc.go",
		"// +k8s:conversion-gen-external-types=k8s.io/api/rbac/v1\n", "")
	files[validationPkg+"/doc.go"] = `// +k8s:conversion-gen-external-types=k8s.io/api/rbac/v1

// Package validation is unrelated to the conversion pairing.
package validation
`

	report := analyzeFiles(t, files)
	analysis, _ := report.Analysis(typeswap.AnalysisMarkers)
	if analysis.Passed {
		t.Fatal("markers passed with the two halves in unrelated packages")
	}
	blockers := strings.Join(analysis.Blockers, "\n")
	if !strings.Contains(blockers, "in one package") {
		t.Errorf("marker blockers do not explain the same package requirement:\n%s", blockers)
	}
}

// TestKeptMarkerIsNotReportedAsStripped proves provenance is not told about a
// change that never happens.
//
// The external types marker names a published package that is never relocated,
// and rewrite's DefaultRules keeps it deliberately. Reporting it as stripped
// put a behaviour change in the record that the rewrite phase does not perform.
func TestKeptMarkerIsNotReportedAsStripped(t *testing.T) {
	t.Parallel()

	report := analyzeFiles(t, rbacFiles)
	summary := strings.Join(report.ChangeSummary(), "\n")

	if !strings.Contains(summary, typeswap.MarkerConversionGen) {
		t.Errorf("the removed conversion-gen marker is not recorded:\n%s", summary)
	}
	if strings.Contains(summary, typeswap.MarkerConversionExternal) {
		t.Errorf("a marker rewrite keeps was reported as stripped:\n%s", summary)
	}
}

// TestInterfaceMethodSetsAreCompared proves an interface pairing is not a
// vacuous pass.
//
// Method sets were taken through a pointer, and the pointer method set of an
// interface is empty, so every interface pairing compared two empty sets and
// passed without checking anything.
func TestInterfaceMethodSetsAreCompared(t *testing.T) {
	t.Parallel()

	iface := `
// Resolver is an interface declared by both packages.
type Resolver interface {
	Resolve(namespace string) ([]PolicyRule, error)
}
`
	files := mutate(externalRBAC+"/types.go", "// Role is a namespaced set of rules.",
		strings.Replace(iface, "Resolve(namespace string)", "Resolve(namespace string, deep bool)", 1)+"\n// Role is a namespaced set of rules.")
	files[internalRBAC+"/types.go"] = strings.Replace(files[internalRBAC+"/types.go"],
		"// Role is a namespaced set of rules.", iface+"\n// Role is a namespaced set of rules.", 1)

	report := analyzeFiles(t, withRetainedUse(files))
	analysis, _ := report.Analysis(typeswap.AnalysisFieldIdentity)
	if analysis.Passed {
		t.Fatal("an interface with a changed method signature compared equal")
	}
	blockers := strings.Join(analysis.Blockers, "\n")
	if !strings.Contains(blockers, "Resolver") {
		t.Errorf("blockers do not mention the interface:\n%s", blockers)
	}
}

// TestReportCarriesSchema proves the encoded shape is versioned, so a field
// rename cannot silently change what provenance records.
func TestReportCarriesSchema(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	f := newFixture(t, rbacFiles)
	analyzer, err := typeswap.New(ctx, typeswap.Options{
		Policy: typeswap.PolicyPreferExternal,
		Pairs:  []typeswap.Pair{rbacPair},
	})
	if err != nil {
		t.Fatalf("new analyzer: %v", err)
	}
	result, err := analyzer.Analyze(ctx, rbacGraph(f))
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if result.Schema != typeswap.ReportSchema {
		t.Errorf("schema = %d, want %d", result.Schema, typeswap.ReportSchema)
	}
	encoded, err := result.JSON()
	if err != nil {
		t.Fatalf("encode result: %v", err)
	}
	for _, want := range []string{`"schema"`, `"pairs"`, `"action"`, `"blockers"`} {
		if !strings.Contains(string(encoded), want) {
			t.Errorf("encoded report has no %s field:\n%s", want, encoded)
		}
	}
}

// TestPrunedGeneratedOutputMarksDirectiveDangling proves a directive is
// dangling when its output is pruned, not only when its target package is.
//
// Removing zz_generated.deepcopy.go while leaving `+k8s:deepcopy-gen=package`
// in place records a generator run that can no longer be reproduced. rewrite's
// DefaultRules removes such markers, so an inventory that only matched target
// values would omit a change the tree actually undergoes.
func TestPrunedGeneratedOutputMarksDirectiveDangling(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	files := mutate(internalRBAC+"/types.go", "// +k8s:deepcopy-gen=package", "// +k8s:deepcopy-gen=package")
	// The retained versioned package carries the marker and its output is
	// pruned alongside the conversions.
	files[internalRBACV1+"/doc.go"] = strings.Replace(files[internalRBACV1+"/doc.go"],
		"// +k8s:defaulter-gen=TypeMeta", "// +k8s:defaulter-gen=TypeMeta\n// +k8s:deepcopy-gen=package", 1)
	files[internalRBACV1+"/zz_generated.deepcopy.go"] = `// Code generated by deepcopy-gen. DO NOT EDIT.

package v1

// DeepCopyStrings copies a slice.
func DeepCopyStrings(in []string) []string {
	out := make([]string, len(in))
	copy(out, in)
	return out
}
`

	f := newFixture(t, files)
	analyzer, err := typeswap.New(ctx, typeswap.Options{
		Policy: typeswap.PolicyPreferExternal,
		Pairs:  []typeswap.Pair{rbacPair},
	})
	if err != nil {
		t.Fatalf("new analyzer: %v", err)
	}
	graph := f.graph(rbacRetained...)
	// Prune entries are written by hand, so this one deliberately uses a form a
	// profile author might type rather than the already-clean form.
	graph.PrunedFiles = append([]string{"./" + internalRBACV1 + "/zz_generated.deepcopy.go"}, rbacPruned...)

	result, err := analyzer.Analyze(ctx, graph)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	report, _ := result.Pair(internalRBAC)

	if got := graph.PrunedGeneratedOutputs(); !containsString(got, typeswap.MarkerDeepCopyGen) {
		t.Fatalf("pruned generated outputs = %v, want it to include %s", got, typeswap.MarkerDeepCopyGen)
	}
	summary := strings.Join(report.ChangeSummary(), "\n")
	if !strings.Contains(summary, typeswap.MarkerDeepCopyGen) {
		t.Errorf("behavior changes do not record the deepcopy marker whose output was pruned:\n%s", summary)
	}
}

// TestPrunePathsAreNormalized proves a prune entry written with a leading dot
// still scopes retained use analysis.
//
// A prune that matches nothing leaves the file it names treated as retained,
// which turns a removed conversion into a retained dependency on the package
// being pruned. That is the direction that breaks, so the normalisation is
// worth pinning.
func TestPrunePathsAreNormalized(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	f := newFixture(t, rbacFiles)
	analyzer, err := typeswap.New(ctx, typeswap.Options{
		Policy: typeswap.PolicyPreferExternal,
		Pairs:  []typeswap.Pair{rbacPair},
	})
	if err != nil {
		t.Fatalf("new analyzer: %v", err)
	}
	graph := f.graph(rbacRetained...)
	graph.PrunedFiles = []string{"./" + internalRBACV1 + "/zz_generated.conversion.go"}

	result, err := analyzer.Analyze(ctx, graph)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	report, _ := result.Pair(internalRBAC)
	if report.Action != typeswap.ActionPruneInternal {
		t.Fatalf("action = %q, want %q; the dotted prune entry did not match", report.Action, typeswap.ActionPruneInternal)
	}
	if len(report.Rewrites) != 0 {
		t.Errorf("rewrites = %v, want none; the pruned conversion file still counted as retained", report.Rewrites)
	}
}

func TestBlankImportBlocksDeadPackagePruning(t *testing.T) {
	t.Parallel()
	files := mutate(validationPkg+"/rule.go",
		`import (
	rbacv1 "k8s.io/api/rbac/v1"
)`,
		`import (
	rbacv1 "k8s.io/api/rbac/v1"
	_ "k8s.io/kubernetes/pkg/apis/rbac"
)`)

	report := analyzeFiles(t, files)
	if report.Action != typeswap.ActionBlocked {
		t.Fatalf("action = %q, want %q", report.Action, typeswap.ActionBlocked)
	}
	analysis, _ := report.Analysis(typeswap.AnalysisReachability)
	if blockers := strings.Join(analysis.Blockers, "\n"); !strings.Contains(blockers, "blank-import") {
		t.Errorf("reachability blockers do not name the side-effect import:\n%s", blockers)
	}
}

// containsString reports whether a slice contains a value.
func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// TestGraphValidationFailsClosed proves an analysis that cannot run is refused
// rather than passed.
//
// A package whose compiled file list does not line up with its parsed files
// would have marker evidence attributed to the wrong file, and a package with
// no type information would make every retained-use scan find nothing and
// report the internal package as dead. Both are the absence of evidence
// standing in for evidence of absence.
func TestGraphValidationFailsClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	analyzer, err := typeswap.New(ctx, typeswap.Options{
		Policy: typeswap.PolicyPreferExternal,
		Pairs:  []typeswap.Pair{rbacPair},
	})
	if err != nil {
		t.Fatalf("new analyzer: %v", err)
	}

	tests := []struct {
		name    string
		mutate  func(*typeswap.Graph)
		problem string
	}{
		{
			name: "compiled files do not align with syntax",
			mutate: func(g *typeswap.Graph) {
				for _, pkg := range g.Packages {
					if pkg.ImportPath == validationPkg {
						pkg.CompiledGoFiles = nil
					}
				}
			},
			problem: "cannot be aligned",
		},
		{
			name: "package carries no type information",
			mutate: func(g *typeswap.Graph) {
				for _, pkg := range g.Packages {
					if pkg.ImportPath == validationPkg {
						pkg.Types = nil
					}
				}
			},
			problem: "not type checked",
		},
		{
			name: "retained package is not loaded",
			mutate: func(g *typeswap.Graph) {
				g.Retained = append(g.Retained, "k8s.io/kubernetes/pkg/registry/rbac/absent")
			},
			problem: "is not loaded",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, rbacFiles)
			graph := f.graph(rbacRetained...)
			graph.PrunedFiles = rbacPruned
			test.mutate(graph)

			_, err := analyzer.Analyze(ctx, graph)
			if err == nil {
				t.Fatalf("Analyze accepted a graph where %s", test.name)
			}
			if !errors.Is(err, typeswap.ErrInvalidOptions) {
				t.Errorf("error does not classify as ErrInvalidOptions: %v", err)
			}
			if !strings.Contains(err.Error(), test.problem) {
				t.Errorf("error %q does not mention %q", err, test.problem)
			}
		})
	}
}
