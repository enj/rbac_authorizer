package provenance

import (
	"slices"
	"strings"

	"github.com/enj/soapbox/tools/internal/rewrite"
)

// readme renders the repository's front page.
//
// It answers, in order, the questions a reader arriving from a search result
// actually has: what this is, whether it is official, where the code came from,
// what it exposes, and what they may not rely on. The last of those is the one
// a hand written README usually omits and the one that matters most here: the
// relocated upstream code is an implementation detail behind an internal
// prefix, and a consumer who reaches for it has no compatibility promise at
// all.
func (o Options) readme() []byte {
	var b strings.Builder
	b.WriteString("# " + o.Module + "\n\n")
	b.WriteString(wrap(o.Summary) + "\n\n")

	b.WriteString("## What this is\n\n")
	b.WriteString(wrap("This module is generated. Its code is copied from "+o.Source.Module+
		" and modified so that it can be consumed as an ordinary Go module, which the upstream "+
		"repository cannot be: upstream requires its own staging modules through repository local "+
		"directory replacements that resolve nowhere else.") + "\n\n")
	b.WriteString(wrap("It is not a "+o.Source.Project+" release. It is not produced by, endorsed by, "+
		"or affiliated with the "+o.Source.Project+" project. See "+NoticeFileName+" for the full "+
		"attribution, licence, and trademark statement, and for the record of what was changed.") + "\n\n")

	b.WriteString("## Provenance\n\n")
	b.WriteString("| field | value |\n| --- | --- |\n")
	for _, row := range [][2]string{
		{"upstream module", o.Source.Module},
		{"upstream repository", o.Source.Repository},
		{"upstream commit", o.Source.SHA},
		{"upstream release", o.Source.Tag},
		{"relocated below", o.InternalPrefix},
	} {
		if row[1] != "" {
			b.WriteString("| " + row[0] + " | `" + row[1] + "` |\n")
		}
	}
	b.WriteString("\n")

	o.writeReadmeAPI(&b)
	o.writeReadmeUsage(&b)
	o.writeReadmeStability(&b)
	return []byte(b.String())
}

// writeReadmeAPI lists what the curated facade publishes.
func (o Options) writeReadmeAPI(b *strings.Builder) {
	b.WriteString("## Public API\n\n")
	if len(o.PublicAPI) == 0 {
		b.WriteString(wrap("This module publishes no curated API yet.") + "\n\n")
		return
	}
	b.WriteString(wrap("Package `"+o.RootPackage+"` at the module root is the entire public API. "+
		"Every name below is an alias of, or forwards to, the relocated upstream declaration it was "+
		"generated from, so a value it produces is the upstream value and an implementation of an "+
		"interface it publishes satisfies the upstream contract.") + "\n\n")
	names := slices.Clone(o.PublicAPI)
	slices.Sort(names)
	for _, name := range slices.Compact(names) {
		b.WriteString("- `" + name + "`\n")
	}
	b.WriteString("\n")
}

// writeReadmeUsage shows the one line a consumer needs.
func (o Options) writeReadmeUsage(b *strings.Builder) {
	b.WriteString("## Usage\n\n")
	b.WriteString("```go\nimport \"" + o.Module + "\"\n```\n\n")
}

// writeReadmeStability states what a consumer may and may not depend on.
//
// The internal prefix is not a suggestion: Go refuses an import of it from
// outside this module. Saying so here turns a compiler error a consumer would
// otherwise have to interpret into an expectation they had before they tried.
func (o Options) writeReadmeStability(b *strings.Builder) {
	b.WriteString("## What you may depend on\n\n")
	b.WriteString(wrap("Only package `"+o.RootPackage+"`. Everything under `"+o.InternalPrefix+
		"` is relocated upstream code, it is reorganised whenever upstream changes, and Go will refuse "+
		"to let you import it from another module in any case.") + "\n\n")
	b.WriteString(wrap("Each relocated package carries a `"+rewrite.ProvenanceFileName+"` file naming "+
		"its upstream path, the upstream commit it was taken at, and every change made to it. Each "+
		"modified file carries a notice above its package clause saying that it is not the upstream "+
		"original.") + "\n\n")

	b.WriteString("## Licence\n\n")
	b.WriteString(wrap("The copied code keeps its upstream licence, reproduced unchanged in `"+
		LicenseFileName+"`. `"+NoticeFileName+"` carries the upstream attribution notices in full, "+
		"together with the statement of modification the licence requires.") + "\n")
}

// doc renders the root package's documentation comment.
//
// The facade file deliberately carries no package documentation, so this is the
// only place the root package is described. Keeping the two apart means a
// regenerated facade never churns the prose, and a reader looking for the
// documentation finds it in the file whose name says so.
func (o Options) doc() []byte {
	var b strings.Builder
	b.WriteString(generatedHeader + "\n\n")
	b.WriteString(comment(o.Summary, "Package "+o.RootPackage+" provides "))
	b.WriteString("//\n")
	b.WriteString(comment("The code behind this package is copied from "+o.Source.Module+" at commit "+
		o.Source.SHA+" and modified. This module is not a "+o.Source.Project+" release and is not "+
		"endorsed by or affiliated with that project. See the "+NoticeFileName+" file for the full "+
		"attribution and the record of what was changed.", ""))
	b.WriteString("//\n")
	b.WriteString(comment("Everything this package exposes is generated from the relocated upstream "+
		"declarations by aliases and forwarding functions, so the published types are the upstream "+
		"types rather than copies of them. Nothing below "+o.InternalPrefix+" is part of the public "+
		"API, and Go will not permit importing it from another module.", ""))
	b.WriteString("package " + o.RootPackage + "\n")
	return []byte(b.String())
}

// generatedHeader marks the files this package generates, using the convention
// at https://go.dev/s/generatedcode so every Go tool that skips generated files
// skips them.
const generatedHeader = "// Code generated by soapbox. DO NOT EDIT."

// commentWidth is the column a generated comment wraps at.
const commentWidth = 77

// comment wraps prose into line comments, optionally starting the first line
// with a fixed lead so a package comment begins with the words godoc requires.
func comment(text, lead string) string {
	var b strings.Builder
	line := "//"
	if lead != "" {
		line = "// " + strings.TrimRight(lead, " ")
	}
	for _, word := range strings.Fields(text) {
		if len(line)+1+len(word) > commentWidth && line != "//" {
			b.WriteString(line + "\n")
			line = "//"
		}
		line += " " + word
	}
	if line != "//" {
		b.WriteString(line + "\n")
	}
	return b.String()
}
