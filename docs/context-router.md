# Role-aware context router

`backend/internal/contextrouter` assembles the context AO hands to an agent,
instead of handing it everything AO happens to have. It is **off by default**;
see [Feature flag](#feature-flag).

Today every dispatch surface sends its whole assembled payload — the planner
gets every context document it found, a worker gets the whole pre-fetched issue
context. The [project-memory baseline](project-memory-baseline.md) is what
records how much of that an agent actually consumes. The router is the other
half of that story.

## What it does

`Router.Select(ctx, Request)` answers one question — *given this role, this
task, and this project, what is worth sending?* — and returns ordered context
sections plus their estimated size against the role's budget.

Evidence comes from facts AO already owns; the router retrieves and ranks, it
never re-derives:

| Section kind | Source                                                            |
| ------------ | ----------------------------------------------------------------- |
| `task`       | the request itself (mandatory; always leads the payload)           |
| `document`   | the documents the caller already assembled                        |
| `diff`       | the current change set (`DiffSource`; `GitDiffSource` by default) |
| `graph`      | impacted files and symbols from [`codegraph`](code-graph.md)      |
| `memory`     | non-stale items from [`projectmemory`](project-memory-store.md)   |

Sizes are estimated with the same bytes/4 heuristic the baseline harness uses
(`observe/projectmemory.EstimateTokensFromBytes`), so a routed payload and a
measured baseline payload are directly comparable. They are estimates, not
provider token counts.

## Role awareness

Role awareness is two things, not one.

**Order.** A planner is deciding what to do and reads documents before a diff.
A worker or a reviewer starts from the change itself. A verify dispatch runs a
command, so a document it will not read must never crowd out the change list.

| Role       | Section order                                 |
| ---------- | --------------------------------------------- |
| `planner`  | task → document → memory → graph → diff       |
| `worker`   | task → diff → graph → document → memory       |
| `reviewer` | task → diff → graph → memory → document       |
| `fix`      | task → diff → graph → memory → document       |
| `verify`   | task → diff → document → graph → memory       |

**Size.** Each role has three token limits, and they are configurable
(see below). The defaults:

| Role       | compact | expanded | hard cap | why                                                       |
| ---------- | ------- | -------- | -------- | --------------------------------------------------------- |
| `planner`  | 6000    | 18000    | 24000    | plans over a whole objective; reads documents first        |
| `reviewer` | 5000    | 15000    | 20000    | judges a change against its surroundings                   |
| `worker`   | 4000    | 10000    | 14000    | implements one task; the diff and its symbols dominate     |
| `fix`      | 2000    | 5000     | 7000     | delivered into a session that already holds the history    |
| `verify`   | 500     | 1200     | 1600     | runs a command; needs the task and what changed            |

Planner and reviewer are deliberately budgeted above fix and verify: the first
two reason about code they have not seen, the last two act on a conclusion
reached elsewhere.

Retrieval scales with the budget too — no single document may claim more than a
quarter of a tier's limit, and no single memory item more than an eighth — so a
small-budget role does not pay to retrieve detail its packer would discard.

## Progressive expansion

1. `Select` runs the **compact** pass: what changed, the symbols in the most
   relevant changed files, a handful of memory facts, the head of each document.
2. The selection reports `EvidenceSufficient`: true when nothing was dropped,
   nothing was truncated, and no source failed.
3. `Expand` runs the **expanded** pass only when that is false (or when
   `Request.ForceExpand` is set): the whole change set, symbol-level detail and
   graph edges, more memory, whole documents.

`Expand` will not expand an already-expanded selection, will not expand a
sufficient one unless forced, and will not exceed the role's hard cap — forcing
expansion buys retrieval depth, never budget.

**The hard cap is never exceeded.** Both tiers pack against a target already
clamped to the cap by `Budget.LimitFor`. A section that does not fit is
truncated into the remaining room, or dropped when that room is below
`minSectionTokens`, and every drop is reported with the size it would have
needed. The mandatory task section is truncated rather than dropped. The
invariant is asserted at the end of every assembly, not only in tests.

A source that fails costs its own evidence and nothing else: the failure
becomes a note, the selection is marked insufficient, and assembly continues.

## Feature flag

```bash
AO_CONTEXT_ROUTER=1                                  # off unless explicitly set
AO_CONTEXT_ROUTER_BUDGETS="planner=8000/20000/26000,verify=400/900/1200"
```

`AO_CONTEXT_ROUTER` defaults to **disabled**, and an unrecognised value reads as
off. With it unset, `daemon.contextRouterFor` returns nil,
`wfrouter.Instrument` hands the workflow dependencies back untouched, and every
provider adapter receives exactly the full context it received before this
package existed.

`AO_CONTEXT_ROUTER_BUDGETS` overrides the defaults as
`role=compact/expanded/cap`, comma separated. A malformed or incoherent
override disables routing with a warning rather than silently applying a budget
the operator did not write.

## Wiring

`backend/internal/contextrouter/wfrouter` wraps dispatch surfaces the same way
`observe/projectmemory/wfdispatch` does — by decorating the workflow port, never
by editing it. Two surfaces are routed, and only two, because they are the ones
where AO itself assembles a *context payload* rather than composing an
instruction:

- `PlannerContext.Documents` — replaced with the routed selection, each routed
  document carrying a checksum of the content actually delivered.
- `SpawnConfig.IssueContext` — replaced with the rendered selection. The
  `Prompt` is never touched.

The reviewer, fix, and verify surfaces carry prompts (standing rules, the
specific correction, the command to run). Budgeting a prompt would truncate
instructions rather than evidence, so those surfaces are left alone even though
the router budgets their roles.

Both routed surfaces need the checkout's **absolute root** — it is what the
diff source runs git in and what the code graph is keyed by. The planner
request carries it (`ProjectRecord.Path`); a `SpawnConfig` carries only a
project id, so the spawner wrapper resolves the root through the same
`workflow.Projects` port the coordinator already uses. A relative path is
refused rather than resolved against the daemon's working directory.

If routing fails for any reason — a source error, an unregistered project, a
record with no absolute path, no `Projects` port wired — the wrapper passes the
original payload through. The router exists to send less, never to prevent a
dispatch, and a routed payload assembled without a root would be a smaller
payload with the diff and graph evidence silently missing.
