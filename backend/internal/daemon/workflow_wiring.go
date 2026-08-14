package daemon

import (
	"log/slog"

	workspacerouter "github.com/aoagents/agent-orchestrator/backend/internal/adapters/workspace/router"
	workflowsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/workflow"
	sessionmanager "github.com/aoagents/agent-orchestrator/backend/internal/session_manager"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// startWorkflows wires the workflow durable foundation (Checkpoint 8A) plus
// Checkpoint 8B's work-step Codex dispatch/observation: a coordinator over
// the store, the raw session manager (the canonical Spawn write path — see
// session_manager.Manager.Spawn), and the routed workspace adapter (the
// ObserveWorkspace read path for live git facts), plus the thin API-facing
// service. It does not start any background goroutine — progress is derived
// at read time (GetRun) and at boot (Reconcile), never polled by a scheduler.
func startWorkflows(store *sqlite.Store, sessionMgr *sessionmanager.Manager, workspace *workspacerouter.Workspace, log *slog.Logger) (*workflowcore.Coordinator, *workflowsvc.Service) {
	coordinator := workflowcore.New(workflowcore.Deps{
		Store:          store,
		Projects:       store,
		Sessions:       store,
		ReviewRuns:     store,
		Spawner:        sessionMgr,
		SessionFacts:   store,
		WorkspaceFacts: workspace,
		Logger:         log,
	})
	return coordinator, workflowsvc.New(coordinator)
}
