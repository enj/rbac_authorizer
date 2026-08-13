package setup

import (
	"cmp"
	"fmt"
	"slices"

	"github.com/enj/soapbox/tools/internal/config"
)

// compose builds every file setup owns, in full, before anything is written.
//
// The payload is composed rather than copied. Nothing in the derived repository
// that setup produces is a template file with edits applied to it, because an
// edit is a function of what the template happened to contain and a composition
// is a function of the profile alone. That is what makes two runs over the same
// profile produce the same bytes.
func (r *run) compose(pin enginePin) error {
	cfg := r.opts.Config
	engineRequires, err := engineRequirements(r.opts.EngineMod)
	if err != nil {
		return policyErrorf("setup: tools go.mod: %w", err)
	}

	rootMod, err := composeRootGoMod(cfg.Destination.Module)
	if err != nil {
		return policyErrorf("setup: root go.mod: %w", err)
	}
	toolsMod, err := composeToolsGoMod(cfg.Destination.Module, pin, engineRequires)
	if err != nil {
		return policyErrorf("setup: tools go.mod: %w", err)
	}

	inputs := workflowInputs{
		branch:    cfg.Destination.Branch,
		goVersion: goVersionOf(cfg.Determinism.Toolchain),
		secrets:   secretNames(cfg),
	}
	if err := inputs.check(); err != nil {
		return policyErrorf("setup: %w", err)
	}

	r.payload = []composedFile{
		{path: rootGoModPath, contents: rootMod},
		{path: toolsGoModPath, contents: toolsMod},
		{path: toolsMainPath, contents: composeToolsMain()},
		{path: ciWorkflowPath, contents: composeCIWorkflow(inputs)},
		{path: syncWorkflowPath, contents: composeSyncWorkflow(inputs)},
	}

	sum, err := composeEngineSum(r.opts.EngineSum, pin, engineRequires)
	if err != nil {
		return policyErrorf("setup: %w", err)
	}
	if sum != nil {
		r.payload = append(r.payload, composedFile{path: toolsGoSumPath, contents: sum})
		r.composedSum = true
	} else {
		r.notices = append(r.notices, engineSumNotice)
	}

	slices.SortFunc(r.payload, func(a, b composedFile) int { return cmp.Compare(a.path, b.path) })
	return r.checkPayload()
}

// checkPayload refuses a payload path that could escape the repository or land
// somewhere other than where it reads.
//
// Every path here is a constant of this package, so the check can only fail
// after an edit to the allowlist. That is exactly when it is worth having: the
// allowlist is the security boundary, and a boundary nothing verifies is a
// comment.
func (r *run) checkPayload() error {
	seen := make(map[string]bool, len(r.payload))
	for _, file := range r.payload {
		if err := config.ValidateRelPath(file.path); err != nil {
			return fmt.Errorf("setup: payload path %q: %w", file.path, err)
		}
		if seen[file.path] {
			return fmt.Errorf("setup: payload path %q is composed twice", file.path)
		}
		seen[file.path] = true
	}
	return nil
}

// secretNames are the App secrets the publishing workflow passes to the engine,
// in the order the workflow renders them.
//
// They come from the profile rather than from a constant here because the engine
// reads them from the process environment by those names, and a workflow that
// exported different names would hand the engine an empty environment and fail
// at token minting rather than at configuration.
func secretNames(cfg *config.Config) []string {
	names := []string{
		cfg.GitHubApp.AppIDEnv,
		cfg.GitHubApp.InstallationIDEnv,
		cfg.GitHubApp.PrivateKeyEnv,
	}
	slices.Sort(names)
	return names
}
