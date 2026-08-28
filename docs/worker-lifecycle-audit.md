# Worker lifecycle audit — durable write points, crash windows, and the CAS model to adopt

**Status:** design contract. This document is the audit the rest of the worker
lifecycle work implements against. It changes no behaviour by itself; every
"proposed" item below is a commitment about what the next implementation step
writes, not a description of what the code does today.

**Reviewer scope.** This audit is about the *worker* lifecycle. It takes the
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
| `backend/internal/workflow/fix_dispatch.go` | The fix-cycle dispatch: per-cycle outbox key, prompt delivery into the work step's **existing** session, the fix attempt row |
| `backend/internal/workflow/fix_progress.go` | Fix-cycle observation, the fix step/run transition, and the fix attempt's finalization |
| `backend/internal/workflow/fix_delivery_recovery.go` | Restart classification for a fix delivery that crossed a crash |

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

### 2.1 The fix-cycle writes

The fix cycle is not a second launch. It reuses the work step's **already-live
session** — `Send` targets `reviewRun.SessionID`, which is the worker's session,
never a new one (`fix_dispatch.go:404-412`) — so it has no intent/confirmation
boundary, no ownership probe and no `workflow_dispatch_checkpoints` row at all.
What it does have is its own outbox row, its own attempt row and its own
observation, and those three are what §4.7 is about. `F#` labels are used by §3
and §4.7.

| # | Write | Site | Guard today |
| --- | --- | --- | --- |
| F0 | Fix outbox row enqueued under a **per-cycle, per-transport** key `workflow-step-fix:<stepID>:cycle<N>[:transport<M>]` | `dispatchFixStep`, `fix_dispatch.go:246`; key built at `fixStepOutboxIdempotencyKey`, `:103` | Natural key. Unlike the worker's single reused row, each cycle gets a **fresh** row — the one structural advantage the fix path has over the work path |
| F1 | Fix outbox claim `pending → dispatched` | `dispatchFixFromPending`, `fix_dispatch.go:292` | `(entryID, expected=pending)` |
| F2 | Prompt delivered into the work step's existing session | `deliverFixPrompt`, `fix_dispatch.go:331`; `deliverAndConfirm`, `:66` | Transport-level. `ports.ErrPromptUndelivered` (refused before any transport side effect) is the only failure that may be retried durably (`:372`) |
| F3 | **Fix attempt row opened** (`outcome IS NULL`), harness copied from the work step's last attempt | `recordFixDispatchSuccess`, `fix_dispatch.go:419` | `len(attempts) < cycleNumber` — a **count comparison** against the cycle number. Not an identity check, not a CAS |
| F4 | `FixAttemptID` written into the delivery record — the only durable binding between a cycle and the attempt row it opened | `fix_dispatch.go:423` (new row) / `:428` (re-entered cycle) | none; it rides in F6's `RetryState` JSON |
| F5 | Fix outbox `dispatched → acknowledged` | `fix_dispatch.go:432` | `(entryID, expected=entry.Status)` |
| F6 | `fix_dispatched` checkpoint: session id, review run id, `fingerprint_before`, delivery record | `fix_dispatch.go:440` | none (append) |
| F7 | Fix step/run transition from observation (`running → waiting` on delivery, `→ failed` on no verifiable change) | `recordFixOutcome`, `fix_progress.go:282` (step), `:292` (run) | `(stepID, expected=step.State)` / `(runID, expected=run.State)`, behind the `ValidWorkflow*Transition` guards |
| F8 | `fix_observed_<state>` checkpoint carrying `fingerprint_after` | `fix_progress.go:301` | none (append) — written **after** F7, the same inversion §4.6 catalogues for W21/W23 |
| F9 | **Fix attempt finalized** (`succeeded` / `failed` + error class) | `recordFixOutcome`, `fix_progress.go:317` (lookup) and `:336` (update) | none — `GetLatestWorkflowAttempt(step.ID)` picks the row **by recency, not by identity**, and the update's error is discarded (`_ =`) |
| F10 | Fix-delivery restart classification, for a cycle whose outbox row was already `dispatched`/`acknowledged` when the process died | `resolveFixDeliveryAfterRestart`, `fix_dispatch.go:272`; `fix_delivery_recovery.go` | Reads F6's delivery record; refuses to conclude when it cannot read one |

Three properties of F3/F4/F9 together are what produce §4.7:

1. **F3's guard is a count, not an identity.** `len(attempts) < cycleNumber`
   answers "does this step have fewer attempt rows than cycles?", which is true
   or false about the *set* of rows and says nothing about whether *this* cycle
   owns one.
2. **F4 records the right binding and F9 does not read it.** The delivery record
   already names the attempt row the cycle opened. The close ignores it and
   takes the latest row on the step instead.
3. **F9 is unconditional and lossy.** No predicate, and a discarded error: a
   close that fails leaves an open attempt and no trace that it was attempted.

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
| CF1 | F1 → F3 | Fix outbox `dispatched`, the prompt possibly already in the session, **no attempt row** | A fix cycle in flight that nothing durable counts | `resolveFixDeliveryAfterRestart` (`fix_dispatch.go:272`) re-enters from `dispatched`/`acknowledged` and `fix_delivery_recovery.go` classifies; F0's per-cycle key means the re-entry cannot be confused with a different cycle |
| CF2 | F3 → F6 | Attempt open; **no `fix_dispatched` checkpoint** | Fix step stuck RUNNING with an open attempt | **none.** `observeFixStep` finds no checkpoint carrying a session id and returns "nothing to observe" (`fix_progress.go:102-107`), and `attemptReaper`'s proof 2 excludes a `running` step, so neither resolver can fire |
| CF3 | F7/F8 → F9 | Fix step terminal-for-the-cycle (`waiting` on delivery, `failed` otherwise), `fix_observed_*` written, **attempt still open** | **`fix_completed_but_attempt_open`** | `attemptReaper` only, after 30 min + four proofs — and proof 3 needs evidence on a *different* step written after the attempt opened, which a run parked by this very cycle may never produce |
| CF4 | Cycle N's observation runs after cycle N+1's F3 opened a new row | F9 closes cycle **N+1**'s live row and leaves cycle N's open | **`fix_completed_but_attempt_open`**, cross-cycle variant: a live attempt closed and a dead one left open | **none.** This is not a crash window at all — it is reachable with no crash, purely from `GetLatestWorkflowAttempt` picking by recency |

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
| 4.7 `fix_completed_but_attempt_open` | **Resolvable** — by binding the close to the attempt the cycle actually opened (F4's `FixAttemptID`), not to the latest row | A cycle whose delivery record is unreadable or names no attempt id, and CF2's pre-checkpoint window, where the step is `running` with no `fix_dispatched` record: neither the observer nor the reaper may act, and the step stops as `ambiguous_worker_state` rather than being closed on a guess |

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

**Windows.** CF2, CF3, CF4.

**Today.** Only `attemptReaper`, and it is a worse fit here than on the work
step:

- CF3 (crash between F7/F8 and F9) leaves a terminal-for-the-cycle fix step
  with an open attempt. The reaper's proof 2 is satisfied (`waiting`/`failed`
  are neither `running` nor `ready`), but **proof 3 requires durable evidence on
  a *different* step, written strictly after the attempt opened, showing the run
  moved on** (`attempt_reaper.go:41-44`). A fix cycle that ended by parking the
  run — `stopFix` / `stopFixAmbiguous`, `fix_progress.go:200`, `:229` — is
  exactly the case where the run does *not* move on, so the evidence proof 3
  wants may never be written. The row stays open indefinitely, and every guard
  that asks "could something still be writing to this tree?" keeps answering yes
  (`verify_branch_advanced.go` proof 5, `work_adoption.go` proof 4). This is the
  fix path's own version of the fossil the reaper exists to clear, in the one
  shape the reaper cannot clear.
- CF2 (crash between F3 and F6) is worse still and has **no resolver at all**.
  The attempt is open, the step is `running`, and there is no `fix_dispatched`
  checkpoint. `observeFixStep` requires that checkpoint to carry a session id
  and otherwise returns "nothing to observe" (`fix_progress.go:102-107`), so
  observation never advances the step; and because the step is `running`, the
  reaper's proof 2 refuses it. Neither mechanism can move, and nothing else
  looks at the pair.
- CF4 needs no crash. `recordFixOutcome` closes
  `GetLatestWorkflowAttempt(step.ID)` (`fix_progress.go:317`, `:336`) — the most
  recent attempt row on the fix step, chosen by recency. A fix step accumulates
  one attempt row per cycle (F3's `len(attempts) < cycleNumber` guard), so as
  soon as cycle N's observation is delayed past cycle N+1's dispatch, the close
  lands on N+1's live row: the newer attempt is finalized while its cycle is
  still running, and cycle N's row is left open forever. One misattribution
  therefore produces both failure modes at once — a live attempt wrongly
  closed, and an abandoned one never closed.

**Gap vs. the reviewer CAS model.** Three of the six principles are missing
here, and the missing pieces are not the same ones as §4.2's:

| Principle | Fix path today |
| --- | --- |
| Generation identity (1) | **Partially present, and unused.** F4 already writes `FixAttemptID` into the durable delivery record — the fix path *does* record which attempt row belongs to which cycle. F9 simply does not read it. The identity exists and the write ignores it |
| Generation-conditioned CAS (3) | **Missing.** `UpdateWorkflowAttemptOutcome` takes an id and no predicate; nothing conditions on the row being open or on the cycle owning it |
| Stale-writer rejection (4) | **Missing, and reachable without a crash** (CF4). A late observation from a superseded cycle wins a write against the current cycle's row |
| Idempotent replay (5) | **Absent by construction.** The close is a blind overwrite of whichever row is newest, so a replayed observation does not re-derive the same answer — it re-targets |
| Fail-closed provenance (6) | **Fail-open.** `_ =` on the update (`fix_progress.go:336`) discards the error: a close that did not happen is indistinguishable from one that did |
| Ownership proof (2) | Not applicable in the launch sense — the fix cycle proves nothing about a session because it launches none; it borrows the work step's. `fixCycleStarted` (`fix_progress.go:44`) is the fix path's analogue and is sound |

**Proposed CAS resolution.** Small, and mostly a matter of using a binding that
already exists:

1. **Close the attempt the cycle named, not the newest one.** `recordFixOutcome`
   resolves its target from the cycle's own `fix_dispatched` delivery record
   (`promptDeliveryRecord.FixAttemptID`, read back via
   `promptDeliveryRecordFromJSON`) instead of `GetLatestWorkflowAttempt`. This
   alone closes CF4 outright — with no schema change, because F4 already writes
   the field.
2. **Condition the close.** The same
   `UpdateWorkflowAttemptOutcomeIfOpen(attemptID, generation)` §4.2 introduces,
   with the fix cycle's identity in the generation column: `WHERE id = ? AND
   outcome IS NULL AND dispatch_generation = ?`. Zero rows updated means the row
   is already closed or is not this cycle's — a no-op, never an error.
3. **Stamp F3.** The attempt row created at `fix_dispatch.go:419` records the
   cycle's identity in the same `dispatch_generation` column §6.2 adds, so (2)
   has something to condition on and so the reaper can tell a fix attempt's
   owner from its recency.
4. **Stop discarding the error.** As with W25 (§4.2 item 3), a close that failed
   is a fossil someone has to clear later; it is worth a log line and a retry on
   the next observation rather than silence.
5. **Extend §4.2's same-generation completion close to fix steps.** The `F8`
   `fix_observed_<state>` checkpoint is already the canonical terminal record
   for a cycle and already carries `fingerprint_after`. When it exists for a
   cycle whose attempt is still open and whose generation matches, the next
   observation or reconcile pass closes that attempt with the outcome read off
   the checkpoint. Pure function of durable rows, so replay re-derives the same
   answer; this is what closes CF3 without waiting on the reaper's proof 3.
6. **Invert F7/F8, as §4.6 inverts W21/W23.** Writing the `fix_observed_*`
   checkpoint before the step/run transition makes (5) able to fire on every
   crash in the window, rather than only on those that got past the checkpoint.

**Classification: resolvable.** CF4 is resolvable by item 1 alone and is not
even ambiguous — the correct row is durably named, and the current code declines
to read it. CF3 is resolvable by items 5–6, on exactly the argument §4.2 makes:
the deciding fact is a record the cycle's own completion wrote, so no inference
from silence is involved and no settle window is needed.

**Fail-closed residue.** Two cases stay `needs_attention` by design:

- **A cycle whose delivery record is unreadable or names no attempt id.** F4
  leaves `FixAttemptID` empty when a cycle re-entered recovery with no attempt
  rows at all, and a corrupt or missing `RetryState` reads back as nothing. With
  no named row, item 1 has no target — and falling back to "the newest row" is
  precisely the defect being removed. The attempt stays open and visible to the
  reaper; the observation raises nothing.
- **CF2's pre-checkpoint window.** A fix step `running` with an open attempt and
  no `fix_dispatched` record cannot be decided: AO does not know whether the
  prompt reached the session, so it cannot know whether a worker is at that
  moment writing to the tree. Closing the attempt would tell every downstream
  guard the tree is quiet on no evidence. The honest answer is
  `ambiguous_worker_state` with the evidence snapshot, via the existing
  `stopFixAmbiguous` gate (`fix_progress.go:200`) — which is a change from
  today's behaviour of doing nothing at all, but it is a change toward stopping
  visibly, not toward guessing.

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
| W12 retryable release | `ReleaseDispatchedWorkflowOutboxGeneration(id, class, gen.token)` | claim lost ⇒ **do not schedule a wake, do not park the run**; another writer owns this step |
| W13 permanent fail | `FailWorkflowOutboxWithGeneration(id, dispatched, now, class, failureGen, gen.token)` | claim lost ⇒ no-op; the step is not failed, the run is not parked |
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
- Verify and planner dispatch paths.
- The fix path, **except** for the four narrow changes §4.7 names: binding the
  attempt close to the cycle's own `FixAttemptID`, conditioning that close,
  stamping the fix attempt row, and inverting F7/F8. Fix *dispatch* — the
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
   step, cycle N's observation arriving after cycle N+1's dispatch: cycle N's
   close lands on cycle N's own row and cycle N+1's row is **untouched and still
   open** — the assertion that fails on today's code. Plus a CF3 fixture (crash
   between the `fix_observed_*` checkpoint and the close) finished by the next
   pass with the outcome read off that checkpoint, a second pass updating zero
   rows, and a CF2 fixture (attempt open, step `running`, no `fix_dispatched`
   record) asserting the attempt stays **open** and the step stops as
   `ambiguous_worker_state` rather than being closed.
9. **R3R, both directions (§4.3).** With proof obligation P satisfied in full —
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

On the seven ambiguous classes (§4), the verdict is that **all seven are
resolvable** — 4.1, 4.2, 4.4 (already resolved upstream), 4.5, 4.6, 4.7, and 4.3
in both halves: *stale* by the durable runtime-instance fence, and *phantom*
whenever that fence lets AO prove, under a matching generation identity, that
the execution it launched is gone. 4.7 —
`fix_completed_but_attempt_open` — is the cheapest of the seven and the only one
reachable with no crash at all: the fix path already writes the binding it needs
(`FixAttemptID`) and simply closes the newest attempt row instead of the named
one.

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
