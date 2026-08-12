// Package publish is the engine's append only publication boundary.
//
// Publication is split in two, and the split is the point. Plan reads the
// destination remote and the local object graph, decides what every requested
// ref update would do, and renders the exact outward actions as a manifest with
// a hash over its own content. Apply takes a plan whose hash the caller has
// approved, re-reads the remote to prove nothing moved underneath it, and
// performs one atomic push. Nothing in this package moves a ref without a plan,
// and no plan is executed without the caller naming its hash.
//
// The gates that decide whether publication should happen at all, the closure
// report, the dependency policy, the API comparison, live above this package.
// They enter here as one value: an approval that names a manifest hash. That
// keeps the boundary honest in both directions. This package cannot be talked
// into publishing by a caller that skipped its checks, and it cannot quietly
// take credit for checks it never ran.
//
// Every refusal is fail closed. A branch that is not a fast forward, a tag that
// already exists with different content, a remote that changed between planning
// and applying, an object the local repository does not have: each one stops
// publication rather than degrading it. There is no force path, no delete path,
// and no way to ask for either, because an append only publisher that can be
// asked nicely is not append only.
//
// # Reading remote refs
//
// Deciding what a push would do requires knowing what the remote holds now, and
// the only safe way to learn that is git ls-remote, which reads refs without
// fetching objects and without a URL that could carry a credential. The typed
// Git boundary does not expose it yet, so this package takes the reader as a
// RemoteRefLister. LocalRemote implements it for a destination that lives on
// this machine, which covers dry runs and tests against real bare remotes. A
// network destination has no implementation here and reports
// ErrRemoteRefsUnsupported rather than guessing; see the package's
// documentation of the missing API in the task report.
package publish

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/enj/soapbox/tools/internal/gitcli"
)

// Kind classifies one destination ref by who reads it.
//
// The distinction that matters is consumer versus not. A consumer ref is one a
// module user or the Go module proxy can observe: the release branch and the
// release tags. Everything else, the resumable state branch and the temporary
// progress refs a long backfill writes between chunks, is the engine talking to
// itself. Both live in the same repository and both are append only, but only
// the first may move after the gates pass, and the two never travel in the same
// push.
type Kind string

// The destination ref kinds.
const (
	// KindBranch is a consumer branch under refs/heads/.
	KindBranch Kind = "branch"
	// KindTag is a consumer release tag under refs/tags/, pointing at an
	// annotated tag object.
	KindTag Kind = "tag"
	// KindProgress is a temporary backfill chunk ref outside refs/heads/ and
	// refs/tags/, where no consumer and no module proxy will look.
	KindProgress Kind = "progress"
	// KindState is the resumable state branch. It lives under refs/heads/ so
	// the repository shows activity, which is exactly why it has to be
	// classified explicitly: it looks like a consumer branch and is not one.
	KindState Kind = "state"
)

// Consumer reports whether a module user or the module proxy can observe this
// kind of ref.
func (k Kind) Consumer() bool { return k == KindBranch || k == KindTag }

// valid reports whether k is one of the four kinds.
func (k Kind) valid() bool {
	switch k {
	case KindBranch, KindTag, KindProgress, KindState:
		return true
	default:
		return false
	}
}

// Effect is what a planned action would do to the remote.
type Effect string

// The effects an append only publisher can produce. There is deliberately no
// fourth: moving a ref backwards, replacing a tag, and deleting anything are
// not effects this package can name, let alone perform.
const (
	// EffectCreate writes a ref the remote does not have.
	EffectCreate Effect = "create"
	// EffectFastForward advances a ref to a descendant of its current value.
	EffectFastForward Effect = "fast-forward"
	// EffectNoOp leaves a ref that already holds the planned object alone. A
	// re-run of a completed publication is the common case, so it has to be a
	// silent success rather than a collision.
	EffectNoOp Effect = "no-op"
)

// Update is one requested destination ref update.
type Update struct {
	// Ref is the fully qualified destination ref name.
	Ref string
	// Kind classifies the ref. It is stated rather than inferred from the name
	// because the state branch is indistinguishable from a consumer branch by
	// name alone, and getting that wrong would push engine bookkeeping in the
	// same atomic batch as a release.
	Kind Kind
	// NewObject is the full object name the ref must end up pointing at: a
	// commit for a branch, a state ref, or a progress ref, and an annotated tag
	// object for a tag. Abbreviations and revision expressions are rejected,
	// because a plan that says "main" describes whatever the repository happens
	// to hold when it is applied rather than what was approved.
	NewObject string
	// ExpectedOld is what the caller observed on the remote, when it observed
	// anything. A plan whose remote read disagrees with it is refused: the
	// caller ran its gates against that value, so a different one means the
	// gates were run against a repository state that no longer exists.
	ExpectedOld string
	// ExpectAbsent states that the caller observed no such ref. It is separate
	// from an empty ExpectedOld, which states nothing at all, because
	// "I saw nothing" and "I did not look" have to be told apart: only the
	// first can be contradicted by the remote.
	ExpectAbsent bool
	// Evidence labels where this update came from, such as replay:master or
	// release:v0.36.1. It travels into the manifest so a person approving one
	// can see why each ref moves. It names the engine's own reasoning, never a
	// path, a URL, or anything that could carry a secret.
	Evidence string
}

// Namespaces describes the destination repository's non consumer ref layout.
//
// The values come from the destination profile rather than from constants here,
// because the engine that generates a repository writes them into that
// repository's configuration and this package must agree with what was written
// rather than with what it would have chosen.
type Namespaces struct {
	// StateRef is the fully qualified resumable state branch, such as
	// refs/heads/soapbox-state. Empty means the destination has none, and a
	// state update is then refused.
	StateRef string
	// ProgressPrefix is the ref namespace holding backfill chunks, such as
	// refs/soapbox/progress/. It ends with a slash and lives outside
	// refs/heads/ and refs/tags/ so no consumer and no module proxy can see it.
	ProgressPrefix string
}

// Options configures a Publisher.
type Options struct {
	// Remote is the push target: an https URL on the publication host, or an
	// absolute path or file URL when AllowLocalRemote is set. A named remote is
	// refused by the Git boundary because its URL would live in configuration
	// this package cannot see.
	Remote string
	// Identity is the canonical destination repository recorded in every
	// manifest, such as github.com/enj/rbac_authorizer.
	//
	// It exists so a manifest describes a repository rather than a location. An
	// https remote derives it and may state it for confirmation; a local remote
	// must state it, because the alternative is a temporary directory path in
	// an approved artifact, which is neither stable across runs nor anybody's
	// business.
	Identity string
	// AllowLocalRemote permits a path or file URL destination. It is off by
	// default so a mistyped configuration cannot publish into a directory.
	AllowLocalRemote bool
	// Namespaces describes where non consumer refs live.
	Namespaces Namespaces
	// Lister reads the refs the destination currently advertises. It is
	// required, since no plan can be made without knowing what is already
	// published.
	Lister RemoteRefLister
	// ObjectFormat is the destination's hash algorithm. Empty adopts the local
	// repository's, and a stated value that disagrees with the local repository
	// is refused rather than silently preferred.
	ObjectFormat gitcli.ObjectFormat
}

// Publisher plans and applies append only publications to one destination.
type Publisher struct {
	git        *gitcli.Runner
	lister     RemoteRefLister
	remote     string
	identity   string
	namespaces Namespaces
	format     gitcli.ObjectFormat
}

// New binds a publisher to one destination and one local repository.
//
// The local repository is probed for its hash algorithm here rather than
// assumed, because object names are only comparable within one algorithm: a
// publisher that assumed SHA-1 against a SHA-256 destination would read every
// remote value as malformed, and one that assumed the reverse would accept
// truncated names.
func New(ctx context.Context, git *gitcli.Runner, opts Options) (*Publisher, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("publisher: %w", err)
	}
	if git == nil {
		return nil, errors.New("publisher: a git runner is required")
	}
	if opts.Lister == nil {
		return nil, errors.New("publisher: a remote ref lister is required")
	}
	if err := gitcli.ValidatePushRemote(opts.Remote); err != nil {
		return nil, fmt.Errorf("publisher: %w", err)
	}
	local, err := isLocalRemote(opts.Remote)
	if err != nil {
		return nil, fmt.Errorf("publisher: %w", err)
	}
	if local && !opts.AllowLocalRemote {
		return nil, fmt.Errorf("publisher: %w", ErrLocalRemoteNotAllowed)
	}
	identity, err := canonicalIdentity(opts.Remote, opts.Identity, local)
	if err != nil {
		return nil, fmt.Errorf("publisher: %w", err)
	}
	if err := validateNamespaces(opts.Namespaces); err != nil {
		return nil, fmt.Errorf("publisher: %w", err)
	}
	format, err := git.ObjectFormat(ctx)
	if err != nil {
		return nil, fmt.Errorf("publisher: %w", err)
	}
	if opts.ObjectFormat != "" && opts.ObjectFormat != format {
		return nil, fmt.Errorf("publisher: destination object format %q does not match the local repository format %q", string(opts.ObjectFormat), string(format))
	}
	return &Publisher{
		git:        git,
		lister:     opts.Lister,
		remote:     opts.Remote,
		identity:   identity,
		namespaces: opts.Namespaces,
		format:     format,
	}, nil
}

// Identity reports the canonical destination recorded in manifests.
func (p *Publisher) Identity() string { return p.identity }

// ObjectFormat reports the hash algorithm object names are read in.
func (p *Publisher) ObjectFormat() gitcli.ObjectFormat { return p.format }

// validateNamespaces checks the non consumer ref layout.
//
// The rules mirror the ones the destination profile is validated against, and
// they are repeated rather than imported because this package is the last place
// the layout is used: a progress namespace that reached here under refs/heads/
// would put backfill chunks where the module proxy looks for branches, whatever
// validated it earlier.
func validateNamespaces(ns Namespaces) error {
	if ns.StateRef != "" {
		if err := gitcli.ValidateRefName(ns.StateRef); err != nil {
			return fmt.Errorf("state ref: %w", err)
		}
		if !strings.HasPrefix(ns.StateRef, branchPrefix) {
			return fmt.Errorf("state ref %q must live under %s", ns.StateRef, branchPrefix)
		}
	}
	if ns.ProgressPrefix == "" {
		return nil
	}
	trimmed := strings.TrimSuffix(ns.ProgressPrefix, "/")
	switch {
	case !strings.HasSuffix(ns.ProgressPrefix, "/"):
		return fmt.Errorf("progress namespace %q must end with a slash", ns.ProgressPrefix)
	case strings.HasPrefix(ns.ProgressPrefix, branchPrefix), strings.HasPrefix(ns.ProgressPrefix, tagPrefix):
		return fmt.Errorf("progress namespace %q must not shadow branches or tags", ns.ProgressPrefix)
	case !strings.HasPrefix(ns.ProgressPrefix, "refs/"):
		return fmt.Errorf("progress namespace %q must start with refs/", ns.ProgressPrefix)
	}
	if err := gitcli.ValidateRefName(trimmed); err != nil {
		return fmt.Errorf("progress namespace: %w", err)
	}
	return nil
}

// The ref namespaces a consumer and the module proxy can see.
const (
	branchPrefix = "refs/heads/"
	tagPrefix    = "refs/tags/"
)

// validateUpdates checks every requested update and rejects duplicates.
//
// Validation is complete before any remote is read, so a malformed request
// costs nothing and, more importantly, cannot reach a network call in a state
// where half of it has already been decided.
func (p *Publisher) validateUpdates(updates []Update) error {
	seen := make(map[string]bool, len(updates))
	for i, update := range updates {
		if err := p.validateUpdate(update); err != nil {
			return fmt.Errorf("update %d: %w", i, err)
		}
		if seen[update.Ref] {
			return fmt.Errorf("update %d: %q: %w", i, update.Ref, ErrDuplicateRef)
		}
		seen[update.Ref] = true
	}
	return checkRefConflicts(slices.Sorted(maps.Keys(seen)))
}

// checkRefConflicts rejects destinations that cannot coexist.
//
// Git stores a ref as a file at its own path, so refs/heads/main and
// refs/heads/main/next are a file and a directory of the same name and only one
// of them can exist. A plan holding both would pass every other check and then
// be rejected as a whole by the destination, which is a worse place to learn
// it: the caller would have an approved manifest describing a publication that
// can never happen.
//
// Sorted order puts a ref immediately before anything nested beneath it, so
// comparing neighbours finds every conflict.
func checkRefConflicts(sorted []string) error {
	for i := 1; i < len(sorted); i++ {
		if strings.HasPrefix(sorted[i], sorted[i-1]+"/") {
			return fmt.Errorf("%q and %q: %w", sorted[i-1], sorted[i], ErrConflictingRefs)
		}
	}
	return nil
}

// validateUpdate checks one requested update against its kind.
func (p *Publisher) validateUpdate(update Update) error {
	if strings.HasPrefix(update.Ref, "+") {
		return fmt.Errorf("%q: %w", update.Ref, ErrForceUpdate)
	}
	if err := gitcli.ValidateRefName(update.Ref); err != nil {
		return err
	}
	if !update.Kind.valid() {
		return fmt.Errorf("%q: unknown ref kind %q", update.Ref, string(update.Kind))
	}
	if err := p.checkNamespace(update); err != nil {
		return err
	}
	if update.NewObject == "" {
		return fmt.Errorf("%q: %w", update.Ref, ErrDeleteUpdate)
	}
	if err := p.validateObjectName("new object", update.NewObject); err != nil {
		return fmt.Errorf("%q: %w", update.Ref, err)
	}
	if update.ExpectAbsent && update.ExpectedOld != "" {
		return fmt.Errorf("%q: an update cannot both expect no ref and expect object %s", update.Ref, update.ExpectedOld)
	}
	if update.ExpectedOld != "" {
		if err := p.validateObjectName("expected object", update.ExpectedOld); err != nil {
			return fmt.Errorf("%q: %w", update.Ref, err)
		}
	}
	if err := validateEvidence(update.Evidence); err != nil {
		return fmt.Errorf("%q: %w", update.Ref, err)
	}
	return nil
}

// checkNamespace holds each kind to the namespace its readers watch.
//
// The state ref is checked against both directions. A branch update that
// happens to name it would publish bookkeeping as a consumer branch, and a
// state update that names anything else would write bookkeeping wherever it
// liked, so neither is a naming preference: they are the difference between a
// ref a module user sees and one they do not.
func (p *Publisher) checkNamespace(update Update) error {
	switch update.Kind {
	case KindBranch:
		if !strings.HasPrefix(update.Ref, branchPrefix) {
			return fmt.Errorf("branch %q must live under %s", update.Ref, branchPrefix)
		}
		if p.namespaces.StateRef != "" && update.Ref == p.namespaces.StateRef {
			return fmt.Errorf("branch %q is the state ref and must be published as one", update.Ref)
		}
	case KindTag:
		if !strings.HasPrefix(update.Ref, tagPrefix) {
			return fmt.Errorf("tag %q must live under %s", update.Ref, tagPrefix)
		}
	case KindState:
		if p.namespaces.StateRef == "" {
			return fmt.Errorf("state ref %q: the destination declares no state ref", update.Ref)
		}
		if update.Ref != p.namespaces.StateRef {
			return fmt.Errorf("state ref %q must be %q", update.Ref, p.namespaces.StateRef)
		}
	case KindProgress:
		if p.namespaces.ProgressPrefix == "" {
			return fmt.Errorf("progress ref %q: the destination declares no progress namespace", update.Ref)
		}
		if !strings.HasPrefix(update.Ref, p.namespaces.ProgressPrefix) {
			return fmt.Errorf("progress ref %q must live under %s", update.Ref, p.namespaces.ProgressPrefix)
		}
	}
	return nil
}

// validateObjectName checks one full object name in the destination's hash
// algorithm.
//
// The null object name is rejected by name rather than by length. It is how
// git's own protocol spells a deletion, so a caller that computed one from an
// empty result would otherwise hand this package a delete request that passed
// every other check.
func (p *Publisher) validateObjectName(kind, name string) error {
	width := p.format.HexLength()
	if len(name) != width {
		return fmt.Errorf("%s %q must be a full %s object name of %d characters", kind, name, string(p.format), width)
	}
	null := true
	for _, r := range name {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		default:
			return fmt.Errorf("%s %q must be lowercase hexadecimal", kind, name)
		}
		if r != '0' {
			null = false
		}
	}
	if null {
		return fmt.Errorf("%s is the null object name: %w", kind, ErrDeleteUpdate)
	}
	return nil
}

// validateEvidence checks a source label.
//
// The label is written into an artifact a person approves and a machine hashes,
// so it must be readable, stable, and incapable of carrying a location. A path
// or URL would break the first two by embedding a temporary directory and could
// break the third by embedding a credential.
func validateEvidence(evidence string) error {
	switch {
	case evidence == "":
		return errors.New("an evidence label is required")
	case strings.HasPrefix(evidence, "/"), strings.HasPrefix(evidence, "."):
		return fmt.Errorf("evidence %q must not be a path", evidence)
	case strings.Contains(evidence, "://"):
		return fmt.Errorf("evidence %q must not be a URL", evidence)
	case len(evidence) > maxEvidence:
		return fmt.Errorf("evidence label is %d characters, past the %d character limit", len(evidence), maxEvidence)
	}
	for _, r := range evidence {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("evidence %q must not contain control characters", evidence)
		}
	}
	return nil
}

// maxEvidence bounds one evidence label. A manifest is read by a person, and an
// unbounded label lets whatever produced it decide how much of the manifest is
// evidence and how much is the plan.
const maxEvidence = 200
