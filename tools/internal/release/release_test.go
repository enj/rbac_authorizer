package release_test

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/release"
)

// TestUnchangedProjection tags the replayed commit directly.
//
// This is the ordinary release: the replayed history already records the exact
// content the release publishes, so there is nothing to commit. A commit written
// here would be an empty one, and an empty commit in published history is a
// permanent record of a change that never happened.
func TestUnchangedProjection(t *testing.T) {
	t.Parallel()
	for _, format := range objectFormats {
		t.Run(string(format), func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			f := newFixture(ctx, t, format)

			result := f.project(ctx, f.options())

			if result.Projected() {
				t.Fatalf("wrote projection commit %s, want none", result.Commit)
			}
			if result.Target != f.replay {
				t.Fatalf("the tag names %s, want the replayed commit %s", result.Target, f.replay)
			}
			if result.Tag != destinationTag {
				t.Fatalf("mapped %s onto %s, want %s", sourceTag, result.Tag, destinationTag)
			}
			if objectType, held := f.objectType(ctx, result.Object); !held || objectType != "tag" {
				t.Fatalf("the tag object is %q held=%t, want an annotated tag", objectType, held)
			}
		})
	}
}

// TestChangedProjection writes exactly one dependency update commit.
//
// The release projection differs from the replayed tree whenever the release
// pins real versions where the replayed commit carries pseudo-versions, which is
// what a staging release does. The commit that records it is the engine's own,
// so it is checked in full: one parent, the projection tree, the bot on both
// identity roles, and no trailer that would let a later run read it back as a
// replayed commit and map an upstream commit onto it.
func TestChangedProjection(t *testing.T) {
	t.Parallel()
	for _, format := range objectFormats {
		t.Run(string(format), func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			f := newFixture(ctx, t, format)

			opts := f.options()
			opts.Projection = f.projection
			result := f.project(ctx, opts)

			if !result.Projected() {
				t.Fatal("wrote no projection commit, want one")
			}
			if result.Target != result.Commit {
				t.Fatalf("the tag names %s, want the projection commit %s", result.Target, result.Commit)
			}
			if tree := f.commitTree(ctx, result.Commit); tree != f.projection {
				t.Fatalf("the projection commit records tree %s, want %s", tree, f.projection)
			}

			read := f.info(ctx, result.Commit)
			if !slices.Equal(read.Parents, []string{f.replay}) {
				t.Fatalf("the projection commit has parents %v, want only the replayed commit %s", read.Parents, f.replay)
			}
			identity := gitcli.Identity(botName, botEmail)
			if read.AuthorIdentity() != identity || read.CommitterIdentity() != identity {
				t.Fatalf("the projection commit is authored by %q and committed by %q, want %q on both roles",
					read.AuthorIdentity(), read.CommitterIdentity(), identity)
			}
			if want := "Update dependencies to " + destinationTag + "\n"; read.RawMessage != want {
				t.Fatalf("the projection commit message is %q, want %q", read.RawMessage, want)
			}
			if len(read.Trailers) != 0 {
				t.Fatalf("the projection commit carries trailers %v, want none: a generated commit claims no source", read.Trailers)
			}
		})
	}
}

// TestProjectionCommitDates checks the raw dates the projection commit records,
// which are the only reason a rerun reproduces its object name.
//
// The commit's name is recomputed from the bytes git stores for a commit rather
// than read through a metadata reader, because no reader reports a date in git's
// raw form and the raw form is exactly what a byte identical rerun depends on.
func TestProjectionCommitDates(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		botDate string
		want    string
	}{
		{name: "the upstream tagger date by default", want: taggerDate},
		{name: "an explicitly supplied raw date", botDate: "1690000000 +0530", want: "1690000000 +0530"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			f := newFixture(ctx, t, gitcli.ObjectFormatSHA1)

			opts := f.options()
			opts.Projection = f.projection
			opts.BotDate = test.botDate
			result := f.project(ctx, opts)

			bot := gitcli.Signature{Name: botName, Email: botEmail, Date: test.want}
			want := commitObjectName(f.format, f.projection, f.replay, bot, bot,
				"Update dependencies to "+destinationTag+"\n")
			if result.Commit != want {
				t.Fatalf("the projection commit is %s, want %s, which records %q on both roles", result.Commit, want, test.want)
			}

			// The tag keeps the upstream release's timestamp whatever the commit
			// records, because the release is what the tag reproduces.
			tagger := gitcli.Signature{Name: botName, Email: botEmail, Date: taggerDate}
			if wantTag := tagObjectName(f.format, result.Target, destinationTag, tagger, result.Message); result.Object != wantTag {
				t.Fatalf("the tag object is %s, want %s, whose tagger date is %q", result.Object, wantTag, taggerDate)
			}
		})
	}
}

// TestSuppliedUpdateMessage records the caller's message verbatim.
func TestSuppliedUpdateMessage(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newFixture(ctx, t, gitcli.ObjectFormatSHA1)

	opts := f.options()
	opts.Projection = f.projection
	opts.UpdateMessage = "chore: pin staging dependencies\n\nthe reason they moved\n"
	result := f.project(ctx, opts)

	if read := f.info(ctx, result.Commit); read.RawMessage != opts.UpdateMessage {
		t.Fatalf("the projection commit message is %q, want %q", read.RawMessage, opts.UpdateMessage)
	}
}

// TestTagObjectBytes checks the annotated tag byte for byte.
//
// The object name is recomputed here from the bytes git stores for a tag rather
// than compared against a recorded constant, so the assertion covers every field
// at once: the target, the tag name, the tagger, the release timestamp, and the
// message. A tag whose bytes differ by one character is a different release, and
// this is what says so.
func TestTagObjectBytes(t *testing.T) {
	t.Parallel()
	for _, format := range objectFormats {
		t.Run(string(format), func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			f := newFixture(ctx, t, format)

			result := f.project(ctx, f.options())

			wantMessage := destinationTag + "\n\n" +
				release.SourceTagKey + ": " + sourceTag + "\n" +
				release.SourceCommitKey + ": " + sourceCommit + "\n" +
				release.SourceReleaseKey + ": " + sourceURL + "\n"
			if result.Message != wantMessage {
				t.Fatalf("the tag message is %q, want %q", result.Message, wantMessage)
			}

			tagger := gitcli.Signature{Name: botName, Email: botEmail, Date: taggerDate}
			if want := tagObjectName(format, f.replay, destinationTag, tagger, wantMessage); result.Object != want {
				t.Fatalf("the tag object is %s, want %s", result.Object, want)
			}
		})
	}
}

// TestReleaseMapping maps upstream tags onto destination tags, including the
// prereleases an alpha or a release candidate publishes.
func TestReleaseMapping(t *testing.T) {
	t.Parallel()
	for _, test := range []struct{ source, want string }{
		{source: "v1.36.1", want: "v0.36.1"},
		{source: "v1.37.0", want: "v0.37.0"},
		{source: "v1.37.0-alpha.2", want: "v0.37.0-alpha.2"},
		{source: "v1.36.0-rc.1", want: "v0.36.0-rc.1"},
		{source: "v1.36.0-beta.0", want: "v0.36.0-beta.0"},
	} {
		t.Run(test.source, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			f := newFixture(ctx, t, gitcli.ObjectFormatSHA1)

			opts := f.options()
			opts.Source.Tag = test.source
			result := f.project(ctx, opts)

			if result.Tag != test.want {
				t.Fatalf("mapped %s onto %s, want %s", test.source, result.Tag, test.want)
			}
			if result.SourceTag != test.source || result.Source != sourceCommit {
				t.Fatalf("the result records source %s %s, want %s %s",
					result.SourceTag, result.Source, test.source, sourceCommit)
			}
			if !strings.Contains(result.Message, release.SourceTagKey+": "+test.source+"\n") {
				t.Fatalf("the tag message %q does not record the upstream tag", result.Message)
			}
		})
	}
}

// TestRefusals covers every input a release must not be built from.
//
// They are one table because they share the property that matters more than any
// one of them: the run refuses, it refuses with a reason a caller can act on,
// and it leaves no ref behind. Half are decided before the repository is opened
// and half only once the objects are probed, and a caller cannot tell which.
func TestRefusals(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		mutate   func(ctx context.Context, f *fixture, opts *release.Options)
		want     error
		contains string
	}{{
		name:     "an unsupported release policy",
		mutate:   func(_ context.Context, _ *fixture, o *release.Options) { o.Policy = "v2-to-v0" },
		contains: "unsupported release policy",
	}, {
		name:     "a source tag outside the policy's major version",
		mutate:   func(_ context.Context, _ *fixture, o *release.Options) { o.Source.Tag = "v2.0.0" },
		contains: "requires a v1 source tag",
	}, {
		name:     "a source tag that is not a version",
		mutate:   func(_ context.Context, _ *fixture, o *release.Options) { o.Source.Tag = "release-1.36" },
		contains: "must start with v",
	}, {
		name:     "a source tag carrying build metadata",
		mutate:   func(_ context.Context, _ *fixture, o *release.Options) { o.Source.Tag = "v1.36.1+build.7" },
		contains: "must not carry build metadata",
	}, {
		name:     "a mapped tag git would refuse as a ref",
		mutate:   func(_ context.Context, _ *fixture, o *release.Options) { o.Source.Tag = "v1.36.1-rc.lock" },
		contains: "must not end with .lock",
	}, {
		name:     "a source commit that is not an object name",
		mutate:   func(_ context.Context, _ *fixture, o *release.Options) { o.Source.Commit = "9f0c1d2e" },
		contains: "must be 40 or 64 hexadecimal characters",
	}, {
		name: "a source commit in upper case",
		mutate: func(_ context.Context, _ *fixture, o *release.Options) {
			o.Source.Commit = strings.ToUpper(sourceCommit)
		},
		contains: "must be lower case hexadecimal",
	}, {
		name:     "a replay commit that is not an object name",
		mutate:   func(_ context.Context, _ *fixture, o *release.Options) { o.Replay.Commit = "HEAD" },
		contains: "replay commit",
	}, {
		name:     "a projection tree that is not an object name",
		mutate:   func(_ context.Context, _ *fixture, o *release.Options) { o.Projection = "refs/tags/v1.36.1" },
		contains: "projection tree",
	}, {
		name:   "a source release URL that is missing",
		mutate: func(_ context.Context, _ *fixture, o *release.Options) { o.Source.URL = "" },
		want:   release.ErrSourceURL,
	}, {
		name: "a source release URL that is not https",
		mutate: func(_ context.Context, _ *fixture, o *release.Options) {
			o.Source.URL = "http://github.com/k/k/releases/tag/v1.36.1"
		},
		want: release.ErrSourceURL,
	}, {
		name: "a source release URL embedding credentials",
		mutate: func(_ context.Context, _ *fixture, o *release.Options) {
			o.Source.URL = "https://token:x-oauth-basic@github.com/k/k/releases/tag/v1.36.1"
		},
		want: release.ErrSourceURL,
	}, {
		name: "a source release URL carrying a query",
		mutate: func(_ context.Context, _ *fixture, o *release.Options) {
			o.Source.URL = sourceURL + "?access_token=secret"
		},
		want: release.ErrSourceURL,
	}, {
		name:   "a source release URL carrying a fragment",
		mutate: func(_ context.Context, _ *fixture, o *release.Options) { o.Source.URL = sourceURL + "#changelog" },
		want:   release.ErrSourceURL,
	}, {
		name: "a source release URL naming a port",
		mutate: func(_ context.Context, _ *fixture, o *release.Options) {
			o.Source.URL = "https://github.com:8443/k/k/releases/tag/v1.36.1"
		},
		want: release.ErrSourceURL,
	}, {
		name:   "a source release URL holding whitespace",
		mutate: func(_ context.Context, _ *fixture, o *release.Options) { o.Source.URL = sourceURL + " " },
		want:   release.ErrSourceURL,
	}, {
		name: "a source release URL naming no host",
		mutate: func(_ context.Context, _ *fixture, o *release.Options) {
			o.Source.URL = "https:///k/k/releases/tag/v1.36.1"
		},
		want: release.ErrSourceURL,
	}, {
		name:     "an upstream tagger date that is not git's raw form",
		mutate:   func(_ context.Context, _ *fixture, o *release.Options) { o.Source.Tagger.Date = "2023-11-14T22:28:20Z" },
		contains: "must be git's raw form",
	}, {
		name:     "a bot date that is not git's raw form",
		mutate:   func(_ context.Context, _ *fixture, o *release.Options) { o.BotDate = "yesterday" },
		contains: "must be git's raw form",
	}, {
		name:     "an upstream tagger without a name",
		mutate:   func(_ context.Context, _ *fixture, o *release.Options) { o.Source.Tagger.Name = "" },
		contains: "source tagger identity: a name is required",
	}, {
		name:     "a bot without an address",
		mutate:   func(_ context.Context, _ *fixture, o *release.Options) { o.Bot.Email = "" },
		contains: "bot identity: an email address is required",
	}, {
		name:     "a bot whose name cannot be recorded faithfully",
		mutate:   func(_ context.Context, _ *fixture, o *release.Options) { o.Bot.Name = "soapbox <bot>" },
		contains: "angle brackets",
	}, {
		name:   "a replay commit the repository does not hold",
		mutate: func(_ context.Context, f *fixture, o *release.Options) { o.Replay.Commit = f.missing() },
		want:   release.ErrObject,
	}, {
		name:   "a replay commit that is a tree",
		mutate: func(_ context.Context, f *fixture, o *release.Options) { o.Replay.Commit = f.replayTree },
		want:   release.ErrObject,
	}, {
		name: "a projection that is a blob",
		mutate: func(ctx context.Context, f *fixture, o *release.Options) {
			o.Projection = f.blob(ctx, "module monis.app/kk/rbac_authorizer\n")
		},
		want: release.ErrObject,
	}, {
		name:   "a replay tree the replay commit does not record",
		mutate: func(_ context.Context, f *fixture, o *release.Options) { o.Replay.Tree = f.projection },
		want:   release.ErrReplayTree,
	}, {
		name: "a dependency update message claiming a source commit",
		mutate: func(_ context.Context, f *fixture, o *release.Options) {
			o.Projection = f.projection
			o.UpdateMessage = "Update dependencies\n\nKubernetes-commit: " + sourceCommit + "\n"
		},
		want: release.ErrProjectionCommit,
	}, {
		name: "a dependency update message that is only whitespace",
		mutate: func(_ context.Context, f *fixture, o *release.Options) {
			o.Projection = f.projection
			o.UpdateMessage = "  \n\n"
		},
		contains: "a message is required",
	}} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			f := newFixture(ctx, t, gitcli.ObjectFormatSHA1)

			opts := f.options()
			test.mutate(ctx, f, &opts)
			result, err := release.Project(ctx, f.git, opts)

			if err == nil {
				t.Fatalf("projected %+v, want a refusal", result)
			}
			if result != nil {
				t.Fatalf("reported result %+v alongside %v, want none", result, err)
			}
			if test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("refused with %v, want %v", err, test.want)
			}
			if test.contains != "" && !strings.Contains(err.Error(), test.contains) {
				t.Fatalf("refused with %v, want a message holding %q", err, test.contains)
			}
			assertNoRefs(ctx, t, f)
		})
	}
}

// TestRefusedURLsAreRedacted keeps a secret out of the diagnostic.
//
// A URL is refused for carrying user information, a query, or a fragment
// precisely because that is where a token hides, so echoing the refused value
// into an error would publish the secret to whatever reads engine output. The
// error still has to say which URL it means, so the rest of it survives.
func TestRefusedURLsAreRedacted(t *testing.T) {
	t.Parallel()
	const secret = "ghp-do-not-log-me"
	for _, test := range []struct{ name, url string }{
		{name: "user information", url: "https://" + secret + "@github.com/k/k/releases/tag/v1.36.1"},
		{name: "a query", url: sourceURL + "?access_token=" + secret},
		{name: "a fragment", url: sourceURL + "#" + secret},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			f := newFixture(ctx, t, gitcli.ObjectFormatSHA1)

			opts := f.options()
			opts.Source.URL = test.url
			_, err := release.Project(ctx, f.git, opts)

			if !errors.Is(err, release.ErrSourceURL) {
				t.Fatalf("refused with %v, want %v", err, release.ErrSourceURL)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("the refusal %q repeats the secret it refused", err)
			}
			if !strings.Contains(err.Error(), "github.com") {
				t.Fatalf("the refusal %q does not say which URL it means", err)
			}
		})
	}
}

// TestNoRefsAreCreated is the boundary this package sits on.
//
// Writing the objects a release is made of is reversible: they are unreachable
// and cost disk. Giving one of them a name is not, because a published tag is
// immutable, so naming it is a decision a publisher takes later and can refuse.
func TestNoRefsAreCreated(t *testing.T) {
	t.Parallel()
	for _, format := range objectFormats {
		t.Run(string(format), func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			f := newFixture(ctx, t, format)

			opts := f.options()
			opts.Projection = f.projection
			result := f.project(ctx, opts)

			if objectType, held := f.objectType(ctx, result.Object); !held || objectType != "tag" {
				t.Fatalf("the tag object is %q held=%t, want an annotated tag to exist", objectType, held)
			}
			assertNoRefs(ctx, t, f)
		})
	}
}

// assertNoRefs requires that neither tag name exists and that nothing has a
// head, which together mean the run named nothing.
func assertNoRefs(ctx context.Context, t *testing.T, f *fixture) {
	t.Helper()
	for _, name := range []string{destinationTag, sourceTag} {
		if _, err := f.git.TagInfo(ctx, name); !errors.Is(err, gitcli.ErrTagNotFound) {
			t.Fatalf("tag %s exists, want no ref: %v", name, err)
		}
	}
	head, err := f.git.HasHead(ctx)
	if err != nil {
		t.Fatalf("probe head: %v", err)
	}
	if head {
		t.Fatal("the destination repository has a head, want no ref at all")
	}
}

// TestRerunIsIdentical projects the same release into two repositories under two
// temporary paths.
//
// This is the property a published release rests on: regenerating it has to
// produce the tag that is already out. Two repositories rather than two runs in
// one, because a second run in the same repository would find its own objects
// already written and could pass while depending on that.
func TestRerunIsIdentical(t *testing.T) {
	t.Parallel()
	for _, format := range objectFormats {
		t.Run(string(format), func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()

			var results []*release.Result
			for range 2 {
				f := newFixture(ctx, t, format)
				opts := f.options()
				opts.Projection = f.projection
				results = append(results, f.project(ctx, opts))
			}

			first, second := results[0], results[1]
			if first.Object != second.Object {
				t.Fatalf("the two runs wrote tag objects %s and %s, want one name", first.Object, second.Object)
			}
			if first.Commit != second.Commit {
				t.Fatalf("the two runs wrote projection commits %s and %s, want one name", first.Commit, second.Commit)
			}
			if first.Target != second.Target {
				t.Fatalf("the two runs tagged %s and %s, want one commit", first.Target, second.Target)
			}
			if !slices.Equal(first.Report(), second.Report()) {
				t.Fatalf("the two runs reported %v and %v, want one report", first.Report(), second.Report())
			}
		})
	}
}

// TestReport renders the release as the deterministic lines a dry run compares.
func TestReport(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newFixture(ctx, t, gitcli.ObjectFormatSHA1)

	unchanged := f.project(ctx, f.options())
	opts := f.options()
	opts.Projection = f.projection
	changed := f.project(ctx, opts)

	for _, test := range []struct {
		name   string
		result *release.Result
		want   []string
	}{{
		name:   "a release the replay already produced",
		result: unchanged,
		want: []string{
			"tag " + destinationTag + " " + unchanged.Object,
			"target " + f.replay,
			"projection none",
			"source " + sourceTag + " " + sourceCommit,
		},
	}, {
		name:   "a release carrying a dependency update commit",
		result: changed,
		want: []string{
			"tag " + destinationTag + " " + changed.Object,
			"target " + changed.Commit,
			"projection " + changed.Commit,
			"source " + sourceTag + " " + sourceCommit,
		},
	}} {
		t.Run(test.name, func(t *testing.T) {
			if got := test.result.Report(); !slices.Equal(got, test.want) {
				t.Fatalf("reported %v, want %v", got, test.want)
			}
		})
	}
	if lines := (*release.Result)(nil).Report(); lines != nil {
		t.Fatalf("a nil result reported %v, want nothing", lines)
	}
	if (*release.Result)(nil).Projected() {
		t.Fatal("a nil result reports a projection commit, want none")
	}
}

// TestCancellation refuses before writing anything.
func TestCancellation(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newFixture(ctx, t, gitcli.ObjectFormatSHA1)

	cancelled, cancel := context.WithCancel(ctx)
	cancel()

	opts := f.options()
	opts.Projection = f.projection
	result, err := release.Project(cancelled, f.git, opts)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("refused with %v, want a cancellation", err)
	}
	if result != nil {
		t.Fatalf("reported result %+v, want none", result)
	}
	// The tag the run would have written does not exist, which is what "wrote
	// nothing" means for a package whose whole output is objects.
	tagger := gitcli.Signature{Name: botName, Email: botEmail, Date: taggerDate}
	message := destinationTag + "\n\n" +
		release.SourceTagKey + ": " + sourceTag + "\n" +
		release.SourceCommitKey + ": " + sourceCommit + "\n" +
		release.SourceReleaseKey + ": " + sourceURL + "\n"
	object := tagObjectName(f.format, f.replay, destinationTag, tagger, message)
	if objectType, held := f.objectType(ctx, object); held {
		t.Fatalf("the repository holds %s as a %s, want a cancelled run to have written nothing", object, objectType)
	}
	assertNoRefs(ctx, t, f)
}

// tagObjectName recomputes the name git gives the annotated tag these fields
// describe.
func tagObjectName(format gitcli.ObjectFormat, target, name string, tagger gitcli.Signature, message string) string {
	body := "object " + target + "\n" +
		"type commit\n" +
		"tag " + name + "\n" +
		"tagger " + gitcli.Identity(tagger.Name, tagger.Email) + " " + tagger.Date + "\n\n" +
		message
	return objectName(format, "tag", body)
}

// commitObjectName recomputes the name git gives the single parent commit these
// fields describe.
func commitObjectName(format gitcli.ObjectFormat, tree, parent string, author, committer gitcli.Signature, message string) string {
	body := "tree " + tree + "\n" +
		"parent " + parent + "\n" +
		"author " + gitcli.Identity(author.Name, author.Email) + " " + author.Date + "\n" +
		"committer " + gitcli.Identity(committer.Name, committer.Email) + " " + committer.Date + "\n\n" +
		message
	return objectName(format, "commit", body)
}

// objectName is git's own naming rule: the type, the body's length, a null byte,
// and the body, hashed with the repository's algorithm.
func objectName(format gitcli.ObjectFormat, objectType, body string) string {
	stored := fmt.Sprintf("%s %d\x00%s", objectType, len(body), body)
	if format == gitcli.ObjectFormatSHA256 {
		return fmt.Sprintf("%x", sha256.Sum256([]byte(stored)))
	}
	return fmt.Sprintf("%x", sha1.Sum([]byte(stored)))
}
