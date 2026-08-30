package workflow

import (
	stdctx "context"
	"errors"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// advanceReviewFixCycle is Checkpoint 8D's automatic-progression cascade: a
// single call that observes a running review step, dispatches the next fix
// cycle when eligible, observes a running fix step, and dispatches the next
// review cycle when eligible — all within one call, so repeated GetRun polls
// (the frontend's existing 2s interval while the run is non-terminal) and
// boot Reconcile drive the whole review<->fix loop forward without a human
// re-clicking Continue after every single transition. This is a deliberate
// continuation of 8B/8C's existing precedent that GetRun (a GET) already has
// real side effects (it writes DB rows via observation; 8B's dispatchWorkStep
// is also opportunistically re-entered from GetRun) — 8D extends the same
// precedent to also drive review/fix dispatch, not a new kind of violation.
//
// includeCycle1Unblock controls whether the very first review dispatch (the
// work-just-completed, review step still "pending" edge) is allowed here.
// GetRun and Reconcile pass false: that unblock stays ContinueRun's explicit,
// human/API-triggered job (mirrors 8C's original behavior — see
// TestReviewStepUntouchedAfterWorkCompletion). ContinueRun passes true.
// Every other transition in this cascade (fix dispatch, fix observation,
// cycle N+1 review dispatch, an already-"ready" review step's crash-recovery
// resume) is allowed from all three call sites — the same idempotency guards
// that make 8B's/8C's dispatch functions safe to call redundantly (outbox
// idempotency keys, the fix step's attempt-count guard, the review step's
// already-reviewed-fingerprint guard) make this equally safe against the 2s
// poll interval re-triggering dispatch redundantly.
func (c *Coordinator) advanceReviewFixCycle(ctx stdctx.Context, run domain.WorkflowRun, steps []domain.WorkflowStep, includeCycle1Unblock bool) (domain.WorkflowRun, error) {
	var workStep, reviewStep, fixStep, verifyStep *domain.WorkflowStep
	for i := range steps {
		switch steps[i].Kind {
		case domain.WorkflowStepWork:
			workStep = &steps[i]
		case domain.WorkflowStepReview:
			reviewStep = &steps[i]
		case domain.WorkflowStepFix:
			fixStep = &steps[i]
		case domain.WorkflowStepVerify:
			verifyStep = &steps[i]
		}
	}
	if workStep == nil || reviewStep == nil || fixStep == nil {
		return run, nil
	}

	refreshRun := func() error {
		if r, ok, err := c.store.GetWorkflowRun(ctx, run.ID); err == nil && ok {
			run = r
		} else if err != nil {
			return err
		}
		return nil
	}

	// 0. Does this step's review still speak for it?
	//
	// Ahead of everything below, because every step that follows acts on the
	// step's CURRENT state and its review_run_id, and this is the one check that
	// can tell those two are contradicting each other. observeReviewStep (step 1)
	// only ever looks at a RUNNING step, so a step resting at `waiting` over a
	// review AO had closed out was, before this, seen by nobody — at boot, on
	// wake, or on any poll. That is how wf-756988ae stayed waiting forever on a
	// review run it had itself cancelled.
	//
	// It launches nothing and never touches an intact pointer. See
	// review_authority.go.
	// Terminal review steps are included deliberately. A late approval may have
	// completed the step just before the process died, but before the durable
	// adoption marker was written. Reconciliation must be allowed to finish that
	// interrupted adoption on boot; ReconcileReviewAuthority itself remains
	// narrow and ignores every terminal step that is not in that exact shape.
	if !run.State.Terminal() {
		// Finish any reviewer termination whose intent is durable and whose
		// confirmation is not. A crash between the two leaves a live orphan AO
		// had already decided it wanted gone; replaying is idempotent, and doing
		// it first means nothing below reasons about a reviewer that should not
		// exist.
		if cerr := c.finishPendingReviewCancellations(ctx, run, *reviewStep); cerr != nil {
			if !errors.Is(cerr, ErrUnrecoverable) {
				return run, cerr
			}
			// A DETERMINISTIC refusal -- a reviewer session AO can neither prove
			// it owns nor prove is gone. Failing the whole cascade on it made
			// every reader of this run fail with it: boot reconciliation, every
			// wake, and an ordinary GET, which is how one stale pane turned into
			// a repeating 500 on a run nobody could even look at.
			//
			// Nothing is adopted and nothing is destroyed. The obligation stays
			// durable, the run is parked with a reason a person can act on, and
			// the cascade continues so the rest of this run's state is still
			// read and reported.
			if c.log != nil {
				c.log.Warn("workflow: a reviewer obligation could not be classified; parking this run and continuing",
					"run", run.ID, "step", reviewStep.ID, "err", cerr)
			}
			c.parkUnreconcilableRun(ctx, run, cerr, c.clock())
			if err := refreshRun(); err != nil {
				return run, err
			}
		}
		outcome, updatedStep, updatedRun, err := c.ReconcileReviewAuthority(ctx, run, *reviewStep)
		if err != nil {
			return run, err
		}
		run = updatedRun
		*reviewStep = updatedStep
		// Re-read the review step unconditionally, not only when the authority
		// call resolved something.
		//
		// A pass that returns "not applicable" may still have DISCOVERED that
		// authority moved — that is exactly what its revalidation is for — and
		// the caller's in-memory step then still names the review that lost it.
		// Everything below acts on that pointer: observation would read the old
		// run and apply its verdict with no guard at all, completing a step a
		// replacement had already taken. One cheap read removes the whole class.
		if refreshed, rerr := c.store.ListWorkflowSteps(ctx, run.ID); rerr == nil {
			for i := range refreshed {
				if refreshed[i].Kind == domain.WorkflowStepReview {
					*reviewStep = refreshed[i]
				}
			}
		}
		if outcome.Resolved() {
			if refreshed, rerr := c.store.ListWorkflowSteps(ctx, run.ID); rerr == nil {
				for i := range refreshed {
					switch refreshed[i].Kind {
					case domain.WorkflowStepWork:
						*workStep = refreshed[i]
					case domain.WorkflowStepReview:
						*reviewStep = refreshed[i]
					case domain.WorkflowStepFix:
						*fixStep = refreshed[i]
					case domain.WorkflowStepVerify:
						if verifyStep != nil {
							*verifyStep = refreshed[i]
						}
					}
				}
			}
		}
	}

	// 1. Observe a running review step's live verdict.
	if !run.State.Terminal() && reviewStep.State == domain.WorkflowStepRunning {
		updated, err := c.observeReviewStep(ctx, run, *reviewStep)
		if err != nil {
			return run, err
		}
		*reviewStep = updated
		if err := refreshRun(); err != nil {
			return run, err
		}
	}

	// 2. Dispatch the next fix cycle once a changes_requested verdict has
	// rested the review step at "waiting" (within budget).
	if !run.State.Terminal() && reviewStep.State == domain.WorkflowStepWaiting && reviewStep.ReviewRunID != nil {
		updated, err := c.maybeDispatchFix(ctx, run, *workStep, *fixStep, *reviewStep)
		if err != nil {
			return run, err
		}
		*fixStep = updated
		if err := refreshRun(); err != nil {
			return run, err
		}
	}

	// 3. Observe a running fix step's live workspace fingerprint.
	if !run.State.Terminal() && fixStep.State == domain.WorkflowStepRunning {
		updated, err := c.observeFixStep(ctx, run, *fixStep)
		if err != nil {
			return run, err
		}
		*fixStep = updated
		if err := refreshRun(); err != nil {
			return run, err
		}
	}

	// 4. Dispatch the next review cycle: "ready" or "running" always resumes
	// (crash recovery — dispatchReviewStep itself no-ops the common
	// already-dispatched "running with a review_run" case via its own
	// ReviewRunID guard); "waiting" resumes once the fix step has delivered
	// a new fingerprint (dispatchReviewStep itself re-checks this); "pending"
	// (cycle 1) only when the caller allows it.
	canDispatchReview := reviewStep.State == domain.WorkflowStepReady ||
		reviewStep.State == domain.WorkflowStepRunning ||
		reviewStep.State == domain.WorkflowStepWaiting ||
		(includeCycle1Unblock && reviewStep.State == domain.WorkflowStepPending)
	if !run.State.Terminal() && canDispatchReview {
		// includeCycle1Unblock is exactly "this call is the explicit,
		// human/API-driven Continue" (only ContinueRun passes true), which is
		// also the licence dispatchReviewStep needs to re-open a review cycle
		// whose launch stopped permanently — see its humanResume parameter.
		updated, err := c.dispatchReviewStep(ctx, run, *workStep, *fixStep, *reviewStep, includeCycle1Unblock)
		if err != nil {
			return run, err
		}
		*reviewStep = updated
		if err := refreshRun(); err != nil {
			return run, err
		}
	}

	// 5. Checkpoint 8P-E.13 Phase 5: a verification that failed repairably and
	// still has budget parked the run at verify_fix_reentry. Hand those
	// findings to the fix worker before considering verification again, so the
	// loop is verify -> fix -> verify rather than a dead end.
	if !run.State.Terminal() && verifyStep != nil {
		updated, err := c.maybeDispatchVerifyFix(ctx, run, *workStep, *fixStep, *reviewStep, *verifyStep)
		if err != nil {
			return run, err
		}
		*fixStep = updated
		if err := refreshRun(); err != nil {
			return run, err
		}
		// A fix dispatched above is now running; observe it in this same call
		// so a single poll can carry the cycle as far as the facts allow,
		// exactly as steps 2-3 do for the review-driven fix.
		if !run.State.Terminal() && fixStep.State == domain.WorkflowStepRunning {
			observed, oerr := c.observeFixStep(ctx, run, *fixStep)
			if oerr != nil {
				return run, oerr
			}
			*fixStep = observed
			if err := refreshRun(); err != nil {
				return run, err
			}
		}
	}

	// 6. An approved review is not completion. Execute the structured local
	// verification target automatically and complete the run only from facts.
	if !run.State.Terminal() && verifyStep != nil && reviewStep.State == domain.WorkflowStepCompleted {
		updatedRun, updatedStep, err := c.maybeVerify(ctx, run, *workStep, *reviewStep, *verifyStep)
		if err != nil {
			return run, err
		}
		run, *verifyStep = updatedRun, updatedStep
	}

	return run, nil
}

// maybeDispatchFix resolves the review step's current review_run, verifies
// it genuinely landed changes_requested, re-derives the completed cycle
// number (the same review_run-count-based counter dispatchReviewStep uses),
// and only then builds the fix prompt and calls dispatchFixStep. The budget
// check here is defensive/idempotent (observeReviewStep already recorded
// next_action: "human_attention" and moved the run to needs_attention the
// moment budget was exhausted) — re-checking here just guarantees this
// function can never itself become the path that dispatches a (budget+1)th
// fix cycle, however it is reached.
func (c *Coordinator) maybeDispatchFix(ctx stdctx.Context, run domain.WorkflowRun, workStep, fixStep, reviewStep domain.WorkflowStep) (domain.WorkflowStep, error) {
	if c.reviewRuns == nil || reviewStep.ReviewRunID == nil {
		return fixStep, nil
	}
	reviewRun, ok, err := c.reviewRuns.GetReviewRun(ctx, *reviewStep.ReviewRunID)
	if err != nil {
		return fixStep, err
	}
	// EffectiveVerdict, not Verdict: an ADOPTED late changes_requested is
	// authoritative and must dispatch its fix. Reading the storage column the
	// normal submission path uses is what let an adopted late verdict become
	// authoritative and then drive nothing at all.
	if !ok || reviewRun.EffectiveVerdict() != domain.VerdictChangesRequested {
		return fixStep, nil
	}

	// The CYCLE NUMBER is derived from the review runs this worker session has
	// had, and must stay that way: it is this cycle's NAME, not its cost. It
	// feeds the outbox idempotency key, so it has to be identical on every poll
	// that re-derives the same pending cycle — a number that counted dispatches
	// instead would change the instant the first one landed and let the next
	// poll dispatch the same cycle again under a new key.
	cycleCount := 0
	runs, err := c.reviewRuns.ListReviewRunsBySession(ctx, reviewRun.SessionID)
	if err != nil {
		return fixStep, err
	}
	for _, r := range runs {
		if r.Harness == reviewRun.Harness {
			cycleCount++
		}
	}
	if cycleCount == 0 {
		// No data yet: do not dispatch.
		return fixStep, nil
	}
	// The BUDGET is a different question with a different answer, counted the
	// one way AO counts it: fix cycles actually dispatched, folded out of the
	// append-only ledger (fix_budget.go). This guard and observeReviewStep's
	// must agree exactly, or one of them becomes the path that spends a cycle
	// the other already refused. An unreadable budget is never a licence to
	// mutate a worktree.
	budget := c.fixBudget(ctx, run)
	if !budget.Known || budget.Exhausted() {
		return fixStep, nil
	}

	artifact, err := c.planArtifactForRun(ctx, run)
	if err != nil {
		return fixStep, err
	}
	// One read of the findings, used for both the prompt and the durable
	// evidence about it. Reading twice would let the two disagree, which is
	// precisely the claim the evidence exists to make unfalsifiable.
	findings := reviewFindingsRef(reviewRun)
	prompt := BuildFixPrompt(FixPromptInput{
		Objective:          run.Objective,
		AcceptanceCriteria: artifact.AcceptanceCriteria,
		EffectiveSpec:      RenderEffectiveSpecification(c.effectiveTaskSpecification(ctx, run, artifact.AcceptanceCriteria)),
		ReviewRunID:        reviewRun.ID,
		// The reviewer's actual findings, from whichever column carries them.
		Findings:    findings.Body,
		CycleNumber: cycleCount,
	})

	// Checkpoint 8M §12/§27: the lifecycle decision itself is applied inside
	// dispatchFixFromPending, not here — maybeDispatchFix can be re-entered
	// on every GetRun poll for the same cycle (before its outbox entry ever
	// reaches "pending"), while dispatchFixFromPending is reached exactly
	// once per cycle (guarded by the outbox idempotency key). Computing/
	// persisting the decision here would create a duplicate checkpoint per
	// poll instead of exactly one per real dispatch.
	return c.dispatchFixStep(ctx, run, workStep, fixStep, reviewRun, cycleCount, prompt, findings)
}

// applyFixLifecycleDecision evaluates SessionLifecyclePolicy for the fix
// role, persists the decision for audit, and — only when it says COMPACT —
// prepends a compact SessionContextPack recap to the fix prompt so the
// reused session gets a fresh fact anchor instead of relying purely on
// accumulated conversation state across many cycles. Never changes which
// session receives the prompt.
func (c *Coordinator) applyFixLifecycleDecision(ctx stdctx.Context, run domain.WorkflowRun, reviewRun domain.ReviewRun, cycleCount int, prompt string) (string, bool) {
	health := domain.SessionHealthUnknown
	if c.sessionFacts != nil {
		rec, found, err := c.sessionFacts.GetSession(ctx, reviewRun.SessionID)
		if err == nil {
			health = sessionHealthFromFacts(rec, found)
		}
	}
	decision := DecideSessionLifecycle(SessionLifecycleRequest{
		Role: domain.WorkflowRoleFixWorker, CurrentSessionID: string(reviewRun.SessionID),
		SessionHealth: health, FixCycleCount: cycleCount, Policy: policyForRun(run),
	})
	decision.ToSessionID = string(reviewRun.SessionID)

	var pack *domain.SessionContextPack
	contextPackUsed := false
	if decision.Action == domain.LifecycleCompact {
		artifact, err := c.planArtifactForRun(ctx, run)
		if err == nil {
			facts := BuildTaskCheckpointSummary(TaskCheckpointSummaryInput{
				Detail: RunDetail{Run: run}, Artifact: &artifact,
			})
			facts.LatestReviewFindings = reviewRun.EffectiveBody()
			built := BuildSessionContextPack(domain.WorkflowRoleFixWorker, facts)
			pack = &built
			decision.ContextPackHash = built.ContentHash()
			// Checkpoint 8P-E.13C: the pack is persisted whole (it is the audit
			// record of what was handed over), but the block PREPENDED to the
			// prompt drops the three fields BuildFixPrompt already carries
			// verbatim — objective, acceptance criteria, and the reviewer's
			// findings. Sending them twice doubled the largest part of the
			// payload for no added context, which is exactly what pushed the
			// fix prompt past the transport's ceiling. Nothing is truncated:
			// every dropped field is present, in full, further down the same
			// message.
			contextPackUsed = true
			prompt = RenderContextPackForRoleExcluding(built, fixPromptDuplicateFields) + "\n\n" + prompt
		}
	}
	// Deliberately NOT associated with fixStep.ID: several read paths
	// (dispatchReviewStep's cycle-N+1 branch, verify_scope_policy.go) rely
	// on "the latest checkpoint for this exact step" meaning a specific
	// durable phase (fix_dispatched) with FingerprintBefore/After set. A
	// second checkpoint for the same step at the same simulated clock tick
	// would tie on CreatedAt and could shadow it — the run-level record is
	// enough for lifecycle audit without risking that collision.
	_ = c.persistSessionLifecycleDecision(ctx, run, nil, decision, pack)
	return prompt, contextPackUsed
}

// planArtifactForRun re-reads the plan step's already-persisted
// PlanArtifact (acceptance criteria) for a run, falling back to a
// deterministic rebuild (BuildPlanArtifact is pure, so this is byte-
// identical to what StartRun originally produced) if the plan step's
// artifact is somehow still empty. Mirrors plan.go's promptForRun.
func (c *Coordinator) planArtifactForRun(ctx stdctx.Context, run domain.WorkflowRun) (PlanArtifact, error) {
	steps, err := c.store.ListWorkflowSteps(ctx, run.ID)
	if err != nil {
		return PlanArtifact{}, err
	}
	for _, s := range steps {
		if s.Kind != domain.WorkflowStepPlan || s.ArtifactJSON == "" || s.ArtifactJSON == "{}" {
			continue
		}
		if artifact, err := UnmarshalPlanArtifact(s.ArtifactJSON); err == nil {
			return artifact, nil
		}
	}
	return BuildPlanArtifact(run.ProjectID, run.Objective, run.PolicyVersion), nil
}
