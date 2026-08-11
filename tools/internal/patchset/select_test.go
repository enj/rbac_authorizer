package patchset_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/patchset"
)

func TestSelect(t *testing.T) {
	t.Parallel()

	series := []patchset.Patch{
		patch("patches/0001-always.patch", "", ""),
		patch("patches/0002-since-c.patch", "c", ""),
		patch("patches/0003-until-c.patch", "", "c"),
		patch("patches/0004-b-until-d.patch", "b", "d"),
		patch("patches/0005-master-only.patch", "", "", "master"),
		patch("patches/0006-release-only.patch", "", "", "release-1.36", "release-1.35"),
	}

	tests := []struct {
		name   string
		target patchset.Target
		want   []string
	}{
		{
			name:   "root commit selects only unbounded and until scoped patches",
			target: patchset.Target{Branch: "master", Commit: "a"},
			want: []string{
				"patches/0001-always.patch",
				"patches/0003-until-c.patch",
				"patches/0005-master-only.patch",
			},
		},
		{
			name:   "since boundary is inclusive",
			target: patchset.Target{Branch: "master", Commit: "c"},
			want: []string{
				"patches/0001-always.patch",
				"patches/0002-since-c.patch",
				"patches/0004-b-until-d.patch",
				"patches/0005-master-only.patch",
			},
		},
		{
			name:   "until boundary is exclusive",
			target: patchset.Target{Branch: "master", Commit: "d"},
			want: []string{
				"patches/0001-always.patch",
				"patches/0002-since-c.patch",
				"patches/0005-master-only.patch",
			},
		},
		{
			name:   "a side branch commit is not a descendant of the mainline since selector",
			target: patchset.Target{Branch: "release-1.36", Commit: "f"},
			want: []string{
				"patches/0001-always.patch",
				"patches/0003-until-c.patch",
				"patches/0004-b-until-d.patch",
				"patches/0006-release-only.patch",
			},
		},
		{
			name:   "an untracked branch selects only unscoped patches",
			target: patchset.Target{Branch: "release-1.30", Commit: "b"},
			want: []string{
				"patches/0001-always.patch",
				"patches/0003-until-c.patch",
				"patches/0004-b-until-d.patch",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := patchset.Select(t.Context(), newDAG(), series, test.target)
			if err != nil {
				t.Fatalf("select: %v", err)
			}
			if names := ids(got); !slices.Equal(names, test.want) {
				t.Errorf("selected %v, want %v", names, test.want)
			}
		})
	}
}

// TestSelectIsDeterministic asserts that repeated selection over one target
// produces the identical series and asks the identical ancestry questions in
// the identical order. Replay depends on this: the same source commit must
// choose the same patches in the same sequence on every run and every machine.
func TestSelectIsDeterministic(t *testing.T) {
	t.Parallel()

	series := []patchset.Patch{
		patch("patches/0001-a.patch", "b", "d"),
		patch("patches/0002-b.patch", "", "", "release-1.36"),
		patch("patches/0003-c.patch", "c", ""),
	}
	target := patchset.Target{Branch: "master", Commit: "c"}

	var first []string
	var firstQueries []string
	for range 5 {
		graph := newDAG()
		got, err := patchset.Select(t.Context(), graph, series, target)
		if err != nil {
			t.Fatalf("select: %v", err)
		}
		names := ids(got)
		if first == nil {
			first, firstQueries = names, graph.queries
			continue
		}
		if !slices.Equal(names, first) {
			t.Fatalf("selected %v, want %v on every run", names, first)
		}
		if !slices.Equal(graph.queries, firstQueries) {
			t.Fatalf("ancestry queries %v, want %v on every run", graph.queries, firstQueries)
		}
	}
	// The branch selector is evaluated first, so the release scoped patch costs
	// no ancestry query while master is the target.
	want := []string{"b->c", "d->c", "c->c"}
	if !slices.Equal(firstQueries, want) {
		t.Errorf("ancestry queries %v, want %v", firstQueries, want)
	}
}

// TestSelectReturnsCopies asserts that mutating a selected patch cannot reach
// the profile's series, so one ref transaction cannot corrupt the next.
func TestSelectReturnsCopies(t *testing.T) {
	t.Parallel()

	series := []patchset.Patch{patch("patches/0001-a.patch", "", "", "master")}
	got, err := patchset.Select(t.Context(), newDAG(), series, patchset.Target{Branch: "master", Commit: "a"})
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("selected %d patches, want 1", len(got))
	}
	got[0].Diff[0] = 'X'
	got[0].Branches[0] = "release-1.36"

	if series[0].Diff[0] == 'X' {
		t.Error("mutating a selected diff changed the configured series")
	}
	if series[0].Branches[0] != "master" {
		t.Error("mutating a selected branch list changed the configured series")
	}
}

func TestSelectRejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		patches []patchset.Patch
		target  patchset.Target
		want    error
	}{
		{
			name:    "no branch",
			patches: []patchset.Patch{patch("patches/0001-a.patch", "", "")},
			target:  patchset.Target{Commit: "a"},
			want:    patchset.ErrIncompleteTarget,
		},
		{
			name:    "no commit",
			patches: []patchset.Patch{patch("patches/0001-a.patch", "", "")},
			target:  patchset.Target{Branch: "master"},
			want:    patchset.ErrIncompleteTarget,
		},
		{
			name:    "no identifier",
			patches: []patchset.Patch{patch("", "", "")},
			target:  patchset.Target{Branch: "master", Commit: "a"},
			want:    patchset.ErrNoIdentifier,
		},
		{
			name:    "empty diff",
			patches: []patchset.Patch{{ID: "patches/0001-a.patch"}},
			target:  patchset.Target{Branch: "master", Commit: "a"},
			want:    patchset.ErrEmptyPatch,
		},
		{
			name: "duplicate identifier",
			patches: []patchset.Patch{
				patch("patches/0001-a.patch", "", ""),
				patch("patches/0001-a.patch", "", ""),
			},
			target: patchset.Target{Branch: "master", Commit: "a"},
			want:   patchset.ErrDuplicatePatch,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := patchset.Select(t.Context(), newDAG(), test.patches, test.target)
			if !errors.Is(err, test.want) {
				t.Errorf("select error %v, want %v", err, test.want)
			}
		})
	}
}

func TestSelectRejectsNilGit(t *testing.T) {
	t.Parallel()

	_, err := patchset.Select(t.Context(), nil, nil, patchset.Target{Branch: "master", Commit: "a"})
	if !errors.Is(err, patchset.ErrNoGit) {
		t.Errorf("select error %v, want %v", err, patchset.ErrNoGit)
	}
}

// TestSelectPropagatesAncestryFailure asserts that an unresolvable selector
// stops the run instead of silently dropping or keeping the patch. A selector
// naming a commit the source repository does not have is a profile bug, and
// guessing either way would publish a tree nobody described.
func TestSelectPropagatesAncestryFailure(t *testing.T) {
	t.Parallel()

	series := []patchset.Patch{patch("patches/0001-a.patch", "missing", "")}
	_, err := patchset.Select(t.Context(), newDAG(), series, patchset.Target{Branch: "master", Commit: "c"})
	if err == nil {
		t.Fatal("select accepted a selector naming an unknown commit")
	}
	if !strings.Contains(err.Error(), "patches/0001-a.patch") {
		t.Errorf("error %v does not name the failing patch", err)
	}
}

func TestSelectHonoursCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := patchset.Select(ctx, newDAG(), []patchset.Patch{patch("patches/0001-a.patch", "", "")}, patchset.Target{Branch: "master", Commit: "a"})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("select error %v, want %v", err, context.Canceled)
	}
}
