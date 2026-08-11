package closure

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
)

// worktree is a contained view of the materialized upstream tree.
//
// Every read, listing, and removal in this package goes through it. os.Root
// re-checks containment on each operation rather than once at validation time,
// and refuses to follow a symbolic link that leaves the tree or that is
// absolute. That matters because the tree is produced from untrusted upstream
// content: a link committed as pkg/foo/bar.go -> /etc/shadow, or a directory
// swapped for a link between the check and the read, cannot make the engine
// read or delete anything outside the worktree it was handed.
//
// Containment is necessary but not sufficient, so the scanner additionally
// refuses any symbolic link it would otherwise copy or descend into. A link
// that stays inside the tree is still not a regular file, and the copy plan has
// no faithful way to reproduce it in the generated module.
type worktree struct {
	root *os.Root
	// dir is the path the root was opened from, used only in messages.
	dir string
}

// openWorktree opens the materialized tree for contained access.
func openWorktree(ctx context.Context, dir string) (*worktree, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("open materialized root: %w", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("open materialized root %s: %w", dir, err)
	}
	return &worktree{root: root, dir: dir}, nil
}

// Close releases the root handle.
func (w *worktree) Close() error {
	if err := w.root.Close(); err != nil {
		return fmt.Errorf("close materialized root %s: %w", w.dir, err)
	}
	return nil
}

// readDir lists one directory in deterministic name order.
func (w *worktree) readDir(ctx context.Context, dir string) ([]fs.DirEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("read directory %s: %w", displayPath(dir), err)
	}
	f, err := w.root.Open(fsName(dir))
	if err != nil {
		return nil, fmt.Errorf("open directory %s: %w", displayPath(dir), err)
	}
	defer f.Close()

	entries, err := f.ReadDir(-1)
	if err != nil {
		return nil, fmt.Errorf("read directory %s: %w", displayPath(dir), err)
	}
	// Readdir order is filesystem dependent. Sorting here is what makes every
	// downstream list, count, and error message reproducible across machines.
	slices.SortFunc(entries, func(a, b fs.DirEntry) int {
		return strings.Compare(a.Name(), b.Name())
	})
	return entries, nil
}

// readFile reads one regular file.
func (w *worktree) readFile(ctx context.Context, name string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("read file %s: %w", name, err)
	}
	data, err := w.root.ReadFile(fsName(name))
	if err != nil {
		return nil, fmt.Errorf("read file %s: %w", name, err)
	}
	return data, nil
}

// lstat inspects one path without following a final symbolic link, so a link
// can be detected instead of being silently resolved to its target.
func (w *worktree) lstat(ctx context.Context, name string) (fs.FileInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("inspect %s: %w", name, err)
	}
	info, err := w.root.Lstat(fsName(name))
	if err != nil {
		return nil, err
	}
	return info, nil
}

// remove deletes one file.
func (w *worktree) remove(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("remove %s: %w", name, err)
	}
	if err := w.root.Remove(fsName(name)); err != nil {
		return fmt.Errorf("remove %s: %w", name, err)
	}
	return nil
}

// lstatPath inspects a path one element at a time and refuses any element that
// is a symbolic link, returning the final element's information.
//
// A plain lstat answers about the last element only: by the time the kernel
// reports on pkg/foo, it has already resolved pkg, and a link there is invisible
// in the answer. os.Root keeps such a link contained, which is the security
// question, but containment is not the whole question for a package root or an
// import path. A link that stays inside the tree makes one directory reachable
// under two names, and a closure that followed it would copy the same package
// twice under two different import paths, each declaring the same Go package.
// That is not something the generated module can be built out of, so every
// element is checked and the first link fails the run.
func (w *worktree) lstatPath(ctx context.Context, name string) (fs.FileInfo, error) {
	if name == "" || name == "." {
		return w.lstat(ctx, name)
	}
	var info fs.FileInfo
	seen := ""
	for _, elem := range strings.Split(name, "/") {
		seen = joinPath(seen, elem)
		var err error
		info, err = w.lstat(ctx, seen)
		if err != nil {
			return nil, err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return nil, &FileError{File: seen, Err: ErrUnsafeSymlink}
		}
	}
	return info, nil
}

// isDir reports whether name is a real directory reached without traversing a
// symbolic link.
func (w *worktree) isDir(ctx context.Context, name string) (bool, error) {
	info, err := w.lstatPath(ctx, name)
	if err != nil {
		return false, err
	}
	return info.IsDir(), nil
}

// fsName converts a repository relative slash path into the form os.Root
// accepts. The empty path and "." both name the root directory itself.
func fsName(p string) string {
	if p == "" {
		return "."
	}
	return p
}

// displayPath renders a path for a message, naming the root explicitly rather
// than as an empty string.
func displayPath(p string) string {
	if p == "" {
		return "."
	}
	return p
}

// joinPath joins a directory and a name into a repository relative slash path.
func joinPath(dir, name string) string {
	if dir == "" || dir == "." {
		return name
	}
	return path.Join(dir, name)
}
