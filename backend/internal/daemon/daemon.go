// Package daemon owns the Agent Orchestrator backend process: config loading,
// loopback HTTP serving, durable storage, CDC fan-out, lifecycle wiring, and
// graceful shutdown.
package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/modelcatalog"
	chatdriverregistry "github.com/aoagents/agent-orchestrator/backend/internal/adapters/chatdriver/registry"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/reviewer"
	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/runtime/runtimeselect"
	"github.com/aoagents/agent-orchestrator/backend/internal/autoreview"
	"github.com/aoagents/agent-orchestrator/backend/internal/branchlock"
	"github.com/aoagents/agent-orchestrator/backend/internal/browserruntime"
	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/daemon/supervisor"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/identity"
	"github.com/aoagents/agent-orchestrator/backend/internal/mailer"
	"github.com/aoagents/agent-orchestrator/backend/internal/mobilebridge"
	"github.com/aoagents/agent-orchestrator/backend/internal/notify"
	baselineevidence "github.com/aoagents/agent-orchestrator/backend/internal/observe/projectmemory"
	usagepipeline "github.com/aoagents/agent-orchestrator/backend/internal/observe/usage"
	"github.com/aoagents/agent-orchestrator/backend/internal/observe/usage/pricing"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/presence"
	"github.com/aoagents/agent-orchestrator/backend/internal/preview"
	"github.com/aoagents/agent-orchestrator/backend/internal/previewserver"
	"github.com/aoagents/agent-orchestrator/backend/internal/projectmemory"
	"github.com/aoagents/agent-orchestrator/backend/internal/push"
	"github.com/aoagents/agent-orchestrator/backend/internal/runfile"
	"github.com/aoagents/agent-orchestrator/backend/internal/runtimegc"
	"github.com/aoagents/agent-orchestrator/backend/internal/secretbox"
	agentsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/agent"
	authsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/authsvc"
	browsersvc "github.com/aoagents/agent-orchestrator/backend/internal/service/browser"
	capacitysvc "github.com/aoagents/agent-orchestrator/backend/internal/service/capacity"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	devimportsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/devimport"
	environmentsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/environment"
	executionpolicysvc "github.com/aoagents/agent-orchestrator/backend/internal/service/executionpolicy"
	importsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/importer"
	notificationsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/notification"
	prsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/pr"
	projectsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/project"
	projectmemorysvc "github.com/aoagents/agent-orchestrator/backend/internal/service/projectmemory"
	providerprofilesvc "github.com/aoagents/agent-orchestrator/backend/internal/service/providerprofile"
	providersetupsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/providersetup"
	questionssvc "github.com/aoagents/agent-orchestrator/backend/internal/service/questions"
	settingssvc "github.com/aoagents/agent-orchestrator/backend/internal/service/settings"
	usagesvc "github.com/aoagents/agent-orchestrator/backend/internal/service/usage"
	"github.com/aoagents/agent-orchestrator/backend/internal/skillassets"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/terminal"
	"github.com/aoagents/agent-orchestrator/backend/internal/workflow/wakepoller"
)

// Run starts the daemon and blocks until it exits. SIGINT/SIGTERM drive
// graceful shutdown through the HTTP server and background workers.
// staticDataDir adapts a fixed AO_DATA_DIR string to
// providerprofilesvc.DataDirer.
type staticDataDir string

func (d staticDataDir) DataDir() string { return string(d) }

// Run loads configuration, wires the daemon's dependencies and serves until
// the process is signalled to stop. It is the whole of the daemon binary's
// behaviour, and returns the first error that makes serving impossible.
func Run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	return RunWithConfig(cfg)
}

// RunWithConfig starts the same daemon composition root as Run with an
// already-resolved configuration. It exists for the foreground `ao server`
// command; Electron continues to use Run and environment-based discovery.
func RunWithConfig(cfg config.Config) error {
	var err error
	if cwd, err := os.Getwd(); err == nil {
		cfg.StartupWorkingDirectory = cwd
	}
	if err := stabilizeWorkingDirectory(cfg.DataDir); err != nil {
		return err
	}
	ignoreBrokenPipeSignal()

	log := newLogger()
	log.Info("daemon starting", "data_dir", cfg.DataDir, "listen", cfg.Addr(), "frontend_root", cfg.WebRoot)
	var browserRuntimeToken string
	if os.Getenv(browserruntime.RuntimeTokenStdinEnv) == "1" {
		browserRuntimeToken, err = browserruntime.ReadRuntimeToken(os.Stdin)
		if err != nil {
			return err
		}
	}
	if browserRuntimeToken == "" {
		browserRuntimeToken, err = browserruntime.NewToken()
		if err != nil {
			return err
		}
	}
	browserAuthority := browsersvc.NewAuthority()
	browserBroker := browserruntime.New(log, browserRuntimeToken)

	// Fail fast only if a daemon is genuinely still serving the recorded port.
	// CheckStale confirms the run-file's PID is alive, but that alone is not
	// proof a predecessor owns the port: the file leaks when the daemon is hard
	// killed without a graceful shutdown (the norm on Windows, where the desktop
	// supervisor can only TerminateProcess it), and Windows reuses the recorded
	// PID for unrelated processes. So a "live" PID is verified against an actual
	// /healthz probe; a run-file left by a crashed/hard-killed/reused-PID
	// predecessor is treated as stale and overwritten when the new server starts.
	if live, err := runfile.CheckStale(cfg.RunFilePath); err != nil {
		return fmt.Errorf("inspect run-file: %w", err)
	} else if live != nil && runFileOwnerServing(&http.Client{Timeout: staleProbeTimeout}, config.LoopbackHost, live) {
		return fmt.Errorf("daemon already running (pid %d, port %d); refusing to start", live.PID, live.Port)
	}

	// Open the durable store and bring up the CDC substrate: DB triggers capture
	// changes into change_log, the poller tails it, and the broadcaster fans
	// events out to live transports.
	store, err := sqlite.Open(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = store.Close() }()

	// Refresh the embedded using-ao skill into the data dir so worker sessions
	// in any project can read the ao CLI catalog from a stable absolute path.
	// Non-fatal: the skill is an enhancement over `ao --help`, not required.
	if err := skillassets.Install(cfg.DataDir); err != nil {
		log.Warn("install using-ao skill", "err", err)
	}

	// Checkpoint 8P-A: application identity. authMgr is wired into every
	// daemon boot (not gated on AO_TRUSTED_LOCAL_MODE) so login/logout/me
	// and the identity middleware always have somewhere real to resolve
	// against; TrustedLocalMode only changes how the middleware/controller
	// behave when NO session cookie is present. Bootstrap runs every boot
	// but only acts once (zero rows in users): it creates the admin from
	// AO_BOOTSTRAP_ADMIN_EMAIL/AO_BOOTSTRAP_ADMIN_PASSWORD when set, and
	// backfills any pre-existing NULL project/workflow-run owner to that
	// admin so nothing is silently orphaned. Never hard-fails startup.
	authMgr := authsvc.New(store, nil)
	bootstrapResult, err := authMgr.Bootstrap(context.Background(), os.Getenv("AO_BOOTSTRAP_ADMIN_EMAIL"), os.Getenv("AO_BOOTSTRAP_ADMIN_PASSWORD"))
	if err != nil {
		log.Warn("bootstrap admin setup failed", "err", err)
	} else if bootstrapResult.Created {
		log.Info("created bootstrap admin user",
			"userId", bootstrapResult.AdminID,
			"backfilledProjects", bootstrapResult.BackfilledProjects,
			"backfilledWorkflowRuns", bootstrapResult.BackfilledWorkflowRuns,
		)
	} else if bootstrapResult.Skipped {
		log.Warn("no admin user exists yet; set AO_BOOTSTRAP_ADMIN_EMAIL and AO_BOOTSTRAP_ADMIN_PASSWORD and restart to create one")
	}

	// Checkpoint 8P-E.8.1: promotes a lone pre-8P-E.8 user (created before
	// the role column existed, backfilled to 'member' by migration 0116) to
	// owner. A no-op once any owner exists or when zero/multiple users
	// exist. Never hard-fails startup.
	if promoted, err := authMgr.EnsureOwnerExists(context.Background()); err != nil {
		log.Warn("ensure-owner-exists check failed", "err", err)
	} else if promoted {
		log.Info("promoted sole pre-existing user to installation owner")
	}

	telemetrySink := newTelemetrySink(cfg, store, log)
	defer func() { _ = telemetrySink.Close(context.Background()) }()
	telemetrySink.Emit(context.Background(), ports.TelemetryEvent{
		Name:       "ao.daemon.started",
		Source:     "daemon",
		OccurredAt: time.Now().UTC(),
		Level:      ports.TelemetryLevelInfo,
		Payload: map[string]any{
			"port":  cfg.Port,
			"agent": cfg.Agent,
		},
	})

	// signal.NotifyContext cancels ctx on SIGINT/SIGTERM, which drives the
	// graceful shutdown inside Server.Run and stops the background goroutines.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cdcPipe, err := startCDC(ctx, store, log)
	if err != nil {
		return err
	}

	// Terminal streaming: the selected runtime (tmux on macOS/Linux, conpty on Windows) supplies the
	// attach Stream and liveness; the CDC broadcaster feeds the session-state channel. The manager
	// is handed to httpd, which mounts it at /mux. Raw PTY bytes never flow
	// through the CDC change_log -- only session-state events do.
	runtimeAdapter := runtimeselect.New(log, cfg.TmuxSocket, filepath.Join(cfg.DataDir, "tmp"))
	managedPreview := previewserver.New(log, cfg.DataDir)
	termMgr := terminal.NewManager(runtimeAdapter, cdcPipe.Broadcaster, log)
	defer termMgr.Close()
	// Checkpoint 8P-B.2: the /mux WebSocket bypasses SessionsController
	// entirely (a client supplies the terminal/pane id only after the
	// connection upgrades, not via a REST path param), so it needs its own
	// authorization boundary rather than inheriting SessionsController's.
	// r.Context() at WS-accept time already carries identity.Middleware's
	// resolved identity (identity.Middleware runs before mountTerminalMux
	// in router.go), so this closure only needs to read it back and check
	// ownership -- same semantics as controllers.AuthorizeSessionAccess,
	// deliberately not importing httpd/controllers here to keep daemon's
	// dependency direction one-way.
	termMgr.SetAttachAuthorizer(func(ctx context.Context, id string) bool {
		if cfg.TrustedLocalMode {
			return true
		}
		user, ok := identity.FromContext(ctx)
		if !ok {
			return false
		}
		owner, err := store.GetSessionOwner(ctx, domain.SessionID(id))
		if err != nil || owner == nil || *owner != user.ID {
			return false
		}
		return true
	})

	// The agent messenger sends validated user input to the session's live
	// runtime pane. Keep this path small until durable inbox semantics are needed.
	// Built before the Lifecycle Manager so the LCM can use it for SCM-driven
	// agent nudges (CI failure, review feedback, merge conflict).
	messenger := newSessionMessenger(store, runtimeAdapter, log)
	lifecycleMessenger := newModeAwareMessenger()
	// The driver registry is the chat capability gate, and Settings reports what
	// it allows, so it is resolved before both.
	chatDrivers := chatdriverregistry.Build(log)
	notificationHub := notify.NewHub()
	notifier := notificationsvc.New(notificationsvc.Deps{Store: store})
	// Daemon-owned preferences. The store's type is field-compatible with the
	// service's, adapted in settings_wiring.go so neither package imports the
	// other. Built here, before the notification writer, because the optional
	// completion-email fan-out resolves its destination through it.
	//
	// The secret box's key file lives beside the database under the AO data
	// dir, so the SMTP password's encryption key travels with the profile it
	// belongs to and never lands in an OS-default app-data location.
	settingsSvc := settingssvc.New(
		settingsStore{store: store},
		chatDrivers,
		func() time.Time { return time.Now().UTC() },
	).WithEmail(secretbox.New(cfg.DataDir), &mailer.Sender{})
	notificationWriter := notify.New(notify.Deps{
		Store:     store,
		Publisher: notificationHub,
		Emailer:   settingssvc.NewNotificationEmailer(settingsSvc),
		Logger:    log,
	})
	// Resolution transitions that happened while the daemon was down never
	// reached lifecycle, so re-check open notifications against the durable
	// session/PR facts before serving. Best-effort: a failure here only leaves
	// stale rows in the unresolved list, never blocks startup.
	if err := notificationWriter.Reconcile(ctx); err != nil {
		log.Warn("notification resolution reconcile failed", "err", err)
	}

	// Bring up the Lifecycle Manager and the reaper first: it makes the session
	// lifecycle write path live (reducer write -> store -> DB trigger ->
	// change_log -> poller -> broadcaster) and gives startSession the shared LCM.
	// The agent resolver is built before the LCM so lifecycle can consume the
	// adapter-declared active-turn steering capability; startSession reuses it.
	defaultAgent := cfg.Agent
	if defaultAgent == "" {
		defaultAgent = config.DefaultAgent
	}
	agents, err := buildAgentResolver(defaultAgent, log)
	if err != nil {
		stop()
		if cdcErr := cdcPipe.Stop(); cdcErr != nil {
			log.Error("cdc pipeline shutdown", "err", cdcErr)
		}
		return fmt.Errorf("wire agent resolver: %w", err)
	}

	lcStack := startLifecycle(ctx, store, runtimeAdapter, lifecycleMessenger, notificationWriter, telemetrySink, agents, log)

	// Wire the controller-facing session service over the same store + LCM, the
	// selected runtime, routed git/scratch workspaces, the per-session agent
	// resolver (AO_AGENT validated here for compatibility), and the agent
	// messenger, then mount it on the API.
	// Chat service. The driver registry is the capability gate: a harness with no
	// registered driver cannot start in chat mode, so an unsupported request fails
	// loudly instead of silently becoming a TUI session.
	chatSvc := chatsvc.New(chatsvc.Options{
		Store:    store,
		Sessions: store,
		// Adapts the store's own snapshot type, so the chat service never has to
		// import the storage layer.
		Reader: chatsvc.SnapshotReaderFunc(func(ctx context.Context, conversationID string) (chatsvc.ConversationRows, error) {
			rows, err := store.LoadConversationSnapshot(ctx, conversationID)
			if err != nil {
				return chatsvc.ConversationRows{}, err
			}
			return chatsvc.ConversationRows{
				Conversation:               rows.Conversation,
				Turns:                      rows.Turns,
				Messages:                   rows.Messages,
				Activities:                 rows.Activities,
				BranchPoints:               rows.BranchPoints,
				BranchedFromEarlierMessage: rows.BranchedFromEarlierMessage,
			}, nil
		}),
		PageReader: chatsvc.SnapshotPageReaderFunc(func(ctx context.Context, conversationID string, beforeSequence, limit int64) (chatsvc.ConversationRows, error) {
			rows, err := store.LoadConversationSnapshotPage(ctx, conversationID, beforeSequence, limit)
			if err != nil {
				return chatsvc.ConversationRows{}, err
			}
			return chatsvc.ConversationRows{
				Conversation:               rows.Conversation,
				Turns:                      rows.Turns,
				Messages:                   rows.Messages,
				Activities:                 rows.Activities,
				BranchPoints:               rows.BranchPoints,
				BranchedFromEarlierMessage: rows.BranchedFromEarlierMessage,
				OldestSequence:             rows.OldestSequence,
				HasMoreBefore:              rows.HasMoreBefore,
			}, nil
		}),
		Drivers: chatDrivers,
		// The LCM satisfies ActivityRecorder directly: a chat turn is a pure
		// lifecycle reduction, same as a hook signal from a terminal session.
		Activity: lcStack.LCM,
		Log:      log,
		NewID:    uuid.NewString,
	})

	sessions, err := startSession(ctx, cfg, runtimeAdapter, store, lcStack.LCM, messenger, telemetrySink, agents, managedPreview, browserBroker, browserAuthority, chatLauncher{svc: chatSvc}, settingsSvc, log)
	if err != nil {
		stop()
		lcStack.Stop()
		if cdcErr := cdcPipe.Stop(); cdcErr != nil {
			log.Error("cdc pipeline shutdown", "err", cdcErr)
		}
		return fmt.Errorf("wire session service: %w", err)
	}
	sessionSvc, reviewSvc, sessMgr := sessions.Service, sessions.Review, sessions.Lifecycle
	rawSessionMgr, workspaceObserver := sessions.Manager, sessions.Workspace
	sessMgr.SetTerminalInputGate(termMgr)
	lifecycleMessenger.Bind(sessionLifecycleMessenger{sessMgr})
	lcStack.LCM.SetCompletionTerminator(sessMgr)
	lcStack.LCM.SetSessionInputLease(sessMgr)
	lcStack.LCM.SetSessionOperationGate(sessMgr)
	termMgr.SetSessionInputLease(sessMgr)
	projectSvc := projectsvc.NewWithDeps(projectsvc.Deps{Store: store, Sessions: sessionSvc, DefaultHarness: domain.AgentHarness(cfg.Agent), Telemetry: telemetrySink, AllowedRoots: cfg.AllowedProjectRoots, BranchLocks: store})
	if err := seedScratchProjectOnBoot(ctx, cfg, projectSvc); err != nil {
		stop()
		lcStack.Stop()
		if cdcErr := cdcPipe.Stop(); cdcErr != nil {
			log.Error("cdc pipeline shutdown", "err", cdcErr)
		}
		return err
	}
	lcStack.trackerDone = startTrackerIntake(ctx, store, sessionSvc, log)

	agentSvc := agentsvc.NewWithDeps(agentsvc.Deps{Cache: store, Discoverer: modelcatalog.Discoverer{}, Projects: store})
	go func() {
		if _, err := agentSvc.Refresh(ctx); err != nil {
			log.Warn("initial agent catalog refresh failed", "err", err)
		}
	}()
	environmentSvc := environmentsvc.New(agentSvc, projectSvc)

	// Connect Mobile: the bridge service needs the LAN listener, but the LAN
	// listener needs the built router's handler, which only exists once srv is
	// constructed — and srv's router mounts the mobile controller, which needs
	// the bridge service. Break the cycle with late binding: build bs with LAN
	// left nil, hand its controller into NewWithDeps, then once srv exists,
	// build the LAN listener over srv.Handler() and assign it onto bs.LAN.
	bs := &controllers.BridgeService{
		ConfigPath:  mobilebridge.Path(cfg.DataDir),
		DefaultPort: mobilebridge.DefaultPort,
	}
	mc := &controllers.MobileController{Bridge: bs}
	browserService := browsersvc.New(sessionSvc, browserBroker, browserAuthority)

	// Standalone shell terminals: user-opened shells with no agent session
	// behind them. They reuse the same runtime adapter (and therefore the same
	// terminal mux) as session panes, but keep their own ids, storage, and
	// lifetime — see internal/service/shellterm.
	shellTermSvc := startShellTerminals(ctx, cfg, runtimeAdapter, store, projectSvc, sessionSvc, log)
	// Late-bound so Kill/Cleanup close a session's scoped shells before its
	// worktree is torn down (shellTermSvc cannot exist before sessMgr does; see
	// SetShellTerminalCloser).
	sessMgr.SetShellTerminalCloser(shellTermSvc)
	var (
		usageCollector *usagesvc.Collector
		usagePipeline  *usagepipeline.Pipeline
	)
	if roots, rootsErr := usagesvc.DefaultSourceRoots(ctx); rootsErr != nil {
		log.Warn("usage collection disabled", "err", rootsErr)
	} else {
		usageCollector = usagesvc.NewCollector(store, roots, func(reconcile bool) {
			if usagePipeline == nil {
				return
			}
			if reconcile {
				usagePipeline.NotifySourcesChanged()
			} else {
				usagePipeline.NotifyInventoryChanged()
			}
		})
		ingestor := usagepipeline.NewIngestor(store, usagepipeline.IngestorConfig{})
		usagePipeline = usagepipeline.NewPipeline(store, ingestor, []string{
			roots.ClaudeProjects,
			roots.CodexSessions,
			roots.CodexArchived,
		}, usagepipeline.CoordinatorConfig{
			Logger:     log,
			Initialize: usageCollector.BackfillActive,
			Reconcile: func(reconcileCtx context.Context) error {
				return usageCollector.ReconcileSources(reconcileCtx, 0)
			},
			ReconcilePath: usageCollector.ReconcilePath,
		})
		lcStack.LCM.SetUsageFinalizer(usageCollector)
	}
	lcStack.scmDone = startSCMObserver(ctx, store, lcStack.LCM, cfg.GitLab, log)
	var prActions prsvc.ActionManager
	prReader := newMultiSCMProvider(cfg.GitLab, log)
	prMerger := newMultiSCMMerger(cfg.GitLab, log)
	if prReader != nil && prMerger != nil {
		prActions = prsvc.NewActionService(prsvc.ActionDeps{Store: store, Merger: prMerger, Reader: prReader})
	} else {
		log.Warn("pr action service disabled: no usable SCM provider")
	}

	// Durable agent-switch reconciliation is a startup safety boundary. The
	// in-memory input fence disappeared with the previous daemon; if AO cannot
	// prove and recover every active saga, do not bind a usable API with user
	// input accidentally reopened. This runs after session-scoped shell wiring
	// (ordinary recovery may tear down a worktree) but before HTTP is bound.
	if reconcileErr := sessMgr.Reconcile(ctx); reconcileErr != nil {
		stop()
		managedPreview.Close()
		lcStack.Stop()
		if cdcErr := cdcPipe.Stop(); cdcErr != nil {
			log.Error("cdc pipeline shutdown", "err", cdcErr)
		}
		return fmt.Errorf("reconcile sessions on boot: %w", reconcileErr)
	}
	if reconcileErr := lcStack.ReconcileRuntime(ctx); reconcileErr != nil {
		log.Error("reconcile agent processes on boot failed", "err", reconcileErr)
	}
	// Workflow durable foundation (Checkpoint 8A) + Checkpoint 8C's review
	// dispatch: wire the coordinator/service and run its own read-mostly boot
	// recovery. Checkpoint 8C needs its own reviewer-launch path — deliberately
	// NOT the reviewEngine/reviewSvc wired into startSession above, since that
	// engine's Launcher always builds a PR-centric prompt internally with no
	// override hook (see workflow_reviewer_launcher.go's doc comment for the
	// full reasoning). It reuses the same reviewer registry/resolver and the
	// same runtime adapter every other session pane already uses — just not
	// the same Launcher wrapper.
	workflowReviewers, err := reviewer.NewResolver()
	if err != nil {
		stop()
		lcStack.Stop()
		if cdcErr := cdcPipe.Stop(); cdcErr != nil {
			log.Error("cdc pipeline shutdown", "err", cdcErr)
		}
		return fmt.Errorf("workflow reviewer resolver: %w", err)
	}
	workflowReviewerLauncher := &workflowReviewerLauncher{
		reviewers:  workflowReviewers,
		runtime:    runtimeAdapter,
		dataDir:    cfg.DataDir,
		runFile:    cfg.RunFilePath,
		auth:       reviewerAgentAuth{agents: agents},
		executable: os.Executable,
	}
	// Checkpoint 8K-B pass 2: the cross-provider Decision Resolver launcher.
	// Unlike workflowReviewerLauncher, this resolves through the SAME
	// per-session agent registry (agents) session_manager itself uses for
	// ordinary worker spawns — a resolver session is a plain read-only
	// worker-agent invocation, not a review pass, so it never needs the
	// reviewer registry/PR-centric prompt workaround workflowReviewerLauncher
	// documents.
	decisionResolverLauncher := &decisionResolverLauncher{
		agents:     agents,
		runtime:    runtimeAdapter,
		dataDir:    cfg.DataDir,
		runFile:    cfg.RunFilePath,
		executable: os.Executable,
	}
	// Checkpoint 8P-E.19: the Incident Advisor's Diagnostic Agent launcher.
	// Same agent registry, same runtime adapter and same PATH/shim handling as
	// the decision resolver above — the only differences are its narrower
	// read-only mandate and its isolated scratch workspace.
	incidentAgentLauncher := &incidentAgentLauncher{
		agents:     agents,
		runtime:    runtimeAdapter,
		dataDir:    cfg.DataDir,
		runFile:    cfg.RunFilePath,
		executable: os.Executable,
	}
	// Checkpoint 8P-E.11: durable direct-branch execution locks. The owner
	// token is this daemon instance's identity -- it is what lets boot
	// reconciliation tell a lock this instance still owns from one a crashed
	// predecessor left behind, without ever guessing from timestamps.
	// P1-D reuses this same token for the execution placements the workflow
	// coordinator freezes, so a placement and the lock protecting it name the
	// SAME daemon incarnation. Two independently generated tokens would mean
	// boot reconciliation could believe it owns the lock and not the placement.
	daemonInstanceToken := uuid.NewString()
	branchLocks := branchlock.New(branchlock.Deps{
		Store:      store,
		Preflight:  workspaceObserver,
		OwnerToken: daemonInstanceToken,
		NewID:      uuid.NewString,
		Logger:     log,
	})
	// Checkpoint 8P-E.14: ordinary tasks take the same locks, over the same
	// manager, so a task and a workflow targeting one repository+branch
	// serialize instead of both writing it. Late-bound because the session and
	// lifecycle managers are built before the lock manager exists.
	rawSessionMgr.SetBranchLocks(sessionBranchLocks{mgr: branchLocks})
	// Checkpoint 8P-E.14A: lifecycle drives the same locks at the session's turn
	// boundaries, through the re-acquire variant (see sessionTurnBranchLocks).
	lcStack.LCM.SetBranchLocks(sessionTurnBranchLocks{mgr: branchLocks})
	// Runtime GC is built BEFORE the workflow coordinator because the
	// coordinator now depends on it: a terminal run ends the runtime it owns
	// through the sweeper's own ReclaimSessionRuntime, so that the immediate
	// terminal path and the periodic sweep apply one implementation of the
	// ownership/incarnation proofs rather than two. Its own boot and periodic
	// sweeps are still started further down, at the point they were before.
	var placementGC runtimegc.WorktreeReleaser
	if wsMgr := taskWorkspaceManager(cfg, store, log); wsMgr != nil {
		placementGC = worktreeReleaser{mgr: wsMgr}
	}
	runtimeGC := newRuntimeGC(runtimeAdapter, store, placementGC, log)
	// Project memory. It is constructed unconditionally because it is inert
	// until something indexes a repository: with no completed pass every
	// context pack comes back empty with a stated reason, and every role falls
	// back to exactly the behaviour it had before P2-A. The graph backend is
	// the in-tree local one; an external adapter would be composed in here
	// with projectmemory.TeeGraph, never substituted for it.
	//
	// It is built before the workflow stack because P2-B's dispatch wrappers
	// take it: whether they do anything is decided by AO_MEMORY_MODE inside
	// startWorkflows, not by whether this value exists.
	projectMemory := projectmemory.NewService(store)
	// ONE provisioner for the whole daemon. It owns the sync single-flight and
	// the pack cache, so constructing a second one for the API surface would
	// give `ao memory report` its own syncer -- and a report could then trigger
	// a sync concurrent with the one a dispatch is already running, which is
	// precisely what the single-flight exists to prevent.
	memoryProvisioning := memoryProvisioner(projectMemory, log)

	workflowCoordinator, workflowSvc, wakeScheduler := startWorkflows(cfg, store, projectMemory, memoryProvisioning, rawSessionMgr, workspaceObserver, branchLocks, workflowReviewerLauncher, runtimeAdapter, decisionResolverLauncher, incidentAgentLauncher, notificationWriter, agents, newTerminalRuntimeReclaimer(runtimeGC, lcStack.LCM, log), plannerUsageRecorderFor(usageCollector), log)
	// Checkpoint 8P-E.13A: reconciliation can only decide a stopped owner's
	// lock once it can ask what that stop means, and only the coordinator knows
	// (branchlock/retention.go). The coordinator needs the lock manager to
	// exist first, so the classifier is handed over here, after construction
	// and before the first reconcile pass.
	branchLocks.SetClassifier(coordinatorLockClassifier{coordinator: workflowCoordinator})
	// Reconcile branch locks BEFORE workflow recovery: a run that is about to
	// be resumed must find its own lock adopted rather than contended, and a
	// run waiting on a branch a crashed or permanently stopped run held must
	// find that branch free.
	if _, blErr := branchLocks.Reconcile(ctx); blErr != nil {
		log.Error("reconcile branch locks on boot failed", "err", blErr)
	}
	if reconcileErr := workflowCoordinator.Reconcile(ctx); reconcileErr != nil {
		log.Error("reconcile workflow runs on boot failed", "err", reconcileErr)
	}
	// Checkpoint 8N.1: the daemon-level poller that claims due wakes and
	// resumes their runs automatically — see workflow/wakepoller's own doc
	// comment for why this is a separate package from wake.Scheduler itself.
	// Durable workflow work (today: plan generation, which takes minutes) runs
	// on a context the coordinator owns rather than on the request or poller
	// tick that entered it -- this is the daemon lifetime that still ends it.
	// Set before the poller starts, so the first wake it fires already has it.
	workflowCoordinator.SetExecutionLifetime(ctx)
	wakePoller := wakepoller.New(wakeScheduler, workflowCoordinator, wakepoller.Config{Logger: log})
	lcStack.wakePollerDone = wakePoller.Start(ctx)
	// P1-C: Runtime GC. It runs once here, at the one moment nothing is
	// mid-launch and a crashed daemon's orphans are most likely to exist, and
	// then on a low-frequency timer. It destroys only runtimes whose
	// ownership, exact incarnation and terminality it can prove.
	// P1-D §X: the sweep also reclaims AO-managed task worktrees whose work
	// provably landed. A nil manager narrows the sweep to runtimes only.
	lcStack.runtimeGCDone = startRuntimeGC(ctx, runtimeGC, log)
	autoReview := autoreview.New(store, reviewSvc, autoreview.Config{Logger: log})
	lcStack.autoReviewDone = autoReview.Start(ctx)
	// Push-device registry: persisted phones that receive OS push notifications.
	// A load failure must not block boot — degrade to no push rather than refusing
	// to start the daemon. pushRegistry (interface) is assigned only when load
	// succeeds so a failure leaves a true nil interface (not a non-nil interface
	// wrapping a nil pointer), which the controller's nil guard relies on to
	// return 501. pushDevices keeps the concrete registry for the dispatcher.
	// deviceRoster (interface) mirrors the same nil-guard as pushRegistry: it is
	// assigned only when load succeeds, so a failed load leaves a true nil
	// interface rather than a non-nil interface wrapping a nil *DeviceRegistry
	// (which would panic on first method call). The roster controller answers
	// 503 DEVICE_REGISTRY_UNAVAILABLE in that state instead of crashing or
	// silently no-oping.
	var (
		pushRegistry controllers.PushRegistry
		pushDevices  *mobilebridge.DeviceRegistry
		deviceRoster controllers.DeviceRoster
	)
	if reg, regErr := mobilebridge.LoadRegistry(mobilebridge.PushDevicesPath(cfg.DataDir)); regErr != nil {
		log.Warn("load push device registry failed; push notifications disabled", "err", regErr)
	} else {
		pushRegistry = reg
		pushDevices = reg
		deviceRoster = reg
	}

	// One presence tracker instance shared by APIDeps.Presence (the
	// heartbeat middleware that touches it) and APIDeps.DeviceLive (the roster
	// controller that reads it) — must be the same instance or every device
	// would silently report offline.
	presenceTracker := presence.NewTracker()

	// Push dispatcher: an additive notification-hub subscriber that relays each
	// new notification to every registered device via the Expo Push Service. Runs
	// for the daemon's lifetime and stops when ctx is cancelled. EXPO_ACCESS_TOKEN
	// (optional) enables Expo's enforced push security when set.
	if pushDevices != nil {
		dispatcher := push.NewDispatcher(notificationHub, pushDevices, push.NewExpoClient(os.Getenv("EXPO_ACCESS_TOKEN")), log)
		go dispatcher.Run(ctx)
	}

	// Shared between APIDeps.ProviderProfiles and providersetup.Service
	// (Checkpoint 8P-E.8.4): both need the same Prober/DataDir-scoped Get,
	// and constructing it twice would let the two surfaces silently drift.
	// Checkpoint 8P-E.13A.5: connecting/enabling/re-probing a profile keeps
	// the owner's stored execution policy priority lists in step, so a
	// provider connected after the policy was first saved does not stay
	// invisible to Settings. Shared with APIDeps.ExecutionPolicy below so both
	// surfaces read and repair the same policy through one implementation.
	executionPolicySvc := &executionpolicysvc.Service{Store: store}
	providerProfilesSvc := &providerprofilesvc.Service{
		Store:      store,
		Prober:     providerprofilesvc.CLIProber{},
		DataDir:    staticDataDir(cfg.DataDir),
		PolicySync: executionPolicySvc,
	}
	providerSetupSvc := &providersetupsvc.Service{
		Profiles:  providerProfilesSvc,
		Terminals: providerSetupTerminals{shellTermSvc},
		Launcher:  providersetupsvc.CLILauncher{},
		Prober:    providerprofilesvc.CLIProber{},
		DataDir:   staticDataDir(cfg.DataDir),
	}

	srv, err := httpd.NewWithDeps(cfg, log, termMgr, httpd.APIDeps{
		Projects:    projectSvc,
		Agents:      agentSvc,
		Environment: environmentSvc,
		Sessions:    sessionSvc,
		PRs:         prActions,
		Reviews:     reviewSvc,
		Workflows:   workflowSvc,
		// P1-C's observability: what is holding the machine, and what GC did.
		Scheduler:          workflowCoordinator,
		RuntimeGC:          runtimeGC,
		Notifications:      notifier,
		NotificationStream: notificationHub,
		Push:               pushRegistry,
		Presence:           presenceTracker,
		DeviceRoster:       deviceRoster,
		DeviceLive:         presenceTracker,
		Import:             importsvc.New(importsvc.Deps{Store: store}),
		ShellTerminals:     shellTermSvc,
		Conversations:      chatSvc,
		Settings:           settingsSvc,
		// The same resolver the workflow wiring uses, so /settings reports the
		// policy that is actually in force rather than a second reading of the
		// environment that could disagree with it.
		MemoryMode: string(memoryConfig(log).Mode),
		CDC:        store,
		Events:     cdcPipe.Broadcaster,
		Activity:   lcStack.LCM,
		UsageHooks: usageCollector,
		// P3-E: the same collector, reached through the pane-shaped entry point
		// that cannot touch a session's lifecycle.
		UsageSubjectHooks: usageCollector,
		UsageSummary:      usagesvc.NewSummaryReader(store),
		// P3-E: the ledger reader owns the run/project token and cost answers.
		// Its rate card is resolved once, from AO's own data dir, so a cost
		// figure always names the rates that produced it -- and a rate card
		// that is absent or unreadable simply leaves cost unknown while tokens
		// keep being reported in full.
		UsageLedger: usagesvc.NewLedgerReader(store, usagePricing(cfg.DataDir, log)),
		// The AO-ASSEMBLED context view, read from the same baseline evidence
		// tree P2 already writes. Deliberately a second reader rather than a
		// widened ledger: what AO sent and what a provider reported receiving
		// are different quantities, and one reader that could return both is
		// one refactor away from adding them.
		UsageContext:  usagesvc.NewContextReader(usageEvidenceSource(log)),
		Capacity:      capacitysvc.NewReader(store),
		ProjectMemory: projectmemorysvc.New(projectMemory, store).WithProvisioner(memoryProvisioning).WithWorkspaces(store),
		Questions: &questionssvc.AnswerService{
			Store: store, Runs: store, Sender: rawSessionMgr, Logger: log,
			// P3-D: the answer is recorded first and delivered second, and the
			// delivery must not claim a write landed in a composer that a modal
			// prompt was covering. This is the same session row the structured
			// dialog path reads.
			Inputs: sessionInputState{store: store},
		},
		Decisions: &questionssvc.ResolverAnswerService{Store: store},
		Telemetry: telemetrySink,
		Mobile:    mc,
		DevImport: devimportsvc.New(devimportsvc.Deps{
			Store:         store,
			TargetDataDir: cfg.DataDir,
			OpenSource: func(ctx context.Context, dataDir string) (devimportsvc.SourceStore, error) {
				return sqlite.OpenReadOnly(ctx, dataDir)
			},
		}),
		Browser:             browserService,
		PreviewServer:       managedPreview,
		SessionCapabilities: browserAuthority,
		Auth:                authMgr,
		ProjectOwnership:    store,
		WorkflowOwnership:   store,
		SessionOwnership:    store,
		ProviderProfiles:    providerProfilesSvc,
		ProviderSetup:       providerSetupSvc,
		ExecutionPolicy:     executionPolicySvc,
	})
	if err != nil {
		stop()
		lcStack.Stop()
		if cdcErr := cdcPipe.Stop(); cdcErr != nil {
			log.Error("cdc pipeline shutdown", "err", cdcErr)
		}
		return err
	}
	previewDone := preview.NewPoller(store, sessionSvc, "http://"+srv.Addr().String(), preview.PollerConfig{Logger: log}).Start(ctx)
	_ = os.Unsetenv(browserruntime.RuntimeAddressEnv)
	if ln, addr, err := browserruntime.Listen(cfg.RunFilePath); err != nil {
		log.Warn("browser runtime: listener unavailable; agent browser control disabled", "err", err)
	} else {
		if err := os.Setenv(browserruntime.RuntimeAddressEnv, addr); err != nil {
			_ = ln.Close()
			return fmt.Errorf("publish browser runtime address: %w", err)
		}
		log.Info("browser runtime: listening", "addr", addr)
		go func() {
			if err := browserBroker.Serve(ctx, ln); err != nil {
				log.Warn("browser runtime: serve stopped with error", "err", err)
			}
		}()
	}
	var usageDone <-chan struct{}

	// Late-bind: the LAN listener shares the exact loopback router instance so
	// the LAN surface and loopback surface never drift apart.
	lan := httpd.NewMobileLAN(srv.Handler(), mobilebridge.DefaultPort, log, telemetrySink)
	bs.LAN = lan

	// Restore Connect Mobile across a daemon restart: if the bridge was left
	// enabled, re-arm the listener on its last port with the same password
	// hash so an already-paired phone keeps working with no new password, and
	// (via bs.RestoreOnBoot) re-apply the secure-pairing proxy against the
	// port Start actually bound. Routed through bs, not lan directly, so the
	// proxy never gets pinned to a dead port after an ephemeral fallback.
	// Best-effort: never blocks boot.
	if err := restoreMobileOnBoot(mobilebridge.Path(cfg.DataDir), bs); err != nil {
		log.Warn("restore mobile bridge on boot failed", "err", err)
	}

	if usagePipeline != nil {
		usageDone = usagePipeline.Start(ctx)
	}
	// ponytail: 5s tolerates a brief frontend restart; tune if dev hot-reload trips it.
	const supervisorGrace = 5 * time.Second

	if ln, addr, err := supervisor.Listen(cfg.RunFilePath); err != nil {
		// Non-fatal: without the link the daemon still works (e.g. headless "ao start"),
		// it just will not auto-stop when a frontend dies. Do not block startup on it.
		log.Warn("supervisor: listener unavailable; frontend-death auto-stop disabled", "err", err)
	} else {
		log.Info("supervisor: listening", "addr", addr)
		sup := supervisor.New(supervisorGrace, srv.RequestShutdown, log)
		go func() {
			if err := sup.Serve(ctx, ln); err != nil {
				log.Warn("supervisor: serve stopped with error", "err", err)
			}
		}()
	}

	runErr := srv.Run(ctx)

	// Both graceful shutdown paths (SIGTERM and POST /shutdown) funnel through
	// srv.Run returning. We deliberately do NOT tear down sessions here: they
	// survive the daemon exit and the next boot's Reconcile adopts them,
	// preserving session IDs. The narrowed sessionLifecycle interface makes
	// teardown-on-shutdown a compile error.

	// Shut the background goroutines down in order: cancel the context FIRST so
	// their loops exit, then wait for them to drain. Doing this explicitly (not
	// via defer) avoids the LIFO trap where a Stop() that blocks on ctx-cancel
	// runs before the cancel: a non-signal exit path would hang otherwise.
	stop()
	managedPreview.Close()
	<-previewDone
	// Close chat controllers before the lifecycle stack: each owns an app-server
	// child process, and closing them also settles any turn left in flight so a
	// restart does not read a half-finished turn as still working.
	chatStopCtx, chatCancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	chatSvc.StopAll(chatStopCtx)
	chatCancel()
	if usageDone != nil {
		<-usageDone
	}
	lcStack.Stop()
	// Tear the tailnet proxy down before the listener it fronts. `tailscale
	// serve --bg` state lives in tailscaled and outlives this process, so
	// leaving it would keep publishing a local port that no longer has the
	// authenticated LAN listener behind it. Best-effort and never blocking:
	// boot restore re-applies it against the next bound port.
	bs.ShutdownServe()
	lanStopCtx, lanCancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer lanCancel()
	if err := lan.Stop(lanStopCtx); err != nil {
		log.Error("mobile LAN listener shutdown", "err", err)
	}
	if err := cdcPipe.Stop(); err != nil {
		log.Error("cdc pipeline shutdown", "err", err)
	}
	return runErr
}

func seedScratchProjectOnBoot(ctx context.Context, cfg config.Config, projects *projectsvc.Service) error {
	if projects == nil {
		return nil
	}
	if _, err := projects.EnsureDefaultScratchProject(ctx, filepath.Join(cfg.DataDir, "scratch", "default")); err != nil {
		return fmt.Errorf("seed scratch project: %w", err)
	}
	return nil
}

// newLogger returns the daemon's slog logger. It writes to stderr so supervisors
// can capture it separately from any structured stdout protocol added later.
func newLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func stabilizeWorkingDirectory(dataDir string) error {
	if dataDir == "" {
		return fmt.Errorf("daemon working directory: data dir is required")
	}
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return fmt.Errorf("daemon working directory: create %s: %w", dataDir, err)
	}
	if err := os.Chdir(dataDir); err != nil {
		return fmt.Errorf("daemon working directory: chdir %s: %w", dataDir, err)
	}
	return nil
}

// usagePricing resolves the P3-E rate card once for this daemon.
//
// A rate card that fails to parse is a configuration error worth surfacing, but
// it is never worth refusing to start over: the embedded catalog stays in
// force, the operator sees the warning, and every model it does not cover
// reports cost=unknown rather than a guess.
func usagePricing(dataDir string, log *slog.Logger) *pricing.Table {
	table, err := pricing.Load(dataDir)
	if err != nil {
		if log != nil {
			log.Warn("usage rate card ignored; falling back to the embedded catalog",
				"file", pricing.RateCardFileName, "err", err)
		}
		return pricing.Embedded()
	}
	return table
}

// usageEvidenceSource opens the P2 baseline evidence tree for reading.
//
// It returns nil rather than failing when the tree cannot be opened: a daemon
// whose evidence directory does not exist yet, or whose data dir is configured
// somewhere the reader refuses, must still serve token usage. The context
// section is then simply absent, which the API renders as "no context data
// recorded" — an absence, not a set of zeroes.
func usageEvidenceSource(log *slog.Logger) *baselineevidence.DirSource {
	source, err := baselineevidence.NewDefaultDirSource()
	if err != nil {
		if log != nil {
			log.Warn("assembled-context evidence is unreadable; usage will report tokens only", "err", err)
		}
		return nil
	}
	return source
}
