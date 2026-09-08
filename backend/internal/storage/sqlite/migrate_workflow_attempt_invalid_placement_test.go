package sqlite

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// TestMigration0160PreservesAttemptsAndAcceptsInvalidPlacement guards the one
// thing a CHECK-widening rebuild can quietly get wrong.
//
// 0102 shaped workflow_attempts; 0133 then APPENDED four columns to it with
// ALTER TABLE. A rebuild written against the 0102 shape and copied with
// SELECT * would mis-map those four -- silently, since every one of them is
// nullable or defaulted. So a realistic pre-0160 row carrying all fourteen
// columns is seeded, and every one is read back after the migration.
//
// The pragma is enabled explicitly so this reproduces production foreign-key
// enforcement rather than accidentally passing under a relaxed one.
func TestMigration0160PreservesAttemptsAndAcceptsInvalidPlacement(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "ao.db") + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	upTo(t, db, 159)

	if _, err := db.Exec(`INSERT INTO projects (id, path, registered_at) VALUES ('p', '/tmp/p', CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO workflow_runs (id, project_id, objective, state, policy_version, policy_snapshot, created_at, updated_at)
		VALUES ('wf-1', 'p', 'objective', 'running', 'v1', '{}', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO workflow_steps (id, workflow_run_id, kind, state, ordinal, created_at, updated_at)
		VALUES ('st-1', 'wf-1', 'work', 'running', 1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("seed step: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO workflow_attempts (
			id, workflow_step_id, attempt_number, harness, model, started_at, finished_at,
			outcome, error_class, retry_after, deadline_at, review_target_fingerprint, review_target_head_sha
		) VALUES (
			'at-1', 'st-1', 1, 'claude-code', 'opus', '2026-01-01 00:00:00', '2026-01-01 00:05:00',
			'failed', 'runtime_failed', '2026-01-01 00:06:00', '2026-01-01 00:30:00', 'fp-abc', 'sha-def'
		)`); err != nil {
		t.Fatalf("seed attempt: %v", err)
	}

	upTo(t, db, 160)

	var (
		harness, model, outcome, errClass string
		fingerprint, headSHA              string
		finished, retryAfter, deadline    sql.NullString
		number                            int
	)
	if err := db.QueryRow(`
		SELECT attempt_number, harness, model, finished_at, outcome, error_class,
		       retry_after, deadline_at, review_target_fingerprint, review_target_head_sha
		FROM workflow_attempts WHERE id = 'at-1'`).
		Scan(&number, &harness, &model, &finished, &outcome, &errClass,
			&retryAfter, &deadline, &fingerprint, &headSHA); err != nil {
		t.Fatalf("read attempt after migration: %v", err)
	}
	for _, c := range []struct{ name, got, want string }{
		{"harness", harness, "claude-code"},
		{"model", model, "opus"},
		{"outcome", outcome, "failed"},
		{"error_class", errClass, "runtime_failed"},
		{"review_target_fingerprint", fingerprint, "fp-abc"},
		{"review_target_head_sha", headSHA, "sha-def"},
	} {
		if c.got != c.want {
			t.Fatalf("%s = %q after migration, want %q (columns were mis-mapped by the rebuild)", c.name, c.got, c.want)
		}
	}
	if number != 1 {
		t.Fatalf("attempt_number = %d, want 1", number)
	}
	if !finished.Valid || !retryAfter.Valid || !deadline.Valid {
		t.Fatalf("nullable timestamps lost: finished=%v retry=%v deadline=%v", finished, retryAfter, deadline)
	}

	// The point of the migration: the new class is now writable.
	if _, err := db.Exec(`
		INSERT INTO workflow_attempts (id, workflow_step_id, attempt_number, harness, started_at, outcome, error_class)
		VALUES ('at-2', 'st-1', 2, 'claude-code', CURRENT_TIMESTAMP, 'failed', 'invalid_placement')`); err != nil {
		t.Fatalf("insert invalid_placement attempt: %v", err)
	}
	// And the CHECK still refuses a class nobody defined, so widening it did not
	// turn the column into free text.
	if _, err := db.Exec(`
		INSERT INTO workflow_attempts (id, workflow_step_id, attempt_number, harness, started_at, outcome, error_class)
		VALUES ('at-3', 'st-1', 3, 'claude-code', CURRENT_TIMESTAMP, 'failed', 'not_a_real_class')`); err == nil {
		t.Fatal("an unknown error_class was accepted; the CHECK constraint was lost in the rebuild")
	}
}

// TestMigration0160PreservesIncomingForeignKeyReferences is the regression for
// the P0 this migration caused: AO would not boot.
//
// The first version of 0160 rebuilt workflow_attempts with the recipe
// 0096-0102 used, which drops the old table. That was safe when it was written
// and stopped being safe at 0133, which gave workflow_attempts three incoming
// foreign keys -- workflow_checkpoints, workflow_dispatch_checkpoints and
// workflow_mutation_provenance all carry attempt_id, all ON DELETE NO ACTION.
// SQLite treats DROP TABLE as deleting every row for foreign-key purposes, so
// the drop raised SQLITE_CONSTRAINT_FOREIGNKEY (787) against the 249 child
// rows a real ~/.ao/data/ao.db held, and the daemon exited 1 during migrations.
//
// The original test missed it for one reason, and this test exists to remove
// that reason: it seeded an attempt with NOTHING REFERENCING IT. So the
// fixture below is the whole point -- every one of the three children gets a
// row pointing at the attempt before the migration runs, and each is checked
// afterwards for the reference it had.
//
// Foreign keys are enabled explicitly so this reproduces production
// enforcement rather than passing under a relaxed one. Without the fix this
// test fails inside upTo with "FOREIGN KEY constraint failed".
func TestMigration0160PreservesIncomingForeignKeyReferences(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "ao.db") + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	upTo(t, db, 159)

	if _, err := db.Exec(`INSERT INTO projects (id, path, registered_at) VALUES ('p', '/tmp/p', CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO workflow_runs (id, project_id, objective, state, policy_version, policy_snapshot, created_at, updated_at)
		VALUES ('wf-1', 'p', 'objective', 'running', 'v1', '{}', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO workflow_steps (id, workflow_run_id, kind, state, ordinal, created_at, updated_at)
		VALUES ('st-1', 'wf-1', 'work', 'running', 1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("seed step: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO workflow_attempts (id, workflow_step_id, attempt_number, harness, started_at, outcome, error_class)
		VALUES ('at-1', 'st-1', 1, 'claude-code', CURRENT_TIMESTAMP, 'failed', 'runtime_failed')`); err != nil {
		t.Fatalf("seed attempt: %v", err)
	}

	// THE FIXTURE THAT MATTERS: one child row per referencing table.
	if _, err := db.Exec(`
		INSERT INTO workflow_checkpoints (id, workflow_run_id, workflow_step_id, attempt_id, project_id, next_action, durable_phase, payload_version, retry_state, created_at)
		VALUES ('cp-1', 'wf-1', 'st-1', 'at-1', 'p', 'next', 'phase', 'v1', '{}', CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("seed checkpoint: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO workflow_dispatch_checkpoints (id, workflow_run_id, workflow_step_id, attempt_id, created_at)
		VALUES ('dc-1', 'wf-1', 'st-1', 'at-1', CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("seed dispatch checkpoint: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO workflow_mutation_provenance (id, workflow_run_id, workflow_step_id, attempt_id, created_at)
		VALUES ('mp-1', 'wf-1', 'st-1', 'at-1', CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("seed mutation provenance: %v", err)
	}

	// This is the statement that used to fail with FOREIGN KEY constraint (787).
	upTo(t, db, 160)

	// Every reference is back, pointing at the same attempt.
	for _, c := range []struct{ table, id string }{
		{"workflow_checkpoints", "cp-1"},
		{"workflow_dispatch_checkpoints", "dc-1"},
		{"workflow_mutation_provenance", "mp-1"},
	} {
		var got sql.NullString
		if err := db.QueryRow("SELECT attempt_id FROM "+c.table+" WHERE id = ?", c.id).Scan(&got); err != nil {
			t.Fatalf("read %s.%s: %v", c.table, c.id, err)
		}
		if !got.Valid || got.String != "at-1" {
			t.Fatalf("%s.attempt_id = %v, want at-1: the rebuild dropped an incoming reference", c.table, got)
		}
	}
	// The attempt itself survived with its class.
	var errClass string
	if err := db.QueryRow(`SELECT error_class FROM workflow_attempts WHERE id = 'at-1'`).Scan(&errClass); err != nil {
		t.Fatalf("read attempt: %v", err)
	}
	if errClass != "runtime_failed" {
		t.Fatalf("error_class = %q, want runtime_failed", errClass)
	}
	// And the database is referentially intact, which is the property the
	// failed migration was correctly refusing to break.
	rows, err := db.Query("PRAGMA foreign_key_check")
	if err != nil {
		t.Fatalf("foreign_key_check: %v", err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("foreign_key_check reported a violation after the migration")
	}
	// No scaffolding left behind.
	var leftover int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name IN ('workflow_attempts_fk_bak','workflow_attempts_new','workflow_attempts_old')`).Scan(&leftover); err != nil {
		t.Fatalf("leftover check: %v", err)
	}
	if leftover != 0 {
		t.Fatalf("%d rebuild scaffolding objects left behind", leftover)
	}
}
