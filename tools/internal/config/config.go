// Package config decodes, normalizes, and validates the versioned soapbox.yaml
// extraction profile.
//
// Decoding is strict. Unknown fields, duplicate keys, and multiple YAML
// documents are rejected so a profile can never silently lose meaning between
// engine versions. Normalization is deterministic because later phases hash the
// output affecting subset of a profile to detect control plane changes.
package config

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"

	"gopkg.in/yaml.v3"
)

// SchemaVersion is the only soapbox.yaml schema version this engine accepts.
const SchemaVersion = 1

// DefaultFileName is the conventional profile file name in a repository root.
const DefaultFileName = "soapbox.yaml"

// Config is a complete extraction profile.
type Config struct {
	Version      int          `yaml:"version"`
	Source       Source       `yaml:"source"`
	Destination  Destination  `yaml:"destination"`
	Packages     Packages     `yaml:"packages"`
	Prune        Prune        `yaml:"prune"`
	Deny         Deny         `yaml:"deny"`
	Closure      Closure      `yaml:"closure"`
	Types        Types        `yaml:"types"`
	Dependencies Dependencies `yaml:"dependencies"`
	Patches      []Patch      `yaml:"patches"`
	Facade       Facade       `yaml:"facade"`
	Release      Release      `yaml:"release"`
	Commit       Commit       `yaml:"commit"`
	Vanity       Vanity       `yaml:"vanity"`
	GitHubApp    GitHubApp    `yaml:"githubApp"`
	Determinism  Determinism  `yaml:"determinism"`
}

// Source describes the upstream repository and the refs the engine tracks.
type Source struct {
	Repository   string `yaml:"repository"`
	ImportPrefix string `yaml:"importPrefix"`
	Refs         Refs   `yaml:"refs"`
}

// Refs selects the upstream history the engine replays.
type Refs struct {
	MinimumRelease     string   `yaml:"minimumRelease"`
	IncludePrereleases bool     `yaml:"includePrereleases"`
	Branches           []string `yaml:"branches"`
	AnchorCommit       string   `yaml:"anchorCommit"`
}

// Destination describes the generated module and the repository that holds it.
type Destination struct {
	Module            string `yaml:"module"`
	Repository        string `yaml:"repository"`
	Remote            string `yaml:"remote"`
	Branch            string `yaml:"branch"`
	StateRef          string `yaml:"stateRef"`
	ProgressRefPrefix string `yaml:"progressRefPrefix"`
	RootPackage       string `yaml:"rootPackage"`
	InternalPrefix    string `yaml:"internalPrefix"`
}

// Packages selects the upstream package roots that seed the closure.
type Packages struct {
	Roots      []string `yaml:"roots"`
	Recursive  bool     `yaml:"recursive"`
	AssetGlobs []string `yaml:"assetGlobs"`
}

// Prune lists the exact files removed from, and required to remain in, the
// materialized closure. Entries are files, never directories or globs.
type Prune struct {
	Files    []string `yaml:"files"`
	Required []string `yaml:"required"`
}

// Deny lists exact import paths that may never reenter the closure.
type Deny struct {
	Imports []string `yaml:"imports"`
}

// Closure bounds the materialized package set.
type Closure struct {
	IncludeTests bool          `yaml:"includeTests"`
	Limits       ClosureLimits `yaml:"limits"`
	Golden       string        `yaml:"golden"`
}

// ClosureLimits are observational publication gates. They never change output
// bytes, so they stay out of the replay profile hash.
type ClosureLimits struct {
	MaxPackages      int `yaml:"maxPackages"`
	MaxFiles         int `yaml:"maxFiles"`
	MaxNonTestLines  int `yaml:"maxNonTestLines"`
	MaxPackageGrowth int `yaml:"maxPackageGrowth"`
}

// Types selects the internal to public API substitution policy.
type Types struct {
	Policy string     `yaml:"policy"`
	Pairs  []TypePair `yaml:"pairs"`
}

// TypePair records a verified internal to external API package pairing.
type TypePair struct {
	Internal string `yaml:"internal"`
	External string `yaml:"external"`
}

// Dependencies decides whether staging packages may be copied.
type Dependencies struct {
	Policy       string               `yaml:"policy"`
	CopyPackages []string             `yaml:"copyPackages"`
	Gates        DependencyGates      `yaml:"gates"`
	Overrides    []DependencyOverride `yaml:"overrides"`
}

// DependencyGates holds the non-overridable correctness gates and the cost
// gates an override may relax.
type DependencyGates struct {
	Interoperability bool      `yaml:"interoperability"`
	GlobalState      bool      `yaml:"globalState"`
	Diamond          bool      `yaml:"diamond"`
	Cost             CostGates `yaml:"cost"`
}

// CostGates bound the size of an approved staging copy.
type CostGates struct {
	MaxCopiedPackages   int   `yaml:"maxCopiedPackages"`
	MaxCopiedLines      int   `yaml:"maxCopiedLines"`
	MaxGeneratedFiles   int   `yaml:"maxGeneratedFiles"`
	MaxDistinctLicenses int   `yaml:"maxDistinctLicenses"`
	MaxModuleZipBytes   int64 `yaml:"maxModuleZipBytes"`
}

// DependencyOverride relaxes exactly one cost gate for one candidate package.
type DependencyOverride struct {
	Package       string `yaml:"package"`
	Gate          string `yaml:"gate"`
	Justification string `yaml:"justification"`
	Approver      string `yaml:"approver"`
	ExpiresAfter  string `yaml:"expiresAfter"`
}

// Patch is one ordered unified diff with ancestry and branch selectors.
type Patch struct {
	File     string   `yaml:"file"`
	Since    string   `yaml:"since"`
	Until    string   `yaml:"until"`
	Branches []string `yaml:"branches"`
}

// Facade describes the curated public API of the generated module.
type Facade struct {
	Package             string               `yaml:"package"`
	File                string               `yaml:"file"`
	AssertionsFile      string               `yaml:"assertionsFile"`
	Exports             []Export             `yaml:"exports"`
	Aliases             []Alias              `yaml:"aliases"`
	InterfaceAssertions []InterfaceAssertion `yaml:"interfaceAssertions"`
}

// Export republishes an internal symbol under its upstream name.
type Export struct {
	Name   string `yaml:"name"`
	Kind   string `yaml:"kind"`
	Source string `yaml:"source"`
}

// Alias republishes an internal symbol under a different name, which is how
// upstream name collisions are resolved in a single facade package.
type Alias struct {
	Name   string `yaml:"name"`
	Kind   string `yaml:"kind"`
	Source string `yaml:"source"`
}

// InterfaceAssertion generates a compile-time implementation assertion.
type InterfaceAssertion struct {
	Type      string `yaml:"type"`
	Pointer   bool   `yaml:"pointer"`
	Interface string `yaml:"interface"`
}

// Release maps upstream release tags onto generated module tags.
type Release struct {
	Policy   string `yaml:"policy"`
	FirstTag string `yaml:"firstTag"`
}

// Commit describes generated commit identity. Generated commits preserve the
// upstream author and are never signed.
type Commit struct {
	AuthorPolicy string   `yaml:"authorPolicy"`
	Committer    Identity `yaml:"committer"`
	TrailerKey   string   `yaml:"trailerKey"`
	Sign         bool     `yaml:"sign"`
}

// Identity is a Git author or committer identity.
type Identity struct {
	Name  string `yaml:"name"`
	Email string `yaml:"email"`
}

// Vanity describes the go-import metadata page for the generated module.
type Vanity struct {
	Repository    string `yaml:"repository"`
	Path          string `yaml:"path"`
	ImportPath    string `yaml:"importPath"`
	RepositoryURL string `yaml:"repositoryURL"`
	ProbeURL      string `yaml:"probeURL"`
}

// GitHubApp names the environment variables that carry App credentials.
// Only names live in configuration. Values never do.
type GitHubApp struct {
	AppIDEnv          string `yaml:"appIDEnv"`
	InstallationIDEnv string `yaml:"installationIDEnv"`
	PrivateKeyEnv     string `yaml:"privateKeyEnv"`
	APIBaseURL        string `yaml:"apiBaseURL"`
}

// Determinism pins the formatting toolchain and the gated backfill chunk size.
type Determinism struct {
	Toolchain string `yaml:"toolchain"`
	ChunkSize int    `yaml:"chunkSize"`
}

// Load reads, decodes, normalizes, and validates the profile stored at path.
func Load(ctx context.Context, path string) (*Config, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("profile load: %w", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read profile: %w", err)
	}
	cfg, err := Decode(data)
	if err != nil {
		return nil, fmt.Errorf("profile %s: %w", path, err)
	}
	return cfg, nil
}

// DecodeError reports profile content that could not be decoded: unknown or
// duplicated fields, malformed YAML, an empty document, or more than one
// document. It is distinct from a filesystem failure, because a profile the
// operator can see and fix is a policy problem rather than a runtime one.
type DecodeError struct {
	Reason string
	Err    error
}

// Error renders the decode failure.
func (e *DecodeError) Error() string {
	if e.Err == nil {
		return "decode profile: " + e.Reason
	}
	return "decode profile: " + e.Err.Error()
}

// Unwrap exposes the underlying YAML error.
func (e *DecodeError) Unwrap() error { return e.Err }

// Decode parses, normalizes, and validates profile bytes. Content problems are
// reported as *DecodeError or *ValidationError so callers can separate them
// from input and output failures.
func Decode(data []byte) (*Config, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, &DecodeError{Reason: "document is empty"}
		}
		return nil, &DecodeError{Reason: "malformed document", Err: err}
	}

	var extra yaml.Node
	switch err := dec.Decode(&extra); {
	case err == nil:
		return nil, &DecodeError{Reason: "multiple YAML documents are not supported"}
	case errors.Is(err, io.EOF):
	default:
		return nil, &DecodeError{Reason: "malformed document", Err: err}
	}

	cfg.normalize()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Canonical renders the normalized profile as deterministic YAML bytes.
func (c *Config) Canonical() ([]byte, error) {
	return encodeYAML(c)
}

// encodeYAML renders a value as deterministic YAML bytes.
func encodeYAML(value any) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(value); err != nil {
		return nil, fmt.Errorf("encode profile: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("encode profile: %w", err)
	}
	return buf.Bytes(), nil
}

// profile is the explicit allowlist of configuration that changes generated
// bytes. It is a separate type rather than a copy of Config with fields cleared,
// so a field added to the schema is excluded until someone decides it belongs
// here, and so no slice is shared by accident.
//
// Everything operational is absent: where refs are published, which branches are
// discovered, chunk sizes, GitHub App environment names, vanity locations, and
// observational closure limits and goldens. Changing any of them must not start
// a new profile epoch, because none of them changes a single generated byte.
type profile struct {
	Version        int          `yaml:"version"`
	SourceRepo     string       `yaml:"sourceRepository"`
	ImportPrefix   string       `yaml:"sourceImportPrefix"`
	AnchorCommit   string       `yaml:"sourceAnchorCommit"`
	Module         string       `yaml:"destinationModule"`
	RootPackage    string       `yaml:"destinationRootPackage"`
	InternalPrefix string       `yaml:"destinationInternalPrefix"`
	Packages       Packages     `yaml:"packages"`
	Prune          Prune        `yaml:"prune"`
	Deny           Deny         `yaml:"deny"`
	IncludeTests   bool         `yaml:"includeTests"`
	Types          Types        `yaml:"types"`
	DependencyPlan dependencies `yaml:"dependencies"`
	Patches        []Patch      `yaml:"patches"`
	Facade         Facade       `yaml:"facade"`
	Release        Release      `yaml:"release"`
	Commit         Commit       `yaml:"commit"`
	Toolchain      string       `yaml:"toolchain"`
}

// dependencies holds the copy decisions that change generated bytes. Gates and
// overrides govern whether a decision is allowed, not what is written.
type dependencies struct {
	Policy       string   `yaml:"policy"`
	CopyPackages []string `yaml:"copyPackages"`
}

// ProfileBytes renders the output affecting subset of the profile. Later phases
// hash these bytes to decide when a control plane change starts a new epoch.
func (c *Config) ProfileBytes() ([]byte, error) {
	view := profile{
		Version:        c.Version,
		SourceRepo:     c.Source.Repository,
		ImportPrefix:   c.Source.ImportPrefix,
		AnchorCommit:   c.Source.Refs.AnchorCommit,
		Module:         c.Destination.Module,
		RootPackage:    c.Destination.RootPackage,
		InternalPrefix: c.Destination.InternalPrefix,
		Packages: Packages{
			Roots:      slices.Clone(c.Packages.Roots),
			Recursive:  c.Packages.Recursive,
			AssetGlobs: slices.Clone(c.Packages.AssetGlobs),
		},
		Prune: Prune{
			Files:    slices.Clone(c.Prune.Files),
			Required: slices.Clone(c.Prune.Required),
		},
		Deny:         Deny{Imports: slices.Clone(c.Deny.Imports)},
		IncludeTests: c.Closure.IncludeTests,
		Types: Types{
			Policy: c.Types.Policy,
			Pairs:  slices.Clone(c.Types.Pairs),
		},
		DependencyPlan: dependencies{
			Policy:       c.Dependencies.Policy,
			CopyPackages: slices.Clone(c.Dependencies.CopyPackages),
		},
		Patches: clonePatches(c.Patches),
		Facade: Facade{
			Package:             c.Facade.Package,
			File:                c.Facade.File,
			AssertionsFile:      c.Facade.AssertionsFile,
			Exports:             slices.Clone(c.Facade.Exports),
			Aliases:             slices.Clone(c.Facade.Aliases),
			InterfaceAssertions: slices.Clone(c.Facade.InterfaceAssertions),
		},
		Release:   c.Release,
		Commit:    c.Commit,
		Toolchain: c.Determinism.Toolchain,
	}
	return encodeYAML(view)
}

// clonePatches copies the ordered patch list and its per patch branch lists.
func clonePatches(patches []Patch) []Patch {
	out := make([]Patch, len(patches))
	for i, patch := range patches {
		out[i] = patch
		out[i].Branches = slices.Clone(patch.Branches)
	}
	return out
}

// normalize orders set-like collections and lowercases URL hosts so that two
// semantically equal profiles produce identical canonical bytes. Patch order is
// meaningful and is never reordered.
func (c *Config) normalize() {
	c.Source.Repository = normalizeURLHost(c.Source.Repository)
	c.Destination.Remote = normalizeURLHost(c.Destination.Remote)
	c.Vanity.RepositoryURL = normalizeURLHost(c.Vanity.RepositoryURL)
	c.Vanity.ProbeURL = normalizeURLHost(c.Vanity.ProbeURL)
	c.GitHubApp.APIBaseURL = normalizeURLHost(c.GitHubApp.APIBaseURL)

	sort.Strings(c.Source.Refs.Branches)
	sort.Strings(c.Packages.Roots)
	sort.Strings(c.Packages.AssetGlobs)
	sort.Strings(c.Prune.Files)
	sort.Strings(c.Prune.Required)
	sort.Strings(c.Deny.Imports)
	sort.Strings(c.Dependencies.CopyPackages)

	sort.SliceStable(c.Types.Pairs, func(i, j int) bool {
		if c.Types.Pairs[i].Internal != c.Types.Pairs[j].Internal {
			return c.Types.Pairs[i].Internal < c.Types.Pairs[j].Internal
		}
		return c.Types.Pairs[i].External < c.Types.Pairs[j].External
	})
	sort.SliceStable(c.Dependencies.Overrides, func(i, j int) bool {
		if c.Dependencies.Overrides[i].Package != c.Dependencies.Overrides[j].Package {
			return c.Dependencies.Overrides[i].Package < c.Dependencies.Overrides[j].Package
		}
		return c.Dependencies.Overrides[i].Gate < c.Dependencies.Overrides[j].Gate
	})
	sort.SliceStable(c.Facade.Exports, func(i, j int) bool {
		return c.Facade.Exports[i].Name < c.Facade.Exports[j].Name
	})
	sort.SliceStable(c.Facade.Aliases, func(i, j int) bool {
		return c.Facade.Aliases[i].Name < c.Facade.Aliases[j].Name
	})
	sort.SliceStable(c.Facade.InterfaceAssertions, func(i, j int) bool {
		a, b := c.Facade.InterfaceAssertions[i], c.Facade.InterfaceAssertions[j]
		if a.Type != b.Type {
			return a.Type < b.Type
		}
		return a.Interface < b.Interface
	})
	for i := range c.Patches {
		sort.Strings(c.Patches[i].Branches)
	}
}
