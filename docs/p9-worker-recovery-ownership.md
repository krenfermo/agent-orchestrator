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

---

## 7. What was implemented (second pass)

| Block | Commit | Content |
| --- | --- | --- |
| A | `dddcda93e` | This document §1–§6 |
| B+C | `561850f2f` | Runtime ownership proof, identities, decision model, fail-closed CAS fixes |
| F | `22bc808b2` | Terminal-run immutability, Fix liveness |
| D+E+G(tmux) | `2d0164d19` | Crash windows C1–C10, real-tmux E2E, daemon discovery/stop |
| H | `458d4b693` | Ownership readback (HTTP + CLI) |
| G(daemon) | `a4e3c4fb0` | Real daemon restart/crash E2E in scratch |

No migration and no sqlc change: every decision input already had a column (§1).

## 8. Identity model as implemented

| Identity | Where it lives | Created | Used for |
| --- | --- | --- | --- |
| Installation | `<data dir>/installation_id` (`aoi-<uuid>`), `daemonmeta.LoadOrCreateInstallationID` — O_EXCL, never regenerated; a malformed file is an error, not a reason to mint another | first daemon boot on that data dir | tmux stamp `AO_INSTALLATION_ID`; `running.json`; `/healthz`; `installation_mismatch` |
| Daemon instance | `daemonmeta.NewDaemonInstanceID` (`aod-<uuid>`), one per process; it IS the branch-lock / placement owner token (one incarnation, one id) | every boot | tmux stamp `AO_DAEMON_INSTANCE_ID` (provenance only), `running.json`, `/healthz`, stop verification |
| Worker launch | `sessions.runtime_launch_id` + owner token `ao-session:<session>:<launch>` in tmux session env | every spawn/restore | the ownership proof |
| Runtime incarnation | tmux `$N` in `sessions.runtime_instance_id` | tmux `new-session -P` | addressing facts and destroys; `instance_mismatch` |
| Dispatch generation | `workflow_outbox.dispatch_generation` (unchanged) | outbox claim | claim fence |

PID is never an identity. A tmux session name is never evidence.

## 9. The ownership proof (`workerownership.Classify`)

Order is the proof: (1) the row must carry complete provenance, and its token must be
`SessionRuntimeOwnerToken(id, launch)` — otherwise `provenance_missing`/`owner_mismatch`
without asking the runtime; (2) the recorded `$N` is read first; (3) only if it is gone is
the name consulted, to separate `absent` from `instance_mismatch`; (4) owner token, then
installation stamp (an unstamped runtime is not a mismatch); (5) workload liveness last.

`session_manager.ObserveWorkerRuntime` wraps it; the daemon wires it as
`workflow.Deps.WorkerRuntimeOwnership`. `tmux.Restart` now re-stamps the owner token
before `respawn-pane` (RC4 — every restored worker would otherwise fail closed falsely).

**conpty / Windows: option B.** No `SessionFactsReader` ⇒ `unsupported` ⇒ recovery and
adoption FAIL CLOSED with `worker_ownership_unproven`. Fresh launches are unaffected.
NOT production-ready for restart adoption on Windows; parity is a separate block.

## 10. Where the decision is enforced

`decideWorkerAdoption` (ADOPT / RELAUNCH / WAIT / NOOP / FAIL_CLOSED, closed reason codes)
is the single answer for:

* `adoptLiveLaunch` (reconciliation of an intended/unconfirmed launch);
* `adoptOrMarkAmbiguous` (dispatched command, natural-key session);
* `resumeWorkerLaunchAfterFailure` (human reopen).

And `ownedExecution.Live()` requires proof `owned` when the port is wired, so the reconcile
sweep can no longer protect or adopt on row + name.

Fences, in order: terminal run → runtime proof → recorded launch id → generation fence (a
session created before the claim in force, when no launch was recorded) → worktree →
branch. An unreadable runtime is waited out for `workerRuntimeUnreadableGrace` (15 min),
then fails closed; it is never adopted.

CAS / fail-closed fixes: RC5 `stillOwnsWorkerDispatch` answers false on a read error; RC6
the session bind's result is checked (a lost bind writes no usage window, lock pointer or
RUNNING); RC7 an unreadable ledger is an exhausted budget; C4 a confirmed, bound, live
launch stuck at `ready` finishes RUNNING through `StartWorkflowStepForSession`; I18 a step
already holding a session is never re-adopted (the order of dispatch records sharing one
clock reading had produced a second confirmation).

## 11. Crash / restart matrix (tests: `p9_worker_recovery_test.go`)

| # | Durable before | Runtime before | Recovery action | Durable after | Runtime after |
| --- | --- | --- | --- | --- | --- |
| C1 | run created, nothing launched | none | next start launches once | one session, RUNNING | one worker |
| C2 | claim + intent + open attempt, no session | none | settle window, then proven absent (natural key) → bounded retry | one replacement, RUNNING | one worker |
| C3 | intent + session row, unconfirmed | owned | ADOPT through the ordinary confirmation | confirmed, RUNNING over it | untouched |
| C3′ | same | instance/owner/installation mismatch, legacy, unsupported | FAIL_CLOSED `worker_ownership_unproven`, outbox stays dispatched | needs_attention, no session bound | untouched, never killed |
| C3″ | same | unreadable | WAIT ≤15 min, then FAIL_CLOSED | unchanged, then needs_attention | untouched |
| C3‴ | unconfirmed record with launch L1 / worktree W1 | owned, but row now L2 / W2 | FAIL_CLOSED (launch / worktree mismatch) | needs_attention | untouched |
| C4 | confirmed + ack + session bound, step `ready` | owned | finish RUNNING via CAS | RUNNING | untouched |
| C5/C7 | confirmed RUNNING | owned | protected; nothing written | unchanged | untouched |
| C6 | RUNNING, turn receipt after dispatch | owned_exited | ending belongs to work observation | observed, no ambiguity | pane kept |
| C8 | confirmed RUNNING, row live | proven absent (phantom) | classified `worker_dispatch_ambiguous` with evidence, never relaunched (the step owns a session that may have written the tree) | needs_attention | none |
| I9 | gen-1 session unconfirmed & dead → gen-2 RUNNING | gen-1 exits late | gen-1's late completion ignored | gen-2 still RUNNING | gen-2 untouched |
| C10 | C3 state | owned | two recoveries race (20 rounds): one owner, one ack, one confirmation | RUNNING | untouched |
| earlier gen | live AO session created before the claim in force | owned | FAIL_CLOSED `generation_mismatch` | needs_attention | untouched |
| terminal | C3 state, run cancelled | owned | NOOP; no row written | unchanged | untouched |

## 12. Sleep / wake

Silence is never a death certificate. A work step's active worker has no silence stop; a
three-hour clock jump over an owned runtime writes nothing (`TestP9Crash_ClockJump…`). A fix
cycle past `fixActiveSilenceWindow` re-reads the runtime: owned → nothing; provably gone →
concluded on workspace evidence; unprovable → nothing (`TestP9Fix_ClockJump…`).
`last_signal_at` is evidence of life, never ownership. Residue: an uncorroborated
`waiting_input` silent for 15 min still stops as before — that stop asks a person, it does
not declare the worker dead.

## 13. Fix liveness

A fix cycle rides in the worker's session and its step has no `session_id`. P9:
`fixCycleStarted` counts an `active` signal after the dispatch (never an idle one —
wf-57f90ff2 stays closed); `observeActiveFixSilence` bounds silence with a runtime re-check;
the liveness view resolves the fix step's session from its dispatch record (P8 debt 11).

## 14. Terminal-run immutability

The boot orphan-reviewer sweep no longer writes `review_reviewer_unproven` nor escalates a
STOP on a completed/failed/cancelled run; a reviewer PROVEN to be AO's is still terminated.
`appendUnprovenReviewerProbe` and `escalateUnprovenReviewer` re-read the run and refuse a
closed one. Held byte-for-byte across repeated boots (row, ledger, full `RunDetail`) for all
three terminal states. Budget and escalation are unchanged on live runs. Worker
reconciliation already skipped terminal runs; `TestP9Crash_TerminalRun…` pins it.

## 15. Daemon discovery and stop

`running.json` v2 adds `formatVersion`, `instanceId`, `installationId`, `dataDir`; `/healthz`
publishes the same identity. `ao status` / `ao stop` inspect BOTH conventions for the
installation (`AO_RUN_FILE` and `<data dir>/running.json`) and classify each:

| State | Meaning | stop |
| --- | --- | --- |
| verified (`ready`/`not_ready`) | PID alive, probe answers with the same PID, instance and data dir | `/shutdown`, then wait for exit (PID gone, or file gone AND the port no longer answering as that incarnation) |
| `unhealthy` | PID alive, probe unreachable — RUNNING BUT UNVERIFIED | refused |
| `running_unverified` | probe answers as another incarnation/installation | refused |
| `foreign` | run-file or probe names another data dir | refused, file kept, never probed |
| `stale` | PID dead, or the port is answered by a different PID | file removed by exact match (`RemoveIfMatches`) |
| two verified daemons | — | refuses to guess |

Nothing ever sends a signal. The daemon's startup guard checks both locations too.
Electron's wedged-orphan takeover (`frontend/src/main.ts`, SIGTERM on a PID that is merely
alive) is unchanged and remains a debt (§20).

## 16. Readback

`GET /api/v1/workflows/{id}/recovery` → `workerOwnership[]` and `ao workflow recover ownership
<id>`: ownership, proof, recorded/observed incarnation, installation, launch state,
generation, phase, attempt, last signal, and the recovery decision + reason code from the
same `decideWorkerAdoption`. Strict read; no prompt, command, env or token.

## 17. False-positive matrix

| Scenario | Base (pre-P9) | P9 |
| --- | --- | --- |
| worker alive, no transition | adopted/protected on row + name | protected on runtime proof |
| worker alive after sleep | protected | protected; nothing written |
| fix active, signalling | running | running, runtime never probed |
| fix active, quiet, runtime alive | running forever | running |
| fix runtime dead, row `active` | running forever | concluded on workspace evidence |
| reviewer alive | adopted on reviewer probe | unchanged |
| runtime dead (unconfirmed launch) | retried | retried once |
| runtime dead (confirmed phantom) | stop with evidence | stop with evidence |
| runtime of another generation | adopted by natural key | FAIL_CLOSED |
| runtime of another installation | adopted if the name matched | FAIL_CLOSED |
| name reused by a new incarnation | adopted (`has-session` true) | FAIL_CLOSED `instance_mismatch` |
| legacy session, no provenance | adopted on liveness | FAIL_CLOSED `legacy_provenance_missing` |
| unreadable runtime probe | adopted (unknown liveness counted as alive) | WAIT, then FAIL_CLOSED |
| restored worker (Restart) | token stale in tmux | token re-stamped, proven |
| stale `running.json` | removed unconditionally | removed by exact match |
| reused PID | stale if the probe PID differs; unhealthy otherwise | same, never signalled |
| daemon under `<data dir>/running.json` | reported STOPPED | found and verified |
| terminal run at boot | `review_reviewer_unproven` appended, possible STOP | nothing written |
| duplicate recovery race | one owner (evidence could duplicate) | one owner, one confirmation |

## 18. Privacy

Runtime stamps are identities only (installation, daemon instance, owner token). Readback,
proofs and stop records carry identities and closed codes; tests put secrets in the session
prompt and assert they never reach a proof detail or the readback JSON.

## 19. Performance

No new query and no schema change; reads use `idx_workflow_dispatch_checkpoints_step`,
`idx_workflow_checkpoints_run` and `idx_sessions_project` (EXPLAIN QUERY PLAN on the real
DB, read-only). A proof is one `show-environment` per stamp, one pane-pid read, one `ps` and
one `has-session`, taken only on boot reconciliation, `ContinueRun`/wakes, a human reopen,
a fix cycle already silent past its window, and the readback. The `/recovery` readback is
gated behind `?ownership=1` so the UI's 5-second recovery poll does not run it.

## 20. Debts left for P10 / P11 and risks

* ~~Electron wedged-orphan takeover signals a PID that is merely alive (`main.ts`).~~ Fixed in
  the integration review (§22): no signal is ever sent to a port holder.
* session_manager's boot `reconcileLive` still adopts a session whose tmux NAME is alive
  (it grants no workflow ownership, but it is the same weaker probe). It now treats only a
  missing socket as death (§22).
* Reviewer residues (untokened ack, status-only still-authorized check) — documented, not
  changed (closed lifecycle, no duplicate demonstrated).
* conpty recovery parity (fail-closed on restart adoption; does not block the macOS merge).
* `ao import` checks only the configured run-file.
* ~~Attempt-outcome refinements in `failover.go` / `work_adoption.go` stay unconditional.~~
  Fixed in §22.
* Trusted-local mode issues no worker credential, so a work report is fenced by the
  session binding alone (tested in §22: a replaced generation's late report is refused).
* A tmux `$N` is unique only within one server lifetime: after a server restart a NEW
  session can receive a recycled `$N`. The owner token (session + launch) is still compared,
  so a recycled `$N` alone never proves ownership.
* An unprovable reviewer that outlives a CLOSED run is visible only as a (deduplicated)
  warning log; the closed run's ledger stays immutable (§22). A read-only diagnostics surface
  for it is P10/P11 work.
* Soak (P11): 15-minute unreadable grace and fix silence window unvalidated over days.

## 21. Independent adversarial review (fresh context, read-only) — dispositions

| # | Finding | Status | Disposition |
| --- | --- | --- | --- |
| 1 | "Resume agent" respawns through `Restart` with a name-only handle; the empty instance id overwrote the recorded `$N`, so every restored worker became `provenance_missing` forever. T5 missed it because it restarted with the `Create` handle. | CONFIRMED | Fixed in `relaunchSessionWithPolicy`: the recorded incarnation is carried over when the runtime returns the same session without naming one. `TestP9ResumeThroughRestartKeepsTheRecordedIncarnation` goes through `ResumeAgentWithMode`. |
| 2 | Startup skipped a live daemon behind the CONFIGURED run-file when it served another data dir, so a scratch daemon sharing `~/.ao/running.json` would overwrite (and on exit delete) a live daemon's handshake. | CONFIRMED | Fixed: a live daemon behind the configured run-file refuses the start whatever data dir it serves. Test covers the shared path. |
| 3 | Startup compared data dirs with `filepath.Clean` only; `/tmp/ao` vs `/private/tmp/ao` would be two daemons on one DB. | CONFIRMED | Fixed: symlink-aware comparison (`sameDataDirPath`), as the CLI already did. Test covers a symlinked data dir. |
| 4 | Unreadable-runtime grace measured from record age: after a reboot the first failed read parked immediately; a dead tmux server was `unavailable`, not `absent`. | PLAUSIBLE → fixed, then narrowed in §22 | (Superseded by §22 2.1: only a MISSING socket is `absent`; "no server running" stays `unavailable`.) Originally: "no server running" / "error connecting" on AO's private socket is now `absent` (session_manager's boot reconciliation already draws that conclusion after a reboot); other failed reads get a grace measured from the first failed read in THIS process. Tests: real tmux `kill-server`, decision table, hours-old launch. |
| 5 | A concurrent pass could fail an in-flight launch closed inside the 30 s settle window (conpty `unsupported`). | PLAUSIBLE → fixed | Inside the settle window only an adoption may be concluded. `TestP9_InFlightLaunchIsNotStoppedInsideTheSettleWindow`. |
| 6 | After an AO restore of an unconfirmed session (new launch L2, record still L1), natural-key adoption fails closed on `launch_mismatch`. | PLAUSIBLE → fixed in §22 | (Superseded by §22.) Originally kept deliberately: the launch fence pre-dates P9 in `adoptLiveLaunch`; P9 applies it on every adoption path. The false stop is conservative, rare (restore of a never-confirmed worker) and names its reason; relaxing the fence needs its own proof. Documented debt. |
| minor | A lost session bind leaves the spawned runtime unbound (logged, not destroyed); a failed `respawn-pane` after the restamp reads as `owner_mismatch`. | noted | Debts; neither creates a second owner. |

Reviewer's "no defect found": no fail-open or second-owner path with the port wired; reviewer
terminal guard correctly scoped; readback writes nothing and leaks nothing; CLI stop/status
never act on unverified or foreign daemons.

## 22. Independent integration review — dispositions

The integration review re-derived every invariant from the code instead of the author report,
with two fresh-context reviewers plus an own pass. Every correction lives on the feature
branch, in separate commits. "Test" names the test that fails without the fix.

### Stale-generation writes (merge blocker class)

| # | Finding | Disposition | Test |
| --- | --- | --- | --- |
| S1 | `failover.go` success path concluded the predecessor with an unconditional update and opened a successor with a plain create: two passes reporting one failure could both open generation N+1, or a stale pass could overwrite a newer outcome. | Predecessor concluded through `ClaimWorkflowAttemptOutcome`; successor opened through the serialized `ClaimOpenWorkflowAttempt`; a lost claim writes nothing further. | `TestP9StaleGeneration_FailoverLosesThePredecessorAndWritesNothing` (gen-2 wins inside gen-1's claim; -race) |
| S2 | `failLiveWorkAttempt` parked the step and wrote a failure checkpoint without owning the attempt. | Claims the attempt first; a lost claim is a no-op (no waiting, no park, no checkpoint). A ledger refusal whose obligation already has a successor (`failoverAdvancedElsewhere`) is "not yours", never a park. | `TestP9StaleGeneration_FailLiveWorkAttemptLosesAndDoesNotPark` |
| S3 | `work_adoption.go` ignored the result of every step CAS, closed "whatever attempt is latest", and could adopt under an authorized launch. | Refuses while the step's spawn command is pending/dispatched; every hop's CAS result is honoured; only the adopted dispatch's attempt is closed, through the claim. | `TestP9StaleGeneration_AdoptionThatLosesTheStepCASCompletesNothing`, `TestP9StaleGeneration_AdoptionRefusesWhileAWorkerLaunchIsAuthorized` |
| S4 | `worker_progress.go` refined a concluded attempt unconditionally. | `RefineConcludedWorkflowAttempt` compare-and-swaps on the outcome the caller read. | `TestRefineConcludedWorkflowAttemptIsFencedByTheReadOutcome` (store, -race) |
| S5 | Trusted-local work report from a replaced generation. | Already bound to the step's CURRENT session; now proven. | `TestP9Crash_ReplacedGenerationsLateWorkReportIsRefused` |

### Recovery decision

| # | Finding | Disposition |
| --- | --- | --- |
| 1.1 | The generation fence was skipped whenever ANY launch was recorded, including an older generation's record. | Evidence is read from the CURRENT generation only (`currentGenerationLaunchEvidence`: the outbox token names the intent, the intent names the attempt); the creation-instant fence applies whenever the runtime was read. |
| 1.2 | A zero claim instant disabled the fence. | Zero `ClaimedAt` or zero session creation instant fails closed (`generation_mismatch`). |
| 4 | `launch_mismatch` after AO's own restore of the same session was a false stop. | Adopted only when the runtime PROVES the row's current launch and this generation's evidence names this very session; a new launch on any other session stays `launch_mismatch`. `TestP9Crash_C3_SameSessionRelaunchedByAOIsAdopted` + decision table. |
| T | A terminated session row could be adopted. | Never adopted (`runtime_missing` → relaunch path). |
| 3.x | Grace measured on the wall clock; readback started it. | Measured on a monotonic clock from this process's first failed read; only a successful read ends the episode (sparse polling cannot reset it); readback is read-only and reports `settle_window` inside the window. |
| 7 | `stopWorkerOwnershipUnproven` could stop a run that became terminal meanwhile. | Re-reads terminal state on disk first. |

### Runtime facts (tmux)

| # | Finding | Disposition |
| --- | --- | --- |
| 2.1 | Any unreachable server was `absent`, so a transient tmux failure could relaunch a second worker (and session_manager could stash/terminate a live session). | New `ports.ErrRuntimeServerAbsent`, wrapped ONLY for `error connecting to <socket> (No such file or directory)` (measured on tmux 3.7b). Classify, `reconcileLive` and `restartRuntime` treat only that as death. Real tmux: kill-server with the socket left → `unavailable`; socket removed → `absent`. |
| 2.2 | `instanceEnv` read any failure as "unmarked". | Only `unknown variable` is unmarked; `no such session` (3.7b) is instance-gone; everything else is an error. |
| 1.3 | Name resolution used a bare target (prefix match). | Measured: `display-message -t =name` answers EMPTY with exit 0 (would read a live session as absent); `-t =name:` is exact. `exactSessionFormatTarget`. |
| 5.1 | Resume handle carried no incarnation. | `RuntimeHandle{ID, InstanceID}` from the row. |
| 5.2 | Restart restamped the owner without rollback. | Reads the previous owner from the exact incarnation first (refuses if unreadable) and restores it on any later failure. Unit tests with the fake runner. |

### Daemon, discovery, Electron

| # | Finding | Disposition |
| --- | --- | --- |
| B1 | Startup guard missed the default run-file for `--data-dir`. | `config.DefaultRunFilePath()` is a candidate; an unreadable candidate refuses the start. |
| B2 | Check-then-write race between two starting daemons. | `internal/daemonlock`: exclusive non-blocking OS locks (flock / LockFileEx) on `<dataDir>/daemon.lock` and `<runFile>.lock`, held for the daemon lifetime, taken before the store opens. Race test: 16 acquirers → 1 winner. |
| B3 | `installation_id` could be observed empty/partial; a malformed one ran unstamped. | Staged temp file + fsync + `os.Link` (exclusive publish) + dir fsync, 0600; a malformed identity refuses the daemon start and is never rewritten. |
| B6 | Unprovable reviewer on a closed run was invisible. | Deduplicated warning log; the closed run's ledger stays immutable (a durable record was tried and rejected: it violates §14). Immutability test now counts the ledger before any read and guards against vacuity (`probeCalls` must move). |
| B10 | CLI: live PID + probe answered by another process → `stale` (file removed). | `running_unverified`: no stop, no signal, file kept. |
| E | Electron takeover sent SIGTERM to a merely-alive PID. | `decidePortHolderTakeover` → spawn / graceful_shutdown (HTTP, identity-verified, PID and instance must match) / refuse. No signal path remains. |
| E2E-3 | — | Real daemon: forged run-file naming a live bystander PID with a mismatched instance → `ao status` unverified, `ao stop` refuses, the bystander receives no signal, the answering daemon survives, the file is kept, and a second daemon on the same data dir refuses to start. |

