package workflow

import (
	stdctx "context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// verify_authority.go — exactly one verification of a target may decide.
//
// A verify attempt row is created from a read-then-write (`GetLatestWorkflow-
// Attempt`, then `CreateWorkflowAttempt`) and its id is a pure function of
// step + target + generation, so the row itself is idempotent. What the row was
// never able to express is OWNERSHIP: `finished_at IS NULL` means "in flight",
// and maybeVerify deliberately reads that as "resume it" so a daemon killed
// mid-verification re-runs the checks instead of hanging forever.
//
// "Resume it" and "someone is running it right now" are the same state. So a
// second caller entering the cascade while the first is still executing — a
// Continue landing on top of the Board's 2s GetRun poll, a master reconcile
// beside a child cascade — re-executes the same checks concurrently. Both then
// concluded the attempt with an unconditional UPDATE and both acted.
//
// That is what happened to wf-04e8309d. Two executions of the SAME plan against
// the SAME fingerprint bfbbb150, 0.3s apart: one short-circuited on a failing
// `go test` (recording 2 checks, since the loop returns on the first failure)
// and opened a fix cycle; the other ran all 9 checks, passed, completed the run
// and opened integration. The run ended terminal with a fix step running and an
// advance step that never ran — a state no single execution could produce.
//
// Two mechanisms, in this order:
//
//  1. an in-process claim, so one daemon does not execute the same attempt
//     twice concurrently and burn the work. It is an optimisation, not a
//     correctness boundary — it cannot span processes or restarts.
//  2. a durable compare-and-swap on the attempt's conclusion, which IS the
//     correctness boundary. Exactly one caller can move an attempt from
//     in-flight to concluded; everyone else loses and must become a no-op that
//     touches nothing — no fix dispatch, no step transition, no run-state
//     change, no integration.
//
// The CAS is deliberately not a timestamp comparison or a lease. Wall-clock
// order cannot arbitrate two writers, and a lease still needs a CAS to settle
// who held it at the end.

// verifyDecisionOutcome is what winning or losing the decision means to the
// caller, kept as a type so a lost decision cannot be mistaken for an error.
type verifyDecisionOutcome struct {
	// Won is true for the single execution allowed to act on its result.
	Won bool
	// Reason explains a loss, for the ledger and the log.
	Reason string
}

// claimVerifyExecution takes the in-process claim for one attempt.
//
// It returns a release function and whether the claim was taken. A caller that
// does not get the claim must not execute the checks: another goroutine in this
// process is already running them, and its CAS will settle the decision.
//
// This is per-process only. Correctness never rests on it — see the CAS below.
func (c *Coordinator) claimVerifyExecution(attemptID string) (release func(), claimed bool) {
	c.verifyClaimsMu.Lock()
	defer c.verifyClaimsMu.Unlock()
	if c.verifyClaims == nil {
		c.verifyClaims = map[string]struct{}{}
	}
	if _, busy := c.verifyClaims[attemptID]; busy {
		return func() {}, false
	}
	c.verifyClaims[attemptID] = struct{}{}
	return func() {
		c.verifyClaimsMu.Lock()
		delete(c.verifyClaims, attemptID)
		c.verifyClaimsMu.Unlock()
	}, true
}

// decideVerify is the single gate every verification outcome passes through
// before it is allowed to change anything.
//
// Winning concludes the attempt durably and entitles the caller to act. Losing
// means another execution of this same attempt already decided — its result is
// the run's answer, and this one is stale by definition, however different its
// findings are. A loser records why it stood down and returns without touching
// the run.
func (c *Coordinator) decideVerify(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	step domain.WorkflowStep,
	attempt domain.WorkflowAttempt,
	outcome domain.WorkflowAttemptOutcome,
	errorClass domain.WorkflowErrorClass,
	result VerifyResult,
) (verifyDecisionOutcome, error) {
	won, err := c.store.ClaimWorkflowAttemptOutcome(ctx, attempt.ID, c.clock(), outcome, errorClass)
	if err != nil {
		return verifyDecisionOutcome{}, err
	}
	if won {
		return verifyDecisionOutcome{Won: true}, nil
	}
	reason := fmt.Sprintf(
		"a concurrent verification of the same target (attempt %s) already decided this run; this execution's %s result is stale and has no effect",
		attempt.ID, verifyOutcomeLabel(outcome, result))
	// The superseded execution is recorded, not discarded: a person comparing
	// two contradictory verify runs needs to see that AO knew about both and
	// which one was allowed to count.
	c.recordSupersededVerify(ctx, run, step, attempt, result, reason)
	if c.log != nil {
		c.log.Info("workflow: a concurrent verification lost the decision and was superseded",
			"run", run.ID, "attempt", attempt.ID, "passed", result.Passed, "errorClass", result.ErrorClass)
	}
	return verifyDecisionOutcome{Reason: reason}, nil
}

func verifyOutcomeLabel(outcome domain.WorkflowAttemptOutcome, result VerifyResult) string {
	if result.Passed {
		return "passing"
	}
	if result.ErrorClass != "" {
		return string(result.ErrorClass)
	}
	return string(outcome)
}

// verifySupersededPhase records an execution that lost the decision. It is
// bookkeeping about a run, never a step of it, so it is excluded from every
// fold into derived state exactly as the reap record and the incident ledger
// are (see isBookkeepingPhase).
const verifySupersededPhase = "verify_result_superseded"

// recordSupersededVerify writes the losing execution's own result, so the
// ledger carries both answers and the reason only one counted. Best-effort: the
// decision has already been made correctly by the winner, and failing to
// annotate it must not turn a harmless loss into an error.
func (c *Coordinator) recordSupersededVerify(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	step domain.WorkflowStep,
	attempt domain.WorkflowAttempt,
	result VerifyResult,
	reason string,
) {
	payload, err := json.Marshal(result)
	if err != nil {
		return
	}
	stepID, attemptID := step.ID, attempt.ID
	_, _ = c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + c.newID(),
		WorkflowRunID:  run.ID,
		WorkflowStepID: &stepID,
		AttemptID:      &attemptID,
		ProjectID:      run.ProjectID,
		RetryState:     string(payload),
		NextAction:     reason,
		DurablePhase:   verifySupersededPhase,
		PayloadVersion: verifyResultVersion,
		CreatedAt:      c.clock(),
	})
}

// runHasActiveWork reports whether any step that can still change the workspace
// or the run's outcome is unfinished.
//
// It is the guard on the transition to terminal. A run that completes while a
// fix worker is running claims the work is done while an agent is still
// changing it, and leaves an advance step that will now never run — which is
// precisely the shape wf-04e8309d ended in.
func runHasActiveWork(steps []domain.WorkflowStep) (string, bool) {
	for _, s := range steps {
		switch s.Kind {
		case domain.WorkflowStepWork, domain.WorkflowStepFix, domain.WorkflowStepReview, domain.WorkflowStepVerify:
			if s.State == domain.WorkflowStepRunning || s.State == domain.WorkflowStepReady {
				return fmt.Sprintf("the %s step is %s", s.Kind, s.State), true
			}
		}
	}
	return "", false
}

// verifyClaimsState is embedded in the Coordinator (see workflow.go) to hold
// the per-process execution claims. It lives here so the whole mechanism is
// readable in one file.
type verifyClaimsState struct {
	verifyClaimsMu sync.Mutex
	verifyClaims   map[string]struct{}
}

// ---- test seams -------------------------------------------------------------
//
// These expose the two halves of the mechanism to the external test package, so
// the race can be exercised through the real code paths rather than re-derived.

// ClaimVerifyExecutionForTest exposes claimVerifyExecution.
func (c *Coordinator) ClaimVerifyExecutionForTest(attemptID string) (func(), bool) {
	return c.claimVerifyExecution(attemptID)
}

// FinishVerifyFailureForTest exposes finishVerifyFailure, the path a losing
// FAILED execution takes and must not have side effects on.
func (c *Coordinator) FinishVerifyFailureForTest(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	step domain.WorkflowStep,
	attempt domain.WorkflowAttempt,
	result VerifyResult,
	reason string,
) (domain.WorkflowRun, domain.WorkflowStep, error) {
	return c.finishVerifyFailure(ctx, run, step, attempt, result, reason)
}

// CompleteVerifiedRunForTest exposes completeVerifiedRun, so the terminal-state
// invariant can be asserted directly.
func (c *Coordinator) CompleteVerifiedRunForTest(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	step domain.WorkflowStep,
) (domain.WorkflowRun, domain.WorkflowStep, error) {
	return c.completeVerifiedRun(ctx, run, step)
}
