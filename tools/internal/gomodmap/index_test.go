package gomodmap_test

import (
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/gomodmap"
)

// Object names used as index keys. They are literals rather than fixture
// commits because the index is a pure data structure: what it stores has to be
// a valid object name, not a commit that exists somewhere.
const (
	sourceA  = "1111111111111111111111111111111111111111"
	sourceB  = "2222222222222222222222222222222222222222"
	stagingA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	stagingB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// pinnedAt renders an intermediate pin naming one staging commit.
func pinnedAt(path, commit string) gomodmap.ModuleVersion {
	return gomodmap.ModuleVersion{
		Path:    path,
		Version: "v0.0.0-20260101000000-" + commit[:12],
		Commit:  commit,
	}
}

func TestIndex_PutAndLookup(t *testing.T) {
	t.Parallel()

	index := gomodmap.NewIndex()
	entry := gomodmap.Entry{
		Source: sourceA,
		Modules: []gomodmap.ModuleVersion{
			pinnedAt("k8s.io/apimachinery", stagingB),
			pinnedAt("k8s.io/api", stagingA),
		},
	}
	if err := index.Put(entry); err != nil {
		t.Fatalf("put: %v", err)
	}
	if index.Len() != 1 {
		t.Errorf("len = %d, want 1", index.Len())
	}

	stored, ok := index.Lookup(sourceA)
	if !ok {
		t.Fatal("lookup: entry not found")
	}
	// Modules are stored sorted regardless of the order they arrived in, so a
	// serialized index does not depend on resolution order.
	if stored.Modules[0].Path != "k8s.io/api" || stored.Modules[1].Path != "k8s.io/apimachinery" {
		t.Errorf("modules = %v, want them sorted by path", stored.Modules)
	}

	if _, ok := index.Lookup(sourceB); ok {
		t.Error("lookup: found an entry that was never recorded")
	}
}

// TestIndex_LookupCopies proves a caller cannot reach into the index and change
// a cached answer. The index is loaded from a file an earlier run wrote, so what
// it holds is input rather than something this process computed.
func TestIndex_LookupCopies(t *testing.T) {
	t.Parallel()

	index := gomodmap.NewIndex()
	if err := index.Put(gomodmap.Entry{
		Source:  sourceA,
		Modules: []gomodmap.ModuleVersion{pinnedAt("k8s.io/api", stagingA)},
	}); err != nil {
		t.Fatalf("put: %v", err)
	}

	first, _ := index.Lookup(sourceA)
	first.Modules[0].Version = "v9.9.9"
	first.Modules[0].Commit = stagingB

	second, _ := index.Lookup(sourceA)
	if second.Modules[0].Version == "v9.9.9" || second.Modules[0].Commit == stagingB {
		t.Errorf("mutating a looked up entry changed the index: %v", second.Modules[0])
	}

	// Entries has to copy for the same reason.
	entries := index.Entries()
	entries[0].Modules[0].Version = "v8.8.8"
	again, _ := index.Lookup(sourceA)
	if again.Modules[0].Version == "v8.8.8" {
		t.Errorf("mutating a reported entry changed the index: %v", again.Modules[0])
	}
}

// TestIndex_PutIdempotent proves a resumed run can replay what it already knows.
func TestIndex_PutIdempotent(t *testing.T) {
	t.Parallel()

	index := gomodmap.NewIndex()
	entry := gomodmap.Entry{
		Source:  sourceA,
		Tag:     "v0.36.1",
		Modules: []gomodmap.ModuleVersion{{Path: "k8s.io/api", Version: "v0.36.1", Commit: stagingA}},
	}
	if err := index.Put(entry); err != nil {
		t.Fatalf("first put: %v", err)
	}
	if err := index.Put(entry); err != nil {
		t.Fatalf("second put: %v", err)
	}
	if index.Len() != 1 {
		t.Errorf("len = %d, want 1", index.Len())
	}
}

// TestIndex_PutConflict proves a second, different answer for one source commit
// is refused rather than silently overwriting the first.
func TestIndex_PutConflict(t *testing.T) {
	t.Parallel()

	index := gomodmap.NewIndex()
	if err := index.Put(gomodmap.Entry{
		Source:  sourceA,
		Modules: []gomodmap.ModuleVersion{pinnedAt("k8s.io/api", stagingA)},
	}); err != nil {
		t.Fatalf("first put: %v", err)
	}

	err := index.Put(gomodmap.Entry{
		Source:  sourceA,
		Modules: []gomodmap.ModuleVersion{pinnedAt("k8s.io/api", stagingB)},
	})
	if err == nil {
		t.Fatal("second put: got nil error, want a conflict")
	}
	if !strings.Contains(err.Error(), "already recorded with different versions") {
		t.Errorf("second put: error = %v, want a conflict error", err)
	}
}

// TestIndex_PutZeroValue proves a struct literal index is usable, because that
// is the natural result of a caller declaring one without the constructor.
func TestIndex_PutZeroValue(t *testing.T) {
	t.Parallel()

	var index gomodmap.Index
	if err := index.Put(gomodmap.Entry{
		Source:  sourceA,
		Modules: []gomodmap.ModuleVersion{pinnedAt("k8s.io/api", stagingA)},
	}); err != nil {
		t.Fatalf("put into a zero value index: %v", err)
	}
	if index.Len() != 1 {
		t.Errorf("len = %d, want 1", index.Len())
	}
}

// TestIndex_PutNil proves a nil index reports rather than silently discarding a
// resolution that cost a proxy round trip.
func TestIndex_PutNil(t *testing.T) {
	t.Parallel()

	var index *gomodmap.Index
	err := index.Put(gomodmap.Entry{
		Source:  sourceA,
		Modules: []gomodmap.ModuleVersion{pinnedAt("k8s.io/api", stagingA)},
	})
	if err == nil {
		t.Fatal("put into a nil index: got nil error, want a failure")
	}
	if !strings.Contains(err.Error(), "no index to record into") {
		t.Errorf("put into a nil index: error = %v, want it to name the nil index", err)
	}
}

func TestIndex_Entries_Sorted(t *testing.T) {
	t.Parallel()

	index := gomodmap.NewIndex()
	// Inserted newest first so a stable output proves sorting rather than
	// insertion order.
	for _, source := range []string{sourceB, sourceA} {
		if err := index.Put(gomodmap.Entry{
			Source:  source,
			Modules: []gomodmap.ModuleVersion{pinnedAt("k8s.io/api", stagingA)},
		}); err != nil {
			t.Fatalf("put %s: %v", source, err)
		}
	}

	entries := index.Entries()
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	if entries[0].Source != sourceA || entries[1].Source != sourceB {
		t.Errorf("entries are ordered %s, %s, want them sorted by source", entries[0].Source, entries[1].Source)
	}
}

func TestIndex_Put_Rejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		entry   gomodmap.Entry
		wantErr string
	}{
		{
			name:    "source is not an object name",
			entry:   gomodmap.Entry{Source: "abc", Modules: []gomodmap.ModuleVersion{pinnedAt("k8s.io/api", stagingA)}},
			wantErr: "must be 40 or 64 hexadecimal characters",
		},
		{
			name:    "no modules",
			entry:   gomodmap.Entry{Source: sourceA},
			wantErr: "records no modules",
		},
		{
			name:    "module without a path",
			entry:   gomodmap.Entry{Source: sourceA, Modules: []gomodmap.ModuleVersion{{Version: "v0.36.1", Commit: stagingA}}},
			wantErr: "module path",
		},
		{
			// A path the go command would refuse is one no proxy can serve.
			name:    "module path is not a module path",
			entry:   gomodmap.Entry{Source: sourceA, Modules: []gomodmap.ModuleVersion{{Path: "not a module path", Version: "v0.36.1", Commit: stagingA}}},
			wantErr: "module path",
		},
		{
			name:    "module without a version",
			entry:   gomodmap.Entry{Source: sourceA, Modules: []gomodmap.ModuleVersion{{Path: "k8s.io/api", Commit: stagingA}}},
			wantErr: "a version is required",
		},
		{
			// A query resolves to whatever the proxy served at that moment, which
			// is the one thing a reproducible extraction cannot contain.
			name:    "module version is a query",
			entry:   gomodmap.Entry{Source: sourceA, Modules: []gomodmap.ModuleVersion{{Path: "k8s.io/api", Version: "latest", Commit: stagingA}}},
			wantErr: "is not a semantic version",
		},
		{
			name:    "module version is the staging placeholder",
			entry:   gomodmap.Entry{Source: sourceA, Modules: []gomodmap.ModuleVersion{{Path: "k8s.io/api", Version: gomodmap.StagingVersion, Commit: stagingA}}},
			wantErr: "is the source placeholder",
		},
		{
			name:    "module version carries build metadata",
			entry:   gomodmap.Entry{Source: sourceA, Modules: []gomodmap.ModuleVersion{{Path: "k8s.io/api", Version: "v0.36.1+meta", Commit: stagingA}}},
			wantErr: "build metadata",
		},
		{
			name:    "module version is not canonical",
			entry:   gomodmap.Entry{Source: sourceA, Modules: []gomodmap.ModuleVersion{{Path: "k8s.io/api", Version: "v0.36", Commit: stagingA}}},
			wantErr: "is not canonical",
		},
		{
			// A major version that disagrees with the path names a module that
			// cannot exist.
			name:    "major version disagrees with the path",
			entry:   gomodmap.Entry{Source: sourceA, Modules: []gomodmap.ModuleVersion{{Path: "k8s.io/klog/v2", Version: "v1.130.1", Commit: stagingA}}},
			wantErr: "k8s.io/klog/v2",
		},
		{
			name: "module recorded twice",
			entry: gomodmap.Entry{Source: sourceA, Modules: []gomodmap.ModuleVersion{
				pinnedAt("k8s.io/api", stagingA),
				pinnedAt("k8s.io/api", stagingB),
			}},
			wantErr: "records module k8s.io/api twice",
		},
		{
			// Every pin names a commit, so one without it records a version
			// nothing can later re-verify.
			name:    "module has no staging commit",
			entry:   gomodmap.Entry{Source: sourceA, Modules: []gomodmap.ModuleVersion{{Path: "k8s.io/api", Version: "v0.36.1"}}},
			wantErr: "staging commit",
		},
		{
			name:    "staging commit is abbreviated",
			entry:   gomodmap.Entry{Source: sourceA, Modules: []gomodmap.ModuleVersion{{Path: "k8s.io/api", Version: "v0.36.1", Commit: stagingA[:12]}}},
			wantErr: "staging commit",
		},
		{
			name:    "release tag is not canonical",
			entry:   gomodmap.Entry{Source: sourceA, Tag: "v0.36", Modules: []gomodmap.ModuleVersion{{Path: "k8s.io/api", Version: "v0.36.1", Commit: stagingA}}},
			wantErr: "release tag",
		},
		{
			// Mixing the two resolution paths inside one entry would let the cache
			// answer a release query with an intermediate pin.
			name: "release entry pins a module at another version",
			entry: gomodmap.Entry{
				Source:  sourceA,
				Tag:     "v0.36.1",
				Modules: []gomodmap.ModuleVersion{pinnedAt("k8s.io/api", stagingA)},
			},
			wantErr: "is tagged v0.36.1 but pins",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := gomodmap.NewIndex().Put(test.entry)
			if err == nil {
				t.Fatalf("put: got nil error, want %q", test.wantErr)
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Errorf("put: error = %v, want it to contain %q", err, test.wantErr)
			}
		})
	}
}

// TestIndex_NilSafe proves a nil index reads as an empty one rather than
// panicking, so a caller that has not loaded a cache yet needs no special case.
func TestIndex_NilSafe(t *testing.T) {
	t.Parallel()

	var index *gomodmap.Index
	if index.Len() != 0 {
		t.Errorf("len = %d, want 0", index.Len())
	}
	if _, ok := index.Lookup(sourceA); ok {
		t.Error("lookup: found an entry in a nil index")
	}
	if entries := index.Entries(); entries != nil {
		t.Errorf("entries = %v, want nil", entries)
	}
}

func TestValidateExactVersion(t *testing.T) {
	t.Parallel()

	// +incompatible is the one build metadata suffix the module system allows,
	// and Kubernetes really does require modules this way.
	for _, version := range []string{"v0.36.1", "v2.0.0+incompatible", "v0.0.0-20260101000000-abcdefabcdef"} {
		if err := gomodmap.ValidateExactVersion(version); err != nil {
			t.Errorf("validate %s: %v", version, err)
		}
	}
	for _, version := range []string{"", "latest", "v0.36", "v0.36.1+meta", gomodmap.StagingVersion} {
		if err := gomodmap.ValidateExactVersion(version); err == nil {
			t.Errorf("validate %q: got nil error, want a failure", version)
		}
	}
}
