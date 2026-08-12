package treebuild_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/gitgraph"
	"github.com/enj/soapbox/tools/internal/treebuild"
)

// The provenance key and a source commit name used throughout these tests.
const (
	provenanceKey = "Kubernetes-commit"
	sourceSHA     = "1234567890abcdef1234567890abcdef12345678"
	otherSHA      = "fedcba0987654321fedcba0987654321fedcba09"
)

// provenanceMessages are the message shapes an upstream history actually
// produces, each paired with what the appended trailer has to end up meaning.
//
// The shapes are the point. A trailer is only a trailer in the last paragraph of
// a message, and "the last paragraph" is decided by rules that a subject, a
// prose body, an existing trailer block, a comment, a blank line, and a patch
// each change. A message whose trailer landed outside the block would publish a
// commit that claims no source, and the mapping a resumed run rebuilds from the
// published history would silently lose it.
var provenanceMessages = []struct {
	name string
	// message is the upstream commit message.
	message string
	// before is how many trailers the message already has.
	before int
	// wantErr reports a message the trailer cannot be appended to safely.
	wantErr bool
}{
	{name: "subject only", message: "feat(rbac): add authorizer\n"},
	{name: "subject without a trailing newline", message: "feat(rbac): add authorizer"},
	{name: "subject and prose body", message: "feat(rbac): add authorizer\n\nThis explains why.\n"},
	{
		name:    "existing trailer block",
		message: "feat(rbac): add authorizer\n\nSigned-off-by: A <a@example.com>\n",
		before:  1,
	},
	{
		name:    "existing trailer under the same key",
		message: "feat(rbac): add authorizer\n\n" + provenanceKey + ": " + otherSHA + "\n",
		before:  1,
	},
	{
		name:    "several existing trailers",
		message: "feat: x\n\nReviewed-by: R <r@example.com>\nSigned-off-by: A <a@example.com>\n",
		before:  2,
	},
	{
		name:    "prose after the trailers disqualifies the block",
		message: "feat: x\n\nSigned-off-by: A <a@example.com>\nand some closing prose\n",
	},
	{
		name:    "last paragraph only looks like a trailer",
		message: "feat: x\n\nSee also: the design document\n",
	},
	{name: "single paragraph shaped like a trailer", message: "Fix: something broke\n"},
	{name: "trailing blank lines", message: "feat: x\n\nbody\n\n\n\n"},
	{
		name:    "comment inside the trailer block",
		message: "feat: x\n\n# a note git ignores\nSigned-off-by: A <a@example.com>\n",
		before:  1,
	},
	{name: "carriage returns", message: "feat: x\r\n\r\nbody text\r\n"},
	{
		name:    "message with a patch appended",
		message: "feat: x\n\nbody\n---\n diff --git a/x b/x\n",
	},
	{
		name:    "trailers before a patch",
		message: "feat: x\n\nSigned-off-by: A <a@example.com>\n---\n diff --git a/x b/x\n",
		before:  1,
	},
	// A patch marker with CRLF endings is a patch marker. Git's own rule is
	// "---" followed by any whitespace, and a carriage return is whitespace, so
	// refusing this message would refuse a shape git reads perfectly well and
	// upstream really does produce. TestProvenanceMessageMatchesGit is what
	// settles it: the result is handed to git interpret-trailers, so if the
	// trailer landed inside the diff git would report it missing.
	{name: "carriage returns before a patch", message: "feat: x\r\n\r\nbody\r\n---\r\n diff\r\n"},
	{name: "empty message", message: "", wantErr: true},
	{name: "message of only whitespace", message: "\n\n  \n", wantErr: true},
}

// TestProvenanceMessage checks that the appended trailer is the effective one
// and that nothing the message already said was lost.
func TestProvenanceMessage(t *testing.T) {
	for _, test := range provenanceMessages {
		t.Run(test.name, func(t *testing.T) {
			got, err := treebuild.ProvenanceMessage(test.message, provenanceKey, sourceSHA)
			if test.wantErr {
				if err == nil {
					t.Fatalf("the message was accepted and produced %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("append trailer: %v", err)
			}

			trailers := gitgraph.TrailerBlock(got)
			if len(trailers) != test.before+1 {
				t.Fatalf("result holds %d trailers, want %d:\n%q", len(trailers), test.before+1, got)
			}
			// The engine's claim is last, so it is the value a reader resolves.
			if last := trailers[len(trailers)-1]; last.Key != provenanceKey || last.Value != sourceSHA {
				t.Fatalf("last trailer is %q: %q, want %q: %q", last.Key, last.Value, provenanceKey, sourceSHA)
			}
			value, found := gitgraph.TrailerValue(got, provenanceKey)
			if !found || value != sourceSHA {
				t.Fatalf("effective %s is %q (found %t), want %q", provenanceKey, value, found, sourceSHA)
			}
			// Every trailer the upstream message carried survives, including one
			// under the same key: the engine appends its claim rather than
			// rewriting what upstream said.
			for i, before := range gitgraph.TrailerBlock(test.message) {
				if trailers[i] != before {
					t.Fatalf("trailer %d became %q: %q, want %q: %q", i, trailers[i].Key, trailers[i].Value, before.Key, before.Value)
				}
			}
			// The upstream text is still there. Comparing the first line is
			// enough to catch a rewrite of the message body, and the trailer
			// checks above cover the tail.
			if subject, _, _ := strings.Cut(strings.TrimLeft(test.message, "\n"), "\n"); subject != "" {
				if !strings.Contains(got, strings.TrimRight(subject, "\r")) {
					t.Fatalf("the subject %q is missing from %q", subject, got)
				}
			}
			// A patch is left alone, and the trailer is placed before it. Both
			// line endings are checked, because recognising only one of them is
			// how the trailer ends up inside the diff.
			for _, marker := range []string{"\n---\n", "\n---\r\n"} {
				if index := strings.Index(test.message, marker); index >= 0 {
					if patch := test.message[index+1:]; !strings.HasSuffix(got, patch) {
						t.Fatalf("the patch part was modified, result is:\n%q", got)
					}
				}
			}
		})
	}
}

// TestProvenanceMessageMatchesGit checks the appended trailer against git's own
// trailer parser rather than only against the engine's.
//
// This is the assertion that cannot be argued with. The engine decides where to
// write a trailer and gitgraph decides what counts as one, and both are ours; if
// they agreed with each other and disagreed with git, every published commit
// would carry provenance that git itself does not see. So the result is handed
// to git interpret-trailers and git is asked what it reads.
func TestProvenanceMessageMatchesGit(t *testing.T) {
	ctx := t.Context()
	git, _ := newRepo(ctx, t, gitcli.ObjectFormatSHA1)

	for _, test := range provenanceMessages {
		if test.wantErr {
			continue
		}
		t.Run(test.name, func(t *testing.T) {
			got, err := treebuild.ProvenanceMessage(test.message, provenanceKey, sourceSHA)
			if err != nil {
				t.Fatalf("append trailer: %v", err)
			}
			trailers, err := git.ParseTrailers(ctx, got)
			if err != nil {
				t.Fatalf("git interpret-trailers: %v", err)
			}
			if len(trailers) == 0 {
				t.Fatalf("git reads no trailers in:\n%q", got)
			}
			last := trailers[len(trailers)-1]
			if last.Key != provenanceKey || last.Value != sourceSHA {
				t.Fatalf("git reads the last trailer as %q: %q, want %q: %q\nin:\n%q",
					last.Key, last.Value, provenanceKey, sourceSHA, got)
			}
			if len(trailers) != test.before+1 {
				t.Fatalf("git reads %d trailers, want %d:\n%q", len(trailers), test.before+1, got)
			}
		})
	}
}

// TestProvenanceMessageRejects covers keys and values that would not survive
// being read back.
func TestProvenanceMessageRejects(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
	}{
		{name: "empty key", key: "", value: sourceSHA},
		{name: "key with a space", key: "Kubernetes commit", value: sourceSHA},
		{name: "key with an underscore", key: "Kubernetes_commit", value: sourceSHA},
		{name: "key with a colon", key: "Kubernetes:commit", value: sourceSHA},
		{name: "empty value", key: provenanceKey, value: ""},
		{name: "value with a line break", key: provenanceKey, value: "abc\ndef"},
		{name: "value with a null byte", key: provenanceKey, value: "abc\x00def"},
		{name: "value with surrounding whitespace", key: provenanceKey, value: " " + sourceSHA + " "},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := treebuild.ProvenanceMessage("feat: x\n", test.key, test.value)
			if err == nil {
				t.Fatalf("the trailer was accepted and produced %q", got)
			}
		})
	}
}

// TestWriteCommitShape checks that a replayed commit records the upstream author
// and the engine's committer, carries the provenance trailer, and is unsigned.
func TestWriteCommitShape(t *testing.T) {
	ctx := t.Context()
	git, _ := newRepo(ctx, t, gitcli.ObjectFormatSHA1)

	tree := writeTree(ctx, t, git)
	const (
		authorDate    = "1700000000 +0530"
		committerDate = "1600000000 -0800"
	)
	opts := treebuild.CommitOptions{
		Tree:          tree,
		Author:        gitcli.Signature{Name: "Upstream Author", Email: "author@k8s.example", Date: authorDate},
		Committer:     gitcli.Signature{Name: "Soapbox Bot", Email: "bot@example.com", Date: committerDate},
		Message:       "feat(rbac): add authorizer\n\nBody text.\n",
		ProvenanceKey: provenanceKey,
		Source:        sourceSHA,
	}
	sha, err := treebuild.WriteCommit(ctx, git, opts)
	if err != nil {
		t.Fatalf("write commit: %v", err)
	}

	commit, err := git.CommitInfo(ctx, sha)
	if err != nil {
		t.Fatalf("read commit: %v", err)
	}
	if commit.AuthorName != "Upstream Author" || commit.AuthorEmail != "author@k8s.example" {
		t.Fatalf("author is %q, want the upstream identity", commit.AuthorIdentity())
	}
	if commit.CommitterName != "Soapbox Bot" || commit.CommitterEmail != "bot@example.com" {
		t.Fatalf("committer is %q, want the engine bot", commit.CommitterIdentity())
	}
	// The zone offset is carried, not normalised away. A date rendered in the
	// reader's zone would produce a different commit object on another machine.
	if !strings.HasSuffix(commit.AuthorDate, "+05:30") {
		t.Fatalf("author date %q lost the +0530 offset", commit.AuthorDate)
	}
	if !strings.HasSuffix(commit.CommitterDate, "-08:00") {
		t.Fatalf("committer date %q lost the -0800 offset", commit.CommitterDate)
	}
	if got := commit.TrailerValues(provenanceKey); len(got) != 1 || got[0] != sourceSHA {
		t.Fatalf("%s trailers are %v, want exactly [%s]", provenanceKey, got, sourceSHA)
	}
	if !strings.HasPrefix(commit.RawMessage, "feat(rbac): add authorizer\n\nBody text.\n") {
		t.Fatalf("the upstream message was rewritten: %q", commit.RawMessage)
	}
	// "N" is git's verdict for a commit carrying no signature.
	if commit.SignatureStatus != "N" {
		t.Fatalf("signature status is %q, want N: a generated commit has no signer", commit.SignatureStatus)
	}

	// The same inputs produce the same object, which is what makes a replay
	// resumable: rebuilding a commit that already exists is a no-op rather than
	// a second commit with the same content.
	again, err := treebuild.WriteCommit(ctx, git, opts)
	if err != nil {
		t.Fatalf("rewrite commit: %v", err)
	}
	if again != sha {
		t.Fatalf("the same commit was written twice as %s and %s", sha, again)
	}
}

// TestWriteSyntheticCommitCarriesNoProvenance checks that an engine authored
// commit claims no source.
//
// The absence is load bearing. A resumed run rebuilds the source to destination
// mapping by reading provenance trailers out of the published history, so a
// generated commit carrying one would map a source commit onto a commit that
// source never produced.
func TestWriteSyntheticCommitCarriesNoProvenance(t *testing.T) {
	ctx := t.Context()
	git, _ := newRepo(ctx, t, gitcli.ObjectFormatSHA1)

	sha, err := treebuild.WriteSyntheticCommit(ctx, git, treebuild.SyntheticCommitOptions{
		Tree:      writeTree(ctx, t, git),
		Message:   "chore: update staging dependencies\n",
		Author:    gitcli.Signature{Name: "Soapbox Bot", Email: "bot@example.com", Date: "1700000000 +0000"},
		Committer: gitcli.Signature{Name: "Soapbox Bot", Email: "bot@example.com", Date: "1700000000 +0000"},
	})
	if err != nil {
		t.Fatalf("write synthetic commit: %v", err)
	}
	commit, err := git.CommitInfo(ctx, sha)
	if err != nil {
		t.Fatalf("read commit: %v", err)
	}
	if got := commit.TrailerValues(provenanceKey); len(got) != 0 {
		t.Fatalf("a synthetic commit claims the sources %v", got)
	}
	mapping, err := gitgraph.MappingFromTrailers([]gitgraph.Commit{{SHA: sha, Message: commit.RawMessage}}, provenanceKey)
	if err != nil {
		t.Fatalf("rebuild mapping: %v", err)
	}
	if mapping.Len() != 0 {
		t.Fatalf("a synthetic commit contributed %d mappings", mapping.Len())
	}
}

// TestWriteCommitRejects covers the inputs that must fail rather than produce a
// commit whose bytes depend on the machine that wrote it.
func TestWriteCommitRejects(t *testing.T) {
	ctx := t.Context()
	git, _ := newRepo(ctx, t, gitcli.ObjectFormatSHA1)
	tree := writeTree(ctx, t, git)

	bot := gitcli.Signature{Name: "Soapbox Bot", Email: "bot@example.com", Date: "1700000000 +0000"}
	tests := []struct {
		name string
		opts treebuild.CommitOptions
		want error
	}{
		{
			name: "author date is not raw",
			opts: treebuild.CommitOptions{
				Tree:          tree,
				Author:        gitcli.Signature{Name: "A", Email: "a@example.com", Date: "2023-11-14T22:13:20Z"},
				Committer:     bot,
				Message:       "feat: x\n",
				ProvenanceKey: provenanceKey,
				Source:        sourceSHA,
			},
			want: treebuild.ErrRawDate,
		},
		{
			name: "committer date is not raw",
			opts: treebuild.CommitOptions{
				Tree:          tree,
				Author:        bot,
				Committer:     gitcli.Signature{Name: "B", Email: "b@example.com", Date: "yesterday"},
				Message:       "feat: x\n",
				ProvenanceKey: provenanceKey,
				Source:        sourceSHA,
			},
			want: treebuild.ErrRawDate,
		},
		{
			name: "zone offset is missing",
			opts: treebuild.CommitOptions{
				Tree:          tree,
				Author:        gitcli.Signature{Name: "A", Email: "a@example.com", Date: "1700000000"},
				Committer:     bot,
				Message:       "feat: x\n",
				ProvenanceKey: provenanceKey,
				Source:        sourceSHA,
			},
			want: treebuild.ErrRawDate,
		},
		{
			name: "source is not an object name",
			opts: treebuild.CommitOptions{
				Tree: tree, Author: bot, Committer: bot,
				Message: "feat: x\n", ProvenanceKey: provenanceKey, Source: "HEAD~1",
			},
		},
		{
			name: "tree is not an object name",
			opts: treebuild.CommitOptions{
				Tree: "HEAD^{tree}", Author: bot, Committer: bot,
				Message: "feat: x\n", ProvenanceKey: provenanceKey, Source: sourceSHA,
			},
		},
		{
			name: "parent is not an object name",
			opts: treebuild.CommitOptions{
				Tree: tree, Parents: []string{"main"}, Author: bot, Committer: bot,
				Message: "feat: x\n", ProvenanceKey: provenanceKey, Source: sourceSHA,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sha, err := treebuild.WriteCommit(ctx, git, test.opts)
			if err == nil {
				t.Fatalf("the commit was accepted as %s", sha)
			}
			if test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("error %v does not wrap %v", err, test.want)
			}
		})
	}
}
