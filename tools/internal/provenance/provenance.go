// Package provenance generates the root evidence files of an extracted module.
//
// The generated module is a derivative work. It carries somebody else's code,
// modified, under somebody else's licence, and it is published from a
// repository that has nothing to do with the project the code came from. Three
// obligations follow, and this package is where they are discharged at the root
// of the tree:
//
//   - The licence travels with the code. The upstream LICENSE is reproduced
//     byte for byte, because a licence that has been reflowed, retitled, or
//     partially quoted is no longer the licence that was granted.
//   - The attribution notices travel with it too. Apache License 2.0 section
//     4(d) requires the upstream NOTICE to be readable in the derivative work,
//     so it is embedded whole in a delimited section that says where it came
//     from and is never merged with anything Soapbox has to say.
//   - The modifications are stated prominently. Section 4(b) requires modified
//     files to carry notices saying they were changed; the rewriting step puts
//     one in every file it touches, and the root NOTICE states the same thing
//     for the module as a whole and enumerates the evidence: which packages
//     were extracted, from which commit, what was pruned, which patches were
//     applied, and which behaviour changed as a result.
//
// A fourth obligation is one the licence does not create and this package is
// careful not to violate anyway. Section 6 grants no trademark rights, so the
// generated files identify the upstream project only to say where the code came
// from, and state plainly that the module is neither a release of that project
// nor endorsed by it.
//
// Every byte is a deterministic function of the inputs. Nothing here reads a
// clock, names a directory on the machine that ran the generation, or accepts a
// credential: these files are committed, and a release that differed from the
// previous one by a timestamp would make every real change harder to see.
package provenance

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/enj/soapbox/tools/internal/relocate"
	"github.com/enj/soapbox/tools/internal/rewrite"
)

// Root file names. They are fixed rather than configurable because a consumer,
// a licence scanner, and a package index all look for exactly these.
const (
	// LicenseFileName holds the upstream licence, reproduced unchanged.
	LicenseFileName = "LICENSE"
	// NoticeFileName holds the upstream attribution notices and the record of
	// what Soapbox changed.
	NoticeFileName = "NOTICE"
	// ReadmeFileName is the repository's front page.
	ReadmeFileName = "README.md"
	// DocFileName holds the root package's documentation comment.
	DocFileName = "doc.go"
)

// Provenance sentinels. Callers use errors.Is to distinguish the failure.
var (
	// ErrOptions reports root provenance that cannot be rendered.
	ErrOptions = errors.New("root provenance options are not usable")
	// ErrSecret reports an input that would publish a credential, which a
	// committed file must never carry.
	ErrSecret = errors.New("value would publish a credential")
	// ErrDelimiter reports an upstream notice that contains the delimiter used
	// to embed it, which would make the embedded section ambiguous.
	ErrDelimiter = errors.New("upstream notice contains the embedding delimiter")
	// ErrLicense reports a licence text that is not the licence its identifier
	// names.
	ErrLicense = errors.New("licence text does not match its identifier")
	// ErrEvidence reports root provenance that does not account for the tree it
	// describes, which would publish a record claiming less happened than did.
	ErrEvidence = errors.New("root provenance does not account for the relocated tree")
)

// Source identifies the upstream commit the module was extracted from.
type Source struct {
	// Repository is the upstream clone URL, such as
	// https://github.com/kubernetes/kubernetes.git.
	Repository string
	// Module is the upstream module path, such as k8s.io/kubernetes.
	Module string
	// Project is the upstream project's display name, used only to say where
	// the code came from. It is never used to claim any relationship with it.
	Project string
	// SHA is the exact upstream commit.
	SHA string
	// Tag is the upstream release the commit belongs to, such as v1.36.1. It is
	// optional, because a commit between releases has none.
	Tag string
	// Packages are the upstream package directories the profile extracted,
	// repository relative.
	Packages []string
}

// ModuleMapping records how one upstream staging directory was resolved to a
// published module version.
//
// The mapping is provenance rather than build metadata. A consumer reading
// go.mod sees a published version; only this record says which directory of
// which upstream commit that version is meant to correspond to, which is what
// makes a claim about a staging module checkable.
type ModuleMapping struct {
	// Source is the upstream repository relative staging directory, such as
	// staging/src/k8s.io/api.
	Source string
	// Module is the published module path, such as k8s.io/api.
	Module string
	// Version is the exact resolved version, such as v0.36.1.
	Version string
}

// BehaviorChange is a documented difference between the extracted code and the
// upstream code it came from.
//
// A behaviour change is the one consequence of extraction that no diff shows.
// Pruning a file that registered types into a scheme changes what the remaining
// code does at import time while leaving every remaining line identical, so the
// change has to be written down by whoever decided it was safe.
type BehaviorChange struct {
	// Summary is a one line statement of what differs.
	Summary string
	// Cause is what produced the change, such as prune or patch.
	Cause string
	// Detail explains why the change is intended and why it is safe.
	Detail string
}

// Options describes the root provenance to generate.
type Options struct {
	// Module is the generated module path, such as monis.app/kk/rbac_authorizer.
	Module string
	// RootPackage is the generated root package name, such as rbacauthorizer.
	RootPackage string
	// Repository is the generated module's own repository URL.
	Repository string
	// InternalPrefix is where relocated upstream code lives, such as
	// internal/kk.
	InternalPrefix string
	// Summary states what the module provides. It is a noun phrase completing
	// "Package <root package> provides ...", because that is the form godoc
	// expects of a package comment and the same words have to work as a
	// standalone tagline in the README and the NOTICE. It is written by the
	// profile rather than derived, because no analysis of the code can say what
	// the code is for.
	Summary string
	// Source identifies the upstream commit.
	Source Source
	// License is the upstream licence, reproduced byte for byte.
	License []byte
	// LicenseID is the SPDX identifier of that licence, such as Apache-2.0. It
	// is required and checked against the licence text, because the generated
	// files quote the obligations of a particular licence by section number and
	// quoting the wrong one would misstate what this module owes.
	LicenseID string
	// UpstreamNotice is the upstream NOTICE, embedded verbatim. It is optional
	// only in the sense that an upstream project may not have one; when the
	// upstream work has a NOTICE, omitting it here is a licence violation
	// rather than a formatting choice.
	UpstreamNotice []byte
	// Packages are the per package records the rewriting step produced. They
	// are the source of the pruning, patch, and modification evidence, so the
	// root NOTICE cannot disagree with the record committed beside each
	// package.
	Packages []*rewrite.PackageProvenance
	// Modules are the staging module version mappings.
	Modules []ModuleMapping
	// Copied are the staging packages copied into the module, with their
	// licences. It is empty for a profile that keeps every dependency external.
	Copied []CopiedPackage
	// BehaviorChanges are the documented differences from upstream.
	BehaviorChanges []BehaviorChange
	// PublicAPI are the names the curated facade publishes, for the README. It
	// is the facade manifest's entry names, so the README cannot describe an
	// API the module does not have.
	PublicAPI []string
}

// Files renders the root provenance files.
//
// The result is sorted by path and shaped as relocated files with no upstream
// source, so a caller composes them into the module's file set through the same
// write boundary the copied files go through.
func (o Options) Files() ([]relocate.File, error) {
	if err := o.validate(); err != nil {
		return nil, fmt.Errorf("root provenance: %w", err)
	}
	files := []relocate.File{
		// The licence is the one file that is copied rather than rendered.
		{Path: LicenseFileName, Mode: relocate.ModeRegular, Contents: slices.Clone(o.License)},
		{Path: NoticeFileName, Mode: relocate.ModeRegular, Contents: o.notice()},
		{Path: ReadmeFileName, Mode: relocate.ModeRegular, Contents: o.readme()},
		{Path: DocFileName, Mode: relocate.ModeRegular, Contents: o.doc(), Generated: true},
	}
	slices.SortFunc(files, func(a, b relocate.File) int { return strings.Compare(a.Path, b.Path) })
	return files, nil
}

// CrossCheck proves the root provenance accounts for the tree it describes.
//
// Validation can only see the records it was handed, so on its own it cannot
// tell complete evidence from evidence that happens to be internally
// consistent. This compares the records against the file set that will actually
// be published: every relocated package must have a record, every relocated
// file must appear in it, and no record may describe a package the tree does
// not contain.
//
// It is separate from Files because it needs the composed tree, which the
// caller assembles after rendering. Callers run both; the split is about what
// each one can see, not about how optional either is.
//
// The last requirement is the one that catches a silent failure rather than a
// bookkeeping slip. A tree of relocated code in which no file records a single
// change would render a NOTICE stating that nothing was modified, which is
// exactly the claim the licence forbids this module from making about code it
// rewrote.
func (o Options) CrossCheck(set relocate.FileSet) error {
	if err := o.validate(); err != nil {
		return fmt.Errorf("root provenance: %w", err)
	}
	records := make(map[string]*rewrite.PackageProvenance, len(o.Packages))
	for _, record := range o.Packages {
		if _, seen := records[record.Package]; seen {
			return fmt.Errorf("root provenance: package %s is recorded twice: %w", record.Package, ErrEvidence)
		}
		records[record.Package] = record
	}

	for _, pkg := range set.Packages {
		if !o.relocatedPackage(pkg.Path) {
			continue
		}
		record, ok := records[pkg.Path]
		if !ok {
			return fmt.Errorf("root provenance: %s is in the tree with no provenance record: %w", pkg.Path, ErrEvidence)
		}
		recorded := make(map[string]bool, len(record.Files))
		for _, file := range record.Files {
			recorded[file.Path] = true
		}
		for _, file := range pkg.Files {
			// The record written beside a package is not itself copied code and
			// has no upstream path to record.
			if path.Base(file) == rewrite.ProvenanceFileName {
				continue
			}
			if !recorded[file] {
				return fmt.Errorf("root provenance: %s is in the tree with no file record: %w", file, ErrEvidence)
			}
		}
		delete(records, pkg.Path)
	}
	// Validation has already refused an empty record list, so a record with no
	// package in the tree is the only shape a tree without relocated code can
	// take here, and it is reported as the more specific failure it is.
	for _, name := range sortedKeys(records) {
		return fmt.Errorf("root provenance: %s has a provenance record but is not in the tree: %w", name, ErrEvidence)
	}
	if len(o.modifiedFiles()) == 0 {
		return fmt.Errorf("root provenance: no relocated file records a change, so the notice would state that nothing was modified: %w", ErrEvidence)
	}
	return nil
}

// sortedKeys reports a map's keys in order, so a failure names the same record
// on every run.
func sortedKeys(records map[string]*rewrite.PackageProvenance) []string {
	names := make([]string, 0, len(records))
	for name := range records {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// relocatedPackage reports whether a destination directory holds relocated
// upstream code, which is what a provenance record has to exist for.
func (o Options) relocatedPackage(destination string) bool {
	return destination == o.InternalPrefix || strings.HasPrefix(destination, o.InternalPrefix+"/")
}

// shaPattern matches the object names Git produces, in either hash algorithm.
var shaPattern = regexp.MustCompile(`^[0-9a-f]{40}$|^[0-9a-f]{64}$`)

// validate refuses inputs that would produce a file the module may not publish.
func (o Options) validate() error {
	for _, required := range []struct{ name, value string }{
		{"module path", o.Module},
		{"root package", o.RootPackage},
		{"summary", o.Summary},
		{"upstream module path", o.Source.Module},
		{"upstream project name", o.Source.Project},
	} {
		if strings.TrimSpace(required.value) == "" {
			return fmt.Errorf("%w: a %s is required", ErrOptions, required.name)
		}
	}
	if err := checkRelative(o.InternalPrefix, "internal prefix"); err != nil {
		return err
	}
	for _, link := range []struct{ name, value string }{
		{"module repository", o.Repository},
		{"upstream repository", o.Source.Repository},
	} {
		if err := checkURL(link.value, link.name); err != nil {
			return err
		}
	}
	if !shaPattern.MatchString(o.Source.SHA) {
		return fmt.Errorf("%w: upstream commit %q is not a Git object name", ErrOptions, o.Source.SHA)
	}
	if len(o.License) == 0 {
		return fmt.Errorf("%w: the upstream licence is required and is reproduced byte for byte", ErrOptions)
	}
	if !utf8.Valid(o.License) {
		return fmt.Errorf("%w: the upstream licence is not valid UTF-8 text", ErrOptions)
	}
	if err := VerifyLicense(o.LicenseID, o.License); err != nil {
		return fmt.Errorf("upstream licence: %w", err)
	}
	if len(o.UpstreamNotice) > 0 && !utf8.Valid(o.UpstreamNotice) {
		return fmt.Errorf("%w: the upstream notice is not valid UTF-8 text", ErrOptions)
	}
	if strings.Contains(string(o.UpstreamNotice), noticeDelimiter) {
		return fmt.Errorf("%w: %w", ErrDelimiter, ErrOptions)
	}
	for _, upstream := range o.Source.Packages {
		if err := checkRelative(upstream, "upstream package"); err != nil {
			return err
		}
	}
	for _, mapping := range o.Modules {
		if err := checkRelative(mapping.Source, "staging directory"); err != nil {
			return err
		}
		if mapping.Module == "" || mapping.Version == "" {
			return fmt.Errorf("%w: staging directory %s maps to module %q at version %q", ErrOptions, mapping.Source, mapping.Module, mapping.Version)
		}
	}
	for _, copied := range o.Copied {
		if err := copied.validate(); err != nil {
			return err
		}
	}
	if len(o.Packages) == 0 {
		// A generated module is relocated upstream code. Rendering root
		// provenance without a single package record would publish a NOTICE
		// whose modification, prune, and patch sections all read "(none)" for a
		// tree full of copied code, which is a false statement rather than an
		// empty one.
		return fmt.Errorf("%w: no relocated package was recorded", ErrEvidence)
	}
	for _, record := range o.Packages {
		if record == nil {
			return fmt.Errorf("%w: a package provenance record is missing", ErrOptions)
		}
		if err := checkRelative(record.Package, "relocated package"); err != nil {
			return err
		}
	}
	for _, change := range o.BehaviorChanges {
		if strings.TrimSpace(change.Summary) == "" {
			return fmt.Errorf("%w: a behaviour change has no summary", ErrOptions)
		}
	}
	return nil
}

// checkRelative refuses a path that would name a location on the machine that
// generated the module rather than one inside it.
func checkRelative(value, what string) error {
	switch {
	case value == "":
		return fmt.Errorf("%w: a %s is required", ErrOptions, what)
	case path.IsAbs(value) || strings.HasPrefix(value, "\\") || strings.Contains(value, ":\\"):
		return fmt.Errorf("%w: %s %q is an absolute path, which would name the machine that generated the module", ErrOptions, what, value)
	case value != path.Clean(value):
		return fmt.Errorf("%w: %s %q is not a clean relative path", ErrOptions, what, value)
	case strings.HasPrefix(value, "../") || value == "..":
		return fmt.Errorf("%w: %s %q escapes the module", ErrOptions, what, value)
	}
	return nil
}

// checkURL refuses a URL that is not a plain public location.
//
// Credentials in a URL are the failure this exists for. A clone URL carrying
// user information is a perfectly ordinary thing to have in a shell history or
// a CI configuration, and writing one into a committed NOTICE would publish it
// to everyone who ever reads the module.
func checkURL(value, what string) error {
	if value == "" {
		return fmt.Errorf("%w: a %s URL is required", ErrOptions, what)
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return fmt.Errorf("%w: %s URL %q: %w", ErrOptions, what, value, err)
	}
	if parsed.Scheme != "https" {
		return fmt.Errorf("%w: %s URL %q must use https", ErrOptions, what, value)
	}
	if parsed.User != nil {
		return fmt.Errorf("%w: %s URL carries user information: %w", ErrSecret, what, ErrOptions)
	}
	if parsed.Host == "" {
		return fmt.Errorf("%w: %s URL %q names no host", ErrOptions, what, value)
	}
	return nil
}

// modifiedPackages reports the relocated packages, sorted, with the counts the
// root notice states.
func (o Options) modifiedPackages() []*rewrite.PackageProvenance {
	records := slices.Clone(o.Packages)
	slices.SortFunc(records, func(a, b *rewrite.PackageProvenance) int {
		return strings.Compare(a.Package, b.Package)
	})
	return records
}

// prunedPaths reports every upstream path the profile removed, sorted and
// without duplicates.
func (o Options) prunedPaths() []string {
	var pruned []string
	for _, record := range o.Packages {
		pruned = append(pruned, record.Pruned...)
	}
	slices.Sort(pruned)
	return slices.Compact(pruned)
}

// appliedPatches reports the patches applied, in application order, without
// repeating one that several packages recorded.
//
// Order is preserved rather than sorted because the order a series applies in
// is part of what it means, and a sorted list would describe a run that never
// happened.
func (o Options) appliedPatches() []string {
	var patches []string
	for _, record := range o.modifiedPackages() {
		for _, id := range record.Patches {
			if !slices.Contains(patches, id) {
				patches = append(patches, id)
			}
		}
	}
	return patches
}

// modifiedFiles reports the relocated files a rewrite changed, sorted by
// destination path.
//
// This is the root level evidence Apache License 2.0 section 4(b) asks for. The
// per file notices the rewriting step inserts are the prominent ones; this list
// is what lets a reader see the whole set without opening every file.
func (o Options) modifiedFiles() []string {
	var modified []string
	for _, record := range o.Packages {
		for _, file := range record.Files {
			if len(file.Changes) > 0 {
				modified = append(modified, file.Path)
			}
		}
	}
	slices.Sort(modified)
	return slices.Compact(modified)
}
