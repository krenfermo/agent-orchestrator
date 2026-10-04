package sqlite

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestMigration0104WorkflowQuestionsSchema(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "ao.db")+pragmas)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	upTo(t, db, 104)

	now := time.Date(2026, time.August, 16, 12, 0, 0, 0, time.UTC)
	if _, err := db.Exec(`
INSERT INTO projects (id, path, registered_at) VALUES ('q-proj', '/repos/q-proj', ?);
INSERT INTO workflow_runs (
    id, project_id, objective, state, policy_version, policy_snapshot, created_at, updated_at
) VALUES ('q-run', 'q-proj', 'obj', 'waiting', 'v1', '{}', ?, ?);
`, now, now, now); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	insert := `
INSERT INTO workflow_questions (
    id, workflow_run_id, fingerprint, question_text, certainty,
    classification, state, created_at
) VALUES (?, 'q-run', ?, ?, ?, ?, ?, ?);
`

	if _, err := db.Exec(insert, "q1", "fp-1", "Should I push to main?", "inferred", "policy_resolvable", "pending", now); err != nil {
		t.Fatalf("insert valid question: %v", err)
	}

	// CHECK constraint: certainty enum.
	if _, err := db.Exec(insert, "q2", "fp-2", "text", "bogus-certainty", "policy_resolvable", "pending", now); err == nil {
		t.Fatal("workflow_questions accepted an invalid certainty value")
	}

	// CHECK constraint: classification enum, including auto_resolvable
	// which must be accepted even though no 8K-A code path emits it yet.
	if _, err := db.Exec(insert, "q3", "fp-3", "text", "inferred", "auto_resolvable", "pending", now); err != nil {
		t.Fatalf("workflow_questions rejected auto_resolvable classification: %v", err)
	}
	if _, err := db.Exec(insert, "q4", "fp-4", "text", "inferred", "not-a-real-classification", "pending", now); err == nil {
		t.Fatal("workflow_questions accepted an invalid classification value")
	}

	// CHECK constraint: state enum.
	if _, err := db.Exec(insert, "q5", "fp-5", "text", "inferred", "human_required", "not-a-real-state", now); err == nil {
		t.Fatal("workflow_questions accepted an invalid state value")
	}

	// Unique fingerprint index: a true duplicate insert must not create a
	// second row (the store layer uses INSERT OR IGNORE against this same
	// index; here we exercise the constraint directly at the raw-SQL
	// level).
	if _, err := db.Exec(`
INSERT OR IGNORE INTO workflow_questions (
    id, workflow_run_id, fingerprint, question_text, certainty,
    classification, state, created_at
) VALUES ('q1-dup', 'q-run', 'fp-1', 'A different question text', 'inferred', 'policy_resolvable', 'pending', ?);
`, now); err != nil {
		t.Fatalf("insert or ignore duplicate fingerprint: %v", err)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM workflow_questions WHERE fingerprint = 'fp-1'`).Scan(&count); err != nil {
		t.Fatalf("count fp-1 rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("fingerprint fp-1 has %d rows, want 1 (unique index should have deduped)", count)
	}

	// A plain (non-IGNORE) duplicate insert must fail outright.
	if _, err := db.Exec(insert, "q1-dup-2", "fp-1", "text", "inferred", "policy_resolvable", "pending", now); err == nil {
		t.Fatal("workflow_questions accepted a duplicate fingerprint via plain INSERT")
	}

	// Index-backed dispatch-guard query shape: state IN (pending, human_required).
	var openCount int
	if err := db.QueryRow(`
SELECT count(*) FROM workflow_questions WHERE workflow_run_id = 'q-run' AND state IN ('pending', 'human_required');
`).Scan(&openCount); err != nil {
		t.Fatalf("count open questions: %v", err)
	}
	if openCount < 1 {
		t.Fatalf("expected at least 1 open question, got %d", openCount)
	}
}
