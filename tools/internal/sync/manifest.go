package sync

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/enj/soapbox/tools/internal/buildinfo"
	"github.com/enj/soapbox/tools/internal/generate"
	"github.com/enj/soapbox/tools/internal/publish"
)

// ManifestSchema is the version of the manifest shape. An approval names a
// hash, and a hash of a shape the reader does not know is not an approval of
// anything it can check.
const ManifestSchema = 1

// Manifest is everything one synchronization would do, rendered to be compared.
//
// It is the artifact the outward action gate approves, so it is built for
// comparison rather than for reading once: field order is fixed, every list is
// sorted and non-nil, and nothing in it depends on where the run happened. Two
// runs that would publish the same thing produce the same bytes and the same
// hash, on any machine, from any directory.
//
// It carries no path, no URL other than the canonical destination, no
// credential, and no environment value. That is not tidiness. The manifest is
// attached to a review, pasted into an approval, and kept as the record of what
// was authorized, and a temporary directory from the machine that produced it
// would break the comparison and leak into the record.
type Manifest struct {
	// Schema is the manifest version, which must equal ManifestSchema.
	Schema int `json:"schema"`
	// Engine identifies what produced the synchronization.
	Engine EngineSummary `json:"engine"`
	// Source is the upstream release being published.
	Source SourceSummary `json:"source"`
	// Module summarizes what the generation decided.
	Module ModuleSummary `json:"module"`
	// Objects are the destination objects this run wrote. None of them is named
	// by a ref until the publication runs.
	Objects ObjectSummary `json:"objects"`
	// Publish is the exact outward action set, carrying its own hash.
	Publish publish.Manifest `json:"publish"`
	// Hash digests every other field, its own value cleared. It is what an
	// approval names, and it covers the publication manifest as well, so
	// approving a synchronization approves both what was built and where it
	// would go.
	Hash string `json:"hash"`
}

// EngineSummary identifies the engine and the profile a run generated under.
type EngineSummary struct {
	// Version is the engine build version.
	Version string `json:"version"`
	// Toolchain is the Go toolchain the profile pins for deterministic
	// formatting.
	Toolchain string `json:"toolchain"`
	// ProfileHash is the digest of the output affecting subset of the profile.
	// A change to it re-derives every transformed commit, so it is the field
	// that says whether two synchronizations are comparable at all.
	ProfileHash string `json:"profileHash"`
}

// SourceSummary is the upstream release being published.
//
// The source remote is absent, including when it was overridden. Its value is
// frequently a path on the machine that ran the generation, and this manifest is
// compared byte for byte between two runs over different layouts.
type SourceSummary struct {
	// Tag is the upstream release tag, and Ref the fully qualified source ref it
	// was proved against.
	Tag string `json:"tag"`
	Ref string `json:"ref"`
	// Commit is the exact upstream commit the release was cut from.
	Commit string `json:"commit"`
	// ReleaseTag is the destination tag the release policy maps the upstream tag
	// onto.
	ReleaseTag string `json:"releaseTag"`
}

// ModuleSummary is what the generation decided, in the terms a reviewer of an
// outward action reads.
//
// It is a selection rather than the whole generation report. The report answers
// "is this module correct", runs to thousands of lines, and is written beside
// the manifest for anyone who wants it. The manifest answers "should this be
// published", and the fields here are the ones that change that answer: what
// the module is, what pruning removed, what the public surface became, what
// behaves differently from upstream, and what the dependency policy decided.
type ModuleSummary struct {
	// Module is the destination module path.
	Module string `json:"module"`
	// ManifestHash digests the complete generated tree: every destination path,
	// its mode, and its content. Two synchronizations that agree on it published
	// the same module.
	ManifestHash string `json:"manifestHash"`
	// Files and Packages are what the tree holds.
	Files    int `json:"files"`
	Packages int `json:"packages"`
	// PrunedFiles and DeniedImports are what the extraction asserted, sorted.
	PrunedFiles   []string `json:"prunedFiles"`
	DeniedImports []string `json:"deniedImports"`
	// PublicAPI are the names the module publishes, sorted. A release that
	// changes them is the one a consumer notices.
	PublicAPI []string `json:"publicApi"`
	// BehaviorChanges are the documented differences from upstream, sorted.
	BehaviorChanges []BehaviorSummary `json:"behaviorChanges"`
	// Dependencies is what the dependency policy decided.
	Dependencies DependencySummary `json:"dependencies"`
	// Notices are the generation's advisory findings, sorted. They did not stop
	// the run, which is exactly why a person approving one should see them.
	Notices []string `json:"notices"`
}

// BehaviorSummary is one documented difference from upstream.
type BehaviorSummary struct {
	Summary string `json:"summary"`
	Cause   string `json:"cause"`
}

// DependencySummary is what the dependency policy decided.
type DependencySummary struct {
	// Policy is the configured default action.
	Policy string `json:"policy"`
	// Copy lists the staging paths the decision approves, sorted. For a profile
	// whose answer is external it is empty, and that emptiness is the decision.
	Copy []string `json:"copy"`
	// Candidates, Copied, and Refused summarize the decision.
	Candidates int `json:"candidates"`
	Copied     int `json:"copied"`
	Refused    int `json:"refused"`
}

// ObjectSummary names every destination object one synchronization wrote.
//
// Nothing here is reachable. The objects exist in the destination repository
// and no ref names them, which is what makes a plan free to throw away: the
// cost of an unpublished synchronization is disk.
type ObjectSummary struct {
	// Format is the hash algorithm every name is written in.
	Format string `json:"format"`
	// Tree is the generated module's tree.
	Tree string `json:"tree"`
	// Commit is the replayed destination commit for the release.
	Commit string `json:"commit"`
	// Tag is the annotated tag object, and TagTarget the commit it names. They
	// differ from Commit only when the release needed a projection commit of its
	// own, and ProjectionCommit is that commit when it was written.
	Tag              string `json:"tag"`
	TagTarget        string `json:"tagTarget"`
	ProjectionCommit string `json:"projectionCommit"`
	// State names the objects the resumable record was stored as, and
	// StateDigest is the record's own digest.
	StateBlob   string `json:"stateBlob"`
	StateTree   string `json:"stateTree"`
	StateCommit string `json:"stateCommit"`
	StateDigest string `json:"stateDigest"`
}

// manifest renders what this run produced.
func (r *run) manifest() (Manifest, error) {
	report := r.opts.Module.Report
	m := Manifest{
		Schema: ManifestSchema,
		Engine: EngineSummary{
			Version:     buildinfo.Version,
			Toolchain:   report.Engine.Toolchain,
			ProfileHash: report.Engine.ProfileHash,
		},
		Source: SourceSummary{
			Tag:        r.opts.Release.Tag,
			Ref:        r.opts.Release.Ref,
			Commit:     r.opts.Release.Commit,
			ReleaseTag: r.result.Release.Tag,
		},
		Module:  moduleSummary(report),
		Objects: r.objectSummary(),
		// The publication manifest is copied down to its action slice. A shallow
		// copy would leave both manifests sharing one array, so a caller that
		// edited or reordered the plan's actions would silently change what the
		// synchronization hashed over, and the approval would then name a set of
		// refs nobody rendered.
		Publish: clonePublishManifest(r.result.Publish.Manifest),
	}
	if err := checkManifestStrings(m); err != nil {
		return Manifest{}, err
	}
	hash, err := m.computeHash()
	if err != nil {
		return Manifest{}, err
	}
	m.Hash = hash
	return m, nil
}

// clonePublishManifest copies a publication manifest and the actions it holds.
func clonePublishManifest(m publish.Manifest) publish.Manifest {
	m.Actions = slices.Clone(m.Actions)
	if m.Actions == nil {
		m.Actions = []publish.Action{}
	}
	return m
}

// objectSummary names what this run wrote.
func (r *run) objectSummary() ObjectSummary {
	return ObjectSummary{
		Format:           string(r.format),
		Tree:             r.result.Tree.Tree,
		Commit:           r.result.Replay.Heads[0].Destination,
		Tag:              r.result.Release.Object,
		TagTarget:        r.result.Release.Target,
		ProjectionCommit: r.result.Release.Commit,
		StateBlob:        r.result.State.Blob,
		StateTree:        r.result.State.Tree,
		StateCommit:      r.result.State.Commit,
		StateDigest:      r.result.State.Digest,
	}
}

// moduleSummary selects the generation facts an approval turns on.
//
// Every list is copied and sorted here rather than trusted to arrive that way.
// The report already sorts them, but the manifest's whole value is that two
// runs produce identical bytes, and a list whose order came from somewhere else
// would make that property depend on a promise made in another package.
func moduleSummary(report generate.Report) ModuleSummary {
	changes := make([]BehaviorSummary, 0, len(report.Provenance.BehaviorChanges))
	for _, change := range report.Provenance.BehaviorChanges {
		changes = append(changes, BehaviorSummary{Summary: change.Summary, Cause: change.Cause})
	}
	slices.SortFunc(changes, func(a, b BehaviorSummary) int {
		if cmp := strings.Compare(a.Summary, b.Summary); cmp != 0 {
			return cmp
		}
		return strings.Compare(a.Cause, b.Cause)
	})
	return ModuleSummary{
		Module:          report.Output.Module,
		ManifestHash:    report.Output.ManifestHash,
		Files:           report.Output.Files,
		Packages:        report.Output.Packages,
		PrunedFiles:     sorted(report.Extract.Post.PrunedFiles),
		DeniedImports:   sorted(report.Extract.Post.DeniedImports),
		PublicAPI:       sorted(report.Provenance.PublicAPI),
		BehaviorChanges: changes,
		Dependencies: DependencySummary{
			Policy:     report.Dependencies.Policy,
			Copy:       sorted(report.Dependencies.Copy),
			Candidates: report.Dependencies.Totals.Candidates,
			Copied:     report.Dependencies.Totals.Copied,
			Refused:    report.Dependencies.Totals.Refused,
		},
		Notices: sorted(report.Notices),
	}
}

// sorted returns a sorted copy that is never nil, so an empty list encodes as
// [] rather than as null and two runs cannot differ over whether something
// happened to be absent.
func sorted(values []string) []string {
	out := slices.Clone(values)
	if out == nil {
		out = []string{}
	}
	slices.Sort(out)
	return out
}

// JSON renders the manifest as deterministic, indented bytes with a trailing
// newline.
func (m Manifest) JSON() ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	// HTML escaping would turn characters in a ref name, a module path, or a
	// notice into escapes and make the bytes depend on the encoder rather than
	// on the synchronization.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(m); err != nil {
		return nil, fmt.Errorf("encode synchronization manifest: %w", err)
	}
	return buf.Bytes(), nil
}

// computeHash digests the manifest with its own hash field cleared.
//
// Clearing rather than omitting keeps one renderer for both purposes: the bytes
// that are hashed are the bytes JSON produces for a manifest that has not been
// hashed yet, so there is no second serialization that could drift from the
// first and make a manifest disagree with its own digest.
func (m Manifest) computeHash() (string, error) {
	unhashed := m
	unhashed.Hash = ""
	encoded, err := unhashed.JSON()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// Verify recomputes the hash and reports a manifest that was modified after it
// was rendered.
//
// The publication manifest is verified as well, because it carries a hash of
// its own and a synchronization manifest whose publication half was replaced
// wholesale would otherwise hash consistently while describing refs nobody
// planned.
func (m Manifest) Verify() error {
	if m.Hash == "" {
		return fmt.Errorf("%w: it carries no hash", ErrManifestModified)
	}
	if err := m.Publish.Verify(); err != nil {
		return err
	}
	computed, err := m.computeHash()
	if err != nil {
		return err
	}
	if computed != m.Hash {
		return fmt.Errorf("%w: it carries %s and hashes to %s", ErrManifestModified, m.Hash, computed)
	}
	return nil
}

// Text renders the manifest for a person, deterministically.
//
// It states the same facts in the same order as the JSON form, so an operator
// reading the text and a gate comparing the hash are looking at one artifact
// rather than two renderings that could disagree.
func (m Manifest) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "synchronization of %s %s\n", m.Source.Ref, m.Source.Commit)
	fmt.Fprintf(&b, "  engine       %s toolchain %s\n", m.Engine.Version, m.Engine.Toolchain)
	fmt.Fprintf(&b, "  profile      %s\n", m.Engine.ProfileHash)
	fmt.Fprintf(&b, "  module       %s release %s\n", m.Module.Module, m.Source.ReleaseTag)
	fmt.Fprintf(&b, "  tree         %s (%d generated files, %d generated packages)\n",
		m.Objects.Tree, m.Module.Files, m.Module.Packages)
	fmt.Fprintf(&b, "  commit       %s\n", m.Objects.Commit)
	fmt.Fprintf(&b, "  tag          %s -> %s\n", m.Objects.Tag, m.Objects.TagTarget)
	fmt.Fprintf(&b, "  state        %s digest %s\n", m.Objects.StateCommit, m.Objects.StateDigest)
	for _, notice := range m.Module.Notices {
		fmt.Fprintf(&b, "  notice       %s\n", notice)
	}
	b.WriteString(m.Publish.Text())
	fmt.Fprintf(&b, "  approve with %s\n", m.Hash)
	return b.String()
}
