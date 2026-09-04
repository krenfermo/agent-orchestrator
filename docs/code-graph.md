# Code graph: the structural half of project memory

`backend/internal/codegraph` gives AO a symbol-level graph of a repository —
files, declarations, and the relations between them — and binds it to project
memory so a dispatch can be told **where the code lives** without being sent the
repository.

The question it exists to answer is a short prompt. Register MEDUSA, SIGE or
Poseidón once, then write:

> "Add export permissions to the Supervisor role, respecting the existing
> architecture."

and have AO find the permission evaluator, the role definition, the handler that
reaches it, the table it writes and the tests that cover it — without thirty
paragraphs of context pasted in front of the task.

## What changed, and what did not

Before this phase there were two memory-shaped subsystems and only one of them
was durable knowledge:

| | before | now |
|---|---|---|
| `internal/projectmemory` | durable facts in SQLite: modules, documents, dependencies, conventions, task knowledge | unchanged, still authoritative |
| `internal/codegraph` | a per-checkout JSON graph, keyed by a filesystem path, read only by `internal/contextrouter` | a project-scoped graph in the same database, versioned with the same generation semantics, read by every role |

**Project memory was not replaced or rewritten.** The code graph is a second
source category beside it: rendered in its own pack section, measured with its
own counters, budgeted separately per role. Nothing that used to come from
durable memory now comes from the graph instead.

`NativeIndexer` (the path-keyed, JSON-persisted provider) is also unchanged and
still serves `internal/contextrouter`, which knows a checkout and nothing else.
`Index` is the project-scoped one; the two share this package's extractors,
scanner and scoring, so there is one definition of what a symbol is and one of
what is relevant.

## Grae / Graphify

**There is still no Grae or Graphify integration in this repository, and this
phase did not invent one.** The determination in
[p2-project-memory-audit.md §4.1](p2-project-memory-audit.md#41-graphify--grae-the-explicit-determination)
is unchanged: no client, no SDK dependency, no vendored module, no configuration
value, no documented endpoint or CLI — every occurrence of either name is prose
naming an example of a third-party tool that *could* implement a port.

So what exists is the port and a production implementation behind it:

- `codegraph.CodeGraphProvider` — the adapter boundary (index, update, query).
- `projectmemory.CodeGraph` — the consumer-side contract project memory reads.
- `projectmemory.MemoryGraph` / `LocalGraph` / `TeeGraph` — the relationship
  port over project memory's own edges, with the in-tree backend as the
  canonical one and an external adapter composable beside it, never in place of
  it.

The backend is recorded on every graph and reported by its real name: the
in-tree one is `local`, everywhere, in `ao memory graph status`, in the API and
in the P3-E metrics. Calling it "Graphify" would claim an external integration
AO does not have — see [usage-accounting.md](usage-accounting.md) for the same
rule applied to the token ledger.

## What a graph fact is

### Symbols

A symbol's identity is `(file, kind, qualified name)`, rendered as
`<path>#<kind>:<name>`. Line numbers are **observations**, not identity: a
reformat moves a declaration, and treating that as a new symbol would make every
gofmt look like a rewrite of the module. A rename moves the path, and therefore
is a delete and a create — recorded as both, which is also the honest
description of what happened to anything that referred to the old path.

Kinds: `function`, `method`, `type`, `interface`, `constant`, `variable`,
`test`, `endpoint`, `table`, `query`, `config`.

Each symbol carries its signature as written, the first sentence of its author's
own documentation, and a bounded **summary** assembled from the two plus any
side effects visible in its body.

### Summaries are derived, never generated

Indexing a project must never require a model. A project registered on a machine
with no provider configured, or with a budget of zero, gets a complete graph
with complete summaries. Two indexes of the same source produce the same bytes,
which is what lets a context pack's digest prove two dispatches were given the
same memory.

If an AI-written semantic summary is ever added it belongs in a separate field
with its own `SummarySource` and its own provider/model provenance, and it must
stay optional. `SummarySource` exists now, with one value (`static`), so that
distinction can never be lost.

### Relations

Every relation is provable from a single file's own text. There is no "probably
calls", no cross-file type inference, and no heuristic that guesses an
implementation from a name.

| kind | from → to | proven by |
|---|---|---|
| `import` | file → module path | the import statement |
| `call` | symbol → callee **as written** | the call site |
| `embeds` | type → embedded/base type | Go embedding, TS `extends`, Python base class |
| `implements` | type → interface | a Go compile-time assertion (`var _ I = (*T)(nil)`) or a TS `implements` clause |
| `references` | symbol → type in its signature | the declaration |
| `tests` | test → the name it exercises | the test's name **and** a call to it in its body |
| `routes_to` | endpoint → handler | a literal method and pattern at a registration site |
| `reads_from` / `writes_to` | query → table | `FROM`/`JOIN` vs `INSERT`/`UPDATE`/`DELETE` |
| `configures` | symbol → configuration key | `os.Getenv`, `process.env.X`, `os.environ[...]` |

Targets are **names**, not resolved declarations. Resolving `service.Delete` to a
declaration needs type information this indexer does not build, and a
resolved-looking edge that guessed wrong is the one failure a reviewer cannot
detect. `Query` and `Retrieve` resolve by name at read time, where the ambiguity
is visible.

That choice also answers dependency invalidation: changing a function's
signature invalidates **no stored edge**, because the callers still call the same
name. There is nothing to recursively rebuild. A sync reports the bounded blast
radius (`SyncOutcome.AffectedSymbols`) instead of chasing it.

## Languages

| language | how | what it gives |
|---|---|---|
| Go | `go/parser` AST | everything in the table above |
| TypeScript / JavaScript | lexer + brace-depth walk | declarations, `Class.method` qualification, imports, scoped calls, `extends`/`implements`, `process.env`, `describe(...)` coverage |
| Python | lexer + indentation walk | declarations, `Class.method` qualification, docstrings, imports, scoped calls, base classes, `os.environ` |
| SQL | statement scan | tables from migrations, sqlc `-- name:` queries, per-statement read/write classification |

No TypeScript or Python parser is available to this binary and none is vendored.
What the extractors do instead is **tokenize first** — masking comments, string
bodies, template literals and docstrings so they cannot be mistaken for code —
and then walk the remaining structure. That is materially different from
matching patterns against raw lines: a `class Foo` inside a comment is no longer
a symbol, an `import ... from 'x'` written inside a string is no longer an edge,
and a method can be qualified by the class that owns it. It is also, honestly,
less than an AST would give, and each extractor's doc comment says exactly where
it stops.

A language is not "supported" because an extractor is registered for it. Each is
proven by fixtures in `extract_lang_test.go` written the way the language is
actually written — with the comments, docstrings, nested scopes and strings that
a scanner gets wrong.

## Identity, storage and isolation

One registered project has exactly **one** canonical graph, keyed by
`(project_id, repo_id)` — the same identity project memory uses, where `repo_id`
is derived from the canonical repository root.

A task's isolated worktree is a checkout of a repository AO already knows, not a
second repository. `EnsureFresh` refuses a linked worktree outright (the P2-E
rule), so a worktree cannot mint a second graph. What a task's own provisional
changes get instead is `Index.AnalyzeChanged`: the changed files are extracted on
demand, in memory, and **nothing is written**. An abandoned task's analysis
simply never existed; an integrated one is absorbed by the next canonical
incremental sync through the ordinary path.

Storage is migration `0153`, in AO's existing SQLite database rather than a
second one:

- `code_graph_index` — one row per repository: backend, generations, phase,
  commit, identity, counts, last-sync measurements, architecture summary.
- `code_graph_files`, `code_graph_symbols`, `code_graph_edges` — the graph,
  indexed by name, by path and by both edge endpoints.

## Generations: staging, CAS, restart safety

Two counters, and the distinction is the whole of the restart-safety argument:

- **`generation`** is the pass allocator, bumped by a claim.
- **`served_generation`** is what readers filter on.

A **full build** stages: it writes at `generation` while readers are still served
`served_generation`, and becomes visible through one `UPDATE` that also collects
the generation it replaces — in one transaction, so there is no state where
`served_generation` names deleted rows. A crash mid-rebuild leaves the previous
complete graph serving and the partial one collectable; a restart **resumes**
from the paths already staged rather than starting the repository over.

An **incremental update** writes in place at `served_generation`, inside one
transaction per path. Atomicity is what makes staging unnecessary there: a
reader sees the old version of a file or the new one, never half of either.

Concurrency: `ClaimCodeGraphBuild` succeeds only from a terminal phase, so two
dispatches starting at once resolve to one builder and the loser stands down. A
build that has gone quiet past a generous threshold is *taken over* instead —
"in flight" and "died in flight" look identical in a row and need opposite
answers, and time is the only evidence available to tell them apart.

## What a sync costs

| situation | what happens |
|---|---|
| never indexed | full build |
| unchanged commit | one row read, nothing else |
| a commit AO can diff from | only the paths the diff names are touched; of those, only the ones whose content hash moved are parsed |
| no provable change set (force-push, shallow clone) | full build — an incremental update on an unprovable change set leaves holes AO cannot detect |
| full build over a quiet tree | every file is read and hashed; unchanged ones are carried forward **by the database**, not re-parsed |

The measurements are recorded, not asserted: `filesParsed` against
`filesReused`, in `SyncOutcome`, in `code_graph_index`, in `ao memory graph
status` and in the P3-E metrics.

## Retrieval

Retrieval is hybrid and says so. Term matching over symbol names, paths and
summaries finds the anchors; the graph expands an anchor into the neighbourhood
that matters — its callers, the tests that exercise it, the tables it writes, the
route that reaches it. Neither half is sufficient: term matching alone is a
keyword grep, graph traversal alone has nowhere to start. **This does not pretend
graph retrieval solves semantic search.**

Ranking, strongest first: a symbol the task named outright; a symbol declared in
a file the work touches; a name matching the objective; a path matching it; a
summary matching it. Architectural symbols (endpoints, tables, queries,
interfaces) get a lift when they match at all, because their names are patterns
and table names rather than identifiers and a plain name match is least likely to
surface them. Generated files are excluded unless asked for — a generated client
has one symbol per API operation and would spend the whole budget restating the
schema — but they remain reachable, because generated code is frequently the API
authority.

Everything is bounded before it is returned, and every result carries what was
**considered** alongside what was **selected**. A retrieval that cannot say how
much it looked at cannot be measured.

## The architecture summary

A bounded, project-level structural summary, derived from the graph when a build
completes and served from one row thereafter. It is **not** a generated README:
every line is counted, not described. `backend/internal/projectmemory — 41
files, 612 symbols, imported by 9 modules` is a fact; "the memory subsystem is
responsible for durable knowledge" is a sentence somebody would have had to
write.

It carries languages, entry points, the most depended-upon modules with their
dependencies, the HTTP surface, the tables and where the queries against them
live, external dependencies, the test surface, and the configuration keys the
code reads (keys only — never a value). Capped at 4 KB, deterministic, and
recomputed only when a sync actually changed something.

The Planner receives it. It is deciding how to split work, and a module map is
worth more to it than any individual function.

## What each role gets

| role | architecture | symbols | why |
|---|---|---|---|
| Planner | yes | 16 | splitting work along real module boundaries |
| Worker | no | 24 | the neighbourhood of what it is about to change |
| Reviewer | no | 32 | "what calls this, what tests cover it, which boundary" — the question an independent review asks, and the one that otherwise costs a repository-wide read |
| Repair | no | 16 | a narrow neighbourhood around a known failure |

Budgets are per-role and independent of the durable-fact budget, so adding
structural evidence can never quietly eat the budget that was paying for facts.

## Memory modes

- **off** — nothing is attached, graph included.
- **assisted** — graph evidence is added beside the durable facts; the agent
  still inspects the repository however it likes.
- **preferred** — memory and graph rank first and may replace legacy documents
  they demonstrably cover. It never stops an agent reading source: the working
  tree is the authority and the pack says so in its own preamble.

## Failure behaviour

Every path degrades, and the degradation is stated rather than silent:

| condition | result |
|---|---|
| no graph configured | pre-phase behaviour; nothing attempted |
| not built yet | empty evidence, reason "has not been built yet" |
| backend fails | empty evidence, reason carries the error, warning logged |
| role budget is zero | empty evidence, reason names the budget |
| evidence over budget | the neighbourhood is dropped before the architecture map |
| graph drifted | reported unhealthy; a drifted graph is never served as current |

A failing graph never empties a pack that has durable facts, and never fails a
dispatch. That is `Provision`'s oldest contract — there is no failure of memory
that should stop a dispatch — extended one layer down.

## Drift

A graph carries the commit and the repository identity it was derived under, and
both can stop being true without anything writing to the graph. `ao memory graph
status` compares what the graph claims against what the checkout says **now**:

- the checkout now identifies as a different repository → drift, fail closed;
- the checkout is at a different commit → drift, run a sync;
- a build in flight → **not** drift; a known temporary state, and the previous
  complete generation is still what readers are being served.

## Security

Files whose content is a secret by convention are refused **before** they are
read, not filtered after — and refused in both the walk and the per-file sync
path, because an incremental update names its own paths and would otherwise
reach a file the walk would never visit. `.env` and its dotted variants, key and
certificate extensions, `id_rsa`, `credentials`, `secrets.json`, `.netrc`,
`kubeconfig`.

Source code that *handles* secrets is still code: `secrets.go`,
`credentials.ts`, `secret.py` are indexed normally. Refusing them would blind the
graph to exactly the code a reviewer most wants to find.

A graph fact may record that a configuration **key** exists — from the code that
reads it. Nothing ever opens the file that holds its value.

## Operator surface

```bash
ao memory graph status <project> [--architecture]
ao memory graph sync   <project> [--repo <path>] [--full]
ao memory graph query  <project> [terms...] [--symbol X] [--path p] [--limit n]
```

`query` runs the same retrieval a dispatch runs, at the same bounds, so what it
prints is what an agent would receive rather than an approximation of it.

HTTP: `GET /api/v1/projects/{id}/memory/graph`,
`POST .../memory/graph/sync`, `GET .../memory/graph/query`.

There is deliberately **no** graph explorer UI. A visualisation is a product
decision this phase does not make, and there is no Project Memory panel to
extend — the operator surface for memory is the CLI and the API.

## Tests

```bash
cd backend && go test ./internal/codegraph/... ./internal/projectmemory/...
```

Covering: per-language extraction fixtures; the architecture summary and
retrieval on a layered fixture project; staged builds and their publication;
concurrent builds resolving to one winner; incremental update touching only the
diff; no-op sync; deletion leaving no zombie symbols; rename identity; resume
after a crash mid-build; worktree analysis writing nothing; graph evidence as a
distinct pack category; and every degradation path above.
