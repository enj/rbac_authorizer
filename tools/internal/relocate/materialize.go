package relocate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
)

// scratchPattern names the temporary tree a materialization is built in. The
// leading dot keeps it out of ordinary listings if a run is killed between the
// build and the rename.
const scratchPattern = ".soapbox-relocate-"

// treeDirMode is the permission the materialized directories carry. Git records
// no directory mode, so this only has to be a sane local value, and it matches
// what the rest of the engine creates.
const treeDirMode = 0o750

// Materialize writes a relocated file set into a fresh destination tree.
//
// The set is validated before anything is created. Materialize is an exported
// write boundary, so the set it receives need not be the one Build returned: it
// can be assembled by hand, decoded, or edited afterwards. Checking it here
// rather than trusting it is what keeps a bad set from reaching a disk at all,
// and a rejected set leaves not even a scratch directory behind. Links are
// validated under [SymlinkInternal], the most permissive policy a relocated set
// can be built under; a set relocated under [SymlinkReject] holds no links, so
// the link rules find nothing to refuse.
//
// The tree is built in a scratch directory beside the destination and moved
// into place with a single rename once every file is written. A run that fails
// or is cancelled halfway therefore leaves the destination exactly as it found
// it: either the complete tree appears or nothing does. The scratch directory
// is a sibling so the rename stays within one file system, which is what makes
// it atomic.
//
// The destination must not exist. Relocation never merges into or overwrites an
// existing tree, because a merge would silently keep files from an earlier run
// that the current profile no longer selects.
func Materialize(ctx context.Context, destination string, set FileSet) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("materialize: %w", err)
	}
	if destination == "" {
		return errors.New("materialize: a destination is required")
	}
	if err := checkFileSet(set, SymlinkInternal); err != nil {
		return fmt.Errorf("materialize %q: %w", destination, err)
	}
	switch _, err := os.Lstat(destination); {
	case err == nil:
		return fmt.Errorf("materialize %q: %w", destination, ErrDestinationExists)
	case !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("materialize %q: %w", destination, err)
	}

	parent := filepath.Dir(destination)
	if err := os.MkdirAll(parent, treeDirMode); err != nil {
		return fmt.Errorf("materialize %q: %w", destination, err)
	}
	scratch, err := os.MkdirTemp(parent, scratchPattern)
	if err != nil {
		return fmt.Errorf("materialize %q: %w", destination, err)
	}

	if err := writeTree(ctx, scratch, set); err != nil {
		// The destination has not been touched yet, so discarding the scratch
		// tree restores the exact starting state.
		return errors.Join(fmt.Errorf("materialize %q: %w", destination, err), removeScratch(scratch))
	}
	if err := os.Rename(scratch, destination); err != nil {
		return errors.Join(fmt.Errorf("materialize %q: %w", destination, err), removeScratch(scratch))
	}
	return nil
}

// removeScratch discards a partial tree, reporting a failure to do so rather
// than leaving a silent temporary directory behind.
func removeScratch(scratch string) error {
	if err := os.RemoveAll(scratch); err != nil {
		return fmt.Errorf("discard scratch tree %q: %w", scratch, err)
	}
	return nil
}

// writeTree writes every file of the set below scratch.
//
// All writes go through an [os.Root] opened on the scratch directory, so
// containment is enforced by the operating system for each individual
// operation. A relocated path that tried to climb out, or a symbolic link that
// tried to redirect a later write outside the tree, fails at the write rather
// than at a check that ran earlier against a different tree state.
func writeTree(ctx context.Context, scratch string, set FileSet) error {
	root, err := os.OpenRoot(scratch)
	if err != nil {
		return err
	}
	defer root.Close()

	if err := root.Chmod(".", treeDirMode); err != nil {
		return fmt.Errorf("set tree permissions: %w", err)
	}
	for _, file := range set.Files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := writeFile(root, file); err != nil {
			return fmt.Errorf("write %q: %w", file.Path, err)
		}
	}
	return nil
}

// writeFile materializes one relocated file.
func writeFile(root *os.Root, file File) error {
	name := filepath.FromSlash(file.Path)
	if dir := path.Dir(file.Path); dir != "." {
		if err := root.MkdirAll(filepath.FromSlash(dir), treeDirMode); err != nil {
			return err
		}
	}
	if file.Mode == ModeSymlink {
		return root.Symlink(filepath.FromSlash(string(file.Contents)), name)
	}
	if err := root.WriteFile(name, file.Contents, file.Mode.FileMode()); err != nil {
		return err
	}
	// The permission passed to WriteFile is masked by the process umask, so the
	// mode is set explicitly. An executable that lost its bit would still be
	// committed as a regular file and would stop working for consumers.
	return root.Chmod(name, file.Mode.FileMode())
}
