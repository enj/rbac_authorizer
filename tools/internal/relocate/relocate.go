// Package relocate maps a materialized upstream file set onto the destination
// paths of the generated module.
//
// Every Kubernetes file keeps its complete upstream relative path below the
// configured internal prefix:
//
//	pkg/registry/rbac/validation/rule.go
//	    becomes
//	internal/kk/pkg/registry/rbac/validation/rule.go
//
// Preserving the full path is a hard invariant rather than a convenience.
// Go resolves an internal import against the last internal element of the
// importing path, so flattening or shortening upstream paths would silently
// change which packages can import which, and a nested internal package that
// upstream keeps unimportable could become reachable in the generated module.
//
// Relocation is a pure mapping. It reads no files and writes none: the caller
// hands over a copy plan holding the bytes the closure selected, and receives a
// deterministic file set it can rewrite in memory before anything reaches a
// disk. Materialization is a separate, atomic step.
//
// The mapping fails closed. A path that escapes its root, a mode Git cannot
// record, two upstream files landing on one destination, two destinations a
// case insensitive file system cannot tell apart, a file whose destination is a
// directory of another file, and a symbolic link the policy does not allow or
// whose chain does not end at a file in the set are all refused rather than
// resolved, because every one of them would produce a tree that the profile
// does not describe.
package relocate

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"
	"unicode"

	"github.com/enj/soapbox/tools/internal/config"
)

// internalElement is the path element that makes a relocated package
// unimportable from outside the generated module.
const internalElement = "internal"

// Relocation sentinels. Callers use errors.Is to distinguish the failure.
var (
	// ErrPrefix reports an internal prefix that is not a usable destination
	// root for relocated packages.
	ErrPrefix = errors.New("internal prefix must be a relative path containing an internal element")
	// ErrUnsupportedMode reports a file mode Git cannot record for a blob, such
	// as a device node, a socket, or a submodule entry.
	ErrUnsupportedMode = errors.New("file mode is not a regular file, an executable, or a symbolic link")
	// ErrCollision reports two upstream files that map onto one destination.
	ErrCollision = errors.New("two upstream files map onto one destination path")
	// ErrCaseCollision reports two destination paths that differ only in case.
	// A case insensitive file system, which is what macOS and Windows give a
	// consumer by default, holds one file where the set describes two, so the
	// tree that materializes there is not the tree that materializes here.
	ErrCaseCollision = errors.New("two destination paths differ only in case")
	// ErrUnsorted reports a file set whose files are not in destination order.
	// The order is part of the type's contract rather than a presentation
	// detail: Lookup binary searches the files, so link resolution against an
	// unordered set would silently miss targets that are present.
	ErrUnsorted = errors.New("relocated files are not sorted by destination path")
	// ErrOverlap reports a destination file that is also a directory of another
	// destination file.
	ErrOverlap = errors.New("destination path is also a directory of another destination path")
	// ErrPackageBoundary reports a file that does not live directly in the
	// package it claims. Packages are copied one at a time, never as recursive
	// directory trees, so a file from a subdirectory would smuggle an
	// undeclared package into the closure.
	ErrPackageBoundary = errors.New("file does not live directly in its package directory")
	// ErrSymlink reports a symbolic link the policy refuses.
	ErrSymlink = errors.New("symbolic link is not permitted")
	// ErrDestinationExists reports a materialization target that is already
	// present. Relocation never merges into or overwrites an existing tree.
	ErrDestinationExists = errors.New("destination already exists")
)

// Mode is the file mode of a plan entry.
//
// The set is exactly what Git records for a blob. Anything else, including a
// setuid bit, a sticky bit, a device node, and a submodule gitlink, has no
// representation in the generated tree and is refused rather than rounded off
// to something that would build differently.
type Mode uint8

const (
	// ModeRegular is a non executable file, Git's 100644.
	ModeRegular Mode = iota
	// ModeExecutable is an executable file, Git's 100755.
	ModeExecutable
	// ModeSymlink is a symbolic link, Git's 120000, whose contents are the
	// link target.
	ModeSymlink
)

// String renders the mode as the Git octal form, which is what a provenance
// record and a tree entry both use.
func (m Mode) String() string {
	switch m {
	case ModeRegular:
		return "100644"
	case ModeExecutable:
		return "100755"
	case ModeSymlink:
		return "120000"
	default:
		return "unknown"
	}
}

// FileMode reports the file system permissions to materialize the mode with.
func (m Mode) FileMode() fs.FileMode {
	if m == ModeExecutable {
		return 0o755
	}
	return 0o644
}

// valid reports whether the mode is one this package can relocate.
func (m Mode) valid() bool {
	return m == ModeRegular || m == ModeExecutable || m == ModeSymlink
}

// ModeOf classifies a file system mode, which is the form a copy plan reads off
// the materialized upstream worktree.
//
// Everything Git cannot record for a blob is refused here rather than rounded
// off. A directory, device node, socket, or named pipe has no representation in
// a tree, and a setuid, setgid, or sticky bit would be dropped silently on the
// way into one, which would publish a file whose permissions differ from the
// upstream file it claims to be.
func ModeOf(mode fs.FileMode) (Mode, error) {
	switch {
	case mode&fs.ModeSymlink != 0:
		return ModeSymlink, nil
	case !mode.IsRegular():
		return 0, fmt.Errorf("mode %v: %w", mode, ErrUnsupportedMode)
	case mode&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky) != 0:
		return 0, fmt.Errorf("mode %v: %w", mode, ErrUnsupportedMode)
	case mode.Perm()&0o111 != 0:
		return ModeExecutable, nil
	default:
		return ModeRegular, nil
	}
}

// SymlinkPolicy decides what happens to a symbolic link in a copy plan.
type SymlinkPolicy uint8

const (
	// SymlinkReject refuses every symbolic link. It is the default because a
	// link is the one plan entry whose meaning depends on the file system it is
	// resolved on, and a generated module that builds only where a link
	// resolves is not independently consumable.
	SymlinkReject SymlinkPolicy = iota
	// SymlinkInternal accepts a link whose target is relative and resolves to
	// another file in the same relocated set. Such a link means the same thing
	// everywhere, because everything it can reach was copied with it.
	SymlinkInternal
)

// PlanFile is one upstream file the closure selected.
type PlanFile struct {
	// Path is the upstream repository relative path, such as
	// pkg/apis/rbac/v1/doc.go.
	Path string
	// Package is the upstream package directory the file belongs to. The file
	// must live directly in it.
	Package string
	// Mode is the recorded file mode.
	Mode Mode
	// Contents are the file bytes. For a symbolic link they are the link
	// target, which is how Git stores one.
	Contents []byte
	// Generated records that the file carries a Code generated marker. The flag
	// travels with the file so later steps can keep the marker in the position
	// that tooling recognises.
	Generated bool
}

// Plan is the copy plan the closure produces for one source commit.
type Plan struct {
	// Files are the selected upstream files. Order does not matter: the output
	// is sorted, so two closures that select the same files in different orders
	// relocate identically.
	Files []PlanFile
}

// Options configures the mapping.
type Options struct {
	// InternalPrefix is the module relative directory every upstream path is
	// preserved below, such as internal/kk.
	InternalPrefix string
	// Symlinks selects the symbolic link policy. The zero value rejects every
	// link.
	Symlinks SymlinkPolicy
}

// File is one relocated file.
type File struct {
	// Source is the upstream repository relative path.
	Source string
	// Path is the destination module relative path.
	Path string
	// Package is the destination package directory.
	Package string
	// SourcePackage is the upstream package directory.
	SourcePackage string
	// Mode is the recorded file mode.
	Mode Mode
	// Contents are the file bytes, or the link target for a symbolic link.
	Contents []byte
	// Generated records that the file carries a Code generated marker.
	Generated bool
}

// Package is one relocated package.
type Package struct {
	// Source is the upstream package directory.
	Source string
	// Path is the destination package directory.
	Path string
	// Files are the destination paths in the package, sorted.
	Files []string
}

// FileSet is the deterministic relocated output.
type FileSet struct {
	// Files are the relocated files, sorted by destination path.
	Files []File
	// Packages are the relocated packages, sorted by destination directory.
	Packages []Package
}

// Lookup reports the relocated file at a destination path.
func (s FileSet) Lookup(destination string) (File, bool) {
	index, found := slices.BinarySearchFunc(s.Files, destination, func(file File, target string) int {
		return strings.Compare(file.Path, target)
	})
	if !found {
		return File{}, false
	}
	return s.Files[index], true
}

// Build maps a copy plan onto destination paths.
//
// The result is deterministic: files are sorted by destination path, packages
// are sorted by destination directory, and no map iteration reaches the output.
// The contents are shared with the plan rather than copied, because the caller
// owns both and the next step rewrites them in place.
func Build(ctx context.Context, plan Plan, opts Options) (FileSet, error) {
	if err := ctx.Err(); err != nil {
		return FileSet{}, fmt.Errorf("relocate: %w", err)
	}
	prefix, err := validatePrefix(opts.InternalPrefix)
	if err != nil {
		return FileSet{}, fmt.Errorf("relocate: %w", err)
	}

	files := make([]File, 0, len(plan.Files))
	for _, entry := range plan.Files {
		file, err := relocateFile(entry, prefix)
		if err != nil {
			return FileSet{}, fmt.Errorf("relocate %q: %w", entry.Path, err)
		}
		files = append(files, file)
	}

	slices.SortFunc(files, compareFiles)
	set := FileSet{Files: files, Packages: groupPackages(files)}
	// The freshly mapped set goes through the same check a set handed to
	// Materialize does. Build's guarantee is then exactly the one the write
	// boundary enforces, and the two cannot drift apart.
	if err := checkFileSet(set, opts.Symlinks); err != nil {
		return FileSet{}, fmt.Errorf("relocate: %w", err)
	}
	return set, nil
}

// compareFiles orders relocated files by destination path and breaks a tie on
// the upstream source.
//
// The tie break is what makes a rejected plan reproducible. Two upstream files
// that map onto one destination are a profile bug, and the closure may hand
// them over in either order, so without a total order the resulting error would
// name the same pair of files the other way round from run to run.
func compareFiles(a, b File) int {
	if order := strings.Compare(a.Path, b.Path); order != 0 {
		return order
	}
	return strings.Compare(a.Source, b.Source)
}

// checkFileSet re-establishes every invariant of a relocated set from the set
// alone.
//
// Build enforces these as it maps a plan, so a set that came from Build passes
// unchanged and the cost is one more pass over files already in memory. The
// check exists because FileSet is an exported type with exported fields, and
// Materialize writes one to a disk: a caller can assemble a set by hand, decode
// one, or edit one after Build returned it. Trusting the type would make the
// write boundary as safe as its least careful caller.
//
// The policy argument is what the caller relocated under. Materialize passes
// SymlinkInternal because that is the most permissive policy Build can produce
// a set under; a set relocated under SymlinkReject holds no links at all, so
// the link rules simply find nothing to refuse.
func checkFileSet(set FileSet, policy SymlinkPolicy) error {
	for i, file := range set.Files {
		if err := checkFile(file); err != nil {
			return fmt.Errorf("%q: %w", file.Path, err)
		}
		// Order is checked before anything reads the set through Lookup, which
		// binary searches it.
		if i > 0 && compareFiles(set.Files[i-1], file) > 0 {
			return fmt.Errorf("%q sorts before %q: %w", file.Path, set.Files[i-1].Path, ErrUnsorted)
		}
	}
	if err := checkCollisions(set.Files); err != nil {
		return err
	}
	if err := checkOverlap(set.Files); err != nil {
		return err
	}
	return checkSymlinks(set, policy)
}

// checkFile validates one relocated file on its own.
//
// The package fields are checked only when they carry a value. They describe
// the file for the rewriting steps rather than for the tree, and a caller that
// materializes a set it assembled itself may legitimately leave the provenance
// empty; what must never be wrong is a package that disagrees with the path it
// is supposed to describe.
func checkFile(file File) error {
	if err := config.ValidatePackagePath(file.Path); err != nil {
		return err
	}
	if !file.Mode.valid() {
		return fmt.Errorf("mode %d: %w", file.Mode, ErrUnsupportedMode)
	}
	if file.Package != "" && file.Package != path.Dir(file.Path) {
		return fmt.Errorf("package %q: %w", file.Package, ErrPackageBoundary)
	}
	if file.Source == "" {
		return nil
	}
	if err := config.ValidatePackagePath(file.Source); err != nil {
		return fmt.Errorf("source %q: %w", file.Source, err)
	}
	if file.SourcePackage != "" && file.SourcePackage != path.Dir(file.Source) {
		return fmt.Errorf("source package %q: %w", file.SourcePackage, ErrPackageBoundary)
	}
	return nil
}

// checkCollisions rejects two files that a file system cannot tell apart.
//
// Comparison is by case folded path, which catches both an exact duplicate and
// a pair like File.go and file.go. The second pair is a real conflict even
// though this machine can hold both: a consumer on macOS or Windows checks out
// one file where the tree records two, and the module that builds for them is
// not the module that was published. Files arrive sorted, so the pair reported
// is the same on every run.
func checkCollisions(files []File) error {
	seen := make(map[string]File, len(files))
	for _, file := range files {
		key := foldPath(file.Path)
		previous, ok := seen[key]
		switch {
		case !ok:
			seen[key] = file
		case previous.Path == file.Path:
			// Both upstream paths are named because the collision is a profile
			// bug, and knowing only the destination would not say which two
			// selections have to be reconciled.
			return fmt.Errorf("relocate %q and %q onto %q: %w", previous.Source, file.Source, file.Path, ErrCollision)
		default:
			return fmt.Errorf("relocate %q onto %q and %q onto %q: %w", previous.Source, previous.Path, file.Source, file.Path, ErrCaseCollision)
		}
	}
	return nil
}

// foldPath maps a destination path onto the key a case insensitive file system
// compares it by.
//
// Each rune becomes the smallest member of its simple case folding orbit, which
// is the equivalence strings.EqualFold reports, so ASCII case and the Unicode
// runes that fold together are both covered. A separator has no case, so the
// key keeps the structure of the path and an ancestor of a folded path is the
// folded ancestor of the original.
func foldPath(p string) string {
	var key strings.Builder
	key.Grow(len(p))
	for _, r := range p {
		key.WriteRune(foldRune(r))
	}
	return key.String()
}

// foldRune returns the smallest rune that case folds together with r.
func foldRune(r rune) rune {
	smallest := r
	for folded := unicode.SimpleFold(r); folded != r; folded = unicode.SimpleFold(folded) {
		if folded < smallest {
			smallest = folded
		}
	}
	return smallest
}

// validatePrefix checks the destination root relocated packages live below.
//
// The internal element requirement is the invariant that keeps relocated code
// unimportable from outside the generated module. Configuration validates it
// too, but a mapping that silently produced an importable tree when called with
// an unvalidated prefix would be a much more expensive mistake than a duplicate
// check.
func validatePrefix(prefix string) (string, error) {
	if err := config.ValidatePackagePath(prefix); err != nil {
		return "", fmt.Errorf("%w: %w", ErrPrefix, err)
	}
	if !slices.Contains(strings.Split(prefix, "/"), internalElement) {
		return "", fmt.Errorf("%q: %w", prefix, ErrPrefix)
	}
	return prefix, nil
}

// relocateFile maps one plan entry onto its destination.
func relocateFile(entry PlanFile, prefix string) (File, error) {
	if err := config.ValidatePackagePath(entry.Path); err != nil {
		return File{}, err
	}
	if err := config.ValidatePackagePath(entry.Package); err != nil {
		return File{}, fmt.Errorf("package %q: %w", entry.Package, err)
	}
	if path.Dir(entry.Path) != entry.Package {
		return File{}, fmt.Errorf("package %q: %w", entry.Package, ErrPackageBoundary)
	}
	if !entry.Mode.valid() {
		return File{}, fmt.Errorf("mode %d: %w", entry.Mode, ErrUnsupportedMode)
	}
	return File{
		Source:        entry.Path,
		Path:          prefix + "/" + entry.Path,
		Package:       prefix + "/" + entry.Package,
		SourcePackage: entry.Package,
		Mode:          entry.Mode,
		Contents:      entry.Contents,
		Generated:     entry.Generated,
	}, nil
}

// checkOverlap rejects a destination file that is also a directory of another
// destination file.
//
// Upstream can hold a file and a directory with the same name in different
// packages, and a plan assembled from several packages can select both. Neither
// a file system nor a Git tree can hold both, so the conflict has to be
// reported here rather than discovered halfway through writing a tree.
//
// Comparison is by case folded path for the same reason collisions are: a
// directory and a file whose names differ only in case are one name on macOS
// and Windows. Refusing the pair also keeps materialization safe, because it
// leaves no destination path whose parent directories could be anything other
// than directories this run created. Collisions have already been refused when
// this runs, so exactly one file occupies each folded path.
func checkOverlap(files []File) error {
	paths := make(map[string]File, len(files))
	for _, file := range files {
		paths[foldPath(file.Path)] = file
	}
	for _, file := range files {
		for dir := path.Dir(foldPath(file.Path)); dir != "." && dir != "/"; dir = path.Dir(dir) {
			if other, ok := paths[dir]; ok {
				return fmt.Errorf("%q from %q and %q from %q: %w", other.Path, other.Source, file.Path, file.Source, ErrOverlap)
			}
		}
	}
	return nil
}

// groupPackages collects the relocated files into their packages.
func groupPackages(files []File) []Package {
	var packages []Package
	for _, file := range files {
		if n := len(packages); n > 0 && packages[n-1].Path == file.Package {
			packages[n-1].Files = append(packages[n-1].Files, file.Path)
			continue
		}
		packages = append(packages, Package{
			Source: file.SourcePackage,
			Path:   file.Package,
			Files:  []string{file.Path},
		})
	}
	// Files are sorted by destination path, so a package's files are contiguous
	// only when no other package sorts between them, which happens when one
	// package directory is a prefix of another. Sorting by directory restores a
	// single record per package.
	slices.SortFunc(packages, func(a, b Package) int { return strings.Compare(a.Path, b.Path) })
	return mergePackages(packages)
}

// mergePackages folds adjacent records that describe one package.
func mergePackages(packages []Package) []Package {
	merged := packages[:0]
	for _, pkg := range packages {
		if n := len(merged); n > 0 && merged[n-1].Path == pkg.Path {
			merged[n-1].Files = append(merged[n-1].Files, pkg.Files...)
			slices.Sort(merged[n-1].Files)
			continue
		}
		merged = append(merged, pkg)
	}
	return merged
}

// checkSymlinks applies the symbolic link policy.
func checkSymlinks(set FileSet, policy SymlinkPolicy) error {
	for _, file := range set.Files {
		if file.Mode != ModeSymlink {
			continue
		}
		if policy != SymlinkInternal {
			return fmt.Errorf("%q: %w", file.Path, ErrSymlink)
		}
		if err := checkLinkChain(set, file); err != nil {
			return fmt.Errorf("%q: %w", file.Path, err)
		}
	}
	return nil
}

// checkLinkChain proves that a link ends at a file the set carries.
//
// A target may itself be a link, so the whole chain is walked. Only a regular
// or executable file is an end. A chain that comes back to a link it has
// already passed through has no end at all: it materializes as a tree where
// every read of the file fails with ELOOP, which is worse than a tree that is
// merely missing something, because the failure surfaces in whatever consumer
// eventually opens the file rather than in the run that produced it.
//
// The walk always terminates. Every step either returns or records a
// destination the chain had not visited, and the set holds finitely many.
func checkLinkChain(set FileSet, file File) error {
	visited := map[string]bool{file.Path: true}
	for link := file; ; {
		next, err := resolveLink(set, link)
		if err != nil {
			// A failure past the first hop names the link that carries the bad
			// target, which is not the link the caller is told about.
			if link.Path != file.Path {
				return fmt.Errorf("chain reaches %q: %w", link.Path, err)
			}
			return err
		}
		if next.Mode != ModeSymlink {
			return nil
		}
		if visited[next.Path] {
			return fmt.Errorf("chain returns to %q: %w", next.Path, ErrSymlink)
		}
		visited[next.Path] = true
		link = next
	}
}

// resolveLink resolves one link onto the file the set holds at its target.
//
// An absolute target, or one that climbs above the destination root, points at
// whatever the machine running the build happens to have there. A target that
// resolves inside the set points at content this run copied, so it means the
// same thing on every machine.
//
// Resolution is lexical, and that matches what a file system would do with the
// materialized tree: checkOverlap has already refused any file that is also a
// directory of another file, so no element of a destination path can be a link
// that would resolve a parent element somewhere else.
func resolveLink(set FileSet, link File) (File, error) {
	target := string(link.Contents)
	switch {
	case target == "":
		return File{}, fmt.Errorf("target is empty: %w", ErrSymlink)
	case path.IsAbs(target):
		return File{}, fmt.Errorf("target %q is absolute: %w", target, ErrSymlink)
	}
	resolved := path.Join(path.Dir(link.Path), target)
	if resolved == ".." || strings.HasPrefix(resolved, "../") {
		return File{}, fmt.Errorf("target %q escapes the destination root: %w", target, ErrSymlink)
	}
	next, ok := set.Lookup(resolved)
	if !ok {
		return File{}, fmt.Errorf("target %q resolves to %q, which is not in the relocated set: %w", target, resolved, ErrSymlink)
	}
	return next, nil
}
