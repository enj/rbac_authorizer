package rewrite_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/enj/soapbox/tools/internal/rewrite"
)

// relocatedAssets is the destination file set an embed directive resolves
// against, as relocation would report it.
var relocatedAssets = []string{
	"internal/kk/pkg/apis/rbac/v1/.hidden.json",
	"internal/kk/pkg/apis/rbac/v1/_scratch.json",
	"internal/kk/pkg/apis/rbac/v1/doc.go",
	"internal/kk/pkg/apis/rbac/v1/schema.json",
	"internal/kk/pkg/apis/rbac/v1/testdata/.ignored.yaml",
	"internal/kk/pkg/apis/rbac/v1/testdata/nested/deep.yaml",
	"internal/kk/pkg/apis/rbac/v1/testdata/roles.yaml",
	"internal/kk/pkg/registry/rbac/validation/rule.go",
}

// embedFile builds a Go file carrying one go:embed directive.
func embedFile(directive string) rewrite.File {
	return goFile("pkg/apis/rbac/v1/assets.go",
		"package v1\n\nimport _ \"embed\"\n\n"+directive+"\nvar asset string\n")
}

func TestVerifyEmbeds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		directive string
		want      []string
	}{
		{
			name:      "a literal file",
			directive: "//go:embed schema.json",
			want:      []string{"internal/kk/pkg/apis/rbac/v1/schema.json"},
		},
		{
			// The toolchain's hidden name filter applies to directory
			// expansion, not to a glob that matches the names directly. This
			// expectation was checked against go list rather than assumed.
			name:      "a glob matches hidden names directly",
			directive: "//go:embed *.json",
			want: []string{
				"internal/kk/pkg/apis/rbac/v1/.hidden.json",
				"internal/kk/pkg/apis/rbac/v1/_scratch.json",
				"internal/kk/pkg/apis/rbac/v1/schema.json",
			},
		},
		{
			name:      "several patterns on one directive",
			directive: "//go:embed schema.json testdata/roles.yaml",
			want: []string{
				"internal/kk/pkg/apis/rbac/v1/schema.json",
				"internal/kk/pkg/apis/rbac/v1/testdata/roles.yaml",
			},
		},
		{
			name:      "a directory embeds its subtree without hidden names",
			directive: "//go:embed testdata",
			want: []string{
				"internal/kk/pkg/apis/rbac/v1/testdata/nested/deep.yaml",
				"internal/kk/pkg/apis/rbac/v1/testdata/roles.yaml",
			},
		},
		{
			name:      "the all prefix includes hidden names",
			directive: "//go:embed all:testdata",
			want: []string{
				"internal/kk/pkg/apis/rbac/v1/testdata/.ignored.yaml",
				"internal/kk/pkg/apis/rbac/v1/testdata/nested/deep.yaml",
				"internal/kk/pkg/apis/rbac/v1/testdata/roles.yaml",
			},
		},
		{
			name:      "a hidden file named directly",
			directive: "//go:embed .hidden.json",
			want:      []string{"internal/kk/pkg/apis/rbac/v1/.hidden.json"},
		},
		{
			name:      "a hidden file inside a directory, named directly",
			directive: "//go:embed testdata/.ignored.yaml",
			want:      []string{"internal/kk/pkg/apis/rbac/v1/testdata/.ignored.yaml"},
		},
		{
			name:      "a quoted pattern",
			directive: "//go:embed \"schema.json\"",
			want:      []string{"internal/kk/pkg/apis/rbac/v1/schema.json"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			embeds, err := rewrite.VerifyEmbeds(t.Context(), embedFile(test.directive), relocatedAssets)
			if err != nil {
				t.Fatalf("verify embeds: %v", err)
			}
			if len(embeds) != 1 {
				t.Fatalf("found %d directives, want 1", len(embeds))
			}
			if !slices.Equal(embeds[0].Matches, test.want) {
				t.Errorf("resolved to %v, want %v", embeds[0].Matches, test.want)
			}
			if embeds[0].Line != 5 {
				t.Errorf("directive reported on line %d, want 5", embeds[0].Line)
			}
		})
	}
}

// TestVerifyEmbedsRejects covers the failure this verification exists for: the
// closure selects Go files by import, so a data file only arrives if an asset
// rule matched it, and a pattern that now resolves to nothing would otherwise
// surface as a build failure in the published module.
func TestVerifyEmbedsRejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		directive string
		want      error
	}{
		{
			name:      "a pattern that was not copied",
			directive: "//go:embed absent.json",
			want:      rewrite.ErrEmbedUnmatched,
		},
		{
			name:      "a glob that matches nothing",
			directive: "//go:embed *.yaml",
			want:      rewrite.ErrEmbedUnmatched,
		},
		{
			name:      "a pattern that leaves the package",
			directive: "//go:embed ../../../registry/rbac/validation/rule.go",
			want:      rewrite.ErrEmbedEscape,
		},
		{
			name:      "an absolute pattern",
			directive: "//go:embed /etc/passwd",
			want:      rewrite.ErrEmbedEscape,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := rewrite.VerifyEmbeds(t.Context(), embedFile(test.directive), relocatedAssets)
			if !errors.Is(err, test.want) {
				t.Errorf("verify embeds error %v, want %v", err, test.want)
			}
		})
	}
}

// TestVerifyEmbedsIgnoresOtherComments asserts that only real directives are
// read: the toolchain requires no space after the slashes, and a sentence about
// embedding is not an instruction.
func TestVerifyEmbedsIgnoresOtherComments(t *testing.T) {
	t.Parallel()

	file := goFile("pkg/apis/rbac/v1/assets.go",
		"package v1\n\n"+
			"// go:embed absent.json\n"+
			"// This file used to //go:embed absent.json before the prune.\n"+
			"var asset string\n")

	embeds, err := rewrite.VerifyEmbeds(t.Context(), file, relocatedAssets)
	if err != nil {
		t.Fatalf("verify embeds: %v", err)
	}
	if len(embeds) != 0 {
		t.Errorf("found %v, want no directives", embeds)
	}
}

// TestVerifyEmbedsDoesNotRewrite asserts embeds are verified rather than
// relocated: the pattern is relative to the file, and the file and its assets
// move together.
func TestVerifyEmbedsDoesNotRewrite(t *testing.T) {
	t.Parallel()

	file := embedFile("//go:embed schema.json")
	before := string(file.Contents)

	result, err := rewrite.GoFile(t.Context(), file, noticedOptions())
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if got := string(result.Contents); got != before {
		t.Errorf("rewrote to:\n%s\nwant it unchanged:\n%s", got, before)
	}
}

// TestVerifyEmbedsIgnoresIllegalPositions covers the comments cmd/go does not
// read as directives.
//
// Reading more than the toolchain reads would fail a build that works: the
// engine would demand an asset for a comment the compiler treats as prose, and
// a profile author would have to satisfy a requirement the Go toolchain never
// makes. Every shape here is a comment whose text is a directive but whose
// position is not.
func TestVerifyEmbedsIgnoresIllegalPositions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
	}{
		{
			name: "inside a function body",
			in: "package v1\n\nimport _ \"embed\"\n\n" +
				"func load() {\n\t//go:embed missing.json\n\t_ = 0\n}\n",
		},
		{
			name: "after code on the same line",
			in: "package v1\n\nimport _ \"embed\"\n\n" +
				"var other = 1 //go:embed missing.json\n",
		},
		{
			name: "on a var that already has a value",
			in: "package v1\n\nimport _ \"embed\"\n\n" +
				"//go:embed missing.json\nvar asset = \"\"\n",
		},
		{
			name: "on a declaration that is not a var",
			in: "package v1\n\nimport _ \"embed\"\n\n" +
				"//go:embed missing.json\ntype Asset struct{}\n",
		},
		{
			// The directive has to begin the comment text. A space after the
			// slashes makes it an ordinary sentence, which is how the Go
			// specification separates the two.
			name: "written with a space after the slashes",
			in: "package v1\n\nimport _ \"embed\"\n\n" +
				"// go:embed missing.json\nvar asset string\n",
		},
		{
			// Without the import the directive is inert and cmd/go rejects the
			// file outright, so there is no pattern here to hold to an asset.
			name: "in a file that does not import embed",
			in:   "package v1\n\n//go:embed missing.json\nvar asset string\n",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			embeds, err := rewrite.VerifyEmbeds(t.Context(),
				goFile("pkg/apis/rbac/v1/assets.go", test.in), relocatedAssets)
			if err != nil {
				t.Fatalf("verify embeds: %v", err)
			}
			if len(embeds) != 0 {
				t.Errorf("read %d directives out of comments cmd/go ignores: %v", len(embeds), embeds)
			}
		})
	}
}

// TestVerifyEmbedsPatternSplitting covers the separator and quoting rules that
// a go:embed pattern list follows.
func TestVerifyEmbedsPatternSplitting(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		directive string
		want      []string
		wantErr   error
	}{
		{
			name:      "tabs separate patterns",
			directive: "//go:embed\tschema.json\ttestdata/roles.yaml",
			want:      []string{"schema.json", "testdata/roles.yaml"},
		},
		{
			name:      "runs of mixed whitespace separate patterns",
			directive: "//go:embed  schema.json \t testdata/roles.yaml",
			want:      []string{"schema.json", "testdata/roles.yaml"},
		},
		{
			name:      "a backquoted pattern",
			directive: "//go:embed `schema.json`",
			want:      []string{"schema.json"},
		},
		{
			name:      "a quoted pattern beside a bare one",
			directive: "//go:embed \"schema.json\" testdata/roles.yaml",
			want:      []string{"schema.json", "testdata/roles.yaml"},
		},
		{
			name:      "an escape inside a quoted pattern",
			directive: "//go:embed \"schema\\x2ejson\"",
			want:      []string{"schema.json"},
		},
		{
			name:      "an unterminated quoted pattern",
			directive: "//go:embed \"schema.json",
			wantErr:   rewrite.ErrEmbedPattern,
		},
		{
			name:      "an unterminated backquoted pattern",
			directive: "//go:embed `schema.json",
			wantErr:   rewrite.ErrEmbedPattern,
		},
		{
			name:      "a quoted pattern running into the next token",
			directive: "//go:embed \"schema.json\"testdata",
			wantErr:   rewrite.ErrEmbedPattern,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			embeds, err := rewrite.VerifyEmbeds(t.Context(), embedFile(test.directive), relocatedAssets)
			if test.wantErr != nil {
				if !errors.Is(err, test.wantErr) {
					t.Fatalf("verify embeds error %v, want %v", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("verify embeds: %v", err)
			}
			if len(embeds) != 1 {
				t.Fatalf("found %d directives, want 1", len(embeds))
			}
			if !slices.Equal(embeds[0].Patterns, test.want) {
				t.Errorf("split into %q, want %q", embeds[0].Patterns, test.want)
			}
		})
	}
}
