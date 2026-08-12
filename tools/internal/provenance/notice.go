package provenance

import (
	"strings"

	"github.com/enj/soapbox/tools/internal/rewrite"
)

// noticeDelimiter fences the embedded upstream notice.
//
// It is a full width rule rather than a short marker so it cannot be produced
// accidentally by wrapped prose, and an upstream notice that contains it anyway
// is refused rather than embedded, because a reader who cannot tell where the
// upstream text ends cannot tell which project is saying what.
const noticeDelimiter = "================================================================================"

// notice renders the root NOTICE.
//
// The file has two halves that are never mixed. The first is what Soapbox has
// to say: what this module is, what it is not, where its code came from, and
// what was changed on the way. The second is the upstream project's own
// attribution notice, reproduced whole inside delimiters that name it. Merging
// them would put words into the upstream project's mouth, and the whole purpose
// of section 4(d) of the licence is that the upstream notice reaches the reader
// as the upstream project wrote it.
//
// It cannot fail. Everything that could make a notice unrenderable, including
// an upstream file that contains the embedding delimiter, is refused by
// validation before any of this runs.
func (o Options) notice() []byte {
	var b strings.Builder
	b.WriteString(o.Module + "\n\n")
	b.WriteString(wrap(o.Summary) + "\n\n")

	o.writeOrigin(&b)
	o.writeModifications(&b)
	o.writeBehaviorChanges(&b)
	o.writeModuleMapping(&b)
	o.writeCopied(&b)
	o.writeTrademarks(&b)
	o.writeUpstreamNotice(&b)
	return []byte(b.String())
}

// writeOrigin states what the module is and where its code came from.
func (o Options) writeOrigin(b *strings.Builder) {
	section(b, "Origin")
	b.WriteString(wrap("This module is a derivative work. It contains source code copied from "+
		o.Source.Module+" and modified by soapbox. It is not a distribution of "+o.Source.Module+
		" itself, and it is not produced by, endorsed by, or affiliated with the "+o.Source.Project+
		" project.") + "\n\n")
	field(b, "upstream module", o.Source.Module)
	field(b, "upstream repository", o.Source.Repository)
	field(b, "upstream commit", o.Source.SHA)
	field(b, "upstream release", o.Source.Tag)
	field(b, "generated module", o.Module)
	field(b, "generated repository", o.Repository)
	field(b, "relocated below", o.InternalPrefix)
	field(b, "licence", o.LicenseID)
	list(b, "extracted upstream packages", o.Source.Packages)
}

// writeModifications is the root level statement Apache License 2.0 section
// 4(b) requires, together with the evidence behind it.
//
// The per file notices the rewriting step inserts are the prominent ones the
// licence asks for. This section exists because a reader who wants to know what
// was done to the work as a whole should not have to open every file to find
// out, and because pruning is a modification that leaves no trace in any file
// that survived it.
func (o Options) writeModifications(b *strings.Builder) {
	section(b, "Modifications")
	b.WriteString(wrap("Files copied from "+o.Source.Module+" were changed. Every changed file "+
		"carries a notice saying so above its package clause, and every relocated package carries a "+
		rewrite.ProvenanceFileName+" record naming its upstream path, its upstream commit, and each "+
		"change made to it. The changes are of three kinds: import paths were rewritten to the "+
		"relocated paths inside this module, files the profile prunes were not copied at all, and any "+
		"patches listed below were applied before relocation.") + "\n\n")

	packages := o.modifiedPackages()
	values := make([]string, 0, len(packages))
	for _, record := range packages {
		values = append(values, record.Package+"  (from "+record.SourcePackage+")")
	}
	list(b, "relocated packages", values)
	list(b, "changed files", o.modifiedFiles())
	list(b, "pruned upstream files", o.prunedPaths())
	list(b, "applied patches", o.appliedPatches())
}

// writeBehaviorChanges records the differences no diff would show.
func (o Options) writeBehaviorChanges(b *strings.Builder) {
	section(b, "Behavior changes")
	if len(o.BehaviorChanges) == 0 {
		b.WriteString(wrap("The profile records no intended behaviour change relative to the upstream "+
			"code at the commit above.") + "\n\n")
		return
	}
	b.WriteString(wrap("These differences from the upstream code are intended. They are recorded here "+
		"because they change what the code does rather than where it lives, so no diff of the copied "+
		"files would reveal them.") + "\n\n")
	for _, change := range sortedChanges(o.BehaviorChanges) {
		b.WriteString("  - " + change.Summary + "\n")
		if change.Cause != "" {
			b.WriteString("    cause: " + change.Cause + "\n")
		}
		if change.Detail != "" {
			b.WriteString(indent(wrap(change.Detail), "    ") + "\n")
		}
	}
	b.WriteString("\n")
}

// writeModuleMapping records which published module version each upstream
// staging directory was resolved to.
func (o Options) writeModuleMapping(b *strings.Builder) {
	section(b, "Staging module mapping")
	if len(o.Modules) == 0 {
		b.WriteString(wrap("This module requires no staging module of "+o.Source.Module+".") + "\n\n")
		return
	}
	b.WriteString(wrap("The upstream repository requires its staging modules through repository local "+
		"directories, which a consumer outside that repository cannot resolve. Each was replaced by the "+
		"published module version that corresponds to the upstream commit above.") + "\n\n")
	values := make([]string, 0, len(o.Modules))
	for _, mapping := range sortedMappings(o.Modules) {
		values = append(values, mapping.Source+" -> "+mapping.Module+"@"+mapping.Version)
	}
	list(b, "mapping", values)
}

// writeCopied records staging packages copied into this module, with the
// licences that came with them.
func (o Options) writeCopied(b *strings.Builder) {
	section(b, "Copied dependency packages")
	if len(o.Copied) == 0 {
		b.WriteString(wrap("No dependency package was copied into this module. Every dependency is "+
			"required as a published module, so its types keep the identity the rest of the ecosystem "+
			"uses.") + "\n\n")
		return
	}
	b.WriteString(wrap("These packages were copied into this module rather than required as published "+
		"modules. Each is listed with the module it came from, the commit it was taken at, and the "+
		"licence files copied with it.") + "\n\n")
	for _, copied := range sortedCopied(o.Copied) {
		b.WriteString("  - " + copied.Package + "\n")
		field2(b, "    ", "module", copied.Module+"@"+copied.Version)
		field2(b, "    ", "upstream repository", copied.SourceRepository)
		field2(b, "    ", "upstream commit", copied.SourceSHA)
		field2(b, "    ", "destination", copied.Destination)
		field2(b, "    ", "approval", copied.Override)
		field2(b, "    ", "licence", copied.LicenseID)
		for _, license := range copied.Licenses {
			b.WriteString("    file: " + license.Destination + "  (from " + license.SourcePath +
				", sha256 " + license.SHA256 + ")\n")
		}
	}
	b.WriteString("\n")
}

// writeTrademarks states the one thing the licence explicitly does not grant.
//
// Section 6 grants no trademark rights, and a derivative work that carries an
// upstream project's name in its module path, its documentation, and its
// provenance is exactly the kind of thing a reader could mistake for an
// official release. Saying so is cheap; leaving it unsaid is the overclaim.
func (o Options) writeTrademarks(b *strings.Builder) {
	section(b, "Trademarks")
	grant := "The " + o.LicenseID + " licence the copied code is under grants no rights to the " +
		"trade names, trademarks, service marks, or product names of the upstream licensor."
	if o.LicenseID == Apache20 {
		// The section number is quoted only for the licence that has it. A
		// citation attached to the wrong licence is worse than no citation:
		// a reader who checks it finds the claim unsupported.
		grant = "Section 6 of the Apache License 2.0 grants no rights to the trade names, " +
			"trademarks, service marks, or product names of the upstream licensor."
	}
	b.WriteString(wrap(grant+" The "+o.Source.Project+
		" name and marks belong to their owners and are used here only as required to identify the "+
		"origin of the copied code. This module is not a "+o.Source.Project+" release and its "+
		"maintainers do not speak for that project.") + "\n\n")
}

// writeUpstreamNotice embeds the upstream NOTICE whole.
//
// The bytes between the delimiters are the upstream file exactly, save for a
// single terminating newline added when the original lacks one so the closing
// delimiter starts its own line. Nothing is reflowed, reordered, or summarised:
// the requirement is that a reader of this module can read the upstream notices
// as the upstream project wrote them.
func (o Options) writeUpstreamNotice(b *strings.Builder) {
	section(b, "Upstream notices")
	if len(o.UpstreamNotice) == 0 {
		b.WriteString(wrap("The upstream work at the commit above carries no NOTICE file.") + "\n")
		return
	}
	requirement := "reproduced in full and unmodified as the " + o.LicenseID + " licence requires."
	if o.LicenseID == Apache20 {
		requirement = "reproduced in full and unmodified as required by section 4(d) of the Apache License 2.0."
	}
	b.WriteString(wrap("The following is the NOTICE file of "+o.Source.Module+" at the commit above, "+
		requirement+" Everything between the delimiters is the upstream project's text, not this "+
		"module's.") + "\n\n")
	b.WriteString(noticeDelimiter + "\n")
	b.WriteString("BEGIN NOTICE OF " + o.Source.Module + " AT " + o.Source.SHA + "\n")
	b.WriteString(noticeDelimiter + "\n")
	b.Write(o.UpstreamNotice)
	if !strings.HasSuffix(string(o.UpstreamNotice), "\n") {
		b.WriteString("\n")
	}
	b.WriteString(noticeDelimiter + "\n")
	b.WriteString("END NOTICE OF " + o.Source.Module + "\n")
	b.WriteString(noticeDelimiter + "\n")
}

// section writes a heading.
func section(b *strings.Builder, title string) {
	b.WriteString(title + "\n")
	b.WriteString(strings.Repeat("-", len(title)) + "\n\n")
}

// field writes one name and value, leaving out a value the caller does not have
// rather than rendering an empty claim.
func field(b *strings.Builder, name, value string) {
	field2(b, "  ", name, value)
}

// field2 writes one indented name and value.
func field2(b *strings.Builder, indentation, name, value string) {
	if value != "" {
		b.WriteString(indentation + name + ": " + value + "\n")
	}
}

// list writes one named list, always stating the empty case rather than
// omitting the section, because "no patches were applied" is evidence and a
// missing section is not.
func list(b *strings.Builder, name string, values []string) {
	b.WriteString("  " + name + ":\n")
	if len(values) == 0 {
		b.WriteString("    (none)\n")
	}
	for _, value := range values {
		b.WriteString("    " + value + "\n")
	}
	b.WriteString("\n")
}

// wrapWidth is the column generated prose wraps at.
const wrapWidth = 78

// wrap fills prose to a fixed width.
//
// The fill is a plain greedy one on spaces and depends on nothing but the text,
// because these bytes are committed and a wrapping that drifted would show up
// as a diff in a release that changed nothing.
func wrap(text string) string {
	var lines []string
	line := ""
	for _, word := range strings.Fields(text) {
		switch {
		case line == "":
			line = word
		case len(line)+1+len(word) > wrapWidth:
			lines = append(lines, line)
			line = word
		default:
			line += " " + word
		}
	}
	if line != "" {
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// indent prefixes every line of a block.
func indent(block, prefix string) string {
	lines := strings.Split(block, "\n")
	for i, line := range lines {
		if line != "" {
			lines[i] = prefix + line
		}
	}
	return strings.Join(lines, "\n")
}
