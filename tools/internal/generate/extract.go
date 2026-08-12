package generate

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/enj/soapbox/tools/internal/config"
	"github.com/enj/soapbox/tools/internal/extract"
)

// runExtract performs the two extraction passes the later phases compare.
//
// The generation needs two trees rather than one. The post-prune tree is the
// module being generated. The pre-prune tree is what that module would have been
// without the profile's pruning, and it is the only thing that can answer the
// two questions pruning raises: whether the published API changed, and whether
// the code that survived still means what it meant when the pruned code was
// present. Neither question can be asked of the post-prune tree alone, because
// the evidence pruning removed is exactly what is missing from it.
//
// The pre-prune tree is never a candidate for the final output. It is written
// into a scratch directory the run owns and removed on the way out, so a tree
// built from a profile variant the operator never wrote cannot be mistaken for
// the module.
func (r *run) runExtract(ctx context.Context) error {
	// The pre-prune pass runs first so a profile whose baseline cannot be built
	// fails before the run has spent a second extraction on it.
	pre, err := r.plan(ctx, prePruneConfig(r.cfg), preDirName, false)
	if err != nil {
		return err
	}
	r.pre = pre
	// Both passes use the same bare cache. Record the repository after the first
	// succeeds so cleanup can unregister a post-prune worktree even when that
	// second pass later refuses.
	r.paths.Cache = pre.Paths.Cache
	r.paths.PreWorktree = pre.Paths.Worktree

	post, err := r.plan(ctx, r.cfg, postDirName, r.opts.Strict)
	if err != nil {
		return err
	}
	r.post = post

	if err := r.checkPasses(); err != nil {
		return err
	}
	r.report.recordExtract(pre, post)
	// The configured cache root holds one bare repository per remote, and the
	// pass reports which one it opened. Later phases read upstream blobs out of
	// that repository rather than out of the root, which is not a repository at
	// all.
	r.paths.Cache = post.Paths.Cache
	r.paths.PreWorktree = pre.Paths.Worktree
	r.paths.PostWorktree = post.Paths.Worktree
	return nil
}

// plan runs one extraction pass into its own scratch subtree.
//
// Both passes materialize, because every later phase needs real files: the
// module phase tidies a directory, the facade phase type checks one, and the
// type policy loads the upstream sources out of the source work tree. Both keep
// their work trees for the same reason, and this run removes them itself rather
// than letting each pass remove the tree the next phase is about to read.
func (r *run) plan(ctx context.Context, cfg *config.Config, name string, strict bool) (*extract.Result, error) {
	base := filepath.Join(r.paths.Work, name)
	opts := extract.Options{
		Config:     cfg,
		ProfileDir: r.opts.ProfileDir,
		CacheRoot:  r.opts.CacheRoot,
		// Each pass gets its own work root so the two source trees coexist. The
		// cache is shared, which is the whole reason one clone serves the run.
		WorkRoot:     filepath.Join(base, sourceDirName),
		OutputRoot:   filepath.Join(base, moduleDirName),
		Ref:          r.opts.Ref,
		PatchBranch:  r.opts.PatchBranch,
		SourceRemote: r.opts.SourceRemote,
		Fetch:        r.opts.Fetch,
		Offline:      r.opts.Offline,
		Materialize:  true,
		KeepWorktree: true,
		Strict:       strict,
		Git:          r.opts.Git,
		LookupEnv:    r.opts.LookupEnv,
	}
	result, err := extract.Plan(ctx, opts)
	if result != nil && result.Paths.Cache != "" {
		// A failed pass may still have created a linked worktree. Cleanup needs
		// the concrete bare repository, not the cache root that contains it.
		r.paths.Cache = result.Paths.Cache
	}
	if result != nil && result.Paths.Worktree != "" {
		// The tree is recorded for cleanup even when the pass failed, because a
		// pass that refused after materializing still left one behind.
		r.worktrees = append(r.worktrees, result.Paths.Worktree)
	}
	if err != nil {
		// Extraction already separates the two kinds of failure, so the
		// classification is read from it rather than decided again here. A clone
		// that could not reach the remote, a cancelled context, and a blob the
		// cache does not hold all arrive as plain errors, and turning them into
		// refusals would send a reviewer to read a profile that is not the
		// problem.
		var policy *extract.PolicyError
		if errors.As(err, &policy) {
			return nil, policyError(stageExtract, fmt.Errorf("%s pass: %w", name, err))
		}
		return nil, runtimeError(stageExtract, fmt.Errorf("%s pass: %w", name, err))
	}
	if result.Paths.Worktree == "" {
		return nil, runtimeError(stageExtract, fmt.Errorf("%s pass: the source work tree was not retained, so the later phases have no upstream sources to read", name))
	}
	return result, nil
}

// checkPasses proves the two passes describe the same upstream commit.
//
// They resolve the ref independently, and a fetch between them could move a
// branch or a repository could move a tag. Everything downstream reads one pass
// for the upstream identity and the other for the baseline, so two passes over
// different commits would produce a comparison whose result means nothing.
func (r *run) checkPasses() error {
	if pre, post := r.pre.Report.Source.Commit, r.post.Report.Source.Commit; pre != post {
		return policyError(stageExtract, fmt.Errorf("the pre-prune pass read commit %s and the post-prune pass read commit %s, so the upstream ref moved while the generation was running", pre, post))
	}
	// The pre-prune tree must actually be a superset. A profile whose prune list
	// removes nothing would make the facade comparison vacuous, and the type
	// policy would be proving a substitution against the same tree twice.
	if r.pre.Report.Output.ManifestHash == r.post.Report.Output.ManifestHash && len(r.cfg.Prune.Files) > 0 {
		return policyError(stageExtract, fmt.Errorf("the profile prunes %d files but the pre-prune and post-prune trees are identical, so the prune list selects nothing", len(r.cfg.Prune.Files)))
	}
	return nil
}

// prePruneConfig derives the baseline profile from the real one.
//
// Exactly four fields are cleared, and each for a reason that would otherwise
// make the baseline impossible rather than merely different.
//
// Prune.Files is what the baseline exists to undo. Prune.Required is the
// assertion that those files were still there to remove, which is meaningless
// once nothing is removed. Deny.Imports names the packages the pruning takes out
// of the closure, so a baseline that kept the denials would refuse the very
// imports it is supposed to include. Closure.Golden pins the post-prune package
// set, and comparing the unpruned closure against it would fail on every profile
// that prunes anything at all.
//
// Everything else is preserved deliberately, including the closure limits. A
// baseline that quietly raised a limit would hide a profile whose unpruned
// closure is larger than the operator believes, and the limits are one of the
// few places that belief is written down.
func prePruneConfig(cfg *config.Config) *config.Config {
	clone := *cfg
	clone.Prune = config.Prune{}
	clone.Deny = config.Deny{}
	clone.Closure.Golden = ""
	return &clone
}
