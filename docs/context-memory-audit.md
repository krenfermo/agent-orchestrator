# Context and memory audit

**Purpose.** Trace every source of context AO assembles or causes to be
assembled for the Planner, Worker, Reviewer and Repair/Fix agents, and record
what is rescanned per task, what is durable, and which builders could consume
project memory without being rewritten. This audit exists so the next phase of
memory work **extends the abstractions that already exist** instead of
introducing a second, parallel memory system.

Scope: `backend/internal` as of this document. Per-component reference docs stay
authoritative for their own component — [code-graph.md](code-graph.md),
[context-router.md](context-router.md),
[context-router-metrics.md](context-router-metrics.md),
[project-memory-baseline.md](project-memory-baseline.md),
[project-memory-store.md](project-memory-store.md). This document is the
cross-cutting view none of them has.

## 1. The one distinction that governs everything below

AO's context falls into two categories that are easy to conflate:

- **AO-assembled context** — bytes AO itself reads, renders and hands to a
  provider: the planner's document set, `SpawnConfig.IssueContext`, the worker
  prompt, the review prompt, the fix prompt, the incident pack.
- **Agent-assembled context** — bytes the coding harness reads on its own,
  inside the worktree, after launch: `AGENTS.md`/`CLAUDE.md` discovery, `git
  diff`, `git status`, source files opened by the agent's own tools.

Only the first category is routable, budgetable, or memory-backed today. The
second is by far the larger of the two and AO can currently only *observe* it
(`internal/observe/projectmemory`), never *supply* it. Any claim that "AO already
sends the reviewer the repository conventions" is false: no AO code path reads
`AGENTS.md` for the reviewer. Grep confirms exactly one non-test Go call site
that opens repository documents for an agent payload —
`adapters/planner/command/context.go:40`.

## 2. Per-role context inventory

### 2.1 Planner

| Source | Built by | Rescanned per run? | Durable? |
| --- | --- | --- | --- |
| `AGENTS.md`, `README.md`, `go.mod`, `package.json`, `docs/architecture.md`, `docs/STATUS.md` (48 KiB cap each) | `adapters/planner/command.ContextBuilder.Build` | **Yes — full re-read from disk on every planner call** | The resulting `PlannerContext` manifest is persisted with the plan (`workflow/task_graph_wiring.go`), so it is durable *as a record*, never reused *as a cache* |
| Branch, HEAD SHA, dirty flag | same builder, three `git` subprocesses | Yes | Recorded in the manifest |
| Routed selection (optional) | `contextrouter/wfrouter.InstrumentPlanner` | Yes | No — transient per dispatch |

The document list is hard-coded. There is no per-project override, no hash-gated
skip, and no reuse across the runs of the same project: two objectives created
back to back re-read and re-hash the same six files. The builder *does* compute
a SHA-256 per document (`PlannerDocument.SHA256`) — the mechanism a cache would
need already exists and is simply never consulted.

### 2.2 Worker

| Source | Built by | Rescanned per task? | Durable? |
| --- | --- | --- | --- |
| Task prompt (objective + acceptance criteria + guardrails, ± read-only variant, ± effective spec) | `workflow.BuildWorkStepPromptWithSpec` — pure, no IO | N/A (template expansion) | Persisted in `workflow_steps.artifact_json`; rebuilt byte-identically after a restart via `promptForRun` |
| `SpawnConfig.IssueContext` — pre-fetched tracker issue body | tracker adapters | Per spawn | No |
| Routed selection replacing `IssueContext` (optional) | `contextrouter/wfrouter.InstrumentSpawner` | Per spawn | No |
| **Everything else** — repo conventions, source, history | the harness itself, in the worktree | Per session, uncontrolled | No |

The worker prompt is deliberately pure and deterministic, which is what makes it
restart-safe. It is also the reason the worker receives **no** project-specific
knowledge from AO: the only variable evidence channel is `IssueContext`.

### 2.3 Reviewer

| Source | Built by | Rescanned? | Durable? |
| --- | --- | --- | --- |
| Objective, acceptance criteria, effective spec, worker session id, branch, worktree path, base SHA, workspace fingerprint, review run id, available dependencies, future planned tasks | `workflow.BuildReviewPrompt` — pure, no IO | N/A | Inputs come from durable rows (plan artifact, task graph, workspace fingerprints) |
| The diff under review | **the reviewer agent runs `git status`/`git diff` itself** — the prompt instructs it to | Per review, per cycle | No |

`BuildReviewPrompt` documents why it does not reuse `internal/review`'s
PR-centric prompt. The relevant fact for this audit is what it omits: the
reviewer is told *where* to look and never *what is already known* about the
project. **The reviewer is not routed** — see §4.

### 2.4 Fix (same session as the worker)

| Source | Built by | Rescanned? | Durable? |
| --- | --- | --- | --- |
| Objective, criteria, effective spec, cycle number | `workflow.BuildFixPrompt` — pure, no IO | N/A | From durable rows |
| Reviewer findings body | fetched live from the `ReviewRuns` port at dispatch (`workflow/fix_dispatch.go`); referenced by id, never copied into a workflow table | Per cycle | The review run is durable; the fix prompt is not stored |
| Role-scoped context pack | `workflow.RenderContextPackForRoleExcluding` over a `TaskCheckpointSummary`, minus the three fields the fix prompt already carries verbatim | Per delivery, **no second fetch** | Derived from durable checkpoint facts |

`RenderContextPackForRoleExcluding` is the closest thing AO has to a working
memory-injection pattern: one computed fact set, several role-scoped views,
explicit de-duplication against the prompt. It is worth copying rather than
reinventing.

### 2.5 Repair Agent / Diagnostic Agent

| Source | Built by | Rescanned? | Durable? |
| --- | --- | --- | --- |
| Run detail, step states, checkpoints, stop reason/detail, signature | `workflow.BuildIncidentContextPack` from `IncidentPackInput` | Per incident | All from durable workflow rows |
| Branch, HEAD, porcelain status, bounded diff, worktree path | `workflow/incident_advisor.go:322-332` via `workspaceFacts` observation | Per incident | Observation is transient |
| Session facts (harness, activity, last-activity, terminated) | `sessionFacts.GetSession` | Per incident | From durable session rows |
| Newest reviewer verdict, newest verify output, provider notes | ports, at build time | Per incident | Durable sources |
| `EvidenceSnapshot`, `ChildEvidence` | `workflow/evidence_snapshot.go`, `workflow/incident_child_pack.go` | Per incident | Rendered from durable facts |

`IncidentPackInput`'s doc comment states the governing rule explicitly: the
builder performs **no I/O** and **never walks the source tree** — the only
repository-derived facts are ones AO had already observed. The pack then applies
a byte budget by priority, marking every section `Truncated`/`Dropped` and
recording `DroppedSections` and a `Digest`.

This is a second, independently-written budgeted context assembler, parallel to
`contextrouter`'s. See §6.

## 3. The three existing subsystems

| Package | What it is | Durability | Populated in production? |
| --- | --- | --- | --- |
| `internal/codegraph` | Provider-agnostic symbol/edge graph; native AST indexer; hash-gated incremental update (`FilesParsed`/`FilesSkipped`/`FilesRenamed` is its audit trail); per-project JSON graph under `~/.ao/codegraph/<key>/graph.json` | **Durable** | **No** |
| `internal/projectmemory` | Durable per-project facts: `MemoryItem{Scope,Type,Content,Source,SourceCommit,Confidence,Stale,StaleReason,ContentHash}`; content-hash idempotent upsert; staleness against HEAD/file hashes; `~/.ao/project-memory/items/<key>/memory.json` | **Durable** | **No** |
| `internal/contextrouter` | Role-aware assembler: per-role section ordering, per-role token budgets, compact→expanded escalation, hard-cap enforcement, `AO_CONTEXT_ROUTER` flag (off by default) | Transient per dispatch | Only behind the flag |

`internal/observe/projectmemory` is a different thing despite the name: it is
the Phase-0 **measurement** recorder (`AO_PROJECT_MEMORY_BASELINE`), writing one
evidence file per dispatch with `FilesInspected`, `RepeatedReads`,
`SourceTokensAvailable`, `ContextSentBytes/Tokens`. `RepeatedReads` is the
metric the durable store is supposed to reduce.

### 3.1 The finding that matters most: nothing writes to either durable store

Outside tests, the only non-test call sites of `codegraph.Index` /
`NativeIndexer` and of `projectmemory.Store.Upsert` / `BaselineReader.Ingest`
are:

- `contextrouter/default.go` — constructs both as **readers**.
- `observe/ctxregress/fixture.go` — the regression **harness**, which indexes a
  fixture repo and upserts fixture memory.

Consequences, in production, today:

1. No daemon path, CLI command, HTTP route, or workspace watcher ever indexes a
   real project. Every `Graph` query from the router therefore returns
   `ErrNotIndexed`, which the router degrades on gracefully — it emits a note
   and routes without graph evidence. Graph evidence is, in effect, permanently
   unavailable.
2. No production path ever writes a `MemoryItem`. `Store.List` returns empty, so
   the router's memory section is always empty.
3. `Store.RefreshStaleness` has no production caller, so no fact is ever
   invalidated — currently vacuous, and a live hazard the moment writes start.

**This, not the absence of abstractions, is the actual gap.** The storage
schema, the identity/idempotency rules, the staleness model, the path isolation
under `~/.ao` and the read-side wiring all exist and are tested.

## 4. Routed vs merely observed: the surface asymmetry

`daemon/workflow_wiring.go:325,333` wraps the dependency set twice.

| Dispatch surface | Baseline observer (`wfdispatch.Instrument`) | Context router (`wfrouter.Instrument`) |
| --- | --- | --- |
| `Planner` | yes | **yes** (routes `PlannerContext.Documents`) |
| `Spawner` (worker) | yes | **yes** (routes `SpawnConfig.IssueContext`) |
| `ReviewerLauncher` | yes | no |
| `MessageSender` (fix delivery) | yes | no |
| `Verifier` | yes | no |

The asymmetry is deliberate and pinned by test
(`contextrouter/wfrouter/wfrouter_test.go:382-390` asserts the last three are
handed back identically). It is nonetheless the boundary any memory work has to
cross: the reviewer and the fix path are exactly the roles for which
`roleSectionOrder` already defines an ordering (`RoleReviewer`, `RoleFix`) that
nothing can currently reach.

Both wrappers are also strictly opt-in and both default to off:
`AO_PROJECT_MEMORY_BASELINE` (unset ⇒ nil recorder ⇒ untouched deps) and
`AO_CONTEXT_ROUTER` (unset ⇒ nil router ⇒ untouched deps).

## 5. Rescanned per task vs already cached

**Rescanned every time (no cache consulted):**

- The planner's six documents and three `git` calls, per planner invocation.
- `contextrouter.GitDiffSource` — one `git diff --name-status --find-renames`
  per routed request, and a routed request may run twice (compact, then
  expanded).
- The reviewer's own `git status`/`git diff`, inside the agent, per review cycle.
- The incident advisor's workspace observation, per incident.
- Everything the harness reads inside the worktree, per session — unbounded and
  unmeasured unless the baseline recorder is on.

**Cached / durable and reusable:**

- `codegraph` graphs — content-hash gated, so re-indexing an unchanged tree is
  cheap and reports `FilesSkipped`. Never populated (§3.1).
- `projectmemory` items — content-hash idempotent, commit-stamped, staleness-
  aware. Never populated (§3.1).
- Plan artifacts, `PlannerContext` manifests, checkpoints, review runs, workspace
  fingerprints — durable in SQLite, and already the sources the pure prompt
  builders read from.

**Durable but not context:** the baseline evidence files are an audit trail, not
a retrieval source; nothing reads them back at dispatch time except the
regression harness.

## 6. Two budgeted assemblers, not one

`contextrouter` and `workflow.BuildIncidentContextPack` were written
independently and solve the same problem with the same shape:

| | `contextrouter` | `IncidentContextPack` |
| --- | --- | --- |
| Unit | `Section{Kind,Title,Source,Content,Priority}` | `IncidentPackSection{Title,Body,Priority}` |
| Budget | per-role token budget + hard cap | whole-pack byte budget |
| Degradation | pack/drop by priority, notes on the selection | drop by priority, `Truncated`/`Dropped`/`DroppedSections` |
| Provenance | selection notes, baseline routing record | `Digest`, `BuiltAt`, `Signature` |
| Evidence sources | diff, graph, memory, documents | durable workflow rows only, no I/O |

They should not be merged blindly — the incident pack's no-I/O rule is
load-bearing and must survive. But a third budgeted assembler must not be
written. If memory needs to reach the Repair Agent, the right move is a
`memory` section added to `IncidentPackInput` (a plain field, filled by the
caller, preserving the no-I/O rule) rather than giving the pack builder a store.

## 7. Which builders could consume memory without a rewrite

Ordered by cost. "No rewrite" means: no change to the builder's signature
contract, no new storage code, no violation of its stated invariants.

1. **`contextrouter` — already consumes it.** `MemorySource.List` is satisfied
   as-is by `*projectmemory.Store`; `gatherMemory` ranks by
   `memoryScore(item, touched, role)` and already skips stale items. Cost of
   making it real: **write a producer**, not a consumer.
2. **`ReviewerLauncher` and `MessageSender`.** `roleSectionOrder` already
   defines `RoleReviewer` and `RoleFix`. The work is a `wfrouter` wrapper per
   surface, mirroring `InstrumentSpawner` — including its rule that an
   unresolvable checkout root means *send the original payload*, never a
   silently-thinner one. Both surfaces are already observed by the baseline, so
   the before/after is measurable on day one.
3. **`adapters/planner/command.ContextBuilder`.** It already hashes every
   document. Adding memory means appending `PlannerDocument`-shaped entries, or
   better, letting the existing planner routing wrapper inject them so the
   builder keeps its single job. Its current hard-coded list is also the
   cheapest place to stop re-reading six files per run.
4. **`workflow.BuildFixPrompt` + `RenderContextPackForRoleExcluding`.** Memory
   would enter as additional `TaskCheckpointSummary`-derived facts, keeping the
   "one fact set, role-scoped views, explicit de-duplication" property intact.
5. **`BuildIncidentContextPack`.** Via a new plain input field only (§6).
6. **`BuildWorkStepPrompt`.** Last, and possibly never. Its purity is what makes
   the worker prompt restart-reproducible (`promptForRun` rebuilds it
   byte-identically). Memory belongs in the worker's `IssueContext` channel,
   which is already routed, not in the prompt.

## 8. Conclusions

1. **Do not build a second memory system.** `projectmemory` (durable facts,
   provenance, idempotent upsert, staleness) and `codegraph` (durable,
   incremental, per-project) are the abstractions to extend. The router already
   reads both.
2. **The gap is production writers, not readers.** Nothing indexes a real
   project and nothing upserts a real fact. Until that changes, enabling
   `AO_CONTEXT_ROUTER` routes on diff and documents alone.
3. **Wire staleness with the first writer.** `RefreshStaleness` exists, is
   tested, and has no caller. A store that accumulates facts without ever
   invalidating them is worse than an empty one.
4. **Extend `wfrouter` to the reviewer and fix surfaces** rather than adding
   context-assembly code inside `workflow`. The role budgets already exist; the
   nil-router-is-a-no-op discipline is what makes that safe to ship dark.
5. **Keep the incident pack's no-I/O rule.** Feed it memory as an input field.
6. **Everything stays behind its flag until measured.** The baseline recorder
   already covers all five surfaces, and `observe/ctxregress` already fails the
   build when routing changes an outcome — that harness is the acceptance gate
   for any memory work, and it needs no new infrastructure either.
