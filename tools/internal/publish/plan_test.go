package publish

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/gitcli"
)

// history is the small graph the planning tests reason about: a linear chain of
// three commits, a fourth that descends from the first but from neither of the
// others, and annotated tags.
type history struct {
	base   string
	middle string
	// forward descends from middle, so a publication planned from base can be
	// overtaken by a writer that reaches middle first.
	forward   string
	divergent string
	tagBase   string
	tagOther  string
}

// buildHistory creates the graph and keeps every commit referenced, so nothing
// under test depends on an unreferenced object surviving.
func (d *destination) buildHistory() history {
	d.tb.Helper()
	base := d.commit("base.txt", "base", "base")
	middle := d.commit("middle.txt", "middle", "middle")
	forward := d.commit("forward.txt", "forward", "forward")
	d.rewind(base)
	divergent := d.commit("divergent.txt", "divergent", "divergent")
	return history{
		base:      base,
		middle:    middle,
		forward:   forward,
		divergent: divergent,
		tagBase:   d.tag("v0.36.1", base, "release v0.36.1"),
		tagOther:  d.tag("v0.36.2", forward, "release v0.36.2"),
	}
}

func TestPlanClassifiesUpdates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// build arranges the remote and returns the updates to plan.
		build func(d *destination, h history) []Update
		// want is the effect expected for each destination ref.
		want map[string]Effect
		// wantErr is the sentinel a refusal must carry.
		wantErr error
		// wantMessage is a fragment a refusal without a sentinel must state.
		wantMessage string
	}{
		{
			name: "absent branch is created",
			build: func(d *destination, h history) []Update {
				return []Update{branchUpdate(testBranch, h.base)}
			},
			want: map[string]Effect{testBranch: EffectCreate},
		},
		{
			name: "identical branch is a no-op",
			build: func(d *destination, h history) []Update {
				d.seed(h.base + ":" + testBranch)
				return []Update{branchUpdate(testBranch, h.base)}
			},
			want: map[string]Effect{testBranch: EffectNoOp},
		},
		{
			name: "descendant branch fast forwards",
			build: func(d *destination, h history) []Update {
				d.seed(h.base + ":" + testBranch)
				return []Update{branchUpdate(testBranch, h.forward)}
			},
			want: map[string]Effect{testBranch: EffectFastForward},
		},
		{
			name: "divergent branch is refused",
			build: func(d *destination, h history) []Update {
				d.seed(h.forward + ":" + testBranch)
				return []Update{branchUpdate(testBranch, h.divergent)}
			},
			wantErr: ErrNonFastForward,
		},
		{
			name: "rewinding a branch to its own ancestor is refused",
			build: func(d *destination, h history) []Update {
				d.seed(h.forward + ":" + testBranch)
				return []Update{branchUpdate(testBranch, h.base)}
			},
			wantErr: ErrNonFastForward,
		},
		{
			name: "absent tag is created",
			build: func(d *destination, h history) []Update {
				return []Update{tagUpdate(tagPrefix+"v0.36.1", h.tagBase)}
			},
			want: map[string]Effect{tagPrefix + "v0.36.1": EffectCreate},
		},
		{
			name: "identical tag is a no-op",
			build: func(d *destination, h history) []Update {
				d.seed(h.tagBase + ":" + tagPrefix + "v0.36.1")
				return []Update{tagUpdate(tagPrefix+"v0.36.1", h.tagBase)}
			},
			want: map[string]Effect{tagPrefix + "v0.36.1": EffectNoOp},
		},
		{
			name: "tag holding a different object is refused",
			build: func(d *destination, h history) []Update {
				d.seed(h.tagBase + ":" + tagPrefix + "v0.36.1")
				return []Update{tagUpdate(tagPrefix+"v0.36.1", h.tagOther)}
			},
			wantErr: ErrTagMoved,
		},
		{
			name: "a tag never fast forwards to a descendant",
			build: func(d *destination, h history) []Update {
				// The tags point at base and at its own descendant, so this is
				// the update a branch would accept. A version means one tree.
				d.seed(h.tagBase + ":" + tagPrefix + "v0.36.1")
				return []Update{tagUpdate(tagPrefix+"v0.36.1", h.tagOther)}
			},
			wantErr: ErrTagMoved,
		},
		{
			name: "progress refs fast forward",
			build: func(d *destination, h history) []Update {
				d.seed(h.base + ":" + testProgressRef)
				return []Update{{Ref: testProgressRef, Kind: KindProgress, NewObject: h.forward, Evidence: "backfill:chunk-2"}}
			},
			want: map[string]Effect{testProgressRef: EffectFastForward},
		},
		{
			name: "the state ref is created",
			build: func(d *destination, h history) []Update {
				return []Update{{Ref: testStateRef, Kind: KindState, NewObject: h.base, Evidence: "state:cursor"}}
			},
			want: map[string]Effect{testStateRef: EffectCreate},
		},
		{
			name: "a progress ref that rewinds is refused",
			build: func(d *destination, h history) []Update {
				d.seed(h.forward + ":" + testProgressRef)
				return []Update{{Ref: testProgressRef, Kind: KindProgress, NewObject: h.divergent, Evidence: "backfill:chunk-2"}}
			},
			wantErr: ErrNonFastForward,
		},
		{
			name: "one ref cannot be updated twice",
			build: func(d *destination, h history) []Update {
				return []Update{branchUpdate(testBranch, h.base), branchUpdate(testBranch, h.forward)}
			},
			wantErr: ErrDuplicateRef,
		},
		{
			name: "a ref nested under another ref is refused",
			build: func(d *destination, h history) []Update {
				return []Update{branchUpdate(testBranch, h.base), branchUpdate(testBranch+"/next", h.forward)}
			},
			wantErr: ErrConflictingRefs,
		},
		{
			name: "a force marker is refused",
			build: func(d *destination, h history) []Update {
				return []Update{branchUpdate("+"+testBranch, h.base)}
			},
			wantErr: ErrForceUpdate,
		},
		{
			name: "the null object name is refused",
			build: func(d *destination, h history) []Update {
				return []Update{branchUpdate(testBranch, strings.Repeat("0", len(h.base)))}
			},
			wantErr: ErrDeleteUpdate,
		},
		{
			name: "an empty object name is refused",
			build: func(d *destination, h history) []Update {
				return []Update{branchUpdate(testBranch, "")}
			},
			wantErr: ErrDeleteUpdate,
		},
		{
			name: "an abbreviated object name is refused",
			build: func(d *destination, h history) []Update {
				return []Update{branchUpdate(testBranch, h.base[:12])}
			},
			wantMessage: "must be a full",
		},
		{
			name: "a revision expression is refused",
			build: func(d *destination, h history) []Update {
				return []Update{branchUpdate(testBranch, "HEAD")}
			},
			wantMessage: "must be a full",
		},
		{
			name: "an absent object is refused",
			build: func(d *destination, h history) []Update {
				return []Update{branchUpdate(testBranch, strings.Repeat("1", len(h.base)))}
			},
			wantErr: ErrObjectMissing,
		},
		{
			name: "a tag must point at an annotated tag object",
			build: func(d *destination, h history) []Update {
				return []Update{tagUpdate(tagPrefix+"v0.36.1", h.base)}
			},
			wantErr: ErrObjectType,
		},
		{
			name: "a branch must point at a commit",
			build: func(d *destination, h history) []Update {
				return []Update{branchUpdate(testBranch, h.tagBase)}
			},
			wantErr: ErrObjectType,
		},
		{
			name: "a contradicted absence expectation is refused",
			build: func(d *destination, h history) []Update {
				d.seed(h.base + ":" + testBranch)
				update := branchUpdate(testBranch, h.forward)
				update.ExpectAbsent = true
				return []Update{update}
			},
			wantErr: ErrExpectation,
		},
		{
			name: "a stale observation is refused",
			build: func(d *destination, h history) []Update {
				d.seed(h.base + ":" + testBranch)
				update := branchUpdate(testBranch, h.forward)
				update.ExpectedOld = h.divergent
				return []Update{update}
			},
			wantErr: ErrExpectation,
		},
		{
			name: "an observation of a ref that does not exist is refused",
			build: func(d *destination, h history) []Update {
				update := branchUpdate(testBranch, h.forward)
				update.ExpectedOld = h.base
				return []Update{update}
			},
			wantErr: ErrExpectation,
		},
		{
			name: "a matching observation is accepted",
			build: func(d *destination, h history) []Update {
				d.seed(h.base + ":" + testBranch)
				update := branchUpdate(testBranch, h.forward)
				update.ExpectedOld = h.base
				return []Update{update}
			},
			want: map[string]Effect{testBranch: EffectFastForward},
		},
		{
			name: "an observation cannot be both absent and a value",
			build: func(d *destination, h history) []Update {
				update := branchUpdate(testBranch, h.forward)
				update.ExpectAbsent = true
				update.ExpectedOld = h.base
				return []Update{update}
			},
			wantMessage: "cannot both expect",
		},
		{
			name: "a branch outside refs/heads is refused",
			build: func(d *destination, h history) []Update {
				return []Update{branchUpdate("refs/other/main", h.base)}
			},
			wantMessage: "must live under refs/heads/",
		},
		{
			name: "a branch may not be the state ref",
			build: func(d *destination, h history) []Update {
				return []Update{branchUpdate(testStateRef, h.base)}
			},
			wantMessage: "is the state ref",
		},
		{
			name: "a tag outside refs/tags is refused",
			build: func(d *destination, h history) []Update {
				return []Update{tagUpdate("refs/heads/v0.36.1", h.tagBase)}
			},
			wantMessage: "must live under refs/tags/",
		},
		{
			name: "a progress ref outside the namespace is refused",
			build: func(d *destination, h history) []Update {
				return []Update{{Ref: "refs/soapbox/other/chunk", Kind: KindProgress, NewObject: h.base, Evidence: "backfill:chunk-1"}}
			},
			wantMessage: "must live under refs/soapbox/progress/",
		},
		{
			name: "a state update must name the configured state ref",
			build: func(d *destination, h history) []Update {
				return []Update{{Ref: "refs/heads/other-state", Kind: KindState, NewObject: h.base, Evidence: "state:cursor"}}
			},
			wantMessage: "must be \"refs/heads/soapbox-state\"",
		},
		{
			name: "an unknown kind is refused",
			build: func(d *destination, h history) []Update {
				return []Update{{Ref: testBranch, Kind: Kind("release"), NewObject: h.base, Evidence: "replay:master"}}
			},
			wantMessage: "unknown ref kind",
		},
		{
			name: "an update without evidence is refused",
			build: func(d *destination, h history) []Update {
				return []Update{{Ref: testBranch, Kind: KindBranch, NewObject: h.base}}
			},
			wantMessage: "evidence label is required",
		},
		{
			name: "evidence that is a path is refused",
			build: func(d *destination, h history) []Update {
				update := branchUpdate(testBranch, h.base)
				update.Evidence = "/tmp/run/replay.json"
				return []Update{update}
			},
			wantMessage: "must not be a path",
		},
		{
			name: "evidence that is a URL is refused",
			build: func(d *destination, h history) []Update {
				update := branchUpdate(testBranch, h.base)
				update.Evidence = "https://example.invalid/run"
				return []Update{update}
			},
			wantMessage: "must not be a URL",
		},
		{
			name: "a ref with a colon is refused",
			build: func(d *destination, h history) []Update {
				return []Update{branchUpdate("refs/heads/main:refs/heads/other", h.base)}
			},
			wantMessage: "must not contain",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			d := newDestination(ctx, t, "")
			updates := test.build(d, d.buildHistory())
			// Planning is a read, so the remote as the plan finds it is the
			// remote it must leave behind, whichever way the plan comes out.
			before := d.remoteRefs()
			defer d.requireRemote(before)

			plan, err := d.pub.Plan(ctx, updates)
			switch {
			case test.wantErr != nil || test.wantMessage != "":
				if err == nil {
					t.Fatalf("plan succeeded, want a refusal: %s", plan.Manifest.Text())
				}
				if test.wantErr != nil && !errors.Is(err, test.wantErr) {
					t.Fatalf("plan error = %v, want %v", err, test.wantErr)
				}
				if test.wantMessage != "" && !strings.Contains(err.Error(), test.wantMessage) {
					t.Fatalf("plan error = %v, want it to state %q", err, test.wantMessage)
				}
				return
			case err != nil:
				t.Fatalf("plan: %v", err)
			}

			got := make(map[string]Effect, len(plan.Manifest.Actions))
			for _, action := range plan.Manifest.Actions {
				got[action.Ref] = action.Effect
			}
			if len(got) != len(test.want) {
				t.Fatalf("plan produced %d actions, want %d: %s", len(got), len(test.want), plan.Manifest.Text())
			}
			for ref, effect := range test.want {
				if got[ref] != effect {
					t.Errorf("action for %s = %q, want %q", ref, string(got[ref]), string(effect))
				}
			}
		})
	}
}

func TestPlanSortsActionsAndRecordsBothEnds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newDestination(ctx, t, "")
	h := d.buildHistory()
	d.seed(h.base + ":" + testBranch)

	plan := d.planUpdates(
		tagUpdate(tagPrefix+"v0.36.2", h.tagOther),
		branchUpdate(testBranch, h.forward),
		Update{Ref: testStateRef, Kind: KindState, NewObject: h.base, Evidence: "state:cursor"},
		tagUpdate(tagPrefix+"v0.36.1", h.tagBase),
	)

	wantOrder := []string{testBranch, testStateRef, tagPrefix + "v0.36.1", tagPrefix + "v0.36.2"}
	var gotOrder []string
	for _, action := range plan.Manifest.Actions {
		gotOrder = append(gotOrder, action.Ref)
	}
	if len(gotOrder) != len(wantOrder) {
		t.Fatalf("plan produced %v, want %v", gotOrder, wantOrder)
	}
	for i, ref := range wantOrder {
		if gotOrder[i] != ref {
			t.Fatalf("action %d is %s, want %s (order %v)", i, gotOrder[i], ref, gotOrder)
		}
	}

	branch := plan.Manifest.Actions[0]
	switch {
	case branch.OldObject != h.base:
		t.Errorf("branch old object = %q, want %q", branch.OldObject, h.base)
	case branch.NewObject != h.forward:
		t.Errorf("branch new object = %q, want %q", branch.NewObject, h.forward)
	case !branch.Consumer:
		t.Error("the consumer branch is not marked as one")
	case branch.Evidence != "replay:master":
		t.Errorf("branch evidence = %q, want %q", branch.Evidence, "replay:master")
	}
	if state := plan.Manifest.Actions[1]; state.Consumer || state.OldObject != "" {
		t.Errorf("state action = %+v, want a non-consumer create", state)
	}
}

func TestPlanRefusesRemoteInAnotherObjectFormat(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	requireSHA256(ctx, t)

	// The local repository reads SHA-1 names and the destination advertises
	// SHA-256 ones. Every ref would look absent, so a plan built on the
	// comparison would try to create refs that already exist.
	local := newDestination(ctx, t, gitcli.ObjectFormatSHA1)
	other := newDestination(ctx, t, gitcli.ObjectFormatSHA256)
	h := other.buildHistory()
	other.seed(h.base + ":" + testBranch)

	pub := local.publisher(Options{Remote: other.remote, AllowLocalRemote: true})
	local.buildHistory()
	_, err := pub.Plan(ctx, []Update{branchUpdate(testBranch, local.commit("late.txt", "late", "late"))})
	if err == nil {
		t.Fatal("plan succeeded against a remote in another object format")
	}
	if !strings.Contains(err.Error(), "must be a full sha1 object name") {
		t.Fatalf("plan error = %v, want it to name the object format mismatch", err)
	}
}

func TestPlanRequiresUpdates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newDestination(ctx, t, "")
	if _, err := d.pub.Plan(ctx, nil); err == nil {
		t.Fatal("planning nothing succeeded, want a refusal")
	}
}

// requireSHA256 skips a test when the Git under test cannot create SHA-256
// repositories, so the suite reports "not proved here" rather than a failure
// that says nothing about this package.
func requireSHA256(ctx context.Context, tb testing.TB) {
	tb.Helper()
	dir := tb.TempDir()
	git, err := gitcli.New(ctx, gitcli.Options{Dir: dir, Inherit: []string{"PATH"}, Isolation: []string{"HOME=" + tb.TempDir()}})
	if err != nil {
		tb.Fatalf("create git runner: %v", err)
	}
	if err := git.InitRepositoryWithFormat(ctx, "probe", gitcli.ObjectFormatSHA256); err != nil {
		tb.Skipf("git does not support sha256 repositories: %v", err)
	}
}
