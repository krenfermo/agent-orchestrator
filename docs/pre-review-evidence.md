# Pre-review evidence and the fast verifiable Task (P5-A phase 2)

Phase 1 made review depth proportional to a change's risk. It could not make
`none` reachable for ordinary code, and the reason was structural rather than
cautious: **review runs before verify**, so at the moment AO chose a depth the
only thing it knew about a change was which paths it touched. "Does it work"
was not among the available facts, because nothing had run it.

Phase 2 closes that gap. Before choosing a depth, AO runs the task's own planned
verification itself, through the same authorized runtime Verify uses, against
the exact tree about to be reviewed. That measurement — and only that
measurement, never the worker's account of it — can lower the review floor for
ordinary code by one step.

## The shape of a fast Task

```
objective → worker implements → (optional) structured report
          → AO runs the planned checks itself   ← pre_review_evidence
          → risk tier + evidence → depth decision
          → no reviewer | light review | deep review
          → Verify (reusing the evidence when the tree has not moved)
          → done
```

## What is evidence, and what is not

AO holds two accounts of every change and never lets one stand in for the other.

| | Produced by | Status in policy |
|---|---|---|
| **Pre-review evidence** | AO, via `VerifyRunner` | Evidence. Can lower a floor by one step. |
| **Worker report** | the party under review | A declaration. Can only ever *deepen*. |

The worker's report is genuinely useful — it tells a reviewer where to look and
what the worker thinks it left undone — and it is never a reason to review less.
The one predicate policy reads from it is `AdmitsAnyFailure`, and believing a
confession costs nothing: the worst case is a fuller review of work that was
fine. A claim contradicted by an observation is recorded as a contradiction and
independently blocks any relief.

## When the floor may drop

`EvaluateReviewEvidenceRelief` is pure, and every clause is a refusal. The floor
drops from `light` to `none` only when **all** of these hold:

- the risk tier is exactly `standard` — `high` and any unrecognised tier are
  untouched, and `low` needs no relief because its floor is already `none`;
- ReviewPolicy recorded no reason a passing suite does not answer (thin
  verification, ambiguous acceptance criteria, a large or multi-module change,
  prior worker attempts, or no observed change);
- the run is **not a child of another run** — a master's work is done by its
  children, and relieving them would spend the master guarantee nobody chose to
  spend;
- AO **observed** the checks: they ran, through the verification runtime, and
  every one passed;
- at least one command actually executed;
- the evidence is about **this** tree — the fingerprint matches the review
  target, and did not move while the checks ran;
- the worker admitted nothing and contradicted nothing.

Failures, timeouts, an unavailable runtime, an unattributable result and a plan
with nothing to run are all the same answer: **no relief**. None of them is
evidence a change is safe.

Note what relief does *not* do. It skips the **reviewer**, never Verify. The
change is still verified on Verify's own authority before the run completes.

## Durable records

Everything is an append-only checkpoint on the existing steps. There is no new
step kind, no new table and no fifth review purpose — `WorkflowStepKind` is a
fixed vocabulary persisted in SQLite, and phase 2 needed nothing it does not
already contain.

| Phase | Written on | Contents |
|---|---|---|
| `pre_review_evidence` | review step | plan, commands, exit codes, bounded log tails, fingerprint before/after, scope, provenance, duration |
| `review_evidence_relief` | review step | granted/denied, the stable reason code, the evidence key it stands on |
| `review_depth_decision` | review step | phase 1's decision, now resolved against the relieved floor |
| `review_skipped_by_evidence` | review step | why nobody reviewed: reason, evidence key, status, fingerprint, tier, depths |
| `work_report` | work step | the worker's bounded, versioned declaration |

`review_skipped_by_evidence` is deliberately **not** `review_policy_skipped`.
The latter means "the paths alone said no reviewer was needed"; the former means
"AO ran the checks, watched them pass, and the policy allowed the skip on that
basis". A reader must be able to tell those apart forever.

## Reuse in Verify

The evidence pass and Verify key their work with the same function,
`verificationTargetKey(fingerprint, narrowedPlan)`. When a verification's key
matches a recorded evidence pass, its passing command results are replayed
verbatim instead of executed again, and the attempt records `reusedCheckCount`.

Reuse is safe **without touching the review-authority invariant**, and the order
of operations is why:

1. Verify derives `reviewed` from the approval (or from the work step's
   completion fingerprint on a skip path) — unchanged.
2. Verify re-observes the worktree and **refuses to proceed unless
   `pre == reviewed`** — unchanged.
3. Only then is reuse consulted, on that already-proven fingerprint.

So the authority question is answered *before* reuse is asked, never instead of
it. Nothing here relaxes a SHA or a CAS check. A fix cycle changes the tree,
which changes the key, which ends reuse by construction rather than by a rule
anyone has to remember.

Two further limits: only **passing** checks are replayed — a failure is re-run
by Verify, because a failing verification is the entry point to infra
classification, transient retries and the fix cycle — and file checks are always
performed by Verify itself.

## Bounds

- `preReviewEvidenceMaxCommands` (12): a plan larger than this is a suite, and a
  suite belongs to Verify, which has the budget and attempt accounting for it.
- `preReviewEvidenceDefaultTimeout` (3 min): shorter than Verify's 10, because
  this pass exists to be cheap.
- The scope is `ComputeVerifyScope` + `NarrowVerificationPlan` — the same
  narrowing Verify applies, so a three-line change does not run a repository
  suite and a sensitive change still does.

## Observability

`PreReviewEvidence.DurationMS` records what the evidence pass cost, and
`VerifyResult.ReusedCheckCount` records how many executions it saved. Together
with the attempt timestamps AO already keeps for review and verify, that is
everything a per-phase cost view needs. Building that view is phase 4.

## Still outstanding

- **Phase 2B** — the worker-report *transport*: `ao work report` plus its HTTP
  route. The contract, validation, storage and consumption all exist and are
  tested; only the wire path is missing, and it carries no safety semantics.
- **Phase 3** — post-approval escalation.
- **Phase 4** — full per-phase metrics.
- **Phase 5** — time budgets per depth.
