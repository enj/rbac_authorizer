package deppolicy_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/deppolicy"
)

// generatedModule is the module path the fixtures generate into.
const generatedModule = "monis.app/kk/rbac_authorizer"

// pureLeafSource is a candidate that is genuinely safe to relocate: no exported
// state, no init, no registration, no context key, and nothing of its own on the
// public boundary.
var pureLeafSource = fixtureSource{
	Modules: []string{generatedModule, "k8s.io/utilpkg"},
	Files: map[string]string{
		"k8s.io/utilpkg/text/text.go": `package text

// Normalize trims and lowers a verb.
func Normalize(in string) string {
	if in == "" {
		return "*"
	}
	return in
}
`,
		generatedModule + "/facade.go": `package rbacauthorizer

import "k8s.io/utilpkg/text"

// Normalize is forwarded from the relocated helper.
func Normalize(in string) string { return text.Normalize(in) }
`,
	},
	ModuleFiles: map[string]string{
		"k8s.io/utilpkg/LICENSE": "Apache License 2.0\n",
	},
}

func TestDecideCopiesPureLeafUtility(t *testing.T) {
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
			Interoperability: true,
			GlobalState:      true,
			Diamond:          true,
			Cost: deppolicy.CostCeilings{
				MaxCopiedPackages:   1,
				MaxCopiedLines:      50,
				MaxGeneratedFiles:   0,
				MaxDistinctLicenses: 1,
				MaxModuleZipBytes:   1 << 20,
				MaxReleasesPerMinor: 4,
			},
		},
	})
	if err != nil {
		t.Fatalf("new decider: %v", err)
	}

	graph := &deppolicy.Graph{
		Fset:       f.fset,
		Boundary:   []*deppolicy.Package{f.pkg(t, generatedModule)},
		Candidates: []deppolicy.Candidate{f.candidate(t, "k8s.io/utilpkg/text")},
		Build:      f.build(),
		Modules: []deppolicy.Module{{
			Path: "k8s.io/utilpkg", Version: "v0.36.1", Dir: f.dir + "/k8s.io/utilpkg",
			ZipBytes: 4096, ZipBytesKnown: true,
			ReleasesPerMinor: 2, CadenceKnown: true,
			Licenses:         []deppolicy.License{{Identifier: "Apache-2.0", Files: []string{"LICENSE"}}},
			LicensesVerified: true,
		}},
	}
	result, err := decider.Decide(ctx, graph)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}

	report, ok := result.Candidate("k8s.io/utilpkg/text")
	if !ok {
		t.Fatalf("result has no candidate, got %+v", result.Candidates)
	}
	if got := report.FailedGates(); len(got) != 0 {
		t.Fatalf("pure leaf utility refused by %v\n%s", got, result)
	}
	if report.Action != deppolicy.ActionCopy {
		t.Errorf("action = %q, want %q", report.Action, deppolicy.ActionCopy)
	}
	if want := []string{"staging/src/k8s.io/utilpkg/text"}; !equalStrings(result.Copy, want) {
		t.Errorf("copy = %v, want %v", result.Copy, want)
	}
	if report.Score.CopiedLines == 0 {
		t.Error("copied lines were not measured")
	}
	if !equalStrings(report.Score.LicenseIdentifiers, []string{"Apache-2.0"}) {
		t.Errorf("license identifiers = %v, want [Apache-2.0]", report.Score.LicenseIdentifiers)
	}
}

func TestNewRejectsInvalidOptions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	base := func() deppolicy.Options {
		return deppolicy.Options{
			ModulePath:     generatedModule,
			InternalPrefix: "internal/kk",
			SourceMinor:    36,
			Policy:         deppolicy.PolicyCopyApproved,
			Proposals:      []string{"staging/src/k8s.io/apiserver/pkg/authorization/authorizer"},
			Gates:          deppolicy.Gates{Interoperability: true, GlobalState: true, Diamond: true},
		}
	}

	tests := []struct {
		name    string
		mutate  func(*deppolicy.Options)
		problem string
	}{
		{
			name:    "unknown policy",
			mutate:  func(o *deppolicy.Options) { o.Policy = "copy-everything" },
			problem: `unsupported value "copy-everything"`,
		},
		{
			name:    "external policy proposing copies",
			mutate:  func(o *deppolicy.Options) { o.Policy = deppolicy.PolicyExternal },
			problem: "cannot propose 1 staging copies",
		},
		{
			name:    "copy policy without proposals",
			mutate:  func(o *deppolicy.Options) { o.Proposals = nil },
			problem: "requires at least one proposal",
		},
		{
			name:    "proposal outside staging",
			mutate:  func(o *deppolicy.Options) { o.Proposals = []string{"pkg/registry/rbac/validation"} },
			problem: "must start at staging/src",
		},
		{
			name:    "duplicate proposal",
			mutate:  func(o *deppolicy.Options) { o.Proposals = append(o.Proposals, o.Proposals[0]) },
			problem: "duplicate proposal",
		},
		{
			name:    "correctness gate disabled while proposing a copy",
			mutate:  func(o *deppolicy.Options) { o.Gates.Diamond = false },
			problem: "cannot disable a correctness gate",
		},
		{
			name: "override relaxing a correctness gate",
			mutate: func(o *deppolicy.Options) {
				o.Overrides = []deppolicy.Override{{
					StagingPath:       o.Proposals[0],
					Gate:              deppolicy.GateInteroperability,
					Justification:     "we accept the risk",
					Approver:          "someone",
					ExpiresAfterMinor: 40,
				}}
			},
			problem: "correctness gate \"interoperability\" cannot be overridden",
		},
		{
			name: "override without an approver",
			mutate: func(o *deppolicy.Options) {
				o.Overrides = []deppolicy.Override{{
					StagingPath:       o.Proposals[0],
					Gate:              deppolicy.GateCopiedLines,
					Justification:     "small",
					ExpiresAfterMinor: 40,
				}}
			},
			problem: "needs an approver",
		},
		{
			name: "override without an expiry",
			mutate: func(o *deppolicy.Options) {
				o.Overrides = []deppolicy.Override{{
					StagingPath:   o.Proposals[0],
					Gate:          deppolicy.GateCopiedLines,
					Justification: "small",
					Approver:      "someone",
				}}
			},
			problem: "needs a Kubernetes minor expiry",
		},
		{
			name:    "identity requirement that is not a qualified type",
			mutate:  func(o *deppolicy.Options) { o.IdentityRequired = []string{"Authorizer"} },
			problem: "must be a fully qualified type name",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			opts := base()
			test.mutate(&opts)

			_, err := deppolicy.New(ctx, opts)
			if err == nil {
				t.Fatalf("New accepted %s", test.name)
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

// equalStrings compares two string slices, treating nil and empty as equal.
func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
