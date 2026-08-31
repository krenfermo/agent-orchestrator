# P2 project-memory audit: existing context sources

**Purpose.** Trace every source of context AO assembles, or causes to be
assembled, for the Planner, Worker, Reviewer, Diagnostic Agent and the two
Repair Agents, and record what is rescanned per task, what is durable, and which
builders could consume project memory without being rewritten. This audit is the
basis for **P2-A extending the abstractions that already exist** instead of
introducing a second, parallel memory system.

**Revalidated against the code at P2-A implementation (2026-08-31).** See
[§0](#0-revalidation-status) for exactly what was re-checked, what was confirmed
unchanged, what P1 hardening had made stale, and what P2-A itself has since
changed. The rest of this document is the corrected text, not the original.

Scope: `backend/internal` as of this document. Per-component reference docs stay
authoritative for their own component — [code-graph.md](code-graph.md),
[context-router.md](context-router.md),
[context-router-metrics.md](context-router-metrics.md),
[project-memory-baseline.md](project-memory-baseline.md),
[project-memory-store.md](project-memory-store.md). This document is the
cross-cutting view none of them has.

## 0. Revalidation status

This audit was written before the P1 hardening that followed it, and before
P2-A. It has been re-checked against the current tree; this section records the
result so a reader can tell a confirmed claim from a corrected one.

**Confirmed still accurate.** The findings this audit exists for all hold:

- The AO-assembled vs agent-assembled distinction, and the specific fact that
  **Worker, Reviewer and both Repair Agents' harness file and git reads are not
  observed by `AO_PROJECT_MEMORY_BASELINE`** (§1). Re-verified directly: the
  worker, reviewer and fix dispatch wrappers still declare
  `Capabilities{ContextPayload: true}` only
  (`wfdispatch.go:58`, `:96`, `:145`), and the planner wrapper is still the only
  one declaring `FileReads` and calling `ObserveFileRead` (`:251`, `:268`).
  **This conclusion is unchanged and is not weakened by anything in P2-A.**
- The planner's hard-coded six-document list and three git probes, re-read in
  full on every planner call (`adapters/planner/command/context.go`).
- Both Repair Agents dispatch through the *worker* path, so the routed Spawner
  surface reaches them (§2.6, §5).
- `approved_head_recovery.go` is still the only git read whose answer — positive
  and negative — is checkpointed (`maxApprovedHeadSearchCommits = 500`).
- The `git log -n 20` per workspace observation, bounded at
  `maxObservedWorkspaceCommits = 20` in both workspace adapters.
- The Graphify/Grae determination in §4.1: still prose-only and still absent
  respectively. P2-A did not add an integration; see
  [project-memory.md §9](project-memory.md#grae--graphify).

**Corrected — stale after P1 hardening.** One claim, in §2.1:

- The audit described the plan-reuse drift check as `plannerContextDrift`
  (`workflow/plan_reuse.go:152-181`) comparing the rebuilt manifest to the
  stored one by **exact string equality**. P1 replaced that with
  `Coordinator.describePlanContextDrift`
  (`workflow/plan_revalidation.go:96`), which compares the manifest
  **itemwise**: structural fields, a path→SHA-256 set comparison of the
  documents, and HEAD/dirty as separately attributable differences. §2.1 below
  has been rewritten accordingly.
- **The cost finding survives the correction, and is if anything sharper.** The
  new comparison still calls `plannerContextBuilder.Build` in full
  (`plan_revalidation.go:116`) — six documents read from disk, three git
  subprocesses — in order to obtain digests it then compares. The stored
  manifest already holds those digests. So the rescan this audit identified is
  unchanged, and the code now demonstrably needs only the hashes.
- `assessPlanReuse` is still re-derived on demand and still reached from
  `AssessRecovery` (`recovery_assessment.go:84`), `resume_obligation.go:210,236`
  and the reuse/regenerate commands, so it is still the most frequently repeated
  scan in the system.

**Superseded by P2-A itself.** Two findings were true when written and have
since been acted on. They are marked inline where they appear:

- §4.2's central finding — *nothing writes to either durable store in
  production* — is now half-addressed. `internal/codegraph` still has no
  production writer. `internal/projectmemory` now has one, but it is a **new
  durable SQLite store** (migration 0144), not the JSON `Store` this audit
  inventoried; that JSON store still has no production writer and remains the
  Phase-0 measurement path.
- §5's surface-asymmetry table and §8's item 3 said the reviewer was observed
  but not routed. P2-A added `wfrouter.InstrumentReviewerLauncher`, which fills
  the previously producer-less `ReviewerLaunchRequest.SystemPrompt`. The fix and
  verify surfaces are still deliberately unrouted.

**Not re-verified line by line.** Every line/range citation in §2–§8 was correct
when written; the sample re-checked above is the load-bearing subset. A citation
that has drifted by a few lines does not change a finding, and the audit is not
re-numbered on every refactor.

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
second is by far the larger of the two, and AO can currently neither *supply*
nor *observe* it. **Worker, Reviewer and both Repair Agents' harness file and
git reads stay unobserved even with `AO_PROJECT_MEMORY_BASELINE` enabled**: the
dispatch wrappers for those roles declare only `ContextPayload` capability and
say so explicitly — "the provider process makes its own file reads and tool
calls, which this surface does not report"
(`internal/observe/projectmemory/wfdispatch/wfdispatch.go:51-66` worker,
`:91-104` reviewer, `:141-150` fix). The only wrapper that declares `FileReads`
and calls `ObserveFileRead` is the planner's (`wfdispatch.go:247-272`), and the
paths it reports are the planner documents *AO itself* assembled — category one,
not category two. So `internal/observe/projectmemory` measures AO-assembled
context only; agent-assembled context is invisible to it.

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
| `AGENTS.md`, `README.md`, `go.mod`, `package.json`, `docs/architecture.md`, `docs/STATUS.md` (48 KiB cap each) | `adapters/planner/command.ContextBuilder.Build` (`context.go:26-52`) | **Yes — full re-read from disk on every planner call, *and* on every plan-reuse assessment** (see the drift row below) | A **content-free** `PlannerContext` manifest is built and persisted by `Coordinator.GeneratePlan` (`workflow/master_coordinator.go:194-273`) into `workflow_plans.context_manifest_json` via `planStore.StartWorkflowPlanCommand` (`storage/sqlite/store/workflow_plan_store.go:60-64`). Durable, and genuinely *reused* — as a comparison artifact, not as a body cache. See below |
| Branch, HEAD SHA, dirty flag | same builder, three `git` subprocesses (`context.go:28-38`) | Yes, on both paths above | Recorded in the manifest |
| **Planner-context drift check** — rebuilds the whole context and compares it item by item against the stored manifest | `Coordinator.describePlanContextDrift` (`workflow/plan_revalidation.go:96`), called from `assessPlanReuse` (`plan_reuse.go:129`) | **Yes — the full six-document read plus three git probes runs again on every assessment** | Consumes the durable manifest as drift evidence; the rebuilt copy is discarded |
| Routed selection (optional) | `contextrouter/wfrouter.InstrumentPlanner` (`wfrouter.go:91-175`) | Yes | No — transient per dispatch |

The document list is hard-coded (`context.go:40`). There is no per-project
override, no hash-gated skip, and no reuse across runs of the same project: two
objectives created back to back re-read and re-hash the same six files. The
builder *does* compute a SHA-256 per document (`PlannerDocument.SHA256`) — the
mechanism a cache would need already exists and is simply never consulted.

The persisted manifest cannot close that gap by itself, and it is worth being
exact about why. `GeneratePlan` deliberately blanks every `Document.Content`
before marshalling, over a **copy** of the slice — the aliasing bug that shared
the backing array is incident wf-80dc9f12, which stripped the repository context
out of every planner call (`master_coordinator.go:239-255`). So the durable
manifest holds paths and hashes and no bodies.

**The manifest is not a body cache, but it is not inert either — it is a durable
comparison artifact, and it has two production consumers.** The distinction
matters for P2-A, so both are inventoried:

1. **`repoRootsFromContextManifest`** (`workflow/task_graph_wiring.go:22-36`) —
   recovers repository-root directory names from the manifest's paths for the
   task classifier. A pure read of already-captured facts, costing no IO.
2. **`Coordinator.describePlanContextDrift`**
   (`workflow/plan_revalidation.go:96`) — the manifest's load-bearing use.
   `assessPlanReuse` (`plan_reuse.go:129`) asks whether the project AO would
   plan against *today* is the project it planned against *then*.
   `describePlanContextDrift` answers by calling `plannerContextBuilder.Build`
   **again** and comparing **item by item**: the structural fields (version,
   project id, project path, branch), the documents as a path→SHA-256 set, and
   HEAD/dirty as separately attributable differences. Naming *what* moved is
   what separates a staleness AO can discharge from one it cannot. Unable to
   rebuild ⇒ `known=false` ⇒ `stale_revalidatable` / `unverifiable`, which
   routes to a person rather than guessing.

   *(P1 hardening replaced an earlier `plannerContextDrift` that compared the
   marshalled manifests by exact string equality. The itemised form is strictly
   better and does not change this audit's cost finding — see §0.)*

So the manifest **is** durable evidence that is really reused, and the reuse is
sound: comparing hashes rather than bodies is what lets AO prove context
stability cheaply *in storage*. What it does not save is the **read**. `Build`
has no hash-only mode, so the drift check re-reads all six documents in full,
with bodies, up to the 48 KiB cap each, hashes them, and throws the bodies away
— paying the entire scan cost to produce a comparison of digests the stored
manifest already holds.

Two consequences for P2-A. The manifest can never serve as a document *cache*
(no bodies are stored), yet its **stored hashes are exactly the gate that would
make the re-read unnecessary** — a `Build` variant that stats and hashes without
loading bodies, or that short-circuits on unchanged mtime/size, would make the
drift check nearly free. And this is not a rare path: `assessPlanReuse` is
re-derived on demand "precisely so it can never be stale"
(`plan_reuse.go:77-79`), reached from `AssessRecovery` — which writes nothing so
that "a poll, a page load and an operator's terminal can all ask freely"
(`recovery_assessment.go:75-85`) and is served over HTTP at
`httpd/controllers/workflow_recovery.go:211` and `httpd/controllers/workflow.go:1416`
— as well as from `resume_obligation.go:210,236` and the reuse/regenerate
commands (`plan_reuse.go:204,279,307,314`). **Every one of those reads spawns
three git subprocesses and six file reads.**

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
| **Prior-dependency task recap** — one `SessionContextPack` per already-completed dependency task, rendered fact-only (never that task's session or transcript) and appended to the child run's *objective*, so it flows through the existing `BuildPlanArtifact`/`BuildWorkStepPrompt` path with no second prompt-assembly surface | `workflow.priorTaskContextBlock` (`task_boundary.go:21-50`) over `BuildTaskCheckpointSummary` + `BuildSessionContextPack` + `RenderContextPackForRole`; called from `master_coordinator.go:1435-1441` at child-run creation | Rebuilt once per task boundary, from durable dependency-run detail (`GetRun`) — not per spawn | **Yes** — the objective text is persisted on the child run, and the first pack plus its `ContentHash()` are persisted with the `LifecycleReasonTaskBoundary` lifecycle decision (`master_coordinator.go:1475-1484`) |
| **Provider-failover switch note** — a Worker-role `SessionContextPack` appended to the agent-switch note handed to the replacement provider (never the failed provider's transcript), reusing 8H's bounded/idempotent `Note`→`UserNote` handoff | `workflow.(*Coordinator).ReportWorkStepProviderFailure` (`failover.go:172`, pack built at `:289-302`), from `planArtifactForRun` + `BuildTaskCheckpointSummary` + `BuildSessionContextPack` | Only on an automatic work-step provider failover | **Yes** — the pack and its hash are persisted in the lifecycle checkpoint (`failover.go:304`); the rendered note itself is bounded/fingerprinted by `agent_switching.go` |
| **Everything else** — repo conventions (`AGENTS.md`/`CLAUDE.md`), source, and **git history** (`git log`/`git show`/`git diff`, agent-initiated — see §3) | the harness itself, in the worktree | Per session, uncontrolled | No |

Three things matter here. First, the worker prompt *builder* is pure and
deterministic, which is what makes it restart-safe: `BuildWorkStepPromptWithSpec`
performs no IO and holds no project-specific knowledge of its own. Purity of the
builder is not emptiness of the prompt, though — its *input* varies, because
`promptForRun` rebuilds the prompt from `run.Objective` (`plan.go:184`), and at a
task boundary that objective already carries the prior-dependency recap. So the
AO-side evidence channels that vary per worker spawn are three, not two: the
system prompt's project-rules section, `IssueContext` (or the routed selection
that replaces it), and the run objective itself.

Second, the two `SessionContextPack` paths differ in *when* and *how* they reach
the Worker, and P2-A should not conflate them. The prior-dependency recap is
assembled once at child-run creation and then travels inside the ordinary
prompt, so the worker has it at spawn and it survives a restart along with the
objective. The failover switch note is assembled mid-session and delivered
outside the prompt entirely, as the `Note`→`UserNote` handoff to the replacement
provider. Both are built from checkpoint facts and persisted with their content
hash, and both are AO-assembled rather than harness-initiated repository
inspection. Together they are the existing proof that project-derived facts can
reach a Worker without editing a prompt builder — one by varying its input, one
by bypassing it (§8, item 6). Third, `AgentRulesFile` is the one repo file AO
re-reads per worker spawn, and it is read without a hash gate.

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
| The diff **and the commit history** under review | **the reviewer agent runs `git status`/`git diff`/`git log`/`git show` itself** — the prompt instructs it to, and the adapter's allowlist permits it (`adapters/reviewer/claudecode/claudecode.go:47-58`); see §3 for the full history audit | Per review, per cycle | No |

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
| Role-scoped context pack (**conditional**) | `RenderContextPackForRoleExcluding` (`session_context_pack.go:55`) over a `TaskCheckpointSummary`, minus the three fields the fix prompt already carries verbatim (`fixPromptDuplicateFields`, `:47-51`) | **Not every delivery**: built and prepended only when `DecideSessionLifecycle` returns `LifecycleCompact` *and* the plan artifact loads; otherwise the fix prompt is sent alone (`cascade.go:342-365`). When it is built, it reuses the already-gathered facts — **no second fetch** (`session_context_pack.go:11-19`) | Derived from durable checkpoint facts; the pack is persisted whole even though the prepended block is de-duplicated |

`RenderContextPackForRoleExcluding` is the closest thing AO has to a working
memory-injection pattern: one computed fact set, several role-scoped views,
explicit de-duplication against the prompt. It is worth copying rather than
reinventing. Note the gate, though: on a `LifecycleNewSession`/`LifecycleReuse`
decision, or when `planArtifactForRun` fails, the fix worker receives
`BuildFixPrompt`'s output *only*. Anything P2-A routes through this pack must
therefore either move the gate or carry its own fallback — the fix role is not
unconditionally reached today.

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
| *(not carried)* commit history | the same observation already holds `Commits` — a bounded 20-commit log (§3) — but `IncidentPackInput` has no field for it, so it is dropped before the pack is built | — | — |
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
| Everything else, including any git history it chooses to read | the worker path (§2.2) plus the harness in the worktree (§3) | Per spawn | — |

**(b) Incident Repair Agent (8P-E.18, `incident_repair.go`).**

| Source | Built by | Rescanned? | Durable? |
| --- | --- | --- | --- |
| Approved diagnosis: summary, what happened, why AO stopped, evidence list, approval reason | `buildIncidentRepairObjective` (`incident_repair.go:189-215`), passed to `CreateRun` at `:133` | Per repair | Objective persisted on the repair run; the diagnosis is durable |
| **Not** the context pack, the ledger, or the diagnostic session | stated explicitly at `incident_repair.go:182-188` | — | — |
| Everything else, including any git history it chooses to read | the worker path (§2.2) plus the harness in the worktree (§3) | Per spawn | — |

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

**AO lifecycle reads — every one of these is bookkeeping; none feeds any
role's context.** Each row names its consumer, its bound, when it runs, and
whether its result is kept.

| Call | Where | Consumer | Bound | Lifecycle | Result durable? |
| --- | --- | --- | --- | --- | --- |
| `git rev-list --max-count=<n> HEAD` | `workflow/approved_head_recovery.go:75-77`, behind the `CommitHistory` port with `execCommitHistory` as the default (`:63-88`) | Reconstructs the commit a durable **approved workspace fingerprint** was read at, so a review target is addressable as a real SHA | **Yes — `maxApprovedHeadSearchCommits = 500`** (`:106-112`); a target beyond the bound is reported unreconstructible rather than guessed | Runs from the approved-head resolution path, at most **once per approval question** | **Yes, and uniquely so.** The outcome is checkpointed under `approved_head_reconstructed` (`:98-105`) keyed by the fingerprint that was asked about. A success becomes the row every later read resolves through; a **failure is recorded too**, precisely so subsequent polls stop re-running the subprocess to re-derive the same "no" |
| `git log -n 20 --pretty=format:...` | `adapters/workspace/directbranch/workspace.go:376` and `adapters/workspace/gitworktree/workspace.go:778`, filling `ports.WorkspaceObservation.Commits` | `workflow/worker_signal_reconcile.go:297-303` counts `CommitsSinceDispatch` as worker-progress evidence; `workflow/work_adoption.go:361-366` records `<sha> <subject>` lines on the adoption record; `session_manager/agent_switching.go:1162` copies them onto a session fact | **Yes — `maxObservedWorkspaceCommits = 20`** (`directbranch:37`, `gitworktree:742`) | Runs on **every workspace observation**, so repeatedly per task | **Partly.** Transient in the observation itself; the subject lines that survive are the ones persisted on the work-adoption record |
| `git rev-list --merges --max-count=1 base..head` | `integration/git.go:225` (`HasMergeCommits`) | Integration merge detection | Yes — `--max-count=1` | Per integration attempt | Consumed by the integration coordinator, not stored as history |
| `git merge-base [--is-ancestor]` | `integration/git.go:197,211`; `worktree/git.go:180` | Ancestry/base resolution for integration and worktree lifecycle | Single answer | Per operation | No |
| `git branch --show-current`, `git rev-parse HEAD`, `git status --porcelain` | `adapters/planner/command/context.go:28-38` | Stamps the planner manifest; and, via `describePlanContextDrift`, decides plan reusability | N/A | Per planner call **and per plan-reuse assessment** — the latter reached from `AssessRecovery`, an HTTP read path a poll or page load may hit freely (§2.1) | Recorded in the manifest (paths/hashes/refs only, §2.1), which is then reused as durable drift evidence |
| `git diff --name-status --find-renames <base>` | `contextrouter.GitDiffSource.Changes` (`sources.go:57-78`) | The router's diff evidence — the one history-adjacent read that **does** become agent context | Name-status only, no patch bodies | Per routed request, and a request may run twice (compact, then expanded) | **No** |
| Porcelain status + HEAD observation | `incident_advisor.go:322-332` via `workspaceFacts` | Incident pack evidence for the Diagnostic Agent | Bounded inside the pack | Per incident | Observation transient |

Two things to carry into P2-A. First, **`approved_head_recovery.go` is the only
history read in AO whose answer is cached durably**, and it is the in-repo
precedent for the whole design: run git once, checkpoint the answer *including
the negative one*, never re-derive it. Second, the workspace `git log -n 20` is
the counter-example directly beside it — the same class of read, run on every
observation, with only a fragment of its result surviving.

Note also what is absent from the AO side: **no `git log`, `git blame`, or
`git show` output is ever assembled into a payload for the Planner, Worker,
Reviewer or either Repair Agent.** The router's name-status diff is the sole
git-derived input to any AO-assembled context.

**Agent-initiated history inspection — unmeasured context for the roles that
have it:**

| Role | Granted by | Bounded? | Durable? |
| --- | --- | --- | --- |
| Reviewer (path a and b) | reviewer adapter allowlists: `Bash(git log:*)`, `Bash(git diff:*)`, `Bash(git show:*)`, `Bash(git status:*)` — `adapters/reviewer/claudecode/claudecode.go:47-58`, and the equivalents in `copilot.go:18`, `kimchi.go:43`, `kilocode.go:108`, `opencode.go:80` | No | No |
| Decision resolver | `daemon/decision_resolver_launcher.go:40` | No | No |
| Worker / both Repair Agents | the harness's own tools; nothing in AO restricts or records them | No | No |

So: **git history reaches agent context only by the agent's own initiative, on
every task, entirely unbounded and unrecorded.** Every AO-side history read is
for AO's bookkeeping. Note the gap that leaves in the baseline: `RepeatedReads`
is the metric *shaped* to expose this repeated work, but it cannot see it. It is
computed only when a dispatch declares `FileReads`
(`observe/projectmemory/recorder.go:461-477`), which only the planner wrapper
does; for worker, reviewer and repair dispatches it is emitted as `Unavailable`
with the reason "this dispatch surface does not report the agent's file reads"
(`recorder.go:450-455`). So agent-initiated `git log`/`git diff`/`git show`
work is unrecorded whether or not the baseline recorder is on, and it is
precisely the kind of fact a durable memory item (`Type`, `Scope`,
`SourceCommit`, `Confidence`) is shaped to hold.

## 4. The three existing subsystems, and the explicit Graphify/Grae answer

| Package | What it is | Durability | Populated in production? |
| --- | --- | --- | --- |
| `internal/codegraph` | Provider-agnostic symbol/edge graph; native AST indexer; hash-gated incremental update (`FilesParsed`/`FilesSkipped`/`FilesRenamed` is its audit trail); per-project JSON graph at `<data dir>/codegraph/projects/<key>/graph.json` | **Durable** | **No** |
| `internal/projectmemory` | Durable per-project facts: `MemoryItem{Scope,Type,Content,Source,SourceCommit,Confidence,Stale,StaleReason,ContentHash}` (`memory.go:101-144`); content-hash idempotent upsert; staleness against HEAD/file hashes; `<data dir>/project-memory/items/projects/<key>/memory.json` | **Durable** | **No** |
| `internal/contextrouter` | Role-aware assembler: per-role section ordering (`router.go:30-35`), per-role token budgets, compact→expanded escalation, hard-cap enforcement, `AO_CONTEXT_ROUTER` flag (off by default) | Transient per dispatch | Only behind the flag |

`internal/observe/projectmemory` is a different thing despite the name: it is
the Phase-0 **measurement** recorder (`AO_PROJECT_MEMORY_BASELINE`), writing one
evidence file per dispatch with `FilesInspected`, `RepeatedReads`,
`SourceTokensAvailable`, `ContextSentBytes/Tokens` (`evidence.go:90-119`).
`RepeatedReads` is the metric the durable store is supposed to reduce — but it
is measured only on the planner dispatch, and reported as `Unavailable` on the
worker, reviewer and repair records (§1).

Store locations, precisely (`codegraph/store.go:26-51`,
`projectmemory/store.go:29-54`): both resolve `DataDir()` as `AO_DATA_DIR` when
set, otherwise **`~/.ao/data`** — *not* `~/.ao` — and both refuse OS-default
app-data locations via `forbiddenPathSegments`. So the default store roots are
`~/.ao/data/codegraph` and `~/.ao/data/project-memory/items`. Beneath each root,
`PathFor` interposes a literal `projects/` segment before the per-project key
(`codegraph/store.go:166-168`, `projectmemory/store.go:168-170`), so a project's
file resolves by default to:

- `~/.ao/data/codegraph/projects/<key>/graph.json`
- `~/.ao/data/project-memory/items/projects/<key>/memory.json`

The per-project directory is the whole of the multi-project isolation guarantee
in both stores: there is no shared file for two projects to leak entries
through. Project memory sits beside, not inside, the baseline evidence
directory, because evidence is a prunable input and memory is the durable
output (`projectmemory/store.go:18-21`).

### 4.1 Graphify / Grae: the explicit determination

**Search performed.** Reproducible as below. The documents that exist to
*record this determination* are excluded, so the counts do not change every time
one of them is edited — this audit, the docs index row that summarises it, and
(since P2-A) `docs/project-memory.md`, whose §9 states the same finding and
documents the port an adapter would implement:

```
EXCLUDE='docs/p2-project-memory-audit.md|docs/project-memory.md|docs/README.md'
grep -rIin 'graphify' --include='*.go' --include='*.ts' --include='*.tsx' \
  --include='*.md' --include='*.json' . \
  | grep -v node_modules | grep -vE "$EXCLUDE"
grep -rIinw 'grae'    --include='*.go' --include='*.ts' --include='*.tsx' \
  --include='*.md' --include='*.json' . \
  | grep -v node_modules | grep -vE "$EXCLUDE"
```

Re-run at P2-A: **unchanged**. Graphify still appears only in the three prose
locations below; Grae still has zero occurrences outside the recording
documents. P2-A added `projectmemory.MemoryGraph` as the port such an adapter
would implement, and added no adapter, no client, no dependency and no config
key.

| Name | Matching lines | What they are |
| --- | --- | --- |
| **Graphify** | 6 in 4 files | Pre-P2-A: `backend/internal/codegraph/codegraph.go:5` (package doc) and `:67` (a sample provider string in `Name()`'s comment), and `docs/code-graph.md:5`. Added by P2-A: `internal/projectmemory/graph.go:17` and `internal/domain/project_memory.go:38,655`. **Every one is prose naming it as an example** of a third-party tool that could implement a port |
| **Grae** | 7 in 4 files | All added by P2-A, and all of the same kind: `internal/projectmemory/graph.go:17,26,42,150` (the port's own doc comment, its "AO works even when Grae is unavailable" rule, its restatement of this determination, and `"grae"` as a sample value for `Name()`), `internal/domain/project_memory.go:38,655` (provider-neutrality notes), and `internal/projectmemory/pack_test.go:590`, a **test fixture that names an unavailable backend in order to prove the outage path works** |

**Both names are documentation, and neither is an integration.** The count grew
at P2-A and the determination did not, which is the distinction that matters:
every added occurrence is a doc comment on the new `MemoryGraph` port, a sample
value in a `Name()` comment, or a test fixture that deliberately names a backend
in order to prove the *unavailable* path works.

There is **no Grae or Graphify client, adapter, SDK dependency, module
requirement, config key, environment variable, endpoint or network call
anywhere in the repository.** Nothing would break, and nothing would be reused,
if both names were deleted tomorrow. `go.mod` gained no entry at P2-A on their
account, and no code path reaches either.

The honest form of the P2-A position: a *port* now exists that such an adapter
would implement, with a real, load-bearing local implementation behind it — see
[project-memory.md §9](project-memory.md#grae--graphify). A port is not an
integration, and this audit's original determination stands: there was nothing
to extend, and P2-A did not manufacture one.

**What *does* exist, and is a different thing from the above.** These are real,
in-tree, tested abstractions — the distinction the two rows above are meant to
draw is precisely that the boundary is shipped while Graphify is only mentioned:

| Abstraction | Where | Status |
| --- | --- | --- |
| `CodeGraphProvider` — provider-neutral boundary: `Name`, `Index`, `IncrementalUpdate`, `Query`, with documented invariants (queries are read-only and must never lazily rebuild; persisted state must live under the data dir) | `codegraph/codegraph.go:60-81` | **Shipped**, with a published contract |
| `NativeIndexer` (`ProviderNameNative = "native"`) — the in-tree implementation: Go-stdlib AST parsing, no external process or network, per-file and per-symbol hashing for incremental update | `codegraph/native.go:35-98` | **Shipped**, and the *only* implementation |
| `codegraph.Store` — per-project persisted graph with multirepo isolation | `codegraph/store.go` | **Shipped** |
| `projectmemory` — durable fact store with provenance, content-hash idempotent upsert, and commit/file-hash staleness | `projectmemory/` | **Shipped** |
| `contextrouter` — role-aware, budgeted assembler reading a diff source, a `GraphQuerier` and a `MemorySource` | `contextrouter/` | **Shipped**, behind `AO_CONTEXT_ROUTER` |

**Precisely which abstractions P2-A extends.** Three, and no new one:

1. **`codegraph.CodeGraphProvider` + `codegraph.Store`** — for indexing. A
   Graphify (or LSP-backed, or hosted) indexer is added as an *implementation of
   the existing interface*, alongside `NativeIndexer`. What P2-A must supply is
   the missing **producer**: a production caller of `Index`/`IncrementalUpdate`
   (§4.2).
2. **`projectmemory.Store`** — for durable facts. Its `MemoryItem` schema,
   identity/idempotency rules and staleness model are already what a memory
   system needs; `MemorySource` is satisfied by `*Store` as-is
   (`contextrouter/sources.go:36-40`). What is missing is a production
   **writer** and a caller of `RefreshStaleness`.
3. **`contextrouter` (+ `wfrouter`)** — for delivery. Role budgets and section
   ordering already exist for all five roles; what is missing is wrapping the
   reviewer and fix surfaces (§5).

Duplicating any of the three would mean two provider boundaries competing for
the same `Query`/`Index`/`IncrementalUpdate` contract, two per-project stores
under the same data dir, and two budgeted assemblers on the dispatch path — with
the second of each necessarily unmeasured by the existing baseline recorder and
the `ctxregress` harness. P2-A therefore adds **producers, providers and call
sites** to these packages, not replacements for them.

### 4.2 The finding that matters most: nothing writes to either durable store

> **Superseded in part by P2-A.** This finding was correct when written and is
> the reason P2-A exists. As of P2-A: `internal/codegraph` **still has no
> production writer**, so everything below about the graph stands unchanged.
> Project memory now does have one — but it is a **new durable SQLite store**
> (migration 0144, `store.PutProjectMemoryItem`, driven by
> `projectmemory.Indexer`), *not* the JSON `Store` inventoried here. That JSON
> store still has no production writer and remains the Phase-0 measurement
> path, deliberately: the baseline recorder has to keep measuring exactly what
> it measured before, or the before/after in
> [project-memory-baseline.md](project-memory-baseline.md) means nothing. See
> [project-memory.md](project-memory.md).

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
| `ReviewerLauncher` | yes | **yes, as of P2-A** (fills the previously producer-less `SystemPrompt`) |
| `MessageSender` (fix delivery) | yes | no |
| `Verifier` | yes | no |

The asymmetry was deliberate and pinned by test
(`TestInstrumentLeavesUnroutedSurfacesAlone` asserted the last three were handed
back identically). It was nonetheless the boundary P2-A had to cross: the
reviewer and the fix path are exactly the roles for which `roleSectionOrder`
already defines an ordering (`RoleReviewer`, `RoleFix`) that nothing could
reach.

**P2-A crossed half of it, and only half, on the distinction this audit drew.**
`wfrouter.InstrumentReviewerLauncher` now fills
`ReviewerLaunchRequest.SystemPrompt` — a field this audit found had *no producer
anywhere in the repository* (§2.3b). That is assembled context with nothing in
it, not an instruction being budgeted, which is precisely why it was safe to
route and why the fix and verify surfaces were left alone: those carry prompts,
and budgeting a prompt truncates instructions rather than evidence. The test
above was updated to assert the new, narrower asymmetry rather than left to pass
incidentally on a nil fixture.

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
- **The same six documents and three `git` calls again, per plan-reuse
  assessment** — `describePlanContextDrift` rebuilds the whole planner context
  only to compare digests the stored manifest already holds
  (`workflow/plan_reuse.go:152-181`). Because `assessPlanReuse` is deliberately
  re-derived on demand and is reached from the `AssessRecovery` HTTP read path,
  this is the most frequently repeated scan in the system: a poll or page load
  can trigger it (§2.1). It is also the one with the clearest fix — the manifest
  already stores the hashes that would make the re-read unnecessary.
- `Config.AgentRulesFile`, re-read from disk per worker spawn
  (`session_manager/prompt.go:133-142`).
- The whole worker/reviewer system prompt, rebuilt per launch — cheap, and
  deliberately not persisted (`manager.go:3237-3241`).
- `contextrouter.GitDiffSource` — one `git diff --name-status --find-renames`
  per routed request, and a routed request may run twice.
- The reviewer's own `git status`/`git diff`/`git log`, inside the agent, per
  review cycle (§3).
- `git log -n 20` on every workspace observation, in both workspace adapters
  (`directbranch/workspace.go:376`, `gitworktree/workspace.go:778`) — repeated
  per observation, with only the adopted subject lines kept (§3).
- The incident advisor's workspace observation, per incident.
- Everything the harness reads inside the worktree, per session — unbounded and
  unmeasured, and it stays unmeasured **even with the baseline recorder on**:
  the worker, reviewer and repair dispatch wrappers report only the payload AO
  sent, never the provider's own reads (§1).

**Durable / cacheable and reusable:**

- `codegraph` graphs — content-hash gated, so re-indexing an unchanged tree is
  cheap and reports `FilesSkipped`. Never populated (§4.2).
- `projectmemory` items — content-hash idempotent, commit-stamped, staleness-
  aware. Never populated (§4.2).
- Plan artifacts, checkpoints, review runs, repair intents, workspace
  fingerprints, work-adoption records — durable in SQLite, and already the
  sources the pure prompt builders read from.
- **The three `SessionContextPack` handoffs** — the task-boundary recap folded
  into a child run's objective (`task_boundary.go:21-50`), the provider-failover
  switch note (`failover.go:289-302`), and the conditional fix-worker pack
  (`cascade.go:342-365`). All three are *derived* from durable checkpoint facts,
  computed once per event rather than per spawn, and persisted with a
  `ContentHash()` on the lifecycle decision — so they are durable **and**
  reused, and they are the existing template for delivering project-derived
  facts without touching a pure prompt builder.
- **The content-free `PlannerContext` manifest**
  (`workflow_plans.context_manifest_json`) — durable *and genuinely reused*, as
  the comparison artifact `describePlanContextDrift` proves plan reusability
  against,
  and as the repo-root signal `repoRootsFromContextManifest` reads (§2.1). It is
  the closest thing AO already has to a durable context-stability cache: the
  comparison is cheap, only the re-derivation of its inputs is not.
- **The `approved_head_reconstructed` checkpoint** — the single existing example
  of a git scan whose answer, positive *and* negative, is written down so it is
  never re-run (`workflow/approved_head_recovery.go:98-112`). It is the model
  P2-A should follow for any scan it introduces.

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
3. **`ReviewerLauncher` — done in P2-A. `MessageSender` — deliberately not.**
   `roleSectionOrder` already defines `RoleReviewer` and `RoleFix`. For the
   reviewer the work was a `wfrouter` wrapper mirroring `InstrumentSpawner`,
   including its rule that an unresolvable checkout root means *send the
   original payload*, never a silently-thinner one; it fills the unused
   `SystemPrompt` field (`review_dispatch.go:267`) with standing project
   knowledge, which is exactly what this item proposed. The fix surface was
   left alone: it carries the specific correction, and there is no empty
   assembled-context field on it to fill.
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
6. **`BuildSessionContextPack` / `BuildTaskCheckpointSummary` — the pack
   builders, already consumed on three paths.** Memory would enter as
   additional `TaskCheckpointSummary`-derived facts, keeping the "one fact set,
   role-scoped views, explicit de-duplication" property intact, and would then
   reach all three existing consumers with no new surface: the fix worker via
   `RenderContextPackForRoleExcluding` (`cascade.go:342-365`, **but only on a
   `LifecycleCompact` decision** — §2.4), the Worker's prior-dependency recap
   (`task_boundary.go:21-50`), and the failover switch note
   (`failover.go:289-302`). The first is conditional and the third is
   event-driven, so neither is a substitute for the routed `IssueContext`
   channel (item 2); the second is the only one that fires on the normal
   task-boundary path.
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
3. **The gap is production writers, not readers.** *(Half closed by P2-A.)*
   Nothing indexed a real project and nothing upserted a real fact. P2-A adds a
   durable project-memory writer (SQLite, migration 0144) plus its indexer,
   incremental update, CLI and API, and wires it in as the router's memory
   evidence — so with `AO_CONTEXT_ROUTER` on, routing now has real memory to
   route. **`internal/codegraph` still has no production writer**, so graph
   evidence remains permanently unavailable and the router still degrades on it
   exactly as described in §4.2.
4. **Wire staleness with the first writer.** *(Done in P2-A, in the new
   store.)* `RefreshStaleness` on the JSON store still has no caller, and the
   JSON store still has no writer, so the pair remains consistent. The durable
   store ships invalidation with its writer from day one — per-path
   invalidation on every incremental pass, a generation retire sweep after a
   complete walk, and an independent drift detector — because a store that
   accumulates facts without ever invalidating them is worse than an empty one.
5. **Reach the Repair Agents through the worker spawn path, not the incident
   pack.** Both repairers are ordinary workflow runs; the pack belongs to the
   read-only Diagnostic Agent (§2.6).
6. **Extend `wfrouter` to the reviewer surface** rather than adding
   context-assembly code inside `workflow`. *(Done in P2-A; the fix surface was
   deliberately not extended — see §8 item 3.)* The role budgets already
   existed; the nil-router-is-a-no-op discipline is what made it safe to ship
   dark.
7. **Git history is the clearest duplicated work.** No AO-side history read
   feeds any role's context; every role that inspects history does so itself,
   per task, unbounded. `approved_head_recovery.go` is the in-repo precedent for
   caching such an answer durably.
8. **Everything stays behind its flag until measured.** The baseline recorder
   already covers all five surfaces, and `observe/ctxregress` already fails the
   build when routing changes an outcome — that harness is the acceptance gate
   for P2-A, and it needs no new infrastructure either.
