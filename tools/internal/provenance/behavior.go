package provenance

import (
	"fmt"
	"slices"
	"strings"

	"github.com/enj/soapbox/tools/internal/typeswap"
)

// BehaviorChangesFrom converts a type policy analysis into the behaviour
// changes the root NOTICE records.
//
// The conversion exists because the two packages answer different halves of one
// question. The type policy analysis decides whether replacing an internal API
// type with its external counterpart is safe, and in doing so it finds the
// import time effects the change removes: a scheme that stops being populated,
// an init function that stops running. Those are precisely the differences no
// diff of the copied files would reveal, and they have to reach the published
// record rather than stopping at an analysis report nobody ships.
//
// An observable change is one a consumer can reach through the generated public
// API, which the analysis treats as a blocker. It is converted anyway, so that a
// profile which overrode the blocker cannot also omit the disclosure: if the
// change is being made, it is being written down.
//
// The result is sorted and deduplicated, because one effect can be found
// through several pairings and a record that listed it twice would suggest two
// separate changes.
func BehaviorChangesFrom(result *typeswap.Result) []BehaviorChange {
	if result == nil {
		return nil
	}
	var changes []BehaviorChange
	for _, pair := range result.Pairs {
		for _, change := range pair.BehaviorChanges {
			changes = append(changes, behaviorChangeFrom(pair, change))
		}
	}
	slices.SortFunc(changes, compareBehaviorChanges)
	return slices.CompactFunc(changes, func(a, b BehaviorChange) bool { return compareBehaviorChanges(a, b) == 0 })
}

// behaviorChangeFrom renders one analysed effect as a published record.
//
// The summary names the effect and the pairing that removes it, because a
// reader of the NOTICE has neither the analysis report nor the upstream tree
// and otherwise could not tell which substitution the entry is about. The
// position is deliberately not carried into the summary: it names a file and
// line of the upstream checkout, which the published record must not depend on.
func behaviorChangeFrom(pair typeswap.PairReport, change typeswap.BehaviorChange) BehaviorChange {
	summary := change.Symbol + " no longer performs its " + change.Kind + " effect at import time."
	detail := change.Detail
	reach := "It is not reachable through the generated public API."
	if change.Observable {
		reach = "It is reachable through the generated public API, so a consumer can observe the difference."
	}
	if detail != "" && !strings.HasSuffix(detail, ".") {
		detail += "."
	}
	return BehaviorChange{
		Summary: summary,
		Cause:   "type policy " + string(pair.Action) + " for " + pair.Internal + " paired with " + pair.External,
		Detail:  strings.TrimSpace(detail + " " + reach),
	}
}

// compareBehaviorChanges orders published behaviour changes deterministically.
func compareBehaviorChanges(a, b BehaviorChange) int {
	if order := strings.Compare(a.Summary, b.Summary); order != 0 {
		return order
	}
	if order := strings.Compare(a.Cause, b.Cause); order != 0 {
		return order
	}
	return strings.Compare(a.Detail, b.Detail)
}

// CheckBehaviorChanges refuses root provenance that omits an analysed effect.
//
// The disclosure is only worth anything if it cannot be forgotten. A profile
// that runs the type policy analysis, acts on its decision, and then renders a
// NOTICE without the effects that decision removes has published a module whose
// documented behaviour is not its behaviour, and nothing else in the pipeline
// would notice.
func (o Options) CheckBehaviorChanges(result *typeswap.Result) error {
	recorded := make(map[string]bool, len(o.BehaviorChanges))
	for _, change := range o.BehaviorChanges {
		recorded[change.Summary] = true
	}
	var missing []string
	for _, change := range BehaviorChangesFrom(result) {
		if !recorded[change.Summary] {
			missing = append(missing, change.Summary)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("root provenance: the type policy analysis found effects the notice does not record: %w:\n  %s",
		ErrEvidence, strings.Join(missing, "\n  "))
}
