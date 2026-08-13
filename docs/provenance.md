# Provenance

A generated module is a redistribution of somebody else's code under somebody
else's licence. The provenance artifacts are how it says so: where each file
came from, what was changed, what was removed, and under what terms.

Nothing in this layer reads a clock, names a directory on the machine that ran
the generation, or accepts a credential.

## The artifacts

| Artifact | Scope | Content |
|---|---|---|
| `LICENSE` | root | The upstream licence text, copied byte for byte. It is the only root file that is not rendered. |
| `NOTICE` | root | Origin, modifications, behaviour changes, staging mapping, copied packages, trademarks, and the embedded upstream notices. |
| `README.md` | root | The module summary and its origin. |
| `doc.go` | root | The package doc comment, opening `Package <rootPackage> provides <summary>`. |
| `SOAPBOX_PROVENANCE.txt` | every relocated package | Per-file upstream path and what happened to it. |
| Per-file notice | every modified file | A comment block above the package clause. |
| `Kubernetes-commit` trailer | every replayed commit | The source commit the tree came from. |
| `Source-tag` / `Source-commit` / `Source-release` | every release tag | The upstream release the tag projects. |

The per-package file name is deliberately loud and deliberately not a Go file.

## NOTICE

Sections are rendered in a fixed order, each titled and underlined with a dash
rule the width of its title:

```text
Origin
------
      upstream module: k8s.io/kubernetes
  upstream repository: https://github.com/kubernetes/kubernetes
      upstream commit: <sha>
     upstream release: v1.36.1
     generated module: monis.app/kk/rbac_authorizer
 generated repository: https://github.com/enj/rbac_authorizer
      relocated below: internal/kk
              licence: Apache-2.0

  extracted upstream packages
    ...

Modifications
-------------
  relocated packages
    internal/kk/pkg/registry/rbac/validation  (from pkg/registry/rbac/validation)
  changed files
  pruned upstream files
  applied patches
    (none)

Behavior changes
----------------

Staging module mapping
----------------------
  <source path> -> <module>@<version>

Copied dependency packages
--------------------------

Trademarks
----------

Upstream notices
----------------
================================================================================
BEGIN NOTICE OF k8s.io/kubernetes AT <sha>
================================================================================
...
================================================================================
END NOTICE OF k8s.io/kubernetes
================================================================================
```

The shape above is illustrative — it is assembled from the renderers rather than
copied from a golden file, because the notice has no golden. The rules it
follows are exact:

- An empty list renders `(none)` rather than being omitted. "No patches were
  applied" is evidence; a missing section is not.
- Licence-specific citations are conditional on the licence actually being that
  licence. Apache-2.0 gets its section 6 trademark citation and its section 4(d)
  notice-reproduction citation; a different identifier gets neither, because a
  citation attached to the wrong licence is worse than no citation.
- When there is nothing to declare, the behaviour-change section says so
  explicitly: *"The profile records no intended behaviour change relative to the
  upstream code at the commit above."*
- Prose wraps at a fixed width, so the file does not change shape with terminal
  width.
- URLs are checked: a non-`https` URL is refused, and a URL carrying user
  information is refused as a secret.

## SOAPBOX_PROVENANCE.txt

One per relocated package:

```text
soapbox package provenance

package: internal/kk/pkg/registry/rbac/validation
upstream package: pkg/registry/rbac/validation
upstream repository: https://github.com/kubernetes/kubernetes
upstream commit: <sha>

files:
  internal/kk/pkg/registry/rbac/validation/rule.go
    upstream: pkg/registry/rbac/validation/rule.go
    <one line per recorded change, or "unchanged">

pruned:
  (none)

patches:
  (none)
```

## Per-file modification notice

Every modified source file keeps its upstream licence header, its build
constraints, and its generated-code marker exactly where upstream put them, and
receives a notice above the package clause:

```go
// This file was modified by soapbox and is not the upstream original.
// Upstream repository: https://github.com/kubernetes/kubernetes
// Upstream path: pkg/registry/rbac/validation/rule.go
// Upstream commit: <sha>
// Imports under k8s.io/kubernetes were rewritten to monis.app/kk/rbac_authorizer/internal/kk.
```

Header preservation is structural rather than inspected. The rewriter never
reprints from the AST; every change is a byte-range replacement, so everything
outside a replaced range survives by construction. The notice anchors above the
file's doc comment when there is one, so it can never wedge itself between the
doc comment and the package clause, and it adopts the file's own line
terminator.

A file with no eligible change is returned byte-identical. It cannot acquire a
notice, and it cannot acquire a trailing newline.

## What "modified" means

Only three kinds of change are made to a source file:

1. **Imports.** Replaced at their literal `ImportSpec` positions, and only when
   rooted at the configured source import prefix. Imports owned by external
   modules are never relocated or rewritten.
2. **Generator directives.** An exact-key allowlist decides each one: keep,
   rewrite, or remove. Nothing is pattern-matched and no string literal is
   globally replaced. `go:generate` is removed because the generated module
   contains neither the generators nor the build harness the command names.
   Every `go:` directive other than `go:generate` is untouchable, as is the
   legacy build marker.
3. **Proto `go_package`.** Read by a dedicated scanner that tracks brace depth,
   so only a file-level option is eligible, and re-scanned afterwards to confirm
   that every changed option was reported and every unchanged one is
   byte-identical.

`go:embed` patterns are verified and never rewritten. A pattern that was correct
upstream stays correct without being touched; what can break is whether the
asset was copied at all, which is checked against the materialized files.

Each rewritten file is then reparsed and three things are proved: the
non-import syntax shape is unchanged, every differing import is justified by a
reported change, and no comment moved or silently became a documentation
comment. Import literals are paired positionally and only the ones a change
claims are blanked before comparison — blanking every literal would make the
check blind to corruption of an external import such as `k8s.io/api/rbac/v1`.

## Relocation

Relocated packages keep their complete upstream relative path:

```text
k8s.io/kubernetes/pkg/registry/rbac/validation
    becomes
monis.app/kk/rbac_authorizer/internal/kk/pkg/registry/rbac/validation
```

This is a hard invariant, not a convenience. Go resolves an `internal` import
against the last `internal` element of the importing path, so flattening or
shortening upstream paths would silently change which packages can import which,
and a nested internal package that upstream keeps unimportable could become
reachable in the generated module. The configured prefix is required to contain
an `internal` element for the same reason.

Packages are relocated one at a time; a file whose directory is not its own
package is refused rather than guessed at. Case-folded collisions are refused
too, so a tree that is fine on Linux cannot be broken on macOS or Windows.

## Cross-checking

Provenance is verified against the composed tree rather than against the
intentions that produced it:

- every relocated package must have a record;
- every relocated file must appear in its package's record;
- no record may describe a package that is not present;
- if no relocated file records a change, generation fails, because the notice
  would otherwise state that nothing was modified.

The behaviour-change disclosure is enforced separately: a profile that runs the
type policy analysis, acts on its decision, and then renders a `NOTICE` without
the effects that decision removed has published a module whose documented
behaviour is not its behaviour. See [behavior-changes.md](behavior-changes.md).

## Commit and tag provenance

Replayed commits carry exactly one `Kubernetes-commit: <sha>` trailer, appended
to an existing trailer block or as a new paragraph, then re-parsed to confirm
the trailer count and the last trailer are what was intended.

Annotated release tags carry `Source-tag`, `Source-commit`, and `Source-release`
in a trailer-only paragraph. The release URL is derived from the source
repository and validated before anything is written; a non-`https` source
repository is refused rather than producing a tag with a guessed provenance
record.

The synthetic release-projection commit deliberately carries *no* trailer, and
that is verified after writing. It is bookkeeping about dependency versions, not
a replay of an upstream commit, and a trailer would claim otherwise.
