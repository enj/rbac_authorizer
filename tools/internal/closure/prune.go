package closure

import (
	"context"
	"fmt"
	"io/fs"
	"path"
	"strings"
)

// pruneTarget is one validated prune entry awaiting removal.
type pruneTarget struct {
	path   string
	dir    string
	goFile bool
	test   bool
}

// applyPrune removes the exact configured prune entries from the materialized
// worktree and reports the ones this pass removed.
//
// Validation is complete before the first removal. A profile with any invalid
// prune entry therefore leaves the worktree exactly as it found it, which is
// what makes a failed run safe to rerun after the profile is corrected.
//
// Pruning is asserted on every pass, not applied once. The extraction pipeline
// applies patches and recomputes the closure until it reaches a fixed point, and
// a patch is entitled to reintroduce a file the profile prunes. What must not
// happen is the opposite mistake: a prune entry that never matched anything is
// an upstream rename, and it fails closed. The builder distinguishes the two by
// remembering what it has already removed, so a second pass over an already
// pruned tree is idempotent while a genuinely absent target still fails.
func (b *Builder) applyPrune(ctx context.Context, w *worktree, pre *closureSet) ([]string, error) {
	targets, err := b.validatePruneTargets(ctx, w, pre)
	if err != nil {
		return nil, err
	}
	if err := checkLastFile(pre, targets); err != nil {
		return nil, err
	}

	removed := make([]string, 0, len(targets))
	for _, target := range targets {
		if err := w.remove(ctx, target.path); err != nil {
			return nil, err
		}
		b.applied[target.path] = true
		removed = append(removed, target.path)
	}
	return removed, nil
}

// validatePruneTargets proves every prune entry is removable before anything is
// removed.
func (b *Builder) validatePruneTargets(ctx context.Context, w *worktree, pre *closureSet) ([]pruneTarget, error) {
	targets := make([]pruneTarget, 0, len(b.opts.PruneFiles))
	for _, entry := range b.opts.PruneFiles {
		info, err := w.lstat(ctx, entry)
		switch {
		case isNotFound(err):
			// Absent and never removed by this builder means the profile names
			// a file upstream no longer has.
			if b.applied[entry] {
				continue
			}
			return nil, &FileError{File: entry, Err: ErrPruneMissing}
		case err != nil:
			return nil, fmt.Errorf("inspect prune target %s: %w", entry, err)
		case info.Mode()&fs.ModeSymlink != 0:
			return nil, &FileError{File: entry, Err: ErrUnsafeSymlink}
		case info.IsDir():
			return nil, &FileError{
				File: entry,
				Err:  fmt.Errorf("%w: prune entries name files, never directories", ErrPruneMissing),
			}
		}

		dir := path.Dir(entry)
		if dir == "." {
			dir = ""
		}
		pkg, ok := pre.packages[dir]
		if !ok {
			return nil, &FileError{File: entry, Err: ErrPruneOutsideClosure}
		}
		target, ok := classifyPruneTarget(pkg, entry, dir)
		if !ok {
			// The file exists inside a materialized package directory but is not
			// one of that package's build inputs, so the closure would never have
			// copied it and pruning it changes nothing.
			return nil, &FileError{File: entry, Err: b.notMaterialized(entry)}
		}
		targets = append(targets, target)
	}
	return targets, nil
}

// notMaterialized explains why a file sitting inside a materialized package
// directory is still not one of that package's build inputs.
//
// The excluded test case is called out because it is the one an operator can
// resolve two ways. A profile that prunes a _test.go file while leaving
// includeTests false has either named the wrong file or set the wrong flag, and
// a message about build inputs would not say which question to ask.
func (b *Builder) notMaterialized(entry string) error {
	if !b.opts.IncludeTests && strings.HasSuffix(entry, "_test.go") {
		return ErrPruneExcludedTest
	}
	return ErrPruneNotMaterialized
}

// classifyPruneTarget locates an entry among a package's build inputs.
func classifyPruneTarget(pkg *pkgScan, entry, dir string) (pruneTarget, bool) {
	for _, file := range pkg.goFiles {
		if file.path == entry {
			return pruneTarget{path: entry, dir: dir, goFile: true, test: file.test}, true
		}
	}
	for _, companion := range pkg.companions {
		if companion.path == entry {
			return pruneTarget{path: entry, dir: dir}, true
		}
	}
	return pruneTarget{}, false
}

// checkLastFile refuses a prune set that would leave a materialized package
// without the Go files it needs to stay a package.
//
// A package that should leave the closure leaves it by losing its importers,
// which is exactly how the RBAC profile drops pkg/apis/rbac without pruning a
// single one of its files. Deleting a package's final file instead either
// breaks a retained importer or expresses an intent the import graph already
// expresses better, so it is always a profile mistake.
//
// There are two ways to make that mistake and both are named here. Removing
// every non-test file leaves a package nothing can import. Removing every Go
// file, which only a test-only package included by includeTests can suffer,
// leaves no package at all; without this the run would fail several steps later
// with a generic report that the directory holds no Go files, naming the
// package rather than the prune entry that emptied it.
func checkLastFile(pre *closureSet, targets []pruneTarget) error {
	type tally struct{ goFiles, nonTest int }

	pruned := make(map[string]tally, len(targets))
	for _, target := range targets {
		if !target.goFile {
			continue
		}
		count := pruned[target.dir]
		count.goFiles++
		if !target.test {
			count.nonTest++
		}
		pruned[target.dir] = count
	}

	for _, dir := range sortedKeys(pruned) {
		pkg := pre.packages[dir]
		var total tally
		for _, file := range pkg.goFiles {
			total.goFiles++
			if !file.test {
				total.nonTest++
			}
		}
		count := pruned[dir]
		if count.goFiles >= total.goFiles || (total.nonTest > 0 && count.nonTest >= total.nonTest) {
			return &PackageError{Dir: displayPath(dir), ImportPath: pkg.importPath, Err: ErrPruneLastFile}
		}
	}
	return nil
}

// checkRequired proves every required file survived into the post-prune
// closure.
//
// Presence on disk is not enough. A required file whose package dropped out of
// the closure is not retained by the generated module, and treating that as
// success is precisely the silent shrink this check exists to prevent.
func checkRequired(required []string, post *closureSet, plan []CopyEntry) error {
	retained := make(map[string]bool, len(plan))
	for _, entry := range plan {
		retained[entry.Path] = true
	}
	for _, file := range required {
		if retained[file] {
			continue
		}
		dir := path.Dir(file)
		if dir == "." {
			dir = ""
		}
		err := ErrRequiredMissing
		if _, ok := post.packages[dir]; !ok {
			err = fmt.Errorf("%w: package %s is no longer in the closure", ErrRequiredMissing, displayPath(dir))
		}
		return &FileError{File: file, Err: err}
	}
	return nil
}

// checkLimits reports the first exceeded limit in a fixed order so a tree that
// passes several ceilings always fails the same way.
func checkLimits(limits Limits, counts Counts, growth Growth) error {
	checks := []struct {
		name  string
		value int
		max   int
	}{
		{"limits.maxPackages", counts.Packages, limits.MaxPackages},
		{"limits.maxFiles", counts.Files, limits.MaxFiles},
		{"limits.maxNonTestLines", counts.NonTestLines, limits.MaxNonTestLines},
		{"limits.maxPackageGrowth", growth.PackagesBeyondRoots, limits.MaxPackageGrowth},
	}
	for _, check := range checks {
		if check.max > 0 && check.value > check.max {
			return &LimitError{Name: check.name, Value: check.value, Max: check.max}
		}
	}
	return nil
}
