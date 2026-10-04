package workflow

import (
	stdctx "context"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// verify_stalled_attempt.go — the ending a FAILED verification attempt owes when
// its own identity can never change again.
//
// THE INCIDENT (wf-0aadfcde, task 4 of wf-2f06ba48).
//
//	workflow_attempts  wfa-verify-…  local-verify  outcome=failed
//	                   model = the verification target key still in force
//	workflow_steps     verify  state=waiting        (non-terminal)
//	workflow_runs      wf-0aadfcde  state=waiting
//
// maybeVerify's answer to that state was one line:
//
//	if hasAttempt && latest.Outcome != "" {
//	    if latest.Outcome == domain.WorkflowAttemptSucceeded { … }
//	    return run, verifyStep, nil        // ← here
//	}
//
// No checkpoint, no transition, no error — a silent success. And it was not a
// transient one: verifyAttemptID is a PURE FUNCTION of (step, target key,
// recovery generation, fix generation), so a pass that re-derives the same four
// inputs finds the same finished row every time. Nothing in that branch could
// make any of the four move. The reconciler woke, re-entered, returned nil, and
// woke again — for as long as the daemon stayed up — while the Board rendered a
// non-terminal verify step as live work.
//
// THE RULE, and it is the same one verify_stuck_reentry pinned for the parking
// half: a verify step may rest in a non-terminal state only while some path can
// still move it. Applied to a FINISHED attempt, "move it" means exactly one
// thing — change the attempt identity — and only two mechanisms can:
//
//	a fix DELIVERY   advances the fix generation      (verifyFixDeliveries)
//	a verify RECOVERY advances the recovery generation (verify_recovery.go)
//
// The first is proven by an unanswered verify_fix_reentry that a fix cycle can
// still answer; the second is a person's Continue, which needs the run to be
// parked at needs_attention with a recoverable stop before it can happen at all.
// So when the first is absent, resting is not waiting for anything — it is the
// loop — and the honest move is to stop, name the attempt, and put the run where
// the recovery door actually is.
//
// What this file deliberately does NOT do:
//
//   - It never re-executes a finished attempt. The failure stands exactly as it
//     was recorded; nothing here re-runs a check, re-reads a verdict or rewrites
//     a result.
//   - It never opens a fix cycle, and never touches a budget. The fix budget is
//     spent by fix cycles, and a stop that quietly refunded one would hide the
//     failure it is reporting.
//   - It never fires while a fix cycle can still answer. The one legitimate
//     resting shape — an open, answerable verify_fix_reentry — stays bit-for-bit
//     as quiet as it always was.

// resolveFailedVerifyAttempt is the ending for the branch that used to return
// nil: this step's current attempt identity has already FAILED.
//
// It rests only while something can still change that identity, and otherwise
// converges — one durable transition, once.
func (c *Coordinator) resolveFailedVerifyAttempt(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	verifyStep domain.WorkflowStep,
	attempt domain.WorkflowAttempt,
) (domain.WorkflowRun, domain.WorkflowStep, error) {
	movable, blocker := c.verifyIdentityCanStillMove(ctx, run, verifyStep)
	if movable {
		// Legitimate rest, and not a silent one: the durable verify_fix_reentry
		// that authorizes it is the record of what this run is waiting for.
		return run, verifyStep, nil
	}
	return c.stopUnretryableVerifyAttempt(ctx, run, verifyStep, attempt, blocker)
}

// verifyIdentityCanStillMove answers the only question that decides whether
// resting on a finished, failed attempt is honest: can anything change the
// attempt identity this step would derive on the next pass?
//
// It reports the blocker when the answer is no, so the stop can say WHICH fact
// closed the last door rather than merely that it declined.
//
// Every read failure answers YES. Failing to read the ledger is never a reason
// to terminate a run's verification — the same direction every other proof in
// this package fails in, and the reason this can never turn a transient storage
// error into a needs_attention stop.
func (c *Coordinator) verifyIdentityCanStillMove(
	ctx stdctx.Context, run domain.WorkflowRun, verifyStep domain.WorkflowStep,
) (bool, string) {
	steps, err := c.store.ListWorkflowSteps(ctx, run.ID)
	if err != nil {
		return true, ""
	}
	var fixStep, reviewStep domain.WorkflowStep
	for _, s := range steps {
		switch s.Kind {
		case domain.WorkflowStepFix:
			fixStep = s
		case domain.WorkflowStepReview:
			reviewStep = s
		}
	}
	if fixStep.ID == "" {
		return false, "this run has no fix step, so no fix cycle can ever change what verification is asked"
	}
	// A fix cycle already in flight will advance the fix generation the moment
	// it delivers. Its own dispatch/observation path owns it from here, and
	// nothing about this verification is stuck while that is true.
	if fixStep.State == domain.WorkflowStepRunning {
		return true, ""
	}
	open, answered, err := c.unansweredVerifyFixReentry(ctx, run.ID, fixStep.ID)
	if err != nil {
		return true, ""
	}
	if !open {
		return false, answered
	}
	// The re-entry is open. It only counts as a path if the fix cycle it asks
	// for can actually be dispatched — the same two facts maybeDispatchVerifyFix
	// requires, read through the same helper, so "resting" and "dispatchable"
	// cannot drift apart.
	blocker, berr := c.verifyFixReentryBlocker(ctx, run, fixStep, reviewStep)
	if berr != nil {
		return true, ""
	}
	if blocker != "" {
		return false, blocker
	}
	return true, ""
}

// stopUnretryableVerifyAttempt converges a verification that has failed and can
// no longer be re-asked under its own identity.
//
// The step transition is the arbiter, and it comes FIRST: it is a compare-and-
// swap on the state this pass observed, so exactly one of several concurrent
// passes records the stop however many wake at once. A pass that loses simply
// stands down — the winner's transition is the decision.
func (c *Coordinator) stopUnretryableVerifyAttempt(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	verifyStep domain.WorkflowStep,
	attempt domain.WorkflowAttempt,
	blocker string,
) (domain.WorkflowRun, domain.WorkflowStep, error) {
	now := c.clock()
	// `pending` has no edge to `failed`; it reaches its terminal state through
	// `ready`, exactly as the attempt-creation path walks it.
	if verifyStep.State == domain.WorkflowStepPending {
		moved, err := c.store.UpdateWorkflowStepState(ctx, verifyStep.ID,
			domain.WorkflowStepPending, domain.WorkflowStepReady, now)
		if err != nil {
			return run, verifyStep, err
		}
		if !moved {
			return run, verifyStep, nil
		}
		verifyStep.State = domain.WorkflowStepReady
	}
	moved, err := c.store.UpdateWorkflowStepState(ctx, verifyStep.ID,
		verifyStep.State, domain.WorkflowStepFailed, now)
	if err != nil {
		return run, verifyStep, err
	}
	if !moved {
		// Another pass already concluded this step, or a recovery moved it out
		// from under this one. Either way this pass is not the owner of the
		// decision and must not record a second stop for it.
		return run, verifyStep, nil
	}
	verifyStep.State = domain.WorkflowStepFailed

	// A run already parked on a NAMED stop has converged; it is not looping, and
	// whatever stopped it said something more specific about why than this can.
	// The verify step still has to leave its non-terminal state — a step resting
	// there renders as live work over a verification that finished long ago —
	// but the run's reason is left exactly as the site that stopped it wrote it.
	if run.State == domain.WorkflowRunNeedsAttention {
		if _, _, known := c.stopReason(ctx, run); known {
			return run, verifyStep, nil
		}
	}

	if run.State == domain.WorkflowRunWaiting {
		if _, err := c.store.UpdateWorkflowRunState(ctx, run.ID,
			domain.WorkflowRunWaiting, domain.WorkflowRunRunning, now); err != nil {
			return run, verifyStep, err
		}
		run.State = domain.WorkflowRunRunning
	}
	if run.State == domain.WorkflowRunRunning {
		if _, err := c.store.UpdateWorkflowRunState(ctx, run.ID,
			domain.WorkflowRunRunning, domain.WorkflowRunNeedsAttention, now); err != nil {
			return run, verifyStep, err
		}
		run.State = domain.WorkflowRunNeedsAttention
	}

	detail := fmt.Sprintf(
		"verify attempt %s already failed (%s) for target %s, and nothing can ask that target again: %s",
		attempt.ID, orValue(string(attempt.ErrorClass), "no error class recorded"),
		shortFingerprint(attempt.Model), orValue(blocker, "no fix cycle and no recovery is open for it"))
	c.recordAttentionStop(ctx, run, &verifyStep.ID, ReasonVerifyAttemptUnretryable, detail)
	if c.log != nil {
		c.log.Info("workflow: a failed verification attempt has no retry path and was stopped for a person",
			"run", run.ID, "step", verifyStep.ID, "attempt", attempt.ID, "blocker", blocker)
	}
	return run, verifyStep, nil
}
