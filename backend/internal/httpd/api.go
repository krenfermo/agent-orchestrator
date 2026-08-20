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
	reviewsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/review"
	workflowsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/workflow"
)

// The synchronous switch budget covers one 60s optional source handoff, a
// separate 2m human permission-decision window, target process/readiness checks,
// composer confirmation retries, and the final 150s generation acknowledgement,
// with headroom for teardown and durable writes.
const minimumSwitchAgentRequestTimeout = 6 * time.Minute

// APIDeps bundles every service the API layer's controllers depend on.
type APIDeps struct {
	Agents       controllers.AgentCatalog
	Projects     projectsvc.Manager
	Environment  controllers.EnvironmentStatusProvider
	Sessions     controllers.SessionService
	Activity     controllers.ActivityRecorder
	UsageHooks   controllers.UsageHookRecorder
	UsageSummary controllers.UsageSummaryService
	// Capacity backs Checkpoint 8J's read-only capacity/quota view. Optional
	// like the other 8H/8J surfaces; nil answers 501.
	Capacity controllers.CapacityService
	PRs      prsvc.ActionManager
	Reviews  reviewsvc.Manager
	// Workflows is nil until wired; the controller then answers 501, matching
	// the other optional surfaces. Checkpoint 8A: durable foundation only, no
	// execution.
	Workflows workflowsvc.Manager
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
	// ProjectOwnership/WorkflowOwnership back Checkpoint 8P-A's minimal
	// ownership scoping on the projects/workflows controllers. Optional:
	// nil disables scoping entirely (pre-8P-A behavior). Both are satisfied
	// by the same *sqlite/store.Store the rest of the daemon already uses.
	ProjectOwnership  controllers.OwnershipStore
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
	cfg              config.Config
	deps             APIDeps
	agents           *controllers.AgentsController
	projects         *controllers.ProjectsController
	environment      *controllers.EnvironmentController
	sessions         *controllers.SessionsController
	usage            *controllers.UsageController
	capacity         *controllers.CapacityController
	prs              *controllers.PRsController
	reviews          *controllers.ReviewsController
	decisions        *controllers.DecisionsController
	notifications    *controllers.NotificationsController
	push             *controllers.PushController
	imports          *controllers.ImportController
	shellTerms       *controllers.ShellTerminalsController
	conversations    *controllers.ConversationsController
	settings         *controllers.SettingsController
	dev              *controllers.DevController
	browser          *controllers.BrowserController
	workflows        *controllers.WorkflowsController
	events           *EventsController
	auth             *controllers.AuthController
	providerProfiles *controllers.ProviderProfilesController
	executionPolicy  *controllers.ExecutionPolicyController
}

// NewAPI constructs the API surface from its dependencies. cfg carries the
// per-request timeout so the REST group can apply it without re-reading the
// environment.
func NewAPI(cfg config.Config, deps APIDeps) *API {
	return &API{
		cfg:  cfg,
		deps: deps,
		agents: &controllers.AgentsController{
			Catalog: deps.Agents,
		},
		projects: &controllers.ProjectsController{
			Mgr:          deps.Projects,
			Ownership:    deps.ProjectOwnership,
			TrustedLocal: cfg.TrustedLocalMode,
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
		},
		usage: &controllers.UsageController{
			Svc:          deps.UsageSummary,
			Ownership:    deps.SessionOwnership,
			TrustedLocal: cfg.TrustedLocalMode,
		},
		capacity: &controllers.CapacityController{Svc: deps.Capacity},
		prs:      &controllers.PRsController{Svc: deps.PRs},
		reviews: &controllers.ReviewsController{
			Svc:          deps.Reviews,
			Ownership:    deps.SessionOwnership,
			TrustedLocal: cfg.TrustedLocalMode,
		},
		decisions:     &controllers.DecisionsController{Svc: deps.Decisions},
		notifications: &controllers.NotificationsController{Svc: deps.Notifications, Stream: deps.NotificationStream},
		push:          &controllers.PushController{Registry: deps.Push},
		imports:       &controllers.ImportController{Svc: deps.Import},
		shellTerms:    &controllers.ShellTerminalsController{Svc: deps.ShellTerminals},
		conversations: &controllers.ConversationsController{
			Svc:          deps.Conversations,
			Ownership:    deps.SessionOwnership,
			TrustedLocal: cfg.TrustedLocalMode,
		},
		settings: &controllers.SettingsController{Svc: deps.Settings},
		dev:      &controllers.DevController{Import: deps.DevImport},
		browser:  &controllers.BrowserController{Svc: deps.Browser},
		workflows: &controllers.WorkflowsController{
			Svc:              deps.Workflows,
			UsageReader:      deps.UsageSummary,
			QuestionsReader:  deps.Questions,
			Ownership:        deps.WorkflowOwnership,
			TrustedLocal:     cfg.TrustedLocalMode,
			ProviderProfiles: deps.ProviderProfiles,
		},
		events:           &EventsController{Source: deps.CDC, Live: deps.Events},
		auth:             &controllers.AuthController{Mgr: deps.Auth, TrustedLocal: cfg.TrustedLocalMode},
		providerProfiles: &controllers.ProviderProfilesController{Mgr: deps.ProviderProfiles, Setup: deps.ProviderSetup},
		executionPolicy:  &controllers.ExecutionPolicyController{Mgr: deps.ExecutionPolicy},
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
			a.agents.Register(r)
			a.projects.Register(r)
			a.environment.Register(r)
			a.sessions.Register(r)
			a.usage.Register(r)
			a.capacity.Register(r)
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
			a.auth.Register(r)
			a.providerProfiles.Register(r)
			a.executionPolicy.Register(r)
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
