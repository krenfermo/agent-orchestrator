# Shared task knowledge (P2-C)

Making what one task learns reusable by the next one, safely.

This document is the design and the operating manual for P2-C. It sits on top
of [`project-memory.md`](project-memory.md) (P2-A: what AO remembers about a
codebase) and [`project-memory-optimization.md`](project-memory-optimization.md)
(P2-B: how that memory reaches each role inside a budget). P2-C adds one thing
neither settled:

> When a task learns something, which later task may be told, and for how long
> does it stay true?

---

## 1. What P2-C is, in one exchange

```
Task A   works in internal/store, verifies, integrates
         -> records: what changed, one decision, one open risk

Task B   works in internal/store
         -> receives A's outcome, A's decision, A's open risk
         -> does not re-derive them, does not read A's transcript

Task C   works in cmd/app
         -> receives none of it
```

Measured on a store holding twelve prior tasks in one module
(`TestMeasureSharedKnowledgeTokenImpact`):

| | pack bytes | est. tokens | shared facts | knowledge bytes |
|---|---|---|---|---|
| Task B (same module) | 6 203 | ~1 551 | 32 | 5 674 |
| Task C (other module) | 1 450 | ~363 | 0 | 0 |

Task C's pack considered 32 shared facts and excluded every one of them. That
difference is the feature; the absolute numbers are only evidence for it.

---

## 2. Built on the P2-A item, not beside it

There is **no second table and no second identity scheme**. Shared knowledge is
`domain.ProjectMemoryItem`, because a shared fact needs exactly what a derived
fact needs — an identity, a provenance and a generation fence — and a parallel
model would mean two answers to "is this still true".

What P2-C adds is a bounded **lifecycle**, carried in the item's existing
`Metadata` map (`domain/task_knowledge.go`):

| Key | Meaning |
|---|---|
| `status` | `active`, `superseded`, `resolved`, `obsolete`, `conflicting` |
| `subject` | the stable identity of *what the fact is about* |
| `share` | `task`, `workflow`, `canonical` — how far it may travel |
| `kind` | `risk` or `follow_up`, within `known_risk` |
| `supersededBy` / `supersedes` | the decision chain |
| `resolvedBy` | the task that closed a risk |
| `conflictsWith` | an incompatible peer AO could not order |
| `task` / `run` | provenance: who produced it, in which run |

Metadata is the right home: already bounded, already normalised, and already
part of the item's content hash — so a status change is a real content change
with a real `UpdatedAt`, and two writers cannot both silently win.

The P2-C vocabulary maps onto the P2-A types like this:

```
task_result              -> MemoryTypeTaskResult
decision                 -> MemoryTypeDecision
known_risk               -> MemoryTypeKnownRisk, kind=risk
follow_up                -> MemoryTypeKnownRisk, kind=follow_up
convention_discovered    -> MemoryTypeConvention
architecture_observation -> MemoryTypeArchitecture
dependency_observation   -> MemoryTypeDependency
verification_fact        -> the "Verified by" line on the task result
```

`verification_fact` has no row of its own because it has no life independent of
the outcome it verified.

---

## 3. What is promoted, and what is not

`RecordTaskOutcome` takes structured fields and nothing else. There is no
transcript field, no reasoning field and nowhere to put one; everything else a
task did is already durable in AO's workflow rows.

**Recorded:** what changed, why, the modules and files touched, how it was
verified, explicit decisions, explicit risks and follow-ups, declared task
dependencies.

**Not recorded:** transcripts, reasoning, command output, failed attempts,
routine steps, and anything already covered by a current canonical fact.

Bounds are enforced at the store, not left to callers: 10 decisions and 10
risks per outcome, 1 KB per rationale, 400 bytes per summary, 8 KB per body.

Nothing is invented. A task that recorded no decisions produces no decisions —
never guessed ones.

---

## 4. Three sharing scopes

`domain.KnowledgeShare` is the sibling-safety rule written down as a field
rather than inferred. Origin says where a fact came from; share says who may
read it, and they are different questions.

| Share | Who may read it | When a task gets it |
|---|---|---|
| `task` | only the task that produced it | the default — unverified, failed, cancelled |
| `workflow` | tasks **downstream of it in its own run** | verified, committed, not yet integrated |
| `canonical` | everyone | integrated |

The decision is made in `workflow/task_knowledge.go:knowledgeShareFor`, at the
boundary that knows the placement and the verification state. Memory does not
make it, and cannot.

**Sibling safety** falls out of the middle row. A workflow-local fact is
readable only by a task that (a) is in the same run and (b) named the producer
in `workflow_task_dependencies`. Two parallel tasks editing the same package
name each other in no dependency list, so neither can read the other's
unintegrated work however much their file sets overlap.

The entitlement travels from the coordinator to the memory boundary on the
context (`projectmemory.TaskAuthority`). A dispatch that never receives one
provisions with canonical knowledge alone — so forgetting to stamp it makes a
task **less** informed and can never make it less safe.

---

## 5. Promotion authority

| Placement | Outcome | Result |
|---|---|---|
| direct branch | verified + committed | canonical, at that commit |
| direct branch | failed / cancelled | discarded |
| isolated worktree | verified, not integrated | workflow-local; **never** canonical |
| isolated worktree | integrated | canonical, at the **integrated** SHA |
| isolated worktree | never integrated | discarded when the run ends |
| needs attention | — | stays task-local, kept for recovery |

The isolated-worktree promotion happens in `finishTaskWorktree`, and that
placement is the whole point: it runs only after the integration checkpoint is
durable and the target ref has moved, which is the first instant at which "the
repository has this work" is provable. Promoting earlier would let a crash
leave canonical memory describing work the repository does not have.

It runs **before** the worktree cleanup, because cleanup is best-effort and may
fail and leave; promotion after it would let a failed housekeeping step
silently keep integrated knowledge task-local.

Both are idempotent. `PromoteTaskMemory` writes the canonical rows first and
discards the task-local originals second, so a duplicate completion callback or
a restart mid-cleanup promotes the same content to the same derived identities
and then finds nothing left to promote.

---

## 6. Decisions supersede; they are never overwritten

A decision's row key is **subject + statement fingerprint**, which is what makes
supersession possible at all:

- Re-recording the *identical* decision addresses the same row — idempotent.
- Deciding the same *subject* differently creates a new row, and the old one is
  marked `superseded` with `supersededBy` naming its replacement.

The subject is derived from the caller's `Topic` when there is one, and from
the statement's significant words otherwise. A decision that never named its
topic can only be superseded by a verbatim restatement of itself or by an
explicit `Supersedes` — silence is never read as "replaces the last thing
anyone said".

**Authority is asymmetric.** An unintegrated branch may not retire the
project's knowledge. A task-local or workflow-local decision that contradicts a
canonical one is marked `conflicting` on **both** sides, and:

- a Worker, Reviewer or Repair Agent is told **neither** (choosing for them
  would be choosing silently);
- a Planner is told both, prefixed `CONFLICTING —`, because deciding what to do
  about it is a planning question;
- integration resolves it: promotion clears the conflict and applies the
  supersession the branch was refused.

---

## 7. Risks and follow-ups

Open risks are carried into the context of work that touches the same area. A
later task closes one by naming it in `ResolvesRisks` (by item id or by
subject); the risk becomes `resolved`, stops being served, and keeps the task
ref that closed it plus a `resolved_by` edge.

Nothing is deleted to change a status. `ao memory risks --status resolved`
shows what has been closed and by whom.

---

## 8. Retrieval

`PackBuilder.filterServable` applies three separate rules, each counted under
its own name so "why did this task not receive that decision" has one answer:

1. **Validity** — the P2-A drift rule. Provenance must still hold.
2. **Entitlement** — the P2-C sharing rule (§4).
3. **Currency** — the P2-C lifecycle rule. Superseded, resolved and obsolete
   facts are kept forever and never served as current.

Then a fourth gate applies to task-produced types only —
`relevantSharedKnowledge`. A fact about the *repository* is relevant to anyone
working in it; a fact about what some earlier *task* did has to earn its place:

- the reader's own task produced it, **or**
- the reader explicitly depends on the producing task, **or**
- its evidence names a path or module this work touches, **or**
- it is stated at project scope, or names no evidence at all.

Repository scope is **not** a free pass, and measuring it is what showed why. A
decision inherits its task's changed paths as evidence and defaults to
repository scope, so treating "repository-scoped" as universally relevant
handed an unrelated task 24 facts and 3.3 KB of another module's history. The
rule is about **evidence**, not scope: a fact whose subject AO can locate must
overlap the work.

Shared knowledge competes **inside** the P2-B role budget, never beside it.
There is one bounded selection per role, and `knowledgeBytes` is a subset of
`packBytes` — the metrics validator refuses a record where it is not.

---

## 9. Compaction

Task knowledge is the only part of memory that grows with time rather than with
the repository, so it is the only part with explicit retention:

| Bound | Value |
|---|---|
| task results per module | 8 |
| task results per repository | 200 |
| active decisions per repository | 60 |
| open risks per repository | 60 |

Per-module rather than global, so one busy area cannot evict every other area's
history. Compaction **retires** (`status=obsolete`) and never deletes:
provenance survives, and an operator can still see that a task existed and when
it aged out.

---

## 10. Freshness

Shared knowledge goes stale like anything else:

- A commit touching a fact's evidence paths marks it stale through the ordinary
  path-anchored invalidation, on the same sweep that handles derived facts.
- Drift detection invalidates a fact whose evidence has been **deleted**, even
  though task knowledge carries no source digest to compare against. It is
  reported as *unverifiable*, never as *confirmed*: AO checked what it could,
  and what it could check held.

**A full re-index must never retire task knowledge**, and this was a real bug
worth naming. The generation sweep retires canonical facts a completed pass did
not re-derive, on the premise that a walk which did not produce a fact has
shown its subject is gone. That premise is false here: no walk ever produces a
decision or a risk, so every one of them looked un-re-derived and the first full
re-index after a promotion silently retired the project's entire decision and
risk memory — at the exact moment memory looked healthiest. Both sweeps now
exclude task-produced types and task lineage edges
(`TestFullReindexDoesNotRetirePromotedKnowledge`).

---

## 11. Context freezing

`project_memory_context_manifests` (migration 0145) records what one execution
was told: the **identities** of the facts, the pack digest, the selection policy
version, the memory generation and commit. Never the prompt, and never the
facts' text.

Identities, not content, is what makes a manifest small and what keeps it
correct: the items may have been superseded since, and a manifest that had
copied them would report a premise nobody ever held. Expanding one shows each
fact's *current* status, and names any that no longer exist at all.

The row id is derived from (project, run, task, role, pack digest), so
re-provisioning the same context after a restart addresses the same row. A
restart that produced a *different* answer gets a second row — which is exactly
what an operator needs to see.

---

## 12. Graph

Lineage is mirrored into the `MemoryGraph` port, over the in-tree `LocalGraph`
that ships by default:

```
Task     -produced->      Knowledge
Task     -changed->       File
Task     -affects->       Module
Task     -depends_on->    Task
Task     -follows_up->    Task
Decision -supersedes->    Decision
Risk     -resolved_by->   Task
Risk     -concerns->      Module
Knowledge-conflicts_with->Knowledge
```

Edges are only asserted for facts AO already holds. Two tasks touching the same
repository are **not** related; that would be the invented relationship P2-C §5
forbids.

A graph backend that is unreachable does not fail a recording. The items are the
durable knowledge; the edges are an index a later pass can rebuild. Nothing here
requires Grae or Graphify, and the adapter surface is unchanged.

---

## 13. Inspecting it

```bash
ao memory knowledge <project>            # what tasks have taught this project
ao memory decisions  <project>           # which choices still govern
ao memory decisions  <project> --status superseded
ao memory risks      <project>           # what is still open
ao memory risks      <project> --status resolved
ao memory task       <project> <task-id> # everything one task produced
ao memory context    <project> --task <task-id> --expand
```

Over `GET /api/v1/projects/{id}/memory/knowledge` and
`.../memory/manifests`. The daemon answers them through the same service call
the pack builder uses, so an operator told a decision is active is looking at
the judgement a Worker's pack made — not an independent guess that agrees most
of the time.

No UI yet, deliberately. A dashboard over numbers nobody has checked hides them.

---

## 14. Observability

Per dispatch, on the existing evidence record (additive, `omitempty`, schema
version unchanged):

```
sharedCandidates            sharedSelected
sharedIrrelevantExcluded    sharedUnauthorizedExcluded
supersededExcluded          conflictingExcluded
decisionsSelected           risksSelected
taskLocalItems              workflowLocalItems      canonicalItems
knowledgeBytes
```

The pair that carries the argument is `sharedCandidates` against
`sharedSelected`. A related task shows candidates it considered and took; an
unrelated task shows candidates it considered and excluded. Neither claim can be
made from a single number.

`taskLocalItems` / `workflowLocalItems` / `canonicalItems` are what make "did
this task read a sibling's unintegrated work" answerable after the fact — the
question sibling safety exists to guarantee an answer to.

---

## 15. Known limitations

- **Decisions and risks are only as wide as their durable sources.** They are
  no longer supplied by hand — see §16 — but every source has an edge, and the
  facts outside it are not recorded rather than approximated. In particular the
  planner's own reasoning for a task has no column: a task carries a title, a
  description, acceptance criteria and a scope, and none of them is "why this
  approach rather than that one". Where the plan rationale becomes durable, it
  becomes a decision the same day; until then AO says nothing about it.
- **Contradiction detection is structural.** AO can prove two decisions are the
  same subject; it cannot prove two different texts mean the same thing. A
  disagreement between an unintegrated branch and the project's own decision is
  detected; two canonical decisions worded differently about the same idea, with
  no shared topic, are not.
- **`workflow_mutation_provenance` is still deferred.** Shared knowledge derives
  changed files from `WorkflowTaskScope`, which is authoritative where it
  exists. A task with no observed scope contributes no file list rather than a
  guessed one, and therefore anchors less well. See
  [`project-memory.md` §14](project-memory.md); P2-C did not attempt to repair
  it and did not invent provenance to work around it.
- **Compaction retires; it does not aggregate.** P2-C bounds task knowledge per
  scope and keeps the retired rows readable, but it does not synthesise a
  "module knowledge aggregate" standing for twenty small changes. That would
  need a fact AO can derive from the twenty without asserting anything none of
  them says, and the honest version of it is a summarisation step P2-C
  deliberately did not add. Task history stays durable and queryable
  (`ao memory knowledge --status obsolete`) even when it no longer appears in a
  context pack.
- **No cross-repository knowledge sharing beyond selection.** Multi-repo
  projects select per repository, and a Planner pack spans them; there is no
  inference that a decision in one repository governs another.

---

## 16. Where decisions and risks come from

P2-C shipped the lifecycle and left the inputs empty, because the *contents* of
a reviewer's opinion and of a planner's reasoning had no durable row. That was
true of the prose and not of everything around it. `taskOutcomeFacts` now
derives both from rows AO already keeps
(`workflow/task_knowledge_sources.go`), and the lifecycle above is unchanged:
these are ordinary decisions and risks, with ordinary subjects, supersession,
resolution, share and promotion.

Three rules decide what counts, and each one costs coverage deliberately.

**Only durable rows.** A transcript, a pane capture and a prompt are not
sources. Neither is a review body: `CountReviewFindings` says outright that AO
has never had a structured finding type, so a review's markdown is prose and
parsing findings out of it would be the invented provenance this model refuses
everywhere else. What AO holds *around* the prose is structured, and that is
what is read.

**Only authoritative rows.** Authority is read off a column, never assumed.

**Nothing is invented.** A field no row proves is left empty. A risk that names
no file is anchored by the task's own changed paths — the fallback
`knowledgeEvidence` already applies — rather than by a guessed one.

### Risks

| Source | Becomes a risk when | Excluded when | Resolved when |
|---|---|---|---|
| Post-Run QA findings (`post_run_qa_runs.findings_json`, migration 0126) | `Finding.Blocking()` | the gate's own predicate says baseline, out-of-scope, report-only, or below major | the gate pass ended `clean` |
| PR review threads (`pr_review_threads`, migration 0004) | the thread is unresolved | — | the provider reports the thread resolved |
| Review runs (`review_run`) | `EffectiveVerdict` is `changes_requested` | the run was superseded, or never concluded | a later, non-superseded pass on the same PR approved |

The QA gate is the one **structured** finding type AO has: each finding carries
its own attribution, scope, verification and severity, so "is this this task's
problem, and does anyone need to act on it" is answered by the row. The filter
is `Finding.Blocking()` itself rather than a second opinion written at the
workflow boundary — a baseline finding was already true before the task ran, an
out-of-scope one belongs to something the execution never owned, and a
report-only one is the agent's prose with nothing behind it. Re-deciding that
here would be two answers to one question.

The review thread is the only reviewer finding AO holds that names a **file**,
which is what lets a derived risk anchor to a path a reviewer actually pointed
at. The row carries no comment text and none is invented; what it can say is
that a reviewer opened a thread on a path and line and nobody has resolved it.

The review run is the review-level fact: a pass that requested changes against a
commit. The risk names the review, never its body.

One caveat, stated rather than left to be discovered: the QA gate's state model
is durable and wired, but **nothing runs its checks yet** — `internal/postrunqa`
says so itself ("no check runners, no repair dispatch, no scheduling"). Until a
runner lands, that row is always empty and the two review-derived sources carry
the risks. The wiring is in place now because it is the part that has to be
right the day the runner arrives.

### Decisions

| Source | Statement | Rationale | Authority |
|---|---|---|---|
| Criterion amendments (`workflow_task_criterion_amendments`, migration 0132) | the criterion as amended, or that it no longer applies | the amendment's own `reason` | the schema refuses an amendment without a human approver, a reason and evidence |
| Answered questions (`workflow_questions` + `workflow_question_resolutions`) | the question and its answer | the resolver's `reason_summary`, or the answer's source | human and policy answers outright; a **resolver** answer only when its own resolution row completed and did not ask for a human |

The amendment ledger is the most authoritative decision AO records anywhere: it
is append-only, human-approved and evidence-bearing by schema. Its topic is
derived from the criterion's **text**, not its index — migration 0132 says an
index stops identifying anything as soon as an earlier criterion is removed —
so a second amendment of the same criterion supersedes the first instead of
sitting beside it.

An answered question is a decision by construction: the work was blocked, an
answer was recorded with its source, and it was delivered and changed what the
agent did. The **question row** is the source, never the pane it was captured
from; the row is classified, fingerprinted and durable, and its fingerprint is
the decision's topic.

### Resolution by topic

`ResolvesRisks` accepts an item id, a subject **or a topic**. The third is what
lets a derivation close what it opened: the boundary that raised a risk from a
review thread knows the thread's id, not memory's subject scheme. Without it a
fixed finding could only fall silent, and an earlier task's risk would stay open
forever — "AO stopped mentioning it" is not "AO knows it was dealt with".

### What this does not change

Nothing about promotion, sharing or freezing. A derived fact from an
unintegrated worktree is exactly as task-local as a hand-supplied one; a derived
decision that contradicts a canonical one conflicts rather than replaces; a
derived risk is compacted under the same bound. Derivation is deterministic over
the same rows, so a duplicate completion callback and a restart produce the same
facts, address the same identities, and write once.
