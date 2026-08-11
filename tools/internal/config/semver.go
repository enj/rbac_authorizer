package config

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ReleasePolicyV1ToV0 maps upstream v1.X.Y tags onto generated v0.X.Y tags,
// which is how Kubernetes staging repositories are versioned.
const ReleasePolicyV1ToV0 = "v1-to-v0"

// Semver is a parsed v prefixed semantic version without build metadata.
type Semver struct {
	Major      int
	Minor      int
	Patch      int
	Prerelease string
}

// String renders the version in its original v prefixed form.
func (v Semver) String() string {
	s := fmt.Sprintf("v%d.%d.%d", v.Major, v.Minor, v.Patch)
	if v.Prerelease != "" {
		s += "-" + v.Prerelease
	}
	return s
}

// ParseSemver parses a v prefixed semantic version. Build metadata is rejected
// because module tags may not carry it.
func ParseSemver(value string) (Semver, error) {
	if !strings.HasPrefix(value, "v") {
		return Semver{}, fmt.Errorf("version %q must start with v", value)
	}
	if strings.Contains(value, "+") {
		return Semver{}, fmt.Errorf("version %q must not carry build metadata", value)
	}
	core, prerelease, hasPre := strings.Cut(value[1:], "-")
	fields := strings.Split(core, ".")
	if len(fields) != 3 {
		return Semver{}, fmt.Errorf("version %q must be vMAJOR.MINOR.PATCH", value)
	}
	nums := make([]int, 3)
	for i, field := range fields {
		n, err := parseNumericID(field)
		if err != nil {
			return Semver{}, fmt.Errorf("version %q: %w", value, err)
		}
		nums[i] = n
	}
	if hasPre {
		if err := validatePrerelease(prerelease); err != nil {
			return Semver{}, fmt.Errorf("version %q: %w", value, err)
		}
	}
	return Semver{Major: nums[0], Minor: nums[1], Patch: nums[2], Prerelease: prerelease}, nil
}

// ParseMinorSeries parses a vMAJOR.MINOR release series such as v1.37.
func ParseMinorSeries(value string) (major, minor int, err error) {
	if !strings.HasPrefix(value, "v") {
		return 0, 0, fmt.Errorf("release series %q must start with v", value)
	}
	fields := strings.Split(value[1:], ".")
	if len(fields) != 2 {
		return 0, 0, fmt.Errorf("release series %q must be vMAJOR.MINOR", value)
	}
	if major, err = parseNumericID(fields[0]); err != nil {
		return 0, 0, fmt.Errorf("release series %q: %w", value, err)
	}
	if minor, err = parseNumericID(fields[1]); err != nil {
		return 0, 0, fmt.Errorf("release series %q: %w", value, err)
	}
	return major, minor, nil
}

// MapReleaseTag maps an upstream release tag onto the generated module tag.
func MapReleaseTag(policy, sourceTag string) (string, error) {
	source, err := ParseSemver(sourceTag)
	if err != nil {
		return "", err
	}
	switch policy {
	case ReleasePolicyV1ToV0:
		if source.Major != 1 {
			return "", fmt.Errorf("release policy %s requires a v1 source tag, got %q", policy, sourceTag)
		}
		mapped := source
		mapped.Major = 0
		return mapped.String(), nil
	default:
		return "", fmt.Errorf("unsupported release policy %q", policy)
	}
}

// parseNumericID parses a semantic version numeric identifier, which may not
// carry leading zeros.
func parseNumericID(field string) (int, error) {
	if field == "" {
		return 0, errors.New("numeric identifier must not be empty")
	}
	if len(field) > 1 && field[0] == '0' {
		return 0, fmt.Errorf("numeric identifier %q must not have a leading zero", field)
	}
	n, err := strconv.Atoi(field)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("numeric identifier %q is not a non-negative number", field)
	}
	return n, nil
}

// validatePrerelease checks the dot separated prerelease identifiers.
func validatePrerelease(prerelease string) error {
	if prerelease == "" {
		return errors.New("prerelease must not be empty")
	}
	for _, id := range strings.Split(prerelease, ".") {
		if id == "" {
			return errors.New("prerelease identifier must not be empty")
		}
		numeric := true
		for _, r := range id {
			switch {
			case r >= '0' && r <= '9':
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '-':
				numeric = false
			default:
				return fmt.Errorf("prerelease identifier %q contains unsupported character %q", id, r)
			}
		}
		if numeric && len(id) > 1 && id[0] == '0' {
			return fmt.Errorf("prerelease identifier %q must not have a leading zero", id)
		}
	}
	return nil
}
