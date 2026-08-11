package gitcli

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
)

// SourceHost is the only host the engine may fetch upstream history from over
// the network. Source acquisition is anonymous, so the allowlist is not
// protecting a credential; it stops a profile from pointing the transformation
// at an attacker's copy of Kubernetes.
const SourceHost = "github.com"

// BloblessFilter is the partial clone filter the source cache uses. Commits and
// trees arrive eagerly because graph traversal needs them, while blobs are
// fetched only for the commits that are actually materialized.
const BloblessFilter = "blob:none"

// ErrCredentialedRunner reports an attempt to reach the source host with a
// runner that still carries caller supplied environment entries.
var ErrCredentialedRunner = errors.New("source commands require an anonymous runner")

// ErrFilterIgnored reports a transfer whose object filter the server did not
// honour, which yields a complete history where a blobless one was asked for.
var ErrFilterIgnored = errors.New("server ignored the object filter and sent a complete history")

// filterIgnoredWarning is what git writes to standard error when the server did
// not recognise the filter. It exits zero afterwards, so this string is the only
// contemporaneous evidence that the clone or fetch was silently upgraded to a
// full one. LC_ALL=C is fixed for every subprocess, so the message is not
// translated.
const filterIgnoredWarning = "filtering not recognized by server"

// anonymousConfig is prepended to every command that reaches the source host.
// The empty credential helper resets the helper list, so a repository local
// helper can neither be consulted nor prompt, and an unauthenticated fetch
// cannot become an authenticated one by inheriting configuration.
var anonymousConfig = []string{"-c", "credential.helper="}

// ValidateSourceRemote checks an anonymous source target. A network target must
// be an https URL on the one allowlisted host and may never embed user
// information. Absolute paths and file URLs stay available for local mirrors
// and tests. A named remote is rejected because its real target would live in
// repository configuration instead of in the reviewed profile.
func ValidateSourceRemote(remote string) error {
	if err := ValidateRemote(remote); err != nil {
		return err
	}
	if strings.Contains(remote, "://") {
		parsed, err := url.Parse(remote)
		if err != nil {
			return fmt.Errorf("remote %q is malformed: %s", redactRemote(remote), urlErrorReason(err))
		}
		if parsed.Scheme == "https" && parsed.Hostname() != SourceHost {
			return fmt.Errorf("remote %q must fetch source from %s", redactRemote(remote), SourceHost)
		}
		return nil
	}
	if filepath.IsAbs(remote) {
		return nil
	}
	return fmt.Errorf("remote %q must be an absolute path or an https URL, a named remote hides its target in configuration", redactRemote(remote))
}

// ValidateFetchRefspec checks one fetch refspec. Both ends must be explicit
// fully qualified refs: wildcards are rejected so a run can only ever update the
// refs its profile named, and a leading plus is rejected so that an upstream
// history rewrite fails the fetch instead of silently replacing the history the
// engine has already published from.
func ValidateFetchRefspec(spec string) error {
	switch {
	case spec == "":
		return errors.New("refspec must not be empty")
	case strings.HasPrefix(spec, "+"):
		return fmt.Errorf("refspec %q: %w", spec, ErrForceRefspec)
	case strings.HasPrefix(spec, "-"):
		return fmt.Errorf("refspec %q: %w", spec, ErrFlagLikeArgument)
	case strings.ContainsAny(spec, " \t\n\r"):
		return fmt.Errorf("refspec %q must not contain whitespace", spec)
	}
	source, destination, ok := strings.Cut(spec, ":")
	if !ok {
		return fmt.Errorf("refspec %q must be <source>:<destination>", spec)
	}
	if strings.Contains(destination, ":") {
		return fmt.Errorf("refspec %q must contain exactly one colon", spec)
	}
	if source == "" {
		return fmt.Errorf("refspec %q: %w", spec, ErrDeleteRefspec)
	}
	if destination == "" {
		return fmt.Errorf("refspec %q must name a destination ref", spec)
	}
	// ValidateRefName already rejects the glob metacharacters, so a wildcard
	// refspec cannot reach the subprocess.
	if err := ValidateRefName(source); err != nil {
		return fmt.Errorf("refspec source: %w", err)
	}
	if err := ValidateRefName(destination); err != nil {
		return fmt.Errorf("refspec destination: %w", err)
	}
	return nil
}

// assertAnonymous fails closed when a command that reaches the source host runs
// on a runner that could carry a credential.
func (r *Runner) assertAnonymous() error {
	if !r.anonymous {
		return ErrCredentialedRunner
	}
	return nil
}

// SourceCloneOptions describes one anonymous clone of the upstream repository.
type SourceCloneOptions struct {
	// Remote is the upstream repository, checked by ValidateSourceRemote.
	Remote string
	// Directory is the repository to create. It must be absolute so a cache
	// location never depends on the process working directory.
	Directory string
	// Filter is the partial clone filter. Empty means BloblessFilter.
	Filter string
	// Bare creates a repository with no work tree, which is what the reusable
	// cache wants because every materialization happens in its own worktree.
	Bare bool
	// NoCheckout leaves the work tree empty so a sparse pattern set can be
	// installed before any file is written. It is redundant when Bare is set.
	NoCheckout bool
}

// CloneSource creates the anonymous partial clone that backs the source cache.
func (r *Runner) CloneSource(ctx context.Context, opts SourceCloneOptions) error {
	if err := r.assertAnonymous(); err != nil {
		return fmt.Errorf("git clone: %w", err)
	}
	if err := ValidateSourceRemote(opts.Remote); err != nil {
		return fmt.Errorf("git clone: %w", err)
	}
	if err := validateArgument("clone directory", opts.Directory); err != nil {
		return fmt.Errorf("git clone: %w", err)
	}
	if !filepath.IsAbs(opts.Directory) {
		return fmt.Errorf("git clone: directory %q must be absolute", opts.Directory)
	}
	filter := opts.Filter
	if filter == "" {
		filter = BloblessFilter
	}
	if err := validateArgument("clone filter", filter); err != nil {
		return fmt.Errorf("git clone: %w", err)
	}
	// A clone reads no repository configuration of its own, but the directory it
	// runs in may sit inside one, and the cache it is about to create is the
	// repository every later source command trusts. The gate is applied here for
	// the same reason it is applied to a push: a rewrite must never be able to
	// decide which repository the engine transforms.
	if err := r.assertNoRemoteRewrites(ctx); err != nil {
		return fmt.Errorf("git clone: %w", err)
	}

	args := append([]string{}, anonymousConfig...)
	args = append(args, "clone", "--filter="+filter)
	switch {
	case opts.Bare:
		args = append(args, "--bare")
	case opts.NoCheckout:
		args = append(args, "--no-checkout")
	}
	args = append(args, "--", opts.Remote, opts.Directory)
	_, stderr, err := r.runCapture(ctx, nil, nil, args...)
	if err != nil {
		return fmt.Errorf("git clone from %q: %w", redactRemote(opts.Remote), err)
	}
	if strings.Contains(stderr, filterIgnoredWarning) {
		return fmt.Errorf("git clone from %q: filter %q: %w", redactRemote(opts.Remote), filter, ErrFilterIgnored)
	}
	return nil
}

// SourceFetchOptions describes one anonymous fetch into the source cache.
type SourceFetchOptions struct {
	// Remote is the upstream repository, checked by ValidateSourceRemote.
	Remote string
	// Refspecs are explicit <source>:<destination> pairs. Every pair is checked
	// by ValidateFetchRefspec.
	Refspecs []string
	// Tags additionally fetches the tags that point into the fetched history.
	// Git refuses to clobber an existing tag without a force refspec, so a
	// retagged upstream release fails the run rather than rewriting the cache.
	Tags bool
	// Filter is the partial clone filter. Empty means BloblessFilter.
	Filter string
}

// FetchSource updates the source cache from the upstream repository.
//
// The fetch is deliberately not quiet: git reports a rejected non fast forward
// update only in its ref summary, and that summary is the evidence that upstream
// rewrote history the engine may already have replayed.
func (r *Runner) FetchSource(ctx context.Context, opts SourceFetchOptions) error {
	if err := r.assertAnonymous(); err != nil {
		return fmt.Errorf("git fetch: %w", err)
	}
	if err := ValidateSourceRemote(opts.Remote); err != nil {
		return fmt.Errorf("git fetch: %w", err)
	}
	if len(opts.Refspecs) == 0 && !opts.Tags {
		return errors.New("git fetch: at least one refspec is required")
	}
	for _, spec := range opts.Refspecs {
		if err := ValidateFetchRefspec(spec); err != nil {
			return fmt.Errorf("git fetch: %w", err)
		}
	}
	filter := opts.Filter
	if filter == "" {
		filter = BloblessFilter
	}
	if err := validateArgument("fetch filter", filter); err != nil {
		return fmt.Errorf("git fetch: %w", err)
	}
	// The cache is on disk between runs, so its configuration is the one piece
	// of state an attacker who can write to the cache directory controls. A
	// rewrite there would send this fetch to their repository while every log
	// line still names the configured upstream.
	if err := r.assertNoRemoteRewrites(ctx); err != nil {
		return fmt.Errorf("git fetch: %w", err)
	}

	args := append([]string{}, anonymousConfig...)
	// FETCH_HEAD is not written because nothing reads it and its contents vary
	// with the order refs happen to arrive in.
	args = append(args, "fetch", "--no-progress", "--no-write-fetch-head", "--filter="+filter)
	if opts.Tags {
		args = append(args, "--tags")
	}
	args = append(args, "--end-of-options", opts.Remote)
	args = append(args, opts.Refspecs...)
	_, stderr, err := r.runCapture(ctx, nil, nil, args...)
	if err != nil {
		// The per ref verdict is the evidence that matters here, and it is never
		// the first line of the output, so it has to be lifted out explicitly.
		if verdict := rejectedFetchRef(err); verdict != "" {
			return fmt.Errorf("git fetch from %q: %s: %w", redactRemote(opts.Remote), verdict, err)
		}
		return fmt.Errorf("git fetch from %q: %w", redactRemote(opts.Remote), err)
	}
	if strings.Contains(stderr, filterIgnoredWarning) {
		return fmt.Errorf("git fetch from %q: filter %q: %w", redactRemote(opts.Remote), filter, ErrFilterIgnored)
	}
	return nil
}

// rejectedFetchRef returns the first rejected ref line of a fetch's ref summary.
// The summary is written to standard error and its first line names only the
// remote, so the reason a fetch failed would otherwise be dropped.
func rejectedFetchRef(err error) string {
	var execErr *ExecError
	if !errors.As(err, &execErr) {
		return ""
	}
	for line := range strings.SplitSeq(execErr.Stderr, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "!") {
			continue
		}
		trimmed = strings.TrimSpace(strings.TrimPrefix(trimmed, "!"))
		return strings.Join(strings.Fields(trimmed), " ")
	}
	return ""
}

// ConfigEntry is one configuration key and the scope it was read from.
type ConfigEntry struct {
	// Scope is git's own name for the file the key came from, such as local or
	// worktree.
	Scope string
	// Key is the fully qualified configuration key, lower cased by git except
	// for a subsection name, which keeps its case.
	Key string
}

// RepositoryScopes are the configuration scopes a repository itself owns.
// Global and system configuration are neutralised for every subprocess, so
// these two are the only ones an attacker who can write to a repository
// directory controls, and the second is invisible to a --local query.
var RepositoryScopes = []string{"local", "worktree"}

// ConfigKeys reports every configuration key the repository itself defines,
// with the scope each came from, ordered by scope and then key.
//
// The listing is what makes a provenance audit possible: reading the handful of
// keys a caller expects would say nothing about the keys it did not think to
// ask about, and a repository restored from an untrusted cache can carry any of
// them.
func (r *Runner) ConfigKeys(ctx context.Context) ([]ConfigEntry, error) {
	out, err := r.run(ctx, "config", "--list", "--show-scope", "--name-only", "-z")
	if err != nil {
		// A directory that holds no repository has no repository scoped
		// configuration, which is an answer rather than a failure.
		var execErr *ExecError
		if errors.As(err, &execErr) && strings.Contains(execErr.Stderr, notARepository) {
			return nil, nil
		}
		return nil, fmt.Errorf("git config listing: %w", err)
	}

	records := splitNull(out)
	if len(records)%2 != 0 {
		return nil, fmt.Errorf("git config listing: got %d records, want scope and key pairs", len(records))
	}
	entries := make([]ConfigEntry, 0, len(records)/2)
	for i := 0; i < len(records); i += 2 {
		if !slices.Contains(RepositoryScopes, records[i]) {
			continue
		}
		entries = append(entries, ConfigEntry{Scope: records[i], Key: records[i+1]})
	}
	slices.SortFunc(entries, func(a, b ConfigEntry) int {
		if a.Scope != b.Scope {
			return strings.Compare(a.Scope, b.Scope)
		}
		return strings.Compare(a.Key, b.Key)
	})
	return slices.Compact(entries), nil
}

// ConfigEffective reads the value git would actually use for one key.
//
// It is deliberately not scoped. A cache audit has to know what the next
// command will do, and a worktree scoped key silently outranks the local one a
// --local read would have returned.
func (r *Runner) ConfigEffective(ctx context.Context, key string) (value string, found bool, err error) {
	if err := validateArgument("config key", key); err != nil {
		return "", false, fmt.Errorf("git config: %w", err)
	}
	out, err := r.run(ctx, "config", "--get", "--end-of-options", key)
	if err != nil {
		if ExitCodeOf(err) == 1 {
			return "", false, nil
		}
		return "", false, fmt.Errorf("git config %s: %w", r.redactor.String(key), err)
	}
	return strings.TrimRight(out, "\n"), true, nil
}

// Ref is one reference in the local repository.
type Ref struct {
	// Name is the fully qualified ref name.
	Name string
	// Target is the object the ref points at, which is the tag object itself for
	// an annotated tag.
	Target string
	// Type is the target's object type, such as commit or tag.
	Type string
	// Commit is the commit the ref ultimately resolves to. For an annotated tag
	// it is the peeled commit, and for every other ref it equals Target.
	Commit string
}

// Annotated reports whether the ref points at a tag object rather than straight
// at a commit.
func (r Ref) Annotated() bool { return r.Type == "tag" }

// refFormat requests the fields of one ref separated by null bytes. A ref name
// can contain neither a null byte nor a newline, so the record and field
// separators are unambiguous.
const refFormat = "%(objectname)%00%(refname)%00%(objecttype)%00%(*objectname)"

// refFieldCount is the number of fields refFormat produces.
const refFieldCount = 4

// ListRefs reports the refs matching the given patterns, or every ref when no
// pattern is given. Results keep git's ref name ordering, so repeated runs
// observe the same sequence.
func (r *Runner) ListRefs(ctx context.Context, patterns ...string) ([]Ref, error) {
	for _, pattern := range patterns {
		if err := validateArgument("ref pattern", pattern); err != nil {
			return nil, fmt.Errorf("git ref list: %w", err)
		}
	}
	args := []string{"for-each-ref", "--format=" + refFormat, "--end-of-options"}
	args = append(args, patterns...)
	out, err := r.run(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("git ref list: %w", err)
	}

	var refs []Ref
	for line := range strings.SplitSeq(out, "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\x00")
		if len(fields) != refFieldCount {
			return nil, fmt.Errorf("git ref list: got %d fields, want %d", len(fields), refFieldCount)
		}
		ref := Ref{Name: fields[1], Target: fields[0], Type: fields[2], Commit: fields[3]}
		if ref.Commit == "" {
			ref.Commit = ref.Target
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

// HasRef reports whether one fully qualified ref exists. A missing ref is a
// supported state rather than an error, because a profile may name a release
// branch that upstream has not created yet.
func (r *Runner) HasRef(ctx context.Context, name string) (bool, error) {
	if err := ValidateRefName(name); err != nil {
		return false, fmt.Errorf("git ref probe: %w", err)
	}
	_, err := r.run(ctx, "show-ref", "--verify", "--quiet", "--", name)
	if err == nil {
		return true, nil
	}
	if ExitCodeOf(err) == 1 {
		return false, nil
	}
	return false, fmt.Errorf("git ref probe for %q: %w", name, err)
}

// IsBareRepository reports whether the working directory is a repository with no
// work tree. A directory that holds no repository at all is a supported state
// and reports false, while a repository that cannot be read is an error, which
// is what keeps a corrupt cache from being silently re-created.
//
// Discovery is pinned to the working directory: the answer describes that exact
// path and never a repository above it. Without the pin, probing a directory
// that is not a repository would report on whatever repository contains it, and
// a cache directory that had been emptied would present itself as a usable
// cache belonging to somebody else.
func (r *Runner) IsBareRepository(ctx context.Context) (bool, error) {
	if r.dir == "" {
		return false, errors.New("git bare repository probe: the runner has no working directory")
	}
	return r.IsBareRepositoryAt(ctx, r.dir)
}

// IsBareRepositoryAt reports whether dir itself is a bare repository.
//
// The git directory is stated rather than discovered, so a directory that is not
// a repository is reported as one rather than resolving to an ancestor, and the
// path git actually opened is compared with the one that was asked about.
func (r *Runner) IsBareRepositoryAt(ctx context.Context, dir string) (bool, error) {
	if err := validateAbsolutePath("repository path", dir); err != nil {
		return false, fmt.Errorf("git bare repository probe: %w", err)
	}
	out, err := r.run(ctx, "--git-dir="+dir, "rev-parse", "--is-bare-repository", "--absolute-git-dir")
	if err != nil {
		var execErr *ExecError
		if errors.As(err, &execErr) && strings.Contains(execErr.Stderr, notARepositoryPinned) {
			return false, nil
		}
		return false, fmt.Errorf("git bare repository probe: %w", err)
	}
	fields := strings.Fields(out)
	if len(fields) != 2 {
		return false, fmt.Errorf("git bare repository probe: unexpected output %q", firstLine(out))
	}
	if !sameFile(fields[1], dir) {
		return false, fmt.Errorf("git bare repository probe: %q resolved to repository %q", dir, fields[1])
	}
	return fields[0] == "true", nil
}

// notARepositoryPinned is the prefix git uses when a stated git directory holds
// no repository. It differs from the discovery message, which names the parents
// it searched.
const notARepositoryPinned = "not a git repository: "

// PartialCloneStatus describes how completely a repository holds its blobs.
type PartialCloneStatus int

// The partial clone verdicts.
const (
	// PartialCloneUnknown reports a tree with no blob in it, which proves
	// nothing either way.
	PartialCloneUnknown PartialCloneStatus = iota
	// PartialCloneConfirmed reports blobs that the repository does not hold, so
	// the filter was applied.
	PartialCloneConfirmed
	// PartialCloneFull reports that every blob is present, so the repository
	// holds a complete history whatever its configuration claims.
	PartialCloneFull
)

// blobProbeLimit bounds how many of a commit's blobs the partial clone probe
// asks about. One absent blob already proves the filter was applied, and a
// bound keeps the probe from rendering the whole tree of a repository the size
// of Kubernetes.
const blobProbeLimit = 64

// PartialCloneStatusOf reports whether the repository really omits the blobs of
// one commit.
//
// Configuration is not evidence here. A server that does not honour --filter
// makes git warn, exit zero, and record remote.origin.promisor and
// remote.origin.partialclonefilter anyway, so a repository that claims to be a
// blobless partial clone and holds every blob is exactly what a silent
// degradation looks like.
//
// The blobs of the commit are enumerated with the same filter that was
// requested and then probed locally with lazy fetching disabled, so the answer
// describes what is on disk and the probe itself cannot download what it asks
// about.
func (r *Runner) PartialCloneStatusOf(ctx context.Context, revision string) (PartialCloneStatus, error) {
	if err := validateRevision(revision); err != nil {
		return PartialCloneUnknown, fmt.Errorf("git partial clone probe: %w", err)
	}
	out, err := r.runWith(ctx, []string{noLazyFetch},
		"rev-list", "--objects", "--no-object-names", "--filter="+BloblessFilter, "--filter-print-omitted",
		"--max-count=1", "--end-of-options", revision, "--")
	if err != nil {
		return PartialCloneUnknown, fmt.Errorf("git partial clone probe: %w", err)
	}

	blobs := make([]string, 0, blobProbeLimit)
	for line := range strings.SplitSeq(out, "\n") {
		// The filter marks the objects it left out with a tilde, which for
		// blob:none is exactly the blobs reachable from the commit's tree.
		omitted, ok := strings.CutPrefix(strings.TrimSpace(line), "~")
		if !ok || omitted == "" {
			continue
		}
		blobs = append(blobs, omitted)
		if len(blobs) == blobProbeLimit {
			break
		}
	}
	if len(blobs) == 0 {
		return PartialCloneUnknown, nil
	}

	infos, err := r.ObjectInfoBatch(ctx, ObjectInfoOptions{Revisions: blobs})
	if err != nil {
		return PartialCloneUnknown, fmt.Errorf("git partial clone probe: %w", err)
	}
	for _, info := range infos {
		if info.Missing {
			return PartialCloneConfirmed, nil
		}
	}
	return PartialCloneFull, nil
}
