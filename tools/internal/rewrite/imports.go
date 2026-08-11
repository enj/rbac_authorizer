package rewrite

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
)

// goSource is a parsed Go file together with the offset mapping its byte range
// transformations need.
type goSource struct {
	fset *token.FileSet
	tok  *token.File
	file *ast.File
	src  []byte
}

// parseGo parses a Go file with its comments.
//
// Comments are required rather than optional: generator markers, build
// constraints, cgo preambles, and go:embed directives all live in them, and a
// parse that dropped them would let a transformation walk over a build
// constraint without ever seeing it. Object resolution is skipped because
// nothing here needs declaration links.
func parseGo(name string, src []byte) (*goSource, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, name, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", name, err)
	}
	return &goSource{fset: fset, tok: fset.File(file.Pos()), file: file, src: src}, nil
}

// offset reports the byte offset of a position.
func (s *goSource) offset(pos token.Pos) int { return s.tok.Offset(pos) }

// line reports the one based line of a position.
func (s *goSource) line(pos token.Pos) int { return s.tok.Line(pos) }

// importEdits claims the byte range of every eligible import path literal.
//
// Only the literal is claimed, never the surrounding spec. An alias, a blank or
// dot import name, a trailing comment, and the exact position of the literal in
// the file all sit outside the claimed range and therefore cannot change.
func importEdits(source *goSource, file File, opts Options) ([]edit, error) {
	var edits []edit
	for _, spec := range source.file.Imports {
		literal := spec.Path
		path, err := strconv.Unquote(literal.Value)
		if err != nil {
			return nil, fmt.Errorf("%s:%d: import path %s: %w", file.Path, source.line(literal.Pos()), literal.Value, err)
		}
		destination, eligible := opts.destination(path)
		if !eligible {
			continue
		}
		edits = append(edits, edit{
			start: source.offset(literal.Pos()),
			end:   source.offset(literal.End()),
			text:  requote(literal.Value, destination),
			change: Change{
				Kind: ChangeImport,
				Path: file.Path,
				Line: source.line(literal.Pos()),
				From: path,
				To:   destination,
			},
		})
	}
	return edits, nil
}

// requote renders an import path in the same literal form the original used.
//
// A raw string literal is legal in an import declaration and upstream is free
// to use one. Rewriting it into an interpreted literal would be a gratuitous
// change to a byte range the transformation had no reason to reshape, and a raw
// literal round trips safely because an import path can contain neither a
// backquote nor a newline.
func requote(original, path string) string {
	if strings.HasPrefix(original, "`") {
		return "`" + path + "`"
	}
	return strconv.Quote(path)
}
