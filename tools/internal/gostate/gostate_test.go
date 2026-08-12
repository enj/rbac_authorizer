package gostate_test

import (
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/gostate"
)

// TestExportedGlobal pins the rule both policies share.
//
// The cases are written as one real package rather than as synthesized
// types.Object values, because the question being asked is about declarations
// as they are actually written, and a hand built object would let the test
// agree with the implementation about something neither would see in source.
func TestExportedGlobal(t *testing.T) {
	t.Parallel()

	const source = `package subject

import "errors"

// Registry is mutable in place.
var Registry = map[string]int{}

// Handlers is mutable in place.
var Handlers []func()

// Endpoint is basic and still rebindable by any importer.
var Endpoint = "https://example.invalid"

// Retries is basic and still rebindable.
var Retries = 3

// Enabled is basic and still rebindable.
var Enabled = false

// ErrNotFound is a sentinel, which is comparison rather than state.
var ErrNotFound = errors.New("not found")

// MaxRetries is a constant, which cannot be rebound at all.
const MaxRetries = 5

// unexported is not reachable by an importer.
var unexported = map[string]int{}

// Exported is a function, not a variable.
func Exported() {}
`

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "subject.go", source, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	config := &types.Config{Importer: importer.ForCompiler(fset, "source", nil)}
	pkg, err := config.Check("subject", fset, []*ast.File{file}, nil)
	if err != nil {
		t.Fatalf("type check: %v", err)
	}

	tests := []struct {
		name   string
		state  bool
		reason string
	}{
		{name: "Registry", state: true, reason: "shared state"},
		{name: "Handlers", state: true, reason: "shared state"},
		{name: "Endpoint", state: true, reason: "rebindable by any importer"},
		{name: "Retries", state: true, reason: "rebindable by any importer"},
		{name: "Enabled", state: true, reason: "rebindable by any importer"},
		{name: "ErrNotFound", state: false},
		{name: "MaxRetries", state: false},
		{name: "unexported", state: false},
		{name: "Exported", state: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			object := pkg.Scope().Lookup(test.name)
			if object == nil {
				t.Fatalf("package declares no %s", test.name)
			}
			reason, got := gostate.ExportedGlobal(object)
			if got != test.state {
				t.Fatalf("ExportedGlobal(%s) = %t, want %t", test.name, got, test.state)
			}
			if !test.state {
				return
			}
			if !strings.Contains(reason, test.reason) {
				t.Errorf("reason %q does not mention %q", reason, test.reason)
			}
		})
	}
}

// TestIsSentinelErrorIsExact proves the one exception stays narrow.
//
// It recognises the predeclared error interface and nothing that merely
// resembles it, because a rule that accepted anything error shaped would let a
// package hide real state behind a type named Error.
func TestIsSentinelErrorIsExact(t *testing.T) {
	t.Parallel()

	const source = `package subject

// Error looks like the predeclared error but is this package's own type.
type Error interface {
	Error() string
}

// Sentinel is the real predeclared error.
var Sentinel error

// Lookalike is the local interface.
var Lookalike Error
`

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "subject.go", source, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	pkg, err := (&types.Config{}).Check("subject", fset, []*ast.File{file}, nil)
	if err != nil {
		t.Fatalf("type check: %v", err)
	}

	if !gostate.IsSentinelError(pkg.Scope().Lookup("Sentinel").Type()) {
		t.Error("the predeclared error interface was not recognised")
	}
	if gostate.IsSentinelError(pkg.Scope().Lookup("Lookalike").Type()) {
		t.Error("a package's own error shaped interface was treated as the predeclared one")
	}
	// The lookalike is therefore still reported as state, which is the point of
	// keeping the exception narrow.
	if _, isState := gostate.ExportedGlobal(pkg.Scope().Lookup("Lookalike")); !isState {
		t.Error("an exported variable of a local error shaped interface was not reported as state")
	}
}
