package sqlite

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestMigration0105WorkflowQuestionResolutionsSchema(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "ao.db")+pragmas)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	upTo(t, db, 105)

	now := time.Date(2026, time.August, 16, 12, 0, 0, 0, time.UTC)
	if _, err := db.Exec(`
INSERT INTO projects (id, path, registered_at) VALUES ('r-proj', '/repos/r-proj', ?);
INSERT INTO workflow_runs (
    id, project_id, objective, state, policy_version, policy_snapshot, created_at, updated_at
) VALUES ('r-run', 'r-proj', 'obj', 'waiting', 'v1', '{}', ?, ?);
INSERT INTO workflow_questions (
    id, workflow_run_id, fingerprint, question_text, certainty,
    classification, state, created_at
) VALUES ('rq-1', 'r-run', 'fp-r-1', 'Which helper already exists for X?', 'inferred', 'auto_resolvable', 'resolving', ?);
`, now, now, now, now); err != nil {
		t.Fatalf("seed run/question: %v", err)
	}

	insert := `
INSERT INTO workflow_question_resolutions (
    id, workflow_question_id, workflow_run_id, resolver_harness, status, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?);
`

	if _, err := db.Exec(insert, "res-1", "rq-1", "r-run", "codex", "running", now, now); err != nil {
		t.Fatalf("insert valid resolution: %v", err)
	}

	// CHECK constraint: status enum.
	if _, err := db.Exec(insert, "res-bad-status", "rq-1", "r-run", "codex", "not-a-real-status", now, now); err == nil {
		t.Fatal("workflow_question_resolutions accepted an invalid status value")
	}

	// CHECK constraint: certainty enum (nullable, but must reject bogus values).
	if _, err := db.Exec(`
INSERT INTO workflow_question_resolutions (
    id, workflow_question_id, workflow_run_id, resolver_harness, status, certainty, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?);
`, "res-bad-certainty", "rq-1", "r-run", "codex", "failed", "bogus-certainty", now, now); err == nil {
		t.Fatal("workflow_question_resolutions accepted an invalid certainty value")
	}

	// Partial unique index: a second concurrent 'running' row for the same
	// question must be rejected, guaranteeing at most one in-flight
	// resolution attempt per question.
	_, err = db.Exec(insert, "res-2", "rq-1", "r-run", "claude-code", "running", now, now)
	if err == nil {
		t.Fatal("workflow_question_resolutions accepted a second concurrent running row for the same question (partial unique index should reject it)")
	}

	// A non-running status for the same question must NOT be blocked by the
	// partial index (it only applies WHERE status = 'running').
	if _, err := db.Exec(insert, "res-3", "rq-1", "r-run", "claude-code", "pending", now, now); err != nil {
		t.Fatalf("workflow_question_resolutions rejected a second pending row for the same question: %v", err)
	}

	// Once the first attempt transitions off 'running', a fresh running row
	// for the same question becomes insertable again.
	if _, err := db.Exec(`UPDATE workflow_question_resolutions SET status = 'failed' WHERE id = 'res-1'`); err != nil {
		t.Fatalf("transition res-1 to failed: %v", err)
	}
	if _, err := db.Exec(insert, "res-4", "rq-1", "r-run", "claude-code", "running", now, now); err != nil {
		t.Fatalf("workflow_question_resolutions rejected a running row after the prior one left running: %v", err)
	}

	// Index-backed lookup query shape: (workflow_question_id, status).
	var runningCount int
	if err := db.QueryRow(`
SELECT count(*) FROM workflow_question_resolutions WHERE workflow_question_id = 'rq-1' AND status = 'running';
`).Scan(&runningCount); err != nil {
		t.Fatalf("count running resolutions: %v", err)
	}
	if runningCount != 1 {
		t.Fatalf("running resolutions for rq-1 = %d, want 1", runningCount)
	}

	// workflow_questions.resolving_run_id: new nullable pointer column,
	// added without touching the existing CHECK constraints.
	if _, err := db.Exec(`UPDATE workflow_questions SET resolving_run_id = 'res-4' WHERE id = 'rq-1'`); err != nil {
		t.Fatalf("set resolving_run_id: %v", err)
	}
	var resolvingRunID sql.NullString
	if err := db.QueryRow(`SELECT resolving_run_id FROM workflow_questions WHERE id = 'rq-1'`).Scan(&resolvingRunID); err != nil {
		t.Fatalf("read resolving_run_id: %v", err)
	}
	if !resolvingRunID.Valid || resolvingRunID.String != "res-4" {
		t.Fatalf("resolving_run_id = %+v, want res-4", resolvingRunID)
	}

	// The existing classification/state CHECK constraints on
	// workflow_questions must still reject invalid values after this
	// migration's ADD COLUMN (i.e. the ALTER TABLE did not silently rebuild
	// the table and drop the constraints).
	if _, err := db.Exec(`
INSERT INTO workflow_questions (
    id, workflow_run_id, fingerprint, question_text, certainty,
    classification, state, created_at
) VALUES ('rq-bad', 'r-run', 'fp-r-bad', 'text', 'inferred', 'not-a-real-classification', 'pending', ?);
`, now); err == nil {
		t.Fatal("workflow_questions accepted an invalid classification value after 0105's ALTER TABLE")
	}
}
