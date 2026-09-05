package emailoutbox

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// The outbox's whole job is that an owed email survives things a goroutine
// would not: a mail server that is down, a permanently rejected message, and a
// daemon that dies mid-send. Each of those is one test below.

// memStore is an in-memory outbox with the same conditional-update semantics
// the SQL has, because those semantics are what the worker relies on.
type memStore struct {
	entries map[string]*domain.EmailOutboxEntry
}

func newMemStore() *memStore {
	return &memStore{entries: map[string]*domain.EmailOutboxEntry{}}
}

func (m *memStore) EnqueueNotificationEmail(_ context.Context, e domain.EmailOutboxEntry) (bool, error) {
	if _, exists := m.entries[e.NotificationID]; exists {
		return false, nil
	}
	e.State = domain.EmailOutboxPending
	if e.NextAttemptAt.IsZero() {
		e.NextAttemptAt = e.CreatedAt
	}
	m.entries[e.NotificationID] = &e
	return true, nil
}

func (m *memStore) ListDueNotificationEmails(_ context.Context, now time.Time, limit int) ([]domain.EmailOutboxEntry, error) {
	var out []domain.EmailOutboxEntry
	for _, e := range m.entries {
		claimable := e.State == domain.EmailOutboxPending || e.State == domain.EmailOutboxFailed
		if claimable && !e.NextAttemptAt.After(now) {
			out = append(out, *e)
		}
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (m *memStore) ClaimNotificationEmail(_ context.Context, id string, now, lease time.Time) (bool, error) {
	e, ok := m.entries[id]
	if !ok || (e.State != domain.EmailOutboxPending && e.State != domain.EmailOutboxFailed) {
		return false, nil
	}
	if e.NextAttemptAt.After(now) {
		return false, nil
	}
	e.State = domain.EmailOutboxSending
	e.AttemptCount++
	e.LeaseExpiresAt = lease
	return true, nil
}

func (m *memStore) transition(id string, to domain.EmailOutboxState, lastErr string, next time.Time) error {
	e, ok := m.entries[id]
	if !ok || e.State != domain.EmailOutboxSending {
		return nil
	}
	e.State = to
	e.LastError = lastErr
	e.LeaseExpiresAt = time.Time{}
	if !next.IsZero() {
		e.NextAttemptAt = next
	}
	return nil
}

func (m *memStore) MarkNotificationEmailSent(_ context.Context, id string, _ time.Time) error {
	return m.transition(id, domain.EmailOutboxSent, "", time.Time{})
}

func (m *memStore) MarkNotificationEmailRetry(_ context.Context, id, lastErr string, _, next time.Time) error {
	return m.transition(id, domain.EmailOutboxFailed, lastErr, next)
}

func (m *memStore) MarkNotificationEmailDead(_ context.Context, id, lastErr string, _ time.Time) error {
	return m.transition(id, domain.EmailOutboxDead, lastErr, time.Time{})
}

func (m *memStore) ReclaimExpiredNotificationEmailLeases(_ context.Context, now time.Time) (int64, error) {
	var n int64
	for _, e := range m.entries {
		if e.State == domain.EmailOutboxSending && !e.LeaseExpiresAt.IsZero() && !e.LeaseExpiresAt.After(now) {
			e.State = domain.EmailOutboxFailed
			e.LeaseExpiresAt = time.Time{}
			e.NextAttemptAt = now
			n++
		}
	}
	return n, nil
}

func (m *memStore) state(id string) domain.EmailOutboxState { return m.entries[id].State }

type stubRenderer struct{ err error }

func (s stubRenderer) RenderNotificationEmail(rec domain.NotificationRecord) (ports.EmailMessage, error) {
	if s.err != nil {
		return ports.EmailMessage{}, s.err
	}
	return ports.EmailMessage{Subject: "[AO] " + rec.Title, Body: rec.Body}, nil
}

type stubTransport struct {
	err   error
	calls int
}

func (s *stubTransport) Send(context.Context, ports.EmailMessage) error {
	s.calls++
	return s.err
}

type clock struct{ now time.Time }

func (c *clock) fn() func() time.Time { return func() time.Time { return c.now } }

func failedRecord() domain.NotificationRecord {
	return domain.NotificationRecord{
		ID: "ntf_1", ProjectID: "mer", WorkflowRunID: "wf-1",
		Type: domain.NotificationWorkflowFailed, Title: "ship it failed",
		Body: "It ended without completing the work.", Status: domain.NotificationUnread,
		Recipient: domain.NotificationRecipientLocal, CreatedAt: time.Now().UTC(),
	}
}

func newOutbox(t *testing.T, store Store, tr ports.EmailTransport, r Renderer, c *clock) *Outbox {
	t.Helper()
	return New(Deps{Store: store, Transport: tr, Renderer: r, Clock: c.fn(), MaxAttempts: 3})
}

// The happy path, and the durability claim underneath it: enqueue writes the
// owing BEFORE anything talks to a mail server.
func TestEnqueueThenDrainSends(t *testing.T) {
	store := newMemStore()
	transport := &stubTransport{}
	c := &clock{now: time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)}
	o := newOutbox(t, store, transport, stubRenderer{}, c)
	ctx := context.Background()

	if err := o.EmailNotification(ctx, failedRecord()); err != nil {
		t.Fatalf("EmailNotification: %v", err)
	}
	if store.state("ntf_1") != domain.EmailOutboxPending {
		t.Fatalf("state after enqueue = %q, want pending", store.state("ntf_1"))
	}
	if transport.calls != 0 {
		t.Fatal("enqueue talked to the mail server; the send must happen on the worker")
	}

	sent, err := o.Drain(ctx)
	if err != nil || sent != 1 {
		t.Fatalf("Drain sent=%d err=%v, want 1", sent, err)
	}
	if store.state("ntf_1") != domain.EmailOutboxSent {
		t.Fatalf("state after drain = %q, want sent", store.state("ntf_1"))
	}
}

// P4-D section 8: with no transport, in-app notifications still work and
// nothing accumulates that could never be delivered.
func TestNoTransportEnqueuesNothing(t *testing.T) {
	store := newMemStore()
	c := &clock{now: time.Now().UTC()}
	o := New(Deps{Store: store, Clock: c.fn()})
	if err := o.EmailNotification(context.Background(), failedRecord()); err != nil {
		t.Fatalf("EmailNotification: %v", err)
	}
	if len(store.entries) != 0 {
		t.Fatalf("entries = %d with no transport configured, want 0", len(store.entries))
	}
}

// A declined render (email off, or an event the user did not select) is the
// normal case on most installs: silence, not an error and not a queued entry.
func TestSuppressedRenderEnqueuesNothing(t *testing.T) {
	store := newMemStore()
	c := &clock{now: time.Now().UTC()}
	o := newOutbox(t, store, &stubTransport{}, stubRenderer{err: ports.ErrEmailSuppressed}, c)
	if err := o.EmailNotification(context.Background(), failedRecord()); err != nil {
		t.Fatalf("EmailNotification returned %v, want nil for a deliberate suppression", err)
	}
	if len(store.entries) != 0 {
		t.Fatalf("entries = %d for a suppressed event, want 0", len(store.entries))
	}
}

// A transient failure parks the entry on a backoff and keeps it; the email is
// not lost because a mail server was down for a minute.
func TestTransientFailureRetriesWithBackoff(t *testing.T) {
	store := newMemStore()
	transport := &stubTransport{err: errors.New("dial tcp: connection refused")}
	c := &clock{now: time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)}
	o := newOutbox(t, store, transport, stubRenderer{}, c)
	ctx := context.Background()

	if err := o.EmailNotification(ctx, failedRecord()); err != nil {
		t.Fatalf("EmailNotification: %v", err)
	}
	if _, err := o.Drain(ctx); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if store.state("ntf_1") != domain.EmailOutboxFailed {
		t.Fatalf("state = %q, want failed (the retry state)", store.state("ntf_1"))
	}
	if next := store.entries["ntf_1"].NextAttemptAt; !next.After(c.now) {
		t.Fatalf("next attempt %v is not in the future; the backoff was not applied", next)
	}

	// Not due yet: a drain now must not spend another attempt.
	before := transport.calls
	if _, err := o.Drain(ctx); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if transport.calls != before {
		t.Fatal("an entry inside its backoff window was retried early")
	}

	// Past the backoff, and the server is healthy again.
	c.now = c.now.Add(time.Hour)
	transport.err = nil
	if sent, err := o.Drain(ctx); err != nil || sent != 1 {
		t.Fatalf("Drain after backoff sent=%d err=%v, want 1", sent, err)
	}
	if store.state("ntf_1") != domain.EmailOutboxSent {
		t.Fatalf("state = %q, want sent", store.state("ntf_1"))
	}
}

// A bounded budget: a permanently wedging message cannot retry forever.
func TestAttemptBudgetIsBounded(t *testing.T) {
	store := newMemStore()
	transport := &stubTransport{err: errors.New("temporary failure")}
	c := &clock{now: time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)}
	o := newOutbox(t, store, transport, stubRenderer{}, c)
	ctx := context.Background()

	if err := o.EmailNotification(ctx, failedRecord()); err != nil {
		t.Fatalf("EmailNotification: %v", err)
	}
	for i := 0; i < 10 && store.state("ntf_1") != domain.EmailOutboxDead; i++ {
		c.now = c.now.Add(24 * time.Hour)
		if _, err := o.Drain(ctx); err != nil {
			t.Fatalf("Drain: %v", err)
		}
	}
	if store.state("ntf_1") != domain.EmailOutboxDead {
		t.Fatalf("state = %q, want dead once the budget is spent", store.state("ntf_1"))
	}
	if got := store.entries["ntf_1"].AttemptCount; got != 3 {
		t.Fatalf("attempts = %d, want exactly the frozen budget of 3", got)
	}
}

// A permanent rejection does not burn the budget: it is dead on the first try.
func TestPermanentFailureDeadLettersImmediately(t *testing.T) {
	store := newMemStore()
	transport := &stubTransport{err: fmt.Errorf("%w: 550 mailbox unavailable", ports.ErrPermanentEmailFailure)}
	c := &clock{now: time.Now().UTC()}
	o := newOutbox(t, store, transport, stubRenderer{}, c)
	ctx := context.Background()

	if err := o.EmailNotification(ctx, failedRecord()); err != nil {
		t.Fatalf("EmailNotification: %v", err)
	}
	if _, err := o.Drain(ctx); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if store.state("ntf_1") != domain.EmailOutboxDead {
		t.Fatalf("state = %q, want dead", store.state("ntf_1"))
	}
	if transport.calls != 1 {
		t.Fatalf("transport calls = %d, want 1: a permanent rejection must not spend the budget", transport.calls)
	}
}

// The crash window the outbox exists to close: a daemon dies holding a claimed
// entry. The lease expires and the next daemon picks it up rather than the
// email being lost with the process.
func TestExpiredLeaseIsReclaimedAfterACrash(t *testing.T) {
	store := newMemStore()
	transport := &stubTransport{}
	c := &clock{now: time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)}
	o := newOutbox(t, store, transport, stubRenderer{}, c)
	ctx := context.Background()

	if err := o.EmailNotification(ctx, failedRecord()); err != nil {
		t.Fatalf("EmailNotification: %v", err)
	}
	// Simulate the crash: the entry was claimed, and nothing ever completed it.
	if _, err := store.ClaimNotificationEmail(ctx, "ntf_1", c.now, c.now.Add(DefaultLease)); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if store.state("ntf_1") != domain.EmailOutboxSending {
		t.Fatalf("state = %q, want sending", store.state("ntf_1"))
	}

	// A drain inside the lease must NOT steal it from a healthy worker.
	if sent, err := o.Drain(ctx); err != nil || sent != 0 {
		t.Fatalf("Drain inside the lease sent=%d err=%v, want 0", sent, err)
	}

	c.now = c.now.Add(DefaultLease + time.Minute)
	if sent, err := o.Drain(ctx); err != nil || sent != 1 {
		t.Fatalf("Drain after the lease expired sent=%d err=%v, want 1", sent, err)
	}
	if store.state("ntf_1") != domain.EmailOutboxSent {
		t.Fatalf("state = %q, want sent", store.state("ntf_1"))
	}
}

// One notification owes at most one email, however many times it is enqueued.
// The notification's own permanent event dedupe is inherited, not duplicated.
func TestEnqueueIsIdempotentPerNotification(t *testing.T) {
	store := newMemStore()
	transport := &stubTransport{}
	c := &clock{now: time.Now().UTC()}
	o := newOutbox(t, store, transport, stubRenderer{}, c)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if err := o.EmailNotification(ctx, failedRecord()); err != nil {
			t.Fatalf("EmailNotification: %v", err)
		}
	}
	if len(store.entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(store.entries))
	}
	if sent, err := o.Drain(ctx); err != nil || sent != 1 {
		t.Fatalf("Drain sent=%d err=%v, want 1", sent, err)
	}
	if transport.calls != 1 {
		t.Fatalf("transport calls = %d, want 1", transport.calls)
	}
}

// A type with no email event behind it is not mailed at all.
func TestNonEmailableTypeIsNotEnqueued(t *testing.T) {
	store := newMemStore()
	c := &clock{now: time.Now().UTC()}
	o := newOutbox(t, store, &stubTransport{}, stubRenderer{}, c)
	rec := failedRecord()
	rec.Type = domain.NotificationNeedsInput
	if err := o.EmailNotification(context.Background(), rec); err != nil {
		t.Fatalf("EmailNotification: %v", err)
	}
	if len(store.entries) != 0 {
		t.Fatalf("entries = %d for a non-emailable type, want 0", len(store.entries))
	}
}

// Every session-scoped fact is emailable, folded into the coarse event the user
// actually chose.
func TestSessionScopedTypesAreEmailable(t *testing.T) {
	for typ, want := range map[domain.NotificationType]domain.EmailEvent{
		domain.NotificationHumanQuestionRequired: domain.EmailEventNeedsAttention,
		domain.NotificationRepairExhausted:       domain.EmailEventNeedsAttention,
		domain.NotificationIntegrationFailed:     domain.EmailEventFailed,
	} {
		got, ok := typ.EmailEventOf()
		if !ok || got != want {
			t.Fatalf("%s email event = %q ok=%v, want %q", typ, got, ok, want)
		}
	}
}
