package gitgraph_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/gitgraph"
)

// sha turns a readable fixture label into a deterministic object name, so a
// topology can be written as "M B C" and still be asserted against real forty
// character names.
func sha(label string) string {
	sum := sha256.Sum256([]byte(label))
	return hex.EncodeToString(sum[:20])
}

// shas maps a whole label list into object names.
func shas(labels ...string) []string {
	names := make([]string, 0, len(labels))
	for _, label := range labels {
		names = append(names, sha(label))
	}
	return names
}

// commits parses a compact topology. Every entry is "child parent...", listed
// parents first, which is the order git rev-list --topo-order --reverse emits.
func commits(spec ...string) []gitgraph.Commit {
	parsed := make([]gitgraph.Commit, 0, len(spec))
	for _, line := range spec {
		fields := strings.Fields(line)
		commit := gitgraph.Commit{SHA: sha(fields[0])}
		for _, parent := range fields[1:] {
			commit.Parents = append(commit.Parents, sha(parent))
		}
		parsed = append(parsed, commit)
	}
	return parsed
}

// build parses a topology and fails the test when it is not a valid graph.
func build(tb testing.TB, spec ...string) *gitgraph.Graph {
	tb.Helper()
	graph, err := gitgraph.New(commits(spec...))
	if err != nil {
		tb.Fatalf("build graph: %v", err)
	}
	return graph
}

// labels renders object names back into fixture labels so a failure message
// names the commit a reader recognises.
func labels(known []string, names []string) []string {
	lookup := make(map[string]string, len(known))
	for _, label := range known {
		lookup[sha(label)] = label
	}
	out := make([]string, 0, len(names))
	for _, name := range names {
		if label, ok := lookup[name]; ok {
			out = append(out, label)
			continue
		}
		out = append(out, name)
	}
	return out
}

// linear is A <- B <- C.
var linear = []string{"A", "B A", "C B"}

// merged is a feature branch merged back into the mainline:
//
//	A <- B <- M
//	 \        /
//	  F1 <- F2
var merged = []string{"A", "B A", "F1 A", "F2 F1", "M B F2"}

// diverged is two release branches off a common anchor.
var diverged = []string{"A", "B A", "R1 B", "R2 B", "R1b R1", "R2b R2"}

func TestNewRejectsMalformedGraphs(t *testing.T) {
	valid := sha("A")

	tests := []struct {
		name    string
		commits []gitgraph.Commit
		want    string
	}{
		{
			name:    "empty object name",
			commits: []gitgraph.Commit{{SHA: ""}},
			want:    "40 or 64 hexadecimal",
		},
		{
			name:    "abbreviated object name",
			commits: []gitgraph.Commit{{SHA: valid[:12]}},
			want:    "40 or 64 hexadecimal",
		},
		{
			name:    "upper case object name",
			commits: []gitgraph.Commit{{SHA: strings.ToUpper(valid)}},
			want:    "lower case hexadecimal",
		},
		{
			name:    "duplicate commit",
			commits: []gitgraph.Commit{{SHA: valid}, {SHA: valid}},
			want:    "appears more than once",
		},
		{
			name:    "self parent",
			commits: []gitgraph.Commit{{SHA: valid, Parents: []string{valid}}},
			want:    "is its own parent",
		},
		{
			name:    "malformed parent",
			commits: []gitgraph.Commit{{SHA: valid, Parents: []string{"nope"}}},
			want:    "40 or 64 hexadecimal",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := gitgraph.New(test.commits)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error %q does not mention %q", err, test.want)
			}
		})
	}
}

func TestNewRejectsCycle(t *testing.T) {
	_, err := gitgraph.New(commits("A C", "B A", "C B"))
	if !errors.Is(err, gitgraph.ErrGraphCycle) {
		t.Fatalf("error %v is not a cycle", err)
	}
}

func TestTopologicalOrder(t *testing.T) {
	tests := []struct {
		name string
		spec []string
		want []string
	}{
		{name: "linear", spec: linear, want: []string{"A", "B", "C"}},
		{name: "merge", spec: merged, want: []string{"A", "B", "F1", "F2", "M"}},
		{
			name: "octopus",
			spec: []string{"A", "S1 A", "S2 A", "S3 A", "O S1 S2 S3"},
			want: []string{"A", "S1", "S2", "S3", "O"},
		},
		{name: "divergent releases", spec: diverged, want: []string{"A", "B", "R1", "R2", "R1b", "R2b"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			graph := build(t, test.spec...)
			got := labels(test.want, graph.TopologicalOrder())
			if !slices.Equal(got, test.want) {
				t.Fatalf("order %v, want %v", got, test.want)
			}
		})
	}
}

// TestTopologicalOrderIsDeterministic rebuilds the same graph repeatedly. The
// graph is built over maps, so a traversal that leaked map iteration order would
// eventually disagree with itself and produce history that is not reproducible.
func TestTopologicalOrderIsDeterministic(t *testing.T) {
	want := build(t, merged...).TopologicalOrder()
	for range 64 {
		got := build(t, merged...).TopologicalOrder()
		if !slices.Equal(got, want) {
			t.Fatalf("order %v, want %v", labels([]string{"A", "B", "F1", "F2", "M"}, got), labels([]string{"A", "B", "F1", "F2", "M"}, want))
		}
	}
}

// TestBoundaryParentsAreRetained covers a graph bounded below by an anchor: the
// anchor's own parents are not nodes, and dropping them would turn a merge into
// an ordinary commit.
func TestBoundaryParentsAreRetained(t *testing.T) {
	graph, err := gitgraph.New([]gitgraph.Commit{
		{SHA: sha("anchor"), Parents: shas("older", "sideline")},
		{SHA: sha("B"), Parents: shas("anchor")},
	})
	if err != nil {
		t.Fatalf("build graph: %v", err)
	}

	parents, err := graph.Parents(sha("anchor"))
	if err != nil {
		t.Fatalf("parents: %v", err)
	}
	if want := shas("older", "sideline"); !slices.Equal(parents, want) {
		t.Fatalf("parents %v, want %v", parents, want)
	}
	if roots := graph.Roots(); !slices.Equal(roots, shas("anchor")) {
		t.Fatalf("roots %v, want the anchor", labels([]string{"anchor", "B"}, roots))
	}
	if order := graph.TopologicalOrder(); !slices.Equal(order, shas("anchor", "B")) {
		t.Fatalf("order %v, want anchor then B", labels([]string{"anchor", "B"}, order))
	}
}

func TestFirstParentLine(t *testing.T) {
	tests := []struct {
		name string
		spec []string
		head string
		want []string
	}{
		{name: "linear", spec: linear, head: "C", want: []string{"C", "B", "A"}},
		{
			name: "merge follows the mainline only",
			spec: merged,
			head: "M",
			want: []string{"M", "B", "A"},
		},
		{
			name: "stops at the graph boundary",
			spec: []string{"B A", "C B"},
			head: "C",
			want: []string{"C", "B"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			graph := build(t, test.spec...)
			line, err := graph.FirstParentLine(sha(test.head))
			if err != nil {
				t.Fatalf("first parent line: %v", err)
			}
			if got := labels(test.want, line); !slices.Equal(got, test.want) {
				t.Fatalf("line %v, want %v", got, test.want)
			}
		})
	}
}

func TestIsAncestor(t *testing.T) {
	graph := build(t, merged...)

	tests := []struct {
		ancestor   string
		descendant string
		want       bool
	}{
		{ancestor: "A", descendant: "M", want: true},
		{ancestor: "F1", descendant: "M", want: true},
		{ancestor: "M", descendant: "A", want: false},
		{ancestor: "B", descendant: "F2", want: false},
		{ancestor: "M", descendant: "M", want: true},
	}
	for _, test := range tests {
		t.Run(test.ancestor+" of "+test.descendant, func(t *testing.T) {
			got, err := graph.IsAncestor(sha(test.ancestor), sha(test.descendant))
			if err != nil {
				t.Fatalf("is ancestor: %v", err)
			}
			if got != test.want {
				t.Fatalf("is ancestor = %v, want %v", got, test.want)
			}
		})
	}

	if _, err := graph.IsAncestor(sha("absent"), sha("M")); !errors.Is(err, gitgraph.ErrUnknownCommit) {
		t.Fatalf("error %v is not an unknown commit", err)
	}
}

func TestMergeBases(t *testing.T) {
	tests := []struct {
		name string
		spec []string
		a    string
		b    string
		want []string
	}{
		{
			// Only the newest common ancestor is a merge base; A is also common
			// but is already contained in B.
			name: "newest common ancestor only",
			spec: diverged,
			a:    "R1b",
			b:    "R2b",
			want: []string{"B"},
		},
		{name: "one side contains the other", spec: linear, a: "A", b: "C", want: []string{"A"}},
		{name: "same commit", spec: linear, a: "C", b: "C", want: []string{"C"}},
		{
			// A criss-cross merge genuinely has two equally good bases, and
			// reporting one of them would hide the ambiguity.
			name: "criss cross merge",
			spec: []string{"A", "P A", "Q A", "M1 P Q", "M2 Q P"},
			a:    "M1",
			b:    "M2",
			want: []string{"P", "Q"},
		},
		{
			name: "disjoint histories",
			spec: []string{"A", "B A", "X", "Y X"},
			a:    "B",
			b:    "Y",
			want: nil,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			graph := build(t, test.spec...)
			got, err := graph.MergeBases(sha(test.a), sha(test.b))
			if err != nil {
				t.Fatalf("merge bases: %v", err)
			}
			// The result is sorted by object name, so the expectation is
			// compared as a set of labels.
			want := shas(test.want...)
			slices.Sort(want)
			if len(got) == 0 {
				got = nil
			}
			if len(want) == 0 {
				want = nil
			}
			if !slices.Equal(got, want) {
				t.Fatalf("merge bases %v, want %v", labels(test.want, got), test.want)
			}
		})
	}

	graph := build(t, linear...)
	if _, err := graph.MergeBases(sha("absent"), sha("A")); !errors.Is(err, gitgraph.ErrUnknownCommit) {
		t.Fatalf("error %v is not an unknown commit", err)
	}
}

func TestCommonAnchor(t *testing.T) {
	tests := []struct {
		name  string
		spec  []string
		heads []string
		want  string
	}{
		{name: "divergent releases", spec: diverged, heads: []string{"R1b", "R2b"}, want: "B"},
		{name: "merge", spec: merged, heads: []string{"M", "F2"}, want: "F2"},
		{name: "single head", spec: linear, heads: []string{"C"}, want: "C"},
		{
			name:  "three heads",
			spec:  []string{"A", "B A", "R1 B", "R2 B", "R3 A"},
			heads: []string{"R1", "R2", "R3"},
			want:  "A",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			graph := build(t, test.spec...)
			anchor, err := graph.CommonAnchor(shas(test.heads...))
			if err != nil {
				t.Fatalf("common anchor: %v", err)
			}
			if anchor != sha(test.want) {
				t.Fatalf("anchor %v, want %v", labels([]string{test.want}, []string{anchor}), test.want)
			}
		})
	}
}

func TestCommonAnchorFailures(t *testing.T) {
	tests := []struct {
		name  string
		spec  []string
		heads []string
		want  error
	}{
		{
			name:  "disjoint histories",
			spec:  []string{"A", "B A", "X", "Y X"},
			heads: []string{"B", "Y"},
			want:  gitgraph.ErrNoCommonAnchor,
		},
		{
			// A criss-cross merge leaves two equally good bases, and choosing
			// one would make published history depend on traversal order.
			name:  "criss cross merge",
			spec:  []string{"A", "P A", "Q A", "M1 P Q", "M2 Q P"},
			heads: []string{"M1", "M2"},
			want:  gitgraph.ErrAmbiguousAnchor,
		},
		{
			name:  "unknown head",
			spec:  linear,
			heads: []string{"absent"},
			want:  gitgraph.ErrUnknownCommit,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			graph := build(t, test.spec...)
			_, err := graph.CommonAnchor(shas(test.heads...))
			if !errors.Is(err, test.want) {
				t.Fatalf("error %v, want %v", err, test.want)
			}
		})
	}
}

func TestValidateAnchor(t *testing.T) {
	graph := build(t, diverged...)

	if err := graph.ValidateAnchor(sha("B"), shas("R1b", "R2b")); err != nil {
		t.Fatalf("validate anchor: %v", err)
	}
	// A branch that does not descend from the recorded anchor would replay into
	// history that shares no root with what was already published.
	err := graph.ValidateAnchor(sha("R1"), shas("R2b"))
	if err == nil {
		t.Fatal("expected an error for a branch off the anchor")
	}
	if !strings.Contains(err.Error(), "does not descend from anchor") {
		t.Fatalf("error %q does not explain the failure", err)
	}
}

func TestRange(t *testing.T) {
	tests := []struct {
		name   string
		spec   []string
		anchor string
		heads  []string
		want   []string
	}{
		{
			name:   "excludes history before the anchor",
			spec:   diverged,
			anchor: "B",
			heads:  []string{"R1b", "R2b"},
			want:   []string{"B", "R1", "R2", "R1b", "R2b"},
		},
		{
			name:   "one head only",
			spec:   diverged,
			anchor: "B",
			heads:  []string{"R1b"},
			want:   []string{"B", "R1", "R1b"},
		},
		{
			name:   "includes a merged side branch",
			spec:   merged,
			anchor: "A",
			heads:  []string{"M"},
			want:   []string{"A", "B", "F1", "F2", "M"},
		},
		{
			name:   "anchor equal to head",
			spec:   linear,
			anchor: "C",
			heads:  []string{"C"},
			want:   []string{"C"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			graph := build(t, test.spec...)
			got, err := graph.Range(sha(test.anchor), shas(test.heads...))
			if err != nil {
				t.Fatalf("range: %v", err)
			}
			if rendered := labels(test.want, got); !slices.Equal(rendered, test.want) {
				t.Fatalf("range %v, want %v", rendered, test.want)
			}
		})
	}
}

func TestDedupeParents(t *testing.T) {
	graph := build(t, linear...)

	tests := []struct {
		name    string
		parents []string
		want    []string
	}{
		{name: "distinct", parents: []string{"C"}, want: []string{"C"}},
		{name: "exact duplicate", parents: []string{"B", "B"}, want: []string{"B"}},
		{
			// C already contains B, so listing both would create a merge with
			// nothing to merge.
			name:    "ancestor of a sibling",
			parents: []string{"C", "B"},
			want:    []string{"C"},
		},
		{name: "ancestor listed first", parents: []string{"A", "C"}, want: []string{"C"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := graph.DedupeParents(shas(test.parents...))
			if err != nil {
				t.Fatalf("dedupe parents: %v", err)
			}
			if rendered := labels(test.want, got); !slices.Equal(rendered, test.want) {
				t.Fatalf("parents %v, want %v", rendered, test.want)
			}
		})
	}

	if _, err := graph.DedupeParents(shas("absent")); !errors.Is(err, gitgraph.ErrUnknownCommit) {
		t.Fatalf("error %v is not an unknown commit", err)
	}
}

// mapping builds a source to destination mapping from label pairs.
func mapping(tb testing.TB, pairs ...[2]string) *gitgraph.Mapping {
	tb.Helper()
	m := gitgraph.NewMapping()
	for _, pair := range pairs {
		if err := m.Set(sha(pair[0]), sha(pair[1])); err != nil {
			tb.Fatalf("set mapping: %v", err)
		}
	}
	return m
}

func TestNearestMappedAncestor(t *testing.T) {
	// B and F2 changed nothing the extraction keeps, so they were never
	// materialized and their descendants must attach to what was.
	graph := build(t, merged...)
	m := mapping(t, [2]string{"A", "dA"}, [2]string{"F1", "dF1"})

	tests := []struct {
		name  string
		from  string
		want  string
		found bool
	}{
		{name: "mapped commit maps to itself", from: "A", want: "dA", found: true},
		{name: "skipped commit falls back to its parent", from: "B", want: "dA", found: true},
		{name: "skipped commit on a side branch", from: "F2", want: "dF1", found: true},
		{name: "merge prefers the first parent line", from: "M", want: "dA", found: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, found, err := graph.NearestMappedAncestor(sha(test.from), m)
			if err != nil {
				t.Fatalf("nearest mapped ancestor: %v", err)
			}
			if found != test.found {
				t.Fatalf("found = %v, want %v", found, test.found)
			}
			if got != sha(test.want) {
				t.Fatalf("ancestor %v, want %v", labels([]string{test.want}, []string{got}), test.want)
			}
		})
	}

	empty := gitgraph.NewMapping()
	if _, found, err := graph.NearestMappedAncestor(sha("M"), empty); err != nil || found {
		t.Fatalf("found = %v, err = %v, want no mapped ancestor", found, err)
	}
	if _, _, err := graph.NearestMappedAncestor(sha("M"), nil); err == nil {
		t.Fatal("expected an error for a nil mapping")
	}
}

func TestMappedParents(t *testing.T) {
	source := build(t, merged...)
	destination := build(t, "dA", "dB dA", "dF2 dA")

	tests := []struct {
		name        string
		commit      string
		pairs       [][2]string
		destination *gitgraph.Graph
		want        []string
	}{
		{
			name:        "merge keeps two distinct parents",
			commit:      "M",
			pairs:       [][2]string{{"A", "dA"}, {"B", "dB"}, {"F2", "dF2"}},
			destination: destination,
			want:        []string{"dB", "dF2"},
		},
		{
			// Both source parents collapse onto the same destination commit, so
			// the replayed commit must not be a merge at all.
			name:        "merge collapses to one parent",
			commit:      "M",
			pairs:       [][2]string{{"A", "dA"}},
			destination: destination,
			want:        []string{"dA"},
		},
		{
			// dB already contains dA, so listing both would be a degenerate
			// merge; only the destination graph knows that.
			name:        "parent already contained in a sibling",
			commit:      "M",
			pairs:       [][2]string{{"B", "dB"}, {"F2", "dA"}},
			destination: destination,
			want:        []string{"dB"},
		},
		{
			name:        "without a destination graph only exact duplicates go",
			commit:      "M",
			pairs:       [][2]string{{"B", "dB"}, {"F2", "dA"}},
			destination: nil,
			want:        []string{"dB", "dA"},
		},
		{
			name:        "root commit has no mapped parent",
			commit:      "A",
			pairs:       [][2]string{{"A", "dA"}},
			destination: destination,
			want:        nil,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := source.MappedParents(sha(test.commit), mapping(t, test.pairs...), test.destination)
			if err != nil {
				t.Fatalf("mapped parents: %v", err)
			}
			rendered := labels([]string{"dA", "dB", "dF2"}, got)
			if len(rendered) == 0 {
				rendered = nil
			}
			if !slices.Equal(rendered, test.want) {
				t.Fatalf("parents %v, want %v", rendered, test.want)
			}
		})
	}
}

func TestMapping(t *testing.T) {
	m := gitgraph.NewMapping()
	if err := m.Set(sha("s1"), sha("d1")); err != nil {
		t.Fatalf("set: %v", err)
	}
	// Several source commits legitimately collapse onto one destination commit.
	if err := m.Set(sha("s2"), sha("d1")); err != nil {
		t.Fatalf("set second source: %v", err)
	}
	// Recording the same pair again is how a resumed run replays what it knows.
	if err := m.Set(sha("s1"), sha("d1")); err != nil {
		t.Fatalf("repeat set: %v", err)
	}
	if err := m.Set(sha("s1"), sha("d2")); err == nil {
		t.Fatal("expected an error when a source is remapped")
	}

	if got, ok := m.Destination(sha("s2")); !ok || got != sha("d1") {
		t.Fatalf("destination = %q, %v", got, ok)
	}
	if got, ok := m.Source(sha("d1")); !ok || got != sha("s1") {
		t.Fatal("reverse lookup must report the first source recorded")
	}
	if _, ok := m.Destination(sha("absent")); ok {
		t.Fatal("absent source must not resolve")
	}
	if m.Len() != 2 {
		t.Fatalf("len = %d, want 2", m.Len())
	}
	if want := []string{sha("s1"), sha("s2")}; !slices.Equal(m.Sources(), sortedCopy(want)) {
		t.Fatalf("sources %v are not sorted", m.Sources())
	}
	if err := m.Set("", sha("d1")); err == nil {
		t.Fatal("expected an error for an empty source")
	}
}

// sortedCopy returns a sorted copy so an expectation does not depend on the
// order the fixture happened to list.
func sortedCopy(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return out
}

func TestTrailerValue(t *testing.T) {
	tests := []struct {
		name    string
		message string
		key     string
		want    string
		found   bool
	}{
		{
			name:    "single trailer",
			message: "subject\n\nbody\n\nKubernetes-commit: abc\n",
			key:     "Kubernetes-commit",
			want:    "abc",
			found:   true,
		},
		{
			name:    "last occurrence wins",
			message: "subject\n\nKubernetes-commit: one\nKubernetes-commit: two\n",
			key:     "Kubernetes-commit",
			want:    "two",
			found:   true,
		},
		{
			name:    "key match is case insensitive",
			message: "subject\n\nkubernetes-commit: abc\n",
			key:     "Kubernetes-commit",
			want:    "abc",
			found:   true,
		},
		{
			// A revert quotes the reverted commit's provenance in its body. That
			// line must not become this commit's provenance.
			name:    "trailer shaped body line does not count",
			message: "subject\n\nThis reverts a commit whose trailer read\nKubernetes-commit: quoted\n\nSigned-off-by: A <a@example.com>\n",
			key:     "Kubernetes-commit",
			found:   false,
		},
		{
			name:    "carriage returns are tolerated",
			message: "subject\r\n\r\nKubernetes-commit: abc\r\n",
			key:     "Kubernetes-commit",
			want:    "abc",
			found:   true,
		},
		{
			name:    "trailer beside other trailers",
			message: "subject\n\nSigned-off-by: A <a@example.com>\nKubernetes-commit: abc\n",
			key:     "Kubernetes-commit",
			want:    "abc",
			found:   true,
		},
		{
			name:    "missing trailer",
			message: "subject\n\nbody only\n",
			key:     "Kubernetes-commit",
			found:   false,
		},
		{
			// Git never reads the first paragraph as trailers, so a subject that
			// happens to be shaped like one is still a subject. Accepting it
			// would let any commit whose subject reads "Fix: thing" claim
			// provenance it does not have.
			name:    "subject alone is not a trailer",
			message: "Kubernetes-commit: abc",
			key:     "Kubernetes-commit",
			found:   false,
		},
		{
			// Continuation lines fold into the trailer above them rather than
			// starting one, so an indented provenance line quoted inside another
			// trailer's value cannot introduce a second claim.
			name:    "indented line continues the trailer above it",
			message: "subject\n\nSigned-off-by: A <a@example.com>\n  Kubernetes-commit: quoted\n",
			key:     "Kubernetes-commit",
			found:   false,
		},
		{
			// One ordinary line disqualifies the whole block, which is what stops
			// a closing paragraph that happens to contain a colon from being read
			// as provenance.
			name:    "prose beside a trailer disqualifies the block",
			message: "subject\n\nSee the notes for details\nKubernetes-commit: abc\n",
			key:     "Kubernetes-commit",
			found:   false,
		},
		{
			// Everything from the patch separator is a diff, and a diff may
			// legitimately contain anything at all.
			name:    "patch part is not part of the message",
			message: "subject\n\nKubernetes-commit: abc\n---\n diff --git a/x b/x\n",
			key:     "Kubernetes-commit",
			want:    "abc",
			found:   true,
		},
		{
			name:    "comment lines are ignored inside the block",
			message: "subject\n\n# note\nKubernetes-commit: abc\n",
			key:     "Kubernetes-commit",
			want:    "abc",
			found:   true,
		},
		{name: "empty message", message: "", key: "Kubernetes-commit", found: false},
		{name: "empty key", message: "subject\n\nk: v\n", key: "", found: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, found := gitgraph.TrailerValue(test.message, test.key)
			if found != test.found {
				t.Fatalf("found = %v, want %v", found, test.found)
			}
			if got != test.want {
				t.Fatalf("value %q, want %q", got, test.want)
			}
		})
	}
}

func TestMappingFromTrailers(t *testing.T) {
	const key = "Kubernetes-commit"
	destination := []gitgraph.Commit{
		{SHA: sha("d1"), Message: "one\n\n" + key + ": " + sha("s1") + "\n"},
		// A generated commit that no single source commit produced, such as a
		// dependency bump, carries no provenance trailer and is skipped.
		{SHA: sha("d2"), Message: "update dependencies\n"},
		{SHA: sha("d3"), Message: "three\n\n" + key + ": " + sha("s3") + "\n"},
	}

	m, err := gitgraph.MappingFromTrailers(destination, key)
	if err != nil {
		t.Fatalf("mapping from trailers: %v", err)
	}
	if m.Len() != 2 {
		t.Fatalf("len = %d, want 2", m.Len())
	}
	if got, ok := m.Destination(sha("s3")); !ok || got != sha("d3") {
		t.Fatalf("destination = %q, %v", got, ok)
	}

	malformed := []gitgraph.Commit{{SHA: sha("d1"), Message: "one\n\n" + key + ": not-a-sha\n"}}
	if _, err := gitgraph.MappingFromTrailers(malformed, key); err == nil {
		t.Fatal("expected an error for a malformed trailer")
	}
	if _, err := gitgraph.MappingFromTrailers(destination, ""); err == nil {
		t.Fatal("expected an error for an empty key")
	}
}

func TestGraphAccessors(t *testing.T) {
	graph := build(t, merged...)

	if graph.Len() != 5 {
		t.Fatalf("len = %d, want 5", graph.Len())
	}
	if !graph.Has(sha("M")) || graph.Has(sha("absent")) {
		t.Fatal("membership is wrong")
	}
	commit, ok := graph.Commit(sha("M"))
	if !ok {
		t.Fatal("merge commit is missing")
	}
	if want := shas("B", "F2"); !slices.Equal(commit.Parents, want) {
		t.Fatalf("parents %v, want %v", commit.Parents, want)
	}
	// The returned parents are a copy, so a caller cannot reach into the graph.
	commit.Parents[0] = sha("tampered")
	if again, _ := graph.Commit(sha("M")); again.Parents[0] != sha("B") {
		t.Fatal("Commit exposed its internal parent slice")
	}

	children, err := graph.Children(sha("A"))
	if err != nil {
		t.Fatalf("children: %v", err)
	}
	if want := []string{"B", "F1"}; !slices.Equal(labels(want, children), want) {
		t.Fatalf("children %v, want %v", labels(want, children), want)
	}
	if heads := graph.Heads(); !slices.Equal(labels([]string{"M"}, heads), []string{"M"}) {
		t.Fatalf("heads %v, want M", heads)
	}
	if _, err := graph.Children(sha("absent")); !errors.Is(err, gitgraph.ErrUnknownCommit) {
		t.Fatalf("error %v is not an unknown commit", err)
	}
	if _, err := graph.Parents(sha("absent")); !errors.Is(err, gitgraph.ErrUnknownCommit) {
		t.Fatalf("error %v is not an unknown commit", err)
	}
	if _, ok := graph.Commit(sha("absent")); ok {
		t.Fatal("absent commit must not resolve")
	}
	if _, err := graph.FirstParentLine(sha("absent")); !errors.Is(err, gitgraph.ErrUnknownCommit) {
		t.Fatalf("error %v is not an unknown commit", err)
	}
}

// TestFirstParentGraphRefusesToShapeAMerge is the fail-closed behaviour that
// keeps a mainline walk from flattening merges.
//
// A first parent walk reports both parents of every merge it passes through but
// never visits the second one, so the second parent is in the node's parent
// list and is not a node of the graph. Resolving such a parent through the
// mapping is impossible, and dropping it turns a merge into an ordinary commit:
// the replayed history would claim the side branch was never merged.
func TestFirstParentGraphRefusesToShapeAMerge(t *testing.T) {
	// The shape rev-list --first-parent --parents produces: the merge keeps
	// both parents, and only the mainline was walked.
	graph, err := gitgraph.NewFirstParent(commits(
		"base",
		"mainOne base",
		"merge mainOne feature",
	))
	if err != nil {
		t.Fatalf("build first parent graph: %v", err)
	}
	if !graph.FollowsFirstParent() {
		t.Fatal("a first parent graph must report itself as one")
	}

	// The parent that was never walked is visible rather than silently absent.
	boundary, err := graph.BoundaryParents(sha("merge"))
	if err != nil {
		t.Fatalf("boundary parents: %v", err)
	}
	if !slices.Equal(boundary, shas("feature")) {
		t.Fatalf("boundary parents = %v, want the unwalked merge parent", labels([]string{"feature"}, boundary))
	}

	mapping := gitgraph.NewMapping()
	for _, label := range []string{"base", "mainOne"} {
		if err := mapping.Set(sha(label), sha("d-"+label)); err != nil {
			t.Fatalf("record mapping: %v", err)
		}
	}

	// A merge cannot be shaped from this graph, and saying so is the point.
	if _, err := graph.MappedParents(sha("merge"), mapping, nil); !errors.Is(err, gitgraph.ErrFirstParentGraph) {
		t.Fatalf("mapped parents of a merge = %v, want %v", err, gitgraph.ErrFirstParentGraph)
	}
	// Ordinary commits on the mainline are unaffected.
	parents, err := graph.MappedParents(sha("mainOne"), mapping, nil)
	if err != nil {
		t.Fatalf("mapped parents of a mainline commit: %v", err)
	}
	if !slices.Equal(parents, []string{sha("d-base")}) {
		t.Fatalf("mapped parents = %v, want the mapped base", parents)
	}
}

// TestMappedParentsDropsBoundedParents pins the case that still has to work: a
// graph bounded below by an anchor has commits whose parents were excluded on
// purpose, and the transformed history starts at the boundary rather than
// reaching behind it.
func TestMappedParentsDropsBoundedParents(t *testing.T) {
	// anchor keeps a parent that is outside the graph, exactly as a range
	// bounded walk produces it.
	graph, err := gitgraph.New(commits(
		"anchor beforeAnchor",
		"one anchor",
		"side anchor",
		"merge one side",
	))
	if err != nil {
		t.Fatalf("build graph: %v", err)
	}
	if graph.FollowsFirstParent() {
		t.Fatal("a fully walked graph must not claim to follow first parents")
	}

	boundary, err := graph.BoundaryParents(sha("anchor"))
	if err != nil {
		t.Fatalf("boundary parents: %v", err)
	}
	if !slices.Equal(boundary, shas("beforeAnchor")) {
		t.Fatalf("boundary parents = %v, want the excluded ancestor", boundary)
	}

	mapping := gitgraph.NewMapping()
	for _, label := range []string{"anchor", "one", "side"} {
		if err := mapping.Set(sha(label), sha("d-"+label)); err != nil {
			t.Fatalf("record mapping: %v", err)
		}
	}

	// The anchor has no parent inside the graph, so it becomes a root.
	parents, err := graph.MappedParents(sha("anchor"), mapping, nil)
	if err != nil {
		t.Fatalf("mapped parents of the anchor: %v", err)
	}
	if len(parents) != 0 {
		t.Fatalf("anchor mapped parents = %v, want none", parents)
	}

	// A merge whose parents were all walked keeps both of them.
	parents, err = graph.MappedParents(sha("merge"), mapping, nil)
	if err != nil {
		t.Fatalf("mapped parents of the merge: %v", err)
	}
	if !slices.Equal(parents, shas("d-one", "d-side")) {
		t.Fatalf("merge mapped parents = %v, want both mapped parents", parents)
	}
}
