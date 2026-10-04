# Canonical workflow lifecycle mapping

**Status:** contract document. This is the agreed mapping from persisted
backend state to the UI lifecycle vocabulary. The backend projection (s2) and
the frontend UI (s4) both derive from this file; if a mapping here is wrong,
fix it here first, then the code.

**Scope:** the existing workflow engine as it stands on
`feat/engineering-control-center` — tables `workflow_runs`, `workflow_steps`,
`workflow_attempts`, `workflow_plans`, `workflow_checkpoints`,
`workflow_wake_schedules`, plus the supporting tables they reference
(`workflow_tasks`, `workflow_task_dependencies`, `workflow_outbox`,
`workflow_questions`, `workflow_question_resolutions`, `branch_locks`,
`review_run`, `sessions`, `post_run_qa_runs`).

Everything below is a statement about durable rows. Consistent with AO's
"persist durable facts, derive display status" rule (`docs/architecture.md`,
`AGENTS.md`), **none of the 13 UI lifecycle labels is a stored column value**.
`workflow_runs.state` has only seven values; the rest of the vocabulary is
derived at read time from steps, plans, tasks, wakes, questions and locks.

---

## 1. Duplicate / parallel workflow-run tables: audit and decision

**Finding: a duplicate `workflow_runs` table did exist in prior discarded work.
It is not present on this branch, and no consolidation work is required.**

Evidence:

| Fact | Value |
| --- | --- |
| Discarded commit | `ac5a6a9da` — `feat(api): expose project board with autonomous runs and interactive sessions` |
| What it added | `backend/internal/storage/sqlite/migrations/0094_workflow_runs.sql` (a **second** table named `workflow_runs`, plus `workflow_run_tasks`), `queries/workflow_runs.sql`, `gen/workflow_runs.sql.go`, `store/workflow_run_store.go`, and a parallel `domain/workflow.go` run type |
| Migration-number collision | Yes — it claimed `0094`, the number already taken by `0094_workflow.sql` (the canonical Checkpoint 8A foundation) |
| Reachable from `HEAD`? | **No.** `git merge-base --is-ancestor ac5a6a9da HEAD` → false; no branch contains it |
| Where it survives | Only under the annotated tag `wip/8p-e9-superseded-duplicate-tables` |
| Leftovers on this branch | **None.** `ls migrations/ \| grep 0094` returns exactly one file; there is no `workflow_run_store.go`, no `workflow_runs.sql`, no `workflow_run_tasks` reference, and no board controller anywhere in `backend/` or `frontend/src` |

The discarded schema is worth recording because it is the origin of part of the
UI vocabulary in this document. Its `workflow_runs.state` CHECK was:

```sql
state TEXT NOT NULL CHECK (state IN (
    'queued', 'planning', 'running', 'waiting', 'blocked',
    'needs_attention', 'completed', 'failed', 'cancelled'
))
```

That was an attempt to **store** the display vocabulary. The canonical engine
deliberately does not: it stores seven operational run states and derives
`queued`/`planning`/`blocked`/`reviewing`/`fixing`/`verifying`/
`waiting_for_capacity` from other durable facts. That is the difference this
document exists to pin down.

**Consolidation decision: none needed — already resolved by exclusion.**
The duplicate was reverted off the branch before this document was written and
is retained only as a tag for archaeology. The canonical set is the one created
by `0094_workflow.sql` and extended by `0095`–`0102`, `0104`–`0106`, `0109`,
`0114`, `0117`, `0118`. Rules that follow from that:

- Do **not** revive `0094_workflow_runs.sql` or `workflow_run_tasks`. The
  master-plan execution unit is `workflow_tasks` (migration `0101`).
- Do **not** add a new run-level table for the projection. s2 projects from the
  tables listed above; if a fact is missing, add a column/checkpoint to the
  existing tables per `AGENTS.md` (never hand-edit a merged migration).
- The next free migration number is `0119`.

---

## 2. Table inventory

| Table | Migration | Role in the lifecycle |
| --- | --- | --- |
| `workflow_runs` | `0094` (+`0101`, `0109`) | One objective. Carries the seven durable run states, the frozen `policy_snapshot`, `parent_workflow_id`/`planned_task_id` (master→child link) and `user_id` (owner). |
| `workflow_steps` | `0094` (+`0095`) | Fixed linear v1 chain of six steps per run: `plan → work → review → fix → verify → advance`. Carries `session_id`, `review_run_id`, `artifact_json`. |
| `workflow_attempts` | `0094`, widened by `0096`, `0097`, `0099`, `0100`, `0102` | One row per execution attempt of a step. Carries `outcome`, `error_class`, `retry_after`. Append-only in practice: a failed provider attempt is never overwritten by the fallback's attempt. |
| `workflow_checkpoints` | `0094` (+`0098`) | Append-only durable narrative: `durable_phase`, `next_action`, `retry_state` (JSON), branch/worktree/SHAs, fingerprints. Never updated — a new row advances the story. |
| `workflow_plans` | `0101` | Master-run planning only. `status`, `approval_mode`, `command_status`, `error_class`, generated/validation JSON. |
| `workflow_tasks` / `workflow_task_dependencies` | `0101` | Master plan execution units; each running task points at a normal child `workflow_runs` row via `execution_run_id`. |
| `workflow_wake_schedules` | `0106`, widened by `0114`, `0118` | Durable, restart-safe "come back to this run at T" rows with CAS claim. `reason` is the wait taxonomy. |
| `workflow_outbox` | `0094` (+`0102`) | Idempotent command staging (`spawn_worker_session`, `trigger_review`, …). The idempotency layer for dispatch, not a lifecycle state source. |
| `workflow_questions` / `workflow_question_resolutions` | `0104`, `0105` | Durable "the agent is stuck on a question" facts and their resolution attempts. |
| `branch_locks` | `0117` | Direct-branch mutual exclusion: `(repo_path, branch)` held by one run. |
| `review_run` / `review` | `0012` (+`0080`, `0092`, `0093`) | The reviewer's own record; `workflow_steps.review_run_id` points here. |
| `post_run_qa_runs` | `0126` | One pass of the Post-Run QA gate over a task or a whole run: phase, the findings it collected (JSON, with per-finding attribution), the repair cycles it spent against its budget, and the verdict. State model in `backend/internal/postrunqa`. |

> **No CDC.** `0094` deliberately adds no `change_log` trigger for any workflow
> table (widening `change_log.event_type`'s CHECK would require a full table
> rebuild and misusing `session_updated` would corrupt the session
> invalidation contract). Clients poll — the renderer uses TanStack Query with
> `refetchInterval: 2000` while the run is non-terminal.

---

## 3. Durable state vocabularies (what is actually stored)

```
workflow_runs.state          pending | running | waiting | needs_attention
                             | completed | failed | cancelled

workflow_steps.kind          plan | work | review | fix | verify | advance
workflow_steps.state         pending | ready | running | waiting
                             | completed | failed | cancelled

workflow_attempts.outcome    NULL (in flight) | succeeded | failed | cancelled

workflow_plans.status        pending | running | validated | approved
                             | invalid | rejected
workflow_plans.command_status idle | pending | running | responded
                             | completed | failed

workflow_tasks.state         blocked | eligible | running | completed | cancelled

workflow_wake_schedules.reason
                             capacity_reset | capacity_probe | transient_retry
                             | question_resolver_capacity | reviewer_capacity
                             | worker_capacity | planner_capacity
                             | autonomous_progress | branch_lock
workflow_wake_schedules.status pending | claimed | completed | cancelled

workflow_questions.state     pending | resolving | answered | human_required
                             | cancelled
workflow_questions.classification
                             policy_resolvable | auto_resolvable
                             | human_required | ambiguous

post_run_qa_runs.phase       pending | checking | auto_fixing | clean
                             | needs_attention
post_run_qa_runs.result      '' (in flight) | clean | needs_attention
post_run_qa_runs findings[].attribution
                             new | baseline | ambiguous
post_run_qa_runs findings[].severity
                             blocker | major | minor | info
```

Run and step transition legality is enforced in Go, not SQL, by
`domain.ValidWorkflowRunTransition` / `domain.ValidWorkflowStepTransition`
(`backend/internal/domain/workflow.go`). Terminal states have **zero** outgoing
transitions — a completed/failed/cancelled run can never be mutated again.

---

## 4. End-to-end trace

Line references are to `backend/internal/workflow/`.

### 4.1 Creation

`Coordinator.CreateRun` → `createSingleTaskRun` (`workflow.go`)

- Inserts `workflow_runs` with `state='pending'`, `policy_version='v1'`, and a
  **frozen** `policy_snapshot` JSON (execution mode, review policy, fix budget).
  The snapshot is never re-derived from later Settings changes.
- Seeds all six `workflow_steps` in one transaction: ordinal 1..6, each
  `depends_on_step_id` pointing at its predecessor. Step 1 (`plan`) starts
  `ready`; every other step starts `pending`.
- The `plan` step's `artifact_json` is populated at creation time with the
  deterministic `PlanArtifact` (objective, task prompt, acceptance criteria,
  verification plan).
- Ownership: `workflow_runs.user_id` (migration `0109`). For a master task's
  child run, `stampChildOwnership` (`child_ownership.go`) copies the **parent
  run's** owner — never the request identity — before any dispatch.

For a master-plan run, `workflow_plans` is created alongside with
`status='pending'` and the caller's `approval_mode`.

### 4.2 Planning

- **Master run**: `workflow_plans.status` walks
  `pending → running → validated → approved` (or `invalid` / `rejected`).
  `command_status` tracks the planner provider command
  (`idle → pending → running → responded → completed|failed`). On approval,
  `workflow_tasks` rows are created (`blocked` or `eligible` by dependency) and
  `workflow_task_dependencies` records the DAG.
- **Single-task run**: there is no `workflow_plans` row. The `plan` step is
  deterministic and executes synchronously inside `StartRun`, so it is almost
  never observed in a non-terminal state by a poller.

### 4.3 Worker dispatch and session linkage

`StartRun` → `dispatchWorkStep` (`dispatch.go`)

1. Stage a `workflow_outbox` row: `command_type='spawn_worker_session'`,
   `status='pending'`, with a derived `idempotency_key`.
2. Call `Spawner.Spawn` **once**, with
   `IssueID = "workflow-step:" + workflow_steps.id` (`workStepIssueID`).
3. On success (`recordDispatchSuccess`):
   - `workflow_attempts` row created (unless one is already open),
   - `workflow_steps.session_id` set to the spawned session,
   - `workflow_outbox.status → 'acknowledged'`,
   - a `workflow_checkpoints` row with `durable_phase='worker_dispatched'`
     carrying `session_id`, `branch`, `worktree_path`, `base_sha`,
   - any held `branch_locks` row is renewed to point at the new `session_id`.
4. On provider failure: `recordWorkAttemptFailure` writes a terminal failed
   attempt row **before** any fallback is considered, then
   `selectFallbackForWork` may retry on another harness (a new attempt row).
   The losing attempt is never deleted or overwritten.

### 4.4 Observation

`GetRun` opportunistically calls `observeWorkStep` while the run is non-terminal
and a work step is running — read-time derivation, not a background scheduler.
Each observation appends a checkpoint with
`durable_phase = "worker_observed_" + <progress>` and a `next_action`.

### 4.5 Review → fix → verify

- `review_dispatch.go` creates/reuses a `review_run` row and sets
  `workflow_steps.review_run_id`. Checkpoint `durable_phase='review_dispatched'`.
  A policy decision to skip review is itself durable
  (`durable_phase='review_policy_skipped'`), so "not reviewed" is
  distinguishable from "approved".
- `review_progress.go` records the verdict
  (`durable_phase='review_observed'`). `changes_requested` drives the fix step.
- `fix_dispatch.go` / `fix_progress.go` run the fix cycle, gated on a genuinely
  new `WorkspaceFingerprint` — `workflow_checkpoints.fingerprint_before` is the
  state the verdict addressed, `fingerprint_after` the state once the fix
  landed. Exceeding `max_fix_cycles` records
  `error_class='fix_budget_exhausted'`.
- `verify.go` runs the local verification plan and persists the full
  `VerifyResult` into `workflow_checkpoints.retry_state`
  (`durable_phase='verify_result'`).

### 4.6 Completion, failure, cancellation

- **Completed**: `completeVerifiedRun` (`verify.go`) — the autonomous local
  commit happens first (while the branch lock is still held), then the verify
  step → `completed` and the run → `completed` with `completed_at`.
  ⚠️ The `advance` step is seeded but **never executed**; it stays `pending`
  forever on a completed run. See §8.
- **Failed**: `workflow_runs.state='failed'` is legal and persistable, but
  **no coordinator code path writes it today.** Every failure route
  (dispatch failure, verify failure, commit failure, ambiguous worker state,
  master task failure) lands on `needs_attention` instead, by design: AO
  surfaces the ambiguity rather than declaring a run dead. Treat `failed` as a
  reachable-but-currently-unwritten state.
- **Cancelled**: `CancelRun` (`workflow.go`) CAS-moves the run to `cancelled`,
  cancels every non-terminal step, cancels open questions and running
  resolutions — and deliberately **does not stop the worker session**. It
  records `durable_phase='worker_left_running_on_cancel'` so a human knows to
  stop it manually.

### 4.7 Waiting and wake-up

When a run parks on provider capacity, a branch lock, or an autonomous
heartbeat, `workflow_wake_schedules` gets a row (upsert by `idempotency_key`,
derived from run + step/role + reason). A daemon poller CAS-claims due rows
(`status: pending → claimed`, stamping `claimed_by`/`claimed_at` as a lease)
and completes them with a second CAS. A crash between claim and complete leaves
a stale `claimed` row that the next due query picks back up once the lease
expires — never silently lost, never double-fired.

`known_reset_at` is **nullable and never fabricated**: it is populated only from
a real `AgentHealthEvent.CooldownUntil`. When NULL, `scheduled_at` comes from
bounded exponential backoff with jitter (`wake/scheduler.go`).

### 4.8 Restart / reconcile

`Coordinator.Reconcile` (`recovery.go`) runs once at daemon boot, after storage
opens and before serving:

- Loads every non-terminal run via `ListNonTerminalWorkflowRuns` (backed by the
  partial index `idx_workflow_runs_nonterminal`).
- **Master runs**: `plan.status='pending'` → re-ensure the autonomous kickoff
  wake; `running` + `command_status='responded'` → finalize the generated plan;
  `running` otherwise → mark `invalid` with `error_class='planner_ambiguous'`
  and move the run to `needs_attention`; `approved` → reconcile tasks.
- **`work` steps** found `ready`/`running` re-enter the *same* idempotent
  `dispatchWorkStep` → `observeWorkStep` path. `Spawn` is never called twice:
  a `dispatched` outbox row goes to `adoptOrMarkAmbiguous`, which adopts a
  natural-key session match with a populated workspace and otherwise records
  `durable_phase='worker_dispatch_ambiguous'` +
  `error_class='ambiguous_worker_state'` and moves the run to
  `needs_attention`. A still-alive worker correctly stays `running`.
- **`review` and `fix` steps are explicitly excluded from the generic fallback**
  (`recovery.go:126-133`). They re-enter `advanceReviewFixCycle(ctx, run, steps,
  false)` — once per run, not per step — which is the *same* idempotent
  dispatch/observe cascade `GetRun`/`ContinueRun` drive. A crash mid-review or
  mid-fix therefore **resumes**; it does not park. `includeCycle1Unblock=false`
  means a `review` step still `pending`, never explicitly unblocked by a
  `ContinueRun`, stays untouched by boot recovery.
- **Only `plan`, `verify` and `advance` steps** found `running` fall through to
  the blind `running → waiting` move (`recovery.go:135-141`). That path is also
  the only one that sets `interrupted = true`, so the follow-on
  `running`/`waiting` run → `needs_attention` fallback fires **only** for those
  three kinds — never for a run whose `review` or `fix` step was mid-flight.
- Idempotent: running it twice in a row is a no-op the second time.

> **Contract consequence:** a `review`/`fix` step found `running` at boot is
> never blind-parked by recovery — it re-enters the same cascade `GetRun`/
> `ContinueRun` drive. Any `waiting`/`needs_attention` such a run ends up in
> after `Reconcile` is a **real outcome of that cascade** — a
> `changes_requested` rest (`cascade.go:70-74`, the trigger for
> `maybeDispatchFix`), a reviewer capacity stall (§8.8), or an ambiguous
> review/fix dispatch (§6.3, run → `needs_attention` via
> `markReviewAmbiguous`/`recordReviewOutcome`) — and must be projected and
> rendered as such. s2/s4 must **not** treat it as a restart artifact and must
> **not** special-case "post-restart" at all: the durable rows do not
> distinguish it, and suppressing `needs_attention` here would hide exactly the
> ambiguity signal §6.3 and §8.1 require surfacing.

---

## 5. The mapping: UI lifecycle state → persisted state

The vocabulary in the brief lists 13 labels. All 13 are mapped below.
Evaluate **in order** — the first matching row wins. Terminal states are checked
before waits so a cancelled run never renders as "waiting".

> **This ordering supersedes the current renderer.** `useWorkflowStatusLabel`
> checks a `human_required` question *ahead of* `needs_attention` and
> `completed`, so today a completed run with a lingering question renders
> "waiting for decision"; under this contract it renders `completed`. Adopting
> the contract ordering is a deliberate behaviour change s4 must make. See §8.4.

| # | UI state | Derived from | Precise condition |
| --- | --- | --- | --- |
| 1 | `cancelled` | `workflow_runs.state` | `= 'cancelled'`. `cancelled_at` is the timestamp. |
| 2 | `failed` | `workflow_runs.state` | `= 'failed'`. Legal but **not written by any current code path** — render it, do not expect it. |
| 3 | `completed` | `workflow_runs.state` | `= 'completed'`. `completed_at` is the timestamp. Do **not** compute progress from the `advance` step (§8). |
| 4 | `needs_attention` | `workflow_runs.state` | `= 'needs_attention'`. Reason carriers in §6.3. |
| 5 | `blocked` | `workflow_wake_schedules.reason` + `workflow_checkpoints` | Run `state='waiting'` **and either** the soonest open wake has `reason='branch_lock'` **or** the newest checkpoint has `durable_phase='waiting_for_branch'`. Accept either signal: `scheduleBranchLockWake` is best-effort (`branch_execution.go:181`), so a nil scheduler or a `Schedule` error leaves the checkpoint with no wake row, and keying on the wake alone would render that run as plain `waiting`. The structured detail (branch, repo path, holding run/session) comes from that checkpoint's `retry_state` (§6.1) and can be cross-checked against `branch_locks` (`state='held'`, matching `lock_key`). Also covers a master run whose only remaining `workflow_tasks` are `state='blocked'` on unmet `workflow_task_dependencies`. **Never** render this as a capacity problem — the blocker is another workflow, not the provider plan. |
| 6 | `waiting_for_capacity` | `workflow_wake_schedules.reason`, **or** `workflow_checkpoints.durable_phase` | An open (`status IN ('pending','claimed')`) wake exists for the run and its `reason` is **not** in `{autonomous_progress, branch_lock}` — i.e. `capacity_reset`, `capacity_probe`, `transient_retry`, `question_resolver_capacity`, `reviewer_capacity`, `worker_capacity`, `planner_capacity`. This is a **denylist by design** (`frontend/src/renderer/lib/workflow-wake-reason.ts`): a reason added later defaults to reading as a capacity wait rather than going unlabeled. Show `scheduled_at` as "next retry" and `attempt_count` as "Attempt: N"; show a reset time **only** when `known_reset_at` is non-NULL. **Also matches with no wake at all** when the run is `waiting` and the newest checkpoint has `durable_phase='review_capacity_retry'` — a reviewer capacity stall writes that checkpoint but schedules no wake (§8.8). In that case there is no retry time to show: render the capacity wait without a "next retry". |
| 7 | `waiting` | `workflow_runs.state` | `= 'waiting'` with no open wake, or an open wake with `reason='autonomous_progress'` (the routine heartbeat of a healthy autonomous run — it must never read as a capacity wait). Sub-case **waiting for a decision**: a `workflow_questions` row for the run with `state='human_required'` (or `state='pending'`), which overrides any checkpoint `next_action` at read time. |
| 8 | `planning` | `workflow_plans.status` | Master run: `status IN ('pending','running','validated')` and the run is not terminal. Single-task run: `workflow_steps` where `kind='plan'` and `state IN ('ready','running')` — rarely observable, the plan step is synchronous. |
| 9 | `reviewing` | `workflow_steps.kind/state` | The run's (or, for a master run, its active task's child run's) first non-terminal step has `kind='review'`. Verdict facts come from `review_run` via `workflow_steps.review_run_id`. A durable `review_policy_skipped` checkpoint means review was skipped by policy — render that, not "approved". |
| 10 | `fixing` | `workflow_steps.kind/state` | Same rule, `kind='fix'`. (The existing renderer labels this `applying_fixes`; s4 should treat `fixing` and `applying_fixes` as the same state and pick one string — see §8.) |
| 11 | `verifying` | `workflow_steps.kind/state` | Same rule, `kind='verify'`. Detail comes from the newest `workflow_checkpoints` row with `durable_phase='verify_result'`, decoding `retry_state` as a `VerifyResult`. |
| 12 | `running` | `workflow_runs.state` | `= 'running'` and the first non-terminal step has `kind='work'` (or the master run has a `workflow_tasks` row in `state='running'`). For a master run, "task N of M" is `ordinal` of the running task over `count(workflow_tasks)`. |
| 13 | `queued` | `workflow_runs.state` | `= 'pending'` — created but never started (no `StartRun` yet). For a master run, also a `workflow_tasks` row in `state='eligible'` whose `execution_run_id IS NULL` (approved, dependency-free, not yet dispatched). |

Reference implementation of the derivation that exists today:
`frontend/src/renderer/hooks/useWorkflowExecutionStatus.ts`
(`useWorkflowStatusLabel`). It orders the checks as: human-required question →
`needs_attention` → `completed` → capacity wait → plan status → active child
step kind → "executing task X of Y". Note the first check: that ordering
**differs** from the table above and must change — §8.4.

### 5.1 Step-state → UI, for the per-step timeline

| `workflow_steps.state` | UI |
| --- | --- |
| `pending` | not started, dependency unmet (`depends_on_step_id`) |
| `ready` | eligible, not dispatched |
| `running` | active — this is the step that names `reviewing`/`fixing`/`verifying`/`running` above |
| `waiting` | parked. For a `review` step the **common case is a `changes_requested` rest** — the verdict deliberately rests the step here so `maybeDispatchFix` can pick it up (`cascade.go:70-74`); it is not an error state. Otherwise: capacity (§8.8), ambiguity (§6.3), or — for `plan`/`verify`/`advance` only — the post-restart blind move (§4.8). |
| `completed` / `failed` / `cancelled` | terminal |

---

## 6. Field carriers

### 6.1 Wait reason

| Field | Meaning |
| --- | --- |
| `workflow_wake_schedules.reason` | **The** wait taxonomy. Nine values (§3). Surfaced through `workflow.RunDetail.WaitReason` → API `WorkflowRunView.waitReason`, taken from the *soonest still-open* wake for the run (`wake.Scheduler.NextForRun`). |
| `workflow_wake_schedules.scheduled_at` | When AO will retry. API `nextWakeAt`. |
| `workflow_wake_schedules.attempt_count` | How many times this exact wait has already retried. API `wakeAttemptCount`. |
| `workflow_wake_schedules.known_reset_at` | Real provider cooldown, or NULL. **Never fabricate a reset time from this field being NULL.** |
| `workflow_wake_schedules.status` / `claimed_by` / `claimed_at` | CAS claim + lease. Not user-facing. |
| `workflow_wake_schedules.last_error` | Last wake-firing error. Diagnostic only. |
| `workflow_checkpoints.durable_phase` | The structured *kind* of the stop. See the partition below — a checkpoint is **not** mutually exclusive with a wake row, but it does not imply one either. |
| `workflow_checkpoints.retry_state` | The structured detail for the wait. For `waiting_for_branch` it is a marshalled `workflow.BranchWait` — `{branch, repoPath, heldByWorkflowRunId, heldBySessionId}` (struct at `branch_execution.go:376-381`, marshalled into the checkpoint at `:141`, read back by `branchWaitFromCheckpoints` (`:409`) from the **newest** such checkpoint). This is the field s4 renders "Waiting for branch X — currently used by WF-Y" from; never parse `next_action` prose for it. Surfaced as API `WorkflowRunView.branchWait`, and populated only while the run is actually `waiting`, so a stale checkpoint from a resolved wait is never shown as current. |
| `workflow_checkpoints.next_action` | Human-readable prose for the same wait, prefixed by kind — e.g. `waiting_for_branch: …`, `waiting_for_capacity: planner unavailable (…)`, `waiting_for_decision: <classification> — <question>`. |
| `workflow_attempts.retry_after` | Per-attempt provider retry hint. |

All three zero-value together (`nextWakeAt` NULL, `waitReason` `""`,
`wakeAttemptCount` 0) when no wake is open — that is "no scheduled retry", not
"unknown". **"No scheduled retry" is not the same as "not waiting"**: five of the
seven phases below have no wake row, and one of them — `review_capacity_retry` —
leaves the run genuinely `waiting` while it has none.

#### `durable_phase` partition: which stops pair with a wake

| `durable_phase` | Run lands in | Wake row? | Written by |
| --- | --- | --- | --- |
| `waiting_for_branch` | `waiting` | **Yes** — `ReasonBranchLock`, but **best-effort**: a nil scheduler or a `Schedule` error only logs, so the checkpoint can exist alone (§5 row 5 accepts either signal) | `markRunWaitingForBranch` writes both on the same path (`branch_execution.go:118`; the wake call is at `:146`) |
| `planner_capacity_wait` | stays `pending` (deliberately — several master-plan paths gate on `Pending`) | **Yes** — `ReasonPlannerCapacity` | `parkPlanForCapacity` (`master_coordinator.go:248-267`) |
| `review_capacity_retry` | `waiting` (step **and** run) | **No** | `handleReviewerCapacityStall` (`review_progress.go:305-353`) — see §8.8 |
| `dirty_worktree` | `needs_attention` | **No, by design** — "nothing about waiting makes someone's uncommitted changes go away" | `markRunDirtyWorktree` (`branch_execution.go:154`) |
| `worker_dispatch_ambiguous` | `needs_attention` | **No** | `adoptOrMarkAmbiguous` (`dispatch.go`) |
| `review_dispatch_ambiguous` | `needs_attention` | **No** | `review_dispatch.go` |
| `fix_dispatch_ambiguous` | `needs_attention` | **No** | `fix_dispatch.go` |

So: only `waiting_for_branch` and `planner_capacity_wait` are genuine
wait-with-a-wake kinds. `review_capacity_retry` is a wait **without** one. The
bottom four rows are `needs_attention` phases, not wait kinds — they are listed
here because they share the `durable_phase`/`next_action` carriers, and their
reason-carrier role is documented in §6.3.

A pure capacity wait may also have **no checkpoint at all**:
`markRunWaitingForCapacity` (`dispatch.go:249-257`) moves the run to `waiting`
and schedules the wake without writing one. For those, `waitReason` is the only
signal — which is exactly why §5 row 6 keys on the wake first.

### 6.2 `error_class`

| Field | Domain |
| --- | --- |
| `workflow_attempts.error_class` | The canonical 23-value taxonomy (SQL CHECK, final form in `0102`; Go mirror `domain.WorkflowErrorClass`). NULL = no error recorded. |
| `workflow_plans.error_class` | Free-text `TEXT NOT NULL DEFAULT ''` for planner failures — e.g. `planner_ambiguous`. **Not** the attempt taxonomy; do not validate it against one. |
| `workflow_outbox.error_class` | Free-text `TEXT NOT NULL DEFAULT ''`, set when a staged command's dispatch fails. Mirrors the attempt class string in practice. |

The attempt taxonomy, grouped for display:

- **Provider/auth:** `rate_limited`, `auth`, `capacity_exhausted`,
  `binary_missing`
- **Infrastructure:** `transient`, `tool`, `runtime_failed`
- **Worker dispatch (8B):** `session_create_failed`, `agent_start_failed`,
  `prompt_delivery_failed`, `worker_terminated_unexpectedly`,
  `ambiguous_worker_state`
- **Review/fix (8C/8D):** `reviewer_launch_failed`,
  `review_changes_requested`, `fix_budget_exhausted`
- **Verify (8E):** `test_failed`, `verify_command_failed`, `verify_timeout`,
  `verify_environment_error`, `verify_artifact_missing`,
  `verify_artifact_mismatch`, `verify_workspace_changed`, `verify_ambiguous`
- **Integration (8M.1):** `integration_failed` — present in
  `domain.WorkflowErrorClass` and surfaced via
  `WorkflowIntegrationStateView.errorClass`, but **not** in the
  `workflow_attempts.error_class` SQL CHECK list. Writing it to an attempt row
  would violate the constraint. s2 must not do so without a new migration.

Meaningful distinctions the UI must preserve: `rate_limited` (time-boxed, a
reset is expected) vs `capacity_exhausted` (no typed reset);
`session_create_failed`/`agent_start_failed` (worker) vs
`reviewer_launch_failed` (reviewer pane); `ambiguous_worker_state` ("could not
prove either way" — never render as success or as failure).

### 6.3 `needs_attention` reason

There is **no** `needs_attention_reason` column. The reason is always the pair:

1. `workflow_checkpoints.next_action` — the newest checkpoint for the run
   (`ORDER BY created_at`) carries the prose. Recognizable prefixes:
   `ambiguous_worker_state: …`, `needs_attention: <class> (<reason>)`,
   `local_commit_failed: …`, `worker session terminated with no verifiable
   work …`, `worker awaiting input/blocked — needs human attention`,
   `worker idle with no verifiable change — needs human review`,
   `worker produced no first signal within … of dispatch`.
2. `workflow_checkpoints.durable_phase` — the machine-readable kind:
   `worker_dispatch_ambiguous`, `review_dispatch_ambiguous`,
   `fix_dispatch_ambiguous`, `work_provider_failure_needs_attention`,
   `autonomous_local_commit_failed`, `dirty_worktree`,
   `master_integration_promotion_failed`.

Plus, when the step's attempt failed, `workflow_attempts.error_class` on the
latest attempt of the latest step gives the typed cause, and
`workflow_checkpoints.retry_state` (JSON) carries the structured payload — the
`VerifyResult` for a verify failure, the routing decision for a routing
checkpoint, the session-lifecycle record for `session_lifecycle_decision`, and
the `BranchWait` struct for `waiting_for_branch` (§6.1).

For a **master** run, `workflow_plans.error_class` and
`workflow_plans.validation_json` carry planner-side reasons, and
`MasterIntegrationSummary.status/errorClass` (derived from
`durable_phase='master_integration_promotion_failed'` checkpoints) carries integration
reasons.

For a **question-driven** stop, `workflow_questions.classification` +
`classification_reason` + `question_text` are the reason, and the run's
`next_action` is overridden at read time by
`waiting_for_decision: <classification> — <text>`.

**Rule for s2/s4: never synthesize a reason.** If `next_action` is empty and no
attempt has an `error_class`, the correct rendering is "needs attention — no
recorded reason", not a guess.

---

## 7. Worker / session linkage

How a spawned worker or reviewer is tied back to its parent run/attempt:

| Direction | Field(s) |
| --- | --- |
| Step → worker session | `workflow_steps.session_id` → `sessions.id`. Set once, by `recordDispatchSuccess`. |
| Session → step (natural key, restart-safe) | `sessions.issue_id = 'workflow-step:' + workflow_steps.id`, scoped by `sessions.project_id`. This is what `FindSessionByProjectAndIssueID` uses after a crash to adopt an already-spawned worker instead of spawning a second one. |
| Checkpoint → session | `workflow_checkpoints.session_id`, alongside `branch`, `worktree_path`, `base_sha`, `head_sha`. |
| Attempt → step | `workflow_attempts.workflow_step_id` + `attempt_number` (UNIQUE together). |
| Checkpoint → attempt | `workflow_checkpoints.attempt_id` (nullable — run-level checkpoints such as `session_lifecycle_decision` deliberately have none). |
| Step → dispatch command | `workflow_outbox.workflow_step_id` + `idempotency_key` (UNIQUE). `status` is the dispatch truth: `pending` = never spawned, `dispatched` = spawn issued, `acknowledged` = session linked. |
| Step → reviewer | `workflow_steps.review_run_id` → `review_run.id`. `review_run.session_id` is the **worker** session under review; `review_run.harness`, `verdict`, `target_sha`, `body` carry the outcome. The reviewer pane itself is ephemeral and is not a workflow-linked session row. |
| Master run → child run | `workflow_tasks.execution_run_id` → `workflow_runs.id` (UNIQUE), and the reverse pair on the child: `workflow_runs.parent_workflow_id` + `workflow_runs.planned_task_id` (UNIQUE partial index `idx_workflow_runs_planned_task`). `planned_task_id` is the natural key `FindWorkflowRunByPlannedTask` uses to re-adopt a child after a crash. |
| Run → owner | `workflow_runs.user_id` (nullable by design). A child run inherits the parent's owner via `stampChildOwnership`; in multi-user mode a child with an unresolved owner is refused dispatch. |
| Run → branch lock → session | `branch_locks.workflow_run_id`, `.workflow_step_id`, `.session_id`, `.owner_token`, `.state='held'`, keyed by `lock_key` = canonical `repo_path` + branch. `owner_token` (daemon instance) is what makes stale-lock reconciliation decidable after a restart — never timestamps alone. |
| Question → its origin | `workflow_questions.workflow_run_id`, `.workflow_step_id`, `.workflow_attempt_id`, `.session_id`, `.asking_harness`, `.asking_role`; `.fingerprint` is UNIQUE (dedupe). |
| Question → resolver session | `workflow_question_resolutions.workflow_question_id`, `.asking_session_id`, `.resolver_session_id` (nullable until launch), `.resolver_harness`; `workflow_questions.resolving_run_id` points at the live attempt. A partial UNIQUE index allows at most one `status='running'` resolution per question. |

**Never derive linkage from timestamps, display names, or worktree paths.**
Every link above is an explicit column or the `workflow-step:` natural key.

---

## 8. Known gaps the projection and UI must handle honestly

These are real properties of the engine as it stands, not TODOs invented here.

1. **`workflow_runs.state='failed'` is never written.** All failure routes land
   on `needs_attention`. s4 should render `failed` if it ever appears but must
   not treat its absence as "nothing failed".
2. **The `advance` step never runs.** `completeVerifiedRun` completes the run at
   the `verify` step, leaving step 6 `pending` forever. Any "N of 6 steps done"
   progress indicator will read 5/6 on a successfully completed run. Compute
   progress from `workflow_runs.state` (and, for master runs, from
   `workflow_tasks`), not from a step count.
3. **`fixing` vs `applying_fixes`.** The existing renderer uses
   `applying_fixes`; this contract names the state `fixing`. They are the same
   state — s4 picks one label and uses it everywhere; s2 emits the state name
   from this document.
4. **`waiting_for_decision` is a sub-case of `waiting`,** not a 14th state. Its
   carrier is `workflow_questions.state='human_required'`.
   **It also changes precedence.** `useWorkflowStatusLabel`
   (`frontend/src/renderer/hooks/useWorkflowExecutionStatus.ts:61-64`) checks the
   human-required question *first*, before `needs_attention` and `completed`:

   ```ts
   const hasHumanRequiredQuestion = (workflow.questions ?? []).some((q) => q.state === "human_required");
   if (hasHumanRequiredQuestion) return "waiting_for_decision";
   if (workflow.run.state === "needs_attention") return "needs_attention";
   if (workflow.run.state === "completed") return "completed";
   ```

   §5 puts terminal states and `needs_attention` first instead, so a **completed
   run with a lingering `human_required` question** renders `waiting_for_decision`
   today but `completed` under this contract. That is deliberate — a durable
   terminal run state is a stronger fact than an unanswered question that no
   longer blocks anything — but it **is** a behaviour change s4 has to make, not
   a description of current behaviour. If s4 decides the existing ordering is
   better, change §5 here first so s2 and s4 stay on one contract.
5. **`integration_failed` is not a valid `workflow_attempts.error_class`.**
   It exists only in Go and on the integration view (§6.2).
6. **No CDC events.** Every workflow read is a poll. Non-terminal runs refetch
   on an interval; terminal runs must stop refetching.
7. **Cancellation does not stop workers.** A cancelled run may still have a
   live session. The UI must surface the
   `worker_left_running_on_cancel` checkpoint rather than implying the worker
   was killed.
8. **A reviewer capacity stall parks the run with no wake row.**
   `handleReviewerCapacityStall` (`review_progress.go:305-353`) records the
   provider health failure, cancels the `review_run`, moves both the review step
   and the run to `waiting`, and writes a `review_capacity_retry` checkpoint —
   but it schedules **no** wake. (Its own comment notes the *next* dispatch
   attempt would take `markRunWaitingForCapacity`'s `reviewer_capacity` wake
   path, but that only happens once something re-drives the run.) The durable
   state is therefore:

   ```
   workflow_runs.state           = 'waiting'
   workflow_steps(review).state  = 'waiting'
   newest durable_phase          = 'review_capacity_retry'
   workflow_wake_schedules       = (no open row)
   ```

   Keyed on the wake alone, §5 row 6 could not fire and the run would fall
   through to plain `waiting` with "no scheduled retry" — while actually being a
   provider-capacity wait. Row 6 therefore matches this checkpoint too. The
   residual gap s2/s4 must still handle honestly: **there is no retry time to
   show**, because there genuinely is none. The run resumes only on the next
   `GetRun`/`ContinueRun` poll or a boot `Reconcile`. Render the capacity wait
   without a "next retry"; do not synthesize one.
