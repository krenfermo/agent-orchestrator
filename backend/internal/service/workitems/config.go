// Package workitems is the service layer for AO's external work-management
// integration (P4-E).
//
// AO REMAINS CANONICAL, and this package is where that is enforced rather than
// merely intended. Three properties hold at every entry point:
//
//   - Nothing here is called from the workflow engine, the lifecycle manager,
//     or the reducer that decides a run's state. The only inbound path from
//     execution is Enqueue, which writes a durable row and returns; it cannot
//     fail a caller and it performs no network I/O.
//   - No function in this package produces an AO state. External state is
//     read, cached for display, and reported as advisory readiness. There is
//     no code path by which a planning board changes what AO is doing.
//   - Every provider call is bounded, classified, and safe to have failed. A
//     Plane outage produces deferred outbox rows and a degraded badge, and
//     changes nothing else.
package workitems

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/workitems/plane"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// Environment variables that supply installation-wide DEFAULTS.
//
// They are defaults, not overrides: a project's stored configuration always
// wins. The reason is that config is per-project by necessity (two projects
// legitimately map to two Plane projects, often in two workspaces), while an
// operator provisioning a fleet wants to state the origin and the token once.
//
// AO_PLANE_API_TOKEN is read at the moment it is needed and is never written
// to the database, so an installation that prefers to keep its credential in
// the process environment can — and the database then holds no secret at all.
const (
	// EnvBaseURL is the Plane origin (no /api/v1 suffix). Empty means Plane
	// Cloud.
	EnvBaseURL = "AO_PLANE_BASE_URL"
	// EnvAPIToken names the variable a fallback API token is read from. The
	// constant is the variable's NAME, not a credential.
	EnvAPIToken = "AO_PLANE_API_TOKEN" //nolint:gosec // an env var name, not a secret
	// EnvWorkspace is a default workspace slug.
	EnvWorkspace = "AO_PLANE_WORKSPACE"
	// EnvProject is a default Plane project id, for the single-project
	// installation where naming it per project would be ceremony.
	EnvProject = "AO_PLANE_PROJECT"
)

// Store is the durable surface this service needs. Declared here, at the
// consumer, following the same local-narrow-interface pattern the rest of the
// tree uses.
type Store interface {
	GetWorkItemConfig(ctx context.Context, projectID domain.ProjectID) (store.WorkItemConfig, bool, error)
	ListEnabledWorkItemConfigs(ctx context.Context) ([]store.WorkItemConfig, error)
	PutWorkItemConfig(ctx context.Context, cfg store.WorkItemConfig, now time.Time) error
	SetWorkItemConfigCheck(ctx context.Context, projectID domain.ProjectID, ok bool, detail string, now time.Time) error
	DeleteWorkItemConfig(ctx context.Context, projectID domain.ProjectID) (int64, error)

	GetWorkItemLink(ctx context.Context, id string) (domain.WorkItemLink, bool, error)
	GetWorkItemLinkByScope(ctx context.Context, projectID domain.ProjectID, scope domain.WorkItemLinkScope, scopeID string) (domain.WorkItemLink, bool, error)
	ListWorkItemLinks(ctx context.Context, projectID domain.ProjectID) ([]domain.WorkItemLink, error)
	PutWorkItemLink(ctx context.Context, link domain.WorkItemLink, now time.Time) error
	TouchWorkItemLinkSnapshot(ctx context.Context, id, title string, state domain.WorkItemStateGroup, now time.Time) error
	SetWorkItemLinkSync(ctx context.Context, id string, enabled bool, now time.Time) (int64, error)
	DeleteWorkItemLink(ctx context.Context, projectID domain.ProjectID, id string) (int64, error)

	EnqueueWorkItemSync(ctx context.Context, row store.WorkItemSyncRow, now time.Time) (bool, error)
	ClaimDueWorkItemSyncs(ctx context.Context, now time.Time, limit int) ([]store.WorkItemSyncRow, error)
	MarkWorkItemSyncDone(ctx context.Context, id string, now time.Time) (int64, error)
	DeferWorkItemSync(ctx context.Context, id, reason string, next, now time.Time) (int64, error)
	MarkWorkItemSyncFailed(ctx context.Context, id, reason string, now time.Time) (int64, error)
	WorkItemSyncCounts(ctx context.Context, projectID domain.ProjectID) (map[string]int64, error)
	ListWorkItemSyncs(ctx context.Context, projectID domain.ProjectID, limit int) ([]store.WorkItemSyncRow, error)

	RecordWorkItemAudit(ctx context.Context, row store.WorkItemAuditRow, now time.Time) error
	ListWorkItemAudit(ctx context.Context, projectID domain.ProjectID, limit int) ([]store.WorkItemAuditRow, error)
}

// SecretBox seals and opens the stored API token. The service holds the
// interface rather than the concrete box so a test can run without a key file.
type SecretBox interface {
	Seal(plaintext string) (string, error)
	Open(ciphertext string) (string, error)
}

// ProviderFactory builds a client for one resolved configuration.
//
// It is injected so tests can substitute a fake provider without an HTTP
// server, and so a second provider can be added later without this package
// learning how to construct it.
type ProviderFactory func(cfg ResolvedConfig) (ports.WorkItems, error)

// Deps is the service's collaborators.
type Deps struct {
	Store   Store
	Secrets SecretBox
	// Provider builds clients. Nil installs the default Plane factory.
	Provider ProviderFactory
	// Env reads an environment variable. Nil uses os.Getenv; tests inject.
	Env    func(string) string
	Now    func() time.Time
	Logger *slog.Logger
	// NewID mints ids for links, outbox rows and audit rows.
	NewID func() string
}

// Service is the operational face of the integration.
type Service struct {
	store    Store
	secrets  SecretBox
	provider ProviderFactory
	env      func(string) string
	now      func() time.Time
	log      *slog.Logger
	newID    func() string
	// notifier is optional: an installation without notifications still syncs,
	// it simply cannot announce a permanent failure. See WithNotifier.
	notifier Notifier
}

// New builds the service. Every dependency has a working default except the
// store and the id source, so a caller that only has those gets something
// usable.
func New(d Deps) *Service {
	s := &Service{
		store: d.Store, secrets: d.Secrets, provider: d.Provider,
		env: d.Env, now: d.Now, log: d.Logger, newID: d.NewID,
	}
	if s.provider == nil {
		s.provider = defaultProviderFactory
	}
	if s.env == nil {
		s.env = osGetenv
	}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	return s
}

// ResolvedConfig is a project's configuration with environment defaults
// applied and the credential decrypted — the shape a provider client is built
// from.
//
// It is a distinct type from store.WorkItemConfig precisely because it holds
// PLAINTEXT. Keeping the two apart means a function that takes a
// store.WorkItemConfig demonstrably cannot leak a token, and the number of
// places a plaintext credential exists is small enough to read.
type ResolvedConfig struct {
	ProjectID domain.ProjectID
	Provider  domain.WorkItemProvider
	BaseURL   string
	Workspace string
	ProjectRef

	// APIToken is plaintext, decrypted or read from the environment. It is
	// never rendered, never logged, and never returned through the API.
	APIToken string

	Enabled      bool
	SyncStates   bool
	SyncComments bool
}

// ProjectRef is the provider-side project a configuration points at.
type ProjectRef struct {
	ExternalProjectID   string
	ExternalProjectName string
	ExternalProjectKey  string
}

// Usable reports whether this configuration can actually reach a provider.
func (c ResolvedConfig) Usable() bool {
	return c.Enabled && c.Workspace != "" && c.ExternalProjectID != "" && c.APIToken != ""
}

// ErrNotConfigured is the sentinel every "there is no integration here" answer
// wraps. It is a normal, expected state — the DEFAULT state — so callers
// branch on it rather than treating it as a failure.
var ErrNotConfigured = errors.New("workitems: no work-management provider is configured for this project")

// Resolve reads a project's configuration and applies environment defaults.
//
// The precedence is: stored value, then environment default, then nothing. A
// stored value always wins because it is the one somebody chose for THIS
// project; an environment variable is what an operator set for the fleet.
func (s *Service) Resolve(ctx context.Context, projectID domain.ProjectID) (ResolvedConfig, error) {
	stored, found, err := s.store.GetWorkItemConfig(ctx, projectID)
	if err != nil {
		return ResolvedConfig{}, err
	}
	if !found {
		// A project with no row still resolves, from the environment alone.
		// That is what makes a single-workspace installation configurable
		// entirely by env, which §3 asks for.
		// Only the fields read below are set: ProjectID travels on the
		// ResolvedConfig from the parameter, not from this placeholder row.
		stored = store.WorkItemConfig{Provider: domain.WorkItemProviderPlane}
	}

	cfg := ResolvedConfig{
		ProjectID: projectID,
		Provider:  stored.Provider,
		BaseURL:   firstNonEmpty(stored.BaseURL, s.env(EnvBaseURL)),
		Workspace: firstNonEmpty(stored.Workspace, s.env(EnvWorkspace)),
		ProjectRef: ProjectRef{
			ExternalProjectID:   firstNonEmpty(stored.ExternalProjectID, s.env(EnvProject)),
			ExternalProjectName: stored.ExternalProjectName,
			ExternalProjectKey:  stored.ExternalProjectKey,
		},
		Enabled:      stored.Enabled,
		SyncStates:   stored.SyncStates,
		SyncComments: stored.SyncComments,
	}
	if cfg.Provider == "" {
		cfg.Provider = domain.WorkItemProviderPlane
	}
	if !found {
		// With no row there is nothing to have enabled. An env-only
		// installation still has to switch it on per project, because
		// "AO writes to your planning board" is not a thing an environment
		// variable should decide on somebody's behalf.
		cfg.Enabled = false
		cfg.SyncStates, cfg.SyncComments = true, true
	}

	if stored.APITokenEncrypted != "" {
		if s.secrets == nil {
			return ResolvedConfig{}, errors.New("workitems: a secret box is required to read the stored API token")
		}
		token, openErr := s.secrets.Open(stored.APITokenEncrypted)
		if openErr != nil {
			// A token that will not decrypt is a broken configuration, not a
			// reason to fall back to the environment: silently using a
			// different credential than the one somebody stored is worse than
			// reporting that the stored one is unreadable.
			return ResolvedConfig{}, fmt.Errorf("workitems: the stored API token could not be decrypted: %w", openErr)
		}
		cfg.APIToken = token
	} else {
		cfg.APIToken = strings.TrimSpace(s.env(EnvAPIToken))
	}
	return cfg, nil
}

// client builds a provider client for a project, or reports that there is
// none. Every caller that talks to a provider goes through here, so the
// "not configured" answer is produced in exactly one place.
func (s *Service) client(ctx context.Context, projectID domain.ProjectID) (ports.WorkItems, ResolvedConfig, error) {
	cfg, err := s.Resolve(ctx, projectID)
	if err != nil {
		return nil, ResolvedConfig{}, err
	}
	if !cfg.Usable() {
		return nil, cfg, ErrNotConfigured
	}
	c, err := s.provider(cfg)
	if err != nil {
		return nil, cfg, err
	}
	return c, cfg, nil
}

func defaultProviderFactory(cfg ResolvedConfig) (ports.WorkItems, error) {
	if cfg.Provider != domain.WorkItemProviderPlane {
		return nil, &ports.WorkItemsError{Op: "provider", Kind: ports.WorkItemsErrNotConfigured,
			Message: "unsupported work-management provider " + string(cfg.Provider)}
	}
	return plane.New(plane.Options{
		BaseURL:   cfg.BaseURL,
		Workspace: cfg.Workspace,
		Token:     plane.StaticToken(cfg.APIToken),
	})
}

// ConfigUpdate is a settings write. Pointer fields distinguish "leave this
// alone" from "set it to empty", which matters most for the token: a form that
// re-submits without the password field must not erase the stored credential.
type ConfigUpdate struct {
	BaseURL   *string
	Workspace *string
	// ExternalProjectID names the provider-side project. Setting it also
	// refreshes the cached name and key, which the service looks up.
	ExternalProjectID *string
	// APIToken nil leaves the stored token; a pointer to "" clears it.
	APIToken     *string
	Enabled      *bool
	SyncStates   *bool
	SyncComments *bool
}

// PutConfig writes a project's configuration.
//
// A configuration is savable whether or not it is complete: somebody half-way
// through a form must be able to keep what they have typed. Only ENABLING is
// gated on completeness, which is the same rule the email settings follow, and
// for the same reason — an incomplete configuration that is off is a draft, and
// an incomplete configuration that is on is a promise AO cannot keep.
func (s *Service) PutConfig(ctx context.Context, projectID domain.ProjectID, update ConfigUpdate) (ConfigView, error) {
	current, found, err := s.store.GetWorkItemConfig(ctx, projectID)
	if err != nil {
		return ConfigView{}, err
	}
	if !found {
		current = store.WorkItemConfig{
			ProjectID: projectID, Provider: domain.WorkItemProviderPlane,
			SyncStates: true, SyncComments: true,
		}
	}

	if update.BaseURL != nil {
		normalized, nErr := plane.NormalizeBaseURL(*update.BaseURL)
		if nErr != nil {
			return ConfigView{}, apierr.Invalid("PLANE_BASE_URL_INVALID", nErr.Error(), nil)
		}
		// An operator who cleared the field means "use the provider default",
		// which is stored as empty rather than as the default's literal value:
		// pinning the default would silently freeze it if it ever changed.
		if strings.TrimSpace(*update.BaseURL) == "" {
			current.BaseURL = ""
		} else {
			current.BaseURL = normalized
		}
	}
	if update.Workspace != nil {
		current.Workspace = strings.Trim(strings.TrimSpace(*update.Workspace), "/")
	}
	if update.ExternalProjectID != nil {
		next := strings.TrimSpace(*update.ExternalProjectID)
		if next != current.ExternalProjectID {
			// The cached display fields describe the OLD project and would be
			// a lie about the new one until a lookup refreshes them.
			current.ExternalProjectName, current.ExternalProjectKey = "", ""
		}
		current.ExternalProjectID = next
	}
	if update.APIToken != nil {
		token := strings.TrimSpace(*update.APIToken)
		switch {
		case token == "":
			current.APITokenEncrypted = ""
		case s.secrets == nil:
			return ConfigView{}, errors.New("workitems: a secret box is required to store an API token")
		default:
			sealed, sErr := s.secrets.Seal(token)
			if sErr != nil {
				return ConfigView{}, fmt.Errorf("workitems: seal API token: %w", sErr)
			}
			current.APITokenEncrypted = sealed
		}
	}
	if update.SyncStates != nil {
		current.SyncStates = *update.SyncStates
	}
	if update.SyncComments != nil {
		current.SyncComments = *update.SyncComments
	}
	if update.Enabled != nil {
		current.Enabled = *update.Enabled
	}

	if current.Enabled {
		// Completeness is judged against the RESOLVED configuration, so an
		// installation whose token and workspace come from the environment can
		// enable a project without re-typing them.
		probe := ResolvedConfig{
			Workspace: firstNonEmpty(current.Workspace, s.env(EnvWorkspace)),
			ProjectRef: ProjectRef{
				ExternalProjectID: firstNonEmpty(current.ExternalProjectID, s.env(EnvProject)),
			},
			APIToken: firstNonEmpty(current.APITokenEncrypted, strings.TrimSpace(s.env(EnvAPIToken))),
			Enabled:  true,
		}
		if !probe.Usable() {
			return ConfigView{}, apierr.Invalid("PLANE_CONFIG_INCOMPLETE",
				"a workspace, a project and an API token are all required before the integration can be switched on", nil)
		}
	}

	// Enriching the cached project name is best-effort and deliberately after
	// validation: a provider that is unreachable must not stop somebody
	// saving a configuration, which is the §13 rule applied to the settings
	// path.
	s.enrichProjectName(ctx, &current)

	if err := s.store.PutWorkItemConfig(ctx, current, s.now()); err != nil {
		return ConfigView{}, err
	}
	return s.viewOf(ctx, current), nil
}

// enrichProjectName fills the cached display name for the mapped project.
func (s *Service) enrichProjectName(ctx context.Context, cfg *store.WorkItemConfig) {
	if cfg.ExternalProjectID == "" || cfg.ExternalProjectName != "" {
		return
	}
	resolved, err := s.Resolve(ctx, cfg.ProjectID)
	if err != nil || resolved.Workspace == "" || resolved.APIToken == "" {
		return
	}
	// Resolve() reads the STORED row, which does not yet hold this update, so
	// the pending values are applied before the lookup.
	resolved.ExternalProjectID = cfg.ExternalProjectID
	if cfg.Workspace != "" {
		resolved.Workspace = cfg.Workspace
	}
	if cfg.BaseURL != "" {
		resolved.BaseURL = cfg.BaseURL
	}
	client, err := s.provider(resolved)
	if err != nil {
		return
	}
	lookupCtx, cancel := context.WithTimeout(ctx, plane.DefaultTimeout)
	defer cancel()
	projects, err := client.ListProjects(lookupCtx)
	if err != nil {
		return
	}
	for _, p := range projects {
		if p.ID == cfg.ExternalProjectID {
			cfg.ExternalProjectName, cfg.ExternalProjectKey = p.Name, p.Identifier
			return
		}
	}
}

// DeleteConfig removes a project's configuration and its stored credential.
func (s *Service) DeleteConfig(ctx context.Context, projectID domain.ProjectID) error {
	_, err := s.store.DeleteWorkItemConfig(ctx, projectID)
	return err
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if t := strings.TrimSpace(v); t != "" {
			return t
		}
	}
	return ""
}
