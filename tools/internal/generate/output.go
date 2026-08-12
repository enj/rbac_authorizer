package generate

import (
	"context"
	"fmt"
	"strings"

	"github.com/enj/soapbox/tools/internal/relocate"
)

// runOutput applies the last gates and writes the module.
//
// Writing is the final action of the run and it happens exactly once. Every
// earlier phase gated the tree in memory, so a generation that refuses leaves no
// output at all rather than a directory an operator has to know not to trust,
// and the write itself is atomic: the tree is assembled beside the destination
// and moved into place with a single rename, so an interrupted run leaves the
// destination as it found it.
func (r *run) runOutput(ctx context.Context) error {
	// Strict runs before the write rather than after it. A notice that becomes a
	// refusal has to prevent the output, because a refusal that arrives once the
	// tree is already on disk is a refusal an operator can ignore by using the
	// tree anyway.
	if err := r.checkStrict(); err != nil {
		return err
	}

	r.report.recordOutput(r.files, r.opts.Materialize)

	if !r.opts.Materialize {
		return nil
	}
	if err := relocate.Materialize(ctx, r.paths.Output, r.files); err != nil {
		// The report already records the tree that would have been written, so a
		// write that failed is distinguishable from a run that produced nothing.
		//
		// Writing is a filesystem operation over a tree every gate already
		// passed, so a failure here is a full disk, a permission, or a cancelled
		// context. The module is not the problem, and reporting it as a refusal
		// would send a reviewer to look for one.
		r.report.Output.Materialized = false
		return runtimeError(stageOutput, err)
	}
	return nil
}

// checkStrict turns advisory notices into a refusal when the run asked for it.
//
// The notices are the ones the phases accumulated, which includes everything the
// extraction passes reported. They never stop a run on their own, because each
// one is a finding a reviewer may reasonably accept; -strict is the setting that
// says this run is the one where nobody is going to read them.
func (r *run) checkStrict() error {
	if !r.opts.Strict || len(r.report.Notices) == 0 {
		return nil
	}
	return policyError(stageOutput, fmt.Errorf("strict mode refuses %d advisory notices: %s",
		len(r.report.Notices), strings.Join(r.report.Notices, "; ")))
}
