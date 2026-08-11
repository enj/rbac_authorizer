// Package source maintains the reusable blobless clone of the upstream
// repository and materializes isolated sparse work trees from it.
//
// The cache is a bare partial clone created with --filter=blob:none. Commits and
// trees arrive eagerly because graph discovery needs them, while blobs are
// downloaded only for the commits a run actually materializes, which is what
// makes a repository the size of Kubernetes practical to track. The cache is
// reused across runs, so acquisition after the first is an explicit fetch of the
// refs the profile names.
//
// Every command here is anonymous. Source history is public, the remote is
// checked against the allowlist in gitcli, and the runner is stripped of caller
// supplied environment entries before the first subprocess starts, so a
// credential that exists for publishing can never travel to the source host.
//
// Nothing in this package writes to a remote.
package source

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/enj/soapbox/tools/internal/config"
	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/gitgraph"
)

// Ref namespaces. Cached refs keep the upstream names so the cache stays
// readable with ordinary git commands during an investigation.
const (
	branchPrefix = "refs/heads/"
	tagPrefix    = "refs/tags/"
)

// worktreePrefix names a materialized work tree directory. The prefix keeps a
// generated directory recognisable in a shared temporary root.
const worktreePrefix = "wt-"

// shortSHALength is how much of a commit name a derived directory name carries.
// It is long enough to stay unique in one run and short enough to keep paths
// well below the platform limit.
const shortSHALength = 12

// Kind distinguishes the two ref namespaces a profile may track.
type Kind string

// The tracked ref kinds.
const (
	KindBranch Kind = "branch"
	KindTag    Kind = "tag"
)

// Revision is one resolved upstream ref.
type Revision struct {
	// Name is the short ref name, such as master or v1.36.1.
	Name string
	// Ref is the fully qualified ref name.
	Ref string
	// Kind reports which namespace the ref lives in.
	Kind Kind
	// Object is what the ref points at, which is the tag object itself for an
	// annotated tag.
	Object string
	// Commit is the commit the ref resolves to, with annotated tags peeled.
	Commit string
	// Annotated reports a tag object rather than a direct commit reference.
	// Release tags are expected to be annotated, because their tagger date and
	// message become part of the generated release.
	Annotated bool
}

// Refs names the upstream refs a run tracks.
type Refs struct {
	// Branches are short branch names such as master or release-1.36.
	Branches []string
	// Tags are short tag names such as v1.36.1.
	Tags []string
	// AllTags additionally fetches every tag reachable in the fetched history.
	// It is how newly published upstream releases are discovered, and it is
	// separate from Tags so that a run can be pinned to exactly the tags its
	// profile named.
	AllTags bool
}

// Options configures a source cache.
type Options struct {
	// Remote is the upstream repository, checked by gitcli.ValidateSourceRemote.
	Remote string
	// CacheRoot is the caller owned directory that holds bare caches. It is
	// created when it does not exist.
	CacheRoot string
	// WorktreeRoot is the caller owned directory that holds materialized work
	// trees. It is created when it does not exist, and it may be a temporary
	// directory that the caller removes wholesale.
	WorktreeRoot string
	// Directory overrides the cache directory name below CacheRoot. Empty
	// derives a stable name from the remote.
	Directory string
	// Git is the runner the cache drives. It is made anonymous before use, so
	// passing the publishing runner cannot leak a credential to the source host.
	Git *gitcli.Runner
}

// Cache is a reusable bare partial clone of the upstream repository.
type Cache struct {
	remote       string
	dir          string
	worktreeRoot string
	git          *gitcli.Runner
	created      bool
}

// Open returns the cache for a remote, cloning it when it does not exist yet and
// reusing it when it does.
//
// A directory that exists but is not a bare repository is an error rather than
// something to overwrite: it is either a partially written cache or a path the
// caller did not mean to hand over, and both deserve a human.
//
// A cache that does exist is audited before it is used. It is the one piece of
// engine state that survives between runs and the one an attacker who can write
// to a build cache controls, so being a bare repository at the right path is not
// enough: it also has to be a repository this configuration would have created.
func Open(ctx context.Context, opts Options) (*Cache, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("source cache: %w", err)
	}
	if err := gitcli.ValidateSourceRemote(opts.Remote); err != nil {
		return nil, fmt.Errorf("source cache: %w", err)
	}
	if opts.Git == nil {
		return nil, errors.New("source cache: a git runner is required")
	}
	if err := validateRoot("cache root", opts.CacheRoot); err != nil {
		return nil, fmt.Errorf("source cache: %w", err)
	}
	if err := validateRoot("work tree root", opts.WorktreeRoot); err != nil {
		return nil, fmt.Errorf("source cache: %w", err)
	}

	name := opts.Directory
	if name == "" {
		name = cacheDirName(opts.Remote)
	}
	if err := config.ValidateRelPath(name); err != nil {
		return nil, fmt.Errorf("source cache directory %q: %w", name, err)
	}
	if strings.Contains(name, "/") {
		return nil, fmt.Errorf("source cache directory %q must be a single element", name)
	}
	if err := os.MkdirAll(opts.CacheRoot, 0o750); err != nil {
		return nil, fmt.Errorf("source cache root: %w", err)
	}
	if err := os.MkdirAll(opts.WorktreeRoot, 0o750); err != nil {
		return nil, fmt.Errorf("source work tree root: %w", err)
	}
	dir, err := config.SafeJoin(ctx, opts.CacheRoot, name)
	if err != nil {
		return nil, fmt.Errorf("source cache: %w", err)
	}
	worktreeRoot, err := config.SafeJoin(ctx, filepath.Dir(opts.WorktreeRoot), filepath.Base(opts.WorktreeRoot))
	if err != nil {
		return nil, fmt.Errorf("source work tree root: %w", err)
	}

	git, err := opts.Git.Anonymous().WithDir(opts.CacheRoot)
	if err != nil {
		return nil, fmt.Errorf("source cache: %w", err)
	}
	// Every capability the cache depends on has to be present before the first
	// command that would silently do the wrong thing without it.
	if err := git.RequireMinimumVersion(ctx); err != nil {
		return nil, fmt.Errorf("source cache: %w", err)
	}

	cache := &Cache{
		remote:       opts.Remote,
		dir:          dir,
		worktreeRoot: worktreeRoot,
		git:          git,
	}
	if err := cache.ensure(ctx); err != nil {
		return nil, err
	}
	if cache.git, err = cache.git.WithDir(dir); err != nil {
		return nil, fmt.Errorf("source cache: %w", err)
	}
	if err := cache.audit(ctx); err != nil {
		return nil, fmt.Errorf("source cache %q: %w", dir, err)
	}
	return cache, nil
}

// validateRoot checks a caller owned root directory. An absolute path is
// required because a cache or work tree that resolved against the process
// working directory would be a different directory depending on where the
// engine happened to be started, and a run must not be able to adopt whatever
// repository is sitting below the shell's current location.
func validateRoot(kind, root string) error {
	switch {
	case root == "":
		return fmt.Errorf("a %s is required", kind)
	case !filepath.IsAbs(root):
		return fmt.Errorf("%s %q must be absolute", kind, root)
	case filepath.Clean(root) != root:
		return fmt.Errorf("%s %q must be in clean form", kind, root)
	}
	return nil
}

// ensure clones the cache when it is absent and checks it when it is present.
func (c *Cache) ensure(ctx context.Context) error {
	switch info, err := os.Stat(c.dir); {
	case err == nil && !info.IsDir():
		return fmt.Errorf("source cache %q is not a directory", c.dir)
	case err == nil:
		// The probe names the git directory rather than letting git discover
		// one, so a cache directory that lost its repository cannot answer with
		// the repository that happens to contain it.
		bare, err := c.git.IsBareRepositoryAt(ctx, c.dir)
		if err != nil {
			return fmt.Errorf("source cache %q: %w", c.dir, err)
		}
		if !bare {
			return fmt.Errorf("source cache %q exists but is not a bare repository", c.dir)
		}
		return nil
	case !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("source cache %q: %w", c.dir, err)
	}

	if err := c.git.CloneSource(ctx, gitcli.SourceCloneOptions{
		Remote:    c.remote,
		Directory: c.dir,
		Filter:    gitcli.BloblessFilter,
		Bare:      true,
	}); err != nil {
		return fmt.Errorf("source cache: %w", err)
	}
	c.created = true
	return nil
}

// Configuration a cache legitimately carries. Everything git itself writes when
// it creates a bare blobless clone, plus what enabling per work tree
// configuration adds, and nothing that can redirect a transfer, run a command,
// or change what a checkout produces.
//
// The check is an allowlist rather than a list of dangerous keys because the
// engine creates this repository itself: any key it did not put there is
// evidence, and a denylist would only ever cover the redirections somebody
// already thought of.
var allowedCacheConfig = map[string]bool{
	"core.repositoryformatversion":     true,
	"core.filemode":                    true,
	"core.bare":                        true,
	"core.ignorecase":                  true,
	"core.precomposeunicode":           true,
	"core.symlinks":                    true,
	"core.logallrefupdates":            true,
	"extensions.objectformat":          true,
	"extensions.refstorage":            true,
	"extensions.worktreeconfig":        true,
	"extensions.partialclone":          true,
	"remote.origin.url":                true,
	"remote.origin.fetch":              true,
	"remote.origin.promisor":           true,
	"remote.origin.partialclonefilter": true,
}

// Cache configuration that proves provenance rather than merely being harmless.
const (
	originURLKey    = "remote.origin.url"
	promisorKey     = "remote.origin.promisor"
	cloneFilterKey  = "remote.origin.partialclonefilter"
	promisorEnabled = "true"
)

// audit proves the cache on disk is the one this configuration describes.
//
// A restored or tampered cache passes every ordinary check: it is a bare
// repository, it holds the ref names the profile asks for, and its objects
// resolve. What it can also carry is a rewrite that sends the next fetch
// somewhere else, an origin pointing at a different repository, or a filter
// setting that never took effect. Each of those turns a run into a replay of
// somebody else's history under this engine's provenance trailers, so the cache
// is checked before it is fetched into, materialized from, or lazily fetched
// through.
func (c *Cache) audit(ctx context.Context) error {
	entries, err := c.git.ConfigKeys(ctx)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !allowedCacheConfig[strings.ToLower(entry.Key)] {
			return fmt.Errorf("%s configuration %q is not part of a generated cache", entry.Scope, entry.Key)
		}
	}

	origin, found, err := c.git.ConfigEffective(ctx, originURLKey)
	if err != nil {
		return err
	}
	if !found || origin != c.remote {
		return fmt.Errorf("%s is %q, not the configured source %q", originURLKey, origin, c.remote)
	}

	// The partial clone settings are checked because a cache that lost them
	// would download every blob of the upstream history on the next
	// materialization, and because their presence is what makes the promisor
	// remote the one just verified.
	promisor, found, err := c.git.ConfigEffective(ctx, promisorKey)
	if err != nil {
		return err
	}
	if !found || promisor != promisorEnabled {
		return fmt.Errorf("%s is %q, so the cache is not a partial clone", promisorKey, promisor)
	}
	filter, found, err := c.git.ConfigEffective(ctx, cloneFilterKey)
	if err != nil {
		return err
	}
	if !found || filter != gitcli.BloblessFilter {
		return fmt.Errorf("%s is %q, not the requested %q", cloneFilterKey, filter, gitcli.BloblessFilter)
	}

	if c.created {
		return c.assertPartialClone(ctx)
	}
	return nil
}

// assertPartialClone proves the filter was applied rather than merely recorded.
//
// A server that does not support object filtering makes git warn, exit zero,
// and write the promisor settings anyway, so configuration alone cannot tell a
// blobless clone from a complete one. The check runs against the freshly cloned
// cache, where nothing has been materialized yet and every blob is therefore
// still expected to be absent.
func (c *Cache) assertPartialClone(ctx context.Context) error {
	head, err := c.git.HasHead(ctx)
	if err != nil {
		return err
	}
	if !head {
		// An upstream with no commits has no blobs to have omitted.
		return nil
	}
	status, err := c.git.PartialCloneStatusOf(ctx, "HEAD")
	if err != nil {
		return err
	}
	if status == gitcli.PartialCloneFull {
		return fmt.Errorf("clone holds every blob of HEAD: %w", gitcli.ErrFilterIgnored)
	}
	return nil
}

// Path reports the cache directory.
func (c *Cache) Path() string { return c.dir }

// Remote reports the upstream repository the cache tracks.
func (c *Cache) Remote() string { return c.remote }

// Created reports whether this call cloned the cache rather than reusing one.
func (c *Cache) Created() bool { return c.created }

// Git reports the anonymous runner scoped to the cache directory.
func (c *Cache) Git() *gitcli.Runner { return c.git }

// WorktreeRoot reports the directory materialized work trees are created under.
func (c *Cache) WorktreeRoot() string { return c.worktreeRoot }

// Fetch updates the cache with the refs a profile names.
//
// Refspecs are explicit and never forced, so an upstream branch that was rewound
// or a tag that was moved fails the fetch instead of quietly replacing history
// the engine may already have published from.
func (c *Cache) Fetch(ctx context.Context, refs Refs) error {
	refspecs, err := refspecsFor(refs)
	if err != nil {
		return fmt.Errorf("source fetch: %w", err)
	}
	if len(refspecs) == 0 && !refs.AllTags {
		return errors.New("source fetch: at least one branch or tag is required")
	}
	if err := c.git.FetchSource(ctx, gitcli.SourceFetchOptions{
		Remote:   c.remote,
		Refspecs: refspecs,
		Tags:     refs.AllTags,
		Filter:   gitcli.BloblessFilter,
	}); err != nil {
		return fmt.Errorf("source fetch: %w", err)
	}
	return nil
}

// refspecsFor renders one explicit refspec per configured ref.
func refspecsFor(refs Refs) ([]string, error) {
	specs := make([]string, 0, len(refs.Branches)+len(refs.Tags))
	for _, branch := range refs.Branches {
		ref, err := branchRef(branch)
		if err != nil {
			return nil, err
		}
		specs = append(specs, ref+":"+ref)
	}
	for _, tag := range refs.Tags {
		ref, err := tagRef(tag)
		if err != nil {
			return nil, err
		}
		specs = append(specs, ref+":"+ref)
	}
	return specs, nil
}

// Resolve reports the resolved metadata of every configured ref and fails when
// any of them is missing, unreadable, or does not resolve to a commit that the
// cache actually holds.
//
// Validation is the point of this call. A profile that names a branch upstream
// deleted, or a tag whose object never arrived, must stop the run here rather
// than produce a partial replay.
func (c *Cache) Resolve(ctx context.Context, refs Refs) ([]Revision, error) {
	wanted := make([]Revision, 0, len(refs.Branches)+len(refs.Tags))
	patterns := make([]string, 0, cap(wanted))
	for _, branch := range refs.Branches {
		ref, err := branchRef(branch)
		if err != nil {
			return nil, fmt.Errorf("source refs: %w", err)
		}
		wanted = append(wanted, Revision{Name: branch, Ref: ref, Kind: KindBranch})
		patterns = append(patterns, ref)
	}
	for _, tag := range refs.Tags {
		ref, err := tagRef(tag)
		if err != nil {
			return nil, fmt.Errorf("source refs: %w", err)
		}
		wanted = append(wanted, Revision{Name: tag, Ref: ref, Kind: KindTag})
		patterns = append(patterns, ref)
	}
	if len(wanted) == 0 {
		return nil, nil
	}

	found, err := c.git.ListRefs(ctx, patterns...)
	if err != nil {
		return nil, fmt.Errorf("source refs: %w", err)
	}
	byName := make(map[string]gitcli.Ref, len(found))
	for _, ref := range found {
		byName[ref.Name] = ref
	}

	resolved := make([]Revision, 0, len(wanted))
	commits := make([]string, 0, len(wanted))
	for _, revision := range wanted {
		ref, ok := byName[revision.Ref]
		if !ok {
			return nil, fmt.Errorf("source refs: %s %q is missing from the cache", revision.Kind, revision.Name)
		}
		revision.Object = ref.Target
		revision.Commit = ref.Commit
		revision.Annotated = ref.Annotated()
		resolved = append(resolved, revision)
		commits = append(commits, revision.Commit)
	}

	// One batched probe answers whether every referenced commit is present
	// locally. Lazy fetching stays off so a ref pointing at an object the cache
	// never received is reported instead of silently downloaded.
	infos, err := c.git.ObjectInfoBatch(ctx, gitcli.ObjectInfoOptions{Revisions: commits})
	if err != nil {
		return nil, fmt.Errorf("source refs: %w", err)
	}
	for i, info := range infos {
		switch {
		case info.Missing:
			return nil, fmt.Errorf("source refs: %s %q points at missing commit %s", resolved[i].Kind, resolved[i].Name, resolved[i].Commit)
		case info.Type != "commit":
			return nil, fmt.Errorf("source refs: %s %q resolves to a %s, not a commit", resolved[i].Kind, resolved[i].Name, info.Type)
		}
	}
	return resolved, nil
}

// ListBranches reports every cached branch, ordered by ref name.
func (c *Cache) ListBranches(ctx context.Context) ([]Revision, error) {
	return c.list(ctx, KindBranch, branchPrefix)
}

// ListTags reports every cached tag, ordered by ref name.
func (c *Cache) ListTags(ctx context.Context) ([]Revision, error) {
	return c.list(ctx, KindTag, tagPrefix)
}

// list reports the cached refs of one namespace.
//
// The pattern is the bare prefix rather than a trailing star, because a star
// does not cross a slash: matching against refs/heads/* would silently skip a
// branch named release/1.36 and report an incomplete ref set.
func (c *Cache) list(ctx context.Context, kind Kind, prefix string) ([]Revision, error) {
	refs, err := c.git.ListRefs(ctx, prefix)
	if err != nil {
		return nil, fmt.Errorf("source %s list: %w", kind, err)
	}
	revisions := make([]Revision, 0, len(refs))
	for _, ref := range refs {
		revisions = append(revisions, Revision{
			Name:      strings.TrimPrefix(ref.Name, prefix),
			Ref:       ref.Name,
			Kind:      kind,
			Object:    ref.Target,
			Commit:    ref.Commit,
			Annotated: ref.Annotated(),
		})
	}
	return revisions, nil
}

// Anchor reports the common ancestor of every given revision, which is the
// commit the transformed history is rooted at. The engine records it so that
// later ref discovery cannot move the base of published history.
//
// More than one best common ancestor, as a criss-cross merge produces, is
// refused rather than resolved. Git's own octopus mode prints one of them and
// gives no sign that it chose, so accepting its answer would make the base of
// published history depend on traversal order; the pure graph in gitgraph
// already refuses the same shape, and the two must not disagree about what a
// valid anchor is.
func (c *Cache) Anchor(ctx context.Context, revisions []string) (string, error) {
	if len(revisions) == 0 {
		return "", errors.New("source anchor: at least one revision is required")
	}
	commits := make([]string, 0, len(revisions))
	for _, revision := range revisions {
		commit, err := c.git.ResolveCommit(ctx, revision)
		if err != nil {
			return "", fmt.Errorf("source anchor: %w", err)
		}
		commits = append(commits, commit)
	}
	if len(commits) == 1 {
		return commits[0], nil
	}

	bases, err := c.git.MergeBasesOctopus(ctx, commits...)
	if err != nil {
		if errors.Is(err, gitcli.ErrNoMergeBase) {
			return "", fmt.Errorf("source anchor: %w", gitgraph.ErrNoCommonAnchor)
		}
		return "", fmt.Errorf("source anchor: %w", err)
	}
	if len(bases) != 1 {
		return "", fmt.Errorf("source anchor: %s: %w", strings.Join(bases, ", "), gitgraph.ErrAmbiguousAnchor)
	}

	// The octopus reduction answers for the set as a whole, so the result is
	// confirmed against each revision individually. A base that is not an
	// ancestor of every ref would root published history at a commit some of it
	// does not descend from.
	anchor := bases[0]
	for i, commit := range commits {
		ancestor, err := c.git.IsAncestor(ctx, anchor, commit)
		if err != nil {
			return "", fmt.Errorf("source anchor: %w", err)
		}
		if !ancestor {
			return "", fmt.Errorf("source anchor: %s is not an ancestor of %q", anchor, revisions[i])
		}
	}
	return anchor, nil
}

// GraphOptions selects the commits a graph covers.
type GraphOptions struct {
	// Heads are the revisions to walk from.
	Heads []string
	// Anchor bounds the walk from below. The anchor itself is included as the
	// root of the graph and its own ancestors are left out, because history
	// before the anchor is never transformed. Empty walks to the root commit.
	Anchor string
	// FirstParent follows only first parents, which yields each head's mainline.
	//
	// The resulting graph knows that a merge on the mainline has a second parent
	// but nothing about the history behind it, so it is built as a first parent
	// graph and refuses to shape a merge rather than quietly reporting the merge
	// as having one parent.
	FirstParent bool
}

// Graph reads commit and parent metadata and returns the DAG the replay phase
// traverses. Only object names and parent edges are read, so the traversal never
// depends on commit messages or dates.
func (c *Cache) Graph(ctx context.Context, opts GraphOptions) (*gitgraph.Graph, error) {
	if len(opts.Heads) == 0 {
		return nil, errors.New("source graph: at least one head is required")
	}
	revList := gitcli.RevListOptions{Include: opts.Heads, FirstParent: opts.FirstParent}

	var anchor string
	if opts.Anchor != "" {
		resolved, err := c.git.ResolveCommit(ctx, opts.Anchor)
		if err != nil {
			return nil, fmt.Errorf("source graph anchor: %w", err)
		}
		anchor = resolved
		// Excluding the anchor also excludes its ancestors, so the anchor node
		// itself is added back below and keeps its parents as boundary edges.
		revList.Exclude = []string{anchor}
	}

	dag, err := c.git.CommitGraph(ctx, revList)
	if err != nil {
		return nil, fmt.Errorf("source graph: %w", err)
	}

	commits := make([]gitgraph.Commit, 0, len(dag)+1)
	if anchor != "" {
		parents, err := c.git.CommitParents(ctx, anchor)
		if err != nil {
			return nil, fmt.Errorf("source graph anchor: %w", err)
		}
		commits = append(commits, gitgraph.Commit{SHA: anchor, Parents: parents})
	}
	for _, commit := range dag {
		commits = append(commits, gitgraph.Commit{SHA: commit.SHA, Parents: commit.Parents})
	}

	build := gitgraph.New
	if opts.FirstParent {
		build = gitgraph.NewFirstParent
	}
	graph, err := build(commits)
	if err != nil {
		return nil, fmt.Errorf("source graph: %w", err)
	}
	return graph, nil
}

// WorktreeOptions describes one materialized work tree.
type WorktreeOptions struct {
	// Commit is the revision to materialize.
	Commit string
	// Name overrides the directory name below the work tree root. Empty derives
	// a name from the resolved commit.
	Name string
	// Patterns are sparse patterns. Empty materializes the whole tree, which for
	// a blobless clone means downloading every blob in it.
	Patterns []string
	// Cone selects cone mode, which matches faster but always includes
	// subdirectories. Package granularity needs pattern mode.
	Cone bool
}

// Worktree is one isolated materialization of a source commit.
type Worktree struct {
	path   string
	commit string
	git    *gitcli.Runner
	cache  *Cache
}

// AddWorktree materializes one commit in its own work tree.
//
// The work tree is created empty, the sparse pattern set is installed, and only
// then is the commit checked out, so a blobless clone fetches blobs for the
// selected paths instead of the entire tree. HEAD is always detached, so a
// materialization can never move a ref in the shared cache.
func (c *Cache) AddWorktree(ctx context.Context, opts WorktreeOptions) (*Worktree, error) {
	commit, err := c.git.ResolveCommit(ctx, opts.Commit)
	if err != nil {
		return nil, fmt.Errorf("source worktree: %w", err)
	}
	name := opts.Name
	if name == "" {
		name = worktreePrefix + shortSHA(commit)
	}
	if err := config.ValidateRelPath(name); err != nil {
		return nil, fmt.Errorf("source worktree %q: %w", name, err)
	}
	if strings.Contains(name, "/") {
		return nil, fmt.Errorf("source worktree %q must be a single element", name)
	}
	dir, err := config.SafeJoin(ctx, c.worktreeRoot, name)
	if err != nil {
		return nil, fmt.Errorf("source worktree: %w", err)
	}

	if err := c.git.AddWorktree(ctx, gitcli.WorktreeOptions{
		Path:       dir,
		Commit:     commit,
		NoCheckout: true,
	}); err != nil {
		return nil, fmt.Errorf("source worktree: %w", err)
	}

	git, err := c.git.WithDir(dir)
	if err != nil {
		return nil, fmt.Errorf("source worktree: %w", err)
	}
	worktree := &Worktree{path: dir, commit: commit, git: git, cache: c}
	if err := worktree.materialize(ctx, opts); err != nil {
		// The half built work tree is removed so a retry is not blocked by it,
		// and the original failure is what the caller sees.
		return nil, errors.Join(err, worktree.Remove(ctx))
	}
	return worktree, nil
}

// materialize installs the pattern set and checks out the commit.
func (w *Worktree) materialize(ctx context.Context, opts WorktreeOptions) error {
	if len(opts.Patterns) > 0 {
		if err := w.git.SetSparseCheckout(ctx, gitcli.SparseOptions{
			Cone:     opts.Cone,
			Patterns: opts.Patterns,
		}); err != nil {
			return fmt.Errorf("source worktree: %w", err)
		}
	}
	if err := w.git.CheckoutDetached(ctx, w.commit); err != nil {
		return fmt.Errorf("source worktree: %w", err)
	}
	return nil
}

// Path reports the work tree directory.
func (w *Worktree) Path() string { return w.path }

// Commit reports the materialized commit.
func (w *Worktree) Commit() string { return w.commit }

// Git reports the runner scoped to this work tree.
func (w *Worktree) Git() *gitcli.Runner { return w.git }

// Remove deletes the work tree and its registration. It is idempotent, so a
// deferred cleanup is safe after a failure that already removed it and cannot
// mask the error that caused the failure.
func (w *Worktree) Remove(ctx context.Context) error {
	if err := w.cache.git.RemoveWorktree(ctx, w.path); err != nil {
		return fmt.Errorf("source worktree: %w", err)
	}
	// A work tree that failed before git registered it leaves a directory
	// behind. It is inside the caller's work tree root because SafeJoin proved
	// so when the path was built.
	if err := os.RemoveAll(w.path); err != nil {
		return fmt.Errorf("source worktree: %w", err)
	}
	if err := w.cache.git.PruneWorktrees(ctx); err != nil {
		return fmt.Errorf("source worktree: %w", err)
	}
	return nil
}

// SparsePatterns renders the sparse pattern set for package roots.
//
// A non-recursive root materializes the files of exactly that directory: the
// include pattern alone would also pull in every subdirectory, because a sparse
// pattern that matches a directory matches everything below it, so each root is
// followed by a negative pattern that excludes its subdirectories. That is what
// makes extraction package granular rather than directory recursive.
func SparsePatterns(roots []string, recursive bool) ([]string, error) {
	if len(roots) == 0 {
		return nil, errors.New("sparse patterns: at least one package root is required")
	}
	cleaned := make([]string, 0, len(roots))
	for _, root := range roots {
		if err := config.ValidatePackagePath(root); err != nil {
			return nil, fmt.Errorf("sparse patterns: package root %q: %w", root, err)
		}
		cleaned = append(cleaned, path.Clean(root))
	}
	if !recursive {
		// Nested roots would have one root's subdirectory exclusion delete
		// another root's files, and the pattern order that resolves it depends
		// on which root is listed first. The combination is refused rather than
		// silently resolved.
		for _, outer := range cleaned {
			for _, inner := range cleaned {
				if outer != inner && strings.HasPrefix(inner, outer+"/") {
					return nil, fmt.Errorf("sparse patterns: package root %q contains %q, which non-recursive materialization cannot express", outer, inner)
				}
			}
		}
	}

	patterns := make([]string, 0, len(cleaned)*2)
	for _, root := range cleaned {
		if recursive {
			patterns = append(patterns, "/"+root+"/")
			continue
		}
		patterns = append(patterns, "/"+root+"/*", "!/"+root+"/*/")
	}
	return patterns, nil
}

// branchRef renders and checks a fully qualified branch ref.
func branchRef(name string) (string, error) {
	if err := gitcli.ValidateBranchName(name); err != nil {
		return "", err
	}
	return branchPrefix + name, nil
}

// tagRef renders and checks a fully qualified tag ref.
func tagRef(name string) (string, error) {
	switch {
	case name == "":
		return "", errors.New("tag name must not be empty")
	case strings.HasPrefix(name, "refs/"):
		return "", fmt.Errorf("tag name %q must be a short name", name)
	}
	ref := tagPrefix + name
	if err := gitcli.ValidateRefName(ref); err != nil {
		return "", err
	}
	return ref, nil
}

// shortSHA abbreviates a commit name for use in a directory name.
func shortSHA(commit string) string {
	if len(commit) <= shortSHALength {
		return commit
	}
	return commit[:shortSHALength]
}

// cacheDirName derives a stable directory name from a remote URL. The readable
// part helps a human recognise a cache directory, and the digest keeps two
// remotes that sanitise to the same text in separate caches.
func cacheDirName(remote string) string {
	digest := sha256.Sum256([]byte(remote))
	var label strings.Builder
	previousDash := false
	for _, r := range remote {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_':
			label.WriteRune(r)
			previousDash = false
		case !previousDash:
			label.WriteRune('-')
			previousDash = true
		}
	}
	name := strings.Trim(label.String(), "-._")
	if len(name) > 48 {
		name = strings.Trim(name[:48], "-._")
	}
	if name == "" {
		name = "source"
	}
	return name + "-" + hex.EncodeToString(digest[:4]) + ".git"
}
