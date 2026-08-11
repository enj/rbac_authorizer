package rewrite

import (
	"context"
	"fmt"
	"go/ast"
	"go/token"
	"path"
	"slices"
	"strconv"
	"strings"
)

// Embed is one verified go:embed directive.
type Embed struct {
	// Line is the one based line the directive sits on.
	Line int
	// Patterns are the patterns the directive names, in written order.
	Patterns []string
	// Matches are the destination paths the patterns resolve to, sorted.
	Matches []string
}

// VerifyEmbeds checks that every go:embed directive in a relocated Go file
// still resolves.
//
// Embed patterns are verified rather than rewritten. A pattern names a path
// relative to the file's own directory, and relocation moves a file and its
// assets together, so a pattern that was correct upstream stays correct without
// being touched. What relocation can break is the assumption that the asset was
// copied at all: the closure selects Go files by import, and a data file only
// arrives if it was matched by an asset rule. A pattern that resolves to
// nothing is exactly that failure, caught here rather than at the first build
// of the published module.
//
// Only comments the toolchain itself would read as directives are considered.
// cmd/go accepts //go:embed on the documentation comment of a package level var
// declaration with no initializer, in a file that imports embed, and treats the
// identical text anywhere else, including inside a function body or after code
// on a line, as an ordinary comment. Reading more than the toolchain does would
// make this fail a build that works, which is a worse failure than the one it
// is here to prevent.
//
// The supported matching subset is the one Kubernetes uses: literal paths, per
// element globs, directory trees, and the all: prefix. Directory expansion
// skips names beginning with a dot or an underscore unless all: was given,
// matching the toolchain.
func VerifyEmbeds(ctx context.Context, file File, present []string) ([]Embed, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("verify embeds in %s: %w", file.Path, err)
	}
	source, err := parseGo(file.Path, file.Contents)
	if err != nil {
		return nil, err
	}
	// Without the embed import the directive is inert, and cmd/go rejects the
	// file rather than embedding anything, so there is nothing here to verify.
	if !importsEmbed(source.file) {
		return nil, nil
	}

	directives := embedComments(source.file)
	dir := path.Dir(file.Path)
	var embeds []Embed
	for _, group := range source.file.Comments {
		if !directives[group] {
			continue
		}
		for _, comment := range group.List {
			// The directive has to start the line's comment text exactly, which
			// is what separates //go:embed from a sentence about it.
			value, ok := strings.CutPrefix(comment.Text, "//go:embed")
			if !ok || (value != "" && !isSpace(value[0])) {
				continue
			}
			line := source.line(comment.Pos())
			patterns, err := splitPatterns(value)
			if err != nil {
				return nil, fmt.Errorf("%s:%d: %w", file.Path, line, err)
			}
			if len(patterns) == 0 {
				return nil, fmt.Errorf("%s:%d: go:embed names no pattern", file.Path, line)
			}
			embed := Embed{Line: line, Patterns: patterns}
			for _, pattern := range patterns {
				matches, err := matchEmbed(dir, pattern, present)
				if err != nil {
					return nil, fmt.Errorf("%s:%d: go:embed %s: %w", file.Path, embed.Line, pattern, err)
				}
				embed.Matches = append(embed.Matches, matches...)
			}
			slices.Sort(embed.Matches)
			embed.Matches = slices.Compact(embed.Matches)
			embeds = append(embeds, embed)
		}
	}
	return embeds, nil
}

// importsEmbed reports whether the file imports embed, under any name. A blank
// import is the usual form when only the directive is wanted.
func importsEmbed(file *ast.File) bool {
	for _, spec := range file.Imports {
		if path, err := strconv.Unquote(spec.Path.Value); err == nil && path == "embed" {
			return true
		}
	}
	return false
}

// embedComments reports the comment groups a go:embed directive may legally sit
// in: the documentation of a package level var declaration that declares
// without initializing, and the line comment of such a declaration's spec.
//
// A var with an initializer is excluded because cmd/go excludes it; the
// directive needs a variable whose value it can supply.
func embedComments(file *ast.File) map[*ast.CommentGroup]bool {
	groups := make(map[*ast.CommentGroup]bool)
	for _, decl := range file.Decls {
		declaration, ok := decl.(*ast.GenDecl)
		if !ok || declaration.Tok != token.VAR {
			continue
		}
		for _, spec := range declaration.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok || len(value.Values) > 0 {
				continue
			}
			for _, group := range []*ast.CommentGroup{declaration.Doc, value.Doc, value.Comment} {
				if group != nil {
					groups[group] = true
				}
			}
		}
	}
	return groups
}

// splitPatterns splits the patterns of one go:embed directive.
//
// The rules are the toolchain's. Patterns are separated by any whitespace, not
// only by a space, so a tab separated list is one list rather than one pattern
// holding a tab. A pattern may be a double quoted Go string literal, with the
// escapes that implies, or a backquoted raw literal, either of which lets it
// hold a space. An unterminated or invalid literal is refused rather than
// silently re-read as whitespace separated words, which would turn one
// malformed pattern into several plausible ones.
func splitPatterns(value string) ([]string, error) {
	var patterns []string
	rest := strings.TrimSpace(value)
	for rest != "" {
		var pattern string
		switch rest[0] {
		case '`':
			quoted, remainder, ok := strings.Cut(rest[1:], "`")
			if !ok {
				return nil, fmt.Errorf("%w: unterminated raw literal %s", ErrEmbedPattern, rest)
			}
			pattern, rest = quoted, remainder
		case '"':
			end := 1
			for ; end < len(rest) && rest[end] != '"'; end++ {
				if rest[end] == '\\' {
					end++
				}
			}
			if end >= len(rest) {
				return nil, fmt.Errorf("%w: unterminated quoted literal %s", ErrEmbedPattern, rest)
			}
			unquoted, err := strconv.Unquote(rest[:end+1])
			if err != nil {
				return nil, fmt.Errorf("%w: invalid quoted literal %s: %w", ErrEmbedPattern, rest[:end+1], err)
			}
			pattern, rest = unquoted, rest[end+1:]
		default:
			// The whitespace that ends the pattern is left in place so the
			// separator check below sees it, exactly as it sees the byte after
			// a closing quote.
			end := 0
			for end < len(rest) && !isSpace(rest[end]) {
				end++
			}
			pattern, rest = rest[:end], rest[end:]
		}
		// A quoted pattern that runs straight into another token is malformed;
		// the toolchain refuses it rather than guessing where the split was.
		if rest != "" && !isSpace(rest[0]) {
			return nil, fmt.Errorf("%w: %s does not separate its patterns", ErrEmbedPattern, value)
		}
		if pattern != "" {
			patterns = append(patterns, pattern)
		}
		rest = strings.TrimSpace(rest)
	}
	return patterns, nil
}

// isSpace reports the whitespace bytes that separate go:embed patterns.
func isSpace(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\v', '\f', '\r':
		return true
	default:
		return false
	}
}

// matchEmbed resolves one pattern against the relocated file set.
func matchEmbed(dir, pattern string, present []string) ([]string, error) {
	cleaned, all := strings.CutPrefix(pattern, "all:")
	switch {
	case cleaned == "":
		return nil, fmt.Errorf("%w: pattern is empty", ErrEmbedUnmatched)
	case path.IsAbs(cleaned), strings.HasPrefix(cleaned, "../"), cleaned == "..",
		strings.Contains(cleaned, "/../"), strings.HasSuffix(cleaned, "/.."):
		return nil, ErrEmbedEscape
	}
	if _, err := path.Match(cleaned, "probe"); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrEmbedUnmatched, err)
	}

	var matches []string
	for _, candidate := range present {
		rel, ok := relativeTo(dir, candidate)
		if !ok {
			continue
		}
		if matchesEmbedPattern(cleaned, rel, all) {
			matches = append(matches, candidate)
		}
	}
	if len(matches) == 0 {
		return nil, ErrEmbedUnmatched
	}
	slices.Sort(matches)
	return matches, nil
}

// relativeTo reports a path relative to dir, and whether it is below dir.
func relativeTo(dir, candidate string) (string, bool) {
	if dir == "." {
		return candidate, true
	}
	rel, ok := strings.CutPrefix(candidate, dir+"/")
	return rel, ok
}

// matchesEmbedPattern reports whether one relative path is embedded by a
// pattern, either by matching it directly or by living below a directory the
// pattern matches.
func matchesEmbedPattern(pattern, rel string, all bool) bool {
	if matched, err := path.Match(pattern, rel); err == nil && matched {
		return true
	}
	for dir := path.Dir(rel); dir != "." && dir != "/"; dir = path.Dir(dir) {
		matched, err := path.Match(pattern, dir)
		if err != nil || !matched {
			continue
		}
		// The pattern named a directory, so the whole subtree is embedded
		// except for names the toolchain hides.
		return all || !hidden(strings.TrimPrefix(rel, dir+"/"))
	}
	return false
}

// hidden reports whether any element of a path begins with a dot or an
// underscore, which excludes it from a directory expansion.
func hidden(rel string) bool {
	for _, element := range strings.Split(rel, "/") {
		if strings.HasPrefix(element, ".") || strings.HasPrefix(element, "_") {
			return true
		}
	}
	return false
}
