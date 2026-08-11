package rewrite

import (
	"slices"
	"strings"
)

// ProvenanceFileName is the file each relocated package carries beside its
// sources. The name is deliberately loud and not a Go file, so it can never be
// confused with copied upstream code or compiled into the module.
const ProvenanceFileName = "SOAPBOX_PROVENANCE.txt"

// FileProvenance records what happened to one relocated file.
type FileProvenance struct {
	// Path is the destination module relative path.
	Path string
	// SourcePath is the upstream repository relative path.
	SourcePath string
	// Generated records that the file carries a Code generated marker.
	Generated bool
	// Changes are every recorded transformation, in report order.
	Changes []Change
}

// PackageProvenance is the record written beside one relocated package.
//
// The record exists so a consumer who finds surprising code in the generated
// module can answer three questions without the engine: where the file came
// from, at which upstream commit, and what the engine changed on the way. The
// rendering is deterministic, because the record is committed and a reordering
// would show up as a diff in every release.
type PackageProvenance struct {
	// Package is the destination package directory.
	Package string
	// SourcePackage is the upstream package directory.
	SourcePackage string
	// SourceRepository and SourceSHA identify the upstream commit.
	SourceRepository string
	SourceSHA        string
	// Files are the relocated files of the package.
	Files []FileProvenance
	// Pruned are the upstream paths the profile removed from this package.
	Pruned []string
	// Patches are the identifiers of the patches applied to the ref
	// transaction, in application order.
	Patches []string
}

// NewPackageProvenance starts a record for one relocated package.
func NewPackageProvenance(destination, source string, opts Options) *PackageProvenance {
	return &PackageProvenance{
		Package:          destination,
		SourcePackage:    source,
		SourceRepository: opts.SourceRepository,
		SourceSHA:        opts.SourceSHA,
	}
}

// AddFile records one relocated file and the changes a rewrite made to it.
func (p *PackageProvenance) AddFile(file File, result Result) {
	p.Files = append(p.Files, FileProvenance{
		Path:       file.Path,
		SourcePath: file.SourcePath,
		Generated:  file.Generated,
		Changes:    slices.Clone(result.Changes),
	})
}

// AddPruned records upstream paths the profile removed from this package.
func (p *PackageProvenance) AddPruned(paths ...string) {
	p.Pruned = append(p.Pruned, paths...)
}

// AddPatches records the patches applied to the ref transaction.
func (p *PackageProvenance) AddPatches(ids ...string) {
	p.Patches = append(p.Patches, ids...)
}

// Render writes the record.
//
// Files, changes, and pruned paths are sorted; patches keep application order,
// because the order a series applies in is part of what it means and sorting it
// would describe a run that never happened.
func (p *PackageProvenance) Render() string {
	files := slices.Clone(p.Files)
	slices.SortFunc(files, func(a, b FileProvenance) int { return strings.Compare(a.Path, b.Path) })
	pruned := slices.Clone(p.Pruned)
	slices.Sort(pruned)
	pruned = slices.Compact(pruned)

	var b strings.Builder
	b.WriteString("soapbox package provenance\n")
	writeProvenanceField(&b, "package", p.Package)
	writeProvenanceField(&b, "upstream package", p.SourcePackage)
	writeProvenanceField(&b, "upstream repository", p.SourceRepository)
	writeProvenanceField(&b, "upstream commit", p.SourceSHA)

	b.WriteString("\nfiles:\n")
	if len(files) == 0 {
		b.WriteString("  (none)\n")
	}
	for _, file := range files {
		b.WriteString("  " + file.Path + "\n")
		b.WriteString("    upstream: " + file.SourcePath + "\n")
		if file.Generated {
			b.WriteString("    generated: yes\n")
		}
		changes := slices.Clone(file.Changes)
		slices.SortFunc(changes, compareChanges)
		if len(changes) == 0 {
			b.WriteString("    unchanged\n")
			continue
		}
		for _, change := range changes {
			b.WriteString("    " + renderProvenanceChange(change) + "\n")
		}
	}

	writeProvenanceList(&b, "pruned", pruned)
	writeProvenanceList(&b, "patches", p.Patches)
	return b.String()
}

// renderProvenanceChange renders one change without repeating the file path,
// which the enclosing section already names.
func renderProvenanceChange(change Change) string {
	stripped := change
	stripped.Path = ""
	return strings.TrimPrefix(stripped.String(), ":")
}

// writeProvenanceField renders one header field, leaving out a value the caller
// did not supply rather than rendering an empty claim.
func writeProvenanceField(b *strings.Builder, name, value string) {
	if value != "" {
		b.WriteString(name + ": " + value + "\n")
	}
}

// writeProvenanceList renders one named list section.
func writeProvenanceList(b *strings.Builder, name string, values []string) {
	b.WriteString("\n" + name + ":\n")
	if len(values) == 0 {
		b.WriteString("  (none)\n")
		return
	}
	for _, value := range values {
		b.WriteString("  " + value + "\n")
	}
}
