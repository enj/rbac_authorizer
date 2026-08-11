package patchset_test

import (
	"context"
	"crypto/sha1"
	"fmt"
	"testing"

	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/patchset"
	"github.com/enj/soapbox/tools/internal/testsupport"
)

// The engine binds patchset.Git to the typed runner in internal/gitcli, which
// is the only package allowed to start a subprocess. This assertion is what
// makes that binding a build failure rather than an integration surprise if
// either side changes, and these tests exercise application against real git
// behaviour in a real temporary repository rather than against a stand in that
// would only prove this package agrees with itself.
var _ patchset.Git = (*gitcli.Runner)(nil)

// gitRepo is a temporary repository bound to the patchset Git interface.
type gitRepo struct {
	*testsupport.Repo
	patchset.Git
}

// newGitRepo creates an initialized repository and binds its runner.
func newGitRepo(ctx context.Context, t *testing.T) *gitRepo {
	t.Helper()

	repo := testsupport.NewRepo(ctx, t, testsupport.Options{
		UserName:  "Soapbox Test",
		UserEmail: "test@example.invalid",
	})
	return &gitRepo{Repo: repo, Git: repo.Git}
}

// status reports the work tree state of the repository.
func (r *gitRepo) status(ctx context.Context, t *testing.T) []gitcli.StatusEntry {
	t.Helper()
	entries, err := r.Status(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	return entries
}

// blobSHA reports the object name Git gives a blob with these contents.
//
// The test computes it rather than asking Git for it, because a three way patch
// has to name the preimage blob it applies to and gitcli deliberately exposes
// no hash-object surface. The formula is Git's own: the object header, a null
// byte, and the contents.
func blobSHA(contents string) string {
	sum := sha1.Sum([]byte(fmt.Sprintf("blob %d\x00%s", len(contents), contents)))
	return fmt.Sprintf("%x", sum)
}
