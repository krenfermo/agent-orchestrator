package settings

import (
	"context"
	"fmt"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/mailer"
)

// NotificationEmailer turns a stored completion notification into an email,
// using the same daemon-owned settings the Settings UI edits.
//
// It lives here rather than in notify because resolving the destination means
// reading settings and decrypting the password — knowledge the notification
// writer has no business holding. notify only knows the Emailer interface.
type NotificationEmailer struct {
	svc *Service
}

// NewNotificationEmailer adapts the settings service to notify.Emailer.
func NewNotificationEmailer(svc *Service) *NotificationEmailer {
	return &NotificationEmailer{svc: svc}
}

// EmailNotification sends one completion notification, or reports why it could
// not. The caller treats every error as a log line, never as a failure of the
// work being reported.
func (e *NotificationEmailer) EmailNotification(ctx context.Context, rec domain.NotificationRecord) error {
	if e == nil || e.svc == nil || e.svc.sender == nil {
		return nil
	}
	snapshot, err := e.svc.store.GetAppSettings(ctx)
	if err != nil {
		return fmt.Errorf("settings: read email settings: %w", err)
	}
	// Disabled is the normal case, not an error: most installs never turn this
	// on, and a warning per finished task would be noise.
	if !snapshot.Email.Enabled {
		return nil
	}
	cfg, err := e.svc.mailerConfig(ctx)
	if err != nil {
		return err
	}
	return e.svc.sender.Send(cfg, message(rec))
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
	body.WriteString("Finished at: " + rec.CreatedAt.UTC().Format("2006-01-02 15:04:05 UTC") + "\n")
	body.WriteString("\n— Agent Orchestrator\n")
	return mailer.Message{Subject: subject(rec), Body: body.String()}
}

func subject(rec domain.NotificationRecord) string {
	switch rec.Type {
	case domain.NotificationWorkflowCompleted:
		return "[AO] Workflow finished: " + rec.Title
	default:
		return "[AO] Task finished: " + rec.Title
	}
}
