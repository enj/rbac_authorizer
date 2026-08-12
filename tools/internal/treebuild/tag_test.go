package treebuild_test

import (
	"context"
	"errors"
	"testing"

	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/treebuild"
)

// writeCommit records a commit on a small tree and reports its object name.
func writeCommit(ctx context.Context, t *testing.T, git *gitcli.Runner) string {
	t.Helper()
	bot := gitcli.Signature{Name: "Soapbox Bot", Email: "bot@example.com", Date: "1700000000 +0000"}
	sha, err := treebuild.WriteSyntheticCommit(ctx, git, treebuild.SyntheticCommitOptions{
		Tree:      writeTree(ctx, t, git),
		Message:   "chore: generated tree\n",
		Author:    bot,
		Committer: bot,
	})
	if err != nil {
		t.Fatalf("write commit: %v", err)
	}
	return sha
}

// TestWriteTagIsDeterministicAndPreservesTheMessage checks that a release tag
// object reproduces exactly, message included.
//
// The message shapes here are the ones git's own tag writer would destroy. Its
// default cleanup deletes lines beginning with a hash and strips trailing
// whitespace, so an upstream release note containing either would be published
// with text missing while still claiming to be that release.
func TestWriteTagIsDeterministicAndPreservesTheMessage(t *testing.T) {
	messages := []struct {
		name    string
		message string
	}{
		{name: "plain", message: "Kubernetes v1.36.1\n"},
		{name: "comment line", message: "release\n\n# this line survives\nmore text\n"},
		{name: "trailing whitespace", message: "release   \n\nbody\t\n"},
		{name: "trailing blank lines", message: "release\n\n\n"},
		{name: "no trailing newline", message: "release without a terminator"},
		{name: "unicode", message: "release café ünïcode 日本語\n"},
		{name: "dashes that look like a patch", message: "release\n---\nnot a diff\n"},
	}
	for _, test := range messages {
		t.Run(test.name, func(t *testing.T) {
			ctx := t.Context()
			git, _ := newRepo(ctx, t, gitcli.ObjectFormatSHA1)
			commit := writeCommit(ctx, t, git)

			opts := treebuild.TagOptions{
				Commit:  commit,
				Name:    "v0.36.1",
				Tagger:  gitcli.Signature{Name: "Soapbox Bot", Email: "bot@example.com", Date: "1700000000 +0530"},
				Message: test.message,
			}
			object, err := treebuild.WriteTag(ctx, git, opts)
			if err != nil {
				t.Fatalf("write tag: %v", err)
			}
			again, err := treebuild.WriteTag(ctx, git, opts)
			if err != nil {
				t.Fatalf("rewrite tag: %v", err)
			}
			if again != object {
				t.Fatalf("the same tag was written twice as %s and %s", object, again)
			}

			// Writing the object must not have claimed the name. Until a
			// publisher creates the ref, the release exists only as an object
			// nothing points at.
			if _, err := git.TagInfo(ctx, opts.Name); !errors.Is(err, gitcli.ErrTagNotFound) {
				t.Fatalf("tag lookup returned %v, want the tag not to exist yet", err)
			}

			// Naming the release is the separate, later step. Doing it through
			// git's own tag writer, with the same message and tagger, is also
			// the strongest available check on the shaping: if the object git
			// writes has the same name as the object this package assembled,
			// then the two are byte for byte the same tag, and the engine is not
			// publishing something only it knows how to produce.
			if err := git.CreateTag(ctx, gitcli.TagOptions{
				Name:    opts.Name,
				Commit:  commit,
				Message: opts.Message,
				Tagger:  opts.Tagger,
			}); err != nil {
				t.Fatalf("create tag: %v", err)
			}
			info, err := git.TagInfo(ctx, opts.Name)
			if err != nil {
				t.Fatalf("read tag: %v", err)
			}
			if info.Object != object {
				t.Fatalf("git wrote the tag object as %s, want the assembled %s", info.Object, object)
			}
			if !info.Annotated {
				t.Fatal("the tag is lightweight, and a release tag records a tagger")
			}
			if info.Target != commit {
				t.Fatalf("the tag names %s, want the commit %s", info.Target, commit)
			}
			if info.Tagger.Date != "1700000000 +0530" {
				t.Fatalf("tagger date is %q, want the raw upstream date", info.Tagger.Date)
			}
			if info.Tagger.Name != "Soapbox Bot" || info.Tagger.Email != "bot@example.com" {
				t.Fatalf("tagger is %q <%q>, want the engine bot", info.Tagger.Name, info.Tagger.Email)
			}
			// The message is stored exactly as given, down to a missing final
			// newline. Nothing is cleaned up, so a release note keeps its
			// comment lines and its trailing whitespace.
			if info.Message != test.message {
				t.Fatalf("tag message round tripped as %q, want %q", info.Message, test.message)
			}
		})
	}
}

// TestWriteTagRejects covers the inputs that must fail rather than produce a tag
// that cannot be regenerated.
func TestWriteTagRejects(t *testing.T) {
	ctx := t.Context()
	git, _ := newRepo(ctx, t, gitcli.ObjectFormatSHA1)
	commit := writeCommit(ctx, t, git)
	bot := gitcli.Signature{Name: "Soapbox Bot", Email: "bot@example.com", Date: "1700000000 +0000"}

	tests := []struct {
		name string
		opts treebuild.TagOptions
		want error
	}{
		{
			name: "tagger date is not raw",
			opts: treebuild.TagOptions{
				Commit:  commit,
				Name:    "v0.36.1",
				Tagger:  gitcli.Signature{Name: "B", Email: "b@example.com", Date: "2023-11-14T22:13:20Z"},
				Message: "release\n",
			},
			want: treebuild.ErrRawDate,
		},
		{
			name: "commit is a revision rather than an object name",
			opts: treebuild.TagOptions{Commit: "HEAD", Name: "v0.36.1", Tagger: bot, Message: "release\n"},
		},
		{
			name: "tag name is not a valid ref component",
			opts: treebuild.TagOptions{Commit: commit, Name: "v0.36..1", Tagger: bot, Message: "release\n"},
		},
		{
			name: "message is empty",
			opts: treebuild.TagOptions{Commit: commit, Name: "v0.36.1", Tagger: bot},
		},
		{
			name: "tagger has no email",
			opts: treebuild.TagOptions{
				Commit: commit, Name: "v0.36.1",
				Tagger:  gitcli.Signature{Name: "B", Date: "1700000000 +0000"},
				Message: "release\n",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			object, err := treebuild.WriteTag(ctx, git, test.opts)
			if err == nil {
				t.Fatalf("the tag was accepted as %s", object)
			}
			if test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("error %v does not wrap %v", err, test.want)
			}
		})
	}
}
