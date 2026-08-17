package controllers

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/identity"
	providerprofilesvc "github.com/aoagents/agent-orchestrator/backend/internal/service/providerprofile"
)

// ProviderProfileIDParam is the {id} path parameter for provider-profile routes.
type ProviderProfileIDParam struct {
	ID string `path:"id" description:"Provider profile identifier."`
}

// ProviderProfileView is the wire shape of a domain.ProviderProfile. It
// never includes SecretCiphertext -- no provider today stores one, and this
// type is the only place a profile is ever rendered onto the wire, so a
// future secret-backed provider adding SecretCiphertext to the domain type
// still can't leak it here by omission.
type ProviderProfileView struct {
	ID           string   `json:"id"`
	Provider     string   `json:"provider"`
	Harness      string   `json:"harness"`
	DisplayName  string   `json:"displayName"`
	Enabled      bool     `json:"enabled"`
	AuthState    string   `json:"authState" enum:"unknown,authenticated,unauthenticated,error"`
	AuthMethod   string   `json:"authMethod" enum:"browser_oauth,device_flow,cli_bootstrap,api_key,external_login,unsupported"`
	DefaultModel string   `json:"defaultModel,omitempty"`
	Capabilities []string `json:"capabilities"`
	CreatedAt    string   `json:"createdAt"`
	UpdatedAt    string   `json:"updatedAt"`
}

func providerProfileView(p domain.ProviderProfile) ProviderProfileView {
	caps := make([]string, 0, len(p.Capabilities))
	for _, c := range p.Capabilities {
		caps = append(caps, string(c))
	}
	return ProviderProfileView{
		ID:           string(p.ID),
		Provider:     p.Provider,
		Harness:      string(p.Harness),
		DisplayName:  p.DisplayName,
		Enabled:      p.Enabled,
		AuthState:    string(p.AuthState),
		AuthMethod:   string(p.AuthMethod),
		DefaultModel: p.DefaultModel,
		Capabilities: caps,
		CreatedAt:    p.CreatedAt.UTC().Format(rfc3339Milli),
		UpdatedAt:    p.UpdatedAt.UTC().Format(rfc3339Milli),
	}
}

// ProviderDescriptorView is the wire shape of a registry entry.
type ProviderDescriptorView struct {
	Provider     string   `json:"provider"`
	Harness      string   `json:"harness"`
	DisplayName  string   `json:"displayName"`
	Capabilities []string `json:"capabilities"`
	AuthMethods  []string `json:"authMethods"`
	Models       []string `json:"models"`
	Available    bool     `json:"available"`
	Unavailable  string   `json:"unavailable,omitempty"`
}

func providerDescriptorView(d domain.ProviderAdapterDescriptor) ProviderDescriptorView {
	caps := make([]string, 0, len(d.Capabilities))
	for _, c := range d.Capabilities {
		caps = append(caps, string(c))
	}
	methods := make([]string, 0, len(d.AuthMethods))
	for _, m := range d.AuthMethods {
		methods = append(methods, string(m))
	}
	return ProviderDescriptorView{
		Provider:     d.Provider,
		Harness:      string(d.Harness),
		DisplayName:  d.DisplayName,
		Capabilities: caps,
		AuthMethods:  methods,
		Models:       d.Models,
		Available:    d.Available,
		Unavailable:  d.Unavailable,
	}
}

// ListProviderProfilesResponse is the body of GET /api/v1/provider-profiles.
type ListProviderProfilesResponse struct {
	Profiles []ProviderProfileView `json:"profiles"`
}

// ProviderProfileResponse wraps a single profile.
type ProviderProfileResponse struct {
	Profile ProviderProfileView `json:"profile"`
}

// ProviderRegistryResponse is the body of GET /api/v1/providers/registry.
type ProviderRegistryResponse struct {
	Providers []ProviderDescriptorView `json:"providers"`
}

// CreateProviderProfileRequest is the body of POST /api/v1/provider-profiles.
type CreateProviderProfileRequest struct {
	Provider     string `json:"provider"`
	Harness      string `json:"harness"`
	DisplayName  string `json:"displayName"`
	DefaultModel string `json:"defaultModel,omitempty"`
}

// UpdateProviderProfileRequest is the body of PATCH/PUT
// /api/v1/provider-profiles/{id}.
type UpdateProviderProfileRequest struct {
	DisplayName  string `json:"displayName"`
	Enabled      bool   `json:"enabled"`
	DefaultModel string `json:"defaultModel,omitempty"`
}

// TestProviderProfileResponse is the body of a successful test/connect call.
type TestProviderProfileResponse struct {
	Profile ProviderProfileView `json:"profile"`
	OK      bool                `json:"ok"`
	Message string              `json:"message"`
}

// ProviderProfilesController owns the /provider-profiles and
// /providers/registry routes (Checkpoint 8P-B). Every handler resolves the
// current user via identity.Require and passes it explicitly into Mgr --
// this controller never trusts a user id from the request body/path/query.
// A nil Mgr keeps routes registered but answers OpenAPI-backed 501s.
type ProviderProfilesController struct {
	Mgr providerprofilesvc.Manager
}

// Register mounts the provider-profile routes on the supplied router.
func (c *ProviderProfilesController) Register(r chi.Router) {
	r.Get("/providers/registry", c.registry)
	r.Get("/provider-profiles", c.list)
	r.Post("/provider-profiles", c.create)
	r.Get("/provider-profiles/{id}", c.get)
	r.Patch("/provider-profiles/{id}", c.update)
	r.Post("/provider-profiles/{id}/connect", c.connect)
	r.Post("/provider-profiles/{id}/disconnect", c.disconnect)
	r.Post("/provider-profiles/{id}/test", c.test)
}

func (c *ProviderProfilesController) registry(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, http.MethodGet, "/api/v1/providers/registry")
		return
	}
	descs := c.Mgr.Registry(r.Context())
	views := make([]ProviderDescriptorView, 0, len(descs))
	for _, d := range descs {
		views = append(views, providerDescriptorView(d))
	}
	envelope.WriteJSON(w, http.StatusOK, ProviderRegistryResponse{Providers: views})
}

func (c *ProviderProfilesController) list(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, http.MethodGet, "/api/v1/provider-profiles")
		return
	}
	user, err := identity.Require(r)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	profiles, err := c.Mgr.List(r.Context(), user.ID)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	views := make([]ProviderProfileView, 0, len(profiles))
	for _, p := range profiles {
		views = append(views, providerProfileView(p))
	}
	envelope.WriteJSON(w, http.StatusOK, ListProviderProfilesResponse{Profiles: views})
}

func (c *ProviderProfilesController) create(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, http.MethodPost, "/api/v1/provider-profiles")
		return
	}
	user, err := identity.Require(r)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	var in CreateProviderProfileRequest
	if err := decodeJSONStrict(r, &in); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid JSON body", nil)
		return
	}
	p, err := c.Mgr.Create(r.Context(), user.ID, providerprofilesvc.CreateInput{
		Provider:     in.Provider,
		Harness:      domain.AgentHarness(in.Harness),
		DisplayName:  in.DisplayName,
		DefaultModel: in.DefaultModel,
	})
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusCreated, ProviderProfileResponse{Profile: providerProfileView(p)})
}

func (c *ProviderProfilesController) get(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, http.MethodGet, "/api/v1/provider-profiles/{id}")
		return
	}
	user, err := identity.Require(r)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	p, err := c.Mgr.Get(r.Context(), user.ID, providerProfileID(r))
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, ProviderProfileResponse{Profile: providerProfileView(p)})
}

func (c *ProviderProfilesController) update(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, r.Method, "/api/v1/provider-profiles/{id}")
		return
	}
	user, err := identity.Require(r)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	var in UpdateProviderProfileRequest
	if err := decodeJSONStrict(r, &in); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid JSON body", nil)
		return
	}
	p, err := c.Mgr.Update(r.Context(), user.ID, providerProfileID(r), providerprofilesvc.UpdateInput{
		DisplayName:  in.DisplayName,
		Enabled:      in.Enabled,
		DefaultModel: in.DefaultModel,
	})
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, ProviderProfileResponse{Profile: providerProfileView(p)})
}

func (c *ProviderProfilesController) connect(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, http.MethodPost, "/api/v1/provider-profiles/{id}/connect")
		return
	}
	user, err := identity.Require(r)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	p, err := c.Mgr.Connect(r.Context(), user.ID, providerProfileID(r))
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, ProviderProfileResponse{Profile: providerProfileView(p)})
}

func (c *ProviderProfilesController) disconnect(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, http.MethodPost, "/api/v1/provider-profiles/{id}/disconnect")
		return
	}
	user, err := identity.Require(r)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	p, err := c.Mgr.Disconnect(r.Context(), user.ID, providerProfileID(r))
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, ProviderProfileResponse{Profile: providerProfileView(p)})
}

func (c *ProviderProfilesController) test(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, http.MethodPost, "/api/v1/provider-profiles/{id}/test")
		return
	}
	user, err := identity.Require(r)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	id := providerProfileID(r)
	result, err := c.Mgr.Test(r.Context(), user.ID, id)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	p, err := c.Mgr.Get(r.Context(), user.ID, id)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, TestProviderProfileResponse{
		Profile: providerProfileView(p),
		OK:      result.OK,
		Message: result.Message,
	})
}

func providerProfileID(r *http.Request) domain.ProviderProfileID {
	return domain.ProviderProfileID(chi.URLParam(r, "id"))
}
