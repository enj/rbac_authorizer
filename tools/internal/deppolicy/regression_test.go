package deppolicy_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/deppolicy"
)

// The cases here are the ones an earlier version of this package got wrong.
// Each one passed a gate it should have refused, or refused for a reason that
// was not the real one, and every failure was silent: the module compiled, the
// tests passed, and the wrong decision shipped under an immutable tag.

// sealedInterfaceSource has a candidate whose interface carries an unexported
// method, which seals it to its declaring package.
var sealedInterfaceSource = fixtureSource{
	Modules: []string{generatedModule, "k8s.io/sealed"},
	Files: map[string]string{
		"k8s.io/sealed/token/token.go": `package token

// Verifier checks a token. The unexported method seals the interface: only
// this package can implement it.
type Verifier interface {
	Verify(raw string) bool
	private()
}
`,
		generatedModule + "/facade.go": `package rbacauthorizer

import "k8s.io/sealed/token"

// Verify uses the sealed interface across the public boundary.
func Verify(v token.Verifier, raw string) bool { return v.Verify(raw) }
`,
	},
}

// TestSealedInterfaceCannotBeRelocated covers the one exception the
// interoperability gate makes, and its limit.
//
// An interface is normally relocatable because it is satisfied structurally.
// That stops being true the moment it has an unexported method: the method name
// is qualified by its declaring package, so nothing outside that package can
// implement either the original or the copy. Permitting it would have produced
// a generated module carrying an interface no consumer could ever satisfy.
func TestSealedInterfaceCannotBeRelocated(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	f := newFixture(t, sealedInterfaceSource)
	decider, err := deppolicy.New(ctx, deppolicy.Options{
		ModulePath:     generatedModule,
		InternalPrefix: "internal/kk",
		SourceMinor:    36,
		Policy:         deppolicy.PolicyCopyApproved,
		Proposals:      []string{"staging/src/k8s.io/sealed/token"},
		Gates: deppolicy.Gates{
			Interoperability: true, GlobalState: true, Diamond: true,
			Cost: deppolicy.CostCeilings{MaxCopiedPackages: 1, MaxCopiedLines: 500, MaxDistinctLicenses: 1, MaxModuleZipBytes: 1 << 20},
		},
	})
	if err != nil {
		t.Fatalf("new decider: %v", err)
	}

	result, err := decider.Decide(ctx, &deppolicy.Graph{
		Fset:       f.fset,
		Boundary:   []*deppolicy.Package{f.pkg(t, generatedModule)},
		Candidates: []deppolicy.Candidate{f.candidate(t, "k8s.io/sealed/token")},
		Build:      f.build(),
		Modules: []deppolicy.Module{{
			Path: "k8s.io/sealed", ZipBytesKnown: true, CadenceKnown: true,
			Licenses:         []deppolicy.License{{Identifier: "Apache-2.0", Files: []string{"LICENSE"}}},
			LicensesVerified: true,
		}},
	})
	if err != nil {
		t.Fatalf("decide: %v", err)
	}

	report, ok := result.Candidate("k8s.io/sealed/token")
	if !ok {
		t.Fatal("result has no token candidate")
	}
	evidence := strings.Join(gateEvidence(report, deppolicy.GateInteroperability), "\n")
	if !strings.Contains(evidence, "seals it to its declaring package") {
		t.Errorf("interoperability evidence does not explain the seal:\n%s", evidence)
	}
	if !containsString(report.FailedGates(), deppolicy.GateInteroperability) {
		t.Errorf("sealed interface passed the interoperability gate; failed gates were %v", report.FailedGates())
	}
}

// exportedBasicVarSource has a candidate whose only global is an exported
// string.
var exportedBasicVarSource = fixtureSource{
	Modules: []string{generatedModule, "k8s.io/basicvar"},
	Files: map[string]string{
		"k8s.io/basicvar/config/config.go": `package config

// DefaultEndpoint is an exported string. It cannot be mutated in place, and any
// importer can still rebind it.
var DefaultEndpoint = "https://example.invalid"

// ErrNotFound is a sentinel, which is comparison rather than state.
var ErrNotFound = newError("not found")

func newError(msg string) error { return &configError{msg: msg} }

type configError struct{ msg string }

func (e *configError) Error() string { return e.msg }

// Endpoint returns the configured endpoint.
func Endpoint() string { return DefaultEndpoint }
`,
		generatedModule + "/facade.go": `package rbacauthorizer

import "k8s.io/basicvar/config"

// Endpoint is forwarded from the relocated helper.
func Endpoint() string { return config.Endpoint() }
`,
	},
}

// TestExportedBasicVarIsGlobalState covers a distinction the scan used to draw
// and should not have.
//
// The old rule flagged maps, slices, and pointers and let basic types through,
// on the theory that a string cannot be mutated. That is true and irrelevant:
// any importer can rebind an exported variable whatever its type, so a
// relocated copy gives the generated module a second one that the consumer's
// assignment never reaches. Sentinel errors remain the one documented
// exception.
func TestExportedBasicVarIsGlobalState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	f := newFixture(t, exportedBasicVarSource)
	decider, err := deppolicy.New(ctx, deppolicy.Options{
		ModulePath:     generatedModule,
		InternalPrefix: "internal/kk",
		SourceMinor:    36,
		Policy:         deppolicy.PolicyCopyApproved,
		Proposals:      []string{"staging/src/k8s.io/basicvar/config"},
		Gates:          deppolicy.Gates{Interoperability: true, GlobalState: true, Diamond: true},
	})
	if err != nil {
		t.Fatalf("new decider: %v", err)
	}

	result, err := decider.Decide(ctx, &deppolicy.Graph{
		Fset:       f.fset,
		Boundary:   []*deppolicy.Package{f.pkg(t, generatedModule)},
		Candidates: []deppolicy.Candidate{f.candidate(t, "k8s.io/basicvar/config")},
		Build:      f.build(),
	})
	if err != nil {
		t.Fatalf("decide: %v", err)
	}

	report, _ := result.Candidate("k8s.io/basicvar/config")
	evidence := strings.Join(gateEvidence(report, deppolicy.GateGlobalState), "\n")
	if !strings.Contains(evidence, "DefaultEndpoint") {
		t.Errorf("global state evidence does not flag the exported string:\n%s", evidence)
	}
	if !strings.Contains(evidence, "rebindable by any importer") {
		t.Errorf("global state evidence does not explain rebinding:\n%s", evidence)
	}
	// The sentinel stays excluded, or every package's ErrNotFound would bury
	// the findings that matter.
	if strings.Contains(evidence, "ErrNotFound") {
		t.Errorf("sentinel error was reported as global state:\n%s", evidence)
	}
}

// twoPackageSource is a complete two package closure with no generated or
// native files, so the aggregate ceiling is the only thing that can refuse it.
var twoPackageSource = fixtureSource{
	Modules: []string{generatedModule, "k8s.io/utilpkg"},
	Files: map[string]string{
		"k8s.io/utilpkg/text/text.go": `package text

// Normalize trims a verb.
func Normalize(in string) string {
	if in == "" {
		return "*"
	}
	return in
}
`,
		"k8s.io/utilpkg/codec/codec.go": `package codec

import "k8s.io/utilpkg/text"

// Encode normalizes every entry.
func Encode(in []string) []string {
	out := make([]string, 0, len(in))
	for _, value := range in {
		out = append(out, text.Normalize(value))
	}
	return out
}
`,
		generatedModule + "/facade.go": `package rbacauthorizer

import "k8s.io/utilpkg/codec"

// Encode is forwarded from the relocated helper.
func Encode(in []string) []string { return codec.Encode(in) }
`,
	},
	ModuleFiles: map[string]string{
		"k8s.io/utilpkg/LICENSE": "Apache License Version 2.0\n",
	},
}

// TestUnmeasuredFactsRefuseTheirGates proves an absent measurement is never
// read as a cheap one.
//
// Scoring an unsupplied zip size or an uninspected licence as zero is the one
// arithmetic that turns missing evidence into approval. An override cannot
// rescue such a gate either, because an override weighs a known cost against a
// justification and there is no known cost to weigh.
func TestUnmeasuredFactsRefuseTheirGates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	const proposal = "staging/src/k8s.io/utilpkg/text"
	tests := []struct {
		name      string
		modules   []deppolicy.Module
		overrides []deppolicy.Override
		wantGates []string
	}{
		{
			name:      "no module facts at all",
			modules:   nil,
			wantGates: []string{deppolicy.GateDistinctLicenses, deppolicy.GateModuleZipBytes},
		},
		{
			name: "licences never inspected",
			modules: []deppolicy.Module{{
				Path: "k8s.io/utilpkg", ZipBytesKnown: true, CadenceKnown: true,
			}},
			wantGates: []string{deppolicy.GateDistinctLicenses},
		},
		{
			name: "an override cannot rescue an unmeasured gate",
			modules: []deppolicy.Module{{
				Path: "k8s.io/utilpkg", ZipBytesKnown: true, CadenceKnown: true,
			}},
			overrides: []deppolicy.Override{{
				StagingPath: proposal, Gate: deppolicy.GateDistinctLicenses,
				Justification: "we will look at the licences later",
				Approver:      "release-approver", ExpiresAfterMinor: 40,
			}},
			wantGates: []string{deppolicy.GateDistinctLicenses},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, pureLeafSource)

			decider, err := deppolicy.New(ctx, deppolicy.Options{
				ModulePath:     generatedModule,
				InternalPrefix: "internal/kk",
				SourceMinor:    36,
				Policy:         deppolicy.PolicyCopyApproved,
				Proposals:      []string{proposal},
				Gates: deppolicy.Gates{
					Interoperability: true, GlobalState: true, Diamond: true,
					Cost: deppolicy.CostCeilings{
						MaxCopiedPackages: 1, MaxCopiedLines: 500,
						MaxDistinctLicenses: 4, MaxModuleZipBytes: 1 << 30,
					},
				},
				Overrides: test.overrides,
			})
			if err != nil {
				t.Fatalf("new decider: %v", err)
			}
			result, err := decider.Decide(ctx, &deppolicy.Graph{
				Fset:       f.fset,
				Boundary:   []*deppolicy.Package{f.pkg(t, generatedModule)},
				Candidates: []deppolicy.Candidate{f.candidate(t, "k8s.io/utilpkg/text")},
				Build:      f.build(),
				Modules:    test.modules,
			})
			if err != nil {
				t.Fatalf("decide: %v", err)
			}

			report, _ := result.Candidate("k8s.io/utilpkg/text")
			for _, want := range test.wantGates {
				if !containsString(report.FailedGates(), want) {
					t.Errorf("gate %q passed on an unmeasured fact; failed gates were %v", want, report.FailedGates())
				}
			}
			if report.Action != deppolicy.ActionExternal {
				t.Errorf("action = %q, want %q", report.Action, deppolicy.ActionExternal)
			}
		})
	}
}

// TestGraphValidationFailsClosed proves an analysis that cannot run is refused
// rather than passed.
//
// Each of these inputs would previously have produced a clean report: a
// candidate with no syntax was scanned for global state and found to have none,
// and an empty build graph gave the diamond gate no retained package to find.
// Both are the absence of evidence being read as evidence of absence, on the
// two gates whose whole job is to refuse.
func TestGraphValidationFailsClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	decider, err := deppolicy.New(ctx, deppolicy.Options{
		ModulePath:     generatedModule,
		InternalPrefix: "internal/kk",
		SourceMinor:    36,
		Policy:         deppolicy.PolicyCopyApproved,
		Proposals:      []string{"staging/src/k8s.io/utilpkg/text"},
		Gates:          deppolicy.Gates{Interoperability: true, GlobalState: true, Diamond: true},
	})
	if err != nil {
		t.Fatalf("new decider: %v", err)
	}

	tests := []struct {
		name    string
		mutate  func(*deppolicy.Graph)
		problem string
	}{
		{
			name:    "candidate carries no syntax",
			mutate:  func(g *deppolicy.Graph) { g.Candidates[0].Package.Syntax = nil },
			problem: "carries no syntax",
		},
		{
			name:    "candidate carries no type information",
			mutate:  func(g *deppolicy.Graph) { g.Candidates[0].Package.Info = nil },
			problem: "carries no type information",
		},
		{
			name:    "resolved build is empty",
			mutate:  func(g *deppolicy.Graph) { g.Build = nil },
			problem: "the diamond gate needs the resolved consumer build",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, pureLeafSource)
			graph := &deppolicy.Graph{
				Fset:       f.fset,
				Boundary:   []*deppolicy.Package{f.pkg(t, generatedModule)},
				Candidates: []deppolicy.Candidate{f.candidate(t, "k8s.io/utilpkg/text")},
				Build:      f.build(),
			}
			test.mutate(graph)

			_, err := decider.Decide(ctx, graph)
			if err == nil {
				t.Fatalf("Decide accepted a graph that %s", test.name)
			}
			if !errors.Is(err, deppolicy.ErrInvalidOptions) {
				t.Errorf("error does not classify as ErrInvalidOptions: %v", err)
			}
			if !strings.Contains(err.Error(), test.problem) {
				t.Errorf("error %q does not mention %q", err, test.problem)
			}
		})
	}
}

// TestIdentityRequirementMustResolve proves a misspelled requirement fails
// rather than silently matching nothing.
//
// These entries are what pin an upstream package in place for the diamond gate.
// A typo would make the requirement match no candidate, remove the diamond
// finding it exists to produce, and approve a copy precisely because the
// evidence against it was misspelled.
func TestIdentityRequirementMustResolve(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	tests := []struct {
		name     string
		required string
		want     error
	}{
		{
			name:     "package that is not in the graph",
			required: "k8s.io/apiserver/pkg/authorization/authorizr.Authorizer",
			want:     deppolicy.ErrIdentityUnknown,
		},
		{
			name:     "not a qualified type name",
			required: "Authorizer",
			want:     deppolicy.ErrIdentityMalformed,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, pureLeafSource)

			opts := deppolicy.Options{
				ModulePath:     generatedModule,
				InternalPrefix: "internal/kk",
				SourceMinor:    36,
				Policy:         deppolicy.PolicyCopyApproved,
				Proposals:      []string{"staging/src/k8s.io/utilpkg/text"},
				Gates:          deppolicy.Gates{Interoperability: true, GlobalState: true, Diamond: true},
			}
			opts.IdentityRequired = []string{test.required}

			decider, err := deppolicy.New(ctx, opts)
			if err != nil {
				// A malformed name is caught during validation, which is also
				// a refusal and is what this case is asserting.
				if errors.Is(err, deppolicy.ErrInvalidOptions) && errors.Is(test.want, deppolicy.ErrIdentityMalformed) {
					return
				}
				t.Fatalf("new decider: %v", err)
			}
			_, err = decider.Decide(ctx, &deppolicy.Graph{
				Fset:       f.fset,
				Boundary:   []*deppolicy.Package{f.pkg(t, generatedModule)},
				Candidates: []deppolicy.Candidate{f.candidate(t, "k8s.io/utilpkg/text")},
				Build:      f.build(),
			})
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}

// TestSourceMinorRequiredWithOverrides proves an unset source minor cannot make
// every override valid forever.
func TestSourceMinorRequiredWithOverrides(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	_, err := deppolicy.New(ctx, deppolicy.Options{
		ModulePath:     generatedModule,
		InternalPrefix: "internal/kk",
		Policy:         deppolicy.PolicyCopyApproved,
		Proposals:      []string{"staging/src/k8s.io/utilpkg/text"},
		Gates:          deppolicy.Gates{Interoperability: true, GlobalState: true, Diamond: true},
		Overrides: []deppolicy.Override{{
			StagingPath: "staging/src/k8s.io/utilpkg/text", Gate: deppolicy.GateCopiedLines,
			Justification: "small", Approver: "release-approver", ExpiresAfterMinor: 40,
		}},
	})
	if err == nil {
		t.Fatal("New accepted overrides with no source minor")
	}
	if !strings.Contains(err.Error(), "expires relative to the source minor") {
		t.Errorf("error %q does not explain why the source minor is required", err)
	}
}

// TestDecideReturnsCancellation proves a cancelled scan is reported as
// cancelled rather than as a clean gate.
func TestDecideReturnsCancellation(t *testing.T) {
	t.Parallel()

	f := newFixture(t, pureLeafSource)
	decider, err := deppolicy.New(context.Background(), deppolicy.Options{
		ModulePath:     generatedModule,
		InternalPrefix: "internal/kk",
		SourceMinor:    36,
		Policy:         deppolicy.PolicyCopyApproved,
		Proposals:      []string{"staging/src/k8s.io/utilpkg/text"},
		Gates:          deppolicy.Gates{Interoperability: true, GlobalState: true, Diamond: true},
	})
	if err != nil {
		t.Fatalf("new decider: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result, err := decider.Decide(ctx, &deppolicy.Graph{
		Fset:       f.fset,
		Boundary:   []*deppolicy.Package{f.pkg(t, generatedModule)},
		Candidates: []deppolicy.Candidate{f.candidate(t, "k8s.io/utilpkg/text")},
		Build:      f.build(),
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if result != nil {
		t.Errorf("a cancelled decision returned a result: %+v", result)
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

// TestCorrectnessGatesAreNotSwitches pins what the gate flags mean.
//
// They are assertions that the gates run, not switches that turn them off. A
// profile proposing a copy with one set false is rejected during validation,
// and a profile that copies nothing still has every gate evaluated, so the
// report explains a candidate whether or not the profile claimed to enable
// anything. Reading them as switches would make disabling a gate the cheapest
// way to pass it.
func TestCorrectnessGatesAreNotSwitches(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	f := newFixture(t, rbacSource)
	// An external profile enables nothing, which is the default zero value.
	decider, err := deppolicy.New(ctx, deppolicy.Options{
		ModulePath:       generatedModule,
		InternalPrefix:   "internal/kk",
		SourceMinor:      36,
		Policy:           deppolicy.PolicyExternal,
		Gates:            deppolicy.Gates{},
		IdentityRequired: []string{"k8s.io/apiserver/pkg/authorization/authorizer.Authorizer"},
	})
	if err != nil {
		t.Fatalf("new decider: %v", err)
	}

	result, err := decider.Decide(ctx, rbacGraph(t, f))
	if err != nil {
		t.Fatalf("decide: %v", err)
	}

	// Every correctness gate still ran and still refused what it should.
	report, ok := result.Candidate("k8s.io/apiserver/pkg/authorization/authorizer")
	if !ok {
		t.Fatal("result has no authorizer candidate")
	}
	for _, want := range []string{deppolicy.GateInteroperability, deppolicy.GateDiamond} {
		if !containsString(report.FailedGates(), want) {
			t.Errorf("gate %q did not run with its flag unset; failed gates were %v", want, report.FailedGates())
		}
	}
}

// TestAggregateCeilingsCountTheWholeCopy proves a ceiling describes the
// generated module rather than each package in it.
//
// Scoring every candidate against the ceiling separately admits five packages
// of two hundred lines under a ceiling of a thousand and also under a ceiling
// of two hundred, which makes the number meaningless. The measurement is taken
// over the accepted set as one thing.
func TestAggregateCeilingsCountTheWholeCopy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	proposals := []string{"staging/src/k8s.io/utilpkg/codec", "staging/src/k8s.io/utilpkg/text"}
	decide := func(t *testing.T, maxLines int) deppolicy.CandidateReport {
		t.Helper()
		f := newFixture(t, twoPackageSource)
		decider, err := deppolicy.New(ctx, deppolicy.Options{
			ModulePath:     generatedModule,
			InternalPrefix: "internal/kk",
			SourceMinor:    36,
			Policy:         deppolicy.PolicyCopyApproved,
			Proposals:      proposals,
			Gates: deppolicy.Gates{
				Interoperability: true, GlobalState: true, Diamond: true,
				Cost: deppolicy.CostCeilings{
					MaxCopiedPackages: 2, MaxCopiedLines: maxLines,
					MaxGeneratedFiles: 0, MaxDistinctLicenses: 1,
					MaxModuleZipBytes: 1 << 20, MaxReleasesPerMinor: 4,
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
				Path: "k8s.io/utilpkg", Dir: f.dir + "/k8s.io/utilpkg",
				ZipBytes: 4096, ZipBytesKnown: true,
				ReleasesPerMinor: 2, CadenceKnown: true,
				Licenses:         []deppolicy.License{{Identifier: "Apache-2.0", Files: []string{"LICENSE"}}},
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
		return report
	}

	// A generous ceiling admits both packages.
	if got := decide(t, 500).FailedGates(); len(got) != 0 {
		t.Fatalf("a generous ceiling refused the copy: %v", got)
	}

	// A ceiling that either package alone would fit under, but the two together
	// do not, must refuse. Per-candidate scoring would have admitted it.
	tight := decide(t, 12)
	if !containsString(tight.FailedGates(), deppolicy.GateCopiedLines) {
		t.Errorf("the aggregate ceiling did not refuse two packages that exceed it together: %v", tight.FailedGates())
	}
	for _, gate := range tight.Gates {
		if gate.Name != deppolicy.GateCopiedLines {
			continue
		}
		if !strings.Contains(gate.Unit, "across the accepted copy") {
			t.Errorf("the copied lines gate does not say it measures the whole copy: %q", gate.Unit)
		}
	}
}

// TestLeverageGateRefusesAPointlessCopy proves the benefit side is enforced.
//
// Copying is only justified by removing something from the consumer's build.
// The usual outcome of copying part of a module that stays for the rest is that
// nothing leaves at all, and a policy that only measured cost would happily
// approve paying for a copy that saves nothing.
func TestLeverageGateRefusesAPointlessCopy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	f := newFixture(t, twoPackageSource)
	decider, err := deppolicy.New(ctx, deppolicy.Options{
		ModulePath:     generatedModule,
		InternalPrefix: "internal/kk",
		SourceMinor:    36,
		Policy:         deppolicy.PolicyCopyApproved,
		Proposals:      []string{"staging/src/k8s.io/utilpkg/codec", "staging/src/k8s.io/utilpkg/text"},
		Gates: deppolicy.Gates{
			Interoperability: true, GlobalState: true, Diamond: true,
			Cost: deppolicy.CostCeilings{
				MaxCopiedPackages: 2, MaxCopiedLines: 500, MaxGeneratedFiles: 4,
				MaxDistinctLicenses: 1, MaxModuleZipBytes: 1 << 20, MaxReleasesPerMinor: 4,
				// The profile demands a benefit this copy cannot deliver.
				MinModulesRemoved: 1, MinPackagesRemoved: 1, MinLinesRemoved: 5000,
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
			Path: "k8s.io/utilpkg", Dir: f.dir + "/k8s.io/utilpkg",
			ZipBytes: 4096, ZipBytesKnown: true,
			ReleasesPerMinor: 2, CadenceKnown: true,
			Licenses:         []deppolicy.License{{Identifier: "Apache-2.0", Files: []string{"LICENSE"}}},
			LicensesVerified: true,
		}},
	})
	if err != nil {
		t.Fatalf("decide: %v", err)
	}

	report, _ := result.Candidate("k8s.io/utilpkg/codec")
	if !containsString(report.FailedGates(), deppolicy.GateMinimumLeverage) {
		t.Fatalf("a copy that delivers no configured benefit was admitted: %v", report.FailedGates())
	}
	evidence := strings.Join(gateEvidence(report, deppolicy.GateMinimumLeverage), "\n")
	if !strings.Contains(evidence, "buys too little to be worth owning") {
		t.Errorf("leverage evidence does not explain the refusal:\n%s", evidence)
	}
}

// TestCadenceGateRefusesFastMovingCode proves upstream cadence is a real gate
// rather than a number that is merely recorded.
func TestCadenceGateRefusesFastMovingCode(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	f := newFixture(t, pureLeafSource)
	decider, err := deppolicy.New(ctx, deppolicy.Options{
		ModulePath:     generatedModule,
		InternalPrefix: "internal/kk",
		SourceMinor:    36,
		Policy:         deppolicy.PolicyCopyApproved,
		Proposals:      []string{"staging/src/k8s.io/utilpkg/text"},
		Gates: deppolicy.Gates{
			Interoperability: true, GlobalState: true, Diamond: true,
			Cost: deppolicy.CostCeilings{
				MaxCopiedPackages: 1, MaxCopiedLines: 500, MaxDistinctLicenses: 1,
				MaxModuleZipBytes: 1 << 20, MaxReleasesPerMinor: 3,
			},
		},
	})
	if err != nil {
		t.Fatalf("new decider: %v", err)
	}

	result, err := decider.Decide(ctx, &deppolicy.Graph{
		Fset:       f.fset,
		Boundary:   []*deppolicy.Package{f.pkg(t, generatedModule)},
		Candidates: []deppolicy.Candidate{f.candidate(t, "k8s.io/utilpkg/text")},
		Build:      f.build(),
		Modules: []deppolicy.Module{{
			Path: "k8s.io/utilpkg", Dir: f.dir + "/k8s.io/utilpkg",
			ZipBytes: 4096, ZipBytesKnown: true,
			// Nine releases in one minor series is nine merges the generated
			// module performs itself once it owns the code.
			ReleasesPerMinor: 9, CadenceKnown: true,
			Licenses:         []deppolicy.License{{Identifier: "Apache-2.0", Files: []string{"LICENSE"}}},
			LicensesVerified: true,
		}},
	})
	if err != nil {
		t.Fatalf("decide: %v", err)
	}

	report, _ := result.Candidate("k8s.io/utilpkg/text")
	if !containsString(report.FailedGates(), deppolicy.GateUpstreamCadence) {
		t.Fatalf("fast moving upstream code was admitted: %v", report.FailedGates())
	}
}
