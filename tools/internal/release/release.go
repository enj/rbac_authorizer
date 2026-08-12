// Package release projects one upstream release onto a deterministic
// destination tag.
//
// A release is two decisions. The first is what the release's content is: at a
// release the generated module depends on real staging versions rather than on
// the pseudo-versions the replayed history carries, so the tree a release is cut
// from is not always the tree the replayed commit records. The second is what
// names it: the destination tag, mapped from the upstream one by policy, written
// as an annotated object that records where the release came from.
//
// Both are a function of what this package is handed. The upstream tag, its
// commit, its tagger's raw date, and its release page arrive as values; the
// replayed commit, the tree it records, and the exact release projection tree
// arrive as object names the caller has already written. Nothing is read from a
// clock, an environment, a ref, or a map, so two runs over the same inputs
// produce the same object names. That is what makes a published release
// reproducible: regenerating it has to yield the tag that is already out.
//
// The package writes objects and nothing else. It creates no ref, moves none,
// and pushes nothing, so the tag it writes is unreachable until a publisher
// decides to name it. Deciding what a release would be and deciding to publish
// it are separate gates, and only the second one can take a name that cannot be
// given back.
//
// Nothing here is specific to Kubernetes. The version policy, the identities,
// the dates, and the trees are all parameters, and the only judgement the
// package makes on its own is that a release URL it is about to publish has to
// be one that is safe to publish.
package release

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/enj/soapbox/tools/internal/config"
	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/gitgraph"
	"github.com/enj/soapbox/tools/internal/treebuild"
)

// Projection sentinels. Callers use errors.Is to distinguish the refusal.
var (
	// ErrObject reports an object that the destination repository does not hold,
	// or holds as something other than the type the release needs it to be.
	ErrObject = errors.New("object is missing from the destination repository or is not the expected type")
	// ErrReplayTree reports a replay commit that does not record the tree the
	// caller said it does.
	ErrReplayTree = errors.New("replay commit does not record the declared tree")
	// ErrSourceURL reports a source release URL that must not be published.
	ErrSourceURL = errors.New("source release URL is not a publishable https URL")
	// ErrProjectionCommit reports a written commit that does not read back as the
	// commit it was asked to be.
	ErrProjectionCommit = errors.New("projection commit did not read back as written")
	// ErrTagTarget reports a written tag object that does not name the commit the
	// release was projected onto.
	ErrTagTarget = errors.New("tag object does not name the projected commit")
)

// Tag message keys.
//
// They are trailer shaped so that what a release was cut from is readable by a
// person and parseable by git's own trailer reader, and they are exported
// because a publisher comparing a released tag against a regenerated one is
// reading exactly these three fields.
const (
	SourceTagKey     = "Source-tag"
	SourceCommitKey  = "Source-commit"
	SourceReleaseKey = "Source-release"
)

// redacted replaces any part of a URL a secret could travel in. It carries no
// character URL encoding would escape, so a redacted URL stays readable.
const redacted = "redacted"

// Identity is a name and address with no date.
//
// The engine's bot is a fixed identity and every date a release records is
// decided by the release rather than by the run, so an identity carrying one
// here would be offering the single value that stops a rerun from reproducing an
// object name.
type Identity struct {
	// Name is the identity's display name.
	Name string
	// Email is the identity's address.
	Email string
}

// Source is the upstream release being projected.
//
// It is carried rather than looked up, because this package never opens the
// source repository: it writes into the destination one. The tagger is the whole
// signature a source read reports rather than a bare date, so a caller hands
// over what it read instead of decomposing it, which is what stops a run from
// pairing one release's identity with another release's timestamp.
type Source struct {
	// Tag is the upstream release tag, such as v1.36.1.
	Tag string
	// Commit is the exact upstream commit object name the release was cut from.
	// It is recorded in the destination tag message and is never resolved here,
	// because it names an object of the source repository and this package only
	// ever opens the destination one.
	Commit string
	// Tagger is the upstream tag object's tagger.
	//
	// Its raw date is what the destination tag records, so a regenerated tag is
	// byte identical to the one the upstream release's timing produced. Its name
	// and address are not written into anything: the engine created this tag, and
	// recording the upstream tagger as its tagger would claim a release they
	// never made. They are still required, because a caller that cannot say who
	// tagged the release upstream did not read an annotated tag, and the date it
	// is offering is then not the one this package is asking for.
	Tagger gitcli.Signature
	// URL is the upstream release page. It is published verbatim inside an object
	// that can never be taken back, so it has to be a URL that is safe to
	// publish.
	URL string
}

// Replay is where the replayed history stands at the release: the destination
// commit the source commit produced, and the tree that commit records.
//
// The tree is stated as well as read, because the caller decided the release
// projection by comparing against it. A tree the caller believes and the commit
// does not record means the two halves of the run disagree about which commit
// this release is being cut from, and a disagreement about that has to end the
// run rather than be resolved in favour of one of them.
type Replay struct {
	// Commit is the destination commit the replay produced for the source commit
	// the release names.
	Commit string
	// Tree is the tree that commit records.
	Tree string
}

// Options describes one release projection.
type Options struct {
	// Policy maps the upstream release tag onto the destination one, such as
	// config.ReleasePolicyV1ToV0.
	Policy string
	// Source is the upstream release being projected.
	Source Source
	// Replay is where the replayed history stands at that release.
	Replay Replay
	// Projection is the exact release projection tree: the release's content
	// carrying real destination dependency versions rather than the
	// pseudo-versions an intermediate commit records. The caller writes it,
	// because this package decides shape and never content.
	Projection string
	// Bot is the identity the destination tag records as its tagger and the
	// projection commit records as both its author and its committer.
	Bot Identity
	// BotDate is the raw date the projection commit records for both roles.
	//
	// Empty adopts the upstream tagger's date, which is the honest default: the
	// release is the only reason that commit exists, so the moment it describes
	// is the moment the release was cut. A caller that needs a different one
	// states it, and states it in git's raw form, because a date this package
	// derived from anything else is a date a rerun would not reproduce.
	BotDate string
	// UpdateMessage is the projection commit's complete message. Empty uses the
	// default shape, which names the destination tag the dependencies were moved
	// to.
	UpdateMessage string
}

// Project writes the objects one release is made of and reports them.
//
// The shape is decided by these rules, and only by these rules:
//
//   - The destination tag name is the policy's mapping of the upstream tag, so
//     an upstream v1.X.Y[-pre] becomes a v0.X.Y[-pre] under the v1-to-v0 policy.
//     A tag the policy cannot map, or maps onto a name git would refuse, is a
//     refusal rather than a name invented here.
//
//   - A release projection equal to the tree the replayed commit already records
//     needs no commit of its own, and the tag names the replayed commit
//     directly. Writing an empty commit to have something release shaped to
//     point at would put a commit in the published history that changed nothing.
//
//   - A release projection differing from it produces exactly one commit: the
//     replayed commit as its only parent, the projection tree as its content,
//     the bot as both author and committer, and no provenance trailer. The
//     trailer's absence is the point. It is how a later run rebuilds the source
//     to destination mapping from the published history, and a commit no
//     upstream commit produced must be skipped by that reader rather than map a
//     source commit onto a commit it never wrote.
//
//   - The tag is an annotated object naming that commit, whichever of the two it
//     turned out to be, with the upstream tagger's raw date and a message
//     recording the upstream tag, the exact upstream commit, and the release
//     page. Everything written is read back and compared against what was asked
//     for, because an object that is wrong is only cheap to discover before a
//     ref names it.
//
// No ref is created and none is moved. The tag object exists, unreachable, and
// what it would be called is a decision a publisher makes later and can refuse.
//
// A run that fails validation writes nothing at all. A run that fails after
// writing leaves unreachable objects, which cost disk and nothing else.
func Project(ctx context.Context, git *gitcli.Runner, opts Options) (*Result, error) {
	// A cancelled run writes nothing. The check is first so that cancellation is
	// the answer even when the options are also wrong: the caller stopped asking,
	// and a critique of inputs it no longer cares about is not the reply.
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("release: %w", err)
	}
	tag, err := validate(opts)
	if err != nil {
		return nil, fmt.Errorf("release %q: %w", opts.Source.Tag, err)
	}
	if err := verifyInputs(ctx, git, opts); err != nil {
		return nil, fmt.Errorf("release %s: %w", tag, err)
	}

	commit, err := projectCommit(ctx, git, opts, tag)
	if err != nil {
		return nil, fmt.Errorf("release %s: %w", tag, err)
	}
	target := opts.Replay.Commit
	if commit != "" {
		target = commit
	}

	message := tagMessage(tag, opts.Source)
	object, err := treebuild.WriteTag(ctx, git, treebuild.TagOptions{
		Commit: target,
		Name:   tag,
		Tagger: gitcli.Signature{
			Name:  opts.Bot.Name,
			Email: opts.Bot.Email,
			Date:  opts.Source.Tagger.Date,
		},
		Message: message,
	})
	if err != nil {
		return nil, fmt.Errorf("release %s: %w", tag, err)
	}
	if err := verifyTag(ctx, git, object, target); err != nil {
		return nil, fmt.Errorf("release %s: %w", tag, err)
	}

	return &Result{
		Tag:       tag,
		Object:    object,
		Target:    target,
		Commit:    commit,
		Source:    opts.Source.Commit,
		SourceTag: opts.Source.Tag,
		Message:   message,
	}, nil
}

// validate checks everything that can be judged without opening the repository
// and reports the destination tag name.
//
// It runs first and completely, because the steps after it open the destination
// repository and then write to it, and an object written for a release whose URL
// turns out to be unpublishable is something that exists in that repository for
// no reason at all.
func validate(opts Options) (string, error) {
	tag, err := config.MapReleaseTag(opts.Policy, opts.Source.Tag)
	if err != nil {
		return "", err
	}
	// The mapped name is checked against git's own rule rather than assumed to be
	// usable. A prerelease is free-form enough to produce a name git refuses, and
	// the same check runs again when the object is written, so this is the same
	// authority answering earlier rather than a second one.
	if err := gitcli.ValidateBranchName(tag); err != nil {
		return "", fmt.Errorf("destination tag: %w", err)
	}

	for _, object := range []struct {
		role string
		name string
	}{
		{"source commit", opts.Source.Commit},
		{"replay commit", opts.Replay.Commit},
		{"replay tree", opts.Replay.Tree},
		{"projection tree", opts.Projection},
	} {
		if err := gitgraph.ValidateSHA(object.name); err != nil {
			return "", fmt.Errorf("%s: %w", object.role, err)
		}
	}

	if err := validateSourceURL(opts.Source.URL); err != nil {
		return "", err
	}
	if err := gitcli.ValidateRawDate(opts.Source.Tagger.Date); err != nil {
		return "", fmt.Errorf("source tagger: %w", err)
	}
	if opts.BotDate != "" {
		if err := gitcli.ValidateRawDate(opts.BotDate); err != nil {
			return "", fmt.Errorf("bot date: %w", err)
		}
	}
	if err := validateIdentity("source tagger", Identity{Name: opts.Source.Tagger.Name, Email: opts.Source.Tagger.Email}); err != nil {
		return "", err
	}
	if err := validateIdentity("bot", opts.Bot); err != nil {
		return "", err
	}
	return tag, nil
}

// validateIdentity checks an identity for presence only.
//
// It is deliberately weaker than the check gitcli performs when it writes an
// identity into an object body. This exists so a run refuses before it writes
// anything, not so that there are two authorities on what an identity is: gitcli
// refuses a name or an address it cannot record faithfully, and a rule here that
// disagreed with it would be a second answer to the same question.
func validateIdentity(role string, identity Identity) error {
	switch {
	case identity.Name == "":
		return fmt.Errorf("%s identity: a name is required", role)
	case identity.Email == "":
		return fmt.Errorf("%s identity: an email address is required", role)
	}
	return nil
}

// validateSourceURL checks the release URL the tag message publishes.
//
// The URL is copied verbatim into an object that gets pushed to a public
// repository and can never be withdrawn, so it is accepted only as a plain https
// link. User information, a query, and a fragment are the three places a
// credential rides along in a URL, and none of them belongs in a link to a
// release page; a scheme other than https and an explicit port are both ways for
// a published link to point somewhere other than where it appears to.
//
// Which hosts are acceptable is a profile decision and not one this package can
// make, so the host is only required to exist.
func validateSourceURL(raw string) error {
	switch {
	case raw == "":
		return fmt.Errorf("%w: a URL is required", ErrSourceURL)
	case strings.ContainsAny(raw, " \t\n\r\x00"):
		return fmt.Errorf("%w: it must not contain whitespace or a null byte", ErrSourceURL)
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		// The value is not echoed. It could not be parsed, so it cannot be
		// inspected for the user information a diagnostic must not repeat.
		return fmt.Errorf("%w: it is malformed", ErrSourceURL)
	}
	switch {
	case parsed.Scheme != "https":
		return fmt.Errorf("%w: %q must use https", ErrSourceURL, safeURL(parsed))
	case parsed.User != nil:
		return fmt.Errorf("%w: %q must not embed credentials", ErrSourceURL, safeURL(parsed))
	case parsed.Hostname() == "":
		return fmt.Errorf("%w: %q must name a host", ErrSourceURL, safeURL(parsed))
	case parsed.Port() != "":
		return fmt.Errorf("%w: %q must not set an explicit port", ErrSourceURL, safeURL(parsed))
	case parsed.RawQuery != "" || parsed.ForceQuery:
		return fmt.Errorf("%w: %q must not carry a query", ErrSourceURL, safeURL(parsed))
	case parsed.Fragment != "" || parsed.RawFragment != "":
		return fmt.Errorf("%w: %q must not carry a fragment", ErrSourceURL, safeURL(parsed))
	}
	return nil
}

// safeURL renders a URL without the parts a credential rides in, because a value
// that carries a token must never reach a log, a report, or a CI annotation. It
// is the refused URL's own diagnostic, so the secret it was refused for is
// exactly what must not appear in it. What remains is enough to recognise which
// URL was rejected.
func safeURL(parsed *url.URL) string {
	safe := *parsed
	if safe.User != nil {
		safe.User = url.User(redacted)
	}
	if safe.RawQuery != "" || safe.ForceQuery {
		safe.RawQuery, safe.ForceQuery = redacted, false
	}
	if safe.Fragment != "" || safe.RawFragment != "" {
		safe.Fragment, safe.RawFragment = redacted, ""
	}
	return safe.String()
}

// verifyInputs checks that the destination repository holds what the release was
// described in terms of.
//
// The names are probed against the local object store rather than trusted,
// because a tree name that is really a blob, or a commit that was never written,
// would otherwise reach the tag object and become a release pointing at
// something that is not a release.
func verifyInputs(ctx context.Context, git *gitcli.Runner, opts Options) error {
	for _, object := range []struct {
		role string
		name string
		want string
	}{
		{"replay commit", opts.Replay.Commit, "commit"},
		{"replay tree", opts.Replay.Tree, "tree"},
		{"projection tree", opts.Projection, "tree"},
	} {
		if err := requireType(ctx, git, object.name, object.want); err != nil {
			return fmt.Errorf("%s: %w", object.role, err)
		}
	}

	recorded, err := git.ResolveTree(ctx, opts.Replay.Commit)
	if err != nil {
		return err
	}
	if recorded != opts.Replay.Tree {
		return fmt.Errorf("%w: commit %s records tree %s, not %s",
			ErrReplayTree, opts.Replay.Commit, recorded, opts.Replay.Tree)
	}
	return nil
}

// requireType checks that an object exists in the destination repository with
// the expected type. The probe answers from the local object store, because
// whether this repository already holds an object is a question about this
// repository.
func requireType(ctx context.Context, git *gitcli.Runner, object, want string) error {
	infos, err := git.ObjectInfoBatch(ctx, gitcli.ObjectInfoOptions{Revisions: []string{object}})
	if err != nil {
		return err
	}
	if len(infos) != 1 {
		return fmt.Errorf("object %s: got %d records, want 1", object, len(infos))
	}
	switch info := infos[0]; {
	case info.Missing:
		return fmt.Errorf("%w: %s is missing", ErrObject, object)
	case info.Type != want:
		return fmt.Errorf("%w: %s is a %s, want a %s", ErrObject, object, info.Type, want)
	}
	return nil
}

// projectCommit writes the dependency update commit the release needs, and
// reports the empty string when it needs none.
func projectCommit(ctx context.Context, git *gitcli.Runner, opts Options, tag string) (string, error) {
	if opts.Projection == opts.Replay.Tree {
		return "", nil
	}

	date := opts.BotDate
	if date == "" {
		date = opts.Source.Tagger.Date
	}
	bot := gitcli.Signature{Name: opts.Bot.Name, Email: opts.Bot.Email, Date: date}
	message := opts.UpdateMessage
	if message == "" {
		message = updateMessage(tag)
	}

	commit, err := treebuild.WriteSyntheticCommit(ctx, git, treebuild.SyntheticCommitOptions{
		Tree:      opts.Projection,
		Parents:   []string{opts.Replay.Commit},
		Author:    bot,
		Committer: bot,
		Message:   message,
	})
	if err != nil {
		return "", err
	}
	if err := verifyProjection(ctx, git, commit, opts, message); err != nil {
		return "", err
	}
	return commit, nil
}

// updateMessage is the projection commit's message when the caller supplies
// none. It names the destination tag rather than the upstream one, because the
// versions the commit moves to are the destination's own.
func updateMessage(tag string) string {
	return "Update dependencies to " + tag + "\n"
}

// verifyProjection reads the written commit back and checks it against what it
// was asked to be.
//
// The commit is the one thing here that is assembled from several inputs at
// once, so it is the one thing worth reading back: a tree, a parent, or an
// identity that arrived in the wrong slot produces a perfectly valid commit that
// is simply not the release. The message is compared byte for byte, and the
// trailer block is required to be empty, because the trailer block is how the
// published history is read back into a source to destination mapping and a
// generated commit has to be invisible to that reader.
func verifyProjection(ctx context.Context, git *gitcli.Runner, commit string, opts Options, message string) error {
	tree, err := git.ResolveTree(ctx, commit)
	if err != nil {
		return err
	}
	if tree != opts.Projection {
		return fmt.Errorf("%w: commit %s records tree %s, not %s", ErrProjectionCommit, commit, tree, opts.Projection)
	}

	read, err := git.CommitInfo(ctx, commit)
	if err != nil {
		return err
	}
	identity := gitcli.Identity(opts.Bot.Name, opts.Bot.Email)
	switch {
	case len(read.Parents) != 1 || read.Parents[0] != opts.Replay.Commit:
		return fmt.Errorf("%w: commit %s has parents %q, want only %s",
			ErrProjectionCommit, commit, strings.Join(read.Parents, " "), opts.Replay.Commit)
	case read.AuthorIdentity() != identity:
		return fmt.Errorf("%w: commit %s is authored by %q, want %q", ErrProjectionCommit, commit, read.AuthorIdentity(), identity)
	case read.CommitterIdentity() != identity:
		return fmt.Errorf("%w: commit %s is committed by %q, want %q", ErrProjectionCommit, commit, read.CommitterIdentity(), identity)
	case read.RawMessage != message:
		return fmt.Errorf("%w: commit %s does not record the message it was written with", ErrProjectionCommit, commit)
	case len(read.Trailers) != 0:
		return fmt.Errorf("%w: commit %s carries %d trailers, and a generated commit claims no source",
			ErrProjectionCommit, commit, len(read.Trailers))
	case read.SignatureStatus != "" && read.SignatureStatus != "N":
		return fmt.Errorf("%w: commit %s is signed, and a generated commit has no signer", ErrProjectionCommit, commit)
	}
	return nil
}

// tagMessage renders the destination tag message.
//
// The shape is a subject naming the release and one paragraph recording where it
// came from, which is a paragraph of nothing but trailers so git reads it as
// such. It is what makes a published tag answer "which upstream release is this"
// on its own, without the reader having to invert a version policy or trust a
// separate manifest. It ends in a newline because a tag message is stored
// verbatim and one that did not would be the only release object whose bytes
// depend on how it was assembled.
func tagMessage(tag string, source Source) string {
	var out strings.Builder
	out.WriteString(tag)
	out.WriteString("\n\n")
	out.WriteString(SourceTagKey + ": " + source.Tag + "\n")
	out.WriteString(SourceCommitKey + ": " + source.Commit + "\n")
	out.WriteString(SourceReleaseKey + ": " + source.URL + "\n")
	return out.String()
}

// verifyTag reads the written tag object back and checks that it is a tag and
// that it names the commit the release was projected onto.
//
// Peeling the object is the check that matters. A tag object records its target
// in its own body, so asking git which commit it resolves to is asking the
// question a publisher will ask, rather than repeating the value that was just
// handed to the writer.
func verifyTag(ctx context.Context, git *gitcli.Runner, object, target string) error {
	if err := requireType(ctx, git, object, "tag"); err != nil {
		return fmt.Errorf("tag object: %w", err)
	}
	named, err := git.ResolveCommit(ctx, object)
	if err != nil {
		return err
	}
	if named != target {
		return fmt.Errorf("%w: tag object %s names %s, not %s", ErrTagTarget, object, named, target)
	}
	return nil
}
