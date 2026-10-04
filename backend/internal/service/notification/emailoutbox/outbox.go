// Package emailoutbox is the durable half of AO's optional notification email.
//
// WHY IT EXISTS. Email fan-out used to be a goroutine with a background
// context: send, and log whatever happened. That protected the right thing -- a
// mail server must never fail the work being reported -- but it made delivery
// entirely non-durable. A daemon that exited between the notification INSERT
// and the SMTP handshake lost the email with no record it was ever owed, and a
// mail server down for ninety seconds lost every notification raised in that
// window. The only trace either way was a log line.
//
// This package makes the OWING durable and keeps the protection. Enqueue writes
// a row saying an email is owed; Worker drains it on a bounded retry budget.
// Nothing here can fail the work being reported: Enqueue's error is the
// caller's to log, and the worker runs on its own schedule entirely outside the
// lifecycle write path.
//
// SEMANTICS. In-app delivery is exactly-once (the notification row IS the
// delivery, and its event-dedupe index is permanent). Email is at-least-once
// and says so in domain.EmailDeliverySemantics: a send that succeeds on the
// wire but crashes before the row is marked sent is retried, and SMTP offers
// nothing that would make that exactly-once. Duplicate mail is the failure mode
// deliberately chosen over lost mail.
package emailoutbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// Store is the outbox persistence surface.
type Store interface {
	EnqueueNotificationEmail(ctx context.Context, entry domain.EmailOutboxEntry) (bool, error)
	ListDueNotificationEmails(ctx context.Context, now time.Time, limit int) ([]domain.EmailOutboxEntry, error)
	ClaimNotificationEmail(ctx context.Context, notificationID string, now, leaseExpiresAt time.Time) (bool, error)
	MarkNotificationEmailSent(ctx context.Context, notificationID string, now time.Time) error
	MarkNotificationEmailRetry(ctx context.Context, notificationID, lastError string, now, nextAttemptAt time.Time) error
	MarkNotificationEmailDead(ctx context.Context, notificationID, lastError string, now time.Time) error
	ReclaimExpiredNotificationEmailLeases(ctx context.Context, now time.Time) (int64, error)
}

// Renderer turns a stored notification into the message that will be sent. It
// is applied ONCE, at enqueue time, so a retry sends what the event produced
// rather than re-rendering against state that has since moved on.
type Renderer interface {
	RenderNotificationEmail(rec domain.NotificationRecord) (ports.EmailMessage, error)
}

// Defaults for the retry budget and pacing.
const (
	// DefaultMaxAttempts is the per-entry attempt budget. Five attempts across
	// the backoff below spans roughly ten minutes, which covers a mail server
	// restart or a short network outage without hammering a server that is
	// genuinely refusing.
	DefaultMaxAttempts = 5
	// DefaultBaseBackoff is the first retry delay; it doubles each attempt.
	DefaultBaseBackoff = 30 * time.Second
	// DefaultMaxBackoff caps the exponential growth.
	DefaultMaxBackoff = 15 * time.Minute
	// DefaultLease is how long a worker may hold a claimed entry before another
	// pass may reclaim it. Comfortably longer than an SMTP timeout, so a slow
	// send is not stolen from under a healthy worker.
	DefaultLease = 5 * time.Minute
	// DefaultBatchSize bounds one drain pass.
	DefaultBatchSize = 20
)

// Deps configures the outbox.
type Deps struct {
	Store     Store
	Transport ports.EmailTransport
	Renderer  Renderer
	Logger    *slog.Logger
	Clock     func() time.Time

	MaxAttempts int
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
	Lease       time.Duration
	BatchSize   int
}

// Outbox enqueues and delivers notification emails.
type Outbox struct {
	store     Store
	transport ports.EmailTransport
	renderer  Renderer
	log       *slog.Logger
	clock     func() time.Time

	maxAttempts int
	baseBackoff time.Duration
	maxBackoff  time.Duration
	lease       time.Duration
	batchSize   int
}

// New builds an Outbox, filling in defaults.
func New(d Deps) *Outbox {
	o := &Outbox{
		store:       d.Store,
		transport:   d.Transport,
		renderer:    d.Renderer,
		log:         d.Logger,
		clock:       d.Clock,
		maxAttempts: d.MaxAttempts,
		baseBackoff: d.BaseBackoff,
		maxBackoff:  d.MaxBackoff,
		lease:       d.Lease,
		batchSize:   d.BatchSize,
	}
	if o.log == nil {
		o.log = slog.Default()
	}
	if o.clock == nil {
		o.clock = func() time.Time { return time.Now().UTC() }
	}
	if o.maxAttempts <= 0 {
		o.maxAttempts = DefaultMaxAttempts
	}
	if o.baseBackoff <= 0 {
		o.baseBackoff = DefaultBaseBackoff
	}
	if o.maxBackoff <= 0 {
		o.maxBackoff = DefaultMaxBackoff
	}
	if o.lease <= 0 {
		o.lease = DefaultLease
	}
	if o.batchSize <= 0 {
		o.batchSize = DefaultBatchSize
	}
	return o
}

// EmailNotification records that one freshly inserted notification owes an
// email. It implements notify.Emailer, so the write path's contract is
// unchanged: it is called once per INSERT, and its error is a log line rather
// than a failure of the work being reported.
//
// It is SYNCHRONOUS and deliberately so. The whole point of the outbox is that
// the owing survives a crash, and a goroutine cannot promise that -- the row
// has to be written before the process can die. It is one local SQLite insert
// on a path that has just done several.
func (o *Outbox) EmailNotification(ctx context.Context, rec domain.NotificationRecord) error {
	if o == nil || o.store == nil {
		return nil
	}
	// Nothing to render or route: no transport is configured at all. In-app
	// notification stands on its own (P4-D section 8), so this is a normal
	// state, not an error, and nothing is enqueued -- an entry nobody can ever
	// send would just accumulate.
	if o.transport == nil || o.renderer == nil {
		return nil
	}
	if _, emailable := rec.Type.EmailEventOf(); !emailable {
		return nil
	}
	msg, err := o.renderer.RenderNotificationEmail(rec)
	if err != nil {
		// A render that declines (email off, or an event the user did not pick)
		// is the normal case on most installs, not a fault.
		if errors.Is(err, ports.ErrEmailSuppressed) || errors.Is(err, ports.ErrEmailTransportUnavailable) {
			return nil
		}
		return fmt.Errorf("emailoutbox: render notification %s: %w", rec.ID, err)
	}
	now := o.now()
	_, err = o.store.EnqueueNotificationEmail(ctx, domain.EmailOutboxEntry{
		NotificationID: rec.ID,
		Recipient:      rec.Recipient,
		Subject:        msg.Subject,
		Body:           msg.Body,
		MaxAttempts:    o.maxAttempts,
		NextAttemptAt:  now,
		CreatedAt:      now,
	})
	if err != nil {
		return fmt.Errorf("emailoutbox: enqueue notification %s: %w", rec.ID, err)
	}
	return nil
}

// Drain runs one delivery pass: reclaim what a dead daemon left behind, then
// send everything currently due. It returns how many entries it completed
// (sent or dead-lettered), which is what a test asserts on.
//
// Every failure mode here is contained. A transport that is unavailable stops
// the pass without spending anyone's budget; a permanent rejection dead-letters
// one entry; anything else parks one entry on a backoff. None of them return an
// error that could propagate into a workflow.
func (o *Outbox) Drain(ctx context.Context) (int, error) {
	if o == nil || o.store == nil {
		return 0, nil
	}
	now := o.now()
	// Entries a dead daemon left mid-send. Their attempt was already counted,
	// so they come back as due retries rather than resetting.
	if _, err := o.store.ReclaimExpiredNotificationEmailLeases(ctx, now); err != nil {
		return 0, fmt.Errorf("emailoutbox: reclaim leases: %w", err)
	}
	due, err := o.store.ListDueNotificationEmails(ctx, now, o.batchSize)
	if err != nil {
		return 0, fmt.Errorf("emailoutbox: list due: %w", err)
	}
	completed := 0
	for _, entry := range due {
		done, err := o.deliver(ctx, entry)
		if err != nil {
			return completed, err
		}
		if done {
			completed++
		}
	}
	return completed, nil
}

// deliver handles one entry end to end. It reports whether the entry reached a
// terminal state.
func (o *Outbox) deliver(ctx context.Context, entry domain.EmailOutboxEntry) (bool, error) {
	if o.transport == nil {
		// No transport: leave the entry due rather than spending an attempt on
		// something that cannot be tried. It sends once email is configured.
		return false, nil
	}
	now := o.now()
	claimed, err := o.store.ClaimNotificationEmail(ctx, entry.NotificationID, now, now.Add(o.lease))
	if err != nil {
		return false, fmt.Errorf("emailoutbox: claim %s: %w", entry.NotificationID, err)
	}
	if !claimed {
		// Another worker took it, or it stopped being due. Both are fine.
		return false, nil
	}
	sendErr := o.transport.Send(ctx, ports.EmailMessage{Subject: entry.Subject, Body: entry.Body})
	now = o.now()
	switch {
	case sendErr == nil:
		if err := o.store.MarkNotificationEmailSent(ctx, entry.NotificationID, now); err != nil {
			return false, err
		}
		return true, nil

	case errors.Is(sendErr, ports.ErrEmailSuppressed),
		errors.Is(sendErr, ports.ErrEmailTransportUnavailable):
		// The user turned email off, or unconfigured it, after this was
		// enqueued. Retrying forever would be wrong and so would counting it as
		// a delivery failure: the entry is closed as something AO chose not to
		// send.
		o.log.Info("emailoutbox: delivery suppressed",
			"notification", entry.NotificationID, "reason", sendErr)
		if err := o.store.MarkNotificationEmailDead(ctx, entry.NotificationID, sendErr.Error(), now); err != nil {
			return false, err
		}
		return true, nil

	case ports.PermanentDeliveryError(sendErr):
		o.log.Warn("emailoutbox: permanent delivery failure",
			"notification", entry.NotificationID, "err", sendErr)
		if err := o.store.MarkNotificationEmailDead(ctx, entry.NotificationID, sendErr.Error(), now); err != nil {
			return false, err
		}
		return true, nil

	default:
		// Transient. The claim already spent this attempt, so the budget check
		// reads the count the claim produced.
		attempt := entry.AttemptCount + 1
		if attempt >= entry.MaxAttempts {
			o.log.Warn("emailoutbox: attempt budget spent",
				"notification", entry.NotificationID, "attempts", attempt, "err", sendErr)
			if err := o.store.MarkNotificationEmailDead(ctx, entry.NotificationID, sendErr.Error(), now); err != nil {
				return false, err
			}
			return true, nil
		}
		if err := o.store.MarkNotificationEmailRetry(
			ctx, entry.NotificationID, sendErr.Error(), now, now.Add(o.backoff(attempt)),
		); err != nil {
			return false, err
		}
		return false, nil
	}
}

// backoff is exponential in the attempt already spent, capped. attempt is 1 on
// the first failure, so the first retry waits baseBackoff.
func (o *Outbox) backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	// Guard the shift: an attempt budget is small, but the arithmetic should not
	// depend on that staying true.
	if attempt > 20 {
		return o.maxBackoff
	}
	delay := time.Duration(math.Pow(2, float64(attempt-1))) * o.baseBackoff
	if delay <= 0 || delay > o.maxBackoff {
		return o.maxBackoff
	}
	return delay
}

func (o *Outbox) now() time.Time { return o.clock().UTC() }
