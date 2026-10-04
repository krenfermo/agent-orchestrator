# Cache-creation lifetime accounting — P7.2B1

**Measurement and pricing only.** No lifecycle change, no compaction decision,
no shadow gate, no reordering of `applyFixLifecycleDecision`, no estimator, no
safety factor, nothing enabled.

---

## 1. The defect

Creating a cache entry that lives for an hour costs more than creating one that
lives for five minutes. The provider reports which one it made, on every
message. AO folded both into `cache_creation_input_tokens` and priced all of it
at the cheap rate.

The embedded catalog was never wrong about what it held — its own `Source`
string has said *"published 5-minute-TTL cache multipliers ... 1.25x write"*
since the day it was written. It simply had no row for the other one, and
nothing measured which one was being bought.

## 2. The real contract

Scanned every Claude transcript AO has a source for: **4,523 billed assistant
messages across 75 files.**

```jsonc
"usage": {
  "input_tokens": 2,
  "cache_read_input_tokens": 82702,
  "cache_creation_input_tokens": 2193,
  "output_tokens": 950,
  "cache_creation": {
    "ephemeral_5m_input_tokens": 0,
    "ephemeral_1h_input_tokens": 2193
  }
}
```

| | |
|---|---:|
| messages carrying `cache_creation` | **4,523 of 4,523 (100%)** |
| distinct bucket shapes seen | **1** — exactly `ephemeral_5m` + `ephemeral_1h` |
| messages where the buckets do not sum to the total | **0** |
| cache creation at the 5-minute lifetime | 200,678 (2.08%) |
| **cache creation at the 1-hour lifetime** | **9,444,101 (97.92%)** |
| messages mixing the two | **0** |

No other bucket exists in any file. No legacy shape exists on disk — which does
not mean the code may assume one, only that the compatibility path is untested
by reality and is therefore tested by fixture.

## 3. What was built

**Parser.** `cache_creation` is decoded as a POINTER, because absent is a
different fact from two zeroes and only a pointer can tell them apart. The
buckets are validated against the total on every message:

- they agree -> the split is carried;
- they disagree -> the total stands, the lifetime is recorded as **unknown**,
  no distribution is invented, and the record raises a new anomaly code
  `cache_ttl_inconsistent`. The event is **not dropped**: its tokens are real
  and the ledger still needs them.

**Domain.** `CacheCreationSplit{Ephemeral5m, Ephemeral1h, UnknownTTL}` on both
the event vector and the summed one. `UnknownTTLTokens` is what makes it
composable: summing two splits is summing three int64s, a legacy contribution
stays unknown in exactly the part that is, and there is no boolean algebra to
get wrong. It is a **split of** `CacheWriteTokens` and never an addition to it.

**Pricing.** `ModelRate` gains `CacheWrite1hPerMTok`. `CacheWritePerMTok` keeps
its name because that is what it always meant. Rates stay explicit per model
row; no multiplier constant appears anywhere in code.

**Session aggregate.** Because the per-event row cannot hold the split (§5), the
parser also sums the lifetimes per session into its own durable state, once per
billed message — accumulated on the message-id transition, since one message
arrives as several records and a per-record sum would count the same writes
three times.

## 4. The rule for a lifetime nobody reported

Two failure modes, kept apart because they are different facts:

| | `Known` | field |
|---|---|---|
| the rate card has no long-lifetime rate, and the vector has long-lived creation | **false** | `TTLUnknownTokens` |
| nothing reported which lifetime, so it was priced at the short rate | true | `TTLAssumedTokens` |

**The second one is a compromise and it deserves its paragraph.** Refusing to
price a lifetime-unknown write is the purer rule and it is what the brief asked
for. It would also have blanked **every cost figure in the product** the day it
shipped: `model_usage_events` has no column for the split, so every ledger read
goes back through a row with a total and no lifetime. So the assumption is kept,
its exact size travels on the cost as `TTLAssumedTokens`, and **a caller that
must not accept it checks that field and refuses for itself** — which turnbench
now does, in all three of its costing paths. When the per-event columns exist,
`TTLAssumedTokens` goes to zero on its own.

No path assumes 1h. No path assumes 5m silently.

## 5. MIGRATION 0170 - approved and applied to the schema (P7.2B1.1)

**`0170_usage_cache_ttl.sql`.** 0170 was verified free across all 68 branches in
this tree, not merely free on ECC -- the `shippedMigrations` ledger exists
because a number claimed twice has already cost this project an outage, and the
claim is registered there in the same change.

```sql
ALTER TABLE model_usage_events ADD COLUMN cache_write_5m_tokens INTEGER;
ALTER TABLE model_usage_events ADD COLUMN cache_write_1h_tokens INTEGER;
-- plus a rebuild of the usage_event_attribution VIEW to project them
```

**Nullable, and no DEFAULT.** NULL means the lifetime was never observed. `0`
would mean no short-lived entry was created -- a claim about the tokens, and on
97.9% of this corpus a false one. `DEFAULT 0` would have made every historical
row assert exactly the thing this checkpoint exists to stop AO asserting. The
distinction is pinned by `TestMigration0170LeavesHistoricalRowsNull`, which also
checks that `SUM` over only NULLs stays NULL rather than collapsing to zero.

**Purely structural.** The migration reads no file, estimates nothing,
apportions no aggregate and assumes neither lifetime; `TestMigration0170IsStructuralOnly`
compares every pre-existing column before and after. No table rebuild: two
`ADD COLUMN`s and a view, which is a projection and not a rebuild -- the same
reasoning 0169 used on the same view. No CHECK ties the pair to the total
because SQLite cannot add one without the rebuild AGENTS.md warns about; the
write path enforces it instead.

**Write path.** `cacheLifetimeColumns` persists the pair only when the split is
complete AND sums to the total it accompanies. Three cases write NULL and all
three are the same statement: no `cache_creation` block, a split the parser
could not reconcile, and creation reported without a lifetime. A contradictory
split is REFUSED rather than stored beside the total it contradicts -- the
parser already raised the anomaly, and putting the contradiction in the ledger
would let every later sum inherit it.

**Dedupe.** `usageEventMatches` deliberately does NOT compare the lifetime
columns. A row written before 0170 carries NULL and the same call re-read today
carries a split; that is the same event better measured, not a conflict, and
comparing them would make every pre-0170 row raise `source_event_conflict` on
the next pass over an unchanged transcript.

**Read path.** Every aggregate -- run, project, role, cycle, family, session --
now returns three figures instead of one: `SUM(5m)`, `SUM(1h)`, and
`SUM(cache_write_tokens) WHERE the pair IS NULL`, which is exactly
`UnknownTTLTokens`. The per-event trajectory read projects the nullable pair
through the view and maps NULL to unknown, never to zero.

**Round trip, measured.** `TestTheMeasuredSessionsReconcileThroughTheDurablePath`
writes both known runs' real vectors through the real ingest path and prices
what comes back: 93.6% -> 98.0% and 67.9% -> 80.1% explained, identical to the
offline arithmetic in section 6.

### What P7.2B1 said before the migration existed

**A per-event lifetime needs two nullable INTEGER columns on
`model_usage_events`. I did not create the migration. This is the STOP the
brief asked for.**

Why it is needed: every cost AO reports for a run, a role, a cycle or a project
is a SUM over `model_usage_events`, and the row has nowhere to put the split. So
the ledger keeps pricing long-lived writes at the short rate and discloses it
through `TTLAssumedTokens`, while the parser holds the true figure one layer
above it.

What P7.2B1 delivers without it:

| consumer | grain | lifetime-correct now? |
|---|---|---|
| `turnbench` | per call, from fixtures | **yes** |
| compaction accounting (`CompactionReader`) | per session, from parser state | **yes**, when the parser aggregate and the ledger agree on the total |
| `LedgerReader` — run / project / role / cycle costs | per event | **yes, for events written after 0170**; rows written before it stay lifetime-unknown until a backfill |

Why I did not force it another way: apportioning a session aggregate across
events would be inventing a per-event distribution, and the brief forbids
exactly that.

The migration, if approved, is additive and back-fillable from transcripts that
are still on disk. It is two columns and a parser-to-store mapping; nothing
about the shape above changes.

## 6. Reconciliation

Both measured sessions, priced before and after, against the harness's own
end-of-session figure. Every number is measured.

### wf-1c2cb9bd / `medusa-12` — 193 calls, never compacted

| | |
|---|---:|
| cache creation | 294,688 — **5m: 0, 1h: 294,688** |
| AO cost before | $23.2127 |
| **AO cost after** | **$24.3178**  (+$1.1051, **+4.8%**) |
| harness cost-state | $24.8056 |
| explained before | 93.6% |
| **explained after** | **98.0%** |
| gap closed | **69.4%** of it |

### wf-66f0ee54 / `ao-canary-fixture-6` — 28 calls, compacted twice

| | |
|---|---:|
| cache creation | 110,284 — **5m: 0, 1h: 110,284** |
| AO cost before | $2.3118 |
| **AO cost after** | **$2.7254**  (+$0.4136, **+17.9%**) |
| harness cost-state | $3.4029 |
| explained before | 67.9% |
| **explained after** | **80.1%** |
| gap closed | **37.9%** of it |

**Neither gap is forced to zero, and the residue is the point.** What is left —
$0.49 on the control, $0.68 on the canary — is spend outside the calls. On the
control that is retries and a generated title; on the canary it is dominated by
the two summarization turns, which is P7.1's subject and not this checkpoint's.
The pricing error closed; the attribution gap did not, and should not have.

## 7. Effect on P7.1's own figures

Every economic number published so far was computed at the short-lifetime rate
and is therefore an understatement.

| | before | **after** |
|---|---:|---:|
| `wf-1c2cb9bd` replay, ANTES calls | $23.2127 | **$24.3178** |
| `wf-1c2cb9bd` replay, DESPUES calls | $16.7253 | **$18.1600** |
| TOKEN SAVINGS | 38.8% | 38.8% (unchanged — it is a token figure) |
| **COST SAVINGS** | 25.1% | **21.2%** |
| canary calls | $2.3118 | **$2.7254** |

The token saving does not move, because tokens are not money. The cost saving
falls again, for the reason it fell the first time: **a compaction rewrites a
prefix, and a rewritten prefix is the dearest thing in the rate card** — now 20x
a cache read on Opus 5 rather than the 12.5x the earlier arithmetic assumed.

`docs/p7-2a-shadow-economic-gate.md` §1.1 predicted this figure from the
harness's own cost line. It is now measured by AO's own code.

## 8. Files

| file | what |
|---|---|
| `backend/internal/domain/usage_ledger.go` | `CacheCreationSplit`; the split on `UsageTokenTotals`; `TTLUnknownTokens` / `TTLAssumedTokens` on `UsageCost`; both folded by `Add`. |
| `backend/internal/domain/usage.go` | the split on `UsageTokenMetrics`; `UsageErrorCacheTTLInconsistent`. |
| `backend/internal/observe/usage/parser.go` | decode, validate, carry, and the per-session aggregate. |
| `backend/internal/observe/usage/compaction.go` | the aggregate on the observations and the apportionment guard. |
| `backend/internal/observe/usage/pricing/pricing.go` | `CacheWrite1hPerMTok`, lifetime-aware `Cost`, catalog rows, provenance. |
| `backend/internal/observe/turnbench/turnbench.go` | per-call split, `CacheTTLKnown`, the replay's stated lifetime. |
| `backend/internal/observe/turnbench/cost.go` | `cache_ttl_unknown`, three refusal points, the split in `Report`. |
| `backend/internal/observe/turnbench/testdata/*.json` | both fixtures regenerated **from the same transcripts**, matched event-for-event against the ledger with zero vector mismatches. |
| tests | `internal/observe/usage/cache_ttl_test.go`, `internal/observe/usage/pricing/cache_ttl_test.go`, plus updates. |

**No migration. No API, DTO, OpenAPI, frontend or locale change.**

## 9. Risks

1. **Historical rows are still short.** Events written before 0170 carry no
   lifetime and are priced at the short rate with `TTLAssumedTokens` set: 3,381
   rows across 78 sessions, 7.5M cache-creation tokens. New events are exact.
   Section 10 says what a backfill would and would not be able to fix.
2. **The 1-hour rates for models AO has never metered are derived, not quoted.**
   Opus 5's is confirmed to six significant figures against the harness's own
   number; the rest follow the same 2x multiplier the vendor publishes, exactly
   as the 5-minute column already did. The `Source` string says so.
3. **`TTLAssumedTokens` is a disclosure, and a disclosure only works if someone
   reads it.** Nothing renders it yet.
4. **The per-session aggregate can drift from the ledger** on a re-read, which
   is why the accounting checks the two totals agree before apportioning and
   falls back to unknown when they do not.
5. **`claude-opus-5[1m]` is untouched** and still unpriced. Separate problem,
   deliberately left alone.


---

## 10. Backfill: analysed, not implemented

**Should it exist?** Probably yes, as `ao usage backfill-cache-ttl`, and it is
cheap -- but it is not urgent, because nothing downstream needs history to be
exact. The shadow gate judges live runs.

**How much is reconstructible:**

| | rows | cache creation |
|---|---:|---:|
| Claude events whose transcript is still on disk | **3,381** | 7,499,849 |
| Claude events whose transcript is gone | 104 | 144,662 |
| Codex events (no cache creation at all) | 389 | 0 |
| events with zero cache creation (nothing to fix) | 396 | 0 |

So ~97% of the affected rows could be reconstructed today. That number falls
every week a transcript is rotated, which is the only argument for doing it
soon.

**How it would have to work.**

1. **Key on `source_event_key`, never on position.** The key is already the
   exactly-once identity and is derived from the artifact, the source kind, the
   native session and the message id -- it is reproducible from the transcript
   without a clock. Re-deriving it and matching gives a row-to-message join that
   cannot drift.
2. **Verify before writing.** A candidate row is updated only when the
   transcript message it matched agrees on the ENTIRE token vector -- uncached,
   read, total creation, output. A transcript that has been rotated or
   truncated then matches nothing rather than matching the wrong thing, and the
   `cache_creation` buckets must still sum to the stored total or the row is
   skipped.
3. **Idempotent by construction.** `UPDATE ... WHERE cache_write_5m_tokens IS
   NULL` -- it can only ever move a row from unobserved to observed, never
   rewrite an observation, and re-running it is a no-op. That single predicate
   is what makes it safe to run twice, or half-way, or after a crash.
4. **Never widen.** No estimation, no apportioning a session aggregate, no
   assuming the dominant lifetime, no filling a row whose transcript is gone.
   A row that cannot be verified stays NULL, which is already the honest answer.
5. **Report, do not decide.** It should print how many rows it matched, skipped,
   and could not find a transcript for, and change nothing else.

**Not a prerequisite for P7.2B2.** Shadow verdicts are about runs happening now,
whose events carry the lifetime from the moment 0170 lands.
