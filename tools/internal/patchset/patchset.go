// Package patchset selects and applies the ordered unified diffs a profile
// carries for one source commit.
//
// Patches exist to export internals and to adapt upstream code that cannot be
// consumed as written. They are applied against upstream paths in the
// materialized source tree, before relocation and before any import rewriting,
// so a patch author writes and reads a diff in upstream coordinates and never
// has to track the generated module layout.
//
// Selection is deterministic. A patch series is an ordered list, selection
// preserves that order, and the ancestry selectors are evaluated in the same
// order for every run, so two runs over the same source commit choose the same
// patches in the same sequence.
//
// Application is all or nothing. A failed patch stops the complete ref
// transaction: the work tree is restored to the pre-patch state, no consumer
// ref moves, and the caller receives a typed [ConflictError] carrying the
// source ref and SHA, the failing patch, the porcelain status, the work tree
// diff with its conflict markers, and the conflicted paths.
//
// This package never starts a subprocess. Every Git operation goes through the
// [Git] interface, which the engine binds to the typed runner in
// internal/gitcli, the one subprocess and credential boundary.
package patchset

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/enj/soapbox/tools/internal/gitcli"
)

// Selection and application sentinels. Callers use errors.Is to distinguish the
// failure from an underlying Git error.
var (
	// ErrNoGit reports a nil Git binding.
	ErrNoGit = errors.New("a git binding is required")
	// ErrDuplicatePatch reports two patches sharing one identifier, which would
	// make a report or a provenance record ambiguous.
	ErrDuplicatePatch = errors.New("patch identifier is not unique")
	// ErrEmptyPatch reports a patch that carries no diff bytes.
	ErrEmptyPatch = errors.New("patch carries no diff")
	// ErrNoIdentifier reports a patch with no identifier.
	ErrNoIdentifier = errors.New("patch has no identifier")
	// ErrIncompleteTarget reports a target that does not name both a branch and a
	// commit. Both are required because a patch may be selected by either.
	ErrIncompleteTarget = errors.New("target must name a source branch and a source commit")
	// ErrIncompleteRequest reports an application request that does not identify
	// the source ref transaction it belongs to. A conflict report is worthless
	// without it.
	ErrIncompleteRequest = errors.New("request must name a source ref and a source SHA")
	// ErrDirtyWorkTree reports a repository whose index or work tree does not
	// match HEAD when a patch series is about to be applied. The rollback is a
	// hard reset to HEAD, so uncommitted content would either be destroyed by it
	// or survive it, and neither is a rollback.
	ErrDirtyWorkTree = errors.New("index and work tree must match HEAD before patches are applied")
)

// Patch is one ordered unified diff and the selectors that decide which source
// commits it applies to.
//
// The zero value is not usable: a patch needs an identifier and diff bytes.
type Patch struct {
	// ID identifies the patch in conflict reports and provenance records. The
	// loader sets it to the configured repository relative file path, which is
	// unique across a profile.
	ID string
	// Diff is the complete unified diff. It is handed to Git verbatim; this
	// package never rewrites, reflows, or reorders a diff.
	Diff []byte
	// Since is the first source commit the patch applies to. Empty means the
	// patch applies to every commit no later than Until.
	Since string
	// Until is the first source commit the patch no longer applies to, which is
	// normally the upstream commit that made the patch unnecessary. Empty means
	// the patch never expires.
	//
	// Together the selectors describe the half open ancestry range
	// [Since, Until). Half open is the only choice that lets one patch stop
	// exactly where its successor starts without the two overlapping on the
	// boundary commit.
	Until string
	// Branches restricts the patch to the named tracked source branches. Empty
	// means every branch, which is the common case for a patch that fixes
	// something present on all of them.
	Branches []string
}

// Clone returns a deep copy so a caller cannot mutate a selected series through
// a shared slice.
func (p Patch) Clone() Patch {
	p.Diff = slices.Clone(p.Diff)
	p.Branches = slices.Clone(p.Branches)
	return p
}

// Target is the source commit a patch series is selected for.
type Target struct {
	// Branch is the tracked source branch being replayed. It is matched exactly
	// against a patch's branch selector.
	Branch string
	// Commit is the source commit object name. It is the descendant side of
	// every ancestry query.
	Commit string
}

// validate rejects a target that cannot answer both selector kinds.
func (t Target) validate() error {
	if t.Branch == "" || t.Commit == "" {
		return ErrIncompleteTarget
	}
	return nil
}

// Git is the Git surface this package needs.
//
// The method set is exactly the subset of the typed runner in internal/gitcli
// that patch application uses, so the engine binds the real runner directly and
// no adapter stands between this package and the one subprocess boundary. The
// option and status types come from gitcli for the same reason: a parallel set
// of types here would have to be kept in agreement with git's own vocabulary
// twice.
//
// Every method operates on the repository the binding is rooted at, which is
// the materialized upstream source tree.
type Git interface {
	// IsAncestor reports whether ancestor is an ancestor of descendant, or is
	// descendant itself, which is what selection asks of the source graph.
	IsAncestor(ctx context.Context, ancestor, descendant string) (bool, error)

	// ApplyPatch applies one unified diff. This package always requests three
	// way and index semantics and exposes no knob to weaken them, because a
	// strict apply would reject a patch whose context drifted by a line and
	// would report nothing a maintainer could act on, and an apply that skipped
	// the index would hide the patched files from the pruning and tree building
	// steps that follow.
	ApplyPatch(ctx context.Context, opts gitcli.ApplyOptions) error

	// Status reports the work tree state, which a failed apply leaves holding
	// the unmerged paths.
	Status(ctx context.Context) ([]gitcli.StatusEntry, error)

	// Diff renders a unified diff for the conflict report, including any
	// conflict markers a three way apply left behind.
	Diff(ctx context.Context, opts gitcli.DiffOptions) (string, error)

	// ResetHard restores the index and work tree, discarding a failed
	// application. It is the rollback that makes a patch pass all or nothing.
	ResetHard(ctx context.Context, revision string) error
}

// validateSeries checks the invariants every entry point relies on: each patch
// is usable and identifiers are unique.
func validateSeries(patches []Patch) error {
	seen := make(map[string]struct{}, len(patches))
	for i, patch := range patches {
		switch {
		case patch.ID == "":
			return fmt.Errorf("patch %d: %w", i, ErrNoIdentifier)
		case len(patch.Diff) == 0:
			return fmt.Errorf("patch %q: %w", patch.ID, ErrEmptyPatch)
		}
		if _, ok := seen[patch.ID]; ok {
			return fmt.Errorf("patch %q: %w", patch.ID, ErrDuplicatePatch)
		}
		seen[patch.ID] = struct{}{}
	}
	return nil
}

// blank reports whether the diff carries no non-space byte, which is how an
// accidentally truncated or placeholder patch file presents itself.
func blank(diff []byte) bool {
	return strings.TrimSpace(string(diff)) == ""
}
