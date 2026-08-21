package sqlite

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// TestMigration0119PreservesTaskDependencies guards a data-loss regression that
// the first draft of 0119 actually caused, caught by running it against a copy
// of a real ~/.ao/data/ao.db: workflow_task_dependencies went from 8 rows to 0.
//
// workflow_tasks is referenced twice by workflow_task_dependencies, both
// ON DELETE CASCADE. Foreign keys ARE enforced while these migrations run, so
// the naive "rebuild the table" recipe that 0114/0118 use safely (their table is
// referenced by nothing) silently deleted every master run's dependency graph
// here. The application would then read every task as dependency-free, mark
// every blocked task eligible, and dispatch a whole plan out of order.
//
// This test seeds the exact shape that broke — tasks with a real dependency
// edge — and asserts both survive the rebuild with the widened CHECK in place.
// The foreign_keys pragma is enabled explicitly so the test reproduces
// production enforcement rather than accidentally passing under a relaxed one.
func TestMigration0119PreservesTaskDependencies(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "ao.db") + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	// Stop one version short of 0119, seed, then apply 0119 alone.
	upTo(t, db, 118)

	if _, err := db.Exec(`INSERT INTO projects (id, path, registered_at) VALUES ('p', '/tmp/p', CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO workflow_runs (id, project_id, objective, state, policy_version, policy_snapshot, created_at, updated_at)
		VALUES ('wf-master', 'p', 'objective', 'running', 'v1', '{}', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	for _, tk := range []struct {
		id, step, state string
		ordinal         int
	}{
		{"wft-1", "model", "completed", 1},
		{"wft-2", "tests", "blocked", 2},
	} {
		if _, err := db.Exec(`
			INSERT INTO workflow_tasks (id, workflow_run_id, plan_step_id, ordinal, title, description,
				acceptance_criteria_json, verify_json, state, created_at, updated_at)
			VALUES (?, 'wf-master', ?, ?, 'title', 'description', '[]', '{}', ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
			tk.id, tk.step, tk.ordinal, tk.state); err != nil {
			t.Fatalf("seed task %s: %v", tk.id, err)
		}
	}
	if _, err := db.Exec(`INSERT INTO workflow_task_dependencies (workflow_task_id, depends_on_task_id) VALUES ('wft-2', 'wft-1')`); err != nil {
		t.Fatalf("seed dependency: %v", err)
	}

	upTo(t, db, 119)

	var deps int
	if err := db.QueryRow(`SELECT count(*) FROM workflow_task_dependencies`).Scan(&deps); err != nil {
		t.Fatalf("count dependencies: %v", err)
	}
	if deps != 1 {
		t.Fatalf("workflow_task_dependencies has %d rows after 0119, want 1 — the dependency graph was destroyed", deps)
	}
	var dependent, dependsOn string
	if err := db.QueryRow(`SELECT workflow_task_id, depends_on_task_id FROM workflow_task_dependencies`).Scan(&dependent, &dependsOn); err != nil {
		t.Fatalf("read dependency: %v", err)
	}
	if dependent != "wft-2" || dependsOn != "wft-1" {
		t.Fatalf("dependency edge = %s>%s, want wft-2>wft-1", dependent, dependsOn)
	}

	var tasks int
	if err := db.QueryRow(`SELECT count(*) FROM workflow_tasks`).Scan(&tasks); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	if tasks != 2 {
		t.Fatalf("workflow_tasks has %d rows after 0119, want 2", tasks)
	}

	// The whole point of the migration: 'failed' is now persistable.
	if _, err := db.Exec(`UPDATE workflow_tasks SET state = 'failed' WHERE id = 'wft-2'`); err != nil {
		t.Fatalf("the widened CHECK does not accept 'failed': %v", err)
	}

	// And the restored table still enforces its own constraints, so this is a
	// faithful rebuild rather than a bare copy.
	if _, err := db.Exec(`INSERT INTO workflow_task_dependencies (workflow_task_id, depends_on_task_id) VALUES ('wft-1','wft-1')`); err == nil {
		t.Fatal("self-dependency was accepted; the CHECK constraint was lost in the rebuild")
	}
	if _, err := db.Exec(`INSERT INTO workflow_task_dependencies (workflow_task_id, depends_on_task_id) VALUES ('wft-2','nope')`); err == nil {
		t.Fatal("dangling dependency was accepted; the foreign key was lost in the rebuild")
	}
}
