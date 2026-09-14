# P9 — Worker recovery, ownership and isolation

> **Principle.** Recover when provably safe. Fail closed when ownership cannot be
> proven. Never guess ownership. Never create two owners for the same generation.

Base: `feat/engineering-control-center` @ `39e1815af33a2fd9cac822085da05c73e2948f7f`.
Branch: `feat/p9-worker-recovery-ownership` (isolated worktree). No push, no merge.

This document is written in two passes. §1–§6 are the **audit and the formal model,
written before any code changed** (BLOCK A). §7 onwards records what was implemented,
how it was proven, and what is still owed.

---

## 1. The lifecycle as it is on the base commit

```
Workflow run ── plan step ── work step ──────────────── review step ── fix step ── verify
                               │                                        │
                               ▼                                        ▼
                     workflow_outbox (spawn_worker_session,   fix delivered into the SAME
                     idempotency key = work step)             worker session (no new session,
                               │                              fix step.session_id stays NULL)
        dispatchFromPending ── admit ── claim (CAS pending→dispatched,
                               │         stamps dispatch_generation = wfd-<id>)
                               ▼
        beginWorkerDispatch ── ClaimOpenWorkflowAttempt ── dispatch record
                               │  phase=worker_launch_intent, id == generation
                               ▼
        launchWorker ── credential Issue ── Spawner.Spawn ── session_manager:
                               │   launchID = uuid, owner token = ao-session:<sid>:<launch>,
                               │   tmux new-session -e AO_SESSION_OWNER=<token>,
                               │   row: runtime_handle_id, runtime_instance_id ($N),
                               │        runtime_owner_token, runtime_launch_id, worktree, branch
                               ▼
        confirmWorkerDispatch ── stillOwnsWorkerDispatch (pre-check)
                               │  ── ObserveSessionOwnership   (reads the DB ROW only)
                               │  ── dispatch record phase=worker_dispatched (+ launch id, worktree)
                               │  ── checkpoint worker_dispatched
                               ▼
        running ── AcknowledgeWorkflowOutboxDispatch (CAS on generation)
                ── UpdateWorkflowStepSession (session_id IS NULL)
                ── StartWorkflowStepForSession (ready→running AND session_id = ?)
                               ▼
        signals ── lifecycle.ApplyActivitySignal drops a signal whose LaunchID ≠ row launch;
                   last_signal_at (coalesced 30 s) is the liveness clock
                               ▼
        completion ── observeWorkStep (session row + turn receipt ≥ attempt start)
                      ClaimWorkflowAttemptOutcome (finished_at IS NULL)
                               ▼
        recovery ── Reconcile at boot: reconcileOrphanedReviewersForAllRuns (ALL runs),
                    then per NON-terminal run: ReconcileWorkStepDispatch
                      (observeDispatchOwnership → adoptLiveLaunch | retry | stop),
                    dispatchWorkStep → adoptOrMarkAmbiguous (natural key),
                    observeWorkStep / observeFixStep, advanceReviewFixCycle
```

Identity that already exists on the base (no invention needed):

| Identity | Where | Lifetime |
| --- | --- | --- |
| run / step | `workflow_runs.id`, `workflow_steps.id` | durable |
| dispatch generation | `workflow_outbox.dispatch_generation` (0138) = id of the `worker_launch_intent` dispatch record | one claim |
| failure generation | `workflow_outbox.failure_generation` (0137) | one failure |
| attempt | `workflow_attempts.id` (open attempt claimed per step) | one provider attempt |
| session | `sessions.id`; natural key `(project, issue_id = work step)` | one agent session |
| launch | `sessions.runtime_launch_id` (0033), uuid per spawn/restore; also on the dispatch record (0134) | one launch |
| runtime owner token | `sessions.runtime_owner_token` (0141) and tmux session env `AO_SESSION_OWNER` = `ao-session:<session>:<launch>` | one launch |
| runtime incarnation | `sessions.runtime_instance_id` (0141) = tmux `$N` | one tmux session incarnation |
| worktree / branch | session metadata; dispatch record (0134); frozen placement (0142) | durable |
| daemon incarnation | `daemonInstanceToken` (uuid, in memory, per process) — branch locks + placements only | one daemon process |
| installation | tmux socket `ao-<sha256(dataDir)[:12]>`; nothing persisted as an id | data dir |

## 2. Reviewer vs worker (what is really enforced, verified in code)

| Invariant | Reviewer | Worker (base) | Gap |
| --- | --- | --- | --- |
| Generation-conditioned claim | `ClaimWorkflowOutboxDispatch` stamps `dispatch_generation`; loser launches nothing | same method, same fence | — |
| Fail / release / reopen CAS on generation | `FailWorkflowOutboxWithGeneration`, `ReleaseDispatched…Generation`, `ReopenFailed…Generation` | same methods | — |
| Success acknowledge on generation | **plain status CAS** (`UpdateWorkflowOutboxStatus`), protected instead by the write-once authority pointer bind | `AcknowledgeWorkflowOutboxDispatch(status, generation)` | reviewer residue (documented, not changed — closed lifecycle, no duplicate demonstrated) |
| "Still authorized" pre-check | status only (`reviewLaunchStillAuthorized`) | `stillOwnsWorkerDispatch` — **answers TRUE on an unreadable outbox** | worker fails open (**RC5**) |
| Ownership proof against the RUNTIME | `ProbeReviewer`: tmux `SessionFacts` owner token `ao-reviewer:<handle>` + `$N` instance recorded on `review_launch_confirmed` | `sessionFactsOwnership` reads **the DB row only**; liveness is `tmux has-session -t <name>` | **RC1** — a name and a row are not ownership |
| Durable intent before runtime | `review_dispatch_authorized` → claim → `review_launch_intent` | outbox claim + open attempt + `worker_launch_intent` record | — |
| Adopt-before-relaunch | probe by name, owned → adopt, absent → abandon/relaunch, unknown/foreign → ambiguous | `adoptLiveLaunch` (launch-id fence, silence adopts) and **natural-key adoption with no launch/runtime check** (`adoptOrMarkAmbiguous`, `resumeWorkerLaunchAfterFailure`) | **RC2**, **RC3** |
| Duplicate prevention | idempotency key, claim, unique review_run index, pointer CAS | idempotency key, claim, `session_id IS NULL` bind — **its result is ignored** | **RC6** |
| Completion claim | `UpdateReviewRunResult WHERE status='running'`, late verdict preserved, adopted only under pointer CAS | `ClaimWorkflowAttemptOutcome`; session never rebound; signals fenced by launch id | residue: attempt refinement/failover writes are unconditional (documented) |
| Terminal behaviour | dispatch/rebind refuse terminal; **boot orphan sweep appends `review_reviewer_unproven` to terminal runs and can escalate to an attention STOP** | boot reconcile lists non-terminal runs only | **RC8** (shared startup pass) |
| Restart | windows before/after intent/runtime/confirm all decided on probe | windows decided on DB row + name probe | RC1–RC3 |
| Daemon identity | none | none (per-process token, memory only) | **RC11/RC12** |

The reviewer has the guarantees the brief assumed at the level that matters (a
claim fence, an authority pointer bound write-once, adoption only on a runtime
proof). Its residues are defense-in-depth gaps, not a hollow reference, so the
STOP condition "the reviewer does not really have the assumed guarantees" is
**not** met. The reviewer lifecycle stays closed; only the terminal-run write in
the shared boot sweep (RC8) is changed, because §19 requires it.

## 3. Root causes found

| # | Root cause | Consequence | Invariants |
| --- | --- | --- | --- |
| RC1 | Worker ownership evidence is the session **row**; worker liveness is `has-session` **by name** | a recreated/reused pane with the same name, or a row whose runtime was replaced, reads as "our live worker" | I3 I4 I5 I6 I16 |
| RC2 | Natural-key adoption (`adoptOrMarkAmbiguous`, `resumeWorkerLaunchAfterFailure`) adopts any session with a workspace path or branch — no launch id, runtime, or worktree check | a gen-1 orphan can be bound as the step's worker after gen-2 was authorized | I2 I3 I9 I13 I15 |
| RC3 | `adoptLiveLaunch`: a missing launch id on either side is "not a mismatch" → adopt on liveness | legacy rows adopted with no provenance | I17 |
| RC4 | tmux `Restart` (restore of a live pane) never rewrites `AO_SESSION_OWNER`, while the row gets the new token | after P9 makes the token load-bearing, every restored worker would fail closed falsely | false positive |
| RC5 | `stillOwnsWorkerDispatch` answers TRUE on a read error | an unreadable outbox is treated as "still ours" | I1 |
| RC6 | `UpdateWorkflowStepSession` result ignored in confirmation | a lost bind still renews branch locks / opens a usage window for a session the step does not hold | I1 |
| RC7 | `workerLaunchAttemptCount` returns 0 on an unreadable ledger | the retry budget fails open | I18 |
| RC8 | Boot orphan-reviewer sweep writes `review_reviewer_unproven` (classified LIFECYCLE) to terminal runs; past its budget it escalates into `stopReviewAmbiguous` → `recordAttentionStop` with no terminal guard | a completed run's NextAction / latest phase / last activity change on every boot; a STOP + notification + wake can be written onto a closed run | I10 I11 |
| RC9 | `observeFixStep`: once a cycle reads `active` there is no silence bound at all, and it never consults `last_signal_at`; the liveness view skips fix steps (no `session_id`) | a dead fix agent is never detected; the UI shows no clocks for Fix | Fix liveness |
| RC10 | `running.json` carries pid/port only; status reads ONE path; stop deletes a "stale" file unconditionally; `/healthz` identity is pid + service | a daemon under the other path convention is invisible; a reused PID can be mistaken; a concurrent start's file can be deleted | J |
| RC11 | No durable installation identity; daemon incarnation id is memory-only and never reaches the runtime or the run-file | "which installation/daemon created this runtime" is unanswerable | §6 |

## 4. Formal ownership model

Identifiers, and which question each one answers:

| Identifier | Question | Sufficient alone for ownership? |
| --- | --- | --- |
| RUN ID / STEP ID | which work | no |
| DISPATCH GENERATION (`dispatch_generation`) | which **claim** may move the outbox row | no — names a claim, not an execution |
| ATTEMPT ID | which provider attempt the outcome belongs to | no |
| SESSION ID | which agent session row | no — survives the process |
| LAUNCH ID | which **launch** of that session | no, until read back from the runtime |
| RUNTIME OWNER TOKEN | "this runtime was created by AO for session S launch L" | only when read back **from the runtime** and equal to the row |
| RUNTIME INSTANCE (`$N`) | which incarnation of the tmux session | no — resets with the tmux server |
| PID | none | **never** (I4) |
| tmux session NAME | none | **never** (I5) |
| INSTALLATION ID | which data dir / AO installation | no — corroborating |
| DAEMON INSTANCE ID | which daemon process launched it | no — provenance only; a new daemon must be able to adopt |
| WORKTREE / BRANCH | where the work lives | mismatch refuses; match alone proves nothing |

**Ownership of a worker runtime is PROVEN iff every link of this chain holds:**

```
runtime (read NOW)                       session row (durable)          dispatch evidence (durable)
────────────────────────────────         ─────────────────────          ──────────────────────────
tmux session exists at instance $N   ==  runtime_instance_id = $N
AO_SESSION_OWNER = ao-session:S:L    ==  runtime_owner_token,
                                         id = S, runtime_launch_id = L  == recorded launch id for step
AO_INSTALLATION_ID (when stamped)    ==  this installation
                                         workspace_path / branch        == recorded worktree / branch (when recorded)
                                         issue_id = work step           == step of the claim (generation G on outbox)
```

The token binds session + launch with a uuid, so an equal token read back from the
runtime is not forgeable by a name collision, a reused PID, a recreated pane or a
different installation. The run/step/attempt/generation links are carried by the
durable chain (session natural key → dispatch record → outbox generation), which is
the "equivalent structure" §9 allows instead of copying every id into tmux. No
prompt, command, env or secret is stamped or read.

## 5. Decision model

`domain.WorkerRuntimeProof` classifies the runtime read (closed set):

| Proof | Meaning |
| --- | --- |
| `owned` | the whole chain holds and the runtime session exists |
| `owned_exited` | chain holds, the workload process is gone (pane kept) |
| `absent` | the runtime answered: no such session / instance (positive fact) |
| `instance_mismatch` | a session answers under that name at a different `$N` |
| `owner_mismatch` | the runtime's owner token differs from the row's |
| `installation_mismatch` | the runtime is stamped by a different installation |
| `legacy_provenance_missing` | the row has no token / launch / instance recorded |
| `unsupported` | the runtime cannot read facts (conpty) |
| `unavailable` | the probe failed; nothing is concluded |

`WorkerRecoveryDecision{Action, Reason}` (pure, table-tested):

| Action | Used when |
| --- | --- |
| `ADOPT` | proof `owned`, launch id equals the recorded one, worktree/branch do not contradict, run not terminal |
| `RELAUNCH` | proof `absent` for the recorded incarnation AND no live session under the natural key — handed to the existing bounded retry, never launched here |
| `WAIT` | inside the settle window, or quiet-but-owned (clock jump, silent fix) |
| `NOOP` | terminal run, or the claim was lost (`cas_lost`) |
| `FAIL_CLOSED` | any mismatch, legacy provenance, unsupported or unavailable proof over a live-looking runtime |

Reason codes: `matching_runtime`, `runtime_missing`, `runtime_exited`,
`instance_mismatch`, `owner_mismatch`, `installation_mismatch`, `launch_mismatch`,
`worktree_mismatch`, `branch_mismatch`, `legacy_provenance_missing`,
`runtime_unsupported`, `runtime_unavailable`, `terminal_run`, `cas_lost`,
`settle_window`.

Nothing about "the worker failed" is ever inferred from a FAIL_CLOSED: the stop
reason is `worker_ownership_unproven`, distinct from every failure reason.

## 6. Plan and scope decisions taken before coding

* **No migration.** Every decision input already has a column (0033, 0134, 0137,
  0138, 0141, 0142). Installation identity is a file in the data dir, not a DB row.
* **Reviewer:** unchanged except the terminal-run guard in the shared boot sweep.
* **conpty/Windows:** option **B** — no `SessionFactsReader`, so the proof is
  `unsupported`, and recovery/adoption FAILS CLOSED there. Fresh launches are
  unaffected (the launching process holds the proof in memory).
* **Fresh launch confirmation** is not re-gated on the runtime read; P9 gates every
  path that acts **without** the launching process's memory (adoption, reopen,
  reconciliation).
