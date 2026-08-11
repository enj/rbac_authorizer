# Soapbox

Soapbox is a Go GitHub template and generator for publishing selected packages from [`kubernetes/kubernetes`](https://github.com/kubernetes/kubernetes) as independently consumable Go modules.

A generated repository can:

1. Select one or more upstream Go packages.
2. Compute and bound their production import closure.
3. Prefer public `k8s.io/api` types when equivalence is proven.
4. Apply durable, ref-scoped patches.
5. Generate a curated public facade over relocated internal code.
6. Replay relevant upstream history with `Kubernetes-commit` provenance.
7. Publish append-only module versions aligned with Kubernetes releases.

The first target is `monis.app/kk/rbac_authorizer`, sourced from `plugin/pkg/auth/authorizer/rbac` beginning at Kubernetes `v1.36.1`.

## Status

Soapbox is under initial implementation. The durable project goal and approved design are in [`plans/`](plans/).

## Design constraints

All maintained executable logic is Go. The engine may invoke installed `git` and `go` executables through typed subprocess boundaries, but the repository contains no shell scripts or shell command composition.

Remote repository creation, vanity metadata changes, and immutable module publication require a separately reviewed outward-action manifest. Nothing is published during local implementation and verification.

## License

Apache License 2.0. See [`LICENSE`](LICENSE) and [`NOTICE`](NOTICE).
