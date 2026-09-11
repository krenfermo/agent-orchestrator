# Context growth and cost control for Tasks — measurement and design

**Status: design, not implemented.** Phase 1 of this branch shipped the liveness
fix (`docs`-free, see `0164_session_last_signal.sql` and `Activity.Liveness()`).
Everything below is measurement and a proposal. Nothing here is built, and two
things are deliberately *not* proposed — see "What this does not propose".

## 1. The measurement

One worked example, taken from a real run rather than a synthetic one:
`wf-1c2cb9bd-6380-4491-918c-37e34b5f229c` (project `medusa`, 2026-09-09), a
single-defect Task — "show the entity name in the biometric browser". It ran
`plan` + `work` to completion and its work step produced seven changed files
across two repositories plus two test suites.

Source: `model_usage_events` for the worker's usage binding (AO's own ledger),
cross-checked against the harness transcript.

| | |
|---|---|
| Model invocations (work step) | **193** |
| Uncached input | 386 tok |
| **Cache read** | **35,787,742 tok (98.8%)** |
| Cache write | 294,688 tok |
| Output | 139,003 tok |
| Total | **36,221,819 tok** |
| Cost at the committed rate card | **$23.21** |
| Wall clock | ~50 min |

### 1.1 Where the tokens actually go

The total is not a leak, a loop or a retry storm. Every one of the 193 calls was
a distinct command; there were no duplicate invocations and no compaction event.
The total is the arithmetic of an agentic loop:

```
billable input  ≈  Σ context(i) for i in 1..N
```

with `N = 193` and `context` growing from 54,402 to 323,736 tokens. The
re-sent-context sum computed from the transcript is 36.08M, which is the
ledger's 36.2M. **98.8% of the bill is the model re-reading a conversation that
never shrinks.**

Growth per call: mean 1,403 tok, median 823. Context doubled 4 minutes in
(17:14:34) and tripled at 14 minutes (17:24:07).

The two levers are therefore `N` and the growth rate, and only those two. No
pricing change and no accounting change moves this number.

### 1.2 Initial context, by source

The first call was 54,402 tokens, split by the provider itself into 26,009 read
from an already-warm cache prefix and 28,393 newly written. That split is the
natural seam:

| Component | Tokens | How obtained |
|---|---:|---|
| Stable cached prefix — harness system prompt, tool JSON schemas, MCP server instructions | **26,009** | provider-reported `cache_read` on call #1 |
| — of which AO can name today | — | *nothing* |
| Session-specific, newly written | **28,393** | provider-reported `cache_creation` on call #1 |
| — project `CLAUDE.md` | ~7,048 | transcript `instructions` attachment, 28,192 B |
| — skill listing | ~2,326 | `skill_listing` attachment, 9,304 B |
| — AO's injected system prompt | ~2,024 | `~/.ao/data/prompts/<session>/system.md`, 8,095 B |
| — AO's objective prompt | ~2,124 | first user message, 8,498 B |
| — deferred tool list | ~779 | `deferred_tools_delta`, 3,116 B |
| — agent listing | ~700 | `agent_listing_delta`, 2,800 B |
| — MCP instructions | ~285 | `mcp_instructions_delta`, 1,142 B |
| — session context / date / env | ~380 | attachments |
| — unattributed remainder | ~12,700 | — |

**The single most important finding of this section is a gap, not a number.**
Roughly half the initial context — the harness's own system prompt and its tool
schemas — is invisible to AO. It is not in the transcript and no provider
reports it as a breakdown. Byte-count-over-four is also an estimate, not a
tokenizer. Any design that claims a precise per-source attribution is claiming
something AO cannot currently observe, and this document does not.

### 1.3 Growth during the run

Measured from the transcript, over the whole work step:

| Contributor | Tokens |
|---|---:|
| Assistant output (turns back into input on the next call) | 139,003 |
| Tool results (96 results; largest 28 KB, median ~700 B) | ~37,500 |
| Per-turn reminders and env re-injections (99 + 21 attachments) | ~5,100 |

Tool results were *not* the dominant contributor here. The agent's own output
was. That matters for §4: truncating tool output would have bought little on
this run, while reducing the number of turns would have bought a lot.

## 2. What already exists (and must not be duplicated)

AO already has a complete usage accounting. Nothing below proposes a second one.

- **Ledger** — `model_usage_events`, fed by `internal/observe/usage`
  (watcher → parser → ingestor → coordinator), keyed by `usage_bindings` per
  session, with attribution windows for role/cycle/repair.
- **Pricing** — `internal/observe/usage/pricing`, a versioned rate card with an
  explicit source, version and effective date, overridable by
  `usage-pricing.json`. It refuses to invent a cost: an unpriced model yields
  `unknown`, never `$0.00`.
- **Budgets** — `domain.UsageBudgetPolicy` (workflow token/cost, project daily
  token/cost, warn percent, parent scope) evaluated by
  `internal/service/usage/budget.go`, with two properties this design keeps
  verbatim: a hard limit is consulted **only at a safe boundary, before a new
  dispatch**, and a cost ceiling is unenforceable while any model in play is
  unpriced.
- **Reporting** — `ao workflow usage`, `/api/v1/.../usage`, per-role, per-model,
  per-child, family totals, plus an explicit `complete` flag distinguishing "the
  bill" from "a floor".
- **Injected-context budget** — `internal/contextrouter`'s `Budget`
  (compact/expanded/hard cap per role). Note this budgets what **AO assembles
  and injects**, which is a small fraction of the 54k initial context and none
  of the growth. It is not a total-consumption budget and cannot become one.

## 3. The gaps, and the bounded proposal for each

### 3.1 A time budget (does not exist)

`UsageBudgetPolicy` has token and cost ceilings, no wall-clock ceiling. A run
that is cheap per minute but runs for six hours is invisible to it.

**Proposal.** Add two fields to the existing policy, alongside the existing ones,
so there is one policy object and one evaluation site:

```go
// WorkflowWallClockBudget caps elapsed time for the run (and, under
// ParentScope, for the family). Zero means no limit, exactly like the
// token and cost ceilings.
WorkflowWallClockBudget time.Duration `json:"workflowWallClockBudget,omitempty"`
ProjectDailyWallClockBudget time.Duration `json:"projectDailyWallClockBudget,omitempty"`
```

Evaluated in `applyThresholds` beside the other two, producing the same
`BudgetWarning` / `BudgetExhausted` states and the same reason strings
(`workflow_wallclock_budget_warning`, `..._exhausted`). Measured from
`run.CreatedAt` to `now` for a live run, to `run.CompletedAt` for a finished one.

Scope note: Task, Autonomous and Master already share `UsageBudgetPolicy` and
`ParentScope`, so this needs no per-kind plumbing. `EffectiveUsageBudgetPolicy`
is where per-kind defaults would go if they are wanted later; this proposal sets
no defaults, so an unset time budget stays unset.

### 3.2 Growth and no-progress warning (does not exist)

AO can see a run getting expensive but not a run getting expensive *without
getting anywhere*. This is the half the wf-1c2cb9bd operator actually wanted:
the run was healthy, and they had no way to know that except by asking.

**Proposal — an advisory only, never a gate.** A `UsageGrowthSignal` derived at
read time from facts AO already holds, with no new storage:

| Fact | Source (already durable) |
|---|---|
| Context size per call | `model_usage_events.cache_read + cache_write` per event |
| Call rate | event count over a window |
| Spend rate | those events priced by the existing rate card |
| **Progress** | `workflow_checkpoints` since dispatch; `workflow_steps.updated_at`; git evidence in the work step's observation (`obs.HeadSHA`, `Dirty`, `Staged`, `Untracked`) |
| **Liveness** | `sessions.last_signal_at` — shipped in Phase 1 |

The signal fires only on the conjunction: *context growing* **and** *no durable
progress fact since dispatch* **and** *the session is alive*. All three matter.
Growth alone is normal (it is what an agentic loop does). Absence of progress
alone is normal early in a step. Liveness is what separates "working hard on
something" from "wedged". On wf-1c2cb9bd the conjunction would **not** have
fired: git evidence appeared at 17:15:22, five minutes in.

Surface: the existing advisor (`internal/workflow/advice.go`) and the run
detail — the same places the budget warning already speaks from. It changes what
AO *says*, never what AO *does*.

### 3.3 Per-source context attribution (partially impossible today)

§1.2 is the honest state: AO can attribute the session-specific half of the
initial context from harness artefacts it can already read, and cannot attribute
the cached-prefix half at all.

**Proposal, in two steps, the second gated on evidence.**

1. *Attribute what is readable.* A one-shot measurement at dispatch of the
   artefacts AO itself controls and can size exactly: the injected system prompt
   (`~/.ao/data/prompts/<session>/system.md`), the objective prompt, and the
   project `CLAUDE.md` AO knows the path of. Report bytes and an explicitly
   labelled token *estimate*. This is worth having because it is the half AO can
   act on: it is AO's own injection.
2. *Do not guess the rest.* Reporting a fabricated split of the cached prefix
   would be the exact failure the pricing package refuses to commit
   ("tokens sí, cost = unknown. No inventar"). If a provider later exposes a
   system-prompt/tool-schema token count, or a first-party token-counting
   endpoint is available, step 2 becomes real. Until then it stays labelled
   "not observable", with the reason, in the same style the usage report already
   uses for an unmeterable role.

### 3.4 Per-run / per-step token and cost breakdown

Already exists per **run** and per **role**. It does not exist per **step**.

**Proposal.** The attribution windows already carry role/cycle/repair; adding
`workflow_step_id` to `usage_event_attribution` (or deriving it from the
attribution window's dispatch instant, which is a step boundary) yields a
per-step breakdown with no new ingestion path and no new accounting. This is the
smallest change that answers "which step cost the $23".

### 3.5 Context reduction

The measurement says the levers are `N` (number of turns) and growth rate, in
that order, and that tool output was not the problem on this run. So:

- **Reduce N, not evidence.** The 193 calls include long polling sequences
  (a `sleep 45`, then a check, repeatedly, while a test suite ran). A harness
  primitive that blocks on a background job instead of polling would have removed
  a double-digit number of calls, each costing a full context re-read (~200k tok,
  ~$0.10) for a single line of output.
- **Prune AO's own injection, which AO owns.** 7k tokens of project `CLAUDE.md`
  ride in every call for the life of the session — 193 × 7k ≈ 1.35M tokens,
  ~$0.68 on this run alone, for text that is mostly irrelevant to any one step.
  The context router already knows how to select and cap; pointing it at the
  guidance document is in-mechanism work, not a new mechanism.
- **Never truncate tool results or drop instructions.** Both destroy the evidence
  a review step depends on, for ~37k tokens on a 36M-token run. The ratio does
  not justify it.

## 3.6 Related: the Board's own "Last activity" (design, not implemented)

The operator report that started this work said "last activity 12 minutes ago"
about a run whose worker was making six model calls a minute. Phase 1 fixed the
SESSION clock. The number on the Board card is a different field, and it was not
wrong by its own definition — it is `lastMeaningfulActivityAt`, the timestamp of
the newest event on the run's timeline, and the newest event was
`worker_launched` at 17:10:18Z. Twelve minutes later, that was still the newest
thing the *workflow* had durably done, because a work step in progress writes no
checkpoints.

So the run-level number is a transition clock too, for the same structural
reason, and `Lifecycle.LastActivityAt` carries an explicit invariant this branch
did not touch: it is "deliberately NOT the worker session's activity timestamp —
an idle worker during a review is not an idle workflow."

**Proposal.** Do not redefine either field. Add a third, `workerLastSignalAt`,
sourced from the running step's session `Activity.Liveness()`, so the card can
say "worker heard from 4s ago" beside "workflow last advanced 12m ago". That
respects the invariant — worker liveness only ever moves a separate number
forward, and an idle worker contributes nothing to it — while answering the
question the operator actually had.

This is not implemented here because it needs a session read on the Board's
polling path, which `RunDetail` does not carry today. That is a real cost on a
hot endpoint and deserves its own decision.

## 4. What this does not propose

Two things were considered and are deliberately excluded, because neither can be
shown safe with what is in the repository today.

- **Automatic cancellation on cost.** The existing budget already refuses to kill
  a provider mid-response, for a stated reason: a run interrupted between "the
  model is generating" and "AO recorded an attempt" is precisely the ambiguous
  lifecycle the workflow package is built to avoid. A cost-triggered kill would
  reintroduce it. The safe-boundary gate that already exists is the correct
  shape, and it is enough.
- **Forced compaction.** AO does not drive the harness's context window and has
  no durable representation of a compaction boundary. Forcing one would mean
  discarding conversation state the workflow cannot reconstruct, with no way to
  prove the run can recover from it. This stays out until (a) the harness exposes
  a compaction primitive AO can invoke and observe, and (b) a work step can be
  shown to resume correctly across one.

## 5. Tests this design would need

Named here so the proposal is costed, not because they exist.

- Time budget: unset stays unset; warn at the percent boundary; exhausted only
  at a safe boundary; parent scope covers the family exactly once.
- Growth signal: fires on the three-way conjunction; stays silent when durable
  progress exists (the wf-1c2cb9bd shape); stays silent when the session is not
  alive (that is the liveness/reconcile path's job, not this one).
- Per-step attribution: an event lands in exactly one step; a repair cycle's
  events do not leak into the base step's total.
- All of it against **usage fixtures and synthetic transcripts**. No paid model
  invocation, and no re-run of the smoke procedure.
