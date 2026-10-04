package sqlite

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// Migration 0121 introduces the completion receipt behind the Completed status.
// Two things must hold for existing installs. Upgrading may not invent success:
// a session AO has no proof about keeps a NULL receipt and goes on reading
// Idle. And where AO already recorded proof — a session-owned execution lock
// released precisely because the task's turn ended — the historical task is
// classified from that record rather than from a guess.
func TestMigration0121BackfillsOnlyProvenCompletions(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "ao.db") + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	upTo(t, db, 120)

	if _, err := db.Exec(`INSERT INTO projects (id, path, registered_at) VALUES ('p1', '/tmp/p1', CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	sessions := []struct {
		id         string
		activity   string
		terminated int
	}{
		{"finished", "idle", 0},
		{"quiet", "idle", 0},
		{"torn-down", "idle", 0},
		{"still-working", "active", 0},
		{"killed", "idle", 1},
		{"workflow-worker", "idle", 0},
	}
	for n, s := range sessions {
		if _, err := db.Exec(`
			INSERT INTO sessions (id, project_id, num, kind, activity_state, activity_last_at, is_terminated, created_at, updated_at)
			VALUES (?, 'p1', ?, 'worker', ?, CURRENT_TIMESTAMP, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
			s.id, n+1, s.activity, s.terminated); err != nil {
			t.Fatalf("seed session %s: %v", s.id, err)
		}
	}

	// One released lock per session, differing only in what released it and
	// who owned it — which is exactly the distinction the backfill draws.
	locks := []struct {
		id, session, run, reason string
	}{
		{"lk-finished", "finished", "", "task turn ended (stop)"},
		{"lk-torn-down", "torn-down", "", "task turn ended (process-exited)"},
		{"lk-working", "still-working", "", "task turn ended (stop)"},
		{"lk-killed", "killed", "", "task turn ended (stop)"},
		{"lk-workflow", "workflow-worker", "wf-run-1", "task turn ended (stop)"},
	}
	for _, l := range locks {
		if _, err := db.Exec(`
			INSERT INTO branch_locks (id, lock_key, project_id, repo_path, repo_name, branch,
				workflow_run_id, session_id, owner_token, state, acquired_at, renewed_at,
				released_at, release_reason, created_at, updated_at)
			VALUES (?, ?, 'p1', '/tmp/p1', '__root__', 'main', ?, ?, 'tok', 'released',
				CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
			l.id, "key-"+l.id, l.run, l.session, l.reason); err != nil {
			t.Fatalf("seed lock %s: %v", l.id, err)
		}
	}

	upTo(t, db, 121)

	want := map[string]bool{
		// Proven: this task's own lock was released because its turn ended.
		"finished": true,
		// No record at all — the overwhelmingly common case on upgrade.
		"quiet": false,
		// The agent's process went away; that is not proof the work got done.
		"torn-down": false,
		// Working again since; its status comes from the work in flight.
		"still-working": false,
		// Killed or cancelled tasks are terminated, never completed.
		"killed": false,
		// Owned by a workflow run, not by the task session itself.
		"workflow-worker": false,
	}
	for id, wantSet := range want {
		var receipt sql.NullTime
		if err := db.QueryRow(`SELECT turn_completed_at FROM sessions WHERE id = ?`, id).Scan(&receipt); err != nil {
			t.Fatalf("read receipt for %s: %v", id, err)
		}
		if receipt.Valid != wantSet {
			t.Fatalf("session %q receipt set = %v, want %v", id, receipt.Valid, wantSet)
		}
	}
}

// The receipt drives a user-visible status and can change on its own — a Stop
// landing on an already-idle row moves no other column — so it has to trip the
// session invalidation stream by itself or the board keeps showing Inactive
// until something else happens to the session.
func TestMigration0121InvalidatesSubscribersOnCompletion(t *testing.T) {
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

	count := func() int {
		t.Helper()
		var n int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM change_log WHERE session_id = 's1' AND event_type = 'session_updated'`,
		).Scan(&n); err != nil {
			t.Fatalf("count change_log: %v", err)
		}
		return n
	}

	before := count()
	// Exactly the write a Stop on an already-idle row produces.
	if _, err := db.Exec(
		`UPDATE sessions SET turn_completed_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP WHERE id = 's1'`,
	); err != nil {
		t.Fatalf("stamp receipt: %v", err)
	}
	if got := count(); got != before+1 {
		t.Fatalf("session_updated events after stamping the receipt = %d, want %d", got, before+1)
	}

	if _, err := db.Exec(
		`UPDATE sessions SET turn_completed_at = NULL, updated_at = CURRENT_TIMESTAMP WHERE id = 's1'`,
	); err != nil {
		t.Fatalf("clear receipt: %v", err)
	}
	if got := count(); got != before+2 {
		t.Fatalf("session_updated events after clearing the receipt = %d, want %d", got, before+2)
	}
}
