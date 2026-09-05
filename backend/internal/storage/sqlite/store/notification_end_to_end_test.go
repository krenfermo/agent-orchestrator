package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/notify"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	notificationsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/notification"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/notification/emailoutbox"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

// The whole chain over a real database: a durable fact goes in, one notification
// and one owed email come out, and neither is duplicated by a restart.
//
// Each layer is separately unit-tested; what this pins is that they agree --
// specifically that the key the producer derives is the key storage enforces,
// which is the claim a restart actually tests.

type chainRenderer struct{}

func (chainRenderer) RenderNotificationEmail(rec domain.NotificationRecord) (ports.EmailMessage, error) {
	return ports.EmailMessage{Subject: "[AO] " + rec.Title, Body: rec.Body}, nil
}

type countingTransport struct{ sent []ports.EmailMessage }

func (c *countingTransport) Send(_ context.Context, msg ports.EmailMessage) error {
	c.sent = append(c.sent, msg)
	return nil
}

// chain is one daemon's worth of notification wiring over a shared store.
type chain struct {
	producer  *notificationsvc.SessionFactNotifier
	outbox    *emailoutbox.Outbox
	transport *countingTransport
}

// newChain builds the wiring a daemon builds. Calling it twice against the same
// store is how a restart is simulated: new objects, no shared memory, same rows.
func newChain(t *testing.T, s *sqlite.Store, ids *int, now func() time.Time) *chain {
	t.Helper()
	transport := &countingTransport{}
	outbox := emailoutbox.New(emailoutbox.Deps{
		Store: s, Transport: transport, Renderer: chainRenderer{}, Clock: now,
	})
	writer := notify.New(notify.Deps{
		Store:   s,
		Emailer: outbox,
		Clock:   now,
		NewID:   func() string { *ids++; return "ntf_" + string(rune('a'+*ids-1)) },
	})
	return &chain{
		producer:  notificationsvc.NewSessionFactNotifier(notificationsvc.SessionFactDeps{Notifier: writer, Clock: now}),
		outbox:    outbox,
		transport: transport,
	}
}

func countAll(t *testing.T, s *sqlite.Store) int {
	t.Helper()
	rows, err := s.ListNotifications(context.Background(), domain.NotificationListAll, time.Time{}, "", 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	return len(rows)
}

// A pending decision, observed twice across a daemon restart. One notification,
// one email.
func TestEndToEnd_SessionFactSurvivesADaemonRestart(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	sess, err := s.CreateSession(ctx, sampleRecord("mer"))
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	pausedAt := time.Date(2026, 9, 4, 9, 0, 0, 0, time.UTC)
	now := pausedAt
	clock := func() time.Time { return now }
	ids := 0

	fact := ports.SessionFact{
		Kind:      ports.SessionFactHumanQuestion,
		SessionID: sess.ID,
		ProjectID: sess.ProjectID,
		// The pause's durable identity: re-read after a restart, it is the same.
		ScopeID:            ports.PauseScopeID(pausedAt),
		SessionDisplayName: "checkout-flow",
		ObservedAt:         pausedAt,
	}

	first := newChain(t, s, &ids, clock)
	if err := first.producer.Record(ctx, fact); err != nil {
		t.Fatalf("first Record: %v", err)
	}
	if got := countAll(t, s); got != 1 {
		t.Fatalf("notifications = %d, want 1", got)
	}

	// The daemon dies before the outbox ever ran. The owing is durable, so the
	// email is not lost with the process.
	entry, ok, err := s.GetNotificationEmail(ctx, "ntf_a")
	if err != nil || !ok {
		t.Fatalf("owed email ok=%v err=%v, want it recorded before any send", ok, err)
	}
	if entry.State != domain.EmailOutboxPending {
		t.Fatalf("owed email state = %q, want pending", entry.State)
	}

	// Restart: fresh wiring, same rows, and the same fact re-observed.
	now = pausedAt.Add(2 * time.Hour)
	second := newChain(t, s, &ids, clock)
	if err := second.producer.Record(ctx, fact); err != nil {
		t.Fatalf("replayed Record: %v", err)
	}
	if got := countAll(t, s); got != 1 {
		t.Fatalf("notifications after a restart = %d, want 1", got)
	}

	sent, err := second.outbox.Drain(ctx)
	if err != nil || sent != 1 {
		t.Fatalf("Drain sent=%d err=%v, want 1", sent, err)
	}
	if len(second.transport.sent) != 1 {
		t.Fatalf("emails sent = %d, want exactly 1", len(second.transport.sent))
	}

	// And a second drain sends nothing more.
	if sent, err = second.outbox.Drain(ctx); err != nil || sent != 0 {
		t.Fatalf("second Drain sent=%d err=%v, want 0", sent, err)
	}
	if len(second.transport.sent) != 1 {
		t.Fatalf("emails sent after a second drain = %d, want 1", len(second.transport.sent))
	}
}

// The autonomous policy, end to end: AO retrying is silent, AO out of moves is
// not. This is the rule that decides whether the channel stays worth reading.
func TestEndToEnd_RepairIsSilentUntilItIsExhausted(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	sess, err := s.CreateSession(ctx, sampleRecord("mer"))
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	now := time.Date(2026, 9, 4, 9, 0, 0, 0, time.UTC)
	ids := 0
	c := newChain(t, s, &ids, func() time.Time { return now })

	base := ports.SessionFact{
		SessionID: sess.ID, ProjectID: sess.ProjectID,
		ScopeID: ports.RepairScopeID("ci-failure:pr-1", 3),
	}

	// Three attempts. AO is working; nobody is told.
	for i := 0; i < 3; i++ {
		attempt := base
		attempt.Kind = ports.SessionFactRepairAttempted
		if err := c.producer.Record(ctx, attempt); err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if got := countAll(t, s); got != 0 {
		t.Fatalf("notifications during repair = %d, want 0", got)
	}

	// The budget is spent.
	exhausted := base
	exhausted.Kind = ports.SessionFactRepairExhausted
	exhausted.Detail = "AO tried 3 times."
	if err := c.producer.Record(ctx, exhausted); err != nil {
		t.Fatalf("exhaustion: %v", err)
	}
	rows, err := s.ListNotifications(ctx, domain.NotificationListAll, time.Time{}, "", 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 || rows[0].Type != domain.NotificationRepairExhausted {
		t.Fatalf("rows = %+v, want exactly one repair_exhausted", rows)
	}

	// Every later observation of the same spent budget stays quiet.
	for i := 0; i < 3; i++ {
		if err := c.producer.Record(ctx, exhausted); err != nil {
			t.Fatalf("re-observed exhaustion: %v", err)
		}
	}
	if got := countAll(t, s); got != 1 {
		t.Fatalf("notifications after re-observation = %d, want 1", got)
	}

	// Raising the budget is a genuinely new repair, and may speak again.
	raised := exhausted
	raised.ScopeID = ports.RepairScopeID("ci-failure:pr-1", 5)
	if err := c.producer.Record(ctx, raised); err != nil {
		t.Fatalf("raised budget: %v", err)
	}
	if got := countAll(t, s); got != 2 {
		t.Fatalf("notifications after raising the budget = %d, want 2", got)
	}
}

// P4-D section 8: with no email transport at all, the in-app notification is
// unaffected and nothing fails.
func TestEndToEnd_InAppWorksWithNoEmailTransport(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	sess, err := s.CreateSession(ctx, sampleRecord("mer"))
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	now := func() time.Time { return time.Date(2026, 9, 4, 9, 0, 0, 0, time.UTC) }

	// An outbox with neither transport nor renderer: the shape of an install
	// that never configured SMTP.
	outbox := emailoutbox.New(emailoutbox.Deps{Store: s, Clock: now})
	writer := notify.New(notify.Deps{
		Store: s, Emailer: outbox, Clock: now,
		NewID: func() string { return "ntf_1" },
	})
	producer := notificationsvc.NewSessionFactNotifier(notificationsvc.SessionFactDeps{Notifier: writer, Clock: now})

	err = producer.Record(ctx, ports.SessionFact{
		Kind: ports.SessionFactIntegrationFailed, SessionID: sess.ID, ProjectID: sess.ProjectID,
		ScopeID: "run-1", Detail: "the reviewer could not start",
	})
	if err != nil {
		t.Fatalf("Record with no email configured: %v", err)
	}
	if got := countAll(t, s); got != 1 {
		t.Fatalf("notifications = %d, want 1: in-app must not depend on email", got)
	}
	// Nothing is owed, so nothing accumulates that could never be sent.
	if _, ok, err := s.GetNotificationEmail(ctx, "ntf_1"); err != nil || ok {
		t.Fatalf("owed email ok=%v err=%v, want none", ok, err)
	}
	if sent, err := outbox.Drain(ctx); err != nil || sent != 0 {
		t.Fatalf("Drain sent=%d err=%v, want a clean no-op", sent, err)
	}
}

// A notification whose type carries no email event owes nothing, and the
// producer still refuses a fact it cannot make idempotent.
func TestEndToEnd_ProducerRejectsAnUnidentifiableFact(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	sess, err := s.CreateSession(ctx, sampleRecord("mer"))
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	ids := 0
	c := newChain(t, s, &ids, func() time.Time { return time.Now().UTC() })

	err = c.producer.Record(ctx, ports.SessionFact{
		Kind: ports.SessionFactHumanQuestion, SessionID: sess.ID, ProjectID: sess.ProjectID,
	})
	if !errors.Is(err, notificationsvc.ErrInvalidSessionFact) {
		t.Fatalf("Record error = %v, want ErrInvalidSessionFact", err)
	}
	if got := countAll(t, s); got != 0 {
		t.Fatalf("notifications = %d, want 0", got)
	}
}
