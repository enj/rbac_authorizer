package gomodmap_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/gomodmap"
)

// newTestStore returns a store backed by a fresh temporary file path.
func newTestStore(t *testing.T) *gomodmap.Store {
	t.Helper()
	store, err := gomodmap.NewStore(filepath.Join(t.TempDir(), "versions.json"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	return store
}

// sampleIndex returns an index with two source commits recorded.
func sampleIndex(t *testing.T) *gomodmap.Index {
	t.Helper()
	index := gomodmap.NewIndex()
	entries := []gomodmap.Entry{
		{
			Source:  sourceB,
			Modules: []gomodmap.ModuleVersion{{Path: "k8s.io/api", Version: "v0.36.1", Commit: stagingA}},
			Tag:     "v0.36.1",
		},
		{
			Source: sourceA,
			Modules: []gomodmap.ModuleVersion{
				pinnedAt("k8s.io/apimachinery", stagingB),
				pinnedAt("k8s.io/api", stagingA),
			},
		},
	}
	for _, entry := range entries {
		if err := index.Put(entry); err != nil {
			t.Fatalf("put %s: %v", entry.Source, err)
		}
	}
	return index
}

func TestStore_SaveAndLoad(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newTestStore(t)
	if err := store.Save(ctx, sampleIndex(t)); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, err := store.Load(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.Len() != 2 {
		t.Fatalf("loaded %d entries, want 2", loaded.Len())
	}

	entry, ok := loaded.Lookup(sourceA)
	if !ok {
		t.Fatal("load: entry for the intermediate commit is missing")
	}
	if len(entry.Modules) != 2 {
		t.Fatalf("modules = %d, want 2", len(entry.Modules))
	}
	if entry.Modules[0].Commit != stagingA {
		t.Errorf("staging commit = %q, want %q", entry.Modules[0].Commit, stagingA)
	}

	tagged, ok := loaded.Lookup(sourceB)
	if !ok {
		t.Fatal("load: entry for the tagged commit is missing")
	}
	if tagged.Tag != "v0.36.1" {
		t.Errorf("tag = %q, want v0.36.1", tagged.Tag)
	}
	// A release pin records its commit too. The tag is immutable to this engine
	// but not to the repository that published it, so the commit it resolved to
	// is the only evidence a later run has that it still names the same code.
	if tagged.Modules[0].Commit != stagingA {
		t.Errorf("staging commit = %q, want %q", tagged.Modules[0].Commit, stagingA)
	}
}

// TestStore_SaveDeterministic proves two saves of the same content produce
// identical bytes, so a stored cache can be compared without normalising it.
func TestStore_SaveDeterministic(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	first := newTestStore(t)
	second := newTestStore(t)

	if err := first.Save(ctx, sampleIndex(t)); err != nil {
		t.Fatalf("first save: %v", err)
	}
	// The second index is built by inserting the same entries in the opposite
	// order, which is what proves the output does not depend on insertion order.
	other := gomodmap.NewIndex()
	for _, entry := range sampleIndex(t).Entries() {
		if err := other.Put(entry); err != nil {
			t.Fatalf("put: %v", err)
		}
	}
	if err := second.Save(ctx, other); err != nil {
		t.Fatalf("second save: %v", err)
	}

	firstBytes, err := os.ReadFile(first.Path())
	if err != nil {
		t.Fatalf("read first: %v", err)
	}
	secondBytes, err := os.ReadFile(second.Path())
	if err != nil {
		t.Fatalf("read second: %v", err)
	}
	if string(firstBytes) != string(secondBytes) {
		t.Errorf("saved bytes differ:\n%s\n---\n%s", firstBytes, secondBytes)
	}
	if !strings.HasSuffix(string(firstBytes), "\n") {
		t.Error("saved document does not end in a newline")
	}
}

// TestStore_SaveLeavesNoScratch proves the atomic write cleans up after itself
// rather than filling the directory with partial documents.
func TestStore_SaveLeavesNoScratch(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	dir := t.TempDir()
	store, err := gomodmap.NewStore(filepath.Join(dir, "versions.json"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	for range 3 {
		if err := store.Save(ctx, sampleIndex(t)); err != nil {
			t.Fatalf("save: %v", err)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "versions.json" {
		names := make([]string, len(entries))
		for i, entry := range entries {
			names[i] = entry.Name()
		}
		t.Errorf("directory holds %v, want only versions.json", names)
	}
}

// TestStore_SaveMerges proves a save keeps what an earlier run recorded.
//
// The file is rewritten whole, so a run that resolved one commit would erase a
// cache built over a long backfill if it wrote only what it held in memory.
func TestStore_SaveMerges(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newTestStore(t)
	if err := store.Save(ctx, sampleIndex(t)); err != nil {
		t.Fatalf("first save: %v", err)
	}

	const sourceC = "3333333333333333333333333333333333333333"
	later := gomodmap.NewIndex()
	if err := later.Put(gomodmap.Entry{
		Source:  sourceC,
		Modules: []gomodmap.ModuleVersion{pinnedAt("k8s.io/api", stagingB)},
	}); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := store.Save(ctx, later); err != nil {
		t.Fatalf("second save: %v", err)
	}

	loaded, err := store.Load(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.Len() != 3 {
		t.Errorf("loaded %d entries, want 3", loaded.Len())
	}
	for _, source := range []string{sourceA, sourceB, sourceC} {
		if _, ok := loaded.Lookup(source); !ok {
			t.Errorf("entry for %s is missing after the merge", source)
		}
	}
}

// TestStore_SaveConflict proves a stored answer and a new one that disagree
// about the same source commit stop the save.
//
// Disagreement means one of the two was resolved against a different staging
// history. Settling it by whichever run wrote last would publish a pin nothing
// can reproduce, so it surfaces instead.
func TestStore_SaveConflict(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newTestStore(t)
	if err := store.Save(ctx, sampleIndex(t)); err != nil {
		t.Fatalf("first save: %v", err)
	}

	conflicting := gomodmap.NewIndex()
	if err := conflicting.Put(gomodmap.Entry{
		Source:  sourceA,
		Modules: []gomodmap.ModuleVersion{pinnedAt("k8s.io/api", stagingB)},
	}); err != nil {
		t.Fatalf("put: %v", err)
	}
	err := store.Save(ctx, conflicting)
	if err == nil {
		t.Fatal("save: got nil error, want a conflicting entry to be refused")
	}
	if !strings.Contains(err.Error(), "already recorded with different versions") {
		t.Errorf("save: error = %v, want it to name the conflict", err)
	}

	// The previous document has to survive the refusal, because it is the only
	// record of what the earlier run believed.
	loaded, err := store.Load(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	entry, ok := loaded.Lookup(sourceA)
	if !ok {
		t.Fatal("load: the stored entry was lost")
	}
	if entry.Modules[0].Commit != stagingA {
		t.Errorf("staging commit = %q, want the stored %q", entry.Modules[0].Commit, stagingA)
	}
}

// TestStore_SaveCorrupt proves an unreadable existing document is refused rather
// than overwritten, so a save never destroys a cache it could not understand.
func TestStore_SaveCorrupt(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "versions.json")
	const corrupt = "this is not a version index\n"
	if err := os.WriteFile(path, []byte(corrupt), 0o600); err != nil {
		t.Fatalf("write index: %v", err)
	}
	store, err := gomodmap.NewStore(path)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	if err := store.Save(t.Context(), sampleIndex(t)); !errors.Is(err, gomodmap.ErrIndexCorrupt) {
		t.Errorf("save: error = %v, want ErrIndexCorrupt", err)
	}

	kept, err := os.ReadFile(path) //nolint:gosec // the path is this test's own temporary file
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	if string(kept) != corrupt {
		t.Errorf("index = %q, want the refused save to leave %q", kept, corrupt)
	}
}

// TestStore_SaveNil proves a missing index is reported rather than written out
// as an empty document, which would erase whatever was cached.
func TestStore_SaveNil(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	err := store.Save(t.Context(), nil)
	if err == nil {
		t.Fatal("save: got nil error, want a nil index to be refused")
	}
	if !strings.Contains(err.Error(), "no index to save") {
		t.Errorf("save: error = %v, want it to name the missing index", err)
	}
	if _, err := store.Load(t.Context()); !errors.Is(err, gomodmap.ErrIndexMissing) {
		t.Errorf("load: error = %v, want the refused save to have written nothing", err)
	}
}

// TestStore_SaveEmpty proves an empty index round trips, so a first run that
// resolved nothing still writes a document the next run can read.
func TestStore_SaveEmpty(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := newTestStore(t)
	if err := store.Save(ctx, gomodmap.NewIndex()); err != nil {
		t.Fatalf("save: %v", err)
	}
	loaded, err := store.Load(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.Len() != 0 {
		t.Errorf("loaded %d entries, want 0", loaded.Len())
	}
}

func TestStore_LoadMissing(t *testing.T) {
	t.Parallel()

	_, err := newTestStore(t).Load(t.Context())
	if !errors.Is(err, gomodmap.ErrIndexMissing) {
		t.Errorf("load: error = %v, want ErrIndexMissing", err)
	}
}

func TestStore_LoadCorrupt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		contents string
	}{
		{
			name:     "not json at all",
			contents: "this is not a version index\n",
		},
		{
			name:     "truncated document",
			contents: `{"schema": 1, "entries": [`,
		},
		{
			// An unknown field means the document was written by something that
			// recorded more than this version knows how to honour.
			name:     "unknown field",
			contents: `{"schema": 1, "entries": [], "policy": "v1-to-v0"}`,
		},
		{
			name:     "unknown schema",
			contents: `{"schema": 99, "entries": []}`,
		},
		{
			name:     "no schema",
			contents: `{"entries": []}`,
		},
		{
			name:     "trailing content",
			contents: `{"schema": 1, "entries": []}{"schema": 1, "entries": []}`,
		},
		{
			name: "one source recorded twice",
			contents: `{"schema": 1, "entries": [
				{"source": "` + sourceA + `", "modules": [{"path": "k8s.io/api", "version": "v0.36.1"}]},
				{"source": "` + sourceA + `", "modules": [{"path": "k8s.io/api", "version": "v0.36.2"}]}
			]}`,
		},
		{
			name: "source is not an object name",
			contents: `{"schema": 1, "entries": [
				{"source": "abc", "modules": [{"path": "k8s.io/api", "version": "v0.36.1"}]}
			]}`,
		},
		{
			name: "entry records no modules",
			contents: `{"schema": 1, "entries": [
				{"source": "` + sourceA + `", "modules": []}
			]}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "versions.json")
			if err := os.WriteFile(path, []byte(test.contents), 0o600); err != nil {
				t.Fatalf("write index: %v", err)
			}
			store, err := gomodmap.NewStore(path)
			if err != nil {
				t.Fatalf("new store: %v", err)
			}
			if _, err := store.Load(t.Context()); !errors.Is(err, gomodmap.ErrIndexCorrupt) {
				t.Errorf("load: error = %v, want ErrIndexCorrupt", err)
			}
		})
	}
}

// TestStore_LoadDirectory proves a path that names a directory is reported
// rather than read as a missing index, so a misconfigured cache location is
// visible instead of looking like a cold cache.
func TestStore_LoadDirectory(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store, err := gomodmap.NewStore(dir)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	_, err = store.Load(t.Context())
	if err == nil {
		t.Fatal("load: got nil error, want a read error")
	}
	if errors.Is(err, gomodmap.ErrIndexMissing) {
		t.Errorf("load: error = %v, want it distinguished from a missing index", err)
	}
}

func TestNewStore_Rejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		path    string
		wantErr string
	}{
		{
			name:    "empty path",
			path:    "",
			wantErr: "a path is required",
		},
		{
			// A relative path would resolve against whatever directory the run
			// happened to start in, so the cache would be written somewhere the
			// next run does not look.
			name:    "relative path",
			path:    "versions.json",
			wantErr: "must be absolute",
		},
		{
			name:    "relative path with a parent reference",
			path:    "../versions.json",
			wantErr: "must be absolute",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := gomodmap.NewStore(test.path)
			if err == nil {
				t.Fatalf("new store: got nil error, want %q", test.wantErr)
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Errorf("new store: error = %v, want it to contain %q", err, test.wantErr)
			}
		})
	}
}

func TestStore_ContextCancelled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	store := newTestStore(t)
	if err := store.Save(ctx, sampleIndex(t)); !errors.Is(err, context.Canceled) {
		t.Errorf("save: error = %v, want context.Canceled", err)
	}
	if _, err := store.Load(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("load: error = %v, want context.Canceled", err)
	}
}
