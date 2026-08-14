package daemon

import (
	"log/slog"

	workflowsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/workflow"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// startWorkflows wires the Checkpoint 8A workflow durable foundation: a
// coordinator over the store plus the thin API-facing service. It does not
// start any background goroutine — 8A has no scheduler, execution, or
// auto-advance; ReconcileWorkflows (called separately from Run, once at
// boot) is the only lifecycle hook this checkpoint needs.
func startWorkflows(store *sqlite.Store, log *slog.Logger) (*workflowcore.Coordinator, *workflowsvc.Service) {
	coordinator := workflowcore.New(workflowcore.Deps{
		Store:      store,
		Projects:   store,
		Sessions:   store,
		ReviewRuns: store,
		Logger:     log,
	})
	return coordinator, workflowsvc.New(coordinator)
}
