package closure

import (
	"errors"
	"fmt"
)

// Closure failure sentinels.
//
// Every failure mode in this package is closed. A closure that cannot be
// proved correct is refused rather than emitted in a reduced form, because the
// generated module is published under an immutable tag: silently dropping a
// package or silently gaining one is not recoverable after the fact. Callers
// use errors.Is to classify a failure and the typed errors below to locate it.
var (
	// ErrRootMissing reports a configured package root that is not a directory
	// in the materialized worktree.
	ErrRootMissing = errors.New("configured package root not found")
	// ErrPackageMissing reports a directory that holds no Go files, either as a
	// configured root or as the target of a followed import.
	ErrPackageMissing = errors.New("package has no Go files")
	// ErrPackageMalformed reports a file that does not parse or a directory
	// whose non-test files disagree about the package name.
	ErrPackageMalformed = errors.New("package is malformed")
	// ErrPruneMissing reports an exact prune target that this builder has never
	// removed and that is not present. An upstream rename must fail the run
	// rather than quietly leaving the renamed file in the closure.
	ErrPruneMissing = errors.New("prune target not found")
	// ErrPruneOutsideClosure reports a prune target whose directory is not part
	// of the materialized package set, which means the profile is pruning a
	// file the closure never would have copied.
	ErrPruneOutsideClosure = errors.New("prune target is outside the materialized package set")
	// ErrPruneNotMaterialized reports a prune target that sits inside a
	// materialized package directory but is not one of that package's build
	// inputs, such as an OWNERS file. The directory is in the closure and the
	// file is not, so saying the target is outside the package set would send an
	// operator looking for the wrong mistake.
	ErrPruneNotMaterialized = errors.New("prune target is not a build input of its package")
	// ErrPruneExcludedTest reports a prune target that is a _test.go file in a
	// build that does not include tests. The closure never carries the file, so
	// the profile is asking for a removal that cannot change the generated
	// module: either includeTests is wrong or the entry is.
	ErrPruneExcludedTest = errors.New("prune target is a test file and includeTests is false")
	// ErrPruneRequired reports a path listed as both pruned and required.
	ErrPruneRequired = errors.New("prune target is also a required file")
	// ErrPruneLastFile reports a prune that would leave a materialized package
	// with no Go files. A package that should disappear disappears by losing
	// its importers, never by having its final file deleted.
	ErrPruneLastFile = errors.New("prune would remove the last Go file of a package")
	// ErrRequiredMissing reports a required file that the post-prune closure
	// does not retain.
	ErrRequiredMissing = errors.New("required file was not retained")
	// ErrImportDenied reports an exact denied import observed in a retained
	// file. Pruning and patching are expected to keep it out; its return is a
	// behavior change that must be reviewed, not absorbed.
	ErrImportDenied = errors.New("denied import reentered the closure")
	// ErrLimitExceeded reports an observational closure limit that was passed.
	ErrLimitExceeded = errors.New("closure limit exceeded")
	// ErrUnsafeSymlink reports a symbolic link where the closure expects a
	// regular file or a real directory. A materialized upstream worktree
	// contains neither, so a link is either an escape attempt or something the
	// copy plan has no way to reproduce.
	ErrUnsafeSymlink = errors.New("path resolves through a symbolic link")
	// ErrRecursivePattern reports a ** pattern. path.Match reads ** as two
	// ordinary stars that still stop at a slash, so honouring the syntax would
	// match far less than an operator expects.
	ErrRecursivePattern = errors.New("recursive ** pattern is not supported")
	// ErrPatternNoMatch reports a go:embed pattern or asset glob that selected
	// nothing, which is how an upstream rename of runtime data presents itself.
	ErrPatternNoMatch = errors.New("pattern matched no files")
	// ErrPatternMalformed reports a pattern that is not a clean relative slash
	// path or that path.Match cannot compile.
	ErrPatternMalformed = errors.New("pattern is malformed")
	// ErrEmbedNestedModule reports embedded content that lies in or below a
	// directory holding its own go.mod. The go tool refuses to embed across a
	// module boundary, and copying such a file would carry a nested module into
	// the generated one, where it would silently stop being built at all.
	ErrEmbedNestedModule = errors.New("embedded path crosses a nested module boundary")
	// ErrEmbedBadName reports an embedded path element that a published module
	// cannot carry, such as a version control directory. The go tool treats these
	// names as not existing for embedding, so honouring one would copy files no
	// build ever reads.
	ErrEmbedBadName = errors.New("embedded path element cannot be packaged into a module")
)

// isContentError reports whether a failure was caused by the bytes of the tree
// rather than by its shape or its safety.
//
// The distinction is what lets the pre-prune measurement pass tolerate a failure
// in content a profile is about to remove without also tolerating a symbolic
// link or a filesystem error. A malformed source file, an unresolvable embed
// pattern, and a nested module boundary all describe content, and content is
// exactly what pruning changes. A link where a regular file belongs describes
// the tree the engine was handed, and no prune entry makes it acceptable, so it
// is excluded here explicitly rather than by not being listed.
func isContentError(err error) bool {
	if errors.Is(err, ErrUnsafeSymlink) {
		return false
	}
	return errors.Is(err, ErrPackageMalformed) ||
		errors.Is(err, ErrPatternMalformed) ||
		errors.Is(err, ErrPatternNoMatch) ||
		errors.Is(err, ErrRecursivePattern) ||
		errors.Is(err, ErrEmbedNestedModule) ||
		errors.Is(err, ErrEmbedBadName)
}

// FileError attributes a failure to one repository relative file.
type FileError struct {
	// File is the path relative to the materialized root.
	File string
	// Err is the sentinel or underlying cause.
	Err error
}

// Error renders the file scoped failure.
func (e *FileError) Error() string {
	return fmt.Sprintf("file %s: %v", e.File, e.Err)
}

// Unwrap exposes the cause so errors.Is can classify it.
func (e *FileError) Unwrap() error { return e.Err }

// ImportError attributes a failure to one import and to the file that
// introduced it.
//
// The introducing file is the load bearing half of this error. An operator who
// is told only that k8s.io/kubernetes/pkg/apis/rbac reentered the closure
// cannot act; an operator who is told which retained file imports it knows
// exactly which file to prune, patch, or accept.
type ImportError struct {
	// File is the retained file holding the import specification.
	File string
	// Import is the exact import path as written in the source.
	Import string
	// Dir is the repository relative directory the import resolves to. It is
	// empty for an import outside the source module.
	Dir string
	// Err is the sentinel or underlying cause.
	Err error
}

// Error renders the import scoped failure.
func (e *ImportError) Error() string {
	return fmt.Sprintf("file %s imports %s: %v", e.File, e.Import, e.Err)
}

// Unwrap exposes the cause so errors.Is can classify it.
func (e *ImportError) Unwrap() error { return e.Err }

// PackageError attributes a failure to one materialized package.
type PackageError struct {
	// Dir is the package directory relative to the materialized root.
	Dir string
	// ImportPath is the package's import path within the source module.
	ImportPath string
	// Err is the sentinel or underlying cause.
	Err error
}

// Error renders the package scoped failure.
func (e *PackageError) Error() string {
	if e.ImportPath == "" {
		return fmt.Sprintf("package %s: %v", e.Dir, e.Err)
	}
	return fmt.Sprintf("package %s (%s): %v", e.Dir, e.ImportPath, e.Err)
}

// Unwrap exposes the cause so errors.Is can classify it.
func (e *PackageError) Unwrap() error { return e.Err }

// LimitError reports one exceeded observational limit.
//
// Limits gate publication but never change generated bytes, which is why they
// stay out of the replay profile hash and why this error names the limit rather
// than any particular file.
type LimitError struct {
	// Name is the limit's configuration field name.
	Name string
	// Value is the measured value.
	Value int
	// Max is the configured ceiling.
	Max int
}

// Error renders the exceeded limit.
func (e *LimitError) Error() string {
	return fmt.Sprintf("%s: %d exceeds limit %d", e.Name, e.Value, e.Max)
}

// Unwrap reports ErrLimitExceeded so callers can classify every limit failure
// with a single errors.Is.
func (e *LimitError) Unwrap() error { return ErrLimitExceeded }
