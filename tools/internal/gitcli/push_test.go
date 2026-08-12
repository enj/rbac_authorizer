package gitcli_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/testsupport"
)

// pushFixture is a repository with a three commit chain and a real remote to
// push it to.
//
// The remote's own branch is left unborn and is never a destination, so no push
// in these tests is refused for being the checked out branch and every refusal
// under test is the one being tested.
type pushFixture struct {
	local  *testsupport.Repo
	remote *testsupport.Repo
	// base, middle, and head are a linear chain, so middle descends from base
	// and head from middle.
	base   string
	middle string
	head   string
}

const publishedRef = "refs/heads/published"

func newPushFixture(ctx context.Context, t *testing.T) *pushFixture {
	t.Helper()
	local := testsupport.NewRepo(ctx, t, testsupport.Options{UserName: testUserName, UserEmail: testUserEmail})
	remote := testsupport.NewRepo(ctx, t, testsupport.Options{Branch: "trunk", UserName: testUserName, UserEmail: testUserEmail})
	return &pushFixture{
		local:  local,
		remote: remote,
		base:   local.WriteAndCommit(ctx, t, "a.txt", "a\n", "feat: a\n"),
		middle: local.WriteAndCommit(ctx, t, "b.txt", "b\n", "feat: b\n"),
		head:   local.WriteAndCommit(ctx, t, "c.txt", "c\n", "feat: c\n"),
	}
}

// seed puts one ref on the remote, standing in for whatever wrote there before.
func (f *pushFixture) seed(ctx context.Context, t *testing.T, ref, object string) {
	t.Helper()
	if err := f.local.Git.Push(ctx, f.remote.Dir, object+":"+ref); err != nil {
		t.Fatalf("seed %s: %v", ref, err)
	}
}

// published reports what the remote holds, or the empty string.
func (f *pushFixture) published(ctx context.Context, t *testing.T, ref string) string {
	t.Helper()
	refs, err := f.remote.Git.ListRefs(ctx, ref)
	if err != nil {
		t.Fatalf("read %s: %v", ref, err)
	}
	if len(refs) == 0 {
		return ""
	}
	return refs[0].Target
}

// TestPushAtomicRefusesAStaleLease is the reason this API exists.
//
// The remote moves from the value the caller read to a descendant of it, and
// the object being pushed descends from that descendant. Every fast forward
// rule is satisfied and a plain push would be accepted, publishing an update
// against a state nobody read. The lease compares values instead, inside the
// ref lock, and refuses.
func TestPushAtomicRefusesAStaleLease(t *testing.T) {
	ctx := t.Context()
	f := newPushFixture(ctx, t)
	f.seed(ctx, t, publishedRef, f.base)
	f.seed(ctx, t, publishedRef, f.middle)

	err := f.local.Git.PushAtomic(ctx, f.remote.Dir, []gitcli.PushUpdate{
		{Ref: publishedRef, New: f.head, ExpectedOld: f.base},
	})
	if err == nil {
		t.Fatal("a push whose lease was stale succeeded")
	}
	if !strings.Contains(err.Error(), "stale info") {
		t.Fatalf("error %q does not report the stale lease", err)
	}
	if got := f.published(ctx, t, publishedRef); got != f.middle {
		t.Fatalf("%s = %q, want the value the other writer left, %q", publishedRef, got, f.middle)
	}
}

func TestPushAtomicAdvancesOnAMatchingLease(t *testing.T) {
	ctx := t.Context()
	f := newPushFixture(ctx, t)
	f.seed(ctx, t, publishedRef, f.base)

	err := f.local.Git.PushAtomic(ctx, f.remote.Dir, []gitcli.PushUpdate{
		{Ref: publishedRef, New: f.head, ExpectedOld: f.base},
	})
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if got := f.published(ctx, t, publishedRef); got != f.head {
		t.Fatalf("%s = %q, want %q", publishedRef, got, f.head)
	}
}

// TestPushAtomicCreatesOnlyWhatIsAbsent proves the create form is exact.
//
// A create whose ref appeared in the meantime must not become an update to it,
// even when the object being pushed descends from what appeared and git would
// therefore treat the push as an ordinary fast forward.
func TestPushAtomicCreatesOnlyWhatIsAbsent(t *testing.T) {
	ctx := t.Context()
	f := newPushFixture(ctx, t)

	err := f.local.Git.PushAtomic(ctx, f.remote.Dir, []gitcli.PushUpdate{
		{Ref: publishedRef, New: f.base, ExpectAbsent: true},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if got := f.published(ctx, t, publishedRef); got != f.base {
		t.Fatalf("%s = %q, want %q", publishedRef, got, f.base)
	}

	err = f.local.Git.PushAtomic(ctx, f.remote.Dir, []gitcli.PushUpdate{
		{Ref: publishedRef, New: f.head, ExpectAbsent: true},
	})
	if err == nil {
		t.Fatal("a create against an existing ref succeeded")
	}
	if got := f.published(ctx, t, publishedRef); got != f.base {
		t.Fatalf("%s = %q, want the create to have left %q alone", publishedRef, got, f.base)
	}
}

// TestPushAtomicRefusesARewind is the other half of the lease's meaning.
//
// A lease is a compare and swap, not authorization: git accepts a rewind whose
// lease matches, so nothing in git stops this push. The refusal has to come
// from this package, before the subprocess starts.
func TestPushAtomicRefusesARewind(t *testing.T) {
	ctx := t.Context()
	f := newPushFixture(ctx, t)
	f.seed(ctx, t, publishedRef, f.head)

	err := f.local.Git.PushAtomic(ctx, f.remote.Dir, []gitcli.PushUpdate{
		{Ref: publishedRef, New: f.base, ExpectedOld: f.head},
	})
	if err == nil {
		t.Fatal("a rewind with a matching lease succeeded")
	}
	if !strings.Contains(err.Error(), "does not descend from") {
		t.Fatalf("error %q does not report the rewind", err)
	}
	if got := f.published(ctx, t, publishedRef); got != f.head {
		t.Fatalf("%s = %q, want %q", publishedRef, got, f.head)
	}
}

// TestPushAtomicRollsBackEveryRef proves one stale lease stops the batch.
func TestPushAtomicRollsBackEveryRef(t *testing.T) {
	ctx := t.Context()
	f := newPushFixture(ctx, t)
	const otherRef = "refs/heads/other"
	f.seed(ctx, t, publishedRef, f.base)
	f.seed(ctx, t, otherRef, f.base)
	f.seed(ctx, t, otherRef, f.middle)

	err := f.local.Git.PushAtomic(ctx, f.remote.Dir, []gitcli.PushUpdate{
		{Ref: publishedRef, New: f.middle, ExpectedOld: f.base},
		{Ref: otherRef, New: f.head, ExpectedOld: f.base},
	})
	if err == nil {
		t.Fatal("a batch holding a stale lease succeeded")
	}
	if got := f.published(ctx, t, publishedRef); got != f.base {
		t.Errorf("%s = %q, want %q: the batch is atomic", publishedRef, got, f.base)
	}
	if got := f.published(ctx, t, otherRef); got != f.middle {
		t.Errorf("%s = %q, want %q", otherRef, got, f.middle)
	}
}

func TestPushAtomicValidatesUpdates(t *testing.T) {
	ctx := t.Context()
	f := newPushFixture(ctx, t)
	nullName := strings.Repeat("0", len(f.base))

	tests := []struct {
		name    string
		updates []gitcli.PushUpdate
		wantErr error
		want    string
	}{
		{
			name: "no updates",
			want: "at least one update is required",
		},
		{
			name:    "no lease",
			updates: []gitcli.PushUpdate{{Ref: publishedRef, New: f.head}},
			want:    "must state the value the remote is expected to hold",
		},
		{
			name:    "both lease forms",
			updates: []gitcli.PushUpdate{{Ref: publishedRef, New: f.head, ExpectedOld: f.base, ExpectAbsent: true}},
			want:    "cannot both expect no ref",
		},
		{
			name:    "a force marker",
			updates: []gitcli.PushUpdate{{Ref: "+" + publishedRef, New: f.head, ExpectAbsent: true}},
			wantErr: gitcli.ErrForceRefspec,
		},
		{
			name:    "the null object name",
			updates: []gitcli.PushUpdate{{Ref: publishedRef, New: nullName, ExpectAbsent: true}},
			wantErr: gitcli.ErrDeleteRefspec,
		},
		{
			name:    "a null expected value",
			updates: []gitcli.PushUpdate{{Ref: publishedRef, New: f.head, ExpectedOld: nullName}},
			wantErr: gitcli.ErrDeleteRefspec,
		},
		{
			name:    "no object at all",
			updates: []gitcli.PushUpdate{{Ref: publishedRef, ExpectAbsent: true}},
			want:    "must be a full object name",
		},
		{
			name:    "an abbreviated object name",
			updates: []gitcli.PushUpdate{{Ref: publishedRef, New: f.head[:12], ExpectAbsent: true}},
			want:    "must be a full object name",
		},
		{
			name:    "a revision expression",
			updates: []gitcli.PushUpdate{{Ref: publishedRef, New: "HEAD~1", ExpectAbsent: true}},
			want:    "must be a full object name",
		},
		{
			name:    "an unqualified ref",
			updates: []gitcli.PushUpdate{{Ref: "published", New: f.head, ExpectAbsent: true}},
			want:    "must be hierarchical",
		},
		{
			name: "one ref twice",
			updates: []gitcli.PushUpdate{
				{Ref: publishedRef, New: f.head, ExpectAbsent: true},
				{Ref: publishedRef, New: f.base, ExpectAbsent: true},
			},
			want: "appears more than once",
		},
		{
			name:    "an update that is not a fast forward",
			updates: []gitcli.PushUpdate{{Ref: publishedRef, New: f.base, ExpectedOld: f.middle}},
			wantErr: gitcli.ErrForceRefspec,
		},
		{
			name:    "an expected value this repository does not have",
			updates: []gitcli.PushUpdate{{Ref: publishedRef, New: f.head, ExpectedOld: strings.Repeat("1", len(f.base))}},
			want:    "ancestor probe",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := f.local.Git.PushAtomic(ctx, f.remote.Dir, test.updates)
			if err == nil {
				t.Fatal("the update was accepted, want a refusal")
			}
			if test.wantErr != nil && !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want %v", err, test.wantErr)
			}
			if test.want != "" && !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error %q does not state %q", err, test.want)
			}
			// Nothing may reach the remote from an update that was refused.
			if got := f.published(ctx, t, publishedRef); got != "" {
				t.Fatalf("%s = %q, want the remote untouched", publishedRef, got)
			}
		})
	}
}

func TestPushAtomicRefusesABadRemote(t *testing.T) {
	ctx := t.Context()
	f := newPushFixture(ctx, t)
	update := []gitcli.PushUpdate{{Ref: publishedRef, New: f.head, ExpectAbsent: true}}

	for _, remote := range []string{"", "origin", "https://example.invalid/enj/x.git", "https://ghs_token@github.com/enj/x.git"} {
		err := f.local.Git.PushAtomic(ctx, remote, update)
		if err == nil {
			t.Errorf("remote %q was accepted", remote)
			continue
		}
		if strings.Contains(err.Error(), "ghs_token") {
			t.Errorf("error echoed a credential: %v", err)
		}
	}
}
