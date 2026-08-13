package sync_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/enj/soapbox/tools/internal/extract"
	"github.com/enj/soapbox/tools/internal/generate"
	"github.com/enj/soapbox/tools/internal/publish"
	"github.com/enj/soapbox/tools/internal/sync"
)

// TestProjectPlansTheFirstReleaseWithoutTouchingTheRemote is the shape every
// other test here varies: a destination that has never been published to, one
// release, and a plan that says exactly what publishing it would do.
func TestProjectPlansTheFirstReleaseWithoutTouchingTheRemote(t *testing.T) {
	ctx := t.Context()
	dest := newDestination(ctx, t)

	result, err := sync.Project(ctx, dest.options())
	if err != nil {
		t.Fatalf("project: %v", err)
	}

	if got := result.Manifest.Source.ReleaseTag; got != testReleaseTag {
		t.Errorf("release tag = %q, want %q", got, testReleaseTag)
	}
	if result.Manifest.Objects.Tree == "" || result.Manifest.Objects.Commit == "" {
		t.Errorf("objects = %+v, want a tree and a commit", result.Manifest.Objects)
	}
	// The release projection tree is the replayed tree for a single release
	// epoch, so the tag names the replayed commit and no projection commit
	// exists. A commit here would mean the engine published an empty change.
	if got := result.Manifest.Objects.ProjectionCommit; got != "" {
		t.Errorf("projection commit = %q, want none: the release tree is the replayed tree", got)
	}
	if got, want := result.Manifest.Objects.TagTarget, result.Manifest.Objects.Commit; got != want {
		t.Errorf("tag target = %q, want the replayed commit %q", got, want)
	}

	// Three refs, each created, and every one of them still absent.
	want := map[string]publish.Effect{
		testStateRef:                  publish.EffectCreate,
		testBranchRef:                 publish.EffectCreate,
		"refs/tags/" + testReleaseTag: publish.EffectCreate,
	}
	got := make(map[string]publish.Effect)
	for _, action := range result.Manifest.Publish.Actions {
		got[action.Ref] = action.Effect
		if action.OldObject != "" {
			t.Errorf("action %s reports old object %q, want none on a fresh destination", action.Ref, action.OldObject)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("planned refs = %v, want %v", got, want)
	}
	for ref, effect := range want {
		if got[ref] != effect {
			t.Errorf("ref %s effect = %q, want %q", ref, got[ref], effect)
		}
	}

	if refs := dest.remoteRefs(ctx, t); len(refs) != 0 {
		t.Errorf("the remote holds %v after planning, want nothing: a plan publishes nothing", refs)
	}
}

// TestPlanIsDeterministicAcrossRoots proves the manifest describes the work
// rather than the machine.
//
// Two synchronizations of one release into two different temporary directories
// have to produce the same hash. It is the property the whole approval flow
// rests on: an operator approves a hash computed on one machine and the
// publication that quotes it runs on another.
func TestPlanIsDeterministicAcrossRoots(t *testing.T) {
	ctx := t.Context()

	first, err := sync.Project(ctx, newDestination(ctx, t).options())
	if err != nil {
		t.Fatalf("first project: %v", err)
	}
	second, err := sync.Project(ctx, newDestination(ctx, t).options())
	if err != nil {
		t.Fatalf("second project: %v", err)
	}

	if first.Manifest.Hash != second.Manifest.Hash {
		t.Errorf("manifest hash differs across roots:\n first  %s\n second %s",
			first.Manifest.Hash, second.Manifest.Hash)
	}
	firstJSON, err := first.Manifest.JSON()
	if err != nil {
		t.Fatalf("encode first manifest: %v", err)
	}
	secondJSON, err := second.Manifest.JSON()
	if err != nil {
		t.Fatalf("encode second manifest: %v", err)
	}
	if string(firstJSON) != string(secondJSON) {
		t.Errorf("manifest bytes differ across roots:\n%s\n%s", firstJSON, secondJSON)
	}
}

// TestManifestCarriesNoPathOrSecret checks the artifact an approval quotes.
//
// The manifest is attached to a review and kept as the record of what was
// authorized, so a temporary directory from the machine that produced it would
// both break the comparison above and leak into that record.
func TestManifestCarriesNoPathOrSecret(t *testing.T) {
	ctx := t.Context()
	dest := newDestination(ctx, t)

	result, err := sync.Project(ctx, dest.options())
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	encoded, err := result.Manifest.JSON()
	if err != nil {
		t.Fatalf("encode manifest: %v", err)
	}

	rendered := string(encoded) + result.Manifest.Text()
	for _, forbidden := range []struct{ what, value string }{
		{"the local repository", dest.dir},
		{"the destination remote path", dest.remote},
	} {
		if forbidden.value == "" {
			continue
		}
		if strings.Contains(rendered, forbidden.value) {
			t.Errorf("the manifest names %s (%q)", forbidden.what, forbidden.value)
		}
	}
	// The canonical destination is a repository rather than a location, which is
	// what makes the manifest comparable between a local dry run and the real
	// publication it rehearses.
	if got := result.Manifest.Publish.Remote; got != testIdentity {
		t.Errorf("manifest remote = %q, want the canonical identity %q", got, testIdentity)
	}
}

// TestManifestHashCoversEveryField proves the approval hash is worth quoting.
//
// An approval names one hash, so every field a reviewer reads has to be inside
// it. A field that could change without changing the hash is a field an
// approval does not actually cover.
func TestManifestHashCoversEveryField(t *testing.T) {
	ctx := t.Context()

	result, err := sync.Project(ctx, newDestination(ctx, t).options())
	if err != nil {
		t.Fatalf("project: %v", err)
	}

	for _, tc := range []struct {
		name   string
		mutate func(*sync.Manifest)
	}{
		{"the upstream commit", func(m *sync.Manifest) { m.Source.Commit = strings.Repeat("9", 40) }},
		{"the profile hash", func(m *sync.Manifest) { m.Engine.ProfileHash = "sha256:" + strings.Repeat("f", 64) }},
		{"the generated tree", func(m *sync.Manifest) { m.Objects.Tree = strings.Repeat("9", 40) }},
		{"the state commit", func(m *sync.Manifest) { m.Objects.StateCommit = strings.Repeat("9", 40) }},
		{"the module manifest hash", func(m *sync.Manifest) { m.Module.ManifestHash = "sha256:" + strings.Repeat("c", 64) }},
		{"a behavior change", func(m *sync.Manifest) { m.Module.BehaviorChanges = nil }},
		{"the dependency decision", func(m *sync.Manifest) { m.Module.Dependencies.Copied = 7 }},
		{"a planned ref", func(m *sync.Manifest) { m.Publish.Actions[0].Ref = "refs/heads/elsewhere" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			modified := result.Manifest
			modified.Publish.Actions = append([]publish.Action(nil), modified.Publish.Actions...)
			tc.mutate(&modified)
			if err := modified.Verify(); err == nil {
				t.Fatalf("changing %s left the manifest verifying, so the hash does not cover it", tc.name)
			} else if !errors.Is(err, sync.ErrManifestModified) && !errors.Is(err, publish.ErrManifestModified) {
				t.Fatalf("changing %s reported %v, want a modified manifest", tc.name, err)
			}
		})
	}
}

// TestProjectHonoursCancellation proves a cancelled run writes nothing.
func TestProjectHonoursCancellation(t *testing.T) {
	ctx := t.Context()
	dest := newDestination(ctx, t)

	cancelled, cancel := context.WithCancel(ctx)
	cancel()

	if _, err := sync.Project(cancelled, dest.options()); !errors.Is(err, context.Canceled) {
		t.Fatalf("project = %v, want a cancellation", err)
	}
	if refs := dest.remoteRefs(ctx, t); len(refs) != 0 {
		t.Errorf("the remote holds %v after a cancelled run, want nothing", refs)
	}
}

// TestProjectRefusesWithoutADestinationRemote proves publication is off by
// default rather than merely unlikely.
func TestProjectRefusesWithoutADestinationRemote(t *testing.T) {
	ctx := t.Context()
	dest := newDestination(ctx, t)

	opts := dest.options()
	opts.Destination.Remote = ""
	if _, err := sync.Project(ctx, opts); !errors.Is(err, sync.ErrPublicationDisabled) {
		t.Fatalf("project = %v, want a disabled publication", err)
	}

	// A local remote is reachable only because the caller said so. Without the
	// option a mistyped configuration would publish into a directory.
	opts = dest.options()
	opts.Destination.AllowLocalRemote = false
	if _, err := sync.Project(ctx, opts); !errors.Is(err, publish.ErrLocalRemoteNotAllowed) {
		t.Fatalf("project = %v, want a refused local remote", err)
	}
}

// TestPlanRefusesARunShapeItCannotPublish covers the guards Plan applies before
// it starts a generation.
//
// They run first because a generation is minutes of work against a clone of
// upstream, and a run that could never have published its result should not
// spend them. A branch is the case that matters: it is a well formed request
// this engine cannot serve, because a release is what gets published and a
// branch names none.
func TestPlanRefusesARunShapeItCannotPublish(t *testing.T) {
	ctx := t.Context()
	dest := newDestination(ctx, t)

	base := func() sync.Options {
		return sync.Options{
			Generate: generate.Options{
				Config: testConfig(),
				Ref:    extract.Ref{Kind: extract.RefTag, Name: testSourceTag},
			},
			Destination: dest.options().Destination,
		}
	}

	for _, tc := range []struct {
		name   string
		mutate func(*sync.Options)
		want   error
	}{{
		name:   "a branch instead of a release",
		mutate: func(o *sync.Options) { o.Generate.Ref = extract.Ref{Kind: extract.RefBranch, Name: "master"} },
		want:   sync.ErrUnsupported,
	}, {
		name:   "no profile",
		mutate: func(o *sync.Options) { o.Generate.Config = nil },
		want:   nil,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			opts := base()
			tc.mutate(&opts)
			_, err := sync.Plan(ctx, opts)
			if err == nil {
				t.Fatalf("plan succeeded, want a refusal")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("plan = %v, want %v", err, tc.want)
			}
			if refs := dest.remoteRefs(ctx, t); len(refs) != 0 {
				t.Errorf("the remote holds %v after a refused plan, want nothing", refs)
			}
		})
	}
}
