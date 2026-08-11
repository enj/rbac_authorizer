package rewrite

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// The verification path only fires when a transformation is wrong, so it cannot
// be reached through the public entry points: every transformation this package
// performs is correct by construction. These tests drive the checks directly
// with deliberately wrong output, which is the only way to prove the safety net
// would catch a future transformation that is not.

func TestVerifyShape(t *testing.T) {
	t.Parallel()

	const (
		eligible  = "k8s.io/kubernetes/pkg/apis/rbac"
		relocated = "monis.app/kk/rbac_authorizer/internal/kk/pkg/apis/rbac"
		external  = "k8s.io/api/rbac/v1"
	)
	original := "package v1\n\n" +
		"import (\n" +
		"\t\"" + eligible + "\"\n" +
		"\trbacv1 \"" + external + "\"\n" +
		")\n\n" +
		"// Resolve resolves rules.\n" +
		"func Resolve(name string) int {\n" +
		"\tif name == \"admin\" {\n" +
		"\t\treturn 1\n" +
		"\t}\n" +
		"\treturn 0\n" +
		"}\n"

	// rewritten is the reported, legitimate rewrite every case starts from.
	rewritten := strings.Replace(original, "\""+eligible+"\"", "\""+relocated+"\"", 1)
	reported := []Change{{Kind: ChangeImport, Path: "v1/doc.go", From: eligible, To: relocated}}

	tests := []struct {
		name    string
		out     string
		changes []Change
		wantErr bool
	}{
		{
			name:    "a reported import path rewrite is permitted",
			out:     rewritten,
			changes: reported,
		},
		{
			name:    "a comment change is not part of the shape",
			out:     strings.Replace(rewritten, "// Resolve resolves rules.", "// Resolve was relocated.", 1),
			changes: reported,
		},
		{
			// The external import keeps the identity the upstream types are
			// defined with. Relocating it would compile and would produce a type
			// that no longer equals the one it must satisfy, so the corruption
			// has to be caught here.
			name: "a corrupted external import is caught",
			out: strings.Replace(rewritten, "\""+external+"\"",
				"\"monis.app/kk/rbac_authorizer/internal/kk/vendor/k8s.io/api/rbac/v1\"", 1),
			changes: reported,
			wantErr: true,
		},
		{
			name:    "an unreported import path rewrite is caught",
			out:     strings.Replace(original, "\""+eligible+"\"", "\""+relocated+"\"", 1),
			changes: nil,
			wantErr: true,
		},
		{
			name:    "a rewrite to a path other than the reported one is caught",
			out:     strings.Replace(original, "\""+eligible+"\"", "\"monis.app/kk/rbac_authorizer/internal/kk/pkg/apis/rbac/v2\"", 1),
			changes: reported,
			wantErr: true,
		},
		{
			name:    "a changed import alias is caught",
			out:     strings.Replace(rewritten, "rbacv1 \"", "rbacv2 \"", 1),
			changes: reported,
			wantErr: true,
		},
		{
			name:    "a dropped import is caught",
			out:     strings.Replace(rewritten, "\trbacv1 \""+external+"\"\n", "", 1),
			changes: reported,
			wantErr: true,
		},
		{
			name:    "a changed identifier is caught",
			out:     strings.Replace(rewritten, "func Resolve(", "func resolve(", 1),
			changes: reported,
			wantErr: true,
		},
		{
			name:    "a changed string literal is caught",
			out:     strings.Replace(rewritten, "\"admin\"", "\"root\"", 1),
			changes: reported,
			wantErr: true,
		},
		{
			name:    "a changed numeric literal is caught",
			out:     strings.Replace(rewritten, "return 1", "return 2", 1),
			changes: reported,
			wantErr: true,
		},
		{
			name:    "a changed operator is caught",
			out:     strings.Replace(rewritten, "name ==", "name !=", 1),
			changes: reported,
			wantErr: true,
		},
		{
			name:    "a dropped statement is caught",
			out:     strings.Replace(rewritten, "\t\treturn 1\n", "", 1),
			changes: reported,
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			file := File{Path: "internal/kk/pkg/apis/rbac/v1/doc.go", Contents: []byte(original)}
			err := verifyShape(file, []byte(test.out), test.changes)
			switch {
			case test.wantErr && err == nil:
				t.Error("verification accepted a changed syntax tree")
			case test.wantErr && !strings.Contains(err.Error(), ErrShapeChanged.Error()):
				t.Errorf("verification error %v, want %v", err, ErrShapeChanged)
			case !test.wantErr && err != nil:
				t.Errorf("verification rejected a permitted change: %v", err)
			}
		})
	}
}

func TestVerifyComments(t *testing.T) {
	t.Parallel()

	original := "// +k8s:deepcopy-gen=package\n" +
		"// +groupName=rbac.authorization.k8s.io\n" +
		"package v1\n"

	tests := []struct {
		name    string
		out     string
		changes []Change
		wantErr string
	}{
		{
			name:    "a reported removal is accepted",
			out:     "// +groupName=rbac.authorization.k8s.io\npackage v1\n",
			changes: []Change{{Kind: ChangeMarkerRemoval, From: "// +k8s:deepcopy-gen=package"}},
		},
		{
			name: "a reported rewrite is accepted",
			out: "// +k8s:deepcopy-gen=relocated\n" +
				"// +groupName=rbac.authorization.k8s.io\npackage v1\n",
			changes: []Change{{
				Kind: ChangeDirective,
				From: "// +k8s:deepcopy-gen=package",
				To:   "// +k8s:deepcopy-gen=relocated",
			}},
		},
		{
			name:    "a reported notice is accepted",
			out:     "// added by soapbox\n\n" + original,
			changes: []Change{{Kind: ChangeNotice, To: "// added by soapbox\n\n"}},
		},
		{
			name:    "an unreported removal is caught",
			out:     "// +groupName=rbac.authorization.k8s.io\npackage v1\n",
			wantErr: "lost // +k8s:deepcopy-gen=package",
		},
		{
			name:    "an unreported addition is caught",
			out:     "// surprise\n" + original,
			wantErr: "added // surprise",
		},
		{
			name: "a removal reported for the wrong comment is caught",
			out:  "// +groupName=rbac.authorization.k8s.io\npackage v1\n",
			changes: []Change{{
				Kind: ChangeMarkerRemoval,
				From: "// +groupName=rbac.authorization.k8s.io",
			}},
			wantErr: "lost // +k8s:deepcopy-gen=package",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			file := File{Path: "internal/kk/pkg/apis/rbac/v1/doc.go", Contents: []byte(original)}
			err := verifyComments(file, parseInternal(t, original), parseInternal(t, test.out), test.changes)
			switch {
			case test.wantErr == "" && err != nil:
				t.Errorf("verification rejected a reported change: %v", err)
			case test.wantErr != "" && err == nil:
				t.Error("verification accepted an unreported comment change")
			case test.wantErr != "" && err != nil && !strings.Contains(err.Error(), test.wantErr):
				t.Errorf("verification error %v, want it to mention %q", err, test.wantErr)
			}
		})
	}
}

// TestApplyEditsRejectsOverlap asserts that two transformations claiming the
// same bytes fail instead of racing. The result of an overlap would depend on
// the order the claims were discovered in, which is exactly the determinism
// replay cannot give up.
func TestApplyEditsRejectsOverlap(t *testing.T) {
	t.Parallel()

	src := []byte("package v1\n")
	_, _, err := applyEdits(src, []edit{
		{start: 0, end: 7, text: "x"},
		{start: 3, end: 9, text: "y"},
	})
	if err == nil {
		t.Fatal("overlapping edits were applied")
	}
	if !strings.Contains(err.Error(), ErrOverlappingEdits.Error()) {
		t.Errorf("error %v, want %v", err, ErrOverlappingEdits)
	}
}

// TestApplyEditsKeepsOriginalSliceWhenEmpty asserts an unchanged file is
// returned as is, so no file can pick up a copy that differs by a byte.
func TestApplyEditsKeepsOriginalSliceWhenEmpty(t *testing.T) {
	t.Parallel()

	src := []byte("package v1\n")
	out, changes, err := applyEdits(src, nil)
	if err != nil {
		t.Fatalf("apply edits: %v", err)
	}
	if len(changes) != 0 {
		t.Errorf("reported %v", changes)
	}
	if &out[0] != &src[0] {
		t.Error("an unchanged file was copied")
	}
}

func TestParseDirective(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		text      string
		wantOK    bool
		wantKind  DirectiveKind
		wantKey   string
		wantValue string
	}{
		{
			name: "ordinary prose is not a directive",
			text: "// Resolve resolves rules.",
		},
		{
			name: "a go directive needs no space",
			text: "// go:generate deepcopy-gen",
		},
		{
			name:     "a marker without a value",
			text:     "// +optional",
			wantOK:   true,
			wantKind: MarkerDirective,
			wantKey:  "optional",
		},
		{
			name:     "a marker without a space",
			text:     "//+optional",
			wantOK:   true,
			wantKind: MarkerDirective,
			wantKey:  "optional",
		},
		{
			name:      "a namespaced marker with a value",
			text:      "// +k8s:conversion-gen=k8s.io/kubernetes/pkg/apis/rbac",
			wantOK:    true,
			wantKind:  MarkerDirective,
			wantKey:   "k8s:conversion-gen",
			wantValue: "k8s.io/kubernetes/pkg/apis/rbac",
		},
		{
			name:      "a deeply namespaced marker",
			text:      "// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object",
			wantOK:    true,
			wantKind:  MarkerDirective,
			wantKey:   "k8s:deepcopy-gen:interfaces",
			wantValue: "k8s.io/apimachinery/pkg/runtime.Object",
		},
		{
			name:      "a legacy build constraint is a marker",
			text:      "// +build linux",
			wantOK:    true,
			wantKind:  MarkerDirective,
			wantKey:   "build",
			wantValue: "linux",
		},
		{
			name:      "a toolchain directive",
			text:      "//go:generate deepcopy-gen -i ./...",
			wantOK:    true,
			wantKind:  GoDirective,
			wantKey:   "go:generate",
			wantValue: "deepcopy-gen -i ./...",
		},
		{
			name:      "a build constraint",
			text:      "//go:build linux && !race",
			wantOK:    true,
			wantKind:  GoDirective,
			wantKey:   "go:build",
			wantValue: "linux && !race",
		},
		{
			name:     "a toolchain directive without a value",
			text:     "//go:noinline",
			wantOK:   true,
			wantKind: GoDirective,
			wantKey:  "go:noinline",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			directive, ok := parseDirective(test.text)
			if ok != test.wantOK {
				t.Fatalf("parsed %t, want %t", ok, test.wantOK)
			}
			if !ok {
				return
			}
			switch {
			case directive.Kind != test.wantKind:
				t.Errorf("kind %d, want %d", directive.Kind, test.wantKind)
			case directive.Key != test.wantKey:
				t.Errorf("key %q, want %q", directive.Key, test.wantKey)
			case directive.Value != test.wantValue:
				t.Errorf("value %q, want %q", directive.Value, test.wantValue)
			}
		})
	}
}

// TestProtectedDirectives asserts that the directives which decide how a file
// builds can never be reached by a rule.
func TestProtectedDirectives(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		text string
		want bool
	}{
		{text: "//go:build linux", want: true},
		{text: "// +build linux", want: true},
		{text: "//go:embed schema.json", want: true},
		{text: "//go:noinline", want: true},
		{text: "//go:linkname probe runtime.probe", want: true},
		{text: "//go:generate deepcopy-gen", want: false},
		{text: "// +k8s:deepcopy-gen=package", want: false},
		{text: "// +groupName=rbac.authorization.k8s.io", want: false},
	} {
		directive, ok := parseDirective(test.text)
		if !ok {
			t.Fatalf("%q did not parse as a directive", test.text)
		}
		if got := protected(directive); got != test.want {
			t.Errorf("%q protected %t, want %t", test.text, got, test.want)
		}
	}
}

// parseInternal parses a file with its comments for a verification test.
func parseInternal(t *testing.T, src string) *ast.File {
	t.Helper()
	parsed, err := parser.ParseFile(token.NewFileSet(), "internal.go", src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return parsed
}

// TestVerifyAttachment drives the documentation attachment check directly.
//
// A comment that gains a declaration is invisible to a comparison of comment
// texts: every byte of it stayed where it was, and only the blank line beneath
// it went away. The check is the safety net for a future absorption rule that
// takes one line too many, so it is exercised here with output no current
// transformation produces.
func TestVerifyAttachment(t *testing.T) {
	t.Parallel()

	const (
		floating = "package v1\n\n// Note.\n//go:generate a\n\ntype Rule struct{}\n"
		attached = "package v1\n\n// Rule is a rule.\n//go:generate a\ntype Rule struct{}\n"
	)
	removal := []Change{{Kind: ChangeMarkerRemoval, From: "//go:generate a"}}

	tests := []struct {
		name    string
		before  string
		after   string
		changes []Change
		wantErr bool
	}{
		{
			name:    "a floating comment that stays floating is accepted",
			before:  floating,
			after:   "package v1\n\n// Note.\n\ntype Rule struct{}\n",
			changes: removal,
		},
		{
			name:    "a documentation comment that stays attached is accepted",
			before:  attached,
			after:   "package v1\n\n// Rule is a rule.\ntype Rule struct{}\n",
			changes: removal,
		},
		{
			// The absorbed blank line is the whole change, and it hands Note to
			// Rule as its godoc.
			name:    "a promoted floating comment is caught",
			before:  floating,
			after:   "package v1\n\n// Note.\ntype Rule struct{}\n",
			changes: removal,
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			file := File{Path: "internal/kk/pkg/apis/rbac/v1/x.go", Contents: []byte(test.before)}
			err := verifyAttachment(file, parseInternal(t, test.before), parseInternal(t, test.after), test.changes)
			switch {
			case test.wantErr && err == nil:
				t.Error("verification accepted a promoted comment")
			case test.wantErr && !strings.Contains(err.Error(), ErrCommentsChanged.Error()):
				t.Errorf("verification error %v, want %v", err, ErrCommentsChanged)
			case !test.wantErr && err != nil:
				t.Errorf("verification rejected an unchanged attachment: %v", err)
			}
		})
	}
}

// TestVerifyProto drives the proto verification directly with output no current
// transformation produces, which is the only way to reach a check that exists
// for a future one.
func TestVerifyProto(t *testing.T) {
	t.Parallel()

	const (
		eligible = "k8s.io/kubernetes/pkg/apis/rbac/v1"
		internal = "monis.app/kk/rbac_authorizer/internal/kk/pkg/apis/rbac/v1"
		external = "k8s.io/api/rbac/v1"
	)
	original := "syntax = \"proto2\";\n\n" +
		"option go_package = \"" + eligible + "\";\n\n" +
		"message Role {\n  optional string name = 1;\n}\n"
	rewritten := strings.Replace(original, eligible, internal, 1)
	reported := []Change{{Kind: ChangeProtoOption, From: eligible, To: internal}}

	tests := []struct {
		name    string
		out     string
		changes []Change
		wantErr bool
	}{
		{
			name:    "a reported rewrite is accepted",
			out:     rewritten,
			changes: reported,
		},
		{
			name:    "an unreported rewrite is caught",
			out:     rewritten,
			changes: nil,
			wantErr: true,
		},
		{
			name:    "a rewrite to a value other than the reported one is caught",
			out:     strings.Replace(original, eligible, external, 1),
			changes: reported,
			wantErr: true,
		},
		{
			name:    "a dropped option is caught",
			out:     strings.Replace(rewritten, "option go_package = \""+internal+"\";\n\n", "", 1),
			changes: reported,
			wantErr: true,
		},
		{
			// An option the rewrite never claimed has to survive byte for byte,
			// which a check that only asked whether the intended values arrived
			// would not notice.
			name: "a corrupted ineligible option is caught",
			out: "syntax = \"proto2\";\n\noption go_package = \"" + external + "\";\n\n" +
				"message Role {\n  optional string name = 1;\n}\n",
			changes: nil,
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			file := File{Path: "internal/kk/pkg/apis/rbac/v1/generated.proto", Contents: []byte(original)}
			err := verifyProto(file, []byte(test.out), test.changes)
			switch {
			case test.wantErr && err == nil:
				t.Error("verification accepted an unreported option change")
			case test.wantErr && !strings.Contains(err.Error(), ErrShapeChanged.Error()):
				t.Errorf("verification error %v, want %v", err, ErrShapeChanged)
			case !test.wantErr && err != nil:
				t.Errorf("verification rejected a reported change: %v", err)
			}
		})
	}
}
