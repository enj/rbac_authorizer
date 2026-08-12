package gomodmap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// indexSchema is the version of the on disk index document.
//
// It is written and checked rather than inferred, because the index is a cache
// whose entries become published dependency pins. A document written by an older
// engine that meant something slightly different has to be rejected and rebuilt,
// which costs one resolution pass, instead of being read as if it meant what
// this version means.
const indexSchema = 1

// ErrIndexMissing reports that no index has been stored yet. It is not a
// failure: the first run of a profile has nothing cached.
var ErrIndexMissing = errors.New("version index does not exist")

// ErrIndexCorrupt reports an index file that is not a document this engine
// wrote.
var ErrIndexCorrupt = errors.New("version index is corrupt")

// indexDocument is the serialized form of an Index.
type indexDocument struct {
	Schema  int     `json:"schema"`
	Entries []Entry `json:"entries"`
}

// Store persists a version Index as one file.
//
// The file is rewritten whole rather than appended to, and every write lands
// through a temporary file and a rename, so a run interrupted mid-write leaves
// the previous index intact rather than a truncated document that the next run
// would have to decide how to interpret.
type Store struct {
	path string
}

// NewStore returns a store backed by one file.
//
// The path must be absolute, so a cache location never depends on the process
// working directory. A run that resolved it relatively would write its cache
// wherever it happened to be started from and silently miss it next time.
func NewStore(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("version index: a path is required")
	}
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("version index: path %q must be absolute", path)
	}
	return &Store{path: filepath.Clean(path)}, nil
}

// Path reports the file backing the store.
func (s *Store) Path() string { return s.path }

// Load reads the stored index.
//
// A file that does not exist reports ErrIndexMissing, which a caller treats as
// an empty cache. Every other unreadable state reports ErrIndexCorrupt, because
// a cache that cannot be understood must be rebuilt rather than partially
// trusted: unknown fields, an unknown schema, and a duplicate entry are all
// evidence that the file was not written by this engine in this version.
func (s *Store) Load(ctx context.Context) (*Index, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("version index %s: %w", s.path, err)
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("version index %s: %w", s.path, ErrIndexMissing)
		}
		return nil, fmt.Errorf("version index %s: %w", s.path, err)
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var document indexDocument
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("version index %s: %w: %w", s.path, ErrIndexCorrupt, err)
	}
	if decoder.More() {
		return nil, fmt.Errorf("version index %s: %w: trailing content after the document", s.path, ErrIndexCorrupt)
	}
	if document.Schema != indexSchema {
		return nil, fmt.Errorf("version index %s: %w: schema %d is not %d", s.path, ErrIndexCorrupt, document.Schema, indexSchema)
	}

	index := NewIndex()
	seen := make(map[string]bool, len(document.Entries))
	for _, entry := range document.Entries {
		// A repeated source is a defect of the document, decided here rather than
		// left to Put. Put tolerates an identical repeat so a resumed run can
		// replay an answer it already holds, and it refuses a divergent one for
		// its own reason, so leaning on it would report two shapes of the same
		// fault and would let the identical shape through entirely. This engine
		// writes one entry per source commit, so a stored index naming one twice
		// was not written by it however well the copies agree.
		if seen[entry.Source] {
			return nil, fmt.Errorf("version index %s: %w: source %s is recorded more than once", s.path, ErrIndexCorrupt, entry.Source)
		}
		seen[entry.Source] = true
		if err := index.Put(entry); err != nil {
			return nil, fmt.Errorf("version index %s: %w: %w", s.path, ErrIndexCorrupt, err)
		}
	}
	return index, nil
}

// Save writes the index, preserving whatever a previous run recorded.
//
// The file is rewritten whole, so writing only the in-memory index would discard
// every entry this process did not happen to resolve, and a run that resolved
// one commit would erase a cache built over a long backfill. The stored entries
// are loaded and merged through Put instead, which keeps them and, more
// importantly, refuses the merge when a stored entry and a new one disagree
// about the same source commit. That disagreement is evidence that one of the
// two was resolved against a different staging history, and it has to surface
// rather than be settled by whichever run wrote last.
//
// A corrupt existing file is refused for the same reason: overwriting it would
// destroy the only record of what the previous run believed.
//
// Entries are sorted and the document ends in a newline, so storing the same
// index twice produces identical bytes and a stored cache can be compared or
// diffed without normalising it first.
func (s *Store) Save(ctx context.Context, index *Index) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("version index %s: %w", s.path, err)
	}
	if index == nil {
		return fmt.Errorf("version index %s: no index to save", s.path)
	}

	merged, err := s.Load(ctx)
	switch {
	case errors.Is(err, ErrIndexMissing):
		merged = NewIndex()
	case err != nil:
		return err
	}
	for _, entry := range index.Entries() {
		if err := merged.Put(entry); err != nil {
			return fmt.Errorf("version index %s: %w", s.path, err)
		}
	}

	entries := merged.Entries()
	if entries == nil {
		entries = []Entry{}
	}

	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetIndent("", "  ")
	// Module paths and versions are not HTML. Escaping the characters a browser
	// would care about would rewrite them into a form that no longer matches the
	// value that was resolved.
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(indexDocument{Schema: indexSchema, Entries: entries}); err != nil {
		return fmt.Errorf("version index %s: %w", s.path, err)
	}
	if err := writeFileAtomic(s.path, buffer.Bytes()); err != nil {
		return fmt.Errorf("version index %s: %w", s.path, err)
	}
	return nil
}

// writeFileAtomic replaces a file's contents in one step.
//
// The temporary file is created in the destination directory rather than in the
// system temporary directory, because a rename is only atomic within one
// filesystem and the two are routinely on different ones. It is removed on every
// failure path, so a run that dies between creating and renaming does not leave
// the directory filling with partial documents.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	scratch, err := os.CreateTemp(dir, "."+filepath.Base(path)+".")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	name := scratch.Name()
	// On every failure path the scratch file is closed and removed. Both are
	// unchecked because both are expected to fail on the success path: the file
	// has already been closed below, and the name has already been renamed away.
	defer func() {
		_ = scratch.Close()
		_ = os.Remove(name)
	}()

	if err := scratch.Chmod(0o600); err != nil {
		return fmt.Errorf("set permissions on %s: %w", name, err)
	}
	if _, err := scratch.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	// The contents have to reach the disk before the rename publishes the name,
	// or a crash can leave the new name pointing at a file whose blocks were
	// never written.
	if err := scratch.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", name, err)
	}
	if err := scratch.Close(); err != nil {
		return fmt.Errorf("close %s: %w", name, err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("rename %s: %w", name, err)
	}
	// The rename itself is a directory change, and syncing the file only flushed
	// its contents. Without this a crash can leave the directory entry pointing
	// at the old name, so the durable state would be the previous document rather
	// than the one this call reported as written.
	return syncDir(dir)
}

// syncDir flushes a directory's own entries.
//
// A filesystem that does not support the operation is not a failure: the sync is
// a durability improvement over what the rename already guarantees, so refusing
// to write on a platform that cannot offer it would trade a real capability for
// a theoretical one.
func syncDir(dir string) error {
	// The directory is the parent of the store's own path, which NewStore
	// required to be absolute and cleaned, and it is opened to be flushed rather
	// than read: nothing is ever taken out of the handle.
	handle, err := os.Open(dir) //nolint:gosec // dir is the parent of the validated store path
	if err != nil {
		return fmt.Errorf("open %s: %w", dir, err)
	}
	defer func() { _ = handle.Close() }()
	if err := handle.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.ENOTSUP) {
		return fmt.Errorf("sync %s: %w", dir, err)
	}
	return nil
}
