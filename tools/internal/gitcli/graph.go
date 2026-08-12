package gitcli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// ErrNoMergeBase reports revisions that share no common ancestor. Git signals
// this with exit status 1, which is a verdict rather than a failure, so it is
// reported as a distinguishable error instead of an empty result.
var ErrNoMergeBase = errors.New("revisions have no common ancestor")

// ErrAmbiguousMergeBase reports revisions with more than one best common
// ancestor, as a criss-cross merge produces. Git picks one and says nothing, so
// the ambiguity is surfaced here instead: a caller that anchors published
// history to a merge base must not have that anchor depend on traversal order.
var ErrAmbiguousMergeBase = errors.New("revisions have more than one best common ancestor")

// noLazyFetch stops a partial clone from reaching the network to satisfy an
// object lookup. Probing which objects are present locally must answer from the
// object store, not silently download the history it is asking about.
const noLazyFetch = "GIT_NO_LAZY_FETCH=1"

// ErrLazyFetchDisabled reports a request for a promisor fetch on a runner that
// was pinned against one by WithNoLazyFetch.
var ErrLazyFetchDisabled = errors.New("this runner refuses promisor fetches")

// DAGCommit is one node of a commit graph: an object name and the parents that
// define its edges. Nothing else is read, because the traversal that selects
// commits must not depend on message or tree contents.
type DAGCommit struct {
	// SHA is the commit object name.
	SHA string
	// Parents are the parent object names in git's order, so Parents[0] is the
	// first parent and defines the mainline.
	Parents []string
}

// RevListOptions selects a commit range.
type RevListOptions struct {
	// Include lists the revisions to walk from, such as branch tips.
	Include []string
	// Exclude lists the revisions whose ancestors are left out, which is how a
	// walk is bounded below by the recorded anchor.
	Exclude []string
	// FirstParent follows only the first parent of every merge, which yields the
	// mainline of a branch.
	FirstParent bool
	// MaxCount bounds the number of commits returned. Zero means no bound.
	MaxCount int
}

// CommitGraph lists commits with their parents in topological order, parents
// before children.
//
// The order is git's own reverse topological order rather than a date order,
// because commit dates in an imported history are attacker and rebase
// controlled and would make the replay sequence unstable.
func (r *Runner) CommitGraph(ctx context.Context, opts RevListOptions) ([]DAGCommit, error) {
	if len(opts.Include) == 0 {
		return nil, errors.New("git rev-list: at least one revision is required")
	}
	for _, revision := range opts.Include {
		if err := validateRevision(revision); err != nil {
			return nil, fmt.Errorf("git rev-list: %w", err)
		}
	}
	for _, revision := range opts.Exclude {
		if err := validateRevision(revision); err != nil {
			return nil, fmt.Errorf("git rev-list: %w", err)
		}
	}
	if opts.MaxCount < 0 {
		return nil, fmt.Errorf("git rev-list: max count %d must not be negative", opts.MaxCount)
	}

	args := []string{"rev-list", "--topo-order", "--reverse", "--parents"}
	if opts.FirstParent {
		args = append(args, "--first-parent")
	}
	if opts.MaxCount > 0 {
		args = append(args, "--max-count="+strconv.Itoa(opts.MaxCount))
	}
	args = append(args, "--end-of-options")
	args = append(args, opts.Include...)
	for _, revision := range opts.Exclude {
		args = append(args, "^"+revision)
	}
	// The trailing separator keeps a revision that happens to match a file name
	// from being read as a path.
	args = append(args, "--")

	out, err := r.run(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("git rev-list: %w", err)
	}

	var commits []DAGCommit
	for line := range strings.SplitSeq(out, "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		commits = append(commits, DAGCommit{SHA: fields[0], Parents: fields[1:]})
	}
	return commits, nil
}

// CommitParents reports the parent object names of one commit in git's order.
func (r *Runner) CommitParents(ctx context.Context, revision string) ([]string, error) {
	if err := validateRevision(revision); err != nil {
		return nil, fmt.Errorf("git commit parents: %w", err)
	}
	out, err := r.run(ctx, "rev-list", "--max-count=1", "--parents", "--end-of-options", revision, "--")
	if err != nil {
		return nil, fmt.Errorf("git commit parents for %q: %w", r.redactor.String(revision), err)
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return nil, fmt.Errorf("git commit parents for %q: empty output", r.redactor.String(revision))
	}
	return fields[1:], nil
}

// ResolveTree resolves the tree object of one commit, which is the cheapest way
// to tell whether two commits produce identical content.
func (r *Runner) ResolveTree(ctx context.Context, revision string) (string, error) {
	if err := validateRevision(revision); err != nil {
		return "", fmt.Errorf("git commit tree: %w", err)
	}
	out, err := r.run(ctx, "rev-parse", "--verify", "--end-of-options", revision+"^{tree}")
	if err != nil {
		return "", fmt.Errorf("git commit tree for %q: %w", r.redactor.String(revision), err)
	}
	return strings.TrimSpace(out), nil
}

// Empty tree object names. Git knows both without them being written to any
// object store, and which one applies depends on the repository's hash
// algorithm rather than on anything the caller can choose.
const (
	emptyTreeSHA1   = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
	emptyTreeSHA256 = "6ef19b41225c5369f1c104d45d8d85efa9b057b53b14b4b9b939dd74decc5321"
)

// EmptyTree reports the empty tree object name for this repository's hash
// algorithm, which is what a comparison "against nothing" needs to name.
func (r *Runner) EmptyTree(ctx context.Context) (string, error) {
	format, err := r.ObjectFormat(ctx)
	if err != nil {
		return "", err
	}
	switch format {
	case ObjectFormatSHA1:
		return emptyTreeSHA1, nil
	case ObjectFormatSHA256:
		return emptyTreeSHA256, nil
	default:
		return "", fmt.Errorf("git object format: unsupported format %q", string(format))
	}
}

// ChangedPaths lists the repository relative paths that differ between two
// revisions. An empty from compares against the empty tree, so the result is
// every path the revision's tree contains, which is what a root commit
// introduced.
//
// The empty tree is named explicitly rather than requested with --root. With
// --root git compares a root commit against nothing but compares every other
// commit against its parent, and for a merge it emits nothing at all, so an
// empty from would silently mean three different things depending on the shape
// of the commit it was handed.
//
// Merges still have no single answer when a parent is meant, so the caller
// passes the parent it means to compare against rather than relying on a
// default.
func (r *Runner) ChangedPaths(ctx context.Context, from, to string) ([]string, error) {
	if err := validateRevision(to); err != nil {
		return nil, fmt.Errorf("git changed paths: %w", err)
	}
	if from == "" {
		empty, err := r.EmptyTree(ctx)
		if err != nil {
			return nil, fmt.Errorf("git changed paths: %w", err)
		}
		from = empty
	} else if err := validateRevision(from); err != nil {
		return nil, fmt.Errorf("git changed paths: %w", err)
	}

	args := []string{"diff-tree", "--no-commit-id", "--name-only", "-r", "-z", "--end-of-options", from, to, "--"}
	out, err := r.run(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("git changed paths for %q: %w", r.redactor.String(to), err)
	}
	return splitNull(out), nil
}

// MergeBase reports the best common ancestor of two revisions, and fails when
// there is more than one, as a criss-cross merge produces.
func (r *Runner) MergeBase(ctx context.Context, a, b string) (string, error) {
	bases, err := r.MergeBases(ctx, a, b)
	if err != nil {
		return "", err
	}
	return soleMergeBase(bases)
}

// MergeBases reports every best common ancestor of two revisions, sorted by
// object name so a caller never depends on git's traversal order.
func (r *Runner) MergeBases(ctx context.Context, a, b string) ([]string, error) {
	return r.mergeBases(ctx, []string{"--all"}, a, b)
}

// MergeBasesOctopus reports every best common ancestor of every revision, which
// is how the immutable transformed anchor is derived from the initially tracked
// refs.
//
// The plural matters. Without --all git prints one of the best common ancestors
// and gives no sign that it chose, so a criss-cross history would silently
// anchor published history to whichever base the traversal reached first.
func (r *Runner) MergeBasesOctopus(ctx context.Context, revisions ...string) ([]string, error) {
	if len(revisions) < 2 {
		return nil, errors.New("git merge-base: at least two revisions are required")
	}
	return r.mergeBases(ctx, []string{"--octopus", "--all"}, revisions...)
}

// soleMergeBase reduces a merge base set to the single answer a caller asked
// for, or reports the ambiguity rather than choosing.
func soleMergeBase(bases []string) (string, error) {
	switch len(bases) {
	case 0:
		return "", fmt.Errorf("git merge-base: %w", ErrNoMergeBase)
	case 1:
		return bases[0], nil
	default:
		return "", fmt.Errorf("git merge-base: %s: %w", strings.Join(bases, ", "), ErrAmbiguousMergeBase)
	}
}

// mergeBases runs one merge-base query and turns git's "no common ancestor"
// verdict into a sentinel error.
func (r *Runner) mergeBases(ctx context.Context, flags []string, revisions ...string) ([]string, error) {
	for _, revision := range revisions {
		if err := validateRevision(revision); err != nil {
			return nil, fmt.Errorf("git merge-base: %w", err)
		}
	}
	args := append([]string{"merge-base"}, flags...)
	args = append(args, "--end-of-options")
	args = append(args, revisions...)

	out, err := r.run(ctx, args...)
	if err != nil {
		if ExitCodeOf(err) == 1 {
			return nil, fmt.Errorf("git merge-base: %w", ErrNoMergeBase)
		}
		return nil, fmt.Errorf("git merge-base: %w", err)
	}

	var bases []string
	for line := range strings.SplitSeq(out, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			bases = append(bases, trimmed)
		}
	}
	if len(bases) == 0 {
		return nil, fmt.Errorf("git merge-base: %w", ErrNoMergeBase)
	}
	slices.Sort(bases)
	return slices.Compact(bases), nil
}

// IsAncestor reports whether ancestor is reachable from descendant. A commit is
// its own ancestor, matching git's own definition.
func (r *Runner) IsAncestor(ctx context.Context, ancestor, descendant string) (bool, error) {
	if err := validateRevision(ancestor); err != nil {
		return false, fmt.Errorf("git ancestor probe: %w", err)
	}
	if err := validateRevision(descendant); err != nil {
		return false, fmt.Errorf("git ancestor probe: %w", err)
	}
	_, err := r.run(ctx, "merge-base", "--is-ancestor", "--end-of-options", ancestor, descendant)
	if err == nil {
		return true, nil
	}
	if ExitCodeOf(err) == 1 {
		return false, nil
	}
	return false, fmt.Errorf("git ancestor probe: %w", err)
}

// ObjectInfo is the type and size of one Git object.
type ObjectInfo struct {
	// Name is the object name as git resolved it, or the requested revision when
	// the object could not be resolved.
	Name string
	// Type is the object type, empty when the object is missing.
	Type string
	// Size is the object size in bytes, zero when the object is missing.
	Size int64
	// Missing reports an object that is not present.
	Missing bool
}

// ObjectInfoOptions configures a batched object probe.
type ObjectInfoOptions struct {
	// Revisions are the objects to describe.
	Revisions []string
	// AllowLazyFetch permits a partial clone to download the objects it is asked
	// about. Leaving it false answers from the local object store only, which is
	// what a "do I already have this" probe means. Setting it true is how a run
	// deliberately prewarms blobs in one batch instead of one fetch per file.
	AllowLazyFetch bool
}

// assertLazyFetchAllowed gates the promisor transfer a lazy fetch performs.
//
// A lazy fetch is a fetch. It reaches the promisor remote, which for the source
// cache is the public upstream, so it carries exactly what CloneSource and
// FetchSource are gated against: a credential must never travel to that host,
// and the repository's own configuration must never decide where the objects
// come from. The only difference is that git performs it implicitly, in the
// middle of what reads like a local lookup, which is why the gate belongs at
// every call that permits one.
func (r *Runner) assertLazyFetchAllowed(ctx context.Context) error {
	// A runner pinned by WithNoLazyFetch has already answered this question. The
	// request is refused rather than silently downgraded, because a caller that
	// asked for the network and got a local answer would read the resulting
	// "missing object" as a fact about the repository.
	if r.noLazyFetch {
		return ErrLazyFetchDisabled
	}
	if err := r.assertAnonymous(); err != nil {
		return err
	}
	return r.assertNoRemoteRewrites(ctx)
}

// ObjectInfoBatch describes many objects in one subprocess. Batching matters for
// a blobless clone, where the alternative is one round trip per object.
func (r *Runner) ObjectInfoBatch(ctx context.Context, opts ObjectInfoOptions) ([]ObjectInfo, error) {
	if len(opts.Revisions) == 0 {
		return nil, nil
	}
	var input strings.Builder
	for _, revision := range opts.Revisions {
		if err := validateRevision(revision); err != nil {
			return nil, fmt.Errorf("git object info: %w", err)
		}
		// The batch protocol is one revision per line and reports an unknown
		// object by echoing the request, so a revision carrying whitespace would
		// desynchronise the request and response streams.
		if strings.ContainsAny(revision, " \t\n\r") {
			return nil, fmt.Errorf("git object info: revision %q must not contain whitespace", revision)
		}
		input.WriteString(revision)
		input.WriteString("\n")
	}

	var env []string
	args := []string{"cat-file", "--batch-check", "--buffer"}
	if opts.AllowLazyFetch {
		if err := r.assertLazyFetchAllowed(ctx); err != nil {
			return nil, fmt.Errorf("git object info: %w", err)
		}
		// The empty credential helper resets the helper list, so a repository
		// local helper can neither be consulted nor prompt during the transfer.
		args = append(slices.Clone(anonymousConfig), args...)
	} else {
		env = []string{noLazyFetch}
	}
	out, err := r.runInput(ctx, []byte(input.String()), env, args...)
	if err != nil {
		return nil, fmt.Errorf("git object info: %w", err)
	}

	infos := make([]ObjectInfo, 0, len(opts.Revisions))
	for line := range strings.SplitSeq(strings.TrimSuffix(out, "\n"), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		switch {
		case len(fields) >= 2 && fields[len(fields)-1] == "missing":
			infos = append(infos, ObjectInfo{Name: fields[0], Missing: true})
		case len(fields) >= 2 && fields[len(fields)-1] == "ambiguous":
			// Git found more than one object the request could mean and declined
			// to choose. Reading that as absent would be the dangerous answer: a
			// caller deciding what it still has to write would write under a name
			// that already means two things, and the one it then reads back need
			// not be the one it wrote.
			return nil, fmt.Errorf("git object info: %q: %w", fields[0], ErrObjectAmbiguous)
		case len(fields) == 3:
			size, err := strconv.ParseInt(fields[2], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("git object info: size %q: %w", fields[2], err)
			}
			infos = append(infos, ObjectInfo{Name: fields[0], Type: fields[1], Size: size})
		default:
			return nil, fmt.Errorf("git object info: unexpected output %q", line)
		}
	}
	if len(infos) != len(opts.Revisions) {
		return nil, fmt.Errorf("git object info: got %d records, want %d", len(infos), len(opts.Revisions))
	}
	return infos, nil
}

// validateRevision rejects a revision that git would parse as an option or that
// carries a range or negation the caller did not ask for.
func validateRevision(revision string) error {
	if err := validateArgument("revision", revision); err != nil {
		return err
	}
	if strings.HasPrefix(revision, "^") {
		return fmt.Errorf("revision %q must not be negated, use the exclude list", revision)
	}
	if strings.Contains(revision, "..") {
		return fmt.Errorf("revision %q must name one commit, not a range", revision)
	}
	return nil
}

// splitNull splits null terminated output into non-empty records.
func splitNull(out string) []string {
	var records []string
	for record := range strings.SplitSeq(out, "\x00") {
		if record != "" {
			records = append(records, record)
		}
	}
	return records
}
