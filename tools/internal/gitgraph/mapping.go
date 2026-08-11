package gitgraph

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Mapping records which destination commit each source commit produced.
//
// The relation is many to one: several source commits collapse onto the same
// destination commit when their transformed trees are identical, which is the
// normal outcome for a commit that touched nothing the extraction keeps.
type Mapping struct {
	forward map[string]string
	reverse map[string]string
}

// NewMapping returns an empty mapping.
func NewMapping() *Mapping {
	return &Mapping{
		forward: make(map[string]string),
		reverse: make(map[string]string),
	}
}

// Set records that source produced destination. Recording the same pair twice
// is accepted so a resumed run can replay what it already knows, while pointing
// one source commit at a second destination commit is refused: published history
// is append only, and a source commit that suddenly produced something else
// means the mapping was rebuilt from inconsistent inputs.
func (m *Mapping) Set(source, destination string) error {
	if err := ValidateSHA(source); err != nil {
		return fmt.Errorf("commit mapping source: %w", err)
	}
	if err := ValidateSHA(destination); err != nil {
		return fmt.Errorf("commit mapping destination: %w", err)
	}
	if existing, ok := m.forward[source]; ok {
		if existing == destination {
			return nil
		}
		return fmt.Errorf("commit mapping: source %s is already mapped to %s, not %s", source, existing, destination)
	}
	m.forward[source] = destination
	// Several sources may share a destination, so the reverse direction keeps
	// the first one recorded and later ones do not overwrite it.
	if _, ok := m.reverse[destination]; !ok {
		m.reverse[destination] = source
	}
	return nil
}

// Destination reports the destination commit produced by a source commit.
func (m *Mapping) Destination(source string) (string, bool) {
	if m == nil {
		return "", false
	}
	destination, ok := m.forward[source]
	return destination, ok
}

// Source reports the source commit a destination commit was produced from. When
// several source commits collapsed onto it, the first one recorded is reported.
func (m *Mapping) Source(destination string) (string, bool) {
	if m == nil {
		return "", false
	}
	source, ok := m.reverse[destination]
	return source, ok
}

// Len reports how many source commits are mapped.
func (m *Mapping) Len() int {
	if m == nil {
		return 0
	}
	return len(m.forward)
}

// Sources reports every mapped source commit, sorted by object name so a report
// built from a mapping is reproducible.
func (m *Mapping) Sources() []string {
	if m == nil {
		return nil
	}
	sources := make([]string, 0, len(m.forward))
	for source := range m.forward {
		sources = append(sources, source)
	}
	slices.Sort(sources)
	return sources
}

// TrailerValue reports the last value recorded under key in a commit message.
//
// The rules are git's own, because the value read here is provenance: it decides
// which source commit a published commit claims to have come from, and a parser
// that accepted more than git does would let an ordinary body line be mistaken
// for that claim. TrailerBlock documents the exact subset, and the differential
// test in the gitcli tests checks it against git interpret-trailers --parse.
func TrailerValue(message, key string) (string, bool) {
	key = strings.TrimSpace(key)
	if key == "" {
		return "", false
	}
	value, found := "", false
	for _, trailer := range TrailerBlock(message) {
		if strings.EqualFold(trailer.Key, key) {
			value, found = trailer.Value, true
		}
	}
	return value, found
}

// Trailer is one parsed commit message trailer.
type Trailer struct {
	// Key is the trailer token, with no surrounding whitespace.
	Key string
	// Value is the rest of the line, with continuation lines folded in.
	Value string
}

// TrailerBlock reports the trailers of a commit message, in order.
//
// This implements the subset of git's rules that a generated commit message can
// exercise, and it is deliberately as strict as git is:
//
//   - Only the last paragraph is considered, and it must not also be the first.
//     A single paragraph message is its subject, so "Fix: thing" is a subject
//     and not a trailer no matter how much it looks like one.
//   - Anything from a line beginning "---" followed by whitespace is a patch,
//     not part of the message.
//   - Every line of the block must be a trailer, a comment, or a continuation
//     of a preceding trailer. One ordinary prose line disqualifies the whole
//     block, which is what stops a closing paragraph that happens to contain a
//     colon from being read as provenance.
//   - A token is one or more alphanumerics and dashes. Whitespace between the
//     token and the colon is allowed and dropped.
//   - A line that starts with whitespace continues the trailer above it. It is
//     never a trailer of its own, so an indented "Source: other" inside a
//     quoted message cannot introduce a second claim.
func TrailerBlock(message string) []Trailer {
	lines := trailerBlockLines(message)
	if len(lines) == 0 {
		return nil
	}
	var trailers []Trailer
	for _, line := range lines {
		switch {
		case isComment(line):
			continue
		case isContinuation(line):
			// The block was already rejected if a continuation had nothing to
			// continue, so there is always a trailer to fold into.
			last := &trailers[len(trailers)-1]
			last.Value = strings.TrimRight(last.Value+" "+strings.TrimSpace(line), " ")
			continue
		}
		token, value, _ := strings.Cut(line, trailerSeparator)
		trailers = append(trailers, Trailer{
			Key:   strings.TrimRight(token, " \t"),
			Value: strings.TrimSpace(value),
		})
	}
	return trailers
}

// trailerSeparator is the only separator git recognises by default. The
// configurable trailer.separators is not honoured, because a generated message
// is written by this engine rather than by an operator's configuration.
const trailerSeparator = ":"

// trailerBlockLines returns the lines of the message's trailer block, or nil
// when the message has none.
func trailerBlockLines(message string) []string {
	normalized := strings.ReplaceAll(message, "\r\n", "\n")
	normalized = normalized[:patchStart(normalized)]
	// Trailing blank lines belong to no paragraph, and a message that is nothing
	// but blank lines has no block at all.
	normalized = strings.TrimRight(normalized, "\n \t")
	if normalized == "" {
		return nil
	}

	// The block is the last paragraph, and a message with only one paragraph is
	// a subject: git never reads the first paragraph as trailers.
	separator := strings.LastIndex(normalized, "\n\n")
	if separator < 0 {
		return nil
	}
	lines := strings.Split(normalized[separator+2:], "\n")

	trailers := 0
	for _, line := range lines {
		switch {
		case isComment(line):
		case isTrailerLine(line):
			trailers++
		case isContinuation(line):
			// A continuation only continues something. Before the first trailer
			// of the block there is nothing to continue, so the line is prose.
			if trailers == 0 {
				return nil
			}
		default:
			return nil
		}
	}
	if trailers == 0 {
		return nil
	}
	return lines
}

// patchStart reports where the patch part of a message begins, which is the
// first line starting with three dashes followed by whitespace or the end of
// the line. Text before it is the message; everything after it is a diff that
// may legitimately contain anything.
func patchStart(message string) int {
	for offset := 0; offset < len(message); {
		line := message[offset:]
		if end := strings.IndexByte(line, '\n'); end >= 0 {
			line = line[:end]
		}
		if rest, ok := strings.CutPrefix(line, "---"); ok && (rest == "" || rest[0] == ' ' || rest[0] == '\t') {
			return offset
		}
		offset += len(line) + 1
	}
	return len(message)
}

// isComment reports a line git ignores inside a trailer block.
func isComment(line string) bool { return strings.HasPrefix(line, "#") }

// isContinuation reports a line that folds into the trailer above it.
func isContinuation(line string) bool {
	return line != "" && (line[0] == ' ' || line[0] == '\t')
}

// isTrailerLine reports a line of the form "<token><optional space>:<value>",
// where the token is one or more alphanumerics and dashes. Git's own token rule
// is exactly this, which is why an otherwise ordinary "See also: x" or a key
// carrying an underscore is not a trailer.
func isTrailerLine(line string) bool {
	token, _, ok := strings.Cut(line, trailerSeparator)
	if !ok {
		return false
	}
	token = strings.TrimRight(token, " \t")
	if token == "" {
		return false
	}
	for _, r := range token {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
		default:
			return false
		}
	}
	return true
}

// MappingFromTrailers rebuilds the source to destination mapping by reading the
// provenance trailer of every destination commit.
//
// This is how a resumed run recovers what it already published without keeping
// state anywhere else: the destination history is its own record. Commits
// without the trailer are skipped, because a destination branch legitimately
// carries generated commits that no single source commit produced.
func MappingFromTrailers(destination []Commit, key string) (*Mapping, error) {
	if strings.TrimSpace(key) == "" {
		return nil, errors.New("commit mapping: a trailer key is required")
	}
	mapping := NewMapping()
	for _, commit := range destination {
		source, ok := TrailerValue(commit.Message, key)
		if !ok {
			continue
		}
		if err := ValidateSHA(source); err != nil {
			return nil, fmt.Errorf("commit mapping: %s trailer of %s: %w", key, commit.SHA, err)
		}
		if err := mapping.Set(source, commit.SHA); err != nil {
			return nil, err
		}
	}
	return mapping, nil
}
