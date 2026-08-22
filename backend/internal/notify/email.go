package notify

import (
	"context"
	"log/slog"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// Emailer delivers one stored notification by email. Implementations resolve
// the user's own SMTP settings; the notify Manager knows only that a
// notification may also be worth emailing.
type Emailer interface {
	// EmailNotification is called once per newly INSERTED notification of the
	// completion family. It must not block for long and must never be treated
	// as part of the work being reported: see Manager.fanOutEmail.
	EmailNotification(ctx context.Context, rec domain.NotificationRecord) error
}

// fanOutEmail delivers a freshly inserted completion notification by email.
//
// Three properties are load-bearing, and all three are about the same rule:
// email is a courtesy, never a dependency of the work.
//
//   - Only newly inserted rows reach here, so the permanent event dedupe in the
//     store is also the email's dedupe. A retry or restart that re-observes the
//     same completion inserts nothing and therefore emails nothing.
//   - It runs on its own goroutine with a background context, so neither a slow
//     mail server nor the caller's context being cancelled the instant the task
//     finishes can delay or skip the send.
//   - Every failure is a log line. A task or workflow that genuinely finished
//     must never be reported as failed because a mail server was down, an app
//     password expired, or the machine was offline.
func (m *Manager) fanOutEmail(rec domain.NotificationRecord) {
	if m.emailer == nil || !rec.Type.Completion() {
		return
	}
	emailer := m.emailer
	logger := m.log
	go func() {
		if err := emailer.EmailNotification(context.Background(), rec); err != nil {
			if logger == nil {
				logger = slog.Default()
			}
			logger.Warn("notify: completion email failed",
				"notification", rec.ID, "type", rec.Type, "err", err)
		}
	}()
}
