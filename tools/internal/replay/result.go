package replay

import (
	"fmt"
	"strings"

	"github.com/enj/soapbox/tools/internal/gitgraph"
)

// Record is what one source commit produced.
//
// Every slice in it belongs to the caller: the run clones what it was handed by
// the transform and what it read from the graph, so nothing a caller does to a
// record can reach back into the run's own state or into the next record.
type Record struct {
	// Source is the source commit object name.
	Source string
	// SourceParents are the source parent object names in git's order,
	// including any parent that was left below the anchor.
	SourceParents []string
	// MappedParents are the destination parents the commit received: each source
	// parent resolved through the mapping and deduplicated, or the epoch parent
	// when the mapping produced none.
	MappedParents []string
	// Tree is the destination tree the transform produced.
	Tree string
	// Destination is the destination commit this source commit maps to. It is
	// the commit that was written, or the one it collapsed onto, and it is empty
	// only when the commit generated nothing at all.
	Destination string
	// Changed is what the transform reported, after the run checked it against
	// the trees.
	Changed bool
	// Collapsed reports that no commit was written.
	Collapsed bool
	// Merge reports a written commit with more than one destination parent.
	Merge bool
	// Evidence is what the transform recorded about its decision.
	Evidence []string
}

// kind names what the record is, for a report line.
func (r Record) kind() string {
	switch {
	case r.Collapsed:
		return "collapse"
	case r.Merge:
		return "merge"
	default:
		return "commit"
	}
}

// Head is where one source head landed.
type Head struct {
	// Source is the source head that was replayed.
	Source string
	// Destination is the commit it produced, empty when the head generated
	// nothing, which is what an entirely irrelevant branch does.
	Destination string
}

// Result is what one replay run produced.
//
// It is a statement about content and shape and nothing else. There is no path,
// no duration, and no count of anything the destination repository merely
// happened to hold, so two runs of the same inputs on different machines produce
// identical results and a difference between two of them is a real difference.
type Result struct {
	// Epoch is the profile epoch the run generated under.
	Epoch Epoch
	// Records are the per commit outcomes in replay order.
	Records []Record
	// Heads are the destination commits the source heads produced, in the order
	// the heads were selected.
	Heads []Head
	// Written counts the commits the run wrote.
	Written int
	// Collapsed counts the source commits that produced no commit of their own.
	Collapsed int
	// Mapping is the source to destination mapping the run built, which is the
	// same relation the provenance trailers of the written commits carry.
	Mapping *gitgraph.Mapping
}

// Report renders the result as deterministic lines for a dry run.
//
// Evidence is deliberately absent. It is prose the transform chose, so it can
// hold anything including a line break, and a report meant to be compared line
// by line cannot let one record decide how many lines it occupies. The records
// themselves carry it for a caller that wants to print it.
func (r *Result) Report() []string {
	if r == nil {
		return nil
	}
	lines := make([]string, 0, len(r.Records)+len(r.Heads)+3)
	lines = append(lines,
		"profile "+r.Epoch.ProfileHash,
		"parent "+orNone(r.Epoch.Parent),
		fmt.Sprintf("written %d collapsed %d", r.Written, r.Collapsed),
	)
	for _, head := range r.Heads {
		lines = append(lines, "head "+head.Source+" "+orNone(head.Destination))
	}
	for _, record := range r.Records {
		lines = append(lines, strings.Join([]string{
			record.kind(), record.Source, orNone(record.Destination), record.Tree,
			"parents " + orNone(strings.Join(record.MappedParents, ",")),
		}, " "))
	}
	return lines
}

// orNone renders an absent object name as a word rather than as an empty field,
// so a report line always has the same number of fields.
func orNone(value string) string {
	if value == "" {
		return "none"
	}
	return value
}
