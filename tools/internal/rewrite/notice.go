package rewrite

import (
	"strings"
)

// noticeIntro is the first line of every modification notice. It is fixed text
// so a reader, a reviewer, and a grep all recognise the same marker.
const noticeIntro = "// This file was modified by soapbox and is not the upstream original."

// noticeEdit inserts a prominent modification notice above the package clause.
//
// The position is chosen so nothing that has to come first stops working. A
// build constraint keeps its place in the file preamble, because the notice is
// a line comment and blank lines and line comments are exactly what a
// constraint may be surrounded by. A `Code generated ... DO NOT EDIT.` marker
// keeps working for the same reason: the convention requires it to appear
// before the first non comment, non blank line, and inserting comments above it
// does not change that. The notice is followed by a blank line so it can never
// be absorbed into a package documentation comment, and it is inserted before
// an existing documentation comment rather than between it and the package
// clause, which would detach the documentation from the package it describes.
//
// The notice is terminated the way the file terminates its own lines, so a
// preserved CRLF file does not acquire a handful of LF lines in its preamble.
func noticeEdit(source *goSource, file File, opts Options, terminator string) edit {
	anchor := source.file.Package
	if source.file.Doc != nil {
		anchor = source.file.Doc.Pos()
	}
	offset := lineStart(source.src, source.offset(anchor))
	text := noticeText(file, opts, terminator)
	return edit{
		start: offset,
		end:   offset,
		text:  text,
		change: Change{
			Kind: ChangeNotice,
			Path: file.Path,
			Line: source.line(anchor),
			To:   text,
		},
	}
}

// noticeText renders the notice and the blank line that separates it from what
// follows, using the file's own line terminator.
func noticeText(file File, opts Options, terminator string) string {
	return strings.Join(noticeLines(file, opts), terminator) + terminator + terminator
}

// noticeLines renders the notice deterministically. A field the caller did not
// supply is left out rather than rendered empty, so the notice never claims an
// unknown provenance.
func noticeLines(file File, opts Options) []string {
	lines := []string{noticeIntro}
	for _, field := range []struct {
		label string
		value string
	}{
		{"Upstream repository", opts.SourceRepository},
		{"Upstream path", file.SourcePath},
		{"Upstream commit", opts.SourceSHA},
	} {
		if field.value != "" {
			lines = append(lines, "// "+field.label+": "+field.value)
		}
	}
	return append(lines, "// Imports under "+opts.SourcePrefix+" were rewritten to "+
		opts.DestinationModule+"/"+opts.InternalPrefix+".")
}

// lineStart reports the offset of the first byte of the line holding offset.
func lineStart(src []byte, offset int) int {
	for i := offset - 1; i >= 0; i-- {
		if src[i] == '\n' {
			return i + 1
		}
	}
	return 0
}

// lineEnd reports the offset just past the newline that ends the line holding
// offset, or the end of the file when the last line has no newline.
func lineEnd(src []byte, offset int) int {
	for i := offset; i < len(src); i++ {
		if src[i] == '\n' {
			return i + 1
		}
	}
	return len(src)
}

// ownsLine reports whether only whitespace precedes offset on its line, which
// is what makes a comment safe to remove whole. A directive written after code
// on the same line is left alone: removing its line would delete the code.
func ownsLine(src []byte, offset int) bool {
	for i := lineStart(src, offset); i < offset; i++ {
		if src[i] != ' ' && src[i] != '\t' {
			return false
		}
	}
	return true
}

// blankLine reports whether the line beginning at offset holds nothing but
// whitespace. An offset at or past the end of the file is not a line.
func blankLine(src []byte, offset int) bool {
	if offset >= len(src) {
		return false
	}
	for i := offset; i < len(src); i++ {
		switch src[i] {
		case '\n':
			return true
		case ' ', '\t', '\r':
		default:
			return false
		}
	}
	return true
}

// previousLineStart reports the first byte of the line before the one beginning
// at offset, or -1 when offset is the start of the file.
func previousLineStart(src []byte, offset int) int {
	if offset <= 0 {
		return -1
	}
	return lineStart(src, offset-1)
}

// blankRunStart reports the first byte of the run of blank lines immediately
// above offset, or offset itself when the line above is not blank.
func blankRunStart(src []byte, offset int) int {
	for offset > 0 {
		previous := previousLineStart(src, offset)
		if previous < 0 || !blankLine(src, previous) {
			break
		}
		offset = previous
	}
	return offset
}

// blankRunEnd reports the offset just past the run of blank lines immediately
// below offset, or offset itself when the next line is not blank.
func blankRunEnd(src []byte, offset int) int {
	for offset < len(src) && blankLine(src, offset) {
		offset = lineEnd(src, offset)
	}
	return offset
}

// onlyBlank reports whether a byte range holds nothing but whitespace.
func onlyBlank(src []byte, from, to int) bool {
	for i := max(from, 0); i < min(to, len(src)); i++ {
		switch src[i] {
		case ' ', '\t', '\r', '\n':
		default:
			return false
		}
	}
	return true
}
