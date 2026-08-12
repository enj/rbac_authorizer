package deppolicy_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/config"
	"github.com/enj/soapbox/tools/internal/deppolicy"
)

// TestGateVocabularyMatchesProfileSchema pins the two packages' agreement on
// what a gate is called and which gates an override may relax.
//
// deppolicy is deliberately independent of config, so nothing but a test stops
// the two vocabularies from drifting. Drift would be quiet and expensive: a
// profile could name a gate this package never evaluates, or an override could
// pass config validation and then relax nothing at all.
func TestGateVocabularyMatchesProfileSchema(t *testing.T) {
	t.Parallel()

	// Both sides are read from their real exported vocabularies. Comparing
	// against copied literals here would keep passing while the two packages
	// drifted apart, which is the only failure this test exists to catch.
	for _, name := range config.CorrectnessGateNames() {
		if !slices.Contains(deppolicy.CorrectnessGates(), name) {
			t.Errorf("profile correctness gate %q is not a deppolicy correctness gate", name)
		}
	}
	for _, name := range config.CostGateNames() {
		if !slices.Contains(deppolicy.CostGates(), name) {
			t.Errorf("profile cost gate %q is not a deppolicy cost gate", name)
		}
		// The dangerous direction: a gate an operator may override in a
		// profile must never be a gate this package treats as correctness,
		// because config validation would accept the override and the gate
		// would refuse anyway with no explanation.
		if slices.Contains(deppolicy.CorrectnessGates(), name) {
			t.Errorf("profile cost gate %q is a deppolicy correctness gate, so an accepted override would silently do nothing", name)
		}
	}
	// Closure completeness is correctness here and must not be offered as an
	// overridable cost gate anywhere.
	if slices.Contains(config.CostGateNames(), deppolicy.GateClosureComplete) {
		t.Errorf("%q is a correctness gate but the profile schema offers it as overridable", deppolicy.GateClosureComplete)
	}

	// The policy values have to agree too, since a caller translates one into
	// the other at the boundary.
	if config.DependencyPolicyExternal != deppolicy.PolicyExternal {
		t.Errorf("external policy value drifted: %q vs %q", config.DependencyPolicyExternal, deppolicy.PolicyExternal)
	}
	if config.DependencyPolicyCopyApproved != deppolicy.PolicyCopyApproved {
		t.Errorf("copy policy value drifted: %q vs %q", config.DependencyPolicyCopyApproved, deppolicy.PolicyCopyApproved)
	}
}

// TestDecideRejectsStaleOverrides covers the two ways an override stops being a
// promise about the code actually being judged.
func TestDecideRejectsStaleOverrides(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	const proposal = "staging/src/k8s.io/utilpkg/text"
	base := deppolicy.Options{
		ModulePath:     generatedModule,
		InternalPrefix: "internal/kk",
		SourceMinor:    36,
		Policy:         deppolicy.PolicyCopyApproved,
		Proposals:      []string{proposal},
		Gates:          deppolicy.Gates{Interoperability: true, GlobalState: true, Diamond: true},
	}

	tests := []struct {
		name     string
		override deppolicy.Override
		want     error
		mentions string
	}{
		{
			name: "expired one minor before the source",
			override: deppolicy.Override{
				StagingPath: proposal, Gate: deppolicy.GateCopiedLines,
				Justification: "small", Approver: "release-approver", ExpiresAfterMinor: 35,
			},
			want:     deppolicy.ErrOverrideExpired,
			mentions: "was good through v1.35, source is v1.36",
		},
		{
			name: "expired several minors before the source",
			override: deppolicy.Override{
				StagingPath: proposal, Gate: deppolicy.GateCopiedLines,
				Justification: "small", Approver: "release-approver", ExpiresAfterMinor: 30,
			},
			want:     deppolicy.ErrOverrideExpired,
			mentions: "release-approver",
		},
		{
			name: "applies to a package the graph does not contain",
			override: deppolicy.Override{
				StagingPath: "staging/src/k8s.io/utilpkg/gone", Gate: deppolicy.GateCopiedLines,
				Justification: "small", Approver: "release-approver", ExpiresAfterMinor: 40,
			},
			want:     deppolicy.ErrOverrideUnused,
			mentions: "staging/src/k8s.io/utilpkg/gone",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, pureLeafSource)

			opts := base
			opts.Overrides = []deppolicy.Override{test.override}
			decider, err := deppolicy.New(ctx, opts)
			if err != nil {
				t.Fatalf("new decider: %v", err)
			}

			_, err = decider.Decide(ctx, &deppolicy.Graph{
				Fset:       f.fset,
				Boundary:   []*deppolicy.Package{f.pkg(t, generatedModule)},
				Candidates: []deppolicy.Candidate{f.candidate(t, "k8s.io/utilpkg/text")},
				Build:      f.build(),
			})
			if err == nil {
				t.Fatal("Decide accepted a stale override")
			}
			if !errors.Is(err, test.want) {
				t.Errorf("error = %v, want %v", err, test.want)
			}
			if !strings.Contains(err.Error(), test.mentions) {
				t.Errorf("error %q does not mention %q", err, test.mentions)
			}
		})
	}
}

// TestOverrideIsGoodThroughItsNamedMinor pins the expiry boundary.
//
// An override written for v1.38 has to still apply while transforming v1.38.
// Expiring it at equality would silently shorten every promise by one release,
// and the failure would look like a policy change nobody made.
func TestOverrideIsGoodThroughItsNamedMinor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	const proposal = "staging/src/k8s.io/utilpkg/text"
	f := newFixture(t, pureLeafSource)
	decider, err := deppolicy.New(ctx, deppolicy.Options{
		ModulePath:     generatedModule,
		InternalPrefix: "internal/kk",
		SourceMinor:    36,
		Policy:         deppolicy.PolicyCopyApproved,
		Proposals:      []string{proposal},
		Gates:          deppolicy.Gates{Interoperability: true, GlobalState: true, Diamond: true},
		Overrides: []deppolicy.Override{{
			StagingPath: proposal, Gate: deppolicy.GateCopiedLines,
			Justification: "a five line helper", Approver: "release-approver",
			// Exactly the minor being transformed.
			ExpiresAfterMinor: 36,
		}},
	})
	if err != nil {
		t.Fatalf("new decider: %v", err)
	}
	if _, err := decider.Decide(ctx, &deppolicy.Graph{
		Fset:       f.fset,
		Boundary:   []*deppolicy.Package{f.pkg(t, generatedModule)},
		Candidates: []deppolicy.Candidate{f.candidate(t, "k8s.io/utilpkg/text")},
		Build:      f.build(),
	}); err != nil {
		t.Fatalf("an override good through the source minor was rejected: %v", err)
	}
}

// TestDecideRejectsUnknownProposal proves an upstream rename fails the run
// rather than quietly reducing an approved copy to no copy.
func TestDecideRejectsUnknownProposal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	f := newFixture(t, pureLeafSource)
	decider, err := deppolicy.New(ctx, deppolicy.Options{
		ModulePath:     generatedModule,
		InternalPrefix: "internal/kk",
		SourceMinor:    36,
		Policy:         deppolicy.PolicyCopyApproved,
		Proposals:      []string{"staging/src/k8s.io/utilpkg/renamed"},
		Gates:          deppolicy.Gates{Interoperability: true, GlobalState: true, Diamond: true},
	})
	if err != nil {
		t.Fatalf("new decider: %v", err)
	}

	_, err = decider.Decide(ctx, &deppolicy.Graph{
		Fset:       f.fset,
		Boundary:   []*deppolicy.Package{f.pkg(t, generatedModule)},
		Candidates: []deppolicy.Candidate{f.candidate(t, "k8s.io/utilpkg/text")},
		Build:      f.build(),
	})
	if !errors.Is(err, deppolicy.ErrProposalUnknown) {
		t.Fatalf("error = %v, want ErrProposalUnknown", err)
	}
}

// TestCostGatesMeasureRealFiles checks the measurements that are read off disk
// rather than derived from the type graph, because those are the ones a fixture
// could silently stop exercising.
func TestCostGatesMeasureRealFiles(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	source := fixtureSource{
		Modules: []string{generatedModule, "k8s.io/utilpkg"},
		Files: map[string]string{
			// A generated file, an incomplete closure, and a sibling that is
			// not proposed, all in one candidate.
			"k8s.io/utilpkg/codec/zz_generated.deepcopy.go": `// Code generated by deepcopy-gen. DO NOT EDIT.

package codec

// DeepCopy returns a copy.
func DeepCopy(in []string) []string {
	out := make([]string, len(in))
	copy(out, in)
	return out
}
`,
			"k8s.io/utilpkg/codec/codec.go": `package codec

import "k8s.io/utilpkg/text"

// Encode normalizes and copies.
func Encode(in []string) []string {
	for i, v := range in {
		in[i] = text.Normalize(v)
	}
	return DeepCopy(in)
}
`,
			"k8s.io/utilpkg/text/text.go": `package text

// Normalize trims a verb.
func Normalize(in string) string { return in }
`,
			generatedModule + "/facade.go": `package rbacauthorizer

import "k8s.io/utilpkg/codec"

// Encode is forwarded from the relocated helper.
func Encode(in []string) []string { return codec.Encode(in) }
`,
		},
		Extra: map[string]string{
			"k8s.io/utilpkg/codec/fast_arm64.s": "// assembly\n",
		},
		ModuleFiles: map[string]string{
			"k8s.io/utilpkg/LICENSE": "Apache License Version 2.0\n",
			"k8s.io/utilpkg/PATENTS": "Additional IP Rights Grant\n",
		},
	}

	f := newFixture(t, source)
	decider, err := deppolicy.New(ctx, deppolicy.Options{
		ModulePath:     generatedModule,
		InternalPrefix: "internal/kk",
		SourceMinor:    36,
		Policy:         deppolicy.PolicyCopyApproved,
		Proposals:      []string{"staging/src/k8s.io/utilpkg/codec", "staging/src/k8s.io/utilpkg/text"},
		Gates: deppolicy.Gates{
			Interoperability: true, GlobalState: true, Diamond: true,
			Cost: deppolicy.CostCeilings{
				MaxCopiedPackages: 2, MaxCopiedLines: 500,
				MaxGeneratedFiles: 0, MaxDistinctLicenses: 2, MaxModuleZipBytes: 1 << 20,
				MaxReleasesPerMinor: 4,
			},
		},
	})
	if err != nil {
		t.Fatalf("new decider: %v", err)
	}

	result, err := decider.Decide(ctx, &deppolicy.Graph{
		Fset:     f.fset,
		Boundary: []*deppolicy.Package{f.pkg(t, generatedModule)},
		Candidates: []deppolicy.Candidate{
			f.candidate(t, "k8s.io/utilpkg/codec"),
			f.candidate(t, "k8s.io/utilpkg/text"),
		},
		Build: f.build(),
		Modules: []deppolicy.Module{{
			Path: "k8s.io/utilpkg", Version: "v0.36.1", Dir: f.dir + "/k8s.io/utilpkg",
			ZipBytes: 8192, ZipBytesKnown: true,
			ReleasesPerMinor: 3, CadenceKnown: true,
			Licenses: []deppolicy.License{
				{Identifier: "Apache-2.0", Files: []string{"LICENSE"}},
				{Identifier: "Google-Patent-Grant", Files: []string{"PATENTS"}},
			},
			LicensesVerified: true,
		}},
	})
	if err != nil {
		t.Fatalf("decide: %v", err)
	}

	report, ok := result.Candidate("k8s.io/utilpkg/codec")
	if !ok {
		t.Fatal("result has no codec candidate")
	}

	if report.Score.GeneratedFiles != 1 {
		t.Errorf("generated files = %d, want 1", report.Score.GeneratedFiles)
	}
	if report.Score.NativeFiles != 1 {
		t.Errorf("native files = %d, want 1", report.Score.NativeFiles)
	}
	// The sibling is proposed too, so the closure is complete and the aggregate
	// cost gates are the ones left to refuse it.
	if len(report.Score.ClosureGaps) != 0 {
		t.Errorf("closure gaps = %v, want none once the sibling is proposed", report.Score.ClosureGaps)
	}
	if !equalStrings(report.Score.LicenseIdentifiers, []string{"Apache-2.0", "Google-Patent-Grant"}) {
		t.Errorf("license identifiers = %v, want [Apache-2.0 Google-Patent-Grant]", report.Score.LicenseIdentifiers)
	}
	// The two Go files really are counted, so the measurement is not a
	// placeholder that happens to be non zero.
	if report.Score.CopiedLines < 15 {
		t.Errorf("copied lines = %d, want the real line count of two files", report.Score.CopiedLines)
	}

	wantFailed := []string{
		deppolicy.GateGeneratedFiles,
		deppolicy.GateNativeCode,
	}
	if got := report.FailedGates(); !equalStrings(got, wantFailed) {
		t.Errorf("failed gates = %v, want %v", got, wantFailed)
	}
	if report.Action != deppolicy.ActionExternal {
		t.Errorf("action = %q, want %q", report.Action, deppolicy.ActionExternal)
	}
}

// TestCostOverrideAdmitsCostButNeverCorrectness is the property the whole
// override design rests on.
func TestCostOverrideAdmitsCostButNeverCorrectness(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	f := newFixture(t, pureLeafSource)
	options := func(overrides []deppolicy.Override) deppolicy.Options {
		return deppolicy.Options{
			ModulePath:     generatedModule,
			InternalPrefix: "internal/kk",
			SourceMinor:    36,
			Policy:         deppolicy.PolicyCopyApproved,
			Proposals:      []string{"staging/src/k8s.io/utilpkg/text"},
			Gates:          deppolicy.Gates{Interoperability: true, GlobalState: true, Diamond: true},
			Overrides:      overrides,
		}
	}
	graph := func() *deppolicy.Graph {
		return &deppolicy.Graph{
			Fset:       f.fset,
			Boundary:   []*deppolicy.Package{f.pkg(t, generatedModule)},
			Candidates: []deppolicy.Candidate{f.candidate(t, "k8s.io/utilpkg/text")},
			Build:      f.build(),
			Modules: []deppolicy.Module{{
				Path: "k8s.io/utilpkg", Dir: f.dir + "/k8s.io/utilpkg",
				ZipBytesKnown: true, CadenceKnown: true,
				Licenses:         []deppolicy.License{{Identifier: "Apache-2.0", Files: []string{"LICENSE"}}},
				LicensesVerified: true,
			}},
		}
	}

	// With every ceiling at zero the candidate is refused on cost alone.
	decider, err := deppolicy.New(ctx, options(nil))
	if err != nil {
		t.Fatalf("new decider: %v", err)
	}
	result, err := decider.Decide(ctx, graph())
	if err != nil {
		t.Fatalf("decide without overrides: %v", err)
	}
	report, _ := result.Candidate("k8s.io/utilpkg/text")
	failed := report.FailedGates()
	if len(failed) == 0 {
		t.Fatal("zero ceilings admitted a copy")
	}
	for _, name := range failed {
		if slices.Contains(deppolicy.CorrectnessGates(), name) {
			t.Fatalf("pure leaf utility failed correctness gate %q", name)
		}
	}

	// Overriding exactly the failed cost gates admits it, which proves an
	// override does relax what it names.
	overrides := make([]deppolicy.Override, 0, len(failed))
	for _, name := range failed {
		overrides = append(overrides, deppolicy.Override{
			StagingPath: "staging/src/k8s.io/utilpkg/text", Gate: name,
			Justification: "a five line helper with no state", Approver: "release-approver", ExpiresAfterMinor: 40,
		})
	}
	decider, err = deppolicy.New(ctx, options(overrides))
	if err != nil {
		t.Fatalf("new decider with overrides: %v", err)
	}
	result, err = decider.Decide(ctx, graph())
	if err != nil {
		t.Fatalf("decide with overrides: %v", err)
	}
	report, _ = result.Candidate("k8s.io/utilpkg/text")
	if got := report.FailedGates(); len(got) != 0 {
		t.Errorf("overridden cost gates still refuse: %v", got)
	}
	if report.Action != deppolicy.ActionCopy {
		t.Errorf("action = %q, want %q", report.Action, deppolicy.ActionCopy)
	}
}
