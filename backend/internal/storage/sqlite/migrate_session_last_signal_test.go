package sqlite

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// Migration 0168 introduces the liveness clock. Two things must hold on upgrade.
//
// No existing session may read as "never heard from": that value is what the
// waiting-input corroboration window ages a session into, so a NULL left behind
// here would push every legacy row straight at the stop the column exists to
// prevent. And the seed may only ever be a LOWER bound on liveness --
// activity_last_at is a real moment this session was heard from, just the last
// one that also changed the state -- so a backfilled row can read as quieter
// than it was, never as more alive.
func TestMigration0168BackfillsLivenessFromTheTransitionClock(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "ao.db") + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	upTo(t, db, 167)

	if _, err := db.Exec(`INSERT INTO projects (id, path, registered_at) VALUES ('p1', '/tmp/p1', CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	// The wf-1c2cb9bd shape: an active worker whose transition clock froze the
	// moment it entered the state, plus an ordinary idle session.
	seeded := map[string]time.Time{
		"busy-worker": time.Date(2026, 9, 9, 17, 10, 20, 0, time.UTC),
		"quiet":       time.Date(2026, 9, 8, 9, 0, 0, 0, time.UTC),
	}
	states := map[string]string{"busy-worker": "active", "quiet": "idle"}
	n := 0
	for id, at := range seeded {
		n++
		if _, err := db.Exec(`
			INSERT INTO sessions (id, project_id, num, kind, activity_state, activity_last_at, is_terminated, created_at, updated_at)
			VALUES (?, 'p1', ?, 'worker', ?, ?, 0, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
			id, n, states[id], at); err != nil {
			t.Fatalf("seed session %s: %v", id, err)
		}
	}

	upTo(t, db, 168)

	for id, at := range seeded {
		var got sql.NullTime
		if err := db.QueryRow(`SELECT last_signal_at FROM sessions WHERE id = ?`, id).Scan(&got); err != nil {
			t.Fatalf("read liveness for %s: %v", id, err)
		}
		if !got.Valid {
			t.Fatalf("session %q was left with no liveness clock; every upgraded row must carry one", id)
		}
		if !got.Time.UTC().Equal(at) {
			t.Fatalf("session %q liveness = %v, want the transition clock %v", id, got.Time.UTC(), at)
		}
	}
}

// The column is deliberately absent from sessions_cdc_update: change_log is an
// append-only ledger with no retention policy, and a clock that moves twice a
// minute per active session does not belong in one. Clients that need liveness
// poll for it. If someone later adds it to the trigger, this test is where the
// trade-off gets re-argued.
func TestMigration0168LivenessDoesNotEmitChangeLogEvents(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "ao.db") + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	upTo(t, db, 168)

	if _, err := db.Exec(`INSERT INTO projects (id, path, registered_at) VALUES ('p1', '/tmp/p1', CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO sessions (id, project_id, num, kind, activity_state, activity_last_at, is_terminated, created_at, updated_at)
		VALUES ('s1', 'p1', 1, 'worker', 'active', CURRENT_TIMESTAMP, 0, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM change_log`); err != nil {
		t.Fatalf("clear change_log: %v", err)
	}

	for i := 1; i <= 5; i++ {
		if _, err := db.Exec(`UPDATE sessions SET last_signal_at = ?, updated_at = ? WHERE id = 's1'`,
			time.Now().UTC().Add(time.Duration(i)*time.Minute), time.Now().UTC()); err != nil {
			t.Fatalf("liveness write %d: %v", i, err)
		}
	}

	var events int
	if err := db.QueryRow(`SELECT COUNT(*) FROM change_log WHERE session_id = 's1'`).Scan(&events); err != nil {
		t.Fatalf("count change_log: %v", err)
	}
	if events != 0 {
		t.Fatalf("liveness-only writes emitted %d change_log rows, want 0", events)
	}

	// A real state change still does emit one: the trigger is intact.
	if _, err := db.Exec(`UPDATE sessions SET activity_state = 'idle', updated_at = ? WHERE id = 's1'`,
		time.Now().UTC()); err != nil {
		t.Fatalf("state change: %v", err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM change_log WHERE session_id = 's1'`).Scan(&events); err != nil {
		t.Fatalf("count change_log: %v", err)
	}
	if events != 1 {
		t.Fatalf("state change emitted %d change_log rows, want 1", events)
	}
}
