package generate

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/config"
	"github.com/enj/soapbox/tools/internal/gomodmap"
	"github.com/enj/soapbox/tools/internal/modgen"
	"github.com/enj/soapbox/tools/internal/relocate"
)

func TestCloneConfigIsDeep(t *testing.T) {
	profile := filepath.Join("..", "..", "..", "soapbox.yaml")
	cfg, err := config.Load(t.Context(), profile)
	if err != nil {
		t.Fatalf("load profile: %v", err)
	}
	clone, err := cloneConfig(cfg)
	if err != nil {
		t.Fatalf("clone profile: %v", err)
	}

	originalRoot := cfg.Packages.Roots[0]
	clone.Packages.Roots[0] = "changed/by/clone"
	if cfg.Packages.Roots[0] != originalRoot {
		t.Fatal("mutating the clone changed the caller's package roots")
	}

	originalPrune := clone.Prune.Files[0]
	cfg.Prune.Files[0] = "changed/by/caller"
	if clone.Prune.Files[0] != originalPrune {
		t.Fatal("mutating the caller changed the cloned prune list")
	}
}

func TestCheckDisjointAllowsIndexOnlyInCache(t *testing.T) {
	root := t.TempDir()
	cache := namedDir{name: "source cache root", path: filepath.Join(root, "cache")}
	insideCache := namedDir{name: "version index", path: filepath.Join(cache.path, "staging-versions.json"), file: true}
	if err := checkDisjoint(cache, insideCache); err != nil {
		t.Fatalf("version index in cache root: %v", err)
	}

	profile := namedDir{name: "profile directory", path: filepath.Join(root, "profile")}
	insideProfile := namedDir{name: "version index", path: filepath.Join(profile.path, "staging-versions.json"), file: true}
	if err := checkDisjoint(profile, insideProfile); !errors.Is(err, ErrPathConflict) {
		t.Fatalf("version index in profile error = %v, want ErrPathConflict", err)
	}
}

func TestPrePruneConfigDropsPostPruneLimits(t *testing.T) {
	cfg := &config.Config{
		Prune: config.Prune{Files: []string{"x.go"}},
		Deny:  config.Deny{Imports: []string{"example.com/internal"}},
		Closure: config.Closure{
			Golden: "shape.json",
			Limits: config.ClosureLimits{MaxPackages: 3, MaxPackageGrowth: 2},
		},
	}
	baseline := prePruneConfig(cfg)
	if baseline.Closure.Golden != "" || baseline.Closure.Limits != (config.ClosureLimits{}) {
		t.Fatalf("baseline closure controls = %#v, want no golden or post-prune limits", baseline.Closure)
	}
	if len(baseline.Prune.Files) != 0 || len(baseline.Deny.Imports) != 0 {
		t.Fatalf("baseline retained prune/deny controls: %#v %#v", baseline.Prune, baseline.Deny)
	}
	if cfg.Closure.Limits.MaxPackageGrowth != 2 || len(cfg.Prune.Files) != 1 {
		t.Fatal("building the baseline mutated the caller's profile")
	}
}

func TestResolveStagingStartsWithAnEmptyIndex(t *testing.T) {
	root, err := gomodmap.ParseRootModule("go.mod", []byte(`module k8s.io/kubernetes

go 1.26.0

require k8s.io/api v0.0.0

replace k8s.io/api => ./staging/src/k8s.io/api
`))
	if err != nil {
		t.Fatalf("parse root module: %v", err)
	}
	store := filepath.Join(t.TempDir(), "state", "versions.json")
	r := &run{opts: Options{StorePath: store, Offline: true}}
	_, _, err = r.resolveStaging(t.Context(), root, strings.Repeat("c", 40))
	if err == nil {
		t.Fatal("offline resolution with a cold index unexpectedly succeeded")
	}
	if errors.Is(err, gomodmap.ErrIndexMissing) {
		t.Fatalf("cold index was treated as corruption instead of an empty cache: %v", err)
	}
	if !strings.Contains(err.Error(), "holds no entry") {
		t.Fatalf("cold offline error = %v, want the missing commit explanation", err)
	}
}

func TestModuleMappingsDescribeOnlyKeptRequirements(t *testing.T) {
	root, err := gomodmap.ParseRootModule("go.mod", []byte(`module k8s.io/kubernetes

go 1.26.0

require (
	k8s.io/api v0.0.0
	k8s.io/apiserver v0.0.0
)

replace (
	k8s.io/api => ./staging/src/k8s.io/api
	k8s.io/apiserver => ./staging/src/k8s.io/apiserver
)
`))
	if err != nil {
		t.Fatalf("parse root module: %v", err)
	}
	r := &run{
		root: root,
		staging: []gomodmap.ModuleVersion{
			{Path: "k8s.io/api", Version: "v0.36.1", Commit: strings.Repeat("a", 40)},
			{Path: "k8s.io/apiserver", Version: "v0.36.1", Commit: strings.Repeat("b", 40)},
		},
		moduleReport: &modgen.Report{Kept: []gomodmap.Requirement{{Path: "k8s.io/api", Version: "v0.36.1"}}},
	}

	mappings, err := r.moduleMappings()
	if err != nil {
		t.Fatalf("module mappings: %v", err)
	}
	if len(mappings) != 1 || mappings[0].Module != "k8s.io/api" {
		t.Fatalf("module mappings = %#v, want only the kept k8s.io/api pin", mappings)
	}
}

func TestExecuteRecordsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r := &run{}
	if err := r.execute(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("execute error = %v, want context.Canceled", err)
	}
	if r.report.Failure == nil || r.report.Failure.Stage != stageExtract {
		t.Fatalf("cancellation failure = %#v, want extract stage", r.report.Failure)
	}
}

func TestRecordOutputCountsTheRootFacadePackage(t *testing.T) {
	report := Report{}
	report.recordOutput(relocate.FileSet{
		Files:    []relocate.File{{Path: "authorizer.go"}, {Path: "internal/kk/pkg/rbac/rbac.go", Package: "internal/kk/pkg/rbac"}},
		Packages: []relocate.Package{{Path: "internal/kk/pkg/rbac"}},
	}, false)
	if report.Output.Packages != 2 {
		t.Fatalf("output packages = %d, want relocated package plus root facade", report.Output.Packages)
	}
}

func TestReportJSONDoesNotEscapeEvidence(t *testing.T) {
	report := Report{Schema: ReportSchema, Failure: &FailureReport{Message: "<work> && <proxy>"}}
	data, err := report.JSON()
	if err != nil {
		t.Fatalf("report JSON: %v", err)
	}
	if !strings.Contains(string(data), "<work> && <proxy>") {
		t.Fatalf("report JSON escaped evidence: %s", data)
	}
}

func TestReportScrubsLoaderEnvironment(t *testing.T) {
	root := t.TempDir()
	cache := filepath.Join(root, "go-cache")
	proxy := "file://" + filepath.Join(root, "proxy")
	report := Report{}
	report.init(Options{Config: &config.Config{Destination: config.Destination{Module: "example.com/generated"}, Facade: config.Facade{Package: "generated"}}})
	report.addLoaderEnvironment([]string{"GOCACHE=" + cache, "GOPROXY=" + proxy})
	report.fail(stageModule, errors.New("read "+proxy+" using "+cache))
	if strings.Contains(report.Failure.Message, root) || strings.Contains(report.Failure.Message, proxy) {
		t.Fatalf("failure leaked loader environment: %q", report.Failure.Message)
	}
}

func TestReportScrubsSourceRemote(t *testing.T) {
	root := t.TempDir()
	mirror := filepath.Join(root, "private", "mirror.git")
	remote := "file://" + mirror
	report := Report{}
	report.init(Options{
		Config: &config.Config{
			Destination: config.Destination{Module: "example.com/generated"},
			Facade:      config.Facade{Package: "generated"},
			Determinism: config.Determinism{Toolchain: "go1.26.5"},
		},
		WorkRoot:     filepath.Join(root, "work"),
		CacheRoot:    filepath.Join(root, "cache"),
		OutputRoot:   filepath.Join(root, "output"),
		ProfileDir:   filepath.Join(root, "profile"),
		StorePath:    filepath.Join(root, "cache", "versions.json"),
		SourceRemote: remote,
	})
	report.fail(stageExtract, errors.New("fetch "+remote+" from "+mirror))
	if strings.Contains(report.Failure.Message, remote) || strings.Contains(report.Failure.Message, mirror) {
		t.Fatalf("failure leaked source remote: %q", report.Failure.Message)
	}
	if !strings.Contains(report.Failure.Message, "<source-remote>") {
		t.Fatalf("failure did not mark the scrubbed source remote: %q", report.Failure.Message)
	}
}
