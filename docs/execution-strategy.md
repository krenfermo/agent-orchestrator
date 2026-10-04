# Execution strategy and approval policy

*Status: implemented (P1-A). This document is the contract later P1 work builds on.*

AO runs work under exactly two independent decisions. They were previously
tangled together in a set of implicit booleans; P1-A separates them, names
them, and makes the first one durable.

| Axis | Values | What it decides |
| --- | --- | --- |
| **Execution strategy** | `task`, `autonomous`, `master` | How much orchestration the run gets |
| **Approval policy** | `automatic`, `manual` | Who approves the plan and drives the run |

They are orthogonal. Every one of the six combinations is representable and
none of them moves the other.

## Why "Manual" is not a strategy

Before P1-A the create-workflow UI offered "Manual workflow" and "Autonomous
workflow". Auditing what those actually did:

- `masterPlan: true` decided whether an objective planner ran at all.
- `planApprovalMode: manual|auto` decided whether a generated plan waited for
  a person.
- `autonomous: bool` was frozen into the run's policy snapshot as
  `ExecutionPolicySnapshot.AutonomousMode`, and decided whether AO scheduled
  its own wake-ups and kept going unattended.

"Manual" was setting the second and third of those. It was never an execution
strategy — it was an **approval policy**, and it stays one. Nothing about its
behaviour changed; it moved to the axis it always belonged on.

## The three strategies

### Task

A small or bounded change with minimal orchestration:

```
task → normalized objective → worker → review → fix → verify → advance
```

There is no objective planner, no plan row, no decomposition and no
hierarchy. What a task **does** keep is the entire durable lifecycle: the same
six-step chain, the same review/fix/verify guarantees, the same crash
recovery. *Task does not mean "skip review" and does not mean "unsafe" — it
means reduced orchestration overhead.*

A task run carries its own acceptance criteria and its own declared
`writeIntent` (`mutating` or `read_only`), bound by the same transaction that
creates the run, because a task has no planner to supply them later. An
undeclared write intent is treated as mutating everywhere, exactly as it is
for a planned task: a read-only task must say so, and that declaration is what
lets a correctly-unchanged worktree read as success rather than as
`ambiguous_worker_state`.

A task run frozen as `task` can never be pushed through the master planner —
`GeneratePlan` refuses it — and the refusal survives restarts because the
strategy does.

### Autonomous

The standard multi-step mode and the default for normal project work:

```
objective → planner → durable plan → dependency-ordered tasks
         → review/fix → verify → parent convergence → completed
```

This is what AO already did for every run the previous UI created. Its
behaviour is unchanged in every respect.

### Master

A large initiative deliberately decomposed into coordinated child
workstreams. It uses the **same** durable planner, plan and hierarchy
machinery autonomous does — P1-A adds no second planner. What master records
is that a person (or the policy, on a deliberate signal) chose breadth: this
is an initiative, not a feature.

Master is never selected by accident. Only an explicit request or one of three
deliberate declarations (§ AUTO rules) can reach it.

## AUTO selection

`strategy: "auto"` asks AO to choose. The rules are a pure function of the
request — no model is consulted, because a strategy decision must stay
reproducible and explainable for exactly the runs whose provenance matters
most. In order:

1. an **explicit** request is honoured, always;
2. `multiWorkstream` → **master** (`multi_workstream_initiative`);
3. `requiresDecomposition` → **master** (`decomposition_required`);
4. `repositoryCount > 1` → **master** (`multi_repository`);
5. `suppliedPlanHierarchy` → **autonomous** (`supplied_plan_hierarchy`);
6. `size: small`, or `expectedSteps` within `ExecutionStrategyMaxTaskSteps`
   → **task** (`bounded_work`);
7. everything else → **autonomous** (`multi_step_project`).

A request with no signals at all resolves to autonomous, which is the safe
default for project work.

## Persistence and provenance

The selection is frozen into `workflow_runs.policy_snapshot` as
`WorkflowPolicy.Strategy`, alongside the routing, wake and execution-policy
snapshots already frozen there — one row per run holding the whole
decision-making configuration. **No schema migration was required.**

It is written by the *same statement that creates the run*, not by a
follow-up write, so there is no crash window in which a run exists without a
strategy. (That is CP3's lesson about the execution-policy freeze, applied up
front instead of after an incident.)

Every selection records:

| Field | Meaning |
| --- | --- |
| `requested` | what the caller asked for, including `auto` |
| `effective` | what the run actually executes under |
| `source` | `explicit`, `policy`, `inherited`, or `recovered` |
| `policyVersion` | the selection-policy version in force at the decision |
| `reason` | a stable machine-checkable explanation code |
| `signals` | the AUTO inputs, so the decision can be *replayed* |
| `parentRunId`, `depth` | a child's place in the bounded hierarchy |

Once written it is **never recomputed** — not by a restart, not by a resume,
not by a recovery pass, not by a change to the selection rules, not by a
`ContinueRun`. Readers resolve it from the run's own snapshot. A daemon
running newer rules over an older run still reports the older run's original
decision, its original policy version and its original reason.

## Master child strategy

A planned child's strategy is derived from its parent's frozen selection and
stamped by the child's own creation statement. Two invariants:

- **A child is never `master`.** Recursive decomposition would let one
  objective fan out without bound.
- **Depth is bounded** by `ExecutionStrategyMaxChildDepth` (1), so a master
  objective decomposes exactly once, into workstreams that execute.

The child's own strategy is `task`. §J of the P1-A brief describes master
children as normally autonomous; that describes a planner emitting
sub-objectives. AO's planner emits **bounded leaf tasks** with their own
acceptance criteria and write intent — which is precisely §J's stated
exception, and precisely what a task is. Recording those children as
autonomous would name a decomposition that never happens. If AO later grows a
planner that emits sub-objectives, `ChildExecutionStrategy` is the single
function that decision lives in.

## Legacy compatibility

A run created before P1-A carries no selection at all. Boot reconciliation
maps it, once, from durable facts that really exist, and records the mapping
as `recovered` — never as a choice somebody made:

| Durable fact | Mapped strategy | Reason code |
| --- | --- | --- |
| the run is a planned child | `task` | `legacy_planned_child` |
| the run owns a `workflow_plans` row | `autonomous` | `legacy_planned_run` |
| neither | `task` | `legacy_single_task_run` |

A legacy objective is mapped to **autonomous, never master**: no legacy row
records that anybody chose a large initiative, and inventing that would be
inventing history. Each answer names what the run already does, so the mapping
changes no behaviour. Once recorded it is stable, because it is no longer
re-derived.

Pre-P1-A **API clients** are equally unaffected. A create request that omits
`strategy` is mapped from its `masterPlan` flag to the strategy that flag has
always meant — `true` → autonomous, absent → task — and recorded as a policy
selection. What those clients get does not change; it is only written down.

## API

`POST /api/v1/projects/{projectId}/workflows` accepts:

```jsonc
{
  "objective": "…",
  "strategy": "task | autonomous | master | auto",
  "strategySignals": {
    "expectedSteps": 0, "requiresDecomposition": false,
    "repositoryCount": 0, "multiWorkstream": false,
    "suppliedPlanHierarchy": false, "size": "small | medium | large"
  },
  "approvalPolicy": "automatic | manual",
  "acceptanceCriteria": ["…"],          // task strategy
  "writeIntent": "mutating | read_only" // task strategy
}
```

Anything outside those vocabularies is a `400` with
`INVALID_EXECUTION_STRATEGY` / `INVALID_APPROVAL_POLICY`, refused **before**
a run exists. The legacy `masterPlan`, `planApprovalMode` and `autonomous`
fields still work exactly as before.

Every run view carries:

```jsonc
"executionStrategy": {
  "requestedStrategy": "auto",
  "effectiveStrategy": "master",
  "selectionSource": "policy",
  "policyVersion": "v1",
  "reasonCode": "multi_workstream_initiative",
  "parentRunId": "…", "depth": 1
}
```

It is omitted, rather than defaulted, when a list response cannot state it
without a second query — a run whose strategy the response cannot state is a
run it should not claim to know.

## Where the code lives

| Concern | File |
| --- | --- |
| The model, the AUTO policy, child rules | `backend/internal/domain/execution_strategy.go` |
| Frozen field on the policy snapshot | `backend/internal/domain/workflow_policy.go` |
| Freeze, resolve, legacy heal, planner guard | `backend/internal/workflow/execution_strategy.go` |
| Task creation | `Coordinator.CreateTaskRun` (`workflow/workflow.go`) |
| Objective creation | `Coordinator.CreateObjectiveRunWithStrategy` (`workflow/master_coordinator.go`) |
| Child stamping | `dispatchMasterTask` (`workflow/master_coordinator.go`) |
| Legacy heal on boot | `reconcileRun` (`workflow/recovery.go`) |
| API request/response | `backend/internal/httpd/controllers/workflow.go` |
| UI | `frontend/src/renderer/routes/_shell.workflows.tsx` |
