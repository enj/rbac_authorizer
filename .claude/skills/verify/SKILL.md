---
name: verify-soapbox
summary: Drive the Soapbox CLI foundation through its real terminal surface.
---

# Verify Soapbox

Build a disposable CLI binary with writable Go caches:

```text
GOPATH=/tmp/soapbox-gopath
GOMODCACHE=/tmp/soapbox-gopath/pkg/mod
GOCACHE=/tmp/soapbox-gocache
GOTOOLCHAIN=go1.26.5
go -C tools build -o /tmp/soapbox-verify ./cmd/soapbox
```

Drive these flows from the repository root:

1. `validate -config soapbox.yaml -format summary` and `-format profile`.
2. `doctor -dir .` with writable `GOPATH`, `GOMODCACHE`, and `GOCACHE`.
3. `validate -h`, an unsupported `-format`, an unknown YAML field, a missing profile, and a credential-bearing URL. Capture output streams and exit codes.
4. Run doctor with a hostile `HOME/.gitconfig` to confirm ambient Git config isolation.
5. Run doctor under a short SIGINT timeout with a deliberately slow disposable Git executable to confirm exit code 4 and bounded cancellation.
6. Render the profile twice and compare bytes. Change only `determinism.chunkSize` and confirm profile identity remains unchanged.

## Plan

`plan` needs an upstream to read. Build a throwaway one rather than reaching the
network: a `git init` repository with two packages where one imports the other,
one file the profile prunes, an annotated `v1.36.1` tag, and
`uploadpack.allowFilter true`. The last is not optional, because the cache is a
blobless partial clone and `source.Open` refuses a cache that holds every blob.
The remote must be the `file://` URL and not the bare path, or git uses its
local transport and ignores the filter. A profile matching that shape is the
`planProfile` constant in `tools/internal/cli/plan_test.go`.

Drive these against the throwaway upstream, always passing `-source-remote` and
a `-cache` under the temporary directory:

7. A plain run and `-format json` with `-report`. Confirm the printed JSON and
   the written report are the same bytes, and that the report names no directory
   from this machine. The summary is the one rendering allowed to, and the cache
   line must name the directory the cache actually opened.
8. Run twice against two different cache roots and compare the two reports byte
   for byte. They must be identical; this is what the whole report shape exists
   for. Both roots have to be fresh: `source.cacheCreated` records whether this
   run cloned the cache, so reusing a root an earlier flow already primed flips
   that one field and reads as a determinism failure that is not one.
9. `-strict` against a profile whose `closure.golden` is absent. Expect exit 3,
   the notice on stderr, and a written `-report` whose `failure.stage` is
   `plan strict`. A refusal that produced no artifact is the regression.
10. Write the previous run's `closure.report` to the path `closure.golden`
    names. Expect `golden ... is match`. Then edit `exact.packages` in that file
    and expect exit 3 from `plan golden` with the gained and lost packages named.
    Nothing may rewrite the golden on its own.
11. `-strict -materialize` with a matching golden. Expect exit 0 and the tree.
    Then repeat with a golden mismatch and confirm `-out` was never created, so
    the refusal is immediately retryable.
12. Prime a cache with a profile whose root is one package, move the upstream
    directory aside, and run the other profile with `-offline`. Expect a failure
    naming `lazy fetching disabled` and never the missing repository: an offline
    run may not reach the promisor remote for a blob the cache lacks. Restore
    the directory afterwards.
13. For a profile carrying patches, confirm the branch: a tag run picks
    `release-1.<minor>` when the profile tracks it, the single tracked branch
    when there is only one, and exits 2 asking for `-patch-branch` when several
    are tracked and none is derived.

Never exercise repository creation, pushes, vanity changes, or tag publication during this verification. Those surfaces require the separately approved outward-action manifest.
