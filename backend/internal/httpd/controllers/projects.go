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
	if c.scopingEnforced() {
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
	p, err := c.Mgr.Add(r.Context(), in)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	c.stampOwner(r, p.ID)
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
	entries, err := c.Mgr.ListAllowedRootEntries(r.Context(), path)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	if entries == nil {
		entries = []projectsvc.BrowseEntry{}
	}
	envelope.WriteJSON(w, http.StatusOK, projectsvc.BrowseResult{Path: path, Entries: entries})
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
	p, err := c.Mgr.CloneFromGitHub(r.Context(), in)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	c.stampOwner(r, p.ID)
	envelope.WriteJSON(w, http.StatusCreated, ProjectResponse{Project: p})
}

func (c *ProjectsController) get(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/projects/{id}")
		return
	}
	id := projectID(r)
	if c.scopingEnforced() {
		user, err := identity.Require(r)
		if err != nil {
			envelope.WriteError(w, r, err)
			return
		}
		if !c.projectVisible(r.Context(), id, user.ID) {
			envelope.WriteAPIError(w, r, http.StatusNotFound, "not_found", "PROJECT_NOT_FOUND", "project not found", nil)
			return
		}
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
