package extract_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/config"
	"github.com/enj/soapbox/tools/internal/extract"
	"github.com/enj/soapbox/tools/internal/gitcli"
)

// sourceRemoteEnv names a filter capable mirror of kubernetes/kubernetes.
const sourceRemoteEnv = "SOAPBOX_SOURCE_REMOTE"

// integrationSkip explains how to produce a remote this test can use.
//
// The instructions are exact because three of the steps are easy to get wrong in
// ways that either fail confusingly or damage something.
//
// An ordinary clone is not a usable remote. The cache is a blobless partial
// clone, and a server that does not advertise object filtering hands back every
// blob, which source.Open refuses rather than silently accepting a full clone.
//
// The remote has to be a file:// URL rather than a plain path. Git optimises a
// path into its local transport, which copies or hard links the object database
// wholesale and never consults the filter, so a plain path produces exactly the
// complete clone the audit refuses.
//
// The setting goes on the mirror, never on the clone it was made from, because
// that clone is a working repository somebody else owns. The mirror costs almost
// no disk: a local clone hard links its objects, so it shares storage with the
// original until either one is repacked. It must therefore sit on the same file
// system as the source clone.
const integrationSkip = `set ` + sourceRemoteEnv + ` to a filter capable bare mirror of kubernetes/kubernetes.

Make one from an existing clone without touching that clone:

    git clone --bare --local \
        /Users/mo/gh_go/kubernetes/kubernetes/src/github.com/kubernetes/kubernetes \
        <mirror>.git
    git -C <mirror>.git config uploadpack.allowFilter true
    ` + sourceRemoteEnv + `=file://<mirror>.git go test ./internal/extract/ -run Kubernetes

The mirror must sit on the same file system as the source clone so its objects
are hard linked. The remote must be the file:// URL and not the bare path, or
git uses its local transport, ignores the object filter, and the cache audit
refuses the complete clone that results. uploadpack.allowFilter belongs on the
mirror only: setting it on the real clone would change a repository this test
does not own.`

// TestPlanKubernetesRBAC runs the whole plan against real upstream source.
//
// The synthetic fixtures in this package already cover the engine's behaviour.
// What this adds is proof that the behaviour survives contact with the real
// thing: real generator markers, real import grouping, real file modes, and the
// exact closure the implementation plan committed to.
//
// It is gated because a normal go test must not depend on the network, on
// several gigabytes of history, or on a repository that happens to be on this
// machine.
func TestPlanKubernetesRBAC(t *testing.T) {
	remote := os.Getenv(sourceRemoteEnv)
	if remote == "" {
		t.Skip(integrationSkip)
	}

	ctx := t.Context()
	cfg := repositoryProfile(t)
	root := t.TempDir()
	git, err := gitcli.New(ctx, gitcli.Options{Inherit: []string{"PATH"}})
	if err != nil {
		t.Fatalf("create git runner: %v", err)
	}

	result, err := extract.Plan(ctx, extract.Options{
		Config:       cfg,
		ProfileDir:   repositoryRoot(t),
		CacheRoot:    filepath.Join(root, "cache"),
		WorkRoot:     filepath.Join(root, "work"),
		OutputRoot:   filepath.Join(root, "tree"),
		Ref:          extract.Ref{Kind: extract.RefTag, Name: cfg.Source.Refs.MinimumRelease},
		PatchBranch:  cfg.Source.Refs.Branches[0],
		SourceRemote: remote,
		Fetch:        true,
		Git:          git,
		LookupEnv:    func(string) (string, bool) { return "", false },
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	report := result.Report
	t.Logf("plan summary:\n%s", result.Summary())

	// The closure the implementation plan commits to: four packages before
	// pruning, three after, and eight exact files removed.
	if got, want := report.Closure.Report.Observed.PrePrune.Packages, 4; got != want {
		t.Errorf("pre-prune packages = %d, want %d", got, want)
	}
	assertEqual(t, "post-prune packages", report.Closure.Report.Exact.Packages, []string{
		"k8s.io/kubernetes/pkg/apis/rbac/v1",
		"k8s.io/kubernetes/pkg/registry/rbac/validation",
		"k8s.io/kubernetes/plugin/pkg/auth/authorizer/rbac",
	})
	assertEqual(t, "removed files", report.Closure.RemovedFiles, slices.Sorted(slices.Values(cfg.Prune.Files)))
	if got, want := len(report.Closure.RemovedFiles), 8; got != want {
		t.Errorf("pruned files = %d, want %d", got, want)
	}

	// The unversioned internal API package disappears, while its retained /v1
	// helper subpackage keeps exactly the two files the profile requires.
	for _, pkg := range report.Closure.Report.Exact.Packages {
		if pkg == "k8s.io/kubernetes/pkg/apis/rbac" {
			t.Error("the denied internal API package is still in the closure")
		}
	}
	assertEqual(t, "retained pkg/apis/rbac/v1 files", filesUnder(report, "pkg/apis/rbac/v1/"), []string{
		"pkg/apis/rbac/v1/doc.go",
		"pkg/apis/rbac/v1/evaluation_helpers.go",
	})

	// Real imports were relocated, and the internal API package's own import
	// path is gone from the retained source along with the markers that pointed
	// at what pruning removed.
	authorizer := contentsOf(t, result, "internal/kk/plugin/pkg/auth/authorizer/rbac/rbac.go")
	if !strings.Contains(authorizer, cfg.Destination.Module+"/"+cfg.Destination.InternalPrefix+"/pkg/registry/rbac/validation") {
		t.Errorf("the authorizer's internal imports were not relocated:\n%s", authorizer)
	}
	if strings.Contains(authorizer, `"k8s.io/kubernetes/`) {
		t.Errorf("an upstream import path survived relocation:\n%s", authorizer)
	}
	// External staging types keep their real identity, which is what makes the
	// generated authorizer satisfy the real apiserver interfaces.
	if !strings.Contains(authorizer, `"k8s.io/apiserver/pkg/authorization/authorizer"`) {
		t.Errorf("an external import lost its identity:\n%s", authorizer)
	}

	doc := contentsOf(t, result, "internal/kk/pkg/apis/rbac/v1/doc.go")
	if strings.Contains(doc, "+k8s:conversion-gen=") {
		t.Errorf("the conversion marker points at a pruned package and must be removed:\n%s", doc)
	}
	if !strings.Contains(doc, "+groupName=") {
		t.Errorf("the API group marker must survive:\n%s", doc)
	}
	if len(report.Rewrite.DirectiveRemovals) == 0 {
		t.Error("no generator markers were removed, which the RBAC profile requires")
	}
	t.Logf("directive removals:\n  %s", strings.Join(report.Rewrite.DirectiveRemovals, "\n  "))

	if len(report.Rewrite.Unparsed) > 0 {
		t.Errorf("relocation produced unparsable files: %v", report.Rewrite.Unparsed)
	}
	if len(report.Rewrite.Unformatted) > 0 {
		t.Errorf("the pinned gofmt would reformat %d relocated files: %v",
			len(report.Rewrite.Unformatted), report.Rewrite.Unformatted)
	}
	if len(report.Notices) > 0 {
		t.Logf("notices:\n  %s", strings.Join(report.Notices, "\n  "))
	}
}

// filesUnder lists the closure's file paths below a directory prefix.
func filesUnder(report extract.Report, prefix string) []string {
	var out []string
	for _, file := range report.Closure.Report.Exact.Files {
		if strings.HasPrefix(file, prefix) {
			out = append(out, file)
		}
	}
	return out
}

// repositoryRoot reports the checkout this engine lives in, which is also the
// profile repository the patch loader reads from.
func repositoryRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("resolve the repository root: %v", err)
	}
	return root
}

// repositoryProfile loads the profile this repository publishes, so the
// integration assertions describe the real profile rather than a copy of it.
func repositoryProfile(t *testing.T) *config.Config {
	t.Helper()
	path := filepath.Join(repositoryRoot(t), config.DefaultFileName)
	cfg, err := config.Load(t.Context(), path)
	if err != nil {
		t.Fatalf("load %s: %v", path, err)
	}
	return cfg
}
