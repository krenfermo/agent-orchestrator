package workflow

import (
	stdctx "context"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// priorTaskContextBlock is Checkpoint 8M's task-boundary handoff (checkpoint
// brief §13, "el mayor ahorro probable"): a compact SessionContextPack per
// already-completed dependency task, never that task's session/transcript.
// A dependency task is only ever listed in another task's Dependencies once
// its own WorkflowTask.State reaches Completed (reconcileMasterTasks), so
// its child run is terminal by construction — reusing GetRun here is safe:
// every cascade GetRun would otherwise run is itself guarded to be a no-op
// against already-terminal run state, the same idempotency invariant
// Reconcile/GetRun already rely on everywhere else in this package. A
// single unreadable dependency is skipped, never fails the whole dispatch —
// the new task still gets its own objective/acceptance criteria regardless.
func (c *Coordinator) priorTaskContextBlock(ctx stdctx.Context, allTasks []domain.WorkflowTask, task domain.WorkflowTask) (string, *domain.SessionContextPack) {
	if len(task.Dependencies) == 0 {
		return "", nil
	}
	byID := make(map[string]domain.WorkflowTask, len(allTasks))
	for _, t := range allTasks {
		byID[t.ID] = t
	}
	var blocks []string
	var firstPack *domain.SessionContextPack
	for _, depID := range task.Dependencies {
		dep, ok := byID[depID]
		if !ok || dep.ExecutionRunID == nil {
			continue
		}
		depDetail, err := c.GetRun(ctx, *dep.ExecutionRunID)
		if err != nil {
			continue
		}
		summary := BuildTaskCheckpointSummary(TaskCheckpointSummaryInput{Detail: depDetail})
		pack := BuildSessionContextPack(domain.WorkflowRoleWorker, summary)
		if firstPack == nil {
			firstPack = &pack
		}
		blocks = append(blocks, "Completed dependency task \""+dep.Title+"\":\n"+RenderContextPackForRole(pack))
	}
	if len(blocks) == 0 {
		return "", nil
	}
	return "Prior completed dependency tasks — facts only, NOT their session history or conversation:\n\n" + strings.Join(blocks, "\n\n"), firstPack
}
