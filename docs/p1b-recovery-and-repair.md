# Recovery, plan reuse and the Repair Agent

*Status: implemented (P1-B). This document is the contract later P1 work builds on. It assumes [`execution-strategy.md`](execution-strategy.md) (P1-A).*

Before P1-B, a workflow that stopped had a reason code and a sentence, and
everything past that sentence was the operator's problem. "Can this continue?
Can the plan be reused? Will Continue duplicate work? Can AO fix this itself?"
were each answered by reading SQL and guessing, and by different surfaces
answering differently.

P1-B makes those answers durable, deterministic and singular.

## The one decision function

`workflow.AssessRecovery(runID)` returns a **RecoveryAssessment**: one
recommendation, derived from durable facts, written by nobody.

| Field | Answers |
| --- | --- |
| `RecommendedAction` | the ONE thing to do |
| `ReasonCode` | the canonical stop reason behind it |
| `Explanation` | AO's own sentence — the same one the Board shows |
| `AutomaticAllowed` | may AO take this action itself? |
| `PlanReusable` | can the durable plan be executed as it stands? |
| `RepairAvailable` / `RepairEligibility` | can a Repair Agent be aimed at this, and if not, why not |
| `BlockingCondition` | what stands between this run and progress |
| `Obligation` | the durable obligation a resume would discharge |
| `Strategy`, `TargetRunID`, `StepID`, `TaskID` | the frozen strategy, and the rows the recommendation is about |

It is **pure and read-only**: no model is consulted, and nothing is written.
An LLM must never be the thing that decides AO has authority to act, and an
operator's recovery choice has to be reproducible.

It also does not introduce a second taxonomy of stops. The canonical attention
reason and its `AttentionDisposition` (`workflow/attention.go`) remain the only
vocabulary; P1-B extended that registry with two fields — `Recovery` and
`Repairable` — because it was already the single place a stop site is forced to
answer "and what does the user do about it?".

### RecoveryAction

A closed set of eleven:

`resume` · `reuse_plan` · `regenerate_plan` · `repair` · `authenticate` ·
`inspect_repository` · `operator_action` · `restart_required` · `abandon` ·
`terminal` · `unrecoverable`

`unrecoverable` has exactly one source: a run that is stopped and whose reason
AO cannot name. It is not a verdict about the run — it is a statement that AO
has no durable fact to reason from, which is precisely what must stop it
recommending anything.

## Resume

`ContinueRun` used to be a verb with no stated object. `ResumeRun` names the
object first.

**ResumeObligation** is the closed vocabulary of what a run can still owe:
`plan_generation`, `plan_approval`, `plan_dispatch`, `work_dispatch`,
`work_observation`, `review_dispatch`, `review_observation`, `fix_delivery`,
`fix_observation`, `verify`, `convergence`, `terminal`, `none`.

Two properties matter:

- **Order is correctness, not style.** An earlier unmet obligation outranks a
  later one, and the fix cycle is checked *before* the review. Mid-cycle the
  review step sits at `waiting` — waiting *for* that fix — and reading it as
  "no reviewer has started" would tell an operator a reviewer is owed at the
  exact moment the thing owed is a fix prompt nobody must send twice.
- **Resume does not re-implement dispatch.** It states the obligation, then
  discharges it through the same evidence-gated path P0 built and hardened.
  Every non-duplication guarantee — no second worker, no second reviewer, no
  re-sent fix prompt, no second child, no double-spent retry budget, no
  accidental second plan — is enforced inside those existing guards, each of
  which re-derives its own evidence at call time. A second dispatcher beside
  them is how duplicates are born. What P1-B adds is that the caller is told
  what was owed and what was discharged.

An obligation that is a person's (a plan awaiting *manual* approval) is
**reported, not driven**.

## Plan reuse

Reusability is two checks, kept separate because they fail separately:

- **Identity.** The stored plan is rehashed and compared with the
  `plan_hash` it was approved under. A mismatch is `not_reusable` outright:
  there is no revalidation that fixes not knowing what you are holding.
- **Compatibility.** The content-free planner context manifest stored with the
  plan is compared with the one AO would build today. Drift — or an inability
  to build one at all — is `stale_but_revalidatable`.

| Classification | Meaning |
| --- | --- |
| `not_applicable` | the run has no plan of its own (a task run, a planned child) |
| `exact` | identity holds and the project context still matches; executable as it stands |
| `stale_but_revalidatable` | a real plan, against a project that has moved. An explicit decision is owed |
| `not_reusable` | no plan, an invalid/rejected one, or one whose identity cannot be proven |

`ReusePlan` accepts **only `exact`**. There is no flag to force a stale plan
through, because a stale plan that quietly executed is the exact outcome the
classification exists to prevent. Reuse never bypasses review or verification:
it decides *which* plan executes, and every task it dispatches goes through the
identical review/fix/verify path.

## Plan revisions

`RegeneratePlan` mints a new durable revision, and the authority model is the
point:

- The superseded revision's identity, hash and task count go on the
  append-only ledger **before** the plan row moves (reason-first ordering, the
  same as CP30/CP31/CP32).
- Its task rows are **never deleted** — they stay readable through
  `ListWorkflowTasksAtRevision`.
- `ListWorkflowTasks` is scoped to the plan's *current* revision, so every
  existing reader (reconcile, convergence, integration, the Board) became
  revision-aware without a single call-site change. **A child bound to a
  superseded task is structurally invisible to its parent** — a property of
  the schema, not of a check somebody has to remember to write.
- The store write is a compare-and-set on the revision the caller observed, so
  two operators working from the same view produce one new revision and the
  loser is refused. Total revisions are bounded (`maxPlanRevisions`).
- It is **operator-initiated only**. Nothing in reconciliation, no wake reason
  and no heartbeat reaches it — restart → regenerate → plan → restart is an
  unbounded provider spend with nobody watching.

Migration 0139 is two `ADD COLUMN`s and an index, deliberately. `workflow_tasks`
is referenced by three `ON DELETE CASCADE` children, and migrations 0119 and
0130 both learned against a real `~/.ao/data/ao.db` what rebuilding it costs.
The two `UNIQUE` constraints from 0101 are left in place: revision 1 keeps its
exact historical spelling (so CP9(b)'s canonical task identity is unchanged for
every row already on disk), and later revisions avoid the constraints by
construction — namespaced `plan_step_id`, offset ordinals. See
`workflow/plan_revision.go`.

## The Repair Agent

### A repair is a workflow run

This is the same argument `incident_repair.go` already makes, and it is why
this subsystem is small. Everything a repair needs *after* "the agent made a
change" already exists and is hardened: an independent reviewer with
cross-provider routing, deterministic verification, bounded fix cycles,
stale-approval detection, worktree isolation or branch locking, and boot
reconciliation for all of it. That is what a workflow run **is**.

So a repair is a bounded **TASK** run (P1-A) against the stopped run's own
project, carrying the failing condition and acceptance criteria **AO wrote from
that condition** — never proposed by the agent that will be judged against them.
Its verification plan is the one belonging to the step being repaired, so the
repair is judged by the same deterministic checks that caught the failure.

(Distinct from `incident_repair.go`, which repairs **AO itself**, in AO's own
checkout, after an LLM diagnosis and an explicit human approval. Different
target, different authority, different code path.)

### RepairIntent

Durable on the checkpoint ledger — no migration, and the intent belongs in the
same timeline as the stop that caused it. It records the run and the target run
(the *affected child* for a master objective), the target step, the condition
reason, an **evidence digest**, the repair generation, the lifecycle generation
it was aimed at, the project, the mutation scope, the acceptance criteria, the
strategy (always `task`), the repair run, who authorised it, and the policy in
force.

The evidence digest is built from durable identifiers and AO's own vocabulary
only — reason, step, step kind, attempt error classes, dispatch generation. No
prompt text, no findings body, no provider output, no paths. **An evidence
digest is an identity, not a disclosure.**

### Generation and single-flight

Three layers, and all three are needed:

1. An **outbox claim** keyed by `(run, evidence digest, generation)` makes a
   double click, a poll and a restart mid-creation converge on one repair.
2. Before claiming, an **unresolved repair already aimed at this evidence
   digest is returned as-is**. The claim alone would let the same failure buy a
   second repair as soon as the first spent a generation — the unbounded shape
   §F rules out. One failure gets one repair agent.
3. A **superseded generation drives nothing**: an intent whose generation is
   behind the run's current repair count describes a repair the run has moved
   past, and it is recorded resolved rather than acted on.

### Repair policy

Frozen in `WorkflowPolicy.Repair` at run creation, exactly like the P1-A
strategy, and read back through `EffectiveRepairPolicy`:

| Mode | Behaviour |
| --- | --- |
| `disabled` | AO never starts a Repair Agent for this run |
| `suggest` | AO surfaces the action and waits for an operator — **the default** |
| `automatic` | AO may start one itself, and only for an explicitly repairable condition |

`suggest` is the default because a repair writes code, and opting into that
unattended should be a decision somebody made. A run created before P1-B falls
back to `suggest` for the same reason: it never opted in.

`ApplyRepairPolicy` refuses once the run has left `pending` — widening what AO
may do to a run already in flight would change the terms under which work
already started. Restarts never re-enter it, so a restart cannot change it
either. The budget (`MaxRepairCycles`, default 2) is enforced by folding the
append-only ledger, so a restart cannot lose a repair that really happened and
hand the run a fresh budget.

### Eligibility is deterministic, and the condition is judged first

Eligibility comes from the stop's own `AttentionDisposition.Repairable`, which
is **`false` by default**. Order matters and is the safety model: the condition
is checked *before* any policy or budget, so no policy setting anywhere can make
an unrepairable stop repairable.

| Eligibility | Meaning |
| --- | --- |
| `eligible` | technical condition, budget left, policy permits |
| `ineligible` | AO recognises the stop and it is not repairable |
| `budget_exhausted` | repairable, but this run has spent its repairs |
| `policy_disabled` | repairable, but the frozen policy forbids it |
| `unknown_condition` | AO cannot name what stopped this run — fail closed |

Repairable today: `verify_budget_exhausted`, `fix_budget_exhausted`,
`fix_no_verifiable_change`.

### Why provenance, credentials and ambiguity can never auto-repair

Not by a deny-list — by construction. A stop nobody has explicitly marked
repairable cannot have a code-writing agent aimed at it, and that includes
every stop AO cannot classify at all. The classes this protects are exactly the
ones where a code change is the wrong instrument:

- **Unprovable provenance** (`verify_approved_head_unprovable`,
  `fix_generation_unprovable`, `review_state_ambiguous`,
  `worker_dispatch_ambiguous`, `fix_dispatch_ambiguous`,
  `recovery_unreconcilable`) — AO does not know what happened. Writing code
  against a state it cannot describe would be guessing with a compiler.
- **Credentials and permissions** (`reviewer_auth_invalid`,
  `provider_auth_required`, `provider_workspace_trust_required`) — only the
  person holding them can act, and no amount of code changes that.
- **Destructive ambiguity** (`dirty_worktree`,
  `verify_workspace_unattributable`, `read_only_workspace_mutated`, integration
  conflicts) — there is uncommitted or unattributable work, and a repair agent
  editing that tree could destroy it.
- **Policy refusals** (`planner_policy_violation`) — the remedy is a decision,
  not a patch.

A regression test asserts each of these is unrepairable *in the registry*,
because that is where the guarantee has to live.

### Escalation

When the budget is spent, the escalation is written to the ledger as a real
stop with its own registered disposition and its own human action. It is a
durable fact rather than the absence of one, and it deliberately is not itself
repairable — the point of escalating is that repairing is no longer on the
table.

## Strategy interaction (P1-A)

- A repair run is **always `task`**, whatever the run being repaired is. A
  repair that decomposes is a repair nobody can reason about, and a master
  repair of a master objective is uncontrolled recursive decomposition.
- **TASK** — the repair stays bounded to that task; no master planning is
  invoked, and the frozen strategy of both runs is unchanged by the repair.
- **AUTONOMOUS / MASTER** — the repair targets the **affected child**, and the
  intent records `SiblingsUntouched`. The parent waits and converges normally.

A master objective's own stop is `child_needs_attention` — a mirror, never
repairable in itself. Eligibility is therefore resolved on the run that *owns*
the problem, after following the mirror; judging it on the mirror would make
master objectives permanently unrepairable.

## Parent/child recovery

- child stops on a human-owned reason → parent mirrors it;
- the assessment sends the operator to `TargetRunID`, the child that actually
  stopped, never to the mirror;
- child recovers → the parent's stale mirror clears and the parent progresses;
- a superseded plan revision's tasks are invisible, so a stale child cannot
  reopen a parent;
- repeated reconciliation is idempotent: the repair outcome is folded in
  exactly once, however many passes run, because the fold reads its own ledger
  rows.

## Legacy behaviour

Everything here reconstructs only from durable proof:

- a plan whose context cannot be re-derived is `stale_but_revalidatable`, not
  silently reusable and not thrown away;
- a plan whose identity cannot be proven is `not_reusable`;
- a stop AO cannot name is `unrecoverable` with `unknown_condition` — an
  **answer**, not an HTTP 500, and never a repair;
- pre-P1-B runs fall back to `suggest`;
- no attempt, review, fix or repair generation is ever invented.

## API and CLI

| Operation | Route | CLI |
| --- | --- | --- |
| assess | `GET /workflows/{id}/recovery` | `ao workflow recover status <id>` |
| resume | `POST /workflows/{id}/resume` | `ao workflow resume <id>` |
| reuse plan | `POST /workflows/{id}/plan/reuse` | `ao workflow plan reuse <id>` |
| regenerate plan | `POST /workflows/{id}/plan/regenerate` | `ao workflow plan regenerate <id>` |
| repair | `POST /workflows/{id}/repair` | `ao workflow repair <id>` |

`repairPolicy` is accepted on run creation.

`POST /workflows/{id}/continue` is unchanged and still what the wake poller
drives — and its response now carries the `recovery` block, so it states which
recovery action AO selected instead of returning a meaningless success. A
deployment without the recovery capability answers exactly as it did before.

A repair refusal is a `409` with `REPAIR_NOT_AVAILABLE` and the reason. An
ineligible condition is a correct answer about the run, not a server fault.

## UI contract

**The frontend renders the backend's assessment. It does not decide safety.**

`WorkflowRecoveryPanel` shows what happened, the recommended action, whether AO
may take it automatically, the blocking condition, the plan status, repair
availability and the outstanding obligation — and offers Resume / Reuse plan /
Regenerate plan / Repair **only** when the assessment authorises each one.

An action the backend has refused is *absent*, not disabled and not "try
anyway": offering a button whose backend will refuse it is how a UI teaches
people to ignore it. A backend refusal is surfaced as a refusal and never
rendered as success. `offeredOperations` is the whole of the renderer's
authorization logic, and every input to it is a field the daemon computed.

## Where the code lives

| Concern | File |
| --- | --- |
| Vocabulary, repair policy, RepairIntent | `backend/internal/domain/recovery.go` |
| Recovery + repair facts on stop reasons | `backend/internal/workflow/attention.go` |
| The assessment | `backend/internal/workflow/recovery_assessment.go` |
| Obligations and Resume | `backend/internal/workflow/resume_obligation.go` |
| Plan reuse / regeneration | `backend/internal/workflow/plan_reuse.go` |
| Revision identity | `backend/internal/workflow/plan_revision.go` |
| Repair Agent | `backend/internal/workflow/repair_agent.go` |
| Migration | `backend/internal/storage/sqlite/migrations/0139_workflow_plan_revision.sql` |
| API | `backend/internal/httpd/controllers/workflow_recovery.go` |
| CLI | `backend/internal/cli/workflow_p1b_recovery.go` |
| UI | `frontend/src/renderer/components/workflow-recovery-panel.tsx`, `hooks/useWorkflowRecovery.ts` |
