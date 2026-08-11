package gitcli

import (
	"errors"
	"fmt"
	"strings"
)

// Ref validation sentinels.
var (
	// ErrForceRefspec reports a refspec that would allow a non fast forward
	// update. The engine publishes append only history, so there is no API that
	// can force a ref backwards.
	ErrForceRefspec = errors.New("refspec must not force a non fast forward update")
	// ErrDeleteRefspec reports a refspec that would delete a ref.
	ErrDeleteRefspec = errors.New("refspec must not delete a ref")
	// ErrFlagLikeArgument reports a value that git would parse as an option.
	ErrFlagLikeArgument = errors.New("value must not start with a dash")
)

// ValidateRefName checks a fully qualified ref name against the rules that
// git check-ref-format enforces.
func ValidateRefName(name string) error {
	if err := validateRefFormat(name); err != nil {
		return err
	}
	if !strings.Contains(name, "/") {
		return fmt.Errorf("ref name %q must be hierarchical, such as refs/heads/main", name)
	}
	return nil
}

// ValidateBranchName checks a short branch name such as master or release-1.36.
func ValidateBranchName(name string) error {
	if err := validateRefFormat(name); err != nil {
		return err
	}
	if name == "HEAD" {
		return errors.New("branch name must not be HEAD")
	}
	if strings.HasPrefix(name, "refs/") {
		return fmt.Errorf("branch name %q must be a short name", name)
	}
	return nil
}

// ValidatePushRefspec checks one push refspec. Leading plus signs, delete
// refspecs, and option like values are rejected because the publisher may only
// fast forward refs it already agreed to move.
func ValidatePushRefspec(spec string) error {
	switch {
	case spec == "":
		return errors.New("refspec must not be empty")
	case strings.HasPrefix(spec, "+"):
		return fmt.Errorf("refspec %q: %w", spec, ErrForceRefspec)
	case strings.HasPrefix(spec, "-"):
		return fmt.Errorf("refspec %q: %w", spec, ErrFlagLikeArgument)
	case strings.ContainsAny(spec, " \t\n\r"):
		return fmt.Errorf("refspec %q must not contain whitespace", spec)
	}
	source, destination, ok := strings.Cut(spec, ":")
	if !ok {
		return fmt.Errorf("refspec %q must be <source>:<destination>", spec)
	}
	if strings.Contains(destination, ":") {
		return fmt.Errorf("refspec %q must contain exactly one colon", spec)
	}
	if source == "" {
		return fmt.Errorf("refspec %q: %w", spec, ErrDeleteRefspec)
	}
	if destination == "" {
		return fmt.Errorf("refspec %q must name a destination ref", spec)
	}
	if source != "HEAD" {
		if err := validateRefFormat(source); err != nil {
			return fmt.Errorf("refspec source: %w", err)
		}
	}
	if err := ValidateRefName(destination); err != nil {
		return fmt.Errorf("refspec destination: %w", err)
	}
	return nil
}

// validateRefFormat applies the shared git check-ref-format rules.
func validateRefFormat(name string) error {
	switch {
	case name == "":
		return errors.New("ref name must not be empty")
	case name == "@":
		return errors.New("ref name must not be @")
	case strings.HasPrefix(name, "-"):
		return fmt.Errorf("ref name %q: %w", name, ErrFlagLikeArgument)
	case strings.HasPrefix(name, "/"), strings.HasSuffix(name, "/"):
		return fmt.Errorf("ref name %q must not start or end with a slash", name)
	case strings.HasSuffix(name, "."):
		return fmt.Errorf("ref name %q must not end with a dot", name)
	case strings.Contains(name, ".."):
		return fmt.Errorf("ref name %q must not contain consecutive dots", name)
	case strings.Contains(name, "@{"):
		return fmt.Errorf("ref name %q must not contain @{", name)
	case strings.Contains(name, "//"):
		return fmt.Errorf("ref name %q must not contain consecutive slashes", name)
	}
	for _, r := range name {
		switch {
		case r <= 0x20, r == 0x7f:
			return fmt.Errorf("ref name %q must not contain control characters or spaces", name)
		case r == '~', r == '^', r == ':', r == '?', r == '*', r == '[', r == '\\':
			return fmt.Errorf("ref name %q must not contain %q", name, r)
		}
	}
	for _, component := range strings.Split(name, "/") {
		switch {
		case component == "":
			return fmt.Errorf("ref name %q must not contain an empty component", name)
		case strings.HasPrefix(component, "."):
			return fmt.Errorf("ref name %q component %q must not start with a dot", name, component)
		case strings.HasSuffix(component, ".lock"):
			return fmt.Errorf("ref name %q component %q must not end with .lock", name, component)
		}
	}
	return nil
}

// validateArgument rejects values that git would interpret as an option.
func validateArgument(kind, value string) error {
	if value == "" {
		return fmt.Errorf("%s must not be empty", kind)
	}
	if strings.HasPrefix(value, "-") {
		return fmt.Errorf("%s %q: %w", kind, value, ErrFlagLikeArgument)
	}
	if strings.ContainsRune(value, '\x00') {
		return fmt.Errorf("%s %q must not contain a null byte", kind, value)
	}
	return nil
}
