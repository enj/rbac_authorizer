package modulegraph

import (
	"errors"
	"fmt"
	"strings"

	"github.com/enj/soapbox/tools/internal/gitcli"
)

// Module graph failure sentinels.
//
// There is no verdict among these. This package decides nothing: it loads a
// module and hands the policy packages a graph, so every failure here is a
// graph that cannot be judged rather than a judgement. That distinction is why
// none of these can be downgraded to a warning; a policy handed an incomplete
// graph does not report less confidence, it reports a clean run over code it
// never saw.
var (
	// ErrOptions reports a load that was asked for in a way that cannot be
	// answered reproducibly.
	ErrOptions = errors.New("module graph options are invalid")
	// ErrLoad reports a load that did not produce a judgeable graph, whether
	// because the go command failed, a package did not compile, a pattern
	// matched nothing, or the loader returned less than was asked of it.
	ErrLoad = errors.New("module graph could not be loaded")
	// ErrPackageMissing reports an adapter naming a package the load does not
	// contain. It is never softened into an empty graph entry: a boundary
	// package or candidate that silently vanished would be judged as having no
	// surface and no cost, which passes every gate it should fail.
	ErrPackageMissing = errors.New("package is not in the module graph")
	// ErrRelabelUnproven reports an upstream relabel this package cannot prove
	// is sound. See the relabel documentation for why an unproven relabel is
	// refused rather than applied to the paths it can reach.
	ErrRelabelUnproven = errors.New("upstream relabel cannot be proven")
	// ErrModuleConflict reports caller supplied module facts that contradict
	// the module identity the load resolved. The two cannot be reconciled
	// here, and picking either one would attach measured evidence to a module
	// it was not measured against.
	ErrModuleConflict = errors.New("module facts contradict the loaded module identity")
)

// GraphError reports every structural problem found in one load or adaptation.
//
// Reporting all of them at once follows the policy packages this feeds: an
// operator fixes what they are shown, and a validator that stops at the first
// problem turns one edit into several rounds.
type GraphError struct {
	// Action names what was being built, so a failure says whether the load or
	// one of the adapters refused.
	Action string
	// Problems are the structural problems, in a deterministic order.
	Problems []string
	// Err is the sentinel cause.
	Err error
}

// Error renders each problem on its own line.
func (e *GraphError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %v: %d problems", e.Action, e.Err, len(e.Problems))
	for _, problem := range e.Problems {
		b.WriteString("\n  - ")
		b.WriteString(problem)
	}
	return b.String()
}

// Unwrap exposes the sentinel so a caller can classify the failure.
func (e *GraphError) Unwrap() error { return e.Err }

// maxReportedProblems bounds how many problems one failure reports. A module
// that does not compile produces cascades, and the first few in a deterministic
// order are what identifies the cause.
const maxReportedProblems = 20

// problems accumulates structural problems in the order they are found.
type problems struct {
	found []string
}

// addf records one problem.
func (p *problems) addf(format string, args ...any) {
	p.found = append(p.found, fmt.Sprintf(format, args...))
}

// err returns a GraphError, or nil when nothing was found.
//
// Every problem is scrubbed on the way out rather than at the point it was
// written. Problems quote go command diagnostics, module paths, and file
// positions, any of which can carry the module proxy URL, and a redaction that
// had to be remembered at each of the several dozen addf call sites is a
// redaction that will eventually be forgotten at one of them. A nil redactor is
// tolerated here because the option check rejects one before a load begins;
// this stays nil safe so that check can itself report a problem.
func (p *problems) err(redactor *gitcli.Redactor, action string, sentinel error) error {
	if len(p.found) == 0 {
		return nil
	}
	reported := p.found
	if len(reported) > maxReportedProblems {
		reported = append(reported[:maxReportedProblems:maxReportedProblems],
			fmt.Sprintf("(and %d more)", len(p.found)-maxReportedProblems))
	}
	return &GraphError{
		Action:   redactor.String(action),
		Problems: redactor.Strings(reported),
		Err:      sentinel,
	}
}
