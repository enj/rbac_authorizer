package state

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Encode renders a validated document as deterministic bytes.
//
// The bytes are the record. Two runs that did the same work produce the same
// bytes, so the blob they are stored in has the same object name, and a resume
// can tell "nothing changed" from "something changed" by comparing names rather
// than by parsing and diffing. That only holds if the rendering has no freedom
// left in it, which is why the document holds no map, why every list is sorted
// before it gets here, and why the encoder settings below are pinned.
func (d Document) Encode() ([]byte, error) {
	if err := d.Validate(); err != nil {
		return nil, err
	}
	return d.encode()
}

// encode renders without validating, for the two callers that cannot validate
// yet: the digest computation, whose input is a document with no digest, and
// Encode itself, which has just validated.
//
// A missing list is rendered as an empty one rather than as null. A document a
// caller assembled without touching a list and one that was cloned from a
// stored record say the same thing, so they have to render the same way;
// otherwise one logical state would have two renderings, two digests, and two
// blobs, and a resume comparing object names would see a change where there is
// none.
func (d Document) encode() ([]byte, error) {
	rendered := d
	if rendered.Cursors == nil {
		rendered.Cursors = []Cursor{}
	}
	if rendered.Tracks == nil {
		rendered.Tracks = []Track{}
	}
	if rendered.Published == nil {
		rendered.Published = []Published{}
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	// HTML escaping would turn characters in a ref name or a module path into
	// escapes and make the bytes depend on the encoder rather than on the run.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(rendered); err != nil {
		return nil, fmt.Errorf("encode state document: %w", err)
	}
	return buf.Bytes(), nil
}

// computeDigest digests the document with its own digest field cleared.
//
// Clearing rather than omitting keeps one renderer for both purposes. The bytes
// that are hashed are the bytes Encode produces for a document that has not
// been digested yet, so there is no second serialization that could drift from
// the first and leave a document disagreeing with its own digest.
func (d Document) computeDigest() (string, error) {
	undigested := d
	undigested.Digest = ""
	encoded, err := undigested.encode()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return digestPrefix + hex.EncodeToString(sum[:]), nil
}

// Decode reads a state document and refuses anything it cannot fully account
// for.
//
// Four refusals matter, and they are separate on purpose. A schema this engine
// does not write is reported by version, before any field is read, so an
// operator running an old engine against a new record is told to upgrade rather
// than shown a parse error. An unknown field is refused because it is either a
// newer schema that forgot to bump its version or a hand edit, and both mean
// the reader is not seeing everything the writer recorded. Trailing bytes are
// refused because a record with a second value appended is a record two readers
// could legitimately disagree about.
//
// The last refusal is the strictest: the input has to be byte for byte the
// rendering this engine produces for the document it decoded. JSON admits many
// spellings of one value, and the alternatives are not harmless here. Different
// whitespace, reordered keys, a repeated key whose first value is discarded, and
// null where an empty list belongs all parse to the same document and all store
// as different blobs. Accepting them would mean one record has many object
// names, and a resume that decides whether anything changed by comparing names
// would be answering about the spelling rather than about the work.
func Decode(data []byte) (Document, error) {
	// The version is read with a lenient decoder so the version check runs
	// first. Reading it strictly would report the new field a future schema
	// added instead of reporting the schema.
	var probe struct {
		Schema int `json:"schema"`
	}
	if err := json.NewDecoder(bytes.NewReader(data)).Decode(&probe); err != nil {
		return Document{}, fmt.Errorf("decode state document: %w", err)
	}
	if probe.Schema != Schema {
		return Document{}, fmt.Errorf("%w: the document declares schema %d, this engine writes %d",
			ErrSchema, probe.Schema, Schema)
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var doc Document
	if err := dec.Decode(&doc); err != nil {
		return Document{}, fmt.Errorf("decode state document: %w", err)
	}
	if err := checkTrailing(dec); err != nil {
		return Document{}, err
	}
	// The decoded document is cloned first, so its lists are the reader's own
	// and an absent one has become an empty one before anything is compared.
	decoded := doc.Clone()
	if err := decoded.Validate(); err != nil {
		return Document{}, err
	}
	// The canonical check runs last. A document that is wrong should report what
	// is wrong with it rather than that it is spelled unusually, and re-rendering
	// a document that has not been validated would compare against bytes nobody
	// should be storing in the first place.
	canonical, err := decoded.encode()
	if err != nil {
		return Document{}, err
	}
	if !bytes.Equal(canonical, data) {
		return Document{}, fmt.Errorf("%w: it is %d bytes and renders as %d",
			ErrNotCanonical, len(data), len(canonical))
	}
	return decoded, nil
}

// checkTrailing reports a second value following the document.
//
// Whitespace after the document is not a second value: the decoder skips it and
// reports the end of input. It is still refused, by the canonical check in
// Decode, which is the right place for it. This rule is about a record holding
// two documents; that one is about a record holding one document spelled a way
// this engine does not write.
func checkTrailing(dec *json.Decoder) error {
	var extra json.RawMessage
	switch err := dec.Decode(&extra); {
	case errors.Is(err, io.EOF):
		return nil
	case err != nil:
		return fmt.Errorf("%w: %w", ErrTrailing, err)
	default:
		return fmt.Errorf("%w: %d more bytes follow the document", ErrTrailing, len(extra))
	}
}
