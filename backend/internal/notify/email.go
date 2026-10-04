package notify

import (
	"context"
	"log/slog"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// Emailer records that one stored notification owes an email. Implementations
// resolve the user's own SMTP settings — including whether this notification's
// event is one the user asked to be emailed about; the notify Manager knows
// only that a notification may also be worth emailing.
//
// The implementation in production is service/notification/emailoutbox.Outbox,
// which writes a durable outbox row and lets its worker do the sending.
type Emailer interface {
	// EmailNotification is called once per newly INSERTED notification of an
	// event-keyed family. It records that the email is OWED; it does not talk
	// to a mail server. It must not block for long and must never be treated as
	// part of the work being reported: see Manager.fanOutEmail.
	EmailNotification(ctx context.Context, rec domain.NotificationRecord) error
}

// fanOutEmail records that a freshly inserted event notification owes an email.
//
// Three properties are load-bearing, and all three are about the same rule:
// email is a courtesy, never a dependency of the work.
//
//   - Only newly inserted rows reach here, so the permanent event dedupe in the
//     store is also the email's dedupe. A retry or restart that re-observes the
//     same completion inserts nothing and therefore owes nothing.
//   - It is SYNCHRONOUS, and that is the change the durable outbox made. The
//     implementation writes a row saying an email is owed and returns; the
//     actual SMTP conversation happens later, on the outbox worker's own
//     schedule, entirely off this path. A goroutine used to hold that owing in
//     memory, where a daemon exit lost it silently -- the crash window this
//     whole outbox exists to close. What it costs here is one local insert.
//   - It runs on its OWN context, not the caller's. The caller's context is
//     routinely cancelled the instant the work finishes, which is exactly when
//     the "it finished" notification is written; inheriting it would cancel the
//     enqueue and lose the email in the common case rather than a rare one.
//     The bound is a timeout of its own, so a wedged database cannot hold the
//     lifecycle write open either.
//   - Every failure is a log line. A task or workflow that genuinely finished,
//     stopped, or failed must never have its recorded state changed because a
//     mail server was down, an app password expired, or the machine was
//     offline.
func (m *Manager) fanOutEmail(rec domain.NotificationRecord) {
	// EventKeyed rather than Completion: the same three properties above are
	// exactly what an attention or failure email needs, and they hold for the
	// same reason — one INSERT per real event, permanently deduped.
	if m.emailer == nil || !rec.Type.EventKeyed() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), enqueueEmailTimeout)
	defer cancel()
	if err := m.emailer.EmailNotification(ctx, rec); err != nil {
		logger := m.log
		if logger == nil {
			logger = slog.Default()
		}
		logger.Warn("notify: queueing notification email failed",
			"notification", rec.ID, "type", rec.Type, "err", err)
	}
}

// enqueueEmailTimeout bounds the one local insert that records an owed email.
// Generous for a SQLite write, short enough that a wedged database degrades to
// a lost email rather than a stalled lifecycle write.
const enqueueEmailTimeout = 5 * time.Second
