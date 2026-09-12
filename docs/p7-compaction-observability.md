# Compaction observability and cost accounting — P7.1

**Measurement only. No decision changed.** Nothing in this checkpoint alters
when AO compacts, what `DecideSessionLifecycle` returns,
`maybeCompactBeforeFix`, any threshold, `sessionCompactionEnabled`, the fix
cycle policy, or any dispatch. The compaction path does not read one line of
what is added here.

| | |
|---|---|
| IMPLEMENTED (the act) | yes (P7) |
| INTEGRATED | yes (P7) |
| MECHANISM-PROVEN | yes (`wf-66f0ee54`) |
| PRODUCTION-SAVINGS-PROVEN | **no** |
| BEHAVIOR CHANGED by P7.1 | **no** |

---

## 1. The defect

P7's economics were argued from `model_usage_events`. On the only run that has
ever compacted — `wf-66f0ee54`, session `ao-canary-fixture-6` — the ledger holds
**28 events and not one of them is a summarization turn.**

A compaction is a model call. It re-reads the whole conversation and it
*generates* a summary, at output prices. Claude Code does not write an assistant
record for it, so the usage parser never sees it, so the ledger never bills it.
Every cost figure built on the ledger alone understates a compacting run — and
only a compacting run, which is exactly the shape of error that makes a
comparison look favourable.

The evidence was already on disk, in the file the usage pipeline was already
tailing:

```
system / compact_boundary
  trigger manual   preTokens 85,847   postTokens 10,956   durationMs 95,073
  trigger manual   preTokens 67,663   postTokens 13,240   durationMs 87,393
```

and, once per session:

```
cost-state   totalCostUSD 3.4077755
  claude-opus-5[1m]           in 6,116  cacheRead 2,083,095  cacheWrite 115,415  out 47,453  thinking 6,326
  claude-haiku-4-5-20251001   in 4,758  out 33
```

## 2. What P7.1 reads, and what it refuses to read

**Captured** (`system` / `compact_boundary`): record uuid, timestamp, trigger
(closed enum), `preTokens`, `postTokens`, `cumulativeDroppedTokens`,
`durationMs`, and the model the session was running. **Captured** (`cost-state`):
per-model `inputTokens` / `cacheReadInputTokens` / `cacheCreationInputTokens` /
`outputTokens` / `thinkingTokens` / `costUSD`, plus the session total and
`hasUnknownModelCost`.

**Not captured**, and not declarable by the structs that do the decoding: the
summary text, the conversation, `content`, `preservedSegment`,
`preservedMessages`, the slug, the cwd, the git branch, prompts, commands,
paths. `TestCompactionObservationsCarryNoContent` asserts it against a fixture
that deliberately contains all of them.

**Per-compaction summary output tokens are NOT captured, because the harness
does not report them.** Counting the characters of the summary would mean
reading the summary. The session-level residual is the measured answer instead,
and §4 shows it is a sharp one.

## 3. Where it is stored — no migration

Observations ride in `usage_sources.parser_state_json`, the parser's own durable
per-artifact state, as an additive, versioned, **bounded** section (32
boundaries kept, the count keeps rising past that; 16 rollup models). The read
path needs no new query either: `ListUsageBindingsForSession`,
`ListUsageSourcesForBinding` and `ListUsageModelAggregates` all predate this
checkpoint.

**No migration. No new table. No new column. No `sqlc` regeneration.** A source
written by a build that had never heard of P7.1 decodes as "observed nothing"
(`TestStateFromABuildWithoutObservationsReadsBackEmpty`). The one asymmetry
worth naming: the strict ingest decoder uses `DisallowUnknownFields`, so a
**downgrade** to a pre-P7.1 binary would reject a state this one wrote, and that
source would go to `failed` until its state was cleared. That is the standing
behaviour of every additive parser-state change in this package, and it is the
reason `parserStateVersion` is still 1.

## 4. What the measurement says

Two sessions, and they are a matched pair: one compacted twice, one never
compacted. Every figure is measured.

| | canary `wf-66f0ee54` | control `wf-1c2cb9bd` |
|---|---:|---:|
| calls attributed | 28 | 193 |
| compactions | **2** | **0** |
| AO-attributed input | 1,829,810 | 36,082,816 |
| AO-attributed output | 30,500 | 139,003 |
| harness-reported output | 47,486 | 139,124 |
| **unattributed output** | **16,986** | **121** |
| unattributed output share | **35.8%** | **0.09%** |
| unattributed cache read | 363,625 | 846,874 |
| AO calculated cost (`claude-opus-5` rates) | $2.3118 | $23.2127 |
| harness-reported cost | $3.4078 | $24.8125 |
| **AO accounts for** | **67.8%** | **93.6%** |

**The output residual is the discriminator.** 121 unattributed output tokens
across 193 calls on the run that never compacted, against 16,986 across 28 calls
on the run that compacted twice. Input-side noise is comparable on both; the
generated half is not. That is the summarization, it is real, and AO was not
counting it.

The two compactions also cost **182 seconds of wall clock** — not money, and
deliberately not converted into any.

## 5. Pricing

Unchanged. `pricing.Table` already carries all four dimensions AO needs
(`InputPerMTok`, `OutputPerMTok`, `CacheReadPerMTok`, `CacheWritePerMTok`), and
P7.1 adds no rate, no multiplier and no fallback. Nothing is hardcoded to Opus 5
and no ratio is baked in anywhere.

It did surface a real gap. **The harness rolls a session up under
`claude-opus-5[1m]` even when its own per-call records say `claude-opus-5`,** and
the embedded catalog covers only the second. So:

- matching the residual against the attributed side strips a trailing bracketed
  annotation, so `claude-opus-5[1m]` is recognised as the same model and the gap
  is a gap rather than the whole rollup;
- **pricing never strips it.** A long-context variant is exactly the kind of
  model that does not cost what the base model costs — the control run's own
  numbers put the harness ~4.8% above base rates — so substituting the base rate
  would be inventing a price.

The production result is therefore: **the residual's tokens are exact and its
cost is unknown**, with `claude-opus-5[1m]` named in `UnpricedModels`. One row in
`~/.ao/usage-pricing.json` fixes it, and that is the documented, supported path.
`TestTheRealResidualIsUnpricedInProduction` pins the behaviour;
`TestARateCardThatCoversTheVariantPricesTheResidual` pins the fix.

**P7.1 does not add that row.** Deciding what a long-context variant costs is a
pricing decision, not an observability one.

## 6. turnbench: token savings and cost savings, never one number

`Measure` still answers only token questions and is unchanged in what it
reported before. Cost now lives in `cost.go`, behind three separate refusals:

| refusal | when |
|---|---|
| `no_pricer` | no rate card supplied |
| `no_model` | the series does not say what it ran on |
| `split_unknown` | the calls do not partition context into uncached / read / write |
| `unpriced_model` | no rate covers the series' model |
| `unpriced_unattributed_model` | the calls priced; the measured residual did not |
| `summary_output_unknown` | the series compacted and nobody measured what it generated |

There is **no partial money figure and no zero**: a refused cost reports nothing
while every token figure survives intact.

The compaction half of a cost carries its own basis — `measured_residual`
(the harness's own rollup minus what AO attributed) or `modelled` (a replay,
which has no residual because the run never happened) — so a reader comparing
two scenarios can see that one side is evidence and the other is arithmetic.

`Report(before, after)` renders the whole table with `TOKEN SAVINGS` and
`COST SAVINGS` on separate labelled lines. `TestTokenSavingsAreNotCostSavings`
fails if they ever converge.

### The corrected worked example

Replaying `wf-1c2cb9bd` at its repair boundaries, priced (see the test for the
two stated assumptions — post-compaction write 31,395 and 8,477 generated tokens
per compaction, the latter being the canary's own measured residual per
compaction):

| | ANTES | DESPUES |
|---|---:|---:|
| cumulative input | 36,082,816 | 22,097,081 |
| uncached input | 386 | 386 |
| cache read | 35,787,742 | 21,714,098 |
| **cache write** | **294,688** | **382,597** |
| output | 139,003 | 139,003 |
| compactions | 0 | 3 |
| compaction pre tokens | 0 | 744,264 |
| compaction summary output | 0 | 25,431 |
| cost — calls | $23.2127 | $16.7253 |
| cost — compactions | $0.4669 *(measured)* | $1.0079 *(modelled)* |
| **cost — total** | **$23.6796** | **$17.7332** |

> **TOKEN SAVINGS 38.8 %**
> **COST SAVINGS 25.1 %**

`docs/p7-turn-economy.md` §0 reports the first of those two numbers and says
"cumulative billed input". It is correct and it is not money. Roughly a third of
the headline disappears when the written half and the summaries are priced.

## 7. Files

| file | what |
|---|---|
| `backend/internal/domain/usage_compaction.go` | **new.** `CompactionBoundary`, `HarnessSessionTotals`, `BuildCompactionAccounting` — all pure. |
| `backend/internal/domain/usage_compaction_test.go` | **new.** The canary and the control as fixtures. |
| `backend/internal/observe/usage/parser.go` | parses `compact_boundary` and `cost-state`; bounded, idempotent, content-free state. |
| `backend/internal/observe/usage/compaction.go` | **new.** Extractor, merge, and the `CompactionReader` read model. |
| `backend/internal/observe/usage/compaction_test.go` | **new.** Parse, bounds, idempotency, leak test, read model. |
| `backend/internal/observe/turnbench/turnbench.go` | `Call` gains the billed split; `Series` gains `Compactions` and `Unattributed`; `Apply` rebills. |
| `backend/internal/observe/turnbench/cost.go` | **new.** The cost fold, the refusals, `Report`. |
| `backend/internal/observe/turnbench/cost_test.go` | **new.** |
| `backend/internal/observe/turnbench/testdata/wf-66f0ee54.json` | **new fixture.** Provenance in the testdata README. |
| `backend/internal/observe/turnbench/testdata/wf-1c2cb9bd-split.json` | **new fixture,** derived. Provenance in the testdata README. |
| `backend/internal/observe/turnbench/testdata/wf-1c2cb9bd.json` | **untouched.** |
| `backend/internal/observe/turnbench/testdata/README.md` | **new.** Where every fixture came from, including one documented segmentation difference. |

## 8. Deliberate omissions

- **No API, DTO, OpenAPI or frontend surface.** Nothing regenerates
  `schema.ts`, nothing adds a locale string. The accounting is reachable
  through `usage.NewCompactionReader`; putting it on screen is a separate
  change with its own demo, and adding it here would have made a
  measurement-only checkpoint touch the API contract.
- **No rate card row for `claude-opus-5[1m]`** — §5.
- **No per-compaction cost split.** The harness reports its spend once per
  session; dividing it by the number of compactions would be a division with no
  basis.
- **No economic gate.** Deciding whether a compaction pays for itself is a
  separate checkpoint (P7-E) and stays a design until this measurement has run
  against something that is not the canary.

## 9. Risks

1. **The residual is a session total, not a compaction total.** It contains
   retries and anything else the harness did outside an assistant record. The
   control bounds that noise on the input side and shows it is near zero on the
   output side, but it is not zero, and a per-compaction figure must not be
   derived from it.
2. **`cost-state` is written once, at the end of a session.** A running session
   has no rollup, so the residual is unavailable while it would be most useful.
   `Observed=false` says so; it must never be read as zero.
3. **Both figures come from the harness.** AO is checking its own ledger against
   another program's self-report, not against a bill. The `AttributedCostShare`
   comparison is a share, never a difference to be spent.
4. **Downgrade.** §3: a pre-P7.1 binary rejects the state this one writes.
5. **Two sessions.** The whole population is one compacting run and one control.
   Every figure in §4 is measured and none of them is a sample.
6. **The `[1m]` spelling may not be stable.** If the harness changes how it
   labels a rollup, the canonical-key match degrades to "unmatched model", which
   reports the whole rollup as unattributed. That fails loudly (a residual the
   size of the session) rather than quietly, which is the right direction, but
   it is a coupling to a harness detail.
