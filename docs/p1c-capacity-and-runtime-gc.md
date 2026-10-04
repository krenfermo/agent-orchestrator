# Runtime capacity and Runtime GC

*Status: implemented (P1-C). Builds on [`execution-strategy.md`](execution-strategy.md) (P1-A) and [`p1b-recovery-and-repair.md`](p1b-recovery-and-repair.md) (P1-B).*

Two problems, one cause: AO had no idea how much it was running.

Nothing bounded concurrency, so a busy machine ended up hosting a planner, four
workers and three reviewers at once — where everything is slower and nothing
finishes. And nothing reclaimed what finished, so a daemon that ran for weeks
accumulated tmux sessions until "why is this machine slow" had a literal
answer: there are forty of them.

## This is not the capacity AO already had

There are now two things called capacity, and keeping them apart is the first
thing to understand.

| | Question | Scope | Where |
| --- | --- | --- | --- |
| **Provider capacity** (8P-E.13A) | is this *provider* usable — auth, an installed CLI, a rate-limit cooldown? | per (harness, user, profile) | `capacity_probe.go`, `capacity_wait.go`, `agent_health_events` |
| **Runtime capacity** (P1-C) | may AO start one more *runtime on this machine* at all? | global, per kind, per workflow | `capacity_scheduler.go`, `capacity_claims` |

A dispatch must be **routable** (a provider can take it) *and* **admitted** (the
machine has room). Neither substitutes for the other, and merging them would
have made "your Claude subscription is throttled" and "this laptop is full" the
same string. They deliberately share the wake reasons and the
`waiting_for_capacity` phase: to a person, "AO is waiting for room to run this"
is one situation, and the reason code says which room. The API paths are
distinct for the same reason — `/api/v1/capacity` is the provider view,
`/api/v1/runtime/capacity` is this one.

## The capacity model

```
ExecutionKind    planner · worker · reviewer · repair
ClaimState       queued → held → released
```

**A repair is its own kind**, not another worker, so a repair storm cannot eat
the worker budget and repairs can be bounded and prioritised separately.

**Fix delivery is not metered.** It writes into a worker session that is
already running; charging a second slot for a message would let a run be
blocked from finishing by its own occupied slot.

### The claim

`capacity_claims` is a real table, not another checkpoint payload, because the
scheduler has to **count held claims inside the same write that grants the
next one**. Counting JSON blobs in an append-only ledger is neither atomic nor
indexable, and a scheduler that can double-grant is not a scheduler.

A claim binds: execution kind · workflow run · step/task · **lifecycle
generation** · **dispatch key** · owner · project · the runtime incarnation it
paid for · priority · enqueue time.

Three properties are carried by the schema itself:

- **`UNIQUE(dispatch_key)`** — one intended launch gets one claim, however many
  times reconciliation, a wake, a restart or a double click re-derives it.
  *"Duplicate reconcile does not double-claim" is a constraint, not a convention.*
- **`state='queued'`** — the queue is durable. A restart reconstructs exactly
  what was waiting, because what was waiting is a row.
- **`lifecycle_generation`** — the fence. A claim carrying a generation older
  than its step's current one describes a launch the lifecycle has moved past;
  it may neither hold capacity nor release a newer claim.

Released rows are **retained**. They are the evidence of what held a slot and
why it came back.

### Claim identity, and why the generation is in the key

The dispatch key is `cap:<kind>:<launch intent>:gen<N>`, where the launch
intent is the outbox idempotency key the dispatch site already mints.

The generation is part of the **key**, not only of the fence, and that is
load-bearing. A retry is a genuinely new launch intent — its predecessor's
claim was released when the lifecycle moved past it — so it needs its own
claim. With a generation-less key, a retry found its predecessor's *released*
claim and was refused forever, parking a run permanently on its own second
attempt. Within one generation the key is stable, so the idempotency the unique
index provides is unchanged.

What the generation counts differs by kind, and each is chosen to agree exactly
with the rule that supersedes it:

| Kind | Generation | Superseded when |
| --- | --- | --- |
| worker, repair | the step's attempt number | the step's attempt count moves past it |
| reviewer | the **review cycle** | a newer cycle's claim exists for the step |
| planner | the planner retry count | the plan row is no longer `running` |

Fencing a reviewer on the step's attempt count instead left every cycle at
generation 1, so nothing superseded anything and one review step accumulated a
held slot per cycle until the run hit its own per-workflow bound and parked
itself. Two definitions of "generation" is how a scheduler leaks.

## Limits and defaults

| Bound | Default | Env |
| --- | --- | --- |
| global concurrent runtimes | 6 | `AO_CAPACITY_GLOBAL` |
| workers | 4 | `AO_CAPACITY_WORKERS` |
| reviewers | 3 | `AO_CAPACITY_REVIEWERS` |
| planners | 2 | `AO_CAPACITY_PLANNERS` |
| repairs | 1 | `AO_CAPACITY_REPAIRS` |
| per workflow | 2 | `AO_CAPACITY_PER_WORKFLOW` |

Chosen from what AO actually did before P1-C rather than from a theory: a
project in the default isolated-worktree mode ran one task at a time per
objective, with a reviewer alongside it and occasionally a planner.

**A zero or unparseable value normalizes to the default.** "No slots" is
deliberately not expressible: a misconfiguration must degrade to a default,
never to a scheduler that grants nothing and deadlocks the daemon.

Owner and project are recorded on every claim so **scoped** limits are a
configuration change rather than a schema change. P1-C enforces global,
per-kind and per-workflow only, and does not invent unfinished multitenancy.

## Queue, order and fairness

When there is no room the run **waits**: it moves to `Waiting` under the
durable wake it already used for a provider-capacity wait, its outbox entry
stays `Pending`, and **no retry budget is spent** — a wait is not an attempt.

Scheduling order is deterministic and stated in one place:

1. eligible work only — a queued claim whose run is terminal or whose
   generation is superseded is released, never promoted;
2. **priority** ascending — repairs at 50, everything else at 100;
3. **`enqueued_at`** ascending, then claim id as a total tiebreak.

**Fairness is a per-workflow concurrency cap plus FIFO.** A master objective
with twenty runnable children can hold at most `PerWorkflow` slots, so there is
always room for another workflow to reach the front of the queue. It is
deliberately not a weighted fair queue: AO schedules a handful of slots on one
machine, and a policy nobody can predict is worse than a simple one.

Releasing a slot wakes the front of the queue — at most one wake per workflow
per release, bounded by `maxCapacityWakesPerRelease`, so a release cannot
produce a wake storm. Wakes upsert by idempotency key, so a duplicate release
cannot produce duplicate wakes.

## Strategy interaction (P1-A)

- **TASK** — one bounded execution path; it holds one worker slot and reserves
  nothing else.
- **AUTONOMOUS** — breadth is the scheduler's, not the plan's. However many
  tasks a plan has, no more than `PerWorkflow` run at once, and the existing
  placement safety (a project not in smart-parallel mode serialises its tasks)
  is unchanged. P1-C does not touch worktree/branch isolation; that is P1-D.
- **MASTER** — children become runnable and the scheduler controls how many
  actually run. **The parent itself never holds a worker or reviewer slot**: it
  owns no such runtime. One master cannot starve another workflow, because the
  per-workflow cap applies to it like anything else.

## Repair interaction (P1-B)

A Repair Agent's run is scheduled like everything else. Its worker dispatch is
metered as `repair` (the repair run carries a durable origin checkpoint, written
before it starts, so the scheduler charges the right meter). Consequently:

- **automatic repair cannot bypass the limits** — it goes through the same
  admission gate;
- a queued repair is a durable row, and it stays queued across restarts;
- **one repair generation is one capacity claim**, and a stale repair
  generation cannot launch;
- a repair waiting for capacity does **not** alter the original run's
  authority — the run it is repairing stays exactly as it was;
- when the repair ends, its slots are released and the original obligation
  resumes through P1-B's own semantics.

**Priority:** repairs get a modest boost (50 vs 100). The justification is that
a repair is the only execution whose purpose is to unblock a run that is
*already stopped*. It is a boost and not a reservation: repairs still compete
for the same global slots, are capped by their own per-kind limit (1), and are
bounded per run by P1-B's repair budget. Those three bounds together are why a
repair queue cannot starve ordinary work.

## Recovery after restart

Every crash boundary converges from durable facts:

| Boundary | Convergence |
| --- | --- |
| queued before claim | the queue *is* the claim row; nothing is lost |
| claim before launch intent | the claim is queued/held; the next pass re-derives the same key and proceeds |
| claim + intent before runtime | the outbox's own state machine decides; the claim is reused, not re-minted |
| runtime before confirmation | unchanged P0 behaviour; the claim stays with the launch |
| completion before release | `reconcileCapacityForRun` releases a terminal step's claim |
| run terminal before release | a terminal run releases everything, unconditionally |
| crash during release | release is CAS'd and idempotent — a second one is a no-op |
| stale generation holds a historical claim | released as superseded, under its *own* generation, so it cannot take a newer claim with it |
| unprovable ownership | the run is parked; every other run keeps scheduling |

Release is driven from three places: the natural completion points (planner
returns, run completes, run cancelled), `GetRun` (reusing step facts it has
already read, so a finished step's slot returns on the next read rather than at
the next restart), and boot reconciliation.

## Runtime GC

### The safety model, before the mechanism

> **Unknown is not dead.**

A session AO cannot prove it owns, cannot prove is finished, or cannot address
by its exact incarnation is **skipped and reported**. Never destroyed on the
strength of a name, a heuristic, an age, or the absence of evidence.

Three proofs, all from durable facts or from the runtime itself, never from
timing:

1. **Ownership** — either the session carries AO's ownership token
   (`AO_SESSION_OWNER`, attached atomically at creation), or a durable capacity
   claim records that AO launched this exact incarnation. A session on AO's own
   socket that satisfies neither is *unprovable*, not owned: "on my server" is
   not the same claim as "mine to destroy".
2. **Incarnation** — every destructive act is addressed to the immutable
   `InstanceID` (tmux's `$N`), re-validated immediately before the kill and
   confirmed immediately after. A session that took the same *name* after the
   candidate was classified survives, because the kill was never addressed to a
   name.
3. **Terminality** — the authority that could still be using it (the capacity
   claim, the run, the step generation) must be finished or superseded. **A
   held claim protects its runtime absolutely**, whatever else the scan
   observes.

### What it deletes, and what it keeps

It destroys **runtime resources only**. It deletes no durable row: not a
session record, not a claim, not a checkpoint, not an attempt. Lifecycle
evidence outlives the runtime that produced it.

That is also why GC needs **no ledger of its own** — and therefore has no
crash windows. A destroy that lands and whose report is lost is simply
re-derived as "absent, nothing to do" on the next sweep. "GC crashed after
destroy before ledger update" and "GC marked intent before destroy" do not
exist here rather than being handled.

### Orphan classification

| Class | Meaning | Handling |
| --- | --- | --- |
| `released_claim` | the claim that paid for it was released | auto-cleanable |
| `superseded_generation` | its claim belongs to a superseded generation | auto-cleanable |
| `terminal_run` | its workflow run ended | auto-cleanable |
| `unreferenced_owned_session` | AO-owned, no live claim references it | auto-cleanable |
| `unprovable_ownership` | AO cannot attribute it | **never cleaned**, always reported |

Plus per-candidate dispositions: `cleaned`, `candidate` (dry run), `live`,
`unprovable`, `foreign` (the name now answers for a different incarnation),
`absent`, `error`.

**Isolation:** one broken candidate never aborts the sweep — it becomes an
`error` finding and the sweep continues. An unreadable inventory narrows the
sweep to nothing and licenses nothing.

### Triggers

Daemon startup (the moment nothing is mid-launch and a crashed daemon's
orphans are most likely to exist), a 15-minute periodic sweep, and explicit
operator invocation. There is deliberately no per-terminal-runtime trigger: a
sweep per finished step would run every few minutes for a problem measured in
days, and the periodic pass reclaims the same resources with the same proofs.

## Legacy state

Pre-P1-C runs have no capacity claims, and **none are fabricated**. A legacy
run's runtime is adopted into the scheduler only when a durable claim records
its exact incarnation; otherwise it is simply unmanaged, and GC treats it as
unprovable and leaves it alone. Terminal legacy runs need no fabricated claim
to be reclaimed — but their runtimes still need the ownership token, or they
stay.

## Failure isolation

A workflow with corrupt or unprovable capacity state is parked by its own
reconciliation; every other workflow keeps scheduling. `reconcileCapacityForRun`
is per-run and best-effort, and never returns a global reconcile error.

## API and CLI

| Operation | Route | CLI |
| --- | --- | --- |
| runtime capacity | `GET /api/v1/runtime/capacity` | `ao capacity status` |
| sweep | `POST /api/v1/runtime/gc` | `ao runtime gc` |
| preview | `{"dryRun": true}` | `ao runtime gc --dry-run` |

The status surface exposes configured limits, live usage per kind, the queue in
scheduling order, claim identity, kind, run/step, generation and priority. It
exposes **no** runtime token, prompt, credential or provider response. A tmux
session name is AO's own local label and is what makes a held slot
correlatable with something an operator can see.

There is deliberately **no force-delete**: every destructive answer comes from
the daemon's proofs, and a flag that skipped them would be a flag that destroys
a live session. `--dry-run` runs the *identical* predicates, so a preview is a
true preview.

## Deferred to P1-D

- Worktree/branch isolation redesign. P1-C respects the **current** placement
  safety (a project not in smart-parallel mode still serialises its tasks) and
  changes none of it.
- Scoped (per-user / per-project / per-tenant) limits. The claim records both
  scopes so this is a configuration change, not a schema one.
- GC of worker sessions that carry no ownership token. AO's session table
  records no runtime incarnation, so a worker's tmux session is only provably
  AO's when a P1-C claim named it; older ones are reported unprovable. Attaching
  the ownership token at worker creation (as reviewers already do) would close
  this.
- Reclaiming a reviewer's slot at the moment its verdict lands, rather than
  when the next cycle supersedes it.
