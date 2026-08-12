package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/enj/soapbox/tools/internal/config"
	"github.com/enj/soapbox/tools/internal/extract"
	"github.com/enj/soapbox/tools/internal/gitcli"
)

// Default locations the plan command owns.
//
// The cache is a sibling of the profile rather than a temporary directory
// because it is expensive to build and meant to be reused, and the work and
// output roots nest below it so that one directory holds everything a plan
// created and an operator can remove all of it in one step.
const (
	defaultCacheDir   = ".soapbox-cache"
	defaultWorkDir    = "work"
	defaultOutputName = "tree"
)

// releaseBranchPrefix is how upstream Kubernetes names the branch a release tag
// was cut from.
//
// A patch is authored against a line of development, and a release tag names no
// line, so a tag run has to derive one. Kubernetes cuts v1.36.1 from
// release-1.36, and the derivation is used only when the profile actually tracks
// the branch it produces, so a profile with a different convention gets no
// silent guess.
const releaseBranchPrefix = "release-"

// planFormats are the supported plan output formats, in help order.
var planFormats = []string{"summary", "json"}

// planFlags holds the parsed plan flags.
type planFlags struct {
	dir          *string
	path         *string
	cache        *string
	work         *string
	out          *string
	report       *string
	tag          *string
	branch       *string
	patchBranch  *string
	sourceRemote *string
	fetch        *bool
	offline      *bool
	materialize  *bool
	keepWorktree *bool
	format       *string
	strict       *bool
}

func planFlagSet() (*flag.FlagSet, *planFlags) {
	fs := newFlagSet("plan")
	return fs, &planFlags{
		dir:          fs.String("dir", ".", "directory that holds the profile"),
		path:         fs.String("config", config.DefaultFileName, "profile path relative to -dir"),
		cache:        fs.String("cache", defaultCacheDir, "source cache root, relative to -dir when not absolute"),
		work:         fs.String("work", "", "scratch root, relative to -dir when not absolute (default <cache>/"+defaultWorkDir+")"),
		out:          fs.String("out", "", "relocated tree destination (default <work>/"+defaultOutputName+")"),
		report:       fs.String("report", "", "also write the JSON report to this path"),
		tag:          fs.String("tag", "", "source tag to plan (default source.refs.minimumRelease)"),
		branch:       fs.String("branch", "", "source branch to plan, instead of a tag"),
		patchBranch:  fs.String("patch-branch", "", "tracked branch patch selectors are matched against"),
		sourceRemote: fs.String("source-remote", "", "read source history from this remote instead of the profile's"),
		fetch:        fs.Bool("fetch", true, "update the source cache before planning"),
		offline:      fs.Bool("offline", false, "refuse every network operation and require an existing cache"),
		materialize:  fs.Bool("materialize", false, "write the relocated tree to -out"),
		keepWorktree: fs.Bool("keep-worktree", false, "leave the materialized source tree in place"),
		format:       fs.String("format", planFormats[0], "output format: "+strings.Join(planFormats, ", ")),
		strict:       fs.Bool("strict", false, "treat advisory notices as a policy failure"),
	}
}

// runPlan computes one extraction plan.
//
// Every usage problem is decided before the profile is read, so a command line
// that cannot work fails the same way whether or not a profile, a cache, or a
// network happens to be there.
func runPlan(ctx context.Context, env Env, args []string) error {
	fs, flags := planFlagSet()
	if err := parseFlags(env, planCommand(), fs, args); err != nil {
		return err
	}
	usage := commandUsage(planCommand(), fs)
	given := setFlags(fs)

	if !slices.Contains(planFormats, *flags.format) {
		return &usageError{
			err:   fmt.Errorf("unsupported -format %q, want %s", *flags.format, strings.Join(planFormats, ", ")),
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

	paths, err := planPaths(env, flags)
	if err != nil {
		return &usageError{err: err, usage: usage}
	}

	// The directories are checked before the profile is opened, because an
	// output tree that would sit where the run's own state lives is a problem
	// with what the operator typed and has to fail the same way whether or not
	// a profile, a cache, or a network happens to be there.
	if err := (extract.Options{
		ProfileDir: paths.dir,
		CacheRoot:  paths.cache,
		WorkRoot:   paths.work,
		OutputRoot: paths.out,
	}).CheckPaths(); err != nil {
		return &usageError{err: err, usage: usage}
	}

	cfg, err := config.Load(ctx, paths.config)
	if err != nil {
		return profileError(env, paths.config, err)
	}
	ref, err := planRef(flags, cfg)
	if err != nil {
		return &usageError{err: err, usage: usage}
	}
	patchBranch, err := planPatchBranch(flags, cfg, ref)
	if err != nil {
		return &usageError{err: err, usage: usage}
	}

	git, err := gitcli.New(ctx, gitcli.Options{})
	if err != nil {
		return err
	}
	result, err := extract.Plan(ctx, extract.Options{
		Config:       cfg,
		ProfileDir:   paths.dir,
		CacheRoot:    paths.cache,
		WorkRoot:     paths.work,
		OutputRoot:   paths.out,
		Ref:          ref,
		PatchBranch:  patchBranch,
		SourceRemote: *flags.sourceRemote,
		Fetch:        *flags.fetch && !*flags.offline,
		Offline:      *flags.offline,
		Materialize:  *flags.materialize,
		KeepWorktree: *flags.keepWorktree,
		Strict:       *flags.strict,
		Git:          git,
	})
	// A refused plan still produces a report whenever it measured anything, and
	// the report is what tells the operator which finding to act on, so it is
	// written before the failure is reported. The write failure is joined rather
	// than substituted: a plan that found a patch conflict and then could not
	// write its report has two problems, and the exit code still has to be the
	// one that says a finding is waiting.
	var writeErr error
	if result != nil {
		writeErr = writePlanOutput(ctx, env, paths.report, *flags.format, result)
	}
	return planError(errors.Join(err, writeErr))
}

// planError maps a plan failure onto the process exit code contract.
//
// A policy failure means the engine ran and the answer is no, which CI reads as
// something to review. Everything else is a runtime failure or a cancellation
// and is left for the dispatcher to classify.
//
// The cause travels with the classification rather than being replaced by it.
// A cancellation reaches here wrapped in whatever the interrupted phase
// reported, sometimes inside a policy failure, and the dispatcher decides on the
// cancellation first; a summary that dropped the chain would turn an interrupted
// run into a finding about the profile. The rendered summary is the whole joined
// failure for the same reason: a work tree that could not be removed is joined
// to the finding, and an operator who is told only the finding is left with a
// directory nobody mentioned.
func planError(err error) error {
	var policy *extract.PolicyError
	if errors.As(err, &policy) {
		return &checkError{summary: err.Error(), err: err}
	}
	return err
}

// resolvedPlanPaths are the absolute directories one plan run uses.
type resolvedPlanPaths struct {
	dir    string
	config string
	cache  string
	work   string
	out    string
	report string
}

// planPaths resolves every path the plan command accepts.
//
// Each one is made absolute here rather than deeper in the engine, because a
// relative path means "against the process working directory" only at the point
// the operator typed it, and the engine's containment checks are meaningless
// against a path that could still be reinterpreted.
func planPaths(env Env, flags *planFlags) (resolvedPlanPaths, error) {
	dir, err := filepath.Abs(env.resolve(*flags.dir))
	if err != nil {
		return resolvedPlanPaths{}, fmt.Errorf("resolve -dir: %w", err)
	}
	paths := resolvedPlanPaths{dir: dir}
	paths.config = resolveAgainst(dir, *flags.path)
	paths.cache = resolveAgainst(dir, *flags.cache)

	paths.work = filepath.Join(paths.cache, defaultWorkDir)
	if *flags.work != "" {
		paths.work = resolveAgainst(dir, *flags.work)
	}
	paths.out = filepath.Join(paths.work, defaultOutputName)
	if *flags.out != "" {
		paths.out = resolveAgainst(dir, *flags.out)
	}
	if *flags.report != "" {
		paths.report = resolveAgainst(dir, *flags.report)
	}
	return paths, nil
}

// resolveAgainst makes one operator supplied path absolute.
func resolveAgainst(dir, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(dir, path)
}

// planRef selects the single upstream ref the plan covers.
func planRef(flags *planFlags, cfg *config.Config) (extract.Ref, error) {
	switch {
	case *flags.branch != "":
		return extract.Ref{Kind: extract.RefBranch, Name: *flags.branch}, nil
	case *flags.tag != "":
		return extract.Ref{Kind: extract.RefTag, Name: *flags.tag}, nil
	case cfg.Source.Refs.MinimumRelease != "":
		return extract.Ref{Kind: extract.RefTag, Name: cfg.Source.Refs.MinimumRelease}, nil
	default:
		return extract.Ref{}, errors.New("the profile names no minimum release, so -tag or -branch is required")
	}
}

// planPatchBranch reports the branch a patch's branch selector is matched
// against.
//
// A patch is authored against a line of development rather than against one
// commit, and a release tag names no line, so a tag run has to derive one. The
// derivation is upstream's own convention, v1.36.1 comes from release-1.36, and
// it is used only when the profile actually tracks the branch it produces.
//
// Where the derivation does not answer, the operator does. A profile that
// carries patches and tracks several branches has no defensible default: taking
// the first listed one would silently match every patch's branch selector
// against a line the maintainer did not choose, and a selector matched against
// the wrong branch applies patches that were never meant for this release. A
// profile that carries no patches needs no branch at all, because nothing will
// be selected.
func planPatchBranch(flags *planFlags, cfg *config.Config, ref extract.Ref) (string, error) {
	switch {
	case *flags.patchBranch != "":
		return *flags.patchBranch, nil
	case ref.Kind == extract.RefBranch:
		return ref.Name, nil
	case len(cfg.Patches) == 0:
		return "", nil
	}

	tracked := cfg.Source.Refs.Branches
	if derived, ok := releaseBranchFor(ref.Name); ok && slices.Contains(tracked, derived) {
		return derived, nil
	}
	if len(tracked) == 1 {
		return tracked[0], nil
	}
	return "", fmt.Errorf(
		"the profile carries %d patches and %s names no line of development, so -patch-branch is required: tracked branches are %s",
		len(cfg.Patches), ref, describeBranches(tracked))
}

// releaseBranchFor derives the branch a release tag was cut from.
func releaseBranchFor(tag string) (string, bool) {
	version, err := config.ParseSemver(tag)
	if err != nil {
		return "", false
	}
	return releaseBranchPrefix + strconv.Itoa(version.Major) + "." + strconv.Itoa(version.Minor), true
}

// describeBranches renders a tracked branch list for a usage message.
func describeBranches(branches []string) string {
	if len(branches) == 0 {
		return "none"
	}
	return strings.Join(branches, ", ")
}

// writePlanOutput renders the plan to the requested stream and file.
//
// The two renderings are the same JSON when -format json is given, so a run that
// prints and writes cannot produce two different records of one plan. The report
// is encoded only when something is going to read it: a summary run with no
// -report has no use for the bytes, and encoding them anyway would turn an
// encoder failure into the failure of a command that never needed the encoder.
func writePlanOutput(ctx context.Context, env Env, reportPath, format string, result *extract.Result) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("write plan output: %w", err)
	}
	var encoded []byte
	if reportPath != "" || format == "json" {
		var err error
		if encoded, err = result.Report.JSON(); err != nil {
			return err
		}
	}
	if reportPath != "" {
		if err := os.MkdirAll(filepath.Dir(reportPath), 0o750); err != nil {
			return fmt.Errorf("write plan report: %w", err)
		}
		if err := os.WriteFile(reportPath, encoded, 0o600); err != nil {
			return fmt.Errorf("write plan report: %w", err)
		}
	}
	rendered := encoded
	if format == "summary" {
		rendered = []byte(result.Summary())
	}
	if _, err := env.Stdout.Write(rendered); err != nil {
		return fmt.Errorf("write plan output: %w", err)
	}
	return nil
}

// setFlags reports which flags the operator actually gave, which is how a
// default that happens to equal a request is told apart from the request.
func setFlags(fs *flag.FlagSet) map[string]bool {
	given := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	return given
}
