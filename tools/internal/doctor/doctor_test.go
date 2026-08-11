package doctor_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/doctor"
	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/testsupport"
)

const (
	testName        = "Soapbox Test"
	testEmail       = "test@example.com"
	testSignoff     = "Soapbox Test <signoff@example.com>"
	testPublicKey   = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBGCSSRl8xJyPumnSt8gZI/pyuFchtZCrv5b3+8mT1zx vector@example.com"
	testFingerprint = "SHA256:cypmBC1kGwq8kRhRatVBjypnUDgPq8Es6Mzo3j+a3/4"
)

func TestSoapboxPolicy(t *testing.T) {
	policy := doctor.SoapboxPolicy()
	if got, want := policy.Identity(), "Monis Khan <i@monis.app>"; got != want {
		t.Fatalf("identity = %q, want %q", got, want)
	}
	if got, want := policy.SignoffTrailer(), "Signed-off-by: Monis Khan <mok@microsoft.com>"; got != want {
		t.Fatalf("signoff trailer = %q, want %q", got, want)
	}
	if policy.SigningKeyPath != "/Users/mo/.config/wunderkind/ssh/id_ed25519.pub" {
		t.Fatalf("signing key path = %q", policy.SigningKeyPath)
	}
	if !policy.MinimumGit.AtLeast(gitcli.Version{Major: 2, Minor: 34}) {
		t.Fatalf("minimum git %v predates SSH signing support", policy.MinimumGit)
	}
	if policy.Toolchain != "go1.26.5" {
		t.Fatalf("toolchain = %q", policy.Toolchain)
	}
}

func TestRunNotARepository(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	report := run(ctx, t, dir, nil, testPolicy(t))

	assertStatus(t, report, map[string]doctor.Status{
		"git.binary":                  doctor.StatusPass,
		"git.version":                 doctor.StatusPass,
		"go.binary":                   doctor.StatusPass,
		"go.version":                  doctor.StatusPass,
		"repo.present":                doctor.StatusWarn,
		"repo.user.name":              doctor.StatusSkip,
		"repo.signing.allowedSigners": doctor.StatusSkip,
		"head.present":                doctor.StatusSkip,
		"head.trailer":                doctor.StatusSkip,
	})
	for _, check := range report.Failures() {
		if strings.HasPrefix(check.Name, "repo.") || strings.HasPrefix(check.Name, "head.") {
			t.Fatalf("check %q failed in a directory that is not a repository: %s", check.Name, check.Detail)
		}
	}
}

func TestRunRepositoryWithoutCommits(t *testing.T) {
	ctx := t.Context()
	policy := testPolicy(t)
	repo := compliantRepo(ctx, t, policy)

	report := run(ctx, t, repo.Dir, repo.Git, policy)
	assertStatus(t, report, map[string]doctor.Status{
		"repo.present":                doctor.StatusPass,
		"repo.user.name":              doctor.StatusPass,
		"repo.user.email":             doctor.StatusPass,
		"repo.signing.format":         doctor.StatusPass,
		"repo.signing.key":            doctor.StatusPass,
		"repo.signing.commit":         doctor.StatusPass,
		"repo.signing.tag":            doctor.StatusPass,
		"repo.signing.allowedSigners": doctor.StatusPass,
		"head.present":                doctor.StatusPass,
		"head.signature":              doctor.StatusSkip,
		"head.signer":                 doctor.StatusSkip,
		"head.author":                 doctor.StatusSkip,
		"head.committer":              doctor.StatusSkip,
		"head.trailer":                doctor.StatusSkip,
		"head.attribution":            doctor.StatusSkip,
	})
	if detail := detailOf(t, report, "head.present"); !strings.Contains(detail, "no commits yet") {
		t.Fatalf("head.present detail = %q", detail)
	}
}

func TestRunRepositoryConfiguration(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(ctx context.Context, t *testing.T, repo *testsupport.Repo, policy *doctor.Policy)
		want   map[string]doctor.Status
		field  string
		want2  string
	}{
		{
			name: "missing signing format",
			mutate: func(ctx context.Context, t *testing.T, repo *testsupport.Repo, _ *doctor.Policy) {
				unset(ctx, t, repo, "gpg.format")
			},
			want:  map[string]doctor.Status{"repo.signing.format": doctor.StatusFail},
			field: "repo.signing.format",
			want2: "is not set locally",
		},
		{
			name: "wrong signing key",
			mutate: func(ctx context.Context, t *testing.T, repo *testsupport.Repo, _ *doctor.Policy) {
				repo.SetConfig(ctx, t, "user.signingkey", "/tmp/other.pub")
			},
			want:  map[string]doctor.Status{"repo.signing.key": doctor.StatusFail},
			field: "repo.signing.key",
			want2: `is "/tmp/other.pub"`,
		},
		{
			name: "commit signing disabled",
			mutate: func(ctx context.Context, t *testing.T, repo *testsupport.Repo, _ *doctor.Policy) {
				repo.SetConfig(ctx, t, "commit.gpgsign", "false")
			},
			want:  map[string]doctor.Status{"repo.signing.commit": doctor.StatusFail},
			field: "repo.signing.commit",
			want2: `is "false", want "true"`,
		},
		{
			name: "tag signing disabled",
			mutate: func(ctx context.Context, t *testing.T, repo *testsupport.Repo, _ *doctor.Policy) {
				unset(ctx, t, repo, "tag.gpgsign")
			},
			want:  map[string]doctor.Status{"repo.signing.tag": doctor.StatusFail},
			field: "repo.signing.tag",
			want2: "annotated engine tags",
		},
		{
			name: "wrong identity",
			mutate: func(ctx context.Context, t *testing.T, repo *testsupport.Repo, _ *doctor.Policy) {
				repo.SetConfig(ctx, t, "user.email", "someone@example.org")
			},
			want:  map[string]doctor.Status{"repo.user.email": doctor.StatusFail},
			field: "repo.user.email",
			want2: "want \"" + testEmail + "\"",
		},
		{
			name: "allowed signers not configured",
			mutate: func(ctx context.Context, t *testing.T, repo *testsupport.Repo, _ *doctor.Policy) {
				unset(ctx, t, repo, "gpg.ssh.allowedsignersfile")
			},
			want:  map[string]doctor.Status{"repo.signing.allowedSigners": doctor.StatusFail},
			field: "repo.signing.allowedSigners",
			want2: "is not set locally",
		},
		{
			name: "allowed signers file is missing",
			mutate: func(ctx context.Context, t *testing.T, repo *testsupport.Repo, _ *doctor.Policy) {
				repo.SetConfig(ctx, t, "gpg.ssh.allowedsignersfile", filepath.Join(repo.Dir, "absent_signers"))
			},
			want:  map[string]doctor.Status{"repo.signing.allowedSigners": doctor.StatusFail},
			field: "repo.signing.allowedSigners",
			want2: "read allowed signers",
		},
		{
			name: "allowed signers authorizes another identity",
			mutate: func(ctx context.Context, t *testing.T, repo *testsupport.Repo, _ *doctor.Policy) {
				write(t, filepath.Join(repo.Dir, "allowed_signers"), "other@example.com "+testPublicKey+"\n")
			},
			want:  map[string]doctor.Status{"repo.signing.allowedSigners": doctor.StatusFail},
			field: "repo.signing.allowedSigners",
			want2: "does not authorize",
		},
		{
			name: "signing key file is missing",
			mutate: func(_ context.Context, t *testing.T, _ *testsupport.Repo, policy *doctor.Policy) {
				policy.SigningKeyPath = filepath.Join(t.TempDir(), "absent.pub")
			},
			want:  map[string]doctor.Status{"repo.signing.allowedSigners": doctor.StatusFail},
			field: "repo.signing.allowedSigners",
			want2: "read signing key",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := t.Context()
			policy := testPolicy(t)
			repo := compliantRepo(ctx, t, policy)
			test.mutate(ctx, t, repo, &policy)

			report := run(ctx, t, repo.Dir, repo.Git, policy)
			assertStatus(t, report, test.want)
			if detail := detailOf(t, report, test.field); !strings.Contains(detail, test.want2) {
				t.Fatalf("%s detail %q does not contain %q", test.field, detail, test.want2)
			}
		})
	}
}

func TestRunHeadDiagnostics(t *testing.T) {
	tests := []struct {
		name    string
		message string
		author  gitcli.Signature
		want    map[string]doctor.Status
		field   string
		detail  string
	}{
		{
			name:    "compliant unsigned commit",
			message: "chore(repo): bootstrap\n\nSigned-off-by: " + testSignoff + "\n",
			want: map[string]doctor.Status{
				"head.author":      doctor.StatusPass,
				"head.committer":   doctor.StatusPass,
				"head.trailer":     doctor.StatusPass,
				"head.attribution": doctor.StatusPass,
				"head.signature":   doctor.StatusFail,
				"head.signer":      doctor.StatusFail,
			},
			field:  "head.signature",
			detail: `signature status "N" (no signature)`,
		},
		{
			name:    "missing signoff trailer",
			message: "chore(repo): bootstrap\n",
			want:    map[string]doctor.Status{"head.trailer": doctor.StatusFail},
			field:   "head.trailer",
			detail:  "found 0 Signed-off-by trailers",
		},
		{
			name:    "duplicate signoff trailer",
			message: "chore(repo): bootstrap\n\nSigned-off-by: " + testSignoff + "\nSigned-off-by: " + testSignoff + "\n",
			want:    map[string]doctor.Status{"head.trailer": doctor.StatusFail},
			field:   "head.trailer",
			detail:  "found 2 Signed-off-by trailers",
		},
		{
			name:    "signoff for the wrong identity",
			message: "chore(repo): bootstrap\n\nSigned-off-by: Someone Else <someone@example.org>\n",
			want:    map[string]doctor.Status{"head.trailer": doctor.StatusFail},
			field:   "head.trailer",
			detail:  "0 matching",
		},
		{
			name:    "co-author attribution",
			message: "chore(repo): bootstrap\n\nSigned-off-by: " + testSignoff + "\nCo-authored-by: Robot <robot@example.org>\n",
			want: map[string]doctor.Status{
				"head.trailer":     doctor.StatusPass,
				"head.attribution": doctor.StatusFail,
			},
			field:  "head.attribution",
			detail: "found 1 co-author trailers",
		},
		{
			name:    "upstream author identity",
			message: "chore(repo): bootstrap\n\nSigned-off-by: " + testSignoff + "\n",
			author:  gitcli.Signature{Name: "Upstream Author", Email: "upstream@example.org"},
			want: map[string]doctor.Status{
				"head.author":    doctor.StatusFail,
				"head.committer": doctor.StatusPass,
			},
			field:  "head.author",
			detail: "author Upstream Author <upstream@example.org>",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := t.Context()
			policy := testPolicy(t)
			repo := compliantRepo(ctx, t, policy)
			repo.WriteFile(t, "README.md", "soapbox\n")
			repo.Commit(ctx, t, test.message, gitcli.CommitOptions{Author: test.author}, "README.md")

			report := run(ctx, t, repo.Dir, repo.Git, policy)
			assertStatus(t, report, test.want)
			if detail := detailOf(t, report, test.field); !strings.Contains(detail, test.detail) {
				t.Fatalf("%s detail %q does not contain %q", test.field, detail, test.detail)
			}
			if !strings.Contains(detailOf(t, report, "head.signer"), testFingerprint) {
				t.Fatalf("head.signer detail does not mention the policy key fingerprint")
			}
		})
	}
}

func TestRunCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := doctor.Run(ctx, doctor.Options{Dir: t.TempDir()}); !errors.Is(err, context.Canceled) {
		t.Fatalf("error %v is not context.Canceled", err)
	}
}

func TestRunMissingGoExecutable(t *testing.T) {
	ctx := t.Context()
	report, err := doctor.Run(ctx, doctor.Options{Dir: t.TempDir(), GoBinary: "soapbox-go-does-not-exist"})
	if err != nil {
		t.Fatalf("doctor run: %v", err)
	}
	assertStatus(t, report, map[string]doctor.Status{
		"go.binary":       doctor.StatusFail,
		"go.version":      doctor.StatusSkip,
		"go.cache.gopath": doctor.StatusSkip,
	})
	if report.OK() {
		t.Fatal("report is OK even though a required check failed")
	}
}

func TestReportWrite(t *testing.T) {
	report := &doctor.Report{Checks: []doctor.Check{
		{Name: "git.binary", Status: doctor.StatusPass, Required: true, Detail: "/usr/bin/git"},
		{Name: "head.trailer", Status: doctor.StatusFail, Required: true, Detail: "found 0 trailers"},
		{Name: "repo.signing.tag", Status: doctor.StatusWarn, Detail: "not set"},
		{Name: "head.signature", Status: doctor.StatusSkip, Detail: "no HEAD"},
	}}

	var buf bytes.Buffer
	if err := report.Write(&buf); err != nil {
		t.Fatalf("write report: %v", err)
	}
	got := buf.String()
	want := "PASS git.binary        /usr/bin/git\n" +
		"FAIL head.trailer      found 0 trailers\n" +
		"WARN repo.signing.tag  not set\n" +
		"SKIP head.signature    no HEAD\n" +
		"4 checks: 1 passed, 1 failed, 1 warned, 1 skipped\n"
	if got != want {
		t.Fatalf("report:\n%q\nwant:\n%q", got, want)
	}
	if report.OK() {
		t.Fatal("report with a failure reported OK")
	}
	if len(report.Failures()) != 1 {
		t.Fatalf("failures = %v", report.Failures())
	}
	if counts := report.Counts(); counts[doctor.StatusPass] != 1 {
		t.Fatalf("counts = %v", counts)
	}
}

// testPolicy returns a policy that matches the compliant temporary repository.
func testPolicy(t *testing.T) doctor.Policy {
	t.Helper()
	policy := doctor.SoapboxPolicy()
	policy.AuthorName = testName
	policy.AuthorEmail = testEmail
	policy.SignoffTrailerValue = testSignoff
	policy.SigningKeyPath = filepath.Join(t.TempDir(), "id_ed25519.pub")
	write(t, policy.SigningKeyPath, testPublicKey+"\n")
	return policy
}

// compliantRepo builds a repository configured exactly as the policy requires.
// Commits stay unsigned because tests never touch a real signing key.
func compliantRepo(ctx context.Context, t *testing.T, policy doctor.Policy) *testsupport.Repo {
	t.Helper()
	repo := testsupport.NewRepo(ctx, t, testsupport.Options{
		UserName:  policy.AuthorName,
		UserEmail: policy.AuthorEmail,
	})
	signers := filepath.Join(repo.Dir, "allowed_signers")
	write(t, signers, policy.AuthorEmail+" "+testPublicKey+"\n")

	repo.SetConfig(ctx, t, "gpg.format", "ssh")
	repo.SetConfig(ctx, t, "user.signingkey", policy.SigningKeyPath)
	repo.SetConfig(ctx, t, "commit.gpgsign", "true")
	repo.SetConfig(ctx, t, "tag.gpgsign", "true")
	repo.SetConfig(ctx, t, "gpg.ssh.allowedsignersfile", signers)
	return repo
}

// run executes the doctor against a repository with an injected policy.
func run(ctx context.Context, t *testing.T, dir string, git *gitcli.Runner, policy doctor.Policy) *doctor.Report {
	t.Helper()
	report, err := doctor.Run(ctx, doctor.Options{Dir: dir, Git: git, Policy: &policy})
	if err != nil {
		t.Fatalf("doctor run: %v", err)
	}
	return report
}

// assertStatus checks the status of the named checks.
func assertStatus(t *testing.T, report *doctor.Report, want map[string]doctor.Status) {
	t.Helper()
	statuses := make(map[string]doctor.Status, len(report.Checks))
	for _, check := range report.Checks {
		if _, duplicate := statuses[check.Name]; duplicate {
			t.Fatalf("check %q reported twice", check.Name)
		}
		statuses[check.Name] = check.Status
	}
	for name, wantStatus := range want {
		got, ok := statuses[name]
		if !ok {
			t.Fatalf("check %q is missing from the report", name)
		}
		if got != wantStatus {
			t.Fatalf("check %q = %s, want %s (%s)", name, got, wantStatus, detailOf(t, report, name))
		}
	}
}

// detailOf returns the detail of one check.
func detailOf(t *testing.T, report *doctor.Report, name string) string {
	t.Helper()
	for _, check := range report.Checks {
		if check.Name == name {
			return check.Detail
		}
	}
	t.Fatalf("check %q is missing from the report", name)
	return ""
}

func write(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// unset removes a repository local configuration key.
func unset(ctx context.Context, t *testing.T, repo *testsupport.Repo, key string) {
	t.Helper()
	if err := repo.Git.UnsetConfigLocal(ctx, key); err != nil {
		t.Fatalf("clear %s: %v", key, err)
	}
}
