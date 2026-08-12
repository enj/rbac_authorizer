package gitcli_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/gitcli"
)

func TestCommitGraph(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)
	git := up.repo.Git

	commits, err := git.CommitGraph(ctx, gitcli.RevListOptions{Include: []string{"refs/heads/" + mainBranch}})
	if err != nil {
		t.Fatalf("commit graph: %v", err)
	}
	if len(commits) != 4 {
		t.Fatalf("walked %d commits, want 4", len(commits))
	}

	// The ordering contract is what replay depends on: a commit may never be
	// emitted before a parent that is also in the walk.
	position := make(map[string]int, len(commits))
	for i, commit := range commits {
		position[commit.SHA] = i
	}
	for i, commit := range commits {
		for _, parent := range commit.Parents {
			if at, ok := position[parent]; ok && at >= i {
				t.Fatalf("commit %s at %d precedes its parent %s at %d", commit.SHA, i, parent, at)
			}
		}
	}
	if commits[0].SHA != up.sha(base) {
		t.Fatalf("walk starts at %q, want the base commit", commits[0].SHA)
	}
	last := commits[len(commits)-1]
	if last.SHA != up.sha(mergeC) {
		t.Fatalf("walk ends at %q, want the merge commit", last.SHA)
	}
	// Parent order is preserved, so the first parent stays identifiable.
	if want := []string{up.sha(mainOne), up.sha(feature)}; !slices.Equal(last.Parents, want) {
		t.Fatalf("merge parents %v, want %v", last.Parents, want)
	}
	if len(commits[0].Parents) != 0 {
		t.Fatalf("root commit has parents %v", commits[0].Parents)
	}
}

func TestCommitGraphSelection(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)
	git := up.repo.Git

	tests := []struct {
		name string
		opts gitcli.RevListOptions
		want []string
	}{
		{
			name: "first parent follows the mainline",
			opts: gitcli.RevListOptions{Include: []string{"refs/heads/" + mainBranch}, FirstParent: true},
			want: []string{base, mainOne, mergeC},
		},
		{
			// Excluding the anchor also excludes everything below it, which is
			// how a walk is bounded without listing the whole history.
			name: "exclude bounds the walk",
			opts: gitcli.RevListOptions{
				Include: []string{"refs/heads/" + mainBranch},
				Exclude: []string{up.sha(base)},
			},
			want: []string{mainOne, feature, mergeC},
		},
		{
			name: "two heads",
			opts: gitcli.RevListOptions{
				Include: []string{"refs/heads/" + mainBranch, "refs/heads/" + releaseBranch},
				Exclude: []string{up.sha(base)},
			},
			want: []string{mainOne, feature, release, mergeC},
		},
		{
			name: "max count",
			opts: gitcli.RevListOptions{Include: []string{up.sha(base)}, MaxCount: 1},
			want: []string{base},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			commits, err := git.CommitGraph(ctx, test.opts)
			if err != nil {
				t.Fatalf("commit graph: %v", err)
			}
			got := make([]string, 0, len(commits))
			for _, commit := range commits {
				got = append(got, commit.SHA)
			}
			want := make([]string, 0, len(test.want))
			for _, label := range test.want {
				want = append(want, up.sha(label))
			}
			slices.Sort(got)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Fatalf("walked %v, want %v", got, want)
			}
		})
	}
}

func TestCommitGraphRejectsUnsafeRevisions(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)
	git := up.repo.Git

	tests := []struct {
		name string
		opts gitcli.RevListOptions
		want string
	}{
		{name: "no revision", opts: gitcli.RevListOptions{}, want: "at least one revision"},
		{
			name: "range",
			opts: gitcli.RevListOptions{Include: []string{"main..release-1.36"}},
			want: "not a range",
		},
		{
			// Negation belongs in the exclude list, where it is explicit.
			name: "negated revision",
			opts: gitcli.RevListOptions{Include: []string{"^main"}},
			want: "must not be negated",
		},
		{
			name: "flag like revision",
			opts: gitcli.RevListOptions{Include: []string{"--all"}},
			want: "must not start with a dash",
		},
		{
			name: "negative max count",
			opts: gitcli.RevListOptions{Include: []string{"HEAD"}, MaxCount: -1},
			want: "must not be negative",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := git.CommitGraph(ctx, test.opts)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error %q does not mention %q", err, test.want)
			}
		})
	}
}

func TestMergeBase(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)
	git := up.repo.Git

	got, err := git.MergeBase(ctx, "refs/heads/"+mainBranch, "refs/heads/"+releaseBranch)
	if err != nil {
		t.Fatalf("merge base: %v", err)
	}
	if got != up.sha(base) {
		t.Fatalf("merge base %q, want the base commit %q", got, up.sha(base))
	}

	octopus, err := git.MergeBasesOctopus(ctx,
		"refs/heads/"+mainBranch, "refs/heads/"+releaseBranch, "refs/tags/"+annotatedTag)
	if err != nil {
		t.Fatalf("octopus merge base: %v", err)
	}
	if len(octopus) != 1 || octopus[0] != up.sha(base) {
		t.Fatalf("octopus merge bases %v, want the single base commit %q", octopus, up.sha(base))
	}
	if _, err := git.MergeBasesOctopus(ctx, "refs/heads/"+mainBranch); err == nil {
		t.Fatal("expected an error for a single revision")
	}
}

// TestMergeBasesReportsCrissCrossAmbiguity proves the merge base query reports
// every best common ancestor rather than one of them.
//
// A criss-cross history has two, and git prints only one unless it is asked for
// all of them, with no sign that it chose. An anchor derived from that answer
// would depend on git's traversal order, so the ambiguity has to reach the
// caller.
func TestMergeBasesReportsCrissCrossAmbiguity(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)
	git := up.repo.Git

	// Two commits from a shared base, merged into each other in both orders.
	left, right := up.sha(mainOne), up.sha(feature)
	tree, err := git.ResolveTree(ctx, left)
	if err != nil {
		t.Fatalf("resolve tree: %v", err)
	}
	author := gitcli.Signature{Name: testUserName, Email: testUserEmail, Date: testRawDate}
	first, err := git.WriteCommit(ctx, gitcli.CommitTreeOptions{
		Tree: tree, Parents: []string{left, right}, Message: "Merge left first\n",
		Author: author, Committer: author,
	})
	if err != nil {
		t.Fatalf("write merge: %v", err)
	}
	second, err := git.WriteCommit(ctx, gitcli.CommitTreeOptions{
		Tree: tree, Parents: []string{right, left}, Message: "Merge right first\n",
		Author: author, Committer: author,
	})
	if err != nil {
		t.Fatalf("write merge: %v", err)
	}

	bases, err := git.MergeBases(ctx, first, second)
	if err != nil {
		t.Fatalf("merge bases: %v", err)
	}
	want := []string{left, right}
	slices.Sort(want)
	if !slices.Equal(bases, want) {
		t.Fatalf("merge bases = %v, want both %v", bases, want)
	}

	// The singular form must refuse rather than pick one of them.
	if _, err := git.MergeBase(ctx, first, second); !errors.Is(err, gitcli.ErrAmbiguousMergeBase) {
		t.Fatalf("merge base error = %v, want %v", err, gitcli.ErrAmbiguousMergeBase)
	}
	octopus, err := git.MergeBasesOctopus(ctx, first, second)
	if err != nil {
		t.Fatalf("octopus merge bases: %v", err)
	}
	if !slices.Equal(octopus, want) {
		t.Fatalf("octopus merge bases = %v, want both %v", octopus, want)
	}
}

// TestMergeBaseWithoutCommonAncestor covers the verdict git reports with exit
// status 1, which must not be mistaken for a command failure.
func TestMergeBaseWithoutCommonAncestor(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)
	git := up.repo.Git

	tree, err := git.ResolveTree(ctx, up.sha(base))
	if err != nil {
		t.Fatalf("resolve tree: %v", err)
	}
	orphan, err := git.WriteCommit(ctx, gitcli.CommitTreeOptions{
		Tree:      tree,
		Message:   "unrelated history\n",
		Author:    gitcli.Signature{Name: testUserName, Email: testUserEmail, Date: testRawDate},
		Committer: gitcli.Signature{Name: testUserName, Email: testUserEmail, Date: testRawDate},
	})
	if err != nil {
		t.Fatalf("write orphan commit: %v", err)
	}

	_, err = git.MergeBase(ctx, orphan, "refs/heads/"+mainBranch)
	if !errors.Is(err, gitcli.ErrNoMergeBase) {
		t.Fatalf("error %v is not a missing merge base", err)
	}
}

func TestIsAncestor(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)
	git := up.repo.Git

	tests := []struct {
		ancestor   string
		descendant string
		want       bool
	}{
		{ancestor: base, descendant: mergeC, want: true},
		{ancestor: feature, descendant: mergeC, want: true},
		{ancestor: base, descendant: release, want: true},
		{ancestor: release, descendant: mergeC, want: false},
		{ancestor: mergeC, descendant: base, want: false},
		{ancestor: mergeC, descendant: mergeC, want: true},
	}

	for _, test := range tests {
		t.Run(test.ancestor+"/"+test.descendant, func(t *testing.T) {
			got, err := git.IsAncestor(ctx, up.sha(test.ancestor), up.sha(test.descendant))
			if err != nil {
				t.Fatalf("ancestor probe: %v", err)
			}
			if got != test.want {
				t.Fatalf("IsAncestor = %v, want %v", got, test.want)
			}
		})
	}
}

func TestChangedPaths(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)
	git := up.repo.Git

	tests := []struct {
		name string
		from string
		to   string
		want []string
	}{
		{
			// An empty from compares against the empty tree, which is how a root
			// commit reports everything it introduced.
			name: "root commit",
			from: "",
			to:   base,
			want: []string{"README.md"},
		},
		{
			name: "ordinary commit",
			from: base,
			to:   mainOne,
			want: []string{"plugin/pkg/auth/authorizer/rbac/rbac.go"},
		},
		{
			// The merge took nothing from the side branch, so against its first
			// parent it changed nothing at all and needs no materialization.
			name: "merge against its first parent",
			from: mainOne,
			to:   mergeC,
			want: nil,
		},
		{
			name: "merge against its second parent",
			from: feature,
			to:   mergeC,
			want: []string{
				"pkg/registry/rbac/validation/rule.go",
				"plugin/pkg/auth/authorizer/rbac/rbac.go",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			from := ""
			if test.from != "" {
				from = up.sha(test.from)
			}
			got, err := git.ChangedPaths(ctx, from, up.sha(test.to))
			if err != nil {
				t.Fatalf("changed paths: %v", err)
			}
			if !slices.Equal(got, test.want) {
				t.Fatalf("changed paths %v, want %v", got, test.want)
			}
		})
	}
}

func TestCommitParentsAndTree(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)
	git := up.repo.Git

	parents, err := git.CommitParents(ctx, up.sha(mergeC))
	if err != nil {
		t.Fatalf("commit parents: %v", err)
	}
	if want := []string{up.sha(mainOne), up.sha(feature)}; !slices.Equal(parents, want) {
		t.Fatalf("parents %v, want %v", parents, want)
	}

	rootParents, err := git.CommitParents(ctx, up.sha(base))
	if err != nil {
		t.Fatalf("root parents: %v", err)
	}
	if len(rootParents) != 0 {
		t.Fatalf("root commit reports parents %v", rootParents)
	}

	// The merge carries its first parent's tree, which is exactly the cheap
	// check that lets replay skip it.
	mergeTree, err := git.ResolveTree(ctx, up.sha(mergeC))
	if err != nil {
		t.Fatalf("resolve merge tree: %v", err)
	}
	mainTree, err := git.ResolveTree(ctx, up.sha(mainOne))
	if err != nil {
		t.Fatalf("resolve mainline tree: %v", err)
	}
	if mergeTree != mainTree {
		t.Fatalf("merge tree %q differs from its first parent's %q", mergeTree, mainTree)
	}
}

func TestObjectInfoBatch(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)
	git := up.repo.Git

	const absent = "0123456789012345678901234567890123456789"
	infos, err := git.ObjectInfoBatch(ctx, gitcli.ObjectInfoOptions{
		Revisions: []string{up.sha(base), up.sha(base) + "^{tree}", absent},
	})
	if err != nil {
		t.Fatalf("object info: %v", err)
	}
	if len(infos) != 3 {
		t.Fatalf("got %d records, want 3", len(infos))
	}
	if infos[0].Type != "commit" || infos[0].Missing || infos[0].Size == 0 {
		t.Fatalf("commit record is %+v", infos[0])
	}
	if infos[1].Type != "tree" {
		t.Fatalf("tree record is %+v", infos[1])
	}
	// A missing object is a verdict, not a failure, so a caller can ask what it
	// already holds without handling an error for the expected answer.
	if !infos[2].Missing || infos[2].Type != "" {
		t.Fatalf("missing record is %+v", infos[2])
	}

	empty, err := git.ObjectInfoBatch(ctx, gitcli.ObjectInfoOptions{})
	if err != nil || empty != nil {
		t.Fatalf("empty batch returned %v, %v", empty, err)
	}
	if _, err := git.ObjectInfoBatch(ctx, gitcli.ObjectInfoOptions{Revisions: []string{"--all"}}); err == nil {
		t.Fatal("expected an error for a flag like revision")
	}
}

// TestGraphOperationsHonourCancellation proves a long walk stops when the run is
// cancelled rather than continuing to completion.
func TestGraphOperationsHonourCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	up := newUpstream(ctx, t)
	git := up.repo.Git
	cancel()

	if _, err := git.CommitGraph(ctx, gitcli.RevListOptions{Include: []string{"HEAD"}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("error %v is not a cancellation", err)
	}
	if _, err := git.ChangedPaths(ctx, "", "HEAD"); !errors.Is(err, context.Canceled) {
		t.Fatalf("error %v is not a cancellation", err)
	}
	if _, err := git.ObjectInfoBatch(ctx, gitcli.ObjectInfoOptions{Revisions: []string{"HEAD"}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("error %v is not a cancellation", err)
	}
}
