package workflow

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// NotificationSink is the workflow-to-notification-producer boundary. Optional
// everywhere: a nil sink means workflow runs simply raise no notifications,
// the same nil-safe-optional convention every other dependency here follows.
type NotificationSink interface {
	Notify(ctx context.Context, intent ports.NotificationIntent) error
}

// completeRun is the single place a run may enter WorkflowRunCompleted.
//
// Both callers used to CAS the state themselves, which meant "the run
// finished" had two homes and a notification could be wired to one and not the
// other. Routing them through here also puts the notification behind the CAS
// result rather than beside it: WorkflowRunCompleted is terminal and has no
// outgoing transitions (domain.ValidWorkflowRunTransition), so the update
// matches a row exactly once per run — a retry, a second observer, or a
// restart re-running the same convergence check all see rows == 0 and notify
// nothing.
func (c *Coordinator) completeRun(ctx context.Context, run domain.WorkflowRun, expected domain.WorkflowRunState) (bool, error) {
	completed, err := c.store.UpdateWorkflowRunState(ctx, run.ID, expected, domain.WorkflowRunCompleted, c.clock())
	if err != nil {
		return false, err
	}
	if !completed {
		return false, nil
	}
	// P1-C: a finished run returns its slots at the exact instant it finishes,
	// behind the same CAS the notification is behind. Waiting for the next
	// reconciliation pass would work too (reconcileCapacityForRun releases a
	// terminal run's claims), but a slot that stays occupied until the next
	// sweep is a slot the queue could have used now.
	c.releaseCapacityForRun(ctx, run.ID, "run completed")
	c.abandonProviderAttemptsForRun(ctx, run.ID, "the run completed")
	c.notifyRunCompleted(ctx, run)
	return true, nil
}

// notifyRunCompleted is best-effort by design, exactly like lifecycle's own
// emitNotification: a notification store that is slow or broken must never turn
// a workflow that genuinely finished into one that failed.
func (c *Coordinator) notifyRunCompleted(ctx context.Context, run domain.WorkflowRun) {
	if c.notifications == nil {
		return
	}
	err := c.notifications.Notify(ctx, ports.NotificationIntent{
		Type:      domain.NotificationWorkflowCompleted,
		ProjectID: domain.ProjectID(run.ProjectID),
		CreatedAt: c.clock(),
		// The run id IS the event: a run reaches the completed state at most
		// once in its life, so this key can never name two different finishes.
		WorkflowRunID:     run.ID,
		DedupeKey:         run.ID,
		WorkflowObjective: run.Objective,
	})
	if err != nil && c.log != nil {
		c.log.Warn("workflow: completion notification failed", "run", run.ID, "err", err)
	}
}
