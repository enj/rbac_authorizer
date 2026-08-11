package closure

import (
	"errors"
	"slices"
	"testing"
)

func TestSplitEmbedPatterns(t *testing.T) {
	tests := []struct {
		name    string
		line    string
		want    []string
		wantErr bool
	}{
		{name: "empty", line: "", want: nil},
		{name: "bare", line: "data.txt", want: []string{"data.txt"}},
		{name: "several bare", line: "a.txt  b.txt\tc.txt", want: []string{"a.txt", "b.txt", "c.txt"}},
		{name: "quoted with space", line: `"quoted name.txt"`, want: []string{"quoted name.txt"}},
		{name: "raw quoted", line: "`raw name.txt`", want: []string{"raw name.txt"}},
		{name: "mixed", line: "plain.txt \"q one.txt\" `r two.txt`", want: []string{"plain.txt", "q one.txt", "r two.txt"}},
		{name: "escaped quote", line: `"with\"quote.txt"`, want: []string{`with"quote.txt`}},
		{name: "unterminated interpreted", line: `"open`, wantErr: true},
		{name: "unterminated raw", line: "`open", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := splitEmbedPatterns(test.line)
			if test.wantErr {
				if !errors.Is(err, ErrPatternMalformed) {
					t.Fatalf("error = %v, want ErrPatternMalformed", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !slices.Equal(got, test.want) {
				t.Errorf("patterns = %q, want %q", got, test.want)
			}
		})
	}
}

func TestIsStandardImport(t *testing.T) {
	tests := []struct {
		imp  string
		want bool
	}{
		{imp: "fmt", want: true},
		{imp: "net/http", want: true},
		{imp: "crypto/x509", want: true},
		{imp: "k8s.io/api/rbac/v1", want: false},
		{imp: "github.com/enj/soapbox/tools", want: false},
		{imp: "monis.app/kk/rbac_authorizer", want: false},
		// The go command's own rule: a first element without a dot cannot be a
		// module path, so it has to be standard.
		{imp: "internal/abi", want: true},
	}
	for _, test := range tests {
		t.Run(test.imp, func(t *testing.T) {
			if got := isStandardImport(test.imp); got != test.want {
				t.Errorf("isStandardImport(%q) = %v, want %v", test.imp, got, test.want)
			}
		})
	}
}

func TestInternalDir(t *testing.T) {
	const prefix = "k8s.io/kubernetes"
	tests := []struct {
		imp        string
		wantDir    string
		wantIsIntl bool
	}{
		{imp: "k8s.io/kubernetes/pkg/apis/rbac", wantDir: "pkg/apis/rbac", wantIsIntl: true},
		{imp: "k8s.io/kubernetes", wantDir: "", wantIsIntl: true},
		// A path that merely starts with the same characters is a different
		// module and must never be relocated.
		{imp: "k8s.io/kubernetes-extra/pkg", wantIsIntl: false},
		{imp: "k8s.io/api/rbac/v1", wantIsIntl: false},
		{imp: "fmt", wantIsIntl: false},
	}
	for _, test := range tests {
		t.Run(test.imp, func(t *testing.T) {
			dir, internal := internalDir(prefix, test.imp)
			if internal != test.wantIsIntl {
				t.Fatalf("internal = %v, want %v", internal, test.wantIsIntl)
			}
			if internal && dir != test.wantDir {
				t.Errorf("dir = %q, want %q", dir, test.wantDir)
			}
		})
	}
}

func TestCountLines(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want int
	}{
		{name: "empty", src: "", want: 0},
		{name: "one terminated line", src: "package a\n", want: 1},
		{name: "one unterminated line", src: "package a", want: 1},
		{name: "several lines", src: "package a\n\nimport \"fmt\"\n", want: 3},
		{name: "trailing partial line", src: "a\nb\nc", want: 3},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := countLines([]byte(test.src)); got != test.want {
				t.Errorf("countLines(%q) = %d, want %d", test.src, got, test.want)
			}
		})
	}
}

func TestClassifyFile(t *testing.T) {
	tests := []struct {
		name         string
		includeTests bool
		wantKind     CopyKind
		wantWanted   bool
	}{
		{name: "rbac.go", wantKind: KindGo, wantWanted: true},
		{name: "rbac_test.go", wantKind: KindGoTest, wantWanted: false},
		{name: "rbac_test.go", includeTests: true, wantKind: KindGoTest, wantWanted: true},
		{name: "shim.c", wantKind: KindNative, wantWanted: true},
		{name: "shim.h", wantKind: KindHeader, wantWanted: true},
		{name: "asm_amd64.s", wantKind: KindAssembly, wantWanted: true},
		{name: "asm_amd64.S", wantKind: KindAssembly, wantWanted: true},
		{name: "prebuilt.syso", wantKind: KindObject, wantWanted: true},
		// Objective-C++ looks like it belongs beside .m and .cc, but the go tool
		// has never compiled it, so it is not a build input.
		{name: "shim.mm", wantWanted: false},
		// Repository metadata the generated module must not inherit.
		{name: "OWNERS", wantWanted: false},
		{name: "BUILD.bazel", wantWanted: false},
		{name: "README.md", wantWanted: false},
		{name: ".import-restrictions", wantWanted: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			kind, wanted := classifyFile(test.name, test.includeTests)
			if wanted != test.wantWanted {
				t.Fatalf("wanted = %v, want %v", wanted, test.wantWanted)
			}
			if wanted && kind != test.wantKind {
				t.Errorf("kind = %q, want %q", kind, test.wantKind)
			}
		})
	}
}

// TestCompanionKinds_MatchesGoBuild pins the non-Go build inputs to the exact
// set go/build recognises.
//
// The list is the one go/build's fileListForExt switch dispatches on. Anything
// the closure adds beyond it is a file no build compiles, copied into the
// generated module under the pretence that it is a build input; anything the
// closure drops is a file the upstream package needs on some platform and the
// generated module would fail to build there. Neither shows up in a test that
// only checks the extensions someone happened to think of, so the whole set is
// compared.
func TestCompanionKinds_MatchesGoBuild(t *testing.T) {
	want := []string{
		".F", ".S", ".c", ".cc", ".cpp", ".cxx", ".f", ".f90", ".for",
		".h", ".hh", ".hpp", ".hxx", ".m", ".s", ".swig", ".swigcxx", ".sx", ".syso",
	}
	if got := sortedKeys(companionKinds); !slices.Equal(got, want) {
		t.Errorf("companion extensions =\n  %q\nwant\n  %q", got, want)
	}
}

func TestValidateGlob(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		want    error
	}{
		{name: "plain path", pattern: "pkg/app/data.yaml"},
		{name: "single star", pattern: "pkg/app/*.yaml"},
		{name: "character class", pattern: "pkg/app/data[0-9].yaml"},
		{name: "recursive", pattern: "pkg/**/data.yaml", want: ErrRecursivePattern},
		{name: "unclosed class", pattern: "pkg/app/data[0-9.yaml", want: ErrPatternMalformed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateGlob(test.pattern)
			switch {
			case test.want == nil && err != nil:
				t.Errorf("unexpected error: %v", err)
			case test.want != nil && !errors.Is(err, test.want):
				t.Errorf("error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestValidateRelPath(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{name: "nested", path: "pkg/apis/rbac/v1/doc.go"},
		{name: "single element", path: "doc.go"},
		{name: "empty", path: "", wantErr: true},
		{name: "absolute", path: "/etc/passwd", wantErr: true},
		{name: "parent traversal", path: "../escape", wantErr: true},
		{name: "embedded traversal", path: "pkg/../../escape", wantErr: true},
		{name: "unclean", path: "pkg//app", wantErr: true},
		{name: "dot", path: ".", wantErr: true},
		{name: "backslash", path: `pkg\app`, wantErr: true},
		{name: "null byte", path: "pkg/app\x00.go", wantErr: true},
		{name: "trailing space", path: "pkg/app.go ", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateRelPath(test.path)
			if (err != nil) != test.wantErr {
				t.Errorf("validateRelPath(%q) = %v, want error: %v", test.path, err, test.wantErr)
			}
		})
	}
}
