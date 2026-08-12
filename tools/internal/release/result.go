package release

// Result is what one release projection produced.
//
// It is a statement about objects and nothing else. There is no path, no
// duration, and no count of anything the destination repository merely happened
// to hold, so two runs of the same inputs on different machines produce
// identical results and a difference between two of them is a real difference.
type Result struct {
	// Tag is the destination tag name the policy mapped the release onto.
	Tag string
	// Object is the annotated tag object that was written. No ref names it, so it
	// is unreachable until a publisher decides to give it one.
	Object string
	// Target is the commit the tag object names.
	Target string
	// Commit is the projection commit the run wrote, empty when the release
	// projection was already what the replayed commit records.
	Commit string
	// Source is the exact upstream commit the release was cut from, and SourceTag
	// is the upstream release it was published as. Both are recorded in the tag
	// message and repeated here so an outward action manifest can be built from
	// results alone, without the options that produced them.
	Source    string
	SourceTag string
	// Message is the tag message exactly as the object records it.
	Message string
}

// Projected reports that the release needed a commit of its own, which is what a
// release projection differing from the replayed commit's tree produces.
//
// It is derived rather than recorded, because a flag stored beside the commit
// name is a second copy of the same fact and the two can disagree.
func (r *Result) Projected() bool {
	return r != nil && r.Commit != ""
}

// Report renders the result as deterministic lines for a dry run.
//
// The tag message is deliberately absent. It carries an upstream release URL and
// spans several lines, and a report meant to be compared line by line cannot let
// one release decide how many lines it occupies. The result carries it for a
// caller that wants to print it.
func (r *Result) Report() []string {
	if r == nil {
		return nil
	}
	return []string{
		"tag " + r.Tag + " " + r.Object,
		"target " + r.Target,
		"projection " + orNone(r.Commit),
		"source " + r.SourceTag + " " + r.Source,
	}
}

// orNone renders an absent object name as a word rather than as an empty field,
// so a report line always has the same number of fields.
func orNone(value string) string {
	if value == "" {
		return "none"
	}
	return value
}
