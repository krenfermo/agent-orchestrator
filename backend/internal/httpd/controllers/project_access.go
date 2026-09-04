package controllers

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/identity"
	rbacsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/rbac"
)

// OKResponse is the body of the mutations whose only meaningful answer is
// "done" -- a revoke, a membership removal, a team deletion.
type OKResponse struct {
	OK bool `json:"ok"`
}

// ProjectGrantSubjectParams are the path parameters identifying one grant.
type ProjectGrantSubjectParams struct {
	ID          string `path:"id" description:"Project identifier (registry key)."`
	SubjectKind string `path:"subjectKind" description:"Grant subject kind: user or team."`
	SubjectID   string `path:"subjectId" description:"Grant subject identifier."`
}

// ProjectGrantView is the wire shape of one access grant.
type ProjectGrantView struct {
	SubjectKind string    `json:"subjectKind" enum:"user,team"`
	SubjectID   string    `json:"subjectId"`
	Role        string    `json:"role" enum:"admin,member,viewer"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// ProjectAccessResponse is the body of GET /api/v1/projects/{id}/access.
type ProjectAccessResponse struct {
	ProjectID string `json:"projectId"`
	// OwnerUserID is the account that registered the project. It is an
	// implicit administrator grant that predates P4-B and cannot be revoked
	// from this surface; it is reported so the list is not silently
	// incomplete.
	OwnerUserID string             `json:"ownerUserId,omitempty"`
	Grants      []ProjectGrantView `json:"grants"`
	// Permissions is what the CALLER may do in this project. The access screen
	// renders its own controls from this rather than re-deriving authority
	// from a role name, so a change to the role tables reaches the UI without
	// a matching change in React.
	Permissions []string `json:"permissions"`
}

// GrantProjectAccessRequest is the body of PUT /api/v1/projects/{id}/access.
type GrantProjectAccessRequest struct {
	SubjectKind string `json:"subjectKind" enum:"user,team"`
	SubjectID   string `json:"subjectId"`
	Role        string `json:"role" enum:"admin,member,viewer"`
}

// ProjectAccessController owns the per-project access list. Unlike /users and
// /teams, these routes are PROJECT-scoped: a project administrator manages
// their own project's access without holding any installation-wide authority,
// which is the whole point of having a project scope at all.
type ProjectAccessController struct {
	Mgr   rbacsvc.Manager
	Guard Guard
}

// Register mounts the project access routes.
func (c *ProjectAccessController) Register(r chi.Router) {
	r.Get("/projects/{id}/access", c.list)
	r.Put("/projects/{id}/access", c.grant)
	r.Delete("/projects/{id}/access/{subjectKind}/{subjectId}", c.revoke)
}

func (c *ProjectAccessController) list(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, http.MethodGet, "/api/v1/projects/{id}/access")
		return
	}
	id := projectID(r)
	if !c.Guard.AllowProject(w, r, domain.PermProjectAccessRead, id, "PROJECT_NOT_FOUND", "project not found") {
		return
	}
	access, err := c.Mgr.ListProjectAccess(r.Context(), id)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, c.accessResponse(r, access))
}

func (c *ProjectAccessController) grant(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, http.MethodPut, "/api/v1/projects/{id}/access")
		return
	}
	id := projectID(r)
	if !c.Guard.AllowProject(w, r, domain.PermProjectAccessManage, id, "PROJECT_NOT_FOUND", "project not found") {
		return
	}
	var in GrantProjectAccessRequest
	if err := decodeJSONStrict(r, &in); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid JSON body", nil)
		return
	}
	actor, err := identity.RequirePrincipal(r)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	if _, err := c.Mgr.GrantProjectAccess(r.Context(), actor, id,
		domain.GrantSubjectKind(in.SubjectKind), in.SubjectID, domain.ProjectRole(in.Role)); err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	access, err := c.Mgr.ListProjectAccess(r.Context(), id)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, c.accessResponse(r, access))
}

func (c *ProjectAccessController) revoke(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, http.MethodDelete, "/api/v1/projects/{id}/access/{subjectKind}/{subjectId}")
		return
	}
	id := projectID(r)
	if !c.Guard.AllowProject(w, r, domain.PermProjectAccessManage, id, "PROJECT_NOT_FOUND", "project not found") {
		return
	}
	actor, err := identity.RequirePrincipal(r)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	kind := domain.GrantSubjectKind(chi.URLParam(r, "subjectKind"))
	if err := c.Mgr.RevokeProjectAccess(r.Context(), actor, id, kind, chi.URLParam(r, "subjectId")); err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, OKResponse{OK: true})
}

func (c *ProjectAccessController) accessResponse(r *http.Request, access rbacsvc.ProjectAccess) ProjectAccessResponse {
	grants := make([]ProjectGrantView, 0, len(access.Grants))
	for _, g := range access.Grants {
		grants = append(grants, ProjectGrantView{
			SubjectKind: string(g.SubjectKind),
			SubjectID:   g.SubjectID,
			Role:        string(g.Role),
			CreatedAt:   g.CreatedAt,
			UpdatedAt:   g.UpdatedAt,
		})
	}
	out := ProjectAccessResponse{
		ProjectID:   string(access.ProjectID),
		Grants:      grants,
		Permissions: []string{},
	}
	if access.OwnerUserID != nil {
		out.OwnerUserID = string(*access.OwnerUserID)
	}
	if sub, ok := c.Guard.Subject(r); ok {
		for _, p := range sub.ProjectPermissions(access.ProjectID) {
			out.Permissions = append(out.Permissions, string(p))
		}
	}
	return out
}
