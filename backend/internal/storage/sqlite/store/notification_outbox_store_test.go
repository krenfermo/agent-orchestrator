package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

// The outbox against real SQLite. The claim/complete transitions are
// conditional updates, and it is those conditions -- not anything in Go -- that
// make two workers safe and a crashed daemon recoverable.

func seedOutboxNotification(t *testing.T, s *sqlite.Store, id string) domain.NotificationRecord {
	t.Helper()
	ctx := context.Background()
	sess, err := s.CreateSession(ctx, sampleRecord("mer"))
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	rec := domain.NotificationRecord{
		ID: id, SessionID: sess.ID, ProjectID: sess.ProjectID,
		Type: domain.NotificationIntegrationFailed, DedupeKey: "sf:integration_failed:" + id,
		Title: "review failed", Status: domain.NotificationUnread,
		CreatedAt: time.Now().UTC().Truncate(time.Second),
	}
	created, _, err := s.CreateNotification(ctx, rec)
	if err != nil {
		t.Fatalf("create notification: %v", err)
	}
	return created
}

func outboxEntry(rec domain.NotificationRecord, now time.Time) domain.EmailOutboxEntry {
	return domain.EmailOutboxEntry{
		NotificationID: rec.ID,
		Recipient:      domain.NotificationRecipientLocal,
		Subject:        "[AO] review failed",
		Body:           "An integration AO runs for this session failed.",
		MaxAttempts:    5,
		NextAttemptAt:  now,
		CreatedAt:      now,
	}
}

func TestOutboxStore_EnqueueIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	rec := seedOutboxNotification(t, s, "ntf_1")
	now := time.Now().UTC().Truncate(time.Second)

	enqueued, err := s.EnqueueNotificationEmail(ctx, outboxEntry(rec, now))
	if err != nil || !enqueued {
		t.Fatalf("first enqueue enqueued=%v err=%v", enqueued, err)
	}
	enqueued, err = s.EnqueueNotificationEmail(ctx, outboxEntry(rec, now))
	if err != nil || enqueued {
		t.Fatalf("second enqueue enqueued=%v err=%v, want false nil", enqueued, err)
	}

	entry, ok, err := s.GetNotificationEmail(ctx, "ntf_1")
	if err != nil || !ok {
		t.Fatalf("GetNotificationEmail ok=%v err=%v", ok, err)
	}
	if entry.State != domain.EmailOutboxPending || entry.AttemptCount != 0 {
		t.Fatalf("entry = %+v, want a fresh pending row", entry)
	}
}

// Exactly one worker may claim a due entry. The second gets nothing rather than
// sending a duplicate.
func TestOutboxStore_ClaimIsExclusive(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	rec := seedOutboxNotification(t, s, "ntf_1")
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := s.EnqueueNotificationEmail(ctx, outboxEntry(rec, now)); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	first, err := s.ClaimNotificationEmail(ctx, "ntf_1", now, now.Add(5*time.Minute))
	if err != nil || !first {
		t.Fatalf("first claim claimed=%v err=%v, want true", first, err)
	}
	second, err := s.ClaimNotificationEmail(ctx, "ntf_1", now, now.Add(5*time.Minute))
	if err != nil || second {
		t.Fatalf("second claim claimed=%v err=%v, want false", second, err)
	}

	entry, _, err := s.GetNotificationEmail(ctx, "ntf_1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	// The attempt is spent at claim time, so a crash mid-send still costs one.
	if entry.State != domain.EmailOutboxSending || entry.AttemptCount != 1 {
		t.Fatalf("entry = %+v, want sending with one attempt spent", entry)
	}
}

func TestOutboxStore_TerminalTransitionsRequireAClaim(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	rec := seedOutboxNotification(t, s, "ntf_1")
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := s.EnqueueNotificationEmail(ctx, outboxEntry(rec, now)); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// Not claimed: completing must be a no-op rather than skipping the queue.
	if err := s.MarkNotificationEmailSent(ctx, "ntf_1", now); err != nil {
		t.Fatalf("mark sent: %v", err)
	}
	entry, _, err := s.GetNotificationEmail(ctx, "ntf_1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if entry.State != domain.EmailOutboxPending {
		t.Fatalf("state = %q, want pending: an unclaimed entry was completed", entry.State)
	}

	if _, err := s.ClaimNotificationEmail(ctx, "ntf_1", now, now.Add(time.Minute)); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := s.MarkNotificationEmailSent(ctx, "ntf_1", now); err != nil {
		t.Fatalf("mark sent: %v", err)
	}
	if entry, _, err = s.GetNotificationEmail(ctx, "ntf_1"); err != nil {
		t.Fatalf("get: %v", err)
	}
	if entry.State != domain.EmailOutboxSent || entry.CompletedAt.IsZero() {
		t.Fatalf("entry = %+v, want sent with a completion time", entry)
	}
}

// The restart guarantee at the storage layer.
func TestOutboxStore_ReclaimReturnsExpiredLeases(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	rec := seedOutboxNotification(t, s, "ntf_1")
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := s.EnqueueNotificationEmail(ctx, outboxEntry(rec, now)); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := s.ClaimNotificationEmail(ctx, "ntf_1", now, now.Add(time.Minute)); err != nil {
		t.Fatalf("claim: %v", err)
	}

	// Still leased: a healthy worker keeps it.
	reclaimed, err := s.ReclaimExpiredNotificationEmailLeases(ctx, now)
	if err != nil || reclaimed != 0 {
		t.Fatalf("reclaim inside the lease = %d err=%v, want 0", reclaimed, err)
	}

	later := now.Add(2 * time.Minute)
	if reclaimed, err = s.ReclaimExpiredNotificationEmailLeases(ctx, later); err != nil || reclaimed != 1 {
		t.Fatalf("reclaim after expiry = %d err=%v, want 1", reclaimed, err)
	}

	due, err := s.ListDueNotificationEmails(ctx, later, 10)
	if err != nil || len(due) != 1 {
		t.Fatalf("due after reclaim = %d err=%v, want 1", len(due), err)
	}
	// The attempt already spent is not refunded.
	if due[0].AttemptCount != 1 {
		t.Fatalf("attempts = %d after reclaim, want the spent attempt kept", due[0].AttemptCount)
	}
}

// Only due entries are listed, so a backoff actually holds.
func TestOutboxStore_ListDueRespectsTheBackoff(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	rec := seedOutboxNotification(t, s, "ntf_1")
	now := time.Now().UTC().Truncate(time.Second)
	entry := outboxEntry(rec, now)
	entry.NextAttemptAt = now.Add(time.Hour)
	if _, err := s.EnqueueNotificationEmail(ctx, entry); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	due, err := s.ListDueNotificationEmails(ctx, now, 10)
	if err != nil || len(due) != 0 {
		t.Fatalf("due before the deadline = %d err=%v, want 0", len(due), err)
	}
	if due, err = s.ListDueNotificationEmails(ctx, now.Add(2*time.Hour), 10); err != nil || len(due) != 1 {
		t.Fatalf("due after the deadline = %d err=%v, want 1", len(due), err)
	}
}

func TestOutboxStore_CountsByState(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	rec := seedOutboxNotification(t, s, "ntf_1")
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := s.EnqueueNotificationEmail(ctx, outboxEntry(rec, now)); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	count, err := s.CountNotificationEmailsByState(ctx, domain.EmailOutboxPending)
	if err != nil || count != 1 {
		t.Fatalf("pending = %d err=%v, want 1", count, err)
	}
	if count, err = s.CountNotificationEmailsByState(ctx, domain.EmailOutboxDead); err != nil || count != 0 {
		t.Fatalf("dead = %d err=%v, want 0", count, err)
	}
}
