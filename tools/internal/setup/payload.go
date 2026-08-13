package setup

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/enj/soapbox/tools/internal/config"
)

// Payload paths setup composes. They are named constants rather than derived
// strings so the whole allowlist can be read in one place and asserted against
// the manifest a run produces.
const (
	rootGoModPath    = "go.mod"
	toolsGoModPath   = "tools/go.mod"
	toolsGoSumPath   = "tools/go.sum"
	toolsMainPath    = "tools/cmd/soapbox/main.go"
	ciWorkflowPath   = ".github/workflows/ci.yml"
	syncWorkflowPath = ".github/workflows/sync.yml"
)

// templateMarkers are the paths that make a checkout recognisably the soapbox
// template rather than some other repository.
var templateMarkers = []string{
	config.DefaultFileName,
	"plans/implementation.md",
	"tools/soapbox.go",
	"tools/internal/cli/cli.go",
	toolsMainPath,
}

// templateAbsentMarkers are the paths whose presence means this is not a
// template.
//
// The template has no root module by design, so a root go.mod means either that
// setup already ran or that this was never a template. Both are answered by
// refusing rather than by composing a second root module over the first.
var templateAbsentMarkers = []string{rootGoModPath}

// templateOwnedDirs are the directory prefixes a derived repository never keeps.
//
// The engine's source and tests are here because a derived repository builds
// against a published engine release instead of carrying one, and the planning,
// agent, and documentation directories are here because they describe the
// template rather than the module it produces.
var templateOwnedDirs = []string{
	".claude/",
	".github/workflows/",
	".serena/",
	"docs/",
	"plans/",
	"tools/cmd/",
	"tools/internal/",
}

// templateOwnedFiles are the exact template files a derived repository never
// keeps. A path setup also composes is replaced rather than removed; classify
// decides that.
var templateOwnedFiles = []string{
	".github/workflows/template-selftest.yml",
	".golangci.yml",
	"CLAUDE.md",
	ciWorkflowPath,
	toolsGoModPath,
	toolsGoSumPath,
	"tools/soapbox.go",
	"tools/soapbox_test.go",
}

// retainedFiles are the exact paths a derived repository keeps untouched.
//
// LICENSE, NOTICE, and README.md are the template's own on a fresh checkout and
// the generated module's after the first generation. Keeping them either way is
// what lets setup run on a repository that already holds generated output, and
// the first generation rewrites the template's copies in any case.
var retainedFiles = []string{
	".gitattributes",
	".gitignore",
	"LICENSE",
	"NOTICE",
	"README.md",
	config.DefaultFileName,
	"doc.go",
}

// retainedDirs are the directory prefixes a derived repository keeps untouched.
var retainedDirs = []string{"patches/"}

// composedFile is one file setup owns, with the exact content it will hold.
type composedFile struct {
	path     string
	contents []byte
}

// checkTemplateMarker refuses a tree that is not an untouched template checkout.
//
// Both halves are needed. The present markers say this is soapbox rather than
// some other repository the operator happened to be standing in, and the absent
// one says setup has not already run here, which is the difference between a
// transformation and a second transformation of its own output.
func checkTemplateMarker(tracked map[string]struct{}) error {
	var missing []string
	for _, marker := range templateMarkers {
		if _, ok := tracked[marker]; !ok {
			missing = append(missing, marker)
		}
	}
	if len(missing) > 0 {
		return policyErrorf("setup: %w: %s is not tracked", ErrNotTemplate, strings.Join(missing, ", "))
	}
	for _, marker := range templateAbsentMarkers {
		if _, ok := tracked[marker]; ok {
			return policyErrorf("setup: %w: %s is already tracked, so this repository has a root module the template does not have", ErrNotTemplate, marker)
		}
	}
	return nil
}

// retained reports whether a path is one the derived repository keeps as it is.
func (r *run) retained(p string) bool {
	if slices.Contains(retainedFiles, p) {
		return true
	}
	facade := r.opts.Config.Facade
	if p == facade.File || p == facade.AssertionsFile {
		return true
	}
	for _, prefix := range retainedDirs {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	// The relocated upstream tree exists only after a generation and is kept
	// whenever it does, because setup transforms the repository shape and never
	// the module's content.
	if prefix := r.opts.Config.Destination.InternalPrefix; prefix != "" {
		if strings.HasPrefix(p, path.Clean(prefix)+"/") {
			return true
		}
	}
	return false
}

// templateOwned reports whether a path belongs to the template itself.
func templateOwned(p string) bool {
	if slices.Contains(templateOwnedFiles, p) {
		return true
	}
	for _, prefix := range templateOwnedDirs {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

// checkTemplateOwnedDisk refuses ignored or otherwise untracked artifacts under
// directories setup intends to prune. Leaving one behind would keep the
// template's directory hierarchy in the derived repository without naming the
// survivor in the approved manifest.
func checkTemplateOwnedDisk(root string, tracked map[string]struct{}, composed map[string]bool) error {
	for _, prefix := range templateOwnedDirs {
		dir := filepath.Join(root, filepath.FromSlash(strings.TrimSuffix(prefix, "/")))
		if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
			continue
		} else if err != nil {
			return fmt.Errorf("setup: inspect %s: %w", prefix, err)
		}
		err := filepath.WalkDir(dir, func(name string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if name == dir {
				return nil
			}
			relative, err := filepath.Rel(root, name)
			if err != nil {
				return err
			}
			relative = filepath.ToSlash(relative)
			if entry.Type()&os.ModeSymlink != 0 {
				return policyErrorf("setup: %w: untracked or ignored symbolic link %s exists in a template-owned directory", ErrSymlink, relative)
			}
			if entry.IsDir() || composed[relative] {
				return nil
			}
			if _, ok := tracked[relative]; !ok {
				return policyErrorf("setup: %w: untracked or ignored file %s exists in a template-owned directory", ErrUnknownOverwrite, relative)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// classify turns the measured tree and the composed payload into the manifest.
//
// Three rules decide every tracked path, in this order: a path setup composes is
// written, a path the derived repository keeps is left alone, and a path the
// template owns is removed. Everything else is ignored, which here means
// preserved and reported rather than silently skipped, because a file this
// package does not recognise is far more likely to be the operator's than a
// template file somebody forgot to list.
func (r *run) classify(pin enginePin) error {
	actions := make([]Action, 0, len(r.payload)+len(r.tracked))
	composed := make(map[string]bool, len(r.payload))

	// The worktree status deliberately excludes ignored files, so every path
	// setup would create is checked on disk as well. An ignored go.mod or shim
	// file is still an operator-owned file and must never be clobbered silently.
	root, err := os.OpenRoot(r.opts.Root)
	if err != nil {
		return fmt.Errorf("setup: open repository: %w", err)
	}
	defer func() { _ = root.Close() }()

	for _, file := range r.payload {
		composed[file.path] = true
		kind := ActionCreate
		if _, exists := r.tracked[file.path]; !exists {
			if _, err := root.Stat(file.path); err == nil {
				for tracked := range r.tracked {
					if strings.EqualFold(tracked, file.path) {
						return policyErrorf("setup: %w: %s and %s differ only by letter case", ErrCaseCollision, tracked, file.path)
					}
				}
				return policyErrorf("setup: %w: untracked or ignored file %s already exists", ErrUnknownOverwrite, file.path)
			} else if !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("setup: inspect %s: %w", file.path, err)
			}
		} else {
			if strings.HasPrefix(file.path, ".github/workflows/") && !slices.Contains(templateOwnedFiles, file.path) {
				return policyErrorf("setup: %w: %s", ErrUnknownOverwrite, file.path)
			}
			if !templateOwned(file.path) {
				return policyErrorf("setup: %w: %s", ErrUnknownOverwrite, file.path)
			}
			kind = ActionReplace
		}
		actions = append(actions, Action{
			Path:   file.path,
			Kind:   kind,
			Digest: digest(file.contents),
			Bytes:  len(file.contents),
		})
	}
	if err := checkTemplateOwnedDisk(r.opts.Root, r.tracked, composed); err != nil {
		return err
	}

	var kept, ignored []string
	for p := range r.tracked {
		switch {
		case composed[p]:
			continue
		case r.retained(p):
			kept = append(kept, p)
		case templateOwned(p):
			contents, err := readTracked(root, p)
			if err != nil {
				return err
			}
			actions = append(actions, Action{
				Path:   p,
				Kind:   ActionDelete,
				Digest: digest(contents),
				Bytes:  len(contents),
			})
		default:
			ignored = append(ignored, p)
		}
	}

	slices.SortFunc(actions, func(a, b Action) int {
		if by := cmp.Compare(a.Path, b.Path); by != 0 {
			return by
		}
		return cmp.Compare(a.Kind, b.Kind)
	})
	slices.Sort(kept)
	slices.Sort(ignored)

	if err := checkCaseCollisions(actions, kept, ignored); err != nil {
		return err
	}

	r.report = Report{
		Schema:  ReportSchema,
		Engine:  engineReport(pin, r.composedSum),
		Module:  moduleReport(r.opts.Config),
		Actions: actions,
		Kept:    nonNil(kept),
		Ignored: nonNil(ignored),
		Notices: nonNil(r.notices),
		Totals:  totalsOf(actions, kept, ignored),
	}
	hash, err := r.report.computeHash()
	if err != nil {
		return err
	}
	r.report.Hash = hash
	return nil
}

// readTracked reads one tracked file from the work tree.
//
// The work tree is clean and every path here is tracked, so reading the file is
// reading what HEAD records, and it costs one open rather than one subprocess.
// The read goes through the caller's [os.Root] on the repository, so a path that
// tried to leave it fails at the operating system rather than at a check this
// package would have to keep correct.
func readTracked(root *os.Root, p string) ([]byte, error) {
	contents, err := root.ReadFile(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, policyErrorf("setup: %s is tracked but missing from the work tree: %w", p, err)
		}
		return nil, fmt.Errorf("setup: read %s: %w", p, err)
	}
	return contents, nil
}

// checkCaseCollisions refuses a manifest two paths of which a case insensitive
// file system could not tell apart.
//
// Deletions and preserved files are covered as well as writes, because the
// failure this guards against is a delete that removes the file a write was
// about to land on, and that pairing only exists between paths of different
// kinds.
func checkCaseCollisions(actions []Action, kept, ignored []string) error {
	all := make([]string, 0, len(actions)+len(kept)+len(ignored))
	for _, action := range actions {
		all = append(all, action.Path)
	}
	all = append(all, kept...)
	all = append(all, ignored...)
	slices.Sort(all)
	all = slices.Compact(all)

	folded := make(map[string]string, len(all))
	for _, p := range all {
		fold := strings.ToLower(p)
		if first, ok := folded[fold]; ok {
			return policyErrorf("setup: %w: %s and %s", ErrCaseCollision, first, p)
		}
		folded[fold] = p
	}
	return nil
}

// totalsOf counts the manifest by kind so a summary need not walk it.
func totalsOf(actions []Action, kept, ignored []string) Totals {
	totals := Totals{Kept: len(kept), Ignored: len(ignored)}
	for _, action := range actions {
		switch action.Kind {
		case ActionCreate:
			totals.Create++
		case ActionReplace:
			totals.Replace++
		case ActionDelete:
			totals.Delete++
		}
	}
	return totals
}

// nonNil renders an empty list as an empty JSON array rather than as null, so
// two manifests that differ only in emptiness still encode identically.
func nonNil(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
