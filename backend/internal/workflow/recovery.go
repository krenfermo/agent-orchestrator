package workflow

import (
	stdctx "context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// Sessions resolves a referenced session for the recovery integrity check.
// Optional: a nil Sessions dependency simply skips the check.
type Sessions interface {
	GetSession(ctx stdctx.Context, id domain.SessionID) (domain.SessionRecord, bool, error)
}

// ReviewRuns resolves a referenced review run for the recovery integrity
// check. Optional: a nil ReviewRuns dependency simply skips the check.
type ReviewRuns interface {
	GetReviewRun(ctx stdctx.Context, id string) (domain.ReviewRun, bool, error)
}

// Reconcile is workflow's own durable self-state repair, run once at daemon
// boot after storage opens and before the daemon starts serving. It never
// calls Session Manager, Review Engine, or spawns/sends anything — 8A has no
// scheduler, so any step still "running" or "waiting" for a workflow run
// represents work interrupted by the restart (no process survives a daemon
// restart in this checkpoint).
//
// Rule: every non-terminal run is loaded with its steps. Any step found
// running is moved to waiting (running -> waiting is a valid transition).
// If the parent run was running or waiting, the run itself moves to
// needs_attention. This is idempotent: a step already waiting is left alone,
// and a run already needs_attention is left alone, so running Reconcile
// twice in a row is a no-op the second time.
func (c *Coordinator) Reconcile(ctx stdctx.Context) error {
	runs, err := c.store.ListNonTerminalWorkflowRuns(ctx)
	if err != nil {
		return err
	}
	now := c.clock()
	for _, run := range runs {
		steps, err := c.store.ListWorkflowSteps(ctx, run.ID)
		if err != nil {
			return err
		}

		interrupted := false
		for _, step := range steps {
			c.checkStepIntegrity(ctx, step)
			if step.State != domain.WorkflowStepRunning {
				continue
			}
			interrupted = true
			if _, err := c.store.UpdateWorkflowStepState(ctx, step.ID, domain.WorkflowStepRunning, domain.WorkflowStepWaiting, now); err != nil {
				return err
			}
		}

		if !interrupted {
			continue
		}
		if run.State != domain.WorkflowRunRunning && run.State != domain.WorkflowRunWaiting {
			continue
		}
		if _, err := c.store.UpdateWorkflowRunState(ctx, run.ID, run.State, domain.WorkflowRunNeedsAttention, now); err != nil {
			return err
		}
	}
	return nil
}

// checkStepIntegrity best-effort verifies that a step's optional session/
// review-run references still resolve. A dangling reference is only logged —
// it must never fail daemon startup.
func (c *Coordinator) checkStepIntegrity(ctx stdctx.Context, step domain.WorkflowStep) {
	if c.log == nil {
		return
	}
	if step.SessionID != nil && c.sessions != nil {
		if _, ok, err := c.sessions.GetSession(ctx, domain.SessionID(*step.SessionID)); err != nil {
			c.log.Warn("workflow recovery: session lookup failed", "step", step.ID, "session", *step.SessionID, "err", err)
		} else if !ok {
			c.log.Warn("workflow recovery: step references missing session", "step", step.ID, "session", *step.SessionID)
		}
	}
	if step.ReviewRunID != nil && c.reviewRuns != nil {
		if _, ok, err := c.reviewRuns.GetReviewRun(ctx, *step.ReviewRunID); err != nil {
			c.log.Warn("workflow recovery: review run lookup failed", "step", step.ID, "reviewRun", *step.ReviewRunID, "err", err)
		} else if !ok {
			c.log.Warn("workflow recovery: step references missing review run", "step", step.ID, "reviewRun", *step.ReviewRunID)
		}
	}
}
