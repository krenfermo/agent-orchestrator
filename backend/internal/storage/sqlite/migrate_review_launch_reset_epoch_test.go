package sqlite

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// TestMigration0136SurvivesLegacyHumanRetryCheckpoints guards the same class of
// failure as TestMigration0013DedupesExistingDuplicates, for the partial UNIQUE
// index 0136 builds over (workflow_step_id, head_sha) WHERE durable_phase =
// 'reviewer_launch_human_retry'.
//
// That phase is NOT new. Every build before this one wrote it with no head_sha
// at all, so a user who has continued a stopped reviewer launch more than once
// on one step already holds two rows that collide under the new index. CREATE
// UNIQUE INDEX would fail on them and wedge startup — the daemon would not come
// up at all, on exactly the databases that have exercised this code path most.
//
// The migration must therefore make legacy rows distinct before it builds the
// index, without deleting any of them: they are the durable history the retry
// budget is read from.
func TestMigration0136SurvivesLegacyHumanRetryCheckpoints(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "ao.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// Stop one version short: the checkpoint table exists, the index does not.
	upTo(t, db, 135)

	if _, err := db.Exec(
		`INSERT INTO projects (id, path, registered_at)
		 VALUES ('p1', '/tmp/p', '2026-06-01T00:00:00Z')`,
	); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO workflow_runs (id, project_id, objective, state, created_at, updated_at)
		 VALUES ('wf1', 'p1', 'o', 'running', '2026-06-01T00:00:00Z', '2026-06-01T00:00:00Z')`,
	); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	for _, step := range []struct {
		id      string
		ordinal int
	}{{"step-1", 1}, {"step-2", 2}} {
		if _, err := db.Exec(
			`INSERT INTO workflow_steps (id, workflow_run_id, kind, ordinal, state, created_at, updated_at)
			 VALUES (?, 'wf1', 'review', ?, 'running', '2026-06-01T00:00:00Z', '2026-06-01T00:00:00Z')`,
			step.id, step.ordinal,
		); err != nil {
			t.Fatalf("seed %s: %v", step.id, err)
		}
	}

	// Two legacy resumes of ONE step, plus one on another step, plus an
	// unrelated phase that the partial index must not constrain at all.
	seed := []struct{ id, step, phase, headSHA string }{
		{"wfc-legacy-1", "step-1", "reviewer_launch_human_retry", ""},
		{"wfc-legacy-2", "step-1", "reviewer_launch_human_retry", ""},
		{"wfc-legacy-3", "step-2", "reviewer_launch_human_retry", ""},
		{"wfc-other-1", "step-1", "reviewer_launch_error", ""},
		{"wfc-other-2", "step-1", "reviewer_launch_error", ""},
	}
	for _, r := range seed {
		if _, err := db.Exec(
			`INSERT INTO workflow_checkpoints (id, workflow_run_id, workflow_step_id, project_id,
			 head_sha, retry_state, durable_phase, created_at)
			 VALUES (?, 'wf1', ?, 'p1', ?, '{"cycle":1}', ?, '2026-06-01T00:00:00Z')`,
			r.id, r.step, r.headSHA, r.phase,
		); err != nil {
			t.Fatalf("seed %s: %v", r.id, err)
		}
	}

	// The migration under test. Before the backfill this failed outright.
	upTo(t, db, 136)

	// Nothing was deleted: the retry budget is read from this history.
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM workflow_checkpoints`).Scan(&n); err != nil {
		t.Fatalf("count checkpoints: %v", err)
	}
	if n != len(seed) {
		t.Fatalf("checkpoint count = %d, want %d — the migration must not drop history", n, len(seed))
	}

	// The legacy rows were made distinct, not merged.
	var distinct int
	if err := db.QueryRow(
		`SELECT count(DISTINCT head_sha) FROM workflow_checkpoints
		 WHERE durable_phase = 'reviewer_launch_human_retry'`,
	).Scan(&distinct); err != nil {
		t.Fatalf("count distinct head_sha: %v", err)
	}
	if distinct != 3 {
		t.Fatalf("distinct head_sha among legacy resets = %d, want 3", distinct)
	}

	// A legacy head_sha must never be mistaken for a claimed generation.
	var genShaped int
	if err := db.QueryRow(
		`SELECT count(*) FROM workflow_checkpoints
		 WHERE durable_phase = 'reviewer_launch_human_retry'
		   AND head_sha LIKE 'review-launch-reset-gen-%'`,
	).Scan(&genShaped); err != nil {
		t.Fatalf("count epoch-shaped: %v", err)
	}
	if genShaped != 0 {
		t.Fatalf("%d legacy rows were backfilled into the generation namespace; they claim none", genShaped)
	}

	// Other phases are untouched and still unconstrained.
	if _, err := db.Exec(
		`INSERT INTO workflow_checkpoints (id, workflow_run_id, workflow_step_id, project_id,
		 head_sha, retry_state, durable_phase, created_at)
		 VALUES ('wfc-other-3', 'wf1', 'step-1', 'p1', '', '{}', 'reviewer_launch_error', '2026-06-02T00:00:00Z')`,
	); err != nil {
		t.Fatalf("the partial index must not constrain other phases: %v", err)
	}

	// And the index really is single-winner for a claimed failed generation.
	if _, err := db.Exec(
		`INSERT INTO workflow_checkpoints (id, workflow_run_id, workflow_step_id, project_id,
		 head_sha, retry_state, durable_phase, created_at)
		 VALUES ('wfc-epoch-1', 'wf1', 'step-1', 'p1', 'review-launch-reset-gen-k|wfc-err-1', '{}', 'reviewer_launch_human_retry', '2026-06-03T00:00:00Z')`,
	); err != nil {
		t.Fatalf("first claim of the generation: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO workflow_checkpoints (id, workflow_run_id, workflow_step_id, project_id,
		 head_sha, retry_state, durable_phase, created_at)
		 VALUES ('wfc-epoch-2', 'wf1', 'step-1', 'p1', 'review-launch-reset-gen-k|wfc-err-1', '{}', 'reviewer_launch_human_retry', '2026-06-04T00:00:00Z')`,
	); err == nil {
		t.Fatal("a second reset claimed the same failed generation; the index is not single-winner")
	}
}
