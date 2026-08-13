# Setup

How to go from this template to a derived repository, and what each command does
along the way. Everything described here is local. No command in this guide
creates a repository, pushes a ref, or publishes a tag.

## Prerequisites

| Requirement | Why |
|---|---|
| Go `1.26.5` exactly | `determinism.toolchain` pins it. The engine refuses to run under any other toolchain because gofmt output and generated module metadata are toolchain dependent. |
| Git `2.45` or later | `GIT_NO_LAZY_FETCH` is honoured from that release, which is what keeps a blobless partial clone from silently fetching during a local probe. |
| Writable Go caches | The Kubernetes module graph does not fit in a small default cache. |

Run every engine command from `tools/`:

```text
GOPATH=/Users/mo/claude/.gocache/gopath
GOMODCACHE=/Users/mo/claude/.gocache/mod
GOCACHE=/Users/mo/claude/.gocache/build
GOLANGCI_LINT_CACHE=/Users/mo/claude/.gocache/lint
```

## The commands

```text
soapbox <command> [flags]

doctor    check the local toolchain, identity, and signing policy
validate  decode and validate the soapbox.yaml profile
plan      compute the extraction plan for one upstream ref
generate  compose the generated module for one upstream release tag
sync      plan, and with an approval publish, one upstream release
setup     transform this template checkout into one derived repository
version   print the engine version
help      print usage for soapbox or one command
```

Exit codes are part of the workflow contract and are stable:

| Code | Name | Meaning |
|---|---|---|
| 0 | `ExitOK` | Success. |
| 1 | `ExitFailure` | An unexpected runtime failure. |
| 2 | `ExitUsage` | A malformed command line. The failing command's flags are printed. |
| 3 | `ExitCheck` | The command ran and found policy violations. This is the code CI reads as "something to review". |
| 4 | `ExitCanceled` | The context ended before the command did. |

Stdout carries the machine-readable artifact and nothing else. Every diagnostic
goes to stderr, so a workflow that captures stdout gets one artifact rather than
an artifact with a log appended to it.

### doctor

```text
go run ./cmd/soapbox doctor -dir ..
```

Checks the local toolchain and, when `-dir` is a Git repository, this project's
commit policy: `gpg.format=ssh`, the configured signing key, commit and tag
signing, an allowed signers file that actually authorizes the policy key, and
that `HEAD` is signed by that key with author and committer `Monis Khan
<i@monis.app>`, exactly one `Signed-off-by: Monis Khan <mok@microsoft.com>`
trailer, and no co-author trailers. A repository with no commits passes rather
than failing, so the check is usable on a fresh checkout.

The Go version check is the one deliberate exception: a version at or above the
`1.26` floor that is not the pinned patch release is a warning, not a failure.

### validate

```text
go run ./cmd/soapbox validate -dir ..
```

Decodes `soapbox.yaml` strictly and prints a summary. `-format canonical` prints
the normalized profile; `-format profile` prints exactly the bytes that feed the
replay profile hash, which is the useful one when asking why an epoch changed.
A profile the operator can fix — an unknown field, a duplicate key, a failed
validation rule — exits 3, not 1.

### plan

```text
go run ./cmd/soapbox plan -dir .. -cache /state/src -tag v1.36.1
```

Computes one extraction: acquire the source, materialize the configured root
packages in a sparse work tree, prune, apply the selected patch series, iterate
to a closure fixed point, relocate, and rewrite. It stops before module
composition, so it needs no Go toolchain and no module proxy.

`-materialize` writes the relocated tree to `-out`. Without it the plan measures
and reports without leaving a tree behind. `-report <path>` writes the JSON
report even when the run is refused, which is what makes a refusal reviewable.

`plan` refuses to start if any of the three GitHub App environment variable
names named in the profile are set. A plan needs no credential, so holding one
is a configuration error rather than a convenience.

### generate

```text
go run ./cmd/soapbox generate -dir .. -cache /state/src -tag v1.36.1 -materialize
```

Everything `plan` does, then: resolve staging module versions, compose and
verify the root `go.mod`, generate the facade from both the pre-prune and the
post-prune tree and refuse any difference between them, run the type policy, run
the dependency policy, render provenance, and write the module.

`-cache` is required and must sit outside the profile directory. A generation
removes its scratch root and refuses to write into an existing output tree, so
defaulting either to a directory nobody named is how a run deletes something an
operator was keeping. The version index defaults to `<cache>/staging-versions.json`;
it is the one path the engine allows to live inside the cache.

### setup

```text
go run ./cmd/soapbox setup -dir .. -engine-version tools/v0.1.0
go run ./cmd/soapbox setup -dir .. -engine-version tools/v0.1.0 \
    -engine-sum /tmp/engine.sum -apply -approve <hash>
```

Transforms a template checkout into a derived repository, in place. It defaults
to a dry run and stays that way unless the operator both asks to apply and names
the manifest hash they read. See [What setup does](#what-setup-does).

### sync

```text
go run ./cmd/soapbox sync -dir .. -cache /state/src -destination /path/to/repo -local-remote
```

Everything `generate` does, then replay the source commit, project the release,
build the state record, and produce a publication manifest. Without `-apply` it
plans and nothing outward happens. The destination must already have the
setup-derived control-plane commit, either as its published consumer branch or
as local `HEAD` during a pre-push rehearsal. Sync preserves `soapbox.yaml`,
patches, the pinned `tools` shim, workflows, and other operator-owned files while
replacing the generated module paths; it refuses an empty destination rather
than publish an unmaintainable generated-only root. See
[Publication](#publication) for what is and is not possible today.

## What setup does

`soapbox setup` composes every file it owns from the profile alone, classifies
every tracked path in the repository, and reports a manifest. Applying that
manifest is a second, separately approved step.

### The payload

Exactly six paths are written, and no others:

```text
go.mod                          the derived repository's root module, no requirements yet
tools/go.mod                    the nested shim, pinning the engine and its indirect graph roots
tools/go.sum                    only when -engine-sum supplies complete verified checksums
tools/cmd/soapbox/main.go       the shim command
.github/workflows/ci.yml        read-only verification
.github/workflows/sync.yml      publishing
```

The root module carries no requirements. The first generation writes them, so
setup never guesses a dependency graph.

### What is removed

Development-only material does not reach a derived repository:

```text
.claude/            .serena/            plans/
docs/               tools/cmd/          tools/internal/
.golangci.yml       CLAUDE.md           tools/soapbox.go
tools/soapbox_test.go
.github/workflows/template-selftest.yml
```

`docs/` is on that list, which is why this guide lives in the template and not
in a generated module: a derived repository documents the module it publishes,
not the engine that built it.

### What is kept

`LICENSE`, `NOTICE`, `README.md`, `soapbox.yaml`, `doc.go`, `.gitattributes`,
`.gitignore`, `patches/`, the facade and assertions files named in the profile,
and everything under `destination.internalPrefix`. Setup writes no `README.md`,
`NOTICE`, or `LICENSE`; the first generation renders them.

Anything tracked that setup does not recognise is preserved and reported under
`ignored`. Setup never deletes a file it cannot name.

### Preconditions

`-dir` must be the root of a Git repository with a `HEAD`, a clean work tree,
and no tracked symlinks. The repository must still look like a template:
`soapbox.yaml`, `plans/implementation.md`, `tools/soapbox.go`,
`tools/internal/cli/cli.go`, and `tools/cmd/soapbox/main.go` must be tracked,
and a root `go.mod` must not be. The profile's `determinism.toolchain` must
equal the engine's own.

### The engine pin

`-engine-version` is required, spelled either `v1.2.3` or `tools/v1.2.3`. It
must be a canonical semantic version naming an immutable release. A
pseudo-version is refused, because the shim pins a published release so the
running engine can be read off the `go.mod`. Run setup from the matching
version of the template: setup reads that template's `tools/go.mod` and records
the engine's graph roots as indirect requirements of the shim. Omitting them
makes Go's pruned module graph request a `go mod tidy` before the shim can run.

`-engine-sum` is optional and takes a *file* holding the complete verified
`go.sum` content for the nested module. A module checksum cannot be computed
from a checkout. Without this input, `tools/go.sum` is not written and the
manifest records a notice telling you to run `go mod download all` inside
`tools/` once the pinned release exists. With it, every line is validated and
the file must cover both the module and `/go.mod` checksum for the pinned engine
and every graph root named by its `go.mod`. This is what lets the generated shim
run from a clean module cache without modifying `tools/go.mod` or `tools/go.sum`.

### The approval

A dry run prints a manifest containing every action — `create`, `replace`, and
`delete` — each with the digest and byte count of the content involved. A delete
records the digest of what it destroys, so an approval covers removed bytes
rather than a filename.

`-apply` requires `-approve <hash>`, and `-approve` requires `-apply`. Neither
half means anything alone. Apply recomputes the plan from the repository as it
is now and refuses if the hash differs, then re-reads and re-digests every file
it is about to delete. The setup manifest hash is bare hex, with no `sha256:`
prefix; the sync manifest hash carries one. They are different artifacts and the
spellings do not interchange.

`-report` must point outside the repository. Setup requires a clean work tree,
so a manifest written into that tree would make the very next command refuse to
run.

## The derived repository

```text
rbac_authorizer/
├── go.mod
├── go.sum
├── authorizer.go                  the curated facade
├── zz_generated_assertions.go     compile-time interface assertions
├── doc.go
├── LICENSE
├── NOTICE
├── README.md
├── internal/kk/<preserved upstream package paths>/
├── soapbox.yaml
├── patches/
├── tools/
│   ├── go.mod
│   ├── go.sum
│   └── cmd/soapbox/main.go
└── .github/workflows/
    ├── ci.yml
    └── sync.yml
```

`refs/heads/soapbox-state` holds the resumable state record without entering the
module tree. `refs/soapbox/progress/` is reserved for gated backfill chunks.

### The generated workflows

`ci.yml` runs on pushes and pull requests to the default branch with
`permissions: {}` at the top level and `contents: read` on the job. It builds,
vets, and tests the root module and the shim, then runs
`go run ./cmd/soapbox validate -dir ..`. It never sees an App secret and the
checkout keeps no token.

`sync.yml` runs on `schedule` at `37 4 * * *` and on `workflow_dispatch`, never
on pull request code, and there is no `pull_request_target` trigger. The job
refuses to run from any ref but the protected default branch, serializes on the
non-cancelling concurrency group `soapbox-sync`, and holds `contents: read` plus
`actions: read` — the workflow token cannot write. All maintained logic is one
invocation:

```text
go run ./cmd/soapbox sync -dir .. -destination .. -cache ${{ runner.temp }}/soapbox-cache
```

It carries no `-apply`. A scheduled workflow that published without an approval
would be an outward action nobody authorized, so enabling publication is a
deliberate edit made at the outward-action gate rather than a default the
template ships.

Both workflows pin their actions to full commit object names.

## Publication

Publication does not work against a network remote today. Deciding what a push
would do requires listing the destination's refs, the typed Git boundary does
not expose `ls-remote`, and only a filesystem destination implements the
listing interface. A network destination is refused with
`publication requires a configured destination remote` wrapping
`listing refs of a network remote needs a gitcli remote ref API`.

What works is a local rehearsal: `-destination` pointing at a real repository on
disk with `-local-remote` to permit it. That is the shape the Phase 7 dry run
uses.

## Current limitations

These are properties of the engine as it stands, not of the design.

1. **Release tags only.** `generate` and `sync` accept a source tag that maps
   under the release policy. A branch is refused: *"only a release tag can be
   generated from until intermediate staging resolution is wired to verified
   repository URLs"*. Commit-to-staging-version mapping is implemented and
   tested in `gomodmap` but no pipeline calls it.
2. **No staging copy materialization.** A profile proposing copies is refused
   before the policy runs, and an approved copy is refused after it. Both say
   *"materializing a copied package is not implemented"*. The RBAC profile
   copies nothing, so this bounds nothing it needs.
3. **No retained-reference type rewrite.** `prefer-external` proves both
   dead-package pruning and actual substitution. Generation applies the first
   and refuses the second as unsupported until the enumerated reference edits
   are applied to the generated bytes. RBAC follows the dead-package path.
4. **No backfill.** `sync` publishes one release. Resuming from a state record
   that names an earlier release is refused: the commits between them would have
   to be replayed. Progress refs are defined, validated, and never emitted;
   `determinism.chunkSize` is validated and unused.
5. **No epoch graft.** A state record written under a different profile hash is
   refused rather than grafted onto a new epoch.
6. **One upstream commit per replay.** The pipeline attaches the transformed
   release commit to the setup-derived control-plane commit. Multi-commit
   traversal, anchor bounding, merge shaping, parent dedup, and unchanged-tree
   collapse are implemented and tested but no pipeline drives them yet.
7. **The state record omits the release tag.** It records the consumer branch
   only, because the record refuses two destination objects claiming one source
   commit.
8. **No vanity page generation.** See [vanity.md](vanity.md).
9. **No repository creation.** The GitHub API client can read a repository,
   list installation repositories, read a workflow, and manage issues. It cannot
   create a repository.

## Where to look next

| Question | Document |
|---|---|
| What every profile field means | [config-reference.md](config-reference.md) |
| How source history becomes destination history | [replay-model.md](replay-model.md) |
| Why two runs produce identical bytes | [determinism.md](determinism.md) |
| What the generated module records about its origin | [provenance.md](provenance.md) |
| What the generated module does differently from upstream | [behavior-changes.md](behavior-changes.md) |
| When a staging package may be copied | [dependency-policy.md](dependency-policy.md) |
| How the publishing identity is set up | [github-app.md](github-app.md) |
| How `monis.app/kk/...` resolves | [vanity.md](vanity.md) |
| What to do when a run is refused | [conflict-runbook.md](conflict-runbook.md) |
