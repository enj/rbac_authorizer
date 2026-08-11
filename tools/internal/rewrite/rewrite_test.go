package rewrite_test

import (
	"bytes"
	"context"
	"errors"
	"go/format"
	"slices"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/rewrite"
)

// testOptions is the RBAC profile's mapping, which every test starts from.
func testOptions() rewrite.Options {
	return rewrite.Options{
		SourcePrefix:      "k8s.io/kubernetes",
		DestinationModule: "monis.app/kk/rbac_authorizer",
		InternalPrefix:    "internal/kk",
		SourceRepository:  "https://github.com/kubernetes/kubernetes.git",
		SourceSHA:         "0123456789abcdef0123456789abcdef01234567",
		Directives:        rewrite.DefaultRules(),
		NoNotice:          true,
	}
}

// goFile builds a file with an upstream identity.
func goFile(path, contents string) rewrite.File {
	return rewrite.File{
		Path:       "internal/kk/" + path,
		SourcePath: path,
		Contents:   []byte(contents),
	}
}

// assertGofmt asserts that the pinned formatter would not change the output,
// which is the property the generated module's gofmt gate depends on.
func assertGofmt(t *testing.T, src []byte) {
	t.Helper()
	formatted, err := format.Source(src)
	if err != nil {
		t.Fatalf("gofmt rejected the output: %v\n%s", err, src)
	}
	if !bytes.Equal(formatted, src) {
		t.Errorf("gofmt would rewrite the output:\ngot:\n%s\nwant:\n%s", src, formatted)
	}
}

func TestGoFileRewritesImports(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
		// skipGofmt marks an input that gofmt itself would reshape, so the
		// output cannot be gofmt clean no matter what the rewrite does.
		skipGofmt bool
	}{
		{
			name: "single import",
			in: "package rbac\n\n" +
				"import \"k8s.io/kubernetes/pkg/apis/rbac/v1\"\n",
			want: "package rbac\n\n" +
				"import \"monis.app/kk/rbac_authorizer/internal/kk/pkg/apis/rbac/v1\"\n",
		},
		{
			name: "the source prefix itself",
			in: "package rbac\n\n" +
				"import \"k8s.io/kubernetes\"\n",
			want: "package rbac\n\n" +
				"import \"monis.app/kk/rbac_authorizer/internal/kk\"\n",
		},
		{
			name: "aliased import keeps its alias",
			in: "package rbac\n\n" +
				"import rbacv1 \"k8s.io/kubernetes/pkg/apis/rbac/v1\"\n",
			want: "package rbac\n\n" +
				"import rbacv1 \"monis.app/kk/rbac_authorizer/internal/kk/pkg/apis/rbac/v1\"\n",
		},
		{
			name: "blank import keeps its underscore",
			in: "package rbac\n\n" +
				"import _ \"k8s.io/kubernetes/pkg/apis/rbac/install\"\n",
			want: "package rbac\n\n" +
				"import _ \"monis.app/kk/rbac_authorizer/internal/kk/pkg/apis/rbac/install\"\n",
		},
		{
			name: "dot import keeps its dot",
			in: "package rbac\n\n" +
				"import . \"k8s.io/kubernetes/pkg/apis/rbac\"\n",
			want: "package rbac\n\n" +
				"import . \"monis.app/kk/rbac_authorizer/internal/kk/pkg/apis/rbac\"\n",
		},
		{
			// gofmt normalises a raw import literal into an interpreted one.
			// The rewrite keeps the form it was given, because reshaping a
			// literal it was not asked to reshape is exactly the gratuitous
			// change byte range replacement exists to avoid.
			name: "raw string literal stays raw",
			in: "package rbac\n\n" +
				"import `k8s.io/kubernetes/pkg/apis/rbac`\n",
			want: "package rbac\n\n" +
				"import `monis.app/kk/rbac_authorizer/internal/kk/pkg/apis/rbac`\n",
			skipGofmt: true,
		},
		{
			name: "grouped imports keep their grouping, order, and comments",
			in: "package rbac\n\n" +
				"import (\n" +
				"\t\"context\"\n" +
				"\n" +
				"\trbacv1 \"k8s.io/api/rbac/v1\"\n" +
				"\t\"k8s.io/apiserver/pkg/authentication/user\"\n" +
				"\n" +
				"\t// the internal validation helpers\n" +
				"\t\"k8s.io/kubernetes/pkg/registry/rbac/validation\" // resolver\n" +
				")\n",
			want: "package rbac\n\n" +
				"import (\n" +
				"\t\"context\"\n" +
				"\n" +
				"\trbacv1 \"k8s.io/api/rbac/v1\"\n" +
				"\t\"k8s.io/apiserver/pkg/authentication/user\"\n" +
				"\n" +
				"\t// the internal validation helpers\n" +
				"\t\"monis.app/kk/rbac_authorizer/internal/kk/pkg/registry/rbac/validation\" // resolver\n" +
				")\n",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			result, err := rewrite.GoFile(t.Context(), goFile("plugin/pkg/auth/authorizer/rbac/rbac.go", test.in), testOptions())
			if err != nil {
				t.Fatalf("rewrite: %v", err)
			}
			if got := string(result.Contents); got != test.want {
				t.Errorf("rewrote to:\n%s\nwant:\n%s", got, test.want)
			}
			if !test.skipGofmt {
				assertGofmt(t, result.Contents)
			}
		})
	}
}

// TestGoFileLeavesIneligibleBytesAlone is the negative half of the contract.
// Everything here shares text with an eligible import and must survive byte for
// byte, because rewriting any of it would break a type identity, an API group,
// or a piece of documentation.
func TestGoFileLeavesIneligibleBytesAlone(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
	}{
		{
			name: "external module imports",
			in: "package rbac\n\n" +
				"import (\n" +
				"\trbacv1 \"k8s.io/api/rbac/v1\"\n" +
				"\t\"k8s.io/apiserver/pkg/authorization/authorizer\"\n" +
				"\t\"k8s.io/apimachinery/pkg/runtime\"\n" +
				")\n",
		},
		{
			name: "a module that only shares a textual prefix",
			in: "package rbac\n\n" +
				"import \"k8s.io/kubernetes-extra/pkg/thing\"\n",
		},
		{
			name: "string literals that read like import paths",
			in: "package rbac\n\n" +
				"const (\n" +
				"\tsource = \"k8s.io/kubernetes/pkg/apis/rbac\"\n" +
				"\tgroup  = \"rbac.authorization.k8s.io\"\n" +
				")\n\n" +
				"func annotation() string { return \"k8s.io/kubernetes/plugin\" }\n",
		},
		{
			name: "comments that quote import paths",
			in: "package rbac\n\n" +
				"// This resolver used to live in k8s.io/kubernetes/pkg/registry/rbac.\n" +
				"// See \"k8s.io/kubernetes/pkg/apis/rbac/v1\" for the helpers.\n" +
				"type Resolver struct{}\n",
		},
		{
			name: "a struct tag holding an import like path",
			in: "package rbac\n\n" +
				"type Rule struct {\n" +
				"\tName string `json:\"name\" doc:\"k8s.io/kubernetes/pkg/apis/rbac\"`\n" +
				"}\n",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			file := goFile("plugin/pkg/auth/authorizer/rbac/rbac.go", test.in)
			result, err := rewrite.GoFile(t.Context(), file, testOptions())
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

// TestGoFileIsIdempotent asserts that rewriting an already rewritten file does
// nothing. Replay reruns the pipeline over the same source commits, so a
// transformation that kept firing would produce a different tree on the second
// run and break the determinism the published tags depend on.
func TestGoFileIsIdempotent(t *testing.T) {
	t.Parallel()

	file := goFile("plugin/pkg/auth/authorizer/rbac/rbac.go",
		"/*\nCopyright 2016 The Kubernetes Authors.\n*/\n\n"+
			"// +k8s:deepcopy-gen=package\n\n"+
			"package rbac\n\n"+
			"import (\n"+
			"\trbacv1 \"k8s.io/api/rbac/v1\"\n"+
			"\t\"k8s.io/kubernetes/pkg/registry/rbac/validation\"\n"+
			")\n\n"+
			"var _ = validation.NewDefaultRuleResolver\n"+
			"var _ rbacv1.PolicyRule\n")

	options := testOptions()
	options.NoNotice = false

	first, err := rewrite.GoFile(t.Context(), file, options)
	if err != nil {
		t.Fatalf("first rewrite: %v", err)
	}
	if !first.Changed() {
		t.Fatal("first rewrite changed nothing")
	}
	assertGofmt(t, first.Contents)

	again := file
	again.Contents = first.Contents
	second, err := rewrite.GoFile(t.Context(), again, options)
	if err != nil {
		t.Fatalf("second rewrite: %v", err)
	}
	if second.Changed() {
		t.Errorf("second rewrite changed %v", second.Changes)
	}
	if !bytes.Equal(second.Contents, first.Contents) {
		t.Errorf("second rewrite produced:\n%s\nwant:\n%s", second.Contents, first.Contents)
	}
}

// TestGoFilePreservesCgo covers the file shape that a printer based rewrite
// destroys: the preamble comment is load bearing C source, and the pseudo
// package "C" must never be treated as an import path.
func TestGoFilePreservesCgo(t *testing.T) {
	t.Parallel()

	in := "//go:build linux\n\n" +
		"package rbac\n\n" +
		"/*\n" +
		"#include <stdlib.h>\n" +
		"#include \"k8s.io/kubernetes/helper.h\"\n" +
		"static int probe(void) { return 1; }\n" +
		"*/\n" +
		"import \"C\"\n\n" +
		"import \"k8s.io/kubernetes/pkg/registry/rbac/validation\"\n\n" +
		"var _ = validation.NewDefaultRuleResolver\n\n" +
		"func probe() int { return int(C.probe()) }\n"

	result, err := rewrite.GoFile(t.Context(), goFile("plugin/pkg/auth/authorizer/rbac/cgo.go", in), testOptions())
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	got := string(result.Contents)
	for _, want := range []string{
		"//go:build linux\n",
		"#include <stdlib.h>\n",
		"#include \"k8s.io/kubernetes/helper.h\"\n",
		"static int probe(void) { return 1; }\n",
		"import \"C\"\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lost %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "\"monis.app/kk/rbac_authorizer/internal/kk/pkg/registry/rbac/validation\"") {
		t.Errorf("the eligible import was not rewritten:\n%s", got)
	}
}

func TestGoFileLineEndingPolicy(t *testing.T) {
	t.Parallel()

	crlf := "package rbac\r\n\r\nimport \"k8s.io/kubernetes/pkg/apis/rbac\"\r\n"

	t.Run("rejected by default", func(t *testing.T) {
		t.Parallel()
		_, err := rewrite.GoFile(t.Context(), goFile("pkg/a/a.go", crlf), testOptions())
		if !errors.Is(err, rewrite.ErrCarriageReturn) {
			t.Errorf("rewrite error %v, want %v", err, rewrite.ErrCarriageReturn)
		}
	})

	t.Run("preserved when configured", func(t *testing.T) {
		t.Parallel()
		options := testOptions()
		options.LineEndings = rewrite.LineEndingPreserve
		result, err := rewrite.GoFile(t.Context(), goFile("pkg/a/a.go", crlf), options)
		if err != nil {
			t.Fatalf("rewrite: %v", err)
		}
		want := "package rbac\r\n\r\nimport \"monis.app/kk/rbac_authorizer/internal/kk/pkg/apis/rbac\"\r\n"
		if got := string(result.Contents); got != want {
			t.Errorf("rewrote to %q, want %q", got, want)
		}
	})
}

func TestGoFileRejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*rewrite.Options)
		wantErr error
	}{
		{
			name:    "source prefix that is not a module path",
			mutate:  func(o *rewrite.Options) { o.SourcePrefix = "kubernetes" },
			wantErr: rewrite.ErrPrefix,
		},
		{
			name:    "destination module that is not a module path",
			mutate:  func(o *rewrite.Options) { o.DestinationModule = "rbac_authorizer" },
			wantErr: rewrite.ErrPrefix,
		},
		{
			name:    "internal prefix that traverses",
			mutate:  func(o *rewrite.Options) { o.InternalPrefix = "../internal/kk" },
			wantErr: rewrite.ErrPrefix,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			options := testOptions()
			test.mutate(&options)
			_, err := rewrite.GoFile(t.Context(), goFile("pkg/a/a.go", "package a\n"), options)
			if !errors.Is(err, test.wantErr) {
				t.Errorf("rewrite error %v, want %v", err, test.wantErr)
			}
		})
	}
}

func TestGoFileRejectsUnparseableSource(t *testing.T) {
	t.Parallel()

	_, err := rewrite.GoFile(t.Context(), goFile("pkg/a/a.go", "package a\n\nfunc broken( {\n"), testOptions())
	if err == nil {
		t.Fatal("rewrite accepted a file that does not parse")
	}
	if !strings.Contains(err.Error(), "parse") {
		t.Errorf("error %v does not report a parse failure", err)
	}
}

func TestGoFileHonoursCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := rewrite.GoFile(ctx, goFile("pkg/a/a.go", "package a\n"), testOptions())
	if !errors.Is(err, context.Canceled) {
		t.Errorf("rewrite error %v, want %v", err, context.Canceled)
	}
}

// TestNoticeAdoptsTheFileLineEnding covers the notice under the preserving
// policy.
//
// A notice written with line feeds into a file that uses carriage returns
// leaves one file with two conventions, which is the state the preserving
// policy exists to avoid producing. The notice adopts whatever the file already
// uses instead.
func TestNoticeAdoptsTheFileLineEnding(t *testing.T) {
	t.Parallel()

	options := testOptions()
	options.LineEndings = rewrite.LineEndingPreserve
	options.NoNotice = false

	t.Run("go file", func(t *testing.T) {
		t.Parallel()
		in := "package rbac\r\n\r\nimport \"k8s.io/kubernetes/pkg/apis/rbac\"\r\n"
		result, err := rewrite.GoFile(t.Context(), goFile("pkg/a/a.go", in), options)
		if err != nil {
			t.Fatalf("rewrite: %v", err)
		}
		assertNoBareLineFeed(t, result.Contents)
		if !strings.Contains(string(result.Contents), "not the upstream original.\r\n") {
			t.Errorf("the notice does not end its lines with CRLF:\n%q", result.Contents)
		}
	})

	t.Run("proto file", func(t *testing.T) {
		t.Parallel()
		in := "syntax = \"proto2\";\r\n\r\noption go_package = \"k8s.io/kubernetes/pkg/apis/rbac/v1\";\r\n"
		result, err := rewrite.ProtoFile(t.Context(), protoFile("pkg/apis/rbac/v1/generated.proto", in), options)
		if err != nil {
			t.Fatalf("rewrite: %v", err)
		}
		assertNoBareLineFeed(t, result.Contents)
	})
}

// assertNoBareLineFeed asserts every line of a CRLF file still ends with CRLF.
func assertNoBareLineFeed(t *testing.T, src []byte) {
	t.Helper()
	for i, b := range src {
		if b == '\n' && (i == 0 || src[i-1] != '\r') {
			t.Fatalf("byte %d is a line feed with no carriage return before it:\n%q", i, src)
		}
	}
}

// TestRejectsMixedLineEndings covers a file that terminates some lines with
// CRLF and others with LF. An inserted notice would have to pick one convention
// and would make the file inconsistent whichever it picked, so the file is
// refused instead.
func TestRejectsMixedLineEndings(t *testing.T) {
	t.Parallel()

	options := testOptions()
	options.LineEndings = rewrite.LineEndingPreserve

	t.Run("go file", func(t *testing.T) {
		t.Parallel()
		in := "package rbac\r\n\nimport \"k8s.io/kubernetes/pkg/apis/rbac\"\r\n"
		_, err := rewrite.GoFile(t.Context(), goFile("pkg/a/a.go", in), options)
		if !errors.Is(err, rewrite.ErrMixedLineEndings) {
			t.Errorf("rewrite error %v, want %v", err, rewrite.ErrMixedLineEndings)
		}
	})

	t.Run("proto file", func(t *testing.T) {
		t.Parallel()
		in := "syntax = \"proto2\";\r\n\noption go_package = \"k8s.io/kubernetes/pkg/apis/rbac/v1\";\r\n"
		_, err := rewrite.ProtoFile(t.Context(), protoFile("pkg/apis/rbac/v1/generated.proto", in), options)
		if !errors.Is(err, rewrite.ErrMixedLineEndings) {
			t.Errorf("rewrite error %v, want %v", err, rewrite.ErrMixedLineEndings)
		}
	})
}

// TestChangeReportIsDeterministic runs one file that exercises every change
// kind and asserts the rewrite is reproducible.
//
// The change report is rendered into the provenance record that ships in the
// generated module, so a report whose order depended on map iteration would
// show up as a diff in a release that transformed nothing new. The file also
// has to be idempotent: replay reruns the transformation over commits that
// already passed through it, and a second pass that found more to do would make
// a replayed tag differ from the tag it replays.
func TestChangeReportIsDeterministic(t *testing.T) {
	t.Parallel()

	in := "/*\nCopyright 2016 The Kubernetes Authors.\n*/\n\n" +
		"// Package v1 holds the external types.\n" +
		"//\n" +
		"//go:generate deepcopy-gen -i ./...\n" +
		"package v1\n\n" +
		"import (\n" +
		"\trbacv1 \"k8s.io/api/rbac/v1\"\n" +
		"\t\"k8s.io/kubernetes/pkg/apis/rbac\"\n" +
		")\n\n" +
		"// +k8s:conversion-gen=k8s.io/kubernetes/pkg/apis/rbac\n" +
		"type Rule struct{}\n"
	assertGofmt(t, []byte(in))

	options := noticedOptions()
	file := goFile("pkg/apis/rbac/v1/types.go", in)

	first, err := rewrite.GoFile(t.Context(), file, options)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	assertGofmt(t, first.Contents)

	// Every kind the report can carry is present, so the ordering assertions
	// below are exercised against a mixed report rather than a uniform one.
	kinds := map[rewrite.ChangeKind]int{}
	for _, change := range first.Changes {
		kinds[change.Kind]++
	}
	for _, want := range []rewrite.ChangeKind{
		rewrite.ChangeImport,
		rewrite.ChangeDirective,
		rewrite.ChangeMarkerRemoval,
		rewrite.ChangeCommentRemoval,
		rewrite.ChangeNotice,
	} {
		if kinds[want] == 0 {
			t.Errorf("the report carries no %s change: %v", want, first.Changes)
		}
	}

	// The report is ordered by position and then by content, which is what makes
	// it byte identical across runs.
	for i := 1; i < len(first.Changes); i++ {
		previous, current := first.Changes[i-1], first.Changes[i]
		if previous.Line > current.Line {
			t.Errorf("change %d is on line %d, after line %d", i, current.Line, previous.Line)
		}
		if previous.Line == current.Line && previous.Kind > current.Kind {
			t.Errorf("changes on line %d are not ordered by kind: %s then %s",
				current.Line, previous.Kind, current.Kind)
		}
	}

	second, err := rewrite.GoFile(t.Context(), file, options)
	if err != nil {
		t.Fatalf("second rewrite: %v", err)
	}
	if !bytes.Equal(first.Contents, second.Contents) {
		t.Errorf("two runs produced different bytes:\n%s\nand:\n%s", first.Contents, second.Contents)
	}
	if !slices.Equal(first.Changes, second.Changes) {
		t.Errorf("two runs produced different reports:\n%v\nand:\n%v", first.Changes, second.Changes)
	}

	// Rewriting the output again must find nothing: the imports and markers now
	// name the destination module, and the removed directives are gone.
	replayed := file
	replayed.Contents = first.Contents
	again, err := rewrite.GoFile(t.Context(), replayed, options)
	if err != nil {
		t.Fatalf("replayed rewrite: %v", err)
	}
	if again.Changed() {
		t.Errorf("a second pass over the output changed %v", again.Changes)
	}
}
