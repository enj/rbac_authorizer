package closure

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"
)

// CopyEntry is one file the closure materializes into the generated module.
type CopyEntry struct {
	// Path is relative to the materialized root.
	Path string
	// Kind records why the file is in the plan.
	Kind CopyKind
	// Mode is the file's permission bits. The copier reproduces them so an
	// executable upstream fixture stays executable in the generated module.
	Mode fs.FileMode
}

// kindPrecedence orders the reasons a file can be in the copy plan. A file that
// qualifies twice, such as a header that is also embedded, is listed once under
// the earliest reason so the plan stays a set of paths.
var kindPrecedence = map[CopyKind]int{
	KindGo:       0,
	KindGoTest:   1,
	KindNative:   2,
	KindHeader:   3,
	KindAssembly: 4,
	KindObject:   5,
	KindEmbed:    6,
	KindAsset:    7,
}

// buildCopyPlan lists every file a portable build of the closure needs.
//
// The plan is package granular. Only the direct files of materialized packages
// are listed, never a subdirectory, so OWNERS files, build system metadata, and
// unimported sibling packages stay upstream. The exceptions are deliberate and
// declared: a go:embed directive names data the compiler will demand, and an
// asset glob names runtime data an operator knows no Go file references.
//
// tolerant relaxes go:embed resolution for the pre-prune measurement pass. That
// pass exists to report what upstream contained, and it runs over packages that
// pruning is about to remove; letting a directive in one of them fail the run
// would be a measurement vetoing the contract, and the profile could not
// express a remedy because pruning has not happened yet. The post-prune pass,
// which decides what is actually copied, is never tolerant.
//
// Tolerance is limited to failures the tree's content caused: an unresolvable
// pattern, a malformed one, a nested module boundary. An unsafe symbolic link
// and a filesystem error are never absorbed, because neither describes content
// a profile can prune and swallowing them would turn the one pass that touched
// the path into the one pass that stayed quiet about it. Nothing is lost by
// deferring a content failure either: every package that survives pruning is
// resolved again by the post-prune pass, which reports the same failure against
// the same file, so tolerance can only ever absorb a failure in content the
// generated module does not contain.
func buildCopyPlan(ctx context.Context, w *worktree, set *closureSet, assets []planFile, tolerant bool) ([]CopyEntry, error) {
	entries := make([]CopyEntry, 0, set.goFileCount()+len(assets))
	for _, dir := range set.dirs {
		pkg := set.packages[dir]
		for _, file := range pkg.goFiles {
			kind := KindGo
			if file.test {
				kind = KindGoTest
			}
			entries = append(entries, CopyEntry{Path: file.path, Kind: kind, Mode: file.mode})
		}
		for _, companion := range pkg.companions {
			entries = append(entries, CopyEntry{Path: companion.path, Kind: companion.kind, Mode: companion.mode})
		}
		for _, file := range pkg.goFiles {
			for _, pattern := range file.embeds {
				embedded, err := resolveEmbed(ctx, w, dir, pattern, file.path)
				if err != nil {
					if tolerant && isContentError(err) {
						continue
					}
					return nil, err
				}
				entries = append(entries, embedded...)
			}
		}
	}
	for _, asset := range assets {
		entries = append(entries, CopyEntry{Path: asset.path, Kind: asset.kind, Mode: asset.mode})
	}

	slices.SortFunc(entries, func(a, b CopyEntry) int {
		if c := strings.Compare(a.Path, b.Path); c != 0 {
			return c
		}
		return kindPrecedence[a.Kind] - kindPrecedence[b.Kind]
	})
	return slices.CompactFunc(entries, func(a, b CopyEntry) bool { return a.Path == b.Path }), nil
}

// resolveEmbed expands one go:embed pattern relative to its package directory.
//
// The semantics mirror the go tool. A pattern that names a directory carries
// that directory's whole subtree, and it is only that walk that skips names
// beginning with a dot or an underscore, which the all: prefix turns off. What
// the pattern itself matches is never filtered for hidden names: the go tool
// documents "image/*" as embedding image/.tempfile while "image" does not, so
// filtering the matches would quietly drop a file a package really does embed.
//
// A pattern that selects nothing fails closed, because that is how an upstream
// rename of embedded data presents itself, and a silently empty embed would fail
// much later as a confusing compile error in the generated module.
func resolveEmbed(ctx context.Context, w *worktree, pkgDir, pattern, fromFile string) ([]CopyEntry, error) {
	body, allowHidden := strings.CutPrefix(pattern, "all:")
	fail := func(err error) error {
		return &FileError{File: fromFile, Err: fmt.Errorf("go:embed %q: %w", pattern, err)}
	}
	if err := validateGlob(body); err != nil {
		return nil, fail(err)
	}

	matches, err := globExpand(ctx, w, joinPath(pkgDir, body))
	if err != nil {
		return nil, err
	}

	var out []CopyEntry
	for _, match := range matches {
		if err := checkEmbedPath(ctx, w, pkgDir, match); err != nil {
			return nil, fail(err)
		}
		info, err := w.lstat(ctx, match)
		if err != nil {
			return nil, &FileError{File: fromFile, Err: fmt.Errorf("go:embed %q: inspect %s: %w", pattern, match, err)}
		}
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			return nil, &FileError{File: match, Err: ErrUnsafeSymlink}
		case info.IsDir():
			files, err := expandDirTree(ctx, w, match, allowHidden)
			if err != nil {
				return nil, err
			}
			out = append(out, files...)
		default:
			out = append(out, CopyEntry{Path: match, Kind: KindEmbed, Mode: info.Mode().Perm()})
		}
	}
	if len(out) == 0 {
		return nil, fail(ErrPatternNoMatch)
	}
	return out, nil
}

// checkEmbedPath applies the checks the go tool applies to every path element
// between a package directory and something the package embeds.
//
// Two things disqualify an element. A directory holding its own go.mod starts a
// different module, and the go tool refuses to embed across that boundary; a
// closure that ignored it would copy a nested module's files into the generated
// module, where they would silently stop being built. A version control
// directory is not carried by a published module at all, so embedding one would
// copy a repository's internals under the pretence that some build reads them.
func checkEmbedPath(ctx context.Context, w *worktree, pkgDir, match string) error {
	for p := match; p != "" && p != "." && p != pkgDir; p = parentDir(p) {
		if isBadEmbedName(path.Base(p)) {
			return fmt.Errorf("%w: %s", ErrEmbedBadName, p)
		}
		nested, err := hasGoMod(ctx, w, p)
		if err != nil {
			return err
		}
		if nested {
			return fmt.Errorf("%w: %s holds its own go.mod", ErrEmbedNestedModule, p)
		}
	}
	return nil
}

// hasGoMod reports whether a directory holds its own go.mod.
//
// Only a successful inspection means a boundary. Every other answer, including
// the one a path that is not a directory gives, means there is no module to stop
// at, which is exactly how the go tool reads it.
func hasGoMod(ctx context.Context, w *worktree, dir string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, fmt.Errorf("inspect %s: %w", joinPath(dir, "go.mod"), err)
	}
	_, err := w.lstat(ctx, joinPath(dir, "go.mod"))
	return err == nil, nil
}

// resolveAssets expands the configured asset globs against the materialized
// root.
//
// The semantics are deliberately narrow. A glob selects regular files only; a
// directory it matches is not expanded, because a pattern that quietly grew to
// carry a whole subtree would defeat the point of measuring the closure. A
// glob's metacharacters match inside one path element and never cross a slash,
// and ** is refused rather than silently reinterpreted as a single star. An
// operator who wants a subtree lists its levels.
func resolveAssets(ctx context.Context, w *worktree, globs []string) ([]planFile, error) {
	out := make([]planFile, 0, len(globs))
	for _, glob := range globs {
		matches, err := globExpand(ctx, w, glob)
		if err != nil {
			return nil, fmt.Errorf("asset glob %q: %w", glob, err)
		}
		found := 0
		for _, match := range matches {
			info, err := w.lstat(ctx, match)
			if err != nil {
				return nil, fmt.Errorf("asset glob %q: inspect %s: %w", glob, match, err)
			}
			switch {
			case info.Mode()&fs.ModeSymlink != 0:
				return nil, &FileError{File: match, Err: ErrUnsafeSymlink}
			case info.IsDir():
				continue
			}
			out = append(out, planFile{path: match, kind: KindAsset, mode: info.Mode().Perm()})
			found++
		}
		if found == 0 {
			return nil, fmt.Errorf("asset glob %q: %w", glob, ErrPatternNoMatch)
		}
	}
	return out, nil
}

// expandDirTree lists every regular file below dir that a module would carry.
//
// The walk stops at a nested module: a subdirectory holding its own go.mod is
// skipped whole, exactly as the go tool skips it, because those files belong to
// a module the generated one does not contain. Version control directories are
// skipped for the same reason and are skipped even under all:, since all: widens
// the walk to hidden names, not to content no module ever publishes.
func expandDirTree(ctx context.Context, w *worktree, dir string, allowHidden bool) ([]CopyEntry, error) {
	entries, err := w.readDir(ctx, dir)
	if err != nil {
		return nil, err
	}

	var out []CopyEntry
	for _, entry := range entries {
		name := entry.Name()
		if isBadEmbedName(name) || (!allowHidden && isHiddenName(name)) {
			continue
		}
		rel := joinPath(dir, name)
		if entry.Type()&fs.ModeSymlink != 0 {
			return nil, &FileError{File: rel, Err: ErrUnsafeSymlink}
		}
		if entry.IsDir() {
			nested, err := hasGoMod(ctx, w, rel)
			if err != nil {
				return nil, err
			}
			if nested {
				continue
			}
			children, err := expandDirTree(ctx, w, rel, allowHidden)
			if err != nil {
				return nil, err
			}
			out = append(out, children...)
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("inspect %s: %w", rel, err)
		}
		out = append(out, CopyEntry{Path: rel, Kind: KindEmbed, Mode: info.Mode().Perm()})
	}
	return out, nil
}

// globExpand resolves a repository relative pattern one path element at a time.
//
// Element-wise matching is what gives * its "does not cross a slash" meaning
// without a recursive matcher, and it means a pattern reads only the
// directories it can actually match instead of walking the whole tree.
//
// The walk keeps an invariant that every intermediate candidate is a real
// directory, so a pattern can never try to descend through a regular file and
// the code needs no platform specific errno to notice when it would have.
func globExpand(ctx context.Context, w *worktree, pattern string) ([]string, error) {
	elements := strings.Split(pattern, "/")
	current := []string{""}
	for i, elem := range elements {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("expand pattern %q: %w", pattern, err)
		}
		if elem == "" {
			return nil, fmt.Errorf("%w: %q has an empty path element", ErrPatternMalformed, pattern)
		}
		last := i == len(elements)-1

		var next []string
		for _, dir := range current {
			entries, err := w.readDir(ctx, dir)
			switch {
			case isNotFound(err):
				continue
			case err != nil:
				return nil, err
			}
			for _, entry := range entries {
				matched := entry.Name() == elem
				if hasGlobMeta(elem) {
					matched, err = path.Match(elem, entry.Name())
					if err != nil {
						return nil, fmt.Errorf("%w: %q: %w", ErrPatternMalformed, pattern, err)
					}
				}
				if !matched {
					continue
				}
				rel := joinPath(dir, entry.Name())
				if entry.Type()&fs.ModeSymlink != 0 {
					return nil, &FileError{File: rel, Err: ErrUnsafeSymlink}
				}
				// Only a directory can carry the rest of the pattern.
				if !last && !entry.IsDir() {
					continue
				}
				next = append(next, rel)
			}
		}
		current = next
	}

	slices.Sort(current)
	return slices.Compact(current), nil
}

// hasGlobMeta reports whether an element carries match syntax.
func hasGlobMeta(s string) bool {
	return strings.ContainsAny(s, "*?[")
}

// isHiddenName reports whether the go tool would skip a name while walking an
// embedded directory's subtree.
func isHiddenName(name string) bool {
	return strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")
}

// isBadEmbedName reports whether a name is one a published module never carries,
// so the go tool treats it as not existing for embedding.
//
// The go tool derives this from module.CheckFilePath plus a list of version
// control directories. Only the part that a materialized upstream worktree can
// actually present is reproduced here: the version control directories, which a
// worktree really does hold, and the relative names, which a directory listing
// never yields but a caller could construct.
func isBadEmbedName(name string) bool {
	switch name {
	case "", ".", "..", ".bzr", ".hg", ".git", ".svn":
		return true
	}
	return false
}

// parentDir returns the parent of a repository relative path, or the empty
// string when the path names something directly in the root.
func parentDir(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[:i]
	}
	return ""
}

// isNotFound reports whether an error means the path simply is not there.
func isNotFound(err error) bool {
	return errors.Is(err, fs.ErrNotExist)
}
