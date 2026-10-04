package project

import "github.com/aoagents/agent-orchestrator/backend/internal/domain"

// GetResult is the discriminated result returned by Service.Get.
type GetResult struct {
	Status   string
	Project  *Project
	Degraded *Degraded
}

// AddInput is the body shape for POST /api/v1/projects.
type AddInput struct {
	Path        string                `json:"path"`
	ProjectID   *string               `json:"projectId,omitempty"`
	Name        *string               `json:"name,omitempty"`
	Config      *domain.ProjectConfig `json:"config,omitempty"`
	AsWorkspace bool                  `json:"asWorkspace,omitempty"`
	// TenantID is the organization to register the project in (P4-C).
	//
	// Consumed by the HTTP controller, not by this service -- tenancy is
	// stamped alongside ownership, and for the same reason: both are facts
	// about WHO registered the project, which the transport layer knows and
	// the project service deliberately does not. Omitted is the common case:
	// a caller who belongs to exactly one organization gets that one, which
	// is what keeps a single-organization installation from ever seeing a
	// picker.
	TenantID string `json:"tenantId,omitempty"`
}

// InitializeRepositoryInput is the body shape for POST /api/v1/projects/initialize.
type InitializeRepositoryInput struct {
	Path string `json:"path"`
}

// InitializeRepositoryResult reports the repository path initialized for onboarding.
type InitializeRepositoryResult struct {
	Path string `json:"path"`
}

// UpdateSettingsInput is the body shape for PUT /api/v1/projects/{id}. It
// atomically replaces the user-facing display name and per-project config.
type UpdateSettingsInput struct {
	DisplayName string               `json:"displayName" minLength:"1" maxLength:"20"`
	Config      domain.ProjectConfig `json:"config"`
}

// SetConfigInput is the body shape for PUT /api/v1/projects/{id}/config. Config
// replaces the project's stored config wholesale; a zero-value config clears it.
type SetConfigInput struct {
	Config domain.ProjectConfig `json:"config"`
}

// RemoveResult reports what DELETE /api/v1/projects/{id} actually did.
type RemoveResult struct {
	ProjectID         domain.ProjectID `json:"projectId"`
	RemovedStorageDir bool             `json:"removedStorageDir"`
}

// CloneInput is the body shape for POST /api/v1/projects/clone. Repo accepts
// either an "owner/repo" slug or an "https://github.com/owner/repo" URL.
type CloneInput struct {
	Repo            string  `json:"repo"`
	DestinationName *string `json:"destinationName,omitempty"`
	// TenantID is the organization to register the clone in. See AddInput.
	TenantID string `json:"tenantId,omitempty"`
}

// BrowseResult is the response shape for GET /api/v1/projects/browse.
type BrowseResult struct {
	Path    string        `json:"path"`
	Entries []BrowseEntry `json:"entries"`
}
