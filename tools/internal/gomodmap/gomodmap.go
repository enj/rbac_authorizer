// Package gomodmap aligns the staging modules of one Kubernetes source commit
// onto real module versions a generated module can require.
//
// Kubernetes builds its own staging modules out of its own tree. The root
// go.mod requires each of them at a placeholder version and then replaces that
// requirement with a relative directory, so the version upstream writes down
// carries no information at all. A module extracted from that tree cannot copy
// those requirements: it has no staging directory to replace them with, so it
// has to name the published staging module version that holds the same code.
//
// Finding that version is the whole job of this package, and it has two shapes.
// At a release tag the answer is arithmetic, because the staging repositories
// are tagged in lockstep with the release. Between tags there is no tag to read,
// so the source commit is mapped onto the staging commit that carries it and the
// Go toolchain is asked what pseudo-version names that commit.
//
// Two rules keep the result honest.
//
// Nothing is constructed. A pseudo-version is never assembled from a timestamp
// and a short hash here, even though the format is public and stable. It is
// asked for, and the answer is checked against the commit it was supposed to
// describe, because a hand built version that is merely well formed will resolve
// to whatever the proxy already has under that name.
//
// Nothing is assumed about the source. The staging layout is read out of the
// source go.mod rather than from a list of repository names kept here, and every
// shape that layout is not is refused rather than worked around. A replacement
// this package does not recognise means the source moved, and stopping is the
// only response that cannot publish something upstream never built.
package gomodmap

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	"golang.org/x/mod/modfile"

	"github.com/enj/soapbox/tools/internal/gitcli"
)

// RootModulePath is the repository relative path of the source module file.
const RootModulePath = "go.mod"

// StagingDir is the repository relative directory holding the staging modules.
const StagingDir = "staging/src"

// stagingTarget is the only replacement target shape that marks a staging
// module. The leading "./" is required: a bare "staging/src/..." would be a
// module path rather than a directory, and the go command reads it as one.
const stagingTarget = "./" + StagingDir + "/"

// StagingVersion is the placeholder version every staging requirement carries
// upstream. The replacement supplies the code, so nothing ever resolves it, and
// upstream writes the same unusable version for all of them.
const StagingVersion = "v0.0.0"

// Requirement is one module requirement of the source module.
type Requirement struct {
	Path    string
	Version string
	// Indirect records whether upstream marked the requirement indirect. It is
	// carried through rather than recomputed because the generated module
	// requires the same modules for the same reasons, and go mod tidy is later
	// asked to confirm exactly that.
	Indirect bool
}

// StagingModule is a module the source commit provides from its own tree
// through a relative replacement rather than from the module proxy.
type StagingModule struct {
	// Path is the module path, such as k8s.io/api.
	Path string
	// Dir is the repository relative directory providing it, slash separated.
	Dir string
	// Required reports whether the root module also requires this staging
	// module.
	//
	// Not every staged module is one the root module builds against. Kubernetes
	// stages sample-cli-plugin and sample-controller so the tree publishes them,
	// but nothing in the root module imports them, so they carry a replacement
	// and no requirement. They are still staging modules, and an extraction that
	// dropped them would be unable to explain a replacement it had seen.
	Required bool
	// Indirect records upstream's own marking, and is meaningful only when
	// Required is set: a module with no requirement has no marking to carry.
	Indirect bool
}

// Godebug is one godebug default the source module sets.
type Godebug struct {
	Key   string
	Value string
}

// Retraction is one retract directive of the source module.
type Retraction struct {
	// Low and High bound the retracted version interval, equal for a single
	// version.
	Low, High string
	Rationale string
}

// RootModule is the parsed root go.mod of one Kubernetes source commit.
//
// Staging and External are disjoint and each is sorted by module path, so two
// runs over the same commit produce the same structure and a report built from
// it is reproducible.
type RootModule struct {
	// Path is the source module path, such as k8s.io/kubernetes.
	Path string
	// Go is the language version from the go directive.
	Go string
	// Toolchain is the toolchain directive, empty when the source sets none.
	Toolchain string
	// Godebug lists the source module's godebug defaults, sorted by key.
	Godebug []Godebug
	// Staging lists every module provided by the source tree itself, whether or
	// not the root module requires it.
	Staging []StagingModule
	// External lists every other requirement at its exact upstream version.
	External []Requirement
	// Tool lists the source module's tool directives, sorted. They are recorded
	// rather than translated: the extracted module builds no tools, and a
	// directive that vanished without being named would leave nothing to explain
	// why the source's build list is larger than the generated one.
	Tool []string
	// Ignore lists the source module's ignore directives, sorted. They scope
	// which directories belong to the module's file tree and never change
	// version selection, so they are recorded and not carried.
	Ignore []string
	// Retract lists the source module's retractions. They are statements about
	// the source module's own published versions, so they say nothing about the
	// generated module and are recorded rather than carried.
	Retract []Retraction
}

// StagingPaths reports the staging module paths in sorted order.
func (r *RootModule) StagingPaths() []string {
	paths := make([]string, len(r.Staging))
	for i, module := range r.Staging {
		paths[i] = module.Path
	}
	return paths
}

// StagingModuleOf reports the staging module with the given path.
func (r *RootModule) StagingModuleOf(modulePath string) (StagingModule, bool) {
	for _, module := range r.Staging {
		if module.Path == modulePath {
			return module, true
		}
	}
	return StagingModule{}, false
}

// ReadRootModule reads and parses the root go.mod of one source commit.
func ReadRootModule(ctx context.Context, git *gitcli.Runner, revision string) (*RootModule, error) {
	data, err := git.ReadBlob(ctx, gitcli.BlobOptions{Revision: revision, Path: RootModulePath})
	if err != nil {
		return nil, fmt.Errorf("source commit %s: %w", revision, err)
	}
	root, err := ParseRootModule(RootModulePath, data)
	if err != nil {
		return nil, fmt.Errorf("source commit %s: %w", revision, err)
	}
	return root, nil
}

// ParseRootModule parses the bytes of a Kubernetes root go.mod.
//
// Every replacement must be a staging replacement, because the generated module
// carries none: an external replacement silently changes which code a version
// means, so copying the version and dropping the replacement would produce a
// module that builds something upstream never built.
//
// A staging module that the root module also requires must carry the
// placeholder version, which is the proof that upstream never expected the
// requirement to resolve against a proxy. A staging module with no requirement
// is normal rather than an error: Kubernetes stages modules it publishes but
// does not build against, and those have no version to check.
func ParseRootModule(name string, data []byte) (*RootModule, error) {
	parsed, err := modfile.Parse(name, data, nil)
	if err != nil {
		return nil, fmt.Errorf("root go.mod: %w", err)
	}
	if parsed.Module == nil || parsed.Module.Mod.Path == "" {
		return nil, errors.New("root go.mod: a module directive is required")
	}
	if parsed.Go == nil || parsed.Go.Version == "" {
		return nil, errors.New("root go.mod: a go directive is required")
	}
	// An exclude removes a version from selection. The generated module carries
	// no excludes, so minimal version selection there could choose the version
	// upstream refused, which is a silent divergence rather than a missing
	// annotation.
	if len(parsed.Exclude) > 0 {
		excluded := make([]string, len(parsed.Exclude))
		for i, exclude := range parsed.Exclude {
			excluded[i] = exclude.Mod.Path + " " + exclude.Mod.Version
		}
		slices.Sort(excluded)
		return nil, fmt.Errorf("root go.mod: exclude directives change version selection and cannot be carried: %s", strings.Join(excluded, ", "))
	}

	replacements, err := stagingReplacements(parsed.Replace)
	if err != nil {
		return nil, err
	}
	godebug, err := godebugDefaults(parsed.Godebug)
	if err != nil {
		return nil, err
	}

	root := &RootModule{
		Path:    parsed.Module.Mod.Path,
		Go:      parsed.Go.Version,
		Godebug: godebug,
	}
	if parsed.Toolchain != nil {
		root.Toolchain = parsed.Toolchain.Name
	}
	for _, tool := range parsed.Tool {
		root.Tool = append(root.Tool, tool.Path)
	}
	for _, ignore := range parsed.Ignore {
		root.Ignore = append(root.Ignore, ignore.Path)
	}
	for _, retract := range parsed.Retract {
		root.Retract = append(root.Retract, Retraction{
			Low:       retract.Low,
			High:      retract.High,
			Rationale: retract.Rationale,
		})
	}
	slices.Sort(root.Tool)
	slices.Sort(root.Ignore)

	// Staging modules are keyed by their replacement, so one is recorded whether
	// or not a requirement mentions it.
	staged := make(map[string]*StagingModule, len(replacements))
	for modulePath, dir := range replacements {
		staged[modulePath] = &StagingModule{Path: modulePath, Dir: dir}
	}

	required := make(map[string]bool, len(parsed.Require))
	for _, require := range parsed.Require {
		modulePath, version := require.Mod.Path, require.Mod.Version
		if required[modulePath] {
			return nil, fmt.Errorf("root go.mod: module %s is required more than once", modulePath)
		}
		required[modulePath] = true

		stagingModule, isStaging := staged[modulePath]
		if !isStaging {
			root.External = append(root.External, Requirement{
				Path:     modulePath,
				Version:  version,
				Indirect: require.Indirect,
			})
			continue
		}
		if version != StagingVersion {
			return nil, fmt.Errorf("root go.mod: staging module %s is required at %s rather than the placeholder %s", modulePath, version, StagingVersion)
		}
		stagingModule.Required = true
		stagingModule.Indirect = require.Indirect
	}

	root.Staging = make([]StagingModule, 0, len(staged))
	for _, stagingModule := range staged {
		root.Staging = append(root.Staging, *stagingModule)
	}
	slices.SortFunc(root.Staging, func(a, b StagingModule) int { return cmp.Compare(a.Path, b.Path) })
	slices.SortFunc(root.External, func(a, b Requirement) int { return cmp.Compare(a.Path, b.Path) })
	return root, nil
}

// stagingReplacements maps each replaced module onto the staging directory that
// provides it.
func stagingReplacements(replacements []*modfile.Replace) (map[string]string, error) {
	dirs := make(map[string]string, len(replacements))
	for _, replace := range replacements {
		dir, err := stagingDirOf(replace)
		if err != nil {
			return nil, fmt.Errorf("root go.mod: %w", err)
		}
		if _, duplicate := dirs[replace.Old.Path]; duplicate {
			return nil, fmt.Errorf("root go.mod: module %s is replaced more than once", replace.Old.Path)
		}
		dirs[replace.Old.Path] = dir
	}
	return dirs, nil
}

// stagingDirOf reports the staging directory one replacement points at.
//
// Every replacement in the source go.mod has to be one of these. A replacement
// that points anywhere else is refused rather than copied, because the generated
// module carries no replace directives at all: an external replacement silently
// changes which code a version means, so copying the version and dropping the
// replacement would produce a module that builds something upstream never built.
func stagingDirOf(replace *modfile.Replace) (string, error) {
	target := replace.New.Path
	switch {
	case replace.Old.Version != "":
		return "", fmt.Errorf("replacement of %s is pinned to version %s, which the staging layout never is", replace.Old.Path, replace.Old.Version)
	case replace.New.Version != "":
		return "", fmt.Errorf("replacement of %s points at module %s %s rather than at the staging tree", replace.Old.Path, target, replace.New.Version)
	case !strings.HasPrefix(target, stagingTarget):
		return "", fmt.Errorf("replacement of %s points at %q rather than below %q", replace.Old.Path, target, stagingTarget)
	}
	// The staging directory is named after the module it provides. Checking the
	// whole target rather than only its prefix is what makes the mapping from a
	// module to a subtree exact, so a replacement cannot serve one module's code
	// out of another module's directory.
	if want := stagingTarget + replace.Old.Path; target != want {
		return "", fmt.Errorf("replacement of %s points at %q rather than at %q", replace.Old.Path, target, want)
	}
	return path.Join(StagingDir, replace.Old.Path), nil
}

// godebugDefaults collects the godebug directives in sorted order.
//
// The generated module inherits these, and a build that honours a different set
// of godebug defaults than upstream is a behaviour change that no test of the
// extracted code would catch.
func godebugDefaults(directives []*modfile.Godebug) ([]Godebug, error) {
	if len(directives) == 0 {
		return nil, nil
	}
	godebug := make([]Godebug, 0, len(directives))
	seen := make(map[string]bool, len(directives))
	for _, directive := range directives {
		if seen[directive.Key] {
			return nil, fmt.Errorf("root go.mod: godebug %s is set more than once", directive.Key)
		}
		seen[directive.Key] = true
		godebug = append(godebug, Godebug{Key: directive.Key, Value: directive.Value})
	}
	slices.SortFunc(godebug, func(a, b Godebug) int { return cmp.Compare(a.Key, b.Key) })
	return godebug, nil
}
