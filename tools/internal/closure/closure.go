// Package closure computes the package granular Go import closure of an
// already materialized upstream worktree, asserts a profile's exact pruning and
// denied imports against it, and plans the files a portable build needs.
//
// The package does no network or version control work. It is handed a directory
// that some earlier phase checked out and it reads, prunes, and measures that
// directory only. Every path it touches is resolved through os.Root, so a
// hostile or merely surprising tree cannot make it read or delete anything
// outside the directory it was given.
//
// Three properties shape the whole package.
//
// Package granularity. A configured root contributes its own files, never its
// subdirectories. A sibling subpackage joins the closure exactly when something
// imports it or an operator configures it. That is what keeps the RBAC profile
// from dragging in bootstrappolicy merely for living next door.
//
// Portability. Every non-test Go file is parsed regardless of its build
// constraints and filename platform suffix, so the closure is the union across
// platforms rather than the set for whichever machine happens to run the
// engine. The generated module has to build everywhere the upstream package
// does.
//
// Fail closed. A prune target that is not there, a denied import that came
// back, a required file that did not survive, a package that lost its last
// file, a symbolic link where a regular file belongs, an embed that reaches
// into a nested module, a pattern that matched nothing: each stops the run with
// a typed error naming the file or import responsible. The generated module is
// published under immutable tags, so a closure that cannot be proved is never
// emitted in a reduced form.
package closure

import (
	"context"
	"fmt"
)

// Builder computes the closure of one materialized worktree.
//
// A Builder is reused across the extraction pipeline's patch passes. Build
// reasserts pruning and recomputes the closure each time it is called, and the
// Builder remembers which prune entries it has already removed so that a second
// pass over an already pruned tree stays idempotent while a prune target that
// upstream genuinely renamed still fails.
//
// That memory is what makes the Builder, rather than the worktree, the unit of
// idempotence. Nothing on disk records that soapbox removed a file, so a fresh
// Builder handed an already pruned tree cannot tell a completed prune from an
// upstream rename and fails with ErrPruneMissing naming the first entry it
// cannot find. This is deliberate and is not weakened: the alternative is
// treating an absent file as proof of the very removal being asserted, and the
// module is published under an immutable tag, so a wrong guess ships. A caller
// resuming after a crash or an abandoned run therefore rematerializes the
// worktree and builds again from the tree as upstream produced it, which is also
// the only state in which the pre-prune baseline means what it says.
//
// A Builder is bound to one worktree path and is not safe for concurrent use.
type Builder struct {
	opts    Options
	applied map[string]bool
	// prunable is the configured prune set as a lookup. It marks the files whose
	// content the tolerant pre-prune pass is allowed to fail to read, since those
	// are the files the profile has already committed to removing.
	prunable map[string]bool
	// baseline is the pre-prune measurement taken on the first Build.
	//
	// It is remembered rather than recomputed because "pre-prune" names the
	// state of upstream before soapbox removed anything, not the state before
	// the most recent pass. A pipeline that patches and rebuilds to a fixed
	// point would otherwise end with a report claiming pruning removed nothing,
	// since by the final pass there is nothing left to remove.
	baseline *Counts
}

// New validates the options and returns a Builder bound to them.
//
// Validation is complete and reported at once, because an operator editing a
// profile is better served by every problem than by the first one. No
// filesystem work happens here: a Builder is cheap to construct and the
// worktree it describes may not be checked out yet.
func New(ctx context.Context, opts Options) (*Builder, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("new closure builder: %w", err)
	}
	normalized := opts.clone()
	normalized.normalize()
	if err := normalized.validate(); err != nil {
		return nil, err
	}
	prunable := make(map[string]bool, len(normalized.PruneFiles))
	for _, file := range normalized.PruneFiles {
		prunable[file] = true
	}
	return &Builder{
		opts:     normalized,
		applied:  make(map[string]bool, len(normalized.PruneFiles)),
		prunable: prunable,
	}, nil
}

// Options returns a copy of the validated, normalized options.
func (b *Builder) Options() Options { return b.opts.clone() }

// Build computes one complete closure.
//
// The order is deliberate. The closure is discovered once as found, so the
// report can state what upstream actually contains and so prune entries can be
// checked against a real package set. Pruning is then asserted and applied.
// Only then is the closure recomputed, and only that second pass enforces
// denied imports and package name consistency: a profile prunes precisely
// because a file is the sole importer of something it does not want, and
// judging the tree before the prune would reject the profile for expressing its
// own purpose.
func (b *Builder) Build(ctx context.Context) (result *Result, err error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("build closure: %w", err)
	}
	w, err := openWorktree(ctx, b.opts.Root)
	if err != nil {
		return nil, err
	}
	defer func() {
		if cerr := w.Close(); cerr != nil && err == nil {
			result, err = nil, cerr
		}
	}()

	// Assets are configured against the worktree root rather than against any
	// package, so nothing but the tree itself decides what they select. They are
	// resolved once per pass rather than once per build: a glob asks a question
	// about the tree, pruning changes the tree, and carrying a pre-prune match
	// into the post-prune plan would list a file that is no longer there. The
	// copier would then fail on a path the report had already counted.
	preAssets, err := resolveAssets(ctx, w, b.opts.AssetGlobs)
	if err != nil {
		return nil, err
	}

	preMode := scanMode{includeTests: b.opts.IncludeTests, prunable: b.prunable}
	preRoots, err := expandRoots(ctx, w, b.opts.Roots, b.opts.Recursive, b.opts.IncludeTests)
	if err != nil {
		return nil, err
	}
	pre, err := buildClosure(ctx, w, preRoots, b.opts.ImportPrefix, preMode, nil)
	if err != nil {
		return nil, err
	}
	prePlan, err := buildCopyPlan(ctx, w, pre, preAssets, true)
	if err != nil {
		return nil, err
	}

	// The baseline is fixed here, while the tree is still the one upstream
	// produced and before anything has been removed. Recording it after the pass
	// succeeded would lose it whenever a pass fails, and a failed pass is
	// expected: the pipeline patches and rebuilds, so the run that finally
	// succeeds is usually not the run that did the pruning. Its report would then
	// claim pruning removed nothing.
	if b.baseline == nil {
		preCounts := countClosure(pre, prePlan)
		b.baseline = &preCounts
	}
	baseline := *b.baseline

	removed, err := b.applyPrune(ctx, w, pre)
	if err != nil {
		return nil, err
	}

	// Roots are expanded again because a recursive root can lose a subdirectory
	// when pruning takes its last Go file.
	postRoots, err := expandRoots(ctx, w, b.opts.Roots, b.opts.Recursive, b.opts.IncludeTests)
	if err != nil {
		return nil, err
	}
	postMode := scanMode{includeTests: b.opts.IncludeTests, strict: true}
	post, err := buildClosure(ctx, w, postRoots, b.opts.ImportPrefix, postMode, b.opts.DeniedImports)
	if err != nil {
		return nil, err
	}
	postAssets, err := resolveAssets(ctx, w, b.opts.AssetGlobs)
	if err != nil {
		return nil, err
	}
	postPlan, err := buildCopyPlan(ctx, w, post, postAssets, false)
	if err != nil {
		return nil, err
	}

	if err := checkRequired(b.opts.RequiredFiles, post, postPlan); err != nil {
		return nil, err
	}

	postCounts := countClosure(post, postPlan)
	growth := Growth{
		RootPackages:        len(postRoots),
		PackagesBeyondRoots: postCounts.Packages - len(postRoots),
		PackagesRemoved:     baseline.Packages - postCounts.Packages,
		GoFilesRemoved:      baseline.GoFiles - postCounts.GoFiles,
		FilesRemoved:        baseline.Files - postCounts.Files,
		NonTestLinesRemoved: baseline.NonTestLines - postCounts.NonTestLines,
	}
	if err := checkLimits(b.opts.Limits, postCounts, growth); err != nil {
		return nil, err
	}

	return &Result{
		Packages:     newPackages(post),
		CopyPlan:     postPlan,
		RemovedFiles: removed,
		Report: ClosureReport{
			Exact: ExactShape{
				ImportPrefix:     b.opts.ImportPrefix,
				Roots:            cloneStrings(postRoots),
				Packages:         post.importPaths(),
				Files:            planPaths(postPlan),
				ExternalPackages: post.external,
				StandardPackages: post.standard,
				PrunedFiles:      cloneStrings(b.opts.PruneFiles),
				DeniedImports:    cloneStrings(b.opts.DeniedImports),
			},
			Observed: ObservedShape{
				PrePrune:  baseline,
				PostPrune: postCounts,
				Growth:    growth,
			},
		},
	}, nil
}
