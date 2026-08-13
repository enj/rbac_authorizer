# RBAC v1.36.1 local proof

This is the completed local-only proof for the first Soapbox target. It was run
on 2026-08-13. It created no remote repository, changed no vanity page, pushed no
network ref, and published no public module tag. Network access was limited to
anonymous dependency downloads for clean-cache verification. Every ref described
below lives in a local bare repository used only for the rehearsal.

## Immutable inputs

| Input | Value |
|---|---|
| Upstream release | `kubernetes/kubernetes` `v1.36.1` |
| Upstream tag object | `5b824a493a7ca248b726b6ea09d53842b9b992c2` |
| Upstream commit | `756939600b9a7180fc2df6550a4585b638875e67` |
| Destination release | `v0.36.1` |
| Engine release | `v0.1.0` (`tools/v0.1.0`) |
| Profile hash | `sha256:baaa140c5a917ce1aedcc8a029f6514666dd0f9ecc3bce96fae3dde8324e1aa4` |
| Toolchain | `go1.26.5` |

`soapbox validate` passed, `soapbox doctor` passed all 22 checks in the template
checkout, and strict offline planning matched
`testdata/closure/rbac-v1.36.1.json`.

## Closure and pruning

The exact production closure was computed from package imports with tests
excluded.

| Measurement | Before pruning | After pruning | Removed |
|---|---:|---:|---:|
| Packages | 4 | 3 | 1 |
| Go/source files | 19 | 6 | 13 |
| Non-test lines | 3,289 | 978 | 2,311 |

The post-prune closure contains:

- `k8s.io/kubernetes/pkg/apis/rbac/v1`;
- `k8s.io/kubernetes/pkg/registry/rbac/validation`; and
- `k8s.io/kubernetes/plugin/pkg/auth/authorizer/rbac`.

All eight exact prune entries matched. The exact denied unversioned import
`k8s.io/kubernetes/pkg/apis/rbac` was absent after pruning. The sibling
`bootstrappolicy` package stayed outside the closure. Type analysis ran in a
scratch module containing only the production closure, so a test-only import in
upstream `rbac_test.go` could not widen or break the proof.

## Public API type decision

The configured pair is
`k8s.io/kubernetes/pkg/apis/rbac` to `k8s.io/api/rbac/v1`. The action was
`prune-internal`, with zero blockers and zero retained references to rewrite.

This is a reachability proof, not a false claim that the declarations are
identical. Kubernetes' unversioned internal RBAC declarations intentionally omit
public JSON/protobuf tags and carry helpers such as `CompactString` that the
public declarations do not. Structural conversion, method-set, and field
identity proofs remain mandatory when a retained reference is actually
rewritten; generation currently refuses that rewrite path until it can apply the
enumerated edits. For RBAC, three retained packages already import the public
package directly, generator markers confirm the intended relationship, all
import-time effects were inventoried, and the pre/post facade manifests were
identical.

The generated facade has 20 entries, including the four configured renamed
aliases. Both assertions against the real `k8s.io/apiserver` `Authorizer` and
`RuleResolver` interfaces compiled. Eight documented behavior changes reached
root provenance. No staging package was copied.

## Deterministic generation

Strict generation ran twice under different work and output roots.

| Artifact | Value |
|---|---|
| Generated files | 17 |
| Generated packages | 4 |
| Staging versions pinned from the index | 31 |
| Module requirements kept / dropped / reclassified | 41 / 161 / 24 |
| Generated `go.sum` lines | 133 |
| Compiled non-standard packages | 198 |
| Compiled modules | 42 |
| Generated manifest | `sha256:11f45f0cd1208cdbbc12e4a4de19057816c846bb7342a99a409d826afc6ffc8b` |
| JSON report SHA-256 | `5e9bfd6d97accde84ca0d4c8b2a86349dc36d58e0006657c497b94526c97f624` |
| Generated-only Git tree | `e102c10183c45a326b97afcf6b5c5eb1103a5a34` |

The reports were byte-identical, every generated file was byte-identical, and
the independently written Git trees had the same object name.

The generated root module passed:

- `gofmt` with no diff;
- `go list -deps ./...`;
- `go vet ./...`;
- `go test ./...`;
- `go test -race ./...`; and
- `go build ./...`.

## Derived repository rehearsal

`soapbox setup` was planned, approved by its exact local manifest hash, and
applied to a fresh clone. The setup manifest was
`6a0911ccffd57c4d1c9c44d0a8012e7b42ba85e1d5f8346bc6ab3694833aed4a`.
The resulting control-plane commit was signed locally as
`8fdd311b5f8706a25594161a226e75f2f4577c32`.

The nested shim pins `github.com/enj/soapbox/tools v0.1.0`, records the engine's
indirect module graph, and carries complete verified checksums. From an empty
module and build cache it downloaded the local rehearsal engine plus its graph,
ran `soapbox validate`, and left both `tools/go.mod` and `tools/go.sum` byte
unchanged. After the repository-local signing policy was configured, its
`soapbox doctor` run passed all 22 checks.

## Replay, release, state, and publication rehearsal

The final local synchronization ran through that generated shim. It preserved
the setup-derived commit as the parent, retained the profile, workflows, shim,
checksums, and other control-plane files, and replaced only generated module
paths.

| Object | Value |
|---|---|
| Setup/control-plane parent | `8fdd311b5f8706a25594161a226e75f2f4577c32` |
| Complete destination tree | `22dab14657b1007ea0d8eaa4f0084eea075f7920` |
| Unsigned replay commit | `47b8d41927fd38b26b67dc378ef67f6720dfa617` |
| Annotated `v0.36.1` tag object | `67022081dc86f809f0b188dedbe956bff4cdd64b` |
| First state commit | `5560b94d42210ca9d2940ee82e2dbc8793d46f09` |
| Reconciled state commit | `15b55e7bb59c33a00bc1283a28067631f761c6ff` |
| Reconciled state digest | `sha256:aba7a57173338555e24f1decafc95332ee1a90470ff52a205b1a5829831c1002` |
| First synchronization approval hash | `sha256:92187119cfab7632472f3f00143be8c1fa4b91956e1b262d87d7850b57c6c8ef` |

The replay commit preserves the upstream author and raw author date, uses
`soapbox[bot]` as committer with the upstream raw committer date, is unsigned,
and carries exactly one
`Kubernetes-commit: 756939600b9a7180fc2df6550a4585b638875e67`
trailer. The annotated tag points at that commit and records the exact source
tag, commit, and release URL.

The local publisher first created `refs/heads/soapbox-state`, then atomically
created `refs/heads/main` and `refs/tags/v0.36.1` under compare-and-swap leases.
A second pass advanced only the state record to describe the consumer branch it
had observed. A third pass produced no-op actions for the branch, state ref, and
tag, proving the synchronization reached a fixed point.

A clean clone of the local published branch contained both the generated module
and the setup control plane. Its root module passed format, vet, test, race, and
build checks; its nested shim built, tested, and validated the profile without
changing module metadata.

## Engine verification and remaining gate

The complete engine passed serial `go test ./...`, `go test -race ./...`,
`go vet ./...`, and `go build ./...` with isolated writable caches. The project
verification flow also exercised CLI exit codes and stream separation, hostile
Git configuration isolation, bounded SIGINT cancellation, stable profile bytes,
deterministic plan reports, strict/golden/materialization gates, offline
promisor refusal, and patch-branch selection against a disposable local
upstream. The container does not have `goimports` or `golangci-lint` installed,
so those two checks are explicitly unverified here rather than reported as
passing.

This proof authorizes no outward action. Repository creation, template enablement,
GitHub App installation/scope, vanity metadata, network pushes, engine tags, and
the public `v0.36.1` module tag remain behind a separate manifest and fresh user
approval.
