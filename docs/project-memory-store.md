# Durable project memory

Project memory is the small set of long-lived facts AO keeps about a project
between runs, together with the provenance that says where each fact came from
and whether it can still be believed.

It is the store side of the project-memory work.
[`project-memory-baseline.md`](project-memory-baseline.md) is the measurement
side: it records per-dispatch evidence about what each agent had available and
what it consumed. This package **reads** that evidence as one memory source. It
does not change, wrap, or duplicate how evidence is recorded — it imports the
evidence schema so the two cannot drift apart.

- Code: `backend/internal/projectmemory` (item schema, store, staleness,
  baseline reader).
- Source it ingests: `backend/internal/observe/projectmemory` (unchanged).

## Where memory lives

Items are written under AO's data dir, honoring `AO_DATA_DIR`:

```
<AO_DATA_DIR or ~/.ao/data>/project-memory/items/projects/<project key>/memory.json
```

One file per project. The `<project key>` is a readable label plus a hash of the
full project id, so two projects whose ids sanitize to the same label never
collide, and no file is ever shared by two projects — that separation *is* the
multi-project isolation guarantee.

It is never written beside the repository it describes and never into an
OS-default application-data location; `ValidateStoreDir` refuses both a relative
directory and any path inside `~/Library/Application Support`, `AppData\Roaming`
or `AppData\Local`, matching the hard rule in `AGENTS.md`.

## What an item holds

| Field | Meaning |
| --- | --- |
| `project` | the project the fact belongs to; every read and write is scoped by it |
| `scope` | where in the project it applies — a module directory, a dispatch role — empty means project-wide |
| `type` | what kind of fact (`baseline-dispatch`, `file-usage`, `note`) |
| `content` | the fact itself, as text an agent can be handed |
| `source` | the evidence: producer kind, the producer's stable ref, the file it was read from and that file's hash |
| `createdAt` / `updatedAt` | when it first entered the store, and when its content or provenance last actually changed |
| `sourceCommit` | the commit it was derived at |
| `confidence` | how much weight it deserves, in `[0,1]`, computed from what the source could actually measure |
| `stale` / `staleReason` | whether its provenance still holds, and what stopped holding |

## Idempotent upsert

An item is addressed by its **identity**: project, scope, type and source kind,
plus the source's stable ref when it has one and the item's content hash when it
does not. `Upsert` compares the stored row's content hash and provenance against
the incoming one:

- identical → the row is left completely alone. No duplicate, and `updatedAt`
  does not move; an unchanged upsert does not even rewrite the file.
- changed → the existing row is updated in place, keeping its `createdAt`,
  moving `updatedAt`, and clearing any stale annotation (the fact was just
  re-derived).

Re-ingesting the same baseline evidence at the same commit is therefore a no-op.

## Staleness

`StaleCheck` re-judges an item's provenance against a checkout. Two things
invalidate a fact:

1. **The commit is unreachable.** The commit in `sourceCommit` is not present in
   the repository, or is no longer reachable from `HEAD` (a rebase, a reset, an
   abandoned branch). The item describes a history the checkout no longer has.
2. **The file moved.** The file in `source.path` no longer hashes to
   `source.fileHash`, or is gone.

The git questions go through `worktree.Git`, AO's existing read-only,
allowlisted git surface — this package adds no git command of its own. A
question git could not answer is an error, never a clean "still fresh".

`Store.RefreshStaleness` runs the check over a project and persists any verdict
that changed. It deliberately does not move `updatedAt`: staleness is a derived
annotation about whether a fact still applies, not a change to the fact itself.

Stale items are kept rather than deleted. Knowing that a fact went stale, and
why, is itself information, and re-deriving it is cheaper from the old row than
from nothing.

## Ingesting baseline evidence

```go
reader, _ := projectmemory.NewDefaultBaselineReader()   // <data dir>/project-memory/baseline
store, _ := projectmemory.NewDefaultStore()             // <data dir>/project-memory/items
result, err := reader.Ingest(ctx, store, projectmemory.IngestOptions{
    Project: "proj-123",     // used only when a record carries no project id of its own
    Repo:    "/abs/checkout", // optional: enables file hashes and HEAD resolution
    Git:     worktree.NewExecGit(""),
})
```

Each evidence record yields one `baseline-dispatch` item (the dispatch summary,
with every metric still labelled measured / estimated / unavailable exactly as
the evidence labelled it) and one `file-usage` item per inspected path, capped
per record and reported in the dispatch item when the cap truncates.

Records written under an evidence schema version this build does not know are
skipped rather than guessed at, and a record with no project id and no fallback
is an error rather than an invented project.
