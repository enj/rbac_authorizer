# GitHub App

Soapbox publishes as a GitHub App installation rather than as a person. An
installation token is scoped to the repositories the App is installed on,
expires in an hour, and can be narrowed further per run. A personal access token
is none of those things.

Creating the App is a one-time browser step. Everything after it is Go.

## The security posture

Five properties hold, and each is enforced somewhere rather than merely
intended.

**The App is never installed on `kubernetes/kubernetes`.** Replayed commit
messages contain upstream text, including issue-closing phrases such as
`Fixes #1234`. If the App could write to the upstream repository, replaying
history would act on it. It cannot, because it is not installed there.

**Pull request workflows never receive App credentials.** The generated `ci.yml`
holds no secret, its token may only read, and its checkout is told not to
persist a token so a later step cannot find one. `pull_request_target` — the one
trigger that would hand a fork's code the repository's own secrets — is
prohibited and appears in no workflow.

**Publishing runs only from the protected default branch.** The generated
`sync.yml` triggers on `schedule` and `workflow_dispatch` only, and its job
refuses to run from any ref but the default branch.

**The workflow token cannot write.** `sync.yml` grants `contents: read` and
`actions: read`. Writes are made with the installation token the engine mints.
Granting the workflow token write access as well would add a second, longer-lived
way to write that no step would use.

**Credentials never appear in an argument vector.** They reach Git through the
process environment only. There is no credential helper, no askpass program, no
`http.extraheader`, and no credential embedded in a remote URL anywhere in the
engine. Messages and tag bodies travel on stdin; identities travel in the
environment.

## Configuration

Only environment variable *names* live in `soapbox.yaml`. Values never do — the
schema has no field that could hold one, and strict decoding turns an invented
field into a decode error.

```yaml
githubApp:
  appIDEnv: SOAPBOX_GITHUB_APP_ID
  installationIDEnv: SOAPBOX_GITHUB_INSTALLATION_ID
  privateKeyEnv: SOAPBOX_GITHUB_APP_PRIVATE_KEY
  apiBaseURL: https://api.github.com
```

Each name must be upper case with underscores, with digits allowed after the
first character; the three must be distinct; and the API base URL's host must be
`api.github.com`.

`soapbox plan` and `soapbox generate` refuse to start when any of the three
names is set in the environment:

```text
plan: a plan must run without publishing credentials: SOAPBOX_GITHUB_APP_ID is set
```

Only the name is printed, never the value. A run that reads upstream and writes
a local tree needs no credential, so holding one is a configuration error rather
than a convenience. `soapbox setup` also builds its Git runner with no
credential entry at all, because it reads one local repository.

## One-time setup

1. Create a GitHub App under the account that will own the generated
   repositories. Give it a name and a homepage; no callback URL, no webhook.
2. Grant repository permissions. `contents: write` is what advances branches and
   creates tags. `metadata: read` is implied. Add `actions: read` only if the
   run should check that its own workflow is still enabled, and `issues: write`
   only if it should file conflict-tracking issues.
3. Do **not** grant organization or account permissions.
4. Generate a private key and download the `.pem`. GitHub shows it once.
5. Install the App on the generated module repository, and on the vanity site
   repository only for as long as the vanity bootstrap needs it. Choose
   *selected repositories*, never *all repositories*. Never install it on
   `kubernetes/kubernetes`.
6. Record the App ID from the App's settings page and the installation ID from
   the installation URL.
7. Add the three values as repository secrets in the generated repository, under
   the names the profile configures. The private key is the PEM file's full
   content including its header and footer lines.

Template secrets are not copied to repositories created from a template, so
every derived repository needs this step of its own.

## The private key

The engine accepts exactly one PEM block, either `RSA PRIVATE KEY` (PKCS#1) or
`PRIVATE KEY` (PKCS#8), holding an RSA key of at least 2048 bits, and validates
the key's internal consistency. An encrypted key is detected and refused — by
block type, by a `DEK-Info` header, or by an `ENCRYPTED` `Proc-Type` — rather
than producing a confusing parse failure. An EC key gets its own message.

No error message ever quotes key bytes.

## How a token is minted

1. An RS256 JWT is signed with the private key. Its header is
   `{"alg":"RS256","typ":"JWT"}` and it carries exactly three claims: `iss` (the
   App ID), `iat` backdated 60 seconds against clock skew, and `exp` 8 minutes
   out, inside GitHub's 10-minute ceiling. It is minted per request and never
   cached.
2. `POST /app/installations/{installation_id}/access_tokens` is called with that
   JWT, requesting the repositories and permissions the run needs. Requests
   carry `Accept: application/vnd.github+json`,
   `X-GitHub-Api-Version: 2022-11-28`, and a `soapbox/<version>` user agent.
3. The response is verified before use. An empty token, a token with no expiry,
   and a token already inside the renewal margin are all refused. The granted
   permissions must be at least the required ones, and the repository set must
   match in **both** directions — a token reaching a repository that was not
   requested is refused too.
4. The token is renewed when it comes within 5 minutes of expiry, which is
   configurable up to 30 minutes.

A verification failure is terminal and cached. The token that failed is dropped
and no retry re-mints it.

The token itself is not exposed by a getter. Callers either take an
`Authorization` header value or run a closure that receives the raw value. Every
minted token is seeded into a redactor before anything reads the response, and
stays seeded across renewals.

## Redaction

The redactor replaces exact secret values, never patterns, and replaces longer
values first so an overlapping prefix cannot leak a suffix. It covers strings,
byte slices, argument vectors, errors, and streams. The streaming writer holds
back enough trailing bytes to catch a secret split across two writes.

Every non-empty environment value handed to the Git runner is seeded
automatically, so forgetting to declare a secret cannot leak it. An environment
entry that is malformed is reported by *index* rather than quoted, because at
that point the whole entry may be the secret and the redactor does not exist
yet.

Remotes are redacted before appearing in any message: user information is
stripped from both URL and scp-like forms, and a value that cannot be parsed is
replaced entirely.

## Push safety

A push target must be `https` to `github.com`, or a local path for a rehearsal,
and may not embed credentials. A named remote is rejected because its URL lives
in configuration. Before pushing, all configuration scopes are queried for
`url.*.insteadOf`, `url.*.pushInsteadOf`, and `remote.*.pushurl`, and a
rewritten remote fails closed.

There is no force-push API. A `+`-prefixed refspec, a refspec with an empty
source, and the all-zero null object are each refused by name.

## What the API client does

| Call | Purpose |
|---|---|
| `POST /app/installations/{id}/access_tokens` | Mint an installation token. |
| `GET /repos/{owner}/{name}` | Repository metadata, including the default branch. |
| `GET /installation/repositories` | The repositories the token reaches. |
| `GET /repos/{owner}/{repo}/actions/workflows/{file}` | Confirm the sync workflow is still enabled, which is how the 60-day schedule disablement is noticed before it matters. |
| `GET`/`POST`/`PATCH` on issues, `POST` on issue comments | The keyed conflict-tracking issue. |

There is no repository creation, no branch protection, no releases, and no Git
data endpoints. Repository creation is an outward action reserved for the
approval gate.

The transport refuses a redirect that leaves the canonical origin, caps redirect
chains, drops the cookie jar, bounds the response size, and bounds pagination.

## Current state

The App client, the token lifecycle, and the credential boundary are implemented
and tested. **No command constructs an App today**, because publication to a
network remote is not yet possible: deciding what a push would do requires
listing the destination's refs, and only a filesystem destination implements
that. The generated `sync.yml` therefore exports the three secrets and runs a
plan, without `-apply`.

Enabling publication is a deliberate edit made at the outward-action gate, not a
default the template ships.
