package provenance_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/provenance"
	"github.com/enj/soapbox/tools/internal/relocate"
	"github.com/enj/soapbox/tools/internal/rewrite"
	"github.com/enj/soapbox/tools/internal/typeswap"
)

// relocatedTree builds the file set the fixture's records describe.
func relocatedTree(t *testing.T) relocate.FileSet {
	t.Helper()
	set, err := relocate.Build(t.Context(), relocate.Plan{Files: []relocate.PlanFile{
		{
			Path: "plugin/pkg/auth/authorizer/rbac/rbac.go", Package: "plugin/pkg/auth/authorizer/rbac",
			Mode: relocate.ModeRegular, Contents: []byte("package rbac\n"),
		},
		{
			Path: "pkg/registry/rbac/validation/rule.go", Package: "pkg/registry/rbac/validation",
			Mode: relocate.ModeRegular, Contents: []byte("package validation\n"),
		},
		{
			Path: "pkg/registry/rbac/validation/doc.go", Package: "pkg/registry/rbac/validation",
			Mode: relocate.ModeRegular, Contents: []byte("package validation\n"),
		},
	}}, relocate.Options{InternalPrefix: "internal/kk"})
	if err != nil {
		t.Fatalf("relocate: %v", err)
	}
	return set
}

// TestCrossCheckAcceptsAccountedTree proves the fixture's records account for
// the tree they describe, which is the case every other subtest departs from.
func TestCrossCheckAcceptsAccountedTree(t *testing.T) {
	t.Parallel()
	if err := newOptions().CrossCheck(relocatedTree(t)); err != nil {
		t.Fatalf("cross check: %v", err)
	}
}

// TestCrossCheckRefusesUnaccountedTree covers every way the record and the tree
// can disagree.
//
// Validation sees only the records it was handed, so on its own it cannot tell
// complete evidence from evidence that is merely self consistent. Each case
// here is a NOTICE that would render without complaint while describing a
// module other than the one being published.
func TestCrossCheckRefusesUnaccountedTree(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*provenance.Options, *relocate.FileSet)
		want   string
	}{
		{
			name: "a relocated package has no record",
			mutate: func(o *provenance.Options, _ *relocate.FileSet) {
				o.Packages = o.Packages[:1]
			},
			want: "with no provenance record",
		},
		{
			name: "a relocated file has no record",
			mutate: func(_ *provenance.Options, set *relocate.FileSet) {
				extended, err := set.With(relocate.File{
					Source:        "plugin/pkg/auth/authorizer/rbac/extra.go",
					Path:          "internal/kk/plugin/pkg/auth/authorizer/rbac/extra.go",
					Package:       "internal/kk/plugin/pkg/auth/authorizer/rbac",
					SourcePackage: "plugin/pkg/auth/authorizer/rbac",
					Mode:          relocate.ModeRegular,
					Contents:      []byte("package rbac\n"),
				})
				if err == nil {
					*set = extended
				}
			},
			want: "with no file record",
		},
		{
			name: "a record describes a package the tree does not hold",
			mutate: func(o *provenance.Options, _ *relocate.FileSet) {
				o.Packages = append(o.Packages, rewrite.NewPackageProvenance(
					"internal/kk/pkg/apis/rbac", "pkg/apis/rbac", rewrite.Options{}))
			},
			want: "is not in the tree",
		},
		{
			name: "one package is recorded twice",
			mutate: func(o *provenance.Options, _ *relocate.FileSet) {
				o.Packages = append(o.Packages, o.Packages[0])
			},
			want: "is recorded twice",
		},
		{
			// Validation has already refused an empty record list, so a tree
			// with no relocated code shows up as records that match nothing.
			name: "the tree holds no relocated package",
			mutate: func(_ *provenance.Options, set *relocate.FileSet) {
				*set = relocate.FileSet{}
			},
			want: "is not in the tree",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			options := newOptions()
			set := relocatedTree(t)
			test.mutate(&options, &set)

			err := options.CrossCheck(set)
			if !errors.Is(err, provenance.ErrEvidence) {
				t.Fatalf("cross check error is %v, want ErrEvidence", err)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Errorf("cross check error %v does not say %q", err, test.want)
			}
		})
	}
}

// TestCrossCheckRefusesUnmodifiedTree is the case that catches a silent failure
// rather than a bookkeeping slip.
//
// A tree of relocated code in which no file records a change would render a
// NOTICE whose modification section reads "(none)". That is not an empty
// statement but a false one: it tells a reader the module ships this code
// unchanged, which is exactly the claim the licence forbids about code that was
// rewritten.
func TestCrossCheckRefusesUnmodifiedTree(t *testing.T) {
	t.Parallel()
	options := newOptions()
	stripped := make([]*rewrite.PackageProvenance, 0, len(options.Packages))
	for _, record := range options.Packages {
		bare := rewrite.NewPackageProvenance(record.Package, record.SourcePackage, rewrite.Options{})
		for _, file := range record.Files {
			bare.AddFile(rewrite.File{Path: file.Path, SourcePath: file.SourcePath}, rewrite.Result{})
		}
		stripped = append(stripped, bare)
	}
	options.Packages = stripped

	err := options.CrossCheck(relocatedTree(t))
	if !errors.Is(err, provenance.ErrEvidence) {
		t.Fatalf("cross check error is %v, want ErrEvidence", err)
	}
	if !strings.Contains(err.Error(), "no relocated file records a change") {
		t.Errorf("cross check error %v does not explain the false claim", err)
	}
}

// TestVerifyLicense covers the identifiers this engine will repeat and the ones
// it refuses to.
//
// A provenance file that names a licence makes a legal claim on the operator's
// behalf. It may name one whose text was checked, and it may not name one that
// was not, however plausible the identifier looks.
func TestVerifyLicense(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		id       string
		contents string
		want     error
	}{
		{
			name:     "Apache 2.0",
			id:       provenance.Apache20,
			contents: "                                 Apache License\n                           Version 2.0, January 2004\n",
		},
		{
			name:     "BSD 3 clause",
			id:       provenance.BSD3Clause,
			contents: "Redistribution and use in source and binary forms, with or without\nmodification\n\nNeither the name of the copyright holder\n",
		},
		{
			name:     "MIT",
			id:       provenance.MIT,
			contents: "MIT License\n\nPermission is hereby granted, free of charge, to any person\n",
		},
		{
			name:     "ISC",
			id:       provenance.ISC,
			contents: "Permission to use, copy, modify, and/or distribute this software for any\n",
		},
		{
			name:     "a text that is not the licence it claims",
			id:       provenance.Apache20,
			contents: "MIT License\n\nPermission is hereby granted, free of charge\n",
			want:     provenance.ErrLicense,
		},
		{
			name:     "a BSD 2 clause text labelled as BSD 3 clause",
			id:       provenance.BSD3Clause,
			contents: "Redistribution and use in source and binary forms, with or without\nmodification\n",
			want:     provenance.ErrLicense,
		},
		{
			name:     "an identifier this engine cannot verify",
			id:       "GPL-3.0-only",
			contents: "GNU GENERAL PUBLIC LICENSE\n",
			want:     provenance.ErrOptions,
		},
		{
			name:     "no identifier at all",
			id:       "",
			contents: "Apache License\nVersion 2.0\n",
			want:     provenance.ErrOptions,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := provenance.VerifyLicense(test.id, []byte(test.contents))
			switch {
			case test.want == nil && err != nil:
				t.Fatalf("verify: %v", err)
			case test.want != nil && !errors.Is(err, test.want):
				t.Fatalf("verify error is %v, want %v", err, test.want)
			}
		})
	}
}

// TestNoticeWordingFollowsTheLicense proves a section number is quoted only for
// the licence that has it.
//
// A citation attached to the wrong licence is worse than no citation: a reader
// who checks it finds the claim unsupported, and every other claim in the file
// becomes suspect.
func TestNoticeWordingFollowsTheLicense(t *testing.T) {
	t.Parallel()
	apache := flatten(renderedFile(t, newOptions(), provenance.NoticeFileName))
	for _, want := range []string{
		"section 4(d) of the Apache License 2.0",
		"Section 6 of the Apache License 2.0 grants no rights",
	} {
		if !strings.Contains(apache, want) {
			t.Errorf("Apache notice does not cite %q", want)
		}
	}

	options := newOptions()
	options.LicenseID = provenance.MIT
	options.License = []byte("MIT License\n\nPermission is hereby granted, free of charge\n")
	mit := flatten(renderedFile(t, options, provenance.NoticeFileName))
	if strings.Contains(mit, "Apache License 2.0") {
		t.Errorf("a module under the MIT licence cites the Apache License\n%s", mit)
	}
	for _, want := range []string{
		"as the MIT licence requires",
		"The MIT licence the copied code is under grants no rights",
		"licence: MIT",
	} {
		if !strings.Contains(mit, want) {
			t.Errorf("MIT notice does not contain %q\n%s", want, mit)
		}
	}
}

// TestCopiedPackageLicenseIsVerified proves a copied dependency's licence claim
// is checked against the grant that was copied with it.
func TestCopiedPackageLicenseIsVerified(t *testing.T) {
	t.Parallel()
	copied := func(id, contents, name string) provenance.CopiedPackage {
		return provenance.CopiedPackage{
			Module: "k8s.io/apiserver", Version: "v0.36.1",
			Package:          "k8s.io/apiserver/pkg/authorization/authorizer",
			Destination:      "internal/kk/staging/src/k8s.io/apiserver/pkg/authorization/authorizer",
			SourceRepository: "https://github.com/kubernetes/kubernetes.git",
			SourceSHA:        upstreamSHA,
			LicenseID:        id,
			Licenses: []provenance.LicenseFile{{
				Name:        name,
				SourcePath:  "staging/src/k8s.io/apiserver/" + name,
				Destination: "internal/kk/staging/src/k8s.io/apiserver/" + name,
				Contents:    []byte(contents),
				SHA256:      strings.Repeat("0", 64),
			}},
		}
	}
	tests := []struct {
		name   string
		copied provenance.CopiedPackage
		want   error
	}{
		{
			name:   "a verified grant",
			copied: copied(provenance.Apache20, upstreamLicense, "LICENSE"),
		},
		{
			name:   "a verified grant under an alternative spelling",
			copied: copied(provenance.Apache20, upstreamLicense, "LICENCE.md"),
		},
		{
			name:   "a mislabelled grant under an alternative spelling",
			copied: copied(provenance.Apache20, "MIT License\n\nPermission is hereby granted, free of charge\n", "COPYING.txt"),
			want:   provenance.ErrLicense,
		},
		{
			name:   "a mislabelled grant",
			copied: copied(provenance.Apache20, "MIT License\n\nPermission is hereby granted, free of charge\n", "LICENSE"),
			want:   provenance.ErrLicense,
		},
		{
			// A NOTICE travels with a grant but states neither the permission
			// nor the conditions, so it can never verify an identifier.
			name:   "only a notice, which grants nothing",
			copied: copied(provenance.Apache20, upstreamNotice, "NOTICE"),
			want:   provenance.ErrOptions,
		},
		{
			name:   "only a patent grant, which states no licence",
			copied: copied(provenance.Apache20, "Additional IP Rights Grant\n", "PATENTS.txt"),
			want:   provenance.ErrOptions,
		},
		{
			name:   "no identifier",
			copied: copied("", upstreamLicense, "LICENSE"),
			want:   provenance.ErrOptions,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			options := newOptions()
			options.Copied = []provenance.CopiedPackage{test.copied}
			_, err := options.Files()
			switch {
			case test.want == nil && err != nil:
				t.Fatalf("files: %v", err)
			case test.want != nil && !errors.Is(err, test.want):
				t.Fatalf("files error is %v, want %v", err, test.want)
			}
		})
	}
}

// TestBehaviorChangesFromTypeswap proves the type policy analysis reaches the
// published record.
//
// The analysis finds the import time effects a substitution removes, and those
// are exactly the differences no diff of the copied files would reveal. An
// analysis report nobody ships is not a disclosure.
func TestBehaviorChangesFromTypeswap(t *testing.T) {
	t.Parallel()
	result := &typeswap.Result{
		Policy: "prefer-external",
		Pairs: []typeswap.PairReport{{
			Internal: "k8s.io/kubernetes/pkg/apis/rbac",
			External: "k8s.io/api/rbac/v1",
			Action:   typeswap.Action("prune-internal"),
			BehaviorChanges: []typeswap.BehaviorChange{
				{
					Kind:     "scheme registration",
					Symbol:   "SchemeBuilder.Register",
					Detail:   "the RBAC types stop being added to the scheme when this module is imported",
					Position: "/build/checkout/pkg/apis/rbac/register.go:58",
				},
				{
					Kind:       "global mutation",
					Symbol:     "init",
					Detail:     "a package level map stops being populated",
					Position:   "/build/checkout/pkg/apis/rbac/v1/defaults.go:31",
					Observable: true,
				},
			},
		}},
	}

	changes := provenance.BehaviorChangesFrom(result)
	if len(changes) != 2 {
		t.Fatalf("conversion produced %d changes, want 2", len(changes))
	}
	// Sorted by summary, so the record is the same on every run.
	if changes[0].Summary > changes[1].Summary {
		t.Errorf("converted changes are not sorted: %v", changes)
	}
	for _, change := range changes {
		if !strings.Contains(change.Cause, "k8s.io/kubernetes/pkg/apis/rbac") {
			t.Errorf("change %q does not say which pairing removed it", change.Summary)
		}
		// A position names a file of the machine that ran the analysis, which a
		// committed record must never carry.
		if strings.Contains(change.Detail, "/build/checkout") || strings.Contains(change.Summary, "/build/checkout") {
			t.Errorf("change %q carries a path from the analysing machine", change.Summary)
		}
	}
	if !strings.Contains(changes[0].Detail, "reachable through the generated public API") {
		t.Errorf("change %q does not say whether a consumer can observe it", changes[0].Summary)
	}

	// The converted changes render, which is the point of converting them.
	options := newOptions()
	options.BehaviorChanges = changes
	notice := flatten(renderedFile(t, options, provenance.NoticeFileName))
	for _, change := range changes {
		if !strings.Contains(notice, flatten(change.Summary)) {
			t.Errorf("notice does not record %q", change.Summary)
		}
	}
}

// TestCheckBehaviorChangesRefusesOmission proves the disclosure cannot be
// forgotten.
//
// A profile that runs the analysis, acts on its decision, and renders a NOTICE
// without the effects that decision removed has published a module whose
// documented behaviour is not its behaviour, and nothing else would notice.
func TestCheckBehaviorChangesRefusesOmission(t *testing.T) {
	t.Parallel()
	result := &typeswap.Result{Pairs: []typeswap.PairReport{{
		Internal: "k8s.io/kubernetes/pkg/apis/rbac",
		External: "k8s.io/api/rbac/v1",
		Action:   typeswap.Action("prune-internal"),
		BehaviorChanges: []typeswap.BehaviorChange{{
			Kind:   "scheme registration",
			Symbol: "SchemeBuilder.Register",
			Detail: "the RBAC types stop being added to the scheme",
		}},
	}}}

	options := newOptions()
	err := options.CheckBehaviorChanges(result)
	if !errors.Is(err, provenance.ErrEvidence) {
		t.Fatalf("check error is %v, want ErrEvidence", err)
	}
	if !strings.Contains(err.Error(), "SchemeBuilder.Register") {
		t.Errorf("check error %v does not name the omitted effect", err)
	}

	options.BehaviorChanges = provenance.BehaviorChangesFrom(result)
	if err := options.CheckBehaviorChanges(result); err != nil {
		t.Errorf("check refuses a notice that records every effect: %v", err)
	}
	// An analysis that found nothing requires nothing.
	if err := newOptions().CheckBehaviorChanges(nil); err != nil {
		t.Errorf("check refuses a run with no analysis: %v", err)
	}
}
