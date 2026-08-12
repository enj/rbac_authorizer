package publish

import (
	"context"
	"strings"
	"testing"
)

func TestManifestIsIdenticalAcrossTemporaryPaths(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Two independent runs of the same publication, in different directories,
	// against different repositories. The manifest is what an operator approves
	// and what a later run compares against, so it has to describe the
	// publication and nothing about where it happened.
	render := func() (string, *destination) {
		d := newDestination(ctx, t, "")
		h := d.buildHistory()
		plan := d.planUpdates(
			branchUpdate(testBranch, h.forward),
			tagUpdate(tagPrefix+"v0.36.1", h.tagBase),
			Update{Ref: testStateRef, Kind: KindState, NewObject: h.base, Evidence: "state:cursor"},
		)
		encoded, err := plan.Manifest.JSON()
		if err != nil {
			t.Fatalf("encode manifest: %v", err)
		}
		return string(encoded), d
	}

	first, firstDest := render()
	second, secondDest := render()
	if first != second {
		t.Fatalf("manifests differ across runs:\n%s\n%s", first, second)
	}
	for _, path := range []string{firstDest.dir, firstDest.remote, secondDest.dir, secondDest.remote} {
		if strings.Contains(first, path) {
			t.Errorf("manifest names the local path %q", path)
		}
	}
	if strings.Contains(first, "/tmp") || strings.Contains(first, "/var/folders") {
		t.Errorf("manifest carries a filesystem path:\n%s", first)
	}
}

func TestManifestHashCoversEveryField(t *testing.T) {
	t.Parallel()

	base := Manifest{
		Remote:       testIdentity,
		ObjectFormat: "sha1",
		Actions: []Action{{
			Ref:       testBranch,
			Kind:      KindBranch,
			Consumer:  true,
			Effect:    EffectFastForward,
			OldObject: strings.Repeat("a", 40),
			NewObject: strings.Repeat("b", 40),
			Evidence:  "replay:master",
		}},
	}
	hash, err := base.computeHash()
	if err != nil {
		t.Fatalf("hash manifest: %v", err)
	}
	base.Hash = hash
	if err := base.Verify(); err != nil {
		t.Fatalf("a freshly hashed manifest did not verify: %v", err)
	}

	tests := []struct {
		name string
		edit func(m *Manifest)
	}{
		{name: "the destination", edit: func(m *Manifest) { m.Remote = "github.com/enj/other" }},
		{name: "the object format", edit: func(m *Manifest) { m.ObjectFormat = "sha256" }},
		{name: "the ref", edit: func(m *Manifest) { m.Actions[0].Ref = "refs/heads/other" }},
		{name: "the kind", edit: func(m *Manifest) { m.Actions[0].Kind = KindTag }},
		{name: "the audience", edit: func(m *Manifest) { m.Actions[0].Consumer = false }},
		{name: "the effect", edit: func(m *Manifest) { m.Actions[0].Effect = EffectCreate }},
		{name: "the old object", edit: func(m *Manifest) { m.Actions[0].OldObject = strings.Repeat("c", 40) }},
		{name: "the new object", edit: func(m *Manifest) { m.Actions[0].NewObject = strings.Repeat("c", 40) }},
		{name: "the evidence", edit: func(m *Manifest) { m.Actions[0].Evidence = "replay:release-1.36" }},
		{name: "an added action", edit: func(m *Manifest) { m.Actions = append(m.Actions, m.Actions[0]) }},
		{name: "a removed action", edit: func(m *Manifest) { m.Actions = nil }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			edited := base
			edited.Actions = append([]Action(nil), base.Actions...)
			test.edit(&edited)

			if err := edited.Verify(); err == nil {
				t.Fatalf("editing %s left the manifest verifying against its own hash", test.name)
			}
			computed, err := edited.computeHash()
			if err != nil {
				t.Fatalf("hash manifest: %v", err)
			}
			if computed == hash {
				t.Fatalf("editing %s did not change the hash", test.name)
			}
		})
	}
}

func TestManifestWithoutHashDoesNotVerify(t *testing.T) {
	t.Parallel()
	manifest := Manifest{Remote: testIdentity, ObjectFormat: "sha1"}
	if err := manifest.Verify(); err == nil {
		t.Fatal("a manifest with no hash verified")
	}
}

func TestManifestTextStatesEveryAction(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newDestination(ctx, t, "")
	h := d.buildHistory()
	d.seed(h.base + ":" + testBranch)

	plan := d.planUpdates(
		branchUpdate(testBranch, h.forward),
		tagUpdate(tagPrefix+"v0.36.1", h.tagBase),
		Update{Ref: testProgressRef, Kind: KindProgress, NewObject: h.base, Evidence: "backfill:chunk-1"},
	)
	text := plan.Manifest.Text()

	for _, want := range []string{
		testIdentity,
		"sha1",
		string(EffectFastForward) + " ",
		string(EffectCreate) + " ",
		"consumer",
		"non-consumer",
		testBranch + " " + h.base + " -> " + h.forward,
		tagPrefix + "v0.36.1 absent -> " + h.tagBase,
		"[replay:master]",
		"[backfill:chunk-1]",
		plan.Hash(),
	} {
		if !strings.Contains(text, want) {
			t.Errorf("manifest text does not state %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, d.remote) || strings.Contains(text, d.dir) {
		t.Errorf("manifest text names a local path:\n%s", text)
	}
	// The text and the JSON are two renderings of one artifact, so the hash the
	// text prints has to be the hash the bytes carry.
	if !strings.HasPrefix(plan.Hash(), "sha256:") {
		t.Errorf("manifest hash %q does not name its algorithm", plan.Hash())
	}
}

func TestPlanScopesSplitConsumerFromBookkeeping(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newDestination(ctx, t, "")
	h := d.buildHistory()

	plan := d.planUpdates(
		branchUpdate(testBranch, h.forward),
		tagUpdate(tagPrefix+"v0.36.1", h.tagBase),
		Update{Ref: testProgressRef, Kind: KindProgress, NewObject: h.base, Evidence: "backfill:chunk-1"},
		Update{Ref: testStateRef, Kind: KindState, NewObject: h.base, Evidence: "state:cursor"},
	)

	consumer := plan.Actions(ScopeConsumer)
	if len(consumer) != 2 {
		t.Fatalf("consumer scope holds %d actions, want 2", len(consumer))
	}
	for _, action := range consumer {
		if !action.Consumer {
			t.Errorf("%s is in the consumer scope and is not a consumer ref", action.Ref)
		}
	}
	internal := plan.Actions(ScopeNonConsumer)
	if len(internal) != 2 {
		t.Fatalf("non-consumer scope holds %d actions, want 2", len(internal))
	}
	for _, action := range internal {
		if action.Consumer {
			t.Errorf("%s is in the non-consumer scope and is a consumer ref", action.Ref)
		}
	}
	if pending := plan.Pending(ScopeConsumer); len(pending) != 2 {
		t.Errorf("consumer scope has %d pending actions, want 2", len(pending))
	}
	if actions := plan.Actions(Scope("everything")); len(actions) != 0 {
		t.Errorf("an unknown scope selected %d actions, want none", len(actions))
	}
}
