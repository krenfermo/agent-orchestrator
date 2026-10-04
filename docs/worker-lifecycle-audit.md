# Worker lifecycle audit — durable write points, crash windows, and the CAS model to adopt

**Status:** design contract. This document is the audit the rest of the worker
lifecycle work implements against. It changes no behaviour by itself; every
"proposed" item below is a commitment about what the next implementation step
writes, not a description of what the code does today.

**Reviewer scope.** This audit covers the whole path that produces a work
step's result and hands it to a reviewer — see **Scope** below; what it is *not*
about is the reviewer's own lifecycle. It takes the
merged reviewer generation-conditioned CAS model as its reference and it
**proposes no change to the reviewer lifecycle** — not to `review_launch_*`, not
to `review_authority.go`, not to migrations `0135`–`0138`, and not to the
reviewer's semantics of cycles, epochs or the authority pointer. Where the
worker path needs the same primitive, it adopts the existing table-generic store
method rather than editing, generalising or re-shaping the reviewer's use of it
(§6.1). The single condition under which that stance changes is narrow and
evidential: **if a later implementation step's tests prove a shared regression**
— that is, a worker-side change demonstrably breaking reviewer behaviour, or a
defect shown by test to live in a primitive both paths share — then the reviewer
lifecycle may be touched, and only as far as that proof reaches. Absent such a
test, no reviewer lifecycle change is in scope, and nothing in §6 may be read as
authorising one.

**Scope:** the whole path a unit of work travels, from the moment an objective
exists to the moment a reviewer is dispatched against its result —

> objective/workflow creation → plan generation → plan persistence/approval →
> workflow run/step state changes → task becomes dispatchable → worker dispatch
> intent → launch → dispatch confirmation → RUNNING → worker terminal/idle →
> completion evidence → review transition

— as it stands on `feat/engineering-control-center`. Nothing in that span is
out of scope for the *audit*; §6.7 scopes only what the **implementation** step
is allowed to change, and §6.9 states, write by write, which plan-segment gaps
that step closes and which it deliberately does not.

**Two entry paths reach the same work step**, and both are audited:

- **Path A — standalone run.** `CreateRun` → `StartRun`. The plan step is a
  deterministic template expansion with no model call at all
  (`BuildPlanArtifact`, `plan.go:58`), executed synchronously inside `StartRun`.
- **Path B — master objective.** `CreateObjectiveRun` → `GeneratePlan` (a real
  planner subprocess) → `finalizeGeneratedPlan` → `ApprovePlan` →
  `reconcileMasterTasksOnce` → `dispatchMasterTask` → `createSingleTaskRun` →
  `StartRun`. Every child run of a master objective re-enters Path A's
  `StartRun` at its last hop, so Path B is a prefix of Path A, not an
  alternative to it.

**Label map.** `P#` is the plan segment (§2.0), `W#`/`R#`/`H#`/`A#` the
work-step launch segment (§2.1), `F#` the fix cycle (§2.3). Crash windows are
`CP#` for the plan segment (§3.0) and `C#`/`CF#` for the rest (§3.1).

**Primary sources.** Every claim below is anchored to code on this branch. The
files that own the path are:

| File | What it owns |
| --- | --- |
| `backend/internal/workflow/dispatch.go` | The dispatch algorithm, outbox transitions, adoption, confirmation (phases 3–4), the durable failure shape |
| `backend/internal/workflow/dispatch_state_machine.go` | The phased launch: intent, launch, confirmation records; the derived `WorkerDispatchStatus` |
| `backend/internal/workflow/worker_launch_recovery.go` | Launch-failure classification, bounded automatic retry, the human-driven reopen |
| `backend/internal/workflow/dispatch_reconcile.go` | Crash reconciliation: six contradictions, six answers, plus the "unprovable ⇒ stop" rule |
| `backend/internal/workflow/worker_progress.go` | Work observation: terminal/idle/blocked evidence, completion, the `start_review` hand-off |
| `backend/internal/workflow/attempt_reaper.go` | Closing attempt rows abandoned by a crash |
| `backend/internal/workflow/work_adoption.go` | Adopting an existing commit as a work step's result |
| `backend/internal/workflow/review_dispatch.go`, `cascade.go` | The review transition that consumes the work step's completion evidence |
| `backend/internal/workflow/fix_dispatch.go` | The fix-cycle dispatch: per-cycle outbox key, prompt delivery into the work step's **existing** session, the fix attempt row |
| `backend/internal/workflow/fix_progress.go` | Fix-cycle observation, the fix step/run transition, and the fix attempt's finalization |
| `backend/internal/workflow/fix_delivery_recovery.go` | Restart classification for a fix delivery that crossed a crash |


The plan segment's own owners, added in this revision because the audit now
starts where the objective does:

| File | What it owns |
| --- | --- |
| `backend/internal/httpd/controllers/workflow.go` | The create request: which constructor runs, then the owner stamp and the policy freeze that follow it (`create`, `:888-933`; `stampOwner`, `:827`) |
| `backend/internal/workflow/workflow.go` | `createSingleTaskRun` (the run + its six step rows), `StartRun` (the plan step's synchronous execution and the plan→work unblock), `ContinueRun` (the resume entry point every wake uses) |
| `backend/internal/workflow/plan.go` | The plan artifact itself: deterministic construction, marshalling, the work prompt, and `promptForRun`'s restart-safe rebuild |
| `backend/internal/workflow/master_coordinator.go` | The objective path: plan command arming, planner invocation, validation/finalization into task rows, approval, the per-pass task reconcile, and child-run dispatch |
| `backend/internal/workflow/execution_policy_resolve.go` | `ApplyExecutionPolicySnapshot` (the one-time policy freeze) and `maybeKickoffAutonomousPlanning` (the autonomous kickoff wake) |
| `backend/internal/workflow/child_ownership.go` | The child run's owner stamp and the hard pre-dispatch ownership gate |
| `backend/internal/workflow/recovery.go` | Boot reconciliation's **plan branch** — the planner's only crash resolver (`reconcileRun`, `:114-147`) — and the work-step branch that re-enters dispatch |
| `backend/internal/storage/sqlite/queries/workflow_plan.sql` | Every plan/task predicate quoted in §2.0, verbatim |

The reviewer counterparts the comparison in §5 is taken from:
`review_launch_phases.go`, `review_launch_recovery.go`, `review_authority.go`,
and migrations `0135`–`0138`.

---

## 1. The durable substrate

Twelve durable homes carry the state of this path. The first seven carry the
work step's own lifecycle; the last five carry the plan segment that produces it. Nothing else is state; every
in-memory value in the dispatch path is a derivation of these.

| Home | Rows written by the worker path | Mutable? |
| --- | --- | --- |
| `workflow_outbox` | One row per work step, keyed `workflow-step-spawn:<stepID>` (`dispatch.go:90-92`) | **Yes** — `pending → dispatched → acknowledged` / `failed`, and back to `pending` on retry. Reused forever under one idempotency key |
| `workflow_dispatch_checkpoints` (0133/0134) | One row per launch boundary: intent, failure, unconfirmed, confirmation, reconciliation | Append-only |
| `workflow_checkpoints` | Phase markers + JSON payloads: `worker_dispatched`, `worker_launch_error`, `worker_launch_human_retry`, `worker_launch_unconfirmed`, `worker_dispatch_ambiguous`, `worker_observed_*`, `attempt_reaped_orphaned`, `work_commit_adopted` | Append-only |
| `workflow_attempts` | One row per launch attempt; `outcome IS NULL` **means** "an attempt is in flight" and every downstream guard reads it that way (`attempt_reaper.go:14-31`) | Mutable (outcome/finished_at) |
| `workflow_steps` | `state`, `session_id`, `review_run_id` | Mutable |
| `workflow_runs` | `state` | Mutable |
| `branch_locks`, `sessions` | Execution ownership outside workflow's own tables | Mutable, owned elsewhere |
| `workflow_plans` | One row per master objective, created `pending`/`idle` (`workflow_plan.sql:1-5`). Carries `status`, `command_status`, the context manifest, the generated plan JSON, the validation verdict, the plan hash and the approval mode | **Yes** — `pending → running → validated → approved`, plus `invalid`/`rejected`, and back to `pending` on a capacity park or a bounded planner retry. Reused forever: one row per objective |
| `workflow_tasks` (+ `workflow_task_dependencies`, `workflow_task_relationships`) | One row per planned task, written once by `InsertWorkflowTasks`; `state`, `scope_json` (write intent, waiting reason, execution strategy) and `execution_run_id` are mutated afterwards | Mutable. `UNIQUE(workflow_run_id, plan_step_id)`, `UNIQUE(execution_run_id)` (`0101_workflow_master_plan.sql:45-47`) — the natural keys every plan-segment recovery reads from |
| `workflow_runs.user_id`, `workflow_runs.policy_snapshot` | The owner stamp and the frozen execution policy, both written **after** the run row exists, by a second and a third statement | Mutable in principle; written exactly once by contract (`UpdateWorkflowRunPolicySnapshot`'s own comment, `workflow.sql:75-82`) |
| `workflow_wake_schedules` | The durable wake that carries every unattended resume — the autonomous kickoff, the planner capacity park, the bounded planner retry, the master heartbeat | Mutable, upserted by idempotency key. Every write to it is **best-effort**: `scheduleWake` logs and swallows (`dispatch.go:416-422`) |
| `workflow_task_worktrees` | The task's isolated worktree record, in the parallel-worktree execution mode | Mutable, and **not written by this path**: `internal/workspace` owns it, and the workflow side's port exposes only `MarkIntegrated`/`Cleanup`/`Preserve`/`Reconcile` (`workflow.go:450-462`). It is named here so §2.0 is not read as claiming the plan segment provisions worktrees — it does not |

Two properties of this substrate drive everything below:

1. **The outbox row is reused.** One row, one key, `pending → dispatched →
   failed → pending → …` for the life of the step. "This row is `failed`" is
   therefore true of *many different failures*, and "this row is `dispatched`"
   is true of *many different dispatches*. No worker-path CAS today names
   *which* one.
2. **Every worker CAS predicate today is `(id, expected_state)`.** That is
   sufficient against a concurrent writer only when a state value can be
   entered at most once. On a reused row it is not: a stale writer that pauses
   across a full turn of the cycle finds its expected value present again and
   wins a write it no longer owns.

---

## 2. Durable write points, in path order

Three segments, in the order a unit of work travels them: the plan segment
(§2.0), the work-step launch segment (§2.1), and the fix cycle (§2.3). "Guard"
is, throughout, the predicate the write is actually made under **today**.

### 2.0 The plan segment (P writes)

Everything from "an objective exists" to "the work step is `ready` and dispatch
is entered". `P#` labels are used by §3.0, §5.1 and §6.9. **Path** is A
(standalone run), B (master objective) or both; the rows are in execution
order, and where the two paths interleave differently the row says so.

The same rule §3 applies to the rest of the document applies here: **no row
collapses two durable mutations that a crash between them would leave in
distinguishable persisted states.** Where a helper writes more than one row
(`recordAttentionStopWithState`, `stopPlanningStep`), each row is its own `P`.

#### 2.0.1 Group 1 — objective / workflow creation

| # | Path | Write | Site | Guard today |
| --- | --- | --- | --- | --- |
| P1 | A | Run row + **all six** step rows (`plan`, `work`, `review`, `fix`, `verify`, `advance`), run `pending`, step 1 `ready`, the plan step already carrying its `artifact_json` | `createSingleTaskRun`, `workflow.go:872` → `store.CreateWorkflowRun`, `workflow_store.go:30` | Insert. **Atomic**: run and steps share one `inTx`, so no crash can leave a run without its steps |
| P1′ | B | Run row + **one** step row (`plan`, `ready`, `artifact_json = "{}"`) | `CreateObjectiveRun`, `master_coordinator.go:38` | Same single transaction. A master run deliberately has no work/review step of its own |
| P2 | B | `workflow_plans` row, `status='pending'`, `command_status='idle'` | `master_coordinator.go:41` → `InsertWorkflowPlan`, `workflow_plan.sql:1-5` | Insert. **Separate transaction from P1′** — this is the segment's first real crash boundary (CP1) |
| P3 | both | Run owner stamped | `stampOwner`, `httpd/controllers/workflow.go:835` → `SetWorkflowRunOwner` | Unconditional overwrite by run id. Skipped entirely when no identity is resolvable |
| P4 | both | **Execution policy frozen** into `policy_snapshot` — the routing priorities and `AutonomousMode` this run will obey for its whole life | `ApplyExecutionPolicySnapshot`, `execution_policy_resolve.go:142` → `UpdateWorkflowRunPolicySnapshot`, `workflow.sql:75-82` | `WHERE id = ?` — **no expected value, no generation**. By contract written exactly once; nothing enforces that |
| P5 | B | Autonomous kickoff wake (`ReasonAutonomousProgress`) — the only thing that ever starts an autonomous objective | `maybeKickoffAutonomousPlanning`, `execution_policy_resolve.go:171` → `scheduleWake` | Best-effort upsert by idempotency key; a failure is logged and swallowed (`dispatch.go:416-422`). Gated on the snapshot P4 just wrote |

#### 2.0.2 Group 2 — plan generation (Path B only)

`GeneratePlan` is re-entered from an HTTP request and from the wake poller via
`ContinueRun` (`workflow.go:1254-1259`), so every row below must be read as
something two callers can reach.

| # | Write | Site | Guard today |
| --- | --- | --- | --- |
| P6 | **Plan command armed**: `status pending → running`, `command_status → running`, provider/model/context manifest recorded | `master_coordinator.go:106` → `StartWorkflowPlanCommand`, `workflow_plan.sql:10-13` | **A real CAS**: `WHERE workflow_run_id = ? AND status='pending' AND command_status IN ('idle','pending')`. A lost race returns `ErrPlanLocked`, never a second planner. This is the plan segment's strongest predicate — and it is *coarse-state*, not generation-conditioned: it cannot tell one arming from the next |
| P7 | Plan step `ready → running` | `master_coordinator.go:115` | `(stepID, expected=ready)` |
| — | **The planner subprocess runs** (`c.planner.Generate`, `:127`) | — | **Not a durable workflow write.** Minutes long, on a workflow-owned context that survives the caller's; the irreducible ambiguity of this segment (CP6) |
| P8 | Planner response persisted: `command_status running → responded`, `generated_plan_json` set | `master_coordinator.go:179` → `PersistWorkflowPlanResponse`, `workflow_plan.sql:15-17` | CAS on `status='running' AND command_status='running'`. `!moved` ⇒ `ErrPlanLocked`, nothing overwritten |
| P9 | **Normalized** plan re-persisted, when normalization changed the bytes | `finalizeGeneratedPlan`, `master_coordinator.go:199` | Same CAS — but on `status='running' AND command_status='running'`, which P8 has already left at `responded`. **This statement therefore updates zero rows every time it runs**, and its result is discarded (`_, _ =`). The row keeps the planner's raw JSON; the normalized form survives only in memory for the rest of this call. Functionally benign — `NormalizeAndValidatePlan` is pure, so every later reader (including RP1) re-derives the identical normalized plan from the raw bytes — but recorded as its own row because a reader must not infer from this line that the stored plan is the normalized one, and because a future non-deterministic normalizer would turn this from cosmetic into a correctness bug |
| P10 | **Task rows + dependency edges** inserted (`state = 'eligible'` for the first task with no dependencies, `'blocked'` for the rest), each carrying its `scope_json` — the classified write set, execution strategy and **write intent** | `master_coordinator.go:253` → `InsertWorkflowTasks`, `workflow_plan_store.go:70` | Insert-or-ignore inside **one transaction** covering tasks and edges. Idempotent under replay only in the weak sense §3.0 CP9 describes: the ids are freshly minted on every pass, so a replay's rows lose to `UNIQUE(workflow_run_id, plan_step_id)` and are silently dropped |
| P11 | Pairwise task relationships (`functional_dependency` / `probable_write_conflict` / `independent`) — what parallel dispatch reads to decide whether two tasks may run at once | `master_coordinator.go:259` → `ReplaceWorkflowTaskRelationships`, `workflow_plan_store.go:264` | Upsert on `(task_id, related_task_id)`, one transaction. **FK-bound** to `workflow_tasks(id)` with `foreign_keys(ON)` (`db.go:37`) — see CP9 |
| P12 | Plan finished: `status → validated`, `command_status → completed`, validation verdict + plan hash | `master_coordinator.go:262` → `FinishWorkflowPlan`, `workflow_plan.sql:19-22` | CAS on `status='running' AND command_status IN ('running','responded')` |
| P13 | Approval mode rewritten to `auto` when it was *policy*, not the client, that decided | `master_coordinator.go:278` → `SetWorkflowPlanApprovalMode` | `WHERE workflow_run_id = ? AND status != 'approved'`. Deliberate: an auto-approval must be inspectable as one |
| P14 | **Invalid-plan branch**: `status → invalid`, `command_status → failed`, error class `planner_policy_violation` | `master_coordinator.go:202` | Same CAS as P12. `invalid` is terminal — `GeneratePlan`'s own switch treats it as a permanent no-op |
| P15 | Invalid-plan branch: run `pending → needs_attention` | `master_coordinator.go:204` | `(runID, expected=run.State)`, only from `pending` |
| P16 | Invalid-plan branch: plan step `running → waiting` | `stopPlanningStep`, `master_coordinator.go:440` | `(stepID, expected=running)` |

#### 2.0.3 Group 3 — plan persistence / approval

| # | Write | Site | Guard today |
| --- | --- | --- | --- |
| P17 | Manual hold: run `pending → waiting` (a validated plan awaiting a person) | `master_coordinator.go:283` | `(runID, expected=run.State)` |
| P18 | Manual hold: plan step `running → waiting` | `master_coordinator.go:287` | `(stepID, expected=running)` |
| P19 | **Plan approved**: `status validated → approved`, `approved_at` set | `ApprovePlan`, `master_coordinator.go:466` → `ApproveWorkflowPlan`, `workflow_plan.sql:24-26` | CAS on `status='validated'`. `!moved` ⇒ return the current detail, no error. The single durable fact that licenses task dispatch |
| P20 | Plan step `waiting → running` | `master_coordinator.go:476` | `(stepID, expected=waiting)` |
| P21 | Plan step `running → completed` | `master_coordinator.go:478` | `(stepID, expected=running)` |
| P22 | Run `waiting`/`pending → running` | `master_coordinator.go:481` | `(runID, expected=run.State)` |
| P23 | Plan rejected: `status → rejected`, then the run is cancelled | `RejectPlan`, `master_coordinator.go:497` → `RejectWorkflowPlan`, `workflow_plan.sql:32-34`, then `CancelRun` | CAS on `status IN ('pending','validated','invalid')`; the result is discarded (`_, _ =`) and the cancel runs regardless |

#### 2.0.4 Group 4 — a task becomes dispatchable

Everything below runs inside `reconcileMasterTasksOnce`, which is re-entered on
**every** `GetRun` of an approved master run (`getMasterRun`, `:503`), from boot
recovery, and from the autonomous heartbeat. It is the plan segment's only
continuously-running loop, and it holds no lock.

| # | Write | Site | Guard today |
| --- | --- | --- | --- |
| P24 | Waiting reason persisted into `scope_json` (`dependency` / `conflict` / cleared) | `persistTaskWaitingReason`, `master_coordinator.go:1022` → `UpdateWorkflowTaskScope` | `WHERE id = ?` — unconditional; read-modify-write of the whole scope blob, so two overlapping passes can each write a stale scope back |
| P25 | Task `eligible → blocked`, when a conflicting sibling is active | `master_coordinator.go:937` | `(taskID, expected=eligible)` |
| P26 | Task `blocked → eligible` | `master_coordinator.go:947` | `(taskID, expected=blocked)` |

#### 2.0.5 Group 5 — child run creation and dispatch intent

`dispatchMasterTask` has two entry branches: a **recovery** branch taken when
`FindWorkflowRunByPlannedTask` already finds a child (`:1063-1079`), and the
**fresh** branch below. The two branches do *not* write the same rows — which is
what CP20–CP22 are about.

| # | Write | Site | Guard today |
| --- | --- | --- | --- |
| P27 | **Child run + its six step rows** created, `planned_task_id` = this task, `parent_workflow_id` = the objective | `dispatchMasterTask`, `master_coordinator.go:1112` → `createSingleTaskRun` → P1 | P1's transaction. `planned_task_id` is the **natural key** every recovery in this group reads back through `FindWorkflowRunByPlannedTask` (`workflow_plan.sql:97-98`) |
| P28 | Child owner stamped from the **parent's** owner — never the request identity | `stampChildOwnership`, `master_coordinator.go:1121` → `child_ownership.go:31` | Unconditional overwrite; idempotent. Re-run by the recovery branch (`:1072`) before any `StartRun`, which is what closes CP19 |
| P29 | Child `policy_snapshot` overwritten with the **parent's frozen execution policy** | `inheritExecutionPolicySnapshot`, `master_coordinator.go:1128` → `execution_policy_resolve.go:197` | `WHERE id = ?`. **Not re-run by the recovery branch** — see CP20 |
| P30 | `session_lifecycle_decision` checkpoint for the task boundary (+ the dependency context pack and its hash), for a task with dependencies | `master_coordinator.go:1140` → `persistSessionLifecycleDecision`, `session_context_pack.go:162` | Append. Best-effort (`_ =`). **Not re-run by the recovery branch** — see CP21 |
| P31 | **Child plan-step artifact overwritten** with the planner's real acceptance criteria and the task's declared `WriteIntent` | `master_coordinator.go:1146` → `UpdateWorkflowStepArtifact`, `workflow.sql:121-127` | `WHERE id = ?` — unconditional. **Not re-run by the recovery branch**, and this is the most consequential omission in the segment — see CP22 |
| P32 | **Task bound to its execution run**: `execution_run_id` set and `state → running` | `master_coordinator.go:1149` → `SetWorkflowTaskExecutionRun`, `workflow_plan.sql:93-95` | A real CAS: `WHERE id = ? AND execution_run_id IS NULL AND state = 'eligible'`. The bool result is discarded at both call sites (`:688`, `:1066`, `:1149`), so a lost CAS is silent |

#### 2.0.6 Group 6 — `StartRun`: the plan step completes and the work step unblocks

Both paths converge here. Path B reaches it at `master_coordinator.go:1078`
(recovery branch) or `:1155` (fresh branch); Path A reaches it from the API.

| # | Write | Site | Guard today |
| --- | --- | --- | --- |
| P33 | Run `pending → running` | `StartRun`, `workflow.go:1117` | `(runID, expected=pending)`. **This is also the point of no return for re-entry**: every later `StartRun` on this run sees a non-`pending` state and returns `GetRun` without doing anything (`workflow.go:1112-1114`) |
| P34 | Plan step `ready → running` | `workflow.go:1152` | `(stepID, expected=ready)` |
| P35 | Plan step artifact rewritten (the artifact re-marshalled, or rebuilt from the run's objective when it was empty) | `workflow.go:1156` | `WHERE id = ?` — unconditional |
| P36 | **Plan step `running → completed`** — the plan's own completion write | `workflow.go:1159` | `(stepID, expected=running)` |
| P37 | **Work step `pending → ready`** — the plan→work unblock, the "one-off hardcoded edge" `StartRun`'s comment names | `workflow.go:1164` | `(stepID, expected=pending)` |
| — | `dispatchWorkStep` entered with the prompt built from the artifact | `workflow.go:1170-1172` | Continues at **W0** (§2.1) |

#### 2.0.7 Group 7 — planner failure, park and retry branches

Each of these is a distinct multi-row remedy, and each row is separately
crashable.

| # | Write | Site | Guard today |
| --- | --- | --- | --- |
| P38 | Capacity park: plan reset to `status='pending'`, `command_status='idle'` (**not** `failed`, or the next arming would `ErrPlanLocked` forever) | `parkPlanForCapacity`, `master_coordinator.go:315` | P12's CAS. Result discarded |
| P39 | Capacity park: `planner_capacity_wait` checkpoint | `master_coordinator.go:316` | Append; result discarded |
| P40 | Capacity park: `ReasonPlannerCapacity` wake | `master_coordinator.go:326` | Best-effort |
| P41 | Bounded retry: plan reset to `pending`/`idle` | `retryPlanOrFail`, `master_coordinator.go:373` | P12's CAS. Result discarded |
| P42 | Bounded retry: plan step `running → waiting` | `stopPlanningStep`, via `:374` | `(stepID, expected=running)` |
| P43 | Bounded retry: `planner_retry_scheduled` attention stop, carrying the attempt evidence | `master_coordinator.go:375` | Once-per-occurrence dedupe. **This row is also the retry budget**: `plannerRetryCount` counts these checkpoints (`:401-415`) rather than storing a counter |
| P44 | Bounded retry: `ReasonTransientRetry` wake | `master_coordinator.go:383` | Best-effort |
| P45 | Permanent failure: `status → invalid`, `command_status → failed`, error class | `failPlan`, `master_coordinator.go:419` | P12's CAS. Result discarded |
| P46 | Permanent failure: run `pending → needs_attention` | `master_coordinator.go:421` | `(runID, expected=run.State)`, only from `pending` |
| P47 | Permanent failure: plan step `running → waiting` | `stopPlanningStep`, via `:423` | `(stepID, expected=running)` |
| P48 | Permanent failure: attention stop with the class and the planner evidence | `master_coordinator.go:429` | Once-per-occurrence dedupe |
| RP1 | **Boot recovery, responded-but-unfinalized**: `finalizeGeneratedPlan` re-entered, replaying P9–P13 | `reconcileRun`, `recovery.go:130-133` | As P9–P13. **Mandatory**: its error aborts this run's reconciliation and parks it |
| RP2 | **Boot recovery, planner in flight**: `status → invalid`, `command_status → failed`, class `planner_ambiguous` | `recovery.go:136` | P12's CAS. Result discarded |
| RP3 | Boot recovery: run `pending`/`waiting`/`running → needs_attention` | `recovery.go:138` | `(runID, expected=run.State)` |
| RP4 | Boot recovery: `planner_ambiguous` attention stop | `recovery.go:140` | Once-per-occurrence dedupe |
| RP5 | Boot recovery, plan still `pending`: the kickoff wake re-ensured | `recovery.go:129` → `maybeKickoffAutonomousPlanning` | Best-effort, idempotent. Reads `policyForRun(run)` — so it heals CP4 and **cannot** heal CP2/CP3 |

#### 2.0.8 What the plan segment already gets right

Four properties are genuinely good and the CAS work must not regress them:

- **P6 is a true CAS on a two-column predicate.** `status='pending' AND
  command_status IN ('idle','pending')` means two callers racing to start one
  planner produce exactly one subprocess, and the loser gets `ErrPlanLocked`
  rather than a duplicate. Every plan-command transition (P8, P12) is likewise
  conditioned on the state it expects, which is more than the worker path's
  outbox claim does with respect to *which* dispatch it is.
- **P27's `planned_task_id` is a durable natural key**, written in the same
  transaction as the child run. It is what makes "did I already create a child
  for this task?" answerable after a crash without any generation token at all —
  the one place in the whole document where the *identity* problem §2's "what is
  missing" describes is already solved, by a unique key rather than by a claim.
- **P28 is re-run before every dispatch**, in both branches, precisely so the
  window between P27 and P28 cannot leave an unowned child that then launches a
  provider process. `requireChildOwnershipForDispatch` (`child_ownership.go:49`)
  is the hard gate behind it, and it fails closed in multi-user mode.
- **RP2 is fail-closed by construction**: a planner that was in flight when the
  daemon died is `planner_ambiguous`, never "assume it did not run". That is
  principle 6 applied correctly, and it is the same instinct the worker path's
  `ContradictionUnprovable` has.

#### 2.0.9 What the plan segment is missing

The same sentence as §2's, with one addition:

> **No plan-segment durable write names the plan generation, approval or task
> dispatch it belongs to** — and, unlike the work path, several of its
> multi-row remedies have **no re-entry at all** once their first row has
> landed.

The second half is the sharper problem. The work path is re-entered from four
places and its outbox row is the re-entry point; the plan segment's re-entry
points are `GeneratePlan`'s status switch, `ApprovePlan`'s
`status='validated'` CAS, and `StartRun`'s `state='pending'` check — and **each
of those is a one-way door**. A crash after the first row of `ApprovePlan` or of
`StartRun` leaves a state that the same function, called again, declines to
touch. §3.0 names each one.

### 2.1 The work-step launch segment (W, R, H, A writes)

`W#` labels are used by §3 and §6. Dispatch is entered from **P37**'s `ready`
work step; W0 is the first write it makes.

| # | Write | Site | Guard today |
| --- | --- | --- | --- |
| W0 | Outbox row enqueued (insert-or-get, idempotent on the step key) | `dispatch.go:149` | Natural key |
| W1 | `routing_decision` checkpoint (which provider was chosen / capacity wait) | `routing_dispatch.go:50` | none (append) |
| W2 | Branch lock acquired (direct-branch mode) | `ensureBranchLock`, `branch_execution.go:95`; the acquire itself at `:100` | Lock table's own CAS |
| W2.5 | Resolved run-stop cleared (a run parked by a capacity or branch wait that is demonstrably gone) | `clearResolvedStop`, `dispatch.go:231` | Human decisions are never cleared here |
| W3 | Run `waiting → running` when capacity came back | `dispatch.go:243` | `(runID, expected=waiting)` |
| W4 | **Outbox claim** `pending → dispatched` | `dispatch.go:248` | `(entryID, expected=pending)` — **no claim token** |
| W5 | `session_lifecycle_decision` checkpoint | `dispatch.go:292` | none (append) |
| W6 | **Attempt row opened** (`outcome IS NULL`) | `dispatch_state_machine.go:446-460` | "latest attempt is terminal or absent" — a read-then-write, not a CAS |
| W7 | **Dispatch intent boundary** (`LaunchOutcomeIntended`) | `dispatch_state_machine.go:426` | none (append); failure here refuses the launch |
| W8 | Attempt concluded `failed` on a pre-launch failure (intent write, runtime env, preflight) | `dispatch.go:578` | none — unconditional update of a named row |
| W9 | Launch-failure boundary (`LaunchOutcomeFailed`) | `dispatch_state_machine.go:536` | none (append, best-effort) |
| W10 | Ambiguous-launch boundary (launcher returned no session id) | `dispatch_state_machine.go:565` | none (append, best-effort) |
| W11 | `worker_launch_error` checkpoint (classification + deep error + attempt count) | `worker_launch_recovery.go:287` | none (append) |
| W12a | Retryable path: outbox `dispatched → pending` under the **same** idempotency key | `worker_launch_recovery.go:310` | `(entryID, expected=entry.Status)` — **no generation** |
| W12b | Durable wake row that carries the retry | `worker_launch_recovery.go:313` | none (best-effort) |
| W12c | Attention stop `worker_launch_retry` | `worker_launch_recovery.go:314` | once-per-occurrence dedupe |
| W13a | Permanent path: outbox → `failed` | `dispatch.go:817` | `(entryID, expected=entry.Status)` — **no generation** |
| W13b | Step `running`/`ready` → `failed` | `dispatch.go:829` | `(stepID, expected=step.State)` |
| W13c | Run `running` → `needs_attention` | `dispatch.go:835` | `(runID, expected=running)` |
| W13d | Attention stop (`dispatch_failed` or `worker_launch_retries_exhausted`) | `dispatch.go:842` | once-per-occurrence dedupe |
| W14a | Unconfirmed-launch boundary (`LaunchOutcomeUnconfirmed`) | `dispatch_state_machine.go:671` | none (append, best-effort — a failure is logged and the path continues) |
| W14b | `worker_launch_unconfirmed` checkpoint (the second durable home) | `dispatch_state_machine.go:710` | none (append, best-effort) |
| W15 | **Confirmation boundary** (`LaunchOutcomeDispatched`) — the gate RUNNING stands on | `dispatch.go:734` | none (append); requires `ownership.Observed` |
| W16 | `worker_dispatched` ledger checkpoint (the phase marker other readers key off) | `dispatch.go:762` | none (append) |
| W17 | **Step session written** (`workflow_steps.session_id`) | `dispatch.go:780` | `(stepID, session_id IS NULL)` — `UpdateWorkflowStepSession` refuses to clobber an already-associated session (`workflow.sql:129-136`). It does **not** name the generation that bound it, which is what §4.3's proof obligation P needs and what §6.5's replacement adds |
| W18 | Branch lock re-pointed at the session | `dispatch.go:790` | best-effort |
| W19 | Outbox `dispatched → acknowledged` | `dispatch.go:794` | `(entryID, expected=entry.Status)` |
| W20 | **Step `ready → running`** | `dispatch.go:799` | `(stepID, expected=ready)` |
| W21 | Work observation transition (`running → completed` / `waiting` / `failed`) | `worker_progress.go:556` | `(stepID, expected=step.State)` |
| W22 | Run transition from observation | `worker_progress.go:574` | `(runID, expected=run.State)` |
| W23 | **Completion checkpoint** — `worker_observed_*` with `branch`, `worktree_path`, `base_sha`, `head_sha`, and (on completion) `fingerprint_after` | `worker_progress.go:604` | none (append) |
| W24 | Attention stop checkpoint when observation parks the run | `worker_progress.go:637` | once-per-occurrence dedupe |
| W25 | **Attempt finalized** (`succeeded` / `failed` + error class) | `worker_progress.go:682` | none — unconditional, error ignored |
| W26 | **The review transition's evidence gate**: `dispatchReviewStep` refuses to do anything at all until the work step is durably `completed` (`review_dispatch.go:519`), then reads W23 (`GetLatestWorkflowCheckpointByStep`) for the session, branch, worktree and target fingerprint | `review_dispatch.go:528-541` | Gated on `workStep.State == completed`; a completed step with no session-bearing checkpoint raises `ambiguous_review_state` rather than reviewing an unknown target (`:534`) |
| W27 | Review policy decision persisted (cycle 1 only, before any reviewer exists) | `persistReviewPolicyDecision`, `review_dispatch.go:577` | Append; its failure aborts the dispatch |
| W28 | **Review step `pending → ready`** — the work→review unblock, the exact mirror of P37's plan→work unblock | `review_dispatch.go:604` | `(stepID, expected=pending)` |
| R1 | Reconciliation boundary (`worker_dispatch_reconciled`) | `dispatch_reconcile.go:903` | none (append); **mandatory**, not best-effort |
| R2a | Reconciliation retry — the reconciliation boundary (`LaunchOutcomeFailed`) | `dispatch_reconcile.go:787` | none (append); **mandatory** |
| R2b | Attempt closed `failed`/`runtime_failed`, **before** the retry is scheduled, so no guard reads a fossil as a live writer | `dispatch_reconcile.go:795` | none — unconditional |
| R2c | Step `waiting → running` (the state dispatch re-enters) | `dispatch_reconcile.go:807` | `(stepID, expected=waiting)` |
| R2d | Then the whole of W11 + W12a–W12c, via `recordWorkerLaunchFailure` | `dispatch_reconcile.go:816` | as W11/W12 |
| R3a | Reconciliation stop — the reconciliation boundary (`LaunchOutcomeAmbiguous`), recorded **first** so the snapshot below contains it | `dispatch_reconcile.go:855` | none (append); **mandatory** |
| R3b | Evidence snapshot + the `ambiguous_worker_state` raise | `dispatch_reconcile.go:861` | **the gate**: if the snapshot cannot be made durable the raise is refused, nothing below runs, and the step is left exactly as it was |
| R3c | Attempt closed with the raise's error class | `dispatch_reconcile.go:873` | none — unconditional |
| R3d | Step `running`/`ready` → `waiting` | `dispatch_reconcile.go:879` | `(stepID, expected=step.State)` |
| R3e | Attention stop `worker_dispatch_ambiguous` | `dispatch_reconcile.go:885` | once-per-occurrence dedupe |
| H1 | Human reopen: `worker_launch_human_retry` checkpoint, then `ReopenFailedWorkflowStep`, then outbox `failed → pending`, then unpark | `worker_launch_recovery.go:583-615` | `(id, expected=failed)` per row — **no generation, no single-winner index** |
| A1 | `attempt_reaped_orphaned` checkpoint, then the attempt row closed `cancelled` | `attempt_reaper.go` | checkpoint keyed to attempt id ⇒ exactly-once |
| A2 | `work_commit_adopted` checkpoint, then the step completes | `work_adoption.go` | bounded generation counter in the record |

**W28 is this audit's terminus.** Everything after it — the reviewer's own
outbox claim, its launch phases, its authority pointer and its generation
model — is the reviewer lifecycle, which this document reads as its reference
(§5) and proposes no change to (§6.7).

### 2.2 The fix-cycle writes

The fix cycle is not a second launch. It reuses the work step's **already-live
session** — `Send` targets `reviewRun.SessionID`, which is the worker's session,
never a new one (`fix_dispatch.go:404-412`) — so it makes no ownership probe and
writes no `workflow_dispatch_checkpoints` row at all.

It does, however, have **its own intent boundary**, and that boundary's contract
is *stronger* than the work path's W7. `recordFixDispatchIntent`
(`fix_delivery_recovery.go:160`) writes a `fix_dispatch_intent` checkpoint
carrying the session id, the review run id, `FingerprintBefore`, and the full
delivery record, **strictly before `Send`, and its failure is fatal to the pass**
(`fix_dispatch.go:361-363`): a delivery AO could not first write down is a
delivery AO does not make. What the fix cycle lacks is the work path's
*confirmation* half — there is no ownership read-back, because there is nothing
newly owned. F8 is its confirmation analogue.

`F#` labels are used by §3 and §4.7. The order below is the order
`deliverFixPrompt` actually executes.

| # | Write | Site | Durable? / guard today |
| --- | --- | --- | --- |
| F0 | Fix outbox row enqueued under a **per-cycle, per-transport** key `workflow-step-fix:<stepID>:cycle<N>[:transport<M>]` | `dispatchFixStep`, `fix_dispatch.go:246`; key built at `fixStepOutboxIdempotencyKey`, `:103` | Durable. Natural key. Unlike the worker's single reused row, each cycle gets a **fresh** row — the one structural advantage the fix path has over the work path. Preceded by a hard re-entry guard at `:237`: `len(attempts) >= cycleNumber` ⇒ this cycle already has its attempt row, never dispatch again |
| F1 | Fix outbox claim `pending → dispatched` | `dispatchFixFromPending`, `fix_dispatch.go:292` | Durable. `(entryID, expected=pending)` |
| F2 | Fix step `pending → ready` | `runFixStep`, `fix_dispatch.go:306` | Durable. `(stepID, expected=pending)` |
| F3 | Fix step `ready`/`waiting` → `running` | `runFixStep`, `fix_dispatch.go:314` | Durable. `(stepID, expected=step.State)`. Both delivery entry points — first pass and post-restart adoption — go through `runFixStep`, so a recovered cycle leaves the step identically |
| F4 | Session lifecycle decision (+ context pack, when the decision is `compact`) | `applyFixLifecycleDecision`, `cascade.go:328`; persisted at `:374` | Durable, **run-level**, best-effort (`_ =`). Deliberately **not** associated with the step (`cascade.go:366-374`) precisely so it cannot shadow the step's latest checkpoint — which is what keeps F5 readable as "the latest checkpoint for this step" |
| F5 | **`fix_dispatch_intent` checkpoint** — session id, review run id, `FingerprintBefore`, prompt receipt digest, delivery record | `recordFixDispatchIntent`, `fix_delivery_recovery.go:160`; called at `fix_dispatch.go:361` | Durable, append, and **mandatory**: a write failure aborts the pass and `Send` is never called. This is the fix path's intent-before-action boundary |
| — | `Send` — the prompt handed to the transport | `deliverAndConfirm`, `fix_dispatch.go:66`, called at `:365` | **Not a durable workflow write.** A transport side effect with no idempotency key; this is the one irreducible ambiguity in the path (CF2) |
| F6 | **Fix attempt row opened** (`outcome IS NULL`), harness copied from the work step's last attempt | `recordFixDispatchSuccess`, `fix_dispatch.go:419` | Durable. Guard is `len(attempts) < cycleNumber` — a **count comparison** against the cycle number. Not an identity check, not a CAS |
| — | `delivery.FixAttemptID = attempt.ID` | `fix_dispatch.go:423` (new row) / `:428` (re-entered cycle) | **Not a durable write — a local Go assignment.** The binding between a cycle and the attempt row it opened becomes durable only when F8 serializes the record. A crash between F6 and F8 leaves the attempt row with nothing naming it |
| F7 | Fix outbox `dispatched → acknowledged` | `fix_dispatch.go:432` | Durable. `(entryID, expected=entry.Status)` |
| F8 | **`fix_dispatched` checkpoint** — session id, review run id, `FingerprintBefore`, and the delivery record **including `FixAttemptID`** | `fix_dispatch.go:440` | Durable, append. The point at which the cycle→attempt binding first exists on disk |
| F9 | Fix step/run transition from observation (`running → waiting` on delivery, `→ failed` on no verifiable change) | `recordFixOutcome`, `fix_progress.go:282` (step), `:292` (run) | Durable. `(stepID, expected=step.State)` / `(runID, expected=run.State)`, behind the `ValidWorkflow*Transition` guards |
| F10 | `fix_observed_<state>` checkpoint carrying `fingerprint_after` | `fix_progress.go:301` | Durable, append — written **after** F9, the same inversion §4.6 catalogues for W21/W23 |
| F11 | **Fix attempt finalized** (`succeeded` / `failed` + error class) | `recordFixOutcome`, `fix_progress.go:317` (lookup) and `:336` (update) | Durable. No predicate: `GetLatestWorkflowAttempt(step.ID)` picks the row **by recency, not by identity**, and the update's error is discarded (`_ =`) |
| F12 | Fix-delivery restart classification, for a cycle whose outbox row was already `dispatched`/`acknowledged` when the process died | `resolveFixDeliveryAfterRestart`, `fix_delivery_recovery.go:331`; entered from `fix_dispatch.go:272` | Reads **F5**, not F8. `fixDeliveryNotSent` on absent intent, `fixDeliveryDelivered` on a receipt-digest match or a turn boundary after the intent's timestamp, `fixDeliveryUnproven` otherwise (`classifyFixDelivery`, `:248`). **But see §2.3: "absent intent" and "the intent ledger could not be read" are the same value here, and the path fails open on the second** |
| F13 | Adoption of a proven delivery: `runFixStep` again, the parked stop cleared, then the ordinary F6–F8 bookkeeping the crash interrupted | `adoptDeliveredFix`, `fix_delivery_recovery.go:377` | Durable. Reuses `recordFixDispatchSuccess` deliberately, so a recovered dispatch and a first-pass dispatch leave **identical** durable state |

**What is already right in the fix path**, and must be preserved:

- **F5 before `Send`, fatally.** This is the same "intent before action"
  discipline the work path has at W7, held to a stricter standard: W7's failure
  refuses the launch, and F5's failure refuses the pass. It is what makes
  "crashed before send" distinguishable from "crashed after send" at all.
- **F12 reads evidence, not state — on the session half.**
  `classifyFixDelivery` decides from the intent record plus the session's own
  facts (a receipt digest recorded at intent time, a turn boundary compared
  against the intent's timestamp) and returns `fixDeliveryUnproven` rather than
  guessing when the **session** is unreadable or missing
  (`fix_delivery_recovery.go:271-280`). That half is principle 6 correctly
  applied. The **ledger** half is not — see §2.3.
- **F0's re-entry guard.** `len(attempts) >= cycleNumber` at `fix_dispatch.go:237`
  means a cycle that got as far as F6 is never re-dispatched, whatever its outbox
  row says. Dispatch and observation therefore cannot both be acting on one
  cycle.
- **The per-cycle outbox key.** Each cycle and each bounded transport retry gets
  its own row, so the fix path is structurally free of the reused-row problem
  that §1 identifies as the root of the work path's gaps.

**What actually produces §4.7** is narrower than a missing intent boundary, and
it is entirely downstream of delivery:

1. **F6's guard is a count, not an identity.** `len(attempts) < cycleNumber`
   answers "does this step have fewer attempt rows than cycles?", which is true
   or false about the *set* of rows and says nothing about whether *this* cycle
   owns one.
2. **F8 records the right binding and F11 does not read it.** The delivery
   record names the attempt row the cycle opened. The close ignores it and takes
   the latest row on the step instead — so a delayed observation from a
   superseded cycle closes the current cycle's live row.
3. **The binding is durable later than the row it names.** F6 creates the
   attempt; F8 records which cycle owns it. Between them the row exists and
   nothing on disk names it.
4. **F11 is unconditional and lossy.** No predicate, and a discarded error: a
   close that fails leaves an open attempt and no trace that it was attempted.

### 2.3 The fix-intent ledger read fails open

`classifyFixDelivery`'s rule 1 is the strongest rule in the path: *no
pre-delivery record ⇒ PROVEN not sent*, and nothing may outrank it, because F5
is written before `Send` and its failure is fatal. That inference is sound —
**but only when the absence was established by a read that succeeded.**

It is not, today. `findFixDispatchIntent` returns a plain three-tuple, and
`found = false` is returned for **two different facts**
(`fix_delivery_recovery.go:197-205`):

1. `ListWorkflowCheckpoints` succeeded and no intent row for this step/cycle/
   transport attempt exists — genuine, provable absence; and
2. `ListWorkflowCheckpoints` **failed** — AO knows nothing at all.

`resolveFixDeliveryAfterRestart` passes that single boolean straight into
`classifyFixDelivery` (`:341-343`), whose `!intentFound` branch returns
`fixDeliveryNotSent` (`:265-266`), and the caller then **re-delivers the prompt**
(`:346-353`). So a transient store read failure is currently read as proof that
`Send` never ran, and the fix findings can be sent a second time into a session
that may already be acting on them.

The function's own comment says the opposite:

> *"A read failure is not evidence of absence. Returning 'found' with a zero
> record would be a lie; returning 'not found' would license a resend on no
> information at all. The caller treats the third state — see
> `resolveFixDeliveryAfterRestart`'s readFailed branch."*
> — `fix_delivery_recovery.go:200-204`

**There is no `readFailed` branch.** `readFailed` appears exactly once in the
package, in that comment. The intended tri-state was documented and never
implemented, so the comment reads as a description of behaviour that does not
exist. This is the one place in the fix path where the design intent and the
code disagree, and the disagreement fails in the unsafe direction.

**Gap vs. reviewer fail-closed provenance (principle 6).** This is precisely the
inversion the reviewer path forbids. `reviewLaunchFailureForEntry` returns "no
generation" on an unreadable ledger and treats that as a refusal to act —
"cannot prove must never become the newest one" (`review_launch_recovery.go:643`)
— and `workerLaunchRecoveryGenerations` returns the budget **maximum** on a read
error rather than 0. Here the same unreadable-ledger condition returns the value
that licenses the most destructive action available on the path. It is the same
defect §6.6 invariant 5 already names for `workerLaunchAttemptCount`
(`worker_launch_recovery.go:396-398`), in a place where the consequence is a
duplicated prompt rather than a mis-sized budget.

**Consequence for the classification of CF1** (§3): CF1 is deterministically
resolvable **only on the successful-read path**. When the ledger read fails, the
correct answer is not "not sent" and not "delivered" — it is `unproven`, and the
run should park exactly as CF2 does. Until an implementation step adds a real
tri-state result, **the read-failure case is a fail-open defect, not a
fail-closed residue**, and it is classified in §4.7 as `needs_attention`-by-right
that the code does not currently deliver.

### 2.4 What the work and fix paths already get right

The ordering discipline is sound and should be preserved verbatim by the CAS
work:

- **Intent before action** (W7 before the launcher; R1 before R2/R3; H1 before
  the reopen; A1 before the close; A2 before the step moves). A record with no
  action behind it is harmless; an action with no record is unreconcilable.
- **Confirmation before RUNNING** (W15 → W16 → W17 → W20). RUNNING is licensed
  only by `WorkerDispatchStatus.LicensesRunning()`, i.e. a durable confirmation
  (`dispatch_state_machine.go:820`).
- **Both halves of the evidence** are required to confirm: a session identity
  *and* an observed ownership read-back (`dispatch.go:723-733`). A launcher's
  word alone routes to the unconfirmed state.
- **Silence is never an answer.** `SessionOwnershipEvidence.Missing` (the read
  succeeded and there is nothing there) is kept distinct from
  `Unavailable` (the read failed / was never wired), and
  `ownedExecution.Unprovable()` is a first-class outcome that stops rather than
  guesses (`dispatch_reconcile.go:169-262`).

### 2.5 What the work and fix paths are missing

Everything in this document that follows is a consequence of one gap:

> **No worker-path durable write names the launch generation it belongs to.**
> Not the outbox claim (W4), not the failure (W13), not the reopen (H1), not
> the confirmation (W15/W16), not the session write (W17), not the RUNNING
> transition (W20), not the attempt finalization (W25).

The append-only boundary rows *record* a generation implicitly (their own row
id), but no *mutating* write conditions on it. Every mutation is therefore
replayable by a stale writer that observed the same coarse state value one turn
of the cycle earlier.

---

## 3. Crash windows

Two tables: §3.0 for the plan segment, §3.1 for the work step and the fix
cycle. Each window in both is the interval between **two adjacent durable
writes** — no window below spans a write. Where an earlier draft of this table collapsed several
writes into one row (`W0 → W4` over W1–W3, `W4 → W6` over W5, and the composite
W12/W13/W14/R2/R3 remedies), the row is now split into its actual adjacent
boundaries, lettered so every cross-reference to the original label still
resolves. Failure and retry branches are enumerated alongside the success path.

"Produces" names the ambiguous state class from §4. "Resolver today" is the code
that actually cleans it up, if any. **Sub-lettered rows are counted as one
window each** in §7's totals.

### 3.0 Plan-segment windows (CP)

Same rule as §3.1: each row is the interval between two **adjacent** durable
writes from §2.0, and no row spans a write. "Resolver today" is the code that
actually cleans it up, if any — and for this segment the honest answer is
"boot `Reconcile`'s plan branch, or nothing", because `GetRun`'s continuous
reconcile only runs once the plan is **approved**.

| # | Window | Durable state left behind | Consequence | Resolver today |
| --- | --- | --- | --- | --- |
| CP1 | P1′ → P2 (master) | A run with a `plan` step and **no `workflow_plans` row** | The run is not recognisable as a master objective by anything: `GetWorkflowPlan` reports "not master", so `getMasterRun` never runs, `ContinueRun` falls through to the work/review lookup and returns `ErrInvalid` (`workflow.go:1276-1278`), and boot `reconcileRun` falls through to the step loop and finds no work step | **None.** A permanently inert run. The two writes are in separate transactions for no reason the code states |
| CP2 | P2 → P3 | Run + plan row exist; **no owner** | Multi-user visibility falls back to "unowned"; `requireChildOwnershipForDispatch` has nothing to enforce later | None, and none is needed for a standalone run. For a master run this is CP3's precondition |
| CP3 | P3 → P4 | Owner stamped; `policy_snapshot` still `DefaultWorkflowPolicy()` — whose `Execution` is the zero value, i.e. **`AutonomousMode = false`** and no routing priorities | An objective the user created as autonomous is durably manual, and routes on default priorities. Nothing ever says so | **None.** RP5 re-ensures the *wake*, but it reads `policyForRun(run)` from this same snapshot (`recovery.go:129`), so it reads `AutonomousMode=false` and does nothing. The run waits for a person who was never told to come |
| CP4 | P4 → P5 | Snapshot frozen autonomous; **no kickoff wake** | An autonomous objective with nothing scheduled to start it | **RP5**, explicitly (`recovery.go:120-129` and its comment). The one window in this group that was designed for |
| CP5 | P5 → P6 | Wake scheduled, plan `pending` | none — steady state | The poller calls `ContinueRun`, which routes a `pending`-plan master run straight into `GeneratePlan` (`workflow.go:1256`) |
| CP6 | P6 → P7 | Plan command armed (`running`/`running`); plan step still `ready` | The plan is armed over a step that does not say so | **RP2–RP4**, fail-closed. Residue: the ambiguous branch does **not** call `stopPlanningStep`, and `stopPlanningStep` only moves `running → waiting` anyway, so the step is left at `ready` under an `invalid` plan |
| CP7 | **P7 → planner subprocess → P8** — the segment's irreducible window, and its C5 analogue | Plan `running`/`running`, step `running`, and a planner process that may have run for minutes and may have produced a complete plan | Whether the objective was planned is not knowable from workflow's own tables | **RP2, fail-closed and lossy.** Unlike the worker path, which adopts a live launch by natural key and ownership probe (`adoptLiveLaunch`), the planner has **no adoption path**: the plan is marked `invalid` — which `GeneratePlan`'s switch treats as permanent — and a person must act. Correct in direction, expensive in fact: a whole planner invocation is discarded rather than re-read |
| CP8 | P8 → P9 | Response persisted (`responded`); normalization not re-persisted | none (P9 is already a no-op, §2.0.2) | RP1 re-enters `finalizeGeneratedPlan` and re-derives the same normalized plan |
| CP9 | P9 → P10 → P11 | Two distinct states. **(a)** No task rows: RP1 replays cleanly and the plan finalizes. **(b)** Task rows written, relationships not | (b) is the dangerous one. RP1's replay mints **fresh task ids** (`idByPlan[s.ID] = "wft-" + c.newID()`, `master_coordinator.go:211`), the `INSERT OR IGNORE` loses to `UNIQUE(workflow_run_id, plan_step_id)` and silently drops them, and P11 then inserts relationship rows naming ids that do not exist. With `foreign_keys(ON)` (`db.go:37`) that insert **fails** | **Fail-closed, and permanently stuck.** `ReplaceWorkflowTaskRelationships`' error propagates out of `finalizeGeneratedPlan` → `reconcileRun` → `parkUnreconcilableRun` (`recovery.go:105`), so the objective is parked with `recovery_unreconcilable` and every subsequent boot reproduces the identical failure. Reachable for any plan with two or more tasks, since `ClassifyTaskRelations` emits exactly one row per pair (`task_graph.go:180-181`). The fix is a natural key on the task id (derive it from `workflow_run_id` + `plan_step_id`), not a generation token |
| CP10 | P11 → P12 | Tasks + relationships written; plan still `running`/`responded` | The plan is complete on disk and unusable: `ApprovePlan` requires `validated`, and nothing dispatches from `running` | RP1, subject to CP9(b)'s failure mode |
| CP11 | P12 → P13/P19 (autonomous or `approval_mode=auto`) | Plan `validated`, never approved | **No task ever dispatches.** Boot `reconcileRun`'s switch has no `validated` case; `getMasterRun` reconciles only when `approved`; `ContinueRun` sees a non-`pending` plan and delegates to `GetRun`, which does the same nothing. The autonomous heartbeat is only rescheduled from inside `reconcileMasterTasks`, which never runs | **None.** An autonomous objective stalls silently at "plan ready". For a **manual** run the same state is correct and expected — it is the approval prompt — which is exactly why the stall is invisible |
| CP12 | P13 → P19 | Approval mode durably `auto`, plan still `validated` | Same as CP11, now with a record that says it should have been approved automatically | **None** |
| CP13 | P19 → P20/P21 | Plan `approved`; plan step still `waiting`/`running` | Tasks dispatch normally (they gate on the plan row, not the step), but the objective's own plan step never reads `completed` | **None**: `ApprovePlan` re-entered returns at its `status == approved` early exit (`master_coordinator.go:459-461`) before reaching the step writes. Cosmetic — nothing gates on a master run's plan-step state — but it is a durable lie in the ledger |
| CP14 | P21 → P22 | Plan approved, plan step `completed`, **run still `pending`/`waiting`** | `reconcileMasterTasksOnce` dispatches tasks, but every branch that parks or completes the objective is gated on `run.State == running` (`:786`, `:828`, `:982`, and `completeRun` at `:877-879`). The objective can never complete or report a stop | **None**, for the same early-exit reason as CP13. This is CP13's consequential twin |
| CP15 | P17 → P18 (manual hold) | Run `waiting`, plan step still `running` | none | `ApprovePlan` handles both `waiting` and `running` step states (`:475-479`) |
| CP16 | P24 → P25/P26 | Waiting reason persisted, task state not yet moved | none | The next reconcile pass re-derives both. Idempotent |
| CP17 | P26 → P27 | Task `eligible`, no child run | none | Next pass re-enters `dispatchMasterTask`; `FindWorkflowRunByPlannedTask` finds nothing and the fresh branch creates one |
| CP18 | P27 → P28 | Child run exists; **no owner** | A child that could dispatch a provider process unowned | **Closed by design.** The recovery branch re-stamps unconditionally before `StartRun` (`master_coordinator.go:1066-1077`), and `stampChildOwnership`'s doc comment names this exact window |
| CP19 | P28 → P29 | Child owned; `policy_snapshot` still the **default** | The child of an autonomous objective is durably non-autonomous and routes on default priorities — the CP3 failure, one level down | **None.** The recovery branch (`:1063-1079`) calls `stampChildOwnership`, `requireChildOwnershipForDispatch` and `StartRun`, and **never** `inheritExecutionPolicySnapshot`. The divergence is permanent and silent |
| CP20 | P29 → P30 | Policy inherited; **no `session_lifecycle_decision` checkpoint** | The task-boundary decision and the dependency context pack hash are lost, so the provenance of "why this task got a new session" is not recoverable | **None**, and the write is best-effort anyway (`_ =`). Provenance gap, not a state gap |
| CP21 | P30 → P31 | Child exists, owned, policy-correct — and its plan step still carries the **generic** artifact `createSingleTaskRun` wrote: `BuildPlanArtifact`'s three boilerplate acceptance criteria and an **empty `WriteIntent`** | **The most severe window in this segment.** The recovery branch goes straight to `StartRun`, which reads that artifact (`workflow.go:1139-1147`) and builds the worker prompt from it. The worker is therefore dispatched against generic criteria instead of the planner's, and a task the plan declared **read-only** is prompted, verified and classified as **mutating** (`WriteIntent` empty ⇒ `Unspecified` ⇒ treated as mutating, `plan.go:27-30`). Nothing downstream can tell: the artifact is a plausible artifact, it is simply the wrong one | **None.** The fix is ordering — bind the task's real criteria into the child at creation (pass them through `createSingleTaskRun`) rather than overwriting them one write later |
| CP22 | P31 → P32 | Artifact correct; task not bound to its execution run (`execution_run_id` NULL, state still `eligible`) | none | **Closed by the natural key.** `FindWorkflowRunByPlannedTask` finds the child by `planned_task_id`, the recovery branch re-binds via P32's CAS (still `eligible`, so it lands) and calls `StartRun` |
| CP23 | P32 → P33 | Task `running` with an execution run; child run still `pending` | none | The recovery branch's `StartRun` (`:1078`), which is idempotent for a `pending` run |
| CP24 | P33 → P34 | Run `running`; plan step still `ready`; work step still `pending` | **A dead end.** `StartRun` re-entered returns at `run.State != pending` (`workflow.go:1112-1114`) without touching either step. Boot recovery's step loop skips a work step that is not `ready`/`running` (`recovery.go:192-193`) and its generic interrupted-fallback skips a step that is not `running`. `ContinueRun` dispatches only from `ready`/`running` (`workflow.go:1366`) | **None.** The run sits `running` with a `ready` plan step and a `pending` work step forever. Nothing raises attention, because nothing is contradicting anything — the state is merely unreachable |
| CP25 | P34 → P35 | Plan step `running`, artifact not rewritten | Same dead end as CP24 for the forward path — but the plan step is now `running`, which boot recovery's generic fallback **does** see: it moves it `running → waiting` and parks the run `needs_attention` with `recovery_interrupted` (`recovery.go:227-229`, `:262-273`) | **Partial**: a person is told, with a reason. Nothing resumes the plan→work unblock, and no later call will — `StartRun` still refuses |
| CP26 | P35 → P36 | Artifact rewritten, plan step still `running` | As CP25 | As CP25 |
| CP27 | P36 → P37 | **Plan step `completed`, work step still `pending`** | The purest form of the dead end: the plan is durably done and the work step is durably blocked on an edge that will never be re-evaluated. Boot recovery sees no `running` step, so not even `recovery_interrupted` fires | **None.** This is the plan segment's C12: a completed producer whose consumer was never unblocked |
| CP28 | P37 → W0 | Work step `ready`, dispatch not yet entered | none | Every dispatch entry point re-enters from `ready`: boot `Reconcile`, `ContinueRun`, the next `GetRun`-driven master pass. **The segment's one clean hand-off** |
| CP29 | P38 → P39 → P40 (capacity park) | Plan reset to `pending`/`idle` with, in turn, no checkpoint and no wake | The plan is armable again but nothing is scheduled to arm it | The master heartbeat (`maybeScheduleAutonomousHeartbeat`) for an autonomous run; a person otherwise. Benign because P38 leaves the plan in exactly the state `GeneratePlan` re-enters from |
| CP30 | P41 → P42 → P43 → P44 (bounded retry) | Four separately-crashable rows | **P43 is the budget.** A crash between P41 and P43 resets the plan for another attempt *without* recording the retry, so `plannerRetryCount` (`:401-415`) under-counts and the budget of 3 is silently widened by every such crash. A crash between P43 and P44 records the retry with no wake to carry it | None. The reviewer's `review_launch_attempt` reasoning — allocate the budget **before** the work the attempt performs — is exactly what P41/P43's order gets backwards |
| CP31 | P45 → P46 → P47 → P48 (permanent failure) | Plan `invalid` with, in turn, a still-`pending` run, a still-`running` step, and no attention stop | A permanently invalid plan under a run that says `pending` and a ledger that says nothing | None. The plan-segment twin of C21a–C21c, with the same shape: the terminal row lands first and the explanation last |
| CP32 | RP2 → RP3 → RP4 | Plan `invalid` with, in turn, no run transition and no stop | An objective stopped with no reason a person can read | None. `planner_ambiguous` is the right verdict recorded in the wrong order — the same defect C21c names for the worker path |

### 3.1 Work-step and fix-cycle windows (C, CF)

| # | Window | Durable state left behind | Produces | Resolver today |
| --- | --- | --- | --- | --- |
| C1a | W0 → W1 | Outbox `pending`; no routing decision recorded | none (a step that has not started) | `dispatchWorkStep` re-enters from `pending`; routing is re-evaluated against fresh capacity |
| C1b | W1 → W2 | Routing decision recorded; **no branch lock** | none | Re-entry re-routes and re-acquires. A routing decision is advisory, not a claim on anything |
| C1c | W2 → W2.5 | Branch lock **held** by a run that is about to stop existing as a dispatcher | Orphaned branch lock | `branch_lock_recovery.go` — the lock table's own recovery, outside this path's authority |
| C1d | W2.5 → W3 | Resolved stop cleared; run still `waiting` | none | Re-entry clears again (idempotent) and re-transitions |
| C1e | W3 → W4 | Run `running`, outbox still `pending` | none — a run that says running over a step that has not been claimed | Re-entry claims. Benign: `pending` is the re-entry point, and the run transition is CAS'd on `waiting` so the replay is a no-op |
| C2a | W4 → W5 | Outbox `dispatched`, **no boundary at all**, no lifecycle decision, no attempt | **stale/phantom dispatched command** | `adoptOrMarkAmbiguous` (`dispatch.go:586`) after a 30 s settle window; reconciliation explicitly defers to it (`dispatch_reconcile.go:624`) |
| C2b | W5 → W6 | As C2a, plus a `session_lifecycle_decision` checkpoint naming a session that was never created | **stale/phantom dispatched command** | Same as C2a. The lifecycle checkpoint is step-associated (`applyWorkLifecycleDecision`, `dispatch.go:292`), so it is the latest step checkpoint until W7 — but no reader treats it as launch evidence, so it misleads nothing |
| C3 | W6 → W7 | Attempt open, no intent boundary | **worker_completed_but_attempt_open** (degenerate: attempt open over nothing) | `attemptReaper` only, and only after 30 min + four proofs. Reconciliation sees `WorkerDispatchNone` + open attempt + step not running ⇒ falls through to `running_without_evidence` only if the step says RUNNING |
| C4 | W7 → launcher return | Intent boundary newest, attempt open, step still `ready` | **running_without_dispatch** precursor | `ContradictionIntentNeverLaunched` (a), gated by `dispatchReconcileSettleWindow` = 30 s so a live launch is never cut off |
| C5 | Launcher returned, process exists, **before W14/W15** | Process alive; no confirmation; outbox `dispatched`; step has no session | **launch unconfirmed** — the one window that cannot be closed by ordering | `adoptLiveLaunch` (c) via natural key + ownership probe. This is the single-write-wide window the design accepts |
| C6 | W14 written, crash | `worker_launch_unconfirmed` record naming the session | launch unconfirmed (recorded) | `latestUnconfirmedLaunchRecord` → `WorkerDispatchUnconfirmed` → adopt |
| C7 | W15 → W16 | Confirmation boundary exists; **no `worker_dispatched` ledger row** | Split-brain between the two durable homes: `WorkerDispatchStatusForStep` says confirmed, `latestWorkerLaunchRecord`/`work_adoption` see no `worker_dispatched` marker | none. Reconciliation reads the dispatch table and calls it confirmed; the ledger-based readers disagree |
| C8 | W16 → W17 | Confirmed + ledger marker; step has **no session** | **stale/phantom running** variant | `ContradictionStaleRunning` second branch (`dispatch_reconcile.go:576-583`) — closes and retries per policy |
| C9 | W17 → W19/W20 | Step has a session; outbox still `dispatched`; step still `ready` | **running_without_dispatch** inverted: a session owner that never entered RUNNING | `dispatchWorkStep`'s `step.SessionID != nil` guard returns early ⇒ **the step never advances**. Reconciliation's confirmed branch requires `state == running` *and* a session, so it takes the "RUNNING transition never completed" path only when the session is absent. A confirmed step holding a session but stuck at `ready` is reconciled to `stale_running` → `retryReconciledDispatch` → and immediately re-routed to `stopReconciledDispatch`, because a step owning a session may not be re-dispatched (`dispatch_reconcile.go:757-771`). Net: a human decision |
| C10 | W20 → first observation | Step RUNNING, worker alive | none | steady state |
| C11 | Worker exits **after** producing work, before W21 | Session row terminated; git evidence present; step still RUNNING; attempt open | **terminal-evidence-before-crash** | `observeWorkStep` on the next pass re-derives from facts. Correct *because* the decision is a pure function of durable/observable facts — the only lifecycle decision in this path that is genuinely idempotent by construction |
| C12 | W21 → W23 | Step `completed`; **no completion checkpoint** | Review transition starves: `dispatchReviewStep` requires a checkpoint with a session id and raises `ambiguous_review_state` when it is absent (`review_dispatch.go:534`) | none — a human decision on a run whose work actually succeeded |
| C13 | W23 → W25 | Step `completed`, checkpoint written, **attempt still open** | **worker_completed_but_attempt_open** | `attemptReaper` after 30 min + four proofs (`attempt_reaper.go:36-49`). Until then every guard that asks "could something still be writing to this tree?" answers yes — `verify_branch_advanced.go` proof 5, `work_adoption.go` proof 4 |
| C14a | W25 → W26 | Complete and consistent; the review transition has not been entered | none | `advanceReviewFixCycle` step 4, `cascade.go:194-201` → `dispatchReviewStep`, whose evidence gate is `workStep.State == completed` (`review_dispatch.go:519`) |
| C14b | W26 → W27 | Evidence read; **no durable state changed at all** | none | Pure re-derivation: the gate and the checkpoint read are both reads, and `EvaluateReviewPolicy` is a pure function of facts recomputed on the next pass |
| C14c | W27 → W28 | Review policy decision durable; review step still `pending` | The work→review unblock is pending on a caller that will supply `includeCycle1Unblock` | **Partial, and asymmetric with P37.** Only `ContinueRun` passes `true` (`workflow.go:1518`); boot `Reconcile` (`recovery.go:250`) and `GetRun` (`workflow.go:941`) pass `false` by design, so cycle 1's unblock waits for an explicit Continue — which the master path issues automatically for a child whose work is done and whose review is pending (`master_coordinator.go:718-724`), and which a standalone run waits for a person or a wake to issue. A replay also appends a second policy-decision record; the decision is deterministic, so the duplicate is a ledger artifact, not a divergence |
| C15 | R1 → R2/R3 | Reconciliation boundary is newest; nothing else moved | Repeat pass sees `DispatchPhaseWorkerDispatchReconciled` as newest and returns "already reconciled" (`dispatch_reconcile.go:538`) — **the remedy is skipped entirely** | none. The boundary is written before the remedy precisely so a duplicate wake is a no-op, but a crash in the same window makes the *first* remedy a no-op too |
| C16 | H1 checkpoint → `ReopenFailedWorkflowStep` | Human-retry checkpoint written; step still `failed`; outbox still `failed` | **repeated wake/reconcile**: the generation is counted (it bounds `maxWorkerLaunchRecoveryGenerations`) but nothing was reopened. A second Continue burns a second generation | documented as intentional (`worker_launch_recovery.go:505-513`); the budget is spent by crashes rather than by decisions |
| C17 | `ReopenFailedWorkflowStep` → outbox reopen | Step `ready`, outbox `failed` | Step ready over a failed command | Next Continue finishes it (`worker_launch_recovery.go:513`) |
| C18a | W12a → W12b (outbox back to `pending`, wake not yet scheduled) | Outbox pending, no wake | Step waits for any other dispatch entry point; `workerLaunchRetryDelay` floor still applies | boot `Reconcile` / capacity wake |
| C18b | W12b → W12c (wake scheduled, attention stop not written) | Retry is pending and will fire; the run shows no `worker_launch_retry` marker | none — a cosmetic gap in the ledger, not a state | The retry proceeds; the next failure writes its own stop |
| C19 | Master: child run moves out of `needs_attention` → parent mirror cleared | Parent durably `child_needs_attention`, child running | **child running while parent needs_attention** | `reconcileMirroredChildStop` (`attention.go:770` (`reconcileMirroredChildStop`)), driven by the parent's own heartbeat, which the mirror deliberately does not kill |
| C20 | Two `Reconcile`/wake passes overlapping in one process, or two processes | Both read the same coarse state and both act | **repeated wake/reconcile** | Partly: R1's boundary and `owned.Live()` short-circuit. Not closed for W4/W12a/W13a/H1, which CAS on `(id, expected)` only |
| C21a | W13a → W13b (outbox `failed`, step not yet `failed`) | A failed command under a step still `running`/`ready` | Step stalls: `dispatchWorkStep` finds a `failed` outbox row and returns without retrying (`dispatch.go:180-186`), and nothing re-fails the step | none. The way back out is H1, whose `ReopenFailedWorkflowStep` has its CAS **hard-coded to expect `failed`** (`workflow.go:81-95`) and so reports false on a step still `running`/`ready` — the run is stuck until reconciliation reads the contradiction |
| C21b | W13b → W13c (step `failed`, run still `running`) | A failed step under a running run | Run never parks; the cascade sees a terminal step and stops advancing | `cascade.go`'s next pass derives the run state from its steps |
| C21c | W13c → W13d (run `needs_attention`, no attention stop) | A parked run with **no reason recorded** | A stop a person cannot read | none — this is the unevidenced dead end §4.3's evidence gate exists to abolish, and W13 is not behind that gate. **Fail-closed by design is not what this is**: it is a genuine ledger gap, listed here so the CAS step does not mistake it for one |
| C22 | W14a → W14b | Unconfirmed boundary in the dispatch table; **no `worker_launch_unconfirmed` checkpoint** | Split-brain across the two durable homes, the unconfirmed-side twin of C7 | `WorkerDispatchStatusForStep` reads the dispatch table and answers `Unconfirmed` correctly; `latestUnconfirmedLaunchRecord` (the checkpoint reader) sees nothing. Both W14a and W14b are best-effort, so this window also opens when *either* write simply fails |
| C23a | R2a → R2b (reconciliation boundary written, attempt not yet closed) | Boundary says "retry scheduled"; the attempt row is still open | Every downstream guard still reads a live writer in the tree | C15's problem in miniature: a repeat pass returns "already reconciled" and the close never happens. **The attempt stays open** |
| C23b | R2b → R2c (attempt closed, step still `waiting`) | Closed attempt under a `waiting` step | A step parked at `waiting` is never re-entered by dispatch, so a retry scheduled over one would never fire | none, for the same C15 reason |
| C23c | R2c → R2d (step `running`, outbox not yet back to `pending`) | Step `running`, outbox still `dispatched`, no attempt | **stale/phantom running** | `ContradictionStaleRunning`, on a later pass that gets past C15's "already reconciled" short-circuit |
| C24a | R3a → R3b (reconciliation boundary written, evidence snapshot not yet durable) | Boundary recorded; nothing moved | Repeat pass short-circuits on C15 | **Correct by design.** The gate refuses the raise if the snapshot cannot be made durable and leaves the step exactly as it was (`dispatch_reconcile.go:864-869`) — an unevidenced stop would be permanent, an unresolved contradiction is still true in three seconds |
| C24b | R3b → R3c (raise durable, attempt not yet closed) | `ambiguous_worker_state` raised with evidence; attempt still open | **worker_completed_but_attempt_open**, over a step a person is already looking at | `attemptReaper`, whose proof 2 admits the step only once it has left `running`/`ready` — which R3d has not yet done |
| C24c | R3c → R3d (attempt closed, step still `running`) | Raised, attempt closed, step still says RUNNING | **stale/phantom running** on a step already parked for attention | Next reconcile pass; the raise is already durable so the second pass is idempotent |
| C24d | R3d → R3e (step `waiting`, no attention stop) | Step parked; the raise exists but the stop reason does not | A parked step whose *reason* is only in the raise, not the stop ledger | `raiseAmbiguousWorkerState` already wrote the evidence, so unlike C21c the reason is recoverable — this is a duplicate-record gap, not an unevidenced stop |
| CF1a | F1 → F2 (outbox claimed, fix step not yet `ready`) | Fix outbox `dispatched`; step still `pending`; no intent record | none — provable non-delivery | **Resolved deterministically, on a successful ledger read.** `classifyFixDelivery` returns `fixDeliveryNotSent` on an absent intent (`fix_delivery_recovery.go:265-266`) and `resolveFixDeliveryAfterRestart` delivers exactly once. F5's fatal-on-failure contract licenses the inference: no intent ⇒ `Send` never ran |
| CF1b | F2 → F3 (step `ready`, not yet `running`) | As CF1a, step `ready` | none — provable non-delivery | Same. `runFixStep` re-runs on both delivery entry points and is idempotent across `pending`/`ready`/`waiting` (`fix_dispatch.go:303-318`) |
| CF1c | F3 → F4 (step `running`, no lifecycle decision) | Step `running` with no intent record | none — provable non-delivery | Same. A `running` fix step with no session-bearing checkpoint is the one case `observeFixStep` returns "nothing to observe" for (`fix_progress.go:102-107`), so observation stands aside and recovery decides |
| CF1d | F4 → F5 (lifecycle decision written, intent not yet) | Run-level lifecycle checkpoint exists; still no step-associated intent | none — provable non-delivery | Same. F4's deliberate run-level scoping (`cascade.go:366-374`) is what keeps it from being mistaken for the step's dispatch evidence |
| **CF1-R** | **Any of CF1a–CF1d, when the intent ledger read itself fails** | Indistinguishable, to the current code, from CF1a–CF1d | **Duplicate prompt delivery** — the findings are re-sent into a session that may already be acting on them | **none, and this fails open.** `findFixDispatchIntent` returns `found=false` on a `ListWorkflowCheckpoints` error exactly as it does on genuine absence (`fix_delivery_recovery.go:197-205`), so rule 1 fires on no information. See §2.3. The correct behaviour is `unproven` + park, as CF2; the code does not do it |
| CF2 | **F5 → `Send` returns** — the one window `Send`'s lack of an idempotency key makes irreducible | Intent record present; whether the prompt reached the agent is not knowable from workflow's own tables | Ambiguous delivery | **Evidence-decided, with a fail-closed floor.** `classifyFixDelivery` proves delivery from the session's own facts — the intent-time receipt digest matching `sess.Metadata.LatestUserPrompt`, or a turn boundary after the intent's timestamp (`:293-307`) — and adopts via `adoptDeliveredFix`. When the session is unreadable, missing, or shows neither signal, `fixDeliveryUnproven` → `markFixDeliveryUnproven` parks the run **once**, with the evidence. This is fail-closed by design, not a gap |
| CF3 | `Send` returned → F6 (attempt row) | From disk, indistinguishable from CF2: intent present, no attempt | Ambiguous delivery | Same as CF2, and correctly so — the durable state *is* the same, and the resolver reasons from session evidence rather than from which side of the return the crash fell |
| CF4 | F6 → F7 (attempt open, outbox still `dispatched`) | Attempt row exists; outbox not acknowledged; **no `fix_dispatched` checkpoint**; step `running`; **intent record is the latest step checkpoint** | none | **Converges today.** Dispatch cannot re-enter (F0's `len(attempts) >= cycleNumber` guard, `fix_dispatch.go:237`), and observation does not need F8: `observeFixStep` accepts *any* latest step checkpoint carrying a non-empty session id (`fix_progress.go:102-107`), and F5 carries one — plus the same `FingerprintBefore` (`reviewRun.TargetSHA`) F8 would have. F4's run-level scoping is what guarantees the intent is the latest *step* checkpoint. Residue: the outbox row stays `dispatched` forever, which nothing reads again |
| CF5 | F7 → F8 (attempt open, outbox acknowledged, no `fix_dispatched`) | As CF4, plus the acknowledgement; the cycle→attempt binding **never became durable** | none | **Converges today**, by the same route as CF4. But the binding §4.7 proposes to close on does not exist for this cycle, so the fix must fail closed here rather than fall back to recency |
| CF6a | F9 → F10 | Fix step/run **terminal-for-the-cycle with no `fix_observed_*` checkpoint at all**; attempt still open | **`fix_completed_but_attempt_open`**, in its unrecoverable form | **none, and it must stay that way.** The canonical terminal record the close would read was never written, so there is no durable statement of what the cycle produced — no `fingerprint_after`, no outcome. This is the fix-cycle twin of C12 and of §4.6's residue: the evidence never existed, and synthesising it would be inventing a verdict. **Fail-closed by design**; the attempt stays open for the reaper and the cycle stays `needs_attention` |
| CF6b | F10 → F11 | Terminal-for-the-cycle **with** the `fix_observed_*` checkpoint written; attempt still open | **`fix_completed_but_attempt_open`**, in its recoverable form | `attemptReaper` only today, after 30 min + four proofs — and proof 3 needs evidence on a *different* step written after the attempt opened, which a run parked by this very cycle may never produce. **Deterministically recoverable** by §4.7 item 5: the checkpoint *is* the canonical terminal record, so the outcome can be read off it |
| CF7 | Cycle N's observation runs after cycle N+1's F6 opened a new row | F11 closes cycle **N+1**'s live row and leaves cycle N's open | **`fix_completed_but_attempt_open`**, cross-cycle variant: a live attempt closed and a dead one left open | **none.** This is not a crash window at all — it is reachable with no crash, purely from `GetLatestWorkflowAttempt` picking by recency |

---

## 4. The ambiguous state classes

Seven classes, as named in the objective. For each: what it is, which windows
produce it, what resolves it today, and the CAS resolution proposed for the
implementation step. The resolutions reuse the reviewer's *principles*
(§5) — durable generation identity, ownership proof, generation-conditioned
CAS, stale-writer rejection, idempotent replay, fail-closed on unprovable
provenance — and reuse the *store primitives* that already exist and are
table-generic, but **no reviewer code is copied**: the reviewer's semantics
(review runs, authority pointer, cycles, fresh-review epochs) do not apply to a
worker and must not be transplanted.

**Classification.** Every class carries an explicit verdict, in the two terms
the objective names:

- **Resolvable** — a durable mechanism can decide the state correctly without a
  human, once the CAS in §6 exists. The ambiguity is an artifact of missing
  generation identity, not of missing information.
- **Must remain fail-closed** — no amount of durable bookkeeping can decide it,
  because the deciding fact lives outside workflow's authority (a live session
  owned by another component, an unreadable runtime) or because acting on a
  guess is worse than stopping. These route to `needs_attention` with evidence,
  by design, and the implementation step must not "fix" them.

**All seven are resolvable**, each with a narrow fail-closed residue. The residue
is never a whole class and never a whole *case* — it is always the same
predicate applied to that case: **AO converges when it can name the generation
and positively prove the state; it stops when the evidence is unreadable, the
identity is absent or mismatched, or the deciding fact belongs to something AO
cannot prove is finished.** The residue is what stays after §6 lands, stated per
class so a later reader does not read a remaining `needs_attention` as an
unfinished bug — and, equally, does not read a provable case as one that was
meant to stop.

| Class | Verdict | Fail-closed residue that must survive §6 |
| --- | --- | --- |
| 4.1 `running_without_dispatch` | **Resolvable** | The step owns a session whose execution AO cannot prove has ended — reconciliation may not release ownership it cannot name as dead (`dispatch_reconcile.go:757-771`). A binding whose own generation can prove the execution behind it ended converges instead, via §4.3's R3R |
| 4.2 `worker_completed_but_attempt_open` | **Resolvable** — by the evidence-driven close rule (§4.2 rule 4), not by generation-stamping alone | An attempt whose `dispatch_generation` is unreadable, absent or mismatched, or for which no canonical terminal record can be read: left open, and the reaper's four proofs stay mandatory (`attempt_reaper.go:30-47`) |
| 4.3 stale / phantom running | **Resolvable** — *stale*, and *phantom* whenever proof obligation P holds: the reconciling generation owns the claim, the attempt and the binding, **and** the execution named by the attempt's `runtime_instance_id` is positively gone (§4.3, transition R3R in §6.5) | Any part of P absent, unreadable or mismatched: an `Unprovable` runtime/ownership read (`dispatch_reconcile.go:257-259`, `:639`), an unstamped instance id or binding generation, a binding written by another generation, or a conditional clear that updates zero rows. All stop as `ambiguous_worker_state` |
| 4.4 child running while parent `needs_attention` | **Resolvable — already resolved**, and out of scope | A child stop that cannot be positively classified as self-remediable: the mirror is held, deliberately |
| 4.5 repeated wake / reconcile | **Resolvable** | Budget exhaustion after the bounded generations are genuinely spent — that is a decision, not an ambiguity |
| 4.6 terminal-evidence-before-crash | **Resolvable** | Rows already `completed` with no completion checkpoint when §6 lands: the evidence never existed, so `ambiguous_review_state` remains the only honest answer (`review_dispatch.go:534`) |
| 4.7 `fix_completed_but_attempt_open` | **Resolvable for CF6b and CF7** — by binding the close to the attempt the cycle actually opened (F8's `FixAttemptID`, stamped onto the row itself at F6) and reading the outcome off the terminal checkpoint. **CF6a is fail-closed by design** | **CF6a**: a cycle whose `fix_observed_*` checkpoint was never written — no outcome exists to read, so the attempt stays open and the cycle stays `needs_attention`. An attempt row that cannot be attributed to a cycle: left open for the reaper, never closed by falling back to recency. In the *delivery* half, CF2 — a written intent whose `Send` outcome cannot be proven from session facts — parks once via `markFixDeliveryUnproven`, irreducible because `Send` has no idempotency key. **And one row that is a defect, not a residue: CF1-R** (§2.3), where an unreadable intent ledger currently licenses a resend instead of parking |

### 4.1 `running_without_dispatch`

**What.** `workflow_steps.state = 'running'` with no durable confirmation
under it — either no dispatch boundary at all (rows predating the state
machine), or a boundary that is only an intent.

**Windows.** C4, C9 (inverted), C15.

**Today.** `ContradictionRunningWithoutEvidence` (d) and
`ContradictionIntentNeverLaunched` (a). Both close the attempt and hand the step
back to the bounded retry policy — *unless* the step owns a session, in which
case they become a human decision.

**Gap.** The transitions that produce it (W17, W20) are unconditioned. A stale
dispatch that lost its claim can still write a session onto the step and still
move it to RUNNING, because `UpdateWorkflowStepSession` takes no expected value
at all and `UpdateWorkflowStepState` takes only `expected=ready`.

**Proposed CAS resolution.** W15–W20 become one generation-conditioned
sequence. The confirmation boundary carries `dispatch_generation`; the ledger
marker, the session write and the RUNNING transition each name that generation
in their own `WHERE` clause. A dispatch that no longer owns the outbox row
updates zero rows and returns "not mine" — which is a no-op, not an error,
exactly as the reviewer's `ReleaseDispatchedWorkflowOutboxGeneration` result is
read. RUNNING then means *this* generation's confirmed launch, not "some
confirmation exists".

**Classification: resolvable.** Nothing here is unknowable — the durable record
of which generation confirmed the launch simply is not written. Once W15–W20
form one generation-conditioned sequence, RUNNING means "this generation's
confirmed launch" and a stale writer cannot manufacture the state at all, so the
class stops being produced rather than being cleaned up after. **Fail-closed
residue:** the sub-case where the contradicted step owns a session whose
execution AO **cannot prove has ended**. There the attempt is closed but the
retry is withheld, because re-dispatching would put a second worker on a
worktree AO cannot prove is free. Note the boundary carefully: it is
*unprovability*, not the mere presence of a session, that withholds the retry —
a binding whose own generation can positively prove the execution behind it
ended converges instead, through the audited clear (§4.3, R3R).

### 4.2 `worker_completed_but_attempt_open`

**What.** `workflow_attempts.outcome IS NULL` after the work it described has
ended — completed, failed, or reconciled away.

**Windows.** C3, C13; also every path where W25's unconditional
`UpdateWorkflowAttemptOutcome` error is discarded (`worker_progress.go:682`).

**Today.** `attemptReaper`, after 30 minutes and four proofs. Correct and
deliberately conservative — a wrongly reaped attempt tells every downstream
guard the tree is quiet while an agent is writing to it — but it is a
*recovery*, not a *guarantee*. In the meantime the open row blocks
`verify_branch_advanced` proof 5 and `work_adoption` proof 4.

**Gap.** The attempt has no durable link to the dispatch generation that opened
it. `openWorkerAttempt` reuses "the latest attempt if it is not terminal"
(`dispatch_state_machine.go:446`), which is a read-then-write: two concurrent
dispatches can both observe the same open attempt and both proceed against it,
and a stale one can conclude an attempt a live dispatch is using.

**Proposed CAS resolution.**
1. `workflow_attempts.dispatch_generation` — stamped at W6, so an attempt names
   the claim that opened it and `openWorkerAttempt` can *reject* an open attempt
   belonging to a different generation instead of adopting it.
2. `UpdateWorkflowAttemptOutcomeIfOpen(attemptID, generation)` — conditions the
   close on `outcome IS NULL AND dispatch_generation = ?`, making W8, W25, R2's
   close and the reaper's close all idempotent-on-replay and refusing a stale
   writer's close of a live attempt.
3. W25's error stops being discarded: an attempt that could not be finalized
   is exactly the fossil the reaper then has to clean up, and that is worth a
   log line and a retry on the next observation rather than silence.
4. **An evidence-driven close rule, because 1–3 are not one.** Generation
   identity establishes *who may* close the attempt; it does not cause the close
   and is not itself proof that the close is owed. A crash after W21/W23 and
   before W25 still leaves an open attempt under a completed step, and no
   amount of stamping changes that. What closes it deterministically is a rule
   that derives the outcome from evidence that is already canonical:

   > **Same-generation completion close.** On any observation or reconcile
   > pass, an attempt with `outcome IS NULL` is closed, with the outcome read
   > off the evidence, when **both** hold: (a) its `dispatch_generation` equals
   > the step's current dispatch generation — the attempt belongs to the
   > lifecycle being observed, not to a superseded one; and (b) a canonical
   > terminal record exists for that same generation — the W23 completion
   > checkpoint (`worker_observed_*`, which already carries the branch, base
   > and head SHAs and the outcome), or a terminal dispatch boundary for it.

   The rule is a pure function of durable rows, so replay re-derives the same
   answer and a duplicate pass updates zero rows. It needs no settle window and
   no elapsed time, because it never infers "the agent must be finished" from
   silence — it reads a record the agent's own completion wrote. This is the
   same shape as §4.6: evidence first, transition conditioned on it.

**Classification: resolvable — by the close rule (4), not by stamping alone.**
It is worth being precise about which piece does the work, because the two are
easy to conflate. Stamping (1–2) supplies *authority*: it makes the close safe,
idempotent, and impossible for a stale writer to perform on a live attempt. It
does **not** supply *causation* — a generation-conditioned close still has to be
called by somebody, and the C13 crash window is precisely the case where nobody
does. Rule (4) supplies the causation: the next pass over the step closes the
attempt from the completion evidence that already exists, so the row converges
without a human and without waiting out a timer.

**Fail-closed residue:** an attempt whose `dispatch_generation` is unreadable,
absent (legacy), or unequal to the step's current generation, and any attempt
for which no canonical terminal record can be read, is **never** closed by rule
(4) — it is left open and falls through to `attemptReaper`'s four proofs,
unchanged. That fallback stays mandatory: a wrongly-reaped attempt tells every
downstream guard the tree is quiet while an agent is writing to it, which is the
one failure mode the guards exist to prevent. Rule (4) narrows how often the
reaper is the only answer; it does not retire it (§6.7).

### 4.3 stale / phantom running

**What.** Two distinct things the code correctly keeps apart:
- **stale**: the confirmation is durable and the transition after it is not
  (C8) — an attempt claiming to be in flight with nothing tracking it;
- **phantom**: `ownedExecution.PhantomRunning()` — the execution AO launched is
  *proven* gone (ownership read back `Missing`, or the liveness probe answered
  "not running") **and** the session row still reads as a usable worker
  (`dispatch_reconcile.go:253-255`). Work observation is structurally blind to
  this: it reads the session row, sees a healthy worker, and leaves the step
  running forever.

**Windows.** C8, C9; and any daemon restart that kills the process behind a
confirmed session.

**Today.** `ContradictionStaleRunning` (e). A phantom whose step owns a session
routes to `stopReconciledDispatch`: the attempt is closed (the fossil goes) and
a human gets the full evidence, because no reconciler may release another
component's session ownership.

**Gap.** "Proven gone" is derived from a runtime probe at reconcile time, not
from a durable fence. The AO session id survives a daemon restart while the
process behind it does not — migration `0134`'s own note. The evidence exists
in the dispatch boundary (`runtime_handle_id`, `runtime_launch_id`,
`agent_session_id`), but nothing compares the *recorded* runtime instance
against the *current* one: a restarted daemon that re-created a runtime handle
with the same session id reads as live.

**Proposed CAS resolution.** Promote the runtime launch id to a first-class
**runtime instance id** on the attempt (`workflow_attempts.runtime_instance_id`,
stamped at confirmation). A confirmed attempt is live only while the observed
instance id equals the recorded one; a different instance id is *positive*
evidence the execution is gone (a stronger and cheaper fact than the probe), and
an unreadable one stays `Unprovable` and stops. This is the worker's form of the
reviewer's `errReviewerInstanceUnproven` rule (`review_launch_phases.go:57-67`)
— stated for the worker, implemented independently.

**Classification: resolvable — for the *provable* case; fail-closed only on
the unprovable residue.** The split runs on **provability**, not on
stale-vs-phantom, and the runtime-instance fence is what moves the line.

*Stale* (C8) is a missing durable fence and nothing more: an instance id on the
attempt turns "is the recorded execution still the running one?" into an
equality test, and that closes C8 without a human.

***Phantom is resolvable too*** — but only under a proof obligation stated in
full, because the session binding is the one piece of worker state AO must never
touch on an inference. Today the phantom routes to a person, and the reason is a
limitation of the *evidence*, not of AO's authority: the AO session id survives
a daemon restart while the process behind it does not (migration `0134`'s own
note), so a session row that still reads as a usable worker might be a live
worker, and refusing to touch it is correct **under that uncertainty**. The
fence removes the uncertainty — but only when every part of the identity chain
is named, and these are *different* identities that must not be conflated:

> `dispatch_generation` is the identity of a **claim** (which dispatch owns this
> step right now). `runtime_instance_id` is the identity of an **execution**
> (which launched process the confirmation was about). The first says who may
> act; the second says what is being proven about. Neither substitutes for the
> other, and no predicate below ever compares one against the other.

**Proof obligation P — all four parts required, evaluated by the reconciling
generation G:**

1. **G holds the claim.** The outbox row's `dispatch_generation` is `G`.
2. **The attempt is G's.** The open attempt `A` under the step has
   `A.dispatch_generation = G`.
3. **The binding is G's own.** The step's `session_id = S` **and**
   `session_dispatch_generation = G` (§6.2) — G is releasing a binding *it
   wrote*, never one another generation wrote.
4. **The execution behind that binding is positively gone.** `A.runtime_instance_id`
   is non-empty and readable, and the runtime, read for `S`, reports either a
   *different* instance id or ownership `Missing`. A read that fails or is
   unwired is `Unavailable` ⇒ `Unprovable` ⇒ **stop**.

When P holds, there is no live owner to protect: G is retiring AO's own record
of an execution AO can prove ended. That is sufficient durable authority for
deterministic convergence, and the transition is audited as **R3R** in §6.5 —
clear the binding conditioned on `(stepID, S, G)`, close the attempt under G,
release the claim under G. The retry then proceeds under a **new** generation
`G'`, so the dead generation cannot re-bind: its claim is gone, and every write
it might still attempt names `G`.

**Fail-closed residue**, which must survive unchanged — any part of P failing
stops as `ambiguous_worker_state`:
- **Unreadable evidence.** An unreadable runtime or ownership read is
  `Unprovable`, never "gone". The gap between `Live()` and `ProvenGone()` is
  load-bearing and must stay open.
- **Missing identity.** An empty `runtime_instance_id` or an unstamped
  `session_dispatch_generation` (legacy rows) is unprovable by construction.
- **Mismatched identity.** A binding written by a generation other than the one
  reconciling, or an attempt belonging to a superseded generation. "Proven gone"
  is a claim about *one named execution*; it licenses nothing about an execution
  AO cannot name.
- **A clear that updates zero rows.** Somebody changed the binding between the
  read and the write ⇒ stop; never retry the clear with a widened predicate.

The rule, stated once: **positive proof of one named execution's death, by the
generation that bound it, converges; anything absent, unreadable, or mismatched
stops.** Preserving the human-decision route for the *provable* case would
preserve exactly the ambiguity the fence exists to remove; widening the clear
beyond P would re-create the double-worker hazard the session guard exists to
prevent.

### 4.4 child running while parent `needs_attention`

**What.** A master run durably stopped on `child_needs_attention` while the
child it mirrors is running again.

**Windows.** C19, plus every pass in which the child's newest checkpoint is
something other than its stop (a worker observation, an incident record, a
wake).

**Today.** Already fixed, and the fix is the model to copy. `attention.go:770` (`reconcileMirroredChildStop`)
derives the mirror from the children's *current* durable states on every
reconcile pass rather than treating it as a historical fact, holds the mirror
when a child's stop cannot be positively classified as self-remediable ("not
being able to name a child's stop is not evidence the child has recovered"),
and keeps the parent's heartbeat alive *through* the mirror so something is
still watching.

**Gap.** None in the rule. The exposure is upstream: the mirror is only as good
as the child's own stop being truthful. Every phantom-running or
stuck-at-`ready` state in §4.1/§4.3 propagates into a parent stop that the
mirror will faithfully hold.

**Proposed CAS resolution.** No new mechanism. The parent mirror is explicitly
**out of scope** for the worker CAS change, and the audit records it here so
the implementation step does not "fix" it twice. The correct contribution is
indirect: fewer unprovable child states ⇒ fewer mirrored stops.

**Classification: resolvable — and already resolved.** The rule at
`attention.go:770` decides this correctly today by re-deriving the mirror from
the children's current durable states rather than trusting a historical fact.
No worker CAS is needed and none should be added. **Fail-closed residue, kept
deliberately:** a child stop that cannot be positively classified as
self-remediable holds the mirror. "Not being able to name a child's stop" is not
evidence the child recovered, and the implementation step must not relax that
into an optimistic clear.

### 4.5 repeated wake / reconcile

**What.** Two passes over the same step — two wakes, a wake racing boot
`Reconcile`, a `ContinueRun` racing a heartbeat, or two daemon processes —
producing two remedies where one was owed.

**Windows.** C15, C16, C20.

**Today.** Three partial protections, each real and each incomplete:
- `owned.Live()` ends a reconcile pass before it can act (`dispatch_reconcile.go:515`);
- the R1 boundary makes a *subsequent* pass a no-op (`:538`) — but see C15;
- `workerLaunchRetryDelay` stops other dispatch entry points front-running a
  scheduled retry and burning the budget in one second (`dispatch.go:170`).

**Gap.** All three are read-then-act. Between the read and the act, the state
they read can turn over. The reviewer path closed exactly this with claim
tokens; the worker path has not.

**Proposed CAS resolution.**
1. **Claim, don't check.** W4 becomes
   `ClaimWorkflowOutboxDispatch(id, now, dispatchGeneration)` — the same store
   method the reviewer already uses. The generation is the id of a
   `worker_dispatch_authorized` checkpoint written *before* the claim, so an
   authorization with no claim is harmless and a claim always has its
   authorization behind it.
2. **Every ownership-dependent transition off `dispatched` names the token**:
   W12, W13, W19 use `FailWorkflowOutboxWithGeneration` /
   `ReleaseDispatchedWorkflowOutboxGeneration` / a generation-conditioned
   acknowledge. A pass that lost the claim updates zero rows.
3. **Single-winner human reset.** H1 gets the reviewer's `0136` shape: a unique
   partial index over `(workflow_step_id, head_sha)` where
   `durable_phase = 'worker_launch_human_retry'`, keyed by the *failed
   generation being resumed* (`<idempotency key>|<failure record id>`), never by
   the generation *number*. Two Continues on one failure produce one reset; a
   duplicate insert is read as "somebody already resumed this failure" — a
   no-op, not an error.
4. **C15 closed by ordering, not by a second boundary.** The reconciliation
   remedy and its boundary become one generation: the boundary records the
   generation it is about, and a repeat pass treats "boundary present, remedy
   not applied for this generation" as *still owed* rather than as answered.

**Classification: resolvable, in full.** This is the class the reviewer path
already proved out end to end, and the only one with no fail-closed residue.
Duplicate passes are not an information problem — every racing writer has all
the facts — they are an authority problem, and a claim token settles authority
outright: the winner acts, the loser updates zero rows and returns a no-op.
The single-winner index does the same for the human reset. What remains after
§6 is not ambiguity but policy: once the bounded generations are genuinely
spent by real failures, stopping for a person is the intended answer.

### 4.6 terminal-evidence-before-crash

**What.** The worker produced real, git-verifiable work (a commit past the
dispatch base, or a dirty/staged/untracked tree) and the daemon died before any
of that became a workflow transition.

**Windows.** C11, C12, C13.

**Today.** The **best-behaved** part of the path, and the reason is worth
stating precisely: `evaluateWorkStepProgress` is a pure function of
`(session facts, live workspace observation, recorded base SHA, dispatch time,
corroborated human-input evidence, read-only expectation)`
(`worker_progress.go:143`). None of its inputs is workflow's own mutable state,
so re-deriving it after a crash produces the same answer. Additionally:
- the two decisions taken on the *absence* of evidence ("terminated with
  nothing to show", "no first signal") are never taken against a throttled
  observation — AO pays for the git call every time (`worker_progress.go:466-473`);
- `work_adoption.go` exists for the case where a human landed the commit by
  hand, with seven proofs and a bounded generation counter.

**Gap.** Two, both narrow:
- **C12**: the transition (W21) lands before the completion checkpoint (W23),
  and the review transition depends on W23's `session_id` / `branch` /
  `worktree_path` / `fingerprint_after`. A crash between them leaves a
  `completed` work step whose review can only be raised as
  `ambiguous_review_state`. This is the *only* place in the worker path where a
  mutating transition precedes the evidence that licenses the next step —
  everywhere else the order is evidence-then-transition.
- **C13**: the attempt stays open (§4.2).

**Proposed CAS resolution.** Invert C12 to match the rest of the path: write
the completion evidence checkpoint **before** the step transition, and condition
the transition on it. Concretely, `observeWorkStep` writes W23 first (it is
append-only and carries no claim), then performs W21 as a
generation-conditioned CAS. A crash between them leaves a `running` step with a
completion checkpoint under it — which the next observation re-derives
identically and finishes — instead of a `completed` step with no evidence,
which nothing can finish.

**Classification: resolvable, by ordering rather than by CAS.** The decision
itself is already a pure function of durable and observable facts, so replay
re-derives it identically — this class is ambiguous only in the one window where
a mutating transition (W21) precedes the evidence that licenses the next step
(W23). Inverting that order closes it with no new state. **Fail-closed
residue:** a step that is *already* `completed` with no completion checkpoint
when the inversion lands cannot be recovered — the branch, worktree and
fingerprint the review needs were never written, and inventing them would send a
reviewer at an unverified tree. `ambiguous_review_state` stays the honest
answer for those rows.

### 4.7 `fix_completed_but_attempt_open`

**What.** The fix-cycle counterpart of §4.2: `workflow_attempts.outcome IS NULL`
on a **fix** step after the cycle that opened the row has ended. It is a
separate class rather than a case of §4.2 because the fix path reaches it by a
different route, and one of those routes needs no crash at all.

**Windows.** CF6a, CF6b and CF7. The delivery-path windows (CF1a–CF5) all
converge or fail closed on their own — **with one exception that is a defect
rather than a residue, CF1-R**, the ledger-read failure documented in §2.3, which
is tracked here because it is the fix path's one fail-open hole even though it
produces a duplicate prompt rather than an open attempt.

Stating the boundary precisely matters, because the attempt-close failure is
**not** a missing intent record or missing session evidence: the fix path has
both, F5 writes the intent before `Send` and treats a failed write as fatal, and
`observeFixStep` reads that intent happily. The failure is downstream of
delivery, in how the attempt row is closed. And CF6 splits: **CF6a** (no
terminal checkpoint) and **CF6b** (terminal checkpoint present) look alike on
disk but are not alike in what can be proven, and only CF6b is recoverable.

**Today.**

- **CF6b** (crash between F10 and F11) leaves a terminal-for-the-cycle fix
  step with an open attempt *and* the `fix_observed_*` checkpoint that says what
  the cycle produced. Only `attemptReaper` can clear it, and it is a
  worse fit here than on the work step: proof 2 is satisfied (`waiting`/`failed`
  are neither `running` nor `ready`), but **proof 3 requires durable evidence on
  a *different* step, written strictly after the attempt opened, showing the run
  moved on** (`attempt_reaper.go:41-44`). A fix cycle that ended by parking the
  run — `stopFix` / `stopFixAmbiguous`, `fix_progress.go:200`, `:229` — is
  exactly the case where the run does *not* move on, so the evidence proof 3
  wants may never be written. The row stays open indefinitely, and every guard
  that asks "could something still be writing to this tree?" keeps answering yes
  (`verify_branch_advanced.go` proof 5, `work_adoption.go` proof 4).
- **CF7** needs no crash. `recordFixOutcome` closes
  `GetLatestWorkflowAttempt(step.ID)` (`fix_progress.go:317`, `:336`) — the most
  recent attempt row on the fix step, chosen by recency. A fix step accumulates
  one attempt row per cycle (F6's `len(attempts) < cycleNumber` guard), so as
  soon as cycle N's observation is delayed past cycle N+1's dispatch, the close
  lands on N+1's live row: the newer attempt is finalized while its cycle is
  still running, and cycle N's row is left open forever. One misattribution
  therefore produces both failure modes at once — a live attempt wrongly
  closed, and an abandoned one never closed.

**Gap vs. the reviewer CAS model.** The fix path scores *better* than the work
path on the two principles the delivery half exercises, and worse on the three
the close half exercises:

| Principle | Fix path today |
| --- | --- |
| Ownership proof (2) | **Present, in the form the path admits.** The fix cycle proves nothing about a *session* because it launches none — it borrows the work step's. What it must prove is *delivery*, and `classifyFixDelivery` does exactly that from the session's own facts (`fix_delivery_recovery.go:248`), with `fixCycleStarted` (`fix_progress.go:44`) guarding the observation side |
| Fail-closed provenance (6) | **Absent on both of the paths that matter, in different ways.** The *session* read is correct: `classifyFixDelivery` returns `fixDeliveryUnproven` on an unreadable or missing session and parks once with evidence. The *ledger* read is not: an unreadable checkpoint list collapses into the same `found=false` as genuine absence and licenses a resend (§2.3) — the exact inversion of `review_launch_recovery.go:643`'s "cannot prove must never become the newest one". And the close discards its error (`_ =`, `fix_progress.go:336`), so a close that did not happen is indistinguishable from one that did |
| Generation identity (1) | **Recorded, unused, and late.** F8 writes `FixAttemptID` into the durable delivery record, so the path *does* record which attempt row belongs to which cycle — F11 simply does not read it. And the binding becomes durable one write *after* the row it names (F6 → F8), so CF5 leaves a row nothing names |
| Generation-conditioned CAS (3) | **Missing.** `UpdateWorkflowAttemptOutcome` takes an id and no predicate; nothing conditions on the row being open or on the cycle owning it |
| Stale-writer rejection (4) | **Missing, and reachable without a crash** (CF7). A late observation from a superseded cycle wins a write against the current cycle's row |
| Idempotent replay (5) | **Absent by construction** in the close. It is a blind overwrite of whichever row is newest, so a replayed observation does not re-derive the same answer — it re-targets. (The *delivery* half is properly idempotent: F13 adopts through the same `recordFixDispatchSuccess` a first pass would have used, so recovered and first-pass dispatches leave identical state) |

**Proposed CAS resolution.** Small, and confined to the close:

1. **Close the attempt the cycle named, not the newest one.** `recordFixOutcome`
   resolves its target from the cycle's own `fix_dispatched` delivery record
   (`promptDeliveryRecord.FixAttemptID`, read back via
   `promptDeliveryRecordFromJSON`) instead of `GetLatestWorkflowAttempt`. This
   alone closes CF7 outright — with no schema change, because F8 already writes
   the field.
2. **Condition the close.** The same
   `UpdateWorkflowAttemptOutcomeIfOpen(attemptID, generation)` §4.2 introduces,
   with the fix cycle's identity in the generation column: `WHERE id = ? AND
   outcome IS NULL AND dispatch_generation = ?`. Zero rows updated means the row
   is already closed or is not this cycle's — a no-op, never an error.
3. **Stamp F6.** The attempt row created at `fix_dispatch.go:419` records the
   cycle's identity in the same `dispatch_generation` column §6.2 adds, so (2)
   has something to condition on, so the reaper can tell a fix attempt's owner
   from its recency, and — importantly — so the binding is durable *with* the
   row rather than one write later. This is what shrinks CF5's residue: an
   attempt stamped at creation names its cycle even when F8 never ran.
4. **Stop discarding the error.** As with W25 (§4.2 item 3), a close that failed
   is a fossil someone has to clear later; it is worth a log line and a retry on
   the next observation rather than silence.
5. **Extend §4.2's same-generation completion close to fix steps.** The F10
   `fix_observed_<state>` checkpoint is already the canonical terminal record
   for a cycle and already carries `fingerprint_after`. When it exists for a
   cycle whose attempt is still open and whose generation matches, the next
   observation or reconcile pass closes that attempt with the outcome read off
   the checkpoint. Pure function of durable rows, so replay re-derives the same
   answer; this is what closes **CF6b** without waiting on the reaper's proof 3.
   It cannot close CF6a, and must not try: with no checkpoint there is no
   outcome to read.
6. **Invert F9/F10, as §4.6 inverts W21/W23.** Writing the `fix_observed_*`
   checkpoint before the step/run transition makes (5) able to fire on every
   crash in the window, rather than only on those that got past the checkpoint.

7. **Give `findFixDispatchIntent` the tri-state its own comment already
   describes** (§2.3). It must distinguish "read succeeded, no intent" from
   "read failed", and `classifyFixDelivery` must fire rule 1 only on the former.
   A read failure becomes `fixDeliveryUnproven`, which the existing
   `markFixDeliveryUnproven` path already handles correctly — parking once with
   the evidence, exactly as CF2 does. This is the smallest change in this
   document and the only one that removes a fail-*open* rather than tightening a
   fail-closed one, so it should land first and independently of the
   generation-stamping work: it needs no migration and no new column.

Note what is **not** proposed: apart from item 7, nothing in the delivery half.
F5's intent contract, F12's *session*-side evidence rules, F13's adoption, the
per-cycle outbox key and F0's re-entry guard are all correct and stay exactly as
they are.

**Classification: mostly resolvable, with one window that must stay
fail-closed.**

- **CF7 — deterministically provable.** Resolvable by item 1 alone, and not even
  ambiguous: the correct row is durably named, and the current code declines to
  read it.
- **CF6b — deterministically provable.** Resolvable by items 5–6, on exactly the
  argument §4.2 makes: the deciding fact is a record the cycle's own completion
  wrote, so no inference from silence is involved and no settle window is needed.
- **CF6a — fail-closed by design, stays `needs_attention`.** The canonical
  terminal record was never written. There is no durable statement of what the
  cycle produced, and the only way to close the attempt would be to assume an
  outcome. Item 6 (inverting F9/F10) shrinks the window that *creates* new CF6a
  rows to nothing, but it cannot retroactively give evidence to rows already on
  disk — the same limit §4.6 states for work steps already `completed` with no
  completion checkpoint.
- **CF1-R — currently fail-open; a defect, not a residue.** The correct
  classification is `needs_attention` (park with evidence, as CF2). The code
  instead re-sends. Item 7 makes the correct classification reachable; until it
  lands, this row is the one place in the fix path where AO acts on no
  information at all.

**Fail-closed residue.** Two cases stay `needs_attention` or stay open by
design, and neither is about missing intent evidence:

- **CF2, and only CF2, in the delivery half.** A cycle whose intent was written
  and whose `Send` outcome cannot be proven from session facts — an unreadable
  or missing session, no receipt-digest match, no turn boundary after the intent
  — parks the run once via `markFixDeliveryUnproven`. This is irreducible:
  `Send` has no idempotency key, so the only alternatives are re-sending
  findings into a session that may already be acting on them, or dropping a
  cycle that may never have been delivered. Stopping with the evidence is the
  correct third answer, and item 3 above does not change it.
- **CF6a: a cycle whose terminal checkpoint was never written.** Rows already on
  disk in this shape cannot be recovered, for the same reason §4.6's residue
  cannot: inventing a `fingerprint_after` and an outcome would be fabricating
  the very evidence the close is supposed to read.
- **A cycle whose attempt row cannot be attributed.** After item 3, this is
  narrow: a row created before the stamping exists, or one whose stamp is
  unreadable. Falling back to "the newest row on the step" is precisely the
  defect being removed, so the close must not — the attempt stays open and
  visible to the reaper, and the observation raises nothing. The pre-stamping
  shape of this is CF5, where F8 never ran and F11 has no durable name to
  resolve; today the recency fallback happens to hit the right row there
  *because* F0's re-entry guard prevents a later cycle from opening one first,
  but "happens to be right because another guard holds" is not a property to
  build a close on.

---

## 5. Side by side: the approved reviewer CAS model vs. this path today

The reviewer model is already merged and is the reference. Its six principles,
first against the work-step launch segment (§2.1), then — in §5.1 — against the
plan segment (§2.0), and finally, in §5.2, as the explicit ledger of what is
fixed, what remains, and what must stay stopped:

| # | Reviewer principle | Reviewer implementation | Worker path today | Verdict |
| --- | --- | --- | --- | --- |
| 1 | **Durable generation / attempt identity** | `reviewLaunchGeneration{OutboxID, IdempotencyKey, RecordID, Cycle, Epoch, Stamped}` (`review_launch_recovery.go:577`); `review_launch_attempt` checkpoint allocates the budget *before* any work the attempt performs (`:750`) | No generation value exists. Attempt budget is *counted* by scanning checkpoints after the fact (`workerLaunchAttemptCount`, `worker_launch_recovery.go:392`), so an attempt that crashes before writing its failure record is invisible and the budget is not spent | **Missing** |
| 2 | **Ownership proof before the state is believed** | `review_launch_intent` / `review_launch_confirmed` markers; a `review_run` row is explicitly *not* proof of a launch (`review_launch_phases.go:22-41`); `errReviewerInstanceUnproven` refuses a confirmation that names only a reusable session | Structurally present and good: `SessionOwnershipEvidence` with `Observed` / `Missing` / `Unavailable` kept apart, both halves required to confirm, `LicensesRunning()` gating RUNNING | **Present** — the one principle the worker already satisfies |
| 3 | **Generation-conditioned CAS transitions** | `ClaimWorkflowOutboxDispatch`, `FailWorkflowOutboxWithGeneration`, `ReleaseDispatchedWorkflowOutboxGeneration`, `ReopenFailedWorkflowOutboxGeneration` (`workflow.go:131-157`); `UpdateWorkflowStepStateIfReviewRun` for the authority pointer | Every mutation is `(id, expected_state)`; `UpdateWorkflowStepSession`'s predicate is `session_id IS NULL`, which prevents a clobber but names no writer — so it cannot express "release the binding I made" | **Missing** |
| 4 | **Stale-writer rejection** | Migration `0138`: a dispatch that pauses, loses its claim to recovery, and wakes up to fail the row updates zero rows because the token no longer matches. Migration `0137`: a resume validated against failure F1 cannot reopen F2 | A stale worker dispatch that wakes after a reconcile-driven retry finds the outbox back at `dispatched` (by the retry) and its `(id, expected)` predicate matches. It can then fail a generation it does not own, or confirm a launch that was superseded | **Missing** |
| 5 | **Idempotent replay** | Cancel is intent → act → confirm, and "cancelling something already gone is success" (`review_launch_phases.go:103-112`); claims and resets are insert-or-lose, never error | Partly. Append-only boundaries replay harmlessly; `attemptReaper` and `work_adoption` are exactly-once by keyed checkpoint. But W17/W20/W25 replay by overwriting, and C15 makes a *first* remedy skippable | **Partial** |
| 6 | **Fail closed on unprovable provenance** | `reviewLaunchFailureForEntry` returns "no generation" on an unreadable ledger — "cannot prove must never become the newest one" (`review_launch_recovery.go:643`); a legacy reset naming no generation fails closed (`0136`) | Strong in reconciliation (`ContradictionUnprovable`, `!status.Readable` ⇒ conclude nothing, no dispatch recorder ⇒ reconcile nothing). Weak in recovery: `workerLaunchAttemptCount` returns **0** on a read error (`worker_launch_recovery.go:396-398`), which is fail-*open* on the budget, while `workerLaunchRecoveryGenerations` correctly returns the maximum on the same error (`:418`) | **Inconsistent** |

### The two structural differences that must **not** be copied

1. **The reviewer has an authority pointer; the worker has a session.**
   `workflow_steps.review_run_id` names exactly one review run that speaks for
   the step, and rebinding it is how a replacement is authorized
   (`review_authority.go:37-46`). The worker's analogue is
   `workflow_steps.session_id`, and it is **not** rebindable: a session is a
   live process on a worktree owned by another component, and no workflow code
   may release it while it is live (`dispatch_reconcile.go:757-771`). The worker
   model therefore gets *stale-writer rejection* but never *supersession* — a
   worker generation that lost its claim stops; it never replaces the winner.

   The distinction that makes this precise, and that §4.3 turns on: **rebinding
   is repointing a binding at a new owner; releasing is retiring a binding whose
   owner is proven dead.** The reviewer rebinds — `review_run_id` is moved from
   one review run to another, and that is how a replacement is authorized. The
   worker never does that: `session_id` is only ever cleared back to NULL, by
   the generation that wrote it, under proof that the execution behind it ended
   (R3R, §6.5). A subsequent dispatch then binds a *fresh* session under a
   *new* generation through the ordinary W17 path — it does not inherit, adopt
   or repoint the old one. So the worker model has release without supersession,
   and no worker generation ever speaks for a step another generation bound.
2. **The reviewer's cycle/epoch vocabulary is review-specific.** Review cycles,
   fresh-review generations and reset epochs exist because a review step is
   re-dispatched many times against changing fingerprints. A work step
   dispatches once and is re-dispatched only on failure. The worker generation
   needs no cycle dimension, and adding one would be inventing state nothing
   reads.

### 5.1 The plan segment against the same six principles

The reviewer model is the reference for the whole path, not only for the launch
segment. Applied to §2.0:

| # | Reviewer principle | Plan segment today | Verdict |
| --- | --- | --- | --- |
| 1 | **Durable generation / attempt identity** | Partly, and by a better mechanism than a token where it exists: `planned_task_id` (`UNIQUE`, written in P27's own transaction) identifies a task's child run for all time, and `UNIQUE(workflow_run_id, plan_step_id)` identifies a task. But **no plan-command arming, approval or `StartRun` has an identity**: P6 can tell `pending` from `running`, never arming *N* from arming *N+1*, and the planner retry budget is counted from checkpoints after the fact (P43), exactly the `workerLaunchAttemptCount` shape principle 1 rejects | **Partial** |
| 2 | **Ownership proof before the state is believed** | **Present and strong** for the child run: P28 stamps the owner from the parent, both dispatch branches re-stamp before any launch, and `requireChildOwnershipForDispatch` refuses to dispatch an unowned or mismatched child (`child_ownership.go:49-66`). Absent for the *plan*: nothing proves a planner subprocess belongs to the arming that started it — CP7's whole problem | **Mixed** |
| 3 | **Generation-conditioned CAS transitions** | Coarse-state CAS throughout (P6, P8, P12, P19, P32 are all real predicates), generation-conditioned nowhere. P4, P24, P29, P31 and P35 have no predicate at all beyond the row existing | **Missing** |
| 4 | **Stale-writer rejection** | Not expressible: with no generation there is nothing to reject. The exposure is lower than the worker path's only because the plan row's states are mostly one-way (`pending → running → validated → approved`) — except where they are not, and P38/P41 deliberately reset `running → pending` so the reused-row problem §1 describes exists here too | **Missing** |
| 5 | **Idempotent replay** | **The segment's weakest principle.** Replay is not merely lossy, it is *refused*: `ApprovePlan` (CP13/CP14) and `StartRun` (CP24–CP27) both early-exit on the state their own first write produced, so a crash mid-remedy is unrecoverable by the same call. RP1 replays `finalizeGeneratedPlan`, and CP9(b) shows that replay can fail permanently on its own fresh ids | **Missing** |
| 6 | **Fail closed on unprovable provenance** | **Present at the one place it matters most** — RP2's `planner_ambiguous` is the correct refusal, and it is recorded with a reason a person can read. Undermined by ordering (CP31/CP32 write the terminal row before the explanation) and by the two silent divergences that fail *open* rather than closed: CP3 and CP19 both substitute a default execution policy for the real one and say nothing, and CP21 substitutes generic acceptance criteria and an empty write intent for the planner's — a read-only task silently becomes a mutating one | **Inconsistent** |

**The structural difference that must not be copied, for this segment.** The
plan segment's re-entry points are *functions with state preconditions*
(`GeneratePlan`'s status switch, `ApprovePlan`'s `validated` CAS, `StartRun`'s
`pending` check), not a durable command row. The worker path re-enters from the
outbox: a row whose status *is* the re-entry point, so re-entry is a read, not a
precondition on the caller's history. Any repair of CP24–CP27 should move the
plan→work unblock behind that same idea — a durable statement of "this run's
plan has completed and its work step must be `ready`" that any caller can
re-derive — rather than adding a generation token to `StartRun`.

### 5.2 The explicit gap ledger

The six questions the objective asks, answered for the whole audited path.
Every row names the §3 window or §2 write it comes from, so nothing here is a
summary of a summary.

#### Worker gaps already fixed (on this branch, verified in code)

| Gap | What closed it |
| --- | --- |
| RUNNING meaning "AO intended to launch" | The phased launch: RUNNING is licensed only by a durable confirmation (`LicensesRunning`, `dispatch_state_machine.go:820`), and the step deliberately does not move at the outbox claim (`dispatch.go:257-259`) |
| A launcher's word taken as proof | Both halves required to confirm — a session identity **and** an observed ownership read-back (`dispatch.go:720-733`); anything less routes to the unconfirmed state |
| Silence read as an answer | `SessionOwnershipEvidence.Missing` kept distinct from `Unavailable`, and `ownedExecution.Unprovable()` a first-class stop (`dispatch_reconcile.go:169-262`) |
| An unevidenced ambiguous stop | R3b's gate: the raise is refused if its evidence snapshot cannot be made durable, and the step is left exactly as it was (`dispatch_reconcile.go:864-869`) |
| A launch failure retried forever, or not at all | Classification + bounded automatic retry + an explicit human reopen (`worker_launch_recovery.go`), with the retry pacing floor that stops other entry points front-running the wake (`dispatch.go:170-172`) |
| A child run dispatching a provider process unowned | P28 + `requireChildOwnershipForDispatch`, re-stamped in both branches (CP18) |
| A stale tmux pane adopted as a live worker | `583b2eefc`'s pane recovery hardening, upstream of this audit |
| Composite crash windows hidden inside one table row | This document's own §3, split to adjacent boundaries — which is what surfaced C21a/C21c/C22/C23a/C23b |

#### Worker gaps still remaining

| Gap | Where | Disposition |
| --- | --- | --- |
| No mutating write names its launch generation | W4, W12a, W13a, W15–W20, W25, H1 (§2.5) | §6 — the CAS model |
| Split-brain between the two confirmation homes | C7, C22 | §6.5 (W15's unique index, W16 written only after) |
| A session-owning step stuck at `ready` | C9 | §6.5 (W20 conditioned on the session this generation wrote) |
| A completed step with no completion evidence | C12 | Fail-closed residue (§4.6); `ambiguous_review_state` stays the honest answer |
| A reconciliation boundary that makes its own remedy skippable | C15, C23a, C23b | §6 ordering discipline inside the composite remedies |
| A budget spent by crashes and races rather than decisions | C16, C20 | §6.3's `worker_launch_attempt` phase + §6.2's single-winner index |
| Outbox `failed` under a step still `running`/`ready`, unreopenable | C21a | §6.5 (H1's CAS widened to the state it actually observed) |
| A parked run with no reason on the ledger | C21c, and its plan-segment twins CP31/CP32 | §6 ordering; the plan-segment twins are **out of scope** (§6.9) |
| Fix-cycle attempt closed by recency, not identity | CF6b, CF7 | §4.7 |
| An unreadable fix-intent ledger licensing a resend | CF1-R | §2.3 — the smallest fix in the document, and the first that should land |
| **Plan segment: no re-entry after the first row of a multi-row remedy** | CP13, CP14, CP24–CP27 | **Documented, not scheduled** (§6.9) |
| **Plan segment: silent substitution of default policy / generic criteria** | CP3, CP19, CP21 | **Documented, not scheduled** (§6.9); CP21 is the highest-severity finding in this revision |
| **Plan segment: `finalizeGeneratedPlan` replay can fail permanently** | CP9(b) | **Documented, not scheduled** (§6.9) |

#### Generation ownership requirements

1. Every mutating worker write conditions on the generation token that owns the
   claim (§6.5), and the token is allocated **before** the claim (§6.3's
   `worker_dispatch_authorized`), never derived after the fact.
2. An attempt row belongs to exactly one generation (`dispatch_generation`,
   §6.2) and may be reused by a re-entering dispatch only when the stamps match.
3. The step's session binding records **which generation bound it**
   (`session_dispatch_generation`), because "release my own binding" is not
   otherwise expressible as a predicate (§6.6 invariant 7).
4. `dispatch_generation` and `runtime_instance_id` are separate identities and
   are never compared to each other: one names a claim, the other an execution.
5. No token is ever a wildcard. An unstamped legacy row is a distinct state and
   is movable only by a writer that observed it unstamped (§6.6 invariant 4).
6. In the plan segment the equivalent obligation is already met **by natural
   key** wherever it is met at all — `planned_task_id`, `UNIQUE(workflow_run_id,
   plan_step_id)` — and any future work there should extend that mechanism
   (CP9's fix is a derived task id) rather than introduce a second, token-shaped
   one.

#### Stale writer rejection requirements

1. A writer whose claim was released and re-taken across a full `dispatched →
   failed → pending → dispatched` turn must update **zero rows** at W12, W13,
   W17, W19 and W20 (§6.8 test 1).
2. Losing a claim is a **no-op, never an error and never an attention stop**
   (§6.6 invariant 3): a lost claim means another writer is handling the step.
3. A confirmation replayed for the same generation loses its insert to the
   unique index and is read as "already confirmed" — idempotent replay without a
   read-then-write (§6.5, W15).
4. A human reopen resumes exactly the failure a person looked at
   (`failure_generation`), and two concurrent Continues produce one reopen
   (§6.8 test 2).
5. W17 is the one write whose loss would put two workers on one worktree: on a
   mismatch it **stops**, it never overwrites.

#### Recovery rules

1. **Intent before action, everywhere** — W7 before the launcher, R1 before its
   remedy, H1 before the reopen, A1 before the close, A2 before the step moves,
   F5 before `Send`. In the plan segment this rule is inverted twice (CP30's
   budget row after the reset, CP31/CP32's explanation after the terminal row).
2. **Evidence before transition** — W23 before W21 (§6.6 invariant 6), and the
   same inversion fixed for F9/F10.
3. **Re-entry must be a read of durable state, not a precondition on the
   caller** — the outbox row is the worker path's re-entry point, and it is what
   the plan segment lacks (§5.1's structural difference).
4. **A remedy's first row must not make the rest skippable** — C15's lesson,
   applied to R2/R3 and to every plan-segment multi-row remedy.
5. **Bounded, and bounded by decisions rather than by crashes** — the retry
   budget is allocated durably at the start of the attempt, not counted from
   whatever records survived it.
6. **Adopt before relaunch** — reconciliation runs before dispatch at every
   entry point (`recovery.go:174`, `workflow.go:1291`), so a live worker AO
   has not yet recognised is adopted before anything can start a second one over
   it. The planner has no equivalent, which is precisely CP7.

#### Provenance / fail-closed cases

These must **stay** stopped, and the implementation step must not "fix" them:

| Case | Why it cannot be decided |
| --- | --- |
| C5 / the launch-unconfirmed window | One durable write wide by construction; adoption by natural key + ownership probe is the answer, and an unprovable probe stops |
| C12 | The completion evidence never existed; synthesising it would invent a verdict |
| §4.3's residue (`Unprovable` runtime or ownership read, absent or foreign generation, a conditional clear that updates zero rows) | The deciding fact belongs to a component AO does not own and cannot prove is finished |
| §4.2's residue (unreadable, absent or mismatched `dispatch_generation`; no readable canonical terminal record) | The attempt stays open for the reaper rather than being closed on an assumption |
| CF2 | `Send` has no idempotency key; an unprovable delivery parks once with its evidence |
| CF6a | No `fix_observed_*` checkpoint was ever written, so there is no outcome to read and none may be invented |
| **CP7** | The planner subprocess's outcome is not knowable from workflow's tables. `planner_ambiguous` is correct — and expensive, which is an argument for an adoption path later, not for guessing now |
| **CP9(b)** | Fail-closed by accident rather than by design: the park is right, the permanence is not. Listed here so it is not mistaken for a considered residue |

And the two that fail **open** and are therefore defects, not residues:
**CF1-R** (§2.3, an unreadable intent ledger licensing a resend) and
**CP3/CP19/CP21** (a default policy or a generic artifact silently substituted
for the real one, with no record that a substitution happened).

---

## 6. Proposed worker CAS schema and transition rules

This is the contract the next implementation step delivers. Nothing here is
implemented yet.

### 6.1 Reuse before addition

`workflow_outbox.dispatch_generation` and `workflow_outbox.failure_generation`
(migrations `0137`, `0138`) are **columns on the shared outbox table**, and the
four CAS store methods over them (`workflow.go:131-157`) take the entry id and
the token as plain strings. They are not reviewer-specific in either schema or
signature. The worker path adopts them as-is:

- no new outbox columns;
- no new store methods for the outbox;
- no fork of the reviewer's Go helpers — the worker gets its own
  `workerDispatchGeneration` type with its own `valid()` / `key()` / `casValue()`
  semantics, in `worker_launch_recovery.go`'s own vocabulary.

### 6.2 New durable fields

| Migration | Change | Why |
| --- | --- | --- |
| `0139_worker_attempt_dispatch_identity.sql` | `ALTER TABLE workflow_attempts ADD COLUMN dispatch_generation TEXT NOT NULL DEFAULT ''` | Ties an attempt row to the claim that opened it. Makes `openWorkerAttempt` able to reject a foreign open attempt instead of adopting it (§4.2), and makes every attempt close conditionable |
| `0139` (same file) | `ALTER TABLE workflow_attempts ADD COLUMN runtime_instance_id TEXT NOT NULL DEFAULT ''` | The generation fence for the *execution*, from `SessionOwnershipEvidence.RuntimeLaunchID`. A recorded instance id that no longer matches the observed one is positive evidence the execution is gone (§4.3). Empty means "not recorded" and always fails closed — never "no instance" |
| `0140_worker_dispatch_generation.sql` | `ALTER TABLE workflow_dispatch_checkpoints ADD COLUMN dispatch_generation TEXT NOT NULL DEFAULT ''` | Makes every launch boundary attributable to a claim, so reconciliation can tell "the confirmation for the generation I am holding" from "a confirmation" |
| `0140` (same file) | `CREATE UNIQUE INDEX idx_workflow_dispatch_confirmation ON workflow_dispatch_checkpoints (workflow_step_id, dispatch_generation) WHERE phase = 'worker_dispatched' AND dispatch_generation <> ''` | One confirmation per generation, enforced by SQLite. A replayed confirmation loses the insert and is read as "already confirmed" — idempotent replay without a read-then-write |
| `0139` (same file) | `ALTER TABLE workflow_steps ADD COLUMN session_dispatch_generation TEXT NOT NULL DEFAULT ''` | Records **which generation bound the step's session**, stamped by W17. Without it, "release my own binding" (§4.3, part 3 of P) is not expressible as a predicate, and R3R could not be distinguished from releasing another generation's ownership. Empty means "bound before this protocol" and always fails closed |
| `0141_worker_launch_reset_single_winner.sql` | Backfill `head_sha = 'worker-launch-reset-legacy-' \|\| id` for existing `worker_launch_human_retry` rows, then `CREATE UNIQUE INDEX … ON workflow_checkpoints (workflow_step_id, head_sha) WHERE durable_phase = 'worker_launch_human_retry'` | Single-winner human reset per failed generation (§4.5). The backfill is mandatory and must run first, exactly as in `0136`: this phase is not new, existing databases already hold colliding rows, and `CREATE UNIQUE INDEX` would wedge startup on precisely the installations that have used the path most |

All six changes are strictly additive: nullable or `DEFAULT ''`, nothing
backfilled except the `0141` legacy namespacing, and every legacy row reads back
with an empty token that fails closed.

### 6.3 New durable phases

| Phase | Written | Carries |
| --- | --- | --- |
| `worker_dispatch_authorized` | **Before** the outbox claim (new W3.5) | The idempotency key, the predecessor generation being replaced (if any), the harness routing chose, and the step/run identity. Its checkpoint id **is** the dispatch generation token |
| `worker_launch_attempt` | Immediately after the claim, before anything the attempt performs | The durable allocation of one attempt against the budget, so an attempt that dies before writing a failure record still spends its budget (the reviewer's `review_launch_attempt` reasoning, `review_launch_recovery.go:735-750`, applied to the worker) |

### 6.4 The Go-side value

```go
// workerDispatchGeneration is the durable identity of ONE worker dispatch
// claim: the outbox row it took, the key it took it under, and the
// authorization checkpoint whose id is the claim token itself.
type workerDispatchGeneration struct {
    OutboxID       string
    IdempotencyKey string
    AuthorizationID string // checkpoint id; the token stamped on the outbox row
    Stamped        bool    // false for a row claimed before this protocol existed
}
```

`valid()` requires all three ids. `casValue()` returns the token for a stamped
claim and the **empty string** for an unstamped legacy row — never a wildcard,
for the same reason as `reviewLaunchGeneration.casValue()`: an unstamped row and
a stamped one are different states and each writer may only move the one it
observed.

A separate `workerLaunchFailureGeneration` (`<idempotency key>|<failure record
id>`) identifies which *failure* a human reopen is resuming, stamped into
`failure_generation` by the same statement that fails the row.

### 6.5 Transition rules

| Write | New predicate | On zero rows updated |
| --- | --- | --- |
| W3.5 authorize | insert (append-only) | write failure ⇒ **do not claim**; nothing was launched |
| W4 claim | `ClaimWorkflowOutboxDispatch(id, now, gen.token)` — `WHERE id = ? AND status = 'pending'`, stamping `dispatch_generation` | somebody else owns the claim ⇒ return, no-op. Never an error |
| W6 attempt open | `dispatch_generation = gen.token` stamped at insert; an existing open attempt is reused **only** when its `dispatch_generation` matches, else the dispatch refuses (fail closed) | refuse the launch and reconcile |
| W8/W25/R2 attempt close | `UpdateWorkflowAttemptOutcomeIfOpen(id, gen.token)` — `WHERE id = ? AND outcome IS NULL AND dispatch_generation = ?` | already closed, or not ours ⇒ no-op |
| **W25R** same-generation completion close (§4.2 rule 4) — the recovery that fires when W25 never ran | the same `UpdateWorkflowAttemptOutcomeIfOpen`, called from the observation/reconcile pass once a canonical terminal record for `gen.token` is readable and the attempt's generation equals the step's current one; outcome derived from that record | evidence unreadable, generation absent/mismatched, or no terminal record ⇒ **leave the attempt open** and fall through to `attemptReaper` |
| **R3R** proven-gone release (§4.3) — the audited session-clear, and the **only** transition permitted to clear `session_id` | **new** `ClearWorkflowStepSessionIfGeneration(stepID, sessionID, gen.token, now)` — `WHERE id = ? AND session_id = ? AND session_dispatch_generation = ?`, setting both columns back to NULL/`''`. Called **only** after proof obligation P (§4.3) holds in full: G owns the claim, the attempt is G's, the binding is G's, and the attempt's `runtime_instance_id` is non-empty, readable, and differs from the observed instance for `S` (or ownership reads `Missing`). The two identities are checked separately and never compared to each other. Followed by the attempt close under G and the claim release under G; the retry runs under a new generation | any part of P absent, unreadable, or mismatched ⇒ **no clear, no retry**; stop as `ambiguous_worker_state`. Zero rows updated ⇒ the binding changed under us ⇒ stop; never widen the predicate and retry |
| W12a retryable release | `ReleaseDispatchedWorkflowOutboxGeneration(id, class, gen.token)` | claim lost ⇒ **do not schedule a wake, do not park the run**; another writer owns this step |
| W13a permanent fail | `FailWorkflowOutboxWithGeneration(id, dispatched, now, class, failureGen, gen.token)` | claim lost ⇒ no-op; the step is not failed, the run is not parked |
| W15 confirmation | insert with `dispatch_generation`; unique index makes a replay lose | insert conflict ⇒ read as "already confirmed for this generation", continue to W16 |
| W16 ledger marker | written only after W15 succeeded **or** conflicted (both mean confirmed) | — |
| W17 session write | **new** `UpdateWorkflowStepSessionIfUnset(stepID, sessionID, gen.token, now)` — `WHERE id = ? AND (session_id IS NULL OR session_id = ?)`, stamping `session_dispatch_generation = gen.token` alongside the session so the binding records its author (§4.3 P, part 3) | a different session already owns the step ⇒ **stop, do not overwrite.** This is the one write whose loss would put two workers on one worktree |
| W19 acknowledge | generation-conditioned acknowledge (`WHERE dispatch_generation = ?`), clearing the token | claim lost ⇒ no-op |
| W20 RUNNING | `UpdateWorkflowStepStateIfSession(stepID, ready, running, sessionID, now)` — the worker's analogue of `UpdateWorkflowStepStateIfReviewRun`, conditioned on the session this generation just wrote | not ours ⇒ no-op |
| W21 completion transition | moved **after** W23; conditioned on `(stepID, expected=running, session_id = ?)` | benign race ⇒ skip, as today |
| H1 human reopen | `worker_launch_human_retry` insert under the unique index (single winner), then `ReopenFailedWorkflowOutboxGeneration(id, class, failureGen.casValue())` | index conflict ⇒ "already resumed"; CAS miss ⇒ the failure a person looked at is no longer the current one ⇒ **no reopen** |

### 6.6 Invariants the implementation must hold

1. **RUNNING ⟹ a durable confirmation for the generation that owns the step's
   session.** (Today: RUNNING ⟹ *a* confirmation exists.)
2. **An open attempt row ⟹ a live claim, or a bounded convergence path to
   closing it.** No attempt may be open whose `dispatch_generation` is not the
   outbox row's current `dispatch_generation`, or whose step has moved past the
   work it described. This is an invariant the system *converges to*, not one
   every instant satisfies: the C13 window transiently violates it by
   construction, and W25R is what restores it on the next pass. Where W25R
   cannot fire — unreadable or mismatched identity — the row stays open and
   visible to the reaper rather than being closed on an assumption.
3. **A worker path write that lost its claim writes nothing and reports
   nothing.** Losing a claim is a no-op, never an error and never an attention
   stop — a lost claim means another writer is handling the step.
4. **No token is ever a wildcard.** Legacy (unstamped) rows are reopened only by
   a writer that observed them unstamped.
5. **Unprovable ⇒ stop, everywhere.** Including the two read paths that
   currently fail open: `workerLaunchAttemptCount` must return the budget
   maximum (not 0) on an unreadable ledger, matching
   `workerLaunchRecoveryGenerations`.
6. **Evidence before transition, without exception.** W23 before W21 closes the
   last inversion in the path (§4.6).
7. **The session is never rebound; it is cleared only by its own generation
   under proof.** No write ever repoints `session_id` from one session to
   another — the reviewer's authority-pointer rebinding has no worker analogue
   (§5, difference 1), and a worker generation that lost its claim stops rather
   than superseding the winner. The single transition permitted to *clear* the
   column is **R3R** (§6.5), and it is bounded by four conditions that must all
   hold (proof obligation P, §4.3): the clearing generation owns the claim, owns
   the open attempt, is the generation that wrote the binding
   (`session_dispatch_generation`), and can positively prove the execution named
   by that attempt's `runtime_instance_id` has ended. `dispatch_generation` and
   `runtime_instance_id` are separate identities and are never compared against
   each other. If any condition is absent, unreadable, or mismatched — or if the
   conditional clear updates zero rows — the binding is untouchable and the step
   stops as `ambiguous_worker_state`. No other code path, reconciler or
   otherwise, may write NULL to `session_id`.

### 6.7 Explicitly out of scope for the implementation step

- The parent/child attention mirror (§4.4) — already correct, and derived per
  pass by design.
- **The reviewer lifecycle, entirely.** No change to `review_launch_phases.go`,
  `review_launch_recovery.go`, `review_authority.go`, or migrations
  `0135`–`0138`; no generalisation of the reviewer's helpers to serve the
  worker; no alteration of review cycles, epochs or the authority pointer. The
  worker adopts the *table-generic* outbox CAS methods as they already are
  (§6.1) and writes its own Go vocabulary over them. **The one exception is
  evidential and comes later, not now: if a later step's tests prove a shared
  regression** — a worker-side change breaking reviewer behaviour under test, or
  a defect a test localises inside a primitive both paths share — the reviewer
  lifecycle may be changed, scoped to what that test proves and no further.
  Nothing in this document proposes such a change, and none may be made on
  reasoning alone.
- Verify dispatch. **The plan segment is *not* out of scope for the audit** —
  §2.0, §3.0 and §5.1 document it in full — but no §6 change touches it; §6.9
  states why, and carries its backlog.
- The fix path, **except** for the four narrow changes §4.7 names: binding the
  attempt close to the cycle's own `FixAttemptID`, conditioning that close,
  stamping the fix attempt row, and inverting F9/F10. Fix *dispatch* — the
  per-cycle outbox key, prompt delivery, transport retry, and
  `resolveFixDeliveryAfterRestart` — is untouched.
- The 30 s settle windows (`dispatchReconcileSettleWindow`,
  `adoptOrMarkAmbiguous`'s in-flight window) — they answer "could this be
  happening right now?", which CAS does not answer, and they must keep agreeing
  with each other.
- `attemptReaper` and `work_adoption`: both are already exactly-once by keyed
  checkpoint and both stay. The CAS work reduces how often they are needed; it
  does not replace them.

### 6.8 Test obligations for the implementation step

Each ambiguous class in §4 gets a test that constructs the crash window
directly against a real store (the existing `dispatch_reconcile_test.go` /
`recovery_hardening_test.go` fixtures already do this shape) and asserts the
CAS refusal, not merely the happy path:

1. A stale dispatch that lost its claim across a full `dispatched → failed →
   pending → dispatched` turn writes zero rows at W12, W13, W17, W19 and W20.
2. Two concurrent Continues on one failed generation produce exactly one
   `worker_launch_human_retry` row and one reopen.
3. A replayed confirmation for the same generation is a no-op and still reaches
   RUNNING exactly once.
4. An attempt open under generation A is never closed by generation B.
5. A crash between W23 and W21 (in the new order) is finished by the next
   observation with an identical decision.
6. A legacy, unstamped outbox row remains reopenable by a writer that observed
   it unstamped, and is not reopenable by one that observed a token.
7. **W25R, both directions (§4.2).** A crash after W23 and before W25 leaves an
   open same-generation attempt; the next observation closes it with the outcome
   read off the completion checkpoint, and a second pass updates zero rows.
   Conversely, the same fixture with the attempt's `dispatch_generation` cleared,
   set to another generation, or with the terminal record unreadable leaves the
   attempt **open** and raises nothing — the reaper remains its only route.
8. **The fix attempt close, both directions (§4.7).** Two cycles on one fix
   step, cycle N's observation arriving after cycle N+1's dispatch (CF7): cycle
   N's close lands on cycle N's own row and cycle N+1's row is **untouched and
   still open** — the assertion that fails on today's code. Plus a CF6 fixture
   (crash between the `fix_observed_*` checkpoint and the close) finished by the
   next pass with the outcome read off that checkpoint, and a second pass
   updating zero rows. Then the fail-closed half: an attempt row with no
   readable cycle stamp is left **open** and the observation raises nothing —
   asserting specifically that the close does *not* fall back to
   `GetLatestWorkflowAttempt`. Separately, a **CF6a** fixture (terminal-for-the-
   cycle, attempt open, **no** `fix_observed_*` checkpoint) must leave the
   attempt open and raise nothing — asserting the close does not synthesise an
   outcome it cannot read.
   Two **regression** tests guard what §4.7 does not touch, because the first
   draft of this audit got them wrong: a CF4/CF5 fixture (attempt open, step
   `running`, no `fix_dispatched` checkpoint, intent record present) must still
   be observed normally off the `fix_dispatch_intent` checkpoint and must reach
   a terminal-for-the-cycle state without human attention; and a CF1a fixture
   (outbox `dispatched`, no intent record, **read succeeding**) must still be
   delivered exactly once by `classifyFixDelivery`'s `fixDeliveryNotSent` path.
9. **The fix-intent ledger tri-state (§2.3, §4.7 item 7).** With
   `ListWorkflowCheckpoints` made to fail, `resolveFixDeliveryAfterRestart` must
   **not** call `Send` — it must park the run once via `markFixDeliveryUnproven`
   with the read failure in the evidence. The companion assertion is that a
   *successful* read returning no intent still delivers exactly once, so the fix
   does not turn a provable non-delivery into a stall. This test fails on today's
   code, which is the point: the current path re-sends.
10. **R3R, both directions (§4.3).** With proof obligation P satisfied in full —
   the reconciling generation owns the claim, the open attempt and the binding
   (`session_dispatch_generation`), and the attempt's `runtime_instance_id` is
   non-empty and differs from the observed instance for the session (or
   ownership reads back `Missing`) — the step converges without a human: attempt
   closed, `session_id` cleared, retry dispatched under a *new* generation that
   binds a fresh session. Then one case per way P can fail, each asserting the
   binding is **untouched** and the step stops as `ambiguous_worker_state`:
   empty `runtime_instance_id`; unreadable/`Unavailable` runtime read; empty or
   foreign `session_dispatch_generation`; attempt belonging to another
   generation; and a concurrent binding change making the conditional clear
   update zero rows. Both directions are required — a test that asserts only the
   fail-closed half would pass on today's code and prove nothing.


### 6.9 The plan segment: audited in full, changed by nothing here

§2.0, §3.0 and §5.1 audit the plan segment to the same standard as the launch
segment — every durable write, every crash boundary between adjacent writes,
and the same six reviewer principles applied. **The implementation step §6
describes changes none of it.** That is a scoping decision about the *fix*, not
a gap in the *audit*, and the difference matters: an undocumented path cannot be
scheduled, and this one now can be.

Two reasons, both narrow:

1. **Nothing in §6 needs it.** The worker CAS model is about a reused outbox row
   and a generation-less claim. The plan segment has neither: its identity
   problem is already solved where it is solved at all, by natural keys
   (`planned_task_id`, `UNIQUE(workflow_run_id, plan_step_id)`), and its real
   defects are *ordering* and *re-entry* defects (§5.1, principle 5), which a
   generation token would not fix.
2. **Its highest-severity finding is not a CAS problem.** CP21 — a child run
   dispatched against generic acceptance criteria and an empty write intent
   because a crash fell between P31 and the recovery branch that does not repeat
   it — is fixed by passing the planner's criteria into `createSingleTaskRun`
   so the child is *created* correct, not by conditioning the overwrite. Mixing
   that into the CAS step would couple two unrelated changes.

The plan-segment work is therefore its own step, and this is its backlog, in
severity order. Each row names the window it closes and the shape of the fix, so
the step can be planned from this document without re-deriving it:

| Priority | Finding | Fix shape |
| --- | --- | --- |
| 1 | **CP21** — a recovered child dispatches on generic criteria and `WriteIntent` Unspecified; a read-only task is silently treated as mutating | Build the child with its real artifact: pass criteria + write intent through `createSingleTaskRun` so P27 and P31 are one transaction. Removes the window rather than guarding it |
| 2 | **CP24–CP27** — a crash inside `StartRun` leaves the plan→work unblock unreachable by every entry point | Make the unblock re-derivable: a durable "plan completed, work must be `ready`" fact any caller can act on, or a `StartRun` that re-enters on `(run running ∧ plan step completed ∧ work step pending)` instead of on `run pending` |
| 3 | **CP19** — a recovered child keeps the default execution policy instead of the parent's | Call `inheritExecutionPolicySnapshot` in the recovery branch too; it is already idempotent |
| 4 | **CP9(b)** — `finalizeGeneratedPlan`'s replay mints fresh task ids, loses them to the unique key, then fails the FK-bound relationship insert forever | Derive the task id from `(workflow_run_id, plan_step_id)` so a replay computes the same ids it computed the first time |
| 5 | **CP11/CP12** — an autonomous objective stalls at `validated` with no resolver | A `validated` case in boot `reconcileRun`'s plan switch, and an approval re-entry from the heartbeat, for `approval_mode = auto` only |
| 6 | **CP13/CP14** — `ApprovePlan`'s early exit skips the run/step transitions its own first write made necessary | Move the early exit after the transitions, or make them re-derivable from `plan.status = approved` |
| 7 | **CP30/CP31/CP32** — the retry budget recorded after the reset, the explanation after the terminal row | Reorder: allocate the budget and write the reason first, exactly as `review_launch_attempt` does |
| 8 | **CP3** — a crash before the policy freeze silently downgrades an autonomous objective to manual | Freeze the policy in the same transaction as the run, or make `RP5` read the requested mode from a durable field rather than from the snapshot it is trying to heal |
| 9 | **CP1** — a master run with no plan row is permanently inert | One transaction for P1′ + P2 |
| 10 | **CP7** — a planner in flight across a restart is discarded rather than adopted | A planner adoption path (intent record + subprocess identity), the planner's analogue of `adoptLiveLaunch`. The largest of the ten and the least urgent: `planner_ambiguous` is already correct, only wasteful |

**Implementation status (P0-A).** Priorities 1–5 and 8, plus the P9 no-op, are
implemented on this branch. What each one now does, and where:

| Finding | State | Where |
| --- | --- | --- |
| CP21 | **Fixed.** The planned task's criteria and write intent travel *into* `createRunWithPlanArtifact`, so the child's plan step is written in the same transaction as the run. `healPlannedTaskArtifact` re-binds a child created before the fix (or by a crash in the old two-write window) from its durable task row | `workflow.go` (`plannedTaskArtifact`), `child_task_artifact.go`, `master_coordinator.go` |
| CP24–CP27 | **Fixed.** `StartRun` re-enters on the *obligation* (`planUnblockOwed`: work step still `pending`, plan step not terminal) rather than on `run.State == pending`, completing the plan step from whatever state it is actually in; boot recovery calls `resumeInterruptedStart` before the generic interrupted-step fallback can park the run | `start_resume.go`, `workflow.go`, `recovery.go` |
| CP19 | **Fixed.** `inheritExecutionPolicySnapshot` runs in the recovery branch too, copies the whole frozen policy (not only priority-bearing ones), and `requireInheritedExecutionPolicy` refuses to dispatch a child that cannot prove it inherited its parent's | `execution_policy_resolve.go`, `master_coordinator.go` |
| CP9(b) | **Fixed.** Task identity is the natural key: whatever is already persisted for `(workflow_run_id, plan_step_id)`, else `canonicalTaskID` derived from it. Relationship endpoints are verified against the durable rows before the FK-bound insert | `plan_identity.go`, `master_coordinator.go` |
| CP11/CP12 | **Fixed.** `resumeValidatedPlan` is reachable from boot `reconcileRun`'s new `validated` case and from `ContinueRun` (the wake poller's entry point). A manual objective is still left for its person | `validated_plan_resume.go`, `recovery.go`, `workflow.go` |
| CP3 | **Fixed.** Run creation stamps `provenance.source = "unfrozen"` in the same statement as the run; the freeze stamps `"frozen"`; recovery re-freezes an owned-but-unfrozen run as `"recovered"` and **fails closed** (parking the run) when it cannot. Legacy and unowned runs are untouched | `domain/workflow_policy.go`, `execution_policy_resolve.go`, `recovery.go` |
| P9 | **Corrected, not removed.** The normalized re-persist has its own statement (`PersistNormalizedWorkflowPlan`) whose CAS matches the state that actually holds (`running`/`responded`) and is additionally conditioned on the exact bytes the caller read; the result is checked, and a stale writer is refused | `queries/workflow_plan.sql`, `store/workflow_plan_store.go`, `master_coordinator.go` |

Priorities 6, 7, 9 and 10 (CP13/CP14, CP30–CP32, CP1, CP7) are **not**
implemented and remain exactly as described above.

Nothing above is a licence to change the reviewer lifecycle, and nothing above
is in the §6 implementation step.

---

## 7. Summary

This audit covers the whole path — objective/workflow creation → plan
generation → plan persistence/approval → run/step transitions → a task becoming
dispatchable → dispatch intent → launch → confirmation → RUNNING → terminal/idle
→ completion evidence → review transition — with every durable write named
(§2.0, §2.1, §2.2) and every crash boundary between two *adjacent* writes named
(§3.0, §3.1).

The **work-step launch segment** already has the two hardest things right:
**ordering** (every durable record precedes the action it describes, and RUNNING
is gated on a confirmation) and **honest evidence** (`observed` / `missing` /
`unavailable` kept apart, with `unprovable` a first-class outcome that stops).
What it lacks is the one thing the reviewer path added and proved: a **durable
generation token that every mutating write conditions on**, so that a writer
which paused across a turn of the reused outbox row cannot win a write it no
longer owns.

The **plan segment** has the opposite profile. Its identity problem is largely
solved — by natural keys rather than by tokens (`planned_task_id`,
`UNIQUE(workflow_run_id, plan_step_id)`) — and its CAS predicates are real
(P6, P8, P12, P19, P32). What it lacks is **re-entry**: `ApprovePlan` and
`StartRun` both refuse to act on the state their own first write produced, so a
crash inside either remedy is unrecoverable by the same call, and three of its
windows substitute a default for the real thing — a default execution policy
(CP3, CP19) or a generic plan artifact with an empty write intent (CP21) —
without recording that a substitution happened.

**Eighty-three windows are enumerated in §3**, each one the interval between two
*adjacent* durable writes: **thirty-two on the plan segment** (CP1–CP32, §3.0),
**thirty-nine on the work path** (C1a–C1e, C2a–C2b, C3–C13, C14a–C14c, C15–C17,
C18a–C18b, C19–C20, C21a–C21c, C22, C23a–C23c, C24a–C24d) and **twelve on the
fix cycle**
(CF1a–CF1d, CF1-R, CF2–CF5, CF6a–CF6b, CF7). Sub-lettered rows count as one
window each. The three sets are counted separately throughout, because they are
different paths with different resolvers; §3's tables are the authority, and
these are counts of *table rows*, not of the ambiguous classes in §4, which group
several windows each. The **fifty-four durable writes of the plan segment** (P1,
P1′, P2–P48, plus RP1–RP5 for boot recovery) are enumerated the same way, and no row of §2.0
collapses two mutations a crash between them would leave distinguishable.

On the work path most windows are already resolved by reconciliation, the
reaper, or pure re-derivation. **Eleven are not**, and they are named rather
than counted off so each can be checked against §3: C7 (split-brain between the
two confirmation homes), C9 (a session-owning step stuck at `ready`), C12 (a
completed step with no completion evidence), C15 (a reconciliation boundary that
makes its own remedy skippable), the pair C16/C20 (a budget spent by crashes and
races rather than by decisions), and five that splitting the composite remedies
exposed — C21a (a failed outbox row under a step still `running`, which H1's
`failed`-only CAS cannot reopen), C21c (a parked run with no reason on the
ledger), C22 (the unconfirmed-side twin of C7's split-brain), and C23a/C23b (a
reconciliation retry whose attempt-close and step transition are both skippable
by C15's short-circuit). C14c is deliberately not among them: cycle 1's review
unblock waiting for an explicit Continue is a design decision (`recovery.go:250`
and `workflow.go:941` both pass `includeCycle1Unblock=false` on purpose), and the
master path issues that Continue itself. Those eleven are what a
generation-conditioned CAS,
plus ordering discipline *inside* the composite remedies, has to close. §6 is
that CAS — and C21a/C21c/C22/C23a/C23b are new to this revision, surfaced only
because the composite rows were split into their real boundaries.

On the fix cycle the split is different and mostly favourable. **Seven of the
twelve converge today** — CF1a–CF1d because F5's fatal intent-before-`Send`
contract makes "never delivered" provable on a successful read, CF3 because
`classifyFixDelivery` decides from session evidence rather than from where the
crash fell, and CF4/CF5 because `observeFixStep` reads the *intent* checkpoint
and does not require `fix_dispatched`. **Two are fail-closed by design**: CF2
(`Send` has no idempotency key, so an unprovable delivery parks once with its
evidence) and **CF6a** (no terminal checkpoint was ever written, so there is no
outcome to read and none may be invented). **Two are open and fixable** — CF6b
and CF7, §4.7's subject. **One is a defect** — CF1-R, below.

**One row is neither resolved nor a residue: CF1-R.** An unreadable intent
ledger currently returns the same value as a proven absent intent and licenses a
re-send of the fix findings (§2.3). That is a fail-*open* defect against
principle 6, and §4.7 item 7 — the tri-state `findFixDispatchIntent` its own
comment already describes — is the smallest fix in this document. It needs no
migration and no new column, and it should land before the generation work.

On the plan segment, **thirteen of the thirty-two windows are unresolved**, and
they are named rather than counted so each can be checked against §3.0: CP1 (a
master run with no plan row, permanently inert), CP3 and CP19 (an autonomous
objective, and then a child of one, silently downgraded to the default execution
policy), CP9(b) (a `finalizeGeneratedPlan` replay that mints fresh ids, loses
them to the unique key and then fails its FK-bound relationship insert on every
boot thereafter), CP11 and CP12 (an autonomous plan stalled at `validated` with
no resolver), CP13 and CP14 (`ApprovePlan`'s early exit skipping the run and step
transitions its own approval made necessary), **CP21** (the severest: a recovered
child dispatched against generic acceptance criteria and an empty write intent,
so a read-only task is prompted and classified as mutating), CP24–CP27 (a crash
inside `StartRun` leaving the plan→work unblock unreachable from every entry
point), and CP30–CP32 (a retry budget recorded after the reset it bounds, and a
stop recorded after the terminal row it explains). Two more are fail-closed and
correct — CP7, the planner-in-flight window, which `planner_ambiguous` refuses
rather than guesses, and CP6's residue — and the rest converge by design: CP4 and
CP18 are the two windows the code was explicitly written to heal, and CP22/CP23
converge on `planned_task_id`, the one natural key in this document that does
what a generation token would. §6.9 carries the backlog in severity order; none
of it is in the §6 implementation step, and saying so is a scoping decision about
the fix, not a limit on the audit.

On the seven ambiguous classes (§4), the verdict is that **all seven are
resolvable** — 4.1, 4.2, 4.4 (already resolved upstream), 4.5, 4.6, 4.7, and 4.3
in both halves: *stale* by the durable runtime-instance fence, and *phantom*
whenever that fence lets AO prove, under a matching generation identity, that
the execution it launched is gone. 4.7 —
`fix_completed_but_attempt_open` — is resolvable in its CF6b and CF7 halves and
fail-closed in its CF6a half; it is the cheapest of the seven, and its CF7 part
is the only case in this document reachable with no crash at all: the fix path
already writes the binding it needs (`FixAttemptID`) and simply closes the newest
attempt row instead of the named one. Its scope is narrow and worth stating
plainly, because the delivery half of the fix path is *stronger* than the work
path's, not weaker — F5 writes its intent before `Send` and treats a failed write
as fatal, F12 decides from evidence and fails closed, and F13 makes a recovered
dispatch leave state identical to a first-pass one. Nothing in §4.7 touches any
of that.

The stance on the reviewer path is unchanged from the header: this audit reads
the reviewer model, adopts its principles and its already-generic store
primitives, and **proposes no reviewer lifecycle change** — unless and until a
later step's tests prove a shared regression, and then only as far as that proof
reaches.

Each class keeps a narrow fail-closed residue, listed in §4's table, and every
residue reduces to one predicate: **provability of a named thing by the
generation entitled to act on it**. AO
converges deterministically when it can name the generation and positively prove
the execution's state; it stops as `ambiguous_worker_state` when the evidence is
unreadable, the identity is absent or mismatched, or the deciding fact belongs
to a component AO does not own and cannot prove is finished. Those residues are
the design, not a backlog. The inverse is equally binding: a case AO *can* prove
must not be routed to a person out of caution, because a stop that a durable
fact could have answered is the same defect as a guess — it just fails in the
other direction.
