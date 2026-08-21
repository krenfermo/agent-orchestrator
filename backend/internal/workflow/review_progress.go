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

// reviewerStallGrace is Checkpoint 8P-D.3's much tighter bound: how long a
// review run may sit "running" with its underlying reviewer session already
// idle/exited before that is treated as a stall worth acting on, instead of
// silently waiting out the full 30-minute reviewStalenessThreshold. Real
// evidence (Checkpoint 8P-D.2's fresh smoke run) showed a reviewer hitting a
// provider usage-limit mid-review: its own session went idle (the CLI's Stop
// hook fired) within seconds without ever calling `ao review submit`, but
// nothing distinguished that from "still genuinely working" for the next 30
// minutes. This grace only guards against the opposite race — checking
// activity state immediately after dispatch, before the reviewer's own first
// hook has even landed, would misread the WORKER's leftover idle state (from
// before the reviewer was launched into the same session) as an instant
// stall. Real reviewer hook latency observed was ~3s; this is deliberately
// generous relative to that, not to the 30-minute threshold it replaces.
const reviewerStallGrace = 20 * time.Second

// reviewCapacityRetryDurablePhase marks a WorkflowCheckpoint written only by
// handleReviewerCapacityStall: the review step is resting at "waiting" with
// no fix cycle involved (no workspace change happened), purely because a
// prior reviewer session stalled without a verdict. dispatchReviewStep's
// WorkflowStepWaiting case checks for this exact phase to distinguish a
// capacity retry from a real fix-cycle N+1 (which instead requires
// fixStep.State == waiting with a fresh fingerprint).
const reviewCapacityRetryDurablePhase = "review_capacity_retry"

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
		return c.stopReview(ctx, run, step, ReasonReviewStateAmbiguous,
			"ambiguous_review_state: review run referenced by this step no longer exists", "", domain.WorkflowErrorAmbiguousWorkerState)
	}

	switch reviewRun.Status {
	case domain.ReviewRunRunning:
		latestCP, hasCP, cperr := c.store.GetLatestWorkflowCheckpointByStep(ctx, step.ID)
		if cperr != nil {
			return step, cperr
		}
		elapsed := time.Duration(0)
		if hasCP {
			elapsed = now.Sub(latestCP.CreatedAt)
		}
		if hasCP && elapsed > reviewerStallGrace {
			if stalled := c.reviewerSessionStalled(ctx, reviewRun, latestCP.CreatedAt); stalled {
				return c.handleReviewerCapacityStall(ctx, run, step, reviewRun, now)
			}
		}
		if hasCP && elapsed > reviewStalenessThreshold {
			return c.stopReview(ctx, run, step, ReasonReviewStateAmbiguous,
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
						if r.Harness == reviewRun.Harness {
							cycleCount++
						}
					}
				}
			}
			policy := policyForRun(run)
			// Checkpoint 8P-E.12: this comparison used to be `>=`, while
			// maybeDispatchFix's own budget guard (cascade.go) used `>`. With
			// MaxFixCycles=3 the two disagreed at exactly cycle 3: this
			// function moved the run to needs_attention with
			// next_action "human_attention", and the very same cascade call
			// then dispatched fix cycle 3 anyway — which is legal, since the
			// policy allows three fix cycles. The run therefore sat in
			// needs_attention while AO kept working, which is precisely the
			// "human_attention does not mean the user is needed" report this
			// checkpoint exists to end. MaxFixCycles is documented as how many
			// fix cycles the loop MAY run, so `>` is the correct reading and
			// the two guards now agree exactly.
			if cycleCount > 0 && cycleCount > policy.MaxFixCycles {
				// Checkpoint 8P-E.13: this is the exact stop that stranded
				// wf-3220567f. recordReviewOutcome tried to carry
				// fix_budget_exhausted on the review step's latest attempt row
				// — but review dispatch never creates workflow_attempts rows,
				// so GetLatestWorkflowAttempt found nothing and the class was
				// silently dropped. The only durable trace left was a
				// "review_observed" checkpoint, which names what AO was doing,
				// not why it stopped. stopReview records the canonical reason
				// as its own checkpoint instead, so the stop is explainable
				// from durable state alone regardless of whether an attempt row
				// happens to exist.
				return c.stopReview(ctx, run, step, ReasonFixBudgetExhausted,
					fmt.Sprintf("fix_budget_exhausted: the reviewer still requests changes after %d review cycles (max_fix_cycles=%d)", cycleCount, policy.MaxFixCycles),
					string(reviewRun.Verdict), domain.WorkflowErrorFixBudgetExhausted)
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
			return c.stopReview(ctx, run, step, ReasonReviewStateAmbiguous,
				"ambiguous_review_state: review run completed with no valid verdict", string(reviewRun.Verdict), domain.WorkflowErrorAmbiguousWorkerState)
		}

	case domain.ReviewRunFailed, domain.ReviewRunCancelled:
		step, err := c.recordReviewOutcome(ctx, run, step, domain.WorkflowStepFailed, domain.WorkflowRunNeedsAttention,
			fmt.Sprintf("review run ended as %s", reviewRun.Status), string(reviewRun.Verdict), domain.WorkflowErrorReviewerLaunchFailed)
		if err != nil {
			return step, err
		}
		c.recordAttentionStop(ctx, run, &step.ID, ReasonReviewerLaunchFailed, fmt.Sprintf("review run ended as %s", reviewRun.Status))
		return step, nil

	default:
		// Unknown/unspecified status: make no change rather than guess.
		return step, nil
	}
}

// stopReview rests the review step at "waiting", moves the run to
// needs_attention, and — the part that did not exist before Checkpoint
// 8P-E.13 — records the canonical reason for the stop as its own durable
// checkpoint.
//
// The review step is deliberately left non-terminal. Whatever the human
// decides (raise the budget, fix it themselves, cancel), the loop must still
// be resumable; a "failed" review step would have zero outgoing transitions
// and would make that impossible forever.
func (c *Coordinator) stopReview(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	step domain.WorkflowStep,
	reason, detail, verdict string,
	errClass domain.WorkflowErrorClass,
) (domain.WorkflowStep, error) {
	updated, err := c.recordReviewOutcome(ctx, run, step, domain.WorkflowStepWaiting, domain.WorkflowRunNeedsAttention,
		detail, verdict, errClass)
	if err != nil {
		return updated, err
	}
	c.recordAttentionStop(ctx, run, &updated.ID, reason, detail)
	return updated, nil
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

// reviewerSessionStalled reports whether reviewRun's underlying AO session
// has gone idle or exited while reviewRun itself is still durably "running"
// with no verdict — the real, observable signature of a reviewer whose turn
// ended (the CLI's own Stop/exit hook fired) without ever reaching
// `ao review submit` (Checkpoint 8P-D.2's real evidence: a Codex reviewer
// that hit a provider usage limit mid-review). A nil sessionFacts port, a
// lookup miss, or any error is treated as "cannot tell" (false), never as
// evidence of a stall — this only ever shortens the wait, it must never
// invent one.
//
// dispatchedAt is this review cycle's own dispatch checkpoint time. Review
// reuses the WORKER's existing session rather than spawning a new one, so
// right after dispatch the session's Activity can still read as Idle purely
// as the worker's own leftover state from before the reviewer ever started —
// requiring Activity.LastActivityAt to be strictly after dispatchedAt is what
// tells a fresh, reviewer-caused idle signal apart from that stale leftover
// (a fixed grace period alone cannot: a reviewer harness with no hook support
// at all would never update LastActivityAt regardless of how long the grace
// window is, and correctly never triggers this path). IsTerminated is
// unconditional — a session cannot un-terminate, so no freshness check
// applies there.
func (c *Coordinator) reviewerSessionStalled(ctx stdctx.Context, reviewRun domain.ReviewRun, dispatchedAt time.Time) bool {
	if c.sessionFacts == nil || reviewRun.SessionID == "" {
		return false
	}
	sess, found, err := c.sessionFacts.GetSession(ctx, reviewRun.SessionID)
	if err != nil || !found {
		return false
	}
	if sess.IsTerminated {
		return true
	}
	if !sess.Activity.LastActivityAt.After(dispatchedAt) {
		return false
	}
	switch sess.Activity.State {
	case domain.ActivityIdle, domain.ActivityExited:
		return true
	default:
		return false
	}
}

// handleReviewerCapacityStall is Checkpoint 8P-D.3's response to a reviewer
// session that stalled without ever producing a verdict: it is classified as
// a provider capacity signal (the real evidence available — an idle turn
// with no submit — is exactly what a mid-review usage-limit/rate-limit hit
// looks like from AO's side, and treating it any other way would either
// silently wait out reviewStalenessThreshold or misfire needs_attention for
// a transient, self-recovering condition), recorded as a durable, scoped
// AgentHealthEvent so it never counts against a different user's or
// profile's connection, and the stalled review_run is closed out (never left
// "running" forever, never left to imply an approved verdict from the
// model's own prose — see Codex's own transcript in the 8P-D.2 evidence,
// which said "approved" in text while never calling submit).
//
// This deliberately does NOT hand-pick a fallback reviewer itself: resting
// the step at "waiting" and letting dispatchReviewStep's normal cascade
// re-enter (cascade.go's advanceReviewFixCycle step 4, same call) means
// reviewerHarnessForStep/routeReviewerDispatch/RouteExecution — the same
// machinery that already implements the frozen UserExecutionPolicy's
// FallbackBehavior/ReviewIndependence rules for every other dispatch —
// naturally either selects an eligible independent fallback (seeing this
// call's own just-recorded cooldown) or returns Waiting=true, which
// dispatchReviewStep already turns into markRunWaitingForCapacity's durable
// reviewer_capacity wake. No new fallback-selection logic to duplicate or
// drift from the dispatch-time path.
func (c *Coordinator) handleReviewerCapacityStall(ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep, reviewRun domain.ReviewRun, now time.Time) (domain.WorkflowStep, error) {
	harness := domain.AgentHarness(reviewRun.Harness)
	_, owner, profileID, _ := c.resolveRuntimeEnv(ctx, run.ID, harness)
	scope := healthScope{userID: owner, profileID: profileID}
	classification := ProviderFailureClassification{Class: domain.WorkflowErrorCapacityExhausted, Certainty: CertaintyInferred, Eligible: true}
	c.recordAgentHealthFailure(ctx, harness, scope, classification, now)

	if c.reviewRuns != nil {
		_, _ = c.reviewRuns.CancelRunningReviewRunsBySessionAndHarness(ctx, reviewRun.SessionID, reviewRun.Harness,
			"reviewer_capacity: session went idle with no verdict after dispatch — treated as provider capacity exhaustion, never fabricated as an approved verdict from pane text")
	}

	if domain.ValidWorkflowStepTransition(step.State, domain.WorkflowStepWaiting) {
		if _, err := c.store.UpdateWorkflowStepState(ctx, step.ID, step.State, domain.WorkflowStepWaiting, now); err != nil {
			return step, err
		}
		step.State = domain.WorkflowStepWaiting
	}
	if domain.ValidWorkflowRunTransition(run.State, domain.WorkflowRunWaiting) {
		if _, err := c.store.UpdateWorkflowRunState(ctx, run.ID, run.State, domain.WorkflowRunWaiting, now); err != nil {
			return step, err
		}
	}

	// reviewStep.ReviewRunID intentionally keeps pointing at the just-
	// cancelled review_run: reviewerHarnessForStep already skips cancelled
	// runs when picking the next cycle's harness, and
	// recordReviewDispatchSuccess unconditionally overwrites this field the
	// moment the retry actually dispatches — nothing here needs to clear it,
	// and maybeDispatchFix's own Verdict!=ChangesRequested guard already
	// makes it a safe no-op against the cancelled (empty-verdict) run in the
	// meantime.
	stepID := step.ID
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:                "wfc-" + c.newID(),
		WorkflowRunID:     run.ID,
		WorkflowStepID:    &stepID,
		ProjectID:         run.ProjectID,
		FingerprintBefore: reviewRun.TargetSHA,
		NextAction:        "retry_review: reviewer session stalled without a verdict, retrying per execution policy",
		DurablePhase:      reviewCapacityRetryDurablePhase,
		PayloadVersion:    "v1",
		RetryState:        "{}",
		CreatedAt:         now,
	}); err != nil {
		return step, err
	}
	return step, nil
}
