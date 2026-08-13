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
	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/setup"
)

// setupFlags holds the parsed setup flags.
type setupFlags struct {
	dir     *string
	path    *string
	engine  *string
	sum     *string
	format  *string
	report  *string
	apply   *bool
	approve *string
}

func setupFlagSet() (*flag.FlagSet, *setupFlags) {
	fs := newFlagSet("setup")
	return fs, &setupFlags{
		dir:     fs.String("dir", ".", "template checkout to transform in place"),
		path:    fs.String("config", config.DefaultFileName, "profile path relative to -dir"),
		engine:  fs.String("engine-version", "", "immutable engine release the nested tools module pins, spelled v1.2.3 or "+setup.EngineTagPrefix+"v1.2.3"),
		sum:     fs.String("engine-sum", "", "file holding the verified go.sum content for the nested tools module"),
		format:  fs.String("format", runFormats[0], "output format: "+strings.Join(runFormats, ", ")),
		report:  fs.String("report", "", "write the manifest to this path, relative to -dir when not absolute"),
		apply:   fs.Bool("apply", false, "write the manifest instead of reporting it, which requires -approve"),
		approve: fs.String("approve", "", "manifest hash a dry run produced, which -apply must match exactly"),
	}
}

// runSetup transforms one template checkout into one derived repository.
//
// The command defaults to a dry run and stays that way unless the operator both
// asks to apply and names the manifest they read. Nothing outward happens here,
// so the approval is not a safety catch on a remote effect; it is the only way
// an operator can say which set of deletions they agreed to, since that set is
// enumerated in the manifest and nowhere else.
func runSetup(ctx context.Context, env Env, args []string) error {
	fs, flags := setupFlagSet()
	if err := parseFlags(env, setupCommand(), fs, args); err != nil {
		return err
	}
	usage := commandUsage(setupCommand(), fs)
	given := setFlags(fs)

	if !slices.Contains(runFormats, *flags.format) {
		return &usageError{
			err:   fmt.Errorf("unsupported -format %q, want %s", *flags.format, strings.Join(runFormats, ", ")),
			usage: usage,
		}
	}
	if *flags.engine == "" {
		return &usageError{
			err:   errors.New("-engine-version is required, because a derived repository pins one immutable engine release rather than tracking whatever the engine becomes"),
			usage: usage,
		}
	}
	// The two halves of an approval are checked against each other rather than
	// against a default, so neither can be supplied alone and mean something the
	// operator did not ask for.
	if given["approve"] && !*flags.apply {
		return &usageError{err: errors.New("-approve applies a manifest, so -apply is required with it"), usage: usage}
	}
	if *flags.apply && *flags.approve == "" {
		return &usageError{err: errors.New("-apply writes the manifest, so -approve must name the hash a dry run produced"), usage: usage}
	}

	paths, err := setupPaths(env, flags)
	if err != nil {
		return &usageError{err: err, usage: usage}
	}

	cfg, err := config.Load(ctx, paths.config)
	if err != nil {
		return profileError(env, paths.config, err)
	}
	engineSum, err := readEngineSum(paths.sum)
	if err != nil {
		return err
	}
	// The runner carries no caller supplied entry. Setup reads the tracked tree
	// and the work tree status of one local repository, so a runner that could
	// carry a publishing credential would be carrying it for no reason.
	git, err := gitcli.New(ctx, gitcli.Options{Dir: paths.dir})
	if err != nil {
		return err
	}

	opts := setup.Options{
		Root:          paths.dir,
		Config:        cfg,
		EngineVersion: *flags.engine,
		EngineSum:     engineSum,
		Git:           git,
	}
	var result *setup.Result
	if *flags.apply {
		result, err = setup.Apply(ctx, opts, *flags.approve)
	} else {
		result, err = setup.Plan(ctx, opts)
	}

	// A refused setup still produced the manifest whenever it got far enough to
	// compute one, and that manifest is what tells the operator which finding to
	// act on, so it is written before the failure is reported. A failed run keeps
	// stdout clean: printing a manifest that was not applied would make a refusal
	// look like the result the operator asked for.
	var writeErr error
	if result != nil {
		switch {
		case err == nil:
			writeErr = writeReportOutput(ctx, env, "setup", paths.report, *flags.format, result.Report.JSON, result.Summary)
		case paths.report != "":
			quiet := env
			quiet.Stdout = io.Discard
			writeErr = writeReportOutput(ctx, quiet, "setup", paths.report, *flags.format, result.Report.JSON, result.Summary)
		}
	}
	return setupError(errors.Join(err, writeErr))
}

// setupError maps a setup failure onto the process exit code contract.
//
// A refused repository means the command ran and the answer is no, which is a
// finding to review rather than a crash. Everything else, cancellation included,
// is left for the dispatcher to classify, and the cause travels with it.
func setupError(err error) error {
	var policy *setup.PolicyError
	if errors.As(err, &policy) {
		return &checkError{summary: err.Error(), err: err}
	}
	return err
}

// resolvedSetupPaths are the absolute locations one setup uses.
type resolvedSetupPaths struct {
	dir    string
	config string
	sum    string
	report string
}

// setupPaths resolves every path the setup command accepts.
//
// The manifest is refused a destination inside the repository being transformed.
// Setup requires a clean work tree, so a manifest written into that tree would
// make the very next command refuse to run, and an operator would be left
// deleting the artifact the previous command told them to read.
func setupPaths(env Env, flags *setupFlags) (resolvedSetupPaths, error) {
	dir, err := filepath.Abs(env.resolve(*flags.dir))
	if err != nil {
		return resolvedSetupPaths{}, fmt.Errorf("resolve -dir: %w", err)
	}
	paths := resolvedSetupPaths{dir: dir}
	paths.config = resolveAgainst(dir, *flags.path)
	if *flags.sum != "" {
		paths.sum = resolveAgainst(dir, *flags.sum)
	}
	if *flags.report != "" {
		paths.report = resolveAgainst(dir, *flags.report)
		if pathContains(dir, paths.report) {
			return resolvedSetupPaths{}, fmt.Errorf(
				"-report %s must be outside the repository %s, because setup refuses a work tree that has uncommitted files in it", paths.report, dir)
		}
	}
	return paths, nil
}

// readEngineSum reads the verified checksums for the nested tools module.
//
// They arrive as a file rather than as a flag value because a go.sum is several
// lines long, and they are optional because the checksum of an engine release
// cannot be derived from a checkout. The engine explains what an absent one
// means; this only reads the bytes.
func readEngineSum(path string) ([]byte, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path) //nolint:gosec // the path is the operator's own -engine-sum argument
	if err != nil {
		return nil, fmt.Errorf("read -engine-sum: %w", err)
	}
	return data, nil
}
