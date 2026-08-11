package gitcli_test

import (
	"testing"

	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/gitgraph"
)

// TestTrailerBlockMatchesGit checks the pure trailer parser in gitgraph against
// git interpret-trailers --parse.
//
// The parser decides what a published commit claims as its provenance, so
// "close enough to git" is not a property that can be argued: it has to be
// measured against git itself. The cases below are the shapes that separate a
// trailer block from text that merely looks like one, and each is run through
// both implementations and compared.
func TestTrailerBlockMatchesGit(t *testing.T) {
	ctx := t.Context()
	git := newRunner(t, t.TempDir())

	messages := []struct {
		name    string
		message string
	}{
		{name: "subject only", message: "subject\n"},
		{name: "subject and trailer", message: "subject\n\nKubernetes-commit: abc\n"},
		{name: "subject shaped like a trailer", message: "Fix: something broke\n"},
		{name: "single paragraph of trailers", message: "Kubernetes-commit: abc\nSigned-off-by: A <a@b>\n"},
		{name: "no blank separator", message: "subject\nKubernetes-commit: abc\n"},
		{name: "body then trailers", message: "subject\n\nbody text\n\nKubernetes-commit: abc\n"},
		{name: "prose after a trailer", message: "subject\n\nKubernetes-commit: abc\nplain line\n"},
		{name: "prose before a trailer", message: "subject\n\nplain line\nKubernetes-commit: abc\n"},
		{name: "one prose line among four trailers", message: "subject\n\nA: 1\nB: 2\nC: 3\nD: 4\nplain\n"},
		{name: "comment inside the block", message: "subject\n\nKubernetes-commit: abc\n# note\nSigned-off-by: A <a@b>\n"},
		{name: "only a comment", message: "subject\n\n# note\n"},
		{name: "continuation folds", message: "subject\n\nKubernetes-commit: abc\n  continued: value\n"},
		{name: "indented block start", message: "subject\n\n  Kubernetes-commit: abc\n"},
		{name: "two blocks", message: "subject\n\nKubernetes-commit: first\n\nKubernetes-commit: second\n"},
		{name: "patch separator", message: "subject\n\nKubernetes-commit: abc\n---\n diff --git a/x b/x\n"},
		{name: "bare patch separator", message: "subject\n\nKubernetes-commit: abc\n---\n"},
		{name: "dashes that are not a separator", message: "subject\n\n---nope\n\nKubernetes-commit: abc\n"},
		{name: "trailing blank lines", message: "subject\n\nKubernetes-commit: abc\n\n\n"},
		{name: "leading blank lines", message: "\n\nsubject\n\nKubernetes-commit: abc\n"},
		{name: "carriage returns", message: "subject\r\n\r\nKubernetes-commit: abc\r\n"},
		{name: "space before the separator", message: "subject\n\nKubernetes-commit : abc\n"},
		{name: "tab before the separator", message: "subject\n\nKubernetes-commit\t: abc\n"},
		{name: "no space after the separator", message: "subject\n\nKubernetes-commit:abc\n"},
		{name: "empty value", message: "subject\n\nKubernetes-commit:\n"},
		{name: "value containing a separator", message: "subject\n\nLink: https://example.com/x\n"},
		{name: "multi word token", message: "subject\n\nTwo words: abc\n"},
		{name: "token with an underscore", message: "subject\n\nKubernetes_commit: abc\n"},
		{name: "token with a dot", message: "subject\n\nKubernetes.commit: abc\n"},
		{name: "repeated key", message: "subject\n\nKubernetes-commit: one\nKubernetes-commit: two\n"},
		{name: "quoted provenance in a revert", message: "subject\n\nThis reverts a commit whose trailer read\nKubernetes-commit: quoted\n\nSigned-off-by: A <a@b>\n"},
		{name: "empty message", message: ""},
		{name: "only blank lines", message: "\n\n\n"},
	}

	for _, test := range messages {
		t.Run(test.name, func(t *testing.T) {
			want, err := git.ParseTrailers(ctx, test.message)
			if err != nil {
				t.Fatalf("git interpret-trailers: %v", err)
			}
			got := gitgraph.TrailerBlock(test.message)

			if len(got) != len(want) {
				t.Fatalf("parsed %d trailers %v, git parsed %d %v", len(got), got, len(want), want)
			}
			for i := range got {
				if got[i].Key != want[i].Key || got[i].Value != want[i].Value {
					t.Fatalf("trailer %d = %q: %q, git reports %q: %q",
						i, got[i].Key, got[i].Value, want[i].Key, want[i].Value)
				}
			}
		})
	}
}

// TestTrailerValueMatchesGitForProvenance checks the lookup the mapping relies
// on, not just the block parse: a message git reads as carrying no trailer must
// never yield a provenance value.
func TestTrailerValueMatchesGitForProvenance(t *testing.T) {
	ctx := t.Context()
	git := newRunner(t, t.TempDir())
	const key = "Kubernetes-commit"

	messages := []string{
		"subject\n\n" + key + ": abc\n",
		key + ": abc",
		"subject\n\nprose\n" + key + ": abc\n",
		"subject\n\nSigned-off-by: A <a@b>\n  " + key + ": abc\n",
		"subject\n\n" + key + ": one\n" + key + ": two\n",
		"subject\n\n" + key + ": abc\n---\n" + key + ": patch\n",
	}
	for _, message := range messages {
		trailers, err := git.ParseTrailers(ctx, message)
		if err != nil {
			t.Fatalf("git interpret-trailers: %v", err)
		}
		wantValue, wantFound := "", false
		for _, trailer := range trailers {
			if trailer.Key == key {
				wantValue, wantFound = trailer.Value, true
			}
		}

		gotValue, gotFound := gitgraph.TrailerValue(message, key)
		if gotFound != wantFound || gotValue != wantValue {
			t.Errorf("TrailerValue(%q) = %q, %v; git reports %q, %v",
				message, gotValue, gotFound, wantValue, wantFound)
		}
	}
}

// TestMinimumVersionCapabilities proves the version this package claims as its
// floor is the version the commands it issues actually need.
//
// The value is asserted against the git the tests run, and the capabilities are
// exercised rather than assumed, because the one that sets the floor,
// GIT_NO_LAZY_FETCH, is ignored rather than rejected by an older git: nothing
// would fail, a probe would just quietly reach the network.
func TestMinimumVersionCapabilities(t *testing.T) {
	ctx := t.Context()
	git := newRunner(t, t.TempDir())

	version, err := git.Version(ctx)
	if err != nil {
		t.Fatalf("git version: %v", err)
	}
	if !version.AtLeast(gitcli.MinimumVersion()) {
		t.Fatalf("git %s is older than the declared minimum %s, so the capability checks below prove nothing",
			version, gitcli.MinimumVersion())
	}
	if err := git.RequireMinimumVersion(ctx); err != nil {
		t.Fatalf("preflight rejected a supported git: %v", err)
	}

	// The releases each capability first shipped in, from git's own release
	// notes. The declared minimum must not be below any of them.
	capabilities := []struct {
		feature string
		version gitcli.Version
	}{
		{feature: "GIT_NO_LAZY_FETCH", version: gitcli.Version{Major: 2, Minor: 45}},
		{feature: "sparse-checkout set --no-cone", version: gitcli.Version{Major: 2, Minor: 35}},
		{feature: "clone and fetch --filter", version: gitcli.Version{Major: 2, Minor: 17}},
		{feature: "fetch --no-write-fetch-head", version: gitcli.Version{Major: 2, Minor: 29}},
		{feature: "config --worktree and worktree config", version: gitcli.Version{Major: 2, Minor: 20}},
		{feature: "worktree add --no-checkout", version: gitcli.Version{Major: 2, Minor: 9}},
		{feature: "worktree list --porcelain", version: gitcli.Version{Major: 2, Minor: 7}},
		{feature: "rev-list --missing and --filter-print-omitted", version: gitcli.Version{Major: 2, Minor: 16}},
		{feature: "merge-base --octopus --all", version: gitcli.Version{Major: 1, Minor: 7, Patch: 3}},
		{feature: "config --get-regexp --name-only", version: gitcli.Version{Major: 2, Minor: 6}},
	}
	for _, capability := range capabilities {
		if !gitcli.MinimumVersion().AtLeast(capability.version) {
			t.Errorf("minimum %s predates %s, which needs %s",
				gitcli.MinimumVersion(), capability.feature, capability.version)
		}
	}
}
