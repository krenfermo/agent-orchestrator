package daemon

import (
	"context"
	"log/slog"
	"os"
	"time"

	plannercommand "github.com/aoagents/agent-orchestrator/backend/internal/adapters/planner/command"
	workspacerouter "github.com/aoagents/agent-orchestrator/backend/internal/adapters/workspace/router"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/workflow"
	sessionmanager "github.com/aoagents/agent-orchestrator/backend/internal/session_manager"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// workflowAgentSwitcher adapts *session_manager.Manager.SwitchAgent — the
// existing durable, generation-fenced Claude<->Codex switching saga — to
// workflow.AgentSwitcher (Checkpoint 8H). Workflow's own package
// deliberately does not import session_manager, so this tiny field-for-field
// translation lives here in composition-root wiring instead.
type workflowAgentSwitcher struct {
	mgr *sessionmanager.Manager
}

func (w workflowAgentSwitcher) SwitchAgent(ctx context.Context, id domain.SessionID, cfg workflowcore.AgentSwitchRequest) (domain.AgentSwitch, error) {
	return w.mgr.SwitchAgent(ctx, id, sessionmanager.SwitchAgentConfig{
		TargetHarness:  cfg.TargetHarness,
		Note:           cfg.Note,
		IdempotencyKey: cfg.IdempotencyKey,
	})
}

// startWorkflows wires the workflow durable foundation (Checkpoint 8A) plus
// Checkpoint 8B's work-step Codex dispatch/observation and Checkpoint 8C's
// review-step dispatch/observation: a coordinator over the store, the raw
// session manager (the canonical Spawn write path — see
// session_manager.Manager.Spawn), the routed workspace adapter (the
// ObserveWorkspace read path for live git facts), and reviewerLauncher (8C's
// real-Claude-reviewer launch path — see workflow_reviewer_launcher.go),
// plus the thin API-facing service. It does not start any background
// goroutine — progress is derived at read time (GetRun) and at boot
// (Reconcile), never polled by a scheduler.
func startWorkflows(store *sqlite.Store, sessionMgr *sessionmanager.Manager, workspace *workspacerouter.Workspace, reviewerLauncher workflowcore.ReviewerLauncher, paneReader workflowcore.PaneReader, log *slog.Logger) (*workflowcore.Coordinator, *workflowsvc.Service) {
	plannerBinary := os.Getenv("AO_PLANNER_BIN")
	if plannerBinary == "" {
		plannerBinary = "claude"
	}
	plannerModel := os.Getenv("AO_PLANNER_MODEL")
	if plannerModel == "" {
		plannerModel = "sonnet"
	}
	coordinator := workflowcore.New(workflowcore.Deps{
		Store:                 store,
		Projects:              store,
		Sessions:              store,
		ReviewRuns:            store,
		Spawner:               sessionMgr,
		SessionFacts:          store,
		WorkspaceFacts:        workspace,
		ReviewerLauncher:      reviewerLauncher,
		MessageSender:         sessionMgr,
		Verifier:              workflowVerifyRunner{},
		Planner:               plannercommand.Planner{Binary: plannerBinary, Model: plannerModel, Timeout: 3 * time.Minute},
		PlannerContextBuilder: plannercommand.ContextBuilder{},
		Switcher:              workflowAgentSwitcher{mgr: sessionMgr},
		QuestionsStore:        store,
		PaneReader:            paneReader,
		Logger:                log,
	})
	return coordinator, workflowsvc.New(coordinator)
}
