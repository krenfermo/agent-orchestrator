package controllers

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/identity"
	rbacsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/rbac"
)

// AdminUserView is the wire shape of an account on the administration
// surfaces. It carries the same fields UserView does plus the lifecycle
// timestamps an administrator needs to tell a dormant account from a new one,
// and -- like every other user shape in this package -- never the password
// hash.
type AdminUserView struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	Email       string `json:"email"`
	Username    string `json:"username"`
	Status      string `json:"status" enum:"active,disabled"`
	Role        string `json:"role" enum:"owner,admin,member,viewer"`
	// Federated reports that this account has no local password and can only
	// sign in through the identity provider. Rendering a "reset password"
	// control for such an account would offer something that cannot work.
	Federated bool      `json:"federated"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func adminUserView(u domain.User) AdminUserView {
	return AdminUserView{
		ID:          string(u.ID),
		DisplayName: u.DisplayName,
		Email:       u.Email,
		Username:    u.Username,
		Status:      string(u.Status),
		Role:        string(u.Role),
		Federated:   u.PasswordHash == "",
		CreatedAt:   u.CreatedAt,
		UpdatedAt:   u.UpdatedAt,
	}
}

func adminUserViews(in []domain.User) []AdminUserView {
	out := make([]AdminUserView, 0, len(in))
	for _, u := range in {
		out = append(out, adminUserView(u))
	}
	return out
}

// UserIDParam is the {id} path parameter for the /users routes.
type UserIDParam struct {
	ID string `path:"id" description:"User identifier."`
}

// ListUsersResponse is the body of GET /api/v1/users.
type ListUsersResponse struct {
	Users []AdminUserView `json:"users"`
}

// UserResponse is the body of the single-account routes.
type UserResponse struct {
	User AdminUserView `json:"user"`
}

// CreateUserRequest is the body of POST /api/v1/users.
type CreateUserRequest struct {
	DisplayName string `json:"displayName"`
	Email       string `json:"email"`
	Username    string `json:"username"`
	Password    string `json:"password"`
	Role        string `json:"role" enum:"admin,member,viewer"`
}

// SetUserRoleRequest is the body of PATCH /api/v1/users/{id}/role.
type SetUserRoleRequest struct {
	Role string `json:"role" enum:"owner,admin,member,viewer"`
}

// SetUserStatusRequest is the body of PATCH /api/v1/users/{id}/status.
type SetUserStatusRequest struct {
	Status string `json:"status" enum:"active,disabled"`
}

// UsersController owns the /users administration routes. Every route here is
// gated by the installation-wide users.read / users.manage permissions in
// GlobalAuthzMiddleware, so the handlers themselves carry no authorization
// logic -- the one place that decides is the one place that decides.
type UsersController struct {
	Mgr rbacsvc.Manager
}

// Register mounts the user administration routes.
func (c *UsersController) Register(r chi.Router) {
	r.Get("/users", c.list)
	r.Post("/users", c.create)
	r.Get("/users/{id}", c.get)
	r.Patch("/users/{id}/role", c.setRole)
	r.Patch("/users/{id}/status", c.setStatus)
	r.Get("/users/{id}/teams", c.teams)
}

func (c *UsersController) list(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, http.MethodGet, "/api/v1/users")
		return
	}
	users, err := c.Mgr.ListUsers(r.Context())
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, ListUsersResponse{Users: adminUserViews(users)})
}

func (c *UsersController) get(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, http.MethodGet, "/api/v1/users/{id}")
		return
	}
	u, err := c.Mgr.GetUser(r.Context(), domain.UserID(chi.URLParam(r, "id")))
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, UserResponse{User: adminUserView(u)})
}

func (c *UsersController) create(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, http.MethodPost, "/api/v1/users")
		return
	}
	var in CreateUserRequest
	if err := decodeJSONStrict(r, &in); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid JSON body", nil)
		return
	}
	actor, err := identity.RequirePrincipal(r)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	u, err := c.Mgr.CreateUser(r.Context(), actor, rbacsvc.CreateUserInput{
		DisplayName: in.DisplayName,
		Email:       in.Email,
		Username:    in.Username,
		Password:    in.Password,
		Role:        domain.UserRole(in.Role),
	})
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusCreated, UserResponse{User: adminUserView(u)})
}

// setRole and setStatus are the same handler shape with a different body type
// and a different service call, so they share one: decode, require the acting
// principal, apply, render. Writing them out twice is how the two drift on the
// error envelope or the principal check, which are exactly the parts that must
// not drift on an authorization route.
func (c *UsersController) setRole(w http.ResponseWriter, r *http.Request) {
	patchUser(c, w, r, "/api/v1/users/{id}/role",
		func(ctx context.Context, actor domain.Principal, id domain.UserID, in SetUserRoleRequest) (domain.User, error) {
			return c.Mgr.SetUserRole(ctx, actor, id, domain.UserRole(in.Role))
		})
}

func (c *UsersController) setStatus(w http.ResponseWriter, r *http.Request) {
	patchUser(c, w, r, "/api/v1/users/{id}/status",
		func(ctx context.Context, actor domain.Principal, id domain.UserID, in SetUserStatusRequest) (domain.User, error) {
			return c.Mgr.SetUserStatus(ctx, actor, id, domain.UserStatus(in.Status))
		})
}

func patchUser[T any](
	c *UsersController,
	w http.ResponseWriter,
	r *http.Request,
	route string,
	apply func(ctx context.Context, actor domain.Principal, id domain.UserID, in T) (domain.User, error),
) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, http.MethodPatch, route)
		return
	}
	var in T
	if err := decodeJSONStrict(r, &in); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid JSON body", nil)
		return
	}
	actor, err := identity.RequirePrincipal(r)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	u, err := apply(r.Context(), actor, domain.UserID(chi.URLParam(r, "id")), in)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, UserResponse{User: adminUserView(u)})
}

func (c *UsersController) teams(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, http.MethodGet, "/api/v1/users/{id}/teams")
		return
	}
	memberships, err := c.Mgr.ListTeamsForUser(r.Context(), domain.UserID(chi.URLParam(r, "id")))
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, ListTeamMembersResponse{Members: teamMemberViews(memberships)})
}
