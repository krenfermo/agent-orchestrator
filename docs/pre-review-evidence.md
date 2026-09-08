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
objective → worker implements → (optional) `ao work report`
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
| `work_report` | the run (no step id — see below) | the worker's bounded, versioned declaration |

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

## When AO does not look at all

A change whose review floor is **already deep** — its risk tier demands a full
independent pass, or the run asked for one — gets no pre-review pass. Nothing
about that review could change if it did: `max(request, floor)` is deep whatever
the checks say, the deep prompt does not render evidence, and Verify runs the
same plan itself on its own authority minutes later.

Running it early would buy one reuse when the tree holds still, and cost one
entire wasted suite the moment a fix cycle moves it — which on a high-risk
change is a *repository-wide* suite, because `VerifyScopePolicy` deliberately
refuses to narrow exactly those.

The choice is recorded as `not_needed`, which is its own status precisely
because it is neither `not_planned` nor `unavailable`: there was a plan, AO
could have run it, and it chose not to. A reader must be able to tell "AO chose
not to look" from "AO looked and could not tell".

## The transport (phase 2B)

A worker records its declaration with:

```
ao work report --json -          # or --summary / --test / --limitation / ...
```

addressed by **session**, defaulting to `AO_SESSION_ID`, which AO sets in every
pane it launches. The route is `POST /api/v1/sessions/{sessionId}/work-report`,
under the same session-ownership scoping and the same session-write permission
`ao review submit` uses — no new permission, no new ownership model, no new
table.

Addressing by session rather than by run id is what makes a worker only able to
report on the work it is actually doing: the session is the thing it provably
is, whereas a run id is a string it would have to be told.

Four states are refused:

| State | Answer |
|---|---|
| No non-terminal run has a work step in this session | `404` — covers a cancelled run, a finished one, and a pane from a superseded launch generation |
| The run has already resolved its review depth | `409` — the report can no longer inform the decision it sits next to |
| The body is unusable | `400` |
| The daemon predates the capability | `501` |

A second report supersedes the first as the one policy reads; both stay on the
ledger, because the write is append-only and a worker that changes its story
should leave a history rather than an edit.

The report is stored **run-scoped, with no step id**. That is load-bearing: a
checkpoint carrying a step id becomes that step's latest, and `dispatchReviewStep`
reads exactly that row to recover the session, worktree, branch and completion
fingerprint. A report attached to the work step displaced those facts the moment
it was newer — which is every real run — and the dispatch then stopped as
`ambiguous_review_state`.

## Worker identity (phase 2C)

Under OIDC a worker needs a credential of its own, and there is an ordering
problem in the way of one:

- the CLI finds its credential only through `AO_AGENT_CREDENTIAL_FILE`, set in
  the pane's environment **at spawn**;
- a credential must be bound to the session it may speak for;
- a worker's session **does not exist until that spawn returns**.

A reviewer has no such problem: its credential is bound to the *worker's*
session, which already exists. So a worker credential is minted **unbound**,
handed over, and bound exactly once when the launch reports which session it
produced:

```
1. Issue(role=worker, run, step, attempt, project, owner)   session_id = ''
2. env[AO_AGENT_CREDENTIAL_FILE] = <dataDir>/agent-credentials/workflow-worker-<attempt>
3. Spawn(...)                                               -> session
4. BindAgentCredentialSession(credID, session)              CAS, once
```

The window between 1 and 4 grants nothing: `AgentAuthority.MayReachSession`
refuses every session route while `SessionID` is empty, so an unbound credential
is **inert rather than permissive**. The bind is one guarded statement
(`WHERE id = ? AND session_id = '' AND revoked_at IS NULL`), so it can only ever
move a credential from unbound to bound, once — a second bind, on any session,
changes nothing, and a revoked credential can never be resurrected into one.

**A binding that does not land is a failed launch.** The credential is taken
back and its file removed, because a pane holding a token AO cannot account for
is the state the mechanism exists to prevent.

### What a worker credential can do

Its ceiling is the pre-existing `AgentRoleWorker` one — session read/write and
workflow read — bound to one project, one run, one step and one attempt. Project
binding is what carries cross-project *and cross-tenant* refusal: a project
belongs to exactly one tenant, and `AgentAuthority.Allows` denies every project
but the bound one whatever the account behind it may do elsewhere.

### When it ends

| Ending | How |
|---|---|
| Spawn failed, launch named no session, bind failed | Immediately, by the launcher |
| Replaced by a new attempt on the same step | Immediately, by the launcher, keyed on the **attempt** |
| Turn ended, cancelled, failed, step gone | The derived sweep, ≤ one reconcile interval |

The sweep is the guarantee and the eager paths are optimizations, which is the
same split the reviewer half already makes. Its rule mirrors the review one
exactly:

> a worker credential may live exactly as long as its work step is running.

`NOT EXISTS` rather than a state comparison, so a step whose row is gone counts
as finished too — an orphan left by a daemon that died mid-launch is discharged
on the next boot rather than living out its TTL. Nothing is remembered and
nothing needs replaying: a pass that failed and a pass that never ran are the
same situation next time round.

Replacement is the one ending the sweep cannot see — a step being re-dispatched
is still running — so it is handled explicitly at the single site that knows a
replacement is happening, rather than at the twenty-three places a step can
transition.

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

- **Phase 3** — post-approval escalation.
- **Phase 4** — full per-phase metrics.
- **Phase 5** — time budgets per depth.
