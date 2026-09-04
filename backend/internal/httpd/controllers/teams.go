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

// TeamView is the wire shape of a team.
type TeamView struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Slug        string    `json:"slug"`
	Description string    `json:"description"`
	Status      string    `json:"status" enum:"active,archived"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

func teamView(t domain.Team) TeamView {
	return TeamView{
		ID:          string(t.ID),
		Name:        t.Name,
		Slug:        t.Slug,
		Description: t.Description,
		Status:      string(t.Status),
		CreatedAt:   t.CreatedAt,
		UpdatedAt:   t.UpdatedAt,
	}
}

// TeamMemberView is the wire shape of one membership.
type TeamMemberView struct {
	TeamID    string    `json:"teamId"`
	UserID    string    `json:"userId"`
	Role      string    `json:"role" enum:"maintainer,member"`
	CreatedAt time.Time `json:"createdAt"`
}

func teamMemberViews(in []domain.TeamMembership) []TeamMemberView {
	out := make([]TeamMemberView, 0, len(in))
	for _, m := range in {
		out = append(out, TeamMemberView{
			TeamID:    string(m.TeamID),
			UserID:    string(m.UserID),
			Role:      string(m.Role),
			CreatedAt: m.CreatedAt,
		})
	}
	return out
}

// TeamIDParam is the {id} path parameter for the /teams routes.
type TeamIDParam struct {
	ID string `path:"id" description:"Team identifier."`
}

// TeamMemberParams are the {id}/{userId} path parameters for one membership.
type TeamMemberParams struct {
	ID     string `path:"id" description:"Team identifier."`
	UserID string `path:"userId" description:"User identifier."`
}

// ListTeamsResponse is the body of GET /api/v1/teams.
type ListTeamsResponse struct {
	Teams []TeamView `json:"teams"`
}

// TeamResponse is the body of the single-team routes.
type TeamResponse struct {
	Team TeamView `json:"team"`
}

// ListTeamMembersResponse is the body of the membership listings.
type ListTeamMembersResponse struct {
	Members []TeamMemberView `json:"members"`
}

// CreateTeamRequest is the body of POST /api/v1/teams.
type CreateTeamRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// UpdateTeamRequest is the body of PATCH /api/v1/teams/{id}.
type UpdateTeamRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Archived, when set, archives or reactivates the team in the same call.
	Archived *bool `json:"archived,omitempty"`
}

// AddTeamMemberRequest is the body of POST /api/v1/teams/{id}/members.
type AddTeamMemberRequest struct {
	UserID string `json:"userId"`
	Role   string `json:"role" enum:"maintainer,member"`
}

// TeamsController owns the /teams routes. Authorization is the
// installation-wide teams.read / teams.manage pair, enforced once in
// GlobalAuthzMiddleware.
type TeamsController struct {
	Mgr rbacsvc.Manager
}

// Register mounts the team routes.
func (c *TeamsController) Register(r chi.Router) {
	r.Get("/teams", c.list)
	r.Post("/teams", c.create)
	r.Get("/teams/{id}", c.get)
	r.Patch("/teams/{id}", c.update)
	r.Delete("/teams/{id}", c.remove)
	r.Get("/teams/{id}/members", c.members)
	r.Post("/teams/{id}/members", c.addMember)
	r.Delete("/teams/{id}/members/{userId}", c.removeMember)
}

func teamID(r *http.Request) domain.TeamID { return domain.TeamID(chi.URLParam(r, "id")) }

func (c *TeamsController) list(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, http.MethodGet, "/api/v1/teams")
		return
	}
	teams, err := c.Mgr.ListTeams(r.Context())
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	views := make([]TeamView, 0, len(teams))
	for _, t := range teams {
		views = append(views, teamView(t))
	}
	envelope.WriteJSON(w, http.StatusOK, ListTeamsResponse{Teams: views})
}

func (c *TeamsController) get(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, http.MethodGet, "/api/v1/teams/{id}")
		return
	}
	t, err := c.Mgr.GetTeam(r.Context(), teamID(r))
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, TeamResponse{Team: teamView(t)})
}

func (c *TeamsController) create(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, http.MethodPost, "/api/v1/teams")
		return
	}
	var in CreateTeamRequest
	if err := decodeJSONStrict(r, &in); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid JSON body", nil)
		return
	}
	actor, err := identity.RequirePrincipal(r)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	t, err := c.Mgr.CreateTeam(r.Context(), actor, in.Name, in.Description)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusCreated, TeamResponse{Team: teamView(t)})
}

func (c *TeamsController) update(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, http.MethodPatch, "/api/v1/teams/{id}")
		return
	}
	var in UpdateTeamRequest
	if err := decodeJSONStrict(r, &in); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid JSON body", nil)
		return
	}
	actor, err := identity.RequirePrincipal(r)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	id := teamID(r)
	if in.Name != "" {
		if _, err := c.Mgr.UpdateTeam(r.Context(), actor, id, in.Name, in.Description); err != nil {
			envelope.WriteError(w, r, err)
			return
		}
	}
	if in.Archived != nil {
		if _, err := c.Mgr.ArchiveTeam(r.Context(), actor, id, *in.Archived); err != nil {
			envelope.WriteError(w, r, err)
			return
		}
	}
	t, err := c.Mgr.GetTeam(r.Context(), id)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, TeamResponse{Team: teamView(t)})
}

func (c *TeamsController) remove(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, http.MethodDelete, "/api/v1/teams/{id}")
		return
	}
	actor, err := identity.RequirePrincipal(r)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	if err := c.Mgr.DeleteTeam(r.Context(), actor, teamID(r)); err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, OKResponse{OK: true})
}

func (c *TeamsController) members(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, http.MethodGet, "/api/v1/teams/{id}/members")
		return
	}
	members, err := c.Mgr.ListTeamMembers(r.Context(), teamID(r))
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, ListTeamMembersResponse{Members: teamMemberViews(members)})
}

func (c *TeamsController) addMember(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, http.MethodPost, "/api/v1/teams/{id}/members")
		return
	}
	var in AddTeamMemberRequest
	if err := decodeJSONStrict(r, &in); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid JSON body", nil)
		return
	}
	actor, err := identity.RequirePrincipal(r)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	m, err := c.Mgr.AddTeamMember(r.Context(), actor, teamID(r), domain.UserID(in.UserID), domain.TeamRole(in.Role))
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, ListTeamMembersResponse{Members: teamMemberViews([]domain.TeamMembership{m})})
}

func (c *TeamsController) removeMember(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, http.MethodDelete, "/api/v1/teams/{id}/members/{userId}")
		return
	}
	actor, err := identity.RequirePrincipal(r)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	if err := c.Mgr.RemoveTeamMember(r.Context(), actor, teamID(r), domain.UserID(chi.URLParam(r, "userId"))); err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, OKResponse{OK: true})
}
