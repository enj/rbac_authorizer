package closure

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
)

// closureSet is one fixed-point traversal of the source module.
type closureSet struct {
	// packages holds every materialized package keyed by its directory.
	packages map[string]*pkgScan
	// dirs lists those directories in sorted order.
	dirs []string
	// external lists non-standard-library boundary imports, sorted. These are
	// the packages the generated module keeps as real module dependencies.
	external []string
	// standard lists standard library imports, sorted. They are boundary
	// imports too, but they never become module dependencies, so the dependency
	// policy needs them kept apart.
	standard []string
}

// importRef is one edge into a package: where it lives, what import path
// reached it, and which retained file wrote that import.
type importRef struct {
	dir        string
	importPath string
	fromFile   string
}

// expandRoots resolves the configured package roots to the directories that
// seed the traversal.
//
// Recursion applies only here. A discovered package enters the closure at the
// exact import path that referenced it, so recursion never widens what an
// import pulls in; it only lets an operator say "this whole subtree is mine"
// about roots they chose deliberately.
func expandRoots(ctx context.Context, w *worktree, roots []string, recursive, includeTests bool) ([]string, error) {
	out := make([]string, 0, len(roots))
	for _, root := range roots {
		isDir, err := w.isDir(ctx, root)
		switch {
		case isNotFound(err):
			return nil, &PackageError{Dir: root, Err: ErrRootMissing}
		case err != nil:
			// A link where a package root belongs is already a typed failure and
			// keeps its own message.
			var unsafe *FileError
			if errors.As(err, &unsafe) {
				return nil, err
			}
			return nil, fmt.Errorf("inspect package root %s: %w", root, err)
		case !isDir:
			return nil, &PackageError{Dir: root, Err: ErrRootMissing}
		}

		if !recursive {
			out = append(out, root)
			continue
		}
		found, err := collectGoDirs(ctx, w, root, includeTests)
		if err != nil {
			return nil, err
		}
		if len(found) == 0 {
			return nil, &PackageError{Dir: root, Err: ErrPackageMissing}
		}
		out = append(out, found...)
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// collectGoDirs lists dir and every subdirectory below it that holds Go files.
func collectGoDirs(ctx context.Context, w *worktree, dir string, includeTests bool) ([]string, error) {
	entries, err := w.readDir(ctx, dir)
	if err != nil {
		return nil, err
	}

	var out, subdirs []string
	hasGo := false
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			continue
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			return nil, &FileError{File: joinPath(dir, name), Err: ErrUnsafeSymlink}
		}
		if entry.IsDir() {
			// The go tool never loads a package from testdata or vendor, so a
			// recursive root must not either.
			if name == "testdata" || name == "vendor" {
				continue
			}
			subdirs = append(subdirs, joinPath(dir, name))
			continue
		}
		if kind, wanted := classifyFile(name, includeTests); wanted && (kind == KindGo || kind == KindGoTest) {
			hasGo = true
		}
	}

	if hasGo {
		out = append(out, dir)
	}
	for _, sub := range subdirs {
		found, err := collectGoDirs(ctx, w, sub, includeTests)
		if err != nil {
			return nil, err
		}
		out = append(out, found...)
	}
	return out, nil
}

// buildClosure follows imports from the seed packages to a fixed point.
//
// Only imports rooted at prefix are followed. Everything else is recorded as a
// boundary import and left alone, because an external module keeps its real
// type identity in the generated build and relocating it would break that.
//
// Cycles terminate naturally: a package is scanned once, and an import that
// points back at an already scanned package is dropped at the queue.
//
// denied is checked against every import of every retained file, internal or
// external, before the import is followed. The check runs on the post-prune
// pass only. Running it before pruning would reject the very profile pruning
// exists to express, where a pruned file is the sole importer of a denied
// package.
func buildClosure(ctx context.Context, w *worktree, seeds []string, prefix string, mode scanMode, denied []string) (*closureSet, error) {
	set := &closureSet{packages: make(map[string]*pkgScan, len(seeds)*4)}
	externals := make(map[string]bool)
	standard := make(map[string]bool)

	frontier := make([]importRef, 0, len(seeds))
	for _, seed := range seeds {
		frontier = append(frontier, importRef{dir: seed, importPath: importPathFor(prefix, seed)})
	}

	for len(frontier) > 0 {
		// Sorting each level makes the traversal, and therefore the first error
		// reported for a tree with several problems, independent of map and
		// directory iteration order.
		slices.SortFunc(frontier, compareImportRef)

		var next []importRef
		for _, ref := range frontier {
			if err := ctx.Err(); err != nil {
				return nil, fmt.Errorf("build closure: %w", err)
			}
			if _, done := set.packages[ref.dir]; done {
				continue
			}
			pkg, err := scanPackage(ctx, w, ref.dir, ref.importPath, mode)
			if err != nil {
				return nil, attributeToImport(ref, err)
			}
			set.packages[ref.dir] = pkg

			for _, file := range pkg.goFiles {
				for _, imp := range file.imports {
					dir, internal := internalDir(prefix, imp)
					if slices.Contains(denied, imp) {
						return nil, &ImportError{File: file.path, Import: imp, Dir: dir, Err: ErrImportDenied}
					}
					if !internal {
						if isStandardImport(imp) {
							standard[imp] = true
						} else {
							externals[imp] = true
						}
						continue
					}
					if _, done := set.packages[dir]; done {
						continue
					}
					next = append(next, importRef{dir: dir, importPath: imp, fromFile: file.path})
				}
			}
		}
		frontier = next
	}

	set.dirs = sortedKeys(set.packages)
	set.external = sortedKeys(externals)
	set.standard = sortedKeys(standard)
	return set, nil
}

// importPaths returns the closure's package import paths in sorted order.
func (c *closureSet) importPaths() []string {
	out := make([]string, 0, len(c.dirs))
	for _, dir := range c.dirs {
		out = append(out, c.packages[dir].importPath)
	}
	slices.Sort(out)
	return out
}

// goFileCount reports how many Go files the closure holds.
func (c *closureSet) goFileCount() int {
	total := 0
	for _, dir := range c.dirs {
		total += len(c.packages[dir].goFiles)
	}
	return total
}

// nonTestLines reports the closure's non-test Go source lines.
func (c *closureSet) nonTestLines() int {
	total := 0
	for _, dir := range c.dirs {
		total += c.packages[dir].nonTestLines()
	}
	return total
}

// attributeToImport re-reports a package level failure against the import that
// reached it, because the import is the edge an operator can actually cut. A
// failure already scoped to a file keeps its own location.
func attributeToImport(ref importRef, err error) error {
	if ref.fromFile == "" {
		return err
	}
	var pkgErr *PackageError
	if errors.As(err, &pkgErr) {
		return &ImportError{File: ref.fromFile, Import: ref.importPath, Dir: ref.dir, Err: pkgErr.Err}
	}
	return err
}

// internalDir maps an import path inside the source module to its repository
// relative directory.
func internalDir(prefix, imp string) (string, bool) {
	switch {
	case imp == prefix:
		return "", true
	case strings.HasPrefix(imp, prefix+"/"):
		return imp[len(prefix)+1:], true
	default:
		return "", false
	}
}

// importPathFor maps a repository relative directory to its import path.
func importPathFor(prefix, dir string) string {
	if dir == "" || dir == "." {
		return prefix
	}
	return prefix + "/" + dir
}

// isStandardImport reports whether an import path names a standard library
// package. The go command's own rule is used: a first path element without a
// dot cannot be a module path, so it must be standard.
func isStandardImport(imp string) bool {
	first, _, _ := strings.Cut(imp, "/")
	return !strings.Contains(first, ".")
}

// compareImportRef orders references so traversal is reproducible.
func compareImportRef(a, b importRef) int {
	if c := strings.Compare(a.dir, b.dir); c != 0 {
		return c
	}
	if c := strings.Compare(a.importPath, b.importPath); c != 0 {
		return c
	}
	return strings.Compare(a.fromFile, b.fromFile)
}

// sortedKeys returns a map's keys in sorted order. The result is always
// non-nil so it encodes as an empty JSON array rather than null.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
