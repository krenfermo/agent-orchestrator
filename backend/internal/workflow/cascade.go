package workflow

import (
	stdctx "context"

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
	var workStep, reviewStep, fixStep *domain.WorkflowStep
	for i := range steps {
		switch steps[i].Kind {
		case domain.WorkflowStepWork:
			workStep = &steps[i]
		case domain.WorkflowStepReview:
			reviewStep = &steps[i]
		case domain.WorkflowStepFix:
			fixStep = &steps[i]
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
		updated, err := c.dispatchReviewStep(ctx, run, *workStep, *fixStep, *reviewStep)
		if err != nil {
			return run, err
		}
		*reviewStep = updated
		if err := refreshRun(); err != nil {
			return run, err
		}
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
	if !ok || reviewRun.Verdict != domain.VerdictChangesRequested {
		return fixStep, nil
	}

	cycleCount := 0
	runs, err := c.reviewRuns.ListReviewRunsBySession(ctx, reviewRun.SessionID)
	if err != nil {
		return fixStep, err
	}
	for _, r := range runs {
		if r.Harness == reviewHarness {
			cycleCount++
		}
	}
	policy := policyForRun(run)
	if cycleCount == 0 || cycleCount > policy.MaxFixCycles {
		// No data yet, or budget already exhausted (recorded elsewhere): do
		// not dispatch.
		return fixStep, nil
	}

	artifact, err := c.planArtifactForRun(ctx, run)
	if err != nil {
		return fixStep, err
	}
	prompt := BuildFixPrompt(FixPromptInput{
		Objective:          run.Objective,
		AcceptanceCriteria: artifact.AcceptanceCriteria,
		ReviewRunID:        reviewRun.ID,
		Findings:           reviewRun.Body,
		CycleNumber:        cycleCount,
	})
	return c.dispatchFixStep(ctx, run, workStep, fixStep, reviewRun, cycleCount, prompt)
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
