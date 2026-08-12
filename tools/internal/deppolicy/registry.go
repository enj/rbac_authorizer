package deppolicy

import (
	"slices"
	"strings"
)

// The registries in this file are curated tables, not heuristics over spelling.
//
// A registration is matched by resolving a selector to a real types.Object and
// comparing its package path and name, so a local function named MustRegister
// does not match and a dot import of the real one still does. The tables are
// deliberately conservative: the failures they prevent are silent at compile
// time and at run time, so the cost of one false refusal is an operator reading
// an evidence line, while the cost of one miss is a generated module whose
// feature gate, scheme, or context key quietly does nothing.
//
// Every rule carries the reason it exists. The reason is what the report prints,
// because "candidate registers into a global registry" is not actionable and
// "a copied metrics registration lands in a second registry that nothing
// scrapes" is.

// stateKind classifies a global state finding. The kinds are report-stable
// strings, so a fixture that pins them notices a reclassification.
const (
	kindContextKey  = "contextKey"
	kindScheme      = "scheme"
	kindFeatureGate = "featureGate"
	kindMetrics     = "metrics"
	kindServeMux    = "serveMux"
	kindDriver      = "driver"
	kindFlag        = "flag"
	kindLogging     = "logging"
	kindRandom      = "random"
	kindSingleton   = "mutableSingleton"
	kindInit        = "initSideEffect"
	kindDeniedPath  = "deniedPath"
)

// denyRule matches one call whose effect does not survive relocation.
type denyRule struct {
	// Pkg is the exact package path declaring the symbol. An empty Pkg matches
	// any package and is reserved for names whose meaning is unmistakable
	// across the Kubernetes tree, such as AddToScheme.
	Pkg string
	// Symbol is the function or method name.
	Symbol string
	// Kind classifies the finding.
	Kind string
	// Reason explains what breaks, in terms of the consequence rather than the
	// mechanism.
	Reason string
}

// deniedCalls is the call side of the registry.
//
// Ordering here is irrelevant to behaviour because lookup is by key, but the
// grouping is by consequence so that adding a rule means first deciding which
// silent failure it prevents.
var deniedCalls = []denyRule{
	// A context key's identity is its type. Copying the type makes a new key,
	// so a value the real package stored is not found and the read reports
	// absence, which callers treat as "no request info" rather than as an
	// error.
	{Pkg: "context", Symbol: "WithValue", Kind: kindContextKey,
		Reason: "a relocated context key type is a distinct key, so values stored by the real package read as absent instead of failing"},

	// A scheme is process global. A copied registration populates a second
	// scheme that no decoder consults, and the symptom is an unrecognised kind
	// at run time rather than a build failure.
	{Pkg: "", Symbol: "AddToScheme", Kind: kindScheme,
		Reason: "a relocated scheme registration populates a second scheme that no decoder consults"},
	{Pkg: "", Symbol: "AddKnownTypes", Kind: kindScheme,
		Reason: "a relocated type registration is invisible to the real scheme"},
	{Pkg: "", Symbol: "AddKnownTypeWithName", Kind: kindScheme,
		Reason: "a relocated type registration is invisible to the real scheme"},
	{Pkg: "", Symbol: "AddConversionFunc", Kind: kindScheme,
		Reason: "a relocated conversion is invisible to the real scheme"},
	{Pkg: "", Symbol: "AddGeneratedConversionFunc", Kind: kindScheme,
		Reason: "a relocated conversion is invisible to the real scheme"},
	{Pkg: "", Symbol: "AddFieldLabelConversionFunc", Kind: kindScheme,
		Reason: "a relocated field label conversion is invisible to the real scheme"},
	{Pkg: "", Symbol: "AddTypeDefaultingFunc", Kind: kindScheme,
		Reason: "a relocated defaulter is invisible to the real scheme"},
	{Pkg: "", Symbol: "SetVersionPriority", Kind: kindScheme,
		Reason: "a relocated version priority is invisible to the real scheme"},
	{Pkg: "k8s.io/apimachinery/pkg/runtime", Symbol: "NewScheme", Kind: kindScheme,
		Reason: "constructing a scheme in a relocated package creates state the real process does not share"},
	{Pkg: "k8s.io/apimachinery/pkg/runtime", Symbol: "NewSchemeBuilder", Kind: kindScheme,
		Reason: "a relocated scheme builder registers into a scheme the consumer never uses"},

	// A feature gate is consulted through a process global. A copied gate is a
	// second gate that no flag parsing reaches, so it silently keeps its
	// defaults while the operator believes they changed it.
	{Pkg: "k8s.io/component-base/featuregate", Symbol: "NewFeatureGate", Kind: kindFeatureGate,
		Reason: "a relocated feature gate is never wired to flag parsing and silently keeps its defaults"},
	{Pkg: "k8s.io/apiserver/pkg/util/feature", Symbol: "DefaultFeatureGate", Kind: kindFeatureGate,
		Reason: "the default feature gate is process global and a relocated reader observes a different one"},
	{Pkg: "k8s.io/apiserver/pkg/util/feature", Symbol: "DefaultMutableFeatureGate", Kind: kindFeatureGate,
		Reason: "the default mutable feature gate is process global and a relocated writer mutates a copy nobody reads"},

	// Metrics registration lands in whichever registry the package was linked
	// against. A copy registers into a second registry that nothing scrapes.
	{Pkg: "k8s.io/component-base/metrics/legacyregistry", Symbol: "MustRegister", Kind: kindMetrics,
		Reason: "a relocated metrics registration lands in a second registry that nothing scrapes"},
	{Pkg: "k8s.io/component-base/metrics/legacyregistry", Symbol: "RawMustRegister", Kind: kindMetrics,
		Reason: "a relocated metrics registration lands in a second registry that nothing scrapes"},
	{Pkg: "k8s.io/component-base/metrics/legacyregistry", Symbol: "Register", Kind: kindMetrics,
		Reason: "a relocated metrics registration lands in a second registry that nothing scrapes"},
	{Pkg: "k8s.io/component-base/metrics/legacyregistry", Symbol: "CustomMustRegister", Kind: kindMetrics,
		Reason: "a relocated metrics registration lands in a second registry that nothing scrapes"},
	{Pkg: "github.com/prometheus/client_golang/prometheus", Symbol: "MustRegister", Kind: kindMetrics,
		Reason: "a relocated metrics registration lands in a second registry that nothing scrapes"},
	{Pkg: "github.com/prometheus/client_golang/prometheus", Symbol: "Register", Kind: kindMetrics,
		Reason: "a relocated metrics registration lands in a second registry that nothing scrapes"},

	// The default mux, driver tables, and gob type table are all process
	// global tables keyed by name, so a copy either double registers and panics
	// or registers where nobody looks.
	{Pkg: "net/http", Symbol: "Handle", Kind: kindServeMux,
		Reason: "registering on the default mux from a relocated package either double registers or serves from a mux the consumer does not use"},
	{Pkg: "net/http", Symbol: "HandleFunc", Kind: kindServeMux,
		Reason: "registering on the default mux from a relocated package either double registers or serves from a mux the consumer does not use"},
	{Pkg: "database/sql", Symbol: "Register", Kind: kindDriver,
		Reason: "a driver name may be registered once per process, so a relocated copy panics when the real module is also linked"},
	{Pkg: "encoding/gob", Symbol: "Register", Kind: kindDriver,
		Reason: "gob type registration is process global and keyed by name, so a relocated copy collides with the real one"},
	{Pkg: "encoding/gob", Symbol: "RegisterName", Kind: kindDriver,
		Reason: "gob type registration is process global and keyed by name, so a relocated copy collides with the real one"},

	// Flags are parsed once from a global set. A relocated registration is
	// either a duplicate flag panic or a flag no command line reaches.
	{Pkg: "flag", Symbol: "Var", Kind: kindFlag, Reason: "a relocated flag registration duplicates or misses the command line the consumer parses"},
	{Pkg: "flag", Symbol: "StringVar", Kind: kindFlag, Reason: "a relocated flag registration duplicates or misses the command line the consumer parses"},
	{Pkg: "flag", Symbol: "BoolVar", Kind: kindFlag, Reason: "a relocated flag registration duplicates or misses the command line the consumer parses"},
	{Pkg: "flag", Symbol: "IntVar", Kind: kindFlag, Reason: "a relocated flag registration duplicates or misses the command line the consumer parses"},
	{Pkg: "flag", Symbol: "DurationVar", Kind: kindFlag, Reason: "a relocated flag registration duplicates or misses the command line the consumer parses"},
	{Pkg: "k8s.io/klog/v2", Symbol: "InitFlags", Kind: kindLogging,
		Reason: "logging is configured once per process and a relocated initialisation fights the consumer's own"},
	{Pkg: "k8s.io/klog/v2", Symbol: "SetLogger", Kind: kindLogging,
		Reason: "logging is configured once per process and a relocated initialisation fights the consumer's own"},

	// The global source is shared, so seeding it from a library changes
	// behaviour the consumer did not ask for.
	{Pkg: "math/rand", Symbol: "Seed", Kind: kindRandom,
		Reason: "seeding the global source from a relocated package changes process wide behaviour the consumer did not choose"},
	{Pkg: "math/rand/v2", Symbol: "Seed", Kind: kindRandom,
		Reason: "seeding the global source from a relocated package changes process wide behaviour the consumer did not choose"},
}

// deniedPaths are packages whose global state is a property of the package
// rather than of any one call in it.
//
// A path rule fires even when the call scan finds nothing, because these
// packages hold the state that other packages mutate. Copying the holder is
// what splits the state in two, and the split is invisible in the holder's own
// source.
var deniedPaths = []denyRule{
	{Pkg: "k8s.io/apiserver/pkg/features", Kind: kindDeniedPath,
		Reason: "the package registers the apiserver feature gates into a process global gate at initialisation"},
	{Pkg: "k8s.io/apiserver/pkg/util/feature", Kind: kindDeniedPath,
		Reason: "the package is the process global feature gate every other package reads"},
	{Pkg: "k8s.io/apiserver/pkg/endpoints/request", Kind: kindDeniedPath,
		Reason: "the package owns the request context keys, whose identity is the type and does not survive relocation"},
	{Pkg: "k8s.io/component-base/featuregate", Kind: kindDeniedPath,
		Reason: "the package defines the feature gate machinery whose instances are process global"},
	{Pkg: "k8s.io/component-base/metrics/legacyregistry", Kind: kindDeniedPath,
		Reason: "the package is the process global metrics registry"},
	{Pkg: "k8s.io/apimachinery/pkg/runtime", Kind: kindDeniedPath,
		Reason: "the package defines the scheme machinery whose instances are process global"},
	{Pkg: "k8s.io/client-go/tools/metrics", Kind: kindDeniedPath,
		Reason: "the package holds the process global client metrics hooks"},
	{Pkg: "k8s.io/client-go/transport", Kind: kindDeniedPath,
		Reason: "the package caches transports in a process global keyed by configuration"},
	{Pkg: "k8s.io/component-base/logs", Kind: kindDeniedPath,
		Reason: "the package configures process global logging"},
}

// securityCriticalSegments mark a package whose code decides who may do what.
//
// Owning a copy of such code means owning its CVE response: a fix published to
// the real module reaches a consumer through a version bump, while a fix to a
// copy reaches nobody until this engine notices, regenerates, and republishes.
// That is the cost this gate scores, and it is why the match is on path
// segments rather than on what the code appears to do.
var securityCriticalSegments = []string{
	"admission",
	"authentication",
	"authorization",
	"authenticator",
	"authorizer",
	"bootstrap",
	"cert",
	"certificates",
	"crypto",
	"encryptionconfig",
	"keyutil",
	"rbac",
	"serviceaccount",
	"tls",
	"token",
	"x509",
}

// nativeExtensions are the non-Go build inputs whose presence makes a copy a
// build system problem rather than a source problem.
//
// A generated module that carries assembly or C is no longer portable by
// inspection: it inherits per architecture files, cgo toolchain requirements,
// and a build that can fail on a platform the engine never tested.
var nativeExtensions = []string{".c", ".cc", ".cpp", ".cxx", ".h", ".hh", ".hpp", ".m", ".s", ".S", ".syso"}

// deniedCall reports the rule matching one resolved symbol, if any.
//
// A rule with an empty Pkg matches on name alone. That is the conservative
// direction: the names it covers are Kubernetes scheme mutators, and a
// candidate that defines its own AddToScheme is exactly as unsafe to relocate
// as one that calls apimachinery's.
func deniedCall(pkgPath, name string) (denyRule, bool) {
	for _, rule := range deniedCalls {
		if rule.Symbol != name {
			continue
		}
		if rule.Pkg == "" || rule.Pkg == pkgPath {
			return rule, true
		}
	}
	return denyRule{}, false
}

// deniedPath reports the rule matching one candidate package path, if any.
//
// Matching includes descendants, because a subpackage of a denied holder shares
// the holder's state as surely as the holder does.
func deniedPath(pkgPath string) (denyRule, bool) {
	for _, rule := range deniedPaths {
		if pkgPath == rule.Pkg || strings.HasPrefix(pkgPath, rule.Pkg+"/") {
			return rule, true
		}
	}
	return denyRule{}, false
}

// securityCriticalSegment returns the path segment that makes a package
// security critical, or the empty string.
//
// The match is on whole slash separated segments so that a package named
// "certificates" matches and one named "uncertain" does not.
func securityCriticalSegment(pkgPath string) string {
	var matched []string
	for _, segment := range strings.Split(pkgPath, "/") {
		if slices.Contains(securityCriticalSegments, segment) {
			matched = append(matched, segment)
		}
	}
	if len(matched) == 0 {
		return ""
	}
	slices.Sort(matched)
	return matched[0]
}

// isNativeFile reports whether a build input makes the copy carry native code.
func isNativeFile(name string) bool {
	for _, extension := range nativeExtensions {
		if strings.HasSuffix(name, extension) {
			return true
		}
	}
	return false
}
