package rewrite

import (
	"context"
	"fmt"
	"strings"
)

// ProtoFile rewrites the go_package option of a relocated proto file.
//
// A proto file is scanned rather than pattern matched. The `go_package` option
// is the one value in a proto file that names a Go import path, and it is the
// only value this package touches: a message field, an option value, a comment,
// or any other string that happens to read like an import path is left alone.
// A dedicated scanner is what makes that distinction possible, because a
// regular expression over the file would match the same text wherever it
// appeared, including inside the comment that explains why the path is what it
// is.
func ProtoFile(ctx context.Context, file File, opts Options) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, fmt.Errorf("rewrite %s: %w", file.Path, err)
	}
	if err := opts.validate(); err != nil {
		return Result{}, fmt.Errorf("rewrite %s: %w", file.Path, err)
	}
	terminator, err := checkLineEndings(file, opts.LineEndings)
	if err != nil {
		return Result{}, fmt.Errorf("rewrite %s: %w", file.Path, err)
	}

	var edits []edit
	for _, option := range findGoPackageOptions(file.Contents) {
		path, suffix, _ := strings.Cut(option.value, ";")
		destination, eligible := opts.destination(path)
		if !eligible {
			continue
		}
		if suffix != "" {
			destination += ";" + suffix
		}
		edits = append(edits, edit{
			start: option.valueStart,
			end:   option.valueEnd,
			text:  destination,
			change: Change{
				Kind: ChangeProtoOption,
				Path: file.Path,
				Line: lineOf(file.Contents, option.valueStart),
				From: option.value,
				To:   destination,
			},
		})
	}
	if len(edits) == 0 {
		return Result{Contents: file.Contents}, nil
	}
	if !opts.NoNotice {
		edits = append(edits, protoNoticeEdit(file, opts, terminator))
	}

	out, changes, err := applyEdits(file.Contents, edits)
	if err != nil {
		return Result{}, fmt.Errorf("rewrite %s: %w", file.Path, err)
	}
	// Rescanning proves the rewritten file still parses as the same option and
	// carries exactly the intended value, which is the proto equivalent of
	// reparsing a rewritten Go file.
	if err := verifyProto(file, out, changes); err != nil {
		return Result{}, err
	}
	return Result{Contents: out, Changes: changes}, nil
}

// goPackageOption is one located go_package option value.
type goPackageOption struct {
	// value is the unquoted option value, which may carry a ;name suffix.
	value string
	// valueStart and valueEnd bound the value inside its quotes.
	valueStart int
	valueEnd   int
}

// findGoPackageOptions locates every file level go_package option in a proto
// file.
//
// Brace depth is tracked because go_package is a file option, and a message,
// enum, or service body may carry options of its own. A go_package written
// inside one of those bodies does not name the Go import path of the file, so
// rewriting it would corrupt an unrelated setting; only depth zero counts.
func findGoPackageOptions(src []byte) []goPackageOption {
	var found []goPackageOption
	scanner := &protoScanner{src: src}
	depth := 0
	for {
		scanner.skipSpaceAndComments()
		if scanner.done() {
			return found
		}
		// A string literal is consumed whole so its contents can never be read
		// as the keywords this scan is looking for.
		if isQuote(scanner.peek()) {
			scanner.stringLiteral()
			continue
		}
		switch scanner.peek() {
		case '{':
			depth++
			scanner.pos++
			continue
		case '}':
			// A file with unbalanced braces is malformed proto; clamping keeps
			// a later file level option from being read as a nested one.
			depth = max(depth-1, 0)
			scanner.pos++
			continue
		}
		word := scanner.identifier()
		if word != "option" {
			if word == "" {
				scanner.pos++
			}
			continue
		}
		fileLevel := depth == 0
		scanner.skipSpaceAndComments()
		if scanner.identifier() != "go_package" {
			continue
		}
		scanner.skipSpaceAndComments()
		if !scanner.accept('=') {
			continue
		}
		scanner.skipSpaceAndComments()
		// The literal is consumed either way so the scan stays in step; only a
		// file level one is reported.
		value, start, end, ok := scanner.stringLiteral()
		if !ok || !fileLevel {
			continue
		}
		found = append(found, goPackageOption{value: value, valueStart: start, valueEnd: end})
	}
}

// protoScanner walks a proto file one token at a time.
type protoScanner struct {
	src []byte
	pos int
}

// done reports whether the scanner reached the end of the file.
func (s *protoScanner) done() bool { return s.pos >= len(s.src) }

// peek reports the current byte, or zero at the end of the file.
func (s *protoScanner) peek() byte {
	if s.done() {
		return 0
	}
	return s.src[s.pos]
}

// accept consumes one expected byte.
func (s *protoScanner) accept(b byte) bool {
	if s.peek() != b {
		return false
	}
	s.pos++
	return true
}

// skipSpaceAndComments advances past whitespace and both comment forms.
func (s *protoScanner) skipSpaceAndComments() {
	for !s.done() {
		switch c := s.src[s.pos]; {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			s.pos++
		case c == '/' && s.pos+1 < len(s.src) && s.src[s.pos+1] == '/':
			s.pos = lineEnd(s.src, s.pos)
		case c == '/' && s.pos+1 < len(s.src) && s.src[s.pos+1] == '*':
			if end := indexFrom(s.src, s.pos+2, "*/"); end >= 0 {
				s.pos = end + 2
				continue
			}
			s.pos = len(s.src)
		default:
			return
		}
	}
}

// identifier consumes an identifier, which in a proto file may be dotted.
func (s *protoScanner) identifier() string {
	start := s.pos
	for !s.done() {
		c := s.src[s.pos]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '.' {
			s.pos++
			continue
		}
		break
	}
	return string(s.src[start:s.pos])
}

// stringLiteral consumes a quoted literal and reports its unquoted value and
// the byte range of that value inside the quotes. Proto accepts both quote
// characters, and a backslash escapes the next byte in either.
func (s *protoScanner) stringLiteral() (value string, start, end int, ok bool) {
	quote := s.peek()
	if !isQuote(quote) {
		return "", 0, 0, false
	}
	s.pos++
	start = s.pos
	for !s.done() {
		switch s.src[s.pos] {
		case '\\':
			s.pos += 2
		case quote:
			end = s.pos
			s.pos++
			return string(s.src[start:end]), start, end, true
		case '\n':
			return "", 0, 0, false
		default:
			s.pos++
		}
	}
	return "", 0, 0, false
}

// isQuote reports whether a byte opens a proto string literal.
func isQuote(b byte) bool { return b == '"' || b == '\'' }

// indexFrom reports the offset of needle at or after start, or -1.
func indexFrom(src []byte, start int, needle string) int {
	if start >= len(src) {
		return -1
	}
	at := strings.Index(string(src[start:]), needle)
	if at < 0 {
		return -1
	}
	return start + at
}

// lineOf reports the one based line holding an offset.
func lineOf(src []byte, offset int) int {
	line := 1
	for i := 0; i < offset && i < len(src); i++ {
		if src[i] == '\n' {
			line++
		}
	}
	return line
}

// protoNoticeEdit inserts the modification notice above the first statement.
//
// The notice goes below any leading license comment and above the first real
// declaration, which for a Kubernetes proto file is the syntax statement. Proto
// requires no particular position for a comment, so this is a readability
// choice rather than a correctness one, and it keeps the notice in the same
// relative place as in a rewritten Go file. It is terminated the way the file
// terminates its own lines, so a preserved CRLF file does not acquire a handful
// of LF lines in its preamble.
func protoNoticeEdit(file File, opts Options, terminator string) edit {
	scanner := &protoScanner{src: file.Contents}
	scanner.skipSpaceAndComments()
	offset := lineStart(file.Contents, min(scanner.pos, len(file.Contents)))
	text := noticeText(file, opts, terminator)
	return edit{
		start: offset,
		end:   offset,
		text:  text,
		change: Change{
			Kind: ChangeNotice,
			Path: file.Path,
			Line: lineOf(file.Contents, offset),
			To:   text,
		},
	}
}

// verifyProto rescans a rewritten proto file and pairs its go_package options
// with the original's.
//
// Pairing is positional because a byte range replacement can neither add,
// remove, nor reorder an option, so a count that changed is itself the failure.
// An option whose value differs must be justified by a reported change, and one
// whose value is unchanged has to be byte identical: an ineligible go_package,
// the option of a proto file that belongs to another module, keeps the identity
// its generated Go package is compiled under, and corrupting it would be
// invisible to a check that only asked whether the intended values arrived.
func verifyProto(file File, out []byte, changes []Change) error {
	before := findGoPackageOptions(file.Contents)
	after := findGoPackageOptions(out)
	if len(before) != len(after) {
		return fmt.Errorf("%s: %w: %d go_package options became %d",
			file.Path, ErrShapeChanged, len(before), len(after))
	}

	reported := make(map[optionRewrite]int, len(changes))
	for _, change := range changes {
		if change.Kind == ChangeProtoOption {
			reported[optionRewrite{from: change.From, to: change.To}]++
		}
	}
	for i, option := range before {
		got := after[i].value
		if got == option.value {
			continue
		}
		claim := optionRewrite{from: option.value, to: got}
		if reported[claim] == 0 {
			return fmt.Errorf("%s: %w: go_package %q became %q without being reported",
				file.Path, ErrShapeChanged, option.value, got)
		}
		reported[claim]--
	}
	return nil
}

// optionRewrite is one reported go_package replacement, used as a map key so a
// file that rewrites the same value twice is matched as often as it was
// reported.
type optionRewrite struct{ from, to string }
