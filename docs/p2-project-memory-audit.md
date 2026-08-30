# P2 project-memory audit: existing context sources

**Purpose.** Trace every source of context AO assembles, or causes to be
assembled, for the Planner, Worker, Reviewer, Diagnostic Agent and the two
Repair Agents, and record what is rescanned per task, what is durable, and which
builders could consume project memory without being rewritten. This audit is the
basis for **P2-A extending the abstractions that already exist** instead of
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
  provider: the worker/reviewer system prompts, project `AgentRules`, the
  planner's document set, `SpawnConfig.IssueContext`, the worker prompt, the
  review prompt, the fix prompt, the incident pack, the repair objective.
- **Agent-assembled context** — bytes the coding harness reads on its own,
  inside the worktree, after launch: `AGENTS.md`/`CLAUDE.md` discovery,
  `git diff`, `git log`, `git status`, source files opened by the agent's own
  tools.

Only the first category is routable, budgetable, or memory-backed today. The
second is by far the larger of the two and AO can currently only *observe* it
(`internal/observe/projectmemory`), never *supply* it.

Specifically, on repository instruction files: **no AO code path reads
`AGENTS.md` or `CLAUDE.md` for the Worker, the Reviewer, or either Repair
Agent.** Those roles see them only because their harness discovers them in the
worktree. Grep confirms exactly one non-test Go call site that opens repository
instruction/documentation files for an AO-assembled payload —
`adapters/planner/command/context.go:40`, the planner. The AO-side analogue for
the worker is a *configured* rules channel, not a repo-file one: `AgentRules` /
`AgentRulesFile` (`domain/projectconfig.go:47-51`).

## 2. Per-role context inventory

### 2.1 Planner

| Source | Built by | Rescanned per run? | Durable? |
| --- | --- | --- | --- |
| `AGENTS.md`, `README.md`, `go.mod`, `package.json`, `docs/architecture.md`, `docs/STATUS.md` (48 KiB cap each) | `adapters/planner/command.ContextBuilder.Build` (`context.go:26-52`) | **Yes — full re-read from disk on every planner call** | The resulting `PlannerContext` manifest is persisted with the plan (`workflow/task_graph_wiring.go:26`), so it is durable *as a record*, never reused *as a cache* |
| Branch, HEAD SHA, dirty flag | same builder, three `git` subprocesses (`context.go:28-38`) | Yes | Recorded in the manifest |
| Routed selection (optional) | `contextrouter/wfrouter.InstrumentPlanner` (`wfrouter.go:91-175`) | Yes | No — transient per dispatch |

The document list is hard-coded (`context.go:40`). There is no per-project
override, no hash-gated skip, and no reuse across runs of the same project: two
objectives created back to back re-read and re-hash the same six files. The
builder *does* compute a SHA-256 per document (`PlannerDocument.SHA256`) — the
mechanism a cache would need already exists and is simply never consulted.

### 2.2 Worker

| Source | Built by | Rescanned per task? | Durable? |
| --- | --- | --- | --- |
| Standing worker system prompt: role block, orchestrator-coordination block, multi-PR branch convention (omitted in direct-branch mode), container-label block, confidentiality guard | `session_manager/prompt.go` — `buildSystemPromptText` (`:76-112`), `workerSystemPrompt` (`:232`), `directBranchGitRules` (`:312`), `workerOrchestratorPrompt` (`:335`), `workerMultiPRPrompt` (`:347`), `workerContainerLabelPrompt` (`:363`), `systemPromptGuard` (`:117`) | Rebuilt per spawn; **deliberately not persisted** so a restored worker points at the orchestrator active *now* (`manager.go:3237-3241`) | No — derived from durable project/session rows |
| **Project rules**: `Config.AgentRules` (inline) + `Config.AgentRulesFile` (a repo-relative file, read from disk) | `buildProjectRules` (`prompt.go:128-147`) via `manager.go:3264-3271`; path validated repo-relative by `projectRelativeFile` (`prompt.go:149`) | **Yes — `AgentRulesFile` is re-read from disk on every worker spawn**; an unreadable file fails the spawn rather than silently dropping rules | The *config* is durable (`domain/projectconfig.go:47-51`); the rendered text is not |
| Project context section (name, path, branch mode) | `projectContextSection` (`prompt.go:373`) | Per spawn | From durable project rows |
| Workspace project prompt, `ao` skill pointer | `manager.go:3275-3283`, `aoSkillPointer` (`manager.go:3295`) via `skillassets.Dir` | Per spawn | Pointer to a file under the data dir |
| Task prompt (objective + acceptance criteria + guardrails, ± read-only variant, ± effective spec) | `workflow.BuildWorkStepPromptWithSpec` (`plan.go:121`) — pure, no IO | N/A (template expansion) | Persisted in `workflow_steps.artifact_json`; rebuilt byte-identically after a restart via `promptForRun` (`plan.go:175`) |
| `SpawnConfig.IssueContext` — pre-fetched tracker issue body, wrapped in an explicit trust boundary | tracker adapters; framed by `issueContextSection` / `issueContextTrustBoundary` (`prompt.go:169-173`) and folded into the task prompt by `buildTaskPrompt` (`prompt.go:53-75`) | Per spawn | No |
| Routed selection replacing `IssueContext` (optional) | `contextrouter/wfrouter.InstrumentSpawner` (`wfrouter.go:186-247`) | Per spawn | No |
| **Everything else** — repo conventions (`AGENTS.md`/`CLAUDE.md`), source, git history | the harness itself, in the worktree | Per session, uncontrolled | No |

Two things matter here. First, the worker prompt itself is pure and
deterministic, which is what makes it restart-safe — and also why AO passes it
no project-specific knowledge: the only *variable* AO-side evidence channels are
the system prompt's project-rules section and `IssueContext`. Second,
`AgentRulesFile` is the one repo file AO re-reads per worker spawn, and it is
read without a hash gate.

### 2.3 Reviewer

There are two reviewer launch paths with **different** context assembly, and
conflating them is easy:

**(a) `internal/review` — session/PR-driven reviews.**

| Source | Built by | Rescanned? | Durable? |
| --- | --- | --- | --- |
| Standing reviewer role: review scope, untrusted-evidence rule, read-only rule | `review/prompt.go` `reviewSystemPrompt` (`:44`), authored centrally in one place | Per launch | No |
| Per-pass task: PR queue, `gh api` posting recipe, `ao review submit` form | `reviewTexts` (`review/prompt.go:17-50`), `reviewQueueText` (`:54`) | Per launch | No |
| Previous review runs for the worker session | `previousReviewHistoryText` (`review/launcher.go:395`) | Per restore | From durable `review_run` rows |
| File-based delivery: `SystemPromptFile`, `TaskPromptFile`/`TaskPromptRoot`, with `Prompt` reduced to a short file reference | `agentLauncher.prepareInvocation` (`review/launcher.go:323-393`); fields on `ports.ReviewInvocation` (`ports/reviewer.go:111-126`) | Per launch, written under the data dir | Files under the data dir; keeps instructions out of the shared PTY |

**(b) `workflow` — the 8C worktree review.**

| Source | Built by | Rescanned? | Durable? |
| --- | --- | --- | --- |
| Objective, acceptance criteria, effective spec, worker session id, branch, worktree path, base SHA, workspace fingerprint, review run id, available dependencies, future planned tasks | `workflow.BuildReviewPrompt` (`review_prompt.go:53`), fed by `review_dispatch.go:1434-1447` | N/A — pure, no IO | Inputs come from durable rows (plan artifact, task graph, workspace fingerprints) |
| Standing reviewer role | **none** — `ReviewerLaunchRequest.SystemPrompt` (`review_dispatch.go:267`) exists but has no producer in `workflow`, so `ports.ReviewInvocation.SystemPrompt` is passed through empty at `daemon/workflow_reviewer_launcher.go:335` | — | — |
| The diff under review | **the reviewer agent runs `git status`/`git diff`/`git log` itself** — the prompt instructs it to, and the adapter's allowlist permits it (`adapters/reviewer/claudecode/claudecode.go:47-58`) | Per review, per cycle | No |

`BuildReviewPrompt` documents why it does not reuse `internal/review`'s
PR-centric prompt (`review_prompt.go:34-52`; the reciprocal note is at
`review_dispatch.go:285-300`). The relevant facts for this audit are what path
(b) *omits*: no standing reviewer system prompt, and no project knowledge at
all — the reviewer is told *where* to look and never *what is already known*
about the project. **Neither reviewer path is routed** — see §5.

### 2.4 Fix (same session as the worker)

| Source | Built by | Rescanned? | Durable? |
| --- | --- | --- | --- |
| Objective, criteria, effective spec, cycle number | `workflow.BuildFixPrompt` (`fix_prompt.go:29`) — pure, no IO | N/A | From durable rows |
| Reviewer findings body | fetched live from the `ReviewRuns` port at dispatch (`fix_dispatch.go`); referenced by id, never copied into a workflow table | Per cycle | The review run is durable; the fix prompt is not stored |
| Role-scoped context pack | `RenderContextPackForRoleExcluding` (`session_context_pack.go:55`) over a `TaskCheckpointSummary`, minus the three fields the fix prompt already carries verbatim (`fixPromptDuplicateFields`, `:47-51`) | Per delivery, **no second fetch** (`:11-19`) | Derived from durable checkpoint facts |

`RenderContextPackForRoleExcluding` is the closest thing AO has to a working
memory-injection pattern: one computed fact set, several role-scoped views,
explicit de-duplication against the prompt. It is worth copying rather than
reinventing.

### 2.5 Diagnostic Agent (read-only) — *not* a Repair Agent

The `IncidentContextPack` is given to the **Diagnostic Agent**, which is
launched read-only and explicitly forbidden to change anything
(`incident_prompt.go:11-30`, `BuildIncidentDiagnosticPrompt` at `:24`, launched
from `incident_advisor.go:434`). It is inventoried here, separately from the
repairers, because the two are frequently conflated and their extension points
are different.

| Source | Built by | Rescanned? | Durable? |
| --- | --- | --- | --- |
| Run detail, step states, checkpoints, stop reason/detail, signature | `BuildIncidentContextPack` (`incident_context_pack.go:188`) from `IncidentPackInput` (`:113`) | Per incident | All from durable workflow rows |
| Branch, HEAD, porcelain status, bounded diff, worktree path | `incident_advisor.go:322-332` via `workspaceFacts` observation | Per incident | Observation is transient |
| Session facts (harness, activity, last-activity, terminated) | `sessionFacts.GetSession` (`incident_advisor.go:310-320`) | Per incident | From durable session rows |
| Newest reviewer verdict, newest verify output, provider notes | `attachIncidentReviewFacts` and siblings | Per incident | Durable sources |
| `EvidenceSnapshot`, `ChildEvidence` | `evidence_snapshot.go`, `incident_child_pack.go:79` | Per incident | Rendered from durable facts |

`IncidentPackInput`'s doc comment states the governing rule explicitly
(`incident_context_pack.go:108-113`): the builder performs **no I/O** and
**never walks the source tree** — the only repository-derived facts are ones AO
had already observed. The pack then applies a byte budget by priority, marking
every section `Truncated`/`Dropped` and recording `DroppedSections` and a
`Digest`.

This is a second, independently-written budgeted context assembler, parallel to
`contextrouter`'s. See §6.

### 2.6 The two Repair Agents

Neither repairer receives the incident pack. **Both are ordinary workflow
runs**: AO writes an objective and acceptance criteria, calls `CreateRun`, and
from that point the repairer is dispatched through the *worker* path in §2.2 —
plan artifact, `BuildWorkStepPrompt`, `Spawner`. This is the single most
important correction in this audit, because it moves the extension point.

**(a) Recovery Repair Agent (P1-B, `repair_agent.go`).**

| Source | Built by | Rescanned? | Durable? |
| --- | --- | --- | --- |
| Stopped run id, condition reason, affected step, generation/budget, `plan.Reason` ("what AO knows about the stop") | `buildRepairObjective` (`repair_agent.go:526`) from a `RepairIntent` + `RepairPlan` (`planRepairFor`, `:191`) | Per repair generation | Objective persisted on the created run; intents durable |
| Acceptance criteria, written by AO from the condition and explicitly **not** by the agent being judged | `repairAcceptanceCriteria` (`repair_agent.go:291`) | N/A | Persisted with the run |
| Everything else | the worker path (§2.2) plus the harness in the worktree | Per spawn | — |

**(b) Incident Repair Agent (8P-E.18, `incident_repair.go`).**

| Source | Built by | Rescanned? | Durable? |
| --- | --- | --- | --- |
| Approved diagnosis: summary, what happened, why AO stopped, evidence list, approval reason | `buildIncidentRepairObjective` (`incident_repair.go:189-215`), passed to `CreateRun` at `:133` | Per repair | Objective persisted on the repair run; the diagnosis is durable |
| **Not** the context pack, the ledger, or the diagnostic session | stated explicitly at `incident_repair.go:182-188` | — | — |
| Everything else | the worker path (§2.2) plus the harness in the worktree | Per spawn | — |

Note also that `BuildIncidentRepairPrompt` (`incident_prompt.go:87-133`) — whose
doc comment likewise says "it carries the diagnosis rather than the pack" — has
**no call site anywhere in the repository, tests included**. The shipped path is
`buildIncidentRepairObjective` → `CreateRun`. Any P2-A change aimed at the
repairer must target the objective builder and the worker dispatch path, not
that function and not the pack.

## 3. Git and history usage, audited concretely

Finding-driven distinction: **AO lifecycle history reads** (AO runs git for its
own bookkeeping; the output never becomes agent context) versus
**agent-initiated history inspection** (the agent runs git inside the worktree;
the output is context AO neither sizes nor sees).

**AO lifecycle reads — none of these feed any role's context:**

| Call | Where | Purpose | Bounded? | Durable? |
| --- | --- | --- | --- | --- |
| `git rev-list --max-count=<n> HEAD` | `workflow/approved_head_recovery.go:75-77`, `CommitHistory`/`execCommitHistory` (`:63-88`) | Reconstruct the commit an approved fingerprint was read at | Yes — `maxApprovedHeadSearchCommits = 500` (`:106-112`) | **Yes** — the outcome is checkpointed under `approved_head_reconstructed` (`:98-105`) so success becomes the row every later read resolves through and failure stops re-running the subprocess |
| `git rev-list --merges --max-count=1 base..head` | `integration/git.go:225` (`HasMergeCommits`) | Integration merge detection | Yes | Consumed by the integration coordinator |
| `git branch --show-current`, `git rev-parse HEAD`, `git status --porcelain` | `adapters/planner/command/context.go:28-38` | Stamp the planner manifest | N/A | Recorded in the manifest |
| `git diff --name-status --find-renames <base>` | `contextrouter.GitDiffSource.Changes` (`sources.go:57-78`) | The router's diff evidence | Name-status only, no patch bodies | **No** — re-run per routed request, and a request may run twice (compact, then expanded) |
| Porcelain status + HEAD observation | `incident_advisor.go:322-332` via `workspaceFacts` | Incident pack evidence | Bounded in the pack | Observation transient |

`approved_head_recovery.go` is the only history read in AO with a durable
result-cache, and it is worth naming as the in-repo precedent for "run git once,
checkpoint the answer, never re-derive it".

**Agent-initiated history inspection — unmeasured context for the roles that
have it:**

| Role | Granted by | Bounded? | Durable? |
| --- | --- | --- | --- |
| Reviewer (path a and b) | reviewer adapter allowlists: `Bash(git log:*)`, `Bash(git diff:*)`, `Bash(git show:*)`, `Bash(git status:*)` — `adapters/reviewer/claudecode/claudecode.go:47-58`, and the equivalents in `copilot.go:18`, `kimchi.go:43`, `kilocode.go:108`, `opencode.go:80` | No | No |
| Decision resolver | `daemon/decision_resolver_launcher.go:40` | No | No |
| Worker / both Repair Agents | the harness's own tools; nothing in AO restricts or records them | No | No |

So: **git history reaches agent context only by the agent's own initiative, on
every task, entirely unbounded and unrecorded.** Every AO-side history read is
for AO's bookkeeping. This is precisely the repeated-work `RepeatedReads` is
meant to expose, and precisely the kind of fact a durable memory item
(`Type`, `Scope`, `SourceCommit`, `Confidence`) is shaped to hold.

## 4. The three existing subsystems, and the explicit Graphify/Grae answer

| Package | What it is | Durability | Populated in production? |
| --- | --- | --- | --- |
| `internal/codegraph` | Provider-agnostic symbol/edge graph; native AST indexer; hash-gated incremental update (`FilesParsed`/`FilesSkipped`/`FilesRenamed` is its audit trail); per-project JSON graph at `<data dir>/codegraph/<key>/graph.json` | **Durable** | **No** |
| `internal/projectmemory` | Durable per-project facts: `MemoryItem{Scope,Type,Content,Source,SourceCommit,Confidence,Stale,StaleReason,ContentHash}` (`memory.go:101-144`); content-hash idempotent upsert; staleness against HEAD/file hashes; `<data dir>/project-memory/items/<key>/memory.json` | **Durable** | **No** |
| `internal/contextrouter` | Role-aware assembler: per-role section ordering (`router.go:30-35`), per-role token budgets, compact→expanded escalation, hard-cap enforcement, `AO_CONTEXT_ROUTER` flag (off by default) | Transient per dispatch | Only behind the flag |

`internal/observe/projectmemory` is a different thing despite the name: it is
the Phase-0 **measurement** recorder (`AO_PROJECT_MEMORY_BASELINE`), writing one
evidence file per dispatch with `FilesInspected`, `RepeatedReads`,
`SourceTokensAvailable`, `ContextSentBytes/Tokens` (`evidence.go:90-119`).
`RepeatedReads` is the metric the durable store is supposed to reduce.

Store locations, precisely (`codegraph/store.go:26-51`,
`projectmemory/store.go:29-54`): both resolve `DataDir()` as `AO_DATA_DIR` when
set, otherwise **`~/.ao/data`** — *not* `~/.ao` — and both refuse OS-default
app-data locations via `forbiddenPathSegments`. So the default roots are
`~/.ao/data/codegraph` and `~/.ao/data/project-memory/items`, with a per-project
directory and a single `graph.json` / `memory.json` beneath each. Project memory
sits beside, not inside, the baseline evidence directory, because evidence is a
prunable input and memory is the durable output (`projectmemory/store.go:18-21`).

### 4.1 Graphify / Grae: what was and was not found

- **Graphify — found as a named integration target, not as an implementation.**
  `internal/codegraph` exists specifically so AO is never hard-wired to one
  graph tool: `CodeGraphProvider` (`codegraph.go:66-81`) publishes three verbs
  (index a project, apply a git diff, ask a question) and its package doc names
  Graphify as an example of a third-party tool that "can be plugged in by
  implementing `CodeGraphProvider`" (`codegraph.go:1-14`, restated in
  [code-graph.md](code-graph.md)). `Name()`'s own comment gives `"graphify"` as
  a sample provider name. **No Graphify client, adapter, dependency, config key
  or network call exists in the repository.** `NativeIndexer`
  (`ProviderNameNative = "native"`) is the only implementation.
- **Grae — not found.** A repository-wide search across Go, TypeScript, Markdown
  and JSON (excluding `node_modules`) returns **zero** matches for "Grae" in any
  casing. There is no Grae implementation, adapter, reference, vendored module
  or documentation note to extend or to conflict with.
- **Therefore P2-A extends, and does not duplicate.** The provider boundary that
  a Graphify integration would attach to already exists, is tested, and is
  already consumed read-only by the router
  (`contextrouter.GraphQuerier`, `sources.go:25-32`, narrowed deliberately so
  routing can never index as a side effect). Adding a second graph or indexing
  abstraction would mean two provider boundaries competing for the same
  `Query`/`Index`/`IncrementalUpdate` contract and two per-project stores under
  the data dir. The same reasoning applies to `projectmemory` (the durable fact
  store, whose `MemorySource` interface is satisfied as-is by `*Store`) and to
  `contextrouter` (the role-aware assembler): P2-A adds **producers and call
  sites** to these three packages, not replacements for them.

### 4.2 The finding that matters most: nothing writes to either durable store

Outside tests, the only non-test call sites of `codegraph.Index` /
`NewNativeIndexer` and of `projectmemory.Store.Upsert` /
`BaselineReader.Ingest` are:

- `contextrouter/default.go:33-42` — constructs both as **readers**.
- `observe/ctxregress/fixture.go:348-364` — the regression **harness**, which
  indexes a fixture repo and upserts fixture memory.

Consequences, in production, today:

1. No daemon path, CLI command, HTTP route, or workspace watcher ever indexes a
   real project. Every `Graph` query from the router therefore returns
   `ErrNotIndexed`, which the router degrades on gracefully (`router.go:453-475`)
   — it emits a note and routes without graph evidence. Graph evidence is, in
   effect, permanently unavailable.
2. No production path ever writes a `MemoryItem`. `Store.List` returns empty, so
   the router's memory section is always empty.
3. `Store.RefreshStaleness` (`stale.go:191`) has no production caller, so no
   fact is ever invalidated — currently vacuous, and a live hazard the moment
   writes start.

**This, not the absence of abstractions, is the actual gap.** The storage
schema, the identity/idempotency rules, the staleness model, the path isolation
under the data dir and the read-side wiring all exist and are tested.

## 5. Routed vs merely observed: the surface asymmetry

`daemon/workflow_wiring.go:325` and `:333` wrap the dependency set twice.

| Dispatch surface | Baseline observer (`wfdispatch.Instrument`) | Context router (`wfrouter.Instrument`) |
| --- | --- | --- |
| `Planner` | yes | **yes** (routes `PlannerContext.Documents`) |
| `Spawner` (worker, and therefore both Repair Agents) | yes | **yes** (routes `SpawnConfig.IssueContext`) |
| `ReviewerLauncher` | yes | no |
| `MessageSender` (fix delivery) | yes | no |
| `Verifier` | yes | no |

The asymmetry is deliberate and pinned by test
(`contextrouter/wfrouter/wfrouter_test.go:382-390` asserts the last three are
handed back identically). It is nonetheless the boundary P2-A has to cross: the
reviewer and the fix path are exactly the roles for which `roleSectionOrder`
already defines an ordering (`RoleReviewer`, `RoleFix`) that nothing can
currently reach.

Both wrappers are strictly opt-in and both default to off:
`AO_PROJECT_MEMORY_BASELINE` (unset ⇒ nil recorder ⇒ untouched deps) and
`AO_CONTEXT_ROUTER` (unset ⇒ nil router ⇒ untouched deps, `flag.go:7-16`).

Note the leverage this table gives P2-A: because both Repair Agents dispatch
through `Spawner` (§2.6), **the already-routed worker path is also the repairer
path.** No new surface is needed to reach them.

## 6. Rescanned per task vs durable/cacheable today

**Rescanned every time (no cache consulted):**

- The planner's six documents and three `git` calls, per planner invocation
  (`adapters/planner/command/context.go`).
- `Config.AgentRulesFile`, re-read from disk per worker spawn
  (`session_manager/prompt.go:133-142`).
- The whole worker/reviewer system prompt, rebuilt per launch — cheap, and
  deliberately not persisted (`manager.go:3237-3241`).
- `contextrouter.GitDiffSource` — one `git diff --name-status --find-renames`
  per routed request, and a routed request may run twice.
- The reviewer's own `git status`/`git diff`/`git log`, inside the agent, per
  review cycle (§3).
- The incident advisor's workspace observation, per incident.
- Everything the harness reads inside the worktree, per session — unbounded and
  unmeasured unless the baseline recorder is on.

**Durable / cacheable and reusable:**

- `codegraph` graphs — content-hash gated, so re-indexing an unchanged tree is
  cheap and reports `FilesSkipped`. Never populated (§4.2).
- `projectmemory` items — content-hash idempotent, commit-stamped, staleness-
  aware. Never populated (§4.2).
- Plan artifacts, `PlannerContext` manifests, checkpoints, review runs, repair
  intents, workspace fingerprints, the `approved_head_reconstructed` checkpoint
  — durable in SQLite, and already the sources the pure prompt builders read
  from.

**Durable but not context:** the baseline evidence files are an audit trail, not
a retrieval source; nothing reads them back at dispatch time except the
regression harness.

**Why this justifies the later steps.** Every item in the first list is work
repeated per task whose result is stable across tasks (repository conventions,
symbol locations, where a subsystem lives). Every item in the second list is a
place AO already proved it can persist a derived fact with provenance and
invalidate it. P2-A therefore needs producers that move facts from the first
list into the second, plus call sites that read them back — not a new store.

## 7. Two budgeted assemblers, not one

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
written. If memory should ever reach the **Diagnostic Agent**, the right move is
a memory section added to `IncidentPackInput` as a plain field filled by the
caller, preserving the no-I/O rule. That change would **not** reach either
Repair Agent (§2.6).

## 8. Which builders could consume memory without a rewrite

Ordered by cost. "No rewrite" means: no change to the builder's signature
contract, no new storage code, no violation of its stated invariants.

1. **`contextrouter` — already consumes it.** `MemorySource.List`
   (`sources.go:36-40`) is satisfied as-is by `*projectmemory.Store`;
   `gatherMemory` (`router.go:553`) ranks by `memoryScore(item, touched, role)`
   and already skips stale items. Cost of making it real: **write a producer**,
   not a consumer.
2. **The `Spawner` path — already routed, and it is also the repairer path.**
   Worker, recovery Repair Agent and incident Repair Agent all dispatch through
   `InstrumentSpawner`, so memory delivered into the routed `IssueContext`
   reaches all three with no new surface. This supersedes any notion of feeding
   the repairers through the incident pack.
3. **`ReviewerLauncher` and `MessageSender`.** `roleSectionOrder` already
   defines `RoleReviewer` and `RoleFix`. The work is a `wfrouter` wrapper per
   surface, mirroring `InstrumentSpawner` — including its rule that an
   unresolvable checkout root means *send the original payload*, never a
   silently-thinner one (`wfrouter.go:255-280`). Both surfaces are already
   observed by the baseline, so the before/after is measurable on day one. The
   workflow reviewer additionally has an unused `SystemPrompt` field
   (`review_dispatch.go:267`) available for standing project knowledge.
4. **`session_manager.buildProjectRules` / `buildSystemPromptText`.** Project
   rules are already a first-class, config-driven section of every worker system
   prompt. Durable memory items scoped to a project are the same shape of thing
   and would enter as an additional section, with no change to the prompt
   contract. This is also the cheapest place to stop re-reading `AgentRulesFile`
   from disk on every spawn.
5. **`adapters/planner/command.ContextBuilder`.** It already hashes every
   document. Adding memory means appending `PlannerDocument`-shaped entries, or
   better, letting the existing planner routing wrapper inject them so the
   builder keeps its single job. Its hard-coded six-file list is the other
   cheapest rescan to eliminate.
6. **`workflow.BuildFixPrompt` + `RenderContextPackForRoleExcluding`.** Memory
   would enter as additional `TaskCheckpointSummary`-derived facts, keeping the
   "one fact set, role-scoped views, explicit de-duplication" property intact.
7. **`BuildIncidentContextPack`.** Via a new plain input field only, and
   understanding that it reaches the Diagnostic Agent alone (§7).
8. **`BuildWorkStepPrompt`.** Last, and possibly never. Its purity is what makes
   the worker prompt restart-reproducible (`promptForRun` rebuilds it
   byte-identically). Memory belongs in the worker's `IssueContext` channel,
   which is already routed, not in the prompt.

## 9. Conclusions

1. **Do not build a second memory system.** `projectmemory` (durable facts,
   provenance, idempotent upsert, staleness) and `codegraph` (durable,
   incremental, per-project, provider-neutral) are the abstractions P2-A
   extends. The router already reads both.
2. **Graphify has a boundary and no implementation; Grae does not exist in this
   repository.** `CodeGraphProvider` is the extension point a Graphify (or LSP,
   or hosted) indexer would implement. P2-A adds providers and producers behind
   that boundary rather than a parallel one (§4.1).
3. **The gap is production writers, not readers.** Nothing indexes a real
   project and nothing upserts a real fact. Until that changes, enabling
   `AO_CONTEXT_ROUTER` routes on diff and documents alone.
4. **Wire staleness with the first writer.** `RefreshStaleness` exists, is
   tested, and has no caller. A store that accumulates facts without ever
   invalidating them is worse than an empty one.
5. **Reach the Repair Agents through the worker spawn path, not the incident
   pack.** Both repairers are ordinary workflow runs; the pack belongs to the
   read-only Diagnostic Agent (§2.6).
6. **Extend `wfrouter` to the reviewer and fix surfaces** rather than adding
   context-assembly code inside `workflow`. The role budgets already exist; the
   nil-router-is-a-no-op discipline is what makes that safe to ship dark.
7. **Git history is the clearest duplicated work.** No AO-side history read
   feeds any role's context; every role that inspects history does so itself,
   per task, unbounded. `approved_head_recovery.go` is the in-repo precedent for
   caching such an answer durably.
8. **Everything stays behind its flag until measured.** The baseline recorder
   already covers all five surfaces, and `observe/ctxregress` already fails the
   build when routing changes an outcome — that harness is the acceptance gate
   for P2-A, and it needs no new infrastructure either.
