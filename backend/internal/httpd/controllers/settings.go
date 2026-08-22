package controllers

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	settingssvc "github.com/aoagents/agent-orchestrator/backend/internal/service/settings"
)

// SettingsService is the controller-facing preferences contract.
type SettingsService interface {
	Get(ctx context.Context) (settingssvc.Snapshot, error)
	SetDefaultSessionMode(ctx context.Context, mode domain.SessionMode) (settingssvc.Snapshot, error)
	ChatHarnesses(candidates []domain.AgentHarness) []domain.AgentHarness
	EmailNotifications(ctx context.Context) (settingssvc.EmailSettings, error)
	SetEmailNotifications(ctx context.Context, update settingssvc.EmailUpdate) (settingssvc.EmailSettings, error)
	SendTestEmail(ctx context.Context) error
}

// SettingsController owns the daemon-owned preference routes.
//
// These are daemon-owned rather than renderer-owned on purpose: desktop, mobile,
// and the CLI all resolve the same value, so a preference held in one client would
// disagree with the others.
type SettingsController struct {
	Svc SettingsService
}

// Register mounts the settings routes.
func (c *SettingsController) Register(r chi.Router) {
	r.Get("/settings", c.get)
	r.Patch("/settings/session-interface", c.setSessionInterface)
	r.Get("/settings/email-notifications", c.getEmailNotifications)
	r.Patch("/settings/email-notifications", c.setEmailNotifications)
	r.Post("/settings/email-notifications/test", c.sendTestEmail)
}

func (c *SettingsController) getEmailNotifications(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/settings/email-notifications")
		return
	}
	cfg, err := c.Svc.EmailNotifications(r.Context())
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, EmailNotificationSettingsEnvelope{
		EmailNotifications: emailSettingsResponse(cfg),
	})
}

func (c *SettingsController) setEmailNotifications(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "PATCH", "/api/v1/settings/email-notifications")
		return
	}
	var req UpdateEmailNotificationSettingsRequest
	if !decodeConversationBody(w, r, &req) {
		return
	}
	cfg, err := c.Svc.SetEmailNotifications(r.Context(), settingssvc.EmailUpdate{
		Enabled:   req.Enabled,
		Recipient: req.Recipient,
		Host:      req.Host,
		Port:      req.Port,
		Username:  req.Username,
		TLS:       req.TLS,
		Password:  req.Password,
	})
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, EmailNotificationSettingsEnvelope{
		EmailNotifications: emailSettingsResponse(cfg),
	})
}

// sendTestEmail proves the credentials work while the user is still looking at
// the form, instead of the first time a task finishes overnight.
func (c *SettingsController) sendTestEmail(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "POST", "/api/v1/settings/email-notifications/test")
		return
	}
	if err := c.Svc.SendTestEmail(r.Context()); err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	cfg, err := c.Svc.EmailNotifications(r.Context())
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, SendTestEmailResponse{Sent: true, Recipient: cfg.Recipient})
}

// emailSettingsResponse maps the service view to the wire. There is no password
// to omit here: the service view has never carried one.
func emailSettingsResponse(cfg settingssvc.EmailSettings) EmailNotificationSettingsResponse {
	return EmailNotificationSettingsResponse{
		Enabled:     cfg.Enabled,
		Recipient:   cfg.Recipient,
		Host:        cfg.Host,
		Port:        cfg.Port,
		Username:    cfg.Username,
		TLS:         string(cfg.TLS),
		PasswordSet: cfg.PasswordSet,
	}
}

func (c *SettingsController) get(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/settings")
		return
	}
	snapshot, err := c.Svc.Get(r.Context())
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, c.response(snapshot))
}

func (c *SettingsController) setSessionInterface(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "PATCH", "/api/v1/settings/session-interface")
		return
	}
	var req UpdateSessionInterfaceRequest
	if !decodeConversationBody(w, r, &req) {
		return
	}

	// Parsed strictly: an unrecognized value is rejected rather than collapsing to
	// a default the caller did not ask for.
	mode, err := domain.ParseSessionMode(req.DefaultSessionMode)
	if err != nil || mode == "" {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "validation",
			"SESSION_MODE_INVALID", `defaultSessionMode must be "chat" or "tui"`, nil)
		return
	}

	snapshot, err := c.Svc.SetDefaultSessionMode(r.Context(), mode)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, c.response(snapshot))
}

func (c *SettingsController) response(snapshot settingssvc.Snapshot) SettingsResponse {
	// Reported so the client can warn that choosing chat narrows which agents are
	// available, instead of letting the user discover it at spawn time.
	chatHarnesses := c.Svc.ChatHarnesses(domain.AllHarnesses)
	names := make([]string, 0, len(chatHarnesses))
	for _, harness := range chatHarnesses {
		names = append(names, string(harness))
	}
	return SettingsResponse{
		DefaultSessionMode: string(snapshot.DefaultSessionMode),
		ChatHarnesses:      names,
	}
}
