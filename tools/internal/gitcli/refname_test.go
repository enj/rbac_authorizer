package gitcli_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/gitcli"
)

func TestValidateRefName(t *testing.T) {
	tests := []struct {
		name    string
		ref     string
		wantErr string
	}{
		{name: "branch ref", ref: "refs/heads/main"},
		{name: "tag ref", ref: "refs/tags/v0.36.1"},
		{name: "progress namespace", ref: "refs/soapbox/progress/backfill"},
		{name: "empty", ref: "", wantErr: "must not be empty"},
		{name: "one level", ref: "main", wantErr: "must be hierarchical"},
		{name: "at sign", ref: "@", wantErr: "must not be @"},
		{name: "option like", ref: "-refs/heads/main", wantErr: "must not start with a dash"},
		{name: "leading slash", ref: "/refs/heads/main", wantErr: "must not start or end with a slash"},
		{name: "trailing slash", ref: "refs/heads/main/", wantErr: "must not start or end with a slash"},
		{name: "trailing dot", ref: "refs/heads/main.", wantErr: "must not end with a dot"},
		{name: "double dot", ref: "refs/heads/ma..in", wantErr: "consecutive dots"},
		{name: "reflog syntax", ref: "refs/heads/main@{1}", wantErr: "must not contain @{"},
		{name: "double slash", ref: "refs/heads//main", wantErr: "consecutive slashes"},
		{name: "space", ref: "refs/heads/ma in", wantErr: "control characters or spaces"},
		{name: "tilde", ref: "refs/heads/main~1", wantErr: "must not contain"},
		{name: "caret", ref: "refs/heads/main^", wantErr: "must not contain"},
		{name: "colon", ref: "refs/heads:main", wantErr: "must not contain"},
		{name: "glob", ref: "refs/heads/*", wantErr: "must not contain"},
		{name: "backslash", ref: `refs/heads/ma\in`, wantErr: "must not contain"},
		{name: "dot component", ref: "refs/heads/.hidden", wantErr: "must not start with a dot"},
		{name: "lock suffix", ref: "refs/heads/main.lock", wantErr: "must not end with .lock"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := gitcli.ValidateRefName(test.ref)
			assertErrContains(t, err, test.wantErr)
		})
	}
}

func TestValidateBranchName(t *testing.T) {
	tests := []struct {
		name    string
		branch  string
		wantErr string
	}{
		{name: "master", branch: "master"},
		{name: "release branch", branch: "release-1.36"},
		{name: "hierarchical", branch: "feature/rbac"},
		{name: "head", branch: "HEAD", wantErr: "must not be HEAD"},
		{name: "full ref", branch: "refs/heads/main", wantErr: "must be a short name"},
		{name: "option like", branch: "--all", wantErr: "must not start with a dash"},
		{name: "empty", branch: "", wantErr: "must not be empty"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertErrContains(t, gitcli.ValidateBranchName(test.branch), test.wantErr)
		})
	}
}

func TestValidatePushRefspec(t *testing.T) {
	tests := []struct {
		name     string
		spec     string
		wantErr  string
		wantKind error
	}{
		{name: "branch update", spec: "refs/heads/main:refs/heads/main"},
		{name: "head to branch", spec: "HEAD:refs/heads/main"},
		{name: "tag", spec: "refs/tags/v0.36.1:refs/tags/v0.36.1"},
		{name: "force plus", spec: "+refs/heads/main:refs/heads/main", wantErr: "force", wantKind: gitcli.ErrForceRefspec},
		{name: "delete", spec: ":refs/heads/main", wantErr: "delete", wantKind: gitcli.ErrDeleteRefspec},
		{name: "option like", spec: "--force", wantErr: "dash", wantKind: gitcli.ErrFlagLikeArgument},
		{name: "no colon", spec: "refs/heads/main", wantErr: "<source>:<destination>"},
		{name: "two colons", spec: "a:b:c", wantErr: "exactly one colon"},
		{name: "empty destination", spec: "refs/heads/main:", wantErr: "must name a destination ref"},
		{name: "short destination", spec: "refs/heads/main:main", wantErr: "must be hierarchical"},
		{name: "whitespace", spec: "refs/heads/main :refs/heads/main", wantErr: "must not contain whitespace"},
		{name: "empty", spec: "", wantErr: "must not be empty"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := gitcli.ValidatePushRefspec(test.spec)
			assertErrContains(t, err, test.wantErr)
			if test.wantKind != nil && !errors.Is(err, test.wantKind) {
				t.Fatalf("error %v is not %v", err, test.wantKind)
			}
		})
	}
}

func TestValidateRemote(t *testing.T) {
	tests := []struct {
		name    string
		remote  string
		wantErr string
	}{
		{name: "name", remote: "origin"},
		{name: "https", remote: "https://github.com/enj/rbac_authorizer.git"},
		{name: "absolute path", remote: "/tmp/soapbox-remote.git"},
		{name: "file url", remote: "file:///tmp/soapbox-remote.git"},
		{name: "credentials", remote: "https://token@github.com/enj/rbac_authorizer.git", wantErr: "must not embed credentials"},
		{name: "userinfo with password", remote: "https://user:pass@github.com/enj/x.git", wantErr: "must not embed credentials"},
		{name: "ssh scp syntax", remote: "git@github.com:enj/rbac_authorizer.git", wantErr: "must be a remote name"},
		{name: "http", remote: "http://github.com/enj/x.git", wantErr: "must use https or file"},
		{name: "option like", remote: "--upload-pack=evil", wantErr: "must not start with a dash"},
		{name: "empty", remote: "", wantErr: "must not be empty"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertErrContains(t, gitcli.ValidateRemote(test.remote), test.wantErr)
		})
	}
}

func TestParseVersion(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    gitcli.Version
		wantErr string
	}{
		{name: "three parts", value: "2.47.3", want: gitcli.Version{Major: 2, Minor: 47, Patch: 3}},
		{name: "two parts", value: "1.26", want: gitcli.Version{Major: 1, Minor: 26}},
		{name: "vendor suffix", value: "2.39.5.rc0", want: gitcli.Version{Major: 2, Minor: 39, Patch: 5}},
		{name: "apple git", value: "2.39.3", want: gitcli.Version{Major: 2, Minor: 39, Patch: 3}},
		{name: "release candidate", value: "2.48.0-rc1", want: gitcli.Version{Major: 2, Minor: 48, Patch: 0}},
		{name: "not numeric", value: "next", wantErr: "no numeric components"},
		{name: "empty", value: "", wantErr: "no numeric components"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := gitcli.ParseVersion(test.value)
			assertErrContains(t, err, test.wantErr)
			if test.wantErr == "" && got != test.want {
				t.Fatalf("ParseVersion(%q) = %v, want %v", test.value, got, test.want)
			}
		})
	}
}

func TestVersionAtLeast(t *testing.T) {
	tests := []struct {
		name  string
		have  gitcli.Version
		least gitcli.Version
		want  bool
	}{
		{name: "equal", have: gitcli.Version{Major: 2, Minor: 34}, least: gitcli.Version{Major: 2, Minor: 34}, want: true},
		{name: "newer patch", have: gitcli.Version{Major: 2, Minor: 34, Patch: 1}, least: gitcli.Version{Major: 2, Minor: 34}, want: true},
		{name: "older patch", have: gitcli.Version{Major: 2, Minor: 34}, least: gitcli.Version{Major: 2, Minor: 34, Patch: 1}},
		{name: "older minor", have: gitcli.Version{Major: 2, Minor: 33, Patch: 9}, least: gitcli.Version{Major: 2, Minor: 34}},
		{name: "newer major", have: gitcli.Version{Major: 3}, least: gitcli.Version{Major: 2, Minor: 47}, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.have.AtLeast(test.least); got != test.want {
				t.Fatalf("%v.AtLeast(%v) = %t, want %t", test.have, test.least, got, test.want)
			}
		})
	}
}

// assertErrContains fails when err does not match the expectation described by
// want. An empty want means no error is expected.
func assertErrContains(t *testing.T, err error, want string) {
	t.Helper()
	switch {
	case want == "" && err != nil:
		t.Fatalf("unexpected error: %v", err)
	case want != "" && err == nil:
		t.Fatalf("expected error containing %q, got nil", want)
	case want != "" && !strings.Contains(err.Error(), want):
		t.Fatalf("error %q does not contain %q", err, want)
	}
}
