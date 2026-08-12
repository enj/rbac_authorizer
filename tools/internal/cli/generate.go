package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/enj/soapbox/tools/internal/config"
	"github.com/enj/soapbox/tools/internal/generate"
	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/gocli"
)

// Default locations the generate command owns.
//
// A generation names no default cache at all, and derives the other two roots
// from the one the operator named. The engine refuses any layout where one of
// its directories contains another, and the profile directory is one of them,
// so the plan command's layout is unavailable here: a cache below the profile
// would refuse every run, and a default scratch root outside it would be a
// directory nobody named that the run removes on the way in and on the way out.
//
// The derived roots are siblings of the cache rather than children of it, which
// the engine's containment check reads as disjoint because it compares path
// elements rather than string prefixes. The version index is the one containment
// the engine permits, so it lives in the cache it belongs to.
const (
	generateWorkSuffix  = "-work"
	generateOutSuffix   = "-module"
	defaultVersionIndex = "staging-versions.json"
)

// goStateVariables are the process environment variables that decide where the
// go command keeps its state.
//
// They are forwarded because a generation drives the go command over the
// upstream module graph, which needs far more room than the caches a default
// HOME resolves to, and an operator who pointed those caches somewhere roomier
// meant it for this run too. Nothing else about the operator's environment
// travels with them: every name here is a location, so the list can move where
// the toolchain writes and cannot change what it trusts or where it fetches
// from, and no value reaches the subprocess as a credential.
var goStateVariables = []string{
	"GOPATH", "GOMODCACHE", "GOCACHE", "GOTMPDIR",
	"TMPDIR", "TEMP", "TMP", "XDG_CACHE_HOME", "XDG_CONFIG_HOME",
}

// generateFlags holds the parsed generate flags.
//
// The shared flags are the plan command's, spelled and behaving identically, so
// an operator who planned a ref generates it by changing the verb. The two that
// are added are the ones a generation has and a plan does not, because only a
// generation drives the Go toolchain.
type generateFlags struct {
	*runFlags
	proxy *string
	index *string
}

func generateFlagSet() (*flag.FlagSet, *generateFlags) {
	fs := newFlagSet("generate")
	shared := registerRunFlags(fs, runSpec{
		verb:  "generate",
		tree:  "generated module",
		cache: "",
		work:  "<cache>" + generateWorkSuffix,
		out:   "<cache>" + generateOutSuffix,
	})
	return fs, &generateFlags{
		runFlags: shared,
		proxy:    fs.String("proxy", gocli.DefaultProxy, "module proxy every Go command resolves through, or "+gocli.ProxyOff+" to resolve nothing"),
		index:    fs.String("version-index", "", "staging version index, relative to -dir when not absolute (default <cache>/"+defaultVersionIndex+")"),
	}
}

// runGenerate composes one complete generated module.
//
// Every usage problem is decided before the profile is read, so a command line
// that cannot work fails the same way whether or not a profile, a cache, or a
// network happens to be there. The command itself decides nothing else: what a
// module may contain is the engine's judgement, and this layer only resolves the
// directories, builds the two runners, and maps the answer onto an exit code.
func runGenerate(ctx context.Context, env Env, args []string) error {
	fs, flags := generateFlagSet()
	if err := parseFlags(env, generateCommand(), fs, args); err != nil {
		return err
	}
	usage := commandUsage(generateCommand(), fs)
	given := setFlags(fs)

	if !slices.Contains(runFormats, *flags.format) {
		return &usageError{
			err:   fmt.Errorf("unsupported -format %q, want %s", *flags.format, strings.Join(runFormats, ", ")),
			usage: usage,
		}
	}
	if given["tag"] && given["branch"] {
		return &usageError{err: errors.New("-tag and -branch select different refs, so only one may be given"), usage: usage}
	}
	// A bare -offline turns fetching off rather than contradicting its default.
	// Only an explicit request for both is a contradiction the operator has to
	// resolve.
	if *flags.offline && given["fetch"] && *flags.fetch {
		return &usageError{err: errors.New("-offline refuses every network operation, so -fetch cannot also be requested"), usage: usage}
	}
	proxy, err := generateProxy(flags, given)
	if err != nil {
		return &usageError{err: err, usage: usage}
	}

	paths, err := generatePaths(env, flags)
	if err != nil {
		return &usageError{err: err, usage: usage}
	}

	cfg, err := config.Load(ctx, paths.config)
	if err != nil {
		return profileError(env, paths.config, err)
	}
	// A branch is selected here and refused by the engine. The command line is
	// well formed, and the reason it cannot be served is that this engine does
	// not implement intermediate staging resolution, which is a statement about
	// the run rather than about what the operator typed.
	ref, err := selectedRef(flags.runFlags, cfg)
	if err != nil {
		return &usageError{err: err, usage: usage}
	}
	patchBranch, err := selectedPatchBranch(flags.runFlags, cfg, ref)
	if err != nil {
		return &usageError{err: err, usage: usage}
	}

	// The Git runner carries no caller supplied entry, which is what makes it
	// anonymous: a generation reads a public source host and nothing else, and
	// the engine refuses a runner that could carry a publishing credential to it.
	git, err := gitcli.New(ctx, gitcli.Options{})
	if err != nil {
		return err
	}
	goRunner, err := generateGoRunner(ctx, paths.dir, proxy)
	if err != nil {
		return err
	}

	result, err := generate.Generate(ctx, generate.Options{
		Config:       cfg,
		ProfileDir:   paths.dir,
		CacheRoot:    paths.cache,
		WorkRoot:     paths.work,
		OutputRoot:   paths.out,
		StorePath:    paths.store,
		Ref:          ref,
		PatchBranch:  patchBranch,
		SourceRemote: *flags.sourceRemote,
		Fetch:        *flags.fetch && !*flags.offline,
		Offline:      *flags.offline,
		Materialize:  *flags.materialize,
		KeepWorktree: *flags.keepWorktree,
		Strict:       *flags.strict,
		Git:          git,
		Go:           goRunner,
	})
	// A refused generation still produces a report whenever it measured
	// anything, and the report is what tells the operator which finding to act
	// on, so it is written before the failure is reported. The write failure is
	// joined rather than substituted: a generation that refused a module and
	// then could not write its report has two problems, and the exit code still
	// has to be the one that says a finding is waiting.
	var writeErr error
	if result != nil {
		switch {
		case err == nil:
			writeErr = writeReportOutput(ctx, env, "generate", paths.report, *flags.format, result.Report.JSON, result.Summary)
		case paths.report != "":
			// A failed generation writes its reviewable artifact when requested,
			// but stderr remains the one human-facing failure channel. Printing a
			// partial summary or JSON to stdout would make a failed command look
			// like it produced its requested result.
			quiet := env
			quiet.Stdout = io.Discard
			writeErr = writeReportOutput(ctx, quiet, "generate", paths.report, *flags.format, result.Report.JSON, result.Summary)
		}
	}
	return generateError(errors.Join(err, writeErr), usage)
}

// generateError maps a generation failure onto the process exit code contract.
//
// A policy failure means the engine ran and the answer is no, which CI reads as
// something to review. A refused run shape is one of those: the engine worked,
// and the honest answer is that it cannot produce this module.
//
// A conflicting layout is the exception, because it is the one failure the
// engine reports that nothing but the command line caused. The rule it breaks
// belongs to the engine rather than to this layer, so it is recognised here
// instead of being restated, which is what keeps the two from drifting apart.
//
// Everything else is a runtime failure or a cancellation and is left for the
// dispatcher to classify. The cause travels with the classification rather than
// being replaced by it, because a cancellation arrives wrapped in whatever the
// interrupted phase reported and the dispatcher decides on the cancellation
// first.
func generateError(err error, usage func(io.Writer)) error {
	var policy *generate.PolicyError
	if errors.As(err, &policy) {
		return &checkError{summary: err.Error(), err: err}
	}
	if errors.Is(err, generate.ErrPathConflict) {
		return &usageError{err: err, usage: usage}
	}
	return err
}

// generateProxy reports the module proxy every Go command resolves through.
//
// Offline decides it rather than colouring it. A run that refuses every network
// operation and then hands the toolchain a reachable proxy is offline in name
// only, so the flag's default is replaced instead of being honoured. An explicit
// proxy is a different matter: the operator asked for two things that cannot
// both happen, and picking one for them would silently discard the other.
func generateProxy(flags *generateFlags, given map[string]bool) (string, error) {
	if !*flags.offline {
		return *flags.proxy, nil
	}
	if given["proxy"] && *flags.proxy != gocli.ProxyOff {
		return "", fmt.Errorf("-offline refuses every network operation, so -proxy %s cannot also be requested", *flags.proxy)
	}
	return gocli.ProxyOff, nil
}

// resolvedGeneratePaths are the absolute locations one generation uses.
type resolvedGeneratePaths struct {
	dir    string
	config string
	cache  string
	work   string
	out    string
	store  string
	report string
}

// generatePaths resolves every path the generate command accepts.
//
// Each one is made absolute here rather than deeper in the engine, because a
// relative path means "against the process working directory" only at the point
// the operator typed it, and the engine's containment checks are meaningless
// against a path that could still be reinterpreted.
//
// The cache root is the one the operator has to supply, and it decides the rest.
// A generation removes its scratch root and refuses to write into an existing
// output tree, so defaulting either one to a directory nobody named is how a run
// deletes something an operator was keeping.
func generatePaths(env Env, flags *generateFlags) (resolvedGeneratePaths, error) {
	if *flags.cache == "" {
		return resolvedGeneratePaths{}, errors.New(
			"a generation keeps every directory it owns outside the profile directory, so -cache is required")
	}
	dir, err := filepath.Abs(env.resolve(*flags.dir))
	if err != nil {
		return resolvedGeneratePaths{}, fmt.Errorf("resolve -dir: %w", err)
	}
	paths := resolvedGeneratePaths{dir: dir}
	paths.config = resolveAgainst(dir, *flags.path)
	paths.cache = resolveAgainst(dir, *flags.cache)

	// The suffixes name siblings of the cache rather than children of it. The
	// engine compares path elements, so a cache at /state/src neither contains
	// nor is contained by /state/src-work.
	paths.work = paths.cache + generateWorkSuffix
	if *flags.work != "" {
		paths.work = resolveAgainst(dir, *flags.work)
	}
	paths.out = paths.cache + generateOutSuffix
	if *flags.out != "" {
		paths.out = resolveAgainst(dir, *flags.out)
	}
	paths.store = filepath.Join(paths.cache, defaultVersionIndex)
	if *flags.index != "" {
		paths.store = resolveAgainst(dir, *flags.index)
	}
	if *flags.report != "" {
		paths.report = resolveAgainst(dir, *flags.report)
	}
	if err := checkGeneratePathLayout(paths); err != nil {
		return resolvedGeneratePaths{}, err
	}
	return paths, nil
}

// checkGeneratePathLayout decides lexical path contradictions before the
// profile is read. The engine repeats the checks at its own trust boundary.
func checkGeneratePathLayout(paths resolvedGeneratePaths) error {
	for _, pair := range []struct {
		leftName, left, rightName, right string
	}{
		{"profile directory", paths.dir, "source cache root", paths.cache},
		{"profile directory", paths.dir, "work root", paths.work},
		{"profile directory", paths.dir, "output tree", paths.out},
		{"source cache root", paths.cache, "work root", paths.work},
		{"source cache root", paths.cache, "output tree", paths.out},
		{"work root", paths.work, "output tree", paths.out},
		{"profile directory", paths.dir, "version index", paths.store},
		{"work root", paths.work, "version index", paths.store},
		{"output tree", paths.out, "version index", paths.store},
	} {
		if err := requireDisjoint(pair.leftName, pair.left, pair.rightName, pair.right); err != nil {
			return err
		}
	}
	if paths.report != "" && pathContains(paths.out, paths.report) {
		return fmt.Errorf("-report %s must not be inside the generated output tree %s", paths.report, paths.out)
	}
	return nil
}

func requireDisjoint(leftName, left, rightName, right string) error {
	switch {
	case left == right:
		return fmt.Errorf("the %s and the %s are both %s", leftName, rightName, left)
	case pathContains(left, right):
		return fmt.Errorf("the %s %s contains the %s %s", leftName, left, rightName, right)
	case pathContains(right, left):
		return fmt.Errorf("the %s %s contains the %s %s", rightName, right, leftName, left)
	default:
		return nil
	}
}

func pathContains(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}

// generateGoRunner builds the runner every Go toolchain phase drives.
//
// It carries no Env entry at all, so nothing the operator holds for publishing
// can reach the go command, and the proxy is the only thing that decides where
// module state may come from. The working directory is the profile directory
// because it is the one directory known to exist when the runner is built; the
// engine rebases the runner onto each scratch module it creates.
func generateGoRunner(ctx context.Context, dir, proxy string) (*gocli.Runner, error) {
	isolation := make([]string, 0, len(goStateVariables))
	for _, name := range goStateVariables {
		if value, ok := os.LookupEnv(name); ok && value != "" {
			isolation = append(isolation, name+"="+value)
		}
	}
	return gocli.New(ctx, gocli.Options{Dir: dir, Isolation: isolation, Proxy: proxy})
}
