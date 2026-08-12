package extract

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/enj/soapbox/tools/internal/closure"
)

// ReportSchema is the version of the report shape.
//
// It is the first field of every report so a consumer can refuse a shape it does
// not understand before parsing the rest. It changes only when a field is
// removed or its meaning changes; adding a field does not.
const ReportSchema = 1

// Report is the deterministic record of one plan.
//
// It carries no absolute path, no environment value, and no secret. That is not
// tidiness: the report is compared byte for byte between two runs over different
// directory layouts, it is attached to CI artifacts, and it is the evidence a
// reviewer reads before approving an outward action. A path from the machine
// that produced it would break the first use and leak into the second.
//
// Every list is sorted and non-nil, so the encoding depends on the plan alone
// and never on map iteration order or on whether a list happened to be empty.
type Report struct {
	Schema     int              `json:"schema"`
	Engine     EngineReport     `json:"engine"`
	Source     SourceReport     `json:"source"`
	Worktree   WorktreeReport   `json:"worktree"`
	Patches    PatchesReport    `json:"patches"`
	Closure    ClosureReport    `json:"closure"`
	Relocation RelocationReport `json:"relocation"`
	Rewrite    RewriteReport    `json:"rewrite"`
	Output     OutputReport     `json:"output"`
	// Failure records why the plan refused, nil when it did not. A report is
	// produced for a policy failure precisely so the refusal is reviewable
	// without rerunning the pipeline, and a refusal with no machine readable
	// record would leave CI parsing a stderr line.
	Failure *FailureReport `json:"failure"`
	// Notices are advisory findings, sorted. They never stop a plan on their
	// own; -strict is what turns them into a policy failure.
	Notices []string `json:"notices"`
}

// FailureReport records one refused plan.
//
// It is written for a policy failure and for the two runtime failures that
// still leave a reviewable tree, a cache that moved under the run and an output
// or report write that did not complete. A runtime failure that stopped the run
// before it measured anything produces no report at all, because there would be
// nothing in it but the message already on stderr.
type FailureReport struct {
	// Stage names the phase that refused, matching PolicyError.Stage.
	Stage string `json:"stage"`
	// Message is the rendered failure. It carries no absolute path, because the
	// stages that can name one render the repository relative path instead.
	Message string `json:"message"`
	// Patch is the structured conflict record, nil unless a patch failed.
	Patch *PatchFailure `json:"patch"`
}

// PatchFailure is the reviewable half of a patch conflict.
//
// It repeats what *patchset.ConflictError carries because the error itself
// reaches only stderr, and reproducing a conflict from a one line message means
// rerunning the whole pipeline. Nothing here is a path on this machine: the
// paths are repository relative and the diff is upstream content.
type PatchFailure struct {
	// SourceRef and SourceSHA identify the abandoned ref transaction.
	SourceRef string `json:"sourceRef"`
	SourceSHA string `json:"sourceSHA"`
	// PatchID, PatchIndex, and PatchCount locate the failure in the series.
	// PatchIndex is zero based, matching the error it is taken from.
	PatchID    string `json:"patchID"`
	PatchIndex int    `json:"patchIndex"`
	PatchCount int    `json:"patchCount"`
	// Stage names the step that failed: apply, prune, or cancel.
	Stage string `json:"stage"`
	// ConflictedPaths lists the unmerged repository relative paths, sorted.
	ConflictedPaths []string `json:"conflictedPaths"`
	// Status is the porcelain status captured before the rollback, rendered one
	// entry per line in the order Git reported them.
	Status []string `json:"status"`
	// Diff is the work tree diff captured before the rollback. For a three way
	// apply it holds the conflict markers, which is what a maintainer edits the
	// patch against.
	Diff string `json:"diff"`
}

// EngineReport identifies what produced the plan.
type EngineReport struct {
	// Version is the engine version.
	Version string `json:"version"`
	// Toolchain is the Go toolchain the profile pins for deterministic
	// formatting.
	Toolchain string `json:"toolchain"`
	// ProfileHash is the digest of the output affecting subset of the profile,
	// which is what a later phase compares to decide whether a control plane
	// change started a new epoch. Operational settings are deliberately absent
	// from it, so changing a cache location or a limit does not move it.
	ProfileHash string `json:"profileHash"`
}

// SourceReport records the upstream commit the plan covers and how it was
// obtained.
//
// The remote is absent on purpose. It is already covered by the profile hash,
// and an override may name a local mirror, which would put an absolute path into
// a report that two runs have to agree on byte for byte.
type SourceReport struct {
	// RefKind and RefName are the selected ref.
	RefKind string `json:"refKind"`
	RefName string `json:"refName"`
	// Ref is the fully qualified ref name.
	Ref string `json:"ref"`
	// Object is what the ref points at, which is the tag object itself for an
	// annotated tag.
	Object string `json:"object"`
	// Commit is the commit the ref resolves to, with annotated tags peeled.
	Commit string `json:"commit"`
	// Annotated reports a tag object rather than a direct commit reference.
	Annotated bool `json:"annotated"`
	// AnchorCommit is the recorded transformed anchor, empty when the profile
	// has not resolved one yet.
	AnchorCommit string `json:"anchorCommit"`
	// AnchorVerified reports that the selected commit descends from the anchor.
	// It is false when no anchor is configured, because nothing was verified.
	AnchorVerified bool `json:"anchorVerified"`
	// Fetched reports that this run updated the cache from the remote.
	Fetched bool `json:"fetched"`
	// CacheCreated reports that this run cloned the cache rather than reusing
	// one.
	CacheCreated bool `json:"cacheCreated"`
	// Offline reports that the run refused every network operation.
	Offline bool `json:"offline"`
	// RemoteOverridden reports that the run read history from a remote other
	// than the profile's, which is how a test or an air-gapped operator points
	// at a local mirror.
	//
	// Only the fact is recorded. The override's value is frequently a path on
	// the machine that ran the plan, and this report is compared byte for byte
	// between two runs over different layouts, so naming it would break the
	// comparison the determinism check depends on. The fact still has to be
	// here: a report produced against a mirror describes whatever that mirror
	// held, and a reviewer must be able to see that before trusting it.
	RemoteOverridden bool `json:"remoteOverridden"`
}

// WorktreeReport records how the source tree was materialized.
type WorktreeReport struct {
	// SparsePatterns is the final pattern set, in the order git applies it.
	SparsePatterns []string `json:"sparsePatterns"`
	// WidenRounds is how many times the closure discovered a package the
	// pattern set did not materialize.
	WidenRounds int `json:"widenRounds"`
	// WidenedPackages are the repository relative directories widening added,
	// sorted. They materialize files; they never seed the closure, so they
	// cannot change which packages the closure contains.
	WidenedPackages []string `json:"widenedPackages"`
	// ScratchAnchor is the local commit the patch phase applies against.
	ScratchAnchor AnchorReport `json:"scratchAnchor"`
	// CacheRefsMoved reports whether any ref in the shared cache changed while
	// the plan ran.
	//
	// A plan makes exactly one commit and makes it on a detached HEAD in a
	// linked work tree, so the value is false for every run that behaved. It is
	// carried in the report rather than merely asserted because the cache is
	// reused across runs and is what later phases publish from: a run that moved
	// a ref has to leave a record a reviewer can find, which means the report
	// has to be produced for that failure rather than suppressed by it.
	CacheRefsMoved bool `json:"cacheRefsMoved"`
}

// AnchorReport records the scratch commit made in the detached work tree.
//
// The commit is unreachable from every ref and is never pushed. It exists
// because three way patch application resolves blobs through the index and the
// object store, and its rollback restores exactly one committed state, so the
// pruned tree has to be a commit before a patch may touch it. Its object name is
// reported because it is deterministic: a fixed identity, a fixed date, a fixed
// message, and the pruned tree of a known parent leave nothing to vary.
type AnchorReport struct {
	// Commit is the scratch commit's object name.
	Commit string `json:"commit"`
	// Tree is the pruned tree it records.
	Tree string `json:"tree"`
	// Parent is the upstream commit it was made on top of.
	Parent string `json:"parent"`
	// StagedDeletions are the pruned paths staged into it, sorted.
	StagedDeletions []string `json:"stagedDeletions"`
}

// PatchesReport records patch selection and application.
type PatchesReport struct {
	// Branch is the tracked branch the branch selectors were matched against,
	// empty for a profile with no patches.
	Branch string `json:"branch"`
	// Available is how many patches the profile carries.
	Available int `json:"available"`
	// Selected are the identifiers the selectors chose, in application order.
	Selected []string `json:"selected"`
	// Applied are the identifiers that applied cleanly, in application order.
	Applied []string `json:"applied"`
	// Reasserted is how many times pruning was reasserted, which is once per
	// applied patch.
	Reasserted int `json:"reasserted"`
	// Reassert names what each reassertion removed, in application order.
	//
	// The count alone cannot answer the question the reassertion exists to
	// answer. Pruning is reasserted after every patch precisely so a patch that
	// reintroduced a pruned file is caught, and when one does the maintainer's
	// first question is which patch, which the count does not carry.
	Reassert []PatchReassert `json:"reassert"`
}

// PatchReassert is one patch's reassertion of the profile's pruning.
type PatchReassert struct {
	// PatchID is the patch that had just applied.
	PatchID string `json:"patchID"`
	// Files are the repository relative paths this reassertion removed again,
	// sorted. It is empty for a patch that reintroduced nothing, which is the
	// normal case.
	Files []string `json:"files"`
}

// ClosureReport records the package closure the plan settled on.
type ClosureReport struct {
	// Rounds is how many closure builds the plan performed before the package
	// set was complete, so one for a profile whose roots already reach
	// everything.
	Rounds int `json:"rounds"`
	// Report is the closure's own exact and observed shape, including the
	// pre-prune baseline, the post-prune result, and the external and standard
	// boundary imports.
	Report closure.ClosureReport `json:"report"`
	// RemovedFiles are the profile's prune entries, sorted. Every one of them
	// was removed from the materialized tree before the scratch anchor was
	// written, so this is the configured prune set rather than a measurement of
	// one pass: a reasserting pass over an already pruned tree removes nothing,
	// and what each of those passes did is recorded per patch in Patches.
	RemovedFiles []string `json:"removedFiles"`
	// Golden is the comparison against the profile's checked-in closure record.
	Golden GoldenReport `json:"golden"`
}

// GoldenReport records the comparison against the closure golden a profile
// pins.
//
// A golden is how a maintainer states the closure they reviewed. Without the
// comparison the limits are the only gate, and limits notice a closure that
// grew past a number rather than one that changed shape underneath them: a
// package swapped for another of the same size passes every limit and is
// exactly the change a reviewer has to see.
type GoldenReport struct {
	// Path is the golden's profile relative path, empty when the profile pins
	// none. It is profile relative rather than absolute because this report is
	// compared between runs over different layouts.
	Path string `json:"path"`
	// Status is absent, match, or diff. It is empty when the profile pins no
	// golden, which is the one case that is not a finding at all.
	Status string `json:"status"`
	// Differences name the exact fields of the golden's exact shape that
	// disagree, sorted. It is empty unless Status is diff.
	Differences []string `json:"differences"`
}

// Golden comparison outcomes.
const (
	// GoldenAbsent reports a pinned golden that is not in the profile
	// directory. It is a notice rather than a failure, because the first run
	// that establishes a closure has nothing to compare against yet.
	GoldenAbsent = "absent"
	// GoldenMatch reports an exact shape identical to the golden's.
	GoldenMatch = "match"
	// GoldenDiff reports an exact shape that disagrees with the golden's.
	GoldenDiff = "diff"
)

// RelocationReport records the upstream to destination mapping.
type RelocationReport struct {
	// InternalPrefix is the module relative directory every upstream path is
	// preserved below.
	InternalPrefix string `json:"internalPrefix"`
	// Packages are the relocated packages, sorted by destination directory.
	Packages []RelocatedPackage `json:"packages"`
}

// RelocatedPackage is one package of the generated module.
type RelocatedPackage struct {
	// SourcePackage is the upstream package directory.
	SourcePackage string `json:"sourcePackage"`
	// Package is the destination package directory.
	Package string `json:"package"`
	// Files are the package's files, sorted by destination path.
	Files []RelocatedFile `json:"files"`
}

// RelocatedFile is one file of the generated module.
type RelocatedFile struct {
	// Source is the upstream repository relative path, empty for a file this
	// engine generated, such as a provenance record.
	Source string `json:"source"`
	// Destination is the module relative path.
	Destination string `json:"destination"`
	// Mode is the Git octal file mode.
	Mode string `json:"mode"`
	// Generated records that the upstream file carried a Code generated marker.
	Generated bool `json:"generated"`
	// SHA256 is the digest of the final bytes, after rewriting.
	SHA256 string `json:"sha256"`
}

// RewriteReport records what the syntax aware transformations did.
type RewriteReport struct {
	// Files are the files a transformation changed, sorted by path. A file
	// nothing changed is absent, because listing every unchanged file would
	// bury the ones a reviewer has to read.
	Files []RewrittenFile `json:"files"`
	// DirectiveRemovals are the removed generator and toolchain directive
	// lines, rendered and sorted. They are listed separately from the per-file
	// changes because a removed marker is the transformation most likely to
	// change behaviour, so it must be readable without scanning every file.
	DirectiveRemovals []string `json:"directiveRemovals"`
	// Embeds are the verified go:embed directives, sorted.
	Embeds []EmbedReport `json:"embeds"`
	// GoFiles is how many Go files the plan transformed and reparsed.
	GoFiles int `json:"goFiles"`
	// Unparsed are destination paths the pinned parser could not read, sorted.
	// It must always be empty.
	Unparsed []string `json:"unparsed"`
	// Unformatted are destination paths the pinned gofmt would reformat,
	// sorted. Relocating an import can move it within its group, so this is
	// where that shows up.
	Unformatted []string `json:"unformatted"`
}

// RewrittenFile is one transformed file.
type RewrittenFile struct {
	// Path is the destination module relative path.
	Path string `json:"path"`
	// NoticeInserted reports that the file received the modification notice.
	NoticeInserted bool `json:"noticeInserted"`
	// Changes are every recorded transformation, rendered and sorted.
	Changes []string `json:"changes"`
}

// EmbedReport is one verified go:embed directive.
type EmbedReport struct {
	// Path is the destination module relative path of the file holding it.
	Path string `json:"path"`
	// Line is the one based line the directive sits on.
	Line int `json:"line"`
	// Patterns are the patterns the directive names, in written order.
	Patterns []string `json:"patterns"`
	// Matches are the destination paths they resolve to, sorted.
	Matches []string `json:"matches"`
}

// OutputReport records the tree the plan produced.
type OutputReport struct {
	// Module is the destination module path.
	Module string `json:"module"`
	// Files is how many files the tree holds.
	Files int `json:"files"`
	// Packages is how many Go packages it holds.
	//
	// It counts the closure's packages rather than the tree's directories. A
	// package that carries embedded data or a matched asset in a subdirectory
	// relocates that file into a directory of its own, and counting directories
	// would report a module with more packages than any build of it has.
	Packages int `json:"packages"`
	// ProvenanceFiles are the generated per-package records, sorted.
	ProvenanceFiles []string `json:"provenanceFiles"`
	// ManifestHash digests the complete tree: every destination path, its mode,
	// and its content. Two plans that agree on it produced the same module.
	ManifestHash string `json:"manifestHash"`
	// Materialized reports that the tree was written to a disk. A plan computes
	// the same tree either way, so the hash above does not depend on it.
	Materialized bool `json:"materialized"`
}

// JSON renders the report as deterministic, indented bytes with a trailing
// newline, which is the form -report writes and -format json prints.
func (r Report) JSON() ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	// HTML escaping would turn an import path's ampersand into an escape and
	// make the bytes depend on the encoder rather than on the plan.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(r); err != nil {
		return nil, fmt.Errorf("encode plan report: %w", err)
	}
	return buf.Bytes(), nil
}

// manifestEntry is one line of the manifest the output hash covers.
type manifestEntry struct {
	path   string
	mode   string
	digest string
}

// manifestHash digests a complete relocated tree.
//
// Path, mode, and content digest all enter the hash, because a tree that differs
// in any one of them is a different module: a file that lost its executable bit
// stops working for consumers, and a file that moved is a different import path.
// Fields are separated by a null byte, which no path, mode, or hex digest can
// contain, so no two distinct trees can render to one manifest.
func manifestHash(entries []manifestEntry) string {
	sorted := slices.Clone(entries)
	slices.SortFunc(sorted, func(a, b manifestEntry) int { return strings.Compare(a.path, b.path) })

	digest := sha256.New()
	for _, entry := range sorted {
		fmt.Fprintf(digest, "%s\x00%s\x00%s\n", entry.path, entry.mode, entry.digest)
	}
	return "sha256:" + hex.EncodeToString(digest.Sum(nil))
}

// contentDigest renders the digest of one file's bytes.
func contentDigest(contents []byte) string {
	sum := sha256.Sum256(contents)
	return hex.EncodeToString(sum[:])
}

// Summary renders the plan for a person.
//
// It is the one rendering allowed to name absolute directories, because an
// operator who just ran the command needs to know where the work tree and the
// output went, and nothing compares this text byte for byte.
func (r *Result) Summary() string {
	var b strings.Builder
	report := r.Report

	fmt.Fprintf(&b, "soapbox plan for %s %s at %s\n", report.Source.RefKind, report.Source.RefName, short(report.Source.Commit))
	fmt.Fprintf(&b, "  engine        %s, toolchain %s\n", report.Engine.Version, report.Engine.Toolchain)
	fmt.Fprintf(&b, "  profile       %s\n", report.Engine.ProfileHash)

	fmt.Fprintf(&b, "  source        %s", describeAcquisition(report.Source))
	if report.Source.AnchorCommit != "" {
		fmt.Fprintf(&b, ", anchor %s %s", short(report.Source.AnchorCommit), verified(report.Source.AnchorVerified))
	}
	b.WriteString("\n")

	observed := report.Closure.Report.Observed
	fmt.Fprintf(&b, "  closure       %d packages and %d files pre-prune, %d and %d after, %d rounds\n",
		observed.PrePrune.Packages, observed.PrePrune.Files,
		observed.PostPrune.Packages, observed.PostPrune.Files, report.Closure.Rounds)
	fmt.Fprintf(&b, "  pruned        %d files, %d non-test lines removed\n",
		len(report.Closure.RemovedFiles), observed.Growth.NonTestLinesRemoved)
	if report.Closure.Golden.Path != "" {
		fmt.Fprintf(&b, "  golden        %s is %s\n", report.Closure.Golden.Path, report.Closure.Golden.Status)
	}
	fmt.Fprintf(&b, "  imports       %d external, %d standard\n",
		len(report.Closure.Report.Exact.ExternalPackages), len(report.Closure.Report.Exact.StandardPackages))

	if report.Worktree.WidenRounds > 0 {
		fmt.Fprintf(&b, "  widened       %s\n", strings.Join(report.Worktree.WidenedPackages, ", "))
	}
	fmt.Fprintf(&b, "  anchor        %s on %s, %d staged deletions, cache refs %s\n",
		short(report.Worktree.ScratchAnchor.Commit), short(report.Worktree.ScratchAnchor.Parent),
		len(report.Worktree.ScratchAnchor.StagedDeletions), moved(report.Worktree.CacheRefsMoved))
	fmt.Fprintf(&b, "  patches       %d available, %d selected, %d applied\n",
		report.Patches.Available, len(report.Patches.Selected), len(report.Patches.Applied))
	fmt.Fprintf(&b, "  rewrite       %d Go files, %d changed, %d directive removals, %d unformatted\n",
		report.Rewrite.GoFiles, len(report.Rewrite.Files),
		len(report.Rewrite.DirectiveRemovals), len(report.Rewrite.Unformatted))
	fmt.Fprintf(&b, "  output        %s with %d packages and %d files\n",
		report.Output.Module, report.Output.Packages, report.Output.Files)
	fmt.Fprintf(&b, "  manifest      %s\n", report.Output.ManifestHash)

	fmt.Fprintf(&b, "  cache         %s\n", r.Paths.Cache)
	if r.Paths.Worktree != "" {
		fmt.Fprintf(&b, "  work tree     %s\n", r.Paths.Worktree)
	}
	if report.Output.Materialized {
		fmt.Fprintf(&b, "  tree          %s\n", r.Paths.Output)
	} else {
		fmt.Fprintf(&b, "  tree          computed only, pass -materialize to write %s\n", r.Paths.Output)
	}

	if len(report.Notices) > 0 {
		fmt.Fprintf(&b, "\n%d notices:\n", len(report.Notices))
		for _, notice := range report.Notices {
			b.WriteString("  " + notice + "\n")
		}
	}
	if failure := report.Failure; failure != nil {
		fmt.Fprintf(&b, "\nrefused at the %s stage:\n  %s\n", failure.Stage, failure.Message)
		if patch := failure.Patch; patch != nil {
			fmt.Fprintf(&b, "  patch %s (%d of %d) at the %s stage\n",
				patch.PatchID, patch.PatchIndex+1, patch.PatchCount, patch.Stage)
			for _, conflicted := range patch.ConflictedPaths {
				b.WriteString("    conflicted " + conflicted + "\n")
			}
		}
		for _, difference := range report.Closure.Golden.Differences {
			b.WriteString("  golden " + difference + "\n")
		}
	}
	return b.String()
}

// describeAcquisition renders how the source commit reached the cache.
func describeAcquisition(source SourceReport) string {
	switch {
	case source.CacheCreated:
		return "cache cloned"
	case source.Offline:
		return "cache reused offline"
	case source.Fetched:
		return "cache fetched"
	default:
		return "cache reused"
	}
}

// verified renders an ancestry check outcome.
func verified(ok bool) string {
	if ok {
		return "verified"
	}
	return "NOT VERIFIED"
}

// moved renders whether the cache changed under the run.
func moved(ok bool) string {
	if ok {
		return "MOVED"
	}
	return "unchanged"
}

// short abbreviates an object name for a human summary, leaving a value that is
// not one alone.
func short(name string) string {
	const shortLength = 12
	if len(name) <= shortLength {
		return name
	}
	return name[:shortLength]
}
