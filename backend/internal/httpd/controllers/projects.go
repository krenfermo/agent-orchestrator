// Package controllers holds the HTTP-facing controllers for the /api/v1
// surface. Each controller groups one resource's routes, exposes a Register
// method, and depends on exactly one resource-level Manager interface — never
// directly on stores, lifecycle reducers, or adapters.
package controllers

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/identity"
	projectsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/project"
)

// OwnershipStore backs Checkpoint 8P-A's minimal project/workflow-run
// ownership scoping. It is intentionally narrow (read/write/list the owner
// column only) so it can sit on top of the existing projectsvc/workflowsvc
// managers without touching their domain records or interfaces. Optional:
// nil disables ownership stamping/scoping entirely, which is what every
// pre-8P-A test configuration (no Ownership wired) keeps behaving like.
type OwnershipStore interface {
	GetProjectOwner(ctx context.Context, id domain.ProjectID) (*domain.UserID, error)
	SetProjectOwner(ctx context.Context, id domain.ProjectID, owner domain.UserID) (bool, error)
}

// TenantPlacement is P4-C's project-tenancy surface. Deliberately as narrow as
// OwnershipStore and wired the same way: nil keeps every pre-P4-C
// configuration behaving exactly as it did, with projects landing in the
// default organization by the column default.
type TenantPlacement interface {
	SetProjectTenant(ctx context.Context, id domain.ProjectID, tenant domain.TenantID) (bool, error)
}

// ProjectsController owns the /projects routes. The controller depends only on
// projectsvc.Manager; nil keeps routes registered but returns OpenAPI-backed 501s.
type ProjectsController struct {
	Mgr projectsvc.Manager
	// Ownership backs Checkpoint 8P-A's ownership scoping. Nil preserves
	// pre-8P-A unscoped behavior exactly.
	Ownership OwnershipStore
	// TrustedLocal mirrors config.Config.TrustedLocalMode. Scoping is only
	// ever enforced when this is false — see list/get below — so today's
	// single-user desktop flow (TrustedLocal default true) is completely
	// unaffected by Ownership being wired.
	TrustedLocal bool
	// Guard is P4-B's authorization gate. When wired it REPLACES the 8P-A
	// owner-equality checks below: a project is reachable because the caller
	// holds a permission on it (as its owner, by a direct grant, or through a
	// team), not because their user id matches a column. A zero Guard leaves
	// the pre-P4-B behavior exactly as it was.
	Guard Guard
	// Tenancy places a newly registered project in an organization (P4-C).
	// Nil preserves pre-P4-C behavior exactly.
	Tenancy TenantPlacement
}

// ownerForbidden reports whether a resource's stored owner (nil means
// unowned — a pre-8P-A row, or one created while no user was resolved) is
// visible to the current request's user. Unowned resources stay visible to
// everyone in this checkpoint (no bulk-claim UX exists yet); only a
// resource with a DIFFERENT explicit owner is hidden. Cross-user access
// reports as 404, never 403, so existence never leaks.
func ownerForbidden(owner *domain.UserID, current domain.UserID) bool {
	return owner != nil && *owner != current
}

// Register mounts the project routes on the supplied router.
func (c *ProjectsController) Register(r chi.Router) {
	r.Get("/projects", c.list)
	r.Post("/projects", c.add)
	r.Post("/projects/initialize", c.initialize)
	r.Get("/projects/browse", c.browse)
	r.Post("/projects/clone", c.clone)
	r.Get("/projects/{id}", c.get)
	r.Put("/projects/{id}", c.updateSettings)
	r.Put("/projects/{id}/config", c.setConfig)
	r.Delete("/projects/{id}", c.remove)
	r.Post("/projects/{id}/repo-connection-test", c.testRepoConnection)
	r.Post("/projects/{id}/workspace-repos/refresh", c.refreshWorkspaceRepos)
}

func (c *ProjectsController) testRepoConnection(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, "POST", "/api/v1/projects/{id}/repo-connection-test")
		return
	}
	id := projectID(r)
	if !c.authorize(w, r, domain.PermProjectRead, id) {
		return
	}
	var in TestRepoConnectionRequest
	if err := decodeJSONStrict(r, &in); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid JSON body", nil)
		return
	}
	result, err := c.Mgr.TestRepoConnection(r.Context(), id, in.Repo)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, TestRepoConnectionResponse{Result: result})
}

func (c *ProjectsController) refreshWorkspaceRepos(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, "POST", "/api/v1/projects/{id}/workspace-repos/refresh")
		return
	}
	id := projectID(r)
	if !c.authorize(w, r, domain.PermProjectRead, id) {
		return
	}
	p, err := c.Mgr.RefreshWorkspaceRepos(r.Context(), id)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, ProjectResponse{Project: p})
}

func (c *ProjectsController) list(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/projects")
		return
	}
	projects, err := c.Mgr.List(r.Context())
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	if projects == nil {
		projects = []projectsvc.Summary{}
	}
	if c.Guard.Enabled() {
		// One resolution for the whole list: the subject is computed once and
		// every row is then decided in memory, so a project list costs the
		// same three queries whether it returns two projects or two hundred.
		sub, ok := c.Guard.Subject(r)
		if !ok {
			envelope.WriteError(w, r, identity.Unauthorized())
			return
		}
		visible := make([]projectsvc.Summary, 0, len(projects))
		for _, p := range projects {
			if sub.CanSeeProject(p.ID) {
				visible = append(visible, p)
			}
		}
		projects = visible
	} else if c.scopingEnforced() {
		user, err := identity.Require(r)
		if err != nil {
			envelope.WriteError(w, r, err)
			return
		}
		projects = c.filterOwnedProjects(r.Context(), projects, user.ID)
	}
	envelope.WriteJSON(w, http.StatusOK, ListProjectsResponse{Projects: projects})
}

func (c *ProjectsController) add(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, "POST", "/api/v1/projects")
		return
	}
	var in projectsvc.AddInput
	if err := decodeJSONStrict(r, &in); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid JSON body", nil)
		return
	}
	c.registerProject(w, r, in.TenantID, func(ctx context.Context) (projectsvc.Project, error) {
		return c.Mgr.Add(ctx, in)
	})
}

// registerProject is the shared tail of every route that brings a NEW project
// into the installation. Each caller decodes its own body and calls its own
// manager method; everything after that is identical, and has to STAY
// identical -- a registration path that forgets to stamp ownership leaves a
// project nobody owns, and one that forgets to stamp tenancy leaves a project
// sitting in whichever organization the column default put it in. One place
// for both stamps is what keeps a third registration route from having to
// remember either.
//
// Note the ordering: the organization is resolved BEFORE the project is
// created, so an ambiguous or unreachable choice is refused while there is
// still nothing to clean up.
func (c *ProjectsController) registerProject(
	w http.ResponseWriter,
	r *http.Request,
	requestedTenant string,
	create func(ctx context.Context) (projectsvc.Project, error),
) {
	tenant, ok := c.resolveTenant(w, r, requestedTenant)
	if !ok {
		return
	}
	p, err := create(r.Context())
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	c.stampOwner(r, p.ID)
	c.stampTenant(r, p.ID, tenant)
	envelope.WriteJSON(w, http.StatusCreated, ProjectResponse{Project: p})
}

func (c *ProjectsController) initialize(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, "POST", "/api/v1/projects/initialize")
		return
	}
	var in projectsvc.InitializeRepositoryInput
	if err := decodeJSONStrict(r, &in); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid JSON body", nil)
		return
	}
	result, err := c.Mgr.InitializeRepository(r.Context(), in)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, result)
}
func (c *ProjectsController) browse(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/projects/browse")
		return
	}
	path := r.URL.Query().Get("path")
	result, err := c.Mgr.ListAllowedRootEntries(r.Context(), path)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	if result.Entries == nil {
		result.Entries = []projectsvc.BrowseEntry{}
	}
	envelope.WriteJSON(w, http.StatusOK, result)
}

func (c *ProjectsController) clone(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, "POST", "/api/v1/projects/clone")
		return
	}
	var in projectsvc.CloneInput
	if err := decodeJSONStrict(r, &in); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid JSON body", nil)
		return
	}
	c.registerProject(w, r, in.TenantID, func(ctx context.Context) (projectsvc.Project, error) {
		return c.Mgr.CloneFromGitHub(ctx, in)
	})
}

func (c *ProjectsController) get(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/projects/{id}")
		return
	}
	id := projectID(r)
	if !c.authorize(w, r, domain.PermProjectRead, id) {
		return
	}
	got, err := c.Mgr.Get(r.Context(), id)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	resp, err := newGetProjectResponse(got)
	if err != nil {
		envelope.WriteAPIError(w, r, http.StatusInternalServerError, "internal", "INTERNAL_ERROR", "Internal server error", nil)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, resp)
}

func (c *ProjectsController) updateSettings(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, "PUT", "/api/v1/projects/{id}")
		return
	}
	if !c.authorize(w, r, domain.PermProjectManage, projectID(r)) {
		return
	}
	var in projectsvc.UpdateSettingsInput
	if err := decodeJSONStrict(r, &in); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid JSON body", nil)
		return
	}
	p, err := c.Mgr.UpdateSettings(r.Context(), projectID(r), in)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, ProjectResponse{Project: p})
}

func (c *ProjectsController) setConfig(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, "PUT", "/api/v1/projects/{id}/config")
		return
	}
	if !c.authorize(w, r, domain.PermProjectManage, projectID(r)) {
		return
	}
	var in projectsvc.SetConfigInput
	if err := decodeJSONStrict(r, &in); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid JSON body", nil)
		return
	}
	p, err := c.Mgr.SetConfig(r.Context(), projectID(r), in)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, ProjectResponse{Project: p})
}

func (c *ProjectsController) remove(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, "DELETE", "/api/v1/projects/{id}")
		return
	}
	if !c.authorize(w, r, domain.PermProjectManage, projectID(r)) {
		return
	}
	result, err := c.Mgr.Remove(r.Context(), projectID(r))
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, result)
}

// scopingEnforced reports whether ownership filtering/masking should apply
// to this request. Deliberately false whenever TrustedLocal is on
// (preserves today's unscoped desktop behavior exactly, per Checkpoint
// 8P-A's spec — trusted-local mode does not enforce scoping) or Ownership
// isn't wired (headless/test configurations without ownership storage).
func (c *ProjectsController) scopingEnforced() bool {
	return !c.TrustedLocal && c.Ownership != nil
}

// authorize is this controller's single authorization gate. With P4-B's Guard
// wired it asks the canonical evaluator; without it, it falls back to 8P-A's
// owner-equality visibility check, so every configuration that predates
// authorization keeps its exact behavior.
//
// Both paths answer a denial with 404, never 403: a project id the caller
// cannot reach must be indistinguishable from one that does not exist.
func (c *ProjectsController) authorize(w http.ResponseWriter, r *http.Request, perm domain.Permission, id domain.ProjectID) bool {
	if c.Guard.Enabled() {
		return c.Guard.AllowProject(w, r, perm, id, "PROJECT_NOT_FOUND", "project not found")
	}
	if !c.scopingEnforced() {
		return true
	}
	user, err := identity.Require(r)
	if err != nil {
		envelope.WriteError(w, r, err)
		return false
	}
	if !c.projectVisible(r.Context(), id, user.ID) {
		envelope.WriteAPIError(w, r, http.StatusNotFound, "not_found", "PROJECT_NOT_FOUND", "project not found", nil)
		return false
	}
	return true
}

// stampOwner records the creating user as a new project's owner. Runs
// unconditionally (both trusted-local and multi-user mode) whenever a user
// resolved and Ownership is wired, so ownership data stays populated for a
// future slice even though trusted-local mode never reads it back to filter
// anything today. Best-effort: a stamping failure must never fail project
// creation, which already succeeded.
func (c *ProjectsController) stampOwner(r *http.Request, id domain.ProjectID) {
	if c.Ownership == nil {
		return
	}
	user, ok := identity.FromContext(r.Context())
	if !ok {
		return
	}
	_, _ = c.Ownership.SetProjectOwner(r.Context(), id, user.ID)
}

// resolveTenant decides which organization a newly registered project belongs
// to, BEFORE the project is created. Ordering matters: refusing an ambiguous
// request after the project already exists would leave it sitting in whichever
// organization the column default put it in, which is the outcome this whole
// function exists to prevent.
//
// The rules are Step 9's, and they are shaped so the common case never sees
// them. A caller who belongs to one organization gets that one and is never
// asked. A caller who belongs to several must say which -- silently picking
// for them would eventually put somebody's repository in front of the wrong
// company. A caller who names one must be able to reach it.
//
// ok is false when a response has already been written.
func (c *ProjectsController) resolveTenant(w http.ResponseWriter, r *http.Request, requested string) (domain.TenantID, bool) {
	if c.Tenancy == nil || !c.Guard.Enabled() {
		// Pre-P4-C wiring: the column default decides, exactly as before.
		return "", true
	}
	sub, ok := c.Guard.Subject(r)
	if !ok {
		envelope.WriteError(w, r, identity.Unauthorized())
		return "", false
	}
	if requested != "" {
		id := domain.TenantID(requested)
		// An organization the caller cannot even see must answer as though it
		// does not exist, for the reason AllowTenant gives.
		if !sub.Allows(domain.PermTenantRead, domain.TenantResource(id)) {
			envelope.WriteAPIError(w, r, http.StatusNotFound, "not_found", "TENANT_NOT_FOUND",
				"organization not found", nil)
			return "", false
		}
		if role, held := sub.TenantRole(id); !held || role == domain.TenantRoleViewer {
			envelope.WriteAPIError(w, r, http.StatusForbidden, "forbidden", "TENANT_READ_ONLY",
				"this account cannot register projects in that organization", nil)
			return "", false
		}
		return id, true
	}
	candidates := make([]domain.TenantID, 0, len(sub.TenantRoles))
	for _, id := range sub.TenantIDs() {
		if sub.TenantRoles[id] == domain.TenantRoleViewer {
			continue
		}
		candidates = append(candidates, id)
	}
	switch len(candidates) {
	case 0:
		// An installation administrator with no membership row of its own.
		// The default organization is where everything else already is.
		return domain.DefaultTenantID, true
	case 1:
		return candidates[0], true
	default:
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "TENANT_REQUIRED",
			"this account belongs to more than one organization; name the one to register the project in",
			map[string]any{"tenantIds": tenantIDStrings(candidates)})
		return "", false
	}
}

func tenantIDStrings(in []domain.TenantID) []string {
	out := make([]string, 0, len(in))
	for _, id := range in {
		out = append(out, string(id))
	}
	return out
}

// stampTenant places a freshly registered project in the organization
// resolveTenant chose. An empty tenant means "leave the column default alone",
// which is the pre-P4-C path.
func (c *ProjectsController) stampTenant(r *http.Request, id domain.ProjectID, tenant domain.TenantID) {
	if c.Tenancy == nil || tenant == "" {
		return
	}
	_, _ = c.Tenancy.SetProjectTenant(r.Context(), id, tenant)
}

// projectVisible reports whether id is visible to current — true for an
// unowned project (nil owner) or one owned by current, false for a
// different explicit owner. A lookup error fails open to "not visible"
// (Get will then either 404 on its own NotFound path, or the caller already
// wrote a 404 — never silently serves a resource whose ownership couldn't
// be verified).
func (c *ProjectsController) projectVisible(ctx context.Context, id domain.ProjectID, current domain.UserID) bool {
	owner, err := c.Ownership.GetProjectOwner(ctx, id)
	if err != nil {
		return false
	}
	return !ownerForbidden(owner, current)
}

// filterOwnedProjects narrows a project list to those visible to current
// (see projectVisible).
func (c *ProjectsController) filterOwnedProjects(ctx context.Context, in []projectsvc.Summary, current domain.UserID) []projectsvc.Summary {
	out := make([]projectsvc.Summary, 0, len(in))
	for _, p := range in {
		if c.projectVisible(ctx, p.ID, current) {
			out = append(out, p)
		}
	}
	return out
}

func projectID(r *http.Request) domain.ProjectID {
	return domain.ProjectID(chi.URLParam(r, "id"))
}

func decodeJSON(r *http.Request, out any) error {
	return json.NewDecoder(r.Body).Decode(out)
}

// decodeJSONStrict rejects request bodies that include keys outside the target
// type. Used on project add/set-config so a misspelled or removed config field
// surfaces as a 400 instead of being silently dropped.
func decodeJSONStrict(r *http.Request, out any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(out)
}
