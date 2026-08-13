# Determinism

Two runs of the same profile over the same source commit must produce
byte-identical trees, commits, branches, and annotated tags. Different temporary
paths, a different machine, and a different iteration order must all make no
difference. This document lists what enforces that.

Determinism is not an aesthetic here. Published module tags are immutable, so a
generation that is only usually reproducible ships a tree nobody can reconstruct
from the profile that claims to describe it.

## The pinned toolchain

`determinism.toolchain` is an exact patch pin, `go1.26.5`. The engine compares
it against the running toolchain and refuses to proceed if they differ, because
gofmt output and generated module metadata are toolchain dependent. A `devel`
toolchain gets its own message rather than a confusing mismatch.

The pin appears in five places and a test keeps them in agreement: `tools/go.mod`
`toolchain`, `soapbox.yaml` `determinism.toolchain`, the shared build constant,
the doctor policy, and the version the engine reports. The generated `sync.yml`
installs the version derived from the same pin.

The pin feeds the profile hash. Changing it starts a new epoch rather than
regenerating older tags under a different formatter.

## No clocks

Nothing on a write path reads the current time.

| Object | Author date | Committer date |
|---|---|---|
| Replayed commit | upstream author date | upstream committer date |
| Release projection commit | supplied bot date, or the upstream tagger's date | same |
| State commit | supplied signature | same |
| Annotated release tag | — | upstream tagger's date |

Every date reaching an object body is validated as a raw Git date by a single
authority, and a friendly or missing date is refused rather than defaulted.

## No machine-specific values

Reports and provenance carry no absolute path, no environment value, and no
secret. The generated artifacts name upstream repositories, commits, packages,
and files, all of which are properties of the source rather than of the machine.
Behavior-change records deliberately exclude the file and line they were
detected at, because that position names a checkout the published record must
not depend on.

Every configured URL is checked for user information, and a URL carrying
credentials is a validation failure rather than something to redact later.

## Deterministic ordering

Everything that could depend on map iteration order is sorted at the point it is
built, and several places re-assert the order rather than trusting it:

- Git tree entries, and the read-back comparison that verifies them.
- The file manifest of a written tree, sorted by path, so the record describes
  the tree rather than the sequence of calls that produced it.
- Publication actions, by ref.
- State record lists, sorted when constructed and required to be sorted when
  validated.
- Provenance packages, modified files, pruned paths, and behavior changes, all
  sorted and deduplicated.
- Facade entries, already in canonical order by public name, with collisions
  refused rather than resolved.

The one deliberate exception is the applied patch list. A series' application
order is part of what it means, so patches are deduplicated without being
sorted; a sorted list would describe a run that never happened.

## Isolated subprocesses

Git and the Go toolchain are the only subprocesses, and both run in a
constructed environment rather than an inherited one.

Git inherits `PATH`, `HOME`, and `SYSTEMROOT` and nothing else, then has
`GIT_CONFIG_NOSYSTEM=1`, `GIT_CONFIG_SYSTEM=/dev/null`,
`GIT_CONFIG_GLOBAL=/dev/null`, `GIT_TERMINAL_PROMPT=0`, `GIT_ASKPASS=`,
`LC_ALL=C`, and `LANG=C` applied on top, plus `-c core.hooksPath=/dev/null` on
every command. `GIT_NO_LAZY_FETCH` is pinned so a blobless partial clone cannot
turn a local presence probe into a network fetch, which is also why Git 2.45 is
the floor.

The Go toolchain gets the same three inherited variables, an allowlist of
location variables the operator may redirect (`GOCACHE`, `GOMODCACHE`, `GOPATH`,
`GOTMPDIR`, `TMPDIR`, `HOME`, and the XDG pair), and then a fixed block appended
last so nothing can override it: `GOENV=off`, `GOWORK=off`, `GOFLAGS=`,
`GOTOOLCHAIN=local`, `GOVCS=*:off`, `GOAUTH=off`, `NETRC=/dev/null`, empty
`GOPRIVATE`, `GONOPROXY`, `GONOSUMDB`, and `GOINSECURE`, and the same Git
isolation variables. The proxy is a single explicit value —
`https://proxy.golang.org`, deliberately without `,direct`, because `direct`
would fall back to VCS.

The isolation channel can move where the toolchain writes. It cannot change what
the toolchain trusts or where it fetches from.

Every Git index operation runs against a fresh index file in a temporary
directory, so no user index is read or written.

## Objects are verified, not assumed

Blob names are computed locally first, by hashing Git's own object framing, and
compared against what the repository reports after the write. An object that
already exists must be a blob of the matching size. A written tree is read back
and compared entry by entry. A commit is written with `--no-gpg-sign` and full
object names for its tree and parents. A tag is peeled after writing and its
target compared.

Messages and tag bodies travel on stdin rather than in an argument vector, and
identities travel in the environment. Nothing about an object body is exposed to
argument quoting.

## Formatting

Generated Go files are checked against the pinned `gofmt`. A file the pinned
formatter would reformat produces an advisory notice, which `-strict` turns into
a refusal.

Generated Kubernetes source is exempt from `goimports` regrouping. Import
position preservation is intentional: imports are replaced at their literal
positions in the original byte stream, which is what keeps aliases, comments,
cgo preambles, and build constraints where upstream put them. Hand-maintained
engine source uses `goimports`.

## Verified twice, not asserted once

The recurring pattern across the engine is that a claim is checked against the
artifact rather than trusted:

- A rewritten file is reparsed, its non-import syntax shape compared, and its
  comment attachment checked, so a rewrite that changed something it did not
  report fails.
- A generated module's `go.mod` is tidied in a scratch directory and compared:
  no requirement may be raised by minimal version selection, none may be added,
  and there must be zero `replace` and zero `exclude` directives.
- The facade is generated from both the pre-prune and the post-prune tree and
  any difference at all refuses the run.
- A state document must re-render byte-identically to the bytes it was decoded
  from.
- The tree written for a release is read back before it is tagged.

## What is not yet pinned

The staging version index caches source-commit-to-dependency-version mappings
and is merged rather than overwritten, refusing a merge where a stored entry and
a new one disagree about the same source commit. Resolution itself asks the Go
toolchain for the version and then checks the answer: the resolved version must
equal the computed tag exactly, and it must have been reached through a
`refs/tags/` ref rather than a branch. No pseudo-version is ever constructed by
hand.

The double-generation check described in the plan — running a complete replay
twice from different temporary paths and comparing every object — is part of the
Phase 7 dry run and is not yet a checked-in test.
