package controllers

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/identity"
)

// work_items.go — the /projects/{id}/workitems surface (P4-E §11, §12).
//
// TENANCY IS THE PROJECT'S TENANCY. Every route is project-scoped and goes
// through Guard.AllowProject, which since P4-C resolves through the caller's
// organizations. A project in another organization never enters the caller's
// resolved subject, so a guessed project id answers 404 here exactly as it does
// on /projects/{id} itself — including for the routes that would otherwise be
// an information leak, like "does this project have a Plane connection".
//
// There is deliberately no second tenancy check. A second one is a second thing
// that can be wrong, and it would be wrong in the direction that matters.
//
// THREE PERMISSIONS, not one:
//
//	workitems.read    see the connection and the links
//	workitems.link    link, create, unlink, force a sync
//	workitems.manage  hold the credential and choose the workspace/project
//
// The split is the difference between "I am tracking my work" and "I decide
// which organization this project reports into". A project member does the
// first; only a project admin does the second.

// WorkItemsService is the controller-facing contract.
type WorkItemsService interface {
	WorkItemsConfig(ctx context.Context, projectID domain.ProjectID) (WorkItemsConfigResponse, error)
	PutWorkItemsConfig(ctx context.Context, projectID domain.ProjectID, req WorkItemsConfigUpdate) (WorkItemsConfigResponse, error)
	DeleteWorkItemsConfig(ctx context.Context, projectID domain.ProjectID) error
	TestWorkItemsConnection(ctx context.Context, projectID domain.ProjectID) (WorkItemsConnectionResponse, error)
	ListWorkItemsProviderProjects(ctx context.Context, projectID domain.ProjectID) (WorkItemsProviderProjectsResponse, error)

	ListWorkItemLinks(ctx context.Context, projectID domain.ProjectID, live bool) (WorkItemLinksResponse, error)
	CreateWorkItemLink(ctx context.Context, req WorkItemLinkRequest) (WorkItemLinkResponse, error)
	DeleteWorkItemLink(ctx context.Context, projectID domain.ProjectID, linkID string) error
	SetWorkItemLinkSync(ctx context.Context, projectID domain.ProjectID, linkID string, enabled bool) error

	WorkItemsHealth(ctx context.Context, projectID domain.ProjectID) (WorkItemsHealthResponse, error)
	SyncWorkItems(ctx context.Context, projectID domain.ProjectID) (WorkItemsSyncResponse, error)
	ListWorkItemsAudit(ctx context.Context, projectID domain.ProjectID, limit int) (WorkItemsAuditResponse, error)
}

// WorkItemsConfigResponse is one project's connection, WITHOUT its credential.
//
// There is no token field on this type at all. That is stronger than
// remembering to strip one: a future handler cannot serialize what does not
// exist, and tokenConfigured answers the only question a UI actually has.
type WorkItemsConfigResponse struct {
	ProjectID string `json:"projectId"`
	Provider  string `json:"provider" enum:"plane"`
	// BaseURL is the configured provider origin, or the provider default.
	BaseURL   string `json:"baseUrl,omitempty"`
	Workspace string `json:"workspace,omitempty"`

	ExternalProjectID   string `json:"externalProjectId,omitempty"`
	ExternalProjectName string `json:"externalProjectName,omitempty"`
	ExternalProjectKey  string `json:"externalProjectKey,omitempty"`

	// TokenConfigured reports that a credential exists. The credential never
	// leaves the daemon.
	TokenConfigured bool `json:"tokenConfigured"`
	// TokenFromEnv reports that the credential comes from the process
	// environment rather than the database — which somebody wondering why
	// clearing the stored token changed nothing needs to be told.
	TokenFromEnv bool `json:"tokenFromEnv"`

	Enabled      bool `json:"enabled"`
	SyncStates   bool `json:"syncStates"`
	SyncComments bool `json:"syncComments"`

	Connected      bool   `json:"connected"`
	Degraded       bool   `json:"degraded"`
	LastCheckAt    string `json:"lastCheckAt,omitempty"`
	LastCheckError string `json:"lastCheckError,omitempty"`
}

// WorkItemsConfigUpdate is a settings write.
//
// Every field is a pointer so "leave this alone" and "set it to empty" are
// distinguishable. That matters most for apiToken: a form re-submitted without
// the credential field must not erase the stored one.
type WorkItemsConfigUpdate struct {
	BaseURL           *string `json:"baseUrl,omitempty"`
	Workspace         *string `json:"workspace,omitempty"`
	ExternalProjectID *string `json:"externalProjectId,omitempty"`
	// APIToken sets the credential. Omit it to keep the stored one; send ""
	// to clear it. It is write-only: no response ever contains it.
	APIToken     *string `json:"apiToken,omitempty"`
	Enabled      *bool   `json:"enabled,omitempty"`
	SyncStates   *bool   `json:"syncStates,omitempty"`
	SyncComments *bool   `json:"syncComments,omitempty"`
}

// WorkItemsConnectionResponse is a successful preflight.
type WorkItemsConnectionResponse struct {
	Provider      string `json:"provider" enum:"plane"`
	Workspace     string `json:"workspace"`
	WorkspaceName string `json:"workspaceName,omitempty"`
	Projects      int    `json:"projects"`
}

// WorkItemsProviderProject is one project in the provider's workspace.
type WorkItemsProviderProject struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Identifier  string `json:"identifier,omitempty"`
	Description string `json:"description,omitempty"`
}

// WorkItemsProviderProjectsResponse is the mapping picker's options.
type WorkItemsProviderProjectsResponse struct {
	Projects []WorkItemsProviderProject `json:"projects"`
}

// WorkItemLinkRequest links AO work to an external item.
type WorkItemLinkRequest struct {
	ProjectID domain.ProjectID `json:"-"`
	// Scope is what the link attaches to.
	Scope string `json:"scope" enum:"project,run,task"`
	// ScopeID is the run id or task id, empty for a project-scoped link.
	ScopeID string `json:"scopeId,omitempty"`
	// Reference is an existing item: "PROJ-123" or a Plane work item URL.
	Reference string `json:"reference,omitempty"`
	// Create asks AO to create the item instead of resolving one. AO does this
	// only when asked: §6's rule is that internal repair and reviewer work
	// never mints items on somebody's board by itself.
	Create      bool   `json:"create,omitempty"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	// SyncEnabled is whether AO may push its execution state to this item.
	SyncEnabled bool   `json:"syncEnabled"`
	Actor       string `json:"-"`
}

// WorkItemLinkResponse is one link and what AO currently knows about the item.
type WorkItemLinkResponse struct {
	ID        string `json:"id"`
	ProjectID string `json:"projectId"`
	Scope     string `json:"scope" enum:"project,run,task"`
	ScopeID   string `json:"scopeId,omitempty"`

	Provider          string `json:"provider" enum:"plane"`
	Workspace         string `json:"workspace"`
	ExternalProjectID string `json:"externalProjectId"`
	ExternalItemID    string `json:"externalItemId"`
	ExternalItemKey   string `json:"externalItemKey,omitempty"`
	URL               string `json:"url,omitempty"`

	Origin      string `json:"origin" enum:"manual,created"`
	SyncEnabled bool   `json:"syncEnabled"`

	// Title and State are the CURRENT values when the provider answered, and
	// the cached ones when it did not — Stale says which.
	Title string `json:"title,omitempty"`
	State string `json:"state,omitempty"`
	// StateName is the provider's own state name, which is what a person
	// recognises ("In Review", not "started").
	StateName string `json:"stateName,omitempty"`
	// Stale reports that Title and State come from the cache because the
	// provider could not be reached, and LiveError says why.
	Stale     bool   `json:"stale"`
	LiveError string `json:"liveError,omitempty"`
	// Readiness is what the external plan SUGGESTS. Advisory only: nothing in
	// AO's execution reads it.
	Readiness  string `json:"readiness,omitempty" enum:"ready,deferred,done,unknown"`
	LastSeenAt string `json:"lastSeenAt,omitempty"`
	CreatedAt  string `json:"createdAt"`
}

// WorkItemLinksResponse is a project's links.
type WorkItemLinksResponse struct {
	Links []WorkItemLinkResponse `json:"links"`
}

// WorkItemsHealthResponse is the integration's operational state.
type WorkItemsHealthResponse struct {
	Configured bool `json:"configured"`
	Enabled    bool `json:"enabled"`
	Connected  bool `json:"connected"`
	// Degraded is the §13 signal: switched on, and not currently working.
	Degraded       bool   `json:"degraded"`
	Pending        int64  `json:"pending"`
	Failed         int64  `json:"failed"`
	Links          int    `json:"links"`
	LastCheckAt    string `json:"lastCheckAt,omitempty"`
	LastCheckError string `json:"lastCheckError,omitempty"`
}

// WorkItemsSyncResponse is what a forced drain did.
type WorkItemsSyncResponse struct {
	Claimed   int `json:"claimed"`
	Delivered int `json:"delivered"`
	Deferred  int `json:"deferred"`
	Failed    int `json:"failed"`
	Skipped   int `json:"skipped"`
}

// WorkItemsAuditEntry is one recorded provider operation.
type WorkItemsAuditEntry struct {
	ID              string `json:"id"`
	Provider        string `json:"provider"`
	Operation       string `json:"operation"`
	ExternalItemID  string `json:"externalItemId,omitempty"`
	ExternalItemKey string `json:"externalItemKey,omitempty"`
	Outcome         string `json:"outcome" enum:"ok,retryable,failed,skipped"`
	ErrorKind       string `json:"errorKind,omitempty"`
	Detail          string `json:"detail,omitempty"`
	Attempts        int64  `json:"attempts"`
	DurationMS      int64  `json:"durationMs"`
	CreatedAt       string `json:"createdAt"`
}

// WorkItemsAuditResponse is the recent operations for a project.
type WorkItemsAuditResponse struct {
	Entries []WorkItemsAuditEntry `json:"entries"`
}

// WorkItemsController owns the /projects/{id}/workitems routes.
type WorkItemsController struct {
	Svc   WorkItemsService
	Guard Guard
}

// Register mounts the routes.
func (c *WorkItemsController) Register(r chi.Router) {
	r.Get("/projects/{id}/workitems", c.scoped(domain.PermWorkItemsRead, c.config))
	r.Put("/projects/{id}/workitems", c.scoped(domain.PermWorkItemsManage, c.putConfig))
	r.Delete("/projects/{id}/workitems", c.scoped(domain.PermWorkItemsManage, c.deleteConfig))
	r.Post("/projects/{id}/workitems/test", c.scoped(domain.PermWorkItemsManage, c.test))
	// Listing the provider's projects requires manage: it enumerates an
	// external organization's contents, which is the connection owner's view
	// rather than a member's.
	r.Get("/projects/{id}/workitems/projects", c.scoped(domain.PermWorkItemsManage, c.providerProjects))

	r.Get("/projects/{id}/workitems/health", c.scoped(domain.PermWorkItemsRead, c.health))
	r.Get("/projects/{id}/workitems/audit", c.scoped(domain.PermWorkItemsRead, c.audit))

	r.Get("/projects/{id}/workitems/links", c.scoped(domain.PermWorkItemsRead, c.links))
	r.Post("/projects/{id}/workitems/links", c.scoped(domain.PermWorkItemsLink, c.link))
	r.Delete("/projects/{id}/workitems/links/{linkId}", c.scoped(domain.PermWorkItemsLink, c.unlink))
	r.Post("/projects/{id}/workitems/links/{linkId}/sync", c.scoped(domain.PermWorkItemsLink, c.setLinkSync))

	r.Post("/projects/{id}/workitems/sync", c.scoped(domain.PermWorkItemsLink, c.sync))
}

// scoped wraps a handler with the project permission it needs. The 404 for a
// project the caller cannot reach is what makes a guessed id indistinguishable
// from a nonexistent one.
func (c *WorkItemsController) scoped(perm domain.Permission, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !c.Guard.AllowProject(w, r, perm, projectID(r), "PROJECT_NOT_FOUND", "project not found") {
			return
		}
		h(w, r)
	}
}

func (c *WorkItemsController) available(w http.ResponseWriter, r *http.Request, method, route string) bool {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, method, route)
		return false
	}
	return true
}

func (c *WorkItemsController) config(w http.ResponseWriter, r *http.Request) {
	if !c.available(w, r, "GET", "/api/v1/projects/{id}/workitems") {
		return
	}
	out, err := c.Svc.WorkItemsConfig(r.Context(), projectID(r))
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, out)
}

func (c *WorkItemsController) putConfig(w http.ResponseWriter, r *http.Request) {
	if !c.available(w, r, "PUT", "/api/v1/projects/{id}/workitems") {
		return
	}
	var req WorkItemsConfigUpdate
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request",
			"INVALID_BODY", "request body is not valid JSON", nil)
		return
	}
	out, err := c.Svc.PutWorkItemsConfig(r.Context(), projectID(r), req)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, out)
}

func (c *WorkItemsController) deleteConfig(w http.ResponseWriter, r *http.Request) {
	if !c.available(w, r, "DELETE", "/api/v1/projects/{id}/workitems") {
		return
	}
	if err := c.Svc.DeleteWorkItemsConfig(r.Context(), projectID(r)); err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (c *WorkItemsController) test(w http.ResponseWriter, r *http.Request) {
	if !c.available(w, r, "POST", "/api/v1/projects/{id}/workitems/test") {
		return
	}
	out, err := c.Svc.TestWorkItemsConnection(r.Context(), projectID(r))
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, out)
}

func (c *WorkItemsController) providerProjects(w http.ResponseWriter, r *http.Request) {
	if !c.available(w, r, "GET", "/api/v1/projects/{id}/workitems/projects") {
		return
	}
	out, err := c.Svc.ListWorkItemsProviderProjects(r.Context(), projectID(r))
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, out)
}

func (c *WorkItemsController) health(w http.ResponseWriter, r *http.Request) {
	if !c.available(w, r, "GET", "/api/v1/projects/{id}/workitems/health") {
		return
	}
	out, err := c.Svc.WorkItemsHealth(r.Context(), projectID(r))
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, out)
}

func (c *WorkItemsController) audit(w http.ResponseWriter, r *http.Request) {
	if !c.available(w, r, "GET", "/api/v1/projects/{id}/workitems/audit") {
		return
	}
	limit, _ := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("limit")))
	out, err := c.Svc.ListWorkItemsAudit(r.Context(), projectID(r), limit)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, out)
}

func (c *WorkItemsController) links(w http.ResponseWriter, r *http.Request) {
	if !c.available(w, r, "GET", "/api/v1/projects/{id}/workitems/links") {
		return
	}
	// Refreshing from the provider is opt-in per request. A list that always
	// probed would be as slow as the provider on every page load, and the
	// cached values are usually what a person wants anyway.
	live := strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("live")), "true")
	out, err := c.Svc.ListWorkItemLinks(r.Context(), projectID(r), live)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, out)
}

func (c *WorkItemsController) link(w http.ResponseWriter, r *http.Request) {
	if !c.available(w, r, "POST", "/api/v1/projects/{id}/workitems/links") {
		return
	}
	var req WorkItemLinkRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request",
			"INVALID_BODY", "request body is not valid JSON", nil)
		return
	}
	req.ProjectID = projectID(r)
	req.Actor = actorOf(r)
	out, err := c.Svc.CreateWorkItemLink(r.Context(), req)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, out)
}

func (c *WorkItemsController) unlink(w http.ResponseWriter, r *http.Request) {
	if !c.available(w, r, "DELETE", "/api/v1/projects/{id}/workitems/links/{linkId}") {
		return
	}
	err := c.Svc.DeleteWorkItemLink(r.Context(), projectID(r), chi.URLParam(r, "linkId"))
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (c *WorkItemsController) setLinkSync(w http.ResponseWriter, r *http.Request) {
	if !c.available(w, r, "POST", "/api/v1/projects/{id}/workitems/links/{linkId}/sync") {
		return
	}
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request",
			"INVALID_BODY", "request body is not valid JSON", nil)
		return
	}
	err := c.Svc.SetWorkItemLinkSync(r.Context(), projectID(r), chi.URLParam(r, "linkId"), body.Enabled)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (c *WorkItemsController) sync(w http.ResponseWriter, r *http.Request) {
	if !c.available(w, r, "POST", "/api/v1/projects/{id}/workitems/sync") {
		return
	}
	out, err := c.Svc.SyncWorkItems(r.Context(), projectID(r))
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, out)
}

// actorOf names the account making a link, for the audit trail.
//
// It is best-effort and never blocks the request: the routes are already
// authorized, so an unattributable link is a link with a missing byline rather
// than one that should be refused. A loopback caller with no identity is
// exactly that case.
func actorOf(r *http.Request) string {
	if user, ok := identity.FromContext(r.Context()); ok {
		if user.Email != "" {
			return user.Email
		}
		return string(user.ID)
	}
	return ""
}

// WorkItemsLinkIDParam documents the {linkId} path parameter for the generated
// spec. It is never decoded from; the handlers read chi.URLParam directly, the
// way every other path-parameter route in this package does.
type WorkItemsLinkIDParam struct {
	LinkID string `path:"linkId" description:"The work item link's id."`
}

// WorkItemsLinksQuery documents the links route's query string.
type WorkItemsLinksQuery struct {
	Live bool `query:"live,omitempty" description:"Refresh each link from the provider. Slower, and degrades to the cached values when the provider cannot be reached."`
}

// WorkItemsAuditQuery documents the audit route's query string.
type WorkItemsAuditQuery struct {
	Limit int `query:"limit,omitempty" description:"How many recent operations to return. Defaults to 50."`
}

// WorkItemLinkSyncRequest is the per-link sync switch's body.
type WorkItemLinkSyncRequest struct {
	Enabled bool `json:"enabled"`
}
