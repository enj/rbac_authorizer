package modulegraph_test

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/modulegraph"
)

// The values the tests below plant and then assert the absence of.
//
// They are distinctive literals so an assertion is about this exact string
// rather than about a pattern something else might also match. seededIdentifier
// is spelled as a Go identifier so it can be planted inside source that the
// type checker will quote back in a diagnostic.
const (
	proxySecret      = "s3cret-loader-token-value"
	seededIdentifier = "SoapboxSeededSecretIdentifier"
	// adapterSentinel is seeded into the shared fixture's runner, which is what
	// lets the adapter test reuse the one shared load rather than taking a
	// second one. Loads are the expensive, contended thing here.
	adapterSentinel = "absent-and-secret"
)

// TestLoadRequiresARedactor covers the option that exists only to stop a leak.
//
// Defaulting it would be silent, and what it would silently stop scrubbing is
// the credential in GOPROXY, so the load refuses instead.
func TestLoadRequiresARedactor(t *testing.T) {
	t.Parallel()

	opts := sharedOptions(t)
	opts.Redactor = nil

	graph, err := modulegraph.Load(t.Context(), opts)
	if err == nil {
		t.Fatalf("a load with no redactor was accepted and returned %v", graph)
	}
	if !errors.Is(err, modulegraph.ErrOptions) {
		t.Fatalf("error = %v, want ErrOptions", err)
	}
	if !strings.Contains(err.Error(), "GOPROXY") {
		t.Fatalf("error %v does not say why a redactor is required", err)
	}
}

// TestLoadRedactsTheProxyCredential is a regression guard on the end to end
// path, and it is deliberately not the proof that the scrubber works.
//
// The module requires a dependency with no replace directive, so resolving it
// has to reach the proxy, and the proxy URL carries a password. The go command
// then fails naming what it was fetching, and that diagnostic reaches the
// caller through go/packages rather than through gocli.
//
// Measured rather than assumed: this test still passes when the redactor is
// replaced with an empty one, because the go command masks the userinfo in a
// module fetch URL itself. So what it pins is the property an operator cares
// about, that the credential never reaches a report, and not the mechanism. The
// mechanism is proved by the two seeded value tests below, which assert a
// placeholder is present and therefore fail if nothing scrubbed. Keeping this
// one is still worth it: the go command's masking covers the paths it knows
// about, this package's covers the rest, and neither is a reason to drop the
// other.
func TestLoadRedactsTheProxyCredential(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "go.mod"), `module example.test/needsproxy

go 1.26.0

require k8s.io/absent v1.0.0
`)
	mustWrite(t, filepath.Join(dir, "use.go"), `package needsproxy

import _ "k8s.io/absent/pkg"
`)

	// An unroutable address, so the fetch fails quickly and locally rather than
	// carrying the credential to anything real.
	proxy := "https://soapbox:" + proxySecret + "@127.0.0.1:1"
	env, redactor, err := buildLoaderEnv(t.Context(), dir, dir, proxy)
	if err != nil {
		t.Fatalf("loader environment: %v", err)
	}
	// The environment genuinely carries the secret. Without this the assertion
	// below would pass for a load that never had anything to leak.
	if !strings.Contains(strings.Join(env, "\n"), proxySecret) {
		t.Fatal("the loader environment does not carry the credential, so this test proves nothing")
	}

	graph, err := modulegraph.Load(t.Context(), modulegraph.Options{
		Dir:      dir,
		Env:      env,
		Patterns: []string{"./..."},
		Redactor: redactor,
	})
	if err == nil {
		t.Fatalf("a module that cannot be resolved was accepted and returned %v", graph)
	}
	if strings.Contains(err.Error(), proxySecret) {
		t.Fatalf("the load error leaked the proxy credential: %v", err)
	}
}

// TestLoadRedactsPackageDiagnostics proves the wiring reaches the per package
// diagnostics rather than only the top level error.
//
// A credential does not naturally appear in a type checker message, so the
// redactor is seeded with a value that certainly will: the undefined symbol the
// broken package names. That makes the assertion deterministic instead of
// depending on whether the go command happens to echo a URL.
func TestLoadRedactsPackageDiagnostics(t *testing.T) {
	t.Parallel()

	f := newFixture(t, map[string]string{
		"generated/broken/broken.go": `package broken

// Broken names a type that does not exist.
func Broken() ` + seededIdentifier + ` { return nil }
`,
	})

	opts := f.options()
	opts.Redactor = gitcli.NewRedactor(seededIdentifier)

	_, err := modulegraph.Load(t.Context(), opts)
	if err == nil {
		t.Fatal("a package that does not compile was accepted")
	}
	if strings.Contains(err.Error(), seededIdentifier) {
		t.Fatalf("a package diagnostic leaked the seeded value: %v", err)
	}
	if !strings.Contains(err.Error(), gitcli.Placeholder) {
		t.Fatalf("error %v carries no placeholder, so the diagnostic was not scrubbed at all", err)
	}
}

// TestAdapterErrorsAreRedacted covers the second reporting surface. The
// adapters quote import paths, module roots, and resolved versions, all of
// which came from the same go command whose output the load had to scrub.
func TestAdapterErrorsAreRedacted(t *testing.T) {
	t.Parallel()

	const absent = "example.test/" + adapterSentinel
	_, graph := shared(t)

	tests := []struct {
		name  string
		adapt func() error
	}{
		{
			name: "type policy",
			adapt: func() error {
				_, err := graph.Typeswap(t.Context(), modulegraph.TypeswapSpec{Packages: []string{absent}})
				return err
			},
		},
		{
			name: "dependency policy",
			adapt: func() error {
				_, err := graph.Deppolicy(t.Context(), modulegraph.DeppolicySpec{Boundary: []string{absent}})
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := test.adapt()
			if err == nil {
				t.Fatal("an absent package was accepted")
			}
			if strings.Contains(err.Error(), absent) {
				t.Fatalf("the adaptation leaked the seeded value: %v", err)
			}
			if !strings.Contains(err.Error(), gitcli.Placeholder) {
				t.Fatalf("error %v carries no placeholder, so the adapter did not scrub", err)
			}
		})
	}
}

// TestLoaderEnvAndRedactorScrubTheSameSecret pins the pairing the whole design
// rests on. LoaderEnv puts the proxy credential into the environment and
// Runner.Redactor is what takes it back out, so a change that seeded one
// without the other would leave the loader unable to scrub what it was handed.
func TestLoaderEnvAndRedactorScrubTheSameSecret(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "go.mod"), "module example.test/pairing\n\ngo 1.26.0\n")

	proxy := "https://soapbox:" + proxySecret + "@proxy.invalid"
	env, redactor, err := buildLoaderEnv(t.Context(), dir, dir, proxy)
	if err != nil {
		t.Fatalf("loader environment: %v", err)
	}
	if !strings.Contains(strings.Join(env, "\n"), proxySecret) {
		t.Fatal("LoaderEnv did not carry the proxy credential")
	}
	if got := redactor.String("fetching " + proxy); strings.Contains(got, proxySecret) {
		t.Fatalf("the runner's redactor does not scrub the credential its own LoaderEnv emitted: %q", got)
	}
}
