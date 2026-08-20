package daemon

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/google/uuid"

	plannercommand "github.com/aoagents/agent-orchestrator/backend/internal/adapters/planner/command"
	workspacerouter "github.com/aoagents/agent-orchestrator/backend/internal/adapters/workspace/router"
	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/providerruntime"
	workflowsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/workflow"
	sessionmanager "github.com/aoagents/agent-orchestrator/backend/internal/session_manager"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
	"github.com/aoagents/agent-orchestrator/backend/internal/workflow/wake"
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
func startWorkflows(cfg config.Config, store *sqlite.Store, sessionMgr *sessionmanager.Manager, workspace *workspacerouter.Workspace, reviewerLauncher workflowcore.ReviewerLauncher, paneReader workflowcore.PaneReader, decisionResolverLauncher workflowcore.DecisionResolverLauncher, log *slog.Logger) (*workflowcore.Coordinator, *workflowsvc.Service, *wake.Scheduler) {
	plannerBinary := os.Getenv("AO_PLANNER_BIN")
	if plannerBinary == "" {
		plannerBinary = "claude"
	}
	plannerModel := os.Getenv("AO_PLANNER_MODEL")
	if plannerModel == "" {
		plannerModel = "sonnet"
	}
	// Checkpoint 8N: the durable wake-up scheduler backing automatic
	// capacity-wait resumption (see backend/internal/workflow/wakepoller for
	// the daemon-level poller that actually claims and fires these). Real
	// clock/id source — deterministic fakes are only for tests.
	wakeScheduler := wake.New(store, nil, uuid.NewString, wake.Config{Policy: domain.DefaultWakePolicy()})
	// Checkpoint 8P-B.1: the single canonical owner/env resolver every
	// dispatch site (worker, reviewer, planner, decision resolver) below
	// calls through workflowcore.Deps.RuntimeIsolation -- see
	// providerruntime.Resolver's doc comment for the trusted-local
	// compatibility behavior.
	runtimeIsolation := &providerruntime.Resolver{
		Owners:       store,
		Profiles:     store,
		DataDir:      cfg.DataDir,
		TrustedLocal: cfg.TrustedLocalMode,
	}
	coordinator := workflowcore.New(workflowcore.Deps{
		Store:            store,
		Projects:         store,
		Sessions:         store,
		ReviewRuns:       store,
		Spawner:          sessionMgr,
		SessionFacts:     store,
		WorkspaceFacts:   workspace,
		ReviewerLauncher: reviewerLauncher,
		MessageSender:    sessionMgr,
		Verifier:         workflowVerifyRunner{},
		// Checkpoint 8P-E.10: Timeout is the floor every call gets; MaxTimeout
		// bounds how far the adapter's own size-proportional scaling
		// (scaledTimeout) may stretch it for a large MEDUSA-class objective +
		// repository context payload. Neither value is a blind global bump --
		// small objectives still finish (or time out) inside 3 minutes.
		Planner:                  plannercommand.Planner{Binary: plannerBinary, Model: plannerModel, Timeout: 3 * time.Minute, MaxTimeout: 12 * time.Minute},
		PlannerContextBuilder:    plannercommand.ContextBuilder{},
		Switcher:                 workflowAgentSwitcher{mgr: sessionMgr},
		QuestionsStore:           store,
		PaneReader:               paneReader,
		DecisionResolverLauncher: decisionResolverLauncher,
		WakeScheduler:            wakeScheduler,
		Logger:                   log,
		RuntimeIsolation:         runtimeIsolation,
		// Checkpoint 8P-C: routing now walks the workflow owner's own
		// UserExecutionPolicy over their own ProviderProfiles instead of a
		// fixed Claude<->Codex table. TrustedLocal mirrors the same
		// desktop-compatibility flag providerruntime.Resolver above already
		// uses, so a bootstrap admin with no configured profiles yet keeps
		// working exactly as before this checkpoint.
		ProviderProfiles:  store,
		ExecutionPolicies: store,
		TrustedLocal:      cfg.TrustedLocalMode,
	})
	return coordinator, workflowsvc.New(coordinator), wakeScheduler
}
