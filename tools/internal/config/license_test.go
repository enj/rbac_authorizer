package config_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/enj/soapbox/tools/internal/config"
	"github.com/enj/soapbox/tools/internal/provenance"
)

// TestLicenseIdentifiersMatchVerifier proves every identifier a profile may name
// is one the engine can check the text of.
//
// The two lists live in different packages because the verifier reaches this one
// through the relocation and rewriting layers, so importing it into the schema
// would close a cycle. That leaves a copied literal, and a copied literal is
// exactly what drifts: a profile could keep validating while naming a licence
// nothing verifies, and the first sign of it would be a published NOTICE
// asserting a grant nobody confirmed. This test is the link between them, and it
// lives in the external test package so the import runs the other way.
//
// An empty text is passed on purpose. A known identifier fails on its missing
// markers, an unknown one fails as unusable options, and only the second is the
// answer this test is asking for.
func TestLicenseIdentifiersMatchVerifier(t *testing.T) {
	identifiers := config.LicenseIdentifiers()
	if len(identifiers) == 0 {
		t.Fatal("the profile schema names no licence identifiers")
	}
	for _, id := range identifiers {
		t.Run(id, func(t *testing.T) {
			err := provenance.VerifyLicense(id, nil)
			if err == nil {
				t.Fatalf("VerifyLicense(%q, nil) accepted an empty licence text", id)
			}
			if errors.Is(err, provenance.ErrOptions) {
				t.Fatalf("the profile schema admits %q, which the engine cannot verify: %v", id, err)
			}
			if !errors.Is(err, provenance.ErrLicense) {
				t.Fatalf("VerifyLicense(%q, nil) = %v, want a licence text failure", id, err)
			}
		})
	}
}

// TestLicenseIdentifiersAreSortedAndDistinct keeps the schema's vocabulary
// stable, because it is rendered into the message an operator reads after
// naming a licence the engine will not publish.
func TestLicenseIdentifiersAreSortedAndDistinct(t *testing.T) {
	identifiers := config.LicenseIdentifiers()
	if !slices.IsSorted(identifiers) {
		t.Errorf("licence identifiers are not sorted: %v", identifiers)
	}
	if len(slices.Compact(slices.Clone(identifiers))) != len(identifiers) {
		t.Errorf("licence identifiers contain a duplicate: %v", identifiers)
	}
}

// TestLicenseIdentifiersAreCopied proves a caller cannot edit the schema's
// vocabulary through the slice it was handed.
func TestLicenseIdentifiersAreCopied(t *testing.T) {
	first := config.LicenseIdentifiers()
	first[0] = "Mutated"
	if second := config.LicenseIdentifiers(); second[0] == "Mutated" {
		t.Error("LicenseIdentifiers exposes its backing array")
	}
}
