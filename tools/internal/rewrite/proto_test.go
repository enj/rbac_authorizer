package rewrite_test

import (
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/rewrite"
)

// protoFile builds a proto file with an upstream identity.
func protoFile(path, contents string) rewrite.File {
	return rewrite.File{
		Path:       "internal/kk/" + path,
		SourcePath: path,
		Contents:   []byte(contents),
	}
}

func TestProtoFile(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "go_package is rewritten",
			in: "syntax = \"proto2\";\n\n" +
				"package k8s.io.kubernetes.pkg.apis.rbac.v1;\n\n" +
				"option go_package = \"k8s.io/kubernetes/pkg/apis/rbac/v1\";\n",
			want: "syntax = \"proto2\";\n\n" +
				"package k8s.io.kubernetes.pkg.apis.rbac.v1;\n\n" +
				"option go_package = \"monis.app/kk/rbac_authorizer/internal/kk/pkg/apis/rbac/v1\";\n",
		},
		{
			name: "a package name suffix survives",
			in:   "option go_package = \"k8s.io/kubernetes/pkg/apis/rbac/v1;rbacv1\";\n",
			want: "option go_package = \"monis.app/kk/rbac_authorizer/internal/kk/pkg/apis/rbac/v1;rbacv1\";\n",
		},
		{
			name: "single quotes are accepted",
			in:   "option go_package = 'k8s.io/kubernetes/pkg/apis/rbac';\n",
			want: "option go_package = 'monis.app/kk/rbac_authorizer/internal/kk/pkg/apis/rbac';\n",
		},
		{
			name: "no spacing is accepted",
			in:   "option go_package=\"k8s.io/kubernetes/pkg/apis/rbac\";\n",
			want: "option go_package=\"monis.app/kk/rbac_authorizer/internal/kk/pkg/apis/rbac\";\n",
		},
		{
			name: "a comment between the tokens is accepted",
			in:   "option /* which package */ go_package = \"k8s.io/kubernetes/pkg/apis/rbac\";\n",
			want: "option /* which package */ go_package = \"monis.app/kk/rbac_authorizer/internal/kk/pkg/apis/rbac\";\n",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			options := testOptions()
			result, err := rewrite.ProtoFile(t.Context(), protoFile("pkg/apis/rbac/v1/generated.proto", test.in), options)
			if err != nil {
				t.Fatalf("rewrite: %v", err)
			}
			if got := string(result.Contents); got != test.want {
				t.Errorf("rewrote to:\n%s\nwant:\n%s", got, test.want)
			}
		})
	}
}

// TestProtoFileLeavesOtherValuesAlone is the negative half of the proto
// contract. Every input here contains the source prefix somewhere the parser
// must refuse to treat as a go_package value.
func TestProtoFileLeavesOtherValuesAlone(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
	}{
		{
			name: "a proto package declaration",
			in:   "package k8s.io.kubernetes.pkg.apis.rbac.v1;\n",
		},
		{
			name: "an import of another proto file",
			in:   "import \"k8s.io/kubernetes/pkg/apis/rbac/generated.proto\";\n",
		},
		{
			name: "a line comment naming the option",
			in:   "// option go_package = \"k8s.io/kubernetes/pkg/apis/rbac\";\n",
		},
		{
			name: "a block comment naming the option",
			in:   "/*\noption go_package = \"k8s.io/kubernetes/pkg/apis/rbac\";\n*/\n",
		},
		{
			name: "a different option with an import like value",
			in:   "option java_package = \"k8s.io/kubernetes/pkg/apis/rbac\";\n",
		},
		{
			name: "a field default holding the option text",
			in:   "optional string doc = 1 [default = \"option go_package = k8s.io/kubernetes/pkg\"];\n",
		},
		{
			name: "an external go_package",
			in:   "option go_package = \"k8s.io/api/rbac/v1\";\n",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			result, err := rewrite.ProtoFile(t.Context(), protoFile("pkg/apis/rbac/v1/generated.proto", test.in), testOptions())
			if err != nil {
				t.Fatalf("rewrite: %v", err)
			}
			if result.Changed() {
				t.Errorf("rewrote %v", result.Changes)
			}
			if got := string(result.Contents); got != test.in {
				t.Errorf("changed bytes:\n%s\nwant:\n%s", got, test.in)
			}
		})
	}
}

func TestProtoFileNotice(t *testing.T) {
	t.Parallel()

	in := "/*\nCopyright 2016 The Kubernetes Authors.\n*/\n\n" +
		"syntax = \"proto2\";\n\n" +
		"option go_package = \"k8s.io/kubernetes/pkg/apis/rbac/v1\";\n"

	options := testOptions()
	options.NoNotice = false

	result, err := rewrite.ProtoFile(t.Context(), protoFile("pkg/apis/rbac/v1/generated.proto", in), options)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	got := string(result.Contents)
	want := "/*\nCopyright 2016 The Kubernetes Authors.\n*/\n\n" +
		"// This file was modified by soapbox and is not the upstream original.\n" +
		"// Upstream repository: https://github.com/kubernetes/kubernetes.git\n" +
		"// Upstream path: pkg/apis/rbac/v1/generated.proto\n" +
		"// Upstream commit: 0123456789abcdef0123456789abcdef01234567\n" +
		"// Imports under k8s.io/kubernetes were rewritten to monis.app/kk/rbac_authorizer/internal/kk.\n\n" +
		"syntax = \"proto2\";\n\n" +
		"option go_package = \"monis.app/kk/rbac_authorizer/internal/kk/pkg/apis/rbac/v1\";\n"
	if got != want {
		t.Errorf("rewrote to:\n%s\nwant:\n%s", got, want)
	}
}

// TestProtoFileIsIdempotent asserts a second pass over rewritten output does
// nothing, which replay depends on.
func TestProtoFileIsIdempotent(t *testing.T) {
	t.Parallel()

	file := protoFile("pkg/apis/rbac/v1/generated.proto",
		"syntax = \"proto2\";\n\noption go_package = \"k8s.io/kubernetes/pkg/apis/rbac/v1\";\n")
	options := testOptions()
	options.NoNotice = false

	first, err := rewrite.ProtoFile(t.Context(), file, options)
	if err != nil {
		t.Fatalf("first rewrite: %v", err)
	}
	again := file
	again.Contents = first.Contents
	second, err := rewrite.ProtoFile(t.Context(), again, options)
	if err != nil {
		t.Fatalf("second rewrite: %v", err)
	}
	if second.Changed() {
		t.Errorf("second rewrite changed %v", second.Changes)
	}
	if string(second.Contents) != string(first.Contents) {
		t.Errorf("second rewrite produced:\n%s\nwant:\n%s", second.Contents, first.Contents)
	}
}

func TestProtoFileReportsTheChange(t *testing.T) {
	t.Parallel()

	result, err := rewrite.ProtoFile(t.Context(), protoFile("pkg/apis/rbac/v1/generated.proto",
		"syntax = \"proto2\";\n\noption go_package = \"k8s.io/kubernetes/pkg/apis/rbac/v1\";\n"), testOptions())
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if len(result.Changes) != 1 {
		t.Fatalf("reported %v, want one change", result.Changes)
	}
	change := result.Changes[0]
	switch {
	case change.Kind != rewrite.ChangeProtoOption:
		t.Errorf("change kind %q, want %q", change.Kind, rewrite.ChangeProtoOption)
	case change.Line != 3:
		t.Errorf("change line %d, want 3", change.Line)
	case change.From != "k8s.io/kubernetes/pkg/apis/rbac/v1":
		t.Errorf("change records %q as the old value", change.From)
	case !strings.HasPrefix(change.To, "monis.app/kk/rbac_authorizer/internal/kk/"):
		t.Errorf("change records %q as the new value", change.To)
	}
}

// TestProtoFileOnlyRewritesFileScope covers option scope.
//
// go_package is a file option that names the Go import path the generated code
// lands in. A message, enum, or service body may carry options of its own, and
// a go_package written inside one is a different setting that happens to share
// a name. Rewriting it would corrupt it, and it would do so invisibly, because
// nothing downstream reads it.
func TestProtoFileOnlyRewritesFileScope(t *testing.T) {
	t.Parallel()

	in := "syntax = \"proto2\";\n\n" +
		"option go_package = \"k8s.io/kubernetes/pkg/apis/rbac/v1\";\n\n" +
		"message Role {\n" +
		"  option go_package = \"k8s.io/kubernetes/pkg/apis/rbac/v1\";\n" +
		"  optional string name = 1;\n" +
		"}\n"
	want := "syntax = \"proto2\";\n\n" +
		"option go_package = \"monis.app/kk/rbac_authorizer/internal/kk/pkg/apis/rbac/v1\";\n\n" +
		"message Role {\n" +
		"  option go_package = \"k8s.io/kubernetes/pkg/apis/rbac/v1\";\n" +
		"  optional string name = 1;\n" +
		"}\n"

	options := testOptions()
	result, err := rewrite.ProtoFile(t.Context(), protoFile("pkg/apis/rbac/v1/generated.proto", in), options)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if got := string(result.Contents); got != want {
		t.Errorf("rewrote to:\n%s\nwant:\n%s", got, want)
	}
	if len(result.Changes) != 1 {
		t.Errorf("recorded %d changes, want only the file level option: %v", len(result.Changes), result.Changes)
	}
}

// TestProtoFilePreservesIneligibleOptions asserts that a go_package belonging
// to another module keeps the identity its generated Go package is compiled
// under. Relocating it would produce a package that no longer matches the one
// its consumers import.
func TestProtoFilePreservesIneligibleOptions(t *testing.T) {
	t.Parallel()

	in := "syntax = \"proto2\";\n\n" +
		"option go_package = \"k8s.io/api/rbac/v1\";\n\n" +
		"message Role {\n  optional string name = 1;\n}\n"

	result, err := rewrite.ProtoFile(t.Context(), protoFile("pkg/apis/rbac/v1/generated.proto", in), testOptions())
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if got := string(result.Contents); got != in {
		t.Errorf("rewrote an ineligible option:\n%s\nwant it untouched:\n%s", got, in)
	}
	if result.Changed() {
		t.Errorf("recorded changes for an untouched file: %v", result.Changes)
	}
}
