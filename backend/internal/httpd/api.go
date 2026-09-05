package httpd

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/aoagents/agent-orchestrator/backend/internal/cdc"
	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/presence"
	authsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/authsvc"
	executionpolicysvc "github.com/aoagents/agent-orchestrator/backend/internal/service/executionpolicy"
	prsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/pr"
	projectsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/project"
	providerprofilesvc "github.com/aoagents/agent-orchestrator/backend/internal/service/providerprofile"
	providersetupsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/providersetup"
	rbacsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/rbac"
	reviewsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/review"
	ssosvc "github.com/aoagents/agent-orchestrator/backend/internal/service/ssosvc"
	workflowsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/workflow"
)

// The synchronous switch budget covers one 60s optional source handoff, a
// separate 2m human permission-decision window, target process/readiness checks,
// composer confirmation retries, and the final 150s generation acknowledgement,
// with headroom for teardown and durable writes.
const minimumSwitchAgentRequestTimeout = 6 * time.Minute

// APIDeps bundles every service the API layer's controllers depend on.
type APIDeps struct {
	// MemoryMode is the daemon's resolved project-memory rollout stage
	// ("off"/"assisted"/"preferred"). It is reported on GET /settings so a
	// task's creation summary can state whether AO will bring remembered
	// project knowledge to the work. Empty is a valid value and means the
	// deployment does not report one.
	MemoryMode  string
	Agents      controllers.AgentCatalog
	Projects    projectsvc.Manager
	Environment controllers.EnvironmentStatusProvider
	Sessions    controllers.SessionService
	Activity    controllers.ActivityRecorder
	UsageHooks  controllers.UsageHookRecorder
	// UsageSubjectHooks receives a runtime pane's own usage callback (P3-E).
	// Separate from UsageHooks because the two accept different things: a
	// session hook carries activity and a launch id, a pane hook carries only
	// what it spent.
	UsageSubjectHooks controllers.UsageSubjectHookRecorder
	UsageSummary      controllers.UsageSummaryService
	// UsageLedger backs P3-E's canonical per-run/per-project token and cost
	// accounting. Optional: nil leaves the ledger sections unset.
	UsageLedger controllers.UsageLedgerService
	// UsageContext backs the AO-assembled context section. Optional.
	UsageContext controllers.UsageContextService
	// Capacity backs Checkpoint 8J's read-only capacity/quota view. Optional
	// like the other 8H/8J surfaces; nil answers 501.
	Capacity controllers.CapacityService
	PRs      prsvc.ActionManager
	Reviews  reviewsvc.Manager
	// Workflows is nil until wired; the controller then answers 501, matching
	// the other optional surfaces. Checkpoint 8A: durable foundation only, no
	// execution.
	Workflows workflowsvc.Manager
	// ProjectMemory backs P2-A's project-memory status/inspect/rebuild/
	// invalidate surface. Optional like every other surface here: nil answers
	// 501, and a daemon built without it behaves exactly as it did before
	// P2-A.
	ProjectMemory controllers.ProjectMemoryService
	// ProjectMemoryGraph backs the code graph's status/sync/query routes. It is
	// separate from ProjectMemory and independently optional: a build with no
	// graph wired leaves it nil and the routes answer not-implemented, which is
	// the same shape every other optional surface here uses.
	ProjectMemoryGraph controllers.ProjectMemoryGraphService
	// ProjectIntelligence backs P4-G's Project Intelligence surface. Optional:
	// nil reports not-implemented, matching the graph's own convention.
	ProjectIntelligence controllers.ProjectIntelligenceService
	// Scheduler and RuntimeGC back P1-C's capacity and runtime-GC surfaces.
	// Optional like every other surface here: nil answers 501.
	Scheduler controllers.SchedulerService
	RuntimeGC controllers.RuntimeGCService
	// Questions backs Checkpoint 8K-A's durable question detection/answer
	// API. Optional: nil leaves the /questions routes and the run-detail
	// Questions field both answering 501/absent, matching the other
	// optional surfaces here.
	Questions controllers.WorkflowQuestionsService
	// Decisions backs Checkpoint 8K-B pass 2's cross-provider Decision
	// Resolver callback (`ao decision resolve`). Optional: nil leaves the
	// /decisions route answering 501, matching Questions' own convention.
	Decisions          controllers.DecisionsService
	Notifications      controllers.NotificationService
	NotificationStream controllers.NotificationStream
	Push               controllers.PushRegistry
	Import             controllers.ImportService
	ShellTerminals     controllers.ShellTerminalService
	// Conversations is nil until a Chat driver is wired; the controller then
	// answers 501 rather than panicking, matching the other optional surfaces.
	Conversations controllers.ConversationService
	// Settings is the daemon-owned preference surface.
	Settings            controllers.SettingsService
	DevImport           controllers.DevImportService
	CDC                 cdc.Source
	Events              cdcSubscriber
	Telemetry           ports.EventSink
	Mobile              *controllers.MobileController
	Browser             controllers.BrowserService
	PreviewServer       controllers.ManagedPreviewServer
	SessionCapabilities controllers.SessionCapabilityValidator

	// Presence tracks which mobile devices are currently running the app.
	// Nil disables presence tracking (the roster then reports every device offline).
	Presence *presence.Tracker

	// DeviceRoster and DeviceLive back the desktop-only mobile device roster.
	DeviceRoster controllers.DeviceRoster
	DeviceLive   controllers.LiveSet

	// Auth backs Checkpoint 8P-A's /auth/login, /auth/logout, /auth/me
	// routes and the identity-resolving middleware. Optional: nil leaves the
	// auth routes answering 501 and the identity middleware resolving no
	// user for every request (server-side ownership checks then behave as
	// if AO_TRUSTED_LOCAL_MODE were off, since there is nothing to resolve
	// to), matching the other optional-surface conventions here.
	Auth authsvc.Manager
	// SSO backs P4-A's OIDC surface (/auth/providers, /auth/oidc/*).
	// Optional in exactly the same sense as every other surface here: nil
	// leaves those routes answering 501 and leaves the installation
	// password-only, which is what every install that configures no provider
	// is.
	SSO ssosvc.Manager

	// Log is the daemon logger the API layer records audit lines to. It is
	// filled in by normalizeAPIDeps from the router's own logger, so callers
	// never set it; a nil logger simply means no audit line is written (the
	// telemetry sink, when wired, still receives the event).
	Log *slog.Logger
	// ProjectOwnership/WorkflowOwnership back Checkpoint 8P-A's minimal
	// ownership scoping on the projects/workflows controllers. Optional:
	// nil disables scoping entirely (pre-8P-A behavior). Both are satisfied
	// by the same *sqlite/store.Store the rest of the daemon already uses.
	ProjectOwnership controllers.OwnershipStore
	// ProjectTenancy places a newly registered project in an organization
	// (P4-C). Nil keeps the pre-P4-C behavior, matching ProjectOwnership's
	// convention.
	ProjectTenancy    controllers.TenantPlacement
	WorkflowOwnership controllers.WorkflowOwnershipStore

	// ProviderProfiles backs Checkpoint 8P-B's per-user provider connection
	// surface. Optional: nil leaves the /provider-profiles and
	// /providers/registry routes answering 501, matching the other
	// optional-surface conventions here.
	ProviderProfiles providerprofilesvc.Manager

	// ProviderSetup backs Checkpoint 8P-E.8.4's zero-terminal guided setup
	// surface (POST/DELETE /provider-profiles/{id}/setup). Optional: nil
	// leaves those two routes answering 501, independent of ProviderProfiles.
	ProviderSetup providersetupsvc.Manager

	// ExecutionPolicy backs Checkpoint 8P-C's per-user configurable
	// routing surface. Optional: nil leaves /execution-policy answering
	// 501, matching ProviderProfiles' own optional-surface convention.
	ExecutionPolicy executionpolicysvc.Manager

	// SessionOwnership backs Checkpoint 8P-B.1/8P-B.2's session ownership
	// scoping, shared by SessionsController, ConversationsController,
	// ReviewsController, and UsageController's per-session read -- see
	// controllers.AuthorizeSessionAccess. Optional: nil disables scoping
	// entirely, matching ProjectOwnership/WorkflowOwnership's convention.
	SessionOwnership controllers.SessionOwnershipStore

	// Authz is P4-B's canonical authorization evaluator, and ProjectScope
	// resolves the project a session or workflow run belongs to so a
	// permission can be asked about it. Both optional and both satisfied by
	// the same *sqlite/store.Store: nil leaves every gate falling back to
	// 8P-A/8P-B's owner-equality checks, which is exactly what a headless or
	// test daemon with no identity layer should do.
	Authz        controllers.Authorizer
	ProjectScope controllers.ProjectScope

	// RBAC backs P4-B's /users, /teams and /projects/{id}/access management
	// surfaces. Optional: nil leaves those routes answering 501, matching
	// every other optional-surface convention here.
	RBAC rbacsvc.Manager
}

// normalizeAPIDeps closes the Presence/DeviceLive duplication trap structurally.
// Liveness enters APIDeps twice — Presence drives the heartbeat middleware that
// touches it, DeviceLive is what the device roster reads — and nothing enforces
// they stay the same tracker. If a future edit set Presence but left DeviceLive
// nil (or re-split them), the roster would silently and permanently report
// every device offline: no error, no log, no test failure short of a live
// phone. Defaulting DeviceLive to Presence here, at the one place APIDeps is
// consumed to build the API, makes that trap unreachable rather than merely
// currently avoided by careful call-site wiring.
//
// A nil Presence on its own is not an error: the roster must keep listing and
// managing devices with every device simply reporting offline (see
// MobileDevicesController.List's own nil-Presence fallback) — that decision
// stands. What IS a real mis-wiring is a live DeviceRoster with no liveness
// source at all after the fallback above; that gets exactly one startup
// warning, because a silent-forever-offline roster is precisely what a
// startup log is for.
func normalizeAPIDeps(deps APIDeps, log *slog.Logger) APIDeps {
	if deps.Log == nil {
		deps.Log = log
	}
	if deps.DeviceLive == nil && deps.Presence != nil {
		deps.DeviceLive = deps.Presence
	}
	if deps.DeviceRoster != nil && deps.DeviceLive == nil {
		log.Warn("mobile device roster has no liveness tracker wired; every device will report offline")
	}
	return deps
}

// API owns one controller per resource and is the single Register call the
// router invokes to mount the /api/v1 surface.
type API struct {
	cfg                config.Config
	deps               APIDeps
	agents             *controllers.AgentsController
	projects           *controllers.ProjectsController
	environment        *controllers.EnvironmentController
	sessions           *controllers.SessionsController
	usage              *controllers.UsageController
	usageSubject       *controllers.UsageSubjectController
	capacity           *controllers.CapacityController
	prs                *controllers.PRsController
	reviews            *controllers.ReviewsController
	decisions          *controllers.DecisionsController
	notifications      *controllers.NotificationsController
	push               *controllers.PushController
	imports            *controllers.ImportController
	shellTerms         *controllers.ShellTerminalsController
	conversations      *controllers.ConversationsController
	settings           *controllers.SettingsController
	dev                *controllers.DevController
	browser            *controllers.BrowserController
	workflows          *controllers.WorkflowsController
	scheduler          *controllers.SchedulerController
	projectMemory      *controllers.ProjectMemoryController
	projectMemoryGraph *controllers.ProjectMemoryGraphController
	projectIntel       *controllers.ProjectIntelligenceController
	events             *EventsController
	auth               *controllers.AuthController
	sso                *controllers.SSOController
	providerProfiles   *controllers.ProviderProfilesController
	executionPolicy    *controllers.ExecutionPolicyController
	users              *controllers.UsersController
	teams              *controllers.TeamsController
	tenants            *controllers.TenantsController
	projectAccess      *controllers.ProjectAccessController
	// guard is P4-B's authorization gate, built once and shared by every
	// controller that scopes a resource. One value, so a route can never be
	// gated by a differently-configured evaluator than its neighbour.
	guard controllers.Guard
}

// NewAPI constructs the API surface from its dependencies. cfg carries the
// per-request timeout so the REST group can apply it without re-reading the
// environment.
func NewAPI(cfg config.Config, deps APIDeps) *API {
	guard := controllers.Guard{Authz: deps.Authz, Scope: deps.ProjectScope}
	return &API{
		guard: guard,
		cfg:   cfg,
		deps:  deps,
		agents: &controllers.AgentsController{
			Catalog: deps.Agents,
		},
		projects: &controllers.ProjectsController{
			Mgr:          deps.Projects,
			Ownership:    deps.ProjectOwnership,
			TrustedLocal: cfg.TrustedLocalMode,
			Guard:        guard,
			Tenancy:      deps.ProjectTenancy,
		},
		environment: &controllers.EnvironmentController{
			Svc: deps.Environment,
		},
		sessions: &controllers.SessionsController{
			Svc:           deps.Sessions,
			Activity:      deps.Activity,
			Usage:         deps.UsageHooks,
			PreviewServer: deps.PreviewServer,
			Capabilities:  deps.SessionCapabilities,
			Ownership:     deps.SessionOwnership,
			TrustedLocal:  cfg.TrustedLocalMode,
			Guard:         guard,
		},
		// P3-E: the loopback callback a reviewer or decision-resolver pane uses
		// to report its OWN token spend. Wired to the same collector the session
		// hook uses, through a strictly narrower entry point.
		usageSubject: &controllers.UsageSubjectController{Usage: deps.UsageSubjectHooks},
		usage: &controllers.UsageController{
			Svc:          deps.UsageSummary,
			Ownership:    deps.SessionOwnership,
			TrustedLocal: cfg.TrustedLocalMode,
			Guard:        guard,
		},
		capacity:           &controllers.CapacityController{Svc: deps.Capacity},
		projectMemory:      &controllers.ProjectMemoryController{Svc: deps.ProjectMemory, Guard: guard},
		projectMemoryGraph: &controllers.ProjectMemoryGraphController{Svc: deps.ProjectMemoryGraph, Guard: guard},
		projectIntel: &controllers.ProjectIntelligenceController{
			Svc: deps.ProjectIntelligence, Sync: deps.ProjectMemoryGraph, Guard: guard,
		},
		prs: &controllers.PRsController{Svc: deps.PRs},
		reviews: &controllers.ReviewsController{
			Svc:          deps.Reviews,
			Ownership:    deps.SessionOwnership,
			TrustedLocal: cfg.TrustedLocalMode,
			Guard:        guard,
		},
		decisions:     &controllers.DecisionsController{Svc: deps.Decisions},
		notifications: &controllers.NotificationsController{Svc: deps.Notifications, Stream: deps.NotificationStream, Guard: guard},
		push:          &controllers.PushController{Registry: deps.Push},
		imports:       &controllers.ImportController{Svc: deps.Import},
		shellTerms:    &controllers.ShellTerminalsController{Svc: deps.ShellTerminals},
		conversations: &controllers.ConversationsController{
			Svc:          deps.Conversations,
			Ownership:    deps.SessionOwnership,
			TrustedLocal: cfg.TrustedLocalMode,
			Guard:        guard,
		},
		settings: &controllers.SettingsController{Svc: deps.Settings, MemoryMode: deps.MemoryMode},
		dev:      &controllers.DevController{Import: deps.DevImport},
		browser:  &controllers.BrowserController{Svc: deps.Browser},
		workflows: &controllers.WorkflowsController{
			Svc:              deps.Workflows,
			UsageReader:      deps.UsageSummary,
			UsageLedger:      deps.UsageLedger,
			UsageContext:     deps.UsageContext,
			QuestionsReader:  deps.Questions,
			Ownership:        deps.WorkflowOwnership,
			TrustedLocal:     cfg.TrustedLocalMode,
			Guard:            guard,
			ProviderProfiles: deps.ProviderProfiles,
		},
		scheduler: &controllers.SchedulerController{Scheduler: deps.Scheduler, GC: deps.RuntimeGC},
		events:    &EventsController{Source: deps.CDC, Live: deps.Events, Guard: guard},
		auth: &controllers.AuthController{
			Mgr:          deps.Auth,
			TrustedLocal: cfg.TrustedLocalMode,
			SSO:          deps.SSO,
			Audit:        controllers.AuthAudit{Log: deps.Log, Sink: deps.Telemetry},
			CookiePolicy: controllers.SessionCookiePolicy{CrossSite: cfg.SessionCookieCrossSite},
			Authz:        deps.Authz,
		},
		sso: &controllers.SSOController{
			Mgr:          deps.SSO,
			Mode:         cfg.AuthMode,
			Audit:        controllers.AuthAudit{Log: deps.Log, Sink: deps.Telemetry},
			CookiePolicy: controllers.SessionCookiePolicy{CrossSite: cfg.SessionCookieCrossSite},
		},
		providerProfiles: &controllers.ProviderProfilesController{Mgr: deps.ProviderProfiles, Setup: deps.ProviderSetup},
		executionPolicy:  &controllers.ExecutionPolicyController{Mgr: deps.ExecutionPolicy},
		users:            &controllers.UsersController{Mgr: deps.RBAC},
		teams:            &controllers.TeamsController{Mgr: deps.RBAC},
		tenants:          &controllers.TenantsController{Mgr: deps.RBAC, Guard: guard},
		projectAccess:    &controllers.ProjectAccessController{Mgr: deps.RBAC, Guard: guard},
	}
}

// Register mounts the bounded /api/v1 REST surface. Long-lived surfaces such
// as muxed terminal streams stay outside this timeout group.
func (a *API) Register(root chi.Router) {
	timeout := a.cfg.RequestTimeout
	if timeout <= 0 {
		timeout = config.DefaultRequestTimeout
	}
	switchAgentTimeout := timeout
	if switchAgentTimeout < minimumSwitchAgentRequestTimeout {
		switchAgentTimeout = minimumSwitchAgentRequestTimeout
	}

	root.Route("/api/v1", func(r chi.Router) {
		// Serve the OpenAPI document from the same origin as the routes it describes.
		r.Get("/openapi.yaml", apispec.ServeYAML)

		r.Group(func(r chi.Router) {
			r.Use(middleware.Timeout(timeout))
			r.Use(presenceMiddleware(a.deps.Presence))
			// P4-B: the installation-wide permission gate. It sits ahead of
			// every REST controller so a settings, provider, user or team
			// route is decided in one place for every transport -- browser,
			// desktop, CLI and LAN bridge alike.
			r.Use(controllers.GlobalAuthzMiddleware(a.guard))
			a.agents.Register(r)
			a.projects.Register(r)
			a.environment.Register(r)
			a.sessions.Register(r)
			a.usage.Register(r)
			a.usageSubject.Register(r)
			a.capacity.Register(r)
			a.projectMemory.Register(r)
			a.projectMemoryGraph.Register(r)
			a.projectIntel.Register(r)
			a.prs.Register(r)
			a.reviews.Register(r)
			a.decisions.Register(r)
			a.notifications.Register(r)
			a.push.Register(r)
			a.imports.Register(r)
			a.shellTerms.Register(r)
			a.conversations.Register(r)
			a.settings.Register(r)
			a.dev.Register(r)
			a.browser.Register(r)
			a.workflows.Register(r)
			a.scheduler.Register(r)
			a.auth.Register(r)
			a.sso.Register(r)
			a.providerProfiles.Register(r)
			a.executionPolicy.Register(r)
			a.users.Register(r)
			a.teams.Register(r)
			a.tenants.Register(r)
			a.projectAccess.Register(r)
			// Sibling REST controllers plug in here.
		})
		// Agent switching synchronously collects a handoff, starts the target,
		// waits for provider readiness, and confirms delivery. Give that bounded
		// workflow enough time to complete without extending every REST route.
		r.Group(func(r chi.Router) {
			r.Use(middleware.Timeout(switchAgentTimeout))
			a.sessions.RegisterSwitchAgent(r)
		})
		// Long-lived streams intentionally bypass the REST timeout middleware.
		a.notifications.RegisterStream(r)
		a.sessions.RegisterStreams(r)
		a.events.Register(r)
	})
}

// notFoundJSON returns the locked envelope for unmatched routes. Chi's default
// 404 is a text/plain body; the API surface must answer JSON so consumers can
// parse it uniformly.
func notFoundJSON(w http.ResponseWriter, r *http.Request) {
	envelope.WriteAPIError(w, r, http.StatusNotFound, "not_found", "ROUTE_NOT_FOUND",
		r.Method+" "+r.URL.Path+" has no handler", nil)
}

// methodNotAllowedJSON returns the locked envelope when a method probes a
// known path without a matching verb (e.g. PUT /projects/{id} after we drop
// the legacy PUT alias).
func methodNotAllowedJSON(w http.ResponseWriter, r *http.Request) {
	envelope.WriteAPIError(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "METHOD_NOT_ALLOWED",
		r.Method+" not allowed on "+r.URL.Path, nil)
}
