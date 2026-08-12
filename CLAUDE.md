# Soapbox Go project

## Project metadata

1. Slug: `soapbox`.
2. Graphiti group: `go-soapbox`.
3. Engine module: `github.com/enj/soapbox/tools` under `tools/`.
4. Generated repositories receive a separate root module during setup.
5. Go toolchain: pin the exact supported patch release in `tools/go.mod`.

## Purpose

Soapbox extracts configured packages from `kubernetes/kubernetes`, transforms them into independently consumable Go modules, preserves relevant upstream history and provenance, and publishes append-only releases.

The approved design is [`plans/implementation.md`](plans/implementation.md). The original requirements are [`plans/goal.md`](plans/goal.md).

## Routing and code conventions

1. Use Serena for symbol navigation and edits.
2. Load the `go-language-defaults` skill for Go work.
3. Keep `tools/cmd/soapbox/main.go` thin.
4. Put implementation in noun-named packages under `tools/internal/`.
5. Expose only the narrow engine entry point required by derived repositories.
6. Every I/O function takes `context.Context` first.
7. Return errors and wrap them with `%w` and noun-phrase context.
8. Use table-driven standard-library tests and real temporary Git repositories. Do not mock subprocesses, file I/O, or pure functions.
9. All maintained executable logic is Go. Never add shell scripts or construct commands through `sh`, `bash`, or `-c`.
10. Generated Kubernetes source preserves upstream import grouping and is checked with `gofmt`, not rewritten by `goimports`.

## Build and test commands

The default GOPATH is read-only, and `/Users/mo/.cache` is a 128 MB tmpfs that is too small for the Kubernetes module graph. Use the roomy workspace cache:

```text
GOPATH=/Users/mo/claude/.gocache/gopath
GOMODCACHE=/Users/mo/claude/.gocache/mod
GOCACHE=/Users/mo/claude/.gocache/build
GOLANGCI_LINT_CACHE=/Users/mo/claude/.gocache/lint
```

Run from `tools/`:

```text
gofmt and goimports with no diff
go vet ./...
go test ./...
go test -race ./...
go build ./...
golangci-lint run
```

These Go checks replace the global Python completion commands for this project.

## Commit policy

Every commit in `enj/soapbox` must:

1. Be SSH signed using `/Users/mo/.config/wunderkind/ssh/id_ed25519.pub`.
2. Have author and committer `Monis Khan <i@monis.app>`.
3. Contain exactly one literal trailer: `Signed-off-by: Monis Khan <mok@microsoft.com>`.
4. Never use `git commit -s`, because it derives the wrong signoff email.
5. Never add Claude attribution or co-author trailers.

Verify every commit mechanically before continuing.

## Publication boundary

Do not create remote repositories, modify `enj/enj.github.io`, or publish any module tag until the local dry run has produced the exact outward-action manifest and the user has explicitly approved that manifest.
