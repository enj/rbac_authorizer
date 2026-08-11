package gitcli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

// Worktree is one registered work tree of a repository.
type Worktree struct {
	// Path is the absolute work tree directory.
	Path string
	// Head is the checked out commit, empty for a bare repository entry.
	Head string
	// Branch is the checked out branch, empty when HEAD is detached.
	Branch string
	// Bare reports the entry that describes the repository itself.
	Bare bool
	// Detached reports a work tree that is not on a branch, which is what every
	// materialization uses so a checkout can never move a ref.
	Detached bool
}

// WorktreeOptions describes one work tree to create.
type WorktreeOptions struct {
	// Path is the absolute directory to create. It must not exist yet.
	Path string
	// Commit is the revision to check out. It is always detached, because a
	// materialization must never advance a branch in the shared cache.
	Commit string
	// NoCheckout leaves the work tree empty so that a sparse pattern set can be
	// installed before any file is written. Without it a full checkout of the
	// source repository happens first and the sparse set only prunes it again,
	// which for a blobless clone means downloading every blob in the tree.
	NoCheckout bool
}

// AddWorktree registers an isolated work tree at a detached commit.
func (r *Runner) AddWorktree(ctx context.Context, opts WorktreeOptions) error {
	if err := validateAbsolutePath("worktree path", opts.Path); err != nil {
		return fmt.Errorf("git worktree add: %w", err)
	}
	if err := validateRevision(opts.Commit); err != nil {
		return fmt.Errorf("git worktree add: %w", err)
	}
	args := []string{"worktree", "add", "--detach", "--quiet"}
	if opts.NoCheckout {
		args = append(args, "--no-checkout")
	}
	args = append(args, "--end-of-options", opts.Path, opts.Commit)
	if _, err := r.run(ctx, args...); err != nil {
		return fmt.Errorf("git worktree add %q: %w", opts.Path, err)
	}
	return nil
}

// ListWorktrees reports every work tree registered with this repository,
// including the repository's own entry.
func (r *Runner) ListWorktrees(ctx context.Context) ([]Worktree, error) {
	out, err := r.run(ctx, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("git worktree list: %w", err)
	}

	var (
		worktrees []Worktree
		current   Worktree
		open      bool
	)
	flush := func() {
		if open {
			worktrees = append(worktrees, current)
		}
		current, open = Worktree{}, false
	}
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			flush()
			continue
		}
		key, value, _ := strings.Cut(line, " ")
		switch key {
		case "worktree":
			flush()
			current, open = Worktree{Path: value}, true
		case "HEAD":
			current.Head = value
		case "branch":
			current.Branch = value
		case "bare":
			current.Bare = true
		case "detached":
			current.Detached = true
		}
	}
	flush()
	return worktrees, nil
}

// RemoveWorktree unregisters a work tree and deletes its directory. Removing a
// work tree that is not registered succeeds, so cleanup after a failed run is
// idempotent and a deferred removal never masks the original error.
func (r *Runner) RemoveWorktree(ctx context.Context, path string) error {
	if err := validateAbsolutePath("worktree path", path); err != nil {
		return fmt.Errorf("git worktree remove: %w", err)
	}
	worktrees, err := r.ListWorktrees(ctx)
	if err != nil {
		return fmt.Errorf("git worktree remove %q: %w", path, err)
	}
	registered := false
	for _, worktree := range worktrees {
		if worktree.Bare {
			continue
		}
		if sameFile(worktree.Path, path) {
			registered = true
			break
		}
	}
	if !registered {
		return nil
	}
	// The force flag covers the expected case: a materialized work tree holds
	// generated files that were never meant to be preserved.
	if _, err := r.run(ctx, "worktree", "remove", "--force", "--end-of-options", path); err != nil {
		return fmt.Errorf("git worktree remove %q: %w", path, err)
	}
	return nil
}

// PruneWorktrees drops administrative entries whose directory is already gone.
func (r *Runner) PruneWorktrees(ctx context.Context) error {
	if _, err := r.run(ctx, "worktree", "prune"); err != nil {
		return fmt.Errorf("git worktree prune: %w", err)
	}
	return nil
}

// SparseOptions selects the files a work tree materializes.
type SparseOptions struct {
	// Cone restricts patterns to directory prefixes, which git can match without
	// consulting every path. A cone pattern always includes subdirectories, so
	// package granularity needs the pattern form instead.
	Cone bool
	// Patterns are gitignore style patterns applied in order, later patterns
	// winning. A directory that matches is included with everything below it, so
	// materializing one package without its subpackages needs an explicit
	// negative pattern such as !/pkg/apis/rbac/v1/*/ after /pkg/apis/rbac/v1/*.
	Patterns []string
}

// SetSparseCheckout installs the pattern set that decides which paths a work
// tree materializes.
//
// Patterns are passed after the option terminator and never through standard
// input, and no path separator is appended, because git writes the argument
// vector verbatim into the pattern file: an extra separator would become a
// pattern of its own.
func (r *Runner) SetSparseCheckout(ctx context.Context, opts SparseOptions) error {
	if len(opts.Patterns) == 0 {
		return errors.New("git sparse-checkout: at least one pattern is required")
	}
	for _, pattern := range opts.Patterns {
		if err := validateSparsePattern(pattern); err != nil {
			return fmt.Errorf("git sparse-checkout: %w", err)
		}
	}
	args := []string{"sparse-checkout", "set"}
	if opts.Cone {
		args = append(args, "--cone")
	} else {
		args = append(args, "--no-cone")
	}
	args = append(args, "--end-of-options")
	args = append(args, opts.Patterns...)
	if _, err := r.run(ctx, args...); err != nil {
		return fmt.Errorf("git sparse-checkout: %w", err)
	}
	return nil
}

// SparseCheckoutPatterns reports the installed pattern set, which is how a run
// proves the work tree it measured is the work tree it asked for.
func (r *Runner) SparseCheckoutPatterns(ctx context.Context) ([]string, error) {
	out, err := r.run(ctx, "sparse-checkout", "list")
	if err != nil {
		return nil, fmt.Errorf("git sparse-checkout list: %w", err)
	}
	var patterns []string
	for line := range strings.SplitSeq(out, "\n") {
		if line = strings.TrimRight(line, "\r"); line != "" {
			patterns = append(patterns, line)
		}
	}
	return patterns, nil
}

// DisableSparseCheckout removes the pattern set and materializes every path.
func (r *Runner) DisableSparseCheckout(ctx context.Context) error {
	if _, err := r.run(ctx, "sparse-checkout", "disable"); err != nil {
		return fmt.Errorf("git sparse-checkout disable: %w", err)
	}
	return nil
}

// CheckoutDetached materializes one commit without moving any branch. It is the
// step that populates a work tree created with NoCheckout, and it honours the
// sparse pattern set installed beforehand.
func (r *Runner) CheckoutDetached(ctx context.Context, revision string) error {
	if err := validateRevision(revision); err != nil {
		return fmt.Errorf("git checkout: %w", err)
	}
	if _, err := r.run(ctx, "checkout", "--detach", "--quiet", "--end-of-options", revision); err != nil {
		return fmt.Errorf("git checkout %q: %w", r.redactor.String(revision), err)
	}
	return nil
}

// ResetHard discards index and work tree state and moves HEAD to revision. It is
// only ever used on a work tree the engine created, never on an operator's.
func (r *Runner) ResetHard(ctx context.Context, revision string) error {
	if err := validateRevision(revision); err != nil {
		return fmt.Errorf("git reset: %w", err)
	}
	if _, err := r.run(ctx, "reset", "--hard", "--quiet", "--end-of-options", revision); err != nil {
		return fmt.Errorf("git reset %q: %w", r.redactor.String(revision), err)
	}
	return nil
}

// Clean removes untracked and ignored files. Ignored files are included because
// a materialized work tree must contain exactly what the run put there, and an
// upstream .gitignore would otherwise hide leftovers from a previous pass.
func (r *Runner) Clean(ctx context.Context) error {
	if _, err := r.run(ctx, "clean", "--force", "-d", "-x", "--quiet"); err != nil {
		return fmt.Errorf("git clean: %w", err)
	}
	return nil
}

// StatusEntry is one changed path reported by git status.
type StatusEntry struct {
	// Code is the two character porcelain status code, such as " M" for a
	// modified file, "??" for an untracked one, or "UU" for a conflict.
	Code string
	// Path is the repository relative path.
	Path string
}

// Conflicted reports an unmerged path, which is how a three way patch
// application signals that it left conflict markers behind.
func (e StatusEntry) Conflicted() bool {
	if len(e.Code) != 2 {
		return false
	}
	x, y := e.Code[0], e.Code[1]
	return x == 'U' || y == 'U' || (x == 'A' && y == 'A') || (x == 'D' && y == 'D')
}

// Status reports the work tree state. Rename detection is off so that every
// record is exactly one path and the parse cannot depend on a similarity score.
func (r *Runner) Status(ctx context.Context) ([]StatusEntry, error) {
	out, err := r.StatusPorcelainZ(ctx)
	if err != nil {
		return nil, err
	}
	var entries []StatusEntry
	for record := range strings.SplitSeq(out, "\x00") {
		if len(record) < 4 {
			continue
		}
		entries = append(entries, StatusEntry{Code: record[:2], Path: record[3:]})
	}
	return entries, nil
}

// StatusPorcelainZ reports the raw null separated porcelain status, for a caller
// that wants to record exactly what git said rather than a parsed form.
//
// Every record is a two character code, a space, and one path. Rename detection
// is off, because a rename record carries two null separated paths instead of
// one and would desynchronise a reader that assumed one path per record.
func (r *Runner) StatusPorcelainZ(ctx context.Context) (string, error) {
	out, err := r.run(ctx, "status", "--porcelain", "-z", "--untracked-files=all", "--no-renames")
	if err != nil {
		return "", fmt.Errorf("git status: %w", err)
	}
	return out, nil
}

// DiffOptions selects the comparison a diff renders.
type DiffOptions struct {
	// Revision compares against a commit instead of the index. Empty compares
	// the work tree with the index, which is what a failed patch leaves behind.
	Revision string
	// Staged compares the index instead of the work tree.
	Staged bool
	// Paths limits the diff to exact repository relative paths.
	Paths []string
}

// Diff renders a unified diff for a failure report. Colour and external diff
// drivers are disabled so the captured text is the same on every machine.
func (r *Runner) Diff(ctx context.Context, opts DiffOptions) (string, error) {
	args := []string{"diff", "--no-color", "--no-ext-diff"}
	if opts.Staged {
		args = append(args, "--cached")
	}
	args = append(args, "--end-of-options")
	if opts.Revision != "" {
		if err := validateRevision(opts.Revision); err != nil {
			return "", fmt.Errorf("git diff: %w", err)
		}
		args = append(args, opts.Revision)
	}
	args = append(args, "--")
	for _, path := range opts.Paths {
		if err := validateArgument("path", path); err != nil {
			return "", fmt.Errorf("git diff: %w", err)
		}
		if err := validateLiteralPath(path); err != nil {
			return "", fmt.Errorf("git diff: %w", err)
		}
		args = append(args, path)
	}
	out, err := r.runWith(ctx, []string{"GIT_LITERAL_PATHSPECS=1"}, args...)
	if err != nil {
		return "", fmt.Errorf("git diff: %w", err)
	}
	return out, nil
}

// DiffWorkTree renders the unified diff of the work tree against the index,
// which is what a failed three way application leaves behind, conflict markers
// included. It is the narrow form of Diff that a conflict report wants.
func (r *Runner) DiffWorkTree(ctx context.Context) (string, error) {
	return r.Diff(ctx, DiffOptions{})
}

// ApplyOptions describes one patch application.
type ApplyOptions struct {
	// Patch is the unified diff to apply. It travels through standard input so
	// no temporary file has to be created and cleaned up.
	Patch []byte
	// ThreeWay falls back to a three way merge when a context line does not
	// match, which is what lets a patch survive unrelated upstream drift. A
	// conflict leaves markers in the work tree and fails the command, so the
	// caller can capture them for the report.
	ThreeWay bool
	// Index updates the index as well as the work tree.
	Index bool
	// Check reports whether the patch would apply without touching anything.
	Check bool
	// Strip is the number of leading path components to remove. Zero means git's
	// default of one, which matches diffs produced against a repository root.
	Strip int
}

// ApplyPatch applies a unified diff to the current work tree.
//
// Paths that escape the work tree are refused by git itself because
// --unsafe-paths is never passed, so a hostile patch cannot write outside the
// materialized tree.
func (r *Runner) ApplyPatch(ctx context.Context, opts ApplyOptions) error {
	if len(opts.Patch) == 0 {
		return errors.New("git apply: patch must not be empty")
	}
	if opts.Strip < 0 {
		return fmt.Errorf("git apply: strip %d must not be negative", opts.Strip)
	}
	args := []string{"apply"}
	if opts.ThreeWay {
		args = append(args, "--3way")
	}
	if opts.Index {
		args = append(args, "--index")
	}
	if opts.Check {
		args = append(args, "--check")
	}
	if opts.Strip > 0 {
		args = append(args, "-p"+strconv.Itoa(opts.Strip))
	}
	if _, err := r.runInput(ctx, opts.Patch, nil, args...); err != nil {
		return fmt.Errorf("git apply: %w", err)
	}
	return nil
}

// ApplyThreeWayIndex applies one unified diff with three way and index
// semantics and no way to weaken either.
//
// Both are load bearing for patch replay. A strict apply would reject a patch
// whose context drifted by one line and report nothing a maintainer could act
// on, and an apply that skipped the index would hide the patched files from the
// pruning and tree building that follow. Offering them as a single call means a
// caller cannot turn one of them off by accident.
func (r *Runner) ApplyThreeWayIndex(ctx context.Context, diff []byte) error {
	return r.ApplyPatch(ctx, ApplyOptions{Patch: diff, ThreeWay: true, Index: true})
}

// validateAbsolutePath rejects a path git would parse as an option and a path
// that depends on the process working directory.
func validateAbsolutePath(kind, path string) error {
	if err := validateArgument(kind, path); err != nil {
		return err
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("%s %q must be absolute", kind, path)
	}
	return nil
}

// validateSparsePattern rejects a pattern git would parse as an option and a
// pattern that would not survive the round trip through the pattern file, which
// is line based.
func validateSparsePattern(pattern string) error {
	switch {
	case pattern == "":
		return errors.New("sparse pattern must not be empty")
	case strings.HasPrefix(pattern, "-"):
		return fmt.Errorf("sparse pattern %q: %w", pattern, ErrFlagLikeArgument)
	case strings.ContainsAny(pattern, "\n\r\x00"):
		return fmt.Errorf("sparse pattern %q must not contain a line break or a null byte", pattern)
	case strings.TrimSpace(pattern) == "":
		return errors.New("sparse pattern must not be blank")
	}
	return nil
}

// sameFile reports whether two paths name the same location once symbolic links
// and relative elements are resolved. Git reports the resolved path of a work
// tree, which on a system with a symlinked temporary directory differs
// textually from the path the caller passed in.
func sameFile(a, b string) bool {
	if a == b {
		return true
	}
	resolvedA, errA := filepath.EvalSymlinks(a)
	resolvedB, errB := filepath.EvalSymlinks(b)
	if errA != nil || errB != nil {
		return false
	}
	return resolvedA == resolvedB
}
