package workflow

import (
	stdctx "context"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// reviewStalenessThreshold bounds how long a review run may sit at status
// "running" before observeReviewStep gives up waiting and surfaces
// ambiguity instead of silently assuming success or failure. A real Claude
// Code review pass can legitimately take a while (reading a diff, running
// read-only checks), so this is deliberately generous — this is the ONLY
// place Checkpoint 8C uses a time-based judgment, and it is framed as "no
// longer confident, ask for attention," never as "assume success."
const reviewStalenessThreshold = 30 * time.Minute

// observeReviewStep is the single fact-based review-step evaluation function,
// used both by GetRun (opportunistic observation) and by boot Reconcile,
// mirroring observeWorkStep's split of pure decision vs. store-touching
// orchestration. Unlike observeWorkStep, this needs no ObserveWorkspace/git
// shell-out: review completion is a pure DB fact (review_run.status/verdict),
// written by the real `ao review submit` CLI call hitting the real HTTP
// endpoint while the reviewer is running.
func (c *Coordinator) observeReviewStep(ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep) (domain.WorkflowStep, error) {
	if step.Kind != domain.WorkflowStepReview || step.State != domain.WorkflowStepRunning {
		return step, nil
	}
	if c.reviewRuns == nil || step.ReviewRunID == nil {
		return step, nil
	}
	now := c.clock()

	reviewRun, found, err := c.reviewRuns.GetReviewRun(ctx, *step.ReviewRunID)
	if err != nil {
		return step, err
	}
	if !found {
		// Defensive: the id we hold does not resolve. Surface ambiguity
		// rather than guessing.
		return c.recordReviewOutcome(ctx, run, step, domain.WorkflowStepWaiting, domain.WorkflowRunNeedsAttention,
			"ambiguous_review_state: review run referenced by this step no longer exists", "", domain.WorkflowErrorAmbiguousWorkerState)
	}

	switch reviewRun.Status {
	case domain.ReviewRunRunning:
		latestCP, hasCP, cperr := c.store.GetLatestWorkflowCheckpointByStep(ctx, step.ID)
		if cperr != nil {
			return step, cperr
		}
		if hasCP && now.Sub(latestCP.CreatedAt) > reviewStalenessThreshold {
			return c.recordReviewOutcome(ctx, run, step, domain.WorkflowStepWaiting, domain.WorkflowRunNeedsAttention,
				"ambiguous_review_state: review has been running longer than expected with no verdict", "", domain.WorkflowErrorAmbiguousWorkerState)
		}
		// Still genuinely working (or too fresh to judge): no change.
		return step, nil

	case domain.ReviewRunComplete, domain.ReviewRunDelivered:
		switch reviewRun.Verdict {
		case domain.VerdictApproved:
			return c.recordReviewOutcome(ctx, run, step, domain.WorkflowStepCompleted, domain.WorkflowRunWaiting, "verify", string(reviewRun.Verdict), "")
		case domain.VerdictChangesRequested:
			// 8C->8D revision: a changes_requested verdict used to send the
			// review step straight to "completed" (a terminal state with zero
			// outgoing transitions), which made a second review cycle for the
			// same step impossible. Checkpoint 8D instead rests the review
			// step at "waiting" (non-terminal): running -> waiting -> running
			// (cycle N+1) -> waiting -> ... until either an approved verdict
			// lands (-> completed, final) or the fix budget is exhausted
			// (stays waiting, non-terminal, resumable later).
			//
			// Budget enforcement lives here, at the moment a changes_requested
			// verdict is first observed for this cycle: the count of
			// review_runs already created for this worker session IS the
			// cycle number this verdict just concluded (reused, not a new
			// counter column). If it has reached policy.MaxFixCycles, the
			// loop must hard-stop rather than dispatch another fix — next_action
			// becomes the literal "human_attention" and the run moves to
			// needs_attention, but the review (and, separately, the fix) step
			// itself is not "failed": the work it already did is not wrong,
			// the loop simply ran out of budget.
			cycleCount := 0
			if c.reviewRuns != nil {
				if runs, err := c.reviewRuns.ListReviewRunsBySession(ctx, reviewRun.SessionID); err == nil {
					for _, r := range runs {
						if r.Harness == reviewHarness {
							cycleCount++
						}
					}
				}
			}
			policy := policyForRun(run)
			if cycleCount > 0 && cycleCount >= policy.MaxFixCycles {
				return c.recordReviewOutcome(ctx, run, step, domain.WorkflowStepWaiting, domain.WorkflowRunNeedsAttention,
					"human_attention", string(reviewRun.Verdict), domain.WorkflowErrorFixBudgetExhausted)
			}
			// Within budget: rest at waiting. next_action "fix" is
			// informational only here — the actual fix dispatch happens from
			// the coordinator's cascade orchestration (workflow.go's
			// advanceReviewFixCycle), which GetRun/Reconcile/ContinueRun all
			// invoke within the same call, per Checkpoint 8D's automatic-
			// progression design.
			return c.recordReviewOutcome(ctx, run, step, domain.WorkflowStepWaiting, domain.WorkflowRunWaiting, "fix", string(reviewRun.Verdict), "")
		default:
			// Complete/delivered with an empty/invalid verdict should not
			// happen given submitOne's own validation, but defend anyway
			// rather than silently treating it as approved.
			return c.recordReviewOutcome(ctx, run, step, domain.WorkflowStepWaiting, domain.WorkflowRunNeedsAttention,
				"ambiguous_review_state: review run completed with no valid verdict", string(reviewRun.Verdict), domain.WorkflowErrorAmbiguousWorkerState)
		}

	case domain.ReviewRunFailed, domain.ReviewRunCancelled:
		return c.recordReviewOutcome(ctx, run, step, domain.WorkflowStepFailed, domain.WorkflowRunNeedsAttention,
			fmt.Sprintf("review run ended as %s", reviewRun.Status), string(reviewRun.Verdict), domain.WorkflowErrorReviewerLaunchFailed)

	default:
		// Unknown/unspecified status: make no change rather than guess.
		return step, nil
	}
}

func (c *Coordinator) recordReviewOutcome(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	step domain.WorkflowStep,
	nextStep domain.WorkflowStepState,
	nextRun domain.WorkflowRunState,
	nextAction string,
	verdict string,
	errClass domain.WorkflowErrorClass,
) (domain.WorkflowStep, error) {
	now := c.clock()

	if domain.ValidWorkflowStepTransition(step.State, nextStep) {
		if _, err := c.store.UpdateWorkflowStepState(ctx, step.ID, step.State, nextStep, now); err != nil {
			return step, err
		}
		step.State = nextStep
	} else if c.log != nil {
		c.log.Info("workflow: skipping invalid review-step observation transition (benign race)",
			"step", step.ID, "from", step.State, "to", nextStep)
	}

	if domain.ValidWorkflowRunTransition(run.State, nextRun) {
		if _, err := c.store.UpdateWorkflowRunState(ctx, run.ID, run.State, nextRun, now); err != nil {
			return step, err
		}
	} else if c.log != nil && run.State != nextRun {
		c.log.Info("workflow: skipping invalid run transition from review-step observation (benign race)",
			"run", run.ID, "from", run.State, "to", nextRun)
	}

	stepID := step.ID
	var reviewRunIDPtr *string
	if step.ReviewRunID != nil {
		rid := *step.ReviewRunID
		reviewRunIDPtr = &rid
	}
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  run.ID,
		WorkflowStepID: &stepID,
		ProjectID:      run.ProjectID,
		ReviewRunID:    reviewRunIDPtr,
		ReviewVerdict:  verdict,
		NextAction:     nextAction,
		DurablePhase:   "review_observed",
		PayloadVersion: "v1",
		RetryState:     "{}",
		CreatedAt:      now,
	}); err != nil {
		return step, err
	}

	if errClass != "" || nextStep.Terminal() {
		latestAttempt, hasAttempt, aerr := c.store.GetLatestWorkflowAttempt(ctx, step.ID)
		if aerr == nil && hasAttempt {
			finishedAt := time.Time{}
			if latestAttempt.FinishedAt != nil {
				finishedAt = *latestAttempt.FinishedAt
			}
			outcome := latestAttempt.Outcome
			ec := latestAttempt.ErrorClass
			switch nextStep {
			case domain.WorkflowStepCompleted:
				outcome = domain.WorkflowAttemptSucceeded
				finishedAt = now
			case domain.WorkflowStepFailed:
				outcome = domain.WorkflowAttemptFailed
				finishedAt = now
			}
			if errClass != "" {
				ec = errClass
			}
			_ = c.store.UpdateWorkflowAttemptOutcome(ctx, latestAttempt.ID, finishedAt, outcome, ec)
		}
	}

	return step, nil
}
