package config

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// ValidateImportPath checks that p is a syntactically valid Go import path.
func ValidateImportPath(p string) error {
	switch {
	case p == "":
		return errors.New("import path must not be empty")
	case strings.TrimSpace(p) != p:
		return errors.New("import path must not have leading or trailing space")
	case strings.HasPrefix(p, "/"), strings.HasSuffix(p, "/"):
		return errors.New("import path must not start or end with a slash")
	}
	for _, elem := range strings.Split(p, "/") {
		if err := validatePathElement(elem); err != nil {
			return fmt.Errorf("import path %q: %w", p, err)
		}
	}
	return nil
}

// ValidateModulePath checks that p is a valid module path with a domain like
// first element.
func ValidateModulePath(p string) error {
	if err := ValidateImportPath(p); err != nil {
		return err
	}
	first, _, _ := strings.Cut(p, "/")
	if !strings.Contains(first, ".") {
		return fmt.Errorf("module path %q must start with a domain name element", p)
	}
	return nil
}

// ParseSymbolRef splits a fully qualified symbol reference such as
// k8s.io/kubernetes/pkg/registry/rbac/validation.RoleGetter into its package
// path and symbol name.
func ParseSymbolRef(ref string) (pkgPath, name string, err error) {
	if ref == "" {
		return "", "", errors.New("symbol reference must not be empty")
	}
	tail := ref
	prefix := ""
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		prefix, tail = ref[:i+1], ref[i+1:]
	}
	dot := strings.LastIndex(tail, ".")
	if dot <= 0 || dot == len(tail)-1 {
		return "", "", fmt.Errorf("symbol reference %q must be <import path>.<Name>", ref)
	}
	pkgPath = prefix + tail[:dot]
	name = tail[dot+1:]
	if err := ValidateImportPath(pkgPath); err != nil {
		return "", "", err
	}
	if err := ValidateIdent(name); err != nil {
		return "", "", fmt.Errorf("symbol reference %q: %w", ref, err)
	}
	return pkgPath, name, nil
}

// ValidateIdent checks that name is a Go identifier.
func ValidateIdent(name string) error {
	if name == "" {
		return errors.New("identifier must not be empty")
	}
	for i, r := range name {
		switch {
		case unicode.IsLetter(r), r == '_':
		case unicode.IsDigit(r) && i > 0:
		default:
			return fmt.Errorf("identifier %q contains unsupported character %q", name, r)
		}
	}
	return nil
}

// ValidateExportedIdent checks that name is an exported Go identifier.
func ValidateExportedIdent(name string) error {
	if err := ValidateIdent(name); err != nil {
		return err
	}
	if r := []rune(name)[0]; !unicode.IsUpper(r) {
		return fmt.Errorf("identifier %q must be exported", name)
	}
	return nil
}

// ValidateEnvName checks that name is a conventional environment variable name.
// Only names are configurable. Secret values never appear in configuration.
func ValidateEnvName(name string) error {
	if name == "" {
		return errors.New("environment variable name must not be empty")
	}
	for i, r := range name {
		switch {
		case r >= 'A' && r <= 'Z', r == '_':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return fmt.Errorf("environment variable name %q must be upper case with underscores", name)
		}
	}
	return nil
}

// ValidateEmail checks that value is a single conservative addr-spec.
func ValidateEmail(value string) error {
	local, domain, ok := strings.Cut(value, "@")
	if !ok || local == "" || domain == "" {
		return fmt.Errorf("email %q must be local@domain", value)
	}
	if strings.Count(value, "@") != 1 {
		return fmt.Errorf("email %q must contain exactly one @", value)
	}
	if strings.ContainsAny(value, " \t\r\n<>,;\"") {
		return fmt.Errorf("email %q contains unsupported characters", value)
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("email %q contains control characters", value)
		}
	}
	if !strings.Contains(domain, ".") || strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") {
		return fmt.Errorf("email %q has an unsupported domain", value)
	}
	return nil
}

// ValidateIdentityName checks a Git author or committer name. Git trims
// surrounding whitespace and treats angle brackets and newlines as identity
// syntax, so a name that Git would rewrite is rejected rather than silently
// changed on the way into a commit object.
func ValidateIdentityName(name string) error {
	switch {
	case name == "":
		return errors.New("identity name must not be empty")
	case strings.TrimSpace(name) != name:
		return fmt.Errorf("identity name %q must not have leading or trailing space", name)
	case strings.ContainsAny(name, "<>"):
		return fmt.Errorf("identity name %q must not contain angle brackets", name)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("identity name %q must not contain control characters", name)
		}
	}
	return nil
}

// ValidateHexSHA checks that value is a lower case Git object name.
func ValidateHexSHA(value string) error {
	if len(value) != 40 && len(value) != 64 {
		return fmt.Errorf("object name %q must be 40 or 64 hexadecimal characters", value)
	}
	for _, r := range value {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f':
		default:
			return fmt.Errorf("object name %q must be lower case hexadecimal", value)
		}
	}
	return nil
}

// ValidateRepositorySlug checks that value is an owner/name repository slug.
func ValidateRepositorySlug(value string) error {
	owner, name, ok := strings.Cut(value, "/")
	if !ok {
		return fmt.Errorf("repository %q must be owner/name", value)
	}
	if strings.Contains(name, "/") {
		return fmt.Errorf("repository %q must be owner/name", value)
	}
	for _, part := range []string{owner, name} {
		if err := validatePathElement(part); err != nil {
			return fmt.Errorf("repository %q: %w", value, err)
		}
	}
	return nil
}
