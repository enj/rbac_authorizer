package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Path validation sentinels. Callers use errors.Is to distinguish the failure.
var (
	// ErrAbsolutePath reports a configured path that is not repository relative.
	ErrAbsolutePath = errors.New("path must be relative")
	// ErrPathTraversal reports a configured path with a parent directory element.
	ErrPathTraversal = errors.New("path must not traverse parent directories")
	// ErrPathNotClean reports a configured path that is not in canonical form.
	ErrPathNotClean = errors.New("path must be in clean slash form")
	// ErrPathEscape reports a path that leaves its permitted root once symbolic
	// links are resolved.
	ErrPathEscape = errors.New("path escapes its permitted root")
	// ErrDanglingSymlink reports a symbolic link whose target does not exist.
	// Such a link cannot be proved to stay inside its root, so it is refused
	// rather than treated as a component that simply does not exist yet.
	ErrDanglingSymlink = errors.New("path resolves through a dangling symbolic link")
)

// ValidateRelPath checks that p is a clean, relative, traversal free slash path.
func ValidateRelPath(p string) error {
	switch {
	case p == "":
		return errors.New("path must not be empty")
	case strings.TrimSpace(p) != p:
		return errors.New("path must not have leading or trailing space")
	case strings.ContainsRune(p, '\x00'):
		return errors.New("path must not contain a null byte")
	case strings.ContainsRune(p, '\\'):
		return errors.New("path must use forward slashes")
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return errors.New("path must not contain control characters")
		}
	}
	if strings.HasPrefix(p, "/") || filepath.IsAbs(p) || hasDriveLetter(p) {
		return ErrAbsolutePath
	}
	if p == ".." || strings.HasPrefix(p, "../") {
		return ErrPathTraversal
	}
	if path.Clean(p) != p {
		if strings.Contains(p, "..") {
			return ErrPathTraversal
		}
		return ErrPathNotClean
	}
	if p == "." {
		return errors.New("path must name a file or directory")
	}
	return nil
}

// ValidatePackagePath checks that p is a repository relative Go package
// directory path.
func ValidatePackagePath(p string) error {
	if err := ValidateRelPath(p); err != nil {
		return err
	}
	for _, elem := range strings.Split(p, "/") {
		if err := validatePathElement(elem); err != nil {
			return err
		}
	}
	return nil
}

// ValidateGlob checks that p is a repository relative match pattern.
//
// Recursive patterns are rejected. path.Match reads ** as two ordinary stars
// that still stop at a slash, so accepting the syntax would silently match far
// less than an operator expects. One shared recursive matcher can be introduced
// when the extraction phase needs it.
func ValidateGlob(p string) error {
	if err := ValidateRelPath(p); err != nil {
		return err
	}
	if strings.Contains(p, "**") {
		return fmt.Errorf("pattern %q uses recursive ** syntax, which is not supported", p)
	}
	if _, err := path.Match(p, "probe"); err != nil {
		return fmt.Errorf("malformed pattern: %w", err)
	}
	return nil
}

// SafeJoin resolves rel below root and fails when the result leaves root, both
// before and after symbolic link resolution. Missing trailing elements are
// allowed so callers can resolve outputs that do not exist yet.
func SafeJoin(ctx context.Context, root, rel string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	if err := ValidateRelPath(rel); err != nil {
		return "", fmt.Errorf("resolve path %q: %w", rel, err)
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve root %q: %w", root, err)
	}
	rootReal, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return "", fmt.Errorf("resolve root %q: %w", root, err)
	}
	target := filepath.Join(rootReal, filepath.FromSlash(rel))
	resolved, err := resolveExisting(target)
	if err != nil {
		return "", fmt.Errorf("resolve path %q: %w", rel, err)
	}
	if !contains(rootReal, resolved) {
		return "", fmt.Errorf("resolve path %q: %w", rel, ErrPathEscape)
	}
	return resolved, nil
}

// resolveExisting resolves symbolic links in the deepest existing ancestor of p
// and reattaches the elements that do not exist yet.
//
// A component that exists on disk but cannot be resolved, which is how a
// dangling symbolic link presents itself, is an error. Treating it as missing
// would let a link that points outside the root pass the containment check.
func resolveExisting(p string) (string, error) {
	missing := make([]string, 0, 8)
	current := p
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return resolved, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		switch _, lerr := os.Lstat(current); {
		case lerr == nil:
			return "", fmt.Errorf("%w: %s", ErrDanglingSymlink, current)
		case !errors.Is(lerr, os.ErrNotExist):
			return "", lerr
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

// contains reports whether child is root itself or lives below root.
func contains(root, child string) bool {
	if child == root {
		return true
	}
	return strings.HasPrefix(child, root+string(filepath.Separator))
}

// hasDriveLetter reports whether p starts with a Windows drive specifier, which
// is absolute even when the engine runs on another operating system.
func hasDriveLetter(p string) bool {
	if len(p) < 2 || p[1] != ':' {
		return false
	}
	c := p[0]
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// validatePathElement checks a single path element of a package path.
func validatePathElement(elem string) error {
	switch elem {
	case "":
		return errors.New("path must not contain an empty element")
	case ".", "..":
		return ErrPathTraversal
	}
	for _, r := range elem {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_', r == '-', r == '.', r == '+', r == '~':
		default:
			return fmt.Errorf("path element %q contains unsupported character %q", elem, r)
		}
	}
	if strings.HasPrefix(elem, ".") || strings.HasPrefix(elem, "-") {
		return fmt.Errorf("path element %q must not start with %q", elem, elem[:1])
	}
	if strings.HasSuffix(elem, ".") {
		return fmt.Errorf("path element %q must not end with a dot", elem)
	}
	return nil
}
