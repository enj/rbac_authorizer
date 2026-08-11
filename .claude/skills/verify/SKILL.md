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

Never exercise repository creation, pushes, vanity changes, or tag publication during this verification. Those surfaces require the separately approved outward-action manifest.
