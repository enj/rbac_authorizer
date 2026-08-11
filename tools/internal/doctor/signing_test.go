package doctor_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/doctor"
	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/testsupport"
)

// TestRunFullyCompliantRepository exercises the whole policy against a real
// signed commit, which is the only way to prove the signature, signer, and
// allowed signers checks agree with git.
func TestRunFullyCompliantRepository(t *testing.T) {
	ctx := t.Context()
	key := testsupport.NewSigningKey(t)
	policy := signingPolicy(key)
	repo := testsupport.NewRepo(ctx, t, testsupport.Options{UserName: policy.AuthorName, UserEmail: policy.AuthorEmail})
	repo.EnableSSHSigning(ctx, t, key, policy.AuthorEmail)

	repo.WriteFile(t, "README.md", "soapbox\n")
	repo.Commit(ctx, t, "chore(repo): bootstrap\n\nSigned-off-by: "+policy.SignoffTrailerValue+"\n",
		gitcli.CommitOptions{Sign: true}, "README.md")

	report := run(ctx, t, repo.Dir, repo.Git, policy)
	for _, check := range report.Checks {
		if check.Status == doctor.StatusFail {
			t.Errorf("check %q failed on a compliant repository: %s", check.Name, check.Detail)
		}
	}
	if !report.OK() {
		t.Fatal("a fully compliant repository did not pass")
	}
	if detail := detailOf(t, report, "head.signature"); !strings.Contains(detail, `"G"`) {
		t.Fatalf("head.signature detail = %q", detail)
	}
	if detail := detailOf(t, report, "head.signer"); !strings.Contains(detail, key.Fingerprint) {
		t.Fatalf("head.signer detail = %q", detail)
	}
}

// TestRunAllowedSignersSemantics covers the OpenSSH principal syntax the doctor
// must read the same way git does.
func TestRunAllowedSignersSemantics(t *testing.T) {
	tests := []struct {
		name       string
		principals string
		options    string
		relative   bool
		otherKey   bool
		want       doctor.Status
	}{
		{name: "exact principal", principals: "signer@example.com", want: doctor.StatusPass},
		{name: "comma separated list", principals: "other@example.org,signer@example.com", want: doctor.StatusPass},
		{name: "wildcard domain", principals: "*@example.com", want: doctor.StatusPass},
		{name: "single character wildcard", principals: "signer@example.co?", want: doctor.StatusPass},
		{name: "options field", principals: "signer@example.com", options: `namespaces="git"`, want: doctor.StatusPass},
		{name: "relative configuration path", principals: "signer@example.com", relative: true, want: doctor.StatusPass},
		{name: "negated principal", principals: "!signer@example.com,*@example.com", want: doctor.StatusFail},
		{name: "negated wildcard", principals: "!*@example.com,signer@example.com", want: doctor.StatusFail},
		{name: "other principal only", principals: "other@example.org", want: doctor.StatusFail},
		{name: "other key", principals: "signer@example.com", otherKey: true, want: doctor.StatusFail},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := t.Context()
			key := testsupport.NewSigningKey(t)
			policy := signingPolicy(key)
			repo := testsupport.NewRepo(ctx, t, testsupport.Options{UserName: policy.AuthorName, UserEmail: policy.AuthorEmail})
			repo.EnableSSHSigning(ctx, t, key, policy.AuthorEmail)

			authorized := key
			if test.otherKey {
				authorized = testsupport.NewSigningKey(t)
			}
			line := test.principals
			if test.options != "" {
				line += " " + test.options
			}
			line += " " + authorized.PublicLine() + "\n"

			signers := filepath.Join(repo.Dir, "allowed_signers")
			if err := os.WriteFile(signers, []byte("# soapbox fixture\n\n"+line), 0o600); err != nil {
				t.Fatalf("write allowed signers: %v", err)
			}
			if test.relative {
				repo.SetConfig(ctx, t, "gpg.ssh.allowedsignersfile", "allowed_signers")
			}

			report := run(ctx, t, repo.Dir, repo.Git, policy)
			assertStatus(t, report, map[string]doctor.Status{"repo.signing.allowedSigners": test.want})
		})
	}
}

// TestRunAllowedSignersOutsideTheRepository confirms that an absolute path is
// still honoured.
func TestRunAllowedSignersOutsideTheRepository(t *testing.T) {
	ctx := t.Context()
	key := testsupport.NewSigningKey(t)
	policy := signingPolicy(key)
	repo := testsupport.NewRepo(ctx, t, testsupport.Options{UserName: policy.AuthorName, UserEmail: policy.AuthorEmail})
	repo.EnableSSHSigning(ctx, t, key, policy.AuthorEmail)

	outside := filepath.Join(t.TempDir(), "allowed_signers")
	if err := os.WriteFile(outside, []byte(policy.AuthorEmail+" "+key.PublicLine()+"\n"), 0o600); err != nil {
		t.Fatalf("write allowed signers: %v", err)
	}
	repo.SetConfig(ctx, t, "gpg.ssh.allowedsignersfile", outside)

	report := run(ctx, t, repo.Dir, repo.Git, policy)
	assertStatus(t, report, map[string]doctor.Status{"repo.signing.allowedSigners": doctor.StatusPass})
}

func TestRunCancelledMidRunReportsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	key := testsupport.NewSigningKey(t)
	policy := signingPolicy(key)
	repo := testsupport.NewRepo(ctx, t, testsupport.Options{UserName: policy.AuthorName, UserEmail: policy.AuthorEmail})

	// Cancelling after the repository exists means the first checks can run and
	// the rest must not, which is the state a signal leaves behind.
	cancel()
	if _, err := doctor.Run(ctx, doctor.Options{Dir: repo.Dir, Git: repo.Git, Policy: &policy}); err == nil {
		t.Fatal("expected a cancellation error rather than a policy verdict")
	}
}

// signingPolicy builds a policy for a generated fixture key.
func signingPolicy(key testsupport.SigningKey) doctor.Policy {
	policy := doctor.SoapboxPolicy()
	policy.AuthorName = "Soapbox Signer"
	policy.AuthorEmail = "signer@example.com"
	policy.SignoffTrailerValue = "Soapbox Signer <signoff@example.com>"
	policy.SigningKeyPath = key.PublicPath
	return policy
}
