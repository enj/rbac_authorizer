# Dependency policy

Kubernetes staging modules can be depended on normally. The question this policy
answers is whether any of their packages should be *copied* into the generated
module instead.

The default answer is no. A large module is not by itself a reason to copy code.

## Copying is refused today

Two refusals stand between a profile and a copied package, and both are
unconditional:

```text
the profile proposes N staging package copies, and materializing a copied
package is not implemented

the decision approves N staging copies, and materializing a copied package is
not implemented
```

The first fires on the profile before the policy runs; the second fires after
the policy runs, if the decision approved anything. The policy itself is fully
implemented and is what the rest of this document describes. It decides; it does
not yet materialize.

## When a copy is allowed

Copying is allowed only for pure leaf utilities with high measured leverage, and
only when every correctness gate passes.

### Correctness gates

These are never overridable. An override naming one is a profile validation
error.

| Gate | What it refuses |
|---|---|
| `interoperability` | A candidate that owns a defined type, interface method type, or function type crossing the generated public boundary. Go type identity is nominal, so a relocated declaration is a different type from the one a consumer already holds. The walk descends through fields, method sets, tuples, maps, channels, slices, arrays, signatures, interfaces, and type parameters; a walk that exhausts its depth bound counts as a finding rather than as a pass. |
| `globalState` | A candidate containing unexported context keys, mutable exported singletons, feature gates, scheme mutations, registry registration, or relevant `init()` side effects. |
| `diamond` | A candidate that would appear both relocated and externally reachable in the same consumer build, or whose package owns a type the generated module must satisfy. |
| `closureCompleteness` | A candidate importing a package of its own module that is not itself a candidate. |

The three booleans in `dependencies.gates` are assertions, not switches. The
gates run whatever those values are; a profile proposing a copy with any of them
`false` is rejected outright.

### Cost gates

These are overridable. Six are ceilings and one is a floor with three
components.

| Gate | Sense | Bound |
|---|---|---|
| `maxCopiedPackages` | ceiling | packages copied |
| `maxCopiedLines` | ceiling | lines copied |
| `maxGeneratedFiles` | ceiling | generated files copied |
| `maxDistinctLicenses` | ceiling | distinct licences taken on |
| `maxModuleZipBytes` | ceiling | module zip bytes avoided |
| `maxReleasesPerMinor` | ceiling | upstream release cadence of the copied code |
| `securityCritical` | ceiling, implicit 0 | candidates on a security-critical path |
| `nativeCode` | ceiling, implicit 0 | cgo and native source files |
| `minimumLeverage` | floor | `minModulesRemoved`, `minPackagesRemoved`, `minLinesRemoved`, all three of which must hold |

Two properties matter more than the individual numbers.

**Cost is measured across the whole accepted copy**, not per candidate. Sizes
accumulate; cadence and removal benefits take the maximum. The aggregate is then
reported against every candidate, so a refusal names the whole proposal rather
than an arbitrary member of it.

**Unmeasured is not zero.** A gate the caller supplied no measurement for is
refused rather than scored as zero: *"the caller supplied no measurement for
this gate, so it is refused rather than scored as zero"*. This applies to
licences, zip bytes, and cadence.

One failed gate of any kind means the dependency stays external. There is no
weighing.

### Overrides

An override relaxes exactly one cost gate for one candidate, and it must carry a
justification, an approver, and a Kubernetes minor expiry. It sets the gate to
passing outright rather than raising the ceiling to a new number, and it is
skipped entirely when the gate is unmeasured.

An expired override fails the run. It does not quietly revert to the unrelaxed
gate:

```text
override <package> gate <gate> approved by <approver> was good through v1.N,
source is v1.M: cost gate override expired
```

An override naming a candidate the resolved graph does not contain also fails,
so an override cannot outlive the thing it was written for.

### What a copy carries

When copying is approved, all files keep their complete upstream relative path
below the internal prefix, including `staging/src/k8s.io/<module>/...`, which
preserves nested Go `internal` restrictions. Provenance records the original
module path, version, source SHA, licence, patent files, and the override that
admitted it.

## The RBAC decision

No staging package is copied. The decision is recorded in
[decisions/0001-no-staging-copy-rbac.md](decisions/0001-no-staging-copy-rbac.md)
and a policy fixture hard-fails the proposal, with overrides applied to every
relaxable gate, so the refusal cannot be weakened by tuning numbers.

## Public API type preference

A separate policy, but the same posture: substitution is a proof obligation, not
a textual rewrite. `types.policy: prefer-external` first decides whether any
retained reference actually needs substitution, then runs the proofs applicable
to that outcome.

| Analysis | What it proves |
|---|---|
| `markers` | Upstream itself records the pairing: `+k8s:conversion-gen=<internal>` and `+k8s:conversion-gen-external-types=<external>` in the same file of the same package. A shared `groupName` corroborates; differing group names block. |
| `reachability` | Either retained package-scope references are enumerated for rewriting, or the internal package is absent from the retained closure while retained code already imports the configured external package. A retained blank import blocks because it depends on import-time effects a type rewrite cannot preserve. This prevents an empty use set from becoming a vacuous pass. |
| `conversions` | For a real rewrite, generated `Convert_X_To_Y` bodies are mechanical — assignment, cast, `unsafe.Pointer` reinterpretation, a nested conversion, or the error check around one — and every field of the output type is assigned. Finding zero conversions is a blocker, not a pass. |
| `methodSets` | For a real rewrite, every exported method of each paired internal type exists on the external type with the same signature, and every internal symbol retained code names exists externally. Extra external methods are compatible growth, not a blocker. |
| `fieldIdentity` | For a real rewrite, recursive structural equality covers field names, field order, embeddedness, exportedness, `json` and `protobuf` tags, container kinds, array lengths, channel direction, signature arity and variadicity, and interface method sets. Zero comparisons is a vacuous pass and is treated as a blocker. |
| `globalEffects` | Every import-time effect of the internal package is inventoried. A reachable effect blocks; an unreachable one becomes a documented behaviour change. |

Any applicable blocker refuses the change. So does any difference in the
generated public API.

When reachability proves that retained code names no internal symbol, the outcome
is `prune-internal`: no Go value changes type, so conversion bodies, method sets,
and field or serialization identity are explicitly reported as inapplicable
rather than falsely reported as equal. That is the RBAC outcome. Retained code
already uses `k8s.io/api/rbac/v1`; the unversioned internal declarations omit
public wire tags and carry helpers the public types do not, but none of those
declarations is substituted.

## Module composition

The dependency policy runs against a real module graph, which means a
provisional module has to exist first.

1. The source commit's root `go.mod` is parsed. A module is a staging module
   exactly when the root replaces it with a directory under `staging/src`. Every
   replacement must be a staging replacement, a staging module that is also
   required must sit at the `v0.0.0` placeholder, and `exclude` directives are
   refused outright. A staged-but-unrequired module is normal.
2. At a Kubernetes tag `v1.X.Y[-pre]`, staging modules pin to `v0.X.Y[-pre]`.
   The arithmetic result is still put to the Go toolchain, and three things are
   checked: the resolved version equals the computed tag exactly, it was reached
   through a `refs/tags/` ref rather than a branch, and the module was not
   answered as the main module, through a replacement, or at a non-canonical
   version.
3. Between tags, the source commit maps to each staging repository through
   `Kubernetes-commit` trailers and the Go toolchain resolves the mapped commit
   to a pseudo-version. No pseudo-version is ever constructed by hand. This path
   is implemented and tested but no pipeline calls it, because branch refs are
   refused earlier.
4. Mappings are cached in a version index keyed by source commit. Saving merges
   rather than overwrites, and refuses a merge where a stored entry and a new one
   disagree about the same source commit.
5. The generated `go.mod` is verified by tidying it in a scratch directory: no
   requirement may be raised by minimal version selection, none may be added,
   the module path and the `go`, `toolchain`, and `godebug` directives must be
   unchanged, and there must be zero `replace` and zero `exclude` directives.
   Modules that dropped out and direct/indirect reclassifications are reported
   rather than refused, because the generated module is a subset of Kubernetes.
6. After the facade is installed, a second tidy runs in diff mode and refuses
   any change at all: *"the generated facade needs module requirements the
   tidied go.mod does not state, so the published metadata would not describe
   the published tree"*.
