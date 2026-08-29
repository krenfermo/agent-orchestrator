package daemon

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/capacityprobe"
	plannercommand "github.com/aoagents/agent-orchestrator/backend/internal/adapters/planner/command"
	workspacerouter "github.com/aoagents/agent-orchestrator/backend/internal/adapters/workspace/router"
	"github.com/aoagents/agent-orchestrator/backend/internal/branchlock"
	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/contextrouter"
	"github.com/aoagents/agent-orchestrator/backend/internal/contextrouter/wfrouter"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/integration"
	"github.com/aoagents/agent-orchestrator/backend/internal/observe/projectmemory"
	"github.com/aoagents/agent-orchestrator/backend/internal/observe/projectmemory/wfdispatch"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/providerpreflight"
	"github.com/aoagents/agent-orchestrator/backend/internal/providerruntime"
	workflowsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/workflow"
	sessionmanager "github.com/aoagents/agent-orchestrator/backend/internal/session_manager"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
	"github.com/aoagents/agent-orchestrator/backend/internal/workflow/wake"
	taskworkspace "github.com/aoagents/agent-orchestrator/backend/internal/workspace"
	"github.com/aoagents/agent-orchestrator/backend/internal/worktree"
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

// Cede forwards P1-D §L's generation-safe branch-lock transfer.
//
// It is on the ADAPTER and not only on the manager because the coordinator
// holds the adapter: workflow type-asserts its branchLocks dependency for this
// capability, and an adapter that did not forward it would leave every
// direct-branch repair silently refused with the P1-B error, in production
// only, while every test that used the manager directly passed.
func (w workflowBranchLocks) Cede(ctx context.Context, lockID, fromRunID, toRunID, toStepID string) (bool, error) {
	return w.mgr.Cede(ctx, lockID, fromRunID, toRunID, toStepID)
}

func (w workflowBranchLocks) RecoverStale(ctx context.Context, runID string) (int64, error) {
	return w.mgr.RecoverStale(ctx, runID)
}

// sessionBranchLocks adapts *branchlock.Manager to
// sessionmanager.BranchLocks (Checkpoint 8P-E.14). Same convention as
// workflowBranchLocks above: the consumer declares its own narrow interface and
// the translation lives here in composition-root wiring.
//
// Note that it is the SAME *branchlock.Manager the workflow coordinator uses,
// over the same lock_key. That is what makes a task and a workflow contend for
// one repository+branch rather than each honoring a lock the other cannot see.
type sessionBranchLocks struct {
	mgr *branchlock.Manager
}

func (s sessionBranchLocks) AcquireForSession(ctx context.Context, projectID domain.ProjectID, sessionID domain.SessionID) ([]domain.BranchLock, error) {
	return s.mgr.Acquire(ctx, branchlock.AcquireRequest{
		ProjectID: projectID,
		SessionID: string(sessionID),
	})
}

func (s sessionBranchLocks) ReleaseSession(ctx context.Context, sessionID, reason string) (int64, error) {
	return s.mgr.ReleaseSession(ctx, sessionID, reason)
}

// sessionTurnBranchLocks adapts *branchlock.Manager to lifecycle's
// turn-boundary lock surface (Checkpoint 8P-E.14A).
//
// It is a second adapter over the same manager rather than a reuse of
// sessionBranchLocks because the two acquisitions are not the same question.
// Task start asks "may this task have the branch at all?", and a dirty
// repository or another owner must refuse it. A turn start asks "is the branch
// still this session's?", where already holding it, and a workflow run holding
// it on this session's behalf, are both normal — see
// branchlock.Manager.ReacquireForSession.
type sessionTurnBranchLocks struct {
	mgr *branchlock.Manager
}

func (s sessionTurnBranchLocks) AcquireForSession(ctx context.Context, projectID domain.ProjectID, sessionID domain.SessionID) ([]domain.BranchLock, error) {
	return s.mgr.ReacquireForSession(ctx, projectID, string(sessionID))
}

func (s sessionTurnBranchLocks) ReleaseSession(ctx context.Context, sessionID, reason string) (int64, error) {
	return s.mgr.ReleaseSession(ctx, sessionID, reason)
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

// The production store MUST be able to record dispatch boundaries. The phased
// worker dispatch refuses to launch anything it cannot first record an intent
// for (workflow/dispatch_state_machine.go), so a store that silently stopped
// satisfying this interface would not degrade worker dispatch -- it would end
// it, at runtime, on every run. Pinning it here makes that a compile error.
var _ workflowcore.DispatchRecorder = (*sqlite.Store)(nil)

func startWorkflows(cfg config.Config, store *sqlite.Store, sessionMgr *sessionmanager.Manager, workspace *workspacerouter.Workspace, branchLocks *branchlock.Manager, reviewerLauncher workflowcore.ReviewerLauncher, paneReader workflowcore.PaneReader, decisionResolverLauncher workflowcore.DecisionResolverLauncher, incidentAgents workflowcore.IncidentAgentLauncher, notifications workflowcore.NotificationSink, agents ports.AgentResolver, log *slog.Logger) (*workflowcore.Coordinator, *workflowsvc.Service, *wake.Scheduler) {
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
	deps := workflowcore.Deps{
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
		Planner:                  plannercommand.Planner{Binary: plannerBinary, Model: plannerModel, Timeout: 3 * time.Minute, MaxTimeout: 12 * time.Minute, Logger: log},
		PlannerContextBuilder:    plannercommand.ContextBuilder{},
		Switcher:                 workflowAgentSwitcher{mgr: sessionMgr},
		QuestionsStore:           store,
		PaneReader:               paneReader,
		DecisionResolverLauncher: decisionResolverLauncher,
		IncidentAgents:           incidentAgents,
		// Checkpoint 8P-E.20: the project holding AO's OWN source, the only
		// repository an approved incident repair may be launched into. Unset
		// means self-repair is unavailable, which is a refusal rather than a
		// fallback — see incident_repair.go.
		SelfRepairProjectID: os.Getenv("AO_SELF_REPAIR_PROJECT"),
		WakeScheduler:       wakeScheduler,
		// A run that durably reaches the completed state raises one "workflow
		// finished" notification, on the same write-side producer lifecycle
		// uses for session notifications.
		Notifications:    notifications,
		Logger:           log,
		RuntimeIsolation: runtimeIsolation,
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
		// The target-integration lane, over the SAME branch-lock manager as
		// BranchLocks above. Sharing the manager is what makes an integration
		// and a direct writer of one branch exclude each other: they contend
		// for one lock key in one table, rather than for two ideas of a lock.
		IntegrationLocks: integration.NewBranchLocker(branchLocks),
		// Checkpoint 8P-E.24: the two evidence sources the recovery work added.
		//
		// WorkerLiveness is the independent "is the runtime still there" fact
		// the missing-first-signal reconciliation consults before it will call a
		// worker dead — the fact whose absence let AO declare a worker
		// agent_start_failed while it was sixteen minutes into a complete
		// implementation.
		//
		// WorkerPreflight asks, before an unattended launch, whether the
		// provider can start without an operator. Both are optional by
		// construction and both answer "unknown" rather than "no" whenever they
		// cannot tell, so neither can ground a dispatch on its own uncertainty.
		WorkerLiveness:  workflowWorkerLiveness{mgr: sessionMgr},
		WorkerPreflight: &providerpreflight.Checker{Agents: agents},
		// Checkpoint 8P-E.13A.4: without an active prober, a provider profile
		// that has never been dispatched to reports CapacityUnknown until a
		// human happens to run it, which is how an authenticated Codex reviewer
		// could sit unusable while a high-risk independent review waited.
		CapacityProber: capacityprobe.New(),
		// P1-C: runtime admission control. This is a DIFFERENT capacity from
		// CapacityProber's -- that one answers "is this provider usable", this
		// one answers "may AO start one more runtime on this machine at all".
		// Wiring it here is what makes "no runtime launch without an
		// authoritative capacity claim" true in every real deployment; a nil
		// store (test doubles only) simply admits everything, as AO did before.
		Capacity:       store,
		CapacityLimits: cfg.CapacityLimits,
		// P1-D: the frozen execution placement and the durable provider-attempt
		// ledger. Both are the same *sqlite.Store every other durable authority
		// uses -- there is deliberately no second database and no second
		// transaction boundary, because a placement and the capacity claim that
		// authorizes launching into it have to be readable in one consistent
		// view after a crash.
		Placements:         store,
		ProviderAttempts:   store,
		PlacementOverrides: store,
		// The SAME token the branch-lock manager stamps on every lock, so a
		// placement and the lock protecting it name one daemon incarnation.
		InstanceToken: branchLocks.OwnerToken(),
		// The AO-owned worktree lifecycle. It is what records a task's work as
		// landed, cleans the worktree and its ao/* branch up afterwards,
		// preserves both when the task did not land, and -- at boot, from
		// Coordinator.Reconcile -- finishes whichever of those a restart cut in
		// half.
		//
		// Task worktrees live under the SAME <data dir>/worktrees root the
		// per-session adapters use (lifecycle_wiring.go), namespaced per
		// project/run/task beneath it, so one AO_DATA_DIR still accounts for
		// every checkout AO has ever created. A construction failure is not
		// fatal: nil leaves worktrees exactly where they are, which is untidy
		// and never unsafe.
		TaskWorkspaces: taskWorkspaces(cfg, store, log),
		// The read side of the same records: which checkout and which ao/*
		// branch hold a task's work, for the planner projection the task API
		// and the Board render. It is the store directly rather than the
		// manager above, because reading a row needs none of the manager's
		// git-side machinery and a lifecycle manager that failed to build must
		// not also blind the board.
		TaskWorktreeRecords: store,
	}
	// Phase 0 project-memory baseline: when enabled, every agent dispatch
	// surface above is wrapped by an observer that records what that dispatch
	// had available and what it consumed. Off by default, and a wrapper never
	// changes what it wraps -- see internal/observe/projectmemory.
	deps = wfdispatch.Instrument(deps, projectMemoryBaselineRecorder(log), log)
	// Role-aware context routing: when enabled, the two surfaces where AO
	// itself assembles a context payload (the planner's documents, a worker
	// spawn's pre-fetched issue context) send a bounded, role-budgeted
	// selection instead of everything they hold. Off by default -- a disabled
	// flag yields a nil router, and wfrouter.Instrument then hands the
	// dependencies back untouched, so provider adapters keep receiving today's
	// full context.
	deps = wfrouter.Instrument(deps, contextRouterFor(log), log)
	coordinator := workflowcore.New(deps)
	return coordinator, workflowsvc.New(coordinator), wakeScheduler
}

// projectMemoryBaselineEnv is the opt-in switch for baseline evidence
// recording. Baseline measurement writes a file per agent dispatch, so it is a
// deliberate choice rather than something a daemon does by default.
const projectMemoryBaselineEnv = "AO_PROJECT_MEMORY_BASELINE"

// projectMemoryBaselineRecorder returns the baseline recorder when the opt-in
// env var is set, and nil otherwise -- nil meaning wfdispatch.Instrument hands
// back the dependencies untouched.
//
// A sink that cannot be constructed (an AO_DATA_DIR pointing somewhere AO must
// not write, an unresolvable home directory) disables recording with a warning
// rather than failing startup: a daemon that refused to boot over its own
// instrumentation would be trading a working orchestrator for a measured one.
func projectMemoryBaselineRecorder(log *slog.Logger) *projectmemory.Recorder {
	if strings.TrimSpace(os.Getenv(projectMemoryBaselineEnv)) == "" {
		return nil
	}
	sink, err := projectmemory.NewDefaultDirSink()
	if err != nil {
		if log != nil {
			log.Warn("project-memory baseline: recording disabled", "err", err)
		}
		return nil
	}
	if log != nil {
		log.Info("project-memory baseline: recording agent dispatch evidence", "dir", sink.Root())
	}
	return projectmemory.NewRecorder(sink)
}

// contextRouterFor builds the role-aware context router when
// contextrouter.FlagEnv opts in, and returns nil otherwise -- nil meaning
// wfrouter.Instrument hands the dependencies back untouched and dispatch keeps
// its pre-existing full-context behavior.
//
// Every evidence source is optional and failing to build one only costs that
// source: a router with a diff but no code graph still routes, and says so in
// the selection's notes. A misconfigured budget override, by contrast,
// disables routing entirely -- an operator who mistyped a budget must get the
// old behavior and a warning, not a payload sized by a budget they did not
// write.
func contextRouterFor(log *slog.Logger) *contextrouter.Router {
	if !contextrouter.Enabled() {
		return nil
	}
	// contextrouter.Default is the one place the shipped router is assembled,
	// so the daemon and the disabled-vs-enabled regression harness measure the
	// same configuration rather than two that merely look alike.
	router, err := contextrouter.Default(log)
	if err != nil {
		if log != nil {
			log.Warn("context router: disabled", "env", contextrouter.BudgetEnv, "err", err)
		}
		return nil
	}
	if log != nil {
		log.Info("context router: routing agent dispatch context", "budgets", router.Budgets().Describe())
	}
	return router
}

// taskWorkspaces builds the worktree lifecycle manager, or nil if it cannot be
// built.
//
// Returning nil rather than failing startup is deliberate and matches every
// other optional dependency above: without it AO creates no task worktrees to
// clean up in the first place, and a daemon that refused to boot over its
// housekeeping would be trading a working orchestrator for a tidy one.
// taskWorkspaceManager builds the AO-owned task worktree lifecycle manager, or
// nil when it cannot be constructed. It is exposed separately from
// taskWorkspaces so P1-D's placement GC can reach the concrete manager: the
// workflow port only needs the narrow interface, and the sweep needs the
// release path.
func taskWorkspaceManager(cfg config.Config, store *sqlite.Store, log *slog.Logger) *taskworkspace.Manager {
	mgr, err := taskworkspace.New(taskworkspace.Options{
		Root:  filepath.Join(cfg.DataDir, "worktrees"),
		Git:   worktree.NewExecGit(""),
		Store: store,
	})
	if err != nil {
		if log != nil {
			log.Error("workflow: AO task worktree lifecycle unavailable", "err", err)
		}
		return nil
	}
	return mgr
}

func taskWorkspaces(cfg config.Config, store *sqlite.Store, log *slog.Logger) workflowcore.TaskWorkspaces {
	mgr, err := taskworkspace.New(taskworkspace.Options{
		Root:  filepath.Join(cfg.DataDir, "worktrees"),
		Git:   worktree.NewExecGit(""),
		Store: store,
	})
	if err != nil {
		if log != nil {
			log.Error("workflow: AO task worktree lifecycle unavailable", "err", err)
		}
		return nil
	}
	return mgr
}

// workflowWorkerLiveness adapts the Session Manager's runtime probe to
// workflow's narrow port. It is a type rather than a direct dependency for the
// same reason every other adapter here is: the coordinator asks exactly one
// question, and naming it is what keeps "the workflow engine can drive the
// runtime" from being true in any broader sense.
type workflowWorkerLiveness struct{ mgr *sessionmanager.Manager }

func (l workflowWorkerLiveness) SessionAlive(ctx context.Context, id domain.SessionID) (bool, bool, error) {
	return l.mgr.SessionRuntimeAlive(ctx, id)
}
