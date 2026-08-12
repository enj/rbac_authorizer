package publish

import (
	"context"
	"errors"
	"testing"

	"github.com/enj/soapbox/tools/internal/gitcli"
)

// TestPublicationLifecycle walks one destination through the sequence a real
// release takes: an empty repository, a first publication, a later one that
// advances the branch and adds a tag, and a re-run that must do nothing.
//
// It runs under both hash algorithms, because object names are the values this
// package compares and a SHA-256 destination is not a SHA-1 one with longer
// strings: every length check, every null object check, and every comparison
// has to be told in the destination's own algorithm.
func TestPublicationLifecycle(t *testing.T) {
	t.Parallel()

	formats := []gitcli.ObjectFormat{gitcli.ObjectFormatSHA1, gitcli.ObjectFormatSHA256}
	for _, format := range formats {
		t.Run(string(format), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			if format == gitcli.ObjectFormatSHA256 {
				requireSHA256(ctx, t)
			}
			d := newDestination(ctx, t, format)

			first := d.commit("doc.go", "package rbac\n", "first")
			firstTag := d.tag("v0.36.1", first, "release v0.36.1")

			// The first publication creates everything, and the bookkeeping
			// travels in its own push before the consumer refs move.
			plan := d.planUpdates(
				branchUpdate(testBranch, first),
				tagUpdate(tagPrefix+"v0.36.1", firstTag),
				Update{Ref: testStateRef, Kind: KindState, NewObject: first, Evidence: "state:cursor"},
			)
			d.apply(plan, ScopeNonConsumer)
			d.requireRemote(map[string]string{testStateRef: first})
			d.apply(plan, ScopeConsumer)
			d.requireRemote(map[string]string{
				testStateRef:          first,
				testBranch:            first,
				tagPrefix + "v0.36.1": firstTag,
			})

			// The next release fast forwards the branch and adds a tag. The
			// published tag is named again and must stay a no-op rather than
			// becoming a second write of the same value.
			second := d.commit("doc.go", "package rbac // updated\n", "second")
			secondTag := d.tag("v0.36.2", second, "release v0.36.2")
			plan = d.planUpdates(
				branchUpdate(testBranch, second),
				tagUpdate(tagPrefix+"v0.36.1", firstTag),
				tagUpdate(tagPrefix+"v0.36.2", secondTag),
			)
			effects := map[string]Effect{
				testBranch:            EffectFastForward,
				tagPrefix + "v0.36.1": EffectNoOp,
				tagPrefix + "v0.36.2": EffectCreate,
			}
			for _, action := range plan.Manifest.Actions {
				if effects[action.Ref] != action.Effect {
					t.Errorf("%s = %q, want %q", action.Ref, string(action.Effect), string(effects[action.Ref]))
				}
			}
			result := d.apply(plan, ScopeConsumer)
			if len(result.Pushed) != 2 || len(result.NoOps) != 1 {
				t.Errorf("pushed %v and skipped %v, want two pushes and one no-op", result.Pushed, result.NoOps)
			}
			d.requireRemote(map[string]string{
				testStateRef:          first,
				testBranch:            second,
				tagPrefix + "v0.36.1": firstTag,
				tagPrefix + "v0.36.2": secondTag,
			})

			// Running the same publication again is a supported outcome, not a
			// collision: every ref already holds what was asked for.
			repeat := d.planUpdates(
				branchUpdate(testBranch, second),
				tagUpdate(tagPrefix+"v0.36.1", firstTag),
				tagUpdate(tagPrefix+"v0.36.2", secondTag),
			)
			again := d.apply(repeat, ScopeConsumer)
			if len(again.Pushed) != 0 {
				t.Errorf("a repeated publication pushed %v", again.Pushed)
			}

			// A release that tried to reuse a published version is fatal, and
			// the destination is left exactly as it was.
			published := d.remoteRefs()
			replacement := d.tag("v0.36.1-again", second, "release v0.36.1")
			_, err := d.pub.Plan(ctx, []Update{tagUpdate(tagPrefix+"v0.36.1", replacement)})
			if !errors.Is(err, ErrTagMoved) {
				t.Fatalf("replanning a published tag = %v, want %v", err, ErrTagMoved)
			}
			d.requireRemote(published)
		})
	}
}
