# Context-selection metrics and the disabled-vs-enabled regression gate

The [context router](context-router.md) sends an agent less than AO used to.
This document covers the two things that have to exist before that is a claim
anyone should believe:

1. **The measurement.** Every per-dispatch [baseline evidence
   record](project-memory-baseline.md) can now also say what the router decided:
   how much context the dispatch could have been sent, how much was sent, and
   how much of what was sent came out of a store AO had already built rather
   than being read for this dispatch.
2. **The gate.** A harness runs one fixture task through dispatch twice — router
   off, then router on — and fails if the routed run reaches a *different* task,
   review, or verify outcome, no matter how much context it saved.

The second one is the point. A payload 75% smaller that changes what the
pipeline concludes is not an optimisation; it is a regression with a flattering
number attached.

## The routing block

`observe/projectmemory.RoutingMetrics`, recorded as `EvidenceRecord.Routing`.

It is **additive**. The field is a pointer with `omitempty`, `EvidenceSchemaVersion`
is unchanged (`project-memory-baseline/v1`), and a record from a dispatch with
no routing story serialises byte-for-byte as it did before this block existed.
Consumers written against the old schema keep reading every record. The block
carries its own `RoutingSchemaVersion` (`context-router-metrics/v1`) so it can
evolve without versioning the record around it.

| Field | Meaning |
| ----- | ------- |
| `enabled` / `reason` | Whether a router selection shaped this payload, and — when it did not — which of the ways it did not (flag off, no router wired, no resolvable checkout root, a selection that failed and fell back). |
| `role` / `tier` | The role budget that applied and how deep retrieval went (`compact` / `expanded`). |
| `sections` / `dropped` / `truncated` | How many blocks were sent, how many candidates the budget could not hold, and how many sent blocks were cut. |
| `potentialBytes` / `potentialTokens` | Everything the dispatch could have been sent: every candidate at **full length**, before any retrieval cap or budget. |
| `selectedBytes` / `selectedTokens` | What was actually sent. |
| `reusedBytes` / `reusedTokens` | The part of what was sent that came from AO's own stores — the [code graph](code-graph.md) and [project memory](project-memory-store.md). |
| `newBytes` / `newTokens` | The rest: content read for this dispatch (caller documents, the diff, the task statement). |
| `limitTokens` / `hardCapTokens` | The token target packed against and the role's cap. |

`reusedBytes + newBytes == selectedBytes`, always. The split is what separates
"AO sent less" from "AO made the agent go and fetch it itself".

Every byte figure is **measured** and every token figure is **estimated** (the
same `bytes / 4` heuristic the baseline uses, named in each metric's `method`),
and each metric says which it is. The record's existing labeling rule is
enforced over the routing block too, so a routing figure that violated it fails
the write rather than reaching disk.

`potentialBytes` is measured against `Section.SourceBytes` — the content a
section was built from *before* the compact tier's per-document cap cut it —
because comparing a packed block with itself would report a saving of zero for
a document that was already halved during retrieval.

### A dispatch the router did not shape

It still gets a block, provided the surface carries a payload to size:
`enabled: false`, a stated `reason`, `potential == selected == ` the payload
that went out, and `reused: 0` **measured** (nothing was drawn from AO's stores,
which is a fact about that dispatch, not a gap). The budget fields are
`unavailable` rather than zero. A dispatch that carries no payload at all — a
verify command run — gets no block, and the key stays absent.

### How the block reaches the record

The two dispatch wrappers stay independent, and either can be installed without
the other:

- `contextrouter/wfrouter` decides what to send. It puts its decision on the
  call's `context.Context` (`projectmemory.WithRouting`).
- `observe/projectmemory/wfdispatch` records what was sent. It reads the block
  off the context (`Span.ObserveRoutingFromContext`).

The daemon wires the recorder first, so the router wrapper sits outside it and
its decision reaches the record of the payload it produced. With only the
recorder installed, the context carries nothing and the record says so.

## Summing it across a run

`backend/internal/observe/context_metrics.go` (`observe.SummarizeContextSelection`)
is the read model over those per-dispatch blocks: it folds a set of evidence
records into one run-level picture — how many dispatches the router shaped, and
the sent / potential / selected / reused / newly-read byte totals.

Summing is where the measured-versus-estimated discipline is easiest to lose, so
it enforces two rules in one place instead of at each call site:

- **Only measured bytes are added.** A size a dispatch could not measure
  contributes nothing and increments `Unmeasured` instead, so `Complete()`
  reports whether the totals are exact or a lower bound. A verify command run,
  which carries no payload at all, is the ordinary reason a run is incomplete.
- **No token figure is ever summed.** Token counts in the evidence are a mix of
  provider-reported (measured) and byte-derived (estimated); adding those
  together yields a number that is neither. `observe.EstimatedTokens(bytes)`
  derives a token view from a byte total instead, and its name makes every call
  site say that it is an estimate.

`ReductionPercent()` and `ReusedSharePercent()` both return `ok=false` rather
than `0` when nothing measured supports them. The regression harness below reads
its run totals from this summary rather than summing the records itself.

## The regression harness

`backend/internal/observe/ctxregress`, driven by `backend/cmd/aoctxregress`.

```bash
cd backend
go run ./cmd/aoctxregress                       # self-contained fixture checkout
go run ./cmd/aoctxregress -repo /path/to/repo   # route against an existing checkout
go run ./cmd/aoctxregress -evidence-dir DIR     # also persist both runs' records
```

It runs **one** fixture task through the real dispatch wrappers twice — router
absent, then the router AO ships (`contextrouter.Default`, the same constructor
the daemon's composition root calls) — and compares:

- **Outcome is the gate.** If the task, review, or verify status differs between
  the runs, the command exits non-zero. It is a *difference* check, not a
  "did it get worse" check: an outcome that moved in either direction means
  routing changed what the pipeline decided.
- **Size is the report.** The measured context-reduction percentage is printed
  next to both runs' tool-call and file-read counts, so a saving is always read
  alongside what it cost.

```
router disabled  task=completed review=approved verify=passed
                 dispatches=4 contextSentBytes=61569 potentialBytes=61569 reusedBytes=0
                 toolCalls=3 fileReads=0

router enabled   task=completed review=approved verify=passed
                 dispatches=4 contextSentBytes=15465 potentialBytes=62159 reusedBytes=400
                 toolCalls=3 fileReads=0

measured context reduction: 74.9% (61569 -> 15465 bytes handed to the agent)

VERDICT: no quality regression — the routed run reached the same task, review, and verify outcomes.
```

### What is real here, and what is not

Real: the dispatch wrappers, the router and its budgets, the git-diff /
code-graph / project-memory evidence sources, the prompt builders the daemon's
own dispatchers call, and every byte count — those are measured on the payloads
that actually went out.

Not real: the agent. **No provider is called.** The fixture agent is a
deterministic stand-in whose outcome depends on exactly one thing: whether the
context it was handed still carried the facts the fixture task cannot be
completed without. Those facts live only in the fixture's tracker context and
nowhere in the checkout, so an agent that is not handed them cannot go and read
them — exactly like a decision recorded in an issue.

That is what makes the gate a test of routing rather than of a model's mood, and
it is also the limit of what a passing run means: *routing preserved this task's
evidence*, not *routing is safe for every task*. When a fact is dropped, the
harness counts the reads the agent spent looking for it, so a "saving" bought by
making the agent search shows up as reads rather than disappearing.

## Tests

- `backend/internal/observe/projectmemory` (`routing_test.go`) — the additive
  wire format (a record without routing emits no `routing` key and keeps its
  schema version), the measured/estimated labeling of every routing size, the
  disabled block, the reduction figure's refusal to invent a percentage, and the
  context carrier between the two wrappers.
- `backend/internal/observe` (`context_metrics_test.go`) — that only measured
  bytes are summed, that an unavailable size counts as unmeasured rather than as
  zero, and that no percentage is invented without a measurement behind it.
- `backend/internal/contextrouter` (`metrics_test.go`) — the indexed/read origin
  split, considered-versus-selected sizing against pre-retrieval size, and the
  conversion to a routing block.
- `backend/internal/observe/ctxregress` — that the shipped router sends
  materially less and changes no outcome, that a budget too small to carry the
  task's evidence is reported as a regression *despite* saving more, and that
  both runs' records carry the routing block. This is the build gate: it runs
  under `go test ./...`.
- `backend/cmd/aoctxregress` — that a clean comparison exits zero and still
  reports what it measured.
