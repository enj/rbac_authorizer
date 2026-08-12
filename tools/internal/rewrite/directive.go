package rewrite

import (
	"strings"
)

// DirectiveKind separates the two comment conventions a Kubernetes source file
// carries.
type DirectiveKind uint8

const (
	// MarkerDirective is a Kubernetes generator marker such as
	// `// +k8s:conversion-gen=k8s.io/kubernetes/pkg/apis/rbac`.
	MarkerDirective DirectiveKind = iota
	// GoDirective is a toolchain directive such as `//go:generate`, which the
	// Go specification requires to start immediately after the slashes.
	GoDirective
)

// Directive is one parsed directive comment.
type Directive struct {
	// Kind separates a generator marker from a toolchain directive.
	Kind DirectiveKind
	// Key is the directive name without its leading plus, such as
	// k8s:conversion-gen, groupName, or go:generate.
	Key string
	// Value is everything after the first equals sign, or after the directive
	// name for a toolchain directive. Empty when the directive names no value.
	Value string
	// Line is the one based line the directive sits on.
	Line int
	// Text is the complete comment text including its leading slashes.
	Text string
}

// DirectiveAction is what happens to a directive whose target still exists.
type DirectiveAction uint8

const (
	// DirectiveKeep leaves the directive byte for byte. It is the action for
	// every key with no rule, so a marker nobody has reasoned about survives.
	DirectiveKeep DirectiveAction = iota
	// DirectiveRewrite rewrites source prefixed import paths inside the value
	// and leaves every other value alone.
	DirectiveRewrite
	// DirectiveRemove removes the directive line.
	DirectiveRemove
)

// DirectiveRule is the handling of one directive key.
type DirectiveRule struct {
	// Action is what happens while the directive's target still exists.
	Action DirectiveAction
	// RemoveWhenDangling removes the directive when the caller reports that its
	// target, or the output it used to generate, was pruned. A marker that
	// points at something no longer in the tree is worse than no marker: it
	// describes a generator run that can never be reproduced.
	RemoveWhenDangling bool
}

// DirectiveRules is the key scoped policy.
//
// The policy is an allowlist keyed by exact directive name. Nothing is matched
// by pattern and nothing is rewritten globally, because a marker value can be
// an API group, an annotation, or free text that merely looks like an import
// path, and a global replacement would corrupt all three.
type DirectiveRules struct {
	// Rules maps an exact key to its handling. A key with no entry is kept.
	Rules map[string]DirectiveRule
	// Dangling reports whether a directive's target was pruned. A nil callback
	// reports nothing as dangling, so a caller that has not run pruning yet
	// cannot strip a marker by omission.
	Dangling func(Directive) bool
}

// ruleFor reports the handling of one key.
func (r DirectiveRules) ruleFor(key string) DirectiveRule {
	return r.Rules[key]
}

// dangling reports whether the caller considers the directive's target pruned.
func (r DirectiveRules) dangling(directive Directive) bool {
	return r.Dangling != nil && r.Dangling(directive)
}

// DefaultRules is the handling the RBAC profile needs, and a reasonable start
// for any Kubernetes package.
//
// Two decisions are encoded here. Generator markers that name an input package
// are rewritten so they keep pointing at the relocated package, and are removed
// when that package was pruned. `go:generate` is removed because the generated
// module contains neither the generators nor the build harness the command
// names, so keeping it would leave an instruction that cannot run and that a
// future maintainer would have to investigate before discovering it is dead.
//
// `+groupName` and the external type markers are kept explicitly rather than by
// falling through to the default, because keeping them is a decision: the API
// group is part of the retained behaviour, and the external marker points at a
// public type that is deliberately never relocated.
func DefaultRules() DirectiveRules {
	return DirectiveRules{Rules: map[string]DirectiveRule{
		"groupName":                         {Action: DirectiveKeep},
		"groupGoName":                       {Action: DirectiveKeep},
		"k8s:conversion-gen-external-types": {Action: DirectiveKeep},
		"k8s:conversion-gen":                {Action: DirectiveRewrite, RemoveWhenDangling: true},
		"k8s:defaulter-gen-input":           {Action: DirectiveRewrite, RemoveWhenDangling: true},
		"k8s:validation-gen-input":          {Action: DirectiveRewrite, RemoveWhenDangling: true},
		"k8s:deepcopy-gen":                  {Action: DirectiveKeep, RemoveWhenDangling: true},
		"k8s:defaulter-gen":                 {Action: DirectiveKeep, RemoveWhenDangling: true},
		"k8s:validation-gen":                {Action: DirectiveKeep, RemoveWhenDangling: true},
		"k8s:protobuf-gen":                  {Action: DirectiveRewrite, RemoveWhenDangling: true},
		"k8s:openapi-gen":                   {Action: DirectiveKeep, RemoveWhenDangling: true},
		"go:generate":                       {Action: DirectiveRemove},
	}}
}

// ParseDirective reads a directive out of a comment, reporting whether the
// comment is one.
//
// It is exported so the analyses that reason about markers before a rewrite
// runs read them with exactly the parser that will later act on them. A second
// implementation elsewhere would eventually disagree about what counts as a
// directive, and the two disagreements that matter are both silent: an analysis
// could report a marker as stripped that the rewrite keeps, or miss one the
// rewrite removes.
func ParseDirective(text string) (Directive, bool) { return parseDirective(text) }

// RemovesWhenDangling reports whether the rules remove a directive with this
// key once its target is pruned.
//
// The type policy needs this to describe behaviour changes truthfully. A
// marker naming a pruned package is only a change if the rewrite actually
// removes it, and DefaultRules keeps some of them deliberately, such as the
// external types marker that points at a package which is never relocated.
func (r DirectiveRules) RemovesWhenDangling(key string) bool {
	return r.ruleFor(key).RemoveWhenDangling
}

// parseDirective reads a directive out of a comment.
//
// A toolchain directive has no space between the slashes and its name, which is
// how the Go specification distinguishes `//go:generate` from a sentence that
// happens to start with go. A generator marker is a plus sign, optionally
// preceded by spaces, which is how upstream writes both `// +optional` and
// `//+optional`.
func parseDirective(text string) (Directive, bool) {
	body, ok := strings.CutPrefix(text, "//")
	if !ok {
		return Directive{}, false
	}
	if rest, ok := strings.CutPrefix(body, "go:"); ok {
		key, value := splitDirective(rest)
		if key == "" {
			return Directive{}, false
		}
		return Directive{Kind: GoDirective, Key: "go:" + key, Value: value, Text: text}, true
	}
	marker, ok := strings.CutPrefix(strings.TrimLeft(body, " \t"), "+")
	if !ok {
		return Directive{}, false
	}
	key, value := splitDirective(marker)
	if key == "" {
		return Directive{}, false
	}
	return Directive{Kind: MarkerDirective, Key: key, Value: value, Text: text}, true
}

// splitDirective separates a directive name from its value. A marker uses an
// equals sign, a toolchain directive uses a space, and a directive may carry
// neither.
func splitDirective(body string) (key, value string) {
	end := len(body)
	for i, r := range body {
		if r == '=' || r == ' ' || r == '\t' {
			end = i
			break
		}
	}
	key = body[:end]
	if end == len(body) {
		return key, ""
	}
	if body[end] == '=' {
		return key, body[end+1:]
	}
	return key, strings.TrimLeft(body[end:], " \t")
}

// protected reports a directive no rule may touch.
//
// Build constraints decide whether a file is compiled at all, and every other
// toolchain directive changes what the compiler or linker does with the code
// around it. `go:embed` is verified against the copied assets rather than
// rewritten, so it is protected here too. `go:generate` is the single toolchain
// directive a rule may act on, because it names an external command rather than
// changing how this file builds.
func protected(directive Directive) bool {
	if directive.Kind == GoDirective {
		return directive.Key != "go:generate"
	}
	// The legacy build constraint form is a marker rather than a toolchain
	// directive, and dropping it would compile a file on a platform upstream
	// excluded.
	return directive.Key == "build"
}

// rewriteDirectiveValue maps every source prefixed import path in a directive
// value and reports whether anything changed.
//
// Values are comma separated lists upstream, as in a deepcopy-gen interfaces
// marker naming several packages. Each element is mapped independently and an
// element that is not an eligible import path is copied through untouched, so
// an API group or a free text element in the same list cannot be corrupted.
func rewriteDirectiveValue(value string, opts Options) (string, bool) {
	elements := strings.Split(value, ",")
	changed := false
	for i, element := range elements {
		trimmed := strings.TrimSpace(element)
		destination, eligible := opts.destination(trimmed)
		if !eligible {
			continue
		}
		elements[i] = strings.Replace(element, trimmed, destination, 1)
		changed = true
	}
	if !changed {
		return value, false
	}
	return strings.Join(elements, ","), true
}
