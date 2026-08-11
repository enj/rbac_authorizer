package patchset

import (
	"context"
	"fmt"
	"slices"
)

// Select reports the patches that apply to one source commit, in configured
// order.
//
// A patch is selected when every configured selector accepts the target:
//
//   - Branches, when non-empty, must contain the target branch exactly.
//   - Since, when set, must be an ancestor of the target commit or the target
//     commit itself.
//   - Until, when set, must not be an ancestor of the target commit, which
//     makes the selected range half open at the upper end.
//
// Selectors are evaluated in configured order and the branch selector is
// evaluated first, so a branch scoped patch costs no ancestry query on the
// branches it does not target. The returned slice preserves configured order
// and holds deep copies, so application order is exactly authoring order and a
// caller cannot mutate the profile's series through it.
func Select(ctx context.Context, git Git, patches []Patch, target Target) ([]Patch, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("select patches: %w", err)
	}
	if git == nil {
		return nil, fmt.Errorf("select patches: %w", ErrNoGit)
	}
	if err := target.validate(); err != nil {
		return nil, fmt.Errorf("select patches: %w", err)
	}
	if err := validateSeries(patches); err != nil {
		return nil, fmt.Errorf("select patches: %w", err)
	}

	selected := make([]Patch, 0, len(patches))
	for _, patch := range patches {
		if len(patch.Branches) > 0 && !slices.Contains(patch.Branches, target.Branch) {
			continue
		}
		applies, err := inRange(ctx, git, patch, target)
		if err != nil {
			return nil, err
		}
		if applies {
			selected = append(selected, patch.Clone())
		}
	}
	return selected, nil
}

// inRange reports whether the target commit lies in the patch's half open
// ancestry range.
func inRange(ctx context.Context, git Git, patch Patch, target Target) (bool, error) {
	if patch.Since != "" {
		started, err := git.IsAncestor(ctx, patch.Since, target.Commit)
		if err != nil {
			return false, fmt.Errorf("select patch %q: since %q ancestry of %q: %w", patch.ID, patch.Since, target.Commit, err)
		}
		if !started {
			return false, nil
		}
	}
	if patch.Until != "" {
		ended, err := git.IsAncestor(ctx, patch.Until, target.Commit)
		if err != nil {
			return false, fmt.Errorf("select patch %q: until %q ancestry of %q: %w", patch.ID, patch.Until, target.Commit, err)
		}
		if ended {
			return false, nil
		}
	}
	return true, nil
}
