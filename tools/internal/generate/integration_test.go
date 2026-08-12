package generate_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/config"
	"github.com/enj/soapbox/tools/internal/generate"
	"github.com/enj/soapbox/tools/internal/gocli"
	"github.com/enj/soapbox/tools/internal/typeswap"
)

// endToEnd is one prepared generation over the fixture repository.
type endToEnd struct {
	upstream *upstream
	proxy    string
	roots    roots
	opts     generate.Options
}

// newEndToEnd assembles everything one complete generation needs.
//
// The pieces are real rather than stood in for: a Git repository on disk, a
// module proxy the go command actually resolves through, and the version index a
// previous run would have written. Nothing here replaces a subprocess or a file
// system with a stub, because every property these tests exist to check is a
// property of what those subprocesses do.
func newEndToEnd(ctx context.Context, t *testing.T, mutate func(cfg *config.Config)) *endToEnd {
	t.Helper()
	return newEndToEndWith(ctx, t, nil, mutate)
}

// newEndToEndWith prepares a generation over a fixture with some files replaced.
func newEndToEndWith(ctx context.Context, t *testing.T, overrides map[string]string, mutate func(cfg *config.Config)) *endToEnd {
	t.Helper()

	// The checksum database is switched off for the fixture proxy, which
	// publishes modules no database has ever seen. Inheriting the variable is
	// the one route the runner leaves open, because it deliberately owns every
	// exemption list itself.
	t.Setenv(goSumDBVariable, "off")

	up := newUpstreamWith(ctx, t, overrides)
	// The profile keeps naming the real upstream repository, because a profile
	// is only valid with a real one. Pointing the run at the fixture is what
	// SourceRemote is for, and the report records that an override happened
	// without recording its value.
	cfg := loadProfile(t, "")
	if mutate != nil {
		mutate(cfg)
	}
	dirs := newRoots(t, cfg)
	writeVersionIndex(ctx, t, dirs.store, up.commit)

	proxy := newProxy(t)
	opts := dirs.options(cfg, anonymousGit(t), fixtureGo(t, proxy))
	opts.SourceRemote = up.url()
	opts.Fetch = true
	opts.Materialize = true
	return &endToEnd{upstream: up, proxy: proxy, roots: dirs, opts: opts}
}

// relayout prepares a second generation over the same upstream commit with
// entirely different directories.
//
// It is what the determinism check needs. Two runs over two fixture repositories
// would differ in their source commit, and the generated evidence records that
// commit, so the trees would legitimately differ and the comparison would prove
// nothing. Holding the commit fixed and moving every directory is the comparison
// that isolates the property being claimed.
func (e *endToEnd) relayout(ctx context.Context, t *testing.T) *endToEnd {
	t.Helper()
	cfg := loadProfile(t, "")
	dirs := newRoots(t, cfg)
	writeVersionIndex(ctx, t, dirs.store, e.upstream.commit)

	opts := dirs.options(cfg, anonymousGit(t), fixtureGo(t, e.proxy))
	opts.SourceRemote = e.upstream.url()
	opts.Fetch = true
	opts.Materialize = true
	return &endToEnd{upstream: e.upstream, proxy: e.proxy, roots: dirs, opts: opts}
}

// fixtureGo builds the Go runner every toolchain phase drives.
//
// Every location the go command keeps state in is the package's own: see
// TestMain for why the module cache is isolated for correctness and the build
// cache for reliability.
func fixtureGo(t *testing.T, proxy string) *gocli.Runner {
	t.Helper()
	home := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(filepath.Join(home, ".config", "go", "telemetry"), 0o750); err != nil {
		t.Fatalf("isolated home: %v", err)
	}
	// Telemetry counters are written asynchronously, so a go command that
	// enabled them could still be writing into the temporary home while the
	// test framework is removing it.
	if err := os.WriteFile(filepath.Join(home, ".config", "go", "telemetry", "mode"), []byte("off\n"), 0o600); err != nil {
		t.Fatalf("isolated home telemetry: %v", err)
	}

	isolation := []string{"HOME=" + home, "GOMODCACHE=" + moduleCache(t), "GOPATH=" + filepath.Join(home, "go")}
	// The build cache is carried over from the process rather than isolated,
	// which is what every other package in this repository that drives the go
	// command does. It is keyed by content, so it cannot serve one fixture's
	// compilation for another's, and a warm cache avoids the burst of concurrent
	// first writes that a cold one performs while the loader compiles the
	// standard library.
	if value, ok := os.LookupEnv("GOCACHE"); ok && value != "" {
		isolation = append(isolation, "GOCACHE="+value)
	}

	runner, err := gocli.New(t.Context(), gocli.Options{
		Dir:       t.TempDir(),
		Inherit:   []string{"PATH", goSumDBVariable},
		Isolation: isolation,
		Proxy:     "file://" + filepath.ToSlash(proxy),
	})
	if err != nil {
		t.Fatalf("go runner: %v", err)
	}
	return runner
}

// generateOnce runs the prepared generation and requires it to succeed.
func (e *endToEnd) generateOnce(ctx context.Context, t *testing.T) *generate.Result {
	t.Helper()
	result, err := generate.Generate(ctx, e.opts)
	if err != nil {
		if result != nil {
			t.Fatalf("generate: %v\n%s", err, result.Summary())
		}
		t.Fatalf("generate: %v", err)
	}
	return result
}

// TestGenerateProvesTheSubstitutionAgainstUpstreamIdentities is the type policy
// gate.
//
// The analysis has to run over the upstream package identities the profile
// names, not over the relocated ones. A profile pairs
// k8s.io/kubernetes/pkg/apis/rbac with a published API package, and proving
// something about a path this engine invented would prove something about the
// engine's own rewriting rather than about the code being extracted.
func TestGenerateProvesTheSubstitutionAgainstUpstreamIdentities(t *testing.T) {
	ctx := t.Context()
	e := newEndToEnd(ctx, t, nil)
	result := e.generateOnce(ctx, t)

	if len(result.Report.Types.Pairs) != 1 {
		t.Fatalf("report: %d pairs analysed, want 1", len(result.Report.Types.Pairs))
	}
	pair := result.Report.Types.Pairs[0]

	// The pairing is spelled in upstream terms on both sides.
	if pair.Internal != "k8s.io/kubernetes/pkg/apis/rbac" {
		t.Errorf("pair internal = %s, want the upstream path the profile names", pair.Internal)
	}
	if pair.External != stagingAPI+"/rbac/v1" {
		t.Errorf("pair external = %s, want %s/rbac/v1", pair.External, stagingAPI)
	}
	if pair.Action != string(typeswap.ActionPruneInternal) {
		t.Errorf("pair action = %s, want %s; blockers:\n  %s",
			pair.Action, typeswap.ActionPruneInternal, joinLines(pair.Blockers))
	}

	// The two analyses that need upstream evidence are the ones that prove the
	// analysis saw real generated conversions rather than merely matching names.
	for _, name := range []string{"markers", "conversions", "fieldIdentity"} {
		analysis, ok := pair.Analysis(name)
		if !ok {
			t.Errorf("analysis %s was not run", name)
			continue
		}
		if !analysis.Passed {
			t.Errorf("analysis %s did not pass: %s", name, joinLines(analysis.Blockers))
		}
	}

	// A behaviour change the analysis found has to reach the published NOTICE,
	// because a change nobody wrote down is a change nobody can test.
	if len(result.Report.Provenance.BehaviorChanges) == 0 {
		t.Error("report: the type policy found no documented behaviour change, so the disclosure gate proved nothing")
	}
	notice := fileContents(t, result, "NOTICE")
	for _, change := range result.Report.Provenance.BehaviorChanges {
		if !strings.Contains(notice, change.Summary) {
			t.Errorf("NOTICE does not state the behaviour change %q", change.Summary)
		}
	}
}

// TestGenerateRefusesAnUnprovableSubstitution is the other half of the same
// gate.
//
// Removing the generated conversions removes the mechanical evidence that the
// two declarations match. The profile still prunes the internal package, so the
// run has to refuse: pruning on an unproved equivalence publishes a module that
// claims something nobody demonstrated.
func TestGenerateRefusesAnUnprovableSubstitution(t *testing.T) {
	ctx := t.Context()
	e := newEndToEndWith(ctx, t, map[string]string{
		"pkg/apis/rbac/v1/zz_generated.conversion.go": conversionsWithoutFunctions,
	}, nil)

	result, err := generateFailure(ctx, t, e.opts)
	if !strings.Contains(err.Error(), "is blocked") {
		t.Errorf("generate: error = %v, want a blocked substitution", err)
	}
	if _, statErr := os.Stat(e.roots.output); !os.IsNotExist(statErr) {
		t.Errorf("output tree exists after a blocked substitution: %v", statErr)
	}
	if result == nil || result.Report.Failure == nil {
		t.Fatalf("generate: no reviewable report for a blocked substitution: %v", err)
	}
	if result.Report.Failure.Stage != "types" {
		t.Errorf("failure stage = %s, want types", result.Report.Failure.Stage)
	}
	// The evidence that was gathered is still in the report, because a refusal
	// is when it is most worth reading.
	if len(result.Report.Types.Pairs) == 0 {
		t.Error("report: the type analysis was discarded by the refusal")
	}
}

// TestGenerateRefusesCopyProposalsAsUnsupported proves a profile that proposes a
// staging copy is refused rather than silently generating a module whose
// provenance would describe files the tree does not contain.
//
// The dependency decision itself is implemented and tested elsewhere. What is
// missing is everything a copy needs afterwards: reading the staging package out
// of the upstream tree, relocating it beside the extracted code, collecting the
// grant that governs it, and recording all of it in the root evidence. Refusing
// is the only answer that does not publish a claim nothing backs.
func TestGenerateRefusesCopyProposalsAsUnsupported(t *testing.T) {
	ctx := t.Context()
	e := newEndToEnd(ctx, t, func(cfg *config.Config) {
		cfg.Dependencies.Policy = config.DependencyPolicyCopyApproved
		cfg.Dependencies.CopyPackages = []string{"staging/src/" + stagingAPIServer + "/pkg/authorization/authorizer"}
		cfg.Dependencies.Gates.Cost.MaxCopiedPackages = 1
		cfg.Dependencies.Gates.Cost.MaxReleasesPerMinor = 1
		cfg.Dependencies.Gates.Cost.MinPackagesRemoved = 1
	})

	result, err := generateFailure(ctx, t, e.opts)
	if !errors.Is(err, generate.ErrUnsupported) {
		t.Errorf("generate: error = %v, want it to be ErrUnsupported", err)
	}
	if _, statErr := os.Stat(e.roots.output); !os.IsNotExist(statErr) {
		t.Errorf("output tree exists after an unsupported copy proposal: %v", statErr)
	}
	if result == nil || result.Report.Failure == nil {
		t.Fatalf("generate: no reviewable report for a copy proposal: %v", err)
	}
	if result.Report.Failure.Stage != "dependencies" {
		t.Errorf("failure stage = %s, want dependencies", result.Report.Failure.Stage)
	}
	// The classification is what a caller acts on: this is neither a bad profile
	// nor a broken engine.
	if !result.Report.Failure.Unsupported {
		t.Errorf("failure = %+v, want it classified as unsupported", result.Report.Failure)
	}
}

// TestGenerateProducesCompleteModule is the end-to-end proof.
//
// It asserts what a consumer of the generated module would find: the relocated
// upstream code, a go.mod that resolves, the curated facade, and the root
// evidence. Each of those is produced by a different phase, so the test is also
// the proof that the phases compose rather than merely each working alone.
func TestGenerateProducesCompleteModule(t *testing.T) {
	ctx := t.Context()
	e := newEndToEnd(ctx, t, nil)
	result := e.generateOnce(ctx, t)

	if result.Report.Failure != nil {
		t.Fatalf("generate: refused at %s: %s", result.Report.Failure.Stage, result.Report.Failure.Message)
	}

	// The tree a consumer sees.
	want := []string{
		"LICENSE",
		"NOTICE",
		"README.md",
		"authorizer.go",
		"doc.go",
		"go.mod",
		"go.sum",
		"zz_generated_assertions.go",
		"internal/kk/plugin/pkg/auth/authorizer/rbac/rbac.go",
		"internal/kk/pkg/registry/rbac/validation/rule.go",
		"internal/kk/pkg/apis/rbac/v1/doc.go",
		"internal/kk/pkg/apis/rbac/v1/evaluation_helpers.go",
	}
	got := treePaths(result)
	if missing, ok := containsAll(got, want); !ok {
		t.Errorf("generated tree is missing %s, got:\n  %s", missing, joinLines(got))
	}

	// Pruning removed the registration file, so the unversioned internal API
	// package must not have reached the module at all.
	for _, path := range got {
		if strings.Contains(path, "pkg/apis/rbac/v1/register.go") {
			t.Errorf("generated tree contains the pruned file %s", path)
		}
		if strings.HasSuffix(path, "internal/kk/pkg/apis/rbac/types.go") {
			t.Errorf("generated tree contains the denied internal API package: %s", path)
		}
	}

	// The tree was actually written, and what was written is what was reported.
	if !result.Report.Output.Materialized {
		t.Error("report: Materialized = false, want the tree to have been written")
	}
	written := walkTree(t, e.roots.output)
	if !slices.Equal(written, got) {
		t.Errorf("written tree differs from the reported one:\n written:\n  %s\n reported:\n  %s", joinLines(written), joinLines(got))
	}
}

// TestGeneratePinsStagingAndTidiesModule proves the module metadata is the
// toolchain's answer rather than the engine's guess.
func TestGeneratePinsStagingAndTidiesModule(t *testing.T) {
	ctx := t.Context()
	e := newEndToEnd(ctx, t, nil)
	result := e.generateOnce(ctx, t)
	if result.Report.Source.ReleaseTag != fixtureStagingTag {
		t.Errorf("release tag = %q, want %q", result.Report.Source.ReleaseTag, fixtureStagingTag)
	}

	// Every required staging module was pinned, at the version the release
	// policy maps the upstream tag onto.
	pinned := make([]string, 0, len(result.Report.Staging.Modules))
	for _, module := range result.Report.Staging.Modules {
		pinned = append(pinned, module.Path)
		if module.Version != fixtureStagingTag {
			t.Errorf("staging pin %s: version = %s, want %s", module.Path, module.Version, fixtureStagingTag)
		}
		if module.Commit != stagingCommits[module.Path] {
			t.Errorf("staging pin %s: commit = %s, want %s", module.Path, module.Commit, stagingCommits[module.Path])
		}
		if module.Directory != "staging/src/"+module.Path {
			t.Errorf("staging pin %s: directory = %s, want staging/src/%s", module.Path, module.Directory, module.Path)
		}
	}
	if !slices.Equal(pinned, stagingPaths()) {
		t.Errorf("staging pins = %v, want %v", pinned, stagingPaths())
	}
	if !result.Report.Staging.Cached {
		t.Error("report: staging pins were resolved rather than read from the index")
	}

	// The published go.mod requires the pinned versions and carries no
	// replacement, which is what makes it resolvable by a consumer.
	goMod := fileContents(t, result, "go.mod")
	if strings.Contains(goMod, "replace ") {
		t.Errorf("generated go.mod carries a replacement:\n%s", goMod)
	}
	if !strings.Contains(goMod, "module "+e.opts.Config.Destination.Module) {
		t.Errorf("generated go.mod does not declare the destination module:\n%s", goMod)
	}
	for _, modulePath := range stagingPaths() {
		if !strings.Contains(goMod, modulePath+" "+fixtureStagingTag) {
			t.Errorf("generated go.mod does not require %s %s:\n%s", modulePath, fixtureStagingTag, goMod)
		}
	}
	// go.sum exists because the module requires code outside the standard
	// library, and a consumer cannot build without it.
	if sum := fileContents(t, result, "go.sum"); !strings.Contains(sum, stagingAPI) {
		t.Errorf("generated go.sum does not cover %s:\n%s", stagingAPI, sum)
	}
}

// TestGenerateProvesPruningKeptThePublicAPI covers the gate the whole pre-prune
// pass exists to make possible.
func TestGenerateProvesPruningKeptThePublicAPI(t *testing.T) {
	ctx := t.Context()
	e := newEndToEnd(ctx, t, nil)
	result := e.generateOnce(ctx, t)

	facade := result.Report.Facade
	if facade.PreManifestHash != facade.PostManifestHash {
		t.Errorf("facade manifests differ across the prune: pre = %s, post = %s", facade.PreManifestHash, facade.PostManifestHash)
	}
	if len(facade.Differences) != 0 {
		t.Errorf("facade differences = %v, want none", facade.Differences)
	}

	// The published names are the profile's, and the README states the same
	// ones, so the two cannot drift apart.
	want := []string{
		"AuthorizationRuleResolver",
		"DefaultRuleResolver",
		"New",
		"NewDefaultRuleResolver",
		"RBACAuthorizer",
		"RoleGetter",
	}
	if !slices.Equal(facade.Entries, want) {
		t.Errorf("facade entries = %v, want %v", facade.Entries, want)
	}
	if !slices.Equal(result.Report.Provenance.PublicAPI, want) {
		t.Errorf("README public API = %v, want %v", result.Report.Provenance.PublicAPI, want)
	}
}

// TestGenerateReportsHonestZeroCandidates proves the dependency phase records a
// decision rather than skipping.
//
// An empty candidate set and a phase that never ran encode identically unless
// the decision itself is written down, and the difference matters: one says this
// profile copies nothing, the other says nobody asked.
func TestGenerateReportsHonestZeroCandidates(t *testing.T) {
	ctx := t.Context()
	e := newEndToEnd(ctx, t, nil)
	result := e.generateOnce(ctx, t)

	deps := result.Report.Dependencies
	if deps.Policy != "external" {
		t.Errorf("dependency policy = %q, want external", deps.Policy)
	}
	if len(deps.Copy) != 0 {
		t.Errorf("dependency copy = %v, want none", deps.Copy)
	}
	if deps.Totals.Candidates != 0 || deps.Totals.Copied != 0 {
		t.Errorf("dependency totals = %+v, want zero candidates and zero copies", deps.Totals)
	}
	if deps.Candidates == nil {
		t.Error("dependency candidates = nil, want an empty list so the encoding is stable")
	}
}

// TestGenerateCrossChecksProvenance proves the root evidence accounts for the
// tree it describes.
func TestGenerateCrossChecksProvenance(t *testing.T) {
	ctx := t.Context()
	e := newEndToEnd(ctx, t, nil)
	result := e.generateOnce(ctx, t)

	prov := result.Report.Provenance
	if prov.LicenseID != "Apache-2.0" {
		t.Errorf("licence = %q, want Apache-2.0", prov.LicenseID)
	}
	if !prov.UpstreamNotice {
		t.Error("report: the upstream NOTICE was not embedded, but the fixture commit has one")
	}

	// The licence travels byte for byte, and the NOTICE quotes the upstream one
	// rather than merging with it.
	if got := fileContents(t, result, "LICENSE"); got != fixtureLicense {
		t.Errorf("generated LICENSE is not the upstream text:\n%s", got)
	}
	notice := fileContents(t, result, "NOTICE")
	if !strings.Contains(notice, "The Kubernetes Authors") {
		t.Errorf("generated NOTICE does not embed the upstream one:\n%s", notice)
	}
	// Every relocated package carries its own record beside it, and the root
	// NOTICE is what ties them together.
	for _, path := range treePaths(result) {
		if strings.HasSuffix(path, "/SOAPBOX_PROVENANCE.txt") {
			return
		}
	}
	t.Error("generated tree carries no per-package provenance record")
}

// TestGenerateIsDeterministicAcrossRoots is the property the report exists for.
//
// Two runs over one source commit with different directory layouts have to
// produce the same module and the same report. It is what makes a report
// comparable in CI and what makes an unexpected difference a real signal rather
// than noise from the machine that produced it.
func TestGenerateIsDeterministicAcrossRoots(t *testing.T) {
	ctx := t.Context()
	first := newEndToEnd(ctx, t, nil)
	firstResult := first.generateOnce(ctx, t)

	// The second run reads the same upstream commit through an entirely
	// different set of directories.
	second := first.relayout(ctx, t)
	secondResult := second.generateOnce(ctx, t)

	if first.roots.output == second.roots.output || first.roots.cache == second.roots.cache {
		t.Fatal("fixture: both runs used the same directories, so this proves nothing")
	}
	if firstResult.Report.Source.Commit != secondResult.Report.Source.Commit {
		t.Fatalf("fixture: the two runs read different commits, %s and %s",
			firstResult.Report.Source.Commit, secondResult.Report.Source.Commit)
	}

	if firstResult.Report.Output.ManifestHash != secondResult.Report.Output.ManifestHash {
		t.Errorf("manifest hashes differ across layouts: %s and %s",
			firstResult.Report.Output.ManifestHash, secondResult.Report.Output.ManifestHash)
	}
	if !slices.Equal(treePaths(firstResult), treePaths(secondResult)) {
		t.Errorf("trees differ across layouts:\n  %s\nand\n  %s",
			joinLines(treePaths(firstResult)), joinLines(treePaths(secondResult)))
	}

	// The reports have to agree byte for byte, which is the property that makes
	// a difference between two runs a real signal rather than noise from the
	// machine that produced it.
	firstJSON, err := firstResult.Report.JSON()
	if err != nil {
		t.Fatalf("report JSON: %v", err)
	}
	secondJSON, err := secondResult.Report.JSON()
	if err != nil {
		t.Fatalf("report JSON: %v", err)
	}
	if string(firstJSON) != string(secondJSON) {
		t.Errorf("reports differ across layouts:\n%s\nand\n%s", firstJSON, secondJSON)
	}

	// The report itself must carry no absolute path from either machine.
	for _, result := range []*generate.Result{firstResult, secondResult} {
		data, err := result.Report.JSON()
		if err != nil {
			t.Fatalf("report JSON: %v", err)
		}
		for _, dir := range []string{
			result.Paths.Cache, result.Paths.Work, result.Paths.Output,
			result.Paths.Store, result.Paths.PreModule, result.Paths.PostModule,
		} {
			if dir != "" && strings.Contains(string(data), dir) {
				t.Errorf("report names the absolute directory %s", dir)
			}
		}
		// The source remote override is a path on this machine, so only the
		// fact of an override may be recorded.
		if strings.Contains(string(data), first.upstream.repo.Dir) {
			t.Error("report names the source remote override")
		}
		if !result.Report.Source.RemoteOverridden {
			t.Error("report: RemoteOverridden = false, want the override recorded as a fact")
		}
	}
}

// TestGenerateLeavesNoOutputWhenAGateRefuses is the fail-closed property.
//
// A refused generation must leave nothing behind. A tree written before the last
// gate ran is a tree an operator can use, and the whole argument for gating is
// that an unacceptable module never becomes available.
func TestGenerateLeavesNoOutputWhenAGateRefuses(t *testing.T) {
	ctx := t.Context()
	// Removing a file the profile requires to survive pruning is a refusal the
	// extraction phase reaches, which is early enough that no later phase can be
	// what prevented the write.
	e := newEndToEnd(ctx, t, func(cfg *config.Config) {
		cfg.Prune.Required = append(cfg.Prune.Required, "pkg/registry/rbac/validation/missing.go")
	})

	result, err := generateFailure(ctx, t, e.opts)
	if _, statErr := os.Stat(e.roots.output); !os.IsNotExist(statErr) {
		t.Errorf("output tree exists after a refusal: %v", statErr)
	}
	// The refusal is still reviewable from an artifact rather than from stderr.
	if result == nil {
		t.Fatalf("generate: got no result for a measured refusal: %v", err)
	}
	if result.Report.Failure == nil {
		t.Fatal("generate: the report records no failure")
	}
	if !result.Report.Failure.Policy {
		t.Errorf("failure = %+v, want it classified as a policy refusal", result.Report.Failure)
	}
	if result.Report.Output.Materialized {
		t.Error("report: Materialized = true after a refusal")
	}
}

// TestGenerateStrictRefusesNoticesBeforeWriting proves strict mode gates the
// output rather than annotating it.
func TestGenerateStrictRefusesNoticesBeforeWriting(t *testing.T) {
	ctx := t.Context()
	e := newEndToEnd(ctx, t, nil)
	e.opts.Strict = true

	result, err := generateFailure(ctx, t, e.opts)
	if _, statErr := os.Stat(e.roots.output); !os.IsNotExist(statErr) {
		t.Errorf("output tree exists after a strict refusal: %v", statErr)
	}
	if result == nil {
		t.Fatalf("generate: got no result for a strict refusal: %v", err)
	}
	if !strings.Contains(err.Error(), "closure golden") && !strings.Contains(err.Error(), "strict") {
		t.Errorf("generate: error = %v, want it to name the notice that refused", err)
	}
}

// treePaths renders the generated module's file paths, sorted.
func treePaths(result *generate.Result) []string {
	paths := make([]string, 0, len(result.Files.Files))
	for _, file := range result.Files.Files {
		paths = append(paths, file.Path)
	}
	slices.Sort(paths)
	return paths
}

// fileContents reads one generated file out of the composed set.
func fileContents(t *testing.T, result *generate.Result, path string) string {
	t.Helper()
	file, ok := result.Files.Lookup(path)
	if !ok {
		t.Fatalf("generated tree has no %s, it has:\n  %s", path, joinLines(treePaths(result)))
	}
	return string(file.Contents)
}

// walkTree lists the files actually written below root, module relative and
// sorted.
func walkTree(t *testing.T, root string) []string {
	t.Helper()
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		paths = append(paths, relativeTo(root, path))
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	slices.Sort(paths)
	return paths
}
