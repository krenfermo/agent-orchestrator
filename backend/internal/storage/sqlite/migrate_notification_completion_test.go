package sqlite

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// Migration 0122 rebuilds the notifications table. A rebuild is the one
// migration shape that can silently lose data, so what matters here is that
// every existing row survives it unchanged, and that the three new schema facts
// (the two new types, a session-less run-level row, and permanent event dedupe)
// actually hold afterwards.
func TestMigration0122PreservesRowsAndAdmitsCompletions(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "ao.db") + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	upTo(t, db, 121)

	if _, err := db.Exec(`INSERT INTO projects (id, path, registered_at) VALUES ('p1', '/tmp/p1', CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO sessions (id, project_id, num, kind, activity_state, activity_last_at, is_terminated, created_at, updated_at)
		VALUES ('s1', 'p1', 1, 'worker', 'idle', CURRENT_TIMESTAMP, 0, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO notifications (id, session_id, project_id, pr_url, type, title, body, status, created_at)
		VALUES ('n1', 's1', 'p1', '', 'needs_input', 'legacy title', 'legacy body', 'unread', CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("seed notification: %v", err)
	}

	upTo(t, db, 122)

	var title, body, workflowRunID, dedupeKey string
	if err := db.QueryRow(
		`SELECT title, body, workflow_run_id, dedupe_key FROM notifications WHERE id = 'n1'`,
	).Scan(&title, &body, &workflowRunID, &dedupeKey); err != nil {
		t.Fatalf("read migrated notification: %v", err)
	}
	if title != "legacy title" || body != "legacy body" {
		t.Fatalf("the rebuild altered an existing row: title=%q body=%q", title, body)
	}
	// The two new columns default to "not applicable", so no historical row is
	// retroactively treated as an event-keyed completion.
	if workflowRunID != "" || dedupeKey != "" {
		t.Fatalf("migrated row = run %q key %q, want both empty", workflowRunID, dedupeKey)
	}

	// A task completion: session-anchored, event-keyed.
	if _, err := db.Exec(`
		INSERT INTO notifications (id, session_id, project_id, workflow_run_id, dedupe_key, type, title, status, created_at)
		VALUES ('n2', 's1', 'p1', '', 's1@t0', 'task_completed', 'finished', 'unread', CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("insert task_completed: %v", err)
	}
	// A workflow completion: no session at all, which the old NOT NULL forbade.
	if _, err := db.Exec(`
		INSERT INTO notifications (id, session_id, project_id, workflow_run_id, dedupe_key, type, title, status, created_at)
		VALUES ('n3', NULL, 'p1', 'wf-1', 'wf-1', 'workflow_completed', 'run finished', 'unread', CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("insert workflow_completed: %v", err)
	}

	// The permanent event index: the same event cannot land twice even after
	// the first row has been read.
	if _, err := db.Exec(`UPDATE notifications SET status = 'read', resolved_at = CURRENT_TIMESTAMP WHERE id = 'n3'`); err != nil {
		t.Fatalf("mark read: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO notifications (id, session_id, project_id, workflow_run_id, dedupe_key, type, title, status, created_at)
		VALUES ('n4', NULL, 'p1', 'wf-1', 'wf-1', 'workflow_completed', 'run finished', 'unread', CURRENT_TIMESTAMP)`); err == nil {
		t.Fatal("the same workflow run was allowed to report completion twice")
	}

	// Every row is anchored to a session or a run; neither is not an option.
	if _, err := db.Exec(`
		INSERT INTO notifications (id, session_id, project_id, workflow_run_id, dedupe_key, type, title, status, created_at)
		VALUES ('n5', NULL, 'p1', '', 'orphan', 'task_completed', 'orphan', 'unread', CURRENT_TIMESTAMP)`); err == nil {
		t.Fatal("a notification with neither a session nor a run was accepted")
	}
}

// Migration 0123 only adds columns, so the one thing worth pinning is that an
// install that never configures email reads as "off" with a sane default port,
// and that the TLS column refuses a value the sender could not act on.
func TestMigration0123DefaultsEmailNotificationsOff(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "ao.db") + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	upTo(t, db, 123)

	var enabled, port int
	var recipient, host, username, password, tls string
	if err := db.QueryRow(`
		SELECT email_notifications_enabled, email_recipient, smtp_host, smtp_port,
		       smtp_username, smtp_password_encrypted, smtp_tls
		FROM app_settings WHERE id = 1`,
	).Scan(&enabled, &recipient, &host, &port, &username, &password, &tls); err != nil {
		t.Fatalf("read app settings: %v", err)
	}
	if enabled != 0 {
		t.Fatal("email notifications default to on; an upgrade must not start sending mail")
	}
	if port != 587 || tls != "starttls" {
		t.Fatalf("defaults = port %d tls %q, want 587/starttls", port, tls)
	}
	if recipient != "" || host != "" || username != "" || password != "" {
		t.Fatalf("defaults are not empty: %q %q %q %q", recipient, host, username, password)
	}

	if _, err := db.Exec(`UPDATE app_settings SET smtp_tls = 'ssl-maybe' WHERE id = 1`); err == nil {
		t.Fatal("an unsupported TLS mode was accepted")
	}
}
