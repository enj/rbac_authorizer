package rewrite

import (
	"go/ast"
	"go/parser"
	"go/token"
)

// generatedParseMode reads exactly as much of a file as the convention needs.
//
// The package clause bounds the search, so nothing after it has to parse, and a
// file whose body is broken still answers correctly. That matters because the
// flag travels with a file from the moment the closure selects it, which is
// before anything has proved the file compiles.
const generatedParseMode = parser.PackageClauseOnly | parser.ParseComments | parser.SkipObjectResolution

// Generated reports whether a Go source file carries the generated file marker.
//
// The convention is the one the Go project publishes at https://go.dev/s/generatedcode:
// a line comment matching `^// Code generated .* DO NOT EDIT\.$` appearing
// before the package clause. It is applied here through go/ast rather than by
// matching text, so the answer is the one every other Go tool would give: a
// marker inside a block comment does not count, neither does one after the
// package clause, and neither does a line with trailing text.
//
// The flag matters because a generated file is not maintained by hand. Later
// steps keep the marker in the position tooling recognises, provenance records
// which relocated files were generated upstream, and a reviewer reading the
// generated module can tell copied source from copied output.
//
// Anything that is not parseable Go is not generated Go, so a file the parser
// cannot find a package clause in reports false rather than an error. Callers
// classify assets and protocol buffer definitions through this same function
// and need an answer for every file in a copy plan.
func Generated(src []byte) bool {
	file, err := parser.ParseFile(token.NewFileSet(), "", src, generatedParseMode)
	if err != nil || file == nil {
		return false
	}
	return ast.IsGenerated(file)
}
