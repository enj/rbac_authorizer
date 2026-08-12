package gomodmap_test

import (
	"context"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/gomodmap"
	"github.com/enj/soapbox/tools/internal/testsupport"
)

// kubernetesGoMod is the shape of a Kubernetes root go.mod, reduced to the
// directives this package reads. The staging modules are replaced with relative
// directories and required at the placeholder version, which is what upstream
// does for all of them.
const kubernetesGoMod = `module k8s.io/kubernetes

go 1.34.0

toolchain go1.34.2

godebug default=go1.34

godebug winsymlink=0

require (
	github.com/spf13/cobra v1.10.1
	k8s.io/api v0.0.0
	k8s.io/apimachinery v0.0.0
	k8s.io/klog/v2 v2.130.1 // indirect
)

replace (
	k8s.io/api => ./staging/src/k8s.io/api
	k8s.io/apimachinery => ./staging/src/k8s.io/apimachinery
)
`

func TestParseRootModule(t *testing.T) {
	t.Parallel()

	root, err := gomodmap.ParseRootModule("go.mod", []byte(kubernetesGoMod))
	if err != nil {
		t.Fatalf("parse root module: %v", err)
	}

	if root.Path != "k8s.io/kubernetes" {
		t.Errorf("module path = %q, want k8s.io/kubernetes", root.Path)
	}
	if root.Go != "1.34.0" {
		t.Errorf("go directive = %q, want 1.34.0", root.Go)
	}
	if root.Toolchain != "go1.34.2" {
		t.Errorf("toolchain = %q, want go1.34.2", root.Toolchain)
	}

	wantGodebug := []gomodmap.Godebug{{Key: "default", Value: "go1.34"}, {Key: "winsymlink", Value: "0"}}
	if len(root.Godebug) != len(wantGodebug) {
		t.Fatalf("godebug = %v, want %v", root.Godebug, wantGodebug)
	}
	for i, want := range wantGodebug {
		if root.Godebug[i] != want {
			t.Errorf("godebug[%d] = %v, want %v", i, root.Godebug[i], want)
		}
	}

	// The staging modules are exactly the relatively replaced ones, in sorted
	// order, each carrying the directory that provides it.
	wantStaging := []gomodmap.StagingModule{
		{Path: "k8s.io/api", Dir: "staging/src/k8s.io/api", Required: true},
		{Path: "k8s.io/apimachinery", Dir: "staging/src/k8s.io/apimachinery", Required: true},
	}
	if len(root.Staging) != len(wantStaging) {
		t.Fatalf("staging = %v, want %v", root.Staging, wantStaging)
	}
	for i, want := range wantStaging {
		if root.Staging[i] != want {
			t.Errorf("staging[%d] = %v, want %v", i, root.Staging[i], want)
		}
	}

	// Everything else keeps its exact upstream version and its directness.
	wantExternal := []gomodmap.Requirement{
		{Path: "github.com/spf13/cobra", Version: "v1.10.1"},
		{Path: "k8s.io/klog/v2", Version: "v2.130.1", Indirect: true},
	}
	if len(root.External) != len(wantExternal) {
		t.Fatalf("external = %v, want %v", root.External, wantExternal)
	}
	for i, want := range wantExternal {
		if root.External[i] != want {
			t.Errorf("external[%d] = %v, want %v", i, root.External[i], want)
		}
	}
}

func TestParseRootModule_Rejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		goMod   string
		wantErr string
	}{
		{
			name:    "no module directive",
			goMod:   "go 1.34.0\n",
			wantErr: "a module directive is required",
		},
		{
			name:    "no go directive",
			goMod:   "module k8s.io/kubernetes\n",
			wantErr: "a go directive is required",
		},
		{
			// A replacement onto a published module is the case that would
			// silently change which code a copied version means.
			name: "replacement onto another module",
			goMod: `module k8s.io/kubernetes

go 1.34.0

require github.com/spf13/cobra v1.10.1

replace github.com/spf13/cobra => github.com/other/cobra v1.0.0
`,
			wantErr: "rather than at the staging tree",
		},
		{
			name: "replacement onto an unrelated directory",
			goMod: `module k8s.io/kubernetes

go 1.34.0

require k8s.io/api v0.0.0

replace k8s.io/api => ./vendor/k8s.io/api
`,
			wantErr: `rather than below "./staging/src/"`,
		},
		{
			// A staging directory that does not match the module path would serve
			// one module's code out of another module's tree.
			name: "staging directory does not match the module",
			goMod: `module k8s.io/kubernetes

go 1.34.0

require k8s.io/api v0.0.0

replace k8s.io/api => ./staging/src/k8s.io/apimachinery
`,
			wantErr: `rather than at "./staging/src/k8s.io/api"`,
		},
		{
			name: "version pinned replacement",
			goMod: `module k8s.io/kubernetes

go 1.34.0

require k8s.io/api v0.1.0

replace k8s.io/api v0.1.0 => ./staging/src/k8s.io/api
`,
			wantErr: "is pinned to version v0.1.0",
		},
		{
			// The placeholder version is the proof that upstream never expected
			// the requirement to resolve against a proxy.
			name: "staging required at a real version",
			goMod: `module k8s.io/kubernetes

go 1.34.0

require k8s.io/api v0.34.1

replace k8s.io/api => ./staging/src/k8s.io/api
`,
			wantErr: "rather than the placeholder v0.0.0",
		},
		{
			name: "godebug set twice",
			goMod: `module k8s.io/kubernetes

go 1.34.0

godebug default=go1.34

godebug default=go1.33

require github.com/spf13/cobra v1.10.1
`,
			wantErr: "godebug default is set more than once",
		},
		{
			// An exclude removes a version from selection. The generated module
			// carries none, so selection there could choose the version upstream
			// refused.
			name: "exclude directive",
			goMod: `module k8s.io/kubernetes

go 1.34.0

require github.com/spf13/cobra v1.10.1

exclude github.com/spf13/cobra v1.9.0
`,
			wantErr: "exclude directives change version selection",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := gomodmap.ParseRootModule("go.mod", []byte(test.goMod))
			if err == nil {
				t.Fatalf("parse root module: got nil error, want %q", test.wantErr)
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Errorf("parse root module: error = %v, want it to contain %q", err, test.wantErr)
			}
		})
	}
}

// TestParseRootModule_UnrequiredStaging proves a staged module the root module
// does not build against is kept rather than refused.
//
// This is the real Kubernetes shape: sample-cli-plugin and sample-controller are
// replaced so the tree publishes them, but nothing in the root module imports
// them, so they carry no requirement and therefore no version to validate.
func TestParseRootModule_UnrequiredStaging(t *testing.T) {
	t.Parallel()

	root, err := gomodmap.ParseRootModule("go.mod", []byte(`module k8s.io/kubernetes

go 1.26.0

require (
	k8s.io/api v0.0.0
	k8s.io/klog/v2 v2.130.1 // indirect
)

replace (
	k8s.io/api => ./staging/src/k8s.io/api
	k8s.io/sample-cli-plugin => ./staging/src/k8s.io/sample-cli-plugin
	k8s.io/sample-controller => ./staging/src/k8s.io/sample-controller
)
`))
	if err != nil {
		t.Fatalf("parse root module: %v", err)
	}

	want := []gomodmap.StagingModule{
		{Path: "k8s.io/api", Dir: "staging/src/k8s.io/api", Required: true},
		{Path: "k8s.io/sample-cli-plugin", Dir: "staging/src/k8s.io/sample-cli-plugin"},
		{Path: "k8s.io/sample-controller", Dir: "staging/src/k8s.io/sample-controller"},
	}
	if len(root.Staging) != len(want) {
		t.Fatalf("staging = %v, want %v", root.Staging, want)
	}
	for i, module := range want {
		if root.Staging[i] != module {
			t.Errorf("staging[%d] = %v, want %v", i, root.Staging[i], module)
		}
	}

	// An unrequired staging module must not leak into the external requirements,
	// which are the versions the generated module copies verbatim.
	for _, requirement := range root.External {
		if strings.HasPrefix(requirement.Path, "k8s.io/sample-") {
			t.Errorf("staged module %s was classified as external", requirement.Path)
		}
	}
}

// TestParseRootModule_RecordsUncarriedDirectives proves directives the generated
// module does not carry are still recorded, so nothing is dropped silently.
func TestParseRootModule_RecordsUncarriedDirectives(t *testing.T) {
	t.Parallel()

	root, err := gomodmap.ParseRootModule("go.mod", []byte(`module k8s.io/kubernetes

go 1.26.0

require github.com/spf13/cobra v1.10.1

tool k8s.io/code-generator/cmd/client-gen

ignore ./_output

retract (
	v1.0.0 // Published by mistake.
	[v1.1.0, v1.2.0]
)
`))
	if err != nil {
		t.Fatalf("parse root module: %v", err)
	}
	if len(root.Tool) != 1 || root.Tool[0] != "k8s.io/code-generator/cmd/client-gen" {
		t.Errorf("tool = %v, want the one tool directive", root.Tool)
	}
	if len(root.Ignore) != 1 || root.Ignore[0] != "./_output" {
		t.Errorf("ignore = %v, want the one ignore directive", root.Ignore)
	}
	if len(root.Retract) != 2 {
		t.Fatalf("retract = %v, want 2", root.Retract)
	}
	if root.Retract[0].Low != "v1.0.0" || root.Retract[0].High != "v1.0.0" {
		t.Errorf("retract[0] = %v, want the single version v1.0.0", root.Retract[0])
	}
	if root.Retract[1].Low != "v1.1.0" || root.Retract[1].High != "v1.2.0" {
		t.Errorf("retract[1] = %v, want the interval v1.1.0 to v1.2.0", root.Retract[1])
	}
}

func TestReadRootModule(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	repo := testsupport.NewRepo(ctx, t, testsupport.Options{
		UserName:  "Test",
		UserEmail: "test@example.com",
	})
	first := repo.WriteAndCommit(ctx, t, "go.mod", kubernetesGoMod, "add module")

	// A later commit changes the module so the read is proved to answer for the
	// revision it was asked about rather than for the branch tip.
	repo.WriteAndCommit(ctx, t, "go.mod", strings.Replace(kubernetesGoMod, "go 1.34.0", "go 1.35.0", 1), "bump go")

	root, err := gomodmap.ReadRootModule(ctx, repo.Git, first)
	if err != nil {
		t.Fatalf("read root module: %v", err)
	}
	if root.Go != "1.34.0" {
		t.Errorf("go directive at %s = %q, want 1.34.0", first, root.Go)
	}
	if len(root.Staging) != 2 {
		t.Errorf("staging modules = %v, want 2", root.Staging)
	}

	head, err := gomodmap.ReadRootModule(ctx, repo.Git, "HEAD")
	if err != nil {
		t.Fatalf("read root module at HEAD: %v", err)
	}
	if head.Go != "1.35.0" {
		t.Errorf("go directive at HEAD = %q, want 1.35.0", head.Go)
	}
}

func TestReadRootModule_MissingFile(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	repo := testsupport.NewRepo(ctx, t, testsupport.Options{
		UserName:  "Test",
		UserEmail: "test@example.com",
	})
	repo.WriteAndCommit(ctx, t, "README.md", "no module here\n", "add readme")

	_, err := gomodmap.ReadRootModule(ctx, repo.Git, "HEAD")
	if err == nil {
		t.Fatal("read root module: got nil error, want a missing object error")
	}
	if !strings.Contains(err.Error(), "HEAD") {
		t.Errorf("read root module: error = %v, want it to name the revision", err)
	}
}

// TestReadRootModule_ContextCancelled proves the read honours cancellation
// rather than running the subprocess to completion.
func TestReadRootModule_ContextCancelled(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	repo := testsupport.NewRepo(ctx, t, testsupport.Options{
		UserName:  "Test",
		UserEmail: "test@example.com",
	})
	repo.WriteAndCommit(ctx, t, "go.mod", kubernetesGoMod, "add module")

	cancelled, cancel := context.WithCancel(ctx)
	cancel()

	if _, err := gomodmap.ReadRootModule(cancelled, repo.Git, "HEAD"); err == nil {
		t.Fatal("read root module: got nil error, want a cancellation error")
	}
}

// TestReadRootModule_UsesRunner proves the read goes through the typed runner's
// object store rather than the working tree, by reading a revision whose go.mod
// is not the one checked out.
func TestReadRootModule_UsesObjectStore(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	repo := testsupport.NewRepo(ctx, t, testsupport.Options{
		UserName:  "Test",
		UserEmail: "test@example.com",
	})
	committed := repo.WriteAndCommit(ctx, t, "go.mod", kubernetesGoMod, "add module")

	// Corrupting the working tree copy must not change what the commit says.
	repo.WriteFile(t, "go.mod", "this is not a go.mod at all\n")

	root, err := gomodmap.ReadRootModule(ctx, repo.Git, committed)
	if err != nil {
		t.Fatalf("read root module: %v", err)
	}
	if root.Path != "k8s.io/kubernetes" {
		t.Errorf("module path = %q, want k8s.io/kubernetes", root.Path)
	}
}
