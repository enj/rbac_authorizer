package modulegraph_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/deppolicy"
	"github.com/enj/soapbox/tools/internal/modulegraph"
)

// spec is the adaptation every dependency test starts from.
func spec() modulegraph.DeppolicySpec {
	return modulegraph.DeppolicySpec{
		Boundary:   []string{facadePkg},
		Candidates: []string{stagingText},
	}
}

func TestDeppolicyAdaptsEveryField(t *testing.T) {
	t.Parallel()
	_, graph := shared(t)

	adapted, err := graph.Deppolicy(t.Context(), spec())
	if err != nil {
		t.Fatalf("adapt dependency policy graph: %v", err)
	}

	if adapted.Fset == nil {
		t.Fatal("the graph carries no file set, so evidence could not carry positions")
	}
	if len(adapted.Boundary) != 1 || adapted.Boundary[0].ImportPath != facadePkg {
		t.Fatalf("boundary = %v, want just the facade", adapted.Boundary)
	}
	if got := adapted.Boundary[0].Module; got != generatedModule {
		t.Fatalf("boundary module = %q, want %q", got, generatedModule)
	}

	if len(adapted.Candidates) != 1 {
		t.Fatalf("got %d candidates, want 1", len(adapted.Candidates))
	}
	candidate := adapted.Candidates[0]
	if candidate.StagingPath != stagingText {
		t.Fatalf("staging path = %q, want %q", candidate.StagingPath, stagingText)
	}
	// deppolicy checks this itself, and a mismatch is the shape that makes a
	// copy preserve the wrong nested internal visibility.
	if candidate.Package.ImportPath != deppolicy.ImportPathOf(stagingText) {
		t.Fatalf("candidate %q does not provide import path %q",
			candidate.StagingPath, candidate.Package.ImportPath)
	}
	if candidate.Package.Types == nil || candidate.Package.Info == nil || len(candidate.Package.Syntax) == 0 {
		t.Fatal("the candidate arrived without the type information the global state scan reads")
	}
	// The policy opens Dir as a root and reads the files named inside it, so a
	// loader's absolute path here would name a file that is not in the root.
	if want := []string{"text.go"}; !slices.Equal(candidate.Package.GoFiles, want) {
		t.Fatalf("candidate files = %v, want %v", candidate.Package.GoFiles, want)
	}
}

// TestDeppolicyBuildGraphIsComplete is what the diamond gate depends on. The
// gate walks these edges looking for a retained reacher, and a walk that ends
// at an edge naming a node the graph does not hold finds none, which passes the
// candidate it should have refused.
func TestDeppolicyBuildGraphIsComplete(t *testing.T) {
	t.Parallel()
	_, graph := shared(t)

	adapted, err := graph.Deppolicy(t.Context(), spec())
	if err != nil {
		t.Fatalf("adapt dependency policy graph: %v", err)
	}

	nodes := make(map[string]bool, len(adapted.Build))
	for _, pkg := range adapted.Build {
		if pkg.ImportPath == "" {
			t.Fatal("a build package has no import path")
		}
		nodes[pkg.ImportPath] = true
	}
	for _, pkg := range adapted.Build {
		for _, imported := range pkg.Imports {
			if !nodes[imported] {
				t.Fatalf("package %q imports %q, which is not a node of the build graph", pkg.ImportPath, imported)
			}
		}
	}

	for _, want := range []string{facadePkg, internalPkg, externalPkg, "strings"} {
		if !nodes[want] {
			t.Fatalf("the build graph is missing %q", want)
		}
	}
	// A standard library package has no module, and the gates read an empty
	// module as exactly that rather than as an unnamed one.
	for _, pkg := range adapted.Build {
		if pkg.ImportPath == "strings" && pkg.Module != "" {
			t.Fatalf("the standard library package reported module %q", pkg.Module)
		}
		if pkg.ImportPath == externalPkg {
			if pkg.Module != utilModule {
				t.Fatalf("candidate module = %q, want %q", pkg.Module, utilModule)
			}
			if pkg.Lines <= 0 {
				t.Fatal("the candidate reported no compiled lines, which reads as unmeasured")
			}
		}
	}
}

// TestDeppolicyLeavesUnmeasuredFactsUnknown is the rule that keeps a copy from
// being approved on missing evidence. An unmeasured zip size and a zip size of
// zero are the same number with opposite meanings, so this package supplies
// neither.
func TestDeppolicyLeavesUnmeasuredFactsUnknown(t *testing.T) {
	t.Parallel()
	_, graph := shared(t)

	adapted, err := graph.Deppolicy(t.Context(), spec())
	if err != nil {
		t.Fatalf("adapt dependency policy graph: %v", err)
	}

	module := moduleOf(t, adapted, utilModule)
	if module.ZipBytesKnown || module.CadenceKnown || module.LicensesVerified {
		t.Fatalf("module %+v claims facts nobody measured", module)
	}
	if module.ZipBytes != 0 || module.ReleasesPerMinor != 0 || len(module.Licenses) != 0 {
		t.Fatalf("module %+v carries invented measurements", module)
	}
	// Identity is a fact the load does establish, and dropping it would leave
	// the policy unable to attach a later measurement to anything.
	if module.Dir == "" {
		t.Fatal("the module identity carries no root, so provenance could not record it")
	}
}

func TestDeppolicyMergesMeasuredFacts(t *testing.T) {
	t.Parallel()
	_, graph := shared(t)

	resolved := moduleOf(t, mustAdapt(t, graph, spec()), utilModule)

	measured := spec()
	measured.Modules = []deppolicy.Module{{
		Path:             utilModule,
		ZipBytes:         4096,
		ZipBytesKnown:    true,
		ReleasesPerMinor: 2,
		CadenceKnown:     true,
		Licenses:         []deppolicy.License{{Identifier: "Apache-2.0", Files: []string{"LICENSE"}}},
		LicensesVerified: true,
	}}

	module := moduleOf(t, mustAdapt(t, graph, measured), utilModule)
	if !module.ZipBytesKnown || module.ZipBytes != 4096 {
		t.Fatalf("zip size = %d known=%v, want the measured 4096", module.ZipBytes, module.ZipBytesKnown)
	}
	if !module.CadenceKnown || module.ReleasesPerMinor != 2 {
		t.Fatalf("cadence = %d known=%v, want the measured 2", module.ReleasesPerMinor, module.CadenceKnown)
	}
	if !module.LicensesVerified || len(module.Licenses) != 1 || module.Licenses[0].Identifier != "Apache-2.0" {
		t.Fatalf("licences = %+v, want the verified Apache-2.0", module.Licenses)
	}
	// Identity stays the load's, so supplying facts about a module is not a way
	// to rename or relocate it.
	if module.Version != resolved.Version || module.Dir != resolved.Dir {
		t.Fatalf("identity = %q in %q, want the resolved %q in %q",
			module.Version, module.Dir, resolved.Version, resolved.Dir)
	}
}

func TestDeppolicyRefusesFactsItCannotAttach(t *testing.T) {
	t.Parallel()
	_, graph := shared(t)
	resolved := moduleOf(t, mustAdapt(t, graph, spec()), utilModule)

	tests := []struct {
		name   string
		module deppolicy.Module
		want   string
	}{
		{
			name:   "module the build does not contain",
			module: deppolicy.Module{Path: "k8s.io/absent", ZipBytesKnown: true},
			want:   `name "k8s.io/absent", which the build does not contain`,
		},
		{
			name:   "different version",
			module: deppolicy.Module{Path: utilModule, Version: resolved.Version + "-stale", ZipBytesKnown: true},
			want:   "was measured at version",
		},
		{
			name:   "different root",
			module: deppolicy.Module{Path: utilModule, Dir: resolved.Dir + "-elsewhere", ZipBytesKnown: true},
			want:   "was measured in",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			measured := spec()
			measured.Modules = []deppolicy.Module{test.module}

			adapted, err := graph.Deppolicy(t.Context(), measured)
			if err == nil {
				t.Fatalf("stale evidence was accepted and returned %v", adapted)
			}
			if !errors.Is(err, modulegraph.ErrModuleConflict) {
				t.Fatalf("error = %v, want ErrModuleConflict", err)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error %v does not mention %q", err, test.want)
			}
		})
	}
}

func TestDeppolicyRejectsBadSpecs(t *testing.T) {
	t.Parallel()
	_, graph := shared(t)

	tests := []struct {
		name    string
		mutate  func(*modulegraph.DeppolicySpec)
		wantErr error
		want    string
	}{
		{
			name:    "no boundary",
			mutate:  func(s *modulegraph.DeppolicySpec) { s.Boundary = nil },
			wantErr: modulegraph.ErrOptions,
			want:    "at least one boundary package is required",
		},
		{
			name:    "candidate that is not a staging path",
			mutate:  func(s *modulegraph.DeppolicySpec) { s.Candidates = []string{externalPkg} },
			wantErr: modulegraph.ErrOptions,
			want:    "is not a staging path",
		},
		{
			name:    "boundary package the graph does not hold",
			mutate:  func(s *modulegraph.DeppolicySpec) { s.Boundary = []string{"example.test/absent"} },
			wantErr: modulegraph.ErrPackageMissing,
			want:    "boundary package",
		},
		{
			name: "candidate the graph does not hold",
			mutate: func(s *modulegraph.DeppolicySpec) {
				s.Candidates = []string{"staging/src/k8s.io/utilpkg/absent"}
			},
			wantErr: modulegraph.ErrPackageMissing,
			want:    "which is not in the module graph",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			s := spec()
			test.mutate(&s)

			adapted, err := graph.Deppolicy(t.Context(), s)
			if err == nil {
				t.Fatalf("a bad specification was accepted and returned %v", adapted)
			}
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want %v", err, test.wantErr)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error %v does not mention %q", err, test.want)
			}
		})
	}
}

// TestDeppolicyGraphIsAcceptedByTheDecider runs the real decider over the real
// adapted graph, which is the only way to prove the shape satisfies the
// validation deppolicy performs and does not export.
func TestDeppolicyGraphIsAcceptedByTheDecider(t *testing.T) {
	t.Parallel()
	_, graph := shared(t)

	adapted, err := graph.Deppolicy(t.Context(), spec())
	if err != nil {
		t.Fatalf("adapt dependency policy graph: %v", err)
	}
	decider, err := deppolicy.New(t.Context(), deppolicy.Options{
		ModulePath:     generatedModule,
		InternalPrefix: internalPrefix,
		SourceMinor:    36,
		Policy:         deppolicy.PolicyExternal,
		Gates:          deppolicy.Gates{Interoperability: true, GlobalState: true, Diamond: true},
	})
	if err != nil {
		t.Fatalf("create decider: %v", err)
	}

	result, err := decider.Decide(t.Context(), adapted)
	if err != nil {
		t.Fatalf("the adapted graph was refused by the decider: %v", err)
	}
	if len(result.Candidates) != 1 {
		t.Fatalf("got %d candidate reports, want 1", len(result.Candidates))
	}
}

// TestDeppolicyCopiesWhatItHandsOut proves neither the caller's specification
// nor an earlier adaptation can reach into a later one. Two policies read one
// load, so a shared slice would let one silently change what the other judged.
func TestDeppolicyCopiesWhatItHandsOut(t *testing.T) {
	t.Parallel()
	_, graph := shared(t)

	licenses := []deppolicy.License{{Identifier: "Apache-2.0", Files: []string{"LICENSE"}}}
	measured := spec()
	measured.Modules = []deppolicy.Module{{Path: utilModule, Licenses: licenses, LicensesVerified: true}}

	first, err := graph.Deppolicy(t.Context(), measured)
	if err != nil {
		t.Fatalf("adapt dependency policy graph: %v", err)
	}
	licenses[0].Identifier = "MIT"
	licenses[0].Files[0] = "COPYING"
	first.Build[0].ImportPath = "example.test/mutated"

	if got := moduleOf(t, first, utilModule); got.Licenses[0].Identifier != "Apache-2.0" || got.Licenses[0].Files[0] != "LICENSE" {
		t.Fatalf("licences = %+v, want the graph to hold its own copy", got.Licenses)
	}
	second, err := graph.Deppolicy(t.Context(), measured)
	if err != nil {
		t.Fatalf("adapt dependency policy graph: %v", err)
	}
	if second.Build[0].ImportPath == "example.test/mutated" {
		t.Fatal("mutating one build graph changed the next one")
	}
}

// mustAdapt adapts the graph or fails the test.
func mustAdapt(t *testing.T, graph *modulegraph.Graph, s modulegraph.DeppolicySpec) *deppolicy.Graph {
	t.Helper()
	adapted, err := graph.Deppolicy(t.Context(), s)
	if err != nil {
		t.Fatalf("adapt dependency policy graph: %v", err)
	}
	return adapted
}

// moduleOf returns the recorded facts for one module of an adapted graph.
func moduleOf(t *testing.T, graph *deppolicy.Graph, modulePath string) deppolicy.Module {
	t.Helper()
	for _, module := range graph.Modules {
		if module.Path == modulePath {
			return module
		}
	}
	t.Fatalf("module %q is not in the adapted graph", modulePath)
	return deppolicy.Module{}
}
