package domain

import "time"

// EmailOutboxState is where one owed notification email stands.
//
// The states and their transitions:
//
//	pending -> sending -> sent      the happy path, terminal at sent
//	        -> sending -> failed    transient; retried after a backoff
//	        -> sending -> dead      permanent, or the attempt budget is spent
//
// A 'sending' row whose lease has expired was claimed by a daemon that died. It
// returns to 'failed' and becomes due immediately, which is what makes a crash
// mid-send converge instead of stranding the email forever.
type EmailOutboxState string

// Email outbox states.
const (
	// EmailOutboxPending means nothing has claimed the entry yet.
	EmailOutboxPending EmailOutboxState = "pending"
	// EmailOutboxSending means a worker holds the entry; its attempt is spent.
	EmailOutboxSending EmailOutboxState = "sending"
	// EmailOutboxSent means the transport accepted it. Terminal.
	EmailOutboxSent EmailOutboxState = "sent"
	// EmailOutboxFailed means a transient failure; next_attempt_at holds the
	// backoff deadline.
	EmailOutboxFailed EmailOutboxState = "failed"
	// EmailOutboxDead means AO gave up: a permanent rejection, or the attempt
	// budget is spent. Terminal.
	EmailOutboxDead EmailOutboxState = "dead"
)

// Valid reports whether s is a supported outbox state.
func (s EmailOutboxState) Valid() bool {
	switch s {
	case EmailOutboxPending, EmailOutboxSending, EmailOutboxSent, EmailOutboxFailed, EmailOutboxDead:
		return true
	default:
		return false
	}
}

// Terminal reports whether s is a state the entry never leaves.
func (s EmailOutboxState) Terminal() bool {
	return s == EmailOutboxSent || s == EmailOutboxDead
}

// EmailOutboxEntry is one durable "this notification still owes an email".
//
// It is keyed by NotificationID, not a surrogate id: the notification row is
// already deduped permanently by event, so keying the outbox by it inherits
// that idempotency exactly. Re-observing the same event inserts no
// notification, and therefore owes no second email.
type EmailOutboxEntry struct {
	NotificationID string
	Recipient      NotificationRecipient
	State          EmailOutboxState
	// Subject and Body are rendered once, at enqueue time, so a retry sends the
	// message the event produced rather than re-rendering against state that
	// has since moved on.
	Subject string
	Body    string
	// AttemptCount is spent at CLAIM time, not after the send, so a crash
	// mid-send still costs an attempt and a permanently wedging message cannot
	// retry forever.
	AttemptCount int
	// MaxAttempts is frozen at enqueue, so raising the default later does not
	// silently revive entries AO already gave up on.
	MaxAttempts    int
	NextAttemptAt  time.Time
	LeaseExpiresAt time.Time
	LastError      string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	CompletedAt    time.Time
}

// EmailDeliverySemantics documents, in code, the guarantee each channel gives.
//
// In-app notification delivery is EXACTLY-ONCE: the notifications row IS the
// delivery, and the permanent event-dedupe index makes the insert idempotent.
//
// Email delivery is AT-LEAST-ONCE. A send that succeeds on the wire but crashes
// before the row is marked sent is retried, and SMTP offers nothing that would
// make it exactly-once. Duplicate mail is the failure mode deliberately chosen
// over lost mail: P4-D requires the semantics be explicit rather than assumed,
// and for "your workflow stopped at 2am" a second copy is far cheaper than
// silence.
const EmailDeliverySemantics = "at-least-once"
