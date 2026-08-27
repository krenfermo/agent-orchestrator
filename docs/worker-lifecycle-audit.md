# Worker lifecycle audit — durable write points, crash windows, and the CAS model to adopt

**Status:** design contract. This document is the audit the rest of the worker
lifecycle work implements against. It changes no behaviour by itself; every
"proposed" item below is a commitment about what the next implementation step
writes, not a description of what the code does today.

**Scope:** the work step's end-to-end path —
plan → dispatch intent → launch → dispatch confirmation → RUNNING → worker
terminal/idle → completion evidence → review transition — as it stands on
`feat/engineering-control-center`.

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

The reviewer counterparts the comparison in §5 is taken from:
`review_launch_phases.go`, `review_launch_recovery.go`, `review_authority.go`,
and migrations `0135`–`0138`.

---

## 1. The durable substrate

Seven durable homes carry worker lifecycle state. Nothing else is state; every
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

`W#` labels are used by §3 and §6. "Guard" is the predicate the write is
actually made under today.

| # | Write | Site | Guard today |
| --- | --- | --- | --- |
| W0 | Outbox row enqueued (insert-or-get, idempotent on the step key) | `dispatch.go:149` | Natural key |
| W1 | `routing_decision` checkpoint (which provider was chosen / capacity wait) | `routing_dispatch.go:50` | none (append) |
| W2 | Branch lock acquired (direct-branch mode) | `branch_execution.go:95` | Lock table's own CAS |
| W3 | Run `waiting → running` when capacity came back | `dispatch.go:243` | `(runID, expected=waiting)` |
| W4 | **Outbox claim** `pending → dispatched` | `dispatch.go:248` | `(entryID, expected=pending)` — **no claim token** |
| W5 | `session_lifecycle_decision` checkpoint | `dispatch.go:292` | none (append) |
| W6 | **Attempt row opened** (`outcome IS NULL`) | `dispatch_state_machine.go:446-460` | "latest attempt is terminal or absent" — a read-then-write, not a CAS |
| W7 | **Dispatch intent boundary** (`LaunchOutcomeIntended`) | `dispatch_state_machine.go:426` | none (append); failure here refuses the launch |
| W8 | Attempt concluded `failed` on a pre-launch failure (intent write, runtime env, preflight) | `dispatch.go:578` | none — unconditional update of a named row |
| W9 | Launch-failure boundary (`LaunchOutcomeFailed`) | `dispatch_state_machine.go:536` | none (append, best-effort) |
| W10 | Ambiguous-launch boundary (launcher returned no session id) | `dispatch_state_machine.go:565` | none (append, best-effort) |
| W11 | `worker_launch_error` checkpoint (classification + deep error + attempt count) | `worker_launch_recovery.go:287` | none (append) |
| W12 | Retryable path: outbox `dispatched → pending` + durable wake + attention stop | `worker_launch_recovery.go:310` | `(entryID, expected=entry.Status)` — **no generation** |
| W13 | Permanent path: outbox → `failed`, step → `failed`, run → `needs_attention`, attention stop | `dispatch.go:815-845` | `(id, expected)` per row — **no generation** |
| W14 | Unconfirmed-launch boundary **and** `worker_launch_unconfirmed` checkpoint (two homes) | `dispatch_state_machine.go:671`, `:710` | none (append, best-effort both) |
| W15 | **Confirmation boundary** (`LaunchOutcomeDispatched`) — the gate RUNNING stands on | `dispatch.go:734` | none (append); requires `ownership.Observed` |
| W16 | `worker_dispatched` ledger checkpoint (the phase marker other readers key off) | `dispatch.go:762` | none (append) |
| W17 | **Step session written** (`workflow_steps.session_id`) | `dispatch.go:780` | none — unconditional |
| W18 | Branch lock re-pointed at the session | `dispatch.go:790` | best-effort |
| W19 | Outbox `dispatched → acknowledged` | `dispatch.go:794` | `(entryID, expected=entry.Status)` |
| W20 | **Step `ready → running`** | `dispatch.go:799` | `(stepID, expected=ready)` |
| W21 | Work observation transition (`running → completed` / `waiting` / `failed`) | `worker_progress.go:556` | `(stepID, expected=step.State)` |
| W22 | Run transition from observation | `worker_progress.go:574` | `(runID, expected=run.State)` |
| W23 | **Completion checkpoint** — `worker_observed_*` with `branch`, `worktree_path`, `base_sha`, `head_sha`, and (on completion) `fingerprint_after` | `worker_progress.go:604` | none (append) |
| W24 | Attention stop checkpoint when observation parks the run | `worker_progress.go:637` | once-per-occurrence dedupe |
| W25 | **Attempt finalized** (`succeeded` / `failed` + error class) | `worker_progress.go:682` | none — unconditional, error ignored |
| W26 | Review dispatch reads W23 (`GetLatestWorkflowCheckpointByStep`) for session/branch/worktree/target fingerprint | `review_dispatch.go:528-541` | Gated on `workStep.State == completed` |
| R1 | Reconciliation boundary (`worker_dispatch_reconciled`) | `dispatch_reconcile.go:903` | none (append); **mandatory**, not best-effort |
| R2 | Reconciliation retry: attempt closed, step `waiting → running`, then W11/W12 | `dispatch_reconcile.go:748-822` | `(id, expected)` per row |
| R3 | Reconciliation stop: evidence snapshot **first**, then attempt closed, step → `waiting`, attention stop | `dispatch_reconcile.go:838-891` | evidence gate refuses the raise if the snapshot cannot be made durable |
| H1 | Human reopen: `worker_launch_human_retry` checkpoint, then `ReopenFailedWorkflowStep`, then outbox `failed → pending`, then unpark | `worker_launch_recovery.go:583-615` | `(id, expected=failed)` per row — **no generation, no single-winner index** |
| A1 | `attempt_reaped_orphaned` checkpoint, then the attempt row closed `cancelled` | `attempt_reaper.go` | checkpoint keyed to attempt id ⇒ exactly-once |
| A2 | `work_commit_adopted` checkpoint, then the step completes | `work_adoption.go` | bounded generation counter in the record |

### What is already right

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

### What is missing

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

Each window is the interval between two adjacent durable writes. "Produces"
names the ambiguous state class from §4. "Resolver today" is the code that
actually cleans it up, if any.

| # | Window | Durable state left behind | Produces | Resolver today |
| --- | --- | --- | --- | --- |
| C1 | W0 → W4 | Outbox `pending`, no attempt, no boundary | none (a step that has not started) | `dispatchWorkStep` re-enters from `pending` |
| C2 | W4 → W6 | Outbox `dispatched`, **no boundary at all**, no attempt | **stale/phantom dispatched command** | `adoptOrMarkAmbiguous` (`dispatch.go:586`) after a 30 s settle window; reconciliation explicitly defers to it (`dispatch_reconcile.go:624`) |
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
| C14 | W25 → review dispatch | Complete and consistent | none | `cascade.go:180` |
| C15 | R1 → R2/R3 | Reconciliation boundary is newest; nothing else moved | Repeat pass sees `DispatchPhaseWorkerDispatchReconciled` as newest and returns "already reconciled" (`dispatch_reconcile.go:538`) — **the remedy is skipped entirely** | none. The boundary is written before the remedy precisely so a duplicate wake is a no-op, but a crash in the same window makes the *first* remedy a no-op too |
| C16 | H1 checkpoint → `ReopenFailedWorkflowStep` | Human-retry checkpoint written; step still `failed`; outbox still `failed` | **repeated wake/reconcile**: the generation is counted (it bounds `maxWorkerLaunchRecoveryGenerations`) but nothing was reopened. A second Continue burns a second generation | documented as intentional (`worker_launch_recovery.go:505-513`); the budget is spent by crashes rather than by decisions |
| C17 | `ReopenFailedWorkflowStep` → outbox reopen | Step `ready`, outbox `failed` | Step ready over a failed command | Next Continue finishes it (`worker_launch_recovery.go:513`) |
| C18 | W12 (outbox → pending) → wake scheduled | Outbox pending, no wake | Step waits for any other dispatch entry point; `workerLaunchRetryDelay` floor still applies | boot `Reconcile` / capacity wake |
| C19 | Master: child run moves out of `needs_attention` → parent mirror cleared | Parent durably `child_needs_attention`, child running | **child running while parent needs_attention** | `reconcileMirroredChildStop` (`attention.go:770` (`reconcileMirroredChildStop`)), driven by the parent's own heartbeat, which the mirror deliberately does not kill |
| C20 | Two `Reconcile`/wake passes overlapping in one process, or two processes | Both read the same coarse state and both act | **repeated wake/reconcile** | Partly: R1's boundary and `owned.Live()` short-circuit. Not closed for W4/W12/W13/H1, which CAS on `(id, expected)` only |

---

## 4. The ambiguous state classes

Six classes, as named in the objective. For each: what it is, which windows
produce it, what resolves it today, and the CAS resolution proposed for the
implementation step. The resolutions reuse the reviewer's *principles*
(§5) — durable generation identity, ownership proof, generation-conditioned
CAS, stale-writer rejection, idempotent replay, fail-closed on unprovable
provenance — and reuse the *store primitives* that already exist and are
table-generic, but **no reviewer code is copied**: the reviewer's semantics
(review runs, authority pointer, cycles, fresh-review epochs) do not apply to a
worker and must not be transplanted.

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

---

## 5. Side by side: the approved reviewer CAS model vs. the worker path today

The reviewer model is already merged and is the reference. Its six principles,
against the worker path:

| # | Reviewer principle | Reviewer implementation | Worker path today | Verdict |
| --- | --- | --- | --- | --- |
| 1 | **Durable generation / attempt identity** | `reviewLaunchGeneration{OutboxID, IdempotencyKey, RecordID, Cycle, Epoch, Stamped}` (`review_launch_recovery.go:577`); `review_launch_attempt` checkpoint allocates the budget *before* any work the attempt performs (`:750`) | No generation value exists. Attempt budget is *counted* by scanning checkpoints after the fact (`workerLaunchAttemptCount`, `worker_launch_recovery.go:392`), so an attempt that crashes before writing its failure record is invisible and the budget is not spent | **Missing** |
| 2 | **Ownership proof before the state is believed** | `review_launch_intent` / `review_launch_confirmed` markers; a `review_run` row is explicitly *not* proof of a launch (`review_launch_phases.go:22-41`); `errReviewerInstanceUnproven` refuses a confirmation that names only a reusable session | Structurally present and good: `SessionOwnershipEvidence` with `Observed` / `Missing` / `Unavailable` kept apart, both halves required to confirm, `LicensesRunning()` gating RUNNING | **Present** — the one principle the worker already satisfies |
| 3 | **Generation-conditioned CAS transitions** | `ClaimWorkflowOutboxDispatch`, `FailWorkflowOutboxWithGeneration`, `ReleaseDispatchedWorkflowOutboxGeneration`, `ReopenFailedWorkflowOutboxGeneration` (`workflow.go:131-157`); `UpdateWorkflowStepStateIfReviewRun` for the authority pointer | Every mutation is `(id, expected_state)`; `UpdateWorkflowStepSession` has no predicate at all | **Missing** |
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
   may release it (`dispatch_reconcile.go:757-771`). The worker model therefore
   gets *stale-writer rejection* but never *supersession* — a worker generation
   that lost its claim stops; it never replaces the winner.
2. **The reviewer's cycle/epoch vocabulary is review-specific.** Review cycles,
   fresh-review generations and reset epochs exist because a review step is
   re-dispatched many times against changing fingerprints. A work step
   dispatches once and is re-dispatched only on failure. The worker generation
   needs no cycle dimension, and adding one would be inventing state nothing
   reads.

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
| `0141_worker_launch_reset_single_winner.sql` | Backfill `head_sha = 'worker-launch-reset-legacy-' \|\| id` for existing `worker_launch_human_retry` rows, then `CREATE UNIQUE INDEX … ON workflow_checkpoints (workflow_step_id, head_sha) WHERE durable_phase = 'worker_launch_human_retry'` | Single-winner human reset per failed generation (§4.5). The backfill is mandatory and must run first, exactly as in `0136`: this phase is not new, existing databases already hold colliding rows, and `CREATE UNIQUE INDEX` would wedge startup on precisely the installations that have used the path most |

All five changes are strictly additive: nullable or `DEFAULT ''`, nothing
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
| W12 retryable release | `ReleaseDispatchedWorkflowOutboxGeneration(id, class, gen.token)` | claim lost ⇒ **do not schedule a wake, do not park the run**; another writer owns this step |
| W13 permanent fail | `FailWorkflowOutboxWithGeneration(id, dispatched, now, class, failureGen, gen.token)` | claim lost ⇒ no-op; the step is not failed, the run is not parked |
| W15 confirmation | insert with `dispatch_generation`; unique index makes a replay lose | insert conflict ⇒ read as "already confirmed for this generation", continue to W16 |
| W16 ledger marker | written only after W15 succeeded **or** conflicted (both mean confirmed) | — |
| W17 session write | **new** `UpdateWorkflowStepSessionIfUnset(stepID, sessionID, now)` — `WHERE id = ? AND (session_id IS NULL OR session_id = ?)` | a different session already owns the step ⇒ **stop, do not overwrite.** This is the one write whose loss would put two workers on one worktree |
| W19 acknowledge | generation-conditioned acknowledge (`WHERE dispatch_generation = ?`), clearing the token | claim lost ⇒ no-op |
| W20 RUNNING | `UpdateWorkflowStepStateIfSession(stepID, ready, running, sessionID, now)` — the worker's analogue of `UpdateWorkflowStepStateIfReviewRun`, conditioned on the session this generation just wrote | not ours ⇒ no-op |
| W21 completion transition | moved **after** W23; conditioned on `(stepID, expected=running, session_id = ?)` | benign race ⇒ skip, as today |
| H1 human reopen | `worker_launch_human_retry` insert under the unique index (single winner), then `ReopenFailedWorkflowOutboxGeneration(id, class, failureGen.casValue())` | index conflict ⇒ "already resumed"; CAS miss ⇒ the failure a person looked at is no longer the current one ⇒ **no reopen** |

### 6.6 Invariants the implementation must hold

1. **RUNNING ⟹ a durable confirmation for the generation that owns the step's
   session.** (Today: RUNNING ⟹ *a* confirmation exists.)
2. **An open attempt row ⟹ a live claim.** No attempt may be open whose
   `dispatch_generation` is not the outbox row's current `dispatch_generation`,
   or whose step has moved past the work it described.
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
7. **The session is never rebound.** A worker generation that lost its claim
   stops; it never supersedes the winner (§5, difference 1).

### 6.7 Explicitly out of scope for the implementation step

- The parent/child attention mirror (§4.4) — already correct, and derived per
  pass by design.
- Reviewer, verify, fix and planner dispatch paths.
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

---

## 7. Summary

The worker path already has the two hardest things right: **ordering** (every
durable record precedes the action it describes, and RUNNING is gated on a
confirmation) and **honest evidence** (`observed` / `missing` / `unavailable`
kept apart, with `unprovable` a first-class outcome that stops). What it lacks
is the one thing the reviewer path added and proved: a **durable generation
token that every mutating write conditions on**, so that a writer which paused
across a turn of the reused outbox row cannot win a write it no longer owns.

Twenty crash windows are enumerated in §3. Fifteen of them are already resolved
by reconciliation, the reaper, or pure re-derivation. The five that are not —
C7 (split-brain between the two confirmation homes), C9 (a session-owning step
stuck at `ready`), C12 (a completed step with no completion evidence), C15 (a
reconciliation boundary that makes its own remedy skippable), and C16/C20 (a
budget spent by crashes and races rather than by decisions) — are exactly the
five that require a generation-conditioned CAS to close. §6 is that CAS.
