# P2-A project-memory audit: what context AO already builds, and what P2 must not rebuild

**Status:** audit only. This document changes no code. It is the pre-work gate
for P2-A ("project memory"): before a single line of memory code is written,
every place AO already assembles, caches, re-sends, or measures context is
traced, classified, and given a verdict — *extend this* or *this is not the
foundation*.

**Method.** Every claim below is anchored to a `file:line` in this checkout.
Where a claim is an absence ("nothing calls this"), it was established by
grepping the whole `backend/` tree for the identifier and discarding `_test.go`
files and the defining package itself; the absences are called out explicitly
in [§7](#7-the-three-real-gaps) because they are the load-bearing findings.

**Headline verdict:** **extend, do not build.** AO already has four of the five
pieces a project-memory system needs — a measurement harness, a durable memory
store with provenance and staleness, a code-graph boundary with a working
native indexer, and a role-aware context router that consumes both. What is
missing is not a foundation; it is *three wires* between components that already
exist. See [§8](#8-recommendation).

---

## 1. Inventory of context sources

| # | Source | Where it is built | Where it is sent | Durable? | Cacheable? |
|---|--------|-------------------|------------------|----------|------------|
| S1 | Planner context documents (`AGENTS.md`, `README.md`, `go.mod`, `package.json`, `docs/architecture.md`, `docs/STATUS.md`) | `backend/internal/adapters/planner/command/context.go:26-52` | `planner.go:152` marshals the whole context to JSON, `:160-161` embeds it in the prompt body, `:168` passes it as argv | durable content, transient assembly | **cacheable** (hashed already) |
| S2 | Planner git facts (branch, HEAD SHA, dirty flag) | `context.go:28-39` | same prompt | transient | must-refresh |
| S3 | Worker task prompt (objective + acceptance criteria + guardrails) | `backend/internal/workflow/plan.go:121` (`BuildWorkStepPromptWithSpec`), artifact from `plan.go:66` | `ports.SpawnConfig.Prompt` | **durable** (`workflow_steps.artifact_json`, migration `0095_workflow_step_artifact.sql:9`) | cacheable — already is |
| S4 | Worker issue context (pre-fetched tracker body) | `backend/internal/service/session/issue_context.go:16-34`, formatted at `:206`, capped at `:14` (12 000 bytes) | `ports.SpawnConfig.IssueContext` → `session_manager/prompt.go:53-74` | transient (tracker-owned) | must-refresh (comments/state move) |
| S5 | Worker/orchestrator system prompt + project rules | `session_manager/prompt.go:76-112`, rules loaded at `:128-147`, wired at `manager.go:3266` | agent system prompt | durable (repo file) | **cacheable** |
| S6 | Reviewer prompt (objective, criteria, scope, worktree, base SHA, fingerprint) | `backend/internal/workflow/review_prompt.go:53-131`, scope block `:137` | reviewer launch | durable inputs, transient rendering | cacheable except SHAs |
| S7 | Fix prompt (objective, criteria, verbatim review findings) | `backend/internal/workflow/fix_prompt.go:29-67` | `MessageSender.Send` into the *existing* worker session | findings are durable in `review_runs`; the prompt is not copied into workflow tables (`fix_prompt.go:17-22`) | must-refresh (fetched live per cycle) |
| S8 | Session context pack (task checkpoint summary, role-scoped view) | `backend/internal/workflow/session_context_pack.go:17` / `:27` / `:55` | `task_boundary.go:41`, `cascade.go:351-364`, `failover.go:299`, `decision_resolver_wiring.go:147` | derived from durable checkpoints | cacheable per checkpoint generation |
| S9 | Incident context pack (Diagnostic Agent's *only* input) | `backend/internal/workflow/incident_context_pack.go:188`, input struct `:113-181`, budgets `:37-56` | `BuildIncidentDiagnosticPrompt`, `incident_prompt.go:23` | durable digest (`IncidentRecord.PackDigest`) | must-refresh (collected fresh, `incident_advisor.go:226-232`) |
| S10 | Repair Agent prompt (the approved *diagnosis*, not the pack) | `backend/internal/workflow/incident_prompt.go:93-133` | repair agent launch | durable (`Incident.Diagnosis`) | cacheable |
| S11 | Repository source scan (file count/bytes by extension) | `backend/internal/observe/projectmemory/estimate.go:97-145` | measurement only — `aobaseline/main.go:200` | transient measurement | must-refresh per commit |
| S12 | Code-graph index (symbols, edges, per-file content hashes) | `backend/internal/codegraph/native.go:107` (`Index`), `:149` (`IncrementalUpdate`), `:199` (`Query`) | `contextrouter/router.go:437` (`gatherGraph`) | **durable** — `<AO_DATA_DIR>/codegraph/projects/<key>/graph.json`, `codegraph/store.go:45`, `:166` | **cacheable, hash-gated** |
| S13 | Working-tree diff (`git diff --name-status --find-renames`) | `backend/internal/contextrouter/sources.go:57-78` | `router.go:382` (`gatherDiff`) | transient | must-refresh |
| S14 | Durable project memory items | `backend/internal/projectmemory/memory.go:1-26`, store `store.go:226` (`Upsert`), staleness `stale.go:119` / `:191` | `contextrouter/router.go:553` (`gatherMemory`) | **durable** — `<AO_DATA_DIR>/project-memory/items/<key>/memory.json`, `store.go:19` | **cacheable, provenance-invalidated** |
| S15 | Per-dispatch baseline evidence | `backend/internal/observe/projectmemory/recorder.go`, sink `store.go`, decorators `wfdispatch/wfdispatch.go:44-277` | not sent to any agent; read by `projectmemory/baseline.go:64` | **durable** — `<AO_DATA_DIR>/project-memory/baseline/<run>/<id>.json` | append-only evidence |
| S16 | Workflow artifacts / checkpoint ledger | `workflow_steps.artifact_json` (`0095_workflow_step_artifact.sql`), `workflow_checkpoints` | rehydrated on recovery: `plan.go:175` (`promptForRun`) | **durable** | cacheable — already is |

Two roles deliberately assemble *no* project context and are listed for
completeness: **Verify** runs a deterministic command (`aobaseline/main.go:152-155`
states why it has no baseline task), and **Decision resolution** answers a
question about a run, not a checkout (`contextrouter.go:99-103`).

---

## 2. What gets re-derived on every single task

These are the costs P2 is being asked to remove. Each is re-computed from
scratch, per dispatch, with no reuse across tasks in the same project:

1. **Every planner context document is re-read and re-sent in full.**
   `context.go:40-51` opens six files, truncates each at 48 KiB
   (`context.go:16`) and SHA-256s them — and then the whole context is
   marshalled (`planner.go:152`) and embedded verbatim in the prompt body
   (`planner.go:160-161`). The hash is computed and *never used to skip
   anything*: two consecutive plans over an unchanged repo send byte-identical
   document bodies twice. This is stated as the router's own motivation at
   `contextrouter/contextrouter.go:5-8`.
2. **Three git commands per planner call** (`context.go:37-39`), plus one more
   `git diff` per routed dispatch (`sources.go:73`).
3. **The tracker issue body is re-fetched per spawn** (`issue_context.go:27`)
   and re-sent in full up to 12 000 bytes, even when a previous task in the same
   run already carried it.
4. **The whole source tree is re-walked** by `ScanSource`
   (`estimate.go:97-145`) for every baseline record — measurement only today,
   but it is the same walk a naive memory implementation would add.
5. **Standing prompt text is re-sent on every dispatch.** The guardrail block
   (`plan.go:146-166`), the review-only guardrails (`review_prompt.go:89-125`),
   the fix guardrails (`fix_prompt.go:58-61`) and the system prompt
   (`prompt.go:76-112`) are byte-identical across every task in a project and
   are transmitted every time.
6. **Project rules are re-read from disk per spawn** (`prompt.go:133-142`).

### What is *already* cached, and must not be re-implemented

- **The code graph.** `native.go:107-142` skips any file whose content hash
  still matches the persisted entry (`IndexResult.FilesSkipped`,
  `codegraph.go:127`), re-keys byte-identical renames without re-parsing
  (`:132`), and persists atomically per project
  (`store.go:200-243`). Isolation is by hashed project key
  (`store.go:121-132`), so sibling checkouts named `api` cannot collide.
- **Durable memory items.** `store.go:226` is a content-hash idempotent upsert:
  re-ingesting an unchanged fact leaves the row byte-for-byte alone, including
  `UpdatedAt` (`memory.go:16-20`).
- **Plan artifacts.** `promptForRun` (`plan.go:169-175`) rebuilds a worker
  prompt from the persisted artifact after a daemon restart, and
  `BuildPlanArtifact` is pure, so the rebuild is byte-identical.
- **Baseline evidence.** Written once per dispatch, never recomputed.

---

## 3. Durable vs transient, cacheable vs must-refresh

The classification rule used below: a fact is **durable** if it survives the
run that produced it and remains true about the project; it is **cacheable** if
its invalidation condition is something AO can *check cheaply* (a content hash,
a commit reachability test); it is **must-refresh** if the only honest way to
know it is to ask again.

### Safe to reuse across tasks (cacheable)

| Fact | Invalidation signal AO already has |
|------|-----------------------------------|
| Project metadata bodies (S1, S5) | SHA-256 already computed at `context.go:49`; `Source.FileHash` on a memory item (`memory.go:21-24`) |
| Symbol/file graph (S12) | per-file content hash, `native.go:256` (`syncFile`) |
| Acceptance criteria, effective spec (S3, S6, S7) | plan artifact version + amendment set |
| Repository conventions, architecture facts | commit reachability, `stale.go:119-190` |
| Prior dispatch measurements (S15) | append-only; superseded, never wrong |
| Diagnosis text (S10) | incident id + pack digest |

### Must always refresh

| Fact | Why a cache would be a lie |
|------|---------------------------|
| Working-tree diff (S13) and dirty flag (S2) | changes between two dispatches of the *same* task |
| HEAD SHA / branch (S2) | the router keys everything else off it |
| Tracker issue body and comments (S4) | owned by an external system; also a trust boundary — `prompt.go:173` marks it as untrusted external text |
| Review findings (S7) | fetched live per fix cycle by design (`fix_prompt.go:17-22`) |
| Incident evidence snapshot (S9) | `incident_advisor.go:226-232`: collected fresh so the operator sees what AO can obtain *now* |
| Session liveness / activity (S9) | a process state, not a project fact |

### The invalidation machinery already exists

`projectmemory/stale.go` is the part most memory projects get wrong and this
repo already has: every item names the commit it was derived at and, when it is
about a file, that file's hash (`memory.go:21-24`); `StaleCheck.Evaluate`
(`stale.go:119`) marks an item stale when the commit is no longer reachable
from HEAD or the file's bytes moved; `RefreshStaleness` (`stale.go:191`) sweeps
a project; and a *failure to check* is never reported as "still fresh"
(`stale.go:17-20`). The router then refuses to serve stale items at all
(`router.go:580-586`). **P2-A must not invent a second staleness model.**

---

## 4. `backend/cmd/aobaseline` and `npm run baseline:project-memory`

**Verdict: reused and extended. Not superseded.**

- The npm script is `package.json:17` — `cd backend && go run ./cmd/aobaseline`.
- The harness is `backend/cmd/aobaseline/main.go` (259 lines), documented in
  `docs/project-memory-baseline.md`.

What it already does, and why it is the right thing to build on:

1. **It measures the real pipeline, not a model of it.** Each of its four tasks
   calls the *unmodified* exported builder the daemon's own dispatcher calls:
   `plannercommand.ContextBuilder{}.Build` (`main.go:221`),
   `BuildWorkStepPromptWithSpec` (`:236`), `BuildReviewPrompt` (`:242`),
   `BuildFixPrompt` (`:252`). A P2 "before/after" measured against anything else
   would not be comparable to production.
2. **It refuses to fake numbers.** Everything the harness cannot observe is
   recorded as `unavailable` with a stated reason (`main.go:46-50`), never as
   zero. `docs/project-memory-baseline.md:9-12` states the rule.
3. **It writes to the same evidence tree the live daemon writes to.** Both go
   through `projectmemory.NewDirSink` — `main.go:82` and
   `daemon/workflow_wiring.go:352` — so a baseline file and a live-run file
   (`AO_PROJECT_MEMORY_BASELINE=1`, `workflow_wiring.go:338`) have the same
   shape.
4. **Its schema is already the memory ingestion input.**
   `projectmemory.BaselineReader` (`baseline.go:24-45`) reads exactly these
   records and turns them into memory items (`baseline.go:147`, `:242`,
   `:309`), importing the recorder's schema rather than restating it — the
   comment at `baseline.go:28-33` is explicit that this is to stop two copies of
   the same knowledge drifting apart.

**How P2-A extends it rather than replacing it:** add task rows to `tasks()`
(`main.go:156-187`) for any new context surface P2 introduces, and add metrics
to the evidence schema for memory hit/miss. Do **not** add a second harness, a
second sink, or a second token-estimation heuristic — `EstimateTokensFromBytes`
(`estimate.go:28`) is already shared by the router
(`contextrouter.go:42-47`, `:327`) precisely so routed and baseline payloads are
counted the same way.

**Also already present and easy to miss:** `backend/cmd/aoctxregress`
(`main.go:1-23`) is the disabled-vs-enabled regression gate — it runs the same
fixture through the pipeline twice, once with routing off and once on, using
`contextrouter.Default` (`ctxregress.go:278`) so the harness measures the
configuration that actually ships, and wiring both decorators in the production
order (`ctxregress.go:327-328`). It is **not** in `package.json`; adding
`ctxregress:context-router` alongside `baseline:project-memory` is a one-line
extension P2-A should make rather than writing new tooling.

---

## 5. Pre-existing Graphify / graph / indexing code

**Yes — substantial graph and indexing code already exists. P2-A extends it; it
must not replace it.**

| What | Path | Size |
|------|------|------|
| Provider-agnostic code-graph boundary | `backend/internal/codegraph/codegraph.go` | 146 lines |
| Native in-tree indexer | `backend/internal/codegraph/native.go` | 493 lines |
| Graph model | `backend/internal/codegraph/graph.go` | 328 lines |
| Per-project persistence | `backend/internal/codegraph/store.go` | 269 lines |
| Extractors (Go AST + scan-based) | `extract.go`, `extract_go.go`, `extract_scan.go` | 411 lines |
| Git-diff → graph-delta parsing | `backend/internal/codegraph/diff.go` | 119 lines |
| Design doc | `docs/code-graph.md` | 128 lines |

**On "Graphify" specifically.** The name appears in exactly two places in this
repository, both as an *example of a pluggable third-party provider*, never as
an integration:

- `backend/internal/codegraph/codegraph.go:5-8` — "A third-party tool
  (Graphify, an LSP-backed indexer, a hosted service) can be plugged in by
  implementing `CodeGraphProvider`".
- `backend/internal/codegraph/codegraph.go:67` — `Name()` returns
  `"native"`, `"graphify"`, ….
- `docs/code-graph.md:5` repeats the same framing.

There is **no Graphify adapter, client, binary invocation, dependency, or
configuration anywhere in the tree.** There is no package, type, or identifier
named `Grae`. Anyone reading the boundary comment and concluding AO "has a
Graphify integration" is wrong; what AO has is a four-method interface
(`codegraph.go:66-80`) with a documented implementation contract
(`:42-65`) and one in-tree implementation behind it.

**P2-A's position:** implement nothing new at this layer. The
`CodeGraphProvider` interface is the extension point; if a richer index is ever
wanted, it arrives as a second implementation of that interface, and every
caller keeps talking to `Index` / `IncrementalUpdate` / `Query`.

---

## 6. The context router: already built, already wired, switched off

`backend/internal/contextrouter` (≈1 700 non-test lines) is the consumer side
of project memory and it is complete enough to route today:

- Role vocabulary and mapping from `domain.WorkflowRole`:
  `contextrouter.go:65-119`.
- Per-role budgets with a hard cap that is asserted at the end of every
  assembly, not only in tests: `budget.go:85-92`, `contextrouter.go:300-302`.
  Defaults: planner 6 000/18 000/24 000, reviewer 5 000/15 000/20 000, worker
  4 000/10 000/14 000, fix 2 000/5 000/7 000, verify 500/1 200/1 600.
- Progressive retrieval, compact → expanded: `contextrouter.go:122-134`.
- Three evidence sources, all read-only: git diff (`router.go:382`), code graph
  (`router.go:437`, narrowed to a query-only interface at `sources.go:25-32` so
  routing *cannot* mutate an index), durable memory (`router.go:553`).
- Honest accounting: `Considered` vs `Selected` sizes split by origin
  (`contextrouter.go:268-281`), and `ReusedBytes`/`NewBytes` aggregated at
  `observe/context_metrics.go:44-56` — the reused/new split is *exactly* the
  metric P2 needs to prove memory is working, and it already exists.
- Wiring by decorator, never by editing dispatch: `wfrouter/wfrouter.go:58`,
  installed at `daemon/workflow_wiring.go:330`.
- **Off by default.** `AO_CONTEXT_ROUTER` (`flag.go:15`, `:19`); an unset flag
  yields a nil router and `Instrument` hands the deps back untouched
  (`wfrouter.go:58-60`).

Only two surfaces are routed today, and the reasoning is sound and worth
preserving: the planner's documents and a worker spawn's `IssueContext`
(`wfrouter.go:12-25`). Reviewer/fix/verify carry *instructions*, and budgeting
an instruction truncates the rules rather than the evidence.

---

## 7. The three real gaps

Everything above is built. These three absences are why none of it produces
value in a live run today. Each was verified by grepping `backend/` for the
identifier, excluding `_test.go` and the defining package.

### G1 — Nothing ever indexes the code graph in production

`codegraph.NewNativeIndexer` has exactly two callers:
`contextrouter/default.go:35` (which only ever calls `Query`) and
`observe/ctxregress/fixture.go:352` (the test harness, which *does* call
`Index` at `fixture.go:356`). **`Index` and `IncrementalUpdate` have no
production caller at all.** So in a live run `gatherGraph` queries a project
that was never indexed, `Query` returns `ErrNotIndexed` (`codegraph.go:33-35`),
and the router records "graph evidence unavailable" in the selection's notes
(`router.go:455`, `:472`) — correctly, honestly, and uselessly.

### G2 — Nothing ever writes a durable memory item in production

`BaselineReader.Ingest` (`baseline.go:147`), `Store.Upsert` (`store.go:226`),
`Store.Replace` (`store.go:313`) and `RefreshStaleness` (`stale.go:191`) have
**no non-test caller**. `NewDefaultBaselineReader` (`baseline.go:49`) is never
called outside the package. The only production construction of the memory
store is `contextrouter/default.go:40`, which passes it to the router as a
*reader*. So `gatherMemory` (`router.go:553`) lists an empty project on every
live dispatch.

### G3 — Both flags are off, so the measured "before" is the only reality

`AO_PROJECT_MEMORY_BASELINE` (`workflow_wiring.go:338`) and `AO_CONTEXT_ROUTER`
(`flag.go:15`) both default to off. That is the right default for routing — it
changes what an agent is handed — but it means no evidence accumulates from
ordinary use, which in turn means G2 would have nothing to ingest even if it
were wired.

**These three gaps are a closed loop.** Evidence is not recorded (G3), so memory
cannot be ingested (G2), so the router has nothing to reuse, and the graph it
would rank against was never built (G1). Closing them in order — index, ingest,
enable — turns roughly 5 000 existing lines from dormant into load-bearing.

---

## 8. Recommendation

> **Extend the existing foundation. Build no new store, no new indexer, no new
> harness, no new staleness model, no new token heuristic, and no new dispatch
> wiring mechanism.**

### Why extend

| P2-A would need | It already exists at | Would rebuilding be justified? |
|-----------------|----------------------|-------------------------------|
| A durable, per-project fact store under `~/.ao` | `internal/projectmemory/store.go:19`, `:137`, `:226` | No — content-hash idempotent upsert and atomic save are already correct |
| Provenance + invalidation | `internal/projectmemory/memory.go:21-24`, `stale.go:119`, `:191` | No — and a second model would contradict the first |
| A symbol/file index with incremental updates | `internal/codegraph/native.go:107`, `:149` | No — hash-gated, isolated per project, atomically persisted |
| A pluggable third-party index seam | `internal/codegraph/codegraph.go:66` | No — this is the seam |
| Budgeted, role-aware assembly | `internal/contextrouter/router.go`, `budget.go:85` | No |
| Measurement of before/after | `cmd/aobaseline`, `cmd/aoctxregress`, `observe/context_metrics.go:29` | No |
| Wiring without editing dispatch | `wfdispatch/wfdispatch.go:277`, `wfrouter/wfrouter.go:58` | No — the decorator pattern is established twice |

### What P2-A should actually build

In dependency order, each item a wire rather than a system:

1. **Close G1 — index on a real trigger.** Call `NativeIndexer.Index` when a
   project is registered or a workspace first materializes, and
   `IncrementalUpdate` from the diff AO already computes at `sources.go:57`.
   The provider interface, the store, the isolation and the hash-gating are
   done; what is missing is a caller and a trigger.
2. **Close G2 — ingest evidence into memory.** Call
   `BaselineReader.Ingest` (`baseline.go:147`) on a bounded schedule against
   `Store.Upsert`, and `RefreshStaleness` (`stale.go:191`) against HEAD.
   `IngestOptions` and the per-record file cap (`baseline.go:22`) already exist.
3. **Close G3 — decide the default posture.** Baseline recording is cheap and
   agent-invisible; routing is not. Recommendation: enable baseline recording
   by default (or on a first-party opt-in surfaced in the UI) and keep
   `AO_CONTEXT_ROUTER` opt-in until `aoctxregress` runs in CI.
4. **Add `ctxregress` to `package.json`** next to `baseline:project-memory`
   (`package.json:17`), so the disabled-vs-enabled gate is runnable the same way
   the baseline is.
5. **Only then** consider new *fact kinds* beyond `baseline-dispatch`,
   `file-usage` and `note` (`memory.go:47-60`) — the item vocabulary is
   explicitly open, so a new source needs no schema change.

### What P2-A must not do

- Must not add a second walk of the source tree. `ScanSource`
  (`estimate.go:97`) and the indexer's walk (`native.go:116`) are the two that
  exist; a third would be a third opinion about what a source file is.
- Must not copy review findings, prompt text, or transcript content into the
  memory store. The fix path deliberately stores a `review_run_id` reference and
  not the body (`fix_prompt.go:17-22`), and the context pack is explicitly
  forbidden from carrying chain-of-thought or transcript content
  (`session_context_pack.go:21-26`).
- Must not write memory anywhere but under `~/.ao`/`AO_DATA_DIR`. Both stores
  already refuse OS-default app-data locations (`codegraph/store.go:54-81`,
  `projectmemory/store.go:57-88`) per the hard rule in `AGENTS.md`.
- Must not let a memory miss degrade a dispatch. The established rule is that a
  failed evidence source costs only that source (`default.go:18-22`) and a
  routing failure returns the *original* request untouched
  (`wfrouter.go:106-110`).
- Must not present an unmeasured number as measured. `docs/project-memory-baseline.md:9-12`.

---

## 9. Related documents

| Doc | What it already covers |
|-----|------------------------|
| [`project-memory-baseline.md`](project-memory-baseline.md) | Evidence schema, the measured/estimated/unavailable rule, the harness |
| [`project-memory-store.md`](project-memory-store.md) | Item schema, provenance, idempotent upsert, staleness, baseline ingestion |
| [`code-graph.md`](code-graph.md) | Provider boundary, native indexer, hash-gated incremental updates, per-project isolation |
| [`context-router.md`](context-router.md) | Role budgets, section ordering, progressive retrieval, the feature flag |
| [`context-router-metrics.md`](context-router-metrics.md) | Potential/selected/reused/new sizes and the disabled-vs-enabled gate |

This audit does not restate them; it records what is **wired**, what is
**dormant**, and what P2-A is therefore allowed to build.
