package typeswap_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/typeswap"
)

// mutate returns the RBAC fixture with one file rewritten.
//
// Every case below changes exactly one thing about a fixture that otherwise
// proves cleanly, so a failure names the property that was broken rather than
// leaving it to be inferred from a pile of differences.
func mutate(file, old, replacement string) map[string]string {
	changed := make(map[string]string, len(rbacFiles))
	for name, contents := range rbacFiles {
		if name == file {
			contents = strings.Replace(contents, old, replacement, 1)
		}
		changed[name] = contents
	}
	return changed
}

// withRetainedUse makes the fixture exercise a real type substitution rather
// than dead-package pruning. The structural proofs are preconditions for this
// path because the retained alias would change Go type identity.
func withRetainedUse(files map[string]string) map[string]string {
	changed := make(map[string]string, len(files))
	for name, contents := range files {
		changed[name] = contents
	}
	name := validationPkg + "/rule.go"
	changed[name] = strings.Replace(changed[name],
		`import (
	rbacv1 "k8s.io/api/rbac/v1"
)`,
		`import (
	rbacv1 "k8s.io/api/rbac/v1"
	rbac "k8s.io/kubernetes/pkg/apis/rbac"
)

// InternalRule is retained code that still names the internal type.
type InternalRule = rbac.PolicyRule`, 1)
	return changed
}

// TestProofsRefuseBrokenSubstitutions is the counterweight to the RBAC proof.
//
// A passing proof is only worth something if the same analysis fails on code
// that should fail. Each case here would produce a module that compiles and a
// test suite that passes while silently dropping a field, reordering a wire
// format, or removing a method a caller depends on.
func TestProofsRefuseBrokenSubstitutions(t *testing.T) {
	t.Parallel()

	conversionFile := internalRBACV1 + "/zz_generated.conversion.go"
	externalTypes := externalRBAC + "/types.go"
	docFile := internalRBACV1 + "/doc.go"

	tests := []struct {
		name     string
		files    map[string]string
		analysis string
		mentions string
	}{
		{
			name: "hand written loop in a conversion",
			files: mutate(conversionFile,
				"	out.Verbs = *(*[]string)(unsafe.Pointer(&in.Verbs))",
				`	for _, verb := range in.Verbs {
		if verb != "deprecated" {
			out.Verbs = append(out.Verbs, verb)
		}
	}`),
			analysis: typeswap.AnalysisConversions,
			mentions: "hand written logic",
		},
		{
			name: "conversion silently drops a field",
			files: mutate(conversionFile,
				"	out.NonResourceURLs = *(*[]string)(unsafe.Pointer(&in.NonResourceURLs))\n",
				""),
			analysis: typeswap.AnalysisConversions,
			mentions: "does not assign NonResourceURLs",
		},
		{
			name: "conversion computes a value instead of copying one",
			files: mutate(conversionFile,
				"	out.Name = in.Name",
				`	out.Name = in.Name + "-migrated"`),
			analysis: typeswap.AnalysisConversions,
			mentions: "computes a value rather than copying one",
		},
		{
			name: "protobuf field number changed",
			files: mutate(externalTypes,
				`protobuf:"bytes,5,rep,name=nonResourceURLs"`,
				`protobuf:"bytes,6,rep,name=nonResourceURLs"`),
			analysis: typeswap.AnalysisFieldIdentity,
			mentions: "protobuf tag",
		},
		{
			name: "json name changed",
			files: mutate(externalTypes,
				`json:"resourceNames,omitempty"`,
				`json:"resource_names,omitempty"`),
			analysis: typeswap.AnalysisFieldIdentity,
			mentions: "json tag",
		},
		{
			name: "field order swapped",
			files: mutate(externalTypes,
				"	Kind      string `json:\"kind\" protobuf:\"bytes,1,opt,name=kind\"`\n	APIGroup  string `json:\"apiGroup,omitempty\" protobuf:\"bytes,2,opt,name=apiGroup\"`",
				"	APIGroup  string `json:\"apiGroup,omitempty\" protobuf:\"bytes,2,opt,name=apiGroup\"`\n	Kind      string `json:\"kind\" protobuf:\"bytes,1,opt,name=kind\"`"),
			analysis: typeswap.AnalysisFieldIdentity,
			mentions: "field 0 is Kind internally and APIGroup externally",
		},
		{
			name: "field type changed inside a nested type",
			files: mutate(externalTypes,
				"	NonResourceURLs []string `json:\"nonResourceURLs,omitempty\" protobuf:\"bytes,5,rep,name=nonResourceURLs\"`",
				"	NonResourceURLs []int `json:\"nonResourceURLs,omitempty\" protobuf:\"bytes,5,rep,name=nonResourceURLs\"`"),
			analysis: typeswap.AnalysisFieldIdentity,
			mentions: "PolicyRule.NonResourceURLs is string internally and int externally",
		},
		{
			name: "external type loses a method",
			files: mutate(externalTypes,
				"// String renders the role for logging.\nfunc (r *Role) String() string { return r.Name }",
				""),
			analysis: typeswap.AnalysisMethodSets,
			mentions: "has method String that k8s.io/api/rbac/v1.Role does not",
		},
		{
			name:     "no generator directive pairs the packages",
			files:    mutate(docFile, "// +k8s:conversion-gen=k8s.io/kubernetes/pkg/apis/rbac\n", ""),
			analysis: typeswap.AnalysisMarkers,
			mentions: "no upstream generator directive pairs",
		},
		{
			name:     "packages describe different API groups",
			files:    mutate(externalTypes, "// +groupName=rbac.authorization.k8s.io", "// +groupName=rbac.example.com"),
			analysis: typeswap.AnalysisMarkers,
			mentions: "declare different API groups",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			f := newFixture(t, withRetainedUse(test.files))
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

			if report.Action != typeswap.ActionBlocked {
				t.Fatalf("action = %q, want %q\n%s", report.Action, typeswap.ActionBlocked, result)
			}
			analysis, ok := report.Analysis(test.analysis)
			if !ok {
				t.Fatalf("report has no %s analysis", test.analysis)
			}
			if analysis.Passed {
				t.Fatalf("%s passed on broken code; blockers elsewhere: %v", test.analysis, report.Blockers)
			}
			blockers := strings.Join(analysis.Blockers, "\n")
			if !strings.Contains(blockers, test.mentions) {
				t.Errorf("%s blockers do not mention %q:\n%s", test.analysis, test.mentions, blockers)
			}
		})
	}
}

// TestRetainedUseProducesRewrites covers the third action. When retained code
// does name an internal type and every proof holds, the answer is to rewrite
// the references rather than to prune.
func TestRetainedUseProducesRewrites(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	files := withRetainedUse(rbacFiles)

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
	report, _ := result.Pair(internalRBAC)

	if len(report.Blockers) > 0 {
		t.Fatalf("pairing is blocked by %v", report.Blockers)
	}
	if report.Action != typeswap.ActionRewriteReferences {
		t.Fatalf("action = %q, want %q", report.Action, typeswap.ActionRewriteReferences)
	}
	if len(report.Rewrites) != 1 {
		t.Fatalf("rewrites = %+v, want exactly one", report.Rewrites)
	}
	rewrite := report.Rewrites[0]
	if rewrite.Symbol != internalRBAC+".PolicyRule" {
		t.Errorf("rewrite symbol = %q, want %q", rewrite.Symbol, internalRBAC+".PolicyRule")
	}
	if rewrite.Replacement != externalRBAC+".PolicyRule" {
		t.Errorf("rewrite replacement = %q, want %q", rewrite.Replacement, externalRBAC+".PolicyRule")
	}
	if rewrite.Package != validationPkg {
		t.Errorf("rewrite package = %q, want %q", rewrite.Package, validationPkg)
	}
	for _, test := range []struct {
		name     string
		mentions string
	}{
		{typeswap.AnalysisConversions, "is field preserving"},
		{typeswap.AnalysisMethodSets, "equivalent method sets"},
		{typeswap.AnalysisFieldIdentity, "match recursively on field names"},
	} {
		analysis, _ := report.Analysis(test.name)
		if !analysis.Passed {
			t.Errorf("%s failed for a real rewrite: %v", test.name, analysis.Blockers)
		}
		if evidence := strings.Join(analysis.Evidence, "\n"); !strings.Contains(evidence, test.mentions) {
			t.Errorf("%s rewrite evidence does not mention %q:\n%s", test.name, test.mentions, evidence)
		}
	}
}

// TestDeadPackagePruningDoesNotClaimTypeIdentity models Kubernetes' real RBAC
// pair: internal and public declarations may differ, but no retained reference
// is converted from one to the other. Those differences must still block the
// rewrite path above and must not become a false blocker for dead-code pruning.
func TestDeadPackagePruningDoesNotClaimTypeIdentity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	externalTypes := externalRBAC + "/types.go"
	conversionFile := internalRBACV1 + "/zz_generated.conversion.go"
	files := mutate(externalTypes,
		`json:"resourceNames,omitempty"`,
		`json:"resource_names,omitempty"`)
	files[externalTypes] = strings.Replace(files[externalTypes],
		"// String renders the role for logging.\nfunc (r *Role) String() string { return r.Name }", "", 1)
	files[conversionFile] = strings.Replace(files[conversionFile],
		"\tout.NonResourceURLs = *(*[]string)(unsafe.Pointer(&in.NonResourceURLs))\n", "", 1)

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
	report, _ := result.Pair(internalRBAC)
	if report.Action != typeswap.ActionPruneInternal {
		t.Fatalf("action = %q, want %q; blockers: %v", report.Action, typeswap.ActionPruneInternal, report.Blockers)
	}
	for _, name := range []string{
		typeswap.AnalysisConversions,
		typeswap.AnalysisMethodSets,
		typeswap.AnalysisFieldIdentity,
	} {
		analysis, _ := report.Analysis(name)
		if !analysis.Passed {
			t.Errorf("inapplicable %s analysis blocked pruning: %v", name, analysis.Blockers)
		}
		evidence := strings.Join(analysis.Evidence, "\n")
		if !strings.Contains(evidence, "makes no claim that the declarations are interchangeable") {
			t.Errorf("%s evidence claims or implies type identity:\n%s", name, evidence)
		}
	}
}

// TestRetainedUseOfAbsentSymbolBlocks proves a substitution that cannot name
// its replacement is refused rather than performed with a guess.
func TestRetainedUseOfAbsentSymbolBlocks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	files := mutate(internalRBAC+"/types.go",
		"// String renders the role for logging.",
		`// EscalationCheck exists only in the internal package.
type EscalationCheck struct {
	Allowed bool
}

// String renders the role for logging.`)
	files[validationPkg+"/rule.go"] = strings.Replace(files[validationPkg+"/rule.go"],
		`import (
	rbacv1 "k8s.io/api/rbac/v1"
)`,
		`import (
	rbacv1 "k8s.io/api/rbac/v1"
	rbac "k8s.io/kubernetes/pkg/apis/rbac"
)

// Check is retained code naming a type the published package does not have.
type Check = rbac.EscalationCheck`, 1)

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
	report, _ := result.Pair(internalRBAC)

	if report.Action != typeswap.ActionBlocked {
		t.Fatalf("action = %q, want %q", report.Action, typeswap.ActionBlocked)
	}
	blockers := strings.Join(report.Blockers, "\n")
	if !strings.Contains(blockers, "declares no EscalationCheck") {
		t.Errorf("blockers do not name the absent symbol:\n%s", blockers)
	}
}

// TestKeepInternalSkipsTheProof records that a profile choosing not to
// substitute produces an explicit action rather than an empty report.
func TestKeepInternalSkipsTheProof(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	f := newFixture(t, rbacFiles)
	analyzer, err := typeswap.New(ctx, typeswap.Options{
		Policy: typeswap.PolicyKeepInternal,
		Pairs:  []typeswap.Pair{rbacPair},
	})
	if err != nil {
		t.Fatalf("new analyzer: %v", err)
	}
	result, err := analyzer.Analyze(ctx, f.graph(rbacRetained...))
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	report, _ := result.Pair(internalRBAC)
	if report.Action != typeswap.ActionKeepInternal {
		t.Errorf("action = %q, want %q", report.Action, typeswap.ActionKeepInternal)
	}
	if len(report.Analyses) != 0 {
		t.Errorf("analyses = %v, want none for a policy that substitutes nothing", report.Analyses)
	}
}

// TestNewRejectsInvalidOptions covers profile level mistakes.
func TestNewRejectsInvalidOptions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	tests := []struct {
		name    string
		opts    typeswap.Options
		problem string
	}{
		{
			name:    "unknown policy",
			opts:    typeswap.Options{Policy: "rewrite-everything"},
			problem: `unsupported value "rewrite-everything"`,
		},
		{
			name: "pair without an external package",
			opts: typeswap.Options{
				Policy: typeswap.PolicyPreferExternal,
				Pairs:  []typeswap.Pair{{Internal: internalRBAC}},
			},
			problem: "needs an external package path",
		},
		{
			name: "pair with itself",
			opts: typeswap.Options{
				Policy: typeswap.PolicyPreferExternal,
				Pairs:  []typeswap.Pair{{Internal: internalRBAC, External: internalRBAC}},
			},
			problem: "is paired with itself",
		},
		{
			name: "duplicate internal package",
			opts: typeswap.Options{
				Policy: typeswap.PolicyPreferExternal,
				Pairs: []typeswap.Pair{
					{Internal: internalRBAC, External: externalRBAC},
					{Internal: internalRBAC, External: "k8s.io/api/rbac/v1beta1"},
				},
			},
			problem: "duplicate internal package",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := typeswap.New(ctx, test.opts)
			if err == nil {
				t.Fatalf("New accepted %s", test.name)
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

// TestAnalyzeRejectsMissingPackage proves an upstream move fails the run rather
// than analyzing nothing and reporting that nothing blocks the substitution.
func TestAnalyzeRejectsMissingPackage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	f := newFixture(t, rbacFiles)
	analyzer, err := typeswap.New(ctx, typeswap.Options{
		Policy: typeswap.PolicyPreferExternal,
		Pairs:  []typeswap.Pair{{Internal: "k8s.io/kubernetes/pkg/apis/moved", External: externalRBAC}},
	})
	if err != nil {
		t.Fatalf("new analyzer: %v", err)
	}
	if _, err := analyzer.Analyze(ctx, f.graph(rbacRetained...)); !errors.Is(err, typeswap.ErrPackageMissing) {
		t.Fatalf("error = %v, want ErrPackageMissing", err)
	}
}
