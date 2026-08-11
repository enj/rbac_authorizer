package patchset_test

import (
	"context"
	"fmt"
	"slices"

	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/patchset"
)

// dag is an in-memory commit graph that answers real ancestry queries by
// walking parent edges. Selection depends only on ancestry, so a graph built in
// Go exercises the selectors exactly as a repository would while letting a
// table describe a topology that would take several commits to create.
//
// The application tests use a real temporary repository, because application
// depends on Git's own three way merge behaviour and nothing in Go reproduces
// that.
type dag struct {
	// parents maps a commit to its parents. A commit with no entry does not
	// exist, which is how an unknown revision is modelled.
	parents map[string][]string
	// queries records every ancestry question in the order it was asked, so a
	// test can assert that selection is deterministic and that a branch scoped
	// patch is skipped before its ancestry is resolved.
	queries []string
}

// errUnsupported reports a Git operation selection never performs.
var errUnsupported = fmt.Errorf("operation is not part of patch selection")

// IsAncestor reports whether ancestor is reachable from descendant through
// parent edges, or is descendant itself, which is what git merge-base
// --is-ancestor answers.
func (d *dag) IsAncestor(ctx context.Context, ancestor, descendant string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	d.queries = append(d.queries, ancestor+"->"+descendant)
	for _, name := range []string{ancestor, descendant} {
		if _, ok := d.parents[name]; !ok {
			return false, fmt.Errorf("revision %q is unknown", name)
		}
	}
	seen := map[string]bool{}
	stack := []string{descendant}
	for len(stack) > 0 {
		current := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if current == ancestor {
			return true, nil
		}
		if seen[current] {
			continue
		}
		seen[current] = true
		stack = append(stack, d.parents[current]...)
	}
	return false, nil
}

func (d *dag) ApplyPatch(context.Context, gitcli.ApplyOptions) error { return errUnsupported }
func (d *dag) Status(context.Context) ([]gitcli.StatusEntry, error)  { return nil, errUnsupported }
func (d *dag) Diff(context.Context, gitcli.DiffOptions) (string, error) {
	return "", errUnsupported
}
func (d *dag) ResetHard(context.Context, string) error { return errUnsupported }

// newDAG builds the topology every selection test shares:
//
//	a -- b -- c -- d        master
//	      \
//	       e -- f           release-1.36
func newDAG() *dag {
	return &dag{parents: map[string][]string{
		"a": nil,
		"b": {"a"},
		"c": {"b"},
		"d": {"c"},
		"e": {"b"},
		"f": {"e"},
	}}
}

// ids reports the identifiers of a selected series in order.
func ids(patches []patchset.Patch) []string {
	names := make([]string, len(patches))
	for i, patch := range patches {
		names[i] = patch.ID
	}
	return names
}

// patch builds a usable patch with the given identifier and selectors.
func patch(id, since, until string, branches ...string) patchset.Patch {
	return patchset.Patch{
		ID:       id,
		Diff:     []byte("--- a/" + id + "\n+++ b/" + id + "\n"),
		Since:    since,
		Until:    until,
		Branches: slices.Clone(branches),
	}
}
