package publish

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/enj/soapbox/tools/internal/gitcli"
)

// Plan decides what each requested update would do and renders the manifest.
//
// Nothing is written. The remote is read, the local object graph is queried,
// and the result is a description an operator can approve and a later Apply can
// prove it is executing unchanged. A plan that would refuse at apply time
// refuses here instead, which is the whole reason the two are separate: a
// non fast forward branch, a tag that already means something else, or a remote
// that disagrees with the caller's observation are all findings about the
// publication rather than accidents of when the push happened to run.
func (p *Publisher) Plan(ctx context.Context, updates []Update) (*Plan, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("publication plan: %w", err)
	}
	if len(updates) == 0 {
		return nil, errors.New("publication plan: at least one update is required")
	}
	if err := p.validateUpdates(updates); err != nil {
		return nil, fmt.Errorf("publication plan: %w", err)
	}
	observed, err := p.remoteRefs(ctx)
	if err != nil {
		return nil, fmt.Errorf("publication plan: %w", err)
	}
	objects, err := p.describeObjects(ctx, updates, observed)
	if err != nil {
		return nil, fmt.Errorf("publication plan: %w", err)
	}

	actions := make([]Action, 0, len(updates))
	for _, update := range updates {
		action, err := p.classify(ctx, update, observed, objects)
		if err != nil {
			return nil, fmt.Errorf("publication plan: %w", err)
		}
		actions = append(actions, action)
	}
	// Sorting by destination ref is what makes two runs of the same
	// publication comparable: the caller's iteration order decides the order of
	// updates, and a manifest that inherited it would hash differently for a
	// set of actions that is the same set.
	slices.SortFunc(actions, func(a, b Action) int { return strings.Compare(a.Ref, b.Ref) })

	manifest := Manifest{
		Remote:       p.identity,
		ObjectFormat: string(p.format),
		Actions:      actions,
	}
	hash, err := manifest.computeHash()
	if err != nil {
		return nil, fmt.Errorf("publication plan: %w", err)
	}
	manifest.Hash = hash
	return &Plan{Manifest: manifest}, nil
}

// describeObjects probes every object a plan reasons about, in one batch.
//
// Both ends of each update are probed. The new object has to exist locally or
// the push could not send it, and its type has to match the ref kind. The old
// object has to exist locally for a ref that already exists, because proving a
// fast forward means walking from it, and an absent one would make the walk
// answer "not an ancestor" for a reason that has nothing to do with the
// history.
//
// Lazy fetching stays off. The probe asks what this repository already has, and
// a partial clone that answered by downloading would turn a local question into
// a network one.
func (p *Publisher) describeObjects(ctx context.Context, updates []Update, observed map[string]string) (map[string]gitcli.ObjectInfo, error) {
	var revisions []string
	for _, update := range updates {
		revisions = append(revisions, update.NewObject)
		if old, ok := observed[update.Ref]; ok {
			revisions = append(revisions, old)
		}
	}
	slices.Sort(revisions)
	revisions = slices.Compact(revisions)

	infos, err := p.git.ObjectInfoBatch(ctx, gitcli.ObjectInfoOptions{Revisions: revisions})
	if err != nil {
		return nil, fmt.Errorf("describe publication objects: %w", err)
	}
	if len(infos) != len(revisions) {
		return nil, fmt.Errorf("describe publication objects: got %d records, want %d", len(infos), len(revisions))
	}
	described := make(map[string]gitcli.ObjectInfo, len(revisions))
	for i, info := range infos {
		described[revisions[i]] = info
	}
	return described, nil
}

// classify decides one action against the remote and the local object graph.
func (p *Publisher) classify(ctx context.Context, update Update, observed map[string]string, objects map[string]gitcli.ObjectInfo) (Action, error) {
	if err := p.requireObject(update.NewObject, objects, wantType(update.Kind)); err != nil {
		return Action{}, fmt.Errorf("%q: new object: %w", update.Ref, err)
	}
	old, present := observed[update.Ref]
	if err := checkExpectation(update, old, present); err != nil {
		return Action{}, err
	}

	action := Action{
		Ref:       update.Ref,
		Kind:      update.Kind,
		Consumer:  update.Kind.Consumer(),
		OldObject: old,
		NewObject: update.NewObject,
		Evidence:  update.Evidence,
	}
	switch {
	case !present:
		action.Effect = EffectCreate
	case old == update.NewObject:
		// A completed publication that runs again is the normal case, not a
		// collision: the remote already holds exactly what was asked for.
		action.Effect = EffectNoOp
	case update.Kind == KindTag:
		return Action{}, fmt.Errorf("%q: %w: it holds %s and the plan names %s", update.Ref, ErrTagMoved, old, update.NewObject)
	default:
		if err := p.requireObject(old, objects, ""); err != nil {
			return Action{}, fmt.Errorf("%q: the remote value cannot be proved an ancestor: %w", update.Ref, err)
		}
		forward, err := p.git.IsAncestor(ctx, old, update.NewObject)
		if err != nil {
			return Action{}, fmt.Errorf("%q: %w", update.Ref, err)
		}
		if !forward {
			return Action{}, fmt.Errorf("%q: %w: %s does not descend from %s", update.Ref, ErrNonFastForward, update.NewObject, old)
		}
		action.Effect = EffectFastForward
	}
	return action, nil
}

// checkExpectation compares the remote with what the caller said it observed.
func checkExpectation(update Update, old string, present bool) error {
	switch {
	case update.ExpectAbsent && present:
		return fmt.Errorf("%q: %w: no ref was expected and the remote holds %s", update.Ref, ErrExpectation, old)
	case update.ExpectedOld != "" && !present:
		return fmt.Errorf("%q: %w: %s was expected and the remote has no such ref", update.Ref, ErrExpectation, update.ExpectedOld)
	case update.ExpectedOld != "" && update.ExpectedOld != old:
		return fmt.Errorf("%q: %w: %s was expected and the remote holds %s", update.Ref, ErrExpectation, update.ExpectedOld, old)
	default:
		return nil
	}
}

// wantType reports the object type a ref kind must point at.
//
// A release tag must be an annotated tag object rather than a commit. The
// annotation is not decoration: it carries the tagger identity, the upstream
// release date, and the source release the tag reproduces, so a lightweight tag
// with the right name would publish a version that cannot say where it came
// from. Every other kind points at a commit.
func wantType(kind Kind) string {
	if kind == KindTag {
		return "tag"
	}
	return "commit"
}

// requireObject reports an object that is absent locally or of the wrong type.
// An empty want skips the type check, which is what a probe for presence alone
// asks for.
func (p *Publisher) requireObject(name string, objects map[string]gitcli.ObjectInfo, want string) error {
	info, ok := objects[name]
	if !ok || info.Missing {
		return fmt.Errorf("%s: %w", name, ErrObjectMissing)
	}
	if want != "" && info.Type != want {
		return fmt.Errorf("%s is a %s and a %s was required: %w", name, info.Type, want, ErrObjectType)
	}
	return nil
}
