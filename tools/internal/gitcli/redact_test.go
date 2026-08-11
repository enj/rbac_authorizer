package gitcli_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/gitcli"
)

func TestRedactorString(t *testing.T) {
	tests := []struct {
		name    string
		secrets []string
		in      string
		want    string
	}{
		{name: "no secrets", in: "fatal: could not read Username", want: "fatal: could not read Username"},
		{
			name:    "single value",
			secrets: []string{"ghs_TOKEN"},
			in:      "remote: rejected for ghs_TOKEN",
			want:    "remote: rejected for " + gitcli.Placeholder,
		},
		{
			name:    "repeated value",
			secrets: []string{"ghs_TOKEN"},
			in:      "ghs_TOKEN ghs_TOKEN",
			want:    gitcli.Placeholder + " " + gitcli.Placeholder,
		},
		{
			name:    "overlapping values replace the longest first",
			secrets: []string{"ghs_", "ghs_TOKEN"},
			in:      "using ghs_TOKEN now",
			want:    "using " + gitcli.Placeholder + " now",
		},
		{
			name:    "empty secrets are ignored",
			secrets: []string{""},
			in:      "unchanged",
			want:    "unchanged",
		},
		{
			name:    "multiline",
			secrets: []string{"s3cret"},
			in:      "line one s3cret\nline two s3cret\n",
			want:    "line one " + gitcli.Placeholder + "\nline two " + gitcli.Placeholder + "\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			r := gitcli.NewRedactor(test.secrets...)
			if got := r.String(test.in); got != test.want {
				t.Fatalf("String() = %q, want %q", got, test.want)
			}
			if got := string(r.Bytes([]byte(test.in))); got != test.want {
				t.Fatalf("Bytes() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestRedactorStrings(t *testing.T) {
	r := gitcli.NewRedactor("ghs_TOKEN")
	got := r.Strings([]string{"push", "https://github.com/enj/x.git", "ghs_TOKEN"})
	want := []string{"push", "https://github.com/enj/x.git", gitcli.Placeholder}
	if len(got) != len(want) {
		t.Fatalf("Strings() = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("Strings()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestRedactWriterSplitAcrossWrites(t *testing.T) {
	tests := []struct {
		name   string
		chunks []string
	}{
		{name: "one write", chunks: []string{"prefix ghs_TOKEN suffix"}},
		{name: "split in the middle", chunks: []string{"prefix ghs_", "TOKEN suffix"}},
		{name: "byte at a time", chunks: splitBytes("prefix ghs_TOKEN suffix")},
		{name: "secret at the end", chunks: []string{"prefix ", "ghs_TOKEN"}},
		{name: "partial secret only", chunks: []string{"prefix ghs_TOK"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var buf bytes.Buffer
			w := gitcli.NewRedactor("ghs_TOKEN").Writer(&buf)
			for _, chunk := range test.chunks {
				if _, err := w.Write([]byte(chunk)); err != nil {
					t.Fatalf("write: %v", err)
				}
			}
			if err := w.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}
			if strings.Contains(buf.String(), "ghs_TOKEN") {
				t.Fatalf("redacted stream still contains the secret: %q", buf.String())
			}
			joined := strings.Join(test.chunks, "")
			if strings.Contains(joined, "ghs_TOKEN") && !strings.Contains(buf.String(), gitcli.Placeholder) {
				t.Fatalf("redacted stream %q has no placeholder", buf.String())
			}
			if !strings.HasPrefix(buf.String(), "prefix ") {
				t.Fatalf("redacted stream %q lost the non secret prefix", buf.String())
			}
		})
	}
}

func TestRedactWriterReportsFullWriteLength(t *testing.T) {
	var buf bytes.Buffer
	w := gitcli.NewRedactor("secret").Writer(&buf)
	payload := []byte("a short line with secret inside")
	n, err := w.Write(payload)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if n != len(payload) {
		t.Fatalf("Write() = %d, want %d", n, len(payload))
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := buf.String(); got != "a short line with "+gitcli.Placeholder+" inside" {
		t.Fatalf("redacted stream = %q", got)
	}
}

func TestRedactWriterWithoutSecretsIsTransparent(t *testing.T) {
	var buf bytes.Buffer
	w := gitcli.NewRedactor().Writer(&buf)
	if _, err := w.Write([]byte("plain output\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := buf.String(); got != "plain output\n" {
		t.Fatalf("stream = %q, want plain output", got)
	}
}

// splitBytes splits s into one string per byte.
func splitBytes(s string) []string {
	chunks := make([]string, 0, len(s))
	for i := range len(s) {
		chunks = append(chunks, s[i:i+1])
	}
	return chunks
}
