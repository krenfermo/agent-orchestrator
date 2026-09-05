package workitems

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// links.go — reading the integration's state, and creating/removing links
// (P4-E §5, §6).

func osGetenv(k string) string { return os.Getenv(k) }

// ConfigView is what the settings surface is shown. It carries NO credential:
// only whether one is stored.
//
// That is a property of the type rather than of the handler that renders it —
// there is no field a future controller could accidentally serialize, which is
// a stronger guarantee than remembering to strip one.
type ConfigView struct {
	ProjectID string `json:"projectId"`
	Provider  string `json:"provider"`
	// BaseURL is the configured origin, or empty for the provider default.
	BaseURL   string `json:"baseUrl,omitempty"`
	Workspace string `json:"workspace,omitempty"`

	ExternalProjectID   string `json:"externalProjectId,omitempty"`
	ExternalProjectName string `json:"externalProjectName,omitempty"`
	ExternalProjectKey  string `json:"externalProjectKey,omitempty"`

	// TokenConfigured says a credential exists. The credential itself never
	// leaves the process.
	TokenConfigured bool `json:"tokenConfigured"`
	// TokenFromEnv says the credential comes from the process environment
	// rather than the database, which an operator debugging "why is it still
	// connecting after I cleared the token" needs to be told.
	TokenFromEnv bool `json:"tokenFromEnv"`

	Enabled      bool `json:"enabled"`
	SyncStates   bool `json:"syncStates"`
	SyncComments bool `json:"syncComments"`
	// Connected is whether the last preflight succeeded.
	Connected      bool   `json:"connected"`
	LastCheckAt    string `json:"lastCheckAt,omitempty"`
	LastCheckError string `json:"lastCheckError,omitempty"`
	// Degraded is the §13 signal: configured and switched on, but the last
	// thing AO tried did not work. The UI shows external sync as degraded
	// rather than hiding it or claiming health.
	Degraded bool `json:"degraded"`
}

func (s *Service) viewOf(ctx context.Context, cfg store.WorkItemConfig) ConfigView {
	view := ConfigView{
		ProjectID:           string(cfg.ProjectID),
		Provider:            string(cfg.Provider),
		BaseURL:             cfg.BaseURL,
		Workspace:           cfg.Workspace,
		ExternalProjectID:   cfg.ExternalProjectID,
		ExternalProjectName: cfg.ExternalProjectName,
		ExternalProjectKey:  cfg.ExternalProjectKey,
		TokenConfigured:     cfg.APITokenEncrypted != "",
		Enabled:             cfg.Enabled,
		SyncStates:          cfg.SyncStates,
		SyncComments:        cfg.SyncComments,
		Connected:           cfg.LastCheckOK,
		LastCheckError:      cfg.LastCheckError,
	}
	if view.Provider == "" {
		view.Provider = string(domain.WorkItemProviderPlane)
	}
	if !cfg.LastCheckAt.IsZero() {
		view.LastCheckAt = cfg.LastCheckAt.UTC().Format(time.RFC3339)
	}
	if !view.TokenConfigured && strings.TrimSpace(s.env(EnvAPIToken)) != "" {
		view.TokenConfigured, view.TokenFromEnv = true, true
	}
	if view.BaseURL == "" {
		view.BaseURL = strings.TrimSpace(s.env(EnvBaseURL))
	}
	if view.Workspace == "" {
		view.Workspace = strings.TrimSpace(s.env(EnvWorkspace))
	}
	if view.ExternalProjectID == "" {
		view.ExternalProjectID = strings.TrimSpace(s.env(EnvProject))
	}
	view.Degraded = view.Enabled && !cfg.LastCheckAt.IsZero() && !cfg.LastCheckOK
	_ = ctx
	return view
}

// Config reads a project's configuration for display.
func (s *Service) Config(ctx context.Context, projectID domain.ProjectID) (ConfigView, error) {
	cfg, found, err := s.store.GetWorkItemConfig(ctx, projectID)
	if err != nil {
		return ConfigView{}, err
	}
	if !found {
		cfg = store.WorkItemConfig{
			ProjectID: projectID, Provider: domain.WorkItemProviderPlane,
			SyncStates: true, SyncComments: true,
		}
	}
	return s.viewOf(ctx, cfg), nil
}

// TestConnection performs a preflight and records the result.
//
// It is the "test connection" button, and it deliberately RECORDS its outcome:
// a person who tested successfully and walked away should find the settings
// page still saying so, and a background sync that starts failing should be
// able to mark the integration degraded through the same field.
func (s *Service) TestConnection(ctx context.Context, projectID domain.ProjectID) (ports.WorkItemsIdentity, error) {
	client, cfg, err := s.client(ctx, projectID)
	if err != nil {
		if errors.Is(err, ErrNotConfigured) {
			return ports.WorkItemsIdentity{}, apierr.Invalid("PLANE_NOT_CONFIGURED",
				"this project has no work-management provider configured", nil)
		}
		return ports.WorkItemsIdentity{}, err
	}
	started := s.now()
	probeCtx, cancel := context.WithTimeout(ctx, providerTimeout)
	defer cancel()

	identity, probeErr := client.Preflight(probeCtx)
	now := s.now()
	detail := ""
	if probeErr != nil {
		detail = providerMessage(probeErr)
	}
	if err := s.store.SetWorkItemConfigCheck(ctx, projectID, probeErr == nil, detail, now); err != nil {
		s.log.Warn("work items: could not record the connection check", "project", projectID, "err", err)
	}
	s.audit(ctx, store.WorkItemAuditRow{
		ProjectID: projectID, Provider: cfg.Provider, Operation: "preflight",
		Outcome:   outcomeFor(probeErr),
		ErrorKind: errorKind(probeErr), Detail: detail,
		DurationMS: now.Sub(started).Milliseconds(),
	})
	if probeErr != nil {
		return ports.WorkItemsIdentity{}, probeErr
	}
	return identity, nil
}

// ListProviderProjects enumerates the provider's projects, so a person mapping
// this AO project can choose from a list.
func (s *Service) ListProviderProjects(ctx context.Context, projectID domain.ProjectID) ([]domain.WorkItemProject, error) {
	client, _, err := s.client(ctx, projectID)
	if err != nil {
		return nil, err
	}
	callCtx, cancel := context.WithTimeout(ctx, providerTimeout)
	defer cancel()
	return client.ListProjects(callCtx)
}

// LinkRequest links AO work to an external item.
//
// Exactly one of Reference and Create is meaningful: either the person is
// naming an item that already exists, or asking AO to make one.
type LinkRequest struct {
	ProjectID domain.ProjectID
	Scope     domain.WorkItemLinkScope
	ScopeID   string
	// Reference is what the person typed: "PROJ-123" or a Plane URL.
	Reference string
	// Create asks AO to create the item instead of resolving one.
	Create bool
	// Title and Description are used only when Create is set.
	Title       string
	Description string
	// SyncEnabled is whether AO may push its state to this item once linked.
	SyncEnabled bool
	// Actor is the account making the link, for the audit trail.
	Actor string
}

// LinkView is one link plus whatever AO currently knows about the item.
type LinkView struct {
	domain.WorkItemLink
	// URL is the browser link, rebuilt from the configuration rather than
	// stored: a self-hosted Plane that moves origin should not leave every
	// stored link pointing at the old host.
	URL string `json:"url,omitempty"`
	// Live is the current item when the provider could be reached, and nil
	// when it could not. A nil Live with a populated LastSeen* is exactly the
	// degraded-but-useful state §13 asks the UI to show.
	Live *domain.WorkItem `json:"live,omitempty"`
	// LiveError says why Live is absent, in words a person can act on.
	LiveError string `json:"liveError,omitempty"`
	// Readiness is what the external plan SUGGESTS about this work. Advisory
	// only — nothing in AO reads it.
	Readiness domain.PlanningReadiness `json:"readiness,omitempty"`
}

// Link creates or replaces the link for one AO thing.
func (s *Service) Link(ctx context.Context, req LinkRequest) (LinkView, error) {
	if !domain.ValidWorkItemLinkScope(req.Scope) {
		return LinkView{}, apierr.Invalid("PLANE_SCOPE_INVALID", "unknown link scope "+string(req.Scope), nil)
	}
	client, cfg, err := s.client(ctx, req.ProjectID)
	if err != nil {
		if errors.Is(err, ErrNotConfigured) {
			return LinkView{}, apierr.Invalid("PLANE_NOT_CONFIGURED",
				"this project has no work-management provider configured", nil)
		}
		return LinkView{}, err
	}

	callCtx, cancel := context.WithTimeout(ctx, providerTimeout)
	defer cancel()

	var (
		item   domain.WorkItem
		origin = domain.WorkItemLinkManual
	)
	started := s.now()
	if req.Create {
		if strings.TrimSpace(req.Title) == "" {
			return LinkView{}, apierr.Invalid("PLANE_TITLE_REQUIRED",
				"a created work item needs a title", nil)
		}
		externalID := domain.ExternalIDFor(req.Scope, req.ScopeID)
		if externalID == "" {
			return LinkView{}, apierr.Invalid("PLANE_SCOPE_INVALID",
				"a project-scoped link cannot create a work item; link an existing one instead", nil)
		}
		item, err = client.Create(callCtx, ports.WorkItemCreateRequest{
			ProjectID:   cfg.ExternalProjectID,
			Title:       req.Title,
			Description: req.Description,
			ExternalID:  externalID,
		})
		origin = domain.WorkItemLinkCreated
	} else {
		if strings.TrimSpace(req.Reference) == "" {
			return LinkView{}, apierr.Invalid("PLANE_REFERENCE_REQUIRED",
				`give a work item reference like "PROJ-123", or ask AO to create one`, nil)
		}
		item, err = client.Resolve(callCtx, req.Reference)
	}
	op := "link"
	if req.Create {
		op = "create"
	}
	s.audit(ctx, store.WorkItemAuditRow{
		ProjectID: req.ProjectID, Provider: cfg.Provider, Operation: op,
		ExternalItemID: item.Ref.ID, ExternalItemKey: item.Ref.Key,
		Outcome:   outcomeFor(err),
		ErrorKind: errorKind(err), Detail: providerMessage(err),
		DurationMS: s.now().Sub(started).Milliseconds(),
	})
	if err != nil {
		return LinkView{}, err
	}

	// A resolved item from another project in the same workspace is a real
	// possibility and is refused: a link whose item lives outside the mapped
	// project would have AO writing into a project nobody mapped.
	if cfg.ExternalProjectID != "" && item.Ref.Project != "" && item.Ref.Project != cfg.ExternalProjectID {
		return LinkView{}, apierr.Invalid("PLANE_PROJECT_MISMATCH",
			"that work item belongs to a different Plane project than the one this AO project is mapped to", nil)
	}

	now := s.now()
	link := domain.WorkItemLink{
		ID:            s.mintID(),
		ProjectID:     req.ProjectID,
		Scope:         req.Scope,
		ScopeID:       strings.TrimSpace(req.ScopeID),
		Ref:           item.Ref,
		Origin:        origin,
		SyncEnabled:   req.SyncEnabled,
		LastSeenTitle: item.Title,
		LastSeenState: item.StateGroup,
		LastSeenAt:    now,
		CreatedBy:     req.Actor,
		CreatedAt:     now,
	}
	if link.Ref.Provider == "" {
		link.Ref.Provider = cfg.Provider
	}
	// Preserve the original creation time when replacing an existing link, so
	// "linked since" stays true across a re-link.
	if existing, found, gErr := s.store.GetWorkItemLinkByScope(ctx, req.ProjectID, req.Scope, link.ScopeID); gErr == nil && found {
		link.ID, link.CreatedAt = existing.ID, existing.CreatedAt
		if existing.CreatedBy != "" && req.Actor == "" {
			link.CreatedBy = existing.CreatedBy
		}
	}
	if err := s.store.PutWorkItemLink(ctx, link, now); err != nil {
		return LinkView{}, err
	}
	return LinkView{
		WorkItemLink: link, URL: item.URL, Live: &item,
		Readiness: domain.ReadinessOf(item.StateGroup),
	}, nil
}

// Unlink removes a link. It does NOT touch the external item: unlinking is AO
// forgetting an association, and deleting somebody's planning item because a
// link was removed would be a destructive surprise.
func (s *Service) Unlink(ctx context.Context, projectID domain.ProjectID, linkID string) error {
	n, err := s.store.DeleteWorkItemLink(ctx, projectID, linkID)
	if err != nil {
		return err
	}
	if n == 0 {
		return apierr.NotFound("PLANE_LINK_NOT_FOUND", "no such work item link in this project")
	}
	s.audit(ctx, store.WorkItemAuditRow{
		ProjectID: projectID, LinkID: linkID, Operation: "unlink",
		Outcome: store.WorkItemAuditOK,
	})
	return nil
}

// SetLinkSync turns state/comment pushing on or off for one link.
func (s *Service) SetLinkSync(ctx context.Context, projectID domain.ProjectID, linkID string, enabled bool) error {
	link, found, err := s.store.GetWorkItemLink(ctx, linkID)
	if err != nil {
		return err
	}
	// The project check is here rather than in the UPDATE because the error
	// has to be indistinguishable from "no such link": a caller must not be
	// able to learn that a link id exists in a project they cannot reach.
	if !found || link.ProjectID != projectID {
		return apierr.NotFound("PLANE_LINK_NOT_FOUND", "no such work item link in this project")
	}
	if _, err := s.store.SetWorkItemLinkSync(ctx, linkID, enabled, s.now()); err != nil {
		return err
	}
	return nil
}

// Links lists a project's links, refreshing each from the provider when it can.
//
// Refreshing is best-effort per link: one unreachable item must not blank the
// whole list, and a provider that is entirely down still yields every link with
// its cached title and an explanation.
func (s *Service) Links(ctx context.Context, projectID domain.ProjectID, live bool) ([]LinkView, error) {
	links, err := s.store.ListWorkItemLinks(ctx, projectID)
	if err != nil {
		return nil, err
	}
	views := make([]LinkView, 0, len(links))

	var client ports.WorkItems
	if live && len(links) > 0 {
		if c, _, cErr := s.client(ctx, projectID); cErr == nil {
			client = c
		}
	}
	for _, link := range links {
		view := LinkView{WorkItemLink: link, Readiness: domain.ReadinessOf(link.LastSeenState)}
		if client != nil {
			callCtx, cancel := context.WithTimeout(ctx, providerTimeout)
			item, gErr := client.Get(callCtx, link.Ref)
			cancel()
			if gErr == nil {
				view.Live, view.URL = &item, item.URL
				view.Readiness = domain.ReadinessOf(item.StateGroup)
				if item.Title != link.LastSeenTitle || item.StateGroup != link.LastSeenState {
					if tErr := s.store.TouchWorkItemLinkSnapshot(ctx, link.ID, item.Title, item.StateGroup, s.now()); tErr != nil {
						s.log.Debug("work items: could not refresh a link snapshot", "link", link.ID, "err", tErr)
					}
				}
			} else {
				view.LiveError = providerMessage(gErr)
			}
		}
		views = append(views, view)
	}
	return views, nil
}

// LinkFor returns the link attached to one AO thing, if any.
func (s *Service) LinkFor(
	ctx context.Context, projectID domain.ProjectID, scope domain.WorkItemLinkScope, scopeID string,
) (LinkView, bool, error) {
	link, found, err := s.store.GetWorkItemLinkByScope(ctx, projectID, scope, scopeID)
	if err != nil || !found {
		return LinkView{}, false, err
	}
	return LinkView{WorkItemLink: link, Readiness: domain.ReadinessOf(link.LastSeenState)}, true, nil
}

func (s *Service) mintID() string {
	if s.newID != nil {
		return s.newID()
	}
	return fmt.Sprintf("wil_%d", s.now().UnixNano())
}
