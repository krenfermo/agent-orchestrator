package workitems

import (
	"context"
	"errors"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// api.go — the projection onto the controller's DTOs.
//
// It is a separate file, and a thin one, on purpose: the service's own types
// are shaped for the work, and the wire types are shaped for the client. The
// one thing this layer genuinely DECIDES is what not to send — and the answer
// is the credential, which is not on any type here to be forgotten.

// WorkItemsConfig implements the controller's config read.
func (s *Service) WorkItemsConfig(
	ctx context.Context, projectID domain.ProjectID,
) (controllers.WorkItemsConfigResponse, error) {
	view, err := s.Config(ctx, projectID)
	if err != nil {
		return controllers.WorkItemsConfigResponse{}, err
	}
	return configResponse(view), nil
}

// PutWorkItemsConfig implements the controller's config write.
func (s *Service) PutWorkItemsConfig(
	ctx context.Context, projectID domain.ProjectID, req controllers.WorkItemsConfigUpdate,
) (controllers.WorkItemsConfigResponse, error) {
	view, err := s.PutConfig(ctx, projectID, ConfigUpdate{
		BaseURL:           req.BaseURL,
		Workspace:         req.Workspace,
		ExternalProjectID: req.ExternalProjectID,
		APIToken:          req.APIToken,
		Enabled:           req.Enabled,
		SyncStates:        req.SyncStates,
		SyncComments:      req.SyncComments,
	})
	if err != nil {
		return controllers.WorkItemsConfigResponse{}, err
	}
	return configResponse(view), nil
}

// DeleteWorkItemsConfig implements the controller's disconnect.
func (s *Service) DeleteWorkItemsConfig(ctx context.Context, projectID domain.ProjectID) error {
	return s.DeleteConfig(ctx, projectID)
}

// TestWorkItemsConnection implements the controller's connection test.
func (s *Service) TestWorkItemsConnection(
	ctx context.Context, projectID domain.ProjectID,
) (controllers.WorkItemsConnectionResponse, error) {
	identity, err := s.TestConnection(ctx, projectID)
	if err != nil {
		return controllers.WorkItemsConnectionResponse{}, providerAPIError(err)
	}
	return controllers.WorkItemsConnectionResponse{
		Provider:      string(identity.Provider),
		Workspace:     identity.Workspace,
		WorkspaceName: identity.WorkspaceName,
		Projects:      identity.Projects,
	}, nil
}

// ListWorkItemsProviderProjects implements the mapping picker.
func (s *Service) ListWorkItemsProviderProjects(
	ctx context.Context, projectID domain.ProjectID,
) (controllers.WorkItemsProviderProjectsResponse, error) {
	projects, err := s.ListProviderProjects(ctx, projectID)
	if err != nil {
		return controllers.WorkItemsProviderProjectsResponse{}, providerAPIError(err)
	}
	out := controllers.WorkItemsProviderProjectsResponse{
		Projects: make([]controllers.WorkItemsProviderProject, 0, len(projects)),
	}
	for _, p := range projects {
		out.Projects = append(out.Projects, controllers.WorkItemsProviderProject{
			ID: p.ID, Name: p.Name, Identifier: p.Identifier, Description: p.Description,
		})
	}
	return out, nil
}

// ListWorkItemLinks implements the controller's link list.
func (s *Service) ListWorkItemLinks(
	ctx context.Context, projectID domain.ProjectID, live bool,
) (controllers.WorkItemLinksResponse, error) {
	views, err := s.Links(ctx, projectID, live)
	if err != nil {
		return controllers.WorkItemLinksResponse{}, err
	}
	out := controllers.WorkItemLinksResponse{
		Links: make([]controllers.WorkItemLinkResponse, 0, len(views)),
	}
	for _, v := range views {
		out.Links = append(out.Links, linkResponse(v))
	}
	return out, nil
}

// CreateWorkItemLink implements the controller's link/create.
func (s *Service) CreateWorkItemLink(
	ctx context.Context, req controllers.WorkItemLinkRequest,
) (controllers.WorkItemLinkResponse, error) {
	view, err := s.Link(ctx, LinkRequest{
		ProjectID:   req.ProjectID,
		Scope:       domain.WorkItemLinkScope(req.Scope),
		ScopeID:     req.ScopeID,
		Reference:   req.Reference,
		Create:      req.Create,
		Title:       req.Title,
		Description: req.Description,
		SyncEnabled: req.SyncEnabled,
		Actor:       req.Actor,
	})
	if err != nil {
		return controllers.WorkItemLinkResponse{}, providerAPIError(err)
	}
	return linkResponse(view), nil
}

// DeleteWorkItemLink implements the controller's unlink.
func (s *Service) DeleteWorkItemLink(ctx context.Context, projectID domain.ProjectID, linkID string) error {
	return s.Unlink(ctx, projectID, linkID)
}

// SetWorkItemLinkSync implements the controller's per-link sync switch.
func (s *Service) SetWorkItemLinkSync(
	ctx context.Context, projectID domain.ProjectID, linkID string, enabled bool,
) error {
	return s.SetLinkSync(ctx, projectID, linkID, enabled)
}

// WorkItemsHealth implements the controller's health read.
func (s *Service) WorkItemsHealth(
	ctx context.Context, projectID domain.ProjectID,
) (controllers.WorkItemsHealthResponse, error) {
	h, err := s.Health(ctx, projectID)
	if err != nil {
		return controllers.WorkItemsHealthResponse{}, err
	}
	return controllers.WorkItemsHealthResponse{
		Configured: h.Configured, Enabled: h.Enabled, Connected: h.Connected,
		Degraded: h.Degraded, Pending: h.Pending, Failed: h.Failed, Links: h.Links,
		LastCheckAt: h.LastCheckAt, LastCheckError: h.LastCheckError,
	}, nil
}

// SyncWorkItems implements the controller's forced drain.
//
// It drains the WHOLE queue rather than only this project's rows, which is
// deliberate: the outbox is small, the operation is idempotent, and giving each
// project its own partial drain would mean a person pressing "sync" on one
// project could leave another's rows sitting behind a backoff they cannot see.
// The authorization is still per project — a caller has to be able to reach a
// project to press the button at all.
func (s *Service) SyncWorkItems(
	ctx context.Context, projectID domain.ProjectID,
) (controllers.WorkItemsSyncResponse, error) {
	out, err := s.SyncOnce(ctx, DefaultBatchSize)
	if err != nil {
		return controllers.WorkItemsSyncResponse{}, err
	}
	_ = projectID
	return controllers.WorkItemsSyncResponse{
		Claimed: out.Claimed, Delivered: out.Delivered,
		Deferred: out.Deferred, Failed: out.Failed, Skipped: out.Skipped,
	}, nil
}

// ListWorkItemsAudit implements the controller's audit read.
func (s *Service) ListWorkItemsAudit(
	ctx context.Context, projectID domain.ProjectID, limit int,
) (controllers.WorkItemsAuditResponse, error) {
	rows, err := s.Audit(ctx, projectID, limit)
	if err != nil {
		return controllers.WorkItemsAuditResponse{}, err
	}
	out := controllers.WorkItemsAuditResponse{
		Entries: make([]controllers.WorkItemsAuditEntry, 0, len(rows)),
	}
	for _, row := range rows {
		out.Entries = append(out.Entries, controllers.WorkItemsAuditEntry{
			ID: row.ID, Provider: string(row.Provider), Operation: row.Operation,
			ExternalItemID: row.ExternalItemID, ExternalItemKey: row.ExternalItemKey,
			Outcome: row.Outcome, ErrorKind: row.ErrorKind, Detail: row.Detail,
			Attempts: row.Attempts, DurationMS: row.DurationMS,
			CreatedAt: row.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	return out, nil
}

func configResponse(v ConfigView) controllers.WorkItemsConfigResponse {
	return controllers.WorkItemsConfigResponse{
		ProjectID:           v.ProjectID,
		Provider:            v.Provider,
		BaseURL:             v.BaseURL,
		Workspace:           v.Workspace,
		ExternalProjectID:   v.ExternalProjectID,
		ExternalProjectName: v.ExternalProjectName,
		ExternalProjectKey:  v.ExternalProjectKey,
		TokenConfigured:     v.TokenConfigured,
		TokenFromEnv:        v.TokenFromEnv,
		Enabled:             v.Enabled,
		SyncStates:          v.SyncStates,
		SyncComments:        v.SyncComments,
		Connected:           v.Connected,
		Degraded:            v.Degraded,
		LastCheckAt:         v.LastCheckAt,
		LastCheckError:      v.LastCheckError,
	}
}

// linkResponse renders one link, preferring live values and falling back to the
// cache with Stale set — which is the §13 "degraded but useful" state the UI
// needs in order to say something true when the provider is down.
func linkResponse(v LinkView) controllers.WorkItemLinkResponse {
	out := controllers.WorkItemLinkResponse{
		ID: v.ID, ProjectID: string(v.ProjectID),
		Scope: string(v.Scope), ScopeID: v.ScopeID,
		Provider: string(v.Ref.Provider), Workspace: v.Ref.Workspace,
		ExternalProjectID: v.Ref.Project, ExternalItemID: v.Ref.ID,
		ExternalItemKey: v.Ref.Key, URL: v.URL,
		Origin: string(v.Origin), SyncEnabled: v.SyncEnabled,
		Title: v.LastSeenTitle, State: string(v.LastSeenState),
		Stale: true, LiveError: v.LiveError,
		Readiness: string(v.Readiness),
		CreatedAt: v.CreatedAt.UTC().Format(time.RFC3339),
	}
	if !v.LastSeenAt.IsZero() {
		out.LastSeenAt = v.LastSeenAt.UTC().Format(time.RFC3339)
	}
	if v.Live != nil {
		out.Title = v.Live.Title
		out.State = string(v.Live.StateGroup)
		out.StateName = v.Live.StateName
		out.Stale = false
		if v.Live.URL != "" {
			out.URL = v.Live.URL
		}
		if v.Live.Ref.Key != "" {
			out.ExternalItemKey = v.Live.Ref.Key
		}
	}
	return out
}

// providerAPIError turns a provider failure into an HTTP-shaped one.
//
// The mapping exists so a person pressing "test connection" with a bad token
// sees a 400 saying the token was rejected, rather than a 500 that reads like
// AO broke. A transient failure stays a 502: it is genuinely not the caller's
// fault, and the distinction is what tells somebody whether to fix something or
// try again.
func providerAPIError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrNotConfigured) {
		return apierr.Invalid("PLANE_NOT_CONFIGURED",
			"this project has no work-management provider configured", nil)
	}
	var wErr *ports.WorkItemsError
	if !errors.As(err, &wErr) {
		return err
	}
	switch wErr.Kind {
	case ports.WorkItemsErrAuth:
		return apierr.Invalid("PLANE_AUTH_FAILED", wErr.Message, nil)
	case ports.WorkItemsErrNotFound:
		return apierr.NotFound("PLANE_NOT_FOUND", wErr.Message)
	case ports.WorkItemsErrInvalid:
		return apierr.Invalid("PLANE_REQUEST_REJECTED", wErr.Message, nil)
	case ports.WorkItemsErrNotConfigured:
		return apierr.Invalid("PLANE_NOT_CONFIGURED", wErr.Message, nil)
	default:
		// Rate limited and unavailable are both "try again", and both are the
		// provider's problem rather than the caller's. Reported as a conflict
		// rather than a 500: AO is fine, the third party is not, and a person
		// pressing the button again in a minute is the right next step.
		return apierr.Conflict("PLANE_UNAVAILABLE", wErr.Message, nil)
	}
}
