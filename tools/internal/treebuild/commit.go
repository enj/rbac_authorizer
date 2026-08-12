package treebuild

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/gitgraph"
)

// ErrProvenanceTrailer reports a message the provenance trailer could not be
// appended to so that git reads it back. It is a refusal rather than a
// best effort, because a published commit whose provenance git cannot parse is
// a commit the engine can never map back to its source.
var ErrProvenanceTrailer = errors.New("provenance trailer would not be read back as a trailer")

// CommitOptions describes one replayed commit.
//
// The upstream identity and message are carried rather than looked up, so this
// package never decides what a commit came from. The caller reads them from the
// source commit and hands them over, which is also what keeps the raw dates
// honest: they have to be the bytes git stored upstream, not a rendering of them.
type CommitOptions struct {
	// Tree is the tree object the commit records.
	Tree string
	// Parents are the destination parent commits in order. The first parent
	// defines the mainline and a root commit has none.
	Parents []string
	// Author is the upstream author, with the upstream raw author date.
	Author gitcli.Signature
	// Committer is the engine's bot identity, with a raw date.
	Committer gitcli.Signature
	// Message is the upstream commit message, preserved as written.
	Message string
	// ProvenanceKey is the trailer key the source commit is recorded under,
	// such as Kubernetes-commit.
	ProvenanceKey string
	// Source is the upstream commit object name recorded under ProvenanceKey.
	Source string
}

// WriteCommit shapes and writes one replayed commit, reporting its object name.
//
// The shape is the contract with the published history. The upstream author and
// author date are preserved so the commit is still attributable to the person
// who wrote it; the committer is the engine's bot, because the engine is what
// recorded this particular commit and pretending otherwise would forge an
// identity; the message is the upstream one with exactly one provenance trailer
// appended; and nothing is signed, because a generated commit has no signer.
func WriteCommit(ctx context.Context, git *gitcli.Runner, opts CommitOptions) (string, error) {
	if err := gitgraph.ValidateSHA(opts.Source); err != nil {
		return "", fmt.Errorf("treebuild commit: source: %w", err)
	}
	message, err := ProvenanceMessage(opts.Message, opts.ProvenanceKey, opts.Source)
	if err != nil {
		return "", fmt.Errorf("treebuild commit: %w", err)
	}
	return writeCommit(ctx, git, opts.Tree, opts.Parents, message, opts.Author, opts.Committer)
}

// SyntheticCommitOptions describes one commit the engine authored itself.
type SyntheticCommitOptions struct {
	// Tree is the tree object the commit records.
	Tree string
	// Parents are the destination parent commits in order.
	Parents []string
	// Author is the identity the change is attributed to, with a raw date.
	Author gitcli.Signature
	// Committer is the identity that recorded it, with a raw date.
	Committer gitcli.Signature
	// Message is the complete commit message, preserved verbatim.
	Message string
}

// WriteSyntheticCommit writes a commit that no upstream commit produced, such as
// a dependency update or a generated facade change, and reports its object name.
//
// It carries no provenance trailer, and that absence is the point rather than an
// omission. The trailer is how a resumed run rebuilds the source to destination
// mapping from the published history, so a generated commit claiming a source
// commit would map that source onto a commit it never produced. A commit with no
// trailer is simply skipped by the reader, which is the correct answer.
func WriteSyntheticCommit(ctx context.Context, git *gitcli.Runner, opts SyntheticCommitOptions) (string, error) {
	if strings.TrimSpace(opts.Message) == "" {
		return "", errors.New("treebuild commit: a message is required")
	}
	return writeCommit(ctx, git, opts.Tree, opts.Parents, opts.Message, opts.Author, opts.Committer)
}

// writeCommit validates the parts every commit shares and writes it.
func writeCommit(ctx context.Context, git *gitcli.Runner, tree string, parents []string, message string, author, committer gitcli.Signature) (string, error) {
	if err := gitgraph.ValidateSHA(tree); err != nil {
		return "", fmt.Errorf("treebuild commit: tree: %w", err)
	}
	for i, parent := range parents {
		if err := gitgraph.ValidateSHA(parent); err != nil {
			return "", fmt.Errorf("treebuild commit: parent %d: %w", i, err)
		}
	}
	if err := validateRawDate("author", author.Date); err != nil {
		return "", fmt.Errorf("treebuild commit: %w", err)
	}
	if err := validateRawDate("committer", committer.Date); err != nil {
		return "", fmt.Errorf("treebuild commit: %w", err)
	}
	commit, err := git.WriteCommit(ctx, gitcli.CommitTreeOptions{
		Tree:      tree,
		Parents:   parents,
		Message:   message,
		Author:    author,
		Committer: committer,
	})
	if err != nil {
		return "", fmt.Errorf("treebuild commit: %w", err)
	}
	return commit, nil
}

// ProvenanceMessage returns message carrying exactly one effective provenance
// trailer.
//
// "Effective" is the whole difficulty. A trailer is only a trailer where git
// says it is: in the last paragraph of the message, before any patch, and only
// when every line of that paragraph is itself a trailer, a comment, or a
// continuation. Appending a line to a message that ends in prose does not
// produce a trailer, it produces one more line of prose, and the published
// commit would then claim no source at all. So the message is extended in
// whichever of the two ways git will read: onto an existing trailer block, or as
// a new paragraph after one that is not.
//
// An upstream trailer under the same key is kept rather than replaced. The
// engine's claim is appended last, and git reports the last value for a key, so
// the engine's claim is the effective one while the upstream record of whatever
// it meant survives in the published message.
//
// The result is then read back through the same parser the rest of the engine
// reads provenance with, and a message that does not parse as intended is
// refused. That check is what makes the writing safe: this function decides
// where to write, and gitgraph remains the authority on what a trailer is, so
// any disagreement between the two ends as a refusal rather than as a commit
// with unreadable provenance.
func ProvenanceMessage(message, key, value string) (string, error) {
	if err := validateTrailerKey(key); err != nil {
		return "", err
	}
	if err := validateTrailerValue(value); err != nil {
		return "", err
	}
	body, patch := splitPatch(message)
	if strings.TrimSpace(body) == "" {
		return "", errors.New("a commit message is required")
	}
	before := gitgraph.TrailerBlock(message)

	var out strings.Builder
	// Trailing blank lines are collapsed to a single terminator. A trailer
	// cannot follow a blank line inside its own paragraph, so preserving them
	// would place the trailer in a paragraph git refuses to read. A carriage
	// return is trimmed with them, because a message with CRLF endings would
	// otherwise keep one and turn the separator into a blank line.
	out.WriteString(strings.TrimRight(body, "\r\n"))
	out.WriteString("\n")
	if len(before) == 0 {
		out.WriteString("\n")
	}
	out.WriteString(key)
	out.WriteString(": ")
	out.WriteString(value)
	out.WriteString("\n")
	out.WriteString(patch)
	result := out.String()

	after := gitgraph.TrailerBlock(result)
	if len(after) != len(before)+1 {
		return "", fmt.Errorf("%w: the message holds %d trailers, want %d", ErrProvenanceTrailer, len(after), len(before)+1)
	}
	for i, trailer := range before {
		if after[i] != trailer {
			return "", fmt.Errorf("%w: trailer %d changed from %q to %q",
				ErrProvenanceTrailer, i, trailer.Key+": "+trailer.Value, after[i].Key+": "+after[i].Value)
		}
	}
	if last := after[len(after)-1]; last.Key != key || last.Value != value {
		return "", fmt.Errorf("%w: the last trailer is %q, want %q",
			ErrProvenanceTrailer, last.Key+": "+last.Value, key+": "+value)
	}
	return result, nil
}

// splitPatch separates the message from a patch appended to it.
//
// git treats a line of exactly three dashes followed by whitespace, or nothing,
// as the start of a patch: text after it is a diff rather than message text, and
// a trailer appended there is not a trailer. The rule is mirrored here because
// this decides where to write, while gitgraph decides what is read; the two are
// reconciled by reading the result back, so a mirror that drifted causes a
// refusal rather than a bad commit.
//
// A carriage return is stripped before the line is judged. Upstream messages
// carrying CRLF endings are real, and without this their "---\r" separator is
// not recognised as one, so the trailer would be appended after the patch
// instead of before it. The carriage return counts as the trailing whitespace
// git's own rule allows.
func splitPatch(message string) (body, patch string) {
	for offset := 0; offset < len(message); {
		line := message[offset:]
		if end := strings.IndexByte(line, '\n'); end >= 0 {
			line = line[:end]
		}
		if rest, ok := strings.CutPrefix(strings.TrimSuffix(line, "\r"), "---"); ok &&
			(rest == "" || rest[0] == ' ' || rest[0] == '\t') {
			return message[:offset], message[offset:]
		}
		offset += len(line) + 1
	}
	return message, ""
}

// validateTrailerKey applies git's token rule: one or more alphanumerics and
// dashes. A key git would not recognise as a token produces a line that looks
// like provenance and is read as prose.
func validateTrailerKey(key string) error {
	if key == "" {
		return errors.New("a trailer key is required")
	}
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
		default:
			return fmt.Errorf("trailer key %q must contain only letters, digits, and dashes", key)
		}
	}
	return nil
}

// validateTrailerValue rejects a value that would not survive being read back.
//
// Surrounding whitespace is refused rather than trimmed: the reader trims it, so
// a value written with it would not compare equal to the value the caller asked
// to record, and the caller should learn that here rather than from a mapping
// that quietly does not match.
func validateTrailerValue(value string) error {
	switch {
	case value == "":
		return errors.New("a trailer value is required")
	case strings.ContainsAny(value, "\n\r\x00"):
		return fmt.Errorf("trailer value %q must not contain a line break or a null byte", value)
	case strings.TrimSpace(value) != value:
		return fmt.Errorf("trailer value %q must not begin or end with whitespace", value)
	}
	return nil
}
