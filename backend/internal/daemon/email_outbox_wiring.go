package daemon

import (
	"context"
	"log/slog"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/service/notification/emailoutbox"
)

// emailOutboxInterval is how often the daemon drains owed notification emails.
//
// Chosen against what the queue is for, not against latency: entries are due
// the moment they are written, so the common case is the pass that runs right
// after one is enqueued. The interval only bounds how long a RETRY waits past
// its backoff, and the shortest backoff is already 30s.
const emailOutboxInterval = 30 * time.Second

// startEmailOutboxWorker drains the durable email outbox: once immediately, to
// pick up whatever a previous daemon owed but never finished sending, and then
// on a ticker.
//
// Everything here is best-effort and deliberately detached. A mail server that
// is down, an expired app password, a machine that is offline -- none of them
// may touch the work being reported, so a drain failure is a log line and the
// next tick tries again. The goroutine exits with ctx, so daemon shutdown
// leaves any in-flight entry leased; the lease expires and the next daemon
// reclaims it.
func startEmailOutboxWorker(ctx context.Context, outbox *emailoutbox.Outbox, log *slog.Logger) {
	if outbox == nil {
		return
	}
	if log == nil {
		log = slog.Default()
	}
	drain := func() {
		sent, err := outbox.Drain(ctx)
		if err != nil {
			log.Warn("notification email outbox drain failed", "err", err)
			return
		}
		if sent > 0 {
			log.Info("notification emails delivered", "count", sent)
		}
	}
	go func() {
		drain()
		ticker := time.NewTicker(emailOutboxInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				drain()
			}
		}
	}()
}
