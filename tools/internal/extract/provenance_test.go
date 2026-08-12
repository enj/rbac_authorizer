package extract_test

import (
	"context"
	"path"
	"slices"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/rewrite"
)

// TestPlanExposesRenderedPackageProvenance proves the structured evidence a plan
// hands back is the same evidence it committed beside each package.
//
// The two could only ever disagree if one were derived from the other by
// parsing, so this compares the rendering of every exposed record against the
// bytes of the file the tree actually carries. A record that described a package
// the module does not ship, or a shipped record no caller can see, is the
// failure that would let the root NOTICE state less than the tree contains.
func TestPlanExposesRenderedPackageProvenance(t *testing.T) {
	ctx := context.Background()
	up := newUpstream(ctx, t)
	result := mustPlan(ctx, t, planOptions(ctx, t, up, fixtureProfile))

	if len(result.Provenance) == 0 {
		t.Fatal("a successful plan exposed no package provenance")
	}

	committed := result.Report.Output.ProvenanceFiles
	if len(committed) != len(result.Provenance) {
		t.Fatalf("exposed %d records for %d committed provenance files:\n  exposed %v\n  committed %v",
			len(result.Provenance), len(committed), recordPackages(result.Provenance), committed)
	}

	rendered := make([]string, 0, len(result.Provenance))
	for _, record := range result.Provenance {
		rendered = append(rendered, record.Render())
	}
	slices.Sort(rendered)

	written := make([]string, 0, len(committed))
	for _, destination := range committed {
		written = append(written, contentsOf(t, result, destination))
	}
	slices.Sort(written)

	for i := range rendered {
		if rendered[i] != written[i] {
			t.Fatalf("exposed record does not match the committed one:\n--- exposed ---\n%s\n--- committed ---\n%s",
				rendered[i], written[i])
		}
	}
}

// TestPlanProvenanceAccountsForEveryRelocatedPackage proves the exposed records
// cover the relocated tree exactly once each, which is the property the root
// provenance cross-check depends on.
func TestPlanProvenanceAccountsForEveryRelocatedPackage(t *testing.T) {
	ctx := context.Background()
	up := newUpstream(ctx, t)
	opts := planOptions(ctx, t, up, fixtureProfile)
	result := mustPlan(ctx, t, opts)

	prefix := opts.Config.Destination.InternalPrefix
	var relocated []string
	for _, pkg := range result.Files.Packages {
		if pkg.Path == prefix || strings.HasPrefix(pkg.Path, prefix+"/") {
			relocated = append(relocated, pkg.Path)
		}
	}
	slices.Sort(relocated)

	recorded := recordPackages(result.Provenance)
	assertEqual(t, "recorded packages", recorded, relocated)

	for _, record := range result.Provenance {
		if record.SourcePackage == "" {
			t.Errorf("record for %s names no upstream package", record.Package)
		}
		// The record is provenance, so it has to say which commit of which
		// repository the package came from.
		if record.SourceSHA == "" || record.SourceRepository == "" {
			t.Errorf("record for %s does not identify its upstream commit: repository %q sha %q",
				record.Package, record.SourceRepository, record.SourceSHA)
		}
		if want := path.Join(prefix, record.SourcePackage); record.Package != want {
			t.Errorf("record for %s relocates %s, want destination %s", record.Package, record.SourcePackage, want)
		}
	}
}

// TestPlanProvenanceIsDeterministic proves the exposed evidence is a function of
// the profile and the source commit rather than of the directories a run used.
//
// The order matters as much as the content. A caller sorts nothing before
// composing the root NOTICE, so a record list that came back in a different
// order would produce a different committed NOTICE for an identical tree.
func TestPlanProvenanceIsDeterministic(t *testing.T) {
	ctx := context.Background()
	up := newUpstream(ctx, t)

	first := mustPlan(ctx, t, planOptions(ctx, t, up, fixtureProfile))
	second := mustPlan(ctx, t, planOptions(ctx, t, up, fixtureProfile))

	assertEqual(t, "record order", recordPackages(second.Provenance), recordPackages(first.Provenance))
	if len(first.Provenance) != len(second.Provenance) {
		t.Fatalf("record count = %d, want %d", len(second.Provenance), len(first.Provenance))
	}
	for i := range first.Provenance {
		if got, want := second.Provenance[i].Render(), first.Provenance[i].Render(); got != want {
			t.Errorf("record for %s differs between runs:\n--- first ---\n%s\n--- second ---\n%s",
				first.Provenance[i].Package, want, got)
		}
	}
}

// TestPlanProvenanceSurvivesAPolicyRefusal proves a refusal that happened after
// the tree was built still carries the evidence that tree produced.
//
// A refusal is when the evidence is most worth reading: the operator is about to
// decide whether the run was wrong or the profile was, and the per-package
// records are what that decision is made from.
func TestPlanProvenanceSurvivesAPolicyRefusal(t *testing.T) {
	ctx := context.Background()
	up := newUpstream(ctx, t)
	opts := planOptions(ctx, t, up, fixtureProfile)
	// The fixture profile pins a golden that does not exist, which is a notice
	// on its own and a refusal under -strict. The refusal happens after
	// relocation, so the records exist and have to survive it.
	opts.Strict = true

	result, err := planFailure(ctx, t, opts)
	mustPolicy(t, err, "plan strict")
	if result == nil {
		t.Fatal("a strict refusal produced no result")
	}
	if len(result.Provenance) == 0 {
		t.Fatal("a refusal after relocation exposed no package provenance")
	}
	if len(result.Provenance) != len(result.Report.Output.ProvenanceFiles) {
		t.Errorf("refusal exposed %d records for %d committed provenance files",
			len(result.Provenance), len(result.Report.Output.ProvenanceFiles))
	}
}

// TestPlanProvenanceCarriesNoAbsolutePath proves the exposed evidence is as
// portable as the report it accompanies.
//
// Every path in a record is repository or module relative by construction, and
// this states that as a property rather than trusting the construction, because
// an absolute path here would reach a committed NOTICE and name the machine that
// generated the module.
func TestPlanProvenanceCarriesNoAbsolutePath(t *testing.T) {
	ctx := context.Background()
	up := newUpstream(ctx, t)
	opts := planOptions(ctx, t, up, fixtureProfile)
	result := mustPlan(ctx, t, opts)

	for _, record := range result.Provenance {
		for _, value := range recordPaths(record) {
			if strings.HasPrefix(value, "/") || value != path.Clean(value) {
				t.Errorf("record for %s carries %q, which is not a clean relative path", record.Package, value)
			}
			for _, root := range []string{opts.CacheRoot, opts.WorkRoot, opts.OutputRoot, opts.ProfileDir} {
				if strings.Contains(value, root) {
					t.Errorf("record for %s carries %q, which names a directory this run used", record.Package, value)
				}
			}
		}
	}
}

// recordPackages lists the destination packages of a record set, in order.
func recordPackages(records []*rewrite.PackageProvenance) []string {
	out := make([]string, 0, len(records))
	for _, record := range records {
		out = append(out, record.Package)
	}
	return out
}

// recordPaths lists every path one record states.
func recordPaths(record *rewrite.PackageProvenance) []string {
	out := []string{record.Package, record.SourcePackage}
	for _, file := range record.Files {
		out = append(out, file.Path, file.SourcePath)
	}
	return append(out, record.Pruned...)
}
