package gitcli

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// CreateRef points a new local ref at a commit and fails when the ref already
// exists.
//
// Git spells that precondition as an empty expected value, and it is the honest
// default for published history: a name that is already taken means something
// else created it, which is a fact the caller has to reconcile rather than
// overwrite.
func (r *Runner) CreateRef(ctx context.Context, name, revision string) error {
	return r.updateRef(ctx, name, revision, "")
}

// UpdateRef moves an existing local ref to a commit, failing unless the ref
// currently holds expected.
//
// The comparison is the point. Without it an update is a blind write: a
// concurrent run, a stale in memory value, or a local rewind between reading a
// ref and writing it would all be applied silently, and the ref the engine
// publishes from would no longer be the one it reasoned about. Git performs the
// comparison and the update under the ref lock, so the check cannot be raced.
//
// This writes only to the local repository. There is still no delete and no
// reflog rewriting, and the append only guarantees the engine publishes under
// live in Push, which is the only call that reaches a remote.
func (r *Runner) UpdateRef(ctx context.Context, name, revision, expected string) error {
	if expected == "" {
		return fmt.Errorf("git update-ref %q: an expected object name is required, use CreateRef to create a ref", name)
	}
	if err := validateRevision(expected); err != nil {
		return fmt.Errorf("git update-ref: expected value: %w", err)
	}
	return r.updateRef(ctx, name, revision, expected)
}

// updateRef performs one compare and swap ref update. An empty expected value is
// git's own spelling of "this ref must not exist yet".
func (r *Runner) updateRef(ctx context.Context, name, revision, expected string) error {
	if err := ValidateRefName(name); err != nil {
		return fmt.Errorf("git update-ref: %w", err)
	}
	if err := validateRevision(revision); err != nil {
		return fmt.Errorf("git update-ref: %w", err)
	}
	commit, err := r.ResolveCommit(ctx, revision)
	if err != nil {
		return fmt.Errorf("git update-ref: %w", err)
	}
	// The expected value is passed as written rather than resolved here, because
	// git resolves it while holding the ref lock and resolving it earlier would
	// reintroduce the window the comparison exists to close.
	if _, err := r.run(ctx, "update-ref", "--end-of-options", name, commit, expected); err != nil {
		return fmt.Errorf("git update-ref %q: %w", name, err)
	}
	return nil
}

// CommitTreeOptions describes one commit object written directly from a tree.
type CommitTreeOptions struct {
	// Tree is the tree object the commit records.
	Tree string
	// Parents are the parent commits in order. The first parent defines the
	// mainline, and more than one parent produces a merge.
	Parents []string
	// Message is the complete commit message, preserved verbatim.
	Message string
	// Author is the identity the change is attributed to.
	Author Signature
	// Committer is the identity that recorded it.
	Committer Signature
}

// WriteCommit writes a commit object without touching the index or the work
// tree, and reports its object name.
//
// Building commits from objects rather than from a checkout is what lets replay
// reproduce a graph exactly: parents are stated rather than inferred from HEAD,
// identity travels through the environment, and nothing depends on the state of
// a work tree that another step may have changed.
//
// Every input is pinned to what the commit will record. The tree and the parents
// must be full object names, because a revision git would resolve, HEAD or a
// branch or an abbreviation, makes the written commit depend on the repository's
// current state rather than on what the caller described. Both identities must
// be complete, because git fills a missing name or address in from the
// environment and would attribute the commit to whoever happened to run it. The
// reported name is checked for the reason WriteTree checks its own.
//
// Dates use Git's raw form as well. An omitted date makes commit-tree record the
// wall clock, while a friendly date string asks Git to reinterpret caller input;
// either would make the object name depend on when or where this runs rather than
// only on the fields supplied here.
func (r *Runner) WriteCommit(ctx context.Context, opts CommitTreeOptions) (string, error) {
	if !isObjectName(opts.Tree) {
		return "", fmt.Errorf("git commit-tree: tree %q must be a full object name", opts.Tree)
	}
	if opts.Message == "" {
		return "", errors.New("git commit-tree: a message is required")
	}
	if err := validateIdentity(opts.Author); err != nil {
		return "", fmt.Errorf("git commit-tree: author: %w", err)
	}
	if err := validateIdentity(opts.Committer); err != nil {
		return "", fmt.Errorf("git commit-tree: committer: %w", err)
	}
	args := []string{"commit-tree", "--no-gpg-sign"}
	for i, parent := range opts.Parents {
		if !isObjectName(parent) {
			return "", fmt.Errorf("git commit-tree: parent %d %q must be a full object name", i, parent)
		}
		args = append(args, "-p", parent)
	}
	// The message travels on standard input so a message beginning with a dash,
	// or carrying anything git might read as an option, cannot reach the
	// argument vector.
	args = append(args, "--end-of-options", opts.Tree)

	env := append(opts.Author.env("AUTHOR"), opts.Committer.env("COMMITTER")...)
	out, err := r.runInput(ctx, []byte(opts.Message), env, args...)
	if err != nil {
		return "", fmt.Errorf("git commit-tree: %w", err)
	}
	commit := strings.TrimSpace(out)
	if !isObjectName(commit) {
		return "", fmt.Errorf("git commit-tree: %q is not an object name", commit)
	}
	return commit, nil
}

// TagOptions describes one tag to create.
type TagOptions struct {
	// Name is the short tag name, such as v0.36.1.
	Name string
	// Commit is the revision the tag points at.
	Commit string
	// Message makes the tag annotated. An empty message creates a lightweight
	// tag, which release tags never are, because a release records its own
	// tagger identity and date.
	Message string
	// Tagger is the identity and date recorded in an annotated tag. The date is
	// taken from upstream so a regenerated tag is byte identical.
	Tagger Signature
}

// CreateTag creates an annotated or lightweight tag. Replacing an existing tag
// is not possible here: published tags are immutable, so a name that already
// exists fails rather than moving.
//
// An annotated message is preserved byte for byte. Git's default cleanup for a
// supplied message strips trailing whitespace, collapses trailing blank lines,
// and deletes every line beginning with a hash, so an upstream release note that
// happened to contain one would be published with that line missing and the tag
// would not be the one it claims to reproduce. The message also travels on
// standard input, which keeps a message beginning with a dash out of the
// argument vector.
func (r *Runner) CreateTag(ctx context.Context, opts TagOptions) error {
	if err := ValidateBranchName(opts.Name); err != nil {
		return fmt.Errorf("git tag: %w", err)
	}
	if err := validateRevision(opts.Commit); err != nil {
		return fmt.Errorf("git tag: %w", err)
	}
	var message []byte
	args := []string{"tag"}
	if opts.Message != "" {
		args = append(args, "--annotate", "--cleanup=verbatim", "--file=-")
		message = []byte(opts.Message)
	}
	args = append(args, "--end-of-options", opts.Name, opts.Commit)

	// An annotated tag records its tagger from the committer identity and date,
	// so there is no separate author role to set here.
	if _, err := r.runInput(ctx, message, opts.Tagger.env("COMMITTER"), args...); err != nil {
		return fmt.Errorf("git tag %q: %w", opts.Name, err)
	}
	return nil
}
