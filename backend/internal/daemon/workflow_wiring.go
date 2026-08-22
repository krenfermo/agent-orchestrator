package daemon

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/capacityprobe"
	plannercommand "github.com/aoagents/agent-orchestrator/backend/internal/adapters/planner/command"
	workspacerouter "github.com/aoagents/agent-orchestrator/backend/internal/adapters/workspace/router"
	"github.com/aoagents/agent-orchestrator/backend/internal/branchlock"
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
// workflowBranchLocks adapts *branchlock.Manager to workflowcore.BranchLocks.
// Workflow deliberately depends on its own narrow types rather than importing
// branchlock, so the request translation lives here in composition-root wiring,
// exactly like workflowAgentSwitcher above.
type workflowBranchLocks struct {
	mgr *branchlock.Manager
}

func (w workflowBranchLocks) Acquire(ctx context.Context, req workflowcore.BranchLockRequest) ([]domain.BranchLock, error) {
	return w.mgr.Acquire(ctx, branchlock.AcquireRequest{
		ProjectID: req.ProjectID,
		RunID:     req.RunID,
		StepID:    req.StepID,
		SessionID: req.SessionID,
	})
}

func (w workflowBranchLocks) ReleaseRun(ctx context.Context, runID, reason string) (int64, error) {
	return w.mgr.ReleaseRun(ctx, runID, reason)
}

func (w workflowBranchLocks) HeldByRun(ctx context.Context, runID string) ([]domain.BranchLock, error) {
	return w.mgr.HeldByRun(ctx, runID)
}

func (w workflowBranchLocks) Renew(ctx context.Context, runID, stepID, sessionID string) {
	w.mgr.Renew(ctx, runID, stepID, sessionID)
}

func (w workflowBranchLocks) RecoverStale(ctx context.Context, runID string) (int64, error) {
	return w.mgr.RecoverStale(ctx, runID)
}

// coordinatorLockClassifier adapts the workflow coordinator to
// branchlock.OwnerClassifier (Checkpoint 8P-E.13A), translating between the two
// packages' own mirrored disposition types so neither has to import the other.
type coordinatorLockClassifier struct {
	coordinator *workflowcore.Coordinator
}

func (c coordinatorLockClassifier) ClassifyLockOwner(ctx context.Context, run domain.WorkflowRun) (branchlock.OwnerDisposition, error) {
	disp, err := c.coordinator.ClassifyLockOwner(ctx, run)
	if err != nil {
		return branchlock.OwnerDisposition{}, err
	}
	return branchlock.OwnerDisposition{
		SelfRemediable: disp.SelfRemediable,
		ProtectsWork:   disp.ProtectsWork,
		Reason:         disp.Reason,
	}, nil
}

func startWorkflows(cfg config.Config, store *sqlite.Store, sessionMgr *sessionmanager.Manager, workspace *workspacerouter.Workspace, branchLocks *branchlock.Manager, reviewerLauncher workflowcore.ReviewerLauncher, paneReader workflowcore.PaneReader, decisionResolverLauncher workflowcore.DecisionResolverLauncher, log *slog.Logger) (*workflowcore.Coordinator, *workflowsvc.Service, *wake.Scheduler) {
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
		// Checkpoint 8P-E.11: direct-branch execution. The workspace router
		// doubles as the committer -- it already knows which adapter a project
		// uses, so the autonomous local commit lands in the direct-branch
		// adapter and is refused for every other mode.
		BranchLocks:        workflowBranchLocks{mgr: branchLocks},
		WorkspaceCommitter: workspace,
		// Checkpoint 8P-E.13A.4: without an active prober, a provider profile
		// that has never been dispatched to reports CapacityUnknown until a
		// human happens to run it, which is how an authenticated Codex reviewer
		// could sit unusable while a high-risk independent review waited.
		CapacityProber: capacityprobe.New(),
	})
	return coordinator, workflowsvc.New(coordinator), wakeScheduler
}
