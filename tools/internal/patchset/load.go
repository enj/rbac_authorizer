package patchset

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/enj/soapbox/tools/internal/config"
)

// Load reads the ordered patch series a profile configures.
//
// Every patch is read through an [os.Root] opened on the profile repository, so
// containment is enforced by the operating system for each read rather than by
// a string comparison performed once. A configured path that resolves outside
// the repository, including one that traverses a symbolic link, fails the read
// itself.
//
// Configured order is authoring order and is preserved. Each patch keeps its
// configured file path as its identifier, which is what conflict reports and
// provenance records name.
func Load(ctx context.Context, repoRoot string, entries []config.Patch) ([]Patch, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("load patches: %w", err)
	}
	if len(entries) == 0 {
		return nil, nil
	}
	root, err := os.OpenRoot(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("open profile repository: %w", err)
	}
	defer root.Close()

	patches := make([]Patch, 0, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("load patch %q: %w", entry.File, err)
		}
		if err := config.ValidateRelPath(entry.File); err != nil {
			return nil, fmt.Errorf("load patch %q: %w", entry.File, err)
		}
		diff, err := root.ReadFile(filepath.FromSlash(entry.File))
		if err != nil {
			return nil, fmt.Errorf("load patch %q: %w", entry.File, err)
		}
		if blank(diff) {
			return nil, fmt.Errorf("load patch %q: %w", entry.File, ErrEmptyPatch)
		}
		patches = append(patches, Patch{
			ID:       entry.File,
			Diff:     diff,
			Since:    entry.Since,
			Until:    entry.Until,
			Branches: append([]string(nil), entry.Branches...),
		})
	}
	if err := validateSeries(patches); err != nil {
		return nil, fmt.Errorf("load patches: %w", err)
	}
	return patches, nil
}
