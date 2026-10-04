package sqlite

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"
)

// Migration 0154 rebuilds the notifications table again, to widen the type
// CHECK and add the canonical model columns. A rebuild is the one migration
// shape that can silently lose data, so what matters is that every existing row
// survives it unchanged, that the backfills are right, and that the Down path
// is valid against data this migration's Up permits.

func openMigrationDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := "file:" + filepath.Join(t.TempDir(), "ao.db") + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func seedNotificationFixtures(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO projects (id, path, registered_at) VALUES ('p1', '/tmp/p1', CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO sessions (id, project_id, num, kind, activity_state, activity_last_at, is_terminated, created_at, updated_at)
		VALUES ('s1', 'p1', 1, 'worker', 'idle', CURRENT_TIMESTAMP, 0, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("seed session: %v", err)
	}
}

func downTo(t *testing.T, db *sql.DB, version int64) error {
	t.Helper()
	gooseMu.Lock()
	defer gooseMu.Unlock()
	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatalf("set dialect: %v", err)
	}
	return goose.DownTo(db, "migrations", version)
}

func TestMigration0154PreservesRowsAndBackfillsTheModel(t *testing.T) {
	db := openMigrationDB(t)
	upTo(t, db, 153)
	seedNotificationFixtures(t, db)

	// One read row and one unread row, across two severities.
	if _, err := db.Exec(`
		INSERT INTO notifications (id, session_id, project_id, workflow_run_id, pr_url, dedupe_key, type, title, body, status, created_at)
		VALUES
			('n1', 's1', 'p1', '', '', '', 'needs_input', 'legacy title', 'legacy body', 'read', CURRENT_TIMESTAMP),
			('n2', 's1', 'p1', '', '', '', 'task_completed', 'finished', '', 'unread', CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("seed notifications: %v", err)
	}

	upTo(t, db, 154)

	var title, body, recipient, severity, delivery, source string
	var readAt sql.NullTime
	if err := db.QueryRow(`
		SELECT title, body, recipient, severity, delivery_state, source, read_at
		FROM notifications WHERE id = 'n1'`,
	).Scan(&title, &body, &recipient, &severity, &delivery, &source, &readAt); err != nil {
		t.Fatalf("read migrated notification: %v", err)
	}
	if title != "legacy title" || body != "legacy body" {
		t.Fatalf("the rebuild altered an existing row: title=%q body=%q", title, body)
	}
	if recipient != "local" {
		t.Fatalf("recipient = %q, want the single local principal", recipient)
	}
	// needs_input is something waiting on a person.
	if severity != "warning" {
		t.Fatalf("severity for needs_input = %q, want warning", severity)
	}
	if delivery != "delivered" {
		t.Fatalf("delivery_state = %q, want delivered: the row itself is the in-app delivery", delivery)
	}
	if source != "lifecycle" {
		t.Fatalf("source = %q, want the lifecycle backfill", source)
	}
	// status = 'read' <=> read_at IS NOT NULL, on historical rows too.
	if !readAt.Valid {
		t.Fatal("a row already marked read has no read_at after the backfill")
	}

	if err := db.QueryRow(`SELECT severity, read_at FROM notifications WHERE id = 'n2'`).
		Scan(&severity, &readAt); err != nil {
		t.Fatalf("read second notification: %v", err)
	}
	if severity != "info" {
		t.Fatalf("severity for task_completed = %q, want info", severity)
	}
	if readAt.Valid {
		t.Fatal("an unread row was given a read_at")
	}
}

func TestMigration0154AdmitsTheSessionScopedTypes(t *testing.T) {
	db := openMigrationDB(t)
	upTo(t, db, 154)
	seedNotificationFixtures(t, db)

	for _, typ := range []string{"human_question_required", "repair_exhausted", "integration_failed"} {
		if _, err := db.Exec(`
			INSERT INTO notifications (id, session_id, project_id, dedupe_key, type, title, status, created_at, severity, source)
			VALUES (?, 's1', 'p1', ?, ?, 'x', 'unread', CURRENT_TIMESTAMP, 'warning', 'lifecycle')`,
			"n-"+typ, "sf:"+typ+":s1:1", typ); err != nil {
			t.Fatalf("insert %s: %v", typ, err)
		}
	}

	// The permanent event dedupe still holds for the new types.
	if _, err := db.Exec(`
		INSERT INTO notifications (id, session_id, project_id, dedupe_key, type, title, status, created_at, severity, source)
		VALUES ('dup', 's1', 'p1', 'sf:repair_exhausted:s1:1', 'repair_exhausted', 'x', 'unread', CURRENT_TIMESTAMP, 'warning', 'lifecycle')`,
	); err == nil {
		t.Fatal("a second row for the same event was accepted; the event dedupe index is not holding")
	}
}

// The reviewer finding this test exists for.
//
// 0154's Up deliberately permits several unresolved run-scoped rows on ONE
// session under distinct dedupe keys -- two repairs on different problems never
// resolve, so they must both be able to exist. A Down migration that tried to
// rebuild the narrow table would abort part-way on exactly that data. This
// asserts a rollback succeeds with precisely the rows the Up permits.
func TestMigration0154DownIsValidWithPostUpData(t *testing.T) {
	db := openMigrationDB(t)
	upTo(t, db, 154)
	seedNotificationFixtures(t, db)

	// Two unresolved repair exhaustions on one session, distinct keys, same
	// (session, type, pr_url) triple -- the shape that broke the earlier draft.
	if _, err := db.Exec(`
		INSERT INTO notifications (id, session_id, project_id, dedupe_key, type, title, status, created_at, severity, source)
		VALUES
			('r1', 's1', 'p1', 'sf:repair_exhausted:s1:ci#3', 'repair_exhausted', 'x', 'unread', CURRENT_TIMESTAMP, 'warning', 'lifecycle'),
			('r2', 's1', 'p1', 'sf:repair_exhausted:s1:review#3', 'repair_exhausted', 'x', 'unread', CURRENT_TIMESTAMP, 'warning', 'lifecycle'),
			('q1', 's1', 'p1', 'sf:human_question:s1:t0', 'human_question_required', 'x', 'unread', CURRENT_TIMESTAMP, 'warning', 'lifecycle')`,
	); err != nil {
		t.Fatalf("seed post-Up data: %v", err)
	}

	if err := downTo(t, db, 153); err != nil {
		t.Fatalf("rollback failed on data this migration's Up permits: %v", err)
	}

	// The rollback is a deliberate one-way no-op, so the rows are still there
	// and readable rather than half-deleted.
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM notifications`).Scan(&count); err != nil {
		t.Fatalf("count after rollback: %v", err)
	}
	if count != 3 {
		t.Fatalf("notifications after rollback = %d, want all 3 preserved", count)
	}
}

func TestMigration0155EmailOutboxRoundTrips(t *testing.T) {
	db := openMigrationDB(t)
	upTo(t, db, 155)
	seedNotificationFixtures(t, db)
	if _, err := db.Exec(`
		INSERT INTO notifications (id, session_id, project_id, dedupe_key, type, title, status, created_at, severity, source)
		VALUES ('n1', 's1', 'p1', 'k1', 'workflow_failed', 'x', 'unread', CURRENT_TIMESTAMP, 'critical', 'workflow')`); err != nil {
		t.Fatalf("seed notification: %v", err)
	}

	if _, err := db.Exec(`
		INSERT INTO notification_email_outbox
			(notification_id, recipient, state, subject, body, attempt_count, max_attempts, next_attempt_at, last_error, created_at, updated_at)
		VALUES ('n1', 'local', 'pending', 's', 'b', 0, 5, CURRENT_TIMESTAMP, '', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("insert outbox entry: %v", err)
	}

	// One email owed per notification: the notification's own permanent event
	// dedupe is inherited rather than duplicated.
	if _, err := db.Exec(`
		INSERT INTO notification_email_outbox
			(notification_id, recipient, state, subject, body, attempt_count, max_attempts, next_attempt_at, last_error, created_at, updated_at)
		VALUES ('n1', 'local', 'pending', 's', 'b', 0, 5, CURRENT_TIMESTAMP, '', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
	); err == nil {
		t.Fatal("a second outbox entry for one notification was accepted")
	}

	// Deleting the notification takes its owed email with it.
	if _, err := db.Exec(`DELETE FROM notifications WHERE id = 'n1'`); err != nil {
		t.Fatalf("delete notification: %v", err)
	}
	var remaining int
	if err := db.QueryRow(`SELECT COUNT(*) FROM notification_email_outbox`).Scan(&remaining); err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("outbox rows after the notification was deleted = %d, want 0", remaining)
	}

	if _, err := db.Exec(`
		INSERT INTO notification_email_outbox
			(notification_id, recipient, state, subject, body, attempt_count, max_attempts, next_attempt_at, last_error, created_at, updated_at)
		VALUES ('nope', 'local', 'bogus', 's', 'b', 0, 5, CURRENT_TIMESTAMP, '', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
	); err == nil {
		t.Fatal("an unknown outbox state was accepted")
	}
}
