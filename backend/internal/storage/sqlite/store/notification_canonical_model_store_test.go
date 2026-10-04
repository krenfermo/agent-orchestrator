package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// The canonical model columns (P4-D) and the restart/dedup guarantees that rest
// on them.

func sessionFactRecord(
	id string,
	session domain.SessionID,
	project domain.ProjectID,
	typ domain.NotificationType,
	dedupeKey string,
	at time.Time,
) domain.NotificationRecord {
	return domain.NotificationRecord{
		ID:            id,
		SessionID:     session,
		ProjectID:     project,
		Type:          typ,
		DedupeKey:     dedupeKey,
		SourceEventID: dedupeKey,
		Source:        domain.NotificationSourceLifecycle,
		Title:         "checkout-flow is waiting on your decision",
		Body:          "The agent is stopped on a permission prompt.",
		Status:        domain.NotificationUnread,
		CreatedAt:     at,
	}
}

func seedFactSession(t *testing.T, s interface {
	CreateSession(context.Context, domain.SessionRecord) (domain.SessionRecord, error)
}) domain.SessionRecord {
	t.Helper()
	sess, err := s.CreateSession(context.Background(), sampleRecord("mer"))
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return sess
}

// A producer that names only the type gets the whole model filled in, so
// "unaddressed, unrated, in-app only" means one thing everywhere.
func TestNotificationStore_CanonicalModelDefaults(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	sess := seedFactSession(t, s)

	rec := sessionFactRecord("ntf_1", sess.ID, sess.ProjectID,
		domain.NotificationHumanQuestionRequired, "sf:human_question:mer-1:pause-1",
		time.Now().UTC().Truncate(time.Second))
	created, inserted, err := s.CreateNotification(ctx, rec)
	if err != nil || !inserted {
		t.Fatalf("insert inserted=%v err=%v", inserted, err)
	}
	if created.Recipient != domain.NotificationRecipientLocal {
		t.Fatalf("Recipient = %q, want the local principal", created.Recipient)
	}
	// human_question_required is something waiting on a person.
	if created.Severity != domain.NotificationSeverityWarning {
		t.Fatalf("Severity = %q, want warning", created.Severity)
	}
	// Writing the row IS the in-app delivery.
	if created.DeliveryState != domain.NotificationDeliveryDelivered {
		t.Fatalf("DeliveryState = %q, want delivered", created.DeliveryState)
	}
	if created.Source != domain.NotificationSourceLifecycle || created.SourceEventID != rec.DedupeKey {
		t.Fatalf("provenance not round-tripped: source=%q event=%q", created.Source, created.SourceEventID)
	}
	if !created.ReadAt.IsZero() {
		t.Fatalf("ReadAt = %v on an unread row, want zero", created.ReadAt)
	}
}

// Marking read writes both halves of the acknowledgement, so a client can ask
// WHEN and not only WHETHER.
func TestNotificationStore_MarkReadStampsReadAt(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	sess := seedFactSession(t, s)
	now := time.Now().UTC().Truncate(time.Second)

	rec := sessionFactRecord("ntf_1", sess.ID, sess.ProjectID,
		domain.NotificationRepairExhausted, "sf:repair_exhausted:mer-1:ci#3", now)
	if _, _, err := s.CreateNotification(ctx, rec); err != nil {
		t.Fatalf("create: %v", err)
	}

	readAt := now.Add(time.Minute)
	got, ok, err := s.MarkNotificationRead(ctx, "ntf_1", readAt)
	if err != nil || !ok {
		t.Fatalf("MarkNotificationRead ok=%v err=%v", ok, err)
	}
	if got.Status != domain.NotificationRead {
		t.Fatalf("Status = %q, want read", got.Status)
	}
	if !got.ReadAt.Equal(readAt) {
		t.Fatalf("ReadAt = %v, want %v", got.ReadAt, readAt)
	}
}

// The restart guarantee, at the layer that actually enforces it. The producer
// derives the same key from the same durable fact after a restart; storage
// turns the second write into a no-op even though the user has already read the
// first -- which is exactly when the open-row rule would have stopped helping.
func TestNotificationStore_SessionFactSurvivesRestartAndRead(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	sess := seedFactSession(t, s)
	now := time.Now().UTC().Truncate(time.Second)
	key := "sf:human_question:" + string(sess.ID) + ":2026-09-04T09:00:00Z"

	first := sessionFactRecord("ntf_1", sess.ID, sess.ProjectID, domain.NotificationHumanQuestionRequired, key, now)
	if _, inserted, err := s.CreateNotification(ctx, first); err != nil || !inserted {
		t.Fatalf("first insert inserted=%v err=%v", inserted, err)
	}
	if _, _, err := s.MarkNotificationRead(ctx, "ntf_1", now.Add(time.Minute)); err != nil {
		t.Fatalf("mark read: %v", err)
	}

	// The daemon restarts and re-observes the same stored pause.
	replay := first
	replay.ID = "ntf_2"
	replay.CreatedAt = now.Add(2 * time.Hour)
	existing, inserted, err := s.CreateNotification(ctx, replay)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if inserted {
		t.Fatal("a restart re-announced a question the user had already read")
	}
	if existing.ID != "ntf_1" {
		t.Fatalf("dedupe returned %q, want the original row", existing.ID)
	}
}

// Two unresolved run-scoped rows on ONE session under distinct keys. Nothing
// ever resolves them, so an open-row rule keyed on (session, type, pr) would
// silence the second forever. The open index is partial on dedupe_key = ”,
// which is what lets both exist.
func TestNotificationStore_TwoUnresolvedSessionFactsCoexist(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	sess := seedFactSession(t, s)
	now := time.Now().UTC().Truncate(time.Second)

	for i, key := range []string{
		"sf:repair_exhausted:" + string(sess.ID) + ":ci-failure#3",
		"sf:repair_exhausted:" + string(sess.ID) + ":review-feedback#3",
	} {
		rec := sessionFactRecord(
			[]string{"ntf_1", "ntf_2"}[i], sess.ID, sess.ProjectID,
			domain.NotificationRepairExhausted, key, now.Add(time.Duration(i)*time.Second),
		)
		if _, inserted, err := s.CreateNotification(ctx, rec); err != nil || !inserted {
			t.Fatalf("insert %d inserted=%v err=%v", i, inserted, err)
		}
	}

	rows, err := s.ListNotifications(ctx, domain.NotificationListAll, time.Time{}, "", 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	exhausted := 0
	for _, row := range rows {
		if row.Type == domain.NotificationRepairExhausted {
			exhausted++
		}
	}
	if exhausted != 2 {
		t.Fatalf("repair_exhausted rows = %d, want 2 distinct repairs", exhausted)
	}
}

// The unread badge is served independently of the history page, and both agree.
func TestNotificationStore_UnreadCountTracksAcknowledgement(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	sess := seedFactSession(t, s)
	now := time.Now().UTC().Truncate(time.Second)

	for i, key := range []string{"a", "b", "c"} {
		rec := sessionFactRecord(
			"ntf_"+key, sess.ID, sess.ProjectID,
			domain.NotificationIntegrationFailed, "sf:integration_failed:x:"+key,
			now.Add(time.Duration(i)*time.Second),
		)
		if _, _, err := s.CreateNotification(ctx, rec); err != nil {
			t.Fatalf("create %s: %v", key, err)
		}
	}

	count, err := s.CountUnreadNotifications(ctx)
	if err != nil || count != 3 {
		t.Fatalf("unread count = %d err=%v, want 3", count, err)
	}
	if _, _, err := s.MarkNotificationRead(ctx, "ntf_a", now); err != nil {
		t.Fatalf("mark read: %v", err)
	}
	if count, err = s.CountUnreadNotifications(ctx); err != nil || count != 2 {
		t.Fatalf("unread count after one ack = %d err=%v, want 2", count, err)
	}
	if _, err := s.MarkAllNotificationsRead(ctx, now); err != nil {
		t.Fatalf("mark all read: %v", err)
	}
	if count, err = s.CountUnreadNotifications(ctx); err != nil || count != 0 {
		t.Fatalf("unread count after mark-all = %d err=%v, want 0", count, err)
	}
}

// Mark-all-read is idempotent: replaying it changes nothing and reports no work.
func TestNotificationStore_MarkAllReadIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedProject(t, s, "mer")
	sess := seedFactSession(t, s)
	now := time.Now().UTC().Truncate(time.Second)

	rec := sessionFactRecord("ntf_1", sess.ID, sess.ProjectID,
		domain.NotificationHumanQuestionRequired, "sf:human_question:x:1", now)
	if _, _, err := s.CreateNotification(ctx, rec); err != nil {
		t.Fatalf("create: %v", err)
	}
	first, err := s.MarkAllNotificationsRead(ctx, now)
	if err != nil || first != 1 {
		t.Fatalf("first mark-all updated=%d err=%v, want 1", first, err)
	}
	second, err := s.MarkAllNotificationsRead(ctx, now.Add(time.Hour))
	if err != nil || second != 0 {
		t.Fatalf("replayed mark-all updated=%d err=%v, want 0", second, err)
	}
}
