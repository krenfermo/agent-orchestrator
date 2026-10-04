# Execution placement, branch authority, integration and provider failover

*Status: implemented (P1-D, closed). Builds on [`execution-strategy.md`](execution-strategy.md), [`p1b-recovery-and-repair.md`](p1b-recovery-and-repair.md) and [`p1c-capacity-and-runtime-gc.md`](p1c-capacity-and-runtime-gc.md). **Read the "What P1-D did" section first** — parts of the model below predate this phase.*

## What P1-D did

The audit that opens P1-D found that most of what the phase asks for **already
existed**, built across checkpoints 8F, 8M, 8H, 8P-E.11 and 8P-E.14:

| Area | State before P1-D |
| --- | --- |
| execution placement (`direct_branch` / `isolated_worktree` / `smart_parallel_worktrees`) | implemented (`domain.ExecutionMode`) |
| durable worktree records with base SHA, target, dependencies, state machine | implemented (`domain.TaskWorktreeRecord`, migration 0128) |
| branch locks with DB-enforced mutual exclusion | implemented (`internal/branchlock`, migration 0117) |
| integration/merge with strategies, conflict classification, CAS and audit | implemented (`internal/integration`) |
| provider failover with classification, budget and priority walk | implemented (checkpoint 8H, `internal/workflow/failover.go`) |
| real-git integration and worktree tests | implemented (`internal/integration/*_git_test.go`, `internal/workspace/*_test.go`) |

**P1-D closed the three deferrals the previous phases recorded, plus the
placement sweep**, and then closed the three blockers its own first pass left
open: the frozen placement record, one unified admission decision, and a durable
provider-attempt ledger with mid-execution failover after proven no mutation.
Sections marked *(pre-existing)* describe what was audited and verified;
sections marked *(P1-D)* describe what changed.

## Execution placement *(pre-existing)*

Three modes, chosen per project:

- **`isolated_worktree`** — one `git worktree` per task, on a generated `ao/*`
  branch, under the AO data dir. The default, and the pre-8P-E.11 behaviour.
- **`direct_branch`** — AO works in the registered repository itself, on the
  configured branch. No worktree, no `ao/*` branch. Concurrency is protected by
  a durable per-(repository, branch) lock instead of by physical isolation.
- **`smart_parallel_worktrees`** — materialises exactly like
  `isolated_worktree`, and additionally permits the planner to give independent
  tasks their own concurrent worktrees. It grants permission to parallelise,
  never a guarantee: a task the classifier cannot prove independent is
  downgraded back to a plain isolated worktree.

A worktree's identity is its durable `TaskWorktreeRecord`, never its path:
repository, path, `ao/*` branch, target branch, base SHA, the dependency
commits it was cut against, execution mode, state, and — once integrated — the
commit its work landed at. Cleanup is authorized by that recorded commit, so
"safe to throw the checkout away" is a proof against a fact rather than an
inference from a state name.

## Branch authority *(pre-existing, extended by P1-D)*

Lock identity is `(repo_path, branch)` — not `(project, branch)`, because a
workspace project registers several independent repositories and collapsing
them would serialise legitimate parallel work. **Mutual exclusion is enforced by
the database**, via a partial unique index on `lock_key WHERE state='held'`: two
daemons, or two goroutines racing inside one, cannot both believe they hold it.
`owner_token` names the daemon instance, which is what makes restart
reconciliation decidable without guessing from timestamps.

## Worker runtime ownership *(P1-D §C — closes P1-C's deferral)*

P1-C could reclaim a finished **reviewer** pane and not a finished **worker**
one. Reviewers attached an ownership token at creation; workers did not, and the
sessions table recorded no incarnation either. A worker's tmux session was
provably AO's only when a capacity claim happened to name it, and everything
older was reported "unprovable" and left on the machine forever.

Migration 0141 adds `sessions.runtime_instance_id` and
`sessions.runtime_owner_token`, and `session_manager` now attaches

```
ao-session:<sessionID>:<launchID>
```

**atomically with `Create`**, exactly as `ports.RuntimeConfig.Owner` documents:
a marker written after creation leaves a window in which a live runtime exists
that AO cannot identify — unadoptable, unterminatable, and therefore permanent.

The token binds the **launch**, not only the session. `RuntimeOwnedBySession`
is a strict equality, so a token from launch N does not satisfy launch N+1 —
which is what stops a stale handle adopting, or destroying, a replacement.
Proven against real tmux, including the ABA.

Legacy sessions carry no token and no incarnation. Nothing is fabricated
backwards: they are reported unprovable and left alone.

## Repair Agent on `direct_branch` *(P1-D §L — closes P1-B's deferral)*

P1-B refused these outright: in direct-branch mode the stopped run holds the
branch lock for its whole life, so a repair run would queue behind the very run
it exists to unblock. Correct, and it left the most common single-developer
setup unable to use the feature.

The lock now **moves**:

```
origin holds the lock
  → bounded repair intent for a repairable stop
  → lock CEDED to the repair run, conditioned on the origin still holding it
  → origin cannot mutate: it no longer holds the lock
  → repair does its bounded work
  → lock RETURNED, conditioned on the repair still holding it
    AND on the repair generation still being current
  → the original obligation resumes (P1-B semantics)
```

Every step is a compare-and-set on **who holds it right now**
(`CedeBranchLock ... WHERE state='held' AND workflow_run_id = ?`), and the row
never leaves `held`, so there is no instant at which the branch is unowned and a
third run could take it. Cession and return are the *same* statement with the
run ids swapped, so they cannot drift apart.

Refusals, all fail-closed: a stale holder cannot cede onward; a stranger cannot
claim; a superseded repair generation cannot return a branch; and a deployment
whose branch-lock manager cannot cede still gets P1-B's original refusal. **No
lock stealing anywhere.**

Both halves are written to the ledger *before* the transfer they describe, so a
crash leaves an explanation for a move that may not have happened — recoverable
— rather than a move nobody can account for.

## Placement GC *(P1-D §X)*

The P1-C sweeper gained two candidate sources:

- **terminated sessions** whose recorded incarnation and token prove AO created
  them (the §C payoff);
- **integrated worktrees**, removed only when *all* of: the record says
  `integrated`, `IntegratedSHA` names the commit the work landed at, and the
  workflow run is terminal.

Never removed — each a separate refusal with its own reason: `active`,
`creating`, `preserved` (AO's explicit durable "do not clean this up"),
`failed`, anything whose run is still going (review, fix, verification or an
unresolved conflict may still need the checkout), and anything claiming
integration without naming a commit. **A human's worktree is not merely spared:
the sweep walks AO's own durable records, so it never sees one.**

Removal is addressed by record identity (run + task), not by path, so a path
reused by something else cannot be removed by a stale candidate.

## Provider failover *(pre-existing, named by P1-D §R/§S)*

*Superseded by "Provider attempts and failover" below, which makes the
vocabulary durable. Kept because it explains where the safety came from before
there was a ledger.*

Failover was already safe, by a stronger mechanism than a vocabulary:

- the launcher's contract is **a session record, or an error having created
  none** — which is what makes a failed launch safe to route elsewhere;
- a launcher that answers "fine" and names no session
  (`errLaunchWithoutEvidence`) is the one ambiguous case, and it **does not
  fail over**. It records an ambiguous-launch boundary and stops;
  `adoptOrMarkAmbiguous` later resolves it from evidence.

What was missing was a **name**: "AO does not fail over after an ambiguous
launch" was true because of where a `return` sits, which no operator can read
and no test can point at. P1-D adds `FailoverSafety` —
`safe_before_execution` · `safe_after_proven_no_mutation` ·
`ambiguous_execution` · `completed_execution` — as a projection over facts the
dispatch path already records, plus `ProviderAttemptIdentity`, which holds the
distinction that **a provider attempt is not a task generation**: the run, step
and lifecycle generation are the obligation and never change; the attempt
number, failover ordinal, source and destination are how AO is currently trying
to discharge it.

Ordering is the safety model: ambiguity is checked *before* the error class, so
an ambiguous launch stays ambiguous even when a workspace probe would call it
clean. Evidence about a worktree cannot answer a question about whether the
provider ran.

## Frozen execution placement *(P1-D final)*

Placement used to be derived from the project's execution mode **at read time**.
That is a derivation over mutable configuration, and it has one failure mode
that matters: a task starts in `isolated_worktree`, creates a worktree and
writes code; somebody switches the project to `direct_branch`; the next
reconcile derives "direct branch", finds no worktree it believes in, and
recovers the run into a placement that never existed.

`execution_placements` (migration 0142) is the durable answer, written **once,
before any mutation**. It carries the repository, the base branch and commit,
the execution branch, the worktree identity once one exists, the merge target,
the owner token naming the daemon incarnation that froze it, the state, and its
own generation. After the freeze the **stored record wins**: project
configuration is not consulted again for that obligation, and recovery reads the
row rather than recomputing policy.

Two properties are carried by the schema rather than by code:

- `UNIQUE(workflow_run_id, task_id, workflow_step_id, placement_generation)`
  makes a re-freeze idempotent;
- a **partial unique index over the non-terminal states** makes "at most one
  live placement per obligation" a constraint, so two passes racing to freeze
  cannot produce two worktrees.

A direct-branch placement may never name a worktree — a `CHECK` refuses it —
because recording one would fabricate an identity AO did not create. The
converse is deliberately *not* symmetric: an isolated placement is frozen before
its checkout exists, so an empty path at that moment is the truth, and it is
filled in when the worktree becomes real.

### Three generations, deliberately

| generation | advances when |
| --- | --- |
| lifecycle | the obligation is retried |
| placement | the *physical* placement is replaced |
| provider attempt | neither — it is how AO is currently trying to discharge the obligation |

Collapsing them would mean every retry minted a new worktree and every failover
looked like new work.

A stale placement generation may do **nothing**: not acquire a branch lock, not
create or reuse a worktree, not launch, not authorize review/fix/verify, not
integrate, and not GC a newer placement. That is one predicate
(`requireCurrentPlacement`), called from the one gate every launch passes, plus
generation-conditioned CAS on every integration and retirement statement — not a
list of call sites each remembering to check.

### Legacy runs

A run that already executed and has no placement row has one **recovered**, and
only from facts that *prove* the mode: an AO worktree record proves
`isolated_worktree` (AO creates them in no other mode), a held branch lock proves
`direct_branch`. Neither proof, or **both**, is ambiguous and fails closed —
"both" means the project's mode changed under a live run, which is precisely what
the freeze exists to prevent and precisely what nobody should resolve by picking
one. Nothing is fabricated backwards: a recovered placement copies the worktree
record's path, branch and base commit rather than deriving them.

### Placement state machine

`selected → waiting → preparing → ready → active → reviewing → integrating →
integrated`, with `conflict`, `preserved` and `terminal` as the other endings.
Every transition is a compare-and-set on the exact identity **and** the expected
current state, so repeated reconciliation is idempotent by construction: a
transition whose expected state no longer holds matches zero rows and reports
"somebody already did this" rather than failing.

`selected` **permits launch**, and that is a statement about AO's architecture
rather than a relaxation: AO materialises an isolated checkout as part of the
launch, so a freshly frozen placement is exactly the state a first launch should
find. `preparing` does not, because it means another pass is materialising it
right now.

## Unified admission *(P1-D final)*

Capacity, branch authority and placement used to be three gates that did not
know about each other. Each was correct; together they could not answer "why is
this not running" with better than "waiting", because whichever gate refused
first parked the run under its own vocabulary.

`Coordinator.Admit` is one decision over the nine conditions, in a fixed order:

| # | condition | refusal |
| --- | --- | --- |
| 1 | lifecycle generation is current | `lifecycle_superseded` |
| 2 | strategy permits execution | `strategy_refused` |
| 3 | dependencies permit execution | `dependency_wait` |
| 4 | provider is eligible | `provider_wait` |
| 5 | placement is frozen | `placement_wait` |
| 6 | branch/worktree authority is ready | `branch_wait` |
| 7 | mutation exclusivity is proven | `branch_wait` |
| 8 | no stale placement generation is active | `placement_wait` |
| 9 | capacity exists | `capacity_wait` |

It is a **gate, not a scheduler**. It owns no queue (P1-C's durable capacity
queue is the queue), no wake (the existing reasons are reused, one per waiting
reason), and grants nothing of its own — every authority it reports was issued by
some other component. There is deliberately no second queue.

Capacity is evaluated **last**, and that is the one ordering decision worth
arguing about: a capacity claim is the only bounded shared resource in the list,
so taking one for a launch a branch or a placement was going to refuse occupies a
slot no other run can use. Everything else is free to fail.

`domain.AdmissionWaitReason.SpendsRetryBudget()` and `ConsumesCapacity()` answer
false for **every** value, and the tests assert that over the whole vocabulary
rather than per reason — so a seventh reason cannot quietly be added that charges
for waiting.

An admitted decision names the tuple the launch is valid for — lifecycle
generation, placement generation, provider attempt, capacity claim, branch
authority (§K's `(G, P, A, C, B)`) — and every component of it is a durable row,
so the tuple is reconstructable after a restart rather than only from the call
stack.

## Provider attempts and failover *(P1-D final)*

The four-way safety vocabulary was a read-time projection. A projection cannot
answer, after a restart, *which provider is authoritative for this obligation
right now, and what did the previous one prove before it stopped* — the two
questions a failover turns on. So `provider_attempts` (migration 0142) is a real
table, and `FailoverSafety` moved into `domain` because the classification is now
**taken once, when the evidence exists, and read back** rather than recomputed
against a world that has moved.

The rule the whole design holds:

> **A provider attempt is not a task generation.**

The run, step, task and lifecycle generation are the obligation and a failover
leaves every one of them where it was. The frozen placement is likewise untouched
(§I): provider B inherits provider A's worktree, because the authority over the
checkout never moved — only the attempt did.

Two properties are again carried by the schema:

- `UNIQUE(run, step, lifecycle_generation, ordinal)` makes the failover budget a
  **durable fact**, so a restart re-reads the highest ordinal instead of starting
  over. That is what stops A→B→A across daemon boots;
- a partial unique index over the authoritative states makes "one live attempt
  per obligation" a constraint, so two providers cannot hold one placement even
  if two passes race.

States: `planned → admitted → launching → running → completed`, with
`failed_safe`, `failed_ambiguous`, `superseded` and `abandoned`. Every transition
CASes on the exact attempt id **and** its expected state, and `Authoritative()`
answers false for every terminal state — so "a stale provider cannot regain
authority" is a property of the state machine rather than of a caller remembering
to check. A stale attempt cannot launch, mutate, release its successor's capacity
or branch authority, write a completion, or authorize a review.

### Failover after proven no mutation *(§H — the case that did not exist)*

Before-execution failover was safe because of the launcher's contract. There was
no rule at all for a provider that got **past** the launch, had a runtime, and
then died, so every such failure stopped the run whether or not the provider had
touched anything.

`MutationProof` is that rule, and it is an **AND over five durable facts**:

1. AO knows the **exact** runtime/session the attempt launched;
2. that runtime has provably stopped;
3. the provider attempt itself is terminal and not ambiguous;
4. the workspace fingerprint recorded **at launch** equals the one now;
5. AO holds no authoritative mutation evidence — no commit, no integration
   record, no recorded write.

A clean `git status` is condition 4 and never the whole proof, which is exactly
what §H asks for. Anything short of all five classifies as `ambiguous_execution`
and does not fail over. The proof's digest is stored on the attempt, and
`FailoverProviderAttempt` **refuses** `safe_after_proven_no_mutation` with an
empty digest — the class cannot be claimed without carrying what proved it.

The `before` fingerprint is read from the dispatch boundary record the phased
dispatch already writes, not recomputed: a fingerprint taken now and compared
with another taken now proves nothing.

The gate applies where the ledger is wired. A deployment without it keeps the
pre-P1-D behaviour exactly, following the same nil-optional-dependency
convention as every other capability in the package; the daemon always wires it.

### Budget and loop prevention *(§J)*

The numeric budget (`MaxWorkProviderAttempts`, from the run's persisted policy
snapshot) is one guard. The other is the **history**: a hop to a provider this
obligation has already been offered is refused whatever the count says, which is
what makes A→B→A impossible rather than merely bounded. The preferred provider
and fallback order come from the owner's stored execution policy; the current
ordinal comes from the ledger. None of the three lives in memory, so a restart
cannot refill any of them.

### Ordering under a crash

The predecessor is terminated **before** the successor is created, because the
authoritative partial index admits one live attempt: a crash between the two
leaves a terminal predecessor and no successor — recoverable — rather than, for
one instant, two live providers on one worktree.

## Reviewer capacity release *(re-audited at closure, still unchanged)*

The unified admission model does not add a review-verdict witness to the
reconciliation path — admission reads capacity, placement, branch and provider
authority, none of which is the review run's terminal verdict. The sufficient
witness §W asks for is therefore still not available where the release would have
to happen, and the conclusion recorded above stands unchanged: releasing on the
review *step's* state frees a slot the imminent next cycle then finds released,
and a released claim is refused by design.

This does not block closure — the slot is released one cycle later either way.

## Observability

| Operation | Route | CLI |
| --- | --- | --- |
| runtime capacity | `GET /api/v1/runtime/capacity` | `ao capacity status` |
| sweep (runtimes + placements) | `POST /api/v1/runtime/gc` | `ao runtime gc [--dry-run]` |
| placement view | same endpoint, filtered for display | `ao worktree list` |
| placement sweep | same endpoint | `ao worktree gc [--dry-run]` |
| frozen placement, provider attempts, admission | `GET /api/v1/workflows/{id}/placement` | `ao workflow placement <id>` · `ao provider attempts <id>` |

`ao worktree` runs the **same** sweep against the same endpoint with the same
proofs; the filter narrows what is *printed*, never what the daemon decided.

The placement route answers all three legs of one question from one call, because
correlating three endpoints by hand is how the situation became unreadable in the
first place. It is **read-only**: there is no route and no command that re-points
a placement, forces a failover or clears a wait — each would be a way to aim a
running agent at a different checkout, or to start a second provider over a state
AO refused to call safe. The operator affordance this phase adds is being able to
*see* the refusal.

No tokens are exposed. A placement's owner token names a daemon incarnation and
is AO's own local identifier, but it is an ownership credential in shape and
nothing an operator needs to diagnose a stuck run.

## Storage

Migration `0142_execution_placements_and_provider_attempts.sql`, canonical
queries in `queries/execution_placements.sql` and `queries/provider_attempts.sql`,
regenerated with `npm run sqlc`. Terminal rows in both tables are retained: they
are the evidence of what held an authority and why it stopped, which is what
makes a stale-writer refusal diagnosable afterwards instead of reconstructed from
logs.

## Legacy behaviour

Nothing is claimed retroactively. Legacy sessions have no ownership token and are
never reclaimed; legacy runs have no capacity claim and are not parallelised; a
legacy run whose placement cannot be proven from durable facts gets no placement
at all rather than a guessed one; a branch whose authority is ambiguous fails
closed to the operator path rather than to a 500 or a loop.

## Deferred to P1-E

Per-task placement overrides in the API — the operator affordance to choose a
different placement for one task before it starts. Deliberately not built here:
every route this phase added is read-only, and a write that re-points a placement
is exactly the shape that needs its own design rather than being appended to an
observability surface.
