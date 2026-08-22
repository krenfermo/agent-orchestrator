package notify

import (
	"context"
	"log/slog"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// Emailer delivers one stored notification by email. Implementations resolve
// the user's own SMTP settings — including whether this notification's event is
// one the user asked to be emailed about; the notify Manager knows only that a
// notification may also be worth emailing.
type Emailer interface {
	// EmailNotification is called once per newly INSERTED notification of an
	// event-keyed family. It must not block for long and must never be treated
	// as part of the work being reported: see Manager.fanOutEmail.
	EmailNotification(ctx context.Context, rec domain.NotificationRecord) error
}

// fanOutEmail delivers a freshly inserted event notification by email.
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
//   - Every failure is a log line. A task or workflow that genuinely finished,
//     stopped, or failed must never have its recorded state changed because a
//     mail server was down, an app password expired, or the machine was offline.
func (m *Manager) fanOutEmail(rec domain.NotificationRecord) {
	// EventKeyed rather than Completion: the same three properties above are
	// exactly what an attention or failure email needs, and they hold for the
	// same reason — one INSERT per real event, permanently deduped.
	if m.emailer == nil || !rec.Type.EventKeyed() {
		return
	}
	emailer := m.emailer
	logger := m.log
	go func() {
		if err := emailer.EmailNotification(context.Background(), rec); err != nil {
			if logger == nil {
				logger = slog.Default()
			}
			logger.Warn("notify: notification email failed",
				"notification", rec.ID, "type", rec.Type, "err", err)
		}
	}()
}
