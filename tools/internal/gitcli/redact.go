package gitcli

import (
	"bytes"
	"fmt"
	"io"
	"slices"
	"strings"
)

// Placeholder replaces every redacted value in captured output.
const Placeholder = "[redacted]"

// Redactor removes exact secret values from text. Redaction is exact value
// based rather than pattern based so a token can never survive because it did
// not match a heuristic.
type Redactor struct {
	values []string
	hold   int
}

// NewRedactor seeds a redactor with exact values. Empty values are ignored and
// longer values are replaced first so an overlapping prefix cannot leak a
// suffix.
func NewRedactor(values ...string) *Redactor {
	seen := make(map[string]bool, len(values))
	kept := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		kept = append(kept, value)
	}
	slices.SortFunc(kept, func(a, b string) int {
		if len(a) != len(b) {
			return len(b) - len(a)
		}
		return strings.Compare(a, b)
	})
	r := &Redactor{values: kept}
	if len(kept) > 0 {
		r.hold = len(kept[0]) - 1
	}
	return r
}

// String replaces every seeded value in s.
func (r *Redactor) String(s string) string {
	if r == nil || len(r.values) == 0 || s == "" {
		return s
	}
	for _, value := range r.values {
		if !strings.Contains(s, value) {
			continue
		}
		s = strings.ReplaceAll(s, value, Placeholder)
	}
	return s
}

// Bytes replaces every seeded value in b. The input slice is returned unchanged
// when nothing matches, so the common case does not copy.
func (r *Redactor) Bytes(b []byte) []byte {
	if r == nil || len(r.values) == 0 || len(b) == 0 {
		return b
	}
	for _, value := range r.values {
		needle := []byte(value)
		if !bytes.Contains(b, needle) {
			continue
		}
		b = bytes.ReplaceAll(b, needle, []byte(Placeholder))
	}
	return b
}

// Strings replaces every seeded value in each element of in.
func (r *Redactor) Strings(in []string) []string {
	if r == nil || len(r.values) == 0 {
		return in
	}
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = r.String(s)
	}
	return out
}

// Error removes every seeded value from an error message while keeping the
// original error in the chain, so errors.Is and errors.As keep working.
func (r *Redactor) Error(err error) error {
	if err == nil || r == nil || len(r.values) == 0 {
		return err
	}
	message := r.String(err.Error())
	if message == err.Error() {
		return err
	}
	return &redactedError{message: message, err: err}
}

// redactedError carries a redacted message and the original error.
type redactedError struct {
	message string
	err     error
}

func (e *redactedError) Error() string { return e.message }
func (e *redactedError) Unwrap() error { return e.err }

// Writer wraps w so that seeded values are removed from streamed output. The
// returned writer holds back the bytes that could still complete a secret, so
// callers must close it to flush the tail. Without secrets the writer is
// transparent and buffers nothing.
func (r *Redactor) Writer(w io.Writer) io.WriteCloser {
	if r == nil || len(r.values) == 0 {
		return nopCloser{Writer: w}
	}
	return &redactWriter{redactor: r, out: w}
}

// nopCloser adds a no-op Close to a writer.
type nopCloser struct {
	io.Writer
}

// Close reports success because there is nothing buffered.
func (nopCloser) Close() error { return nil }

// redactWriter redacts a stream that may split a secret across writes.
type redactWriter struct {
	redactor *Redactor
	out      io.Writer
	pending  []byte
}

// Write redacts everything buffered so far and emits all but the trailing bytes
// that could still become part of a seeded value.
func (w *redactWriter) Write(p []byte) (int, error) {
	w.pending = w.redactor.Bytes(append(w.pending, p...))
	hold := 0
	if w.redactor != nil {
		hold = w.redactor.hold
	}
	if len(w.pending) > hold {
		emit := w.pending[:len(w.pending)-hold]
		if _, err := w.out.Write(emit); err != nil {
			return 0, fmt.Errorf("write redacted output: %w", err)
		}
		w.pending = append(w.pending[:0], w.pending[len(w.pending)-hold:]...)
	}
	return len(p), nil
}

// Close flushes the held back tail.
func (w *redactWriter) Close() error {
	if len(w.pending) == 0 {
		return nil
	}
	tail := w.redactor.Bytes(w.pending)
	w.pending = nil
	if _, err := w.out.Write(tail); err != nil {
		return fmt.Errorf("flush redacted output: %w", err)
	}
	return nil
}
