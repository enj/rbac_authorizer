package patchset

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/enj/soapbox/tools/internal/gitcli"
)

// conflictedPaths reports every unmerged path in a status, sorted and
// deduplicated so a conflict report is byte identical across runs.
//
// Git already reports status in path order, but the report is an artifact CI
// uploads and a tracking issue quotes, so the ordering it depends on is
// established here rather than inherited.
func conflictedPaths(entries []gitcli.StatusEntry) []string {
	var paths []string
	for _, entry := range entries {
		if entry.Conflicted() {
			paths = append(paths, entry.Path)
		}
	}
	slices.Sort(paths)
	return slices.Compact(paths)
}

// renderStatus renders one status entry the way git status would print it, so a
// maintainer reading the report sees the vocabulary the Git documentation uses.
func renderStatus(entry gitcli.StatusEntry) string {
	return entry.Code + " " + entry.Path
}

// dirtyPathLimit bounds how many paths a precondition failure names. A caller
// that handed over a whole materialized but uncommitted tree would otherwise
// produce a message thousands of paths long, and the first few already identify
// the mistake.
const dirtyPathLimit = 10

// describeDirty renders the entries that fail the clean HEAD precondition,
// sorted and bounded so the message is byte identical across runs.
func describeDirty(entries []gitcli.StatusEntry) string {
	rendered := make([]string, 0, len(entries))
	for _, entry := range entries {
		rendered = append(rendered, renderStatus(entry))
	}
	slices.Sort(rendered)
	if len(rendered) <= dirtyPathLimit {
		return strings.Join(rendered, ", ")
	}
	return strings.Join(rendered[:dirtyPathLimit], ", ") +
		fmt.Sprintf(", and %s more", strconv.Itoa(len(rendered)-dirtyPathLimit))
}
