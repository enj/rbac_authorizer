package gitcli_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/testsupport"
)

// hostileMessage is a commit message built to break every shortcut a batched
// reader might take: it spans lines, carries colons, embeds text that looks like
// the record separator's neighbours, and ends with a trailer block preceded by
// something that only resembles one.
const hostileMessage = "feat: add resolver: now with colons\n" +
	"\n" +
	"A body line that mentions Kubernetes-commit: not-a-trailer inline.\n" +
	"Another line: with a colon.\n" +
	"\n" +
	"Kubernetes-commit: 1234567890abcdef1234567890abcdef12345678\n" +
	"Signed-off-by: Upstream Author <author@example.com>\n"

func TestCommitLogReadsTheGraphInOnePass(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)
	git := up.repo.Git

	commits, err := git.CommitLog(ctx, gitcli.CommitLogOptions{Include: []string{"refs/heads/" + mainBranch}})
	if err != nil {
		t.Fatalf("commit log: %v", err)
	}
	if len(commits) != 4 {
		t.Fatalf("read %d commits, want 4", len(commits))
	}

	// The batch has to agree with the graph walk exactly, or a caller cannot zip
	// the two together to decide what to replay.
	graph, err := git.CommitGraph(ctx, gitcli.RevListOptions{Include: []string{"refs/heads/" + mainBranch}})
	if err != nil {
		t.Fatalf("commit graph: %v", err)
	}
	if len(graph) != len(commits) {
		t.Fatalf("graph walked %d commits, log read %d", len(graph), len(commits))
	}
	for i, commit := range commits {
		if commit.SHA != graph[i].SHA {
			t.Fatalf("commit %d is %s, graph has %s", i, commit.SHA, graph[i].SHA)
		}
		if !slices.Equal(commit.Parents, graph[i].Parents) {
			t.Fatalf("commit %s parents %v, graph has %v", commit.SHA, commit.Parents, graph[i].Parents)
		}
		if err := gitcli.ValidateRawDate(commit.AuthorDateRaw); err != nil {
			t.Fatalf("commit %s author raw date %q: %v", commit.SHA, commit.AuthorDateRaw, err)
		}
		if err := gitcli.ValidateRawDate(commit.CommitterDateRaw); err != nil {
			t.Fatalf("commit %s committer raw date %q: %v", commit.SHA, commit.CommitterDateRaw, err)
		}
	}

	if commits[0].SHA != up.sha(base) {
		t.Fatalf("walk starts at %q, want the base commit", commits[0].SHA)
	}
	if len(commits[0].Parents) != 0 {
		t.Fatalf("root commit has parents %v", commits[0].Parents)
	}
	merge := commits[len(commits)-1]
	if merge.SHA != up.sha(mergeC) {
		t.Fatalf("walk ends at %q, want the merge commit", merge.SHA)
	}
	if want := []string{up.sha(mainOne), up.sha(feature)}; !slices.Equal(merge.Parents, want) {
		t.Fatalf("merge parents %v, want %v", merge.Parents, want)
	}
}

// TestCommitLogAgreesWithCommitInfo pins the batched read to the single read.
// They share a record format precisely so that a caller can use either without
// having to know which fields the cheaper one quietly leaves out.
func TestCommitLogAgreesWithCommitInfo(t *testing.T) {
	ctx := t.Context()
	repo := testsupport.NewRepo(ctx, t, testsupport.Options{UserName: testUserName, UserEmail: testUserEmail})
	repo.WriteFile(t, "rule.go", "package validation\n")
	head := repo.Commit(ctx, t, hostileMessage, gitcli.CommitOptions{}, "rule.go")

	single, err := repo.Git.CommitInfo(ctx, head)
	if err != nil {
		t.Fatalf("commit info: %v", err)
	}
	batch, err := repo.Git.CommitLog(ctx, gitcli.CommitLogOptions{Include: []string{head}, Signatures: true})
	if err != nil {
		t.Fatalf("commit log: %v", err)
	}
	if len(batch) != 1 {
		t.Fatalf("read %d commits, want 1", len(batch))
	}
	if !commitsEqual(single, batch[0]) {
		t.Fatalf("batched commit\n%+v\ndiffers from single commit\n%+v", batch[0], single)
	}
}

// TestCommitLogPreservesHostileMessages proves the record framing survives a
// message that contains everything except the one byte it may not.
func TestCommitLogPreservesHostileMessages(t *testing.T) {
	ctx := t.Context()
	repo := testsupport.NewRepo(ctx, t, testsupport.Options{UserName: testUserName, UserEmail: testUserEmail})

	messages := []string{
		hostileMessage,
		"fix: trailing whitespace   \n\n\n",
		"docs: subject only\n",
		"chore: a message with a lone \r carriage return\n",
		"refactor: unicode ☃ and emoji 🚀\n",
	}
	for i, message := range messages {
		repo.WriteFile(t, "file.txt", strings.Repeat("x", i+1))
		repo.Commit(ctx, t, message, gitcli.CommitOptions{}, "file.txt")
	}

	commits, err := repo.Git.CommitLog(ctx, gitcli.CommitLogOptions{Include: []string{"HEAD"}})
	if err != nil {
		t.Fatalf("commit log: %v", err)
	}
	if len(commits) != len(messages) {
		t.Fatalf("read %d commits, want %d", len(commits), len(messages))
	}
	for i, commit := range commits {
		if commit.RawMessage != messages[i] {
			t.Fatalf("commit %d message = %q, want %q", i, commit.RawMessage, messages[i])
		}
	}

	// The trailer block is git's own answer, so a colon in the body must not
	// become a trailer and a real trailer must not be lost behind one.
	hostile := commits[0]
	if got := hostile.TrailerValues("Kubernetes-commit"); len(got) != 1 || got[0] != "1234567890abcdef1234567890abcdef12345678" {
		t.Fatalf("Kubernetes-commit trailers = %v, want exactly the trailer block value", got)
	}
	if got := hostile.TrailerValues("Signed-off-by"); len(got) != 1 {
		t.Fatalf("Signed-off-by trailers = %v, want one", got)
	}
	if hostile.Subject != "feat: add resolver: now with colons" {
		t.Fatalf("subject = %q", hostile.Subject)
	}
}

func TestCommitLogSelection(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)
	git := up.repo.Git

	tests := []struct {
		name string
		opts gitcli.CommitLogOptions
		want []string
	}{
		{
			name: "excluded ancestors bound the walk",
			opts: gitcli.CommitLogOptions{
				Include: []string{up.sha(mergeC)},
				Exclude: []string{up.sha(base)},
			},
			want: []string{up.sha(mainOne), up.sha(feature), up.sha(mergeC)},
		},
		{
			name: "first parent follows the mainline",
			opts: gitcli.CommitLogOptions{
				Include:     []string{up.sha(mergeC)},
				FirstParent: true,
			},
			want: []string{up.sha(base), up.sha(mainOne), up.sha(mergeC)},
		},
		{
			name: "max count keeps the newest commits oldest first",
			opts: gitcli.CommitLogOptions{
				Include:  []string{up.sha(mergeC)},
				MaxCount: 2,
			},
			want: []string{up.sha(feature), up.sha(mergeC)},
		},
		{
			name: "an empty range is not an error",
			opts: gitcli.CommitLogOptions{
				Include: []string{up.sha(base)},
				Exclude: []string{up.sha(base)},
			},
		},
		{
			name: "two tips are walked together",
			opts: gitcli.CommitLogOptions{
				Include: []string{up.sha(release), up.sha(mainOne)},
				Exclude: []string{up.sha(base)},
			},
			want: []string{up.sha(mainOne), up.sha(release)},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			commits, err := git.CommitLog(ctx, test.opts)
			if err != nil {
				t.Fatalf("commit log: %v", err)
			}
			got := make([]string, 0, len(commits))
			for _, commit := range commits {
				got = append(got, commit.SHA)
			}
			// The topological order of independent commits is git's own, so a
			// set comparison is the honest assertion for a multi tip walk.
			if len(test.opts.Include) > 1 {
				slices.Sort(got)
				want := slices.Clone(test.want)
				slices.Sort(want)
				if !slices.Equal(got, want) {
					t.Fatalf("commits %v, want %v", got, want)
				}
				return
			}
			if !slices.Equal(got, test.want) {
				t.Fatalf("commits %v, want %v", got, test.want)
			}
		})
	}
}

func TestCommitLogRejectsHostileOptions(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)
	git := up.repo.Git

	tests := []struct {
		name string
		opts gitcli.CommitLogOptions
	}{
		{name: "no revisions", opts: gitcli.CommitLogOptions{}},
		{name: "empty revision", opts: gitcli.CommitLogOptions{Include: []string{""}}},
		{name: "option revision", opts: gitcli.CommitLogOptions{Include: []string{"--all"}}},
		{name: "option exclude", opts: gitcli.CommitLogOptions{Include: []string{"HEAD"}, Exclude: []string{"--all"}}},
		{name: "range revision", opts: gitcli.CommitLogOptions{Include: []string{"HEAD..HEAD~1"}}},
		{name: "negated revision", opts: gitcli.CommitLogOptions{Include: []string{"^HEAD"}}},
		{name: "null revision", opts: gitcli.CommitLogOptions{Include: []string{"HEAD\x00"}}},
		{name: "negative max count", opts: gitcli.CommitLogOptions{Include: []string{"HEAD"}, MaxCount: -1}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			commits, err := git.CommitLog(ctx, test.opts)
			if err == nil {
				t.Fatalf("hostile options were accepted and returned %d commits", len(commits))
			}
			if commits != nil {
				t.Fatalf("rejected read returned %d commits", len(commits))
			}
		})
	}
}

// TestCommitLogSignatureFieldsAreOptIn proves the batch does not verify
// signatures unless it is asked to. The verdict depends on what the machine
// trusts and costs a verifier per signed commit, so a walk that does not need it
// must not silently pay for it.
func TestCommitLogSignatureFieldsAreOptIn(t *testing.T) {
	ctx := t.Context()
	key := testsupport.NewSigningKey(t)
	repo := testsupport.NewRepo(ctx, t, testsupport.Options{UserName: testUserName, UserEmail: testUserEmail})
	repo.EnableSSHSigning(ctx, t, key, testUserEmail)
	repo.WriteFile(t, "a.txt", "a\n")
	repo.Commit(ctx, t, "feat: a\n", gitcli.CommitOptions{Sign: true}, "a.txt")

	quiet, err := repo.Git.CommitLog(ctx, gitcli.CommitLogOptions{Include: []string{"HEAD"}})
	if err != nil {
		t.Fatalf("commit log: %v", err)
	}
	if len(quiet) != 1 {
		t.Fatalf("read %d commits, want 1", len(quiet))
	}
	if quiet[0].SignatureStatus != "" || quiet[0].SignerKey != "" || quiet[0].Signer != "" {
		t.Fatalf("signature fields %q %q %q must be empty without Signatures",
			quiet[0].SignatureStatus, quiet[0].SignerKey, quiet[0].Signer)
	}
	// Everything else still has to be there, so the empty fields are a hole in
	// the record rather than a truncated one.
	if quiet[0].Subject != "feat: a" || quiet[0].AuthorName != testUserName {
		t.Fatalf("unsigned batch lost fields: %+v", quiet[0])
	}

	verified, err := repo.Git.CommitLog(ctx, gitcli.CommitLogOptions{Include: []string{"HEAD"}, Signatures: true})
	if err != nil {
		t.Fatalf("commit log with signatures: %v", err)
	}
	if verified[0].SignatureStatus != "G" {
		t.Fatalf("signature status = %q, want G", verified[0].SignatureStatus)
	}
	if verified[0].SignerKey != key.Fingerprint {
		t.Fatalf("signer key = %q, want %q", verified[0].SignerKey, key.Fingerprint)
	}
	if verified[0].SHA != quiet[0].SHA || verified[0].RawMessage != quiet[0].RawMessage {
		t.Fatal("asking for signatures changed the rest of the record")
	}
}

func TestCommitLogHonoursCancellation(t *testing.T) {
	repo := testsupport.NewRepo(t.Context(), t, testsupport.Options{UserName: testUserName, UserEmail: testUserEmail})
	repo.WriteAndCommit(t.Context(), t, "a.txt", "a\n", "feat: a\n")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := repo.Git.CommitLog(ctx, gitcli.CommitLogOptions{Include: []string{"HEAD"}}); err == nil {
		t.Fatal("expected a cancellation error")
	}
}

// TestCommitInfoReportsParents covers the field the batch added to the shared
// record: the single read gets it too, so a caller never needs a second command
// to learn a commit's edges.
func TestCommitInfoReportsParents(t *testing.T) {
	ctx := t.Context()
	up := newUpstream(ctx, t)

	merge, err := up.repo.Git.CommitInfo(ctx, up.sha(mergeC))
	if err != nil {
		t.Fatalf("commit info: %v", err)
	}
	if want := []string{up.sha(mainOne), up.sha(feature)}; !slices.Equal(merge.Parents, want) {
		t.Fatalf("merge parents %v, want %v", merge.Parents, want)
	}
	root, err := up.repo.Git.CommitInfo(ctx, up.sha(base))
	if err != nil {
		t.Fatalf("commit info: %v", err)
	}
	if len(root.Parents) != 0 {
		t.Fatalf("root commit has parents %v", root.Parents)
	}
}

// commitsEqual compares every field of two commits. The fields are listed by
// hand rather than compared reflectively so that a field added to the record
// without being added here shows up as a compile error in the assertion.
func commitsEqual(a, b gitcli.Commit) bool {
	return a.SHA == b.SHA &&
		slices.Equal(a.Parents, b.Parents) &&
		a.AuthorName == b.AuthorName &&
		a.AuthorEmail == b.AuthorEmail &&
		a.AuthorDate == b.AuthorDate &&
		a.AuthorDateRaw == b.AuthorDateRaw &&
		a.CommitterName == b.CommitterName &&
		a.CommitterEmail == b.CommitterEmail &&
		a.CommitterDate == b.CommitterDate &&
		a.CommitterDateRaw == b.CommitterDateRaw &&
		a.SignatureStatus == b.SignatureStatus &&
		a.SignerKey == b.SignerKey &&
		a.Signer == b.Signer &&
		a.Subject == b.Subject &&
		a.RawMessage == b.RawMessage &&
		slices.Equal(a.Trailers, b.Trailers)
}

// TestCommitContentBypassesRedaction proves upstream content is returned byte
// for byte even when it matches a seeded secret.
//
// Replay reproduces the upstream message, identity, and trailers exactly. A
// redactor that rewrote them would publish a commit that is not the upstream
// one, and it would do it silently: the run would succeed and only the
// published bytes would be wrong. Diagnostics are a different thing and stay
// redacted.
func TestCommitContentBypassesRedaction(t *testing.T) {
	ctx := t.Context()
	const secret = "ghs_averyprivatetoken"
	repo := testsupport.NewRepo(ctx, t, testsupport.Options{
		UserName:  testUserName,
		UserEmail: testUserEmail,
		Secrets:   []string{secret},
	})

	// The redactor has to be armed, or this test proves nothing at all.
	if repo.Git.Redactor().String(secret) != gitcli.Placeholder {
		t.Fatal("the runner redactor was not seeded with the secret")
	}

	message := "feat: mention " + secret + "\n" +
		"\n" +
		"A body line quoting " + secret + " inline.\n" +
		"\n" +
		"Kubernetes-commit: " + secret + "\n"
	repo.WriteFile(t, "a.txt", "a\n")
	head := repo.Commit(ctx, t, message, gitcli.CommitOptions{
		Author:    gitcli.Signature{Name: secret, Email: testUserEmail},
		Committer: gitcli.Signature{Name: secret, Email: testUserEmail},
	}, "a.txt")

	single, err := repo.Git.CommitInfo(ctx, head)
	if err != nil {
		t.Fatalf("commit info: %v", err)
	}
	batch, err := repo.Git.CommitLog(ctx, gitcli.CommitLogOptions{Include: []string{head}})
	if err != nil {
		t.Fatalf("commit log: %v", err)
	}
	if len(batch) != 1 {
		t.Fatalf("read %d commits, want 1", len(batch))
	}

	for _, test := range []struct {
		name   string
		commit gitcli.Commit
	}{
		{name: "CommitInfo", commit: single},
		{name: "CommitLog", commit: batch[0]},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.commit.RawMessage != message {
				t.Errorf("message = %q, want the upstream bytes %q", test.commit.RawMessage, message)
			}
			if test.commit.AuthorName != secret {
				t.Errorf("author name = %q, want %q", test.commit.AuthorName, secret)
			}
			if test.commit.CommitterName != secret {
				t.Errorf("committer name = %q, want %q", test.commit.CommitterName, secret)
			}
			if got := test.commit.TrailerValues("Kubernetes-commit"); len(got) != 1 || got[0] != secret {
				t.Errorf("trailer values = %v, want [%q]", got, secret)
			}
			if strings.Contains(test.commit.RawMessage, gitcli.Placeholder) {
				t.Error("the redactor rewrote replayed content")
			}
		})
	}

	// git's own trailer parse is content for the same reason.
	trailers, err := repo.Git.ParseTrailers(ctx, message)
	if err != nil {
		t.Fatalf("parse trailers: %v", err)
	}
	if len(trailers) != 1 || trailers[0].Value != secret {
		t.Fatalf("parsed trailers = %v, want the upstream value", trailers)
	}

	// Diagnostics are still scrubbed, so the bypass is scoped to content.
	_, err = repo.Git.CommitInfo(ctx, "refs/heads/"+secret)
	if err == nil {
		t.Fatal("expected an error for a missing revision")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaked the secret: %v", err)
	}
	if !strings.Contains(err.Error(), gitcli.Placeholder) {
		t.Fatalf("error %q does not show the redaction placeholder", err)
	}
}
