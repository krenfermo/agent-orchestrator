package workflow

import (
	stdctx "context"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// fix_authority.go — a fix cycle may only be delivered on authority that is
// still current.
//
// A fix prompt is the one thing AO sends that is MEANT to change the worktree.
// Everything downstream — the review that reads the result, the verification
// that certifies it — is arbitrated by generation and CAS, but the mutation
// itself was not: dispatchFixStep took the reviewRun it was handed and sent the
// prompt, whatever had happened to that review since.
//
// Two shapes of stale delivery follow from that, and both are real:
//
//   - a cycle derived from a review run the step no longer points at (an
//     authority rebind, a replacement, an adopted late verdict), which would
//     send a reviewer's findings into a tree a DIFFERENT reviewer now speaks
//     for;
//   - a cycle derived from a review that has since APPROVED, which would mutate
//     the very state an approval was just given for and invalidate it in the
//     same breath.
//
// So the authority is re-read at the last possible moment — inside
// deliverFixPrompt, immediately before the prompt is written — and a delivery
// whose authority has moved on is refused rather than sent. Refusing costs
// nothing: no durable state changes, the outbox entry stays where it is, and
// the next poll re-derives the cycle from whatever authority is current then.
//
// The one case where an approved review DOES authorize a fix is the
// verify-driven re-entry: verification failed repairably, AO recorded
// verify_fix_reentry, and that checkpoint is itself the authorization. It is
// answered by the first fix attempt that starts at or after it, which is the
// same rule maybeDispatchVerifyFix dispatches on — one fix per re-entry, and
// the re-review the changed tree then needs is verify.go's job, not this one's.

// fixAuthorityRefusal returns "" when this fix cycle may be delivered, and a
// precise reason when it may not. Its default is refusal: a store it cannot
// read is not a licence to mutate a worktree.
func (c *Coordinator) fixAuthorityRefusal(
	ctx stdctx.Context, run domain.WorkflowRun, fixStep domain.WorkflowStep, reviewRun domain.ReviewRun,
) string {
	if c.reviewRuns == nil {
		// No reviewer wiring at all: the review-driven loop does not exist in
		// this configuration, and there is no generation to be stale against.
		return ""
	}
	steps, err := c.store.ListWorkflowSteps(ctx, run.ID)
	if err != nil {
		return "AO could not read this run's steps to prove the fix cycle's review authority is current"
	}
	var reviewStep *domain.WorkflowStep
	for i := range steps {
		if steps[i].Kind == domain.WorkflowStepReview {
			reviewStep = &steps[i]
		}
	}
	if reviewStep == nil {
		return ""
	}
	if reviewStep.ReviewRunID == nil {
		return fmt.Sprintf("the review step no longer points at any review run, so review %s cannot authorize a fix cycle", reviewRun.ID)
	}
	if *reviewStep.ReviewRunID != reviewRun.ID {
		return fmt.Sprintf("this fix cycle was derived from review run %s, but the review step now speaks for %s", reviewRun.ID, *reviewStep.ReviewRunID)
	}
	// Re-read the verdict from storage rather than trusting the copy that
	// travelled here: the whole point is that it may have changed since.
	current, found, err := c.reviewRuns.GetReviewRun(ctx, reviewRun.ID)
	if err != nil || !found {
		return fmt.Sprintf("AO could not read review run %s to prove it still authorizes a fix cycle", reviewRun.ID)
	}
	switch current.EffectiveVerdict() {
	case domain.VerdictChangesRequested:
		return ""
	case domain.VerdictApproved:
		open, reason, err := c.unansweredVerifyFixReentry(ctx, run.ID, fixStep.ID)
		if err != nil {
			return "AO could not read this run's verify re-entry ledger to prove an approved review's fix cycle is authorized"
		}
		if open {
			return ""
		}
		return fmt.Sprintf("review run %s is approved and %s, so nothing authorizes a fix cycle that would change what it approved", reviewRun.ID, reason)
	default:
		return fmt.Sprintf("review run %s has no verdict that authorizes a fix cycle", reviewRun.ID)
	}
}

// unansweredVerifyFixReentry reports whether this run's newest
// verify_fix_reentry checkpoint is still waiting for its one fix cycle.
//
// "Answered" is a fix ATTEMPT that started at or after the re-entry — the same
// rule maybeDispatchVerifyFix uses to decide it has already dispatched, kept in
// one place so the dispatcher and the authority check can never disagree about
// which re-entries are still open.
func (c *Coordinator) unansweredVerifyFixReentry(ctx stdctx.Context, runID, fixStepID string) (bool, string, error) {
	cps, err := c.store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		return false, "", err
	}
	var newest domain.WorkflowCheckpoint
	found := false
	for _, cp := range cps {
		if cp.DurablePhase == ReasonVerifyFixReentry && (!found || cp.CreatedAt.After(newest.CreatedAt)) {
			newest, found = cp, true
		}
	}
	if !found {
		return false, "no verification has asked for a fix cycle", nil
	}
	attempts, err := c.store.ListWorkflowAttempts(ctx, fixStepID)
	if err != nil {
		return false, "", err
	}
	for _, a := range attempts {
		if !a.StartedAt.Before(newest.CreatedAt) {
			return false, "the verify re-entry it could have served has already had its fix cycle", nil
		}
	}
	return true, "", nil
}
