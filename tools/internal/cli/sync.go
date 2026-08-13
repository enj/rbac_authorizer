package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"

	"github.com/enj/soapbox/tools/internal/config"
	"github.com/enj/soapbox/tools/internal/generate"
	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/gocli"
	"github.com/enj/soapbox/tools/internal/sync"
)

// syncFlags holds the parsed sync flags.
//
// The generation flags are the generate command's, spelled and behaving
// identically, so an operator who generated a release synchronizes it by
// changing the verb. What is added is the destination: where the objects are
// written, where they would go, and whether this invocation is allowed to send
// them.
type syncFlags struct {
	*generateFlags
	destination *string
	remote      *string
	identity    *string
	localRemote *bool
	stateCommit *string
	apply       *bool
	approve     *string
}

func syncFlagSet() (*flag.FlagSet, *syncFlags) {
	fs := newFlagSet("sync")
	shared := registerRunFlags(fs, runSpec{
		verb:  "synchronize",
		tree:  "generated module",
		cache: "",
		work:  "<cache>" + generateWorkSuffix,
		out:   "<cache>" + generateOutSuffix,
	})
	return fs, &syncFlags{
		generateFlags: &generateFlags{
			runFlags: shared,
			proxy:    fs.String("proxy", gocli.DefaultProxy, "module proxy every Go command resolves through, or "+gocli.ProxyOff+" to resolve nothing"),
			index:    fs.String("version-index", "", "staging version index, relative to -dir when not absolute (default <cache>/"+defaultVersionIndex+")"),
		},
		destination: fs.String("destination", "", "local destination repository the objects are written into (required)"),
		remote:      fs.String("remote", "", "push target (default destination.remote from the profile)"),
		identity:    fs.String("identity", "", "canonical destination repository recorded in the manifest (default derived from the profile)"),
		localRemote: fs.Bool("local-remote", false, "permit a filesystem destination, which only a local dry run should need"),
		stateCommit: fs.String("state-commit", "", "previous state record to resume from, empty for a destination that holds none"),
		apply:       fs.Bool("apply", false, "publish the plan, which requires -approve and a reachable destination"),
		approve:     fs.String("approve", "", "the manifest hash being approved, required by -apply"),
	}
}

// runSync computes one complete synchronization and, when it is approved,
// publishes it.
//
// Every usage problem is decided before the profile is read, so a command line
// that cannot work fails the same way whether or not a profile, a cache, or a
// network happens to be there. Publication is off unless the operator asked for
// it and quoted the hash they are approving: the default outcome of this
// command is a manifest on stdout and nothing outward at all.
func runSync(ctx context.Context, env Env, args []string) error {
	fs, flags := syncFlagSet()
	if err := parseFlags(env, syncCommand(), fs, args); err != nil {
		return err
	}
	usage := commandUsage(syncCommand(), fs)
	given := setFlags(fs)

	if err := checkSyncFlags(flags, given); err != nil {
		return &usageError{err: err, usage: usage}
	}
	proxy, err := generateProxy(flags.generateFlags, given)
	if err != nil {
		return &usageError{err: err, usage: usage}
	}
	paths, err := generatePaths(env, flags.generateFlags)
	if err != nil {
		return &usageError{err: err, usage: usage}
	}
	destination, err := filepath.Abs(env.resolve(*flags.destination))
	if err != nil {
		return &usageError{err: fmt.Errorf("resolve -destination: %w", err), usage: usage}
	}

	cfg, err := config.Load(ctx, paths.config)
	if err != nil {
		return profileError(env, paths.config, err)
	}
	ref, err := selectedRef(flags.runFlags, cfg)
	if err != nil {
		return &usageError{err: err, usage: usage}
	}
	patchBranch, err := selectedPatchBranch(flags.runFlags, cfg, ref)
	if err != nil {
		return &usageError{err: err, usage: usage}
	}

	// The source runner is anonymous, exactly as a generation's is: reading
	// upstream talks to the public source host and to nothing else. The
	// destination runner is separate and is the only one a publication pushes
	// with, which is what keeps a credential from ever reaching a source read.
	sourceGit, err := gitcli.New(ctx, gitcli.Options{})
	if err != nil {
		return err
	}
	destinationGit, err := gitcli.New(ctx, gitcli.Options{Dir: destination, Inherit: []string{"PATH"}})
	if err != nil {
		return err
	}
	goRunner, err := generateGoRunner(ctx, paths.dir, proxy)
	if err != nil {
		return err
	}

	result, err := sync.Plan(ctx, sync.Options{
		Generate: generate.Options{
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
			Git:          sourceGit,
			Go:           goRunner,
		},
		Destination: sync.Destination{
			Git:              destinationGit,
			Remote:           syncRemote(flags, cfg),
			Identity:         syncIdentity(flags, cfg),
			AllowLocalRemote: *flags.localRemote,
		},
		StateCommit: *flags.stateCommit,
	})
	if err != nil {
		return syncError(err, usage)
	}

	if err := writeReportOutput(ctx, env, "sync", paths.report, *flags.format,
		result.Manifest.JSON, result.Manifest.Text); err != nil {
		return err
	}
	if !*flags.apply {
		return nil
	}
	applied, err := sync.Apply(ctx, result, sync.ApplyOptions{Approval: *flags.approve})
	// What a failed publication already did is reported before the failure is,
	// because it is the first thing an operator needs. A push that failed having
	// applied some of its refs, or one that failed after the bookkeeping half
	// landed, leaves a destination somebody has to reason about, and a bare
	// error would say only that something went wrong.
	writeApplied(env, applied)
	if err != nil {
		return syncError(err, usage)
	}
	return nil
}

// checkSyncFlags decides every contradiction the command line can hold, before
// anything is read.
//
// The approval pair is the one that matters. A publication asked for without a
// hash cannot be served, and a hash offered without a publication is an operator
// who believes they published and did not, so both halves are refused rather
// than one being taken as implying the other.
func checkSyncFlags(flags *syncFlags, given map[string]bool) error {
	if !slices.Contains(runFormats, *flags.format) {
		return fmt.Errorf("unsupported -format %q, want %s", *flags.format, strings.Join(runFormats, ", "))
	}
	if given["tag"] && given["branch"] {
		return errors.New("-tag and -branch select different refs, so only one may be given")
	}
	if *flags.branch != "" {
		return errors.New("a synchronization publishes a release, so -branch cannot select it")
	}
	if *flags.offline && given["fetch"] && *flags.fetch {
		return errors.New("-offline refuses every network operation, so -fetch cannot also be requested")
	}
	if *flags.destination == "" {
		return errors.New("a synchronization writes its objects into a destination repository, so -destination is required")
	}
	switch {
	case *flags.apply && *flags.approve == "":
		return errors.New("-apply publishes, so the manifest hash being approved must be given with -approve")
	case !*flags.apply && *flags.approve != "":
		return errors.New("-approve names a manifest to publish, so -apply must also be given")
	}
	return nil
}

// syncRemote reports the push target, which the profile decides unless the
// operator overrode it.
func syncRemote(flags *syncFlags, cfg *config.Config) string {
	if *flags.remote != "" {
		return *flags.remote
	}
	return cfg.Destination.Remote
}

// syncIdentity reports the canonical destination recorded in the manifest.
//
// It is derived from the profile's repository rather than from the remote,
// because a manifest describes a repository rather than a location and a local
// dry run's remote is a temporary directory. An https publication would derive
// the same value from its own remote, so the two agree.
func syncIdentity(flags *syncFlags, cfg *config.Config) string {
	if *flags.identity != "" {
		return *flags.identity
	}
	if cfg.Destination.Repository == "" {
		return ""
	}
	return "github.com/" + cfg.Destination.Repository
}

// writeApplied reports what a publication did, on stderr.
//
// Stdout carries the manifest and nothing else, so a workflow that captures it
// gets one artifact rather than an artifact with a log appended to it.
func writeApplied(env Env, applied *sync.ApplyResult) {
	if applied == nil {
		return
	}
	if applied.DryRun {
		fmt.Fprintf(env.Stderr, "soapbox: rehearsed, nothing was pushed\n")
		return
	}
	// A nil half is a half that was never attempted, which is what the consumer
	// half is when the bookkeeping half failed. It is reported as such rather
	// than skipped, because "not attempted" and "attempted and changed nothing"
	// are different destinations.
	for _, half := range []struct {
		name    string
		outcome *sync.Outcome
	}{
		{"non-consumer", applied.NonConsumer},
		{"consumer", applied.Consumer},
	} {
		switch {
		case half.outcome == nil:
			fmt.Fprintf(env.Stderr, "soapbox: %s refs were not attempted\n", half.name)
		case half.outcome.Failed && !half.outcome.Verified:
			fmt.Fprintf(env.Stderr,
				"soapbox: %s push failed and the destination could not be read afterwards, so %s may or may not have been published\n",
				half.name, strings.Join(half.outcome.Attempted, ", "))
		case half.outcome.Failed:
			fmt.Fprintf(env.Stderr, "soapbox: %s push failed: %s published, %s not\n",
				half.name, orNone(half.outcome.Pushed), orNone(half.outcome.Unapplied))
		case len(half.outcome.Pushed) == 0:
			fmt.Fprintf(env.Stderr, "soapbox: %s refs were already published\n", half.name)
		default:
			fmt.Fprintf(env.Stderr, "soapbox: published %s %s\n",
				half.name, strings.Join(half.outcome.Pushed, ", "))
		}
	}
}

// orNone renders a ref list for a person, naming the empty case rather than
// printing nothing where a list was promised.
func orNone(refs []string) string {
	if len(refs) == 0 {
		return "none"
	}
	return strings.Join(refs, ", ")
}

// syncError maps a synchronization failure onto the process exit code contract.
//
// A generation policy failure means the engine ran and the answer is no, which
// CI reads as something to review. A refused approval and a run shape this
// engine does not implement are the operator's to fix and are reported as usage
// problems, because in both cases the command line is what has to change.
// Everything else is a runtime failure or a cancellation and is left for the
// dispatcher to classify.
func syncError(err error, usage func(io.Writer)) error {
	var policy *generate.PolicyError
	if errors.As(err, &policy) {
		return &checkError{summary: err.Error(), err: err}
	}
	if errors.Is(err, generate.ErrPathConflict) {
		return &usageError{err: err, usage: usage}
	}
	if errors.Is(err, sync.ErrApproval) || errors.Is(err, sync.ErrPublicationDisabled) {
		return &usageError{err: err, usage: usage}
	}
	return err
}
