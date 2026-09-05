package settings

import (
	"context"
	"fmt"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/mailer"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// NotificationEmailer resolves AO's daemon-owned email settings for the
// notification outbox. It plays two roles, split by WHEN each one runs:
//
//   - Renderer (enqueue time): it decides whether this notification is one the
//     user asked to be emailed about, and renders the message once. Rendering
//     early is what makes a retry send the message the EVENT produced rather
//     than one re-rendered against state that has since moved on.
//   - EmailTransport (send time): it resolves the current SMTP config and
//     hands the message to the sender. Resolving late is what lets a user fix
//     a wrong password and have queued mail go out on the next retry.
//
// It lives here rather than in notify or emailoutbox because resolving the
// destination means reading settings and decrypting the password -- knowledge
// neither the notification writer nor the outbox has any business holding.
type NotificationEmailer struct {
	svc *Service
}

// NewNotificationEmailer adapts the settings service to the outbox's Renderer
// and ports.EmailTransport.
func NewNotificationEmailer(svc *Service) *NotificationEmailer {
	return &NotificationEmailer{svc: svc}
}

// RenderNotificationEmail renders one notification, or declines.
//
// Declining is the normal case on most installs, so it is reported with the
// sentinels the outbox treats as "nothing owed" rather than as errors: email
// switched off, or an event the user did not select, must not enqueue an entry
// and must not log a warning per finished task.
func (e *NotificationEmailer) RenderNotificationEmail(rec domain.NotificationRecord) (ports.EmailMessage, error) {
	if e == nil || e.svc == nil || e.svc.sender == nil {
		return ports.EmailMessage{}, ports.ErrEmailTransportUnavailable
	}
	// A type with no email event behind it is not something this build knows how
	// to mail, and silently mailing it would bypass the user's selection.
	event, emailable := rec.Type.EmailEventOf()
	if !emailable {
		return ports.EmailMessage{}, ports.ErrEmailSuppressed
	}
	// Rendering needs no I/O beyond the preference check, and the preference
	// check is what decides whether anything is owed at all.
	snapshot, err := e.svc.store.GetAppSettings(context.Background())
	if err != nil {
		return ports.EmailMessage{}, fmt.Errorf("settings: read email settings: %w", err)
	}
	if !snapshot.Email.Enabled || !snapshot.Email.Events.Allows(event) {
		return ports.EmailMessage{}, ports.ErrEmailSuppressed
	}
	msg := message(rec)
	return ports.EmailMessage{Subject: msg.Subject, Body: msg.Body}, nil
}

// Send delivers one rendered message over the currently configured SMTP
// destination. It implements ports.EmailTransport.
//
// The settings are re-read here, at send time, on purpose: an entry queued
// while the password was wrong should go out once the password is fixed, and an
// entry queued before the user switched email off should not go out at all.
func (e *NotificationEmailer) Send(ctx context.Context, msg ports.EmailMessage) error {
	if e == nil || e.svc == nil || e.svc.sender == nil {
		return ports.ErrEmailTransportUnavailable
	}
	snapshot, err := e.svc.store.GetAppSettings(ctx)
	if err != nil {
		return fmt.Errorf("settings: read email settings: %w", err)
	}
	if !snapshot.Email.Enabled {
		return ports.ErrEmailSuppressed
	}
	cfg, err := e.svc.mailerConfig(ctx)
	if err != nil {
		// An incomplete or undecryptable configuration is not something a retry
		// fixes on its own, but it IS something the user fixes in Settings, so
		// it stays retryable rather than dead-lettering the entry.
		return fmt.Errorf("settings: resolve SMTP config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ports.ErrEmailTransportUnavailable, err)
	}
	return e.svc.sender.Send(cfg, mailer.Message{Subject: msg.Subject, Body: msg.Body})
}

func message(rec domain.NotificationRecord) mailer.Message {
	var body strings.Builder
	body.WriteString(rec.Title)
	body.WriteString("\n")
	if rec.Body != "" {
		body.WriteString("\n")
		body.WriteString(rec.Body)
		body.WriteString("\n")
	}
	body.WriteString("\n")
	if rec.SessionID != "" {
		body.WriteString("Session: " + string(rec.SessionID) + "\n")
	}
	if rec.WorkflowRunID != "" {
		body.WriteString("Workflow run: " + rec.WorkflowRunID + "\n")
	}
	body.WriteString("Project: " + string(rec.ProjectID) + "\n")
	body.WriteString(timestampLabel(rec) + ": " + rec.CreatedAt.UTC().Format("2006-01-02 15:04:05 UTC") + "\n")
	body.WriteString("\n— Agent Orchestrator\n")
	return mailer.Message{Subject: subject(rec), Body: body.String()}
}

func timestampLabel(rec domain.NotificationRecord) string {
	switch {
	case rec.Type.Attention(),
		rec.Type == domain.NotificationHumanQuestionRequired,
		rec.Type == domain.NotificationRepairExhausted:
		return "Stopped at"
	case rec.Type.Failure(), rec.Type == domain.NotificationIntegrationFailed:
		return "Failed at"
	default:
		return "Finished at"
	}
}

func subject(rec domain.NotificationRecord) string {
	switch rec.Type {
	case domain.NotificationWorkflowCompleted:
		return "[AO] Workflow finished: " + rec.Title
	case domain.NotificationTaskNeedsAttention:
		return "[AO] Task needs attention: " + rec.Title
	case domain.NotificationWorkflowNeedsAttention:
		return "[AO] Workflow needs attention: " + rec.Title
	case domain.NotificationTaskFailed:
		return "[AO] Task failed: " + rec.Title
	case domain.NotificationWorkflowFailed:
		return "[AO] Workflow failed: " + rec.Title
	case domain.NotificationHumanQuestionRequired:
		return "[AO] Waiting on your decision: " + rec.Title
	case domain.NotificationRepairExhausted:
		return "[AO] AO stopped retrying: " + rec.Title
	case domain.NotificationIntegrationFailed:
		return "[AO] Integration failed: " + rec.Title
	default:
		return "[AO] Task finished: " + rec.Title
	}
}
