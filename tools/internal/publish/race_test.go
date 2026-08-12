package publish

import (
	"context"
	"errors"
	"testing"

	"github.com/enj/soapbox/tools/internal/gitcli"
)

// racingLister reads the destination and then moves it.
//
// It stages the one window a re-read cannot close by itself. Apply reads the
// remote, decides nothing drifted, and pushes; a writer that lands in between
// makes the decision stale before it is acted on. Moving the destination from
// inside the read is a deterministic way to be exactly that writer, and the
// move is a real push to the real repository rather than a pretend one.
type racingLister struct {
	inner *LocalRemote
	// after names the read to move the destination behind: 1 is Apply's own
	// drift read, which is the last thing that happens before the push.
	after int
	calls int
	move  func()
}

func (l *racingLister) RemoteRefs(ctx context.Context, remote string) ([]gitcli.Ref, error) {
	refs, err := l.inner.RemoteRefs(ctx, remote)
	l.calls++
	if l.calls == l.after {
		l.move()
	}
	return refs, err
}

// TestApplyLosesTheRaceToTheLease is the reason the push carries expected
// values rather than trusting the read that preceded it.
//
// In each case the destination moves after Apply has read it and before the
// push lands, to a value the push would otherwise have been allowed to
// overwrite: git's own rules are satisfied, and the update would be published
// against a state nobody read, planned, or approved.
func TestApplyLosesTheRaceToTheLease(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// arrange seeds the destination and returns the plan to approve.
		arrange func(d *destination, h history) *Plan
		// move is the concurrent writer, run inside Apply's drift read.
		move func(d *destination, h history)
		// want is what the destination must hold afterwards: the racing
		// writer's value, untouched.
		want func(h history) map[string]string
	}{
		{
			name: "a branch overtaken by a commit the plan would fast forward over",
			arrange: func(d *destination, h history) *Plan {
				d.seed(h.base + ":" + testBranch)
				return d.planUpdates(branchUpdate(testBranch, h.forward))
			},
			// middle sits between the approved old value and the approved new
			// one, so pushing forward is a fast forward from it and a plain
			// push would be accepted.
			move: func(d *destination, h history) { d.seed(h.middle + ":" + testBranch) },
			want: func(h history) map[string]string { return map[string]string{testBranch: h.middle} },
		},
		{
			name: "a branch create overtaken by an ancestor of what it creates",
			arrange: func(d *destination, h history) *Plan {
				return d.planUpdates(branchUpdate(testBranch, h.forward))
			},
			move: func(d *destination, h history) { d.seed(h.base + ":" + testBranch) },
			want: func(h history) map[string]string { return map[string]string{testBranch: h.base} },
		},
		{
			name: "a tag create overtaken by another release",
			arrange: func(d *destination, h history) *Plan {
				return d.planUpdates(tagUpdate(tagPrefix+"v0.36.1", h.tagBase))
			},
			move: func(d *destination, h history) { d.seed(h.tagOther + ":" + tagPrefix + "v0.36.1") },
			want: func(h history) map[string]string {
				return map[string]string{tagPrefix + "v0.36.1": h.tagOther}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			d := newDestination(ctx, t, "")
			h := d.buildHistory()
			plan := test.arrange(d, h)

			lister := &racingLister{
				inner: NewLocalRemote(d.git),
				after: 1,
				move:  func() { test.move(d, h) },
			}
			racing := d.publisher(Options{Lister: lister})

			_, err := racing.Apply(ctx, plan, ApplyOptions{Approval: plan.Hash(), Scope: ScopeConsumer})
			if err == nil {
				t.Fatal("a push that lost the race was reported as a publication")
			}
			var pushErr *PushError
			if !errors.As(err, &pushErr) {
				t.Fatalf("apply error = %v, want a *PushError", err)
			}
			if len(pushErr.Applied) != 0 {
				t.Errorf("applied %v, want nothing", pushErr.Applied)
			}
			if lister.calls < 2 {
				t.Errorf("the destination was read %d times, want a read before and after the push", lister.calls)
			}
			// The concurrent writer's value survives untouched. Nothing this
			// publication planned reached the destination.
			d.requireRemote(test.want(h))
		})
	}
}

// TestApplyStillPublishesWhenNothingRaces keeps the guard honest: the same
// path, with no concurrent writer, has to publish.
func TestApplyStillPublishesWhenNothingRaces(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newDestination(ctx, t, "")
	h := d.buildHistory()
	d.seed(h.base + ":" + testBranch)

	plan := d.planUpdates(branchUpdate(testBranch, h.forward), tagUpdate(tagPrefix+"v0.36.1", h.tagBase))
	result := d.apply(plan, ScopeConsumer)
	if !result.Verified {
		t.Error("the publication was not confirmed")
	}
	d.requireRemote(map[string]string{testBranch: h.forward, tagPrefix + "v0.36.1": h.tagBase})
}
