# Project memory in the execution cycle (P2-B)

P2-A built durable project memory and left it as a capability nothing reached
for. P2-B makes it part of the normal cycle: indexed on first use, kept current
incrementally, and attached to Planner, Worker, Reviewer and Repair
automatically — under an explicit rollout switch, with measured before/after
numbers.

Design and storage: [`project-memory.md`](project-memory.md). The context
inventory both rest on: [`p2-project-memory-audit.md`](p2-project-memory-audit.md).

---

## 1. Rollout modes

`AO_MEMORY_MODE`, **default `off`**.

| Mode | What it does | Risk |
| --- | --- | --- |
| `off` | Nothing. Byte-for-byte the pre-P2-B behaviour. | none |
| `assisted` | **Adds** a bounded pack; every legacy source is still sent. | cost only — it can only grow a payload |
| `preferred` | Additionally **replaces** a legacy document, but only where equivalence and freshness are proved *per document*. | needs the measurement below |

Three modes rather than a boolean because *adding* memory and *replacing*
context need different amounts of evidence. There is no mode in which memory
replaces something on the strength of the mode alone: `preferred` is a
necessary condition, never a sufficient one.

`assisted` still reports what `preferred` *would* save (`CoveredBytes`), which
is how an operator decides whether to promote a project without guessing.

Other knobs (`AO_MEMORY_SYNC_TIMEOUT`, `AO_MEMORY_CACHE`, `AO_MEMORY_MAX_FILES`,
`AO_MEMORY_BUDGETS`) all default conservatively. **A malformed value is an
error, not a fallback** — the same rule the context router's budget parser
follows, so a mistyped setting produces the previous behaviour and a warning
rather than a policy nobody wrote.

---

## 2. Trigger model

One entry point, `Syncer.EnsureFresh`, called at every boundary. Four roles ask
the same question — "is memory current for this repository at this commit" — and
differ only in what they do with the answer.

| Boundary | What happens |
| --- | --- |
| Planner dispatch | `EnsureFresh`. Cold project → one bounded full index. |
| Worker spawn (and **both Repair Agents**, same path) | `EnsureFresh`, which on a warm project is a single indexed row read. |
| Reviewer launch | `EnsureFresh`, then a changed-area pack. |
| Verified + committed task | invalidate the paths it wrote, `RecordOutcome`, and promote **only** if the placement proves integration. |
| Cancelled / failed task | `DiscardTask` — its beliefs are not project knowledge. |

`SyncKind` is reported on every dispatch: `none` (warm), `incremental`, `full`,
`coalesced`, `skipped`.

**A sync never blocks a dispatch.** Every wait is bounded by
`AO_MEMORY_SYNC_TIMEOUT` (20 s default), the work runs on a context derived
with `WithoutCancel` so an overrun is abandoned rather than cancelling the
dispatch, and every failure yields a stated reason with the legacy context
intact.

### Currency vs usability

A repository whose commit AO cannot read (a scratch directory, a checkout with
no history) still gets memory. It simply cannot prove that memory is current,
so it re-confirms on each boundary instead of taking the warm path. Withholding
memory there would be failing closed against a condition that is not a
staleness; claiming the warm path would assert a currency nothing established.
`Freshness.Usable` and `Freshness.Current` are the two separate answers.

---

## 3. Single-flight

Concurrent callers on the same `(project, repo, commit, branch)` coalesce onto
one in-flight sync; the rest wait for its result and reuse it. A caller
arriving after it finished sees the freshness marker and does nothing.

The in-process map is the right scope, not a shortcut: the durable
cross-process guard already exists one layer down, in P2-A's
generation-conditioned pass claim, which succeeds only from a terminal phase.
This map exists to stop *one* daemon's four roles queueing behind each other.

`FilesRead` is attributed to the caller that actually read, never to the
waiters — otherwise one pass would be counted four times.

---

## 4. Eliminating the AO-controlled rescan

The audit's clearest finding was the plan-reuse assessment: it rebuilt the whole
planner context — six files read with their bodies, up to 48 KiB each — purely
to compute digests it then compared against digests the stored manifest already
held. It is reached from an HTTP read a poll can hit freely.

P2-B adds `PlannerManifestBuilder`, an optional digest-only interface, and
`plannercommand.MemoryBackedBuilder` which answers it from project memory's
digest ledger. Two conditions gate it, both necessary:

- Memory must be indexed at **exactly** the commit being asked about.
- The file must be at or under the planner's 48 KiB truncation cap, or the two
  digests describe different byte ranges.

Anything the ledger cannot answer is read, so drift still means "read only what
is affected". `Build` is unchanged — a planner still receives full bodies.

---

## 5. Budgets and eviction

Per-role, four dimensions, in `AO_MEMORY_BUDGETS` (`role=bytes/items[/docs]`):

| Role | bytes | ~tokens | items | docs |
| --- | --- | --- | --- | --- |
| planner | 24 KiB | 6144 | 40 | 4 |
| reviewer | 16 KiB | 4096 | 30 | 2 |
| worker | 16 KiB | 4096 | 24 | 2 |
| repair | 12 KiB | 3072 | 20 | 0 |

Eviction order, as specified: **freshness → relevance → confidence → scope
proximity → deterministic tie-break** (section order, then the derived id).
Freshness is a bucket, not a timestamp: within one pass every canonical fact
shares an `UpdatedAt`, so a raw-time tie-break would be noise dressed as signal.

Over budget, a fact is reduced to its summary before being dropped, and the
budget is enforced against **rendered** bytes.

---

## 6. Dedupe

A legacy document is dropped only when a memory item names that exact path
among its source paths, the item is currently valid and in the pack **with its
body**, and its provenance digest matches the document AO is holding.

The digest check is what makes it safe: an item derived at an older commit will
not match the file AO just read, so the document survives. Dedupe cannot serve
stale content in place of current content.

Aggregates (a module census, a repository overview) name several sources and
summarise their *combination*; they contribute nothing to coverage and may not
license replacing any one member.

### Coverage-first selection

**This was added because the first measurement showed the opposite of the
goal.** With relevance-first ranking, `preferred` mode still produced a *larger*
planner context than baseline (101,532 B vs 87,209 B): the pack spent its budget
on module censuses and replaced one document out of six.

So when replacement is permitted, the documents the dispatch is already
carrying are handed to selection, and facts that can replace one rank first.
They are the only facts that can pay for themselves. In add-only mode the
signal is not set, because covering a document buys nothing there.

---

## 7. Cache

Keyed on the **authority**, never on prompt text: project, repository, indexed
commit, memory generation, role, budget policy version + the role's budget, and
the selection scope.

Invalidation is implicit: a sync that advances the commit or the generation
changes every key derived from it, so previous entries are simply never asked
for again. There is no invalidation call to forget. A pack with no provable
provenance, or one that fell back, is not cached.

---

## 8. Measured before/after

Measured on this repository (`agent-orchestrator`,
`feat/engineering-control-center`), default limits and budgets. **These are
AO-assembled context only** — see §10.

### Cold, then warm

| | sync | files read | pack |
| --- | --- | --- | --- |
| **A.** first task, cold project | `full` | full bounded index (~5.5 s) | 15 items / 24,461 B / ~6,116 tok |
| **B.** second task, unchanged — planner | `none` | **0** | 16 items / 24,576 B |
| second task — worker | `none` | **0** | 6 items / 15,927 B |
| second task — reviewer | `none` | **0** | 15 items / 16,221 B |
| second task — repair | `none` | **0** | 15 items / 12,149 B |

Syncer counters across the six checks: `{Checks:6 NoOp:5 Full:1 Incremental:0
Coalesced:0 TimedOut:0}`. **One index, five no-ops.** The warm path reads no
files and touches no filesystem.

### Planner context, before and after

The planner's six documents on this repository weigh 87,009 B; with a 200-byte
objective the pre-P2-B AO-assembled context is **87,209 B (~21,803 est. tokens)**.

| Mode | Context bytes | ~Est. tokens | Documents kept | Replaced |
| --- | --- | --- | --- | --- |
| baseline (memory off) | 87,209 | 21,803 | 5 of 5 | — |
| `assisted` | 111,670 | 27,918 | 5 of 5 | 0 B (covered 10,138 B) |
| `preferred` | **40,378** | **10,095** | 1 of 5 | **70,324 B** |

**`preferred` reduction: 46,831 B absolute, 53.7%** of the AO-assembled planner
context, and the same proportion of its estimated tokens.

`assisted` **increases** the payload by 24,461 B, and that is reported plainly
rather than presented as a saving: in add-only mode a pack is a cost, and its
value is what the agent does with it, not a byte count. `assisted` is the safe
rollout step, not the efficient one.

`docs/STATUS.md` was not replaced: it is admitted to the index but derives no
single-source fact, so nothing covers it. That is the model working — no fact,
no replacement.

### Reproducing

```bash
AO_MEMORY_MODE=preferred ao memory report <project-id>
```

`ao memory report` runs the ordinary lifecycle check and assembles each role's
pack through the same provisioner a dispatch uses, so what an operator reads is
what an agent receives.

---

## 9. Failure and fallback

| Failure | Result |
| --- | --- |
| memory database unreadable | empty pack, stated reason, legacy context sent |
| sync timeout / overrun | abandoned, dispatch proceeds |
| stale generation | write refused by the CAS, pass stops cleanly |
| corrupted item | row treated as replaceable, not fatal |
| index incomplete / failed pass | pack withheld with the failure named |
| repository moved / git unavailable | degraded to "cannot prove currency" |
| budget misconfigured | memory disabled with a warning |

`Provision` never returns an error. No memory failure sets `needs_attention` or
stops a run: AO's pre-P2-B path is always available and is what a failure falls
back to.

---

## 10. What these numbers do **not** say

They describe **AO-assembled context only**.

Per the audit's §1 finding, the Worker's, Reviewer's and both Repair Agents'
*harness* file and git reads are not observable by AO at all — those dispatch
wrappers declare only `ContextPayload`, and `RepeatedReads` is emitted as
`Unavailable` for them even with `AO_PROJECT_MEMORY_BASELINE` on.

So:

- `DedupeSavedBytes` is context AO **stopped sending**. It is real and measured.
- `SourcesReused` lists paths whose *summarised* content came from memory. **It
  is not a count of reads an agent avoided**, and nothing here may be reported
  as one.
- `MemoryMetrics.ReductionPercent` is computed only against
  `DedupeSavedBytes` — never against the pack's own size. A pack that adds
  6 KB and replaces nothing has reduced nothing.

Whether memory changes how much the harness reads is a question P2-B cannot
answer, because AO cannot see it. Answering it needs observability AO does not
have.

---

## 11. Observability

Every instrumented dispatch carries a `MemoryMetrics` block into the baseline
evidence record (additive, `omitempty`, schema version unchanged — a dispatch
with no memory story produces exactly the record this schema always produced).

It answers "why did this role receive this context": mode, role, generation,
indexed commit, sync performed/kind/files-read/millis, pack items/bytes/
estimated tokens, candidates and what the budget excluded, stale withheld,
legacy/task/deduped/context bytes, cache hit and key, pack digest, fallback
reason.

---

## 12. Known limitations

1. **`assisted` mode costs bytes.** It is the safe rollout step; the saving
   requires `preferred`, which requires the per-project measurement in §8.
2. **Dedupe only pays where legacy documents carry digests**, which today means
   the planner. A worker's pre-fetched issue body is synthetic and unprovable,
   so it is never replaced.
3. **`preferred` is not enabled by default** and must not be until §8 is
   reproduced for the project in question.
4. **Repair packs replace nothing** (`docs: 0`). A repair agent acts on specific
   findings, and its legacy context is small.
5. **Single-flight is per-process.** Two daemons on one repository serialise via
   the durable pass claim, which is correct but coarser.
6. **Harness reads stay unobserved.** See §10.
