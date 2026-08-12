package publish

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// Action is one outward change a plan would make, decided against what the
// remote held when the plan was made.
type Action struct {
	// Ref is the fully qualified destination ref.
	Ref string `json:"ref"`
	// Kind classifies the ref.
	Kind Kind `json:"kind"`
	// Consumer reports whether a module user or the module proxy can see this
	// ref. It is rendered rather than left to be derived from the kind, because
	// it is the property a person approving a manifest is actually reading for.
	Consumer bool `json:"consumer"`
	// Effect is what applying this action would do.
	Effect Effect `json:"effect"`
	// OldObject is what the remote held, empty when the ref does not exist.
	OldObject string `json:"oldObject"`
	// NewObject is what the ref will point at.
	NewObject string `json:"newObject"`
	// Evidence labels where the update came from.
	Evidence string `json:"evidence"`
}

// Manifest is the exact set of outward actions a plan would perform.
//
// It is the artifact the outward action gate approves, so it is built to be
// compared rather than read once: field order is fixed, actions are sorted by
// destination ref, and nothing in it depends on where the run happened. Two
// runs that would publish the same thing produce the same bytes and the same
// hash, on any machine, from any directory.
type Manifest struct {
	// Remote is the canonical destination repository, without credentials and
	// without a location.
	Remote string `json:"remote"`
	// ObjectFormat is the hash algorithm every object name is written in.
	ObjectFormat string `json:"objectFormat"`
	// Actions are the outward changes, sorted by destination ref.
	Actions []Action `json:"actions"`
	// Hash digests every other field. It is excluded from its own input, which
	// is what lets an approval name a manifest by hash and lets Apply prove the
	// plan it was handed is the one that was approved.
	Hash string `json:"hash"`
}

// JSON renders the manifest as deterministic, indented bytes with a trailing
// newline.
func (m Manifest) JSON() ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	// HTML escaping would turn characters in a ref name or an evidence label
	// into escapes and make the bytes depend on the encoder rather than on the
	// publication.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(m); err != nil {
		return nil, fmt.Errorf("encode publication manifest: %w", err)
	}
	return buf.Bytes(), nil
}

// computeHash digests the manifest with its own hash field cleared.
//
// Clearing rather than omitting keeps one renderer for both purposes: the bytes
// that are hashed are the bytes JSON produces for a manifest that has not been
// hashed yet, so there is no second serialization that could drift from the
// first and make a manifest disagree with its own digest.
func (m Manifest) computeHash() (string, error) {
	unhashed := m
	unhashed.Hash = ""
	encoded, err := unhashed.JSON()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// Verify recomputes the hash and reports a manifest that was modified after it
// was rendered.
func (m Manifest) Verify() error {
	if m.Hash == "" {
		return fmt.Errorf("%w: the manifest carries no hash", ErrManifestModified)
	}
	computed, err := m.computeHash()
	if err != nil {
		return err
	}
	if computed != m.Hash {
		return fmt.Errorf("%w: it carries %s and hashes to %s", ErrManifestModified, m.Hash, computed)
	}
	return nil
}

// Text renders the manifest for a person, deterministically.
//
// It states the same facts as the JSON form in the same order, so an operator
// reading the text and a gate comparing the hash are looking at one artifact
// rather than two renderings that could disagree.
func (m Manifest) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "publication manifest for %s (%s)\n", m.Remote, m.ObjectFormat)
	if len(m.Actions) == 0 {
		b.WriteString("  no actions\n")
	}
	for _, action := range m.Actions {
		old := action.OldObject
		if old == "" {
			old = "absent"
		}
		fmt.Fprintf(&b, "  %-12s %-8s %-12s %s %s -> %s [%s]\n",
			string(action.Effect), string(action.Kind), audience(action.Consumer),
			action.Ref, old, action.NewObject, action.Evidence)
	}
	fmt.Fprintf(&b, "  hash %s\n", m.Hash)
	return b.String()
}

// audience names who can observe a ref.
func audience(consumer bool) string {
	if consumer {
		return "consumer"
	}
	return "non-consumer"
}

// Plan is an immutable description of one publication, approved by hash.
//
// It carries nothing beyond the manifest on purpose. Everything Apply needs,
// which ref, from which object, to which object, is in the manifest the
// approval names, so there is no second copy of the plan that could differ from
// the one that was read and approved.
type Plan struct {
	// Manifest is the exact outward action set.
	Manifest Manifest
}

// Hash reports the manifest hash an approval must name.
func (p *Plan) Hash() string {
	if p == nil {
		return ""
	}
	return p.Manifest.Hash
}

// Actions reports the actions in one scope, in manifest order.
func (p *Plan) Actions(scope Scope) []Action {
	if p == nil {
		return nil
	}
	var actions []Action
	for _, action := range p.Manifest.Actions {
		if scope.covers(action) {
			actions = append(actions, action)
		}
	}
	return actions
}

// Pending reports the actions in one scope that would change the remote.
func (p *Plan) Pending(scope Scope) []Action {
	var pending []Action
	for _, action := range p.Actions(scope) {
		if action.Effect != EffectNoOp {
			pending = append(pending, action)
		}
	}
	return pending
}
