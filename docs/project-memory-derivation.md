# Automatic project-memory derivation (P4-H)

How AO comes to know things about a project nobody told it about, what those
things are, and what each one is worth.

Companion documents: `project-memory.md` (the model), `project-memory-store.md`
(the durable store), `project-memory-authority.md` (provenance and authority),
`project-memory-optimization.md` (dispatch integration).

---

## 1. The bug this phase fixed

P4-G shipped the Code Graph with an automatic lifecycle, and it worked: a
project imported into AO reached `ready` on its own. Project Memory did not.
Checked against four real projects — agent-orchestrator, MEDUSA, SIGE and
Poseidón — search returned **zero** durable memory hits on every one.

Every piece of machinery memory needed had already shipped. A bounded indexer
(P2-A), an incremental update, generation fencing, drift detection, an
authority model (P2-D), a promotion path (P2-C). What never shipped was a
**caller**. `projectmemory.Service.Sync` had exactly two entry points:

- an operator typing `ao memory rebuild`, and
- the P2-B dispatch wrapper, which is inert unless `AO_MEMORY_MODE` is moved
  off its default of `off`.

So on a normal installation nothing ever derived anything, while every
subsystem reported itself healthy. The Memory and Search tabs P4-G built were
correct and empty.

A second, smaller problem sat behind it. What the indexer *did* derive when it
ran was document-shaped: a README's first sentence, an AGENTS.md excerpt, a
manifest's dependency list, a per-directory census. On a real repository that
is mostly `module` rows — SIGE derives 333 of them — and on a repository with
no such documents it is almost nothing. The questions a Planner actually needs
answered first were not among them.

## 2. Consumption and knowledge are different questions

`AO_MEMORY_MODE` governs **consumption**: whether an agent is handed a memory
pack, and whether that pack may stand in for a legacy document. Those change
what a model is told and deserve a staged rollout.

Whether AO *knows things about a project it has imported* is a different
question with a different risk profile. Deriving facts nobody reads costs one
bounded pass and can mislead nobody.

P4-H separates them:

| | governed by | default |
|---|---|---|
| deriving durable memory | nothing — it always runs | on |
| recording what a task learned | nothing — it always runs | on |
| putting memory into a dispatch | `AO_MEMORY_MODE` | off |
| letting memory replace a document | `AO_MEMORY_MODE=preferred` | off |

A cautious operator now gets a populated Memory tab and dispatches that are
byte-for-byte what they were.

## 3. The driver

`service/projectmemory.Reconciler` — P4-G's automatic indexer — gained a second
subject rather than a sibling. Each tick, per repository, it asks two
questions: does the graph describe the checkout, and does memory. The order
within a repository is graph first, memory second, because the high-level facts
read the graph's structural summary.

That order is a **preference, never a precondition**. Memory derives whether or
not the graph succeeded and picks up the graph-backed half at the next tick, so
a first task arriving before either is finished is served with whatever exists
rather than made to wait.

The derivation lifecycle is derived on every read, never stored, in the same
five words the graph uses (`service/projectmemory/memory_state.go`):

    pending → deriving → ready → stale → failed

`failed` is deliberately not rescheduled. A pass that failed will fail again
for the same reason, and retrying it every tick turns one broken repository
into a permanent busy loop that starves every other project of the per-tick
budget.

## 4. What gets derived

Nine high-level facts per repository, at most, from two evidence sources that
are each optional (`projectmemory/insight.go`):

| fact | evidence | typical content |
|---|---|---|
| `architecture` | code graph | files, symbols, relations, languages, top modules |
| `entry_point` | graph, else naming | `backend/cmd/ao/main.go`, `frontend/src/main.tsx` |
| `runtime_surface` | graph, else naming | "255 HTTP endpoints registered in 4 locations" |
| `persistence` | graph, else naming | "116 tables declared by the schema" |
| `auth_model` | naming | "concentrated in `internal/httpd/controllers`, `internal/service/rbac`, `internal/oidc` (86 files)" |
| `integration` | code graph | the external packages the code imports most |
| `testing_surface` | code graph | "1137 test files covering 764 symbols" |
| `config_surface` | naming + graph | config files, and the env keys the code reads |
| `deployment` | naming | "Docker, Docker Compose", "GitHub Actions" |

Four rules constrain all of it:

- **Not a second graph.** Nothing is per-symbol or per-file. The Code Graph
  answers "which function decides this"; memory answers "which subsystem, and
  where does it live".
- **Deterministic.** No model is consulted. Every fact is a count, a name, or a
  path AO read, so the same repository at the same commit derives byte-identical
  facts — which is what makes the idempotent upsert meaningful.
- **Bounded.** Nine categories, one fact each, each body capped at
  `MaxProjectMemoryContent`. A fact names at most twelve evidence paths while
  still reporting the true total.
- **Anchored.** Every fact names the paths it was derived from, so an
  incremental pass can invalidate exactly what a change touches.

**No secrets, ever.** The path census refuses live credential stores by name and
by directory (`.env`, `*.pem`, `secrets/`, `credentials/`, `.ssh/`), refuses
third-party and build trees, and refuses coding-agent scratch space (`.claude`,
`.cursor`, `.aider` — `.claude/worktrees` holds whole copies of the repository,
and the first operational run reported MEDUSA's entry points as living inside
one). Configuration **keys** are recorded; configuration **values** never are.

## 5. Evidence class

Migration 0157 adds a third provenance axis beside P2-D's two:

| axis | question |
|---|---|
| `provenance_kind` | which PROOF applies to this row |
| `authority` | does that proof still hold |
| `evidence_class` | how strong is the claim itself |

The third exists because two rows can be repo derivations with intact authority
and still be very different claims. "AGENTS.md says worktrees live under
`~/.ao`" is a sentence AO copied out of a file a reader can open.
"Authorisation is decided in these three packages" is AO's own reading of a
directory census. Rendering the second the way the first is rendered is how a
plausible guess becomes a premise nobody rechecks.

    workflow_verified  a workflow produced it AND verification passed
    user_provided      a person stated it
    observed           AO read it and is repeating it
    derived            AO concluded it from evidence it observed

Empty is legal and means "this row does not say" — every row written before
P4-H. Nothing is backfilled: assigning a class to rows whose producer never
made the distinction would fabricate exactly the signal the column exists to
make trustworthy. An unrecognised class ranks **below** every known one, so a
value from a newer build loses arguments rather than winning them by being
unfamiliar.

The one event that upgrades a class is promotion: a task's account of its own
work is `derived` until a promotion proof holds, at which point it becomes
`workflow_verified`.

The `auth_model` fact is the sharpest case and is handled accordingly. It is
located by naming alone, so it is `derived`, carries the inferred confidence
(0.55), and states in its own body: *"AO has NOT verified that these files
implement the authorization model, only that they are where it would be."*

## 6. Incremental update

`insightScope` (`projectmemory/insight.go`) decides which high-level facts a
change set could have moved, and nothing else is derived at all.

"Not derived" rather than "derived and found unchanged" is the requirement, and
the reason is subtle: every fact carries the commit it was derived at, so
restating an untouched fact after a commit still rewrites its row and moves its
`updated_at`. The only way to leave the deployment fact alone when somebody
edits auth code is not to derive it. The first run of the incremental test
caught exactly that.

    auth code changed        → auth_model in scope, nothing else
    README changed           → the instruction/convention facts, nothing else
    an unrelated test changed → no high-level fact in scope
    the code graph rebuilt   → the graph-backed facts; auth and deployment sit out

A full pass sets `insightScope{All: true}`: it re-read the whole repository, so
every fact is being restated from evidence that pass actually saw.

## 7. Staleness and conflict

Unchanged from P2-A/P2-D, and the high-level facts inherit all of it because
they are written inside a pass at that pass's generation:

- A deleted or modified source path invalidates every fact naming it, **before**
  anything is re-derived.
- A fact that cannot be re-derived — its evidence is gone — stays invalidated
  with the reason, rather than being silently rewritten into something that
  looks current.
- The retire sweep, the drift detector and the authority validator all apply
  unchanged.

Because insight facts are aggregates over a sample rather than about a single
file, they carry **no** `source_digest`, like the module census and the
overview. Drift detection reports them as unverifiable rather than pretending
to check them.

## 8. Reading value first

`Service.Inspect` now orders by value before truncating. Without it, the window
a truncated inspect returned was whatever order the store produced — dominated
by module rows. SIGE derives 333 of those against 12 high-level facts, so a
200-row window could contain, and did contain, not one fact worth reading. Both
callers that truncate — the Memory tab and Search — were looking at the least
useful two thirds of memory. Search additionally asks for the whole corpus
rather than one page of it.

## 9. What a role is handed

The high-level facts sit **above** the per-unit ones in every role's section
order (`projectmemory/pack.go`). There are at most nine per repository and each
is one paragraph, so they cost a fraction of what a module census costs while
answering the questions a role otherwise spends its first tool calls
rediscovering. Below the census, a repository with four hundred modules would
exhaust the budget before reaching them.

Measured on this repository, a worker task about authorisation:

    memory:  557 candidate facts (159,773 B) → 18 selected (23,566 B), 539 dropped
    graph:   687 symbols / 143 relations considered → 24 / 76 selected (5,962 B)
    the pack carries an "Authentication and authorization" section: one fact,
    naming the three packages where permissions are decided.

Stated as **selected and avoided**, never as a saving. AO cannot observe what a
coding harness reads inside a worktree, so it cannot know what its context
prevented anybody from reading.

## 10. Operational results

Derived from a cold start, against real checkouts, with a code graph:

| project | facts | high-level | derivation | memory hits on five representative questions |
|---|---|---|---|---|
| agent-orchestrator | 575 | 9 | 2.2 s | 237 / 250 / 248 / 40 / 68 |
| MEDUSA | 217 | 9 | 1.1 s | 17 / 24 / 21 / 10 / 23 |
| SIGE | 344 | 9 | 0.8 s | 8 / 28 / 22 / 6 / 17 |
| Poseidón | 114 | 9 | 0.3 s | 13 / 18 / 15 / 8 / 17 |

Every one of those questions returned zero before this phase.

Reproduce with:

```bash
cd backend
AO_P4H_REAL_PROJECTS=/path/to/repo1:/path/to/repo2 \
  go test ./internal/service/projectmemory/ -run TestRealProjectsDeriveDurableMemory -v
```

## 11. Known limits

- **`auth_model` is naming-based.** There is no static analysis proving a
  function decides a permission. On Poseidón it names `backend/app/schemas`
  and `backend/alembic/versions` alongside the real ones, because those hold
  files with `user`/`auth` in the name. The fact is labelled `derived` at 0.55
  confidence and says so in its body; it is a pointer to the right files to
  open, not a verified model.
- **Entry-point detection is convention-based** where the graph has no answer.
  SIGE — a legacy Java WAR with no conventional entry file — gets a weak one.
- **`integration` reflects import analysis.** A service reached only over HTTP
  with no client library does not appear, and Go's stdlib imports rank highly on
  a Go repository.
- **No LLM summarisation.** Section 7 permits it under evidence-backed
  constraints; this phase did not need it, and the deterministic path is
  cheaper, reproducible and impossible to hallucinate through. If one is added
  later, the model/provider metadata and source references it would have to
  carry are the ones the evidence class already models.

## 12. External Grae / Graphify

**Not integrated.** No real contract, API or CLI was available to integrate
against. The provider-neutral boundary is unchanged: `MemoryGraph`
(`projectmemory/graph.go`) and `CodeGraph` (`projectmemory/graphmemory.go`)
remain the ports an external backend would implement, and nothing added by this
phase assumes the in-tree implementation. Adapter-ready, not adapted.
