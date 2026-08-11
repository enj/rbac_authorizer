package config_test

import (
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/config"
)

func TestParseSemver(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    config.Semver
		wantErr string
	}{
		{name: "release", value: "v1.36.1", want: config.Semver{Major: 1, Minor: 36, Patch: 1}},
		{name: "zero patch", value: "v1.37.0", want: config.Semver{Major: 1, Minor: 37}},
		{
			name:  "alpha prerelease",
			value: "v1.37.0-alpha.1",
			want:  config.Semver{Major: 1, Minor: 37, Prerelease: "alpha.1"},
		},
		{
			name:  "release candidate",
			value: "v1.37.0-rc.0",
			want:  config.Semver{Major: 1, Minor: 37, Prerelease: "rc.0"},
		},
		{name: "no v prefix", value: "1.36.1", wantErr: "must start with v"},
		{name: "two components", value: "v1.36", wantErr: "vMAJOR.MINOR.PATCH"},
		{name: "four components", value: "v1.36.1.2", wantErr: "vMAJOR.MINOR.PATCH"},
		{name: "leading zero", value: "v1.06.1", wantErr: "leading zero"},
		{name: "build metadata", value: "v1.36.1+build", wantErr: "build metadata"},
		{name: "empty prerelease", value: "v1.36.1-", wantErr: "prerelease must not be empty"},
		{name: "prerelease leading zero", value: "v1.37.0-alpha.01", wantErr: "leading zero"},
		{name: "not numeric", value: "vX.Y.Z", wantErr: "not a non-negative number"},
		{name: "empty", value: "", wantErr: "must start with v"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := config.ParseSemver(test.value)
			switch {
			case test.wantErr == "":
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if got != test.want {
					t.Fatalf("ParseSemver(%q) = %+v, want %+v", test.value, got, test.want)
				}
				if got.String() != test.value {
					t.Fatalf("String() = %q, want %q", got.String(), test.value)
				}
			case err == nil:
				t.Fatalf("expected an error containing %q", test.wantErr)
			case !strings.Contains(err.Error(), test.wantErr):
				t.Fatalf("error %q does not contain %q", err, test.wantErr)
			}
		})
	}
}

func TestMapReleaseTag(t *testing.T) {
	tests := []struct {
		name    string
		policy  string
		tag     string
		want    string
		wantErr string
	}{
		{name: "first release", policy: config.ReleasePolicyV1ToV0, tag: "v1.36.1", want: "v0.36.1"},
		{name: "later minor", policy: config.ReleasePolicyV1ToV0, tag: "v1.37.0", want: "v0.37.0"},
		{name: "prerelease", policy: config.ReleasePolicyV1ToV0, tag: "v1.37.0-alpha.2", want: "v0.37.0-alpha.2"},
		{name: "major two source", policy: config.ReleasePolicyV1ToV0, tag: "v2.0.0", wantErr: "requires a v1 source tag"},
		{name: "unsupported policy", policy: "identity", tag: "v1.36.1", wantErr: "unsupported release policy"},
		{name: "malformed tag", policy: config.ReleasePolicyV1ToV0, tag: "v1.36", wantErr: "vMAJOR.MINOR.PATCH"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := config.MapReleaseTag(test.policy, test.tag)
			switch {
			case test.wantErr == "":
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if got != test.want {
					t.Fatalf("MapReleaseTag(%q, %q) = %q, want %q", test.policy, test.tag, got, test.want)
				}
			case err == nil:
				t.Fatalf("expected an error containing %q", test.wantErr)
			case !strings.Contains(err.Error(), test.wantErr):
				t.Fatalf("error %q does not contain %q", err, test.wantErr)
			}
		})
	}
}

func TestParseMinorSeries(t *testing.T) {
	tests := []struct {
		name      string
		value     string
		wantMajor int
		wantMinor int
		wantErr   string
	}{
		{name: "kubernetes minor", value: "v1.37", wantMajor: 1, wantMinor: 37},
		{name: "patch version", value: "v1.37.1", wantErr: "vMAJOR.MINOR"},
		{name: "no prefix", value: "1.37", wantErr: "must start with v"},
		{name: "leading zero", value: "v1.07", wantErr: "leading zero"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			major, minor, err := config.ParseMinorSeries(test.value)
			switch {
			case test.wantErr == "":
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if major != test.wantMajor || minor != test.wantMinor {
					t.Fatalf("ParseMinorSeries(%q) = %d, %d, want %d, %d", test.value, major, minor, test.wantMajor, test.wantMinor)
				}
			case err == nil:
				t.Fatalf("expected an error containing %q", test.wantErr)
			case !strings.Contains(err.Error(), test.wantErr):
				t.Fatalf("error %q does not contain %q", err, test.wantErr)
			}
		})
	}
}

func TestParseSymbolRef(t *testing.T) {
	tests := []struct {
		name    string
		ref     string
		wantPkg string
		wantSym string
		wantErr string
	}{
		{
			name:    "relocated package",
			ref:     "k8s.io/kubernetes/pkg/registry/rbac/validation.RoleGetter",
			wantPkg: "k8s.io/kubernetes/pkg/registry/rbac/validation",
			wantSym: "RoleGetter",
		},
		{
			name:    "versioned package",
			ref:     "k8s.io/api/rbac/v1.PolicyRule",
			wantPkg: "k8s.io/api/rbac/v1",
			wantSym: "PolicyRule",
		},
		{name: "standard library", ref: "fmt.Stringer", wantPkg: "fmt", wantSym: "Stringer"},
		{name: "no symbol", ref: "k8s.io/api/rbac/v1", wantErr: "must be <import path>.<Name>"},
		{name: "trailing dot", ref: "k8s.io/api/rbac/v1.", wantErr: "must be <import path>.<Name>"},
		{name: "empty", ref: "", wantErr: "must not be empty"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pkgPath, symbol, err := config.ParseSymbolRef(test.ref)
			switch {
			case test.wantErr == "":
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if pkgPath != test.wantPkg || symbol != test.wantSym {
					t.Fatalf("ParseSymbolRef(%q) = %q, %q, want %q, %q", test.ref, pkgPath, symbol, test.wantPkg, test.wantSym)
				}
			case err == nil:
				t.Fatalf("expected an error containing %q", test.wantErr)
			case !strings.Contains(err.Error(), test.wantErr):
				t.Fatalf("error %q does not contain %q", err, test.wantErr)
			}
		})
	}
}

func TestValidateIdentityName(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr string
	}{
		{name: "bot name", value: "soapbox[bot]"},
		{name: "person", value: "Monis Khan"},
		{name: "unicode", value: "Renée Müller"},
		{name: "empty", value: "", wantErr: "must not be empty"},
		{name: "leading space", value: " soapbox", wantErr: "leading or trailing space"},
		{name: "trailing space", value: "soapbox ", wantErr: "leading or trailing space"},
		{name: "trailing tab", value: "soapbox\t", wantErr: "leading or trailing space"},
		{name: "open bracket", value: "soapbox <bot", wantErr: "angle brackets"},
		{name: "close bracket", value: "soapbox bot>", wantErr: "angle brackets"},
		{name: "trailing newline", value: "soapbox\n", wantErr: "leading or trailing space"},
		{name: "embedded newline", value: "soap\nbox", wantErr: "control characters"},
		{name: "carriage return", value: "soap\rbox", wantErr: "control characters"},
		{name: "null byte", value: "soap\x00box", wantErr: "control characters"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := config.ValidateIdentityName(test.value)
			switch {
			case test.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case test.wantErr != "" && err == nil:
				t.Fatalf("expected an error containing %q", test.wantErr)
			case test.wantErr != "" && !strings.Contains(err.Error(), test.wantErr):
				t.Fatalf("error %q does not contain %q", err, test.wantErr)
			}
		})
	}
}

func TestValidateEmail(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr string
	}{
		{name: "person", value: "i@monis.app"},
		{name: "bot", value: "1234+soapbox[bot]@users.noreply.github.com"},
		{name: "no at sign", value: "monis.app", wantErr: "must be local@domain"},
		{name: "two at signs", value: "a@b@monis.app", wantErr: "exactly one @"},
		{name: "empty local", value: "@monis.app", wantErr: "must be local@domain"},
		{name: "empty domain", value: "i@", wantErr: "must be local@domain"},
		{name: "no dot in domain", value: "i@localhost", wantErr: "unsupported domain"},
		{name: "angle brackets", value: "<i@monis.app>", wantErr: "unsupported characters"},
		{name: "space", value: "i @monis.app", wantErr: "unsupported characters"},
		{name: "newline", value: "i\nx@monis.app", wantErr: "unsupported characters"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := config.ValidateEmail(test.value)
			switch {
			case test.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case test.wantErr != "" && err == nil:
				t.Fatalf("expected an error containing %q", test.wantErr)
			case test.wantErr != "" && !strings.Contains(err.Error(), test.wantErr):
				t.Fatalf("error %q does not contain %q", err, test.wantErr)
			}
		})
	}
}

func TestValidateModulePath(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		wantErr string
	}{
		{name: "vanity module", path: "monis.app/kk/rbac_authorizer"},
		{name: "kubernetes", path: "k8s.io/kubernetes"},
		{name: "no domain", path: "kubernetes/pkg", wantErr: "must start with a domain name element"},
		{name: "trailing slash", path: "k8s.io/kubernetes/", wantErr: "must not start or end with a slash"},
		{name: "empty element", path: "k8s.io//kubernetes", wantErr: "empty element"},
		{name: "space", path: "k8s.io/kube rnetes", wantErr: "unsupported character"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := config.ValidateModulePath(test.path)
			switch {
			case test.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case test.wantErr != "" && err == nil:
				t.Fatalf("expected an error containing %q", test.wantErr)
			case test.wantErr != "" && !strings.Contains(err.Error(), test.wantErr):
				t.Fatalf("error %q does not contain %q", err, test.wantErr)
			}
		})
	}
}
