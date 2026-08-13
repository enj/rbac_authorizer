package sync

import "errors"

// The refusals this package can produce.
//
// They are separate from the ones the composed packages raise, because a
// synchronization refuses for reasons none of its parts can see: an approval
// that names a different plan, a resume shape this engine does not implement,
// and a publication asked for without anything to publish with.
var (
	// ErrApproval reports an approval that does not name this manifest. It is
	// this package's own rather than publish.ErrApproval, because the hash an
	// operator approves covers the whole synchronization and not only the refs
	// it would move.
	ErrApproval = errors.New("approval does not name this synchronization")

	// ErrManifestModified reports a manifest that was changed after it was
	// rendered, which is what a recomputed hash disagreeing with the recorded
	// one means.
	ErrManifestModified = errors.New("synchronization manifest does not match its own hash")

	// ErrManifestLocation reports a manifest string that names the machine the
	// run happened on. The manifest is compared between machines and kept as the
	// record of what was authorized, so a path, a URL, or a line break in one of
	// its free-form strings is a refusal rather than something to render
	// carefully.
	ErrManifestLocation = errors.New("synchronization manifest must carry no path, URL, or control character")

	// ErrUnsupported reports a run shape this engine refuses rather than
	// approximates. It is neither a bad profile nor a broken engine: it is work
	// that has not been implemented, and producing an approximate answer for it
	// would publish history nobody designed.
	ErrUnsupported = errors.New("run shape is not supported by this engine")

	// ErrPublicationDisabled reports an apply asked for without a configured way
	// to reach the destination. A synchronization plans by default and publishes
	// only when it was given both a credentialed way to push and an approval, so
	// the absence of either is a refusal rather than a plan silently becoming a
	// publication.
	ErrPublicationDisabled = errors.New("publication requires a configured destination remote")
)
