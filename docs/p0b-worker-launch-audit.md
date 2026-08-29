# P0-B — worker launch / attempt state machine audit

**Status: IMPLEMENTED.** This document was written as the design contract for
the P0-B block (steps 2–6), and every "proposed" item in it has since been
built. It is kept verbatim as the rationale — the *why* behind each predicate is
still the load-bearing part — with one addition: §7 below records what shipped,
where, and the two places the implementation deliberately differs from what §4.2
proposed. Where this document says "proposed", read "implemented as described
unless §7 says otherwise".

**Branch audited:** `feat/engineering-control-center`, at the state of this
commit. Line numbers are given as `file:line` where a claim depends on a specific
statement; function names are given everywhere, because they survive edits that
line numbers do not. All paths below are relative to `backend/internal/`.

**Relationship to [`worker-lifecycle-audit.md`](worker-lifecycle-audit.md).**
That document is the wide audit: it enumerates all thirty-two plan-segment crash
windows (`CP1`–`CP32`) and the work-segment ones (`C#`/`CF#`), and its §6.9
deferred four of them — CP13/CP14, CP30/CP31/CP32, CP1 and CP7 — as "documented,
not scheduled". This document is the narrow one: it re-derives the worker launch
→ attempt lifecycle → completion path against the code as it stands *now* (some
of the wide audit's line references have since drifted — e.g. `ApprovePlan`'s
early exit is at `workflow/master_coordinator.go:517`, not `:459`) and turns
those four deferred items into an implementable contract with an identity model
and a CAS predicate per transition. Where the two disagree on a line number, this
one is current.

**Scope of the audit vs. scope of the block.** The audit covers the whole path
from dispatch intent to review hand-off. The *implementation* steps that follow
(2–6) touch only what §5 marks **Fix**; everything marked **Fail closed** is to
be made honest (refuse, record, stop with a readable reason) rather than made
correct.

**Explicitly out of scope:** the reviewer's own lifecycle.
`workflow/review_dispatch.go`, `review_launch_recovery.go`,
`review_authority.go` and migrations `0135`–`0138` are read here as the
*reference implementation* of the CAS model the worker path should adopt.
Nothing in this block edits them. Where the worker path needs the same primitive
it adopts the existing table-generic store method
(`ClaimWorkflowOutboxDispatch`, `FailWorkflowOutboxWithGeneration`,
`ReleaseDispatchedWorkflowOutboxGeneration`,
`ReopenFailedWorkflowOutboxGeneration`, `ClaimWorkflowAttemptOutcome` — all in
`storage/sqlite/store/workflow_store.go`) rather than generalising the
reviewer's use of it.

---

## 1. The worker launch path, stage by stage

Two entry paths reach the same work step and both are covered:

- **Path A — standalone run.** `CreateRun` → `StartRun` (`workflow/workflow.go`).
  The plan step is a deterministic template expansion (`BuildPlanArtifact`,
  `workflow/plan.go`), executed synchronously inside `StartRun`.
- **Path B — master objective.** `CreateObjectiveRun` → `GeneratePlan` →
  `finalizeGeneratedPlan` → `ApprovePlan` → `reconcileMasterTasksOnce` →
  `dispatchMasterTask` → `createSingleTaskRun` → `StartRun` (all
  `workflow/master_coordinator.go`). Path B is a **prefix** of Path A, not an
  alternative to it: every child run re-enters `StartRun`.

### 1.1 Stage map

| # | Stage | Owning function | Location | What is durable at the end of it |
| --- | --- | --- | --- | --- |
| S0 | Dispatchability gate | `dispatchWorkStep` | `workflow/dispatch.go:125` | Nothing. Pure guards: terminal run/step, open question (`hasOpenQuestion`), `step.SessionID != nil`, no launcher wired |
| S1 | Outbox enqueue (idempotency key) | `dispatchWorkStep` → `EnqueueWorkflowOutboxEntry` | `dispatch.go:149`; key from `workStepOutboxIdempotencyKey`, `dispatch.go:90` | One `workflow_outbox` row per step, `status=pending`, key `work:<stepID>` |
| S2 | Retry floor | `latestWorkerLaunchRecord` / `dueForRetry` | `worker_launch_recovery.go:361`, `:232` | Nothing. A bounded retry that is not yet due owns the step, and every other entry point (boot reconcile, capacity wake, master pass) backs off — `dispatch.go:172-175` |
| S3 | Routing + capacity + branch lock | `dispatchFromPending` → `routeWorkerDispatch`, `ensureBranchLock` | `dispatch.go:192-215` | A `routing_decision` checkpoint; on `Waiting`, `markRunWaitingForCapacity` and the entry left **pending** — never `dispatched` with nothing spawned |
| S4 | Outbox CAS pending → dispatched | `dispatchFromPending` | `dispatch.go:248` (`UpdateWorkflowOutboxStatus`, expected `pending`) | The outbox row is `dispatched`. **The step deliberately does NOT move to running here** (`dispatch.go:257-260`) |
| S5 | **Dispatch intent (phase 1)** | `beginWorkerDispatch` → `openWorkerAttempt` + `recordDispatchBoundary` | `dispatch_state_machine.go:398`, `:446`, `:308` | An **attempt row** (`workflow_attempts`, outcome empty) *and* a dispatch record with `stage=intent`, `outcome=intended`. A store with no `DispatchRecorder` refuses to launch at all (`errNoDispatchRecorder`, `:174`) |
| S6 | Runtime env + provider preflight | `resolveRuntimeEnv`, `preflightWorkerDispatch` | `dispatch.go:561`, `:473` | On failure: attempt concluded (`concludeWorkerAttemptFailure`, `dispatch.go:577`), a `failed` boundary at stage `runtime_env`/`preflight`, then `recordWorkerLaunchFailure` |
| S7 | **Runtime launch (phase 2)** | `launchWorker` → `WorkerLauncher.LaunchWorker` | `dispatch_state_machine.go:496`; production impl `spawnerWorkerLauncher.LaunchWorker`, `:218` → `session_manager.Manager.Spawn` | Nothing in workflow's tables. The session row and its runtime identity are written by `session_manager/manager.go:922-936`: `RuntimeHandleID` from `runtime.Create`, `RuntimeLaunchID` from `superviseAgentProcess`, then `lcm.MarkSpawned` |
| S8 | Ownership read-back | `SessionOwnership.ObserveSessionOwnership` | `dispatch_state_machine.go:242` (`sessionFactsOwnership`) | Nothing. Produces `SessionOwnershipEvidence{Observed, RuntimeHandleID, RuntimeLaunchID, AgentSessionID, Branch, WorktreePath, BaseSHA}` — or `Unavailable`/`Missing`, which are facts, not absences |
| S9 | **Launch confirmation (phase 3)** | `confirmWorkerDispatch` | `dispatch.go:684`; the boundary write at `:734`, the `worker_dispatched` checkpoint at `:762` | A dispatch record with `stage=confirm`, `outcome=dispatched`, carrying session id + runtime handle/launch/agent-session ids, then the checkpoint the adoption and recovery readers key off |
| S10 | **RUNNING (phase 4)** | `confirmWorkerDispatch`, strictly after S9 | `dispatch.go:780` (`UpdateWorkflowStepSession`), `:794` (outbox → acknowledged), `:799` (step `ready → running`) | The step is running, with a session on it, over a launch that was proven |
| S11 | Observation: idle / terminal / blocked | `observeWorkStep` → `evaluateWorkStepProgress` | `worker_progress.go:403`, `:143` | Step transition (`:556`), run transition (`:574`), a `worker_observed_<progress>` checkpoint (`:604`), and on a stop an attention row (`:637`) |
| S12 | Completion evidence | `observeWorkStep` completion branch | `worker_progress.go:556`, `:604` (checkpoint carrying `HeadSHA` + `FingerprintAfter`), `:682` (`UpdateWorkflowAttemptOutcome`) | Step `completed`, checkpoint `NextAction="start_review"`, attempt outcome `succeeded` |
| S13 | Review transition | `advanceReviewFixCycle` → `dispatchReviewStep` | `cascade.go:34`, `:201`; `review_dispatch.go:484` | Out of scope for change; consumes S12's fingerprint as cycle 1's `target_sha` |

### 1.2 The three failure shapes S7–S9 can produce

These are deliberately different states, and the difference is the whole point of
the phased machine:

| Shape | Trigger | Recorder | Resulting state |
| --- | --- | --- | --- |
| **Launch failed** | `LaunchWorker` returned an error | `recordLaunchFailureBoundary` (`dispatch_state_machine.go:522`), then `recordWorkerLaunchFailure` (`worker_launch_recovery.go:254`) | Attempt concluded `failed`; then either the outbox back to `pending` + a wake (bounded retry, `worker_launch_recovery.go:310-314`) or `recordDispatchFailure` (`dispatch.go:815`: outbox `failed` `:817`, step `failed` `:829`, run `needs_attention` `:835`) |
| **Launched, unnameable** | `LaunchWorker` returned nil error and an empty `Session.ID` (`errLaunchWithoutEvidence`, `dispatch_state_machine.go:182`; raised at `:512`) | `recordAmbiguousLaunchBoundary` (`:558`) | Outbox stays `dispatched`, step stays out of RUNNING, **nothing is retried**. Resolved later by `adoptOrMarkAmbiguous` (`dispatch.go:586`) or `reconcileWorkStepDispatch` |
| **Launched, unconfirmable** | ownership not `Observed` (`dispatch.go:720`), or the confirmation write failed (`:751`) | `recordUnconfirmedLaunch` (`dispatch_state_machine.go:645`), reasons `unconfirmedOwnershipUnproven` / `unconfirmedWriteFailed` | Distinguishable from both success and failure: the evidence says an agent exists, so a later pass **adopts** rather than relaunches |

### 1.3 Crash-recovery readers of that state

| Reader | Location | What it is allowed to conclude |
| --- | --- | --- |
| `WorkerDispatchStatusForStep` | `dispatch_state_machine.go:828` | The derived phase (`none`/`intended`/`unconfirmed`/`confirmed`) and `LicensesRunning()` |
| `reconcileWorkStepDispatch` | `dispatch_reconcile.go:450` | Six contradictions (`running_without_evidence`, `stale_running`, `unprovable`, …) and five actions (`noop`, `protected`, `adopted`, `retry_scheduled`, `needs_attention`) |
| `observeDispatchOwnership` | `dispatch_reconcile.go:320` | "Is there a live worker under this dispatch key", across every identity the key could wear. This IS the duplicate-wake protection |
| `adoptLiveLaunch` | `dispatch_reconcile.go:674` | A launch AO could not confirm, whose worker is *provably* alive, is confirmed now — never relaunched |
| `reapOrphanedAttempts` | `attempt_reaper.go:108` | Closes attempt rows abandoned by a crash, only with `laterProgressEvidence` (`:205`) / `attemptOwnerIsQuiet` (`:235`) corroboration |
| `ContinueRun` | `workflow.go:1344` | The wake poller's only entry point; runs `ReconcileWorkStepDispatch` (`:1440`) **before** any resume or dispatch |
| `reconcileRun` | `recovery.go:114` | Boot recovery; the plan switch at `:132-174`, then the per-step work reconciliation |

---

## 2. Identity fields in play today

| Field | Type / source | Written | Read as identity | Gap |
| --- | --- | --- | --- | --- |
| **Run id** | `wf-<id>`, `workflow_runs.id` | `CreateRun`; `CreateObjectiveRun` (`master_coordinator.go:38`) | Everywhere | None |
| **Task id** | `workflow_tasks.id` (master path only) | `finalizeGeneratedPlan` (`master_coordinator.go:193`) | `reconcileMasterTasksOnce` (`:701`), `dispatchMasterTask` (`:1120`) | **Binding sound; one ABA on the park/resume cycle.** The two-way task↔run binding is unique and monotonic, and the terminal mirrors are irreversible, so those need nothing (§2.1). The gap is that `running` recurs across park/resume and no predicate names *which* attempt — the counter that would, `WorkflowTaskAttention.Attempt`, is durable but unread |
| **Step id** | `wfs-<id>` | run creation | The dispatch **natural key**: `workStepOutboxIdempotencyKey(stepID)` (`dispatch.go:90`) and `workStepIssueID(stepID)` (`:67`) | None |
| **Attempt id** | `wfa-<id>`, `workflow_attempts.id` | `openWorkerAttempt` (`dispatch_state_machine.go:446`) | Carried on the intent boundary and in the confirm boundary's `evidence["attemptId"]` (`dispatch.go:731`) | **No generation.** "The open attempt" is positional — `attempts[len(attempts)-1].Outcome == ""` (`dispatch_state_machine.go:456`) — so two concurrent passes can both believe they hold it |
| **Attempt generation** | — | — | — | **Does not exist for workers.** The reviewer has the equivalent via `workflow_outbox.dispatch_generation` + the `review_dispatch_authorized` checkpoint id (`review_dispatch.go:1149`); the worker path has nothing |
| **Owner** | `domain.UserID` + `ProviderProfileID` | `resolveRuntimeEnv` (`dispatch.go:561`) | Passed to `Spawn`; also the policy-freeze owner | Resolved per launch, never compared on a later transition |
| **Runtime handle id** | tmux pane/session handle, `session_metadata.runtime_handle_id` | `session_manager/manager.go:927`, from `runtime.Create` | `SessionOwnershipEvidence.RuntimeHandleID`; `tmux.Runtime.IsAlive` (`adapters/runtime/tmux/tmux.go:816`) | Recorded on the confirm boundary; **not** part of any predicate |
| **Runtime launch id (launch generation)** | `session_metadata.runtime_launch_id`; also exported to the subprocess as `AO_RUNTIME_LAUNCH_ID` (`session_manager/manager.go:137-138`) | `superviseAgentProcess` (`manager.go:902`), stamped at `:928` | `tmux.Runtime.IsExactSupervisedProcessAlive` (`tmux.go:856`) matches `--launch <id>` in the supervisor argv (`isSupervisorCommand`, `tmux.go:1466`) | **The strongest fence AO owns, and no workflow-side write is conditioned on it.** A session id survives a daemon restart; the process behind it does not — only this id separates them |
| **Agent session id** | provider-native session id | session metadata | `SessionOwnershipEvidence.AgentSessionID`; resume/continuation | Recorded, never predicated |
| **Outbox dispatch generation** | `workflow_outbox.dispatch_generation` (`domain/workflow.go:519`) | `ClaimWorkflowOutboxDispatch` (`workflow_store.go:431`; SQL `queries/workflow.sql:359-372`) | `FailWorkflowOutboxWithGeneration`, `ReleaseDispatchedWorkflowOutboxGeneration` | **Reviewer-only today.** The worker path uses `UpdateWorkflowOutboxStatus`, which by design *clears* both tokens (`queries/workflow.sql:336-357`) |
| **Outbox failure generation** | `workflow_outbox.failure_generation` (`domain/workflow.go:507`) | `FailWorkflowOutboxWithGeneration` | `ReopenFailedWorkflowOutboxGeneration` — the human resume | Reviewer-only today |

### 2.1 How the master task id reaches the child run

The task identity is not carried in the dispatch tuple at all — it is carried by
a **two-way durable binding between the task row and the child run row**, and the
worker stages S0–S13 never see it. The path, exactly:

| | Write | Location | Predicate today |
| --- | --- | --- | --- |
| T1 | Child run created with `planned_task_id = task.ID` and `parent_workflow_id = parent.ID` | `dispatchMasterTask` (`master_coordinator.go:1183`) → `createRunWithPlanArtifact` (`workflow.go:868`), field set at `workflow.go:905`, written by `CreateWorkflowRun` **in the same transaction as the run's six step rows** | **Partial unique index** `idx_workflow_runs_planned_task` on `workflow_runs(planned_task_id) WHERE planned_task_id IS NOT NULL` (migration `0101_workflow_master_plan.sql:7-8`). At most one child run per task, enforced by the schema |
| T2 | Task row bound to the child: `execution_run_id = child`, `state → running` | `SetWorkflowTaskExecutionRun` (`master_coordinator.go:1216`; SQL `queries/workflow_plan.sql:107-109`) | `id = ? AND execution_run_id IS NULL AND state = 'eligible'` — a genuine claim |
| T3 | Recovery re-bind after a crash between T1 and T2 | `dispatchMasterTask`'s `FindWorkflowRunByPlannedTask` branch (`master_coordinator.go:1121-1124`), and the same lookup in `reconcileMasterTasksOnce` (`:743-747`) | Same T2 claim, but **its boolean result is discarded** (`_, _ =` at `:1124`) |
| T4a | Convergence: a **terminal** child run state mirrored onto the task | `reconcileMasterTasksOnce` `:860` (`running → completed`), `:872` (`→ failed`), `:896` (`→ cancelled`); `syncCancelledTask` (`branch_lock_recovery.go:111`) | `id = ? AND state = ?` — sufficient, because the source state is irreversible |
| T4b/c | Park and resume: the task's own **re-enterable** state | `ParkWorkflowTaskForAttention` (`task_integration_route.go:570`); `ResumeWorkflowTaskFromAttention` (`:667`) | `id = ? AND state = ?` — **`running` recurs across the cycle, so this matches every generation of it** |

Two properties of this binding are already strong and should be preserved rather
than re-engineered:

- **T1 is atomic and unique.** The `planned_task_id` stamp lands inside the run
  creation transaction, and the partial unique index makes a second child run for
  the same task impossible at the storage layer.
- **The binding is monotonic and unique in both directions.**
  `SetWorkflowTaskExecutionRun` (`queries/workflow_plan.sql:107-109`) is the
  *only* statement in the whole schema that writes `execution_run_id`, and it
  never writes `NULL`; `UpdateWorkflowTaskState`, `ParkWorkflowTaskForAttention`
  and `ResumeWorkflowTaskFromAttention` (`:63-77`) do not touch the column. The
  `workflow_tasks` table also carries `UNIQUE(execution_run_id)` (migration
  `0119_workflow_task_failed_state.sql:145`, carried forward from
  `0101_workflow_master_plan.sql:47`). So one task names at most one run, one run
  is named by at most one task, and once named the name is final.

Those two properties settle the *binding* races outright: a second child run for
one task, or a second task for one child run, is refused by the schema, and no
statement can re-point a binding once taken. They also settle most of T4, and it
is worth being explicit about why, because it removes a fence a reader might
otherwise expect this document to propose:

- **`execution_run_id` must NOT be added to the T4 predicates.** It is written
  once and never changed, so for the whole life of a task it is a constant. A
  constant in a WHERE clause cannot change the outcome of any write: every
  candidate writer reads the same value, so it accepts and rejects exactly the
  writers `id + expected_state` already accepts and rejects. It would look like
  provenance and fence nothing.
- **The three terminal mirrors are already sound.** `:860` (`→ completed`),
  `:872` (`→ failed`), `:896` (`→ cancelled`) and `syncCancelledTask`
  (`branch_lock_recovery.go:111`) all fire on a *terminal* child run state, and
  terminal run states have no outgoing transitions at all —
  `ValidWorkflowRunTransition`'s switch has no arm for them and falls to
  `default: return false` (`domain/workflow.go:87-88`). A verdict read from a
  terminal child is therefore permanently true; a pass holding a stale copy of it
  is holding a fact that has not aged. `id + expected_state` is sufficient, and
  the task's own terminal states (`domain/workflow_plan.go:83-85`) make the write
  once-only besides.

**The one genuinely mutable state, and the actual gap, is the park/resume
cycle.** `WorkflowTaskNeedsAttention` is deliberately non-terminal and its only
exit is a person (`domain/workflow_plan.go:73-79`), so a task can travel
`running → needs_attention → running` and back again without bound:
`ParkWorkflowTaskForAttention` (`task_integration_route.go:570`) takes it out,
`ResumeWorkflowTaskFromAttention` (`:667`, targeting `WorkflowTaskRunning`) puts
it back. Both predicates are `id = ? AND state = ?`, and `state = 'running'` is
satisfied by *every* generation of running. That is an ABA: a pass that observed
the conflict of integration attempt N and then paused — a slow git probe, a
rebase, a re-scheduled poll — can land its park after a human has already resumed
the task into attempt N+1, parking the new attempt for the old attempt's
conflict and overwriting the new attempt's `attention_json` with stale SHAs.

**The value that distinguishes those generations already exists.**
`WorkflowTaskAttention.Attempt` (`domain/workflow_plan.go:121-124`) lives in
`workflow_tasks.attention_json`, is incremented by the parking caller itself
(`task_integration_route.go:568`, `task.Attention.Attempt + 1`), and — decisively
— **survives a resume**: `ResumeWorkflowTaskFromAttention`
(`queries/workflow_plan.sql:74-77`) clears `attention_reason` and `attention_at`
and leaves `attention_json` alone. Its own doc comment already claims the
property this predicate would make true — *"the resume produced exactly one new
attempt"* — and today nothing in the storage layer checks it.

T3 is a smaller, different problem and should not be conflated with this one: it
*re-claims* on the recovery path (`_, _ =` at `master_coordinator.go:1124`, and
again at `:746`) and discards the answer, so a task already bound to a run other
than the one `FindWorkflowRunByPlannedTask` returned produces no error, no stop
and no log line. Under the unique indexes above that disagreement should be
impossible. The point of checking it is not that it is likely — it is that if the
impossible ever happens, the current code is structurally incapable of telling
anyone.

The two paths therefore end up in different places, and the audit should not
flatten them. **The master-task path is mostly sound**: its bindings are
schema-enforced and its convergence reads irreversible states, leaving one real
ABA (T4b/T4c) whose discriminator is already durable and merely unread. **The
worker path is not**: every transition off `dispatched` is `id +
expected_status`, a predicate any concurrent pass satisfies, over a launch whose
generation nothing records at all. That is where the weight of §4 falls, and it
is why `attempt_generation` — not `task_id` — is the field this block has to
invent.

---

## 3. The four deferred defects, located

### 3.1 CP13 / CP14 — `ApprovePlan`'s early exits skip the transitions its own first write made necessary

**Location:** `workflow/master_coordinator.go:502-541`
(`(*Coordinator).ApprovePlan`).

Four durable writes in sequence:

| | Write | Line |
| --- | --- | --- |
| W1 | `ApproveWorkflowPlan` — plan `validated → approved` | `:524` (SQL `queries/workflow_plan.sql:38-40`, CAS on `status='validated'`) |
| W2 | plan step `waiting → running`, only if it was waiting | `:534` |
| W3 | plan step `running → completed` | `:536` |
| W4 | run `waiting|pending → running` | `:539` |

Two early exits sit above them:

- `:517-519` — `if plan.Status == domain.WorkflowPlanApproved { return c.GetRun(...) }`
- `:528-530` — `if !moved { return c.GetRun(...) }`, which after W1 has landed is
  the same condition reached through the CAS instead of the read

**CP13** is a crash between W1 and W3: the plan is `approved` and the plan step
is still `waiting`/`running`. Re-entering `ApprovePlan` hits `:517` (or `:528`)
and returns before W2–W4. Tasks still dispatch — `reconcileMasterTasksOnce` gates
on the plan row, not the step — so the damage is a durable lie in the ledger
rather than a stall.

**CP14** is a crash between W3 and W4: plan approved, plan step `completed`, run
still `pending`/`waiting`. This one is **consequential**: every branch of
`reconcileMasterTasksOnce` that parks or completes the objective is gated on
`run.State == running` (`:844`, `:886`, `:921`, `:1040`, `:1059`, and
`completeRun`). The objective dispatches tasks forever and can never complete or
report a stop. Re-entry is refused for the same reason as CP13.

**Root cause, precisely:** the early exits test the state that W1 itself
produces, so the function is not idempotent across its own write set.

**Verdict: fixable in this block.** W2–W4 are fully re-derivable from
`plan.status = 'approved'`; no external evidence and no ambiguity is involved.

### 3.2 CP30 — the retry budget is recorded *after* the reset it bounds

**Location:** `workflow/master_coordinator.go:420-443`
(`(*Coordinator).retryPlanOrFail`), with the counter at `:459-473`
(`plannerRetryCount`) and the budget constant at `:398`
(`maxPlannerRetries = 3`).

Write order today:

| | Write | Line |
| --- | --- | --- |
| P41 | `FinishWorkflowPlan(pending, idle, …)` — **the reset that arms the next attempt** | `:431` |
| P42 | `stopPlanningStep` — plan step `running → waiting` | `:432` |
| P43 | `recordAttentionStopWithState(ReasonPlannerRetryScheduled, …)` — **the budget row** | `:433` |
| P44 | `scheduleWake(ReasonTransientRetry)` | `:441` |

`plannerRetryCount` derives the budget by counting durable
`planner_retry_scheduled` checkpoints. Therefore:

- **Crash between P41 and P43:** the plan is re-armed for another attempt with
  the retry *not* recorded. The budget of 3 is silently widened by one for every
  such crash.
- **Crash between P43 and P44:** the retry is recorded with no wake to carry it —
  the objective sits armed and nothing rings the bell.

**The counterexample lives in this repo.** `recordWorkerLaunchFailure`
(`worker_launch_recovery.go:287`) writes its `workerLaunchRecordPhase`
checkpoint — the row `workerLaunchAttemptCount` counts — **before** it moves the
outbox back to `pending` at `:310`. Same problem, correct order, in a different
file. `retryPlanOrFail` has it backwards.

**Verdict: fixable in this block.** A pure reordering: P43 → P41 → P42 → P44.
Recording a retry that then fails to arm costs one unit of budget; arming a retry
that fails to record costs the budget itself.

### 3.3 CP31 / CP32 — the terminal row lands before the explanation

**CP31 location:** `workflow/master_coordinator.go:475-493` (`failPlan`).

| | Write | Line |
| --- | --- | --- |
| P45 | `FinishWorkflowPlan(invalid, failed, …)` — **terminal: `GeneratePlan`'s status switch at `:66` treats `invalid` as permanent forever after** | `:477` |
| P46 | run `pending → needs_attention` | `:479` |
| P47 | `stopPlanningStep` | `:481` |
| P48 | `recordAttentionStopWithState(class, cause, …)` — **the only human-readable reason** | `:487` |

**CP32 location:** `workflow/recovery.go:149-156` — the `plan.Status == running`
arm of `reconcileRun`'s plan switch.

| | Write | Line |
| --- | --- | --- |
| RP2 | `FinishWorkflowPlan(invalid, failed, "planner_ambiguous")` | `:151` |
| RP3 | run → `needs_attention` | `:153` |
| RP4 | `recordAttentionStop(ReasonPlannerAmbiguous, "…cannot prove whether it produced a plan")` | `:155` |

In both, a crash after the first write leaves a **permanently invalid plan under
a run that still says `pending`, with nothing on the ledger a person can read.**
The verdict is right; the order is wrong. This is the plan-segment twin of the
work segment's C21a–C21c.

**Verdict: fixable in this block.** Reorder so the explanation is durable first.
The reason row is harmless if the terminal row never lands (the next pass
re-derives and overwrites it); the terminal row is *not* harmless without the
reason.

### 3.4 CP1 — a master run with no plan row is permanently inert

**Location:** `workflow/master_coordinator.go:41` and `:44` —
`CreateObjectiveRun` calls `c.store.CreateWorkflowRun(...)` and then, in a
**separate transaction**, `c.planStore.CreateWorkflowPlan(...)`.

A crash between them leaves a run with a `plan` step and no `workflow_plans` row.
Nothing can then recognise it as a master objective:

- `GetWorkflowPlan` reports `master == false`, so `getMasterRun`
  (`master_coordinator.go:559`) never runs;
- `ContinueRun` falls through its master branch (`workflow.go:1395-1409`) into
  the work/review lookup and returns
  `ErrInvalid: workflow run %q is missing its work/review step`
  (`workflow.go:1426`);
- boot `reconcileRun` (`recovery.go:129-175`) falls through the whole `planStore`
  block to the generic step loop and finds no work step.

The result is a run that is not resumable, not completable, and not explicable.
The two writes are in separate transactions for no reason the code states.

**Verdict: fixable in this block, with a caveat.** The strong fix is one
transaction spanning both writes, which needs a store method owning both tables —
`workflow_store.go` has none today. The **guaranteed** fix, and the one this
block should take if the transactional one proves invasive, is a boot-time
healer: a run whose only step is `kind='plan'` and which has no `workflow_plans`
row is either completed by inserting the missing plan row, or parked with a
readable reason. Note what is *not* recoverable: the approval mode is a parameter
of the create request and lives nowhere on the run. A healer must therefore
default it to `manual` **and record that it did** — inferring `auto` would start
an unattended planner nobody asked for. That is the fail-closed form.

### 3.5 CP7 — a planner in flight across a restart is discarded, not adopted

**Location:** the window is between `StartWorkflowPlanCommand`
(`master_coordinator.go:109`; CAS `status='pending' AND command_status IN
('idle','pending')` → `running`/`running`, `queries/workflow_plan.sql:10-13`) and
`PersistWorkflowPlanResponse` (`:182`). The subprocess runs at `:136` under
`plannerExecutionContext`, which deliberately outlives the HTTP request that
started it.

The restart remedy is `recovery.go:149-156`: any plan found `running` and not
`responded` is marked `invalid` with `planner_ambiguous`, and `GeneratePlan`'s
own switch (`:66`) treats `invalid` as permanent. A whole planner invocation —
minutes of wall clock, real provider budget, possibly a complete plan — is
discarded, and a human must act.

**Compare the worker path.** A worker in the same window is *adopted*: the launch
is findable by natural key (`FindSessionByProjectAndIssueID` via
`adoptOrMarkAmbiguous`, `dispatch.go:586`), its liveness is provable through the
runtime (`observeDispatchOwnership` → `IsExactSupervisedProcessAlive`, fenced by
`RuntimeLaunchID`), and `adoptLiveLaunch` makes the confirmation durable without
launching a second agent. **The planner has none of this**: no intent record
naming the subprocess, no runtime identity on the plan row, no natural key, no
adoption path.

**Verdict: NOT fixable in this block — fail closed, and make the refusal
cheaper.** A planner adoption path means giving the plan row the planner's own
launch identity (a launch-id fence plus the subprocess handle), which is a
migration and a new adapter contract — properly its own block. What this block
should do:

1. Keep `planner_ambiguous` as the verdict. It is correct: AO cannot prove
   whether a plan was produced, and guessing puts a fabricated plan under a real
   objective.
2. Fix its **write order** (that is CP32, §3.3) so the refusal is at least
   readable.
3. Stop treating it as *permanent*. `planner_ambiguous` is a statement about one
   crossed restart, not about the objective, yet it lands as
   `WorkflowPlanInvalid`, which `GeneratePlan:66` refuses forever. It should be
   reopenable by an explicit human action — under the **generation-less,
   observed-version** contract specified in §3.6, not by analogy to the
   reviewer's named-generation reopen. The planner has no launch identity to
   name, and this block does not invent one.

### 3.6 The CP7 fail-closed reopen contract (generation-less, and why)

The worker and reviewer reopens (`ReopenFailedWorkflowOutboxGeneration`) name a
generation because **`workflow_outbox` reuses one row across every retry**: the
row cycles `pending → dispatched → failed → pending → …`, so `status = 'failed'`
is satisfied by *any* failure of that row and a human resume that observed
failure F1 could arrive after F1 had been resumed, redispatched and failed again
as F2 — reopening a launch and a fresh budget epoch nobody asked for. The token
is what distinguishes F1 from F2 in a row that cannot distinguish them itself.

**A generation fence is impossible here, and this block does not invent one.** A
generation has to be minted by the thing it fences. The outbox token is minted by
the dispatch that claims the row; a planner generation would have to be minted by
the planner launch — and nothing durably records a planner subprocess: no intent
row, no launch id, no handle, no natural key (§3.5). There is no field to name
and no writer to name it, so **the rule for this block is generation-less**, and
§4.2 and §5 say so in the same words.

What *is* available is the plan row's own version. `workflow_plans.updated_at`
(migration `0101_workflow_master_plan.sql:25`) is written by every statement that
touches the row, including RP2's `FinishWorkflowPlan`
(`queries/workflow_plan.sql:33-36`), so **the second ambiguity writes a different
`updated_at` than the first**. That makes the reopen an ordinary
**observed-version compare-and-swap** — the human's action carries the exact row
version their view was rendered from — rather than a token that names a planner
attempt. The distinction is not pedantic: it is the difference between "this is
the row I looked at" and "this is the planner run I looked at", and only the
first is knowable today.

Concretely, Task 3 should implement:

```sql
-- ReopenAmbiguousWorkflowPlan
UPDATE workflow_plans
   SET status = 'pending', command_status = 'idle',
       error_class = '', validation_json = ?, updated_at = ?
 WHERE workflow_run_id = ?
   AND status = 'invalid'
   AND command_status = 'failed'
   AND error_class = 'planner_ambiguous'
   AND updated_at = sqlc.arg(observed_updated_at);
```

**The three state columns alone would NOT be sufficient, and an earlier revision
of this document wrongly said they were.** `SetWorkflowPlanApprovalMode`
(`queries/workflow_plan.sql:42-44`) is fenced only on `status != 'approved'`, so
it fires happily against an ambiguous row, updating `approval_mode` and
`updated_at` and leaving `status`, `command_status` and `error_class` exactly as
they were. A predicate on those three columns still matches afterwards, and would
still match after a second ambiguity, which writes the identical triple. The
three columns are a **type check** — "this row is in the ambiguous-terminal
state" — and `updated_at` is the part that makes it *this* ambiguous-terminal
state. Both halves are required.

- **Target state chosen deliberately:** `pending`/`idle` is the exact state
  `StartWorkflowPlanCommand`'s own CAS arms from
  (`queries/workflow_plan.sql:10-13`), and the state `GeneratePlan`'s status
  switch falls through to real generation on — the same reasoning
  `parkPlanForCapacity` records at `master_coordinator.go:369-372`. The reopen
  therefore re-enters the ordinary path, never a parallel one.
- **Idempotent:** a double-submit reopens once. The second call carries a version
  the row no longer has (and finds `error_class` cleared besides), matches zero
  rows, and its caller no-ops — the same `moved == false` convention
  `ApprovePlan:528` and `retryPlanOrFail` already use.
- **Every false answer is a refusal, never an accept.** `SetWorkflowPlanApprovalMode`
  bumping `updated_at` under a human who is mid-decision makes their reopen match
  zero rows; so does any other write to the row. The human re-reads and
  re-submits. A reopen that should have succeeded and did not is a nuisance; a
  reopen that lands on a state nobody looked at is the failure this predicate
  exists to prevent.
- **Non-looping, and bounded:** the reopen is reachable **only** from an explicit
  human action. `reconcileRun` must not call it, no wake reason may schedule it,
  and the autonomous heartbeat must not reach it — otherwise restart → reopen →
  planner → restart is an unbounded loop that spends provider budget with nobody
  watching. On top of that it is bounded the way `plannerRetryCount`
  (`master_coordinator.go:459-473`) already bounds the retry budget: count the
  run's durable `planner_ambiguous` attention stops, and past a small bound refuse
  the reopen and say so, so even a human holding the button cannot loop forever.
- **Ordering:** the reopen writes the plan row **last**, after its own
  human-readable checkpoint — the same rule CP30/CP31/CP32 exist to enforce.

**What this contract does and does not guarantee, stated plainly.** With
`updated_at` in the predicate, the ambiguity-#1-versus-#2 replay *is* caught: the
two ambiguities differ in the one column the reopen compares, so a stale reopen
matches zero rows and refuses. What remains is narrower and is a property of
timestamps, not of the rule:

- **Two writes that land on an identical stored `updated_at` are
  indistinguishable.** The value comes from the injected `c.clock()`, so a test
  clock that does not advance between two ambiguities, or a storage precision
  coarser than the interval between them, collapses two versions into one. In
  production the two are separated by a daemon restart, so the practical exposure
  is negligible — but Task 3 must implement and describe this as a **row version**,
  never as a generation or a unique token, because it is neither.
- **It identifies the row, not the planner run.** Nothing here tells anyone
  whether the discarded planner produced a plan; the reopen only decides *which
  observed state* is being reopened. Recovering the planner's own work still
  requires the launch identity CP7 lacks, which is out of scope for this block
  (§5, §6) — and the remedy for that residue is that identity, not a cleverer
  predicate over the columns that exist.

---

## 4. Proposed identity + CAS model

### 4.1 The identity tuple

Every worker-launch transition should name, at minimum:

```
(run_id, task_id?, step_id, attempt_id, attempt_generation, owner, runtime_instance)
```

- **`task_id`** is present for every run dispatched from a master objective and
  **absent for a standalone run** — that is what the `?` marks, and it is the one
  optional member of the tuple. It is not carried in the dispatch call at all: it
  is read from `run.PlannedTaskID`, stamped inside the child-run creation
  transaction (`workflow.go:905`) and made unique by
  `idx_workflow_runs_planned_task`. Its reverse binding is
  `workflow_tasks.execution_run_id` (§2.1). Because that binding is unique and
  monotonic, `task_id` needs **no** generation of its own to identify an
  execution — the pair is already 1:1 for life, which is why §2.1 rejects adding
  `execution_run_id` to the task-state predicates. What `task_id` does need, on
  the one state it can re-enter, is an **attempt** discriminator:
  `WorkflowTaskAttention.Attempt` for the `running → needs_attention → running`
  cycle.
- **`attempt_generation`** is the new field. Definition, chosen so it needs no
  new column: **the id of the `intent` dispatch-boundary record written by
  `beginWorkerDispatch`** (`dispatch_state_machine.go:426`). It is durable before
  anything is launched, unique per launch pass, reconstructable from the dispatch
  records, and it is the exact analogue of the reviewer's generation (the id of
  the `review_dispatch_authorized` checkpoint whose claim took the row,
  `review_dispatch.go:1149`). It rides in `workflow_outbox.dispatch_generation`,
  which already exists and is already table-generic.
- **`runtime_instance`** is `RuntimeLaunchID` — never `RuntimeHandleID`, never
  `SessionID` alone. A tmux pane can be recreated and a session row outlives its
  process; only the launch id, which the supervisor carries in its own argv
  (`tmux.go:1466`), separates "this session exists" from "the process I started
  is alive".
- **`owner`** is the `(UserID, ProviderProfileID)` pair from `resolveRuntimeEnv`,
  recorded at intent and re-compared at confirmation: a confirmation whose owner
  disagrees with its own intent is evidence, not a detail.

### 4.2 CAS predicate per transition

`✓` = today's predicate is already sufficient. **Bold** = the change this block
proposes.

| Transition | Function | Predicate today | Proposed predicate |
| --- | --- | --- | --- |
| S1 enqueue | `EnqueueWorkflowOutboxEntry` | unique `(run_id, idempotency_key)` | ✓ |
| S4 pending → dispatched | `dispatchFromPending`, `dispatch.go:248` | `id = ? AND status = 'pending'`, clearing both generation tokens | **`ClaimWorkflowOutboxDispatch(id, now, attempt_generation)`** — same `status='pending'` CAS, stamping the intent-record id. Requires the intent id to be minted before S4 (i.e. S5's record write moves ahead of S4, or the id is generated up front and the record written under it) |
| S5 attempt open | `openWorkerAttempt`, `dispatch_state_machine.go:446` | positional: `attempts[len-1].Outcome == ""` | **store-enforced `(step_id, outcome IS NULL)`**, so two passes cannot both adopt the same open attempt; the loser re-reads |
| S5 intent record | `recordDispatchBoundary`, `:308` | insert-only | ✓ — it is the generation source and must stay insert-only |
| S7 launch | `LaunchWorker`, `:496` | none (external) | ✓ — no durable write, by design |
| S9 confirmation | `confirmWorkerDispatch`, `dispatch.go:734` | insert-only boundary | **insert, predicated on `outbox.dispatch_generation = attempt_generation`**: a pass that no longer owns the claim must not confirm over the pass that does |
| S10 outbox dispatched → acknowledged | `dispatch.go:794` | `id = ? AND status = ?` | **`… AND dispatch_generation = attempt_generation`** |
| S10 step ready → running | `dispatch.go:799` | `id = ? AND state = 'ready'` | **`… AND session_id = <confirmed session>`**, so RUNNING is licensed by the confirmation and not merely by order of execution |
| Launch failure → outbox failed | `recordDispatchFailure`, `dispatch.go:817` | `id = ? AND status = ?` | **`FailWorkflowOutboxWithGeneration(id, expected, failure_generation=<launch-record id>, dispatch_generation=attempt_generation)`** — a paused pass must not stamp its failure on somebody else's live launch |
| Bounded retry → outbox pending | `recordWorkerLaunchFailure`, `worker_launch_recovery.go:310` | `id = ? AND status = ?` | **`ReleaseDispatchedWorkflowOutboxGeneration(id, attempt_generation)`** |
| Human resume | `resumeWorkerLaunchAfterFailure`, `worker_launch_recovery.go:516`, outbox write at `:607` | reads the latest failure record, then a plain `failed → pending` CAS | **`ReopenFailedWorkflowOutboxGeneration(id, failure_generation)`** — reopen the failure the human actually saw, not whichever one is current |
| Attempt finalization | `UpdateWorkflowAttemptOutcome`, `worker_progress.go:682` and `dispatch.go:578` | `id = ?` (last writer wins) | **`ClaimWorkflowAttemptOutcome`** (`workflow_store.go:592`, predicate `id = ? AND finished_at IS NULL`) — it already exists and is already used elsewhere; it is simply not used here |
| Adoption | `adoptLiveLaunch`, `dispatch_reconcile.go:674` | live-ownership probe | **`… AND runtime_launch_id = evidence.RuntimeLaunchID`** — adopt the launch that is alive, not merely a session under the same key |
| Plan approve (W1) | `ApproveWorkflowPlan` | `status = 'validated'` | ✓ |
| Plan approve (W2–W4) | `ApprovePlan`, `master_coordinator.go:534-539` | ordering only | **re-derive from `plan.status='approved'` on every entry**, so the early exits at `:517`/`:528` fall *through* to them rather than over them (CP13/CP14) |
| Plan retry arm | `retryPlanOrFail`, `:431` | `FinishWorkflowPlan` status CAS | ✓ predicate; **wrong order** — budget row first (CP30) |
| Plan terminal | `failPlan` `:477`, `recovery.go:151` | `FinishWorkflowPlan` status CAS | ✓ predicate; **wrong order** — reason row first (CP31/CP32) |
| Objective creation | `CreateObjectiveRun`, `:41`/`:44` | two independent inserts | **one transaction**, or a boot healer that fails closed on the unrecoverable approval mode (CP1) |
| **T1** child-run creation | `createRunWithPlanArtifact`, `workflow.go:868`, stamp at `:905` | run + steps in one transaction; `planned_task_id` unique via `idx_workflow_runs_planned_task` | ✓ — the strongest binding in the master path. Do not touch it |
| **T2** task → child-run binding | `SetWorkflowTaskExecutionRun`, `master_coordinator.go:1216` | `id = ? AND execution_run_id IS NULL AND state = 'eligible'` | ✓ predicate. **Check the returned boolean**: a claim that moved zero rows on the creation path means somebody else bound this task, and the caller must not proceed to `StartRun` as if it had won |
| **T3** recovery re-bind | `dispatchMasterTask`, `master_coordinator.go:1121-1124`; `reconcileMasterTasksOnce`, `:743-747` | same T2 claim, **result discarded** (`_, _ =`) | **Verify instead of claim.** Read the task row; if `execution_run_id` is non-NULL and ≠ the run `FindWorkflowRunByPlannedTask` returned, that is a contradiction between two durable bindings and must stop with a readable reason, not be silently ignored |
| **T4a** terminal mirror | `reconcileMasterTasksOnce` `:860`/`:872`/`:896`; `syncCancelledTask`, `branch_lock_recovery.go:111` | `id = ? AND state = ?` | ✓ **sufficient — change nothing.** The source state is a terminal child run, which is irreversible (`domain/workflow.go:87-88`), so a stale read of it is still true. Do **not** add `execution_run_id`: it is constant for the life of the task and would fence nothing (§2.1) |
| **T4b** task park | `ParkWorkflowTaskForAttention`, `task_integration_route.go:570` | `id = ? AND state = 'running'` — satisfied by *every* generation of running | **`… AND <attempt counter> = <the attempt this conflict was observed on>`**, so a park computed for attempt N cannot land on attempt N+1 after a human resume. The counter is `WorkflowTaskAttention.Attempt`, already written at `:568` and already preserved across resume (`queries/workflow_plan.sql:74-77`) |
| **T4c** task resume | `ResumeWorkflowTaskFromAttention`, `task_integration_route.go:667` | `id = ? AND state = 'needs_attention'` | **same addition, symmetric**: resume the stop the human read, not whichever one is current. Both callers already treat a `false` return as "somebody else decided" (`:575-580`, `:671-673`), so tightening the predicate needs no new caller-side branch |
| — | *implementation note for T4b/T4c* | `Attempt` lives inside `attention_json` | Reaching it from a predicate needs either `json_extract(attention_json,'$.attempt')` or promoting the counter to its own column. This is the one place in the master path where a new column may be the better answer than reusing what exists — the counter is already computed by the caller, only unenforced |
| **CP7** ambiguous plan reopen | new `ReopenAmbiguousWorkflowPlan` (§3.6) | does not exist; the state is permanently `invalid` | **`workflow_run_id = ? AND status='invalid' AND command_status='failed' AND error_class='planner_ambiguous' AND updated_at = <observed>`** — **generation-less by necessity** (nothing mints a planner generation, §3.6); the three state columns are the type check and `updated_at` is the observed-version CAS. Human-initiated only, bounded by the run's `planner_ambiguous` stop count. It is a row version, **not** a generation, and must not be described as one |

### 4.3 The invariant these predicates buy

> No durable transition off a launch may be taken by a pass that does not hold
> the generation that launch was made under.

That invariant holds for the reviewer today and holds for nothing on the worker
path. Most of the remaining ambiguity in §1.2 and §1.3 is a consequence of its
absence — with one case that must stay ambiguous no matter what:
`errLaunchWithoutEvidence`. A launcher that reports success and names nothing has
proven nothing, and no predicate can rescue that.

---

## 5. Disposition summary

| Defect | Location | Disposition in this block |
| --- | --- | --- |
| CP13 | `master_coordinator.go:517`, `:528` vs `:534-536` | **Fix.** Make W2–W4 re-derivable from `plan.status='approved'` |
| CP14 | same early exits vs `:539` | **Fix.** Same change; this is the consequential half |
| CP30 | `master_coordinator.go:431` before `:433` | **Fix.** Reorder: budget row before the reset |
| CP31 | `master_coordinator.go:477` before `:487` | **Fix.** Reorder: reason before terminal row |
| CP32 | `recovery.go:151` before `:155` | **Fix.** Same reorder |
| CP1 | `master_coordinator.go:41`/`:44` | **Fix** by one transaction if the store allows; otherwise a boot healer that defaults the approval mode to `manual` and records that it did. Never infer the mode silently |
| CP7 | `master_coordinator.go:109`–`:182`; remedy at `recovery.go:149` | **Fail closed.** Keep `planner_ambiguous`; fix its order (CP32); make it reopenable under the **generation-less, observed-version, human-only, bounded** contract in §3.6 — CAS on the three state columns **plus `updated_at`**. Explicitly *not* a named generation: nothing mints one for a planner run. A planner adoption path is a separate block |
| Task park/resume ABA — `running` recurs and no predicate names the attempt | `task_integration_route.go:570`, `:667` | **Fix.** Add the `WorkflowTaskAttention.Attempt` discriminator to the park and resume predicates (§2.1, §4.2 T4b/T4c). The terminal mirrors (T4a) need **no** change, and `execution_run_id` must **not** be added anywhere — it is constant and would fence nothing |
| Recovery re-bind discards its own claim result | `master_coordinator.go:1124`, `:746` | **Fix.** Verify the existing binding and stop on a mismatch instead of re-claiming into a discarded boolean |
| Worker attempt generation absent | `dispatch_state_machine.go:446`; `dispatch.go:248`/`:794`/`:817` | **Fix.** Adopt the existing outbox generation columns and store methods; no migration, no reviewer change |
| Attempt finalization last-writer-wins | `worker_progress.go:682`, `dispatch.go:578` | **Fix.** Switch to `ClaimWorkflowAttemptOutcome` |

## 6. What steps 2–6 must not do

- Do not edit `review_dispatch.go`, `review_launch_recovery.go`,
  `review_authority.go`, or migrations `0135`–`0138`. Adopt their primitives; do
  not reshape them. The single exception is a **test-proven** shared regression,
  and only as far as that proof reaches.
- Do not resolve `errLaunchWithoutEvidence` by launching. A second worker on one
  worktree is worse than a stopped run.
- Do not turn `planner_ambiguous` into a guess. Reopenable is the goal;
  re-derivable it is not — and the reopen must stay human-initiated and bounded,
  or restart → reopen → planner → restart becomes an unattended loop spending
  provider budget (§3.6).
- Do not describe the CP7 reopen as generation-fenced in code comments, commit
  messages or UI copy. It is not, it cannot be until the planner has a launch
  identity, and §3.6 states exactly what it does and does not guarantee.
- Do not add a column where an existing table-generic one already carries the
  same meaning. `workflow_outbox.dispatch_generation` and `.failure_generation`
  are already the right shape for the worker path.


---

## 7. What shipped, and where it differs

Every row of §5's disposition table is implemented. This section is the map from
the contract to the code, and it exists mainly for the two rows where the
implementation is not literally what §4.2 wrote down.

### 7.1 The plan segment

| Defect | Where it landed |
| --- | --- |
| CP13 / CP14 | `master_coordinator.go` — `ApprovePlan` now `switch`es on the plan status and falls THROUGH to `convergeApprovedPlan`, which re-derives W2–W4 from `plan.status='approved'` alone. `recovery.go`'s approved-plan arm calls it too, and that is the only place a CP14 crash is ever healed: nothing re-enters `ApprovePlan` once a plan is approved |
| CP30 | `retryPlanOrFail` — the `planner_retry_scheduled` checkpoint is written before `FinishWorkflowPlan` re-arms the plan |
| CP31 | `failPlan` — `recordAttentionStopWithState` before the terminal `FinishWorkflowPlan` |
| CP32 | `recovery.go` — `recordAttentionStop(planner_ambiguous)` before the terminal `FinishWorkflowPlan` |
| CP1 | Closed rather than healed for new objectives: `Store.CreateObjectiveRunWithPlan` writes the run, its plan step and its `workflow_plans` row in ONE transaction, asserted at the call site through the optional `objectiveRunCreator` interface. `healOrphanedObjectiveRun` (`master_coordinator.go`, called from `reconcileRun`) still heals the rows a pre-fix daemon left on disk, defaulting the unrecoverable approval mode to `manual` and recording `objective_plan_recovered` that it did |
| CP7 | `planner_ambiguous_reopen.go` + `Store.ReopenAmbiguousWorkflowPlan`. Human-only, bounded at `maxAmbiguousPlanReopens`, and an observed-version compare-and-swap on `updated_at` — **not** a generation, for the reasons §3.6 gives. Reachable only from `POST /api/v1/workflows/{id}/plan/reopen` and `ao workflow recover plan`; nothing in `reconcileRun`, `ContinueRun`, any wake reason or the heartbeat can reach it |

### 7.2 The worker segment

`attempt_generation` is exactly what §4.1 defines: the id of the `intent`
dispatch-boundary record, minted in `dispatchFromPending` before the row is
claimed and used as that record's own id, stamped into
`workflow_outbox.dispatch_generation` by `ClaimWorkflowOutboxDispatch`. No new
column, no migration, no reviewer change.

Every transition off `dispatched` now names it: the confirmation
(`stillOwnsWorkerDispatch` plus the fenced acknowledge), the acknowledge
(`AcknowledgeWorkflowOutboxDispatch`), the failure
(`FailWorkflowOutboxWithGeneration`), the bounded retry's release
(`ReleaseDispatchedWorkflowOutboxGeneration`) and the human resume
(`ReopenFailedWorkflowOutboxGeneration`). `openWorkerAttempt` is
store-enforced through `ClaimOpenWorkflowAttempt`; attempt finalization goes
through `ClaimWorkflowAttemptOutcome`; `adoptLiveLaunch` refuses a session whose
`RuntimeLaunchID` disagrees with the launch AO recorded.

**Difference 1 — a fallback provider does not mint a second generation.** §4.2
describes one generation per launch pass and is silent on
`attemptWorkHarness`'s fallback recursion. The generation is minted per
**claim**, not per provider attempt, and threaded through the fallback: a
fallback is a second provider under ONE claim, and minting a second token for it
would fence the fallback out of the very row its own predecessor holds. The
fallback still writes its own `intent` record, under a fresh record id.

**Difference 2 — `AcknowledgeWorkflowOutboxDispatch` takes the expected status.**
§4.2 writes the acknowledge as a transition from `dispatched`. It is also
reached from `failed`, by `resumeWorkerLaunchAfterFailure`'s adoption of a
session that turned up after a durable failure. Both are real transitions to
`acknowledged`, so the statement compare-and-swaps against the status its caller
actually read. The generation fence is unchanged and is vacuous on the `failed`
path by construction: a failed row carries no claim.

Two smaller points worth naming because they are not in §4.2:

- **`worker_progress.go`'s attempt write is two different writes.** The
  finalization goes through the claim; the deliberate later REFINEMENT of an
  already-concluded attempt's error class (to `ambiguous_worker_state`) keeps the
  unconditional update, because the claim would match zero rows and silently drop
  it — hiding the ambiguity from the UI, which is the bug that write exists to
  fix.
- **RUNNING is a predicate, not an ordering.** `StartWorkflowStepForSession`
  moves the step `ready → running` only while it durably holds the session the
  confirmation just wrote.

### 7.3 The master-task segment

T1 and T4a are untouched, as §4.2 requires. T2's claim result is checked and a
lost claim stops instead of proceeding to `StartRun`; T3 verifies the existing
binding and refuses on a disagreement (`bindTaskToChildRun`,
`reportTaskBindingContradiction`) instead of discarding a boolean. T4b/T4c carry
the `WorkflowTaskAttention.Attempt` discriminator, read from the predicate with
`json_extract(attention_json,'$.attempt')` — the reuse option §4.2's
implementation note offers, not a new column. `execution_run_id` was added
nowhere.

### 7.4 The approved-head provenance gap

Not in the original audit, and the production failure this block was finished
under: a run whose review approved a target, whose fix cycles advanced the
branch, and whose verification then stopped because AO could not name the commit
that approval was read at. `pinReviewTargetHead` had already closed it for new
cycles; the runs already on disk were permanently inert.

`approved_head_recovery.go` adds exactly two mechanisms and nothing between
them:

- **Reconstruction.** A clean worktree's `WorkspaceFingerprint` is a pure
  function of its commit, so the commit is recovered by recomputing that same
  function forwards over the branch until it matches. It is a proof, not an
  inference, it cannot produce a false positive (including for an approval read
  at a dirty tree, whose fingerprint no clean-tree hash can equal), and its
  answer is recorded so rule 3 of `approvedHeadSHA` resolves it thereafter like
  any other observation. It runs as rule 5, after every durable lookup.
- **An operator recovery** (`RecoverUnprovableApprovedHead`) when it is not:
  human-only, bounded, and it DISCARDS the unlocatable approval and asks for one
  fresh review of the live workspace. It never attests a commit, never verifies
  code no reviewer read, and never changes what AO proved — the provenance record
  still says `UNKNOWN`.

Where the head genuinely cannot be proved the behaviour is unchanged and
fail-closed. What changed is that the stop is now named
(`verify_approved_head_unprovable`), carries a human action, and has a way out.

### 7.5 Deferred, deliberately

- **A planner launch identity.** CP7 is fail-closed, not adopted, exactly as
  §3.5 concludes: giving the plan row the planner's own launch id plus a
  subprocess handle is a migration and a new adapter contract, and it is the only
  thing that would let a discarded planner's work be recovered rather than
  redone. Its own block.
- **`errLaunchWithoutEvidence`.** Still ambiguous, still never resolved by
  launching, and no predicate can rescue it (§4.3).
- **The `ownedExecution.Live()` reading of an unanswered liveness probe.**
  Unchanged on purpose: `AGENTS.md`'s hard rule is that a failed or unknown
  runtime probe is never proof a session is dead, and the launch-id fence added
  here refuses a stated disagreement rather than converting silence into one.
