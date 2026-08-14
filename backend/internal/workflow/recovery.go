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
// calls Review Engine or spawns a reviewer — but for the work step kind,
// Checkpoint 8B does let it drive the SAME idempotent dispatch/observation
// machinery StartRun and GetRun use, because that is precisely what proves a
// crash mid-dispatch (recovery boundaries A/B/ambiguous) heals correctly:
// re-entering dispatchWorkStep after a restart is safe by construction (see
// dispatch.go), never calls Spawner.Spawn a second time for an
// already-attempted step, and never assumes success it cannot prove.
//
// Rule: every non-terminal run is loaded with its steps. For any step kind
// OTHER than work, 8A's original generic rule still applies unmodified: a
// step found running is moved to waiting (no independent fact source exists
// for those kinds in this checkpoint, since nothing dispatches them yet), and
// if the parent run was running/waiting it moves to needs_attention.
//
// For a work step found ready or running, Checkpoint 8B replaces that blind
// rule with: (1) dispatchWorkStep, which is a no-op if the step already has a
// session, otherwise resumes exactly where the outbox left off (pending ->
// call Spawn once; dispatched -> adopt via natural key or surface ambiguity;
// never re-dispatch a step that never got past pending==the run itself never
// started, since a work step stays "pending" — not ready/running — until
// StartRun unblocks it); then (2), if the step is now running with a session,
// observeWorkStep evaluates its live progress. A still-alive, still-active
// worker session is NOT necessarily "interrupted" by the daemon restart
// (session_manager.reconcileLive already adopts a still-alive tmux process as
// a no-op), so a work step may correctly stay running after Reconcile — only
// real evidence (terminated session, idle-with-no-change, etc.) moves it.
//
// This remains idempotent: a step already waiting/completed/failed is left
// alone by its respective rule, and a run already needs_attention is left
// alone, so running Reconcile twice in a row is a no-op the second time.
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

			if step.Kind == domain.WorkflowStepWork {
				if step.State != domain.WorkflowStepReady && step.State != domain.WorkflowStepRunning {
					continue
				}
				prompt := promptForRun(run, steps)
				updated, err := c.dispatchWorkStep(ctx, run, step, prompt)
				if err != nil {
					return err
				}
				if refreshed, ok, rerr := c.store.GetWorkflowRun(ctx, run.ID); rerr == nil && ok {
					run = refreshed
				}
				if updated.State == domain.WorkflowStepRunning {
					if _, err := c.observeWorkStep(ctx, run, updated); err != nil {
						return err
					}
				}
				// dispatchWorkStep/observeWorkStep persist their own state/run
				// transitions (or leave them unchanged if the worker is still
				// legitimately active); the work step never participates in
				// the generic "interrupted -> needs_attention" fallback below.
				continue
			}

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
		// Re-read the run: a work step's observeWorkStep call above may
		// already have moved it (e.g. to needs_attention or waiting).
		current, ok, err := c.store.GetWorkflowRun(ctx, run.ID)
		if err != nil {
			return err
		}
		if !ok || (current.State != domain.WorkflowRunRunning && current.State != domain.WorkflowRunWaiting) {
			continue
		}
		if _, err := c.store.UpdateWorkflowRunState(ctx, run.ID, current.State, domain.WorkflowRunNeedsAttention, now); err != nil {
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
