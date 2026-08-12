package provenance_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/provenance"
	"github.com/enj/soapbox/tools/internal/relocate"
)

// TestCollectIsEmptyWithoutCopies is the case this engine is expected to be in.
//
// The RBAC profile copies nothing, because keeping every dependency external is
// what preserves the type identity the rest of the ecosystem uses. Collection
// has to say so with an empty result rather than by refusing, so a caller can
// run it unconditionally.
func TestCollectIsEmptyWithoutCopies(t *testing.T) {
	t.Parallel()
	licenses, err := provenance.Collect(t.Context(), provenance.CollectOptions{
		ModuleRoot:     "staging/src/k8s.io/apiserver",
		InternalPrefix: "internal/kk",
	})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(licenses) != 0 {
		t.Errorf("collect found %d licences for a profile that copies nothing", len(licenses))
	}
}

// TestCollectGathersGrantsUpToTheModuleRoot proves the walk finds every grant
// that governs a copied package, wherever the repository put it.
//
// A repository may keep its licence at the module root, beside a subtree that is
// licensed differently, or both, and a copy that carried only one of them would
// misstate what it is distributing.
func TestCollectGathersGrantsUpToTheModuleRoot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	files := map[string]string{
		// The grant at the module root governs everything below it.
		"staging/src/k8s.io/apiserver/LICENSE": upstreamLicense,
		"staging/src/k8s.io/apiserver/NOTICE":  upstreamNotice,
		// A subtree with its own additional grants, under the spellings a
		// repository actually publishes them with.
		"staging/src/k8s.io/apiserver/pkg/authorization/authorizer/PATENTS.txt": "Additional IP Rights Grant\n",
		"staging/src/k8s.io/apiserver/pkg/authorization/authorizer/COPYING.md":  "BSD 3-Clause\n",
		"staging/src/k8s.io/apiserver/pkg/authorization/LICENCE":                "the British spelling is the same document\n",
		// Above the module root, which the walk must not reach: it is a
		// different module's grant.
		"LICENSE": "the whole repository's licence\n",
		// Not a grant, however much it looks like one. A plural stem, a Go
		// file, and a document about licences are all matched by a glob and by
		// none of the exact names.
		"staging/src/k8s.io/apiserver/LICENSES.md":     "a summary of licences\n",
		"staging/src/k8s.io/apiserver/license.go":      "package apiserver\n",
		"staging/src/k8s.io/apiserver/pkg/NOTICES.rst": "not a NOTICE\n",
	}
	for name, contents := range files {
		writeFile(t, filepath.Join(root, filepath.FromSlash(name)), contents)
	}

	licenses, err := provenance.Collect(t.Context(), provenance.CollectOptions{
		FS:         os.DirFS(root),
		ModuleRoot: "staging/src/k8s.io/apiserver",
		Packages: []string{
			"staging/src/k8s.io/apiserver/pkg/authorization/authorizer",
			// The same module root twice, so a grant found through two packages
			// is collected once.
			"staging/src/k8s.io/apiserver/pkg/authorization/path",
		},
		InternalPrefix: "internal/kk",
	})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}

	want := []string{
		"internal/kk/staging/src/k8s.io/apiserver/LICENSE",
		"internal/kk/staging/src/k8s.io/apiserver/NOTICE",
		"internal/kk/staging/src/k8s.io/apiserver/pkg/authorization/LICENCE",
		"internal/kk/staging/src/k8s.io/apiserver/pkg/authorization/authorizer/COPYING.md",
		"internal/kk/staging/src/k8s.io/apiserver/pkg/authorization/authorizer/PATENTS.txt",
	}
	got := make([]string, len(licenses))
	for i, license := range licenses {
		got[i] = license.Destination
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("collected destinations are\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	for _, license := range licenses {
		// The destination preserves the full upstream path below the internal
		// prefix, so a grant sits at the same relative position to the files it
		// governs as it did upstream.
		if license.Destination != "internal/kk/"+license.SourcePath {
			t.Errorf("%s does not preserve its upstream path", license.Destination)
		}
		if license.SHA256 == "" || len(license.SHA256) != 64 {
			t.Errorf("%s has digest %q, want a sha256 hex digest", license.Destination, license.SHA256)
		}
		if len(license.Contents) == 0 {
			t.Errorf("%s carries no contents", license.Destination)
		}
	}
}

// TestLicenseNamesCoverPublishedSpellings pins the vocabulary a collection
// looks for.
//
// A repository publishes its grant as a bare name, as Markdown, or as plain
// text, and under either spelling of licence. Missing one would report a
// package as having no grant, which fails a copy closed for a reason that is
// not true. Matching by pattern instead would sweep up documents about licences
// and copy them as though they were grants.
func TestLicenseNamesCoverPublishedSpellings(t *testing.T) {
	t.Parallel()
	for _, want := range []string{
		"LICENSE", "LICENSE.md", "LICENSE.txt",
		"LICENCE", "LICENCE.md", "LICENCE.txt",
		"COPYING", "COPYING.md", "COPYING.txt",
		"NOTICE", "NOTICE.md", "NOTICE.txt",
		"PATENTS", "PATENTS.md", "PATENTS.txt",
	} {
		if !slices.Contains(provenance.LicenseNames, want) {
			t.Errorf("the collection does not look for %s", want)
		}
	}
	for _, unwanted := range []string{"LICENSES", "LICENSES.md", "license", "NOTICES", "LICENSE.rst"} {
		if slices.Contains(provenance.LicenseNames, unwanted) {
			t.Errorf("the collection looks for %s, which states no grant", unwanted)
		}
	}
	if !slices.IsSorted(provenance.LicenseNames) {
		t.Errorf("the collection order is not fixed: %v", provenance.LicenseNames)
	}
}

// TestCollectedLicenseComposesAsARelocatedFile proves a collected grant reaches
// the tree through the same write boundary as the code it belongs to.
func TestCollectedLicenseComposesAsARelocatedFile(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "staging", "src", "k8s.io", "apiserver", "LICENSE"), upstreamLicense)

	licenses, err := provenance.Collect(t.Context(), provenance.CollectOptions{
		FS:             os.DirFS(root),
		ModuleRoot:     "staging/src/k8s.io/apiserver",
		Packages:       []string{"staging/src/k8s.io/apiserver"},
		InternalPrefix: "internal/kk",
	})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(licenses) != 1 {
		t.Fatalf("collect found %d licences, want 1", len(licenses))
	}

	file := licenses[0].File()
	set, err := relocate.FileSet{}.With(file)
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	composed, ok := set.Lookup("internal/kk/staging/src/k8s.io/apiserver/LICENSE")
	if !ok {
		t.Fatalf("composed set does not hold the collected licence")
	}
	if composed.Source != "staging/src/k8s.io/apiserver/LICENSE" {
		t.Errorf("collected licence records upstream source %q", composed.Source)
	}
	if string(composed.Contents) != upstreamLicense {
		t.Errorf("collected licence was not copied unchanged")
	}
	if composed.Generated {
		t.Errorf("a copied licence is not a generated file")
	}

	// The returned file owns its bytes, so a caller mutating the composed tree
	// cannot reach back into the collection.
	composed.Contents[0] = 'X'
	if licenses[0].Contents[0] == 'X' {
		t.Errorf("the composed file shares its bytes with the collected licence")
	}
}

// TestCollectRefusesUnusableInputs covers the failures that would otherwise
// produce a copy whose licence position or provenance is wrong.
func TestCollectRefusesUnusableInputs(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "staging", "src", "k8s.io", "apiserver", "LICENSE"), upstreamLicense)
	writeFile(t, filepath.Join(root, "staging", "src", "k8s.io", "empty", "LICENSE"), "")

	tests := []struct {
		name string
		opts provenance.CollectOptions
	}{
		{
			name: "no worktree",
			opts: provenance.CollectOptions{
				ModuleRoot: "staging/src/k8s.io/apiserver", InternalPrefix: "internal/kk",
				Packages: []string{"staging/src/k8s.io/apiserver"},
			},
		},
		{
			name: "package outside the module root",
			opts: provenance.CollectOptions{
				FS: os.DirFS(root), ModuleRoot: "staging/src/k8s.io/apiserver", InternalPrefix: "internal/kk",
				Packages: []string{"staging/src/k8s.io/api"},
			},
		},
		{
			name: "absolute module root",
			opts: provenance.CollectOptions{
				FS: os.DirFS(root), ModuleRoot: "/staging/src/k8s.io/apiserver", InternalPrefix: "internal/kk",
				Packages: []string{"/staging/src/k8s.io/apiserver"},
			},
		},
		{
			name: "internal prefix escaping the module",
			opts: provenance.CollectOptions{
				FS: os.DirFS(root), ModuleRoot: "staging/src/k8s.io/apiserver", InternalPrefix: "../kk",
				Packages: []string{"staging/src/k8s.io/apiserver"},
			},
		},
		{
			name: "empty licence file states no grant",
			opts: provenance.CollectOptions{
				FS: os.DirFS(root), ModuleRoot: "staging/src/k8s.io/empty", InternalPrefix: "internal/kk",
				Packages: []string{"staging/src/k8s.io/empty"},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := provenance.Collect(t.Context(), test.opts); !errors.Is(err, provenance.ErrOptions) {
				t.Fatalf("collect error is %v, want ErrOptions", err)
			}
		})
	}
}

// TestCollectHonoursCancellation proves the collection is a context aware
// boundary like every other one in the engine.
func TestCollectHonoursCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "staging", "src", "k8s.io", "apiserver", "LICENSE"), upstreamLicense)

	_, err := provenance.Collect(ctx, provenance.CollectOptions{
		FS:             os.DirFS(root),
		ModuleRoot:     "staging/src/k8s.io/apiserver",
		Packages:       []string{"staging/src/k8s.io/apiserver"},
		InternalPrefix: "internal/kk",
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("collect error is %v, want context.Canceled", err)
	}
}
