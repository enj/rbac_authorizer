package rewrite

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// verify proves that a rewrite changed only what it reported.
//
// Two independent checks run. The syntax tree of the rewritten file must equal
// the original's once import path literals are excluded, which covers every
// declaration, identifier, literal, and operator in the file. The comments must
// equal the original's once the recorded removals, rewrites, and the inserted
// notice are accounted for, which covers the one part of a source file the
// syntax tree comparison deliberately ignores.
//
// Together they turn "the transformation looked right" into "nothing else
// moved", which is the property a published tag depends on.
func verify(before *goSource, file File, out []byte, changes []Change) error {
	after, err := parseGo(file.Path, out)
	if err != nil {
		return fmt.Errorf("reparse rewritten %s: %w", file.Path, err)
	}
	if err := verifyShape(file, out, changes); err != nil {
		return err
	}
	return verifyComments(file, before.file, after.file, changes)
}

// verifyShape compares the syntax trees of the original and rewritten files.
//
// Import path literals are the one thing a rewrite is allowed to change, so
// they are settled first and individually. Every import is paired with its
// counterpart by position; a pair that still reads the same stays in the tree
// comparison, and a pair that differs has to be justified by a reported import
// change before it is excluded from it. Blanking every literal instead would
// make the comparison blind to exactly the corruption it most needs to catch:
// an external import such as k8s.io/api/rbac/v1, which must keep its real
// identity or the generated module stops satisfying the upstream types.
func verifyShape(file File, out []byte, changes []Change) error {
	original, err := parseShape(file.Path, file.Contents)
	if err != nil {
		return err
	}
	rewritten, err := parseShape(file.Path, out)
	if err != nil {
		return err
	}
	if err := verifyImports(file, original, rewritten, changes); err != nil {
		return err
	}

	before, after := renderShape(original), renderShape(rewritten)
	if before == after {
		return nil
	}
	return fmt.Errorf("%s: %w: %s", file.Path, ErrShapeChanged, firstDifference(before, after))
}

// importRewrite is one reported import path replacement. It is a map key so a
// file that rewrites the same path more than once is matched exactly as often
// as it was reported.
type importRewrite struct{ from, to string }

// verifyImports pairs the imports of the original and rewritten files and
// proves that every difference between them was reported.
//
// Pairing is positional because a byte range replacement can neither add,
// remove, nor reorder an import declaration, so a count that changed is itself
// the failure. Each justified pair has its literal blanked in both trees, which
// removes it from the tree comparison and leaves every other import, its alias,
// and its quoting form still covered by it.
func verifyImports(file File, original, rewritten *ast.File, changes []Change) error {
	if len(original.Imports) != len(rewritten.Imports) {
		return fmt.Errorf("%s: %w: %d imports became %d",
			file.Path, ErrShapeChanged, len(original.Imports), len(rewritten.Imports))
	}

	reported := make(map[importRewrite]int, len(changes))
	for _, change := range changes {
		if change.Kind == ChangeImport {
			reported[importRewrite{from: change.From, to: change.To}]++
		}
	}
	for i, before := range original.Imports {
		after := rewritten.Imports[i]
		if before.Path.Value == after.Path.Value {
			continue
		}
		from, err := importPath(file, before.Path.Value)
		if err != nil {
			return err
		}
		to, err := importPath(file, after.Path.Value)
		if err != nil {
			return err
		}
		claim := importRewrite{from: from, to: to}
		if reported[claim] == 0 {
			return fmt.Errorf("%s: %w: import %q became %q without being reported",
				file.Path, ErrShapeChanged, from, to)
		}
		reported[claim]--
		before.Path.Value = ""
		after.Path.Value = ""
	}
	return nil
}

// importPath unquotes one import literal for comparison with a change report,
// which records paths rather than literals.
func importPath(file File, literal string) (string, error) {
	path, err := strconv.Unquote(literal)
	if err != nil {
		return "", fmt.Errorf("%s: %w: import literal %s: %w", file.Path, ErrShapeChanged, literal, err)
	}
	return path, nil
}

// parseShape parses a file without its comments.
//
// Documentation and directive comments are absent from the tree and are checked
// separately by [verifyComments], because a tree that carried them would report
// every intended directive removal as a structural change.
func parseShape(name string, src []byte) (*ast.File, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("parse %s for comparison: %w", name, err)
	}
	return file, nil
}

// renderShape renders a canonical description of a file's syntax tree.
//
// Positions never enter the description, because every rewrite shifts the bytes
// that follow it and a position sensitive comparison would report every
// successful rewrite as a failure.
func renderShape(file *ast.File) string {
	var b strings.Builder
	ast.Inspect(file, func(node ast.Node) bool {
		if node == nil {
			b.WriteString(")")
			return true
		}
		fmt.Fprintf(&b, "(%T", node)
		switch typed := node.(type) {
		case *ast.Ident:
			b.WriteString(" " + typed.Name)
		case *ast.BasicLit:
			b.WriteString(" " + typed.Kind.String() + " " + typed.Value)
		case *ast.GenDecl:
			b.WriteString(" " + typed.Tok.String())
		case *ast.BinaryExpr:
			b.WriteString(" " + typed.Op.String())
		case *ast.UnaryExpr:
			b.WriteString(" " + typed.Op.String())
		case *ast.IncDecStmt:
			b.WriteString(" " + typed.Tok.String())
		case *ast.AssignStmt:
			b.WriteString(" " + typed.Tok.String())
		case *ast.BranchStmt:
			b.WriteString(" " + typed.Tok.String())
		case *ast.RangeStmt:
			b.WriteString(" " + typed.Tok.String())
		case *ast.ChanType:
			fmt.Fprintf(&b, " %d", typed.Dir)
		case *ast.StructType:
			fmt.Fprintf(&b, " %t", typed.Incomplete)
		case *ast.InterfaceType:
			fmt.Fprintf(&b, " %t", typed.Incomplete)
		case *ast.Ellipsis:
			b.WriteString(" ...")
		}
		return true
	})
	return b.String()
}

// differenceContext is how many bytes of each shape are quoted when they
// diverge. Enough to identify the node, short enough to stay readable.
const differenceContext = 60

// firstDifference reports where two shapes diverge.
func firstDifference(original, rewritten string) string {
	limit := min(len(original), len(rewritten))
	at := limit
	for i := range limit {
		if original[i] != rewritten[i] {
			at = i
			break
		}
	}
	return fmt.Sprintf("at %d: %q became %q", at, window(original, at), window(rewritten, at))
}

// window quotes a short slice of a shape starting at an offset.
func window(shape string, at int) string {
	if at >= len(shape) {
		return ""
	}
	return shape[at:min(at+differenceContext, len(shape))]
}

// verifyComments compares the comments of the original and rewritten files.
//
// The expectation is built from the original's comments by applying exactly the
// changes that were reported: a removal drops one comment, a directive rewrite
// exchanges one for another, and the notice adds its own lines. A comment that
// vanished without a report, or one that appeared without a report, fails here.
func verifyComments(file File, before, after *ast.File, changes []Change) error {
	expected := commentCounts(before)
	for _, change := range changes {
		switch change.Kind {
		case ChangeMarkerRemoval, ChangeCommentRemoval:
			expected[change.From]--
		case ChangeDirective:
			expected[change.From]--
			expected[change.To]++
		case ChangeNotice:
			// The notice is terminated the way the file terminates its lines,
			// but the scanner strips a carriage return out of a comment's text,
			// so the expectation has to be built the same way or a preserved
			// CRLF file would report every notice line as both lost and added.
			for _, line := range strings.Split(strings.TrimRight(change.To, "\r\n"), "\n") {
				if line := strings.TrimSuffix(line, "\r"); line != "" {
					expected[line]++
				}
			}
		case ChangeImport, ChangeProtoOption:
			// Neither touches a comment.
		}
	}
	for text, count := range expected {
		if count == 0 {
			delete(expected, text)
		}
	}

	got := commentCounts(after)
	if !maps.Equal(expected, got) {
		return fmt.Errorf("%s: %w: %s", file.Path, ErrCommentsChanged, describeCommentDifference(expected, got))
	}
	return verifyAttachment(file, before, after, changes)
}

// verifyAttachment proves that no comment became a declaration's documentation
// comment that was not already one.
//
// Counting comment texts is not enough on its own. Absorbing the blank line
// under a floating comment leaves every byte of that comment in place, so the
// count comparison sees nothing at all, while the parser now reads it as the
// documentation of the declaration below it. That is a real change to the
// generated module's godoc, made by a transformation that only meant to delete
// a directive two lines away.
//
// Attachment can only shrink here, because every transformation in this package
// either deletes whole lines or inserts a notice above one, and neither can
// push a documentation comment away from what it documents. A text that
// documents more declarations after the rewrite than before is therefore the
// promotion this check exists to catch.
func verifyAttachment(file File, before, after *ast.File, changes []Change) error {
	allowed := docCounts(before)
	for _, change := range changes {
		if change.Kind == ChangeDirective {
			// A rewritten directive does not move, so it still documents
			// whatever it documented, now under its new text.
			allowed[change.To] += allowed[change.From]
		}
	}

	var promoted []string
	for text, count := range docCounts(after) {
		if count > allowed[text] {
			promoted = append(promoted, text)
		}
	}
	if len(promoted) == 0 {
		return nil
	}
	slices.Sort(promoted)
	return fmt.Errorf("%s: %w: became a documentation comment: %s",
		file.Path, ErrCommentsChanged, strings.Join(promoted, ", "))
}

// commentCounts reports how often each comment text appears in a file.
func commentCounts(file *ast.File) map[string]int {
	counts := make(map[string]int)
	for _, group := range file.Comments {
		for _, comment := range group.List {
			counts[comment.Text]++
		}
	}
	return counts
}

// docCounts reports how often each comment text appears in a documentation
// comment, which is the subset of a file's comments that godoc renders.
func docCounts(file *ast.File) map[string]int {
	counts := make(map[string]int)
	ast.Inspect(file, func(node ast.Node) bool {
		if doc := docOf(node); doc != nil {
			for _, comment := range doc.List {
				counts[comment.Text]++
			}
		}
		return true
	})
	return counts
}

// docGroups reports the comment groups the parser attached as documentation.
func docGroups(file *ast.File) map[*ast.CommentGroup]bool {
	groups := make(map[*ast.CommentGroup]bool)
	ast.Inspect(file, func(node ast.Node) bool {
		if doc := docOf(node); doc != nil {
			groups[doc] = true
		}
		return true
	})
	return groups
}

// docOf reports the documentation comment a node carries, if it carries one.
func docOf(node ast.Node) *ast.CommentGroup {
	switch typed := node.(type) {
	case *ast.File:
		return typed.Doc
	case *ast.GenDecl:
		return typed.Doc
	case *ast.FuncDecl:
		return typed.Doc
	case *ast.TypeSpec:
		return typed.Doc
	case *ast.ValueSpec:
		return typed.Doc
	case *ast.ImportSpec:
		return typed.Doc
	case *ast.Field:
		return typed.Doc
	default:
		return nil
	}
}

// describeCommentDifference renders the missing and unexpected comments in a
// stable order.
func describeCommentDifference(expected, got map[string]int) string {
	var missing, unexpected []string
	for text := range maps.Keys(expected) {
		if got[text] < expected[text] {
			missing = append(missing, text)
		}
	}
	for text := range maps.Keys(got) {
		if expected[text] < got[text] {
			unexpected = append(unexpected, text)
		}
	}
	slices.Sort(missing)
	slices.Sort(unexpected)

	var parts []string
	if len(missing) > 0 {
		parts = append(parts, "lost "+strings.Join(missing, ", "))
	}
	if len(unexpected) > 0 {
		parts = append(parts, "added "+strings.Join(unexpected, ", "))
	}
	return strings.Join(parts, "; ")
}
