# Project memory (P2-A)

AO's durable, incremental, provenance-carrying summary of a project, so the
Planner, Worker, Reviewer and the two Repair Agents do not re-derive the same
facts on every task.

This document is the design and the operating manual. The pre-existing context
inventory it was built from is [`p2-project-memory-audit.md`](p2-project-memory-audit.md);
the Phase-0 measurement recorder it sits beside is
[`project-memory-baseline.md`](project-memory-baseline.md); the older JSON fact
store it does **not** replace is [`project-memory-store.md`](project-memory-store.md);
the lifecycle P2-C layers on top of the task and decision memory described in
§8 is [`shared-task-knowledge.md`](shared-task-knowledge.md).

---

## 1. The rule everything else follows from

**Project memory is a cache. The repository is the source of truth.**

Concretely:

- The source of truth is the repository on disk plus AO's durable workflow rows
  (plan artifacts, checkpoints, review runs, repair intents, workspace
  fingerprints). Those are facts AO owns or can read directly.
- Project memory is a *derived semantic cache* over them: bounded summaries,
  each stamped with the commit it was derived at, the paths it was derived
  from, and a digest over those paths' content.
- **When the two conflict, the repository wins.** The memory item is marked
  stale and withheld; it is never reconciled with, or preferred over, what the
  working tree says.
- Every pack AO renders says this to the agent in its own header, because an
  agent that treats a cache as authoritative is the failure mode the whole
  provenance model exists to prevent.

The corollary is the **fail-closed rule**: a fact whose provenance cannot
currently be demonstrated is not served as authoritative context. Not
"served with a warning" — withheld. See §5.

---

## 2. What is stored

`domain.ProjectMemoryItem` (`backend/internal/domain/project_memory.go`) is one
durable fact:

| Field | What it is |
| --- | --- |
| `Key` | `(ProjectID, RepoID, Type, Scope, Key)` — the natural identity |
| `ID` | derived from the key (and the origin ref, for task-local facts) |
| `Origin` / `OriginRef` | `canonical`, or `task_local` plus the task it belongs to |
| `Summary` / `Content` | the one-line form and the bounded body |
| `SourcePaths` | the repo-relative paths it was derived from |
| `SourceCommit` / `SourceDigest` | the provenance a later check compares against |
| `Generation` | the indexing generation that last wrote it — the CAS fence |
| `State` / `StateReason` | `valid`, `stale`, `invalidated`, `rebuilding` |
| `Confidence` | in [0,1], from what the producer could actually observe |
| `CreatedAt` / `UpdatedAt` / `InvalidatedAt` | |
| `Metadata` | small, structured, provider-neutral annotation |

Types written today: `project_overview`, `architecture`, `module`,
`file_summary`, `dependency`, `convention`, `instruction`, `build_test`,
`decision`, `task_result`, `known_risk`. (`symbol_summary` and
`repository_relationship` are in the vocabulary and are not produced by the
P2-A indexer — see §11.)

**Bounds are enforced in `Validate`, not left to producers**: 400 bytes of
summary, 8 KiB of content, 64 source paths, 16 metadata entries. "Bounded and
pragmatic" is a property of the store, not an intention of whoever last wrote an
indexer.

### Confidence means something

The scale is *how directly AO observed it*:

| | |
| --- | --- |
| 0.95 | a verbatim excerpt of a file AO read this pass |
| 0.90 | a field parsed out of a declared manifest |
| 0.85 | a structural fact counted from the tree AO walked |
| 0.65 | what a prose document claims about the code |

There is no "0.5 by default" tier, because there are no guesses: every
derivation is mechanical (a heading, a manifest field, an import line). Nothing
is inferred by a model.

---

## 3. Where it is kept

SQLite, migration `0144_project_memory.sql`:

| Table | Holds |
| --- | --- |
| `project_memory_index` | one row per (project, repo): generation, phase, indexed commit, resume cursor |
| `project_memory_items` | the facts |
| `project_memory_relations` | the graph edges |
| `project_memory_sources` | reverse index: `(owner, path)`, so "what did this path prove" is an indexed lookup |
| `project_memory_files` | per-path digest ledger, so deletions are detectable without git |

It is SQLite rather than a file beside the JSON stores for four reasons, all
load-bearing: generation-conditioned CAS in a `WHERE` clause; path-indexed
invalidation instead of a full scan; a durable resume cursor; and one
transaction per fact so a partial write cannot leave un-invalidatable
provenance.

**Generation-conditioned CAS is mandatory and absolute.** Every mutating
statement carries `AND generation <= ?`. A write from a pass whose generation is
behind the stored row's is refused with
`store.ErrProjectMemoryStaleGeneration` and changes nothing. A stalled indexer
that wakes up after a newer pass finished cannot undo it.

---

## 4. Indexing

### Initial pass — bounded and restart-safe

`projectmemory.Indexer.Index` walks the repository in lexical order under
explicit bounds (`IndexLimits`), all of which are enforced in the indexer rather
than trusted to the caller:

| Bound | Default |
| --- | --- |
| `MaxFiles` | 6,000 |
| `MaxFileBytes` | 512 KiB |
| `MaxTotalBytes` | 256 MiB |
| `MaxModules` | 800 |
| `IgnoredDirs` | `.git`, `node_modules`, `vendor`, `dist`, `build`, `target`, `.venv`, `__pycache__`, … |
| `IgnoredExts` | images, archives, media, fonts, binaries, lockfiles, … |
| binary detection | a NUL byte in the first 8 KB, whatever the extension says |
| `CheckpointEvery` | 200 files between durable progress writes |

**It does not index every byte.** Each admitted path is classified into a role,
and only a named set of roles produces a fact: instruction files, READMEs,
architecture documents, manifests, build/test entry points, config files and
program entry points. An ordinary source file contributes its *shape* — module
membership and its import targets — and nothing else. That is what keeps a
3,000-file repository at ~560 facts rather than ~3,000.

**Restart safety** is the durable `(generation, phase, resume_cursor)` triple.
A crash leaves the row non-terminal; the next pass *resumes* that generation
rather than claiming a new one, skips re-deriving the paths at or before the
cursor, and keeps everything the dead pass had already written. Claiming a pass
from a terminal phase and resuming one in flight are two different statements,
and both are generation-conditioned, so two daemons racing to index or to
recover cannot both proceed.

**Honest limitation.** A full pass must *read* every admitted file, because a
content digest cannot be computed without reading. What a full pass saves on an
unchanged repository is the derivation and the writes: the store *reconfirms* an
unchanged fact (refreshing provenance and generation, leaving `updated_at`
alone) instead of rewriting it. The saving on the read itself belongs to the
incremental path.

### Incremental update — where the incrementality actually is

`Indexer.UpdateChanged` takes a change set and reads only the paths it names.
`Service.Sync` derives that change set from `git diff --name-status
--find-renames` between the last indexed commit and the current one.

Per changed path, in this order — and the order is the fail-closed rule:

1. **Invalidate first.** Everything derived from the path stops being
   authoritative. If the pass dies here, what is left is stale memory correctly
   marked stale, which is the safe direction.
2. **Re-derive second**, from the file as it now stands.

| Case | Handling |
| --- | --- |
| file changed | invalidate by path, re-read, re-derive, refresh the module census |
| file deleted | invalidate by path, drop the ledger row |
| file renamed | retire the old path, derive the new one |
| new file | derive, add to the ledger, refresh the module census |
| module removed | its last file's removal leaves no census entry → the module fact is invalidated |
| manifest changed | ordinary modified-file path; dependency and build/test facts are re-derived |
| branch moved, nothing indexed changed | empty change set → commit advances, zero files read, nothing invalidated |
| repo changed externally | not visible to a diff — this is what drift detection (§5) is for |

An incremental pass deliberately **does not** run the generation retire sweep.
That sweep means "a walk saw everything and did not re-confirm this"; an
incremental pass saw only what the diff named.

`Sync` falls back to a full pass — and says so in `FallbackReason` — whenever it
cannot get a trustworthy change set: no prior index, an unreachable previous
commit after a force-push, a non-git checkout. Guessing at a change set produces
holes AO cannot detect, and an undetectable hole in memory is worse than a scan.

---

## 5. Drift and invalidation

Four states, and the distinctions matter:

| State | Meaning | Served? |
| --- | --- | --- |
| `valid` | provenance checked and still holds | yes |
| `stale` | provenance moved (file edited, commit unreachable) | **no** — kept, because re-deriving from a known previous answer beats re-deriving from nothing |
| `invalidated` | the subject is gone (file deleted, module removed) | **no** — cannot be refreshed, only replaced |
| `rebuilding` | an indexer has claimed it and is re-deriving | **no** |

`projectmemory.Detector.Check` is the independent check for what no pass can
see: a file edited outside AO, a checkout swapped underneath it, a branch reset.
It recomputes the digest of each valid fact's source paths and compares.

It is built on one asymmetry, and that asymmetry is the safety argument: **a
fact can be disproved cheaply and can never be proved by this check.** So the
detector only ever demotes. It never restores a stale item to valid, because a
matching digest does not show the *derivation* is still right — only a
re-derivation does, and that is the indexer's job.

Aggregates (the repository overview, a module census) have no per-path
provenance: their source is the whole tree, and recomputing that is a full pass.
They are reported as `Unverifiable` rather than silently counted as confirmed.

**Fail closed at the pack boundary** (§6): a repository with no completed index,
or whose last pass failed, contributes *nothing*, with the reason stated. Half a
memory an agent cannot tell from a whole one is worse than none.

---

## 6. Memory context packs

`projectmemory.PackBuilder` assembles a bounded, deterministic, role-specific
`ContextPack`. Never the whole memory.

**Selection** is relevance → scope specificity → freshness → confidence, with
the derived item id as the final tiebreak, so ties can never be resolved by map
iteration order. Relevance is combined rather than compared, so a weak signal
cannot outrank a direct hit: a changed-path match is worth more than any number
of keyword matches, by construction.

**Section order per role** — not arbitrary:

| Role | Order | Why |
| --- | --- | --- |
| Planner | overview → architecture → repo relationships → modules → conventions → decisions → build/test | told what the system *is* before what any file does; spans every repository of the project |
| Worker | conventions → instructions → modules → files → dependencies → build/test → decisions → risks | told the rules it must obey and the modules it is about to touch |
| Reviewer | conventions → architecture → risks → decisions → modules → files → build/test | a review is an application of rules, so the rules come first |
| Repair | risks → previous task outcomes → decisions → modules → files → conventions → build/test | it is repairing something that already went wrong |

**Budget**: 24 KiB and 40 facts by default, enforced against the *rendered*
bytes rather than against the raw facts — rendering adds a header, section
headings, list markers and body indentation, measured at roughly 8% on a real
repository, which is enough for a pack to exceed a budget it claimed to honour.
Degradation is two-stage: a fact that will not fit whole is first reduced to its
summary alone, and dropped only if even that will not fit. Both are counted and
both are stated in the rendered pack.

**Determinism**: the same store, request and budget produce byte-identical
output, and `Digest` proves it. Two dispatches with the same digest were given
the same memory.

---

## 7. Multi-repo and worktree semantics

The distinction the model enforces:

| | |
| --- | --- |
| **Project** | the AO registry entry; a Planner pack spans it |
| **Repository** | `RepoID = hash(resolved absolute root)`; the unit memory is keyed by |
| **Branch** | recorded on the index row, so a branch move is visible even when it changed nothing |
| **Worktree** | *not* a repository — see below |
| **Source commit** | stamped on every fact |

`RepoID` is a hash of the **symlink-resolved** path, so two paths reaching one
checkout are one repository. A moved checkout gets a different identity rather
than silently inheriting another repository's memory; `repo_path` is stored
alongside so the hashed identity stays explainable.

**An isolated worktree never creates a parallel permanent memory.** Facts
derived from a task's unintegrated work are stored with `Origin = task_local`
and that task's ref:

- They are visible **only** to that task. `PackBuilder` filters by
  `OriginRef`, and the router adapter filters them out entirely (it cannot know
  which task it is assembling for).
- A repository walk never retires them: the walk did not produce them, so its
  silence says nothing about them.
- They carry a **different row identity** from the canonical fact with the same
  key, so the two coexist.
- `DiscardTaskMemory` removes them when the task ends.
- `PromoteTaskMemory` turns them canonical — called by whatever authority
  *integrated* the work, and by nothing else. A task saying "I decided X" is a
  claim about a branch; it becomes a fact about the project only when that
  branch is part of the project.

---

## 8. Task and decision memory

`Service.RecordTaskOutcome` persists what one task did: what changed, why, which
files and modules, the decisions taken, the verification result, the risks left
behind. Decisions and risks become facts of their own, because they outlive the
task that produced them.

**There is no field for a transcript, a session log or a diff body, and nowhere
to put one.** Everything else is already durable in AO's workflow rows, and a
second unbounded copy in memory is exactly what the brief forbids.

Task memory is the one part of the system that grows with *time* rather than
with the repository, so it is the one part with an explicit retention rule:
`MaxRetainedTaskResults = 200` per repository, oldest beyond the bound retired
(not deleted — "AO's memory of task X aged out" stays readable).

**P2-C extends this section rather than replacing it.** The row shape here is
unchanged; what was added is the lifecycle that decides who may read a task's
facts and for how long they stay current — sharing scopes, decision
supersession, risk resolution, conflict marking, per-scope compaction, and the
evidence-based relevance gate that keeps one task's history away from unrelated
work. See [`shared-task-knowledge.md`](shared-task-knowledge.md).

---

## 9. The graph port

`projectmemory.MemoryGraph` is three verbs — `Name`, `Upsert`, `Neighbors` — so
an external backend can be wrapped without exposing its model.

Edge vocabulary: `depends_on`, `implements`, `defined_in`, `changed`, `affects`,
`contains`, `derived_from`. Endpoints are `(kind, key)` rather than memory-item
ids, so a relation may name a module that has no summary item yet: the graph and
the item set are allowed to be at different completeness, and neither blocks the
other.

Traversal is bounded (`MaxGraphDepth = 3`, `MaxGraphLimit = 512`) and
breadth-first, so a limit truncates the far edge of the fan-out rather than one
arbitrary branch. Order is deterministic, because a pack's digest depends on it.

### Grae / Graphify

**No Grae or Graphify integration exists in this repository, and P2-A does not
add one.** The audit's reproducible search
([§4.1](p2-project-memory-audit.md#41-graphify--grae-the-explicit-determination))
found Graphify named only in prose and Grae not at all. Adding a fragile
external dependency in order to claim an integration would be worse than a clean
port.

`LocalGraph`, over the SQLite relations table, is **not a placeholder** — it is
the implementation AO ships and relies on. To connect an external backend later:

1. Implement `MemoryGraph` over its client. Map
   `domain.ProjectMemoryRelationKind` onto that backend's edge labels; do not
   leak its vocabulary upward.
2. Compose with `TeeGraph`: canonical writes go to `LocalGraph` and are
   mirrored to the external one. Reads prefer the external backend and fall back
   on any failure.
3. Register it at the composition root (`daemon.go`). Nothing else changes.

`UnavailableGraph` exists so "the optional adapter is down" is a real, tested
state rather than a nil check: writes are dropped, traversals return
`ErrGraphUnavailable`, and `TeeGraph` absorbs both into a fallback. Indexing and
traversal both keep working — that is the structural form of "AO works even
when Grae is not available".

---

## 10. Role integration

Opt-in and fallback-safe, gated by the pre-existing `AO_CONTEXT_ROUTER` flag,
which is **off by default**. With it unset, `wfrouter.Instrument` hands every
dependency back untouched and dispatch is byte-for-byte what it was before P2-A.

| Role | How it is reached | New surface? |
| --- | --- | --- |
| Planner | already-routed `PlannerContext.Documents` | no |
| Worker | already-routed `SpawnConfig.IssueContext` | no |
| Repair (both agents) | **the same Spawner path** — both repairers are ordinary workflow runs (audit §2.6) | no |
| Reviewer | `ReviewerLaunchRequest.SystemPrompt`, which had **no producer at all** before P2-A | wrapper only |

`contextrouter.DurableMemorySource` adapts the durable store to the
`MemorySource` the router has always read, so no prompt builder changes. It
enforces the two rules the router cannot: only `valid` + `canonical` facts cross
the boundary, and a read failure yields no items and no error.

The reviewer wrapper only ever **adds**: a `SystemPrompt` somebody already set
is left alone, `Prompt` is never touched (it carries the instruction), and every
failure path sends the original request.

**A memory failure cannot disable AO.** `Service.Context` never returns an
error: a storage problem yields an empty pack with a stated `FallbackReason`,
exactly as an unindexed repository does.

---

## 11. Baseline (measured, not asserted)

Measured on this repository (`agent-orchestrator`,
`feat/engineering-control-center`), default limits and default pack budget:

**Indexing**

| | |
| --- | --- |
| files walked | 3,333 |
| files admitted | 3,136 (197 excluded by the bounds) |
| bytes read | 31.8 MB |
| facts written | 560 |
| relations written | 589 |
| modules discovered | 429 |
| truncated | no |
| first pass | ~780 ms |
| **second pass, unchanged tree** | 3,136 skipped, **0 facts written, 560 reconfirmed, 0 retired**, ~530 ms |

**Facts by type**: 429 module, 78 file_summary, 22 build_test, 13 architecture,
11 dependency, 4 instruction, 2 convention, 1 project_overview.

**Context packs** (one changed path, 24 KiB / 40-fact budget):

| Role | Candidates | Candidate bytes | Selected | Selected bytes | ~Tokens | Dropped |
| --- | --- | --- | --- | --- | --- | --- |
| Planner | 467 | 173,108 | 40 | 5,981 | 1,496 | 427 |
| Worker | 546 | 154,752 | 14 | 23,734 | 5,934 | 532 (+1 to summary) |
| Reviewer | 544 | 201,300 | 40 | 15,409 | 3,853 | 504 |
| Repair | 531 | 134,798 | 40 | 15,409 | 3,853 | 491 |

### What these numbers do and do not say

They describe **AO-assembled context only**. Per the audit's §1 finding, the
Worker's, Reviewer's and both Repair Agents' *harness* file and git reads are
not observable by AO at all — the dispatch wrappers for those roles declare only
`ContextPayload` capability, and `RepeatedReads` is emitted as `Unavailable` for
them even with `AO_PROJECT_MEMORY_BASELINE` on.

So `SourcesReused` is named carefully: it lists the paths whose *summarised*
content AO supplied from memory instead of re-deriving it this dispatch.
**It is not a count of reads the agent avoided**, and nothing here should be
reported as one. Any claim about harness reads would need observability AO does
not have — that is P2-B's problem, not a number P2-A may invent.

The one saving P2-A *can* demonstrate end to end is the second row of the
indexing table: re-indexing an unchanged repository writes zero facts and
reconfirms 560, so a repeated pass costs no writes and no invalidation.

---

## 12. Operating it

```bash
ao memory status <project-id>        # generation, indexed commit, per-state census, phase
ao memory inspect <project-id>       # the individual facts, stale ones included
  --repo --state --type --path --limit
ao memory rebuild <project-id>       # re-derive; --purge deletes first
ao memory invalidate <project-id>    # --path retires those paths; no --path runs drift detection
```

HTTP: `GET /api/v1/projects/{id}/memory`, `GET …/memory/items`,
`POST …/memory/rebuild`, `POST …/memory/invalidate`.

Nothing is deleted by an ordinary invalidation. A retired fact stays readable,
because knowing that it went stale and why is information, and re-deriving from
it is cheaper than from nothing.

There is deliberately **no command that prints a context pack**: a pack is
assembled for a specific dispatch with that dispatch's changed paths and budget,
and printing one out of context would show something no agent was ever given.

---

## 13. Known limitations

> **Superseded by P2-B for items 1.** P2-B adds the lifecycle trigger, the
> single-flight, the rollout modes and the measured before/after — see
> [project-memory-optimization.md](project-memory-optimization.md). The
> limitation below is kept because it records why P2-A stopped where it did.

1. **No automatic trigger, for indexing or for task memory.** P2-A ships the
   indexer, the incremental path, the drift detector, the packs, the CLI and the
   API — and nothing in the daemon calls them on its own. Verified, not assumed:
   outside this package and its service there is no production caller of
   `Index`, `Sync`, `UpdateChanged`, `RecordTaskOutcome`, `PromoteTaskMemory` or
   `DiscardTaskMemory`. Today an operator drives indexing through
   `ao memory rebuild`, and task memory is an API a later checkpoint calls.

   This is deliberate rather than unfinished. *When* to spend a scan, and at
   which workflow boundary a task's outcome and its promotion authority belong,
   are both cost-and-lifecycle questions — P2-B is the cost checkpoint, and the
   promotion authority has to be the same one that proves integration. Wiring
   them before those are decided would mean choosing them by accident.
2. **`symbol_summary` and `repository_relationship` are unproduced.** They are
   in the vocabulary and the store handles them; the P2-A indexer derives
   neither. Symbols would come from `internal/codegraph`'s existing native
   indexer, which remains the right producer for them.
3. **Aggregates are unverifiable by drift detection** (§5). Only a full pass can
   confirm a module census or a repository overview.
4. **Harness reads stay unobserved.** See §11. No metric here should ever be
   read as a saving in agent-side reads.
5. **`workflow_mutation_provenance` is still empty in production.** See §14.
6. **The `internal/projectmemory` package now holds two stores**: the durable
   P2-A one and the older JSON `Store`/`BaselineReader` that feeds the Phase-0
   measurement recorder. They are deliberately separate — the baseline recorder
   must keep measuring exactly what it measured before, or the before/after in
   [`project-memory-baseline.md`](project-memory-baseline.md) means nothing.

---

## 14. `workflow_mutation_provenance`: deferred, with reasons

**Status: deferred to P2-D.** The debt is real and unchanged: the table exists
(migration 0133), the store method `RecordWorkflowMutationProvenance` exists,
and **no non-test caller writes to it**, so it is empty in production.

Project memory was assessed against it during P2-A, and the finding is:

- **Project memory does not need it.** Every provenance field a memory item
  carries — source paths, source commit, source digest, generation — is derived
  from the repository itself, not from AO's mutation ledger.
- **It could not supply what task memory wants anyway.** A mutation-provenance
  row records branch, worktree, base/head SHA and workspace fingerprints; it
  carries **no file list**. `TaskOutcome.FilesChanged` would still have to come
  from the caller or from a diff.
- **A "minimal, clearly correct" integration does not exist at this scope.** The
  mutation boundaries are where AO observes a workspace fingerprint change —
  work adoption, worker signal reconciliation. Writing rows there means deciding
  what counts as an AO-owned mutation, how rows dedupe across retries, and how
  they fence against generations. That is P1 correctness work, and the P2-A
  brief explicitly forbids turning this into a P1 reimplementation.

**No rows were fabricated to make the table look populated.** When P2-D takes
it up, the useful thing for project memory is a commit range per task, from
which a diff yields the changed-file list that `RecordTaskOutcome` currently
receives from its caller.
