package state_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/state"
)

// TestEncodeDecodeRoundTrip proves a stored record reads back as the record
// that was stored, under both hash algorithms.
func TestEncodeDecodeRoundTrip(t *testing.T) {
	for _, format := range objectFormats {
		t.Run(string(format), func(t *testing.T) {
			doc := mustNew(t, base(format))
			encoded, err := doc.Encode()
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			decoded, err := state.Decode(encoded)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if decoded.Digest != doc.Digest {
				t.Fatalf("decoded digest %s, want %s", decoded.Digest, doc.Digest)
			}
			again, err := decoded.Encode()
			if err != nil {
				t.Fatalf("re-encode: %v", err)
			}
			if !bytes.Equal(again, encoded) {
				t.Fatalf("re-encoding produced different bytes:\n%s\nand\n%s", encoded, again)
			}
		})
	}
}

// TestEncodeIsDeterministic pins the property the whole seam rests on: two
// documents describing the same work render byte identically, whatever order
// the work arrived in and however many times they are rendered.
//
// Byte equality rather than field equality is the assertion, because the bytes
// are what becomes a blob, and the blob's object name is how a resume decides
// whether anything changed.
func TestEncodeIsDeterministic(t *testing.T) {
	format := gitcli.ObjectFormatSHA256
	sorted := base(format)
	sorted.Cursors = []state.Cursor{
		{Ref: "refs/heads/main", Source: sha(format, "a"), Destination: sha(format, "aa")},
		{Ref: "refs/heads/master", Source: sha(format, "b"), Destination: sha(format, "bb")},
	}
	shuffled := sorted.Clone()
	shuffled.Cursors = []state.Cursor{sorted.Cursors[1], sorted.Cursors[0]}

	first, err := mustNew(t, sorted).Encode()
	if err != nil {
		t.Fatalf("encode sorted: %v", err)
	}
	second, err := mustNew(t, shuffled).Encode()
	if err != nil {
		t.Fatalf("encode shuffled: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("reordered input rendered differently:\n%s\nand\n%s", first, second)
	}
	if !bytes.HasSuffix(first, []byte("\n")) {
		t.Fatalf("the rendering does not end in a newline")
	}
}

// TestEncodeOrdersFieldsAsDeclared checks that the rendering follows the type
// declaration.
//
// The digest covers the bytes, so field order is part of the format. Moving a
// field in the struct changes every digest the engine has ever written, and that
// has to be a deliberate schema change rather than a tidy up nobody noticed.
func TestEncodeOrdersFieldsAsDeclared(t *testing.T) {
	encoded, err := mustNew(t, base(gitcli.ObjectFormatSHA1)).Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	want := []string{
		`"schema"`, `"objectFormat"`, `"destination"`, `"anchor"`, `"epoch"`,
		`"cursors"`, `"mapping"`, `"tracks"`, `"published"`, `"engine"`, `"digest"`,
	}
	rendered := string(encoded)
	offset := 0
	for _, key := range want {
		index := strings.Index(rendered[offset:], key)
		if index < 0 {
			t.Fatalf("key %s does not appear after the ones before it:\n%s", key, rendered)
		}
		offset += index + len(key)
	}
}

// TestEncodeRefusesAnInvalidDocument checks that a document is validated on its
// way out rather than only on its way in. A record that cannot be read back
// should never reach a blob.
func TestEncodeRefusesAnInvalidDocument(t *testing.T) {
	doc := mustNew(t, base(gitcli.ObjectFormatSHA1))
	doc.Engine.Toolchain = "1.26.5"
	if _, err := doc.Encode(); err == nil {
		t.Fatalf("encode accepted a document validation refuses")
	}
}

// TestDecodeRefusals covers every way a stored record can fail to be one.
//
// The cases are stated as edits to a valid rendering, because that is what the
// failures look like in practice: a record written by a newer engine, a record
// a person opened and changed, and a record something appended to.
func TestDecodeRefusals(t *testing.T) {
	valid, err := mustNew(t, base(gitcli.ObjectFormatSHA1)).Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	tests := []struct {
		name    string
		data    []byte
		want    error
		message string
	}{{
		name: "empty input",
		data: nil,
		want: nil, message: "decode state document",
	}, {
		name: "truncated document",
		data: valid[:len(valid)/2],
		want: nil, message: "decode state document",
	}, {
		name: "not json at all",
		data: []byte("soapbox state\n"),
		want: nil, message: "decode state document",
	}, {
		name: "a future schema",
		data: bytes.Replace(valid, []byte(`"schema": 1`), []byte(`"schema": 2`), 1),
		want: state.ErrSchema,
	}, {
		name: "a field this engine does not know",
		data: bytes.Replace(valid, []byte(`"schema": 1,`), []byte(`"schema": 1,`+"\n"+`  "chunkSize": 500,`), 1),
		want: nil, message: "unknown field",
	}, {
		name: "a second document appended",
		data: append(append([]byte{}, valid...), valid...),
		want: state.ErrTrailing,
	}, {
		name: "a stray value appended",
		data: append(append([]byte{}, valid...), []byte("null\n")...),
		want: state.ErrTrailing,
	}, {
		name: "trailing garbage",
		data: append(append([]byte{}, valid...), []byte("}\n")...),
		want: state.ErrTrailing,
	}, {
		name: "a field of the wrong type",
		data: bytes.Replace(valid, []byte(`"entries": 12`), []byte(`"entries": "12"`), 1),
		want: nil, message: "decode state document",
	}, {
		name: "content edited under an unchanged digest",
		data: bytes.Replace(valid, []byte("enj/rbac_authorizer"), []byte("enj/rbac_authorize2"), 1),
		want: state.ErrDigest,
	}, {
		name: "a digest edited to match nothing",
		data: bytes.Replace(valid, []byte(`"digest": "sha256:`), []byte(`"digest": "sha256:0`), 1),
		want: nil, message: "65 digest characters",
	}, {
		name: "lists reordered by hand",
		data: reorderCursors(t, gitcli.ObjectFormatSHA1),
		want: state.ErrUnsorted,
	}}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := state.Decode(test.data)
			if err == nil {
				t.Fatalf("decode accepted the input")
			}
			if test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("decode: %v, want %v", err, test.want)
			}
			if test.message != "" && !strings.Contains(err.Error(), test.message) {
				t.Fatalf("decode: %v, want a message holding %q", err, test.message)
			}
		})
	}
}

// reorderCursors renders a valid document and swaps its two cursors in the
// bytes, which is what a hand edit that preserved the digest would look like.
func reorderCursors(t *testing.T, format gitcli.ObjectFormat) []byte {
	t.Helper()
	doc := base(format)
	doc.Cursors = []state.Cursor{
		{Ref: "refs/heads/main", Source: sha(format, "a"), Destination: sha(format, "aa")},
		{Ref: "refs/heads/master", Source: sha(format, "b"), Destination: sha(format, "bb")},
	}
	encoded, err := mustNew(t, doc).Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	rendered := string(encoded)
	first := `"ref": "refs/heads/main"`
	second := `"ref": "refs/heads/master"`
	// The names are swapped rather than the blocks moved, which reverses the
	// order without changing anything else about the rendering.
	rendered = strings.Replace(rendered, second, "\x00", 1)
	rendered = strings.Replace(rendered, first, second, 1)
	rendered = strings.Replace(rendered, "\x00", first, 1)
	return []byte(rendered)
}

// TestDecodeAcceptsOnlyTheCanonicalRendering pins the rule that one record has
// one blob.
//
// JSON admits many spellings of one value and none of the alternatives are
// harmless here. Reindented bytes, a repeated key whose first value the decoder
// discards, null where an empty list belongs, and a trailing blank line all
// parse to exactly the document that was written and all store as different
// blobs. Accepting them would give one record several object names, and a resume
// that decides whether anything changed by comparing names would be answering
// about the spelling rather than about the work.
func TestDecodeAcceptsOnlyTheCanonicalRendering(t *testing.T) {
	valid, err := mustNew(t, base(gitcli.ObjectFormatSHA1)).Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if _, err := state.Decode(valid); err != nil {
		t.Fatalf("decode the canonical rendering: %v", err)
	}

	// Every alternative below is built from the canonical bytes and still parses
	// to the same document, which is what makes each of them a spelling rather
	// than a different record.
	compact := new(bytes.Buffer)
	if err := json.Compact(compact, valid); err != nil {
		t.Fatalf("compact: %v", err)
	}

	tests := []struct {
		name string
		data []byte
	}{{
		name: "the same document with its indentation removed",
		data: compact.Bytes(),
	}, {
		name: "a blank line appended",
		data: append(slices.Clone(valid), '\n'),
	}, {
		name: "trailing spaces appended",
		data: append(slices.Clone(valid), ' ', ' '),
	}, {
		name: "a leading blank line",
		data: append([]byte("\n"), valid...),
	}, {
		name: "the schema repeated with the same value",
		data: bytes.Replace(valid, []byte(`"schema": 1,`), []byte(`"schema": 1,`+"\n"+`  "schema": 1,`), 1),
	}, {
		name: "an earlier value for a repeated key that the decoder discards",
		data: bytes.Replace(valid, []byte(`"schema": 1,`), []byte(`"schema": 99,`+"\n"+`  "schema": 1,`), 1),
	}, {
		name: "an empty list written as null",
		data: emptyListsAs(t, "null"),
	}}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := state.Decode(test.data)
			if !errors.Is(err, state.ErrNotCanonical) {
				t.Fatalf("decode: %v, want %v", err, state.ErrNotCanonical)
			}
		})
	}
}

// emptyListsAs renders a document whose lists are all empty and rewrites the
// empty arrays as the given spelling, which is what a record written by
// something that did not normalise would look like.
func emptyListsAs(t *testing.T, spelling string) []byte {
	t.Helper()
	doc := base(gitcli.ObjectFormatSHA1)
	doc.Cursors = nil
	doc.Tracks = nil
	doc.Published = nil
	encoded, err := mustNew(t, doc).Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	rewritten := bytes.ReplaceAll(encoded, []byte("[]"), []byte(spelling))
	if bytes.Equal(rewritten, encoded) {
		t.Fatalf("the rendering holds no empty list to rewrite:\n%s", encoded)
	}
	return rewritten
}

// TestDecodeReportsContentBeforeSpelling checks the order the refusals run in.
//
// A record that is wrong should say what is wrong with it. If the canonical
// check ran first, a hand edited record with a real problem would be reported as
// oddly formatted, and whoever read the error would go looking for a whitespace
// difference instead of the invariant they broke.
func TestDecodeReportsContentBeforeSpelling(t *testing.T) {
	valid, err := mustNew(t, base(gitcli.ObjectFormatSHA1)).Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	// Content edited and reindented at once, so both refusals apply.
	edited := bytes.Replace(valid, []byte("enj/rbac_authorizer"), []byte("enj/rbac_authorize2"), 1)
	compact := new(bytes.Buffer)
	if err := json.Compact(compact, edited); err != nil {
		t.Fatalf("compact: %v", err)
	}
	if _, err := state.Decode(compact.Bytes()); !errors.Is(err, state.ErrDigest) {
		t.Fatalf("decode: %v, want %v", err, state.ErrDigest)
	}
}
