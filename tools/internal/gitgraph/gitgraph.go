// Package gitgraph provides the commit graph data structures and the pure graph
// algorithms that source discovery and deterministic replay share.
//
// The algorithms are adapted from the Apache License 2.0 licensed
// kubernetes/publishing-bot, specifically the concepts in pkg/git/mainline.go,
// pkg/git/mapping.go, and pkg/git/kube.go: the first parent mainline of a
// branch, the source to destination commit mapping carried by a provenance
// trailer, the nearest mapped ancestor of a source commit that was never
// materialized, and the deduplication of mapped merge parents. This is a generic
// reimplementation over the types declared here rather than a copy of that
// project's code, and it carries no Kubernetes specific behaviour: the
// provenance trailer key is a parameter.
//
// Nothing in this package performs input or output. Every algorithm is a pure
// function of the graph it is handed, so replay order is testable without a
// repository, and every returned slice has a documented deterministic order.
// Results never depend on map iteration order, because two runs of the engine
// must produce byte identical history.
package gitgraph

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Graph sentinels. Callers use errors.Is to distinguish the failure.
var (
	// ErrUnknownCommit reports a revision that is not a node of the graph.
	ErrUnknownCommit = errors.New("commit is not in the graph")
	// ErrNoCommonAnchor reports refs whose histories never meet, which means
	// there is no commit the transformed history could be anchored to.
	ErrNoCommonAnchor = errors.New("refs have no common ancestor")
	// ErrAmbiguousAnchor reports refs with more than one best common ancestor,
	// as a criss-cross merge produces. Picking one arbitrarily would make
	// published history depend on traversal order, so the run stops instead.
	ErrAmbiguousAnchor = errors.New("refs have more than one best common ancestor")
	// ErrGraphCycle reports parent edges that form a cycle. Git cannot produce
	// one, so it means the input was assembled or truncated incorrectly.
	ErrGraphCycle = errors.New("commit graph contains a cycle")
	// ErrFirstParentGraph reports a merge shaping query against a graph that
	// holds only first parent ancestry. Such a graph records that a merge has a
	// second parent but nothing about the history behind it, so any answer about
	// the merge's parents would silently be the answer for a non-merge.
	ErrFirstParentGraph = errors.New("graph follows first parents only and cannot shape a merge")
)

// Commit is one node of a commit graph.
//
// Only SHA and Parents take part in traversal. The remaining fields carry
// metadata a caller has already read and wants to keep alongside the node, and
// an algorithm that needed them would stop being a pure graph algorithm.
type Commit struct {
	// SHA is the commit object name.
	SHA string
	// Parents are the parent object names in git's order, so Parents[0] is the
	// first parent and defines the mainline.
	Parents []string
	// Tree is the commit's tree object name. It may be empty.
	Tree string
	// Subject is the commit's subject line. It may be empty.
	Subject string
	// Message is the complete commit message. It may be empty.
	Message string
}

// Graph is an immutable commit DAG.
//
// A parent that is not itself a node of the graph is a boundary edge: it is
// preserved in the node's parent list, because dropping it would silently turn
// a merge into an ordinary commit, but traversal treats it as absent. That is
// what makes a graph bounded below by an anchor usable without materializing
// the history before it.
type Graph struct {
	nodes    []Commit
	index    map[string]int
	children map[string][]string
	topo     []string
	// firstParent records a graph built from first parent ancestry only. It is
	// not an optimisation hint: it is the difference between a boundary parent
	// that was deliberately left below the anchor and one whose history was
	// never walked at all.
	firstParent bool
}

// New builds a graph from commits. The slice order is the caller's traversal
// order, normally git's own reverse topological order, and it is the tie-break
// that makes TopologicalOrder reproducible.
func New(commits []Commit) (*Graph, error) {
	return newGraph(commits, false)
}

// NewFirstParent builds a graph from commits that were walked along first
// parents only, such as the output of rev-list --first-parent.
//
// Such a walk still reports both parents of every merge it passes through, but
// it never visits the second one, so the graph knows that a merge exists and
// nothing about the side it merged. Recording that here is what stops a later
// merge shaping query from resolving the merge to its first parent alone and
// quietly publishing a merge with one parent.
func NewFirstParent(commits []Commit) (*Graph, error) {
	return newGraph(commits, true)
}

// newGraph builds and validates a graph.
func newGraph(commits []Commit, firstParent bool) (*Graph, error) {
	g := &Graph{
		nodes:       make([]Commit, 0, len(commits)),
		index:       make(map[string]int, len(commits)),
		children:    make(map[string][]string, len(commits)),
		firstParent: firstParent,
	}
	for _, commit := range commits {
		if err := ValidateSHA(commit.SHA); err != nil {
			return nil, fmt.Errorf("commit graph: %w", err)
		}
		if _, exists := g.index[commit.SHA]; exists {
			return nil, fmt.Errorf("commit graph: commit %s appears more than once", commit.SHA)
		}
		node := commit
		node.Parents = slices.Clone(commit.Parents)
		for _, parent := range node.Parents {
			if err := ValidateSHA(parent); err != nil {
				return nil, fmt.Errorf("commit graph: parent of %s: %w", commit.SHA, err)
			}
			if parent == commit.SHA {
				return nil, fmt.Errorf("commit graph: commit %s is its own parent", commit.SHA)
			}
		}
		g.index[node.SHA] = len(g.nodes)
		g.nodes = append(g.nodes, node)
	}

	for _, node := range g.nodes {
		for _, parent := range node.Parents {
			if _, present := g.index[parent]; present {
				g.children[parent] = append(g.children[parent], node.SHA)
			}
		}
	}

	topo, err := g.sortTopologically()
	if err != nil {
		return nil, err
	}
	g.topo = topo
	return g, nil
}

// ValidateSHA checks that value is a full lower case hexadecimal object name.
// Abbreviated names are refused because two different commits can share a
// prefix and the graph is keyed by identity.
func ValidateSHA(value string) error {
	if len(value) != 40 && len(value) != 64 {
		return fmt.Errorf("object name %q must be 40 or 64 hexadecimal characters", value)
	}
	for _, r := range value {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f':
		default:
			return fmt.Errorf("object name %q must be lower case hexadecimal", value)
		}
	}
	return nil
}

// Len reports the number of commits in the graph.
func (g *Graph) Len() int { return len(g.nodes) }

// Has reports whether sha is a node of the graph.
func (g *Graph) Has(sha string) bool {
	_, present := g.index[sha]
	return present
}

// Commit returns the recorded commit.
func (g *Graph) Commit(sha string) (Commit, bool) {
	i, present := g.index[sha]
	if !present {
		return Commit{}, false
	}
	node := g.nodes[i]
	node.Parents = slices.Clone(node.Parents)
	return node, true
}

// Parents reports the parent object names in git's order, including boundary
// parents that are not nodes of this graph.
func (g *Graph) Parents(sha string) ([]string, error) {
	i, present := g.index[sha]
	if !present {
		return nil, fmt.Errorf("commit %s: %w", sha, ErrUnknownCommit)
	}
	return slices.Clone(g.nodes[i].Parents), nil
}

// Children reports the commits that name sha as a parent, in the order they
// were passed to New.
func (g *Graph) Children(sha string) ([]string, error) {
	if _, present := g.index[sha]; !present {
		return nil, fmt.Errorf("commit %s: %w", sha, ErrUnknownCommit)
	}
	return slices.Clone(g.children[sha]), nil
}

// FollowsFirstParent reports a graph that holds first parent ancestry only.
func (g *Graph) FollowsFirstParent() bool { return g.firstParent }

// BoundaryParents reports the parents of sha that are not themselves nodes of
// this graph, in git's parent order.
//
// A bounded graph has them by construction: the commits just above the boundary
// name parents that were deliberately excluded. Reporting them explicitly is
// what lets a caller tell that a commit's parent list was truncated rather than
// discovering it from a merge that arrived with one parent.
func (g *Graph) BoundaryParents(sha string) ([]string, error) {
	i, present := g.index[sha]
	if !present {
		return nil, fmt.Errorf("commit %s: %w", sha, ErrUnknownCommit)
	}
	var boundary []string
	for _, parent := range g.nodes[i].Parents {
		if _, present := g.index[parent]; !present {
			boundary = append(boundary, parent)
		}
	}
	return boundary, nil
}

// Roots reports the commits with no parent inside the graph, in the order they
// were passed to New. A bounded graph has the commits just above its boundary
// as roots.
func (g *Graph) Roots() []string {
	var roots []string
	for _, node := range g.nodes {
		if g.presentParents(node) == 0 {
			roots = append(roots, node.SHA)
		}
	}
	return roots
}

// Heads reports the commits no other commit in the graph descends from, in the
// order they were passed to New.
func (g *Graph) Heads() []string {
	var heads []string
	for _, node := range g.nodes {
		if len(g.children[node.SHA]) == 0 {
			heads = append(heads, node.SHA)
		}
	}
	return heads
}

// TopologicalOrder reports every commit with each parent strictly before its
// children. Commits that are ready at the same time are emitted in the order
// they were passed to New, so the sequence is stable across runs.
func (g *Graph) TopologicalOrder() []string { return slices.Clone(g.topo) }

// presentParents counts the parents of a node that are inside the graph.
func (g *Graph) presentParents(node Commit) int {
	count := 0
	for _, parent := range node.Parents {
		if _, present := g.index[parent]; present {
			count++
		}
	}
	return count
}

// sortTopologically runs Kahn's algorithm over the parent relation, breaking
// ties with the position each commit had in the input. A ready set that empties
// before every commit is emitted means the parent edges form a cycle.
func (g *Graph) sortTopologically() ([]string, error) {
	remaining := make([]int, len(g.nodes))
	ready := make(intHeap, 0, len(g.nodes))
	for i, node := range g.nodes {
		remaining[i] = g.presentParents(node)
		if remaining[i] == 0 {
			ready.push(i)
		}
	}

	order := make([]string, 0, len(g.nodes))
	for len(ready) > 0 {
		sha := g.nodes[ready.pop()].SHA
		order = append(order, sha)
		for _, child := range g.children[sha] {
			j := g.index[child]
			remaining[j]--
			if remaining[j] == 0 {
				ready.push(j)
			}
		}
	}
	if len(order) != len(g.nodes) {
		return nil, fmt.Errorf("commit graph: %w", ErrGraphCycle)
	}
	return order, nil
}

// intHeap is a minimal binary min-heap of input positions. container/heap is
// not used because its any based interface would require an unchecked type
// assertion on every push and pop.
type intHeap []int

// push adds a position and restores the heap property.
func (h *intHeap) push(value int) {
	*h = append(*h, value)
	for i := len(*h) - 1; i > 0; {
		parent := (i - 1) / 2
		if (*h)[parent] <= (*h)[i] {
			return
		}
		(*h)[parent], (*h)[i] = (*h)[i], (*h)[parent]
		i = parent
	}
}

// pop removes and returns the smallest position.
func (h *intHeap) pop() int {
	old := *h
	smallest := old[0]
	last := len(old) - 1
	old[0] = old[last]
	*h = old[:last]

	heapified := *h
	for i := 0; ; {
		left, right, next := 2*i+1, 2*i+2, i
		if left < len(heapified) && heapified[left] < heapified[next] {
			next = left
		}
		if right < len(heapified) && heapified[right] < heapified[next] {
			next = right
		}
		if next == i {
			return smallest
		}
		heapified[i], heapified[next] = heapified[next], heapified[i]
		i = next
	}
}

// FirstParentLine reports the mainline of head: head itself, then its first
// parent, and so on until the line leaves the graph. This is the notion of a
// branch's own history that publishing-bot calls the mainline, and it is what
// distinguishes commits a branch made from commits it merged in.
func (g *Graph) FirstParentLine(head string) ([]string, error) {
	if _, present := g.index[head]; !present {
		return nil, fmt.Errorf("commit %s: %w", head, ErrUnknownCommit)
	}
	var (
		line    []string
		visited = make(map[string]bool, len(g.nodes))
		current = head
	)
	for {
		if visited[current] {
			return nil, fmt.Errorf("commit graph: %w", ErrGraphCycle)
		}
		visited[current] = true
		line = append(line, current)

		i, present := g.index[current]
		if !present {
			return line, nil
		}
		parents := g.nodes[i].Parents
		if len(parents) == 0 {
			return line, nil
		}
		if _, present := g.index[parents[0]]; !present {
			return line, nil
		}
		current = parents[0]
	}
}

// Ancestors reports every commit reachable from sha by following parents,
// including sha itself, which matches git's convention that a commit is its own
// ancestor.
func (g *Graph) Ancestors(sha string) (map[string]bool, error) {
	if _, present := g.index[sha]; !present {
		return nil, fmt.Errorf("commit %s: %w", sha, ErrUnknownCommit)
	}
	seen := map[string]bool{sha: true}
	queue := []string{sha}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		i, present := g.index[current]
		if !present {
			continue
		}
		for _, parent := range g.nodes[i].Parents {
			if _, present := g.index[parent]; !present || seen[parent] {
				continue
			}
			seen[parent] = true
			queue = append(queue, parent)
		}
	}
	return seen, nil
}

// IsAncestor reports whether ancestor is reachable from descendant. A commit is
// its own ancestor, matching git merge-base --is-ancestor.
func (g *Graph) IsAncestor(ancestor, descendant string) (bool, error) {
	if _, present := g.index[ancestor]; !present {
		return false, fmt.Errorf("commit %s: %w", ancestor, ErrUnknownCommit)
	}
	reachable, err := g.Ancestors(descendant)
	if err != nil {
		return false, err
	}
	return reachable[ancestor], nil
}

// MergeBases reports the best common ancestors of two commits: the common
// ancestors that are not themselves ancestors of another common ancestor. The
// result is sorted by object name so a caller never depends on traversal order.
func (g *Graph) MergeBases(a, b string) ([]string, error) {
	ancestorsA, err := g.Ancestors(a)
	if err != nil {
		return nil, err
	}
	ancestorsB, err := g.Ancestors(b)
	if err != nil {
		return nil, err
	}
	common := make(map[string]bool, min(len(ancestorsA), len(ancestorsB)))
	for sha := range ancestorsA {
		if ancestorsB[sha] {
			common[sha] = true
		}
	}

	// An intersection of two ancestor sets contains the ancestors of everything
	// in it, so a common ancestor is superseded exactly when one of its children
	// is also common. That local test replaces a reachability query per
	// candidate, which is the difference between one linear pass and a quadratic
	// walk over a history the size of Kubernetes.
	kept := make([]string, 0, 4)
	for _, node := range g.nodes {
		if !common[node.SHA] {
			continue
		}
		superseded := false
		for _, child := range g.children[node.SHA] {
			if common[child] {
				superseded = true
				break
			}
		}
		if !superseded {
			kept = append(kept, node.SHA)
		}
	}
	slices.Sort(kept)
	return kept, nil
}

// best reduces candidates to those that are not a proper ancestor of another
// candidate, sorted by object name. It answers a reachability query per
// candidate, so it is used only for the small sets that merge base folding and
// parent deduplication produce, never for a whole ancestor set.
func (g *Graph) best(candidates []string) ([]string, error) {
	unique := make([]string, 0, len(candidates))
	seen := make(map[string]bool, len(candidates))
	for _, candidate := range candidates {
		if seen[candidate] {
			continue
		}
		seen[candidate] = true
		unique = append(unique, candidate)
	}

	superseded := make(map[string]bool, len(unique))
	for _, candidate := range unique {
		reachable, err := g.Ancestors(candidate)
		if err != nil {
			return nil, err
		}
		for _, other := range unique {
			if other != candidate && reachable[other] {
				superseded[other] = true
			}
		}
	}

	kept := make([]string, 0, len(unique))
	for _, candidate := range unique {
		if !superseded[candidate] {
			kept = append(kept, candidate)
		}
	}
	slices.Sort(kept)
	return kept, nil
}

// CommonAnchor reports the single best common ancestor of every head. The
// engine records that commit as the immutable anchor of the transformed
// history, so an absent or ambiguous answer must stop the run rather than
// resolve itself arbitrarily.
func (g *Graph) CommonAnchor(heads []string) (string, error) {
	if len(heads) == 0 {
		return "", errors.New("common anchor: at least one head is required")
	}
	for _, head := range heads {
		if _, present := g.index[head]; !present {
			return "", fmt.Errorf("common anchor: commit %s: %w", head, ErrUnknownCommit)
		}
	}

	bases := []string{heads[0]}
	for _, head := range heads[1:] {
		var next []string
		for _, base := range bases {
			pairwise, err := g.MergeBases(base, head)
			if err != nil {
				return "", fmt.Errorf("common anchor: %w", err)
			}
			next = append(next, pairwise...)
		}
		reduced, err := g.best(next)
		if err != nil {
			return "", fmt.Errorf("common anchor: %w", err)
		}
		bases = reduced
		if len(bases) == 0 {
			return "", fmt.Errorf("common anchor: %w", ErrNoCommonAnchor)
		}
	}
	switch len(bases) {
	case 0:
		return "", fmt.Errorf("common anchor: %w", ErrNoCommonAnchor)
	case 1:
		return bases[0], nil
	default:
		return "", fmt.Errorf("common anchor: %s: %w", strings.Join(bases, ", "), ErrAmbiguousAnchor)
	}
}

// ValidateAnchor checks that every head descends from anchor. A newly tracked
// branch that does not is refused, because replaying it would produce history
// that shares no root with what was already published.
func (g *Graph) ValidateAnchor(anchor string, heads []string) error {
	if _, present := g.index[anchor]; !present {
		return fmt.Errorf("anchor %s: %w", anchor, ErrUnknownCommit)
	}
	for _, head := range heads {
		descends, err := g.IsAncestor(anchor, head)
		if err != nil {
			return fmt.Errorf("anchor validation: %w", err)
		}
		if !descends {
			return fmt.Errorf("anchor validation: commit %s does not descend from anchor %s", head, anchor)
		}
	}
	return nil
}

// Range reports the commits reachable from heads without the proper ancestors of
// anchor, with anchor itself retained as the base of the transformed history.
// The result is in topological order, parents before children.
func (g *Graph) Range(anchor string, heads []string) ([]string, error) {
	if err := g.ValidateAnchor(anchor, heads); err != nil {
		return nil, err
	}
	reachable := make(map[string]bool, len(g.nodes))
	for _, head := range heads {
		ancestors, err := g.Ancestors(head)
		if err != nil {
			return nil, err
		}
		for sha := range ancestors {
			reachable[sha] = true
		}
	}
	excluded, err := g.Ancestors(anchor)
	if err != nil {
		return nil, err
	}

	selected := make([]string, 0, len(reachable))
	for _, sha := range g.topo {
		if !reachable[sha] {
			continue
		}
		if excluded[sha] && sha != anchor {
			continue
		}
		selected = append(selected, sha)
	}
	return selected, nil
}

// DedupeParents removes duplicate parents and any parent that is already an
// ancestor of another parent, preserving the order in which they first appear.
//
// This is what keeps a replayed merge honest. Two source parents whose changes
// were both irrelevant collapse onto the same destination commit, and a merge
// listing that commit twice, or listing a commit that its sibling already
// contains, is a merge with nothing to merge.
func (g *Graph) DedupeParents(parents []string) ([]string, error) {
	unique := make([]string, 0, len(parents))
	seen := make(map[string]bool, len(parents))
	for _, parent := range parents {
		if _, present := g.index[parent]; !present {
			return nil, fmt.Errorf("parent %s: %w", parent, ErrUnknownCommit)
		}
		if seen[parent] {
			continue
		}
		seen[parent] = true
		unique = append(unique, parent)
	}

	superseded := make(map[string]bool, len(unique))
	for _, parent := range unique {
		reachable, err := g.Ancestors(parent)
		if err != nil {
			return nil, err
		}
		for _, other := range unique {
			if other != parent && reachable[other] {
				superseded[other] = true
			}
		}
	}

	kept := make([]string, 0, len(unique))
	for _, parent := range unique {
		if !superseded[parent] {
			kept = append(kept, parent)
		}
	}
	return kept, nil
}

// NearestMappedAncestor reports the destination commit of the closest ancestor
// of sha that has a mapping, and whether one exists. A source commit whose
// transformed tree was unchanged is never materialized, so its descendants have
// to attach to whatever ancestor was.
//
// The search is breadth first in parent order, so at equal distance the first
// parent line wins and the answer does not depend on map iteration order.
func (g *Graph) NearestMappedAncestor(sha string, m *Mapping) (string, bool, error) {
	if m == nil {
		return "", false, errors.New("nearest mapped ancestor: mapping must not be nil")
	}
	if _, present := g.index[sha]; !present {
		return "", false, fmt.Errorf("commit %s: %w", sha, ErrUnknownCommit)
	}
	visited := map[string]bool{sha: true}
	queue := []string{sha}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if destination, ok := m.Destination(current); ok {
			return destination, true, nil
		}
		i, present := g.index[current]
		if !present {
			continue
		}
		for _, parent := range g.nodes[i].Parents {
			if _, present := g.index[parent]; !present || visited[parent] {
				continue
			}
			visited[parent] = true
			queue = append(queue, parent)
		}
	}
	return "", false, nil
}

// MappedParents reports the destination parents of a source commit: each source
// parent resolved through NearestMappedAncestor, then deduplicated against the
// destination graph.
//
// A parent that is not a node of this graph is below its boundary: its history
// was excluded on purpose, and the transformed history starts at the boundary
// rather than reaching behind it. Such a parent contributes nothing, which is
// how the first replayed commit ends up with no parent at all.
//
// That reasoning only holds for a graph that walked every parent. A first parent
// graph records a merge's second parent without ever having visited it, so
// dropping it would not be bounding the history, it would be turning a merge
// into an ordinary commit. Merge shaping against such a graph is refused.
//
// A nil destination graph still removes exact duplicates but cannot remove a
// parent that the destination history already contains, so callers that have
// the destination graph should pass it.
func (g *Graph) MappedParents(sha string, m *Mapping, destination *Graph) ([]string, error) {
	parents, err := g.Parents(sha)
	if err != nil {
		return nil, err
	}
	if g.firstParent && len(parents) > 1 {
		return nil, fmt.Errorf("mapped parents of %s: %w", sha, ErrFirstParentGraph)
	}
	mapped := make([]string, 0, len(parents))
	seen := make(map[string]bool, len(parents))
	for _, parent := range parents {
		if _, present := g.index[parent]; !present {
			continue
		}
		target, ok, err := g.NearestMappedAncestor(parent, m)
		if err != nil {
			return nil, fmt.Errorf("mapped parents of %s: %w", sha, err)
		}
		if !ok || seen[target] {
			continue
		}
		seen[target] = true
		mapped = append(mapped, target)
	}
	if destination == nil {
		return mapped, nil
	}
	deduped, err := destination.DedupeParents(mapped)
	if err != nil {
		return nil, fmt.Errorf("mapped parents of %s: %w", sha, err)
	}
	return deduped, nil
}
