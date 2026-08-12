package deppolicy_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/deppolicy"
)

// rbacGolden is the checked-in decision for the RBAC shaped fixture.
const rbacGolden = "testdata/rbac/decision.json"

// The fixture below is shaped like the real RBAC extraction rather than
// abbreviated to the shapes under test. That matters because the decision it
// pins is a real one: soapbox.yaml copies no staging package, and the reason is
// that copying k8s.io/apiserver would break authorizer type identity, request
// context keys, feature gate behaviour, and module identity for CVE response,
// in exchange for removing nothing from the consumer's build.
//
// A fixture that only asserted "the answer is no" would keep passing if the
// analysis stopped working. This one asserts which gate refuses which package
// and on what evidence, so an analysis that silently degrades fails here.
var rbacSource = fixtureSource{
	Modules: []string{generatedModule, "k8s.io/apiserver", "k8s.io/component-base"},
	Files: map[string]string{
		"k8s.io/apiserver/pkg/authentication/user/user.go": `package user

// Info describes an authenticated request subject.
type Info interface {
	GetName() string
	GetUID() string
	GetGroups() []string
	GetExtra() map[string][]string
}

// DefaultInfo is the ordinary implementation of Info.
type DefaultInfo struct {
	Name   string
	UID    string
	Groups []string
	Extra  map[string][]string
}

func (i *DefaultInfo) GetName() string                { return i.Name }
func (i *DefaultInfo) GetUID() string                 { return i.UID }
func (i *DefaultInfo) GetGroups() []string            { return i.Groups }
func (i *DefaultInfo) GetExtra() map[string][]string  { return i.Extra }
`,
		"k8s.io/apiserver/pkg/authorization/authorizer/interfaces.go": `package authorizer

import (
	"context"

	"k8s.io/apiserver/pkg/authentication/user"
)

// Decision is the outcome of one authorization check.
type Decision int

const (
	DecisionDeny Decision = iota
	DecisionAllow
	DecisionNoOpinion
)

// Attributes describes the request being authorized.
type Attributes interface {
	GetUser() user.Info
	GetVerb() string
	IsReadOnly() bool
	GetNamespace() string
	GetResource() string
}

// Authorizer decides whether a request is allowed.
type Authorizer interface {
	Authorize(ctx context.Context, a Attributes) (Decision, string, error)
}
`,
		"k8s.io/apiserver/pkg/endpoints/request/requestinfo.go": `package request

import (
	"context"

	"k8s.io/apiserver/pkg/authentication/user"
)

type requestInfoKeyType int

const requestInfoKey requestInfoKeyType = iota

type userKeyType int

const userKey userKeyType = iota

// RequestInfo is the parsed shape of an API request.
type RequestInfo struct {
	Verb      string
	Resource  string
	Namespace string
}

// WithRequestInfo returns a context carrying the parsed request.
func WithRequestInfo(parent context.Context, info *RequestInfo) context.Context {
	return context.WithValue(parent, requestInfoKey, info)
}

// RequestInfoFrom reads the parsed request a context carries.
func RequestInfoFrom(ctx context.Context) (*RequestInfo, bool) {
	info, ok := ctx.Value(requestInfoKey).(*RequestInfo)
	return info, ok
}

// WithUser returns a context carrying the authenticated subject.
func WithUser(parent context.Context, info user.Info) context.Context {
	return context.WithValue(parent, userKey, info)
}

// UserFrom reads the authenticated subject a context carries.
func UserFrom(ctx context.Context) (user.Info, bool) {
	info, ok := ctx.Value(userKey).(user.Info)
	return info, ok
}
`,
		"k8s.io/component-base/featuregate/feature_gate.go": `package featuregate

// Feature names one gated behaviour.
type Feature string

// MutableFeatureGate is the process global gate every component reads.
type MutableFeatureGate interface {
	Enabled(key Feature) bool
	Add(features map[Feature]bool) error
}

type featureGate struct {
	enabled map[Feature]bool
}

func (g *featureGate) Enabled(key Feature) bool { return g.enabled[key] }

func (g *featureGate) Add(features map[Feature]bool) error {
	for name, value := range features {
		g.enabled[name] = value
	}
	return nil
}

// NewFeatureGate returns a gate with no features registered.
func NewFeatureGate() MutableFeatureGate {
	return &featureGate{enabled: map[Feature]bool{}}
}
`,
		"k8s.io/apiserver/pkg/features/kube_features.go": `package features

import (
	"k8s.io/component-base/featuregate"
)

// RBACPolicyRuleCaching gates the rule resolution cache.
const RBACPolicyRuleCaching featuregate.Feature = "RBACPolicyRuleCaching"

// DefaultMutableFeatureGate is the process global gate.
var DefaultMutableFeatureGate = featuregate.NewFeatureGate()

var defaultKubernetesFeatureGates = map[featuregate.Feature]bool{
	RBACPolicyRuleCaching: true,
}

func init() {
	_ = DefaultMutableFeatureGate.Add(defaultKubernetesFeatureGates)
}
`,
		generatedModule + "/authorizer.go": `package rbacauthorizer

import (
	"context"

	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/authorization/authorizer"
)

// PolicyRule is the relocated Kubernetes rule shape.
type PolicyRule struct {
	Verbs     []string
	Resources []string
}

// AuthorizationRuleResolver resolves the rules that apply to a subject.
type AuthorizationRuleResolver interface {
	RulesFor(ctx context.Context, subject user.Info, namespace string) ([]PolicyRule, error)
}

// RBACAuthorizer authorizes requests against RBAC rules.
type RBACAuthorizer struct {
	resolver AuthorizationRuleResolver
}

// New returns an authorizer backed by the resolver.
func New(resolver AuthorizationRuleResolver) *RBACAuthorizer {
	return &RBACAuthorizer{resolver: resolver}
}

// Authorize decides one request.
func (r *RBACAuthorizer) Authorize(ctx context.Context, attrs authorizer.Attributes) (authorizer.Decision, string, error) {
	if r.resolver == nil {
		return authorizer.DecisionNoOpinion, "no resolver configured", nil
	}
	return authorizer.DecisionAllow, "", nil
}
`,
	},
	ModuleFiles: map[string]string{
		"k8s.io/apiserver/LICENSE":      "Apache License Version 2.0\n",
		"k8s.io/apiserver/PATENTS":      "Additional IP Rights Grant\n",
		"k8s.io/component-base/LICENSE": "Apache License Version 2.0\n",
	},
}

// rbacCandidates are the staging packages the refused proposal would copy.
var rbacCandidates = []string{
	"k8s.io/apiserver/pkg/authentication/user",
	"k8s.io/apiserver/pkg/authorization/authorizer",
	"k8s.io/apiserver/pkg/endpoints/request",
	"k8s.io/apiserver/pkg/features",
	"k8s.io/component-base/featuregate",
}

// rbacGraph builds the fixture graph, including the retained apiserver package
// that keeps the module in a real consumer's build.
func rbacGraph(t *testing.T, f *fixture) *deppolicy.Graph {
	t.Helper()

	candidates := make([]deppolicy.Candidate, 0, len(rbacCandidates))
	for _, importPath := range rbacCandidates {
		candidates = append(candidates, f.candidate(t, importPath))
	}

	// A consumer of an RBAC authorizer runs it inside an apiserver, so the
	// delegating authorizer path is in their build whether or not this module
	// copies anything. It is the retained importer that makes the copy a
	// diamond rather than a saving.
	build := append(f.build(), deppolicy.BuildPackage{
		ImportPath: "k8s.io/apiserver/pkg/authorization/authorizerfactory",
		Module:     "k8s.io/apiserver",
		Imports: []string{
			"k8s.io/apiserver/pkg/authentication/user",
			"k8s.io/apiserver/pkg/authorization/authorizer",
		},
		Lines: 120,
	})

	return &deppolicy.Graph{
		Fset:       f.fset,
		Boundary:   []*deppolicy.Package{f.pkg(t, generatedModule)},
		Candidates: candidates,
		Build:      build,
		Modules: []deppolicy.Module{
			{
				Path:             "k8s.io/apiserver",
				Version:          "v0.36.1",
				Dir:              filepath.Join(f.dir, "k8s.io", "apiserver"),
				ZipBytes:         2_830_000,
				ZipBytesKnown:    true,
				ReleasesPerMinor: 9,
				CadenceKnown:     true,
				Licenses: []deppolicy.License{
					{Identifier: "Apache-2.0", Files: []string{"LICENSE"}},
					{Identifier: "Google-Patent-Grant", Files: []string{"PATENTS"}},
				},
				LicensesVerified: true,
			},
			{
				Path:             "k8s.io/component-base",
				Version:          "v0.36.1",
				Dir:              filepath.Join(f.dir, "k8s.io", "component-base"),
				ZipBytes:         1_120_000,
				ZipBytesKnown:    true,
				ReleasesPerMinor: 9,
				CadenceKnown:     true,
				Licenses:         []deppolicy.License{{Identifier: "Apache-2.0", Files: []string{"LICENSE"}}},
				LicensesVerified: true,
			},
		},
	}
}

// rbacOptions is the profile that proposes the copy soapbox.yaml refuses.
//
// It deliberately carries cost overrides. The point of the fixture is that
// relaxing every cost gate an operator is allowed to relax still does not admit
// the copy, because what refuses it is not cost.
func rbacOptions() deppolicy.Options {
	proposals := make([]string, 0, len(rbacCandidates))
	for _, importPath := range rbacCandidates {
		proposals = append(proposals, "staging/src/"+importPath)
	}
	overrides := make([]deppolicy.Override, 0, len(proposals)*2)
	for _, proposal := range proposals {
		for _, gate := range []string{deppolicy.GateCopiedLines, deppolicy.GateCopiedPackages} {
			overrides = append(overrides, deppolicy.Override{
				StagingPath:       proposal,
				Gate:              gate,
				Justification:     "the compiled subset is small, so the copy looks cheap",
				Approver:          "release-approver",
				ExpiresAfterMinor: 40,
			})
		}
	}
	return deppolicy.Options{
		ModulePath:     generatedModule,
		InternalPrefix: "internal/kk",
		SourceMinor:    36,
		Policy:         deppolicy.PolicyCopyApproved,
		Proposals:      proposals,
		Gates: deppolicy.Gates{
			Interoperability: true,
			GlobalState:      true,
			Diamond:          true,
		},
		Overrides:        overrides,
		IdentityRequired: []string{"k8s.io/apiserver/pkg/authorization/authorizer.Authorizer"},
	}
}

// TestRBACProposalIsHardRefused is the fixture the design calls for: a policy
// proposing to copy k8s.io/apiserver must fail, must fail for correctness
// reasons rather than size, and must select no copies.
func TestRBACProposalIsHardRefused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	f := newFixture(t, rbacSource)
	decider, err := deppolicy.New(ctx, rbacOptions())
	if err != nil {
		t.Fatalf("new decider: %v", err)
	}
	result, err := decider.Decide(ctx, rbacGraph(t, f))
	if err != nil {
		t.Fatalf("decide: %v", err)
	}

	if len(result.Copy) != 0 {
		t.Errorf("copy = %v, want no copies", result.Copy)
	}
	if result.Totals.Copied != 0 {
		t.Errorf("copied = %d, want 0", result.Totals.Copied)
	}
	if result.Totals.Refused != len(rbacCandidates) {
		t.Errorf("refused = %d, want %d", result.Totals.Refused, len(rbacCandidates))
	}

	// Every candidate must be refused by a correctness gate specifically. A
	// refusal that rested only on a cost gate would be one an override could
	// remove, and these overrides are present.
	wantGates := map[string][]string{
		"k8s.io/apiserver/pkg/authentication/user":      {deppolicy.GateDiamond, deppolicy.GateInteroperability},
		"k8s.io/apiserver/pkg/authorization/authorizer": {deppolicy.GateDiamond, deppolicy.GateInteroperability},
		"k8s.io/apiserver/pkg/endpoints/request":        {deppolicy.GateGlobalState},
		"k8s.io/apiserver/pkg/features":                 {deppolicy.GateGlobalState},
		"k8s.io/component-base/featuregate":             {deppolicy.GateGlobalState},
	}
	for importPath, want := range wantGates {
		report, ok := result.Candidate(importPath)
		if !ok {
			t.Errorf("result has no candidate %s", importPath)
			continue
		}
		if report.Action != deppolicy.ActionExternal {
			t.Errorf("%s action = %q, want %q", importPath, report.Action, deppolicy.ActionExternal)
		}
		correctness := failedCorrectnessGates(report)
		slices.Sort(want)
		if !equalStrings(correctness, want) {
			t.Errorf("%s failed correctness gates = %v, want %v", importPath, correctness, want)
		}
	}
}

// TestRBACInteropEvidenceNamesTheRealBreakage checks that the refusal explains
// itself in terms of the types that would change identity, because that
// explanation is what an operator reviews and what provenance records.
func TestRBACInteropEvidenceNamesTheRealBreakage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	f := newFixture(t, rbacSource)
	decider, err := deppolicy.New(ctx, rbacOptions())
	if err != nil {
		t.Fatalf("new decider: %v", err)
	}
	result, err := decider.Decide(ctx, rbacGraph(t, f))
	if err != nil {
		t.Fatalf("decide: %v", err)
	}

	report, ok := result.Candidate("k8s.io/apiserver/pkg/authorization/authorizer")
	if !ok {
		t.Fatal("result has no authorizer candidate")
	}
	evidence := strings.Join(gateEvidence(report, deppolicy.GateInteroperability), "\n")

	// Decision is a named basic type crossing the boundary as a result, and
	// Attributes is an interface that cannot be relocated because its method
	// set reaches user.Info. Both are the real reasons, so both must appear.
	for _, want := range []string{
		"k8s.io/apiserver/pkg/authorization/authorizer.Decision",
		"k8s.io/apiserver/pkg/authorization/authorizer.Attributes",
		"k8s.io/apiserver/pkg/authentication/user.Info",
		"RBACAuthorizer",
	} {
		if !strings.Contains(evidence, want) {
			t.Errorf("interoperability evidence does not mention %q:\n%s", want, evidence)
		}
	}

	// user.Info blocks too. The approved design admits no structural exception
	// for interfaces, and the reason is worth stating precisely: a relocated
	// interface is a distinct type, so a consumer holding the real one cannot
	// pass it across the boundary even though it could implement the copy.
	userReport, ok := result.Candidate("k8s.io/apiserver/pkg/authentication/user")
	if !ok {
		t.Fatal("result has no user candidate")
	}
	userEvidence := strings.Join(gateEvidence(userReport, deppolicy.GateInteroperability), "\n")
	if !strings.Contains(userEvidence, "a relocated interface is a distinct type") {
		t.Errorf("user.Info refusal does not explain the nominal identity problem:\n%s", userEvidence)
	}
	if !slices.Contains(userReport.FailedGates(), deppolicy.GateInteroperability) {
		t.Error("user package should fail the interoperability gate")
	}
}

// TestRBACGlobalStateEvidenceNamesTheKeys checks the context key and feature
// gate findings, which are the failures that are silent at run time.
func TestRBACGlobalStateEvidenceNamesTheKeys(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	f := newFixture(t, rbacSource)
	decider, err := deppolicy.New(ctx, rbacOptions())
	if err != nil {
		t.Fatalf("new decider: %v", err)
	}
	result, err := decider.Decide(ctx, rbacGraph(t, f))
	if err != nil {
		t.Fatalf("decide: %v", err)
	}

	tests := []struct {
		importPath string
		want       []string
	}{
		{
			importPath: "k8s.io/apiserver/pkg/endpoints/request",
			want: []string{
				"contextKey: k8s.io/apiserver/pkg/endpoints/request.requestInfoKeyType",
				"contextKey: k8s.io/apiserver/pkg/endpoints/request.userKeyType",
				"contextKey: context.WithValue",
			},
		},
		{
			importPath: "k8s.io/apiserver/pkg/features",
			want: []string{
				"initSideEffect: k8s.io/apiserver/pkg/features.init",
				"mutableSingleton: k8s.io/apiserver/pkg/features.DefaultMutableFeatureGate",
				"featureGate: k8s.io/component-base/featuregate.NewFeatureGate",
			},
		},
		{
			importPath: "k8s.io/component-base/featuregate",
			want:       []string{"deniedPath: k8s.io/component-base/featuregate"},
		},
	}

	for _, test := range tests {
		t.Run(test.importPath, func(t *testing.T) {
			report, ok := result.Candidate(test.importPath)
			if !ok {
				t.Fatalf("result has no candidate %s", test.importPath)
			}
			evidence := strings.Join(gateEvidence(report, deppolicy.GateGlobalState), "\n")
			for _, want := range test.want {
				if !strings.Contains(evidence, want) {
					t.Errorf("global state evidence does not mention %q:\n%s", want, evidence)
				}
			}
		})
	}
}

// TestRBACDecisionGolden pins the whole decision, including every measured
// value, so an upstream change or an analysis change shows up as a reviewable
// diff. Set SOAPBOX_UPDATE_GOLDEN=1 to rewrite it after an intended change.
func TestRBACDecisionGolden(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	f := newFixture(t, rbacSource)
	decider, err := deppolicy.New(ctx, rbacOptions())
	if err != nil {
		t.Fatalf("new decider: %v", err)
	}
	result, err := decider.Decide(ctx, rbacGraph(t, f))
	if err != nil {
		t.Fatalf("decide: %v", err)
	}

	encoded, err := result.JSON()
	if err != nil {
		t.Fatalf("encode result: %v", err)
	}
	// Positions carry the fixture's temporary directory, which changes every
	// run. Normalising it is what makes the rest of the report comparable.
	got := strings.ReplaceAll(string(encoded), f.dir, "$FIXTURE")

	if os.Getenv("SOAPBOX_UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(rbacGolden), 0o750); err != nil {
			t.Fatalf("create testdata directory: %v", err)
		}
		if err := os.WriteFile(rbacGolden, []byte(got), 0o600); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("updated %s", rbacGolden)
		return
	}

	want, err := os.ReadFile(rbacGolden)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if got != string(want) {
		t.Errorf("decision does not match %s\ngot:\n%s", rbacGolden, got)
	}
}

// TestRBACDecisionIsStableAcrossRuns proves the decision depends on the graph
// alone. The replay phase compares generated output across machines, so a
// report that varied with map iteration order would make a rerun look like a
// policy change.
func TestRBACDecisionIsStableAcrossRuns(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	encode := func() string {
		f := newFixture(t, rbacSource)
		decider, err := deppolicy.New(ctx, rbacOptions())
		if err != nil {
			t.Fatalf("new decider: %v", err)
		}
		result, err := decider.Decide(ctx, rbacGraph(t, f))
		if err != nil {
			t.Fatalf("decide: %v", err)
		}
		encoded, err := result.JSON()
		if err != nil {
			t.Fatalf("encode result: %v", err)
		}
		return strings.ReplaceAll(string(encoded), f.dir, "$FIXTURE")
	}

	if first, second := encode(), encode(); first != second {
		t.Errorf("decision is not stable across runs\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

// failedCorrectnessGates returns the correctness gates that refused a
// candidate, sorted.
func failedCorrectnessGates(report deppolicy.CandidateReport) []string {
	var failed []string
	for _, gate := range report.Gates {
		if gate.Kind == deppolicy.KindCorrectness && !gate.Passed {
			failed = append(failed, gate.Name)
		}
	}
	slices.Sort(failed)
	return failed
}

// gateEvidence returns one gate's evidence.
func gateEvidence(report deppolicy.CandidateReport, name string) []string {
	for _, gate := range report.Gates {
		if gate.Name == name {
			return gate.Evidence
		}
	}
	return nil
}
