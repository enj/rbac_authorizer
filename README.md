# Soapbox

Soapbox is a Go GitHub template and deterministic generator for publishing selected packages from [`kubernetes/kubernetes`](https://github.com/kubernetes/kubernetes) as independently consumable Go modules.

A generated repository can:

1. compute and bound the production package closure;
2. apply exact pruning and ref-scoped patches;
3. prefer public `k8s.io/api` types only after equivalence proofs;
4. relocate retained source beneath `internal/kk` while preserving paths;
5. generate a curated, type-checked public facade and provenance files;
6. map Kubernetes staging modules to exact published versions;
7. replay relevant upstream DAG history with `Kubernetes-commit` trailers; and
8. plan append-only, compare-and-swap publication behind an approval hash.

The first target is `monis.app/kk/rbac_authorizer`, sourced from `plugin/pkg/auth/authorizer/rbac` at Kubernetes `v1.36.1` and mapped to module tag `v0.36.1`.

## Status

The local engine, release projection, replay, state, GitHub App client, setup transformation, and append-only publication planner are implemented. The remaining delivery gate is the documented real Kubernetes RBAC dry run and review of its exact outward-action manifest. No remote repository, vanity page, branch, or module tag is created by the development workflow.

The durable requirements and approved design are in [`plans/`](plans/).

Known limitations of the engine as it stands are listed in
[docs/setup.md](docs/setup.md#current-limitations). In short: exact release tags
only, no branch generation, no backfill of prior releases, no epoch graft, no
staging-package materialization, no vanity page generation, and publication
rehearsed against a local destination because listing a network remote's refs is
not yet implemented.

## Architecture

`enj/soapbox` is both a GitHub template and the source of a versioned Go engine.

1. The template has no root `go.mod`. The engine is the nested module
   `github.com/enj/soapbox/tools`.
2. `tools/soapbox.go` is the entire public surface. Everything else lives under
   `tools/internal/`, so the engine can evolve without changing the contract a
   derived repository compiles against.
3. `soapbox setup` creates the derived repository's root module, replaces the
   copied engine with a small nested `tools` module and command shim, and pins
   that shim to an immutable `tools/vX.Y.Z` release. Tool dependencies never
   enter the generated library's module graph.
4. Generated source, `soapbox.yaml`, patches, the shim, and workflows coexist on
   the default branch. Replay commits modify only generated paths. Configuration
   or engine changes form explicit profile epochs and never rewrite published
   history.

```text
soapbox/
├── soapbox.yaml            the extraction profile
├── plans/                  the durable goal and approved design
├── docs/                   this documentation
├── patches/                ordered unified diffs
├── .github/workflows/      ci.yml, template-selftest.yml
└── tools/                  the engine module
    ├── soapbox.go          the public entry point
    ├── cmd/soapbox/        the command
    └── internal/           config, gitcli, source, closure, patchset,
                            relocate, rewrite, gomodmap, modgen, deppolicy,
                            typeswap, facade, provenance, treebuild, replay,
                            release, publish, state, ghapp, ghapi, extract,
                            generate, sync, setup, doctor, cli
```

A derived repository keeps its facade, assertions, `internal/kk/<upstream
paths>/`, `soapbox.yaml`, `patches/`, the nested shim, and two generated
workflows. `refs/heads/soapbox-state` carries resumable state without entering
the module tree.

## Commands

Run the nested engine from `tools/`:

```text
go run ./cmd/soapbox validate -dir ..
go run ./cmd/soapbox doctor -dir ..
go run ./cmd/soapbox plan -dir .. -tag v1.36.1
go run ./cmd/soapbox generate -dir .. -cache /absolute/cache -tag v1.36.1
go run ./cmd/soapbox setup -dir .. -engine-version v0.1.0
go run ./cmd/soapbox sync ...
```

`plan`, `generate`, `setup`, and `sync` are dry-run oriented. Operations that write an approved local transformation or move refs require the exact manifest hash produced by the corresponding plan. Generation currently supports exact release tags; intermediate-commit staging resolution and staging-package materialization remain fail-closed.

Exit codes are part of the workflow contract and are stable: `0` success, `1`
runtime failure, `2` usage, `3` the command ran and found policy violations, and
`4` canceled. Stdout carries the machine-readable artifact and nothing else;
every diagnostic goes to stderr. A refused run still writes its report before
reporting the failure, so a finding is always reviewable. Full flag
documentation is in [docs/setup.md](docs/setup.md#the-commands).

## The first profile

`soapbox.yaml` extracts the Kubernetes RBAC authorizer:

1. source package `plugin/pkg/auth/authorizer/rbac` at `v1.36.1`, at package
   granularity, which excludes the sibling package `bootstrappolicy`;
2. eight exact prune files that reduce a four-package, 3,289-line internal
   closure to three packages and about 978 lines, and drop
   `k8s.io/kubernetes/pkg/apis/rbac` entirely;
3. one denied import, the exact unversioned `pkg/apis/rbac`, leaving its
   retained `/v1` helper subpackage in place;
4. type policy `prefer-external`, which for RBAC prunes rather than rewrites,
   because the retained code already uses `k8s.io/api/rbac/v1`;
5. dependency policy `external` with an empty copy list, recorded in
   [docs/decisions/0001-no-staging-copy-rbac.md](docs/decisions/0001-no-staging-copy-rbac.md);
6. 16 facade exports, 4 renaming aliases, and 2 compile-time assertions against
   the real `k8s.io/apiserver` authorizer interfaces.

Pruning the registration, conversion, and defaulting files stops import-time
mutation of the `k8s.io/api/rbac/v1` scheme builder. That is an intentional
behaviour change, and it is recorded as one in
[docs/behavior-changes.md](docs/behavior-changes.md).

## Documentation

- [Setup and derived repositories](docs/setup.md)
- [Configuration reference](docs/config-reference.md)
- [Replay and profile epochs](docs/replay-model.md)
- [Determinism model](docs/determinism.md)
- [Provenance and licence evidence](docs/provenance.md)
- [Intentional RBAC behavior changes](docs/behavior-changes.md)
- [Dependency copy policy](docs/dependency-policy.md)
- [GitHub App setup](docs/github-app.md)
- [Vanity import bootstrap](docs/vanity.md)
- [Conflict and recovery runbook](docs/conflict-runbook.md)
- [Why RBAC copies no staging dependency](docs/decisions/0001-no-staging-copy-rbac.md)

## Development checks

From `tools/`, use the writable caches documented in [`CLAUDE.md`](CLAUDE.md), then run:

```text
gofmt and goimports with no diff
go vet ./...
go test ./...
go test -race ./...
go build ./...
golangci-lint run
```

[`.github/workflows/ci.yml`](.github/workflows/ci.yml) runs exactly these.
[`.github/workflows/template-selftest.yml`](.github/workflows/template-selftest.yml)
additionally plans the template transformation against the real checkout and
exercises setup, generation, and publication against real temporary Git
repositories. Neither workflow holds a credential, neither can write, and both
pin every action to a full commit object name.

## Hard boundaries

All maintained executable logic is Go. The engine invokes installed `git` and `go` executables only through typed subprocess boundaries; it contains no shell command construction.

GitHub credentials are short-lived App installation tokens, never PATs, and never appear in command arguments, remote URLs, reports, or artifacts. The App is never installed on `kubernetes/kubernetes`.

Remote creation, vanity metadata changes, and immutable module publication require a separately reviewed outward-action manifest and a fresh approval of its hash.

## License

Apache License 2.0. See [`LICENSE`](LICENSE) and [`NOTICE`](NOTICE).
