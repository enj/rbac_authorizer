package extract

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/closure"
	"github.com/enj/soapbox/tools/internal/config"
	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/patchset"
	"github.com/enj/soapbox/tools/internal/relocate"
	"github.com/enj/soapbox/tools/internal/source"
	"github.com/enj/soapbox/tools/internal/testsupport"
)

// TestSummarizeTreeFailsClosedOnAMissingFile proves the manifest covers exactly
// the tree.
//
// The hash is what two plans compare to decide they produced the same module. A
// package index naming a file the set does not hold is an engine fault, and
// skipping it would leave the hash covering fewer files than the tree holds, so
// two different modules would agree.
func TestProfileHashBindsEngineVersion(t *testing.T) {
	t.Parallel()
	profile := []byte("profile bytes\n")
	first := profileHash(profile, "0.1.0")
	if first != profileHash(profile, "0.1.0") {
		t.Fatal("identical profile and engine inputs produced different hashes")
	}
	if first == profileHash(profile, "0.1.1") {
		t.Fatal("changing the engine version left the profile epoch unchanged")
	}
	if first == profileHash([]byte("other profile\n"), "0.1.0") {
		t.Fatal("changing the profile left the profile epoch unchanged")
	}
	if !strings.HasPrefix(first, "sha256:") || len(first) != len("sha256:")+64 {
		t.Fatalf("profile hash = %q, want canonical sha256 form", first)
	}
}

func TestSummarizeTreeFailsClosedOnAMissingFile(t *testing.T) {
	present := relocate.File{
		Source: "pkg/only/only.go", Path: "internal/kk/pkg/only/only.go",
		Package: "internal/kk/pkg/only", SourcePackage: "pkg/only",
		Mode: relocate.ModeRegular, Contents: []byte("package only\n"),
	}
	r := &run{
		cfg:           &config.Config{},
		closureResult: &closure.Result{},
		tree: relocate.FileSet{
			Files: []relocate.File{present},
			Packages: []relocate.Package{{
				Source: "pkg/only",
				Path:   "internal/kk/pkg/only",
				Files:  []string{present.Path, "internal/kk/pkg/only/absent.go"},
			}},
		},
	}

	err := r.summarizeTree()
	if err == nil {
		t.Fatal("a destination the file set does not hold must stop the run")
	}
	if !strings.Contains(err.Error(), "absent.go") {
		t.Errorf("the failure does not name the missing destination: %v", err)
	}
	if r.report.Output.ManifestHash != "" {
		t.Errorf("a manifest was emitted for an incomplete tree: %q", r.report.Output.ManifestHash)
	}
}

// TestSummarizeTreeCountsClosurePackages proves the reported package count is
// the closure's rather than the tree's directory count.
//
// A package that carries embedded data in a subdirectory relocates that file
// into a directory of its own. Counting directories would report a module with
// more packages than any build of it has.
func TestSummarizeTreeCountsClosurePackages(t *testing.T) {
	code := relocate.File{
		Source: "pkg/only/only.go", Path: "internal/kk/pkg/only/only.go",
		Package: "internal/kk/pkg/only", SourcePackage: "pkg/only",
		Mode: relocate.ModeRegular, Contents: []byte("package only\n"),
	}
	asset := relocate.File{
		Source: "pkg/only/testdata/asset.go", Path: "internal/kk/pkg/only/testdata/asset.go",
		Package: "internal/kk/pkg/only/testdata", SourcePackage: "pkg/only/testdata",
		Mode: relocate.ModeRegular, Contents: []byte("not source\n"),
	}
	r := &run{
		cfg:           &config.Config{},
		closureResult: &closure.Result{Packages: []closure.Package{{Dir: "pkg/only"}}},
		tree: relocate.FileSet{
			Files: []relocate.File{code, asset},
			Packages: []relocate.Package{
				{Source: "pkg/only", Path: "internal/kk/pkg/only", Files: []string{code.Path}},
				{Source: "pkg/only/testdata", Path: "internal/kk/pkg/only/testdata", Files: []string{asset.Path}},
			},
		},
	}

	if err := r.summarizeTree(); err != nil {
		t.Fatalf("summarize: %v", err)
	}
	if got, want := r.report.Output.Packages, 1; got != want {
		t.Errorf("output packages = %d, want %d", got, want)
	}
	if got, want := r.report.Output.Files, 2; got != want {
		t.Errorf("output files = %d, want %d", got, want)
	}
}

// TestPatchPolicyKeepsCancellations proves the one cancellation shape that
// arrives wearing a finding's type.
//
// Patch application reports a context that ended as a conflict at the cancel
// stage, which is the same type a patch that no longer applies produces.
// Classifying on the type alone would turn every interrupted patch pass into a
// verdict on the patch, and the command would exit with the code CI reads as
// "review this" for a run that was simply stopped.
func TestPatchPolicyKeepsCancellations(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		isPolicy bool
	}{
		{
			name: "cancelled patch pass",
			err: &patchset.ConflictError{
				Stage: patchset.StageCancel, PatchID: "patches/x.patch",
				Err: context.Canceled,
			},
		},
		{
			name: "expired deadline",
			err: &patchset.ConflictError{
				Stage: patchset.StageCancel, PatchID: "patches/x.patch",
				Err: context.DeadlineExceeded,
			},
		},
		{
			name: "patch that no longer applies",
			err: &patchset.ConflictError{
				Stage: patchset.StageApply, PatchID: "patches/x.patch",
				Err: errors.New("does not apply"),
			},
			isPolicy: true,
		},
		{
			name:     "profile names two patches with one identifier",
			err:      patchset.ErrDuplicatePatch,
			isPolicy: true,
		},
		{
			name:     "tree was dirty before the pass",
			err:      patchset.ErrDirtyWorkTree,
			isPolicy: true,
		},
		{
			// A file that could not be read is an environment fault rather than
			// a statement about the patch series.
			name: "patch file could not be read",
			err:  errors.New("load patch \"patches/x.patch\": permission denied"),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			classified := patchPolicy(test.err)
			var policy *PolicyError
			if got := errors.As(classified, &policy); got != test.isPolicy {
				t.Errorf("policy failure = %t, want %t: %v", got, test.isPolicy, classified)
			}
			if !errors.Is(classified, test.err) {
				t.Errorf("classification severed the cause: %v", classified)
			}
		})
	}
}

// TestScrubReplacesLocalPaths proves a failure message cannot carry a directory
// from the machine that produced it into the report.
//
// The message is the one part of the report assembled from text a lower package
// wrote, and those packages name the file they were working on.
func TestScrubReplacesLocalPaths(t *testing.T) {
	root := t.TempDir()
	r := &run{
		opts: Options{
			CacheRoot:    filepath.Join(root, "cache"),
			ProfileDir:   filepath.Join(root, "profile"),
			SourceRemote: "file://" + filepath.Join(root, "mirror"),
		},
		paths: Paths{
			Cache:    filepath.Join(root, "cache", "k8s.git"),
			Work:     filepath.Join(root, "work"),
			Worktree: filepath.Join(root, "work", "src", "wt-abc"),
			Output:   filepath.Join(root, "tree"),
		},
	}

	message := strings.Join([]string{
		"read " + r.paths.Worktree + "/pkg/only/only.go",
		"cache " + r.paths.Cache,
		"work " + r.paths.Work,
		"output " + r.paths.Output,
		"profile " + r.opts.ProfileDir,
		"remote " + r.opts.SourceRemote,
	}, "; ")

	scrubbed := r.scrub(message)
	for _, local := range []string{
		r.paths.Worktree, r.paths.Cache, r.paths.Work,
		r.paths.Output, r.opts.ProfileDir, r.opts.SourceRemote, root,
	} {
		if strings.Contains(scrubbed, local) {
			t.Errorf("the scrubbed message still names %q:\n%s", local, scrubbed)
		}
	}
	// The nested roots are replaced by the most specific placeholder, so the
	// work tree does not surface as the work root plus a suffix.
	if !strings.Contains(scrubbed, "<worktree>/pkg/only/only.go") {
		t.Errorf("the work tree was not replaced as a whole:\n%s", scrubbed)
	}
}

// TestWidenBoundFollowsTheProfile proves the widening bound is the profile's own
// package ceiling rather than a number the engine chose.
func TestWidenBoundFollowsTheProfile(t *testing.T) {
	tests := []struct {
		name  string
		limit int
		roots []string
		want  int
	}{
		{name: "profile sets a ceiling", limit: 200, roots: []string{"a", "b"}, want: 203},
		{name: "profile sets none", limit: 0, roots: []string{"a"}, want: defaultWidenCeiling + 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Closure.Limits.MaxPackages = test.limit
			cfg.Packages.Roots = test.roots

			if got := (&run{cfg: cfg}).widenBound(); got != test.want {
				t.Errorf("widen bound = %d, want %d", got, test.want)
			}
		})
	}
}

// TestOwningPackageTakesTheLongestAncestor proves an asset below a package
// belongs to that package rather than to the nearest directory that happens to
// hold files.
func TestOwningPackageTakesTheLongestAncestor(t *testing.T) {
	r := &run{closureDirs: map[string]bool{
		"pkg/apis/rbac":    true,
		"pkg/apis/rbac/v1": true,
	}}
	tests := []struct {
		dir   string
		want  string
		found bool
	}{
		{dir: "pkg/apis/rbac/v1", want: "pkg/apis/rbac/v1", found: true},
		{dir: "pkg/apis/rbac/v1/testdata", want: "pkg/apis/rbac/v1", found: true},
		{dir: "pkg/apis/rbac/v1/testdata/nested", want: "pkg/apis/rbac/v1", found: true},
		{dir: "pkg/apis/rbac/other", want: "pkg/apis/rbac", found: true},
		{dir: "docs", want: "", found: false},
		{dir: ".", want: "", found: false},
	}
	for _, test := range tests {
		t.Run(test.dir, func(t *testing.T) {
			got, found := r.owningPackage(test.dir)
			if got != test.want || found != test.found {
				t.Errorf("owningPackage(%q) = %q, %t, want %q, %t", test.dir, got, found, test.want, test.found)
			}
		})
	}
}

// TestCentralProvenanceNamesAreDistinct proves two package directories that
// flatten to one file name still get separate records.
func TestCentralProvenanceNamesAreDistinct(t *testing.T) {
	first := centralProvenancePath("pkg/apis_rbac")
	second := centralProvenancePath("pkg/apis/rbac")
	if first == second {
		t.Fatalf("two package directories produced one record path: %s", first)
	}
	for _, path := range []string{first, second} {
		if err := config.ValidatePackagePath(path); err != nil {
			t.Errorf("record path %q is not a usable relative path: %v", path, err)
		}
		if !strings.HasPrefix(path, provenanceDirName+"/") {
			t.Errorf("record path %q is not below the reserved directory", path)
		}
	}
	// The mapping is a function of the package alone, so two runs agree.
	if centralProvenancePath("pkg/apis/rbac") != second {
		t.Error("the record path is not deterministic")
	}
}

// TestAssertCacheUnchangedDetectsAMovedRef proves the invariant over a real
// cache whose refs really moved.
//
// The comparison is what makes the shared cache safe to reuse, and it is
// checked on the way out of every plan rather than only the successful one: a
// run that failed for an unrelated reason is exactly when nobody would think to
// look.
func TestAssertCacheUnchangedDetectsAMovedRef(t *testing.T) {
	ctx := t.Context()
	cache := openTestCache(ctx, t)

	r := &run{cfg: &config.Config{}, cache: cache}
	r.report.Schema = ReportSchema

	refs, err := cache.Git().ListRefs(ctx)
	if err != nil {
		t.Fatalf("list refs: %v", err)
	}
	if len(refs) == 0 {
		t.Fatal("the cache holds no refs, so moving one proves nothing")
	}
	r.cacheRefs = refs

	// Nothing moved yet, so the invariant holds and the report says so.
	if err := r.assertCacheUnchanged(ctx); err != nil {
		t.Fatalf("an untouched cache was reported as moved: %v", err)
	}
	if r.report.Worktree.CacheRefsMoved {
		t.Error("an untouched cache was recorded as moved")
	}

	// A real ref appears in the cache, which is exactly what a plan may never
	// cause and must always notice.
	fresh := &run{cfg: &config.Config{}, cache: cache, cacheRefs: refs}
	fresh.report.Schema = ReportSchema
	if err := cache.Git().CreateTag(ctx, gitcli.TagOptions{
		Name:    "intruder",
		Commit:  refs[0].Commit,
		Message: "a ref a plan must never create\n",
		Tagger:  gitcli.Signature{Name: "Soapbox Test", Email: "test@example.com", Date: "2026-01-02T03:04:05Z"},
	}); err != nil {
		t.Fatalf("create the intruding tag: %v", err)
	}

	moved := fresh.assertCacheUnchanged(ctx)
	if moved == nil {
		t.Fatal("a ref moved in the cache and the invariant did not notice")
	}
	if !fresh.report.Worktree.CacheRefsMoved {
		t.Error("the report does not record the moved ref")
	}
	if !strings.Contains(moved.Error(), "refs/tags/intruder") {
		t.Errorf("the failure does not name the ref that appeared: %v", moved)
	}

	// The answer is memoised, so the failure paths that check again on the way
	// out cannot report a different one from the phase that checked first.
	if again := fresh.assertCacheUnchanged(ctx); !errors.Is(again, moved) {
		t.Errorf("the second check reported %v rather than the first answer", again)
	}

	// The partial report is what makes the corruption reviewable.
	result := fresh.failedResult(moved)
	if !result.Report.Worktree.CacheRefsMoved {
		t.Error("the partial report does not record the moved ref")
	}
	if result.Report.Failure == nil {
		t.Fatal("the partial report carries no failure section")
	}
	if _, err := result.Report.JSON(); err != nil {
		t.Errorf("the partial report does not encode: %v", err)
	}
}

// openTestCache builds a real upstream repository and the blobless cache of it
// that a plan would open.
func openTestCache(ctx context.Context, t *testing.T) *source.Cache {
	t.Helper()
	repo := testsupport.NewRepo(ctx, t, testsupport.Options{
		Branch:    "master",
		UserName:  "Soapbox Test",
		UserEmail: "test@example.com",
	})
	// The cache is a partial clone and source.Open proves the filter took
	// effect, so the fixture server has to advertise object filtering.
	repo.SetConfig(ctx, t, "uploadpack.allowFilter", "true")
	repo.WriteFile(t, "pkg/only/only.go", "package only\n")
	repo.Commit(ctx, t, "feat: add a package\n", gitcli.CommitOptions{}, "pkg/only/only.go")

	git, err := gitcli.New(ctx, gitcli.Options{Inherit: []string{"PATH"}})
	if err != nil {
		t.Fatalf("create git runner: %v", err)
	}
	root := t.TempDir()
	cache, err := source.Open(ctx, source.Options{
		Remote:       "file://" + repo.Dir,
		CacheRoot:    filepath.Join(root, "cache"),
		WorktreeRoot: filepath.Join(root, "work", worktreeDirName),
		Git:          git,
	})
	if err != nil {
		t.Fatalf("open cache: %v", err)
	}
	if err := cache.Fetch(ctx, source.Refs{Branches: []string{"master"}}); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	return cache
}

// TestCompareExactNamesTheDifference proves a golden mismatch says what changed.
//
// A single "they differ" leaves the reader diffing two JSON documents, and the
// whole value of the gate is that the change is named.
func TestCompareExactNamesTheDifference(t *testing.T) {
	golden := closure.ExactShape{
		ImportPrefix: "k8s.io/kubernetes",
		Packages:     []string{"k8s.io/kubernetes/pkg/a", "k8s.io/kubernetes/pkg/b"},
		Files:        []string{"pkg/a/a.go"},
	}
	current := closure.ExactShape{
		ImportPrefix: "k8s.io/kubernetes",
		Packages:     []string{"k8s.io/kubernetes/pkg/a", "k8s.io/kubernetes/pkg/c"},
		Files:        []string{"pkg/a/a.go"},
	}

	differences := compareExact(golden, current)
	want := []string{
		"packages gained k8s.io/kubernetes/pkg/c",
		"packages lost k8s.io/kubernetes/pkg/b",
	}
	if len(differences) != len(want) {
		t.Fatalf("differences = %v, want %v", differences, want)
	}
	for i, difference := range differences {
		if difference != want[i] {
			t.Errorf("difference %d = %q, want %q", i, difference, want[i])
		}
	}
	if same := compareExact(golden, golden); len(same) != 0 {
		t.Errorf("a golden differs from itself: %v", same)
	}
}

// TestCheckNotAncestorRefusesContainment covers the directory relationship the
// output tree may never have with the run's own state.
func TestCheckNotAncestorRefusesContainment(t *testing.T) {
	tests := []struct {
		name    string
		dir     string
		other   string
		refused bool
	}{
		{name: "same directory", dir: "/a/b", other: "/a/b", refused: true},
		{name: "contains it", dir: "/a", other: "/a/b", refused: true},
		{name: "inside it", dir: "/a/b", other: "/a"},
		{name: "sibling", dir: "/a/b", other: "/a/c"},
		{name: "shared prefix is not containment", dir: "/a/b", other: "/a/b-2"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := checkNotAncestor("output tree", filepath.FromSlash(test.dir),
				"work root", filepath.FromSlash(test.other))
			if refused := err != nil; refused != test.refused {
				t.Errorf("refused = %t, want %t: %v", refused, test.refused, err)
			}
		})
	}
}
