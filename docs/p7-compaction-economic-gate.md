# Compaction economic gate — design (P7-E)

**Status: DESIGN ONLY. Nothing here is implemented, nothing is committed, no run
was launched.** Every figure is either *measured* (read from `model_usage_events`
or from the harness's own `compact_boundary` metadata in the transcript AO
already tails) or *modelled*, and each one says which.

Standing facts this document does not change:

| | |
|---|---|
| IMPLEMENTED (the act) | yes |
| INTEGRATED (lifecycle → `/compact`) | yes |
| MECHANISM-PROVEN | yes (`wf-66f0ee54`) |
| PRODUCTION-SAVINGS-PROVEN | **no** |
| global default | stays **off** |

The Economic Canary live A/B is closed **DO NOT EXECUTE**. This document is what
replaces it: the same question answered from records that already exist.

---

## 0. The one-line finding

> A compaction is not a discount on the next call. It is a **purchase**: you pay
> an output-priced summary plus a cache-write-priced rewrite now, in exchange for
> a cache-read-priced discount on *every call that comes after*. The only
> variable that decides whether the purchase pays is **how many calls come
> after**, and that is precisely the variable the current gate never looks at.

On Opus 5 list prices the exchange rate is brutal: **one token written costs 12.5
cache-reads, one token generated costs 50.** A summary is both.

---

## 1. The formula

### 1.1 Symbols

| symbol | meaning | where AO can get it |
|---|---|---|
| `Ci` | $/token, uncached input | pricing table |
| `Cr` | $/token, cache read | pricing table |
| `Cw` | $/token, cache write | pricing table |
| `Co` | $/token, output | pricing table |
| `B` | conversation size **the compaction will read** = last call's context + last call's output | `SessionContextReading.LastContextTokens` (+ a new `LastOutputTokens`) |
| `P` | stable cached prefix that survives compaction (system prompt, tool schemas, CLAUDE.md) | first placeable call's `cache_read_tokens` for the session (**new fold**) |
| `A` | context of the FIRST call after compaction | predicted; see §1.5 |
| `D` | `(B + Δ) − A`, the per-call context saving | derived |
| `Δ` | tokens of the fix prompt + context pack AO is about to send anyway | `promptBytes / 4` — AO knows it exactly, *if the prompt is built before the decision* |
| `S` | tokens the harness **generates** as the summary | per-harness prior; upper-bounded by `postTokens` from a previous `compact_boundary` |
| `N` | calls the session will still make **after** the first post-compaction call | predicted; §4 |

### 1.2 Cost of compacting (forward-looking only)

Three terms, all marginal against the do-nothing counterfactual:

```
cost = B·Cr                     (a) the /compact turn re-reads the conversation
     + S·Co                     (b) it generates the summary — OUTPUT prices
     + (A − P − Δ)·Cw           (c) the first post-compact call rewrites the new
                                    prefix instead of reading a cached one
     − (B − P)·Cr               (d) …but that first call no longer reads B
```

(d) is a credit, not a saving to be counted twice: it is the first call's share,
so `N` below counts calls *after* the first.

### 1.3 Benefit

```
saving_per_call = D·Cr ,  D = (B + Δ) − A
benefit(N)      = N · D · Cr
```

### 1.4 Break-even

```
                 B·Cr + S·Co + (A − P − Δ)·Cw − (B − P)·Cr
N* =  ───────────────────────────────────────────────────────
                            D · Cr
```

Divide through by `Cr` and it becomes rate-ratio arithmetic, which is the form
worth carrying in your head:

```
        P + (Co/Cr)·S + (Cw/Cr)·(A − P − Δ)
N*  =   ───────────────────────────────────
                   (B + Δ) − A
```

**COMPACT only if `N_predicted ≥ N* × safetyFactor`.**

Two structural facts fall straight out of it:

1. `A` is **roughly independent of `B`** — measured: the canary compacted 85,847
   and 67,663 into 57,803 and 61,099. The summarizer has its own output budget,
   so a bigger conversation does not produce a proportionally bigger summary. So
   `D` grows ~linearly with `B`, and `N*` is a hyperbola in `B`: it falls fast as
   the conversation gets big and goes to infinity as `B → A`.
2. The numerator is **almost constant**. It barely depends on `B` (the `P` term
   is all that survives). So the gate is essentially: *is the conversation big
   enough, given how many calls are left.*

### 1.5 The critical-context form (the form to implement)

Solve for `B`:

```
B*(N) = [ P + (Co/Cr)·S + (Cw/Cr)·(A − P − Δ) ] / N  +  A − Δ
```

With Opus-5 rates, `P` = 30,000, `S` = 4,700, `A` = 51,800, `Δ` = 2,500:

| calls remaining after the first | compaction pays only above |
|---:|---:|
| 3 | 218,000 |
| 5 | **150,600** |
| 10 | 99,900 |
| 15 | 83,100 |
| 20 | 74,600 |
| 30 | 66,200 |
| 50 | 59,400 |
| 100 | 54,400 |

The 150,000 threshold already in `UsageBudgetProfile.ContextPerCallTokens` turns
out to be the break-even context for **≈5 remaining calls**. That is a much
better accident than it looks (§6).

### 1.6 What this approximation hides — stated, not buried

| simplification | why it is acceptable | when it breaks |
|---|---|---|
| **uncached input ignored** | 99.2% of the measured input is `cache_read` (36,082,816 → 35,787,742 on `wf-1c2cb9bd`). | A gap longer than the cache TTL makes the next read cost `Ci` = **10× `Cr`**. That makes compaction *more* attractive, so pricing at `Cr` fails toward **not** compacting. Deliberate. |
| **cache creation charged once** | AO cannot see re-writes caused by TTL expiry mid-cycle. | A long, gappy cycle rewrites the prefix repeatedly; the modelled saving is then optimistic on both arms roughly equally. |
| **output of ordinary calls ignored** | It is identical in both arms — the work is the same work. Only the summary's output is marginal. | Never, under the replay assumption (§1.7). |
| **fixed per-call cost = 0** | No provider in the catalog charges one. | A provider with per-request pricing; the rate card has no field for it. Out of scope, and its absence must not be silently read as zero — see §5 fail-closed. |
| **the summary is the only generated text** | Measured: `S` ≈ 4,626 / 4,337 tokens on the two canary compactions. | A harness that regenerates a plan or re-reads files after compacting. Partly visible: the canary's first post-compact call rewrote 20,422 tokens while the summary was only 4,626 — the rest is re-attached files and preserved segment. That is why term (c) uses `A − P − Δ` (measured) and **not** `S`. |
| **call count held fixed** | Same assumption `turnbench` already makes and labels. | A compacted agent may need an extra turn to re-find something. `turnbench` §K.3 states this; nothing here improves on it. |
| **list price ≠ what is billed** | `pricing` says so in its own package comment: a Claude subscription's marginal cost is not this number. | The gate is a *relative* comparison between two arms priced from the same card, so a uniform multiplier cancels. A subscription with a hard cap does not cancel — see §12 risk 5. |

### 1.7 The term nobody was counting

**AO's ledger recorded no event for either compaction turn.** 28 events for
session `ao-canary-fixture-6`, none of them the summarization. The evidence that
it happened at all is in the transcript, not the ledger:

```
system / compact_boundary
  trigger: manual   preTokens: 85,847   postTokens: 10,956   durationMs: 95,073
  trigger: manual   preTokens: 67,663   postTokens: 13,240   durationMs: 87,393
```

So the canary's true Claude-side spend is **$0.30 higher** than the $2.31 the
ledger reports — a 13% understatement — and 3 minutes of wall clock went
unattributed. Any economic model built only from `model_usage_events` is
structurally blind to the largest single component of a compaction's cost. This
is a measurement gap, not a modelling choice; §10 and §13 list the fix.

---

## 2. The two real compactions of `wf-66f0ee54` (MEASURED)

Rates: Opus 5, `anthropic-list-price` v2026-06-24 — in 5.00, out 25.00, cache
read 0.50, cache write 6.25 per MTok. `Δ` from the durable
`fix_dispatch_intent.promptBytes` (4,724 and 4,246 bytes ÷ 4). `S` from the
summary record following each `compact_boundary`. `P` = 37,379, observed
identically on both post-compaction calls.

| | compact 1 | compact 2 |
|---|---:|---:|
| fired because | `context_pressure_actual` (threshold overridden to 60,000) | same |
| `B` (preTokens) | 85,847 | 67,663 |
| `A` (first post-compact call) | 57,803 | 61,099 |
| `Δ` (fix prompt + pack) | 1,181 | 1,062 |
| **`D`** | **29,225** | **7,626** |
| harness `postTokens` | 10,956 | 13,240 |
| `S` (summary generated) | 4,626 | 4,337 |
| rewritten at cache-write (`A−P`) | 20,422 | 23,718 |
| duration | 95.1 s | 87.4 s |
| saving per later call | $0.01461 | $0.00381 |
| (a) `/compact` read | $0.0429 | $0.0338 |
| (b) summary output | $0.1157 | $0.1084 |
| (c)−(d) first-call penalty | $0.0960 | $0.1265 |
| **total cost** | **$0.2546** | **$0.2687** |
| **break-even calls `N*`** | **17.4** | **70.5** |
| calls actually left | **6** | **4** |
| **net** | **−$0.167** | **−$0.253** |

Cycle totals, same calls priced both ways: **with compaction $1.2121, without
$0.7917 — compaction made the two repair cycles 53% more expensive.** Plus
$0.30 of spend AO never metered and 3 minutes of wall clock.

Why compact 2 is so much worse than compact 1 is the important part: by then the
conversation was **37,379 of stable prefix + 28,806 of compactable region**, and
most of that region was the *previous* summary plus the fix prompt AO itself had
just prepended. A summarizer cannot compress a summary. It returned 23,718 for
28,806 — a 17.7% reduction against compact 1's 57.0%. **Compacting a
recently-compacted conversation is close to a pure loss, always.**

### 2.1 Token savings ≠ cost savings, on the same two events

| | compact 1 | compact 2 |
|---|---:|---:|
| context reduction (vs `B + Δ`) | −33.6% | −11.1% |
| cumulative-input reduction over the cycle | −31.9% | −10.8% |
| **cost change over the cycle** | **+31.0%** | **+100.1%** |

Both compactions cut tokens. Both raised the bill. That is the whole thesis of
this document in one table.

---

## 3. The replay `wf-1c2cb9bd` (MODELLED, from the real split)

Measured, no compaction, Claude worker session only — 193 calls:

| | tokens | at list price |
|---|---:|---:|
| uncached input | 386 | $0.00 |
| cache read | 35,787,742 | $17.89 |
| cache write | 294,688 | $1.84 |
| output | 139,003 | $3.48 |
| **billed input** | **36,082,816** | |
| **total** | | **$23.21** |

(The whole run was 236 calls / 38,204,868 input; the 43 reviewer calls ran on
`gpt-5.6-sol`, which has **no rate** — see §5. Those 2,122,052 input tokens are
reported and uncosted, correctly.)

Replay with compaction at each of the three repair boundaries, modelled with the
canary's measured shape (`P` = 26,009 — this run's own first-call cache read;
`S` = 4,700; post-compaction `A` = 47,809; `Δ` = 2,500):

| | `B` | `A` | `D` | saving/call | cost | `N*` | calls left | net |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| compact 1 | 199,892 | 47,809 | 154,583 | $0.0773 | $0.251 | **3.2** | 29 | **+$1.99** |
| compact 2 | 251,148 | 47,809 | 205,839 | $0.1029 | $0.251 | **2.4** | 24 | **+$2.22** |
| compact 3 | 293,224 | 47,809 | 247,915 | $0.1240 | $0.251 | **2.0** | 19 | **+$2.10** |

**Modelled net: +$6.31 on a $23.21 run — 27%.**

Reconciliation with the published figures, since they differ and the difference
is the point:

| measure | value |
|---|---:|
| cumulative input, measured | 36,082,816 |
| cumulative input, replay (turnbench, `X`=57,404) | 21,997,829 → **−39.0%** |
| cumulative input, this reconstruction | 22,097,087 → −38.8% (0.45% off turnbench; different cycle-boundary attribution) |
| **cost**, metered terms only | −27.9% |
| **cost**, including the unmetered `/compact` turns | **−17.0% … −27%** depending on `S` |

So: **39% of the tokens, 17–27% of the money.** Roughly *half the headline
saving is an artefact of measuring in tokens.* `turnbench` is not wrong — it
measures what it says it measures (cumulative input) — but §K's table must never
be read as a cost reduction, and today it easily is.

One correction to `docs/p7-turn-economy.md` §K.2 while we are here: it assumed
"3,000 for the summary and fact pack". Measured, that number is **~20,000** of
rewritten prefix (of which ~4,600 is generated summary). The *level* it assumed
(57,404) turns out to be close to what the canary actually resumed at (57,803),
so the token figure survives; the decomposition does not, and it is the
decomposition that carries the cost.

---

## 4. Estimating remaining calls — the hard part

Preference order was: deterministic/structural → local history → cheap heuristic
→ **never an LLM call to decide whether to compact**. Nothing below calls a
model.

### 4.1 Signals AO already has at a fix boundary

| signal | source | available today? |
|---|---|---|
| calls this session has made | `SessionContextReading.ProviderCalls` | **yes** |
| last context, peak context | same | **yes** |
| current fix cycle `k` | `cascade.go` `cycleCount` | **yes** |
| `MaxFixCycles` (default 3) | frozen policy | **yes** |
| strategy (task / autonomous / master) | frozen policy | **yes** |
| calls per prior cycle *of this run* | `ListRunContextTrajectoryEvents`, grouped by `(role, cycle)` | yes, as rows; wants a fold |
| review pending / verify pending | step states | yes |
| findings count | `fix_dispatch_intent.findings.count` | yes |
| history across runs by strategy+phase | ledger | yes, but not folded |
| stable prefix `P`, session model id | — | **no — new fold needed** |

### 4.2 The estimator

Within-run, deterministic, no new storage:

```
predictedRemaining =
    k == 1 :  floor(0.25 × callsInCycle0)          # first repair cycle
    k >= 2 :  floor(0.80 × min(callsInCycle[1..k-1]))
```

then clamp: `min(predicted, callsRemainingBudget)` where `callsRemainingBudget`
is a cheap structural ceiling — `(MaxFixCycles − k + 1) × predicted` is *not*
counted (see below), so the clamp only ever lowers.

Validation against the only two runs that exist:

| boundary | callsIn(prev) | predicted | actual | error |
|---|---:|---:|---:|---|
| medusa cycle 1 | 118 (cycle 0) | 29 | 29 | exact |
| medusa cycle 2 | 30 | 24 | 24 | exact |
| medusa cycle 3 | min(30,25)=25 | 20 | 19 | +1, optimistic by 5% |
| canary cycle 1 | 16 (cycle 0) | 4 | 6 | −2, **conservative** |
| canary cycle 2 | 7 | 5 | 4 | +1 |

The 0.25 and 0.80 constants are anchored on exactly two runs and must be
documented as such — the same honesty `p7-turn-economy.md` §J applies to
`repeated_wait_check_shape`. They are the *lower* of the two observed ratios
(medusa 30/118 = 0.25, canary 7/16 = 0.44), so the anchor is the conservative
end of a two-point sample. That is thin. It is also survivable, because §9
shows the decision is 6–20× away from the boundary in both directions: the
estimator would have to be wrong by ~400% to flip a verdict.

### 4.3 What the estimator deliberately does NOT count

- **Calls in later fix cycles.** A compaction's `D` does persist into cycle
  `k+1` if no second compaction happens — so counting only the current cycle
  *understates* the benefit. Kept anyway: the alternative is claiming a benefit
  that the next COMPACT decision can annul.
- **`(MaxFixCycles − k) × expectedCallsPerCycle`.** Same reason, plus it would
  systematically favour compaction at cycle 1 of 3, where the estimate is
  weakest.
- **Verify re-entry.** A verify failure re-enters fix with more calls; unknown
  at decision time; not counted.

### 4.4 Fail closed

If `ProviderCalls == 0`, or the cycle-0 call count is unreadable, or the session
reading is not `Observable` — **do not compact.** The lifecycle decision stays
whatever it was and the existing `unknown_usage` reason code already exists to
say so. An unmeasurable economy is not a neutral economy.

---

## 5. Provider / model pricing

### 5.1 What exists

- `internal/observe/usage/pricing` — `Table`, `ModelRate{InputPerMTok,
  OutputPerMTok, CacheReadPerMTok, CacheWritePerMTok}`, `Catalog{Source,
  Version, EffectiveDate, Currency}`. Longest-prefix match on a normalized model
  id. **All four dimensions the formula needs are already there.**
- Embedded catalog: Anthropic only. Cache rates are *derived* (0.1× / 1.25×) and
  the `Source` string says so.
- Operator override: `~/.ao/usage-pricing.json`, whole-catalog provenance
  replacement, validated, `ErrRateCardInvalid` rather than a silent fallback.
- An unknown model returns `UsageCost{Known:false, Basis: CostUnknown,
  UnpricedModels:[…]}`. **There is no default rate and no zero.**
- `internal/workflow` already declares `UsagePricer{ Cost(modelID, tokens) }`
  and the Coordinator already holds one (`c.usagePricer`, wired in
  `daemon.go:usagePricing`).

### 5.2 The ratio table that matters

| model | in | out | cache read | cache write | `k = (Co+Cw)/Cr` | `Cw/Cr` |
|---|---:|---:|---:|---:|---:|---:|
| claude-opus-5 | 5.00 | 25.00 | 0.50 | 6.25 | 62.5 | 12.5 |
| claude-sonnet-5 | 2.00 | 10.00 | 0.20 | 2.50 | 62.5 | 12.5 |
| claude-haiku-4-5 | 1.00 | 5.00 | 0.10 | 1.25 | 62.5 | 12.5 |
| claude-fable-5 | 10.00 | 50.00 | 1.00 | 12.50 | 62.5 | 12.5 |
| **claude-fable-5-1 / mythos-5-1** | 10.00 | 50.00 | **0.25** | 12.50 | **250.0** | **50.0** |
| gpt-5.6-sol | — | — | — | — | **unpriced** | — |

Break-even in *calls* is model-independent across most of the card — which is a
coincidence of uniform cache multipliers, not a law. `fable-5-1` breaks it by 4×:
the same conversation needs **four times as many remaining calls** to justify
compaction. Hardcoding 62.5 would therefore be wrong on a model already in the
embedded catalog today. **Read the rates.**

### 5.3 Getting rates into the gate

`UsagePricer.Cost` returns a total, not rates. Two options:

- **(a) unit probes** — `Cost(model, {CacheReadTokens: 1_000_000})` returns
  exactly `CacheReadPerMTok`. Zero new surface, but it is a trick and reads like
  one.
- **(b) a narrow rate port** (recommended) — add
  `domain.ModelRateView{InputPerMTok, OutputPerMTok, CacheReadPerMTok,
  CacheWritePerMTok, Currency, Source, Version}` and `(*pricing.Table)
  .RateView(modelID) (domain.ModelRateView, bool)`; `internal/workflow` declares
  the matching optional interface and type-asserts, exactly as it already does
  for `ConversationCompactingSender` and `usageBudgetStore`.

Either way the gate also needs **which model this session is running**, which no
current read returns. Add `model_id` (most recent placeable call) to the
`GetSessionContextReading` fold.

### 5.4 Missing pricing → no economic decision

The rule, matching `usage_budget.go`'s own "a budget AO cannot measure does not
block":

```
rate missing, or currency mismatch, or any of the four rates is zero
    → NO automatic economic decision
    → decision falls back to the pre-economics behaviour for that run
```

and the fallback must be **skip, not compact**: the pre-economics behaviour is
what produced the canary's two losses. Concretely: `reason =
economics_unavailable_unpriced_model`, `decision = skip`, and the fix cycle is
delivered unchanged — which is exactly what a refused compaction already does
today, so no new failure mode is introduced.

For `gpt-5.6-sol` specifically: it is unpriced, so a Codex-hosted session can
never be compacted on economic grounds. That is the honest outcome and it costs
nothing today, because `ports.ConversationCompactor` is implemented by
`claudecode` only. An operator who wants Codex compaction must supply a rate
card — which is the documented, supported path, and it makes the provenance of
the decision auditable.

**Never invent a price. Never fall back to another model's rates. Never treat a
missing rate as zero.**

---

## 6. The 150,000 threshold — keep it, as a precondition

**Recommendation: C — context pressure AND economic benefit.** Concurs with the
initial preference, and the reason is sharper than "belt and braces":

1. `B*(5 calls) = 150,600`. The existing threshold *is* the economic threshold
   for a short tail. It was derived from a completely different argument (the
   measured run's own mean, 186,957, rounded down) and lands within 0.4% of the
   economic answer. Keeping it costs nothing and it is already the number the
   `context_per_call_above_profile` advisory shows a human — `cascade.go` calls
   that agreement "by construction rather than by two constants that happen to
   match today", and that property should survive.
2. **The threshold is cheap and total; the economics are expensive and
   estimated.** `B ≥ threshold` needs one indexed read. The economic gate needs
   rates, a prefix, a summary prior and a call forecast. Ordering the cheap
   certain test first means the expensive uncertain one runs only where it can
   change something.
3. **The threshold alone is not sufficient** — which is the case against option
   A. At `B` = 150,000 with `P` = 37,379 and the canary's own compaction ratio,
   `N*` ≈ 22. A run that reaches 150k on its last fix cycle with 5 calls left
   still loses money. The canary is the proof: it never came close to 150k and
   still compacted, because the threshold is overridable per run
   (`workflowContextPerCallWarnTokens: 60000`) — and an override is exactly the
   kind of lever an experiment reaches for. The economics must be the thing that
   cannot be switched off by lowering a number.
4. **Economics alone is not sufficient either** — the case against option B. As
   `B → ∞`, `N* → 1`, so a pure economic gate would compact a 300k conversation
   with 2 calls left. It would be *right* to, arithmetically. But `A` and `S` are
   predictions, and a gate that fires on a 2-call margin has no headroom for
   them being wrong. Context pressure is the sanity floor.

**Do not lower the default.** Do keep the per-run override, but make the
economic gate un-overridable by it: the override moves the precondition, never
the arithmetic.

### 6.1 And fix the other trigger

`DecideSessionLifecycle` reaches COMPACT on `many_fix_cycles`
(`FixCycleCount >= MaxFixCycles`) and `many_attempts` as well as on context
pressure. Those fire **at the last cycle** — precisely when the fewest calls
remain and amortization is least likely. On the default `MaxFixCycles: 3` the
`many_fix_cycles` branch is a machine for producing the canary's compact 2. The
economic gate must sit in front of *all three* reasons, not only context
pressure. Note that COMPACT-the-decision keeps its other meaning (prepend a
fresh fact pack), which stays unconditional and free; only the `/compact`
request is gated.

---

## 7. The production gate

### 7.1 Order of evaluation — cheapest and most certain first

```
 1. policy.SessionCompactionEnabled            else skip(policy_off)
 2. harness implements ConversationCompactor   else skip(no_vocabulary)
 3. not already requested this cycle           else skip(already_requested)   [exists]
 4. reading.Observable                         else skip(usage_unknown)
 5. B >= profile.ContextPerCallTokens          else skip(no_context_pressure)
 6. cooldown: lastCompactionCycle < k - 0      else skip(cooldown)            §7.3
 7. rates known for the session's model        else skip(unpriced_model)
 8. predicted A, D > 0 and D >= minReductionFraction·B   else skip(reduction_too_small)
 9. predictedRemaining >= N* × safetyFactor    else skip(insufficient_remaining_calls)
10. -> COMPACT
```

Every `skip` leaves today's behaviour byte-identical: the lifecycle decision is
still COMPACT, the fact pack is still built and prepended, the fix prompt is
still delivered. Only the `/compact` directive is withheld. That property is
already asserted by `TestFixCycleIsDeliveredWhateverHappensToTheCompaction` and
must not be weakened.

### 7.2 Prediction inputs, and their conservative defaults

| input | source | if unknown | direction of the default |
|---|---|---|---|
| `P` | session's first placeable `cache_read_tokens` | 0 | 0 ⇒ larger `(A−P)` ⇒ higher cost ⇒ **skip** |
| `A` | `P + expectedPost + expectedReattach + Δ` | `P + 25,000 + Δ` | high `A` ⇒ small `D` ⇒ **skip** |
| `expectedPost` | previous `compact_boundary.postTokens` for this harness, else prior 13,000 | prior | high ⇒ **skip** |
| `S` | prior 5,000/harness, upper-bounded by `expectedPost` | 13,000 (= `expectedPost`) | high ⇒ **skip** |
| `Δ` | the prompt AO is about to send, in bytes ÷ 4 | 0 | 0 ⇒ smaller `D` ⇒ **skip** |
| `N` | §4.2 | 0 | 0 ⇒ **skip** |

Every unknown pushes toward not compacting. There is no input whose absence
makes compaction look better.

### 7.3 Cooldown

Two mechanisms, both needed:

- **Per cycle** — already implemented via the `session_compaction_requested`
  checkpoint marker. Keep.
- **Cross cycle** — the canary's compact 2 was 4 calls and one cycle after
  compact 1 and lost $0.25. Rule: **do not compact a session whose conversation
  has not grown by at least `minGrowthSinceLastCompaction` (proposed: `2 × S`,
  i.e. the summary must at least have been earned back in new material) since
  the last compaction.** Detectable without new state: `PeakContextTokens` vs
  `LastContextTokens` already distinguishes "was compacted once and is small
  again" from "never grew", and the compaction checkpoints carry the cycle.

`minReductionFraction` (step 8) covers the same failure from the other side:
compact 2 predicted an 11.1% reduction; a floor of 25% would have rejected it on
that test alone, before any call forecast.

### 7.4 Sunk cost

**Never an input.** A compaction already paid for is gone; only the forward
`(cost, benefit)` pair decides the next one. Its *consequence* — a smaller
compactable region — enters correctly and only through `D`. Stating this because
the natural temptation after paying for compact 1 is to "make it worth it", and
that is precisely the reasoning that bought compact 2.

---

## 8. Telemetry

New durable phase `compaction_economic_decision`, written **before** the
compaction is attempted (same ordering rule as
`session_compaction_requested`: the record must not be lost by a crash), and
mirrored onto `SessionLifecycleDecision` so the run's own audit shows it.

```jsonc
{
  "version": "compaction-economics/v1",
  "step": "wfs-…", "cycle": 2, "session": "…",
  "model": "claude-opus-5", "harness": "claude-code", "provider": "anthropic",

  "contextBefore": 67663,
  "stablePrefix": 37379,
  "estimatedContextAfter": 61099,
  "estimatedReduction": 7626,
  "estimatedReductionPercent": 11.1,
  "promptTokens": 1062,

  "predictedRemainingCalls": 5,
  "remainingCallsBasis": "prior_cycle_min_discounted",
  "callsSoFarInSession": 23,
  "fixCycle": 2, "maxFixCycles": 3,
  "strategy": "task",

  "breakEvenCalls": 70.5,
  "safetyFactor": 1.5,
  "requiredCalls": 105.8,

  "estimatedRewriteCostMicros": 268700,
  "estimatedFutureSavingsMicros": 19065,
  "currency": "USD",
  "pricingSource": "anthropic-list-price + published 5m cache multipliers (0.1x read, 1.25x write)",
  "pricingVersion": "2026-06-24",
  "summaryTokensAssumed": 5000,
  "summaryTokensBasis": "harness_prior",

  "decision": "skip",
  "reason": "insufficient_remaining_calls"
}
```

Rules:

- **No prompts, no command text, no findings text, no secrets.** Every field is
  a number, an id, or a value from a closed enum — the same bar
  `usage_turn_class.go` holds, and it should get the same kind of test
  (`TestEconomicDecisionCarriesNoContent`).
- Costs in **integer micros**, not floats: a float in a durable payload makes two
  records from different builds incomparable.
- `reason` is a **closed enum** extending `SessionLifecycleReason`, which already
  has a `Valid()` contract: `economics_insufficient_remaining_calls`,
  `economics_reduction_too_small`, `economics_unpriced_model`,
  `economics_cooldown`, `economics_unmeasurable`, `economics_positive`.
- A **skip is recorded exactly like a compact.** The whole point is to be able to
  answer, months later: *"it did not compact because it needed ~106 calls to
  amortize and had 5."*
- Record the decision even when the policy knob is off (as
  `decision: "skip", reason: "policy_off"`), so an operator can see what the gate
  *would* have done on real runs before enabling anything. This is how the next
  batch of evidence gets collected without spending a dollar.

---

## 9. Offline simulation

Ran over every compaction that exists, using the cost model of §1 and the
estimator of §4.2. No LLM was invoked; no run was started.

| run | boundary | `N*` | predicted | actual | sf 1.0 | sf 1.25 | sf 1.5 | sf 2.0 | right call? |
|---|---|---:|---:|---:|---|---|---|---|---|
| canary (measured) | compact 1 | 17.4 | 4 | 6 | SKIP | SKIP | SKIP | SKIP | yes — it lost $0.17 |
| canary (measured) | compact 2 | 70.5 | 5 | 4 | SKIP | SKIP | SKIP | SKIP | yes — it lost $0.25 |
| medusa (replay) | compact 1 | 3.2 | 29 | 29 | GO | GO | GO | GO | yes — +$1.99 |
| medusa (replay) | compact 2 | 2.4 | 24 | 24 | GO | GO | GO | GO | yes — +$2.22 |
| medusa (replay) | compact 3 | 2.0 | 20 | 19 | GO | GO | GO | GO | yes — +$2.10 |

**Allowed: 3 (all three profitable). Rejected: 2 (both losses). Zero errors in
either direction, at every safety factor tested.**

Sensitivity to `safetyFactor` is *nil over [1.0, 2.0]*, and that is the finding —
not that 1.5 is finely tuned, but that **the decision is nowhere near the
boundary**:

| | margin `predicted / (N* × 1.5)` |
|---|---:|
| canary compact 1 | 0.15 (rejected by 6.5×) |
| canary compact 2 | 0.05 (rejected by 21×) |
| medusa compact 1 | 6.0 (allowed by 6×) |
| medusa compact 2 | 6.7 |
| medusa compact 3 | 6.7 |

The two populations are two orders of magnitude apart. The estimator would have
to be wrong by 400–600% to flip any of the five.

**This is five data points from two runs, and it is not optimization — it is the
whole population.** The gate was not tuned to fit them: the formula comes from
the rate card and the estimator constants come from the conservative end of the
same two runs. There is no held-out set, and §12 says so.

**Recommended safetyFactor: 1.5.** Not because 1.5 is special — 1.0 gives the
same five answers — but because `A`, `S` and `N` are all predictions with
one-sample priors, and 1.5 is the cheapest insurance against all three being
optimistic at once. It costs nothing on the observed population.

---

## 10. Files this would touch

No migration. No new table. The decision rides in the existing
`workflow_checkpoints.retry_state` payload, and the reads extend a query that
already exists. Given `sqlite-rebuild-check-incoming-fks`, that is deliberate.

**Domain (pure, most of the work):**
- `backend/internal/domain/compaction_economics.go` — **new.** The formula, as
  a pure function over an explicit input struct. No IO, no clock, table-tested.
- `backend/internal/domain/session_lifecycle.go` — new reason codes + `Valid()`,
  new fields on `SessionLifecycleDecision`, `SessionCompactionEconomicsVersion`.
- `backend/internal/domain/usage_trajectory.go` — `SessionContextReading` gains
  `StablePrefixTokens`, `LastOutputTokens`, `ModelID`.
- `backend/internal/domain/model_rate_view.go` — **new**, the rate view type.

**Workflow:**
- `backend/internal/workflow/session_compaction.go` — the gate in
  `maybeCompactBeforeFix`, the durable economic record, the cooldown read.
- `backend/internal/workflow/cascade.go` — `sessionContextPressure` becomes
  `sessionCompactionEconomics`; **and the fix prompt/pack must be built before
  the decision** so `Δ` is known. Today the order is the opposite and the
  comment explains why ("Ask BEFORE the pack is built") — that ordering is about
  *delivery*, not construction, so building early and sending late preserves it.
  This is the one non-trivial restructuring.
- `backend/internal/workflow/usage_budget.go` — the `UsageRateCard` optional
  port beside `UsagePricer`.
- `backend/internal/workflow/workflow.go` — dependency wiring.
- `backend/internal/workflow/remaining_calls.go` — **new**, the estimator.

**Storage:**
- `backend/internal/storage/sqlite/queries/usage_ledger.sql` — extend
  `GetSessionContextReading`; add `ListRunCycleCallCounts` (a fold, not rows).
  **ASCII only** (`sqlc-non-ascii-query-comments`).
- `backend/internal/storage/sqlite/gen/**` — `npm run sqlc`.
- `backend/internal/storage/sqlite/store/usage_attribution_store.go`.

**Pricing:**
- `backend/internal/observe/usage/pricing/pricing.go` — `RateView`.

**Measurement gap (§1.7) — separable, and arguably should land first:**
- `backend/internal/observe/usage/parser.go` — ingest `compact_boundary`
  metadata (`preTokens`, `postTokens`, `durationMs`; numbers only, no content),
  so `A` and `expectedPost` become *measured* per harness instead of priors.
  This is the single highest-value item in the list and it changes no behaviour.

**Surface:**
- `backend/internal/httpd/controllers/workflow_usage_view.go` + `dto.go` +
  `apispec` — expose the decision; `npm run api` (**check both generated files
  moved** — `npm-run-api-ts-silent-failure`).
- `frontend/src/api/schema.ts`, plus labels in all eight locales if the reason
  codes render.

**Benchmark and docs:**
- `backend/internal/observe/turnbench/turnbench.go` — a `Cost` fold beside
  `Measure`, so §0's table can carry a money column and stop being read as one.
- `docs/p7-turn-economy.md` — correct §K.2's 3,000-token assumption and add a
  pointer to this document from §0 and §E.
- `docs/README.md`.

---

## 11. Tests

**Domain, pure:**
1. Break-even arithmetic against the five historical boundaries — the table in
   §9 becomes a table test, with the expected `N*` pinned to 0.1.
2. `D <= 0` ⇒ skip, never a division by zero, never a negative `N*`.
3. Every missing input independently forces skip (§7.2, one case per row).
4. Rate-ratio sensitivity: the same conversation on `fable-5-1` needs 4× the
   calls. This is the test that fails if anyone hardcodes 62.5.
5. Costs are integer micros and round-trip through the payload unchanged.

**Estimator:**
6. The five historical predictions (§4.2) pinned exactly.
7. Zero prior cycles, zero calls, unobservable reading ⇒ 0 ⇒ skip.

**Workflow:**
8. `TestFixCycleIsDeliveredWhateverHappensToTheCompaction` extended with the new
   skip reasons — the delivery must stay byte-identical for all of them.
9. The gate is consulted for `many_fix_cycles` and `many_attempts`, not only
   `context_pressure_actual` (§6.1).
10. Cooldown: a session compacted in cycle `k` is not compacted in `k+1` without
    the required growth. **This is the canary's compact 2 as a regression test.**
11. The economic record is written before the request, exactly once per cycle,
    and survives restart without a duplicate (mirrors
    `TestCompactionIsRequestedAtMostOncePerCycle`).
12. With the policy knob off, the decision is still recorded and still skips.
13. `TestEconomicDecisionCarriesNoContent` — no prompt, path, command or finding
    text can reach the payload, asserted against a deliberately content-rich
    fixture (the `turn_class` test's own pattern).

**Pricing:**
14. Unpriced model ⇒ `economics_unpriced_model` ⇒ skip; **no cost is invented**.
15. An operator rate card changes the verdict, and its `Source`/`Version` appear
    in the record.
16. A rate card with a zero cache-read rate does not produce a division by zero
    or an infinite saving.

**Benchmark:**
17. `turnbench` cost fold reproduces §3's table (−39% tokens, −17…−27% cost) and
    **fails if the two are ever reported as the same number.**

Gate discipline from memory: `-race` on `internal/workflow` needs ~1100–1200s,
so it needs an explicit timeout, and `golangci-lint` must be run serially — an
empty, fast lint result in this repo is a blocked run, not a clean one. Neither
should be run beside a live AO on this host.

---

## 12. Risks

1. **`S` and `A` are priors from one session.** Two compactions of one fixture.
   A different project, a bigger CLAUDE.md, a different `--append-system-prompt`
   moves `P`, and a longer conversation may move `S`. Mitigation: ingest
   `compact_boundary` (§10) and the priors become measurements per harness —
   until then, the defaults are set high, which skips.
2. **The summarization turn is invisible to AO's ledger.** The gate charges for
   it from a model AO cannot verify against its own records. If the harness ever
   starts emitting that event, the cost would be double-counted. Mitigation:
   derive the charge from `compact_boundary` when present, from the prior
   otherwise, and never from both.
3. **The estimator is anchored on two runs.** 0.25 and 0.80 are a two-point
   sample. The margins in §9 are 6–20×, so it survives being quite wrong — but
   an autonomous or master run has no representation at all in that sample, and
   those are exactly the runs with the most to save. Mitigation: gate stays on
   Task-shaped evidence; record decisions for every strategy with the knob off
   and re-derive the constants when there are runs to derive them from.
4. **Building the prompt before the decision** reorders `applyFixLifecycleDecision`.
   The existing ordering comment is about delivery, but the code path is
   load-bearing and recovery-sensitive. Mitigation: build early, send unchanged,
   and keep the durable-record-before-request invariant exactly as it is.
5. **List price is not the bill.** On a subscription the marginal cash cost of
   these tokens is not $23.21, and compaction's real value there may be
   *staying under a context limit*, not money. The gate optimizes dollars. If
   the operator's actual constraint is a context ceiling or wall-clock, this gate
   optimizes the wrong thing — and will happily skip a compaction the user wanted
   for a non-economic reason. Mitigation: the gate is opt-in per run, the
   threshold precondition still exists, and a future explicit
   `compactionReason: "context_limit"` override should bypass the economics
   entirely rather than pretending to price it.
6. **Wall clock is not priced at all.** Each compaction cost ~90 seconds. On a
   run under a deadline that is a real cost the formula ignores; on a run under a
   budget it is free. Stated, not modelled.
7. **Replay-derived remains replay-derived.** The medusa +$6.31 holds the call
   sequence fixed. A compacted agent may re-read a file it forgot. §K.3's caveat
   applies unchanged and the gate does not make it less true.
8. **Two runs is not a population.** Every constant in this document could be
   re-derived from the third run that compacts. Nothing here should be read as
   validated.

---

## 13. Recommendation

**IMPLEMENT — in three separable steps, in this order.**

**Step 1 — measure, change nothing (no risk, highest value).**
Ingest `compact_boundary` metadata in the usage parser and add the `turnbench`
cost fold. This turns `A`, `postTokens` and the compaction's duration into
measurements, corrects the ledger's 13% blind spot on compacting runs, and stops
§0's 39% being read as money. It changes no behaviour and needs no policy.

**Step 2 — decide in the dark.**
Implement the formula, the estimator and the telemetry, wired so the record is
written on every COMPACT decision **but the verdict is not yet acted on** (the
existing knob stays off). Collect decisions from real runs. This costs nothing
and produces the held-out set §9 does not have.

**Step 3 — act, opt-in only.**
Only once step 2 has produced decisions on runs that are not the canary and not
medusa, let the verdict gate the `/compact` request, at `safetyFactor` 1.5, with
`SessionCompactionEnabled` still default-off and the 150,000 precondition intact.

And one item that is **IMPLEMENT NOW regardless**: §6.1 — the `many_fix_cycles`
and `many_attempts` branches request compaction at the point of minimum
remaining calls. Even with no economics at all, those two should not reach
`/compact`. That is a two-line change with a measured justification, and it is
the change that would have prevented the canary's compact 2.

**DO NOT** turn `sessionCompactionEnabled` on globally. **DO NOT** treat the
39% as a cost saving. **DO NOT** run the live A/B — every question it would have
answered is answered above from records that already existed, except the two
priors, and those are better answered by step 1 for free.

---

## Appendix — provenance of every figure

| figure | source |
|---|---|
| 193 calls, 36,082,816 input, 35,787,742 cache read | `model_usage_events` via `usage_event_attribution`, run `wf-1c2cb9bd` |
| $23.21 | that split × embedded catalog v2026-06-24 |
| 21,997,829 / −39.0% | `docs/p7-turn-economy.md` §0, `turnbench` fixture |
| 22,097,087 / −38.8% | this document's reconstruction of the same replay from the ledger |
| 84,897 → 57,803, 66,185 → 61,099 | `model_usage_events`, session `ao-canary-fixture-6` |
| preTokens 85,847 / 67,663, postTokens 10,956 / 13,240, durationMs 95,073 / 87,393 | `compact_boundary` records in the session transcript |
| `S` = 4,626 / 4,337 | length of the summary record following each boundary, ÷ 4 bytes/token |
| `Δ` = 1,181 / 1,062 | `fix_dispatch_intent.promptBytes` 4,724 / 4,246, ÷ 4 |
| `P` = 37,379 | `cache_read_tokens` of both post-compaction calls, identical |
| threshold 60,000 | the canary's frozen `usage.workflowContextPerCallWarnTokens` |
| rate ratios | `internal/observe/usage/pricing/pricing.go` embedded catalog |
| everything else | derived arithmetic, shown |

4 bytes/token is the same estimator `p7-turn-economy.md` §F uses, and it is an
estimate: it moves `S` and `Δ`, not `B`, `A`, `D` or `P`, all of which are
provider-reported.
