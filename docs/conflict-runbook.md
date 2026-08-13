# Conflict runbook

What to do when a run is refused. Every refusal in this document is deliberate:
the engine ran, reached an answer, and the answer is no. Nothing has been
published, and no consumer ref has moved.

## First: read the exit code

| Code | Meaning | What it tells you |
|---|---|---|
| 0 | Success | — |
| 1 | Runtime failure | Something broke: a subprocess failed, a file could not be read. Not a policy decision. |
| 2 | Usage | The command line cannot work. The failing command's flags are printed with the error. Fix the invocation. |
| 3 | Check | The engine ran and found policy violations. **This is the interesting one.** A report exists. |
| 4 | Canceled | The context ended before the command did. Cancellation is decided before any other classification, so an interrupted run never reports as a finding about the profile. |

## Second: read the report, not the error

Stdout carries the machine-readable artifact and nothing else. Every diagnostic
goes to stderr. A refused run still writes its report whenever it measured
anything, *before* the failure is returned, so a conflict never loses its
artifact. If the report could not be written, that failure is joined to the
finding rather than replacing it — you are told about both.

Always pass `-report <path>` in automation:

```text
go run ./cmd/soapbox plan -dir .. -cache /state/src -report /tmp/plan.json
```

The report is JSON with a schema version, and every list in it is sorted and
non-null. It contains no absolute path, no environment value, and no secret.

Its `failure` object names the stage and the message, and for a patch failure it
embeds the whole conflict.

## Patch conflicts

### What happened

A patch in the configured series did not apply against the source commit. The
run stopped, the work tree was reset to `HEAD`, and nothing was written.

There is no partially patched success state. A half-applied series would produce
a tree that no profile describes and that no maintainer could reproduce.

### What you get

The report's `failure.patch` object:

```text
sourceRef        the ref being processed
sourceSHA        the source commit
patchID          the patch file
patchIndex       zero-based position in the series
patchCount       series length
stage            apply, prune, or cancel
conflictedPaths  sorted, deduplicated
status           git status, one rendered line per entry
diff             git diff against HEAD, with conflict markers
```

The diff is taken against `HEAD` rather than the index deliberately. A three-way
conflict records its markers in the index as well as the work tree, so an
index-relative diff would omit exactly the evidence you need.

The evidence is collected *before* the rollback, because the rollback destroys
it, and the rollback runs on a detached context with its own budget, because
cancellation is one of the ways this path is reached and an inherited context
would turn the rollback into a no-op that reports success at restoring nothing.

### What to do

Read `stage` first.

**`apply`** — the patch did not apply to the upstream tree. Upstream moved. Fix
the patch:

1. Reproduce locally with `-keep-worktree` so the materialized tree survives:

   ```text
   go run ./cmd/soapbox plan -dir .. -cache /state/src -tag <tag> -keep-worktree
   ```

2. Rebase the patch against the new upstream content.
3. Narrow the selectors. `since` and `until` bound the ancestry the patch
   applies to; `branches` bounds which lines of development it applies to. A
   patch that has been upstreamed should get an `until`, not a deletion, so the
   history it covered still reproduces.
4. Re-run the plan.

**`prune`** — the patch applied but pruning could not be reasserted afterwards.
The patch and the prune list disagree. A patch may not target a pruned file, and
a patch that reintroduces a pruned path will fail here. Either the patch is
wrong or the prune list is.

**`cancel`** — the run was interrupted between patches. Re-run it.

### What not to do

Do not commit the conflicted tree. Do not apply the patch by hand into the
scratch worktree — the next run rebuilds it from scratch, so the fix has to live
in the patch file.

## Closure and prune failures

| Message shape | Cause | Fix |
|---|---|---|
| a prune entry names a file that is not there | Upstream renamed or deleted it. Absent fails closed rather than being assumed already done. | Update `prune.files` to the new path, or remove the entry if upstream removed the file. |
| prune entries name files, never directories | A directory or glob in `prune.files`. | List the files. |
| a required file is missing, or its package is no longer in the closure | Upstream moved code, or a prune removed the last importer of a package. | Decide whether the shrink is intended. If it is, update `prune.required`; if not, the prune list is wrong. |
| a prune would remove a package's last Go file | The profile is trying to delete a package by emptying it. | Remove its importers instead. A package leaves the closure by losing its importers. |
| a denied import reentered the closure | Upstream added an import of a denied package to retained code. | Prune the new importer, or accept the package and remove the deny entry with a recorded reason. |
| a closure limit was exceeded | The closure grew. | Investigate *why* first. Raising a limit to match reality is the correct action only once you know the growth is legitimate. |

Limits are evaluated in a fixed order, so a tree over several ceilings always
fails the same way. A golden diff is reported as `diff` rather than as a
failure of its own; read it alongside whichever limit fired.

## Type policy blockers

```text
the substitution of <internal> by <external> is blocked, so the profile prunes a
package whose equivalence was not proved
```

One of the five analyses found something. Read the blocker list in the report.

- **Markers** — upstream stopped declaring the pairing, or the group names
  diverged. The pairing is no longer upstream's own claim, so it cannot be the
  engine's either.
- **Conversions** — a hand-written or lossy conversion appeared, or a field is
  no longer assigned. Field-preserving mechanical conversion is the whole proof.
- **Method sets** — the external type lost a method, or a signature changed.
- **Field identity** — a field, its order, or a `json`/`protobuf` tag changed.
- **Global effects** — an import-time effect became reachable through the public
  API. That is no longer a documentable behaviour change; it is observable.

There is also a blocker that no analysis produces:

```text
pruning changed the published API: ...
```

The facade generated from the pre-prune tree differs from the one generated from
the post-prune tree. The argument for pruning was that no consumer can tell, and
that argument just failed. Do not weaken the comparison — find what changed.

## Dependency policy refusals

A candidate refused by a **correctness gate** cannot be admitted by editing
numbers. Read the evidence: it names the exact type, the exact path through the
public API that reaches it, or the exact global. See
[dependency-policy.md](dependency-policy.md).

A candidate refused by a **cost gate** can be admitted by an override, which
needs a justification, an approver, and a Kubernetes minor expiry. Two failure
modes to expect later:

```text
override <package> gate <gate> approved by <approver> was good through v1.N,
source is v1.M: cost gate override expired
```

The override lapsed. Re-justify it or drop the copy — it does not revert to the
unrelaxed gate.

```text
cost gate override applies to no candidate
```

The override outlived the thing it was written for. Remove it.

A gate reported as unmeasured is refused rather than scored as zero. Supply the
measurement.

## Generation refusals

| Message | Meaning |
|---|---|
| `ref <x> is a branch, and only a release tag can be generated from ...` | Branch generation is not implemented. Use a release tag. |
| `... materializing a copied package is not implemented` | The profile proposes staging copies, or the policy approved some. Neither can be materialized today. |
| `release policy v1-to-v0 requires a v1 source tag, got ...` | The tag does not map under the release policy. |
| `the source commit stages no module the root module requires ...` | The source commit is not a Kubernetes checkout of the expected shape. |
| `the source commit declares module <a> but the profile is written against <b>` | Wrong source repository. |
| `the pre-prune pass read commit <a> and the post-prune pass read commit <b>` | The upstream ref moved mid-run. Re-run it; a tag should not do this. |
| `the profile prunes N files but the pre-prune and post-prune trees are identical` | The prune list selects nothing. Upstream moved the files, or the paths are wrong. |
| `the generated facade needs module requirements the tidied go.mod does not state` | The facade references a module the metadata does not declare. |
| `strict mode refuses N advisory notices` | `-strict` turned advisories into a failure. Read the notices; they are listed. |

## Synchronization refusals

| Message | Meaning | Action |
|---|---|---|
| `a synchronization publishes a release, and <ref> names no release` | Only tags are publishable. | Use `-tag`. |
| `the destination records release <a> and this run publishes <b>, which needs the commits between them replayed` | Backfill is not implemented. | Publish releases in order, or wait for backfill. |
| `the destination records profile <a> and this run generated under <b>, which starts an epoch this engine cannot graft` | The profile hash changed. | Grafting a new epoch is not implemented. Compare `soapbox validate -format profile` output to see what changed. |
| `publication requires a configured destination remote` | No remote, or a network remote whose refs cannot be listed. | Use a local rehearsal with `-local-remote`. |
| `the module was generated from an overridden source remote, so a tag claiming <url> would be a false provenance record` | `-source-remote` was used. | Do not publish a run that read from a mirror. |
| `approval does not name this synchronization` | The hash does not match. | Re-read the manifest and quote its hash exactly. |

`-apply` requires `-approve`, and `-approve` requires `-apply`. A publication
asked for without a hash cannot be served, and a hash offered without a
publication is an operator who believes they published and did not.

## Publication failures

These are the append-only boundary doing its job. None of them should be worked
around.

| Sentinel | Meaning |
|---|---|
| tag moved | An existing tag points somewhere else. A published tag never moves. Identical tags are a no-op; differing ones are fatal. |
| non-fast-forward | A branch update would rewind. |
| force update | A refspec carried a `+`. |
| delete update | A refspec would delete a ref. |
| remote drift | The remote moved between planning and pushing. Re-plan. |
| manifest modified | The manifest does not hash to its recorded hash. |
| object missing / object type | The object is absent locally, or a tag does not resolve to a tag object. |
| scope mismatch | A consumer and a non-consumer ref were about to travel in one push. |

### After a failed `-apply`

The engine reports what it did, on stderr, for each half:

```text
soapbox: published non-consumer refs/heads/soapbox-state
soapbox: consumer push failed: none published, refs/heads/main, refs/tags/v0.36.1 not
```

Read the phrasing carefully, because the halves mean different things:

- *"refs were not attempted"* — that half never ran. The consumer half is not
  attempted when the bookkeeping half fails, which is why the split exists: a
  failure between them leaves a resumable state rather than an unaccounted
  release.
- *"refs were already published"* — the push was a no-op. Nothing to do.
- *"push failed: A published, B not"* — a partial result the engine could read
  back. Re-run; the append-only rules make a re-run safe.
- *"push failed and the destination could not be read afterwards, so ... may or
  may not have been published"* — the one case needing a human. Read the
  destination yourself before re-running, and compare against the manifest's
  recorded object names.

Non-consumer refs are pushed first, so the state ref landing without the branch
and tag is the expected shape of a mid-publication failure, not a corruption.

## Setup refusals

| Message | Fix |
|---|---|
| the work tree has uncommitted files | Commit or stash. Setup transforms a committed tree. |
| `<dir>` is inside the repository rooted at `<root>` | Point `-dir` at the repository root. |
| a tracked symlink | Remove it. |
| `<path>` is already tracked, so this repository has a root module the template does not have | This is not a template checkout, or setup already ran. |
| an untracked file already sits at a payload path | Remove it. Setup will not overwrite a file it did not plan for. |
| a case-only variant of a payload path exists | Rename it. |
| `-report <path>` must be outside the repository | Setup requires a clean work tree; a report inside it would make the next command refuse. |
| approved `<a>`, manifest is `<b>` | The repository changed since the dry run. Re-read the manifest and approve the new hash. |
| `<path>` changed or disappeared after planning | Something edited the tree between plan and apply. Re-plan. |

A failed apply reports itself as partial. Setup writes before it deletes and
renames atomically per file, so a partial apply leaves files present rather than
missing.

## Escalation

Anything that is not on this list and exits 1 is a runtime failure rather than a
policy decision. Capture the report, the exit code, and stderr. The engine
distinguishes the three deliberately, so "which of the three was it" is the
first question worth answering.
