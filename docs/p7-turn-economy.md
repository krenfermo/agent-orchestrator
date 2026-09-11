# Turn economy and context growth — P7

**Status.** Phases A–B, E, G–L are **implemented** on this branch. Phases C, D
and F are **measured and closed without a change**: for each one this document
gives the number that says the change is not worth making, rather than building
machinery the evidence does not support. Nothing below claims a mechanism AO
does not have, and every figure is labelled either *measured* or
*replay-derived*.

It builds on `docs/task-performance-cost-and-liveness.md` (P5/P6) and
`docs/task-context-and-cost-budget.md`, which hold the raw measurement of run
`wf-1c2cb9bd` and are not repeated except where a figure is used.

---

## 0. The answer in one table

Run `wf-1c2cb9bd` (2026-09-09, project medusa), worker session `medusa-12`.
A one-field defect: an enrolled image already linked to an entity still showed
"Persona sin identificar".

| | ANTES (measured) | DESPUÉS (replay-derived) | change |
|---|---:|---:|---:|
| model calls | **193** | **193** | 0% |
| initial context | 54,404 | 54,404 | 0% |
| peak context | **323,738** | **196,853** | **−39.2%** |
| final context | 323,738 | 196,853 | −39.2% |
| cumulative billed input | **36,082,816** | **21,997,829** | **−39.0%** |
| output tokens | 139,003 | 139,003 | 0% |
| elapsed | 47m 24s | not modelled | — |
| tool turns (work) | 191 | 191 | 0% |
| coordination / poll turns | 2 | 2 | 0% |
| Verify | unchanged | unchanged | — |
| Review | unchanged | unchanged | — |

**The success criterion was ≥30% off one of the two levers. Criterion B is met
at 39.0%; criterion A (call count) is not met and is not claimed.**

The "después" column is produced by `internal/observe/turnbench` replaying the
run's recorded call series with the conversation replaced at each repair
boundary, which is what `workflow.maybeCompactBeforeFix` does when the session
lifecycle policy reaches COMPACT. It is a projection of one lever with the
other held fixed, and §K says precisely what it can and cannot support.

The whole run, including the three reviewer sessions (43 calls, 2,122,052
input, 13,488 output), was 236 calls and 38,204,868 billed input. The reviewer
sessions are short-lived and start fresh; nothing here changes them.

---

## A. The turn loop, as it actually is

### A.1 The path

```
Task -> workflow.Coordinator.dispatchWorkStep      internal/workflow/dispatch.go
     -> WorkerLaunchRequest -> Spawner             dispatch_state_machine.go
     -> session_manager.Spawn                      internal/session_manager/manager.go
     -> agent adapter GetLaunchCommand             adapters/agent/claudecode/claudecode.go
     -> runtime adapter (tmux / ptyexec / conpty)  adapters/runtime/*
     -> `claude` runs INTERACTIVELY in a pane
          |
          +-- model call -> tool call -> tool result -> model call -> ...
          |   ALL OF THIS IS INSIDE THE HARNESS PROCESS. AO is not in the loop.
          |
          +-- hooks (adapters/agent/hooksjson) -> `ao hooks` -> lifecycle
          |     -> sessions.activity_state / activity_last_at / last_signal_at
          |
          +-- transcript JSONL -> observe/usage {watcher,parser,ingestor}
                -> model_usage_events -> usage_attribution_windows (role, cycle, STEP)
```

### A.2 The load-bearing fact

**AO does not own the model loop.** It launches an interactive harness in a
pane and writes prompts into it. Every question in the phase brief about "when
to call the model again", "what history is kept", "which tool results are
added", "is there compaction", "does the provider support pruning" has the same
answer for AO: *that is decided inside the harness process, and AO observes it
after the fact through the transcript.*

So the levers are exactly three, and they are the only three:

1. **What AO sends** — the prompt, the standing instructions, the fact packs.
2. **How often AO dispatches** — one session per task; repair cycles into the
   same session; reviewer and resolver in their own.
3. **What AO can ask the harness to do to itself** — which today is exactly one
   thing: replace its conversation with a summary of it.

Everything implemented in this checkpoint is one of those three. Everything
asked for that is none of them is reported in §C/§D/§F as out of AO's reach,
with the measurement that says so.

### A.3 Where each decision lives

| Decision | Where | Owner |
|---|---|---|
| when to call the model again | harness process | **not AO** |
| what history is kept per call | harness process | **not AO** |
| which tool results enter the prompt | harness process | **not AO** |
| whether compaction happens | harness, on request | **AO can ask** (P7) |
| resume / checkpoint | `claude --resume <uuid>`, `claudecode/continuation.go` | AO |
| how long work waits | inside the harness's own tool call | **not AO** |
| polling | inside the harness's own tool call | **not AO** |
| subagents | harness; transcripts ingested as `claude_subagent` | AO observes |
| CLAUDE.md / tool schemas / skills / MCP | harness reads them itself | **not AO** |
| system prompt | `--append-system-prompt-file`, `session_manager/prompt.go` | AO |
| task prompt | `workflow/plan.go` `BuildWorkStepPrompt` | AO |
| repair prompt + fact pack | `workflow/cascade.go`, `session_context_pack.go` | AO |
| which session a repair goes to | `workflow/fix_dispatch.go` | AO |

---

## B. Classifying turns — implemented

`model_usage_events.turn_class` (migration **0169**), derived in
`internal/observe/usage/turn_class.go` from the block types and tool names of
each billed assistant message. Vocabulary in
`internal/domain/usage_turn_class.go`: `edit`, `command`, `read`, `wait`,
`subagent`, `plan`, `message`, `mixed`, plus the empty **unclassified**, which
is an absence of information and never folded into a real class.

**Nothing but the block type and the tool name is read.** The decoded struct
(`claudeContentBlock`) has no field for `input`, `text`, `thinking` or
`content`, so no command, argument, path or message body can reach the column
even by accident. `TestParserStateNeverCarriesCommandText` asserts it against a
transcript deliberately full of them. This closes the gap
`service/usage/dynamics.go` documented as underivable — it believed detecting a
repeated command needed a durable ledger of command text; it needs only to know
that a call's single act was to check on something already started.

One billed message arrives as several transcript records. On the worked
example, **109 of 193** had their thinking block in the first record and the
tool call in a later one, so classifying from the record that produced the event
would have called 56% of the run "message". Classes are accumulated as a set
across records of one message id, in durable parser state so a tailer batch
boundary cannot split a message, and a differing class on an already-stored key
is treated as a **refinement** — never as the conflict a differing token vector
would be, and never downward.

Composed with the role/cycle axis the ledger already carries, this answers the
question the brief asked:

### B.1 ¿Cuántas llamadas fueron trabajo y cuántas coordinación?

| class | calls | share |
|---|---:|---:|
| `command` | 183 | 94.8% |
| `edit` | 8 | 4.1% |
| `message` | 2 | 1.0% |
| `read`, `wait`, `subagent`, `plan`, `mixed` | 0 | 0% |
| **work** | **191** | **99.0%** |
| **coordination** | **2** | **1.0%** |

**This redirected the whole checkpoint.** The expensive run was not wasteful in
turn kind. It polled zero times. It delegated zero times. It wrote no plan
state. 99% of its calls did something. No amount of eliminating coordination
turns could have paid for it.

---

## C. Agentic polling — measured, nothing built

Of the 191 tool calls, **four** contained a `sleep`, and three of those four
were not polling at all: they were bounded wait loops *inside a single command*
(`for i in $(seq 1 40); do [ -s "$OUT" ] && break; sleep …; done`). The agent
also used background tasks that woke the session on completion — eleven
`<task-notification>` wakeups appear in the transcript.

**The wait was already outside the model.** There is no AO-side runtime wait to
build here, because the harness already has one and the agent already used it.
Inventing an `ao wait` and instructing agents to prefer it would add a
vocabulary that duplicates a working mechanism.

What is built instead is the *detection*: `TurnWait` and the warn-only
`repeated_wait_check_shape` advisory, so a run that does exhibit the shape is
visible rather than assumed absent. Its threshold (10 calls) is explicitly
documented as **not anchored on a measurement**, because the measurement was
zero.

---

## D. Batching — measured, nothing built

The agent was **already batching**. Of 191 commands, the majority chained
several operations with `&&` or `;` in one call — `ls -la && git status &&
ls backend_node`, a `wc -l` and a `cat` together, a `grep` with a heredoc SQL
query. A rough classification of what the commands did:

| shape | commands |
|---:|---|
| read and/or search only | ~119 (62%) |
| build / test | ~16 |
| remote ssh / psql | ~15 |
| git | ~15 |

The exploration was the cost, not the failure to batch it. An AO-side
`ao ctx snapshot` that returned `git status` + `git diff` + `git log` in one
call would have collapsed a handful of the 15 git commands and left the 119
reads untouched — under 5% of the calls, and none of the growth. It is not
built, and this paragraph is why.

The *encouragement* to batch is in the Task turn-economy section (§G/I), where
it costs eighty tokens instead of a new command surface.

---

## E. Checkpoint and compaction — implemented

### E.1 What already existed

Checkpoint 8M defined three lifecycle actions and described COMPACT as reusing
the session *"but with a fresh, fact-only SessionContextPack instead of relying
purely on accumulated conversation state"*. Only the second half was built:
`applyFixLifecycleDecision` prepended the pack and said so in its own comment —
*"Never changes which session receives the prompt"* — so the accumulated
conversation the action was defined against went on accumulating.

`SessionLifecycleRequest.ContextPressure` carried a comment since 8M saying no
production call site set it, because *"AO has no per-session live token/context
signal yet"*.

### E.2 What P7 adds

**The signal.** `domain.SessionContextReading` folds the same
`model_usage_events` rows the cost ledger reads into the two figures a decision
needs (last and peak context). `Coordinator.sessionContextPressure` compares
the last figure against `UsageBudgetProfile.ContextPerCallTokens` — **the same
field the `context_per_call_above_profile` advisory reads**, so a decision and
the warning a person sees about the same run agree by construction. Task:
150,000, anchored on the measured run's own mean of 186,957 rounded down.

An unobserved session is **unobserved, never small**: `UnderPressure` returns a
second boolean, and the policy records `unknown_usage` rather than reusing on a
silent assumption.

**The act.** `ports.ConversationCompactor` is an optional adapter capability;
`claudecode` implements it as `/compact <focus>`. `session_manager.Manager.
CompactConversation` resolves the harness's own vocabulary and delivers it
through `SendReportingSubmission`, so a directive left sitting in a composer is
reported as not-requested rather than leaving the fix prompt to land underneath
it. `workflow.maybeCompactBeforeFix` records the request durably **before**
attempting it, so a crash loses a compaction instead of repeating one, and is
best-effort throughout.

**Every failure mode returns to the previous behaviour exactly:** policy off,
harness without the capability, transport refusal, unsubmitted directive. The
fix prompt and its fact pack are delivered unchanged in all four.
`TestFixCycleIsDeliveredWhateverHappensToTheCompaction` pins two of them
directly.

**Default off** (`policy.sessionCompactionEnabled`). The saving is arithmetic
and large, but the *act* has never been observed live, and a default that turns
on unobserved behaviour in every repair cycle of every run would be asserting
the saving rather than offering it.

### E.3 What is never dropped

A compaction is a harness summarising itself; AO's durable facts are what
actually cross it, in full, in the message that follows. The objective, the
acceptance criteria, the unresolved review findings, the changed files, the
tests and the next action travel in the `SessionContextPack`
(`session_context_pack.go`) — unchanged by this checkpoint. Approval state,
verify requirements and recovery provenance never lived in the conversation at
all: they are durable rows, and the recovery path reads them from there.

---

## F. Tool result compression — measured, nothing built

Attributing the 269,334 tokens of growth to what appeared in the transcript
between consecutive calls (estimates at 4 bytes/token, ~84% of the observed
growth accounted):

| what grew the conversation | share |
|---|---:|
| tool results | 28.1% |
| the agent's own thinking | 19.4% |
| the agent's own `Bash` tool inputs (the commands) | 19.3% |
| harness prompt snapshot | 15.3% |
| the agent's own `Write` inputs | 3.9% |
| harness `edited_text_file` attachments | 3.8% |
| AO's own queued prompts (`ao send`, task notifications) | 2.9% |
| harness environment / token reminders | 4.0% |
| the agent's own text | 1.2% |

**The agent's own output is the largest contributor (≈44%), ahead of tool
results (28%).** Confirmed independently by the totals: 139,003 output tokens
against roughly 61,000 of tool result.

And the largest single tool result was 27,533 characters — about 6,900 tokens.
Storing it out of band and leaving a reference would have saved ~7k of a 269k
growth. **Tool output was not the problem in this run**, so no out-of-band
capture is built; doing it would add a retrieval surface, a staleness question
and a truncation risk in exchange for under 3%.

What the number *does* argue for is output discipline, which is §G.

---

## G. Agent output discipline — implemented (unproven)

139,003 output tokens, of which the agent's prose was only ~3,800: the rest is
thinking (~44k) and the tool-call inputs themselves (~54k). There is no
recapitulation habit to cut here — two of 193 calls were text-only.

So the guidance is bounded to what the evidence supports: keep the messages
*between* actions short, because every later turn re-reads them; the report that
matters is the final one and the `ao work report` beside it. It is one line in
the Task turn-economy section, it applies only to intermediate messages, and it
is not counted in any before/after figure — see §I.

---

## H. Subtask / short-context execution — implemented (unproven)

The worker system prompt said flatly *"Do not use the agent runtime's built-in
subagent or task-delegation tools."* That prohibition protects something real —
work produced outside the AO session is outside what AO tracks, reviews and
verifies — but it was broader than the thing it protected, and it forbade the
one use that would keep reading out of the conversation.

It is now narrowed to the work itself: the change, its commits and its report
must come from the AO session; answering a bounded read-only question in a
separate context is explicitly allowed. Attribution is already handled — the
usage pipeline ingests `claude_subagent` transcripts and binds them to the same
session (`domain.UsageSourceClaudeSubagent`).

No fan-out is enabled and none is encouraged. The Task section says *"Delegate
the QUESTION only"*.

---

## I. Fast Task turn policy — implemented (unproven)

`workflow.turnEconomySection`, appended to the work prompt of
`strategy=task` runs **only**, bound into the durable `PlanArtifact` so a
restart rebuilds the same prompt the worker was actually given. Six habits:
batch related questions, do not survey the repository, wait outside a turn,
verify once, keep intermediate messages short, delegate bounded questions.

Autonomous and Master get none of it and keep their wider advisory profiles: an
open-ended run asked not to explore the repository has been asked not to do its
job.

**No model was changed.** No turn cap is imposed — AO cannot enforce one, and a
hard limit buys a cheap run that stops before it is finished.

**This is not a measured saving and is excluded from every figure in §0.** Its
effect cannot be replayed, because changing the text changes which calls the
agent makes and there is no recording of the run it would have produced. It is
here because it is cheap and true.

---

## J. Budgets of shape — implemented, warn-only

Three new advisory codes in `domain.UsageAdvisoryCode`, all `warn`, none
capable of stopping a dispatch, parking a run or cancelling an attempt:

| code | threshold | fires on wf-1c2cb9bd? |
|---|---|---|
| `context_per_call_above_profile` | Task 150,000 — the measured mean 186,957 rounded down | **yes**, at 186,957 |
| `coordination_turns_dominant` | 50% of classified calls | no — that run was 1% |
| `repeated_wait_check_shape` | 10 wait-class calls | no — that run had 0 |

The last two are documented in the source as **not anchored on a measurement**,
because there was nothing to anchor them on. Saying so is the point: a
threshold presented as derived when it was chosen is worse than one that admits
it.

`context_per_call_above_profile` is also the threshold the lifecycle decision
reads (§E.2), which is the only place in this checkpoint where an advisory
number changes behaviour — and it changes it into COMPACT, which is warn-shaped
too until the policy knob is on.

---

## K. Benchmark — `internal/observe/turnbench`

`testdata/wf-1c2cb9bd.json` is the recorded **shape** of the worker session:
193 entries of context tokens, output tokens, a turn class and a segment label.
It contains no prompt, command, path or message body.

`TestWorkedExampleMatchesTheRecordedLedger` pins the fixture against the
figures AO's own ledger recorded, so the benchmark cannot drift away from what
happened. `TestCompactingAtRepairBoundariesCutsBillableInput` produces the §0
table and **fails below 30%**, so a later change that loses the saving fails in
CI rather than in a bill.

### K.1 The segments — where the money went

| segment | calls | context at start | context at end | cumulative input |
|---|---:|---:|---:|---:|
| work (cycle 0) | 118 | 54,404 | 196,853 | 16,275,887 |
| fix cycle 1 | 32 | 204,146 | 252,350 | 7,315,301 |
| fix cycle 2 | 24 | 257,032 | 294,234 | 6,553,118 |
| fix cycle 3 | 19 | 299,413 | 323,738 | 5,938,510 |

**75 of the 193 calls (38.9%) were repair, and they consumed 19,806,929 of the
36,082,816 billed input (54.9%)** — each one re-reading a base exploration that
had already produced the change under review.

### K.2 Sensitivity

The one quantity a replay cannot observe is the size a conversation resumes at
after compaction, because no call in the recording was ever made against a
compacted one. The §0 figure uses this run's own first-call context (54,404)
plus 3,000 for the summary and fact pack.

| post-compaction context | cumulative input | reduction |
|---:|---:|---:|
| 57,404 | 21,997,829 | **39.0%** |
| 80,000 | 23,692,529 | 34.3% |
| 100,000 | 25,192,529 | 30.2% |
| 120,000 | 26,692,529 | 26.0% |

The saving survives a post-compaction conversation more than twice the assumed
size. `TestTheSavingSurvivesAPessimisticPostCompactionSize` pins a 25% floor
across that range.

### K.3 What the replay cannot say

It cannot say whether the agent would have made the **same calls**. A compacted
agent might need one more turn to re-find a file, or one fewer because it is
not re-reading its own dead ends. The replay holds the call count, the order,
the outputs and the classes fixed and recomputes only how much conversation
each call carries. Wherever the figure appears it is labelled *replay-derived*.

---

## L. Regressions

| # | requirement | covered by |
|---|---|---|
| 1 | long-running command does not produce model polling | `turnbench`: `TestWorkedExampleWasAlmostEntirelyWork` (0 wait turns); advisory `repeated_wait_check_shape` |
| 2 | completion wakes correctly | unchanged; existing `cancel_wake_test.go`, `wake_integration_test.go` |
| 3 | timeout still works | unchanged; no timeout path touched |
| 4 | cancellation still works | unchanged; `cancel_wake_test.go` |
| 5 | recovery after a crash keeps the checkpoint | `TestCompactionIsRequestedAtMostOncePerCycle`; `TestFixLifecycleSurvivesRestartNoDuplicateDispatch` |
| 6 | `waiting_input` is not confused with a process wait | unchanged (P5/P6 corroboration window, `last_signal_at`) |
| 7 | review receives enough evidence | `TestFixCycleIsDeliveredWhateverHappensToTheCompaction` asserts the fact pack survives |
| 8 | Verify receives enough evidence | unchanged; verify reads durable rows, not the conversation |
| 9 | large tool output does not disappear | nothing compresses tool output (§F) — the property holds by not being touched |
| 10 | compacted context can reopen evidence | `SessionContextPack` carries the facts; the transcript is still on disk and still tailed |
| 11 | a small Task does not use the planner | existing `p1a_execution_strategy_test.go:215`, `p1e_integrated_test.go` §D |
| 12 | budgets stay warn-only | `domain.UsageAdvisorySeverity` — every new code is `warn`; nothing reads them to act |

New tests added by this checkpoint:

- `internal/observe/usage/turn_class_test.go` — classification, accumulation
  across records, batch-boundary survival, and that no command text can reach
  parser state.
- `internal/storage/sqlite/store/usage_turn_class_store_test.go` — refinement
  lands on the row, is never lowered, defaults to unclassified.
- `internal/workflow/session_compaction_test.go` — pressure escalates to
  COMPACT; unobserved does not; policy-off does not; the fix cycle is delivered
  whatever happens; at most one request per cycle.
- `internal/workflow/turn_economy_prompt_test.go` — Task only, appended not
  rewritten, and the habits are named.
- `internal/observe/turnbench/turnbench_test.go` — the before/after, with a 30%
  gate and a sensitivity floor.

---

## Validation actually run

| gate | result |
|---|---|
| `go build ./...` | ok |
| `go vet ./...` | ok, no findings |
| `go test ./... -short` | ok, no failures |
| `go test ./internal/workflow/...` | ok |
| `go test -race` on `observe/usage`, `observe/turnbench`, `domain`, `service/usage` | ok |
| `golangci-lint v2.12.2` on every touched package | **17 findings after the fix, every one pre-existing.** The first pass reported 18: seventeen in files this branch never opened, plus one that was mine (a missing doc comment on an exported threshold accessor), now fixed. The only remaining finding in a file this branch touched is `workflow.go:1379`, which is the same line that sat at `workflow.go:1363` before -- my additions moved it, the diff never touches it. Lint delta: **0**. |
| `npm run api` + `openapi-typescript@7.4.4` | both artifacts regenerated and committed |

**Not run, and why.** The wide `go test -race ./internal/workflow/...` was not
run: the host had a live AO with active sessions throughout, and the standing
rule here is that heavy race/Vitest/Playwright gates are not run beside one and
AO is not killed to make room. The changes to that package add no concurrency —
the compaction request happens inside the existing outbox-claimed, single-flight
fix dispatch, behind the same claim that already serialises it — so this is a
deferred gate, not a skipped risk. Frontend Vitest was not run for the same
reason; the frontend change is three keys in a `Record<string, string>` and
three strings per locale, with no type surface and no render logic.

The FIRST lint pass of this work reported nothing and exited 0. It was not
clean: that is golangci-lint's known behaviour when another run holds its lock
-- it prints zero findings and reads as green. Re-run serially, it produced the
eighteen. Any lint result in this repo that is both empty and fast should be
re-run before it is believed.

`npm run api` exits 0 even when its `api:ts` half fails to find the binary, so
`frontend/src/api/schema.ts` was regenerated explicitly with the pinned
`openapi-typescript@7.4.4` and both generated files were verified to have moved.

## Deliberate omissions

- **The turn mix is on the API and not yet on screen.** `trajectory.turns`
  carries the classification (work / coordination / per-class counts /
  unclassified share) and the three new advisories render with real labels in
  all eight locales, but no new UI block was added: the host had a live AO with
  active sessions throughout this work, and the frontend gates are not run
  beside one. Rendering it is a follow-up with its own demo.
- **No `ao ctx snapshot` batching command** — §D says why, with the number.
- **No out-of-band tool-result capture** — §F says why, with the number.
- **No `ao wait`** — §C says why: the wait was already outside the model.
- **No model was changed** and no turn cap was imposed — §I.

## Residual risks

1. **`/compact` has never been exercised by AO against a live session.** The
   transport, the submission confirmation and the durable record are tested;
   the harness's response to the directive is not. This is why the policy knob
   defaults off.
2. **A compaction is a harness summarising itself.** If it drops something AO's
   fact pack does not carry, the repair cycle is worse informed than it was.
   The pack carries objective, criteria, findings, changed files, tests and
   next action; anything outside that list is at the harness's discretion.
3. **The 39% is replay-derived.** §K.3 states exactly what it holds fixed.
4. **The call count is untouched.** Criterion A was not met and no mechanism
   here attacks it, because AO cannot: 99% of the measured run's calls were
   work, and how many calls a piece of work takes is decided inside the harness.
5. **Turn classification is Claude-only.** A Codex rollout's envelope does not
   expose per-call tool blocks, so those events stay unclassified. Read models
   report the unclassified share rather than absorbing it.

## Recommendation

Integrate. The telemetry (§B, §J) and the benchmark (§K) are inert observation
and can ship on. The lifecycle change (§E) ships with its act switched off, so
the only behaviour that changes by default is that a repair cycle into a large
conversation now reaches COMPACT — and COMPACT with the knob off is exactly
what it was before: a fact pack prepended to the prompt.

Then turn `sessionCompactionEnabled` on for one real Task, read the trajectory
block, and argue about the default from a measurement rather than from this
document.
