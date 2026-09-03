# Usage accounting: tokens, cost, context (P3-E)

AO answers four questions about what a run cost, and it keeps them apart on
purpose, because they are four different measurements with four different
warranties.

| Question | Where it comes from | What it may claim |
| --- | --- | --- |
| How many tokens did a provider report? | `model_usage_events`, parsed from the provider's own transcript | `provider_reported` — printed bare |
| What did that cost? | a named rate card in `internal/observe/usage/pricing` | `calculated`, a list-price equivalent — or `unknown` |
| How much context did AO assemble? | the P2 baseline evidence tree | `estimated` from measured bytes, never a provider tokenizer |
| How much context did AO avoid assembling? | memory dedupe + router candidates not sent | only where a comparable baseline exists |

The one rule that governs all of them: **a number never travels without the
claim it is entitled to make.** An estimate is marked. An unknown is "unknown",
never zero. A missing measurement is an absence, never a zero.

## The ledger

`model_usage_events` (migration 0052) was already exactly-once: each row is keyed
`UNIQUE (binding_id, source_event_key)`, and the key is derived from the
artifact rather than from a clock, so re-reading a transcript re-derives the same
key and the insert is a no-op. A durable byte cursor plus parser state make the
read itself restart-safe.

P3-E added two nullable columns (migration 0149):

- `observed_at` — the provider's own event time, when the record carried one.
  **Never** part of `source_event_key`: an identity that moved with a clock would
  stop being exactly-once the moment a transcript was re-read.
- `recorded_at` — when AO ingested the row.

`observed_at` stays NULL rather than being filled with "now" when an artifact
carries no timestamp. A fabricated instant would silently file a token under the
wrong role.

## Role attribution is a time window, not a column

AO deliberately delivers a repair prompt into the **worker's own session**
(`fix_dispatch.go`), so one AO session, one harness and one native root carry both
base execution and every repair cycle — one usage binding for several roles.

Before P3-E the run detail asked the per-session usage reader once per step and
added the answers up, so a run with one worker step and two fix steps counted the
same session's tokens three times. The total was not merely unattributed, it was
inflated.

The fix is `usage_attribution_windows`. When AO hands a role a session it records
the instant; from there until the next window on that session, tokens spent in it
belong to that role. Nothing closes a window — the next opening is the close —
so a crash between "opened" and anything else can neither lose an end nor invent
one.

Resolution happens at **read** time, in the `usage_event_attribution` view: an
event belongs to the newest window opened at or before its `observed_at`, or, if
it has none, to the session's earliest window. Exactly one window per event, so a
total cannot double count; derived rather than stored, so a restart cannot
duplicate it and a corrected window fixes every past total. The fallback case is
reported as `attribution_basis = 'approximate'` and surfaced all the way to the
UI, because a role breakdown built from fallbacks is a weaker claim than one built
from timestamps.

Windows are idempotent on `dedupe_key`, derived from the durable obligation
(run, step, attempt, ordinal, cycle, role, session) and never from the clock — so
a dispatch replayed by failover, by resume after a restart, or by a wake re-opens
the same window instead of splitting a role's tokens across two.

### Query shape matters

The aggregates in `queries/usage_ledger.sql` use a CTE that ends in `LIMIT -1`
followed by a `CROSS JOIN`. Neither is decoration:

- `LIMIT -1` stops SQLite flattening the single-use CTE back into the outer
  query. Flattened, the planner drives from the WINDOWS and re-resolves every
  event of a session once per window of that session — quadratic. Measured at
  10,000 events: 1.2s flattened, 27ms not. (`AS MATERIALIZED` says the same thing
  more clearly, but sqlc's SQLite grammar cannot parse it.)
- `CROSS JOIN` pins the join order so windows are then looked up by integer
  primary key.

`usage_attribution_perf_test.go` is the tripwire, at 1k / 10k / 100k events.

## Every provider-backed role is metered

A binding names a **subject**, not necessarily a session (migration 0150):

| Subject kind | Used by | How its conversation is discovered |
| --- | --- | --- |
| `session` | worker, and the repair cycles delivered into it | the session's own activity hook |
| `runtime_pane` | reviewer (keyed on the **review run**), decision resolver (keyed on the **resolution**) | the pane's own harness hook, forwarded against `AO_USAGE_SUBJECT` |
| `planner_invocation` | one `claude --print` planner call | the print-mode **response envelope** — there is no transcript |
| `provider_attempt` | reserved; nothing emits it yet | — |

`session_id` survives as a real, nullable, foreign-keyed column, so every
session-scoped read, index and CDC trigger keeps meaning exactly what it meant.
The rebuild copied every row and preserved every id.

### The planner is not in-process

`adapters/planner/command` shells out to
`claude --print --output-format json … --no-session-persistence`. That is a real
Anthropic call on every objective AO plans. The last flag is why the
transcript pipeline can never see it: **there is no transcript.** Its spend is
stated once, in the envelope the adapter already parses, and is recorded against
a `planner_invocation` subject — on **both** outcomes, because a planner call
that timed out after generating most of a plan is billed like one that
succeeded.

That is also why `model_usage_events.usage_source_id` is nullable: NULL means
"reported in a response, not read from an artifact". Exactly-once is unaffected —
it has always lived in `UNIQUE (binding_id, source_event_key)`, never in the
source reference.

### Why panes do not report through the session path

`RecordHook` is session-shaped in two ways that are actively wrong for a pane,
and both were live hazards:

1. It **overwrites the signal's harness with the session's**. A Codex reviewer
   beside a Claude worker would have had its rollout registered as a
   `claude_main` artifact — wrong parser, wrong root.
2. On a finalizing event it calls `finalizeSession`, which finalizes **every**
   binding of that session. A reviewer's own `session-end` would have cut the
   **worker's** ingestion short, mid-run.

So a pane meters itself through `RecordSubjectHook`: its harness comes from its
own hook, and it finalizes only its own subject. `POST /usage/subject-hook` is
deliberately the narrowest route in the daemon — usage only, no activity state,
no session id, and a session subject is refused outright.

### A resolver could never launch on a worktree-placed run

Proving the Decision Resolver's usage for real surfaced a genuine P3-C defect,
fixed here rather than worked around.

`dispatchDecisionResolver` read the step's **latest** checkpoint to find the
workspace to hand the resolver. A step's checkpoints are append-only and several
phases legitimately record no workspace — `worker_blocked` is one, and it is
exactly the phase a step is in when its worker pauses on a question. So dispatch
read a blank worktree path and parked on
`waiting_for_capacity: no worktree recorded yet`, for a checkout AO had already
written down twice on the same step. The wake fired, re-read the same blank row,
and the question sat in `resolving` forever.

Direct-branch runs were unaffected, because `placementWorkspacePath` answers for
them — which is why it went unnoticed.

`resolverWorkspaceForQuestion` now takes the latest checkpoint **that actually
carries a workspace**, scoped to that step. It changes no question semantics and
invents nothing: a step that never recorded a workspace still yields none, and
the placement fallback and capacity-wait behave exactly as before.

### Lower bound vs whole bill

`complete: true` means every provider-backed role this run dispatched has
reported. `complete: false` names the roles still pending, and the totals are
then a floor. A role listed as unobservable is **awaiting a provider report**,
not architecturally unmeasurable — and never zero.

## Cost

`internal/observe/usage/pricing` owns rates, because a rate hidden in a React
component is a number nobody can audit, version, or override.

- The embedded catalog holds **Anthropic** rates only, with input/output at
  published list price and cache rates derived by the published 5-minute cache
  multipliers (0.1x read, 1.25x write). `Source` names both halves.
- There is deliberately **no** OpenAI/Codex entry. AO meters Codex tokens exactly
  as it meters Claude's, but this binary has no rate it can vouch for, so those
  tokens report `cost: unknown` until an operator supplies one.
- An operator rate card at `<AO_DATA_DIR>/usage-pricing.json` layers over the
  embedded catalog and **replaces the provenance wholesale**: a cost computed
  partly from an operator's rates is not "Anthropic list price".

A calculated cost is a **list-price equivalent of the tokens spent, not a bill**.
AO cannot see how a provider actually charges the account behind a harness — a
subscription may make the marginal cost of those tokens nothing at all. That is
what `domain.UsageChannel` is for, and it stays `unknown` until a real billing
signal exists.

```json
{
  "source": "acme-negotiated-rates",
  "version": "2026-09",
  "effectiveDate": "2026-09-01",
  "currency": "USD",
  "models": [
    {"match": "gpt-5-codex", "provider": "openai",
     "inputPerMTok": 2.0, "outputPerMTok": 8.0,
     "cacheReadPerMTok": 0.2, "cacheWritePerMTok": 2.5}
  ]
}
```

## Budgets

`WorkflowPolicy.Usage` is frozen into the run's policy snapshot at creation, for
the same reason every other policy is: a later Settings change must not widen
what an in-flight run may spend, and a restart must reach the same verdict.

- A warning is **advice**. It changes what the detail page and the advisor say and
  nothing else.
- A hard limit is a gate, consulted only at **safe boundaries** — before a worker
  dispatch, before a repair cycle, before a new autonomous child task. Nothing
  terminates a running attempt: a provider killed mid-answer leaves an attempt
  whose mutation state nobody can prove, and a budget is not worth reintroducing
  that for. An over-budget run finishes what it started and starts nothing more.
- A parent's ceiling covers its **whole family** by default. Ten children each
  holding the parent's budget is the failure the default exists to prevent.
- A budget AO cannot measure does not block: no ledger, a failed read, or a cost
  ceiling whose models have no rate all report "unmeasurable" and let the dispatch
  proceed. Refusing to dispatch on a number AO does not have would stop real work
  for a fiction. A **partial** cost may warn but may never exhaust.
- Unset is not zero. `BudgetUnset` is a distinct state, and the UI draws no meter
  for it.

## AO-assembled context

Read from the P2 baseline evidence tree (opt in with
`AO_PROJECT_MEMORY_BASELINE=1`). It reports what AO built and sent, broken down by
role and by source, together with what project memory contributed.

It is **never added to the provider token figures and never compared with them as
equals.** AO does not observe what a coding harness reads inside the worktree
after the prompt lands, and it runs no provider tokenizer. On a real smoke run the
two figures were ~400 estimated AO-assembled tokens against 158,208
provider-reported ones; treating them as one quantity would be nonsense.

A "saving" may only be claimed where a comparable baseline exists — memory that
demonstrably replaced an equivalent document, or router candidates assembled and
then not sent. The label is **"estimated AO context avoided"**, never "the
provider billed X fewer tokens". With no baseline, the UI shows no saving at all,
not zero.

The graph backend is reported by its real name, `LocalGraph`. An external
Graphify/Grae adapter does not exist, and calling LocalGraph "Graphify" would
claim one that does.

## Surfaces

- `GET /api/v1/workflows/{id}/usage` — the canonical run ledger, plus context.
- `GET /api/v1/projects/{id}/usage?range=today|7d|30d|all` — the project rollup,
  bucketed by **dispatch time** (a fact AO recorded itself), which `periodBasis`
  states so nobody reads it as a provider billing period.
- Run detail embeds the same ledger at `usage.tokens`.
- The Board carries one compact line per card, filled from one grouped query for
  the whole project.
- `ao workflow usage <run>` and `ao project usage <project> --range ...`.

**The frontend computes no totals.** Every figure is read straight off the
response, because only the backend knows that a session shared by several roles
must be counted once.
