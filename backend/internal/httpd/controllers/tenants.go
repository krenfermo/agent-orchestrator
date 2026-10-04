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

// TenantView is the wire shape of an organization.
type TenantView struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Slug        string    `json:"slug"`
	Description string    `json:"description"`
	Status      string    `json:"status" enum:"active,archived"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
	// Role is the caller's own role in this organization, so the frontend can
	// decide what to offer without a second round trip per row. Empty when the
	// caller reaches the organization by installation-wide authority rather
	// than by belonging to it.
	Role string `json:"role,omitempty" enum:"owner,admin,member,viewer"`
}

func tenantView(t domain.Tenant, role domain.TenantRole) TenantView {
	return TenantView{
		ID:          string(t.ID),
		Name:        t.Name,
		Slug:        t.Slug,
		Description: t.Description,
		Status:      string(t.Status),
		CreatedAt:   t.CreatedAt,
		UpdatedAt:   t.UpdatedAt,
		Role:        string(role),
	}
}

// TenantMemberView is the wire shape of one organization membership.
type TenantMemberView struct {
	TenantID  string    `json:"tenantId"`
	UserID    string    `json:"userId"`
	Role      string    `json:"role" enum:"owner,admin,member,viewer"`
	CreatedAt time.Time `json:"createdAt"`
}

func tenantMemberViews(in []domain.TenantMembership) []TenantMemberView {
	out := make([]TenantMemberView, 0, len(in))
	for _, m := range in {
		out = append(out, TenantMemberView{
			TenantID:  string(m.TenantID),
			UserID:    string(m.UserID),
			Role:      string(m.Role),
			CreatedAt: m.CreatedAt,
		})
	}
	return out
}

// TenantIDParam is the {id} path parameter for the /tenants routes.
type TenantIDParam struct {
	ID string `path:"id" description:"Organization identifier."`
}

// TenantMemberParams are the {id}/{userId} path parameters for one membership.
type TenantMemberParams struct {
	ID     string `path:"id" description:"Organization identifier."`
	UserID string `path:"userId" description:"User identifier."`
}

// ListTenantsResponse is the body of GET /api/v1/tenants.
type ListTenantsResponse struct {
	Tenants []TenantView `json:"tenants"`
}

// TenantResponse is the body of the single-organization routes.
type TenantResponse struct {
	Tenant TenantView `json:"tenant"`
}

// ListTenantMembersResponse is the body of the membership listing.
type ListTenantMembersResponse struct {
	Members []TenantMemberView `json:"members"`
}

// CreateTenantRequest is the body of POST /api/v1/tenants.
type CreateTenantRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// UpdateTenantRequest is the body of PATCH /api/v1/tenants/{id}.
type UpdateTenantRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Archived, when set, archives or reactivates the organization in the same
	// call.
	Archived *bool `json:"archived,omitempty"`
}

// AddTenantMemberRequest is the body of POST /api/v1/tenants/{id}/members.
type AddTenantMemberRequest struct {
	UserID string `json:"userId"`
	Role   string `json:"role" enum:"owner,admin,member,viewer"`
}

// AssignProjectTenantRequest is the body of
// PUT /api/v1/projects/{id}/tenant.
type AssignProjectTenantRequest struct {
	TenantID string `json:"tenantId"`
}

// TenantsController owns the /tenants routes.
//
// Unlike /users and /teams, these are NOT gated by GlobalAuthzMiddleware.
// Authorization here is per-organization and the middleware cannot resolve an
// organization from the path alone, so every handler asks Guard itself -- the
// same arrangement /projects has had since P4-B, and for the same reason.
type TenantsController struct {
	Mgr   rbacsvc.Manager
	Guard Guard
}

// Register mounts the organization routes.
func (c *TenantsController) Register(r chi.Router) {
	r.Get("/tenants", c.list)
	r.Post("/tenants", c.create)
	r.Get("/tenants/{id}", c.get)
	r.Patch("/tenants/{id}", c.update)
	r.Get("/tenants/{id}/members", c.members)
	r.Post("/tenants/{id}/members", c.addMember)
	r.Delete("/tenants/{id}/members/{userId}", c.removeMember)
}

func tenantID(r *http.Request) domain.TenantID { return domain.TenantID(chi.URLParam(r, "id")) }

// list returns the organizations the caller can see, each with the caller's
// own role in it.
//
// The list is resolved once and filtered in memory, the way the project list
// is: a subject knows every organization it belongs to, so deciding a hundred
// rows costs no more queries than deciding one.
func (c *TenantsController) list(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, http.MethodGet, "/api/v1/tenants")
		return
	}
	tenants, err := c.Mgr.ListTenants(r.Context())
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	views := make([]TenantView, 0, len(tenants))
	if !c.Guard.Enabled() {
		for _, t := range tenants {
			views = append(views, tenantView(t, ""))
		}
		envelope.WriteJSON(w, http.StatusOK, ListTenantsResponse{Tenants: views})
		return
	}
	sub, ok := c.Guard.Subject(r)
	if !ok {
		envelope.WriteError(w, r, identity.Unauthorized())
		return
	}
	for _, t := range tenants {
		if !sub.Allows(domain.PermTenantRead, domain.TenantResource(t.ID)) {
			continue
		}
		role, _ := sub.TenantRole(t.ID)
		views = append(views, tenantView(t, role))
	}
	envelope.WriteJSON(w, http.StatusOK, ListTenantsResponse{Tenants: views})
}

func (c *TenantsController) get(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, http.MethodGet, "/api/v1/tenants/{id}")
		return
	}
	id := tenantID(r)
	if !c.Guard.AllowTenant(w, r, domain.PermTenantRead, id) {
		return
	}
	t, err := c.Mgr.GetTenant(r.Context(), id)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, TenantResponse{Tenant: tenantView(t, c.callerRole(r, id))})
}

func (c *TenantsController) create(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, http.MethodPost, "/api/v1/tenants")
		return
	}
	// Founding an organization is installation-wide: there is no organization
	// to be scoped to yet.
	if !c.Guard.AllowGlobal(w, r, domain.PermTenantCreate) {
		return
	}
	var in CreateTenantRequest
	if err := decodeJSONStrict(r, &in); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid JSON body", nil)
		return
	}
	actor, err := identity.RequirePrincipal(r)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	t, err := c.Mgr.CreateTenant(r.Context(), actor, in.Name, in.Description)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusCreated, TenantResponse{Tenant: tenantView(t, domain.TenantRoleOwner)})
}

func (c *TenantsController) update(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, http.MethodPatch, "/api/v1/tenants/{id}")
		return
	}
	id := tenantID(r)
	if !c.Guard.AllowTenant(w, r, domain.PermTenantManage, id) {
		return
	}
	var in UpdateTenantRequest
	if err := decodeJSONStrict(r, &in); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid JSON body", nil)
		return
	}
	actor, err := identity.RequirePrincipal(r)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	t, err := c.Mgr.UpdateTenant(r.Context(), actor, id, in.Name, in.Description)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	if in.Archived != nil {
		t, err = c.Mgr.ArchiveTenant(r.Context(), actor, id, *in.Archived)
		if err != nil {
			envelope.WriteError(w, r, err)
			return
		}
	}
	envelope.WriteJSON(w, http.StatusOK, TenantResponse{Tenant: tenantView(t, c.callerRole(r, id))})
}

func (c *TenantsController) members(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, http.MethodGet, "/api/v1/tenants/{id}/members")
		return
	}
	id := tenantID(r)
	if !c.Guard.AllowTenant(w, r, domain.PermTenantMembersRead, id) {
		return
	}
	members, err := c.Mgr.ListTenantMembers(r.Context(), id)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, ListTenantMembersResponse{Members: tenantMemberViews(members)})
}

func (c *TenantsController) addMember(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, http.MethodPost, "/api/v1/tenants/{id}/members")
		return
	}
	id := tenantID(r)
	if !c.Guard.AllowTenant(w, r, domain.PermTenantMembersManage, id) {
		return
	}
	var in AddTenantMemberRequest
	if err := decodeJSONStrict(r, &in); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid JSON body", nil)
		return
	}
	actor, err := identity.RequirePrincipal(r)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	m, err := c.Mgr.AddTenantMember(r.Context(), actor, id, domain.UserID(in.UserID), domain.TenantRole(in.Role))
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusCreated, tenantMemberViews([]domain.TenantMembership{m})[0])
}

func (c *TenantsController) removeMember(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, http.MethodDelete, "/api/v1/tenants/{id}/members/{userId}")
		return
	}
	id := tenantID(r)
	if !c.Guard.AllowTenant(w, r, domain.PermTenantMembersManage, id) {
		return
	}
	actor, err := identity.RequirePrincipal(r)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	if err := c.Mgr.RemoveTenantMember(r.Context(), actor, id, domain.UserID(chi.URLParam(r, "userId"))); err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// callerRole reports the caller's own role in an organization, for the
// response body. It is presentation, not authorization: the decision was
// already made by AllowTenant above.
func (c *TenantsController) callerRole(r *http.Request, id domain.TenantID) domain.TenantRole {
	sub, ok := c.Guard.Subject(r)
	if !ok {
		return ""
	}
	role, _ := sub.TenantRole(id)
	return role
}
