package gitcli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Object read sentinels. Each names a verdict a caller has to act on rather than
// a command that failed, so they are distinguishable instead of being folded
// into one opaque error.
var (
	// ErrObjectNotFound reports a revision or path that resolves to no object.
	// In a partial clone this also covers an object the repository legitimately
	// does not hold yet, which is why the lazy fetch decision is explicit.
	ErrObjectNotFound = errors.New("object does not exist")
	// ErrObjectAmbiguous reports a short name matching more than one object.
	ErrObjectAmbiguous = errors.New("object name is ambiguous")
	// ErrNotABlob reports a resolved object that is a tree, commit, or tag. A
	// caller asking for file content must not receive a tree listing instead.
	ErrNotABlob = errors.New("object is not a blob")
	// ErrBlobTooLarge reports a blob past the caller's read limit.
	ErrBlobTooLarge = errors.New("blob is larger than the read limit")
)

// DefaultBlobLimit bounds a read that names no limit of its own. Upstream
// content is untrusted input, so there is no unbounded form of this call: a
// caller that says nothing gets a ceiling rather than the repository's largest
// object in memory.
const DefaultBlobLimit = 64 << 20

// blobHeaderSlack is how much longer than the request the response header may
// be. git answers a resolved object with an object name, a type, and a decimal
// size, and an unresolved one by echoing the request with a verdict appended, so
// the request's own length plus this bounds both.
const blobHeaderSlack = 128

// BlobOptions selects one blob to read.
type BlobOptions struct {
	// Revision names the snapshot to read from, such as a commit or a tree.
	// With an empty Path it names the blob object itself.
	Revision string
	// Path is the repository relative path within Revision, separated by forward
	// slashes. It is not a pathspec: no magic, no wildcards, and no traversal.
	Path string
	// Limit is the largest blob to return in bytes. Zero means DefaultBlobLimit
	// and a negative value is rejected.
	//
	// It bounds the result in memory, not the network. Under AllowLazyFetch git
	// downloads the object before it can report its size, so an oversized blob is
	// refused after it has already reached the object store. A read that must not
	// pull bytes over the network has to ask ObjectInfoBatch without lazy fetch
	// first and decide from the size it reports.
	Limit int64
	// AllowLazyFetch permits a partial clone to download the blob. Leaving it
	// false answers from the local object store only, so a missing blob is
	// reported instead of silently reaching the network.
	//
	// Permitting it is permitting a fetch: the runner must be anonymous, and the
	// gate that guards an explicit transfer is applied here too.
	AllowLazyFetch bool
}

// ReadBlob returns the exact bytes of one blob.
//
// The object is named on standard input rather than in the argument vector,
// using git's batch protocol, so a hostile revision or path is data to git's
// object parser and never a candidate option. The response carries the size
// before the content, so the limit is enforced against what git promises rather
// than against what was already copied into memory: an oversized blob is refused
// on its header and the read is abandoned. What that saves is memory, not
// traffic. Under AllowLazyFetch the object is already local by the time its
// header can be read.
//
// The bytes are returned verbatim, including null bytes and invalid UTF-8, and
// are not passed through the redactor. A blob is content rather than
// diagnostics, and rewriting bytes that merely happened to match a secret would
// corrupt the file the engine is about to parse or hash.
func (r *Runner) ReadBlob(ctx context.Context, opts BlobOptions) ([]byte, error) {
	expression, err := blobExpression(opts.Revision, opts.Path)
	if err != nil {
		return nil, fmt.Errorf("git blob: %w", err)
	}
	name := r.redactor.String(expression)
	limit := opts.Limit
	switch {
	case limit < 0:
		return nil, fmt.Errorf("git blob %q: read limit %d must not be negative", name, limit)
	case limit == 0:
		limit = DefaultBlobLimit
	}

	var env []string
	args := []string{"cat-file", "--batch", "-z", "--buffer"}
	if opts.AllowLazyFetch {
		if err := r.assertLazyFetchAllowed(ctx); err != nil {
			return nil, fmt.Errorf("git blob %q: %w", name, err)
		}
		// The empty credential helper resets the helper list, so a repository
		// local helper can neither be consulted nor prompt during the transfer.
		args = append(slices.Clone(anonymousConfig), args...)
	} else {
		env = []string{noLazyFetch}
	}

	// The read is abandoned as soon as the header settles the answer, so a blob
	// past the limit is never copied into memory to be measured.
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	sink := &blobSink{limit: limit, headerLimit: len(expression) + blobHeaderSlack, stop: cancel}
	// -z frames the request with a null byte, which the validated expression can
	// never contain, so the request cannot be split into two.
	_, runErr := r.runOutput(streamCtx, []byte(expression+"\x00"), env, sink, args...)

	// The sink's verdict is preferred over the run error because the run error
	// after a refusal is the cancellation the sink itself asked for. Only once
	// the command is known to have finished on its own is an incomplete response
	// treated as a fault rather than as the expected result of stopping it.
	if sink.err != nil {
		return nil, fmt.Errorf("git blob %q: %w", name, sink.err)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("git blob %q: %w", name, err)
	}
	if runErr != nil {
		return nil, fmt.Errorf("git blob %q: %w", name, runErr)
	}
	content, err := sink.blob()
	if err != nil {
		return nil, fmt.Errorf("git blob %q: %w", name, err)
	}
	return content, nil
}

// blobSink parses one batch response as it streams and never retains more than
// the caller's limit.
type blobSink struct {
	limit int64
	// headerLimit bounds the response header, so a stream that never produces
	// one cannot be buffered without end.
	headerLimit int
	// stop ends the transfer once the answer is settled.
	stop context.CancelFunc

	header []byte
	parsed bool
	// size is the content length the header promised.
	size    int64
	content []byte
	err     error
}

// Write consumes one chunk of the response.
//
// It always reports the full length and never an error. The pipe has to keep
// draining even after the answer is decided, because a sink that stopped reading
// would leave git blocked on a full pipe with nothing to unblock it. What a
// refusal actually stops is the process, through stop.
//
// Every append is bounded before it happens rather than measured afterwards,
// which is the whole point of reading the response as it arrives.
func (s *blobSink) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 && s.err == nil {
		if !s.parsed {
			end := bytes.IndexByte(p, '\n')
			line := p
			if end >= 0 {
				line = p[:end]
			}
			if len(s.header)+len(line) > s.headerLimit {
				s.fail(fmt.Errorf("object header exceeds %d bytes", s.headerLimit))
				return n, nil
			}
			s.header = append(s.header, line...)
			if end < 0 {
				return n, nil
			}
			p = p[end+1:]
			s.parseHeader()
			continue
		}
		remaining := s.size - int64(len(s.content))
		if remaining <= 0 {
			// Everything past the promised length is the batch delimiter.
			break
		}
		if remaining > int64(len(p)) {
			remaining = int64(len(p))
		}
		s.content = append(s.content, p[:remaining]...)
		p = p[remaining:]
	}
	return n, nil
}

// parseHeader reads the object name, type, and size git reports before the
// content, and decides whether the content is wanted at all.
func (s *blobSink) parseHeader() {
	s.parsed = true
	header := string(s.header)
	// A request git could not resolve is echoed back with the verdict appended,
	// and the echo is the caller's own expression, so the verdict is read from
	// the end of the line rather than from a field position.
	switch {
	case strings.HasSuffix(header, " missing"):
		s.fail(ErrObjectNotFound)
		return
	case strings.HasSuffix(header, " ambiguous"):
		s.fail(ErrObjectAmbiguous)
		return
	}
	fields := strings.Fields(header)
	if len(fields) != 3 {
		s.fail(fmt.Errorf("unexpected object header %q", header))
		return
	}
	size, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil || size < 0 {
		s.fail(fmt.Errorf("unexpected object size %q", fields[2]))
		return
	}
	if fields[1] != "blob" {
		s.fail(fmt.Errorf("%w, it is a %s", ErrNotABlob, fields[1]))
		return
	}
	if size > s.limit {
		s.fail(fmt.Errorf("%w: %d bytes exceed the %d byte limit", ErrBlobTooLarge, size, s.limit))
		return
	}
	s.size = size
	// The allocation happens only once the header has been checked against the
	// limit, so the promised size can never be an allocation the caller did not
	// agree to.
	s.content = make([]byte, 0, size)
}

// fail records the first verdict and ends the transfer.
func (s *blobSink) fail(err error) {
	if s.err == nil {
		s.err = err
	}
	s.stop()
}

// blob reports the content, or why a command that finished on its own did not
// deliver the response its header promised.
func (s *blobSink) blob() ([]byte, error) {
	switch {
	case !s.parsed:
		return nil, errors.New("no object header was reported")
	case int64(len(s.content)) != s.size:
		return nil, fmt.Errorf("got %d content bytes, want %d", len(s.content), s.size)
	}
	return s.content, nil
}

// blobExpression builds the object expression for a revision and path.
//
// The colon is ours rather than the caller's: a revision that carried its own
// would let a caller reach a different path, the index, or a message search from
// what looks like a plain revision argument.
func blobExpression(revision, path string) (string, error) {
	if err := validateRevision(revision); err != nil {
		return "", err
	}
	if strings.ContainsAny(revision, ":\n\r") {
		return "", fmt.Errorf("revision %q must not contain a colon or a line break, name the path separately", revision)
	}
	if path == "" {
		return revision, nil
	}
	if err := validateBlobPath(path); err != nil {
		return "", err
	}
	return revision + ":" + path, nil
}

// validateBlobPath rejects a path that would resolve to something other than the
// file it names.
func validateBlobPath(path string) error {
	switch {
	case strings.ContainsRune(path, '\x00'):
		return fmt.Errorf("path %q must not contain a null byte", path)
	case strings.ContainsAny(path, "\n\r"):
		// The batch protocol reports an unresolved object on a single line, so a
		// path spanning two of them would make that verdict unreadable.
		return fmt.Errorf("path %q must not contain a line break", path)
	case strings.HasPrefix(path, "-"):
		return fmt.Errorf("path %q: %w", path, ErrFlagLikeArgument)
	case strings.HasPrefix(path, "/"):
		return fmt.Errorf("path %q must be repository relative", path)
	case strings.HasPrefix(path, ":"):
		return fmt.Errorf("path %q must not use pathspec magic", path)
	case strings.HasSuffix(path, "/"):
		return fmt.Errorf("path %q must name a file, not a directory", path)
	}
	for _, component := range strings.Split(path, "/") {
		switch component {
		case "":
			return fmt.Errorf("path %q must not contain an empty component", path)
		case ".", "..":
			// Git resolves these against the process working directory rather
			// than against the named revision.
			return fmt.Errorf("path %q must not contain a %q component", path, component)
		}
	}
	return nil
}
