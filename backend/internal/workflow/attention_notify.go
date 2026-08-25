package workflow

import (
	stdctx "context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// notifyAttentionStop raises the durable notification for a run that stopped on
// something only a person can resolve.
//
// Why it hangs off recordAttentionStop rather than off each state write: the
// state write is scattered across two dozen call sites (and several of them
// deliberately tolerate a failed CAS, because the run is already parked), while
// recordAttentionStop is the one place every stop is NAMED. A stop AO cannot
// name is a stop it cannot ask anyone about, so naming and notifying belong at
// the same point.
//
// Only human-owned reasons notify. The registry already distinguishes the three
// kinds of stop, and the two AO owns — the ones it retries by itself, and the
// ones it could not classify at all — are not decisions anyone can be asked to
// make. Emailing about a branch queue that clears itself in forty seconds is
// how a notification channel gets muted.
//
// Best-effort, like every other observer here: a notification store that is
// slow or broken must never change what the run's own state says happened.
func (c *Coordinator) notifyAttentionStop(ctx stdctx.Context, run domain.WorkflowRun, reason, detail string) {
	if c.notifications == nil || reason == "" {
		return
	}
	if disp, ok := attentionDispositions[reason]; !ok || disp.HumanAction == "" {
		return
	}
	// Checkpoint 8P-E.24: a parent's mirror of a child's stop is the SAME
	// real-world incident the child already reported, seen from one level up.
	// Notifying about both is two messages for one problem, and — because the
	// parent re-derives its mirror on every reconcile pass while the child stays
	// stopped — it is the half most likely to keep arriving. The person is told
	// once, about the run that actually stopped, and the parent's own ledger
	// still records the mirror for anyone reading the objective.
	if reason == ReasonChildNeedsAttention || reason == ReasonChildFailed {
		return
	}
	c.notifyRunEvent(ctx, run, attentionTypeFor(run), reason, detail)
}

// notifyRunFailed raises the durable notification for a run observed in the
// terminal WorkflowRunFailed state: it ended, and it did not do the work.
func (c *Coordinator) notifyRunFailed(ctx stdctx.Context, run domain.WorkflowRun, detail string) {
	if c.notifications == nil {
		return
	}
	c.notifyRunEvent(ctx, run, failureTypeFor(run), "", detail)
}

// notifyRunEvent is the shared write.
//
// The dedupe key is "<run id>@<type>". Both halves are load-bearing:
//
//   - The run id scopes the event to one run, so two runs stopping for the same
//     reason are two notifications, as they should be.
//   - The type keeps a stop and a failure of the SAME run distinct, so a run
//     that a person resolves and that later fails still reports the failure.
//
// What it deliberately does NOT include is the reason or the occurrence: a run
// re-derives its stop on every poll, and recordAttentionStop is reached again
// on each one. Keying per occurrence would mean one notification per poll;
// keying per run and type means one, ever — including across a reconcile, a
// retry, and a daemon restart, because the store's event-dedupe index is
// permanent rather than scoped to rows that are still open.
func (c *Coordinator) notifyRunEvent(ctx stdctx.Context, run domain.WorkflowRun, typ domain.NotificationType, reason, detail string) {
	err := c.notifications.Notify(ctx, ports.NotificationIntent{
		Type:              typ,
		ProjectID:         domain.ProjectID(run.ProjectID),
		CreatedAt:         c.clock(),
		WorkflowRunID:     run.ID,
		DedupeKey:         run.ID + "@" + string(typ),
		WorkflowObjective: run.Objective,
		AttentionReason:   reason,
		Detail:            detail,
	})
	if err != nil && c.log != nil {
		c.log.Warn("workflow: attention notification failed", "run", run.ID, "type", typ, "err", err)
	}
}

// attentionTypeFor and failureTypeFor split the vocabulary the way the Board
// does: a run created for a planned task of a master plan is a Task, and
// anything else is a Workflow. The user is told about the thing they are
// looking at, not about the row that happens to carry the state.
func attentionTypeFor(run domain.WorkflowRun) domain.NotificationType {
	if run.PlannedTaskID != nil {
		return domain.NotificationTaskNeedsAttention
	}
	return domain.NotificationWorkflowNeedsAttention
}

func failureTypeFor(run domain.WorkflowRun) domain.NotificationType {
	if run.PlannedTaskID != nil {
		return domain.NotificationTaskFailed
	}
	return domain.NotificationWorkflowFailed
}
