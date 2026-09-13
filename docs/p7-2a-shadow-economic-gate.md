# Shadow economic gate — design (P7.2A)

**DESIGN ONLY. Nothing implemented, nothing committed, no run started, no model
called.** Shadow mode means AO computes what it *would* have decided and records
it. It governs nothing: not `LifecycleCompact`, not `maybeCompactBeforeFix`, not
the `/compact` directive, not `sessionCompactionEnabled`, not a threshold, not
the fix lifecycle, not Review, not Verify.

Supersedes `docs/p7-compaction-economic-gate.md` where the two disagree. That
document was written before P7.1 measured anything; §1 below lists exactly what
it got wrong.

---

## 1. What P7.1 invalidated in the earlier design

Five corrections, and four of them move the answer in the same direction: **the
earlier design under-priced compaction.**

### 1.1 The cache-write rate is wrong for every Claude session AO meters

The design's central ratio was `Cw/Cr = 12.5`, taken from the embedded
catalog's cache-write rate of $6.25/MTok — which that catalog documents as
derived from "published 5-minute-TTL cache multipliers ... 1.25x write".

**Every cache write in both measured sessions was created at the ONE-HOUR TTL.**
From the transcripts' own `cache_creation` block:

| session | cache writes | at 5m TTL | at 1h TTL |
|---|---:|---:|---:|
| `ao-canary-fixture-6` | 110,284 | 0 | **110,284 (100%)** |
| `medusa-12` | 294,688 | 0 | **294,688 (100%)** |

The 1-hour write multiplier is 2x input, i.e. **$10.00/MTok for Opus 5, not
$6.25**. This is not an inference. Pricing medusa's own token vector with that
one rate changed and everything else at base reproduces the harness's own
figure exactly:

```
1,488·5.00 + 36,634,616·0.50 + 294,688·10.00 + 139,003·25.00  (per MTok)
  = $24.805628        harness cost-state reports  $24.805628
```

Consequences:

- `Cw/Cr` is **20**, not 12.5. `k = (Co+Cw)/Cr` is **70**, not 62.5.
- AO's own ledger under-prices these sessions by **4.5%** (medusa) and **15.2%**
  (canary) — before any compaction accounting.
- **AO's parser does not decode the TTL split at all.** `claudeTranscriptRecord`
  declares `cache_creation_input_tokens` and not the `cache_creation` object
  beside it, so both TTLs fold into one number and every one of them is priced
  as if it were the cheap kind.

This is a real defect in AO's cost reporting, it is **not** created by P7.1, and
it is **larger in relative terms than the compaction gap on the control run.**
It is not fixed here — see §2 and §17.

### 1.2 The summarization is about twice the size the design assumed

The design estimated `S` (generated tokens) at 4,626 and 4,337 by counting the
characters of the summary text. P7.1 measures it from the harness's own rollup:
**16,986 unattributed output tokens across two compactions**, of which the
non-compaction baseline (§1.5) accounts for at most ~900. So `S ≈ 8,000 per
compaction`, roughly **1.9x** the design's figure. The missing half is thinking
tokens, which the rollup reports separately (6,326 for the canary) and which the
summary text obviously does not contain.

### 1.3 The design's own headline correction was itself understated

It said the canary's true spend was "$0.30 higher than the $2.31 the ledger
reports — a 13% understatement". Measured against the harness's own figure the
gap is **$1.10, or 32%**. The design was wrong about the size of the error it
was written to expose, by 3.6x.

### 1.4 Recomputed, the verdicts hold and the numbers do not

Same formula, corrected rates (`Cw` = $10.00/MTok), measured `S` = 8,050:

| | `N*` design | `N*` corrected | real N | P/L corrected |
|---|---:|---:|---:|---:|
| canary compact 1 | 17.4 | **28.2** | 6 | −$0.325 |
| canary compact 2 | 70.5 | **117.1** | 4 | −$0.431 |
| medusa replay 1 | 3.2 | **5.3** | 29 | +$1.834 |
| medusa replay 2 | 2.4 | **4.0** | 24 | +$2.063 |
| medusa replay 3 | 2.0 | **3.3** | 19 | +$1.948 |

The canary's total loss is **−$0.756**, not the −$0.42 the design reported. The
medusa replay's gain is +$5.84, not +$6.31. **Every verdict is unchanged and
every margin moved against compaction.**

The critical-context table moves with it:

| remaining calls N | B* design | **B\* corrected** |
|---:|---:|---:|
| 5 | 150,600 | **213,000** |
| 8 | — | **151,612** |
| 10 | 99,900 | **131,150** |
| 20 | 74,600 | **90,225** |
| 50 | 59,400 | **65,670** |

So the 150,000 threshold is the break-even context for **≈8 remaining calls**,
not 5. The design's "a much better accident than it looks" observation survives;
the number behind it does not.

### 1.5 "The control" is now a population, not a run

The design had one control. P7.1's residual can be computed for **62 sessions**
that carry a harness rollup:

| unattributed output share | |
|---|---:|
| median | 0.46% |
| p90 | 1.88% |
| max, excluding the canary | 7.23% |
| **`ao-canary-fixture-6` (2 compactions)** | **35.77%** |

The compacting session is **5x the worst non-compacting one and 19x the p90**.
The signal is not a two-point contrast any more.

### 1.6 What the design got right and P7.1 confirms

- The direction and the mechanism: compaction is a purchase, priced in
  cache-writes and output, repaid in cache-reads.
- `A` is roughly independent of `B`.
- The gate must be provider/model aware and must never invent a price.
- The `many_fix_cycles` finding (§7 below sharpens it).

---

## 2. `claude-opus-5[1m]`

**It is a model id, reported by the harness, for the 1M-context variant of
Opus 5.** Not an alias AO invented, not a tier label AO derived: it is the
string Claude Code itself uses to identify the model a session ran on. (This
very session's own model id is `claude-opus-5[1m]`; the harness surfaces it the
same way in its rollup.)

Where it comes from: the per-call `assistant` records say `claude-opus-5`, and
the once-per-session `cost-state` rollup keys the same spend as
`claude-opus-5[1m]`. Two spellings of the same session from the same program.

**What AO can say about its rates, from evidence:** on both measured sessions,
the harness's own cost figure is reproduced by base `claude-opus-5` rates for
input, cache read and output, with the cache write at the 1-hour multiplier.
Medusa matches to six significant figures; the canary to 0.3%, the remainder
consistent with ~2% of its writes at the 5-minute rate. **There is no observed
long-context premium** — medusa ran contexts to 323,738 tokens, well past the
usual 200k tier boundary, and still priced at base.

**That is evidence, not a rate card, and P7.2A adds no rate.**

`pricing.Table` needs no new mechanism to support it: `Rate()` matches an exact
id, so an operator rate card row with `"match": "claude-opus-5[1m]"` prices it
today with zero code change. What does not exist is an **alias** concept — no
`aliasOf` field, no way to say "this is the same billing entity as that" — and
prefix matching cannot reach it because the bracket is not a hyphen.

### The rule

```
resolve the model id the harness reported, exactly
  hit  -> pricingStatus = priced
  miss -> pricingStatus = unpriced_model, verdict = UNKNOWN, action = SKIP
```

No silent normalization, no falling back to the canonical spelling, no
substituting a base model's rate for a variant's. The canonical-key match that
P7.1 uses to line a residual up against an attributed line is a **matching**
device and must never become a **pricing** device; that separation is already
asserted by `TestModelVariantsAreMatchedForTheResidualAndNeverForThePrice` and
P7.2 must not weaken it.

### The prerequisite this exposes

The TTL defect (§1.1) is upstream of everything here. A gate that prices cache
writes at $6.25 when the provider charges $10.00 will systematically overstate
the profit of compacting, because the first post-compaction call is the single
largest cache-write in a session. **Fixing the TTL split is a precondition for
enforcement, not for shadow mode** — in shadow the wrong rate produces a
recorded verdict nobody acts on, and the recorded `pricingVersion` makes the
whole cohort re-computable afterwards.

---

## 3. Second real compaction sample: **unavailable**

Every transcript AO knows about was scanned — 137 of 138 registered sources
still on disk, across 125 runs.

```
transcripts containing a compact_boundary record:  1
  ao-canary-fixture-6 ........ 2 boundaries
```

`SECOND REAL COMPACTION SAMPLE = unavailable.` No run was created, no threshold
lowered, no model called to establish this.

What the same scan *did* find is the 62-session rollup population of §1.5, which
is a materially better baseline than the design had, and 8 usable fix-cycle
samples for §6.

---

## 4. The shadow algorithm

Evaluated where `maybeCompactBeforeFix` already sits, on the same inputs, and
returning a verdict that **nothing reads**.

```
shadowEconomicVerdict(run, step, cycle, sessionID, promptTokens) ->

 0. decision == LifecycleCompact ?            no  -> not evaluated (no record)
 1. reading.Observable ?                      no  -> UNKNOWN / usage_unobservable
 2. rates for reading.ModelID known ?         no  -> UNKNOWN / unpriced_model
 3. stablePrefix P known ?                    no  -> UNKNOWN / prefix_unknown
 4. structural opportunity remains ? (§7)     no  -> SKIP    / no_remaining_cycles
 5. estimate A (§5)                    unavailable -> UNKNOWN / context_after_unknown
 6. estimate S (§5.4)                  unavailable -> UNKNOWN / summary_size_unknown
 7. D = (B + delta) - A ;  D <= 0            -> SKIP / no_reduction
 8. D < minReductionFraction * B             -> SKIP / reduction_too_small
 9. estimate N (§6)                     unavailable -> UNKNOWN / remaining_calls_unknown
10. N* = cost / (D * Cr)                (§11)
11. N >= N* * safetyFactor ?  yes -> COMPACT / economics_positive
                              no  -> SKIP    / insufficient_remaining_calls
```

Every branch writes one telemetry record (§8). **UNKNOWN is a verdict, not an
error**, and it is the one a first deployment should produce most of — a gate
that can price nothing yet is exactly what the first weeks of shadow should
look like.

### Fail-closed direction

Every unknown maps to SKIP-or-UNKNOWN, never to COMPACT. Every estimate that
can be biased is biased toward a **larger** `A`, a **larger** `S`, a **smaller**
`N` and a **smaller** `D` — all four push the verdict away from compacting.

---

## 5. The inputs, and which kind each one is

| input | kind | source | if absent |
|---|---|---|---|
| `B` context before | **OBSERVED** | `SessionContextReading.LastContextTokens` + that call's output | UNKNOWN |
| `P` stable prefix | **OBSERVED** | the session's FIRST placeable call's `cache_read_tokens` (needs a new fold on an existing query) | UNKNOWN |
| `delta` prompt tokens | **OBSERVED** | the fix prompt + pack AO is about to send, in bytes / 4 | 0 (shrinks D) |
| rates | **OBSERVED** | `pricing.Table`, keyed on the model the session reported | UNKNOWN |
| threshold | **OBSERVED** | the run's frozen `UsageBudgetProfile` | n/a |
| cycle, MaxFixCycles, strategy | **OBSERVED** | frozen policy + `cycleCount` | n/a |
| `A` context after | **ESTIMATED** | §5.1 | UNKNOWN |
| `S` generated tokens | **ESTIMATED** | §5.4 | UNKNOWN |
| `N` remaining calls | **ESTIMATED** | §6 | UNKNOWN |

### 5.1 Estimating `A` without look-ahead

Priority order, first available wins:

1. **This session's own previous compaction.** P7.1 records `postTokens` per
   boundary. A session compacting for the second time has a measured
   `A` from its first — the strongest possible prior, and it is the case where
   the design's worst error (compact 2's tiny `D`) lives.
   `A_est = P + lastPostTokens * reattachFactor + delta`.
2. **Per (harness, model) percentile** over completed sessions' boundaries.
   Use a **high** percentile (p75), because a larger `A` is the conservative
   direction.
3. **Nothing** -> `A` is UNKNOWN -> the verdict is UNKNOWN. Not a constant, not
   a guess.

`reattachFactor` accounts for what the harness re-attaches on the first
post-compaction call beyond the summary itself. Measured once, on the canary:
`A = 57,803` against `P + postTokens + delta = 37,379 + 10,956 + 1,181 =
49,516`, so the factor is **1.17x on the post-compaction conversation**, or
+8,287 tokens absolute. One observation. Carried as a named constant with its
provenance, biased upward, and recalibrated the moment there is a second.

**The look-ahead trap, stated so it cannot be walked into.** P7.1 now records
`postTokens` for the very compaction a decision is about. The gate must read
only boundaries whose record predates the decision: filter on cycle strictly
less than the current one, and on `ObservedAt` strictly before the decision
time. The evaluation path (§10) reads the later ones; the gate never does. They
must be different functions taking different inputs, and the gate's input must
be a frozen struct assembled by a caller that has no access to the current
cycle's boundary.

### 5.4 Estimating `S`, and why it can never come from this session

`S` is only measurable from the harness's end-of-session rollup, and that record
is written **once, when the session ends**. A running session has no rollup. So
`S` is structurally a **cross-session prior** — there is no version of this
design in which a session estimates its own summary cost.

1. Per (harness, model) mean of `unattributedOutput / compactionCount` over
   completed sessions, net of the non-compaction baseline (§1.5 gives that
   baseline as p90 = 1.88% of output).
2. Today that population is **one session**: `S = 8,050`.
3. Below a minimum sample (proposed: 3 sessions), use the single observation
   **inflated to the conservative end** of its uncertainty — the residual is an
   upper bound on the summarization anyway, so use it un-netted: `S = 8,493`.
4. No observation at all -> UNKNOWN -> SKIP.

---

## 6. Remaining calls: revalidated against every fix cycle AO has

The design validated its estimator on the two runs it was built from. Rerun
across **every** run in the ledger with a worker phase and at least one fix
cycle — 5 runs, 8 samples, three of them runs the design never saw:

| run | cycle | calls in cycle 0 | calls in cycle | predicted N | real N | abs err | rel err | direction |
|---|---:|---:|---:|---:|---:|---:|---:|---|
| `wf-0aadfcde` | 1 | 85 | 19 | 21 | 18 | 3 | +17% | **OVER** |
| `wf-1c2cb9bd` | 1 | 118 | 30 | 29 | 29 | 0 | 0% | exact |
| `wf-1c2cb9bd` | 2 | 118 | 25 | 24 | 24 | 0 | 0% | exact |
| `wf-1c2cb9bd` | 3 | 118 | 20 | 20 | 19 | 1 | +5% | **OVER** |
| `wf-5b2210f5` | 1 | 27 | 57 | 6 | 56 | 50 | −89% | under |
| `wf-66f0ee54` | 1 | 16 | 7 | 4 | 6 | 2 | −33% | under |
| `wf-66f0ee54` | 2 | 16 | 5 | 5 | 4 | 1 | +25% | **OVER** |
| `wf-88e71ef2` | 1 | 12 | 5 | 3 | 4 | 1 | −25% | under |

```
N = 8      over = 3 (38%)    under = 3    exact = 2
median absolute error 1.0 call      max 50
worst OVERprediction: +25% relative, +3 calls absolute
```

**The error profile is favourable and it is worth being explicit about why.**
Overprediction is the dangerous direction: it authorizes a compaction that will
not amortize. Every overprediction observed is small — at most +3 calls and
+25% relative. The one catastrophic error is an **under**prediction (`wf-5b2210f5`:
6 predicted, 56 real), where a 27-call worker phase was followed by a 57-call
repair. That costs a missed saving and nothing else.

So the estimator is already biased the safe way by accident of shape, not by
design. Two changes make it so on purpose:

- **Cap, do not extend.** `N_est = min(0.25 * calls(cycle 0), p50 of that
  strategy's historical fix-cycle sizes)`. The cap only ever lowers.
- **Never extrapolate across cycles.** Count only the current cycle's calls,
  as the design already specified. A compaction's benefit does persist into
  later cycles, so this understates — deliberately.

`safetyFactor` then has to absorb at most the observed +25%. See §11.

**Eight samples from five runs is still a small population**, and none of them
is an Autonomous or Master run. The estimator's constants stay labelled as
anchored on Task-shaped evidence only.

---

## 7. The last-fix-cycle rule

The design said `many_fix_cycles` "fires at the last cycle". Reading the code,
it is sharper than that and the sharpness matters.

- `cascade.go` derives `cycleCount` from the review runs this session has had,
  so it is the **number of the cycle about to be dispatched** (1 for the first
  repair). Confirmed against the canary's own durable records: `"cycle":1` then
  `"cycle":2`.
- `DecideSessionLifecycle` reaches COMPACT via `many_fix_cycles` when
  `FixCycleCount >= MaxFixCycles`.
- `fixBudgetState.Exhausted()` stops the loop when `Spent >= Budget`.

With the default `MaxFixCycles: 3`, `many_fix_cycles` therefore fires **only
when dispatching cycle 3 — the last cycle that will ever run.** It is not "late",
it is *terminal*: there are structurally zero further cycles into which the
saving could be amortized, and the only calls left are the ones inside that
cycle. `many_attempts` has the identical shape on the attempt axis.

### The shadow rule

```
structuralOpportunity = (MaxFixCycles - cycle) cycles remain after this one
if structuralOpportunity == 0 and the ONLY reason for COMPACT is
   many_fix_cycles or many_attempts:
       verdict = SKIP, reason = no_remaining_cycles
```

Stated as a **structural** test rather than an economic one, because it needs no
estimate at all: it is arithmetic on the frozen policy. It is also the cheapest
possible guard and belongs at step 4 of §4, before anything is priced.

Note what it does **not** say: a terminal cycle with genuine context pressure
and a large `N` can still be economically positive, and the rule above only
fires when the fix-cycle/attempt counters are the sole reason. **Lifecycle is
not modified in P7.2A.** This rule lives entirely inside the shadow verdict.

---

## 8. Break-even, final form

```
                 P + (Co/Cr)·S + (Cw/Cr)·(A − P − delta)
N*  =  ──────────────────────────────────────────────────
                        (B + delta) − A

COMPACT  iff  N_est >= N* * safetyFactor
```

with every rate read from `pricing.Table` for the model the session reported,
and `Cw` being whichever cache-write rate the rate card carries for it. Nothing
is hardcoded: on the embedded catalog `(Co+Cw)/Cr` is 62.5, and against the rate
the provider actually charged these sessions it is 70. **The gate must never
carry either number as a constant.**

Sanity anchors, at the corrected rates:

- `B* (8 remaining calls) = 151,612` — which is what the 150,000 threshold means.
- A conversation of 100,000 with 10 calls left does not pay. One of 300,000 with
  4 calls left does not pay either.

---

## 9. Telemetry

New durable phase `compaction_economic_decision`, payload version
`compaction-economics/v1`.

```jsonc
{
  "version": "compaction-economics/v1",
  "estimatorVersion": "n-est/v1",
  "run": "wf-...", "step": "wfs-...", "cycle": 2, "session": "...",
  "at": "2026-09-12T10:00:00Z",

  "harness": "claude-code", "provider": "anthropic", "model": "claude-opus-5[1m]",
  "pricingStatus": "priced | unpriced_model | no_rate_card",
  "pricingSource": "...", "pricingVersion": "2026-06-24",

  "contextBefore": 67663,           "contextBeforeBasis": "observed",
  "contextThreshold": 60000,
  "stablePrefixTokens": 37379,      "stablePrefixBasis": "observed",
  "promptTokens": 1062,             "promptTokensBasis": "observed",
  "estimatedContextAfter": 61099,   "contextAfterBasis": "session_prior | model_percentile",
  "estimatedReduction": 7626,
  "estimatedReductionPercent": 11.1,
  "summaryTokens": 8050,            "summaryTokensBasis": "model_prior | single_observation",
  "summarySampleSize": 1,

  "estimatedRemainingCalls": 5,     "remainingCallsBasis": "prior_cycle_min_capped",
  "callsSoFarInSession": 23,
  "cycle": 2, "maxFixCycles": 3, "structuralCyclesRemaining": 1,
  "strategy": "task",
  "lifecycleReasons": ["context_pressure_actual"],

  "breakEvenCalls": 117.1,
  "safetyFactor": 1.5,
  "requiredCalls": 175.7,
  "estimatedCompactionCostMicros": 446420,
  "estimatedFutureSavingsMicros": 19065,
  "currency": "USD",

  "verdict": "compact | skip | unknown",
  "reason": "economics_positive | insufficient_remaining_calls | reduction_too_small | no_remaining_cycles | unpriced_model | context_after_unknown | summary_size_unknown | remaining_calls_unknown | usage_unobservable | prefix_unknown",
  "wouldHaveActed": false
}
```

Rules, all inherited from what this repo already enforces:

- Every field is a number, an id, a timestamp or a **closed enum**. No prompt,
  finding, command, conversation, summary, path or secret — and the struct that
  serializes it declares no field that could hold one. Needs the same test
  `TestParserStateNeverCarriesCommandText` and P7.1's own leak test have:
  a fixture deliberately full of content, asserted absent.
- **Money in integer micros.** A float in a durable payload makes two records
  from different builds incomparable.
- **Every estimate carries its basis and its sample size.** A verdict computed
  from a one-session prior must be re-identifiable later as such; that is the
  difference between a cohort that can be recalibrated and one that can only be
  thrown away.
- `wouldHaveActed` is always `false` in P7.2. It exists so the field does not
  have to be added later, and so a reader can never mistake the cohort for
  enforcement.
- **A record is written for every COMPACT decision, including when
  `sessionCompactionEnabled` is off.** That is how evidence accrues without
  spending anything.

---

## 10. Persistence: `workflow_checkpoints`, no migration

`parser_state_json` was the right home for P7.1's observations — they are facts
derived from one transcript artifact. A shadow verdict is **not** that: it is a
workflow decision about a run, a step and a cycle, and forcing it into the
parser's state would put run-scoped data in an artifact-scoped place and make it
unreadable by the only code that has the run in hand.

`workflow_checkpoints` is where it belongs and needs nothing new:

- `durable_phase` is free `TEXT` with no CHECK constraint; **111 distinct phases
  are already in use**, and P7 added `session_compaction_requested` to it with
  no migration.
- `retry_state` is `TEXT` with `CHECK (json_valid(...))` — the payload above
  fits as-is.
- `payload_version` already exists for the version string.

**No migration in P7.2.**

One load-bearing convention to copy exactly: `persistSessionLifecycleDecision`
and `recordSessionCompactionRequest` both write the checkpoint **without**
setting `workflow_step_id`, putting the step id inside the payload instead.
`cascade.go` explains why — several read paths mean "the latest checkpoint for
this exact step", and a second checkpoint at the same simulated clock tick could
shadow the one they need. P7.2 must write run-level records with the step in the
payload.

---

## 11. Evaluating shadow verdicts afterwards

### What becomes knowable

When a compaction naturally happens (someone turns the per-run knob on), P7.1
already records everything needed to score the verdict that preceded it:

| quantity | from |
|---|---|
| `B` actual | the boundary's `preTokens` |
| `A` actual | the first post-compaction call's context |
| `N` actual | calls in that cycle after the first |
| `S` actual | the session's residual / compaction count, once the session ends |

So a **COMPACT** verdict can be scored end to end: realized P/L = `N_actual * D_actual
* Cr − actual cost`. True positive if positive, **false positive** if negative.

### What does not become knowable

A **SKIP** verdict on a compaction that then did not happen has no observable
counterfactual. AO never learns what `A` or `S` would have been, so:

- "correct skip" is never *measured*, only *predicted*;
- **false negatives are structurally unobservable without an A/B**, and the A/B
  is the thing that was closed DO NOT EXECUTE.

The honest mitigation is not to measure them but to **bound** them: record the
margin `N_est / (N* * safetyFactor)` on every skip. A cohort whose skips cluster
at margin 0.03–0.09 — which is where both canary compactions sit — contains no
plausible false negative. A cohort with skips at 0.9 does, and that is the
signal to lower the safety factor rather than to keep skipping.

### The tension nobody should discover later

**Shadow mode accumulates predictions, not outcomes.** With
`sessionCompactionEnabled` default-off, compactions will stay rare, so verdicts
will pile up and scored outcomes will not. What shadow mode *can* calibrate
without a single compaction is three of the five inputs:

| input | calibratable without any compaction? |
|---|---|
| `N` remaining calls | **yes** — every fix cycle is a sample (§6 used 8) |
| rates / `pricingStatus` | **yes** — AO's calculated cost vs the harness rollup, 62 samples today |
| `B`, `P`, `delta` | **yes** — observed, no estimate to calibrate |
| `A` context after | **no** — needs a compaction |
| `S` summary size | **no** — needs a compaction, and a *finished* session |

That split is the whole shape of the §12 criterion.

---

## 12. `safetyFactor`

**Initial value for shadow: 1.5, and it is not load-bearing yet** — in shadow
nothing acts, so the factor's only job is to make the recorded verdict
comparable later.

Why 1.5 rather than the 1.0 that would give the same verdicts on all five known
points:

- The largest **overprediction** the remaining-calls estimator has ever made is
  **+25%** (§6). 1.25 covers exactly that and nothing else.
- `A` and `S` are one-observation priors, each biased conservative but each
  capable of being wrong in the same direction at the same time.
- 1.5 costs nothing on the observed population: the GO margins are 3.65x–4.04x
  and the SKIP margins 0.03x–0.09x at the corrected rates. There is a ~40x gap
  between the two populations and no point anywhere near the boundary.

**How to calibrate it, rather than keep asserting it.** Once there are scored
outcomes (§11), set it from the realized error distribution: the factor should
be the quantile of `N* / N_actual` that keeps false positives at zero on the
scored cohort, floored at 1.25 by the estimator's own worst case. Until then it
is a stated assumption, recorded on every row so the cohort can be re-scored at
a different value without re-running anything.

---

## 13. The criterion for P7.2 -> P7.3 (enforcement)

Not "a few days". Five conditions, and the third is the one that actually costs
something to satisfy.

1. **Pricing is honest for the sessions being judged.** The 1-hour cache TTL
   split (§1.1) is decoded and priced, and `pricingStatus == priced` for at
   least 90% of recorded verdicts. Enforcing a gate on a rate that is 60% of the
   true cache-write price is enforcing the wrong arithmetic.
2. **The remaining-calls estimator is validated on a population that is not this
   one.** At least **25** fix-cycle samples across at least **8 runs** and at
   least **3 projects**, with no overprediction beyond the safety factor and a
   median absolute error at or under 3 calls. Today: 8 samples, 5 runs.
   *Collectible with zero compactions.*
3. **At least 5 scored compactions across at least 3 sessions and 2 projects**,
   with `A` and `S` measured, and **zero false positives** — no verdict of
   COMPACT that turned out to lose money. This is the condition that cannot be
   met by waiting: it needs real compactions, which means enabling the per-run
   knob on runs whose shadow verdict was COMPACT **with a margin above 3x**,
   one at a time, with the realized P/L reported each time. That is not an A/B
   and it is not the Economic Canary — it is acting on the gate's own most
   confident recommendations and checking them.
4. **The skip cohort contains no near-misses.** No SKIP verdict at a margin
   above 0.8 that a later measurement shows would have been profitable. A cohort
   full of 0.9-margin skips means the factor is wrong, not that the gate is safe.
5. **The whole cohort is re-computable.** Every record carries
   `estimatorVersion`, `pricingVersion` and the basis of every estimate, so the
   decision to enforce can be re-derived at a different safety factor without
   re-running anything.

Fail any of the five: stay in shadow. Fail (1): fix pricing first, because
everything else is measured in the wrong currency.

---

## 14. Files P7.2 would touch

No migration. No API, DTO, OpenAPI, frontend or locale change — the verdict is a
durable record, and putting it on screen is a separate change with its own demo.

| file | what |
|---|---|
| `backend/internal/domain/compaction_economics.go` | **new.** The formula and the verdict, pure, table-tested. Input struct, no IO, no clock. |
| `backend/internal/domain/compaction_economics_estimators.go` | **new.** `A`, `S`, `N` estimators, each pure, each returning a basis and a sample size. |
| `backend/internal/domain/session_lifecycle.go` | new reason codes + `Valid()`; the verdict enum. |
| `backend/internal/domain/usage_trajectory.go` | `SessionContextReading` gains `StablePrefixTokens`, `LastOutputTokens`, `ModelID`. |
| `backend/internal/domain/model_rate_view.go` | **new.** A rate view so the gate can read four rates instead of a total. |
| `backend/internal/observe/usage/pricing/pricing.go` | `RateView(modelID) (domain.ModelRateView, bool)` — a read, no new rate. |
| `backend/internal/observe/usage/parser.go` | **decode the `cache_creation` TTL split** (precondition (1) of §13; separable and arguably its own checkpoint). |
| `backend/internal/workflow/compaction_economics.go` | **new.** Assembles the frozen input, calls the pure gate, writes the record. Returns nothing anyone reads. |
| `backend/internal/workflow/session_compaction.go` | one call site, after the durable compaction record, discarding the verdict. |
| `backend/internal/workflow/cascade.go` | build the fix prompt before the decision so `delta` is observed rather than guessed. **The only structural change, and the only real risk.** |
| `backend/internal/workflow/usage_budget.go` | the optional `UsageRateCard` port beside `UsagePricer`. |
| `backend/internal/storage/sqlite/queries/usage_ledger.sql` | extend `GetSessionContextReading` (prefix, last output, model); add a folded per-cycle call count. **ASCII only.** Then `npm run sqlc`. |
| `backend/internal/storage/sqlite/store/usage_attribution_store.go` | the mapping. |
| `docs/p7-2a-shadow-economic-gate.md` | this file, as the shipped design. |

## 15. Tests P7.2 needs

**Pure domain**
1. The five historical points reproduce their `N*` to 0.1 at the corrected rates.
2. `D <= 0`, `D` tiny, `B < A` — SKIP, never a division by zero or a negative `N*`.
3. Each missing input independently forces UNKNOWN, never COMPACT (one case per §5 row).
4. Rate-ratio sensitivity: the same conversation at a 1-hour cache-write rate
   needs measurably more calls than at the 5-minute one. **This is the test that
   fails if anyone hardcodes 62.5 or 70.**
5. Micros round-trip unchanged through the payload.

**Estimators**
6. The 8 historical remaining-calls samples pinned exactly, including
   `wf-5b2210f5`'s 10x underprediction — it documents the failure mode.
7. `A` from a session's own prior boundary beats the model percentile; absent
   both, UNKNOWN.
8. **Look-ahead guard:** given a boundary belonging to the CURRENT cycle, the
   estimator must not see it. Construct the input with the current boundary
   present and assert the verdict is identical to the input without it.
9. `S` never sourced from the session being judged.

**Workflow**
10. The verdict changes nothing: `TestFixCycleIsDeliveredWhateverHappensToTheCompaction`
    extended to assert byte-identical delivery with every verdict, including
    COMPACT.
11. A record is written for every COMPACT decision, exactly once per cycle,
    surviving restart without a duplicate — mirroring
    `TestCompactionIsRequestedAtMostOncePerCycle`.
12. A record is written with the policy knob **off**.
13. The record is written run-level with the step in the payload, and does not
    shadow `fix_dispatched` reads.
14. `no_remaining_cycles` fires on the terminal cycle when the only lifecycle
    reason is `many_fix_cycles` — **this is the canary's compact 2 as a
    regression test.**
15. `TestEconomicVerdictCarriesNoContent`, against a content-rich fixture.

**Pricing**
16. Unpriced model -> UNKNOWN -> no cost invented, model named.
17. An operator rate card covering `claude-opus-5[1m]` changes `pricingStatus`
    and nothing else.
18. A zero cache-read rate produces no division by zero and no infinite saving.

## 16. Risks

1. **The TTL defect is upstream of the gate.** Until cache writes are priced at
   the rate the provider charges, every figure the gate computes is in the wrong
   currency — and wrong in the direction that flatters compaction.
2. **`A` and `S` are one-observation priors**, from a fixture run on a synthetic
   task. A real project with a large CLAUDE.md moves `P`; a longer conversation
   may move `S`.
3. **False negatives are unobservable.** §11. The margin distribution is a bound,
   not a measurement.
4. **Building the prompt before the decision** reorders `applyFixLifecycleDecision`.
   The existing ordering comment is about *delivery*, not construction, but the
   path is recovery-sensitive. This is the only change in P7.2 that could break
   something that works today.
5. **Eight remaining-calls samples, five runs, one project shape.** No Autonomous
   or Master run is represented at all, and those are the runs with the most to
   save.
6. **Shadow mode can look like progress while collecting nothing decisive.**
   Verdicts accumulate; `A` and `S` do not. §13 condition (3) exists to stop the
   cohort being mistaken for evidence.
7. **The canary is a fixture, not a workload.** Sixteen worker calls on a
   synthetic Go task is not what a real repair cycle looks like, and it is half
   the population.

---

## 17. Recommendation

**IMPLEMENT P7.2 SHADOW**, with one reordering.

The design is sound, it is now anchored on measurements rather than on
character counts, every verdict it would have produced on the five known points
is correct, and shadow mode cannot cause harm because nothing reads the verdict.
It is also the only way to accumulate the evidence that any enforcement decision
needs.

The reordering: **decode the cache-creation TTL split first**, as its own small
change, before or alongside the gate. It is a dozen lines in the parser, it
corrects AO's cost reporting for every Claude session and not only compacting
ones, and it is the difference between a shadow cohort that can be trusted later
and one that has to be recomputed. It is §13's first condition and the cheapest
of the five to satisfy.

And one thing to stop claiming: `docs/p7-turn-economy.md`'s replay figures and
the earlier gate design both quote list-price arithmetic at the 5-minute cache
rate. Both are understatements of the same kind, and P7.1's own corrected table
is too.

```
P7.1  IMPLEMENTED = yes   VALIDATED = yes   INTEGRATED = yes   BEHAVIOR CHANGED = no
P7.2A IMPLEMENTED = no    (design only)
MECHANISM-PROVEN          = yes
PRODUCTION-SAVINGS-PROVEN = no
```

---

# P7.2B2 — AS SHIPPED

**Status: IMPLEMENTED, SHADOW ONLY. Nothing reads the verdict.** The sections
above are the design; this section is what landed, where it departed from the
design, and what it costs.

```
P7.2B2 IMPLEMENTED = yes    SHADOW ONLY = yes
BEHAVIOR CHANGED   = no     ENFORCEMENT = no
```

## What the arithmetic actually is

The break-even form of §8, with one change that P7.2B1 forced:

```
        P + (Co/Cr)·S + (Cw_observed/Cr)·(A − P − Δ)
N*  =   ───────────────────────────────────────────      COMPACT iff N ≥ N*·safety
                      (B + Δ) − A
```

`Cw_observed` is **whichever cache-write rate this session's own writes were
actually created at**, read from the per-event TTL split migration 0170 added and
the 2026-09-12 backfill populated. It is not a constant, not the catalog's
five-minute rate, and not an average: any unknown contribution at all refuses the
whole verdict, and where both lifetimes are present the dearer one wins. On the
embedded catalog `(Co+Cw)/Cr` is 62.5 at the short rate and **70** at the long
one; the gate carries neither number and
`TestTheRateRatiosComeFromTheCardAndAreNotUniform` fails if anyone writes one in.

Every historical boundary reproduces: N* = 28.2, 117.1, 5.3, 4.0, 3.3, pinned to
±0.05 in `TestBreakEvenCallsReproducesEveryHistoricalBoundary`.

## Where it runs

`applyFixLifecycleDecision`, immediately **before** `maybeCompactBeforeFix`. The
design said after; before is strictly safer, because every observation the gate
consults is then untouched by the act being judged, which makes the look-ahead
guard structural rather than a promise.

**The design's one real risk did not materialise.** §16 risk 4 expected
`cascade.go` to be reordered so `Δ` would be observable. It already is: the fix
prompt is built in `maybeDispatchFix` and passed into
`applyFixLifecycleDecision` as a parameter. No reordering was needed and none was
done.

## Departures from the design

| design | as shipped | why |
|---|---|---|
| extend `GetSessionContextReading` (prefix, last output, model) + a new folded per-cycle call count query, then `npm run sqlc` | **no query change at all.** One existing indexed read, `ListRunContextTrajectoryEvents`, already carries role, cycle, model, the per-event TTL split, the first call's cache read and the last call's context. A thin store method narrows it to the worker roles and returns domain types. | Zero new SQL, zero sqlc regeneration, zero new index, and one read instead of three. Directly satisfies the performance rule. |
| `A` from this session's prior boundary, else a per-(harness, model) p75 | **session prior only.** No percentile. | The population is one session, so a "p75" over it is a single observation wearing a statistic's name. Absent a prior boundary the answer is UNKNOWN. |
| `S` from a cross-session prior | **port declared, deliberately unwired.** | `S` is only measurable from a harness end-of-session rollup, so it is structurally cross-session; reading it means walking other sessions' sources, and a corpus walk on every fix boundary is what §24 forbids. It needs a read model folded when a session completes. Consequence, stated rather than hidden: **live verdicts are UNKNOWN/`summary_cost_unknown` until that read model exists.** |
| reason codes extending `SessionLifecycleReason` | **a separate closed enum.** | The shadow gate must not be able to put a value into the enum the lifecycle decision validates. A shared enum is the first step toward a shared code path. |
| no readback surface in P7.2 | **`ao usage compaction-verdicts <run-id>`**, read-only. | §15 asked to be able to interrogate the cohort. It opens the database with `sqlite.OpenReadOnly` — no writable connection, no migration, no daemon required — so it needs no HTTP API, no DTO, no OpenAPI and no frontend change. |

## The estimator constants, and what anchors each

| constant | value | anchored on |
|---|---|---|
| `safetyFactor` | 1.5 | the estimator's worst overprediction (+25%) plus headroom for two one-observation priors. 1.0 gives the same five verdicts. |
| `minReductionFraction` | 0.25 | the measured compaction that reduced 11.1% and lost $0.43. |
| `reattachFactor` | **1.25** | the ONE out-of-sample prediction the corpus supports (compact 2's A from compact 1's post size): 61,099/49,397 = 1.2369, rounded up. See the review findings below for why 1.19 was wrong. |
| remaining-calls ratios | 0.25 / 0.80 | the bottom of the observed range, across 8 fix-cycle samples in 5 runs. |
| remaining-calls ceiling | 19 | p50 of those 8 samples (19.5, floored). Only ever lowers. |
| near-miss floor | 0.8 | both measured compactions sit at 0.03–0.09, two orders of magnitude below. |

All six are named constants beside their provenance and all six travel on every
record through `gateVersion` / `estimatorVersion`, so the whole cohort is
re-scorable at different values without re-running anything.

## Scoring, and the thing that can never be scored

`ScoreCompactionVerdict` is a separate pure function taking the already-written
record and a separately-assembled observed outcome. It cannot write back, and the
record cannot have been computed from the outcome.

- **COMPACT + compaction happened** → scorable. Profitable is a true positive, a
  loss is a **false positive**, and the enforcement criterion requires zero.
- **SKIP + compaction happened anyway** (the policy asked; the gate governs
  nothing) → the only skip a MEASUREMENT ever confirms. Both canary compactions
  are this shape and both score `skip_vindicated` at −$0.325 and −$0.431.
- **SKIP + no compaction** → `unscored_counterfactual`. **Not a correct skip.**
  AO never learns what `A` or `S` would have been, so false negatives are
  structurally unobservable without the A/B that is closed DO NOT EXECUTE. The
  margin recorded on each skip is a BOUND, never a measurement.
- **UNKNOWN** → `unscorable`. A verdict that declined to predict cannot be right.

`CompactionScoreboard.EnforcementReady()` returns two bools, and the second is
the load-bearing one: **an empty scoreboard has not passed the criterion, it has
not taken it.** Shadow mode accumulates predictions while outcomes do not, and a
count of predictions can look like progress for months.

## What this cohort will actually contain on live work today

`UNKNOWN / summary_cost_unknown`, on nearly every row, because the summary prior
is unwired and — separately — because the real corpus contains **zero** harness
rollups and **zero** compaction boundaries: 138 registered sources, none of whose
durable parser state carries either (measured 2026-09-12). P7.1's observation
fields exist in the parser, but every existing source was parsed before them and
sits at an advanced byte offset, so it will never re-read the records that carry
them.

That is the fail-closed outcome and it is what the design predicted a first
deployment would look like. It is not nothing: every row still carries the
observed `B`, `P`, `Δ`, the model, the pricing status, the cache lifetime the
rewrite WOULD be priced at, the remaining-calls forecast with its basis and
sample count, and the structural cycle arithmetic — which is precisely the subset
§11 says shadow mode can calibrate without a single compaction.

## The replay figure, corrected again

At the one-hour cache-write rate the `wf-1c2cb9bd` replay is **−39.0% of the
tokens and −24.0% of the money** (net +$5.844951 against a verified baseline of
$24.317756). P7.1 published 25.1%; the earlier design published −17…−27%.
`docs/p7-turn-economy.md` now carries the derivation, and
`TestTheReplayCostReductionIsNotTheTokenReduction` fails if the two figures are
ever reported as one.

**A note on provenance, because the number was asked for as ≈21.2%:** that value
is not reproducible from the ledger. 24.04% uses each boundary's own measured call
tail (29, 24, 19); substituting the estimator's capped forecast (19 on all three)
gives 18.74%. 21.2% falls between them and no combination of the measured inputs
produces it, so it is not published here. If it came from a different `S` or a
different base, say which and this section is a one-line change.

## What is still required before P7.3

1. **A cross-session summary read model**, folded at session completion. Without
   it `S` is UNKNOWN and the gate can price nothing end to end on live work.
2. **Compaction boundaries in the corpus.** Zero exist. `A` is UNKNOWN until a
   session compacts with the P7.1 parser in force.
3. **Remaining-calls samples: 8 of 25, across 5 runs of 8 and 4 projects of 3.**
   Collectible with zero compactions, and the only criterion currently moving.
4. **Zero scored compactions of the 5 required.** Needs real compactions, which
   means enabling the per-run knob on runs whose shadow verdict was COMPACT with a
   margin above 3x, one at a time, reporting realized P/L each time.
5. **`internal/workflow -race`** has not been run against this diff. It needs
   ~1100–1200s and an explicit timeout; it should pass before integration merge.

---

# P7.2B2 — INDEPENDENT INTEGRATION REVIEW FINDINGS

Reviewed at feature `d6df06706`, base ECC `ae670846e`. Three defects found and
corrected before merge; two claims in the implementation report corrected.

## D1 (CODE, behaviour-visible) — the verdict displaced the run's latest phase

`foldCheckpointAuthority` folds every checkpoint whose phase is not
`isBookkeepingPhase` into `LatestCheckpointPhase`/`LatestCheckpointAt` — the
field whose own comment says *"this is the run's timeline, and the lifecycle
derivation reads it"*. `compaction_economic_decision` was not in that set, so an
unknown phase fell through `classifyCheckpointPhase` to `authorityLifecycle` and
counted.

**A row that governs nothing was able to rename the run's last activity.** That
is precisely the hazard `isBookkeepingPhase` documents, and its own entry test —
*"can this row, on its own, change what the run owes?"* — answers no for a shadow
verdict by construction.

Fixed by one entry in `isBookkeepingPhase`, which `classifyCheckpointPhase`
consults FIRST, so the same edit also makes the phase an observation rather than a
lifecycle row. Guarded by three tests, verified to fail without the fix:
`TestTheShadowEconomicVerdictIsBookkeeping`,
`TestTheShadowEconomicVerdictCanNeverBecomeAStopOrALifecyclePhase`,
`TestAShadowVerdictDoesNotDisplaceTheRunsLatestPhase`, plus the end-to-end
`TestARecordedVerdictDoesNotBecomeTheRunsLatestPhase`.

Every other checkpoint consumer was cleared: all five `switch cp.DurablePhase`
folds in the launch/recovery paths `continue` on `cp.WorkflowStepID == nil`
before reaching the switch, and the run-level nil-step convention is what makes
them structurally blind to this phase. `NextAction` is guarded by
`if cp.NextAction != ""`, which an empty one cannot displace.

## D2 (CODE, hardening) — the gate aliased the caller's reason slice

`LifecycleReasons: decision.Reasons` handed the gate the same backing array as the
`SessionLifecycleDecision` persisted moments later. Nothing appended to it, so
nothing was wrong — but the one path by which a shadow evaluation could reach out
and alter the decision it shadows should not exist as a matter of reading. Now
copied, with `TestTheRecordIsIndependentOfTheCallersReasonSlice` pinning it.

## D3 (ESTIMATOR, optimistic) — `reattachFactor` was fitted in sample

1.19 was derived by dividing each boundary's measured `A` by **its own** reported
`postTokens`:

```
compact 1   57,803 / (37,379 + 10,956 + 1,181) = 1.1673
compact 2   61,099 / (37,379 + 13,240 + 1,062) = 1.1822
```

Compact 2's own post size is the one number a prediction about compact 2 cannot
have — it is produced BY the compaction being predicted. **The factor was fitted
with the answer in the training set**, which is the same look-ahead the estimator
exists to prevent, one level up.

The corpus supports exactly one honest prediction: compact 2's `A` from compact
1's post size, which is what the estimator actually holds.

```
61,099 / (37,379 + 10,956 + 1,062) = 61,099 / 49,397 = 1.2369   ->  1.25
```

At 1.19 that prediction is **58,782 against a measured 61,099 — 3.8% optimistic**,
and optimistic on `A` is the direction that compacts. Corrected to **1.25**
(1.06% conservative on the one point it can be checked against). The estimator
test was rewritten to be the out-of-sample prediction rather than the in-sample
fit, and it asserts `A_est >= 61,099` so a future re-fit cannot slip back.

## R1 (REPORT) — the canary retrospective claimed a SKIP it could not have made

The implementation report said the gate *"would have said SKIP to both"* canary
compactions. It would not have, and the correction matters more than the claim:

| | honest pre-decision verdict | why |
|---|---|---|
| compact 1 | **UNKNOWN / `context_after_unknown`** | it is the session's FIRST compaction, so there is no prior boundary and `A` is unknowable. The measured A=57,803 was produced by the compaction being judged. |
| compact 2, as the corpus stands | **UNKNOWN / `summary_cost_unknown`** | `A` is estimable from compact 1, but `S` is a cross-session prior and the only session with a rollup is the one being judged. |
| compact 2, once any other session supplies a summary prior | **SKIP / `reduction_too_small`** | at A_est = 61,746 the predicted reduction is 6,979 = 10.3% of the conversation, below the 25% floor — and the measurement confirms it lost $0.4313. |

Pinned in `TestTheCanaryPreDecisionVerdictUsesOnlyWhatWasAvailable`. The SKIP
that survives is a genuine one: it is reached from an `A` that never saw compact
2's own post size.

## R2 (REPORT) — "69/69 sessions priceable, 100% coverage" had the wrong denominator

The claim was computed over sessions with any usage at all, with a `MAX()` that
let one priceable model cover a session that also ran an unpriceable one. The
correct denominators:

| denominator | scope | priceable | note |
|---|---|---:|---|
| A — all ledger events | 3,879 events | **3,481 (89.7%)** | includes non-session subjects |
| B — events on a session | 3,433 | 3,429 (99.9%) | the 4 exceptions are `<synthetic>` |
| C — sessions with any usage | 69 | 68 single-model priceable, 1 multi-model | the prior claim's scope, and it was 68/69 not 69/69 |
| **D — the gate's actual universe: sessions with a worker/fix_worker role** | **23** | **22 (95.7%)** | 1 session ran `claude-opus-5` + `<synthetic>` and records `inconsistent_accounting` |

**Denominator D is the honest one for the enforcement criterion**, and 95.7%
clears the ≥90% bar. Both of the following are true and do not contradict each
other: *the ledger still holds 263,976 unknown-TTL tokens*, and *no shadow verdict
can be blocked by them* — because those 5 rows carry `model_id = "sonnet"` on
bindings with **no session id**, so they are outside the gate's universe
entirely. The earlier "100%" was not a lie about pricing; it was a denominator
that had quietly excluded exactly the rows in question.

## CANONICAL ECONOMIC NOMENCLATURE

The review found four different figures all being called "cost savings". They are
different scopes and from now on they carry different names. **Never use the bare
phrase "cost saving" for any of them.**

| canonical name | scope | measured or modelled | figure |
|---|---|---|---|
| **LEDGER COST** | the run's attributed events, priced by the embedded card at the observed 1h cache-write rate. Unpriced models report tokens with cost unknown and are NOT in the amount. | MEASURED | **$24.317756** (`wf-1c2cb9bd`) |
| **HARNESS COST** | the harness's own end-of-session cost figure. A different party, a different rate card, and it includes spend AO holds no event for. Never to be folded into a ledger total. | MEASURED by the harness | $24.805628 |
| **TOKEN REDUCTION** | cumulative billed input, replayed by `turnbench`. **Not money.** | MEASURED replay of the real call series | **−39.0%** |
| **MODELLED REPLAY SAVING (measured tails)** | the 3 replay boundaries with `A`, `S`, `P` as priors and `N` as each boundary's OWN measured call tail (29, 24, 19) | MODELLED | **+$5.844951 = 24.04% of LEDGER COST** |
| **MODELLED REPLAY SAVING (forecast tails)** | the same, with `N` from the estimator's capped forecast (19, 19, 19) | MODELLED | +$4.557438 = 18.74% |
| **REALIZED P/L** | a compaction that actually happened, every term measured | MEASURED | canary: **−$0.3247** and **−$0.4313** |

```
LEDGER COST        = 386·5.00 + 35,787,742·0.50 + 294,688·10.00 + 139,003·25.00  (per MTok)
per-boundary cost  = P·Cr + S·Co + (A−P−Δ)·Cw1h                     = $0.407255
saving per call    = D·Cr ,  D = (B+Δ) − A
MODELLED REPLAY SAVING (measured tails) = Σ N·D·Cr − Σ cost         = $5.844951
                                        / LEDGER COST               = 24.04%
```

**THE CURRENT FIGURE, EXPLICITLY: the `wf-1c2cb9bd` replay is a TOKEN REDUCTION of
−39.0% and a MODELLED REPLAY SAVING (measured tails) of 24.04% of LEDGER COST.**
The word "realized" does not apply to it: that run never compacted. `~21.2%` is
not reproducible from any of these scopes and is not published; `~27%` and `25.1%`
are superseded.

## S READ MODEL — DECISION: DO NOT IMPLEMENT NOW

The brief asked whether a minimal durable read model for `S` should land before
merge so the gate is observable on real runs. **No, and the reason is not
scope — it is that the input does not exist.**

Measured on the real database, 2026-09-13:

```
usage_sources                      138
  carrying "compactions"             0
  carrying "harness_totals"          0
  carrying "cache_creation"          0
sessions that could supply an S prior  0
```

Not one source's durable parser state carries any P7.1 observation. The fields
exist in the parser, but every registered source was consumed past those records
before P7.1 landed and sits at an advanced byte offset, and no session has
compacted since. A read model built now would read an empty set, so it cannot
change a single verdict — while costing two new sqlc queries, a service reader,
daemon wiring and their tests.

**The blocker is observation availability, not the read model**, and that
reordering is the finding. Therefore:

> **P7.2B2 is integrated but NOT operationally useful yet.**

and the next checkpoint is scoped accordingly:

> **P7.2B2.1 — Compaction Observation Availability, then the Summary Cost Read
> Model.** In that order. (1) Establish how a source's P7.1 observations get
> written at all — a re-read from offset zero for complete sources, or acceptance
> that only sessions started after P7.1 will ever carry them. (2) Only then fold
> the cross-session summary prior at session completion, keyed by (harness,
> model), with sample counts and no decision-time corpus walk.

Until (1) lands, a shadow observation period would accumulate rows that all say
`summary_cost_unknown`. That is honest and it is not evidence. **An UNKNOWN must
not be reported as a SKIP to make the cohort look populated.**

---

# P7.2B2.1 — COMPACTION OBSERVATION AVAILABILITY + SUMMARY COST READ MODEL

**Status: IMPLEMENTED. Shadow still governs nothing.** P7.2B2's review closed with
"integrated but not operationally useful": the gate was correct and the corpus held
no evidence for it. This closes that, and the first thing it establishes is that
the parser was never at fault.

## Root cause — proven, not assumed

**The parser was already right. The offsets were already past.** Every registered
source had been consumed beyond the records that carry compaction evidence before
P7.1 taught the parser to look for them, and a source at an advanced byte offset
never re-reads what it has already passed.

Demonstrated three ways rather than argued:

| question | answer | evidence |
|---|---|---|
| does the parser detect a compaction on a new transcript? | **yes** | `TestANewSessionProducesEveryObservationTheEconomicGateNeeds` — a fixture of ordinary turns, a boundary, the post-compaction rewrite and the session rollup, parsed from offset zero |
| is the historical gap only advanced offsets? | **yes** | 137 of 138 transcripts still on disk, and they still CONTAIN the evidence: 2 `compact_boundary` records in 1 file, **166 `cost-state` records across 74 files (60 sessions)**. The one file with boundaries sits at `byte_offset == size`, last updated 2026-09-11 21:13 UTC, before P7.1 existed. Nothing was lost; it was skipped. |
| would a session created today accumulate correctly? | **yes** | the same test asserts boundary identity, pre/post tokens, duration, trigger, timestamp, model, uuid, the rollup's output AND reasoning, the full cache vector, and the 5m/1h/unknown split partitioning its own reported total |

And the two properties a recovery would depend on:
`TestObservationsSurviveAParserResumeFromThePersistedOffset` (interrupted directly
after the boundary, resumed, nothing re-counted) and
`TestReprocessingTheWholeSessionIsIdempotent`.

## Forward observation path — before and after

Before: nothing was wrong with it and nothing exercised it end to end. After: the
whole shape is a test, including the two things the earlier design got wrong by
estimating them — **reasoning**, which is most of what a summarizer generates and
none of what its text contains (6,326 tokens on the measured session), and the
**cache-creation lifetimes**, without which the rewrite is priced at 62% of what
the provider charges.

No parser change was needed. No offset was rewound. No `source_event_key` moved.
The four assistant turns in the fixture produce exactly four ledger events, and
the boundary and the rollup produce none — asserted, because an observation that
billed something would be a second telemetry.

## Historical recovery — assessment, and the decision

**DECISION: (A) FORWARD ONLY. No backfill implemented.**

It *could* be done safely: the transcripts exist, re-parsing into throwaway state
and merging only the observation fields is deterministic, read-only over
transcripts, and idempotent (proven above). A dry-run would recover:

```
compact_boundary records recoverable    2   (1 session: ao-canary-fixture-6)
cost-state rollups recoverable        166   (74 files, 60 sessions)
sessions gaining a COMPLETE summary observation   1
```

That last number is why the answer is no. **A backfill would not make S known.**
The prior needs three independent sessions that compacted; exactly one exists on
disk, and recovering it moves the cohort from 0/3 to 1/3. The brief's own rule —
*do not choose the backfill merely to accelerate statistics* — decides it, and
§20's rule that shadow observation may begin with S UNKNOWN removes the urgency.
The other recoverable rollups belong to sessions that never compacted; they are the
non-compaction baseline, which this estimator does not net against.

If a later checkpoint wants them, the shape is settled: a separate command,
dry-run by default, that never touches the ledger, the offsets or the event keys.

## S — SUMMARY COST UPPER BOUND

`S` is an **upper bound** on the tokens the harness GENERATES producing a summary,
per compaction, derived as **the harness's own output total minus what AO's ledger
holds an event for**. It is never a realized cost and must not be reported as one.

The arithmetic, per session (`domain.BuildCompactionAccounting`):

```
for each model bucket m in the harness rollup (cost-state, cumulative, last wins):
    residual_m = max(0, rollup_output_m - ledger_output_credit_m)   # credit consumed once
S_session  = sum_m residual_m / compactions                           # integer division
```

- **rollup output** is `modelUsage[m].outputTokens` from the harness's own
  `cost-state` record: every output token the harness was billed for in the
  session, on every model, including calls that never become an assistant record
  (the summarization, background title/topic calls on a small model).
- **ledger-attributed output** is `model_usage_events.output_tokens` summed for the
  session: exactly the assistant records AO parsed.
- **Reasoning is counted once.** Both sides use the provider's `output_tokens`,
  which already INCLUDES thinking; `thinkingTokens` is a sub-count carried beside
  it and is never added. No double counting.
- **Never negative.** Each dimension is floored at zero, and credit is consumed as
  it is matched so two rollup spellings of one model cannot subtract it twice.

Why it bounds the summary from ABOVE: the residual is summarization output PLUS
every other unattributed call. Everything extra overstates `S`, and overstating the
cost of compacting can only move a verdict away from COMPACT. The bound holds only
while the rollup covers at least everything the ledger attributed — see the
admissibility rules below; where AO cannot show that, the session is not a sample.

Unknown pricing does not enter here at all: the reader carries tokens and never a
price, and the gate's own `RatesKnown` / TTL checks refuse to price a verdict.

### Admissibility — when a session is a sample (tightened in review)

A session contributes an observation only when ALL hold; otherwise it costs one
sample and never enters the prior:

| rule | why the residual would otherwise understate |
|---|---|
| the harness wrote a rollup and the session compacted | no rollup, no residual at all |
| `UnattributedNegative` is **false** | AO attributed spend the rollup does not admit to: a rollup written before later turns (resumed session) or a ledger counting sources the rollup does not. The subtraction no longer bounds anything. |
| compaction count == distinct detailed boundaries | an overflowed boundary list (identity unknown) or the same boundaries counted by two artifact generations inflates the divisor |
| residual output > 0 | a compacting session with no unattributed output is a measurement problem, not a free summary |
| every boundary names one harness + one model | the cohort rule above |
| a placeable timestamp | the look-ahead rule below |

The candidate list is capped at 64 sessions and **reaching the cap fails closed**:
the prior is a maximum, and a maximum over an arbitrary subset can only understate.

Measured against the only real compacting session (`ao-canary-fixture-6`,
recomputed independently from its transcript and the ledger): `UnattributedNegative
= false`, residual output 16,953 (opus) + 33 (haiku) = 16,986 over 2 compactions,
**8,493 per compaction**. The guards do not exclude the one real sample.

**Known limitation, not fixed here.** A `cost-state` record carries no timestamp and
the parser does not store where in the transcript it sat. A session whose latest
rollup was written *before* its latest boundary *and* made no call after it cannot be
told apart from a complete one, and would contribute a figure that misses that
compaction's summary. The ledger-coverage rule above catches every such session that
made a call after the rollup; closing the rest needs the rollup's position in parser
state, which is an ingest change and out of scope for a read model.

Three properties make it usable and each one is deliberate:

- **It includes reasoning.** The rollup reports `thinkingTokens` separately and the
  residual contains them. Counting the summary's characters instead understated it
  roughly twofold.
- **It is an UPPER BOUND, not a measurement.** The harness reports spend once per
  session, not once per compaction, so there is no per-compaction figure to read.
  Overstating the cost of compacting can only move a verdict away from COMPACT,
  which is the direction this design always takes.
- **It can never come from the session being judged.** Structural: the rollup is
  written when a session ends, so a running session has none.

### Grouping — EXACT (harness, model) cohorts (corrected in review)

As first implemented the port took `(harness, modelID)` and ignored both, on the
grounds that today's corpus is homogeneous. The review rejected that: a prior that
mixes a cheap summarizer's sessions into an expensive one's is optimistic for the
expensive cohort, and "homogeneous today" is not a property of the code.

Now:

- An observation's cohort is the binding's **harness** and the **model its
  boundaries were taken on**, in the ledger's own spelling. The boundary — not the
  rollup — supplies the model, so it matches the judged session's call series
  (`claude-opus-5`) **exactly**, with no alias resolution: the rollup's
  `claude-opus-5[1m]` is never consulted for identity.
- A session whose boundaries disagree on model or harness belongs to **no** cohort.
- The gate reads the judged session's harness from its session record (one
  primary-key read) and passes the call series' model. Either one empty, or the
  parser's `unknown` placeholder, is an unidentified cohort: **UNKNOWN**, and the
  candidate list is not even read.
- The reader filters on exact equality, and the estimator filters again.
  Case and suffix are not normalized.

## The minimum sample rule, and the estimator

```
fewer than 3 INDEPENDENT sessions  ->  UNKNOWN, with the sample count reported
3 or more                          ->  the MAXIMUM per-compaction figure
```

Both halves changed in this checkpoint and both got stricter:

- P7.2B2 returned the most expensive observation below the minimum. It now returns
  **UNKNOWN**. One or two observations are a point estimate wearing a statistic's
  name, and the whole purpose of a shadow phase is to find the distribution rather
  than assume it.
- Above the minimum it returned the **mean**; it now returns the **maximum**. `S`
  enters the *cost* of compacting, so the mean understates on every session above
  it. The maximum never understates any session AO has observed, and with a cohort
  this small it is the most conservative choice that is not absurd. A percentile is
  the right answer when a percentile means something; that change bumps
  `estimatorVersion`.

**INDEPENDENT means distinct sessions.** Three boundaries of one session are ONE
sample — they share a harness, a project, a task shape and a CLAUDE.md, and the
quantity being estimated moves with all of them. Pinned by test.

`estimatorVersion` is now **`compaction-estimators/v2`**. The v1 label had shipped
with the mean and the below-minimum maximum; a verdict must never carry a label
that names two different rules.

## Look-ahead protections

Three, and the third is new:

1. `A` reads only boundaries strictly before the decision instant, and a boundary
   with no timestamp is inadmissible rather than assumed old.
2. `N` counts only cycles strictly below the current one.
3. **`S` now carries `ObservedAt` per observation and `DecisionAt` on the input**,
   with the same strict comparison. `TestTheSummaryPriorExcludesTheFutureAndThe
   JudgedSession` offers the estimator the judged session's own rollup, an
   observation at the decision instant, one after it, one unplaceable in time and
   one with no session id — and asserts the result is IDENTICAL to the clean
   cohort.

The filter lives in the pure estimator as well as in the caller, because a guard
that exists in one place is a guard one refactor can remove.

`S`'s `ObservedAt` is the session's **latest boundary** timestamp, because the rollup
has none. That is a lower bound on when the evidence became complete. On the
decision path it cannot leak the future — the read happens at decision time and
cannot see bytes not yet written — but an OFFLINE replay over a historical snapshot
would need the rollup's own time to be strict. No such replay exists today.

## Read model and decision-time cost

No migration. No new table. No session-completion hook. **One new query**, and the
filter is what makes it affordable: *a prior about compaction is only ever about
sessions that compacted*, so SQL narrows the candidate list to those and the
per-session arithmetic reuses the fold that already existed.

Measured on the real 905 MB database:

```
EXPLAIN QUERY PLAN
  SEARCH ub USING COVERING INDEX idx_usage_bindings_session_state (session_id>?)
  SEARCH us USING INDEX idx_usage_sources_binding_kind (binding_id=?)

wall clock, three runs           0.01s / 0.00s / 0.00s
usage_sources touched            138 rows, 43,629 bytes of parser state
model_usage_events touched       NONE
transcripts opened               NONE
```

Two indexes, and the 3,879-row ledger table is not in the plan at all. Stated
precisely: the plan walks every **session binding's sources** through those
indexes and evaluates `json_extract` on each one's parser state, so the work grows
with the number of sources, not with the number of compacted sessions; only the
OUTPUT is the handful. 138 sources today; if that ever becomes a decision-path cost,
the answer is the folded aggregate, not a wider cap.
The candidate list is additionally capped at 64 sessions: a cohort large enough to
reach that ceiling has earned a folded aggregate of its own, and truncating is
better than an unbounded read on a decision path.

## What this changes about starting shadow observation

Before: observing produced nothing, because the input the gate was missing was not
something observation generated. Every verdict would have said
`summary_cost_unknown` forever.

After: **a session that compacts now records the evidence, and the prior becomes
known on its own at the third independent session.** S is still UNKNOWN today —
`ao usage compaction-observations` reports `0 of 3 needed` against the real
database — and that is the correct answer, not a reason to lower the minimum.

That difference is the whole point of this checkpoint, and it is why shadow
observation can begin: the cohort now accrues the thing it was missing.
