package patchset

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/enj/soapbox/tools/internal/gitcli"
)

// headRevision is the pre-patch state every pass starts from and rolls back to.
// The caller records the materialized and pruned tree as HEAD before the first
// patch, because three way application resolves blobs through the index and
// object store and the rollback restores exactly that state.
const headRevision = "HEAD"

// Stage names the step of a patch pass that failed. It is part of the conflict
// report because a diff that applies cleanly but reintroduces a pruned file is
// a different maintenance problem from a diff that no longer applies.
type Stage string

const (
	// StageApply reports that Git could not apply the diff.
	StageApply Stage = "apply"
	// StagePrune reports that the caller's prune reassertion rejected the tree
	// the patch produced.
	StagePrune Stage = "prune"
	// StageCancel reports that the caller's context ended before the patch was
	// attempted. It is a distinct stage because nothing is wrong with the patch
	// or the tree: the pass was stopped from outside, and a maintainer reading
	// the report needs to see that rather than hunt for a conflict that is not
	// there.
	StageCancel Stage = "cancel"
)

// rollbackBudget bounds the detached rollback.
//
// Evidence collection and the rollback run on a context derived from the
// caller's with its cancellation removed, because the most common reason to
// reach them is that the caller's context ended, and a rollback that inherited
// that cancellation would fail immediately and leave exactly the partially
// patched tree it exists to prevent. The budget keeps a wedged Git from hanging
// a run forever; it is generous because the three commands it covers are local
// and fast even on a large tree.
const rollbackBudget = 2 * time.Minute

// Request describes one patch pass over a materialized upstream source tree.
type Request struct {
	// SourceRef is the tracked source ref the transaction belongs to.
	SourceRef string
	// SourceSHA is the source commit being transformed.
	SourceSHA string
	// Patches are the selected patches, in application order.
	Patches []Patch
	// ReassertPrune is called after every applied patch with the patch that was
	// just applied. Pruning is reasserted after every patch rather than once at
	// the end so the report names the exact patch that reintroduced a pruned
	// file. A nil callback skips reassertion, which is only appropriate for a
	// profile with no prune entries.
	ReassertPrune func(ctx context.Context, patch Patch) error
}

// validate rejects a request that could not produce a usable conflict report.
func (r Request) validate() error {
	if r.SourceRef == "" || r.SourceSHA == "" {
		return ErrIncompleteRequest
	}
	return validateSeries(r.Patches)
}

// AppliedPatch records one patch that applied cleanly.
type AppliedPatch struct {
	// ID is the patch identifier, which provenance records verbatim.
	ID string
	// Index is the zero based position of the patch in the applied series.
	Index int
}

// Result reports a complete, successful pass.
type Result struct {
	// Applied lists every patch that was applied, in application order.
	Applied []AppliedPatch
}

// PatchIDs reports the applied patch identifiers in application order, which is
// the form a provenance record stores.
func (r Result) PatchIDs() []string {
	ids := make([]string, len(r.Applied))
	for i, applied := range r.Applied {
		ids[i] = applied.ID
	}
	return ids
}

// Apply applies a selected patch series against the materialized upstream tree.
//
// The pass is all or nothing. Every patch must apply and every prune
// reassertion must pass; the first failure restores the tree to HEAD and
// returns a [ConflictError]. There is no partially patched success state,
// because a half applied series would produce a tree that no profile describes
// and that no maintainer could reproduce.
//
// The caller must record the pre-patch state, the materialized and pruned tree,
// as HEAD of the repository the binding is rooted at. Three way application
// resolves blobs through the index and object store, and the rollback restores
// exactly that state, so a pre-patch state that only exists as uncommitted work
// tree content can neither be merged against nor restored. A series with at
// least one patch therefore refuses to start unless the work tree and index
// already match HEAD; see [ErrDirtyWorkTree].
func Apply(ctx context.Context, git Git, req Request) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, fmt.Errorf("apply patches: %w", err)
	}
	if git == nil {
		return Result{}, fmt.Errorf("apply patches: %w", ErrNoGit)
	}
	if err := req.validate(); err != nil {
		return Result{}, fmt.Errorf("apply patches: %w", err)
	}
	// An empty series touches nothing, so it neither needs the precondition nor
	// may impose it: a source commit the profile has no patches for must not
	// require a committed tree to pass through.
	if len(req.Patches) > 0 {
		if err := requireCleanHead(ctx, git); err != nil {
			return Result{}, fmt.Errorf("apply patches: %w", err)
		}
	}

	applied := make([]AppliedPatch, 0, len(req.Patches))
	for i, patch := range req.Patches {
		if err := ctx.Err(); err != nil {
			// Cancellation between patches is not a clean stop. Every patch
			// already applied is staged in the index and present in the work
			// tree, so the pass leaves through the same rollback a conflict
			// does. Returning here instead would publish the one thing this
			// function promises never to produce: a partially patched tree.
			return Result{}, conflict(ctx, git, req, StageCancel, i, err)
		}
		if err := git.ApplyPatch(ctx, gitcli.ApplyOptions{Patch: patch.Diff, ThreeWay: true, Index: true}); err != nil {
			return Result{}, conflict(ctx, git, req, StageApply, i, err)
		}
		if req.ReassertPrune != nil {
			if err := req.ReassertPrune(ctx, patch); err != nil {
				return Result{}, conflict(ctx, git, req, StagePrune, i, err)
			}
		}
		applied = append(applied, AppliedPatch{ID: patch.ID, Index: i})
	}
	return Result{Applied: applied}, nil
}

// requireCleanHead enforces the precondition [Apply] documents: the repository
// holds the pre-patch state as HEAD, with an index and work tree that match it
// exactly.
//
// The check exists because the rollback is git reset --hard, and a reset is
// only a rollback when there is nothing to lose. A modified or staged tracked
// file would be discarded rather than restored, which destroys a caller's
// materialization; an untracked file would survive the reset and leave the
// restored tree holding content the pre-patch state never had. Both are silent
// failures of the all or nothing guarantee, so the pass refuses to start rather
// than promise something it could not keep.
func requireCleanHead(ctx context.Context, git Git) error {
	entries, err := git.Status(ctx)
	if err != nil {
		return fmt.Errorf("inspect work tree: %w", err)
	}
	if len(entries) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrDirtyWorkTree, describeDirty(entries))
}

// conflict captures the failure evidence and rolls the tree back.
//
// Evidence is collected before the rollback because the rollback destroys it.
// A failure to collect evidence, or a failed rollback, is joined into the
// returned cause rather than replacing it: the original failure is what a
// maintainer needs, and a failed rollback is an additional problem that must
// not be swallowed.
//
// Every Git call here runs on a context detached from the caller's and bounded
// by [rollbackBudget]. Detaching is the whole point: a cancelled caller context
// is one of the ways this function is reached, and inheriting that cancellation
// would turn the rollback into a no-op that reports success at restoring
// nothing.
func conflict(ctx context.Context, git Git, req Request, stage Stage, index int, cause error) error {
	patch := req.Patches[index]
	report := &ConflictError{
		SourceRef:  req.SourceRef,
		SourceSHA:  req.SourceSHA,
		Stage:      stage,
		PatchID:    patch.ID,
		PatchIndex: index,
		PatchCount: len(req.Patches),
		Err:        cause,
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackBudget)
	defer cancel()

	var problems []error
	if entries, err := git.Status(ctx); err != nil {
		problems = append(problems, fmt.Errorf("collect status: %w", err))
	} else {
		report.Status = entries
		report.ConflictedPaths = conflictedPaths(entries)
	}
	// The comparison is against HEAD rather than the index so the report shows
	// everything the series changed, not only what is still unstaged. A three
	// way conflict records its markers in the index as well as the work tree, so
	// an index relative diff would omit exactly the evidence a maintainer needs.
	if diff, err := git.Diff(ctx, gitcli.DiffOptions{Revision: headRevision}); err != nil {
		problems = append(problems, fmt.Errorf("collect work tree diff: %w", err))
	} else {
		report.Diff = diff
	}
	if err := git.ResetHard(ctx, headRevision); err != nil {
		problems = append(problems, fmt.Errorf("roll back patch series: %w", err))
	}

	if len(problems) > 0 {
		report.Err = errors.Join(append([]error{cause}, problems...)...)
	}
	return report
}

// ConflictError reports a patch pass that could not complete. It carries
// everything a maintainer needs to reproduce the failure without rerunning the
// pipeline, and everything CI needs to render an artifact and update the keyed
// tracking issue.
type ConflictError struct {
	// SourceRef and SourceSHA identify the abandoned ref transaction.
	SourceRef string
	SourceSHA string
	// Stage names the step that failed.
	Stage Stage
	// PatchID, PatchIndex, and PatchCount locate the failure in the series.
	PatchID    string
	PatchIndex int
	PatchCount int
	// Status is the porcelain status captured before the rollback.
	Status []gitcli.StatusEntry
	// Diff is the work tree diff captured before the rollback. For a three way
	// apply it contains the conflict markers.
	Diff string
	// ConflictedPaths lists the unmerged paths, sorted and deduplicated.
	ConflictedPaths []string
	// Err is the underlying failure, joined with any evidence collection or
	// rollback failure.
	Err error
}

// Error renders a single line naming the failure and its location.
func (e *ConflictError) Error() string {
	message := fmt.Sprintf("patch %q (%d of %d) failed at the %s stage for %s at %s",
		e.PatchID, e.PatchIndex+1, e.PatchCount, e.Stage, e.SourceRef, e.SourceSHA)
	if len(e.ConflictedPaths) > 0 {
		message += ": conflicted paths " + strings.Join(e.ConflictedPaths, ", ")
	}
	if e.Err != nil {
		message += ": " + e.Err.Error()
	}
	return message
}

// Unwrap exposes the underlying failure.
func (e *ConflictError) Unwrap() error { return e.Err }

// Report renders the deterministic conflict artifact.
//
// The rendering is stable for a given failure: fixed section order, sorted
// paths, and status entries in the order Git reported them, which is itself
// sorted by path. The caller adds the profile identity it owns.
func (e *ConflictError) Report() string {
	var b strings.Builder
	b.WriteString("soapbox patch conflict\n")
	writeField(&b, "source ref", e.SourceRef)
	writeField(&b, "source sha", e.SourceSHA)
	writeField(&b, "stage", string(e.Stage))
	writeField(&b, "patch", e.PatchID)
	writeField(&b, "position", strconv.Itoa(e.PatchIndex+1)+" of "+strconv.Itoa(e.PatchCount))
	if e.Err != nil {
		writeField(&b, "error", e.Err.Error())
	}

	b.WriteString("\nconflicted paths:\n")
	if len(e.ConflictedPaths) == 0 {
		b.WriteString("  (none)\n")
	}
	for _, path := range e.ConflictedPaths {
		b.WriteString("  " + path + "\n")
	}

	b.WriteString("\nstatus:\n")
	if len(e.Status) == 0 {
		b.WriteString("  (clean)\n")
	}
	for _, entry := range e.Status {
		b.WriteString("  " + renderStatus(entry) + "\n")
	}

	b.WriteString("\ndiff:\n")
	if e.Diff == "" {
		b.WriteString("  (empty)\n")
		return b.String()
	}
	b.WriteString(e.Diff)
	if !strings.HasSuffix(e.Diff, "\n") {
		b.WriteString("\n")
	}
	return b.String()
}

// writeField renders one report header field.
func writeField(b *strings.Builder, name, value string) {
	b.WriteString(name + ": " + value + "\n")
}
