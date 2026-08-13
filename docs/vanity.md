# Vanity import path

The generated module is imported as `monis.app/kk/rbac_authorizer` and stored in
`github.com/enj/rbac_authorizer`. The two are connected by a static page that
the Go toolchain reads when resolving the import path.

## Status: not implemented

The engine does not generate the vanity page. There is no `vanity` package, no
HTML rendering, no `go-import` meta tag emission, and nothing that touches the
site repository.

What exists is five validated configuration fields. They are checked whenever
`soapbox validate` runs, so the values the bootstrap will use cannot drift from
the module they describe, but no code reads them beyond that.

The bootstrap below is therefore a manual procedure performed once, at the
outward-action gate, alongside repository creation and the first tag.

## Configuration

```yaml
vanity:
  repository: enj/enj.github.io
  path: kk/rbac_authorizer/index.html
  importPath: monis.app/kk/rbac_authorizer
  repositoryURL: https://github.com/enj/rbac_authorizer
  probeURL: https://monis.app/kk/rbac_authorizer?go-get=1
```

Validation ties every field to the rest of the profile, so a page that would
send the toolchain to the wrong repository is a profile error rather than a
runtime surprise:

| Field | Rule |
|---|---|
| `repository` | An `owner/name` slug. |
| `path` | Relative, traversal free, and must end with `/index.html`. |
| `importPath` | Must equal `destination.module` exactly. |
| `repositoryURL` | Must equal `https://github.com/<destination.repository>`, with **no** `.git` suffix. This is the repository *page*, not the clone URL, which is why it differs from `destination.remote`. |
| `probeURL` | Must equal `https://<destination.module>?go-get=1`, and its host must be the module's own domain rather than `github.com`. |

## How resolution works

`go get monis.app/kk/rbac_authorizer` does not know that `monis.app` is not a
code host. It issues `GET https://monis.app/kk/rbac_authorizer?go-get=1` and
looks for a meta tag in the response:

```html
<meta name="go-import" content="monis.app/kk/rbac_authorizer git https://github.com/enj/rbac_authorizer">
```

Three space-separated fields: the import path prefix, the VCS, and the clone
root. A `go-source` tag is optional and only improves links from documentation
tools:

```html
<meta name="go-source" content="monis.app/kk/rbac_authorizer https://github.com/enj/rbac_authorizer https://github.com/enj/rbac_authorizer/tree/main{/dir} https://github.com/enj/rbac_authorizer/blob/main{/dir}/{file}#L{line}">
```

The page must be served over HTTPS at the exact path, and the import path prefix
in the tag must match what the toolchain asked for.

## Bootstrap procedure

Performed once, and only after the outward-action manifest is approved.

1. Add `kk/rbac_authorizer/index.html` to `enj/enj.github.io`, following the
   shape of the existing `go/index.html` page in that repository.
2. Wait for GitHub Pages to publish, then confirm the metadata is live:

   ```text
   curl -s 'https://monis.app/kk/rbac_authorizer?go-get=1'
   ```

   The response must contain the `go-import` tag above.
3. Publish the module tag `v0.36.1`.
4. Verify end to end from a clean module cache:

   ```text
   go get monis.app/kk/rbac_authorizer@v0.36.1
   ```

5. Remove the App's access to the site repository. Normal sync never needs it
   again — the page is written once and the module repository is the only thing
   that changes afterwards.

## Ordering

The page must exist before the first tag is published, or a consumer resolving
the module during that window gets a resolution failure that the module proxy
may cache. Publishing the page first costs nothing: a `go-import` tag pointing
at a repository with no tags simply resolves to no versions.

## Why the module domain is not the repository host

The import path is the module's identity for the lifetime of every `go.mod` that
names it. Keeping it on a domain the author controls means the code can move
between hosts without every consumer editing their requirements, and without a
`v2`-style path change that has nothing to do with the API.

The cost is one static page that must keep being served. That is the reason the
probe is verified as part of the bootstrap rather than assumed.
