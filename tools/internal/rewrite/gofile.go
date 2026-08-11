package rewrite

import (
	"cmp"
	"context"
	"fmt"
	"go/ast"
	"go/token"
	"slices"
	"strings"
)

// GoFile rewrites one relocated Go source file.
//
// The file is transformed in a single pass over its original bytes. Import path
// literals rooted at the source prefix are replaced, generator directives are
// handled according to their key scoped rules, and a changed file receives a
// modification notice. A file with nothing to change is returned byte for byte,
// including its original slice, so an unchanged file cannot acquire a notice, a
// reformatting, or a trailing newline it did not have.
//
// The result is verified before it is returned: the rewritten file is reparsed,
// its syntax tree is compared with the original's outside the import literals,
// and its comments are compared with the original's outside the recorded
// changes.
func GoFile(ctx context.Context, file File, opts Options) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, fmt.Errorf("rewrite %s: %w", file.Path, err)
	}
	if err := opts.validate(); err != nil {
		return Result{}, fmt.Errorf("rewrite %s: %w", file.Path, err)
	}
	terminator, err := checkLineEndings(file, opts.LineEndings)
	if err != nil {
		return Result{}, fmt.Errorf("rewrite %s: %w", file.Path, err)
	}

	source, err := parseGo(file.Path, file.Contents)
	if err != nil {
		return Result{}, err
	}
	edits, err := importEdits(source, file, opts)
	if err != nil {
		return Result{}, err
	}
	directives, err := directiveEdits(source, file, opts)
	if err != nil {
		return Result{}, err
	}
	edits = append(edits, directives...)
	if len(edits) == 0 {
		return Result{Contents: file.Contents}, nil
	}
	if !opts.NoNotice {
		edits = append(edits, noticeEdit(source, file, opts, terminator))
	}

	out, changes, err := applyEdits(file.Contents, edits)
	if err != nil {
		return Result{}, fmt.Errorf("rewrite %s: %w", file.Path, err)
	}
	if err := verify(source, file, out, changes); err != nil {
		return Result{}, err
	}
	return Result{Contents: out, Changes: changes}, nil
}

// directiveEdits claims the byte ranges of the directives a rule acts on.
//
// A directive with no rule, a protected directive, and a directive whose value
// holds nothing eligible are all left exactly as they are. Removal claims the
// complete line including its newline, so no blank line is left where a marker
// used to be, and only when the directive owns that line.
func directiveEdits(source *goSource, file File, opts Options) ([]edit, error) {
	var edits []edit
	var removals []int
	removed := make(map[*ast.Comment]bool)
	for _, group := range source.file.Comments {
		for _, comment := range group.List {
			directive, ok := parseDirective(comment.Text)
			if !ok || protected(directive) {
				continue
			}
			directive.Line = source.line(comment.Pos())
			rule := opts.Directives.ruleFor(directive.Key)
			removing := rule.Action == DirectiveRemove ||
				(rule.RemoveWhenDangling && opts.Directives.dangling(directive))
			if !removing && (rule.Action != DirectiveRewrite || directive.Value == "") {
				continue
			}

			start, end, err := commentRange(source, comment)
			if err != nil {
				return nil, fmt.Errorf("%s:%d: %w", file.Path, directive.Line, err)
			}
			if removing {
				// A directive written after code on the same line cannot be
				// removed by deleting its line, so it is kept instead. Upstream
				// never writes a marker that way, and guessing at a partial
				// deletion would be worse than leaving it in place.
				if !ownsLine(source.src, start) {
					continue
				}
				removed[comment] = true
				removals = append(removals, len(edits))
				edits = append(edits, edit{
					start: lineStart(source.src, start),
					end:   lineEnd(source.src, end),
					change: Change{
						Kind: ChangeMarkerRemoval,
						Path: file.Path,
						Line: directive.Line,
						From: comment.Text,
					},
				})
				continue
			}

			value, changed := rewriteDirectiveValue(directive.Value, opts)
			if !changed {
				continue
			}
			edits = append(edits, edit{
				// The value is always the tail of the directive comment, so the
				// key, the leading slashes, and any spacing stay untouched.
				start: end - len(directive.Value),
				end:   end,
				text:  value,
				change: Change{
					Kind: ChangeDirective,
					Path: file.Path,
					Line: directive.Line,
					From: comment.Text,
					To:   strings.TrimSuffix(comment.Text, directive.Value) + value,
				},
			})
		}
	}

	edits, removals, err := strandedSeparators(source, file, edits, removals, removed)
	if err != nil {
		return nil, err
	}
	absorbBlankLines(source, edits, removals)
	return edits, nil
}

// strandedSeparators also claims the empty comment lines a removal would leave
// at the end of a documentation comment.
//
// gofmt reformats documentation comments and drops an empty // line from the
// end of one, so a doc comment whose trailing directives were removed would sit
// one formatting pass away from the committed tree. The separator existed only
// to hold the prose apart from the directives it no longer has, so it leaves
// with them, and it is reported like any other removed comment rather than
// disappearing quietly.
//
// Only documentation comments are touched. gofmt does not reformat a floating
// comment group, so an empty line in one is load bearing to nobody but is also
// not a formatting problem, and removing it would be a change this
// transformation has no reason to make.
func strandedSeparators(source *goSource, file File, edits []edit, removals []int, removed map[*ast.Comment]bool) ([]edit, []int, error) {
	if len(removed) == 0 {
		return edits, removals, nil
	}
	docs := docGroups(source.file)
	for _, group := range source.file.Comments {
		if !docs[group] {
			continue
		}
		last := len(group.List) - 1
		for last >= 0 && removed[group.List[last]] {
			last--
		}
		if last == len(group.List)-1 {
			continue
		}
		for ; last >= 0 && separator(group.List[last]); last-- {
			comment := group.List[last]
			start, end, err := commentRange(source, comment)
			if err != nil {
				return nil, nil, fmt.Errorf("%s:%d: %w", file.Path, source.line(comment.Pos()), err)
			}
			removals = append(removals, len(edits))
			edits = append(edits, edit{
				start: lineStart(source.src, start),
				end:   lineEnd(source.src, end),
				change: Change{
					Kind: ChangeCommentRemoval,
					Path: file.Path,
					Line: source.line(comment.Pos()),
					From: comment.Text,
				},
			})
		}
	}
	return edits, removals, nil
}

// separator reports an empty line comment, the // that gofmt writes between a
// documentation comment's prose and the directives under it.
func separator(comment *ast.Comment) bool {
	return strings.TrimRight(comment.Text, " \t") == "//"
}

// commentRange reports the byte range one comment occupies in the original
// source.
//
// The end is derived from the raw bytes rather than from [ast.Comment.End],
// because the scanner strips carriage returns out of a comment's text and the
// position it derives from that text is then short by one byte for every
// stripped return. Under [LineEndingPreserve] such a file reaches this code,
// and an end that is short would make a value rewrite claim one byte too few
// and leave the tail of the old value welded to the new one. A comment whose
// raw bytes do not match its text carries an embedded carriage return, which is
// reported rather than edited around.
func commentRange(source *goSource, comment *ast.Comment) (start, end int, err error) {
	start = source.offset(comment.Pos())
	end = start + len(comment.Text)
	if end <= len(source.src) && string(source.src[start:end]) == comment.Text {
		return start, end, nil
	}
	return 0, 0, fmt.Errorf("%w: directive comment holds an embedded carriage return", ErrCarriageReturn)
}

// absorbBlankLines extends the removal claims over the blank lines that would
// otherwise be left where the pinned gofmt allows none.
//
// The generated module is committed and then gated on gofmt, so a removal that
// leaves the tree one formatting pass away from itself fails the release rather
// than merely looking untidy. Reprinting the file from its syntax tree would
// fix the spacing and reformat everything else with it, so the spacing is
// repaired by claiming a few more bytes instead.
//
// Absorption reasons about a whole removal block rather than a single line. Two
// removals separated by nothing but blank lines end up adjacent, so they leave
// one gap between the surviving lines and have to be judged together; deciding
// per line is what let a removal absorb the blank line that kept a floating
// comment detached, silently promoting it to the documentation comment of the
// declaration below.
//
// Four positions allow no blank line, and gofmt was measured rather than
// assumed for each: the end of the file, the start of the file, and the two
// edges of a field list or a parenthesized declaration group. A block bordering
// one of them takes the neighbouring blank lines with it. Everywhere else a
// blank line is allowed but two are not, so a block with a blank line above
// gives up the ones below it. A function body, a case clause, and a composite
// literal all keep a blank line at their edges, so a block there is left alone.
func absorbBlankLines(source *goSource, edits []edit, removals []int) {
	for _, block := range removalBlocks(source.src, edits, removals) {
		absorbAround(source, edits, block)
	}
}

// removalBlocks groups the removal claims that will be adjacent once the
// removal has happened, in source order.
func removalBlocks(src []byte, edits []edit, removals []int) [][]int {
	ordered := slices.Clone(removals)
	slices.SortFunc(ordered, func(a, b int) int { return cmp.Compare(edits[a].start, edits[b].start) })

	var blocks [][]int
	for _, index := range ordered {
		if n := len(blocks); n > 0 {
			previous := blocks[n-1][len(blocks[n-1])-1]
			if onlyBlank(src, edits[previous].end, edits[index].start) {
				blocks[n-1] = append(blocks[n-1], index)
				continue
			}
		}
		blocks = append(blocks, []int{index})
	}
	return blocks
}

// absorbAround settles the blank lines around one removal block.
func absorbAround(source *goSource, edits []edit, block []int) {
	src := source.src
	first, last := &edits[block[0]], &edits[block[len(block)-1]]

	// A block's interior blank lines belong to the claim above them, which
	// makes the block one contiguous deletion and keeps two claims from
	// reaching for the same bytes.
	for i := range len(block) - 1 {
		edits[block[i]].end = edits[block[i+1]].start
	}

	above, below := blankRunStart(src, first.start), blankRunEnd(src, last.end)
	switch {
	case below >= len(src):
		// Nothing but blank lines follows, so the file would end on them.
		first.start, last.end = above, len(src)
	case first.start == 0:
		// The block opens the file, so a surviving blank line would too.
		last.end = below
	case atListEdge(source, first.start, last.end):
		first.start, last.end = above, below
	case above < first.start:
		// A blank line above already separates the survivors, so the ones below
		// would double it.
		last.end = below
	}
}

// atListEdge reports whether a block sits against the opening or closing
// delimiter of the innermost field list or parenthesized declaration group
// containing it, which are the two interior positions gofmt allows no blank
// line at.
func atListEdge(source *goSource, start, end int) bool {
	open, closing, ok := enclosingList(source, start, end)
	if !ok {
		return false
	}
	return onlyBlank(source.src, open+1, start) || onlyBlank(source.src, end, closing)
}

// enclosingList reports the delimiter offsets of the innermost field list or
// parenthesized declaration group containing a byte range.
//
// A block statement, a case clause, and a composite literal are deliberately
// absent: gofmt keeps a blank line at their edges, so a removal there needs no
// repair and doing one anyway would change bytes the transformation has no
// reason to touch.
func enclosingList(source *goSource, start, end int) (open, closing int, found bool) {
	ast.Inspect(source.file, func(node ast.Node) bool {
		var lead, tail token.Pos
		switch typed := node.(type) {
		case *ast.FieldList:
			lead, tail = typed.Opening, typed.Closing
		case *ast.GenDecl:
			lead, tail = typed.Lparen, typed.Rparen
		default:
			return true
		}
		if !lead.IsValid() || !tail.IsValid() {
			return true
		}
		// Nodes arrive outermost first, so a later match is always the tighter
		// one.
		if o, c := source.offset(lead), source.offset(tail); o < start && end <= c {
			open, closing, found = o, c, true
		}
		return true
	})
	return open, closing, found
}
