package publish

import "errors"

// Publication refusals. Every one of them is a fail closed answer: the engine
// would rather publish nothing than publish something a consumer cannot undo.
var (
	// ErrNonFastForward reports an update whose new object does not descend
	// from what the remote already holds. A consumer that fetched the old
	// commit would have to rewind to accept the new one, which is the one thing
	// an append only publisher may never ask of them.
	ErrNonFastForward = errors.New("update is not a fast forward")
	// ErrTagMoved reports a tag that exists on the remote with a different
	// object. A published tag is a module version: the proxy, the checksum
	// database, and every consumer's lock file already recorded what it means,
	// so moving it is not an update but a forgery.
	ErrTagMoved = errors.New("published tag must never move")
	// ErrDuplicateRef reports the same destination ref named twice in one plan.
	// Two updates for one ref have no defined order, so the plan would describe
	// an outcome it cannot guarantee.
	ErrDuplicateRef = errors.New("destination ref appears more than once")
	// ErrConflictingRefs reports two destinations that cannot both exist,
	// because one is nested beneath the other in git's ref store.
	ErrConflictingRefs = errors.New("destination refs cannot both exist")
	// ErrForceUpdate reports a destination that carries git's force marker. The
	// marker cannot survive into a refspec here, but a caller that wrote one
	// meant to force, and answering that intent with a silent fast forward
	// would hide the disagreement.
	ErrForceUpdate = errors.New("destination ref must not carry a force marker")
	// ErrDeleteUpdate reports an update that would remove a ref, either by
	// naming no new object or by naming the null object git's protocol spells a
	// deletion with. This package has no delete path at all.
	ErrDeleteUpdate = errors.New("publication never deletes a ref")
	// ErrExpectation reports a remote that does not hold what the caller said
	// it observed. The caller ran its gates against that observation, so a
	// remote that says otherwise invalidates the gates rather than the read.
	ErrExpectation = errors.New("remote ref does not match the stated observation")
	// ErrRemoteDrift reports a remote ref that changed between planning and
	// applying. The approved manifest described the old value, so applying it
	// against a different one would publish an outcome nobody approved.
	ErrRemoteDrift = errors.New("remote ref moved between planning and applying")
	// ErrApproval reports an apply whose approval token is not the hash of the
	// plan it was given. It is the seam where "all gates passed" enters this
	// package, since the gates themselves live above it.
	ErrApproval = errors.New("approval does not name this manifest")
	// ErrManifestModified reports a plan whose actions no longer hash to the
	// manifest hash they carry, which means the plan was edited after it was
	// rendered and approved.
	ErrManifestModified = errors.New("plan does not match its own manifest hash")
	// ErrLocalRemoteNotAllowed reports a path or file URL destination that the
	// caller did not explicitly permit. Local destinations exist for dry runs
	// and tests, so reaching one by accident has to be impossible.
	ErrLocalRemoteNotAllowed = errors.New("local remote requires an explicit option")
	// ErrRemoteRefsUnsupported reports a network destination whose refs cannot
	// be read, because reading them needs a Git remote ref listing this engine
	// does not expose yet. See the package documentation.
	ErrRemoteRefsUnsupported = errors.New("listing refs of a network remote needs a gitcli remote ref API")
	// ErrObjectMissing reports an object the local repository does not have.
	// Nothing can be proved about an object that is not there: not its type,
	// not that a branch fast forwards to it, and not that a push would send it.
	ErrObjectMissing = errors.New("object is not present in the local repository")
	// ErrObjectType reports an object whose type contradicts the ref kind, such
	// as a release tag pointing straight at a commit rather than at the
	// annotated tag object that records the tagger and the source release.
	ErrObjectType = errors.New("object type does not match the ref kind")
	// ErrScopeMismatch reports a plan applied to a destination other than the
	// one it was planned for.
	ErrScopeMismatch = errors.New("plan was made for a different destination")
)
