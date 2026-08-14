package workflow

import (
	stdctx "context"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// observeFixStep is Checkpoint 8D's fact-based fix-step evaluation function,
// mirroring observeWorkStep's shape and conservatism exactly: it never
// trusts agent transcript text, only session facts (IsTerminated/
// ActivityState) and a live workspace fingerprint comparison against the
// fingerprint recorded when this fix cycle was dispatched
// (workflow.WorkspaceFingerprint over ports.WorkspaceObservation). A fix
// step never reaches "completed" in this checkpoint (verify, the natural
// judge of "truly done," is out of scope) — it only ever resolves to
// "waiting" (a genuinely new fingerprint was observed: the cycle is
// considered delivered, ready for the next re-review) or "failed" (the
// worker session ended with no verifiable change at all).
func (c *Coordinator) observeFixStep(ctx stdctx.Context, run domain.WorkflowRun, step domain.WorkflowStep) (domain.WorkflowStep, error) {
	if step.Kind != domain.WorkflowStepFix || step.State != domain.WorkflowStepRunning {
		return step, nil
	}
	if c.sessionFacts == nil || c.workspaceFacts == nil {
		return step, nil
	}
	latestCP, hasCP, err := c.store.GetLatestWorkflowCheckpointByStep(ctx, step.ID)
	if err != nil {
		return step, err
	}
	if !hasCP || latestCP.SessionID == nil || *latestCP.SessionID == "" {
		// Defensive: a running fix step should always have a dispatch
		// checkpoint with a session id (recordFixDispatchSuccess always
		// writes one). If it somehow doesn't, there is nothing to observe.
		return step, nil
	}
	now := c.clock()
	if now.Sub(latestCP.CreatedAt) <= observationThrottle {
		// Too fresh to justify another live ObserveWorkspace shell-out; wait
		// for a later call. Not an error.
		return step, nil
	}
	fingerprintBefore := latestCP.FingerprintBefore

	sessionID := domain.SessionID(*latestCP.SessionID)
	sess, found, err := c.sessionFacts.GetSession(ctx, sessionID)
	if err != nil {
		return step, err
	}

	terminatedOrExited := !found || sess.IsTerminated || sess.Activity.State == domain.ActivityExited
	if terminatedOrExited {
		obs, ok := c.observeFixWorkspace(ctx, sess)
		if !ok {
			return step, nil
		}
		fp := WorkspaceFingerprint(obs)
		if fp != fingerprintBefore {
			return c.recordFixOutcome(ctx, run, step, domain.WorkflowStepWaiting, domain.WorkflowRunWaiting,
				fp, true, "fix delivered (worker session ended) — awaiting next review cycle", "")
		}
		return c.recordFixOutcome(ctx, run, step, domain.WorkflowStepFailed, domain.WorkflowRunNeedsAttention,
			"", false, "fix worker session terminated with no verifiable change (no dirty, staged, or untracked change, and fingerprint unchanged)",
			domain.WorkflowErrorWorkerTerminatedUnexpectedly)
	}

	switch sess.Activity.State {
	case domain.ActivityActive:
		return step, nil
	case domain.ActivityWaitingInput, domain.ActivityBlocked:
		return c.recordFixOutcome(ctx, run, step, domain.WorkflowStepWaiting, domain.WorkflowRunNeedsAttention,
			"", false, "fix worker awaiting input/blocked — needs human attention", "")
	case domain.ActivityIdle:
		obs, ok := c.observeFixWorkspace(ctx, sess)
		if !ok {
			return step, nil
		}
		fp := WorkspaceFingerprint(obs)
		if fp != fingerprintBefore {
			return c.recordFixOutcome(ctx, run, step, domain.WorkflowStepWaiting, domain.WorkflowRunWaiting,
				fp, true, "fix delivered — awaiting next review cycle", "")
		}
		// Conservative, mirrors evaluateWorkStepProgress's idle+no-evidence
		// rule exactly: "Codex went idle but did not actually change
		// anything new" must not silently trigger a new review.
		return c.recordFixOutcome(ctx, run, step, domain.WorkflowStepWaiting, domain.WorkflowRunNeedsAttention,
			"", false, "fix worker idle with no verifiable new change — needs human review", domain.WorkflowErrorAmbiguousWorkerState)
	default:
		return step, nil
	}
}

func (c *Coordinator) observeFixWorkspace(ctx stdctx.Context, sess domain.SessionRecord) (ports.WorkspaceObservation, bool) {
	if sess.Metadata.WorkspacePath == "" {
		return ports.WorkspaceObservation{}, false
	}
	obs, err := c.workspaceFacts.ObserveWorkspace(ctx, ports.WorkspaceInfo{
		Path:      sess.Metadata.WorkspacePath,
		Branch:    sess.Metadata.Branch,
		SessionID: sess.ID,
		ProjectID: sess.ProjectID,
	})
	if err != nil {
		return ports.WorkspaceObservation{}, false
	}
	return obs, true
}

// recordFixOutcome persists one fix-step observation's outcome: the step/run
// transitions (through the same ValidWorkflowStepTransition/
// ValidWorkflowRunTransition guards every other workflow write goes
// through), an append-only checkpoint carrying the newly observed
// fingerprint (when resolved), and, for a resolved or failed outcome, the
// current cycle's workflow_attempt outcome/error_class.
func (c *Coordinator) recordFixOutcome(
	ctx stdctx.Context,
	run domain.WorkflowRun,
	step domain.WorkflowStep,
	nextStep domain.WorkflowStepState,
	nextRun domain.WorkflowRunState,
	fingerprintAfter string,
	resolved bool,
	nextAction string,
	errClass domain.WorkflowErrorClass,
) (domain.WorkflowStep, error) {
	now := c.clock()

	if domain.ValidWorkflowStepTransition(step.State, nextStep) {
		if _, err := c.store.UpdateWorkflowStepState(ctx, step.ID, step.State, nextStep, now); err != nil {
			return step, err
		}
		step.State = nextStep
	} else if c.log != nil {
		c.log.Info("workflow: skipping invalid fix-step observation transition (benign race)",
			"step", step.ID, "from", step.State, "to", nextStep)
	}

	if domain.ValidWorkflowRunTransition(run.State, nextRun) {
		if _, err := c.store.UpdateWorkflowRunState(ctx, run.ID, run.State, nextRun, now); err != nil {
			return step, err
		}
	} else if c.log != nil && run.State != nextRun {
		c.log.Info("workflow: skipping invalid run transition from fix-step observation (benign race)",
			"run", run.ID, "from", run.State, "to", nextRun)
	}

	stepID := step.ID
	if _, err := c.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:               "wfc-" + c.newID(),
		WorkflowRunID:    run.ID,
		WorkflowStepID:   &stepID,
		ProjectID:        run.ProjectID,
		FingerprintAfter: fingerprintAfter,
		NextAction:       nextAction,
		DurablePhase:     "fix_observed_" + string(nextStep),
		PayloadVersion:   "v1",
		RetryState:       "{}",
		CreatedAt:        now,
	}); err != nil {
		return step, err
	}

	if resolved || errClass != "" || nextStep.Terminal() {
		latestAttempt, hasAttempt, aerr := c.store.GetLatestWorkflowAttempt(ctx, step.ID)
		if aerr == nil && hasAttempt {
			finishedAt := time.Time{}
			if latestAttempt.FinishedAt != nil {
				finishedAt = *latestAttempt.FinishedAt
			}
			outcome := latestAttempt.Outcome
			ec := latestAttempt.ErrorClass
			switch {
			case resolved:
				outcome = domain.WorkflowAttemptSucceeded
				finishedAt = now
			case nextStep == domain.WorkflowStepFailed:
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
