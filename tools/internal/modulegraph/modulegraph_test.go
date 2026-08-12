package modulegraph_test

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/modulegraph"
)

func TestLoadReturnsTheResolvedGraph(t *testing.T) {
	t.Parallel()
	_, graph := shared(t)

	paths := graph.ImportPaths()
	for _, want := range []string{facadePkg, internalPkg, externalPkg} {
		if !slices.Contains(paths, want) {
			t.Fatalf("package %q is missing from the graph", want)
		}
	}
	// NeedDeps is requested, so the transitive graph has to be there too. A
	// load that stopped at the roots would let every policy walk a graph with
	// no edges and find nothing.
	if !slices.Contains(paths, "strings") {
		t.Fatal("the transitive standard library dependency is missing, so NeedDeps did not take effect")
	}
	if !slices.IsSorted(paths) {
		t.Fatalf("import paths are not sorted: %v", paths)
	}
	if graph.Fset() == nil {
		t.Fatal("the graph carries no file set, so evidence could not carry positions")
	}
}

// TestLoadSharesOneFileSet proves positions from two different packages live in
// one coordinate space. Both policy reports render evidence positions, and two
// file sets would produce two unrelated coordinate spaces that look alike.
func TestLoadSharesOneFileSet(t *testing.T) {
	t.Parallel()
	_, graph := shared(t)
	adapted, err := graph.Typeswap(t.Context(), modulegraph.TypeswapSpec{})
	if err != nil {
		t.Fatalf("adapt type policy graph: %v", err)
	}

	seen := map[string]string{}
	for _, pkg := range adapted.Packages {
		if len(pkg.Syntax) == 0 {
			t.Fatalf("package %q carries no syntax", pkg.ImportPath)
		}
		position := graph.Fset().Position(pkg.Syntax[0].FileStart)
		if !position.IsValid() {
			t.Fatalf("package %q has a file that the shared file set cannot locate", pkg.ImportPath)
		}
		seen[pkg.ImportPath] = position.Filename
	}
	for _, importPath := range []string{facadePkg, internalPkg, externalPkg} {
		if !filepath.IsAbs(seen[importPath]) {
			t.Fatalf("package %q resolved to %q, which is not an absolute file", importPath, seen[importPath])
		}
	}
}

func TestLoadRejectsBadOptions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*modulegraph.Options)
		want   string
	}{
		{
			name:   "no directory",
			mutate: func(o *modulegraph.Options) { o.Dir = "" },
			want:   "a module directory is required",
		},
		{
			name:   "relative directory",
			mutate: func(o *modulegraph.Options) { o.Dir = "generated" },
			want:   "must be absolute",
		},
		{
			name:   "no environment",
			mutate: func(o *modulegraph.Options) { o.Env = nil },
			want:   "an explicit environment is required",
		},
		{
			name:   "no patterns",
			mutate: func(o *modulegraph.Options) { o.Patterns = nil },
			want:   "at least one package pattern is required",
		},
		{
			name:   "no redactor",
			mutate: func(o *modulegraph.Options) { o.Redactor = nil },
			want:   "a redactor is required",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			opts := sharedOptions(t)
			test.mutate(&opts)

			graph, err := modulegraph.Load(t.Context(), opts)
			if err == nil {
				t.Fatalf("bad options were accepted and returned %v", graph)
			}
			if !errors.Is(err, modulegraph.ErrOptions) {
				t.Fatalf("error = %v, want ErrOptions", err)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error %v does not mention %q", err, test.want)
			}
		})
	}
}

// TestLoadRefusesAnEmptyMatch pins the one go/packages result that looks like
// success. A pattern that matches nothing returns no package and no error, and
// downstream that becomes a policy run over an empty graph, which passes
// everything it was asked to judge.
func TestLoadRefusesAnEmptyMatch(t *testing.T) {
	t.Parallel()

	opts := sharedOptions(t)
	opts.Patterns = []string{"./nothing/matches/this/..."}

	graph, err := modulegraph.Load(t.Context(), opts)
	if err == nil {
		t.Fatalf("an empty match was accepted and returned %v", graph)
	}
	if !errors.Is(err, modulegraph.ErrLoad) {
		t.Fatalf("error = %v, want ErrLoad", err)
	}
}

// TestLoadRefusesAPackageThatDoesNotCompile keeps a broken package from
// reaching a policy. A type checker diagnostic means the graph does not
// describe code the toolchain would build, so nothing decided from it is a fact
// about the module that would ship.
func TestLoadRefusesAPackageThatDoesNotCompile(t *testing.T) {
	t.Parallel()
	f := newFixture(t, map[string]string{
		"generated/broken/broken.go": `package broken

// Broken names a type that does not exist.
func Broken() Missing { return nil }
`,
	})

	graph, err := modulegraph.Load(t.Context(), f.options())
	if err == nil {
		t.Fatalf("a package that does not compile was accepted and returned %v", graph)
	}
	if !errors.Is(err, modulegraph.ErrLoad) {
		t.Fatalf("error = %v, want ErrLoad", err)
	}
	if !strings.Contains(err.Error(), "Missing") {
		t.Fatalf("error %v does not name the undefined symbol, so it does not locate the cause", err)
	}
}

// TestLoadReportsEveryProblemAtOnce proves the loader does not stop at the
// first broken package. An operator fixes what they are shown, and a load that
// reported one problem per run would turn one repair into several.
func TestLoadReportsEveryProblemAtOnce(t *testing.T) {
	t.Parallel()
	f := newFixture(t, map[string]string{
		"generated/first/first.go": `package first

// First refers to nothing.
func First() { undefinedFirst() }
`,
		"generated/second/second.go": `package second

// Second refers to nothing.
func Second() { undefinedSecond() }
`,
	})

	_, err := modulegraph.Load(t.Context(), f.options())
	if err == nil {
		t.Fatal("two broken packages were accepted")
	}
	for _, want := range []string{"undefinedFirst", "undefinedSecond"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %v does not mention %q, so it reports only part of the damage", err, want)
		}
	}
}

func TestLoadHonoursACancelledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := modulegraph.Load(ctx, sharedOptions(t)); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

// TestAdaptersHonourACancelledContext covers the adapters as well as the load.
// Adapting the Kubernetes graph walks every package, so a cancelled run has to
// stop there too rather than only at the subprocess boundary.
func TestAdaptersHonourACancelledContext(t *testing.T) {
	t.Parallel()
	_, graph := shared(t)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := graph.Typeswap(ctx, modulegraph.TypeswapSpec{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("type policy adaptation error = %v, want context.Canceled", err)
	}
	if _, err := graph.Deppolicy(ctx, spec()); !errors.Is(err, context.Canceled) {
		t.Fatalf("dependency policy adaptation error = %v, want context.Canceled", err)
	}
}
