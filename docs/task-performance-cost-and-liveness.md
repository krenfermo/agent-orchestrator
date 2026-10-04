# Task performance, cost, liveness and context routing — P5/P6

**Status.** Phases A–D and G–J are **implemented** on this branch. Phases E, F
and I are **design and inventory**: for each one this document says exactly what
already exists, what the measurement says a change there is worth, and what is
deliberately not built. Nothing below claims a mechanism AO does not have.

It builds on `docs/task-context-and-cost-budget.md`, which holds the raw
measurement of run `wf-1c2cb9bd` and is not repeated here.

---

## A. Inventory — Task, end to end

### A.1 The path

```
UI (routes/_shell.workflows.$workflowId.tsx)
  -> GET /api/v1/workflows/{id}            controllers/workflow.go
  -> workflow.Coordinator.GetRun           internal/workflow/workflow.go:1330
       observeWorkStep                     worker_progress.go        (git + session facts)
       reconcileQuestions                  questions_wiring.go
       advanceReviewFixCycle               cascade.go
       applyCheckpointAuthority            checkpoint_authority.go
       observeWorkerLiveness               worker_liveness_view.go   (NEW, P5/P6)
  -> DerivePresentation / DeriveLifecycle / DeriveAdvice   (pure projections)

CreateRun seeds six steps: plan, work, review, fix, verify, advance.
StartRun completes `plan` LOCALLY (BuildPlanArtifact) and unblocks `work`.
Dispatch -> spawner -> agent harness -> hooks -> lifecycle.Manager
  -> sessions.activity_state / activity_last_at / last_signal_at
Usage:  harness transcript -> observe/usage {watcher,parser,ingestor,coordinator}
        -> model_usage_events, bound by usage_bindings, attributed through
           usage_attribution_windows (role, cycle, attempt, STEP).
```

### A.2 Where each thing asked for actually lives

| Asked for | Where it is | State |
|---|---|---|
| `model_usage_events` | `internal/observe/usage/*`, table since migration 0052 | exists |
| pricing | `internal/observe/usage/pricing` — versioned rate card, refuses to invent a cost | exists |
| lifecycle manager | `internal/lifecycle/manager.go` | exists |
| `worker_progress` | `internal/workflow/worker_progress.go` | exists |
| `activity_last_at` | `sessions`, a TRANSITION clock | exists |
| liveness | `sessions.last_signal_at`, migration **0168**, `Activity.Liveness()` | **added (Phase B)** |
| signals | `ports.ActivitySignal` — carries a tool NAME, no arguments, not persisted | exists, bounded |
| context construction | `internal/contextrouter` (+ `wfrouter`), `session_context_pack.go`, `task_knowledge.go` | exists, **off by default** |
| `CLAUDE.md` / system prompt | `~/.ao/data/prompts/<session>/system.md`, written by `session_manager` | exists |
| MCP / tools | harness-side; AO passes `--append-system-prompt-file` and nothing else | not AO's |
| memory / code graph | `internal/projectmemory`, `internal/codegraph` | exists |
| Grae / Graphify adapters | **ports only** — `projectmemory/graph.go`, `codegraph/codegraph.go` | see §F.3 |
| compaction | no harness primitive AO can invoke or observe | does not exist |
| budgets | `domain.UsageBudgetPolicy` + `service/usage/budget.go` | exists (tokens, cost) |
| per-step attribution | `usage_attribution_windows.workflow_step_id` was stored, never grouped on | **added (Phase C)** |
| context trajectory | — | **added (Phase C)** |

### A.3 The two things this inventory refused to build

1. **A second telemetry.** Every figure added by this branch is folded from
   `model_usage_events` rows that already existed, read through the attribution
   view the cost ledger already reads. No new ingestion, no new table, no new
   hook.
2. **A tool-call ledger.** See §G.2.

---

## B. Liveness — implemented

Brought forward from `fix/worker-liveness-signal` rather than rewritten, with
the migration renumbered 0164 → **0168** because Skills had burned 0164–0167.

- `activity_last_at` stays a **transition** clock. The lifecycle reducer folds a
  same-state repeat without writing it, which is correct: `PauseScopeID`,
  `humanQuestionFact` and the `waiting_input` telemetry each identify ONE
  episode, and refreshing on every repeat would collapse them all onto "now".
- `last_signal_at` is the **liveness** clock. It advances on every signal that
  passes the generation fences, same-state repeats included, coalesced to 30s —
  one write per half-minute per active session rather than one per tool call.
- `Activity.Liveness()` is the single point every silence question goes through.
  A row written before the column falls back to the transition clock, so a
  legacy session does not read as "never heard from".
- `workerNeedsInputCorroborationWindow` now measures against `Liveness()`. A
  genuinely mute worker keeps its recovery path untouched.
- **Deliberately not in `sessions_cdc_update`.** `change_log` is an append-only
  ledger with no retention policy and a heartbeat does not belong in one.

Tests: same-state repeats, the coalescing window, out-of-order signals, a real
transition, the pause instant intact, the completion receipt intact,
corroboration with and without signals, real silence, the no-column fallback,
persistence across all four write paths, and respawn.

---

## C. Context telemetry — implemented

### C.1 What is now reported, per run and per step

`GET /api/v1/workflows/{id}/usage` → `dynamics`:

| Field | Source |
|---|---|
| input / output / cache read / cache write | `model_usage_events` (already reported) |
| cost | existing versioned rate card, per model, never blended |
| **provider calls** | count of placeable events |
| **initial context** | `input_tokens` of the first placeable call |
| **current context** | `input_tokens` of the last |
| **peak context** | max over the series |
| **growth** | last − first, floored at zero |
| **growth per call** | growth ÷ (calls − 1); null below two calls |
| **series duration** | last observed − first observed |
| **per step** | grouped by `(workflow_step_id, role, cycle)` |

`input_tokens` IS the context: the V1 parser folds cache reads and writes into
it, so `context(i)` is one column rather than an addition a caller could get
wrong.

### C.2 Classification by source

Already existed and is untouched: `domain.ContextCompositionView`
(`service/usage/context.go`) classifies **what AO assembled** into `task_spec`,
`project_memory`, `shared_knowledge`, `repo_content`, `index_reuse`, `other`,
in measured bytes with an explicitly labelled token estimate.

**The other half is not observable and is not estimated.** Of the worked run's
54,402-token first call, 26,009 were a cache-warm prefix — the harness's own
system prompt and its tool JSON schemas. No provider reports a breakdown of it
and it is not in the transcript. AO reports what it can name and says the rest
is unattributed, rather than inventing a split. This is the same refusal the
pricing package makes for an unpriced model.

### C.3 What is not stored

No prompt, no message body, no tool argument and no secret is stored or re-read
to compute any of the above. Measuring a conversation's SIZE never requires
reading its content.

---

## D. Budgets — implemented, warn-only

Two kinds of ceiling now exist and they are **not** the same kind.

**Hard ceilings (pre-existing, unchanged).** `WorkflowTokenBudget`,
`WorkflowCostBudgetUSD` and the project-daily pair. Consulted only at a **safe
boundary** — before AO starts a worker, a repair, a child task — never
mid-response. An over-budget run finishes what it started and starts nothing
more. A cost ceiling is unenforceable while any model in play is unpriced.

**Advisory profiles (new).** `domain.UsageBudgetProfileFor(strategy)`:

| | wall clock | provider calls | context growth | cost |
|---|---|---|---|---|
| **Task** | 30 min | 120 | 100,000 tok | $5 |
| **Autonomous** | 2 h | 600 | 250,000 tok | $25 |
| **Master** | 6 h | 2,000 | 400,000 tok | $75 |

Task's numbers are the measured run rounded **down**, not up: 193 calls and
50 minutes for a single-field fix is the thing that should have been said out
loud, so the line sits below it.

Overridable per run through four new `UsageBudgetPolicy` fields
(`workflowWallClockWarnSeconds`, `workflowProviderCallWarn`,
`workflowContextGrowthWarnTokens`, `workflowCostWarnUsd`). Zero means "use the
profile default", exactly as zero means "no limit" on the ceilings — a snapshot
written before these existed must not read as a run permanently in warning.
They do not make `Configured()` true, so they can never be mistaken for a gate.

**An unknown strategy gets the AUTONOMOUS profile, not the Task one.** Measuring
a run of unknown shape against the tightest expectations would warn constantly
and teach a person to ignore warnings.

**No hard cancel, and the reason is not timidity.** A run interrupted between
"the model is generating" and "AO recorded an attempt" is precisely the
ambiguous lifecycle `internal/workflow` spends thousands of lines preventing. A
wall-clock budget has no durable recovery path today, so it gets no authority
today. When one exists and is proven, promoting a threshold from advisory to
enforcing is a one-line change at the evaluation site. Inventing the authority
first and the recovery later is how a budget eats a run nobody can restart.

---

## E. Fast Task profile — what is already free, and what cannot be removed

The brief asks for a Task profile that drops the planner, keeps structured
Verify, injects minimum context, drops irrelevant tools, avoids indiscriminate
repo reads, and keeps proportional review. Measured against the real run, here
is the honest state of each.

### E.1 Already true, at zero cost

- **No planner.** `ExecutionStrategy.Planned()` is false for `task`, and
  `requirePlannedStrategy` refuses to push a task run through the master
  planner — durably, so a later caller, a stale writer or a resumed wake cannot
  do it either. A Task's `plan` step is completed **locally** by
  `BuildPlanArtifact` in `StartRun`: it makes **no model call at all**. There is
  nothing here left to remove; the saving was already banked.
- **Structured Verify stays mandatory.** Enforced on `main` and re-verified by
  this branch's gate run (§J.5). Not weakened.
- **Proportional review stays in force.** `ReviewDepthPolicySnapshot` +
  `domain.ResolveReviewDepth`, defaulting to `deep` for any snapshot that
  predates the field so scrutiny is never retroactively downgraded.

### E.2 What AO could remove, and what it is worth — measured

AO's own injection into the worked run's 54,402-token first call:

| AO-owned | tokens | share of the first call | share of the 36.2M bill |
|---|---:|---:|---:|
| injected system prompt (`system.md`) | ~2,024 | 3.7% | ~1.1% |
| objective prompt | ~2,124 | 3.9% | ~1.1% |
| **total AO owns** | **~4,148** | **7.6%** | **~2.2%** |

Everything else in that call is the harness's: the project `CLAUDE.md` (~7,048),
the skill listing (~2,326), the deferred tool list, the agent listing, the MCP
instructions, and the ~26,009-token cached system/tool-schema prefix. **AO does
not inject any of it and cannot drop any of it.** `--append-system-prompt-file`
is the entire interface AO has to the harness's context.

So the maximum a "minimum context" Task profile can save, if AO injected
*nothing at all*, is ~2.2% of the bill — and it would cost the worker its
objective. There is no version of this that is both material and safe.

### E.3 What is deliberately NOT removed, and why it is not safe to

- **Tool / MCP pruning.** The harness owns its tool schemas; AO has no flag to
  narrow them. Claiming a Task profile "drops irrelevant tools" would be
  describing a mechanism that does not exist.
- **Truncating tool results.** ~37,500 tokens across 96 results on a 36.2M-token
  run — 0.1%. It is also the evidence a review step reads. The ratio does not
  come close to justifying it.
- **Dropping project instructions.** Same argument, plus they are the harness's
  read and not AO's to drop.
- **Forced compaction.** AO does not drive the harness's context window and has
  no durable representation of a compaction boundary. Forcing one would discard
  conversation state the workflow cannot reconstruct, with no way to prove the
  run recovers from it.

### E.4 Where the cost actually is

```
billable input  ~=  sum over i of context(i)
```

193 calls × a context growing 54k → 324k. **Two levers, and only two: the number
of turns, and the growth rate.** On this run the agent's own output (139,003
tokens) turned into input on every later call and outweighed every tool result
combined (~37,500). The long polling sequences — `sleep 45`, check, repeat,
while a suite ran — each cost a full context re-read (~200k tokens, ~$0.10) for
one line of output. That is the real target, it lives in the harness's turn
loop, and AO's honest contribution to it today is to **make it visible** (§C)
and to **say so when it is out of shape** (§G).

---

## F. Context Router — what exists, and what a Task bundle is

### F.1 It is already built

`internal/contextrouter` assembles exactly the ContextBundle the brief
describes, and it has since P2:

- **candidate files** — `GitDiffSource`, name-status against the checkout;
- **related symbols** — `codegraph` native indexer, per-file symbol extraction;
- **dependencies** — graph edges (expanded tier only);
- **applicable instructions** — `SectionDocument`;
- **relevant history** — durable project memory (`projectmemory`), through
  `NewDurableMemorySource` or wfmemory's deduplicated pack.

It is **role-aware** (`roleSectionOrder`: a planner reads documents before a
diff; a worker and a reviewer start from the change) and **budgeted per role**
(`compact / expanded / hard cap`), with a compact retrieval tier that exists
precisely so AO does not pay for retrieval the budget would drop. The task
section leads every role, because a payload that dropped it would be smaller and
useless.

It avoids the whole repo, unrelated global memory and the whole graph **by
construction**: the retrieval tiers ask the sources for a bounded number of
files, symbols, edges and memory items, and the budget packs whatever they
return.

### F.2 What is NOT true, and this branch does not change

**It is off.** `AO_CONTEXT_ROUTER` gates it; unset, `wfrouter.Instrument` hands
the dependencies back untouched and every adapter receives exactly the context it
received before the package existed.

**Turning it on for Task would not have helped the measured run**, and the
arithmetic is §E.2's: the router budgets what AO *assembles and injects*, which
is 4k of a 54k first call and **none of the 269k of growth**. It is a good
mechanism aimed at a small number. Enabling it by default is a decision about
dispatch payloads that deserves its own evidence, and this branch does not make
it on the strength of a benefit it has just measured as ~2%.

### F.3 Grae / Graphify — not integrated, and not pretended to be

`projectmemory/graph.go` and `codegraph/codegraph.go` define **ports**. The
shipped implementations are `LocalGraph` and the native indexer, and the read
model reports the backend by its real name:

> `Provider` names the graph backend actually in use. AO ships LocalGraph; it is
> reported by its real name and never as "Graphify", which is a separate
> external adapter that does not exist yet.

The audit that produced these ports found Graphify named only in prose and Grae
not at all. Nothing on this branch changes that, and nothing on this branch
reports a graph provider AO is not running.

---

## G. Loop and cost anomaly detection — implemented, warn-only

### G.1 What AO can and does detect

Derived at read time from durable facts, advisory only:

| Code | Fires when |
|---|---|
| `provider_calls_above_profile` | call count past the strategy profile |
| `context_growth_above_profile` | growth past the strategy profile |
| `duration_above_profile` | wall clock past the strategy profile |
| `cost_above_profile` | calculated cost past the advisory figure (never from an unknown cost) |
| `growth_without_progress` | **all three:** context growing **and** no durable progress since dispatch **and** the worker alive |
| `cache_read_dominant` | ≥90% of billable input is cache reads — **`info`, not `warn`** |

**The conjunction is the load-bearing one.** Growth alone is what an agentic
loop does. No-progress alone is normal early in a step. A silent worker belongs
to the recovery path, not to a second competing opinion. On `wf-1c2cb9bd` the
conjunction would **not** have fired: git evidence appeared five minutes in.
That is the property that makes it worth having.

**Long legitimate tests are not loops** and are separated by that same
conjunction, plus the liveness clock: a suite that runs for forty minutes has
durable progress since dispatch, or a worker that is still being heard from, or
both. The regression is `TestALongRunningStepWithDurableProgressIsNotWarnedAbout`.

**Cache-read dominance is `info`.** 98.8% was measured on a run with no loop, no
retry and no defect. Styling it as a fault would teach a person to distrust a
healthy run; it exists so a large number is not left to be interpreted alone.

### G.2 What AO cannot detect, and why it is not approximated

Three of the requested shapes — **the same command repeated**, **excessive
polling**, **the same suite run again and again** — are not derivable from
anything AO stores:

- `ports.ActivitySignal` carries `ToolName` and `ToolUseID`. **No arguments.**
- It is **not persisted**. `lifecycle.Manager` keeps in-flight tool ids in memory
  to correlate a blocking dialog and drops them at the turn boundary.

Detecting a repeated command would require a durable tool-call ledger carrying
command text — a new telemetry (explicitly out of scope) and a store of user
content AO has no reason to hold. They are **named** here rather than
approximated, for the same reason the pricing package reports `unknown` rather
than `$0.00`.

---

## H. UI — implemented

Run detail now shows, under the status and above the actions:

```
Agent                                    Working
Last signal        3s ago
Last state change  20m ago
```

and inside the token ledger, directly under the total it explains:

```
Shape of the cost
Provider calls    193
Context           54.4k → 323.7k (+269.3k)
Peak context      323.7k
Series duration   50m
By step           worker  180 calls · 34.0k · USD 0.42
                  fix_worker · repair 1  13 calls · 9.0k · USD 0.11
[warn] The conversation has grown more than this kind of run usually does. (269,334 of 100,000)
[info] Almost all of the bill is the model re-reading the same conversation. Normal for a long turn, not a fault.
```

Budget percentage keeps its existing meter, unchanged.

**The rule the UI now enforces:** no surface may say "silent", "stuck" or
"inactive" from the transition clock. Only `silentForSeconds` — derived by the
daemon from `lastSignalAt` so every surface agrees — may make that claim, and
only past a 10-minute threshold generous enough that a worker mid-suite never
trips it. Regressions:
`only says an agent is quiet when the SIGNAL clock is old` and
`does not call an agent quiet on an old transition clock alone`.

The Board card is deliberately **not** given this read: it polls every two
seconds across every card, and one session read per card is a real cost on a hot
path. Its `lastMeaningfulActivityAt` remains the workflow's own last durable act,
which is correct by its own definition.

---

## I. Cost routing — design only, nothing activated

### I.1 What exists

Routing is per **role**, not per run kind: `UserExecutionPolicy` holds
`PlannerPriority`, `WorkerPriority`, `ReviewerPriority` and
`DecisionResolverPriority` as ordered `ProviderProfileID` lists, frozen into the
run at creation as `ExecutionPolicySnapshot`, with **live eligibility re-checked
at dispatch** — a disabled or deleted profile is skipped, never force-used.
`ExecutionRouter` records a `RoutingDecision` per step (preferred, selected,
fallback used, reason codes), which the run detail already renders.

### I.2 The proposed shape

Add a **second dimension** — strategy — to an existing per-role list, as a
defaulting rule and never as an override:

```
effective priority(role, strategy) =
    user's explicit priority for that role, if they set one     <- always wins
    else the strategy's default order for that role
    else the shipped default
```

with defaults of the form: Task prefers the fastest eligible profile for
`worker`; Autonomous keeps today's order; Master prefers the strongest for
`planner` and `reviewer`. **Reviewer is never downgraded by strategy** — review
independence is a safety property, not a cost dial.

### I.3 The four constraints it must respect

1. **User override wins, always.** A strategy default may only fill a list the
   user left empty. Anything else silently changes a model somebody chose.
2. **Provider availability.** The live eligibility re-check stays exactly where
   it is; a strategy preference for an unavailable profile falls through the
   existing fallback chain and records the same reason codes.
3. **Risk.** A change's risk tier already drives review depth
   (`ResolveReviewDepth`). A cheap model for the worker must not be able to
   lower the reviewer's, and the reviewer's selection must stay independent of
   the worker's.
4. **Review policy.** `ReviewIndependence` is unchanged and un-overridable by
   this mechanism.

### I.4 Why it is not activated here

Changing which model runs a Task changes the output of every Task. That needs
its own evidence — a comparison of outcomes, not of prices — and the honest
finding of this branch is that the measured cost was **not** a model-choice
problem: it was 193 calls against a growing context, and the same 193 calls
against a cheaper model is the same shape at a different unit price. Nothing on
this branch changes any default model.

---

## J. Regressions

| # | Requirement | Where |
|---|---|---|
| 1 | worker active >20 min reads as recently active | `worker_liveness_view_internal_test.go`, `workflow-liveness-panel.test.tsx`, `worker_liveness_test.go` |
| 2 | spurious `waiting_input` with recent signals does not park the run | `TestWorkStep_WaitingInputIsNotAmbiguousWhileSignalsArrive` |
| 3 | context 50k→170k earns a warning | `TestContextGrowthPastTheProfileEarnsAWarning` |
| 4 | cache-read dominance is separated in the UI | `separates cache-read dominance as information rather than as a warning` |
| 5 | a Task with no Verify is still rejected | `TestWorkflowCreateTaskRunWithoutVerificationIsRefused`, `TestWorkflowCreateTaskRunWithAnUnrunnableCommandIsRefused`, `TestTaskStrategyNeverPlansAndKeepsReviewAndVerify` — pre-existing, re-verified rather than duplicated |
| 6 | committed-clean changes stay visible to review | `TestAWorkerThatCommittedItsWorkIsNotSeenAsAWorkerThatDidNone`, `TestCommittedAndUncommittedWorkAreOneDeduplicatedSet`, `TestASensitiveFileThatWasCommittedStillForcesAReview` — pre-existing, re-verified |
| 7 | long tests are not classified as a dead worker | `TestALongRunningStepWithDurableProgressIsNotWarnedAbout`, `TestASilentWorkerIsNotThisAdvisorysBusiness` |

Plus: real silence still stops the run
(`TestWorkStep_WaitingInputStillAgesIntoAmbiguityWhenTrulySilent`), a
corroborated question still outranks liveness
(`TestWorkStep_CorroboratedQuestionStopsRunEvenWhileSignalsArrive`), and no
advisory carries authority (`TestAdvisoriesCarryNoAuthority`).

---

## What this branch did not do

- Did not enable the context router.
- Did not change any default model.
- Did not weaken review, Verify, recovery or any authority.
- Did not add a hard cancel on time, calls, growth or advisory cost.
- Did not add a second telemetry, a tool-call ledger, or any store of prompt
  content.
- Did not estimate the harness half of the initial context.
