package extract_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/enj/soapbox/tools/internal/closure"
	"github.com/enj/soapbox/tools/internal/extract"
	"github.com/enj/soapbox/tools/internal/patchset"
)

// TestPlanOfflineReachesNoRemote proves an offline plan performs no network
// I/O, including the fetch nobody asks for.
//
// Refusing to clone and refusing to fetch is not enough. The cache is blobless,
// so checking out a commit whose blobs never arrived makes git download them
// from the promisor remote on its own, and a run that did that would be reading
// upstream while claiming to be offline.
//
// The runner is the ordinary anonymous one every other test in this file uses,
// which is the point: the refusal has to be a property of the plan rather than
// something a caller has to remember to build into its runner. The remote is
// moved out of the way, so a checkout that reached for it would fail naming the
// repository instead of the object.
func TestPlanOfflineReachesNoRemote(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)

	// A first online run over one package, so the cache holds every commit and
	// tree and the blobs of that package alone.
	narrow := planOptions(ctx, t, up, narrowProfile)
	mustPlan(ctx, t, narrow)

	offline := planOptions(ctx, t, up, fixtureProfile)
	offline.CacheRoot = narrow.CacheRoot
	offline.Fetch = false
	offline.Offline = true

	moved := up.repo.Dir + "-moved"
	if err := os.Rename(up.repo.Dir, moved); err != nil {
		t.Fatalf("move the upstream out of the way: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Rename(moved, up.repo.Dir); err != nil {
			t.Errorf("restore the upstream: %v", err)
		}
	})

	_, err := extract.Plan(ctx, offline)
	if err == nil {
		t.Fatal("the offline plan succeeded, so it either found every blob locally or fetched")
	}
	// A transfer that was attempted and failed says so in git's own words.
	// Neither spelling may appear, because neither can happen without one
	// having been started.
	for _, transport := range []string{
		"does not appear to be a git repository",
		"Could not read from remote repository",
		"remote helper",
		moved,
	} {
		if strings.Contains(err.Error(), transport) {
			t.Errorf("the offline plan reached for the remote (%q): %v", transport, err)
		}
	}
	if !strings.Contains(err.Error(), "lazy fetching disabled") {
		t.Errorf("the failure does not show the refusal that produced it: %v", err)
	}
}

// TestPlanOnlineStillFetchesBlobsLazily proves the guard is scoped to offline
// runs.
//
// Lazily fetching the blobs of the selected paths is exactly how a blobless
// cache is meant to work, and a guard that applied to every run would turn the
// ordinary case into a failure.
func TestPlanOnlineStillFetchesBlobsLazily(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)

	// The same shape as the offline case, without the offline flag: a cache
	// primed over one package, then a wider closure whose blobs are missing.
	narrow := planOptions(ctx, t, up, narrowProfile)
	mustPlan(ctx, t, narrow)

	wide := planOptions(ctx, t, up, fixtureProfile)
	wide.CacheRoot = narrow.CacheRoot
	wide.Fetch = false
	result := mustPlan(ctx, t, wide)
	if len(result.Files.Files) == 0 {
		t.Error("the online run produced no files")
	}
}

// TestPlanReportsPolicyFailures proves a refused plan still produces the report
// the refusal has to be reviewed from.
//
// A finding that reached only stderr is a finding CI cannot attach, cannot
// diff, and cannot key an issue on. The failure section is what makes the
// refusal machine readable, and the rest of the report is what makes it
// reproducible.
func TestPlanReportsPolicyFailures(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)

	// A limit the fixture closure passes is the shortest route to a policy
	// failure that happens after the plan measured something worth reporting.
	profile := replace("    maxPackages: 8\n", "    maxPackages: 2\n")(fixtureProfile)
	result, err := planFailure(ctx, t, planOptions(ctx, t, up, profile))
	mustPolicy(t, err, "plan closure")

	if result == nil {
		t.Fatal("a policy failure must still report what the plan found")
	}
	failure := result.Report.Failure
	if failure == nil {
		t.Fatal("the report carries no failure section")
	}
	if failure.Stage != "plan closure" {
		t.Errorf("failure stage = %q, want %q", failure.Stage, "plan closure")
	}
	if !strings.Contains(failure.Message, "maxPackages") {
		t.Errorf("the failure message does not name the limit that was passed: %q", failure.Message)
	}
	if failure.Patch != nil {
		t.Errorf("a closure failure must carry no patch conflict: %+v", failure.Patch)
	}

	// The partial report is still a report: normalized, encodable, and free of
	// anything about the machine it ran on.
	assertReportIsClean(t, result)
}

// TestPlanReportsPatchConflicts proves the structured conflict record.
//
// Naming the patch is not enough. A maintainer's next step is to rewrite the
// diff against the new upstream, and the conflicted paths and the captured diff
// with its markers are what they rewrite it from.
func TestPlanReportsPatchConflicts(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)
	profile := withPatch(fixtureProfile, "patches/stale.patch")
	opts := planOptions(ctx, t, up, profile)
	writePatch(t, opts.ProfileDir, "patches/stale.patch", conflictingPatch)

	result, err := planFailure(ctx, t, opts)
	mustPolicy(t, err, "plan patch")
	if result == nil {
		t.Fatal("a patch conflict must still report what the plan found")
	}
	failure := result.Report.Failure
	if failure == nil || failure.Patch == nil {
		t.Fatalf("the report carries no patch conflict: %+v", failure)
	}

	patch := failure.Patch
	if patch.PatchID != "patches/stale.patch" {
		t.Errorf("patch id = %q", patch.PatchID)
	}
	if patch.Stage != string(patchset.StageApply) {
		t.Errorf("patch stage = %q, want %q", patch.Stage, patchset.StageApply)
	}
	if patch.PatchCount != 1 || patch.PatchIndex != 0 {
		t.Errorf("patch position = %d of %d, want 1 of 1", patch.PatchIndex+1, patch.PatchCount)
	}
	if patch.SourceSHA != up.commit {
		t.Errorf("patch source sha = %q, want %q", patch.SourceSHA, up.commit)
	}
	if patch.SourceRef == "" {
		t.Error("the conflict names no source ref")
	}
	assertReportIsClean(t, result)
}

// assertReportIsClean requires a report that encodes, normalizes every list, and
// names nothing about this machine.
func assertReportIsClean(t *testing.T, result *extract.Result) {
	t.Helper()
	encoded, err := result.Report.JSON()
	if err != nil {
		t.Fatalf("encode report: %v", err)
	}
	var decoded any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	assertNoNulls(t, "", decoded)

	for _, local := range []string{result.Paths.Cache, result.Paths.Work, result.Paths.Output} {
		if local != "" && strings.Contains(string(encoded), local) {
			t.Errorf("the report leaks the local path %q", local)
		}
	}
}

// nullableReportFields are the report members a plan is allowed not to have.
//
// Both are objects rather than lists: the failure section of a plan that
// succeeded, and the patch conflict of a failure that was not one. Absence is
// the meaning in both cases, which a list never has.
var nullableReportFields = []string{"failure", "patch"}

// assertNoNulls walks a decoded report and reports every null outside the two
// members that are allowed one.
func assertNoNulls(t *testing.T, at string, value any) {
	t.Helper()
	switch typed := value.(type) {
	case map[string]any:
		for _, key := range slices.Sorted(maps.Keys(typed)) {
			member := typed[key]
			if member == nil {
				if !slices.Contains(nullableReportFields, key) {
					t.Errorf("%s/%s encodes null, so a list was left unnormalized", at, key)
				}
				continue
			}
			assertNoNulls(t, at+"/"+key, member)
		}
	case []any:
		for i, member := range typed {
			assertNoNulls(t, fmt.Sprintf("%s[%d]", at, i), member)
		}
	}
}

// TestPlanNormalizesEveryList proves no list in a report encodes as null,
// including the ones nested inside an embed record.
//
// A consumer meets a nested list exactly the way it meets a top level one, and
// a report that spells "nothing" two ways will eventually be read by something
// that handles only one of them.
func TestPlanNormalizesEveryList(t *testing.T) {
	ctx := t.Context()
	up := embeddingUpstream(ctx, t)
	result := mustPlan(ctx, t, planOptions(ctx, t, up, embedProfile))

	if len(result.Report.Rewrite.Embeds) == 0 {
		t.Fatal("the embedding fixture produced no verified embeds, so the nested lists are untested")
	}
	for _, embed := range result.Report.Rewrite.Embeds {
		if embed.Patterns == nil {
			t.Errorf("%s:%d has a nil pattern list", embed.Path, embed.Line)
		}
		if embed.Matches == nil {
			t.Errorf("%s:%d has a nil match list", embed.Path, embed.Line)
		}
	}
	assertReportIsClean(t, result)

	// A successful report says so rather than carrying an empty failure object.
	if result.Report.Failure != nil {
		t.Errorf("a successful plan reports a failure: %+v", result.Report.Failure)
	}
}

// TestPlanLeavesEmbeddedGoFilesAlone proves the plan tells the module's source
// apart from the data it ships.
//
// A file named .go under a testdata directory is bytes a consumer reads through
// embed.FS. Parsing it would fail, relocating its imports would corrupt it, and
// inserting a modification notice into it would change what the module serves.
func TestPlanLeavesEmbeddedGoFilesAlone(t *testing.T) {
	ctx := t.Context()
	up := embeddingUpstream(ctx, t)
	result := mustPlan(ctx, t, planOptions(ctx, t, up, embedProfile))

	const asset = "internal/kk/pkg/only/testdata/asset.go"
	if got := contentsOf(t, result, asset); got != embeddedGoAsset {
		t.Errorf("the embedded asset was modified:\n got %q\nwant %q", got, embeddedGoAsset)
	}
	for _, file := range result.Report.Rewrite.Files {
		if file.Path == asset {
			t.Errorf("the embedded asset was recorded as transformed: %+v", file)
		}
	}
	if slices.Contains(result.Report.Rewrite.Unparsed, asset) {
		t.Error("the embedded asset was parsed, which it is not source for")
	}
	if slices.Contains(result.Report.Rewrite.Unformatted, asset) {
		t.Error("the embedded asset was formatted, which it is not source for")
	}

	// The relocated file also has to be reported as upstream data rather than
	// as a generated file, because nothing read its bytes to decide.
	entry := relocatedFile(t, result, asset)
	if entry.Generated {
		t.Error("the embedded asset was marked as carrying the generated marker")
	}
	if entry.Source != "pkg/only/testdata/asset.go" {
		t.Errorf("the embedded asset reports source %q", entry.Source)
	}
}

// TestPlanKeepsProvenanceOutOfEmbeds proves a generated record never lands where
// a published go:embed pattern would pick it up.
//
// The failure this prevents is silent: the module would build, and the record
// would be served to consumers as part of an embedded asset set that upstream
// defined.
func TestPlanKeepsProvenanceOutOfEmbeds(t *testing.T) {
	ctx := t.Context()
	up := embeddingUpstream(ctx, t)
	result := mustPlan(ctx, t, planOptions(ctx, t, up, embedProfile))

	// The record for the embedding package moved out of that package, and the
	// records for the packages with no broad pattern stayed beside their code.
	displaced := ""
	for _, provenance := range result.Report.Output.ProvenanceFiles {
		if strings.HasPrefix(provenance, "internal/kk/SOAPBOX_PROVENANCE/") {
			displaced = provenance
		}
		if provenance == "internal/kk/pkg/only/SOAPBOX_PROVENANCE.txt" {
			t.Error("the record stayed in a package whose go:embed pattern would capture it")
		}
	}
	if displaced == "" {
		t.Fatalf("no record was displaced, got %v", result.Report.Output.ProvenanceFiles)
	}
	if !slices.Contains(destinations(result), displaced) {
		t.Errorf("the displaced record %q is not in the tree", displaced)
	}

	// The move is stated rather than silent, and the record still describes the
	// package it was displaced from.
	if !hasNoticeAbout(result.Report.Notices, "pkg/only") {
		t.Errorf("the displacement produced no notice: %v", result.Report.Notices)
	}
	record := contentsOf(t, result, displaced)
	for _, want := range []string{"upstream package: pkg/only", "pkg/only/only.go"} {
		if !strings.Contains(record, want) {
			t.Errorf("the displaced record does not mention %q:\n%s", want, record)
		}
	}

	// No embed in the published tree resolves to a record this engine wrote.
	for _, embed := range result.Report.Rewrite.Embeds {
		for _, match := range embed.Matches {
			if slices.Contains(result.Report.Output.ProvenanceFiles, match) {
				t.Errorf("%s:%d embeds the generated record %s", embed.Path, embed.Line, match)
			}
		}
	}
	// The broad pattern still resolves to the upstream file it was written for.
	if !embedMatches(result, "internal/kk/pkg/only/notes.txt") {
		t.Error("displacing the record broke the pattern that matched the upstream note")
	}
}

// TestPlanAttributesAssetsToTheirPackage proves an asset below a package belongs
// to that package rather than to a package invented for its directory.
func TestPlanAttributesAssetsToTheirPackage(t *testing.T) {
	ctx := t.Context()
	up := embeddingUpstream(ctx, t)
	result := mustPlan(ctx, t, planOptions(ctx, t, up, embedProfile))

	// One record per real closure package and no more, so the testdata
	// directory the embed pulled in did not become a package of its own.
	if got, want := len(result.Report.Output.ProvenanceFiles), 1; got != want {
		t.Errorf("provenance records = %d, want %d: %v", got, want, result.Report.Output.ProvenanceFiles)
	}
	for _, provenance := range result.Report.Output.ProvenanceFiles {
		if strings.Contains(provenance, "/testdata/") {
			t.Errorf("a record was written into an asset directory: %s", provenance)
		}
	}
	if got, want := result.Report.Output.Packages, 1; got != want {
		t.Errorf("output packages = %d, want %d, which is the closure's own count", got, want)
	}

	// The asset is accounted for by the package that owns it.
	displaced := ""
	for _, provenance := range result.Report.Output.ProvenanceFiles {
		if strings.HasPrefix(provenance, "internal/kk/SOAPBOX_PROVENANCE/") {
			displaced = provenance
		}
	}
	if displaced == "" {
		t.Fatalf("expected the embedding package's record to be displaced, got %v", result.Report.Output.ProvenanceFiles)
	}
	if record := contentsOf(t, result, displaced); !strings.Contains(record, "testdata/asset.go") {
		t.Errorf("the owning package's record does not account for its asset:\n%s", record)
	}
}

// TestPlanGolden covers the three answers the pinned closure record can give.
func TestPlanGolden(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)

	t.Run("absent", func(t *testing.T) {
		opts := planOptions(ctx, t, up, fixtureProfile)
		result := mustPlan(ctx, t, opts)

		golden := result.Report.Closure.Golden
		if golden.Path != fixtureGolden {
			t.Errorf("golden path = %q, want %q", golden.Path, fixtureGolden)
		}
		if golden.Status != extract.GoldenAbsent {
			t.Errorf("golden status = %q, want %q", golden.Status, extract.GoldenAbsent)
		}
		if !hasNoticeAbout(result.Report.Notices, fixtureGolden) {
			t.Errorf("an absent golden produced no notice: %v", result.Report.Notices)
		}
		// Nothing was written. A gate that repairs itself gates nothing.
		if _, err := os.Stat(filepath.Join(opts.ProfileDir, filepath.FromSlash(fixtureGolden))); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the plan created the golden it was supposed to compare against: %v", err)
		}
	})

	t.Run("absent under strict", func(t *testing.T) {
		opts := planOptions(ctx, t, up, fixtureProfile)
		opts.Strict = true
		result, err := planFailure(ctx, t, opts)
		mustPolicy(t, err, "plan strict")
		if result == nil || result.Report.Closure.Golden.Status != extract.GoldenAbsent {
			t.Fatalf("the strict refusal does not report the absent golden: %+v", result)
		}
	})

	t.Run("match", func(t *testing.T) {
		opts := planOptions(ctx, t, up, fixtureProfile)
		first := mustPlan(ctx, t, opts)
		writeGolden(t, opts.ProfileDir, first.Report.Closure.Report)

		second := mustPlan(ctx, t, opts)
		if got := second.Report.Closure.Golden.Status; got != extract.GoldenMatch {
			t.Errorf("golden status = %q, want %q", got, extract.GoldenMatch)
		}
		if len(second.Report.Closure.Golden.Differences) > 0 {
			t.Errorf("a matching golden reported differences: %v", second.Report.Closure.Golden.Differences)
		}
		if hasNoticeAbout(second.Report.Notices, fixtureGolden) {
			t.Errorf("a matching golden produced a notice: %v", second.Report.Notices)
		}
	})

	t.Run("diff", func(t *testing.T) {
		opts := planOptions(ctx, t, up, fixtureProfile)
		first := mustPlan(ctx, t, opts)

		// A package swapped for another passes every limit the profile sets and
		// is exactly the change a review exists to catch.
		drifted := first.Report.Closure.Report
		drifted.Exact.Packages = []string{"k8s.io/kubernetes/pkg/apis/rbac/v1", "k8s.io/kubernetes/pkg/imaginary"}
		writeGolden(t, opts.ProfileDir, drifted)

		result, err := planFailure(ctx, t, opts)
		mustPolicy(t, err, "plan golden")
		if result == nil {
			t.Fatal("a golden mismatch must still report what the plan found")
		}
		golden := result.Report.Closure.Golden
		if golden.Status != extract.GoldenDiff {
			t.Errorf("golden status = %q, want %q", golden.Status, extract.GoldenDiff)
		}
		// The named differences are what turn "they differ" into a review.
		joined := strings.Join(golden.Differences, "\n")
		for _, want := range []string{"packages lost k8s.io/kubernetes/pkg/imaginary", "packages gained"} {
			if !strings.Contains(joined, want) {
				t.Errorf("the differences do not mention %q:\n%s", want, joined)
			}
		}
		assertReportIsClean(t, result)
	})

	t.Run("malformed", func(t *testing.T) {
		opts := planOptions(ctx, t, up, fixtureProfile)
		writeGoldenBytes(t, opts.ProfileDir, []byte("{not json"))

		_, err := planFailure(ctx, t, opts)
		// A golden the maintainer meant to be authoritative and that cannot be
		// read is a broken gate rather than a finding about the closure.
		var policy *extract.PolicyError
		if errors.As(err, &policy) {
			t.Fatalf("a malformed golden must not be reported as a closure finding: %v", err)
		}
		if !strings.Contains(err.Error(), fixtureGolden) {
			t.Errorf("the failure does not name the golden: %v", err)
		}
	})
}

// TestPlanRefusesAnotherToolchain proves the plan does not claim gofmt
// cleanliness under a toolchain the profile does not pin.
func TestPlanRefusesAnotherToolchain(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)

	pinned := "  toolchain: " + runtime.Version() + "\n"
	if !strings.Contains(fixtureProfile, pinned) {
		t.Skipf("the fixture pins a toolchain other than the running %s", runtime.Version())
	}
	profile := replace(pinned, "  toolchain: go1.25.0\n")(fixtureProfile)

	_, err := planFailure(ctx, t, planOptions(ctx, t, up, profile))
	mustPolicy(t, err, "plan toolchain")
	if !strings.Contains(err.Error(), "go1.25.0") || !strings.Contains(err.Error(), runtime.Version()) {
		t.Errorf("the refusal does not name both toolchains: %v", err)
	}
}

// TestPlanStrictWritesNothing proves -strict refuses before any side effect.
//
// The refusal has to be immediately retryable, which it is not if the run left a
// tree at a destination the next run requires to be absent.
func TestPlanStrictWritesNothing(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)

	opts := planOptions(ctx, t, up, fixtureProfile)
	opts.Strict = true
	opts.Materialize = true

	result, err := planFailure(ctx, t, opts)
	mustPolicy(t, err, "plan strict")
	if result == nil {
		t.Fatal("a strict refusal must still report what it found")
	}
	if result.Report.Output.Materialized {
		t.Error("the report claims a tree the refusal was supposed to prevent")
	}
	if _, err := os.Stat(opts.OutputRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the strict refusal wrote the output tree: %v", err)
	}

	// Immediately retryable: the same directories, with the notices accepted,
	// produce the tree.
	opts.Strict = false
	retry := mustPlan(ctx, t, opts)
	if !retry.Report.Output.Materialized {
		t.Error("the retry did not write the tree")
	}
	assertEqual(t, "written tree", treePaths(t, opts.OutputRoot), wantDestinations)
}

// TestPlanWorktreesAreUniquePerRun proves two plans over one commit do not
// collide, and that keeping one leaves it inspectable.
func TestPlanWorktreesAreUniquePerRun(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)

	kept := planOptions(ctx, t, up, fixtureProfile)
	kept.KeepWorktree = true
	first := mustPlan(ctx, t, kept)
	if first.Paths.Worktree == "" {
		t.Fatal("-keep-worktree removed the tree anyway")
	}
	if len(worktreePaths(t, first.Paths.Worktree)) == 0 {
		t.Error("the kept work tree holds no files")
	}

	// A second plan over the same cache and the same commit gets its own tree
	// and does not disturb the one that was kept.
	second := mustPlan(ctx, t, kept)
	if second.Paths.Worktree == first.Paths.Worktree {
		t.Fatalf("both runs used the work tree %s", first.Paths.Worktree)
	}
	if len(worktreePaths(t, first.Paths.Worktree)) == 0 {
		t.Error("the second run emptied the first run's kept work tree")
	}
	if second.Report.Output.ManifestHash != first.Report.Output.ManifestHash {
		t.Error("the second run over the same commit produced a different module")
	}

	// A run without -keep-worktree removes only its own tree.
	transient := planOptions(ctx, t, up, fixtureProfile)
	transient.CacheRoot = kept.CacheRoot
	transient.WorkRoot = kept.WorkRoot
	if _, err := extract.Plan(ctx, transient); err != nil {
		t.Fatalf("third plan: %v", err)
	}
	if len(worktreePaths(t, first.Paths.Worktree)) == 0 {
		t.Error("a run without -keep-worktree removed a kept work tree that was not its own")
	}
}

// TestPlanConcurrentRunsShareACache proves two plans can run at once against one
// cache without either one taking the other's work tree.
func TestPlanConcurrentRunsShareACache(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)

	// The cache is built once, so the concurrent runs exercise reuse rather
	// than two clones racing to create the same directory.
	shared := planOptions(ctx, t, up, fixtureProfile)
	first := mustPlan(ctx, t, shared)

	const runs = 4
	var wait sync.WaitGroup
	hashes := make([]string, runs)
	errs := make([]error, runs)
	for i := range runs {
		opts := planOptions(ctx, t, up, fixtureProfile)
		opts.CacheRoot = shared.CacheRoot
		opts.WorkRoot = shared.WorkRoot
		wait.Add(1)
		go func() {
			defer wait.Done()
			result, err := extract.Plan(ctx, opts)
			if err != nil {
				errs[i] = err
				return
			}
			hashes[i] = result.Report.Output.ManifestHash
		}()
	}
	wait.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent plan %d: %v", i, err)
		}
	}
	for i, hash := range hashes {
		if hash != first.Report.Output.ManifestHash {
			t.Errorf("concurrent plan %d produced %s, want %s", i, hash, first.Report.Output.ManifestHash)
		}
	}
}

// TestPlanRecordsSourceOverride proves the report says a mirror was used without
// naming it.
//
// A report produced against a mirror describes whatever that mirror held, which
// a reviewer has to know. The mirror's path is usually a directory on the
// machine that ran the plan, which the report may not carry.
func TestPlanRecordsSourceOverride(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)
	opts := planOptions(ctx, t, up, fixtureProfile)
	result := mustPlan(ctx, t, opts)

	if !result.Report.Source.RemoteOverridden {
		t.Error("the run read from a mirror and the report does not say so")
	}
	encoded, err := result.Report.JSON()
	if err != nil {
		t.Fatalf("encode report: %v", err)
	}
	if strings.Contains(string(encoded), up.repo.Dir) {
		t.Error("the report names the mirror it read from")
	}
}

// TestPlanRecordsReassertionPerPatch proves the reassertion names the patch that
// reintroduced a pruned file.
//
// Reasserting after every patch exists to catch exactly that, and a count cannot
// say which patch did it.
func TestPlanRecordsReassertionPerPatch(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)
	profile := withPatch(fixtureProfile, "patches/export.patch")
	opts := planOptions(ctx, t, up, profile)
	writePatch(t, opts.ProfileDir, "patches/export.patch", cleanPatch)

	result := mustPlan(ctx, t, opts)
	if got, want := len(result.Report.Patches.Reassert), 1; got != want {
		t.Fatalf("reassertions = %d, want %d", got, want)
	}
	entry := result.Report.Patches.Reassert[0]
	if entry.PatchID != "patches/export.patch" {
		t.Errorf("reassertion names patch %q", entry.PatchID)
	}
	// A patch that reintroduced nothing re-prunes nothing, and the list still
	// has to be present rather than null.
	if entry.Files == nil {
		t.Error("the reassertion carries a nil file list")
	}
	if len(entry.Files) != 0 {
		t.Errorf("the clean patch reintroduced %v", entry.Files)
	}
	if result.Report.Patches.Reasserted != len(result.Report.Patches.Reassert) {
		t.Errorf("reassertion count %d disagrees with the %d recorded",
			result.Report.Patches.Reasserted, len(result.Report.Patches.Reassert))
	}

	// The configured prune set is what the closure section reports, whatever
	// the reassertions did afterwards.
	assertEqual(t, "removed files", result.Report.Closure.RemovedFiles, []string{
		"pkg/apis/rbac/v1/zz_generated.conversion.go",
		"pkg/apis/rbac/v1/zz_generated.deepcopy.go",
		"pkg/registry/rbac/validation/internal_version_adapter.go",
	})
}

// TestPlanStripsMarkersAPatchDeleted proves the dangling rule reads the settled
// tree rather than the profile's prune list.
//
// A generated file a patch deleted leaves its marker exactly as dangling as one
// the profile pruned, and a module that kept the marker would carry an
// instruction nothing can carry out.
func TestPlanStripsMarkersAPatchDeleted(t *testing.T) {
	ctx := t.Context()
	// The retained package carries a deepcopy marker and the file its generator
	// writes. Nothing in the profile prunes that file.
	up := newUpstreamWith(ctx, t, map[string]string{
		"pkg/registry/rbac/validation/rule.go": markedRule,
	})
	profile := withPatch(fixtureProfile, "patches/drop-generated.patch")
	opts := planOptions(ctx, t, up, profile)
	writePatch(t, opts.ProfileDir, "patches/drop-generated.patch", deleteGeneratedPatch)

	result := mustPlan(ctx, t, opts)
	rule := contentsOf(t, result, "internal/kk/pkg/registry/rbac/validation/rule.go")
	if strings.Contains(rule, "+k8s:deepcopy-gen") {
		t.Errorf("a patch deleted the marker's output, so the marker must go:\n%s", rule)
	}
	if slices.Contains(destinations(result), "internal/kk/pkg/registry/rbac/validation/zz_generated.deepcopy.go") {
		t.Error("the patch was supposed to delete the generated file")
	}
}

// markedRule carries a generator marker whose output the fixture retains, so a
// patch is what removes it.
const markedRule = `// +k8s:deepcopy-gen=package

package validation

import rbacv1 "k8s.io/api/rbac/v1"

// RuleResolver resolves the rules a subject holds.
type RuleResolver interface {
	Rules() []rbacv1.PolicyRule
}
`

// deleteGeneratedPatch removes the file the deepcopy marker's generator writes.
const deleteGeneratedPatch = `--- a/pkg/registry/rbac/validation/zz_generated.deepcopy.go
+++ /dev/null
@@ -1,6 +0,0 @@
-// Code generated by deepcopy-gen. DO NOT EDIT.
-
-package validation
-
-// DeepCopyResolver copies nothing.
-func DeepCopyResolver() {}
`

// TestPlanNoticesUnknownGeneratorKeys proves the output table is treated as
// evidence rather than as a complete catalogue.
func TestPlanNoticesUnknownGeneratorKeys(t *testing.T) {
	ctx := t.Context()
	up := newUpstreamWith(ctx, t, map[string]string{
		"pkg/apis/rbac/v1/doc.go": unknownGeneratorDoc,
	})
	result := mustPlan(ctx, t, planOptions(ctx, t, up, fixtureProfile))

	if !hasNoticeAbout(result.Report.Notices, "k8s:openapi-gen") {
		t.Errorf("a marker outside the output table produced no notice: %v", result.Report.Notices)
	}
	// It is a notice rather than a removal: nothing proved the marker dangles.
	doc := contentsOf(t, result, "internal/kk/pkg/apis/rbac/v1/doc.go")
	if !strings.Contains(doc, "+k8s:openapi-gen=true") {
		t.Errorf("an unproved marker was removed:\n%s", doc)
	}
}

// unknownGeneratorDoc carries a marker whose generator writes into a central
// package rather than beside the types it describes.
const unknownGeneratorDoc = `// +k8s:openapi-gen=true
// +groupName=rbac.authorization.k8s.io

package v1
`

// TestPlanWidensBeyondAFixedBound proves the widening loop is bounded by the
// profile's own package ceiling rather than by a number the engine chose.
//
// A profile whose closure legitimately reaches more packages than a hardcoded
// bound would otherwise be refused for a reason its author could not find, and
// the refusal would name widening rather than the package count.
func TestPlanWidensBeyondAFixedBound(t *testing.T) {
	ctx := t.Context()
	up := newUpstreamWith(ctx, t, chainFiles(chainLength))
	result := mustPlan(ctx, t, planOptions(ctx, t, up, chainProfile))

	// One round per discovered package after the root, plus the round that
	// finally succeeded. The point of the case is that this is well past the
	// bound the engine used to hardcode.
	if got, want := result.Report.Closure.Rounds, chainLength; got != want {
		t.Errorf("closure rounds = %d, want %d", got, want)
	}
	if got, want := len(result.Report.Closure.Report.Exact.Packages), chainLength; got != want {
		t.Errorf("closure packages = %d, want %d", got, want)
	}
	if got, want := len(result.Report.Worktree.WidenedPackages), chainLength-1; got != want {
		t.Errorf("widened packages = %d, want %d", got, want)
	}
}

// TestPlanStillBoundsWidening proves the bound is a bound rather than a
// formality: a profile whose ceiling the closure passes is still refused.
func TestPlanStillBoundsWidening(t *testing.T) {
	ctx := t.Context()
	up := newUpstreamWith(ctx, t, chainFiles(chainLength))
	bounded := replace("    maxPackages: 200\n", "    maxPackages: 4\n")(chainProfile)

	_, err := planFailure(ctx, t, planOptions(ctx, t, up, bounded))
	mustPolicy(t, err, "plan closure")
	if !errors.Is(err, closure.ErrLimitExceeded) && !strings.Contains(err.Error(), "widening rounds") {
		t.Fatalf("error %v is neither a limit nor a widening bound", err)
	}
}

// TestPlanRefusesOutputInsideItsOwnState covers the directory relationships a
// plan cannot work under.
func TestPlanRefusesOutputInsideItsOwnState(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)

	tests := []struct {
		name string
		edit func(*extract.Options)
		want string
	}{
		{
			name: "output is the cache root",
			edit: func(o *extract.Options) { o.OutputRoot = o.CacheRoot },
			want: "source cache root",
		},
		{
			name: "output contains the cache root",
			edit: func(o *extract.Options) { o.OutputRoot = filepath.Dir(o.CacheRoot) },
			want: "source cache root",
		},
		{
			name: "output is the work root",
			edit: func(o *extract.Options) { o.OutputRoot = o.WorkRoot },
			want: "work root",
		},
		{
			name: "output contains the work root",
			edit: func(o *extract.Options) { o.WorkRoot = filepath.Join(o.OutputRoot, "work") },
			want: "work root",
		},
		{
			name: "output is the profile directory",
			edit: func(o *extract.Options) { o.OutputRoot = o.ProfileDir },
			want: "profile directory",
		},
		{
			name: "output inside the materialized source root",
			edit: func(o *extract.Options) { o.OutputRoot = filepath.Join(o.WorkRoot, "src", "tree") },
			want: "materialized source root",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			opts := planOptions(ctx, t, up, fixtureProfile)
			test.edit(&opts)
			_, err := extract.Plan(ctx, opts)
			if err == nil {
				t.Fatal("expected a refusal")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error %v does not name the %s", err, test.want)
			}
			// The refusal happens before anything is read, so nothing was made.
			if _, err := os.Stat(opts.CacheRoot); err == nil {
				t.Error("a refused option set still created the cache root")
			}
		})
	}

	// The documented defaults nest, and they have to keep working.
	t.Run("documented nesting", func(t *testing.T) {
		opts := planOptions(ctx, t, up, fixtureProfile)
		opts.WorkRoot = filepath.Join(opts.CacheRoot, "work")
		opts.OutputRoot = filepath.Join(opts.WorkRoot, "tree")
		mustPlan(ctx, t, opts)
	})
}

// TestPlanRefusesOutputReachedThroughALink proves containment is checked on
// directories rather than on the names used to reach them.
func TestPlanRefusesOutputReachedThroughALink(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)
	opts := planOptions(ctx, t, up, fixtureProfile)

	if err := os.MkdirAll(opts.WorkRoot, 0o750); err != nil {
		t.Fatalf("create the work root: %v", err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(opts.WorkRoot, link); err != nil {
		t.Skipf("this platform does not support symbolic links: %v", err)
	}
	// Lexically the output is somewhere else entirely; it is the same directory
	// as the materialized source root.
	opts.OutputRoot = filepath.Join(link, "src")

	_, err := extract.Plan(ctx, opts)
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "materialized source root") {
		t.Fatalf("error %v does not name the materialized source root", err)
	}
}

// TestPlanCancellationIsNeverAFinding proves an interrupted run reports the
// interruption rather than a verdict on the profile.
//
// The deadline is deliberately short and the stage it lands in is deliberately
// unspecified: the guarantee is about every stage, so the assertion is about
// the classification rather than about where the run got to.
func TestPlanCancellationIsNeverAFinding(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)
	profile := withPatch(fixtureProfile, "patches/export.patch")
	opts := planOptions(ctx, t, up, profile)
	writePatch(t, opts.ProfileDir, "patches/export.patch", cleanPatch)

	// The cache is built first so the deadline lands inside the pipeline rather
	// than inside the clone.
	mustPlan(ctx, t, opts)
	opts.Fetch = false

	for _, budget := range []time.Duration{time.Millisecond, 5 * time.Millisecond, 25 * time.Millisecond} {
		deadline, cancel := context.WithTimeout(ctx, budget)
		_, err := extract.Plan(deadline, opts)
		cancel()
		if err == nil {
			continue
		}
		if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
			t.Fatalf("a %v budget produced %v, which is not a cancellation", budget, err)
		}
		var policy *extract.PolicyError
		if errors.As(err, &policy) {
			t.Errorf("a %v budget reported the cancellation as the finding %v", budget, policy)
		}
	}
}

// TestPlanClassifiesCancelledPatchStages proves the one cancellation shape that
// arrives dressed as a finding.
//
// Patch application reports a context that ended as a conflict at the cancel
// stage, which is the same type a patch that no longer applies produces. Reading
// the type alone would turn every interrupted patch pass into a verdict on the
// patch.
func TestPlanClassifiesCancelledPatchStages(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)
	profile := withPatch(fixtureProfile, "patches/export.patch")
	opts := planOptions(ctx, t, up, profile)
	writePatch(t, opts.ProfileDir, "patches/export.patch", cleanPatch)
	mustPlan(ctx, t, opts)
	opts.Fetch = false

	// A cancelled run whose failure carries a conflict has to remain a
	// cancellation, whatever stage the conflict names.
	saw := false
	for _, budget := range []time.Duration{time.Millisecond, 3 * time.Millisecond, 10 * time.Millisecond, 30 * time.Millisecond} {
		deadline, cancel := context.WithTimeout(ctx, budget)
		_, err := extract.Plan(deadline, opts)
		cancel()

		var conflict *patchset.ConflictError
		if !errors.As(err, &conflict) {
			continue
		}
		saw = true
		if conflict.Stage != patchset.StageCancel {
			t.Errorf("a cancelled run produced a conflict at the %s stage", conflict.Stage)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("the cancelled conflict lost its cause: %v", err)
		}
		var policy *extract.PolicyError
		if errors.As(err, &policy) {
			t.Errorf("a cancelled patch pass was reported as the finding %v", policy)
		}
	}
	if !saw {
		t.Skip("no budget landed inside the patch pass on this machine")
	}
}

// relocatedFile reports one destination's entry in the relocation section.
func relocatedFile(t *testing.T, result *extract.Result, destination string) extract.RelocatedFile {
	t.Helper()
	for _, pkg := range result.Report.Relocation.Packages {
		for _, file := range pkg.Files {
			if file.Destination == destination {
				return file
			}
		}
	}
	t.Fatalf("%q is not in the relocation report", destination)
	return extract.RelocatedFile{}
}

// hasNoticeAbout reports whether any notice mentions a substring.
func hasNoticeAbout(notices []string, want string) bool {
	return slices.ContainsFunc(notices, func(notice string) bool {
		return strings.Contains(notice, want)
	})
}

// embedMatches reports whether any verified embed resolves to a destination.
func embedMatches(result *extract.Result, destination string) bool {
	for _, embed := range result.Report.Rewrite.Embeds {
		if slices.Contains(embed.Matches, destination) {
			return true
		}
	}
	return false
}
