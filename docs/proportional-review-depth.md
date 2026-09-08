# Proportional execution and review depth

*Status: P5-A phase 1 implemented. Later phases are the explicit roadmap at the
end of this document; nothing in them is implied by what ships here.*

## The problem, stated against what AO already does

AO runs every mutating change through the same six-step durable chain —
`plan → work → review → fix → verify → advance` — and, since Checkpoint 8I, it
already decides **whether** a reviewer runs at all from a deterministic policy
over durable facts (`workflow/review_policy.go`, `EvaluateReviewPolicy`).

What that policy does *not* decide is **how much reviewing** a change gets when
a reviewer does run. There is exactly one review shape: a full independent
reviewer, handed the objective and the acceptance criteria, told to inspect the
worktree and judge everything. It is the right shape for a migration and a
wildly disproportionate one for a three-line change in one package — and because
`EvaluateReviewPolicy` is deliberately conservative (`ReasonDefaultConservative`
resolves to `REQUIRED`), the disproportionate case is the common one.

So the cost of an ordinary small change is a full independent review pass, and
the practical loop a person actually wants —

> objective → the agent implements → report → **bounded** review → concrete fix
> if needed → delivery

— is not representable. This document adds the missing axis and nothing else.

## What already exists (and is therefore not rebuilt here)

Inspected before designing. Every one of these is reused as-is:

| Concern | Where it lives | Status |
|---|---|---|
| Which of task/autonomous/master a run is | `domain/execution_strategy.go`, frozen in `policy_snapshot` | reused unchanged |
| Whether a reviewer runs at all | `workflow/review_policy.go` `EvaluateReviewPolicy` | reused; one trigger added |
| Sensitive-path risk tables | `workflow/review_policy.go` (`authOrSecurityPathPatterns`, …) | reused; payments table added |
| Durable, replayable risk decision | `review_policy_decision` checkpoint | reused as the depth decision's input |
| Narrowing what Verify runs | `workflow/verify_scope_policy.go` `NarrowVerificationPlan` | reused unchanged |
| Frozen-at-creation policy axes | `domain.WorkflowPolicy` + `Effective*Policy()` | pattern followed exactly |
| Per-run token/cost ceiling | `workflow/usage_budget.go` | reused unchanged |
| Bounded, provenance-carrying evidence | `workflow/evidence_snapshot.go` (`EvidenceStatus`) | pattern followed |
| Review→fix delivery proof | `workflow/fix_delivery_report.go` | reused unchanged |
| Reviewer independence / RBAC / credentials | `ExecutionPolicySnapshot.ReviewIndependence`, `docs/rbac.md`, `docs/agent-identity.md` | untouched |

**No schema migration is required**, for the same reason P1-A needed none: the
new axis is another field on the run's single frozen `policy_snapshot`, and the
new decision is another append-only `workflow_checkpoints` row.

## The new axis: review depth

Execution strategy answers *how much orchestration*. Approval policy answers
*who drives*. Neither answers *how much scrutiny a delivered change gets*, and
folding that into either would repeat exactly the mistake P1-A untangled.

```
Execution strategy   task | autonomous | master     how much orchestration
Approval policy      automatic | manual             who approves and drives
Review depth         none | light | deep            how much scrutiny  <- new
```

| Depth | What the reviewer is asked to do |
|---|---|
| `deep` | Today's review, byte-identical. Full independent pass over the worktree against the objective and acceptance criteria. |
| `light` | A bounded pass: judge the **diff** against the acceptance criteria and the evidence AO hands it. Do not re-read the repository. Do not run a full suite. |
| `none` | No reviewer process. AO's own deterministic Verify remains the gate. |

### Depth is a request; the risk tier is the floor

The user (or the strategy default) states a **requested** depth. A deterministic
**risk tier**, derived from the risk decision AO already computes, states a
**minimum**. The effective depth is the higher of the two, always, and the clamp
is recorded with the reasons that caused it.

```
effective = max(requested, riskTier.MinimumDepth())
```

This is the whole non-degradability guarantee, and it is one line rather than a
set of special cases scattered across call sites.

## Risk matrix

The tier is a pure function of the `ReviewPolicyDecision` AO already persists at
review cycle 1. No new signal, no second LLM call, no divergent risk evaluation.

| Tier | Minimum depth | Triggering `ReviewReason` |
|---|---|---|
| **high** | `deep` — **non-degradable** | `auth_or_security_path`, `payments_or_billing_path`, `migration_or_schema_path`, `concurrency_sensitive_path`, `infrastructure_or_cicd_path`, `public_api_or_contract_path`, `dependency_or_security_config_change`, `destructive_operation_intent` |
| **standard** | `light` | `large_or_multi_module_change`, `prior_work_provider_attempts`, `ambiguous_acceptance_criteria`, `verify_coverage_insufficient`, `default_conservative_required`, `no_changed_files_observed` |
| **low** | `none` | `docs_only_change_fully_verified`, `exact_content_single_file_change` |

Two properties this table is built to have:

- **Any high reason wins.** Tiers are computed as the maximum over all reasons,
  so one auth path among forty docs files is still `high`.
- **`low` is exactly today's `SKIPPED`.** The two `low` reasons are precisely the
  two `EvaluateReviewPolicy` already resolves to `ReviewSkipped`. So the `none`
  depth **cannot skip a review that AO does not already skip today**. That is
  deliberate and it is the phase-1 safety line: a depth of `none` for an
  ordinary code change would rest delivery on the worker's own prose, which is
  the one thing this design refuses. Widening it is Phase 2 and needs the
  worker-report and pre-review verification contracts described there.

`payments_or_billing_path` is new. Payments were named as non-degradable and the
existing tables did not cover them; the patterns are deliberately narrow
(`payment`, `billing`, `invoice`, `checkout`, `stripe`, `paypal`, `refund`,
`payout`) to avoid false positives. Adding a `REQUIRED` trigger only ever makes
`EvaluateReviewPolicy` stricter, never looser.

## Defaults per strategy

Frozen at run creation by the same statement that freezes the strategy, exactly
as the repair and autonomy policies are:

| Strategy | Requested depth | Effect after the clamp |
|---|---|---|
| `task` | `none` | low → no reviewer (as today); standard → **light**; high → **deep** |
| `autonomous` | `light` | low → light; standard → **light**; high → **deep** |
| `master` | `deep` | every tier → **deep**. Unchanged from today in every case. |

**Master's behaviour does not change at all.** A master run requests `deep`, and
`max(deep, anything)` is `deep`. That is the migration-safety property: this
change cannot alter what an existing master or high-risk run does, no matter how
the policy is configured.

A run created before this exists carries no depth. `EffectiveReviewDepthPolicy`
reads that zero value as `deep` — the behaviour those runs already had — never as
a cheaper depth nobody chose.

## Escalation

Escalation raises the depth the **next** review cycle is dispatched at. It never
lowers anything, it is recorded durably, and — importantly for a lifecycle this
delicate — it changes no state transition: the existing review/fix loop already
produces the next cycle, and it simply reads a higher depth when it does.

| Trigger | Rule | Reason code |
|---|---|---|
| Risk tier | `requested < tier.MinimumDepth()` at cycle 1 | `clamped_by_risk_tier` |
| The reviewer asks | A `light` review's `changes_requested` body begins with `AO-ESCALATE:` | `escalated_by_reviewer` |
| Non-convergence | A `light` review has produced `ReviewDepthMaxLightCycles` (2) `changes_requested` cycles | `escalated_by_cycles` |

The escalation channel is why the light prompt is safe: a bounded reviewer that
finds itself out of its depth has a cheap, machine-checkable way to say so, and
it is the reviewer's *judgement* driving a *deterministic* AO rule rather than
AO trying to guess from prose.

**Exhaustion never approves.** A review that runs out of time, dies, or ends
without a durable verdict already reaches `ambiguous_review_state` or
`review run ended as failed` — never `approved`. Depth changes nothing about
that, and the behaviour is pinned by test.

## The light review contract

`BuildReviewPrompt` gains a depth. At `deep` it produces byte-identical output
to what it produced before, so nothing that runs deep can have changed.

At `light` the reviewer is handed a bounded evidence pack, all of it AO-observed
rather than agent-reported:

| Field | Source | Provenance |
|---|---|---|
| Changed paths + git status codes | `ReviewPolicyDecision.Facts.ChangedFilePaths` (from `ObserveWorkspace`) | observed |
| Changed-file count | same | observed |
| Risk tier and its reasons | the persisted depth decision | derived, deterministic |
| The commands and file checks AO will run at Verify | the run's `VerificationPlan` | declared |
| Base commit and reviewed fingerprint | the work step's completion checkpoint | observed |
| Prior worker provider attempts | `workflow_attempts` | observed |

and told, in the prompt itself, that:

- the **diff and the tests are the evidence**; the worker's own account of what
  it did is not in front of it and is not evidence;
- it must **not** re-read the repository beyond what the diff requires, and must
  **not** run a full suite;
- AO will independently run the listed verification afterwards, so it does not
  need to prove what AO is about to prove;
- if the change is outside what a bounded review can honestly judge, it must
  escalate rather than approve.

Everything else — the read-only guardrails, the scope boundary from
`ReviewTaskScope`, the `ao review submit` verdict protocol — is unchanged and
shared with the deep prompt.

## Isolation, provenance and credentials

Nothing here relaxes any existing check, and the reason it does not need to is
that all of them are upstream of it:

- The reviewed target is still the work step's own **content-aware workspace
  fingerprint**, pinned per cycle. Depth does not touch `targetSHA`, the fresh-
  review generation machinery, or any SHA/CAS comparison.
- The workspace is still whatever the run's **placement** made it (isolated
  worktree or direct branch under the repository+branch execution lock), so a
  light review is scoped to this task's own changes by construction, not by
  asking the reviewer to be careful.
- Reviewer **independence** (`ExecutionPolicySnapshot.ReviewIndependence`),
  agent credentials, RBAC and tenant isolation are read exactly where they were;
  depth is not consulted by any of them and does not appear in any launch,
  credential or routing decision.
- No remote write. The light prompt keeps the existing prohibition on `gh`, on
  pushing, and on touching a pull request.

## Where the code lives

| Concern | File |
|---|---|
| Depth/tier vocabulary, clamp, frozen snapshot | `backend/internal/domain/review_depth.go` |
| Frozen field + forward-compatible default | `backend/internal/domain/workflow_policy.go` |
| Tier mapping, resolution, durable decision, escalation | `backend/internal/workflow/review_depth.go` |
| Payments risk trigger | `backend/internal/workflow/review_policy.go` |
| Cycle-1 resolution and prompt wiring | `backend/internal/workflow/review_dispatch.go` |
| Light prompt | `backend/internal/workflow/review_prompt.go` |
| Reviewer-driven escalation detection | `backend/internal/workflow/review_progress.go` |
| Creation-time freeze | `backend/internal/workflow/execution_strategy.go` |
| API request/response | `backend/internal/httpd/controllers/workflow.go` |
| UI | `frontend/src/renderer/routes/_shell.workflows.tsx` |

## Roadmap — deliberately not in phase 1

Each of these is a separate, separately-approvable change. None is implied by
what ships above.

**Phase 2 — the worker report and pre-review test evidence.** Today a review
runs *before* Verify, so at review time AO holds no test evidence at all and the
only account of what was tested is the worker's prose, which the worker prompt
itself labels informational. Two additions close that:

- an **AO-assembled** structured worker report (changed files, fingerprints,
  attempt history) following the `EvidenceStatus` provenance pattern, with the
  worker's own claims carried in a separately-labelled `claimed` section that is
  never load-bearing;
- a **pre-review verification pass** that runs only the `RetrySafe` commands of
  the run's existing `VerificationPlan` and hands the results to the reviewer.
  This is plausibly a net *latency win* rather than double work, because Verify
  is already keyed by `verificationTargetKey(fingerprint, plan)` and an unchanged
  fingerprint should let the later Verify reuse the result — but that reuse must
  be proven against the verify authority machinery before it is relied on, which
  is why it is not asserted here.

With those two in place, widening the `low` tier so an ordinary small change can
legitimately reach depth `none` becomes a decision that rests on AO-run tests
rather than on agent prose. That widening is itself a third, separate decision.

**Phase 3 — post-approval escalation.** Phase 1 escalates the *next* cycle,
which covers the reviewer-asks and non-convergence triggers but not "a light
review approved and AO wants a second, deeper opinion on the same target". That
needs a new `freshReviewPurpose` (`depthescalation`) driving the existing
`VerifyFreshReviewRecord` machinery — supersede the light run, reopen the review
step, dispatch one deep cycle at the same target. The machinery exists; wiring a
fifth purpose into it is a change to a delicate state machine and deserves its
own checkpoint and its own recovery tests.

**Phase 4 — per-phase time and cost observability.** Checkpoints already carry
`DurablePhase` and `CreatedAt`, and the usage ledger already attributes tokens
and cost per run and per session. Folding those into a per-phase view
(scheduling, work, review, fix, verify, waiting, recovery) is additive and
read-only, but it is a new API surface with its own DTO, spec regeneration and
tests.

**Phase 5 — a review-depth time budget.** `usage_budget.go` bounds spend and
refuses to *start* new work over the ceiling, deliberately never killing a
running attempt. A per-depth wall-clock bound would follow the same rule — a
light review that exceeds its bound escalates or stops, and under no
circumstances approves — reusing `reviewStalenessThreshold`'s existing
never-assume-success framing rather than inventing a second timeout model.
