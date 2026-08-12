package gitcli_test

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/enj/soapbox/tools/internal/gitcli"
)

// bloblessCache clones the fixture without blobs and returns a runner scoped to
// the clone, along with the commit whose blobs are therefore absent.
func bloblessCache(t *testing.T) (*gitcli.Runner, *upstream, string) {
	t.Helper()
	ctx := t.Context()
	up := newUpstream(ctx, t)
	root := t.TempDir()
	runner := newAnonymousRunner(t, root)

	dir := filepath.Join(root, "cache.git")
	if err := runner.CloneSource(ctx, gitcli.SourceCloneOptions{
		Remote:    up.url(),
		Directory: dir,
		Filter:    gitcli.BloblessFilter,
		Bare:      true,
	}); err != nil {
		t.Fatalf("clone: %v", err)
	}
	cache, err := runner.WithDir(dir)
	if err != nil {
		t.Fatalf("scope runner to the cache: %v", err)
	}
	return cache, up, dir
}

// TestNoLazyFetchIsHonoured is the behavioural check behind
// MinimumNoLazyFetchVersion. The variable is ignored rather than rejected by an
// older git, so the floor is only meaningful if the git actually running obeys
// it, and that is what this proves.
func TestNoLazyFetchIsHonoured(t *testing.T) {
	ctx := t.Context()
	cache, up, _ := bloblessCache(t)
	blob := up.sha(mainOne) + ":plugin/pkg/auth/authorizer/rbac/rbac.go"

	// The blob is genuinely absent, and reachable, so the two answers below
	// differ only because of the variable.
	pinned := cache.WithNoLazyFetch()
	infos, err := pinned.ObjectInfoBatch(ctx, gitcli.ObjectInfoOptions{Revisions: []string{blob}})
	if err != nil {
		t.Fatalf("object info: %v", err)
	}
	if len(infos) != 1 || !infos[0].Missing {
		t.Fatalf("object info = %+v, want a missing object", infos)
	}
	content, err := cache.ReadBlob(ctx, gitcli.BlobOptions{
		Revision:       up.sha(mainOne),
		Path:           "plugin/pkg/auth/authorizer/rbac/rbac.go",
		AllowLazyFetch: true,
	})
	if err != nil {
		t.Fatalf("the blob was not reachable, so the fixture proves nothing: %v", err)
	}
	if got := string(content); got != "package rbac\n" {
		t.Fatalf("content = %q", got)
	}

	if !gitcli.MinimumVersion().AtLeast(gitcli.MinimumNoLazyFetchVersion()) {
		t.Fatal("the package floor is below the version this capability needs")
	}
	version, err := cache.Version(ctx)
	if err != nil {
		t.Fatalf("git version: %v", err)
	}
	if !version.AtLeast(gitcli.MinimumNoLazyFetchVersion()) {
		t.Fatalf("git %s is below the declared no lazy fetch floor %s", version, gitcli.MinimumNoLazyFetchVersion())
	}
}

// TestWithNoLazyFetchStopsWorkTreeCommands is the reason the pin exists.
//
// A checkout, a reset, or a diff in a blobless clone fetches whatever it is
// missing, and none of them takes an option that says otherwise. Pinning the
// runner is the only way to make those commands offline.
//
// Each case is a differential against the same repository with the promisor
// remote left reachable: the pinned runner must fail and the unpinned one must
// succeed. Asserting only that the pinned call fails would prove nothing, since
// a command can fail for any number of reasons; it is the unpinned call
// succeeding that shows the blobs were there for the taking and the pin is what
// refused them.
func TestWithNoLazyFetchStopsWorkTreeCommands(t *testing.T) {
	tests := []struct {
		name string
		call func(*gitcli.Runner, *upstream) error
	}{
		{
			name: "checkout",
			call: func(r *gitcli.Runner, up *upstream) error {
				return r.CheckoutDetached(t.Context(), up.sha(mainOne))
			},
		},
		{
			name: "reset",
			call: func(r *gitcli.Runner, up *upstream) error {
				return r.ResetHard(t.Context(), up.sha(mainOne))
			},
		},
		{
			name: "diff against a commit",
			call: func(r *gitcli.Runner, up *upstream) error {
				_, err := r.Diff(t.Context(), gitcli.DiffOptions{Revision: up.sha(mainOne)})
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// A fresh clone per case, because the first call to succeed would
			// otherwise leave the blobs behind for the next one.
			work, up := bloblessWorkTree(t)
			if err := test.call(work.WithNoLazyFetch(), up); err == nil {
				t.Fatal("the pinned runner obtained blobs it should not have")
			}
			if err := test.call(work, up); err != nil {
				t.Fatalf("the unpinned runner failed, so the pin is not what refused the blobs: %v", err)
			}
		})
	}
}

// TestWithNoLazyFetchDoesNotMutateItsReceiver keeps the clone a clone.
func TestWithNoLazyFetchDoesNotMutateItsReceiver(t *testing.T) {
	work, _ := bloblessWorkTree(t)
	pinned := work.WithNoLazyFetch()
	if !pinned.IsNoLazyFetch() {
		t.Fatal("the clone does not report the pin")
	}
	if work.IsNoLazyFetch() {
		t.Fatal("WithNoLazyFetch mutated the runner it was called on")
	}
}

// bloblessWorkTree clones the fixture without blobs and without a checkout, and
// returns a runner scoped to the work tree. The promisor remote is left
// reachable, so a command that reaches for it will succeed rather than fail for
// an unrelated reason.
func bloblessWorkTree(t *testing.T) (*gitcli.Runner, *upstream) {
	t.Helper()
	ctx := t.Context()
	up := newUpstream(ctx, t)
	root := t.TempDir()
	runner := newAnonymousRunner(t, root)

	dir := filepath.Join(root, "work")
	if err := runner.CloneSource(ctx, gitcli.SourceCloneOptions{
		Remote:     up.url(),
		Directory:  dir,
		Filter:     gitcli.BloblessFilter,
		NoCheckout: true,
	}); err != nil {
		t.Fatalf("clone: %v", err)
	}
	work, err := runner.WithDir(dir)
	if err != nil {
		t.Fatalf("scope runner to the work tree: %v", err)
	}
	return work, up
}

// TestNoLazyFetchPinSurvivesAndCannotBeShadowed covers the two ways a pin that
// lived in the environment rather than on the runner would be lost.
func TestNoLazyFetchPinSurvivesAndCannotBeShadowed(t *testing.T) {
	ctx := t.Context()
	cache, up, _ := bloblessCache(t)
	blob := up.sha(mainOne) + ":plugin/pkg/auth/authorizer/rbac/rbac.go"

	// Anonymous rebuilds the environment from the inherited snapshot, so a pin
	// held as an entry would vanish exactly when a run is about to reach the
	// network.
	pinned := cache.WithNoLazyFetch().Anonymous()
	if !pinned.IsNoLazyFetch() {
		t.Fatal("Anonymous dropped the pin")
	}
	infos, err := pinned.ObjectInfoBatch(ctx, gitcli.ObjectInfoOptions{Revisions: []string{blob}})
	if err != nil {
		t.Fatalf("object info: %v", err)
	}
	if len(infos) != 1 || !infos[0].Missing {
		t.Fatalf("object info = %+v, want a missing object", infos)
	}

	// An explicit request for a fetch is refused rather than downgraded, so a
	// caller never reads a local answer as a fact about the remote.
	for _, test := range []struct {
		name string
		call func() error
	}{
		{
			name: "read blob",
			call: func() error {
				_, err := pinned.ReadBlob(ctx, gitcli.BlobOptions{
					Revision:       up.sha(mainOne),
					Path:           "plugin/pkg/auth/authorizer/rbac/rbac.go",
					AllowLazyFetch: true,
				})
				return err
			},
		},
		{
			name: "object info",
			call: func() error {
				_, err := pinned.ObjectInfoBatch(ctx, gitcli.ObjectInfoOptions{
					Revisions:      []string{blob},
					AllowLazyFetch: true,
				})
				return err
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.call(); !errors.Is(err, gitcli.ErrLazyFetchDisabled) {
				t.Fatalf("error = %v, want %v", err, gitcli.ErrLazyFetchDisabled)
			}
		})
	}
}
