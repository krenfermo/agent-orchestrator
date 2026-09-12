# turnbench fixtures — what they are and where they came from

Every file here is **numbers and a closed vocabulary**: context tokens, the
billed split, output tokens, a turn class, a segment label, and — since P7.1 —
compaction boundaries and the unattributed residual. No prompt, command, path,
finding or message body is reproduced in any of them, and the structs they
decode into have no field that could hold one.

## `wf-1c2cb9bd.json` — the original P7 recording. **Unmodified.**

The shape of the worker session of run `wf-1c2cb9bd-6380-4491-918c-37e34b5f229c`
as P7 recorded it: 193 calls, `contextTokens` / `outputTokens` / `class` /
`segment` / `observedAt`. It carries **no billed split**, which is exactly why
P7.1 exists — a series like this can answer every token question and no money
question at all, and `turnbench.CostOf` refuses to price it with
`split_unknown` rather than pricing it wrong.

It is left byte-for-byte as it was. `TestWorkedExampleMatchesTheRecordedLedger`
still pins it against the ledger and still passes.

## `wf-1c2cb9bd-split.json` — derived, new in P7.1

The **same 193 calls of the same session**, with the three billed dimensions
this package now needs. Generated from AO's own ledger:

```sql
SELECT w.role, w.cycle, e.model_id,
       e.input_tokens, e.uncached_input_tokens,
       e.cache_read_tokens, e.cache_write_tokens,
       e.output_tokens, e.turn_class, e.observed_at
FROM usage_attribution_windows w
JOIN usage_event_attribution e ON e.window_id = w.id
WHERE w.workflow_run_id = 'wf-1c2cb9bd-6380-4491-918c-37e34b5f229c'
  AND e.model_id LIKE 'claude%'
ORDER BY e.observed_at ASC, e.event_id ASC;
```

Totals, which `TestSplitFixtureMatchesTheRecordedLedger` pins:

| | |
|---|---:|
| calls | 193 |
| cumulative input | 36,082,816 |
| uncached input | 386 |
| cache read | 35,787,742 |
| cache write | 294,688 |
| output | 139,003 |

**One documented difference from the original fixture.** The segment labels are
derived here from the attribution window's own `(role, cycle)`, and they split
the repair cycles at 30 / 25 / 20 calls where the original file says 32 / 24 /
19. The call set, their order and every total are identical — only where one
dispatch is said to end and the next to begin moves, by one or two calls, because
the two files derive the boundary from different rows. Neither is wrong and
neither was edited to match the other; the difference is recorded here so a
reader comparing the two tables is not left to guess.

`unattributed` is the measured residual for this session: the harness's own
end-of-session `cost-state` rollup minus what the usage pipeline attributed.
**This run never compacted, so it is the control**: 121 unattributed output
tokens across 193 calls. Compare with the canary below.

## `wf-66f0ee54.json` — the canary, new in P7.1

The worker/fix session of run `wf-66f0ee54-52ff-4bc4-910e-20b570d4c741`, the
only run that has ever compacted. Same query as above. 28 calls, 1,829,810
cumulative input, 30,500 output.

`compactions` are the harness's own `system` / `compact_boundary` records, read
from the session transcript:

| | preTokens | postTokens | durationMs |
|---|---:|---:|---:|
| compact 1 | 85,847 | 10,956 | 95,073 |
| compact 2 | 67,663 | 13,240 | 87,393 |

`summaryOutputTokens` is **deliberately absent**: the harness does not report
what a compaction generated per boundary, and counting the characters of the
summary would mean reading the summary. The session-level `unattributed` figure
is the measured answer instead, and it is 16,953 output tokens — against the
control's 121.

## The unattributed model id

Both `unattributed` blocks name the model **`claude-opus-5[1m]`**, because that
is the spelling the harness rolls a session up under even when its own per-call
records say `claude-opus-5`. It is carried verbatim and never rewritten to the
base model: a long-context variant is priced differently, so substituting the
base rate would be inventing a price. The embedded catalog does not cover it, so
against production pricing these residuals are reported in **tokens with an
unknown cost** — which is the behaviour `TestTheRealResidualIsUnpricedInProduction`
pins, and which one row in an operator rate card fixes.
