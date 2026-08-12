package gomodmap

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"

	"github.com/enj/soapbox/tools/internal/gitgraph"
)

// incompatibleSuffix marks a major version 2 or later module published without
// the matching path suffix. It is the only build metadata a module version may
// carry.
const incompatibleSuffix = "+incompatible"

// ValidateExactVersion refuses any version the go command would have to resolve.
//
// A version written as a query such as "latest" would resolve to whatever the
// proxy served at the moment of the run, which is the one thing a reproducible
// extraction cannot contain. The staging placeholder is refused for the opposite
// reason: it is a version upstream never expected to resolve at all.
func ValidateExactVersion(version string) error {
	switch {
	case version == "":
		return errors.New("a version is required")
	case version == StagingVersion:
		return fmt.Errorf("version %s is the source placeholder, which names no published module", version)
	case !semver.IsValid(version):
		return fmt.Errorf("version %q is not a semantic version", version)
	}
	// Reporting other build metadata as itself is more useful than reporting it
	// as merely non canonical, which is what the check below would call it.
	core, _ := strings.CutSuffix(version, incompatibleSuffix)
	if build := semver.Build(core); build != "" {
		return fmt.Errorf("version %q carries build metadata %q, which module versions may not", version, build)
	}
	// CanonicalVersion rather than semver.Canonical, because the latter strips
	// +incompatible and would report a legitimate version as one needing a
	// rewrite.
	if canonical := module.CanonicalVersion(version); canonical != version {
		return fmt.Errorf("version %q is not canonical, which would resolve to %q", version, canonical)
	}
	return nil
}

// ModuleVersion is one staging module pinned to one resolved version.
type ModuleVersion struct {
	// Path is the staging module path, such as k8s.io/api.
	Path string `json:"path"`
	// Version is the resolved version, either a release tag such as v0.36.1 or a
	// pseudo-version the Go toolchain produced.
	Version string `json:"version"`
	// Commit is the staging commit the version names, always populated.
	//
	// It is recorded for a release version as well as an intermediate one. A
	// release tag is immutable to this engine but not to the repository that
	// published it, and the commit the tag resolved to is the only evidence a
	// later run has that the tag still names what it named before. Entry.Tag,
	// rather than an absent commit, is what distinguishes a release pin from an
	// intermediate one.
	Commit string `json:"commit"`
}

// Entry is the resolved staging versions of one source commit.
type Entry struct {
	// Source is the source commit these versions were resolved for.
	Source string `json:"source"`
	// Tag is the upstream release tag when the source commit is exactly a tagged
	// release, and empty for an intermediate commit. It is what distinguishes the
	// two resolution paths.
	Tag string `json:"tag,omitempty"`
	// Modules are the pinned staging modules, sorted by path.
	Modules []ModuleVersion `json:"modules"`
}

// Index caches the staging versions resolved for each source commit.
//
// Resolving a version costs a subprocess and, for a pseudo-version, a proxy
// round trip, so a replay that walks thousands of source commits would otherwise
// pay for the same answer repeatedly. The cache is keyed by source commit
// because that is what determines the answer: the same source commit always maps
// onto the same staging commits, and therefore onto the same versions.
//
// Everything entering the index is validated and everything leaving it is
// copied. The contents are read back from a file an earlier run wrote, which
// makes them input rather than something this process computed, and a caller
// holding a slice into the index could otherwise change a cached answer in
// place.
type Index struct {
	entries map[string]Entry
}

// NewIndex returns an empty index.
func NewIndex() *Index {
	return &Index{entries: make(map[string]Entry)}
}

// Len reports how many source commits the index covers.
func (i *Index) Len() int {
	if i == nil {
		return 0
	}
	return len(i.entries)
}

// Lookup reports the entry recorded for a source commit.
//
// The returned entry owns its module slice, so a caller may keep or modify it
// without changing what the index holds.
func (i *Index) Lookup(source string) (Entry, bool) {
	if i == nil {
		return Entry{}, false
	}
	entry, ok := i.entries[source]
	if !ok {
		return Entry{}, false
	}
	entry.Modules = slices.Clone(entry.Modules)
	return entry, true
}

// Put records the versions resolved for one source commit.
//
// Recording the same entry twice is accepted so a resumed run can replay what it
// already knows. Recording a different answer for a source commit already in the
// index is refused: the mapping is a function of the source commit, so a second
// answer means one of the two was resolved against a different staging history,
// and silently keeping either would publish a pin nothing can reproduce.
func (i *Index) Put(entry Entry) error {
	// A nil index is a programming error rather than an empty cache, and it is
	// reported instead of silently discarding a resolution that cost a proxy
	// round trip. A zero value index is a different case: it is what a struct
	// literal produces, so it is filled in rather than refused.
	if i == nil {
		return errors.New("version index: no index to record into")
	}
	normalized, err := normalizeEntry(entry)
	if err != nil {
		return err
	}
	if i.entries == nil {
		i.entries = make(map[string]Entry)
	}
	if existing, ok := i.entries[normalized.Source]; ok {
		if entriesEqual(existing, normalized) {
			return nil
		}
		return fmt.Errorf("version index: source %s is already recorded with different versions", normalized.Source)
	}
	i.entries[normalized.Source] = normalized
	return nil
}

// Entries reports every entry, sorted by source commit, so a serialized index
// and any report built from one are byte identical across runs. Each entry owns
// its module slice.
func (i *Index) Entries() []Entry {
	if i == nil {
		return nil
	}
	entries := slices.Collect(maps.Values(i.entries))
	for index := range entries {
		entries[index].Modules = slices.Clone(entries[index].Modules)
	}
	slices.SortFunc(entries, func(a, b Entry) int { return cmp.Compare(a.Source, b.Source) })
	return entries
}

// normalizeEntry validates one entry and returns it in canonical order.
func normalizeEntry(entry Entry) (Entry, error) {
	if err := gitgraph.ValidateSHA(entry.Source); err != nil {
		return Entry{}, fmt.Errorf("version index: source commit: %w", err)
	}
	if entry.Tag != "" {
		if err := ValidateExactVersion(entry.Tag); err != nil {
			return Entry{}, fmt.Errorf("version index: source %s: release tag: %w", entry.Source, err)
		}
	}
	if len(entry.Modules) == 0 {
		return Entry{}, fmt.Errorf("version index: source %s records no modules", entry.Source)
	}

	modules := slices.Clone(entry.Modules)
	slices.SortFunc(modules, func(a, b ModuleVersion) int { return cmp.Compare(a.Path, b.Path) })
	for index, pinned := range modules {
		if err := module.CheckPath(pinned.Path); err != nil {
			return Entry{}, fmt.Errorf("version index: source %s: module path: %w", entry.Source, err)
		}
		if index > 0 && modules[index-1].Path == pinned.Path {
			return Entry{}, fmt.Errorf("version index: source %s records module %s twice", entry.Source, pinned.Path)
		}
		if err := ValidateExactVersion(pinned.Version); err != nil {
			return Entry{}, fmt.Errorf("version index: source %s: module %s: %w", entry.Source, pinned.Path, err)
		}
		// Check pairs the path with the version, which is what catches a major
		// version that disagrees with the path.
		if err := module.Check(pinned.Path, pinned.Version); err != nil {
			return Entry{}, fmt.Errorf("version index: source %s: %w", entry.Source, err)
		}
		// Every pin names a commit, whether it came from a tag or from an
		// intermediate mapping, so one without a commit records a version nothing
		// can later re-verify.
		if err := gitgraph.ValidateSHA(pinned.Commit); err != nil {
			return Entry{}, fmt.Errorf("version index: source %s: module %s staging commit: %w", entry.Source, pinned.Path, err)
		}
		// A release entry pins every staging module at the one tag the release
		// maps onto, because the staging repositories are tagged in lockstep. The
		// mapped tag is not compared against Entry.Tag, which is the upstream
		// release tag and deliberately a different version: v1.36.1 of the source
		// is v0.36.1 of every staging module. What is checked is that the entry
		// does not mix the two resolution paths, since a cache that did could
		// answer a release query with an intermediate pin.
		if entry.Tag != "" {
			if module.IsPseudoVersion(pinned.Version) {
				return Entry{}, fmt.Errorf("version index: source %s is tagged %s but pins %s at the pseudo-version %s", entry.Source, entry.Tag, pinned.Path, pinned.Version)
			}
			if pinned.Version != modules[0].Version {
				return Entry{}, fmt.Errorf("version index: source %s is tagged %s but pins %s at %s and %s at %s", entry.Source, entry.Tag, modules[0].Path, modules[0].Version, pinned.Path, pinned.Version)
			}
		}
	}
	entry.Modules = modules
	return entry, nil
}

// entriesEqual reports whether two normalized entries record the same answer.
func entriesEqual(a, b Entry) bool {
	return a.Source == b.Source && a.Tag == b.Tag && slices.Equal(a.Modules, b.Modules)
}
