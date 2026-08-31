package workflow

import (
	stdctx "context"
	"encoding/json"
	"errors"
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

// errReviewAuthorityLost is a guarded write that did not land because the review
// run it was being applied for is no longer the step's authority. It is a
// control signal, not a failure: the caller abandons its decision and whoever
// owns the step now decides instead.
var errReviewAuthorityLost = errors.New("workflow: this review run is no longer the step's authority")

// reviewObservedPhase is the durable phase of a review OBSERVATION: AO read a
// review run and recorded what it said. It is deliberately not a stop and not a
// lifecycle transition of its own — the transition, when there is one, is the
// step and run rows this function writes. See checkpoint_authority.go for why
// that distinction is load-bearing rather than pedantic.
const reviewObservedPhase = "review_observed"

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
		return c.stopReviewAmbiguous(ctx, run, step,
			"ambiguous_review_state: review run referenced by this step no longer exists", "")
	}

	if reviewRun.Status == domain.ReviewRunRunning {
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
			return c.stopReviewAmbiguous(ctx, run, step,
				"ambiguous_review_state: review has been running longer than expected with no verdict", "")
		}
		// Still genuinely working (or too fresh to judge): no change.
		return step, nil
	}

	updated, handled, err := c.applyTerminalReviewRun(ctx, run, step, reviewRun, "")
	if err != nil {
		return step, err
	}
	if handled {
		return updated, nil
	}
	// Unknown/unspecified status: make no change rather than guess.
	return step, nil
}

// applyTerminalReviewRun applies whatever a review run has durably CONCLUDED to
// its step: a verdict, or an ending without one. handled is false when the run
// has concluded nothing yet, in which case the caller must leave the step alone.
//
// It is a function rather than three switch arms because two callers need it.
// The obvious one is observation. The other is the reviewer-stall path, which
// has to be able to say "my cancellation lost — this review actually finished"
// and then apply that finish by exactly the route an on-time one takes. Two
// copies of this decision would be two different answers to "what does this
// verdict mean", and the stall path would be the copy nobody maintained.
func (c *Coordinator) applyTerminalReviewRun(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	step domain.WorkflowStep,
	reviewRun domain.ReviewRun,
	authorityRunID string,
) (domain.WorkflowStep, bool, error) {
	// The verdict decides first, and it is read through EffectiveVerdict so that
	// a reviewer's answer drives the same cascade whichever column holds it.
	// Before this, an ADOPTED late `changes_requested` reached here on a run
	// whose `verdict` column is empty by design, fell through to the status
	// switch, and was applied as "the review run ended as cancelled" — failing
	// the step instead of dispatching the fix its findings were asking for.
	switch reviewRun.EffectiveVerdict() {
	case domain.VerdictApproved:
		updated, err := c.recordReviewOutcome(ctx, run, step,
			domain.WorkflowStepCompleted, domain.WorkflowRunWaiting, "verify", string(reviewRun.EffectiveVerdict()), "", authorityRunID)
		return updated, true, err

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
		// verdict is first observed for this cycle. What it counts is FIX
		// CYCLES ACTUALLY DISPATCHED, folded out of the append-only ledger
		// by fix_budget.go — never review_run rows, which is what made
		// wf-724a1e97 report "6 review cycles" against a budget of 3 and,
		// far worse, made every post-recovery fresh review land already
		// over budget. See fix_budget.go for the full accounting.
		budget := c.fixBudget(ctx, run)
		// Fail closed only in the direction that costs nothing: an
		// unreadable budget does not manufacture a stop here (it refuses
		// the DISPATCH instead, in maybeDispatchFix), so a store hiccup
		// can never park a run on an exhaustion it cannot prove.
		if budget.Exhausted() {
			// Checkpoint 8P-E.13: stopReview records the canonical reason
			// as its own checkpoint, so the stop is explainable from
			// durable state alone regardless of whether an attempt row
			// happens to exist (review dispatch never creates one).
			updated, err := c.stopReview(ctx, run, step, ReasonFixBudgetExhausted,
				fmt.Sprintf("fix_budget_exhausted: the reviewer still requests changes after %d fix cycles (max_fix_cycles=%d)", budget.Spent, budget.Budget),
				string(reviewRun.EffectiveVerdict()), domain.WorkflowErrorFixBudgetExhausted, authorityRunID)
			return updated, true, err
		}
		// Within budget: rest at waiting. next_action "fix" is
		// informational only here — the actual fix dispatch happens from
		// the coordinator's cascade orchestration (workflow.go's
		// advanceReviewFixCycle), which GetRun/Reconcile/ContinueRun all
		// invoke within the same call, per Checkpoint 8D's automatic-
		// progression design.
		updated, err := c.recordReviewOutcome(ctx, run, step,
			domain.WorkflowStepWaiting, domain.WorkflowRunWaiting, "fix", string(reviewRun.EffectiveVerdict()), "", authorityRunID)
		return updated, true, err

	}

	// No verdict of any kind. What the run's own status says it became is then
	// the whole answer.
	switch reviewRun.Status {
	case domain.ReviewRunComplete, domain.ReviewRunDelivered:
		// Concluded with an empty/invalid verdict should not happen given
		// submitOne's own validation, but defend anyway rather than silently
		// treating it as approved.
		updated, err := c.stopReviewAmbiguous(ctx, run, step,
			"ambiguous_review_state: review run completed with no valid verdict", string(reviewRun.Verdict))
		return updated, true, err

	case domain.ReviewRunFailed, domain.ReviewRunCancelled:
		updated, err := c.recordReviewOutcome(ctx, run, step,
			domain.WorkflowStepFailed, domain.WorkflowRunNeedsAttention,
			fmt.Sprintf("review run ended as %s", reviewRun.Status), string(reviewRun.Verdict),
			domain.WorkflowErrorReviewerLaunchFailed, authorityRunID)
		if err != nil {
			return updated, true, err
		}
		c.recordAttentionStop(ctx, run, &updated.ID, ReasonReviewerLaunchFailed,
			fmt.Sprintf("review run ended as %s", reviewRun.Status))
		return updated, true, nil

	default:
		return step, false, nil
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
// stopReviewAmbiguous is the ONLY way review observation reaches
// ambiguous_worker_state. Like its fix-step counterpart it goes through the
// evidence gate first: a review AO cannot read the state of is still a state AO
// holds a dozen durable facts about, and the stop must carry them.
func (c *Coordinator) stopReviewAmbiguous(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	step domain.WorkflowStep,
	detail, verdict string,
) (domain.WorkflowStep, error) {
	// A review step has no worktree of its own to observe, but it does have a
	// reviewer session — so the one reading AO can take here (is that runtime
	// still alive?) is taken and written down before the raise.
	//
	// Resolved through DurableSessionForStep, NOT off step.SessionID: only work
	// dispatch ever writes that column, so a review step's own id is empty and
	// reading it probed nothing, recorded nothing, and left the stop with a
	// liveness field permanently unavailable for a session that plainly exists.
	// The identity lives on this step's own checkpoints.
	// The reason is fixed: this helper exists for exactly one stop.
	const reason = ReasonReviewStateAmbiguous
	raised, err := c.raiseAmbiguousWorkerState(ctx, run, step, reason, detail,
		c.observedWorkerFactsFor(ctx, c.DurableSessionForStep(ctx, run.ID, step), nil))
	if err != nil {
		return step, err
	}
	if err := assertAmbiguousEvidence(raised.ErrorClass(), raised); err != nil {
		return step, err
	}
	return c.stopReview(ctx, run, step, reason, detail, verdict, raised.ErrorClass(), "")
}

func (c *Coordinator) stopReview(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	step domain.WorkflowStep,
	reason, detail, verdict string,
	errClass domain.WorkflowErrorClass,
	authorityRunID string,
) (domain.WorkflowStep, error) {
	updated, err := c.recordReviewOutcome(ctx, run, step, domain.WorkflowStepWaiting, domain.WorkflowRunNeedsAttention,
		detail, verdict, errClass, authorityRunID)
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
	// authorityRunID, when set, makes the step transition valid ONLY while that
	// review run is still the step's authority pointer. Empty keeps the
	// unguarded transition every pre-existing caller relies on: observation has
	// already read the run THROUGH the pointer in the same pass. Late-verdict
	// adoption sets it, because its writes span a window in which a replacement
	// can take the step.
	authorityRunID string,
) (domain.WorkflowStep, error) {
	now := c.clock()
	movedSomething := false

	if domain.ValidWorkflowStepTransition(step.State, nextStep) {
		var err error
		if authorityRunID != "" {
			var moved bool
			moved, err = c.store.UpdateWorkflowStepStateIfReviewRun(
				ctx, step.ID, step.State, nextStep, authorityRunID, now)
			if err == nil && !moved {
				// The pointer moved, or the step left the state this decision
				// was made against. This verdict no longer speaks for this step,
				// so nothing further may be applied on its behalf.
				return step, errReviewAuthorityLost
			}
		} else {
			_, err = c.store.UpdateWorkflowStepState(ctx, step.ID, step.State, nextStep, now)
		}
		if err != nil {
			return step, err
		}
		step.State = nextStep
		movedSomething = true
	} else if c.log != nil {
		c.log.Info("workflow: skipping invalid review-step observation transition (benign race)",
			"step", step.ID, "from", step.State, "to", nextStep)
	}

	if domain.ValidWorkflowRunTransition(run.State, nextRun) {
		if _, err := c.store.UpdateWorkflowRunState(ctx, run.ID, run.State, nextRun, now); err != nil {
			return step, err
		}
		movedSomething = true
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
	// An observation that changed nothing and says nothing new writes nothing.
	//
	// wf-c4c84f52 is why. Its review step had already FAILED when its reviewer's
	// approved verdict arrived, so every pass re-applied that verdict, found
	// both transitions invalid (failed -> completed, needs_attention -> waiting),
	// logged the benign race — and wrote the checkpoint anyway. 301 identical
	// rows later, the run's own stop was buried under three hours of AO
	// re-reading one verdict, and every reader that took "the newest checkpoint"
	// for "what happened" lost the reason the run was parked.
	//
	// The ledger is still append-only and no row is ever rewritten: what is
	// suppressed is only a row that would carry no information. The bar is
	// deliberately narrow — nothing moved AND the newest observation of this
	// step is already this exact one — so a real re-observation (a different
	// verdict, a different review run, a transition that lands) is always
	// recorded. The livelock this papers over is a separate defect and stays
	// visible in the logs above; what it must stop doing is destroying the
	// run's stop reason.
	//
	// Only the ROW is suppressed. Everything else this function does still runs,
	// including the attempt-outcome update below, so a suppressed duplicate is
	// exactly a no-op and never a skipped side effect.
	redundant := !movedSomething && c.reviewObservationIsRedundant(ctx, stepID, reviewRunIDPtr, verdict, nextAction)
	if !redundant {
		if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
			ID:             "wfc-" + c.newID(),
			WorkflowRunID:  run.ID,
			WorkflowStepID: &stepID,
			ProjectID:      run.ProjectID,
			ReviewRunID:    reviewRunIDPtr,
			ReviewVerdict:  verdict,
			NextAction:     nextAction,
			DurablePhase:   reviewObservedPhase,
			PayloadVersion: "v1",
			RetryState:     "{}",
			CreatedAt:      now,
		}); err != nil {
			return step, err
		}
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
	if c.reviewRuns == nil {
		return step, nil
	}

	// THE CANCELLATION, and then the only thing that decides what follows: what
	// this review run actually BECAME.
	//
	// A reviewer AO has judged stalled may be submitting its verdict in the same
	// instant — that is the whole shape of this race, and the submit side is
	// already built for it. CancelRunningReviewRunsBySessionAndHarness is
	// CAS-guarded in SQL on `status='running' AND verdict=''`, so a verdict that
	// landed first leaves it matching nothing.
	//
	// This used to discard both the row count and the error and park the step
	// regardless. That produced the one state from which nothing recovers: a
	// review run durably COMPLETE with an approval, and its workflow parked at
	// `waiting` under review_capacity_retry — invisible to observation (which
	// only looks at running steps), and read as intact by authority
	// reconciliation (a run with a verdict still speaks). The approval was never
	// applied and the run never moved again.
	//
	// The row count alone would not be proof either: the cancel is keyed by
	// (session, harness), not by this run's id, so it can report rows for a
	// sibling run. The durable proof is re-reading THIS run.
	// The EXTERNAL reviewer first, through the same restart-safe protocol a
	// losing replacement uses. Marking the row cancelled while its process keeps
	// running is the orphan this protocol exists to prevent — and it is exactly
	// what this path did: a stalled reviewer's pane outlived every retry, and
	// the replacement was launched beside it.
	//
	// The cancellation is durable-intent-first, so a crash anywhere in it is
	// finished by the next pass rather than lost.
	if identity, known := c.reviewerIdentityFor(ctx, run, step, reviewRun.ID); known {
		if xerr := c.cancelReviewerExternally(ctx, run, step, reviewRun.ID, identity,
			"the reviewer stalled without producing a verdict"); xerr != nil {
			// Leave the row alone: a review_run called cancelled over a reviewer
			// AO could not terminate is the lie this ordering removes. The next
			// pass retries the termination.
			if c.log != nil {
				c.log.Warn("workflow: could not terminate a stalled reviewer; leaving its run open",
					"run", run.ID, "step", step.ID, "reviewRun", reviewRun.ID, "err", xerr)
			}
			return step, nil
		}
	}
	if _, cerr := c.reviewRuns.CancelRunningReviewRunsBySessionAndHarness(ctx, reviewRun.SessionID, reviewRun.Harness,
		"reviewer_capacity: session went idle with no verdict after dispatch — treated as provider capacity exhaustion, never fabricated as an approved verdict from pane text"); cerr != nil {
		// AO cannot prove the cancellation won, so it may not act as if it did.
		// The step is left exactly as it was and the next observation pass tries
		// again — a stall that is still true in three seconds is still true,
		// while a step parked over a verdict AO never applied is permanent.
		if c.log != nil {
			c.log.Warn("workflow: could not close out a stalled review run; leaving the step untouched",
				"run", run.ID, "step", step.ID, "reviewRun", reviewRun.ID, "err", cerr)
		}
		return step, nil
	}

	fresh, found, ferr := c.reviewRuns.GetReviewRun(ctx, reviewRun.ID)
	if ferr != nil {
		return step, ferr
	}
	if !found {
		return step, nil
	}
	if fresh.Status != domain.ReviewRunCancelled {
		// The cancellation did not win this run. Whatever it became instead is
		// authoritative, and it is applied by exactly the route an on-time
		// conclusion takes — including an approval, which completes the step
		// here and now rather than being stranded behind a retry nobody needs.
		//
		// `handled` false means it concluded nothing after all (still running):
		// no verdict to apply, and no proof the cancellation won, so nothing is
		// parked and nothing is retried.
		updated, handled, aerr := c.applyTerminalReviewRun(ctx, run, step, fresh, "")
		if aerr != nil {
			return step, aerr
		}
		if handled {
			if c.log != nil {
				c.log.Info("workflow: a stalled reviewer had in fact concluded; its verdict wins over the cancellation",
					"run", run.ID, "step", step.ID, "reviewRun", fresh.ID,
					"status", fresh.Status, "verdict", fresh.Verdict)
			}
			return updated, nil
		}
		return step, nil
	}

	// Only now, with the cancellation durably proven to have won, is this a
	// capacity stall: record the provider health failure and park for the retry.
	harness := domain.AgentHarness(reviewRun.Harness)
	_, owner, profileID, _ := c.resolveRuntimeEnv(ctx, run.ID, harness)
	scope := healthScope{userID: owner, profileID: profileID}
	classification := ProviderFailureClassification{Class: domain.WorkflowErrorCapacityExhausted, Certainty: CertaintyInferred, Eligible: true}
	c.recordAgentHealthFailure(ctx, harness, scope, classification, now)

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
	// The retry's own evidence. This used to be a sentence and a `{}` payload:
	// "retrying per execution policy", naming neither the run it had just closed
	// out nor the target that run was reviewing nor the provider it was using.
	// After a restart nothing could reconstruct what was being retried from it —
	// which is exactly what a reconciler needs (review_authority.go), and exactly
	// what wf-756988ae's ledger could not tell anyone. Additive: the same phase
	// and the same sentence, with the facts attached.
	stepID := step.ID
	runID := reviewRun.ID
	payload, _ := json.Marshal(reviewCapacityRetryRecord{
		ClosedRunID: reviewRun.ID,
		Status:      string(domain.ReviewRunCancelled),
		TargetSHA:   reviewRun.TargetSHA,
		Harness:     string(reviewRun.Harness),
		SessionID:   string(reviewRun.SessionID),
		Reason:      "reviewer session went idle with no verdict after dispatch",
	})
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:                "wfc-" + c.newID(),
		WorkflowRunID:     run.ID,
		WorkflowStepID:    &stepID,
		ProjectID:         run.ProjectID,
		ReviewRunID:       &runID,
		FingerprintBefore: reviewRun.TargetSHA,
		NextAction:        "retry_review: reviewer session stalled without a verdict, retrying per execution policy",
		DurablePhase:      reviewCapacityRetryDurablePhase,
		PayloadVersion:    "v1",
		RetryState:        string(payload),
		CreatedAt:         now,
	}); err != nil {
		return step, err
	}
	return step, nil
}

// reviewCapacityRetryRecord is what a capacity retry records about itself: which
// review run it closed out, what that run was reviewing, and with which
// provider. Enough, after a restart, to say what the retry was for.
type reviewCapacityRetryRecord struct {
	ClosedRunID string `json:"closedRunId"`
	Status      string `json:"status"`
	TargetSHA   string `json:"targetSha"`
	Harness     string `json:"harness"`
	SessionID   string `json:"sessionId"`
	Reason      string `json:"reason"`
}

// reviewObservationIsRedundant reports whether the newest checkpoint for this
// step is already this same observation.
//
// It compares only what an observation asserts: which review run, which
// verdict, and what AO said comes next. A read failure returns false — an
// observation AO cannot compare is one it records, because losing a real
// observation is worse than keeping a duplicate.
func (c *Coordinator) reviewObservationIsRedundant(
	ctx stdctx.Context, stepID string, reviewRunID *string, verdict, nextAction string,
) bool {
	latest, ok, err := c.store.GetLatestWorkflowCheckpointByStep(ctx, stepID)
	if err != nil || !ok {
		return false
	}
	if latest.DurablePhase != reviewObservedPhase {
		return false
	}
	if latest.ReviewVerdict != verdict || latest.NextAction != nextAction {
		return false
	}
	switch {
	case reviewRunID == nil && latest.ReviewRunID == nil:
		return true
	case reviewRunID == nil || latest.ReviewRunID == nil:
		return false
	default:
		return *reviewRunID == *latest.ReviewRunID
	}
}
