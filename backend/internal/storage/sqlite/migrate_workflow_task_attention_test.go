package sqlite

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// TestMigration0130PreservesTaskGraphAndAddsAttention is 0119's lesson applied
// to a table that has since grown two more children.
//
// 0130 widens workflow_tasks.state, which SQLite can only do by rebuilding the
// table — and workflow_tasks is now referenced ON DELETE CASCADE by THREE
// tables: workflow_task_dependencies (0101), workflow_task_relationships (0127)
// and workflow_task_worktrees (0128). Foreign keys are enforced while goose
// runs, and PRAGMA foreign_keys is silently ignored inside its transaction, so
// a naive rebuild would drop the dependency graph, every pair classification
// and every worktree registration of every master run on disk.
//
// The pragma is enabled explicitly so this reproduces production enforcement
// rather than accidentally passing under a relaxed one.
func TestMigration0130PreservesTaskGraphAndAddsAttention(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "ao.db") + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	// Stop one version short, seed a realistic pre-0130 database, then apply
	// 0130 alone.
	upTo(t, db, 129)

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
		{"wft-2", "tests", "running", 2},
		{"wft-3", "docs", "blocked", 3},
	} {
		if _, err := db.Exec(`
			INSERT INTO workflow_tasks (id, workflow_run_id, plan_step_id, ordinal, title, description,
				acceptance_criteria_json, verify_json, scope_json, state, created_at, updated_at)
			VALUES (?, 'wf-master', ?, ?, 'title', 'description', '[]', '{}', '{"version":"v1"}', ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
			tk.id, tk.step, tk.ordinal, tk.state); err != nil {
			t.Fatalf("seed task %s: %v", tk.id, err)
		}
	}
	if _, err := db.Exec(`INSERT INTO workflow_task_dependencies (workflow_task_id, depends_on_task_id) VALUES ('wft-3', 'wft-1')`); err != nil {
		t.Fatalf("seed dependency: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO workflow_task_relationships (workflow_run_id, task_id, related_task_id, relation, reason, detail, overlap_json, created_at)
		VALUES ('wf-master', 'wft-1', 'wft-2', 'probable_write_conflict', 'shared_write_set', 'both write internal/api', '["internal/api"]', CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("seed relationship: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO workflow_task_worktrees (task_id, workflow_run_id, project_id, repo_path, worktree_path,
			branch, target_branch, base_sha, dependencies_json, execution_mode, state, created_at, updated_at)
		VALUES ('wft-2', 'wf-master', 'p', '/tmp/p', '/tmp/wt-2', 'ao/wft-2', 'main', 'abc123', '[]',
			'isolated_worktree', 'active', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("seed worktree: %v", err)
	}

	upTo(t, db, 130)

	// Existing tasks survive with their states and their scope intact. This is
	// the "migración desde una BD anterior conserva tasks existentes" guarantee.
	var tasks int
	if err := db.QueryRow(`SELECT count(*) FROM workflow_tasks`).Scan(&tasks); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	if tasks != 3 {
		t.Fatalf("workflow_tasks has %d rows after 0130, want 3", tasks)
	}
	var state, scope string
	if err := db.QueryRow(`SELECT state, scope_json FROM workflow_tasks WHERE id = 'wft-2'`).Scan(&state, &scope); err != nil {
		t.Fatalf("read task: %v", err)
	}
	if state != "running" || scope != `{"version":"v1"}` {
		t.Fatalf("wft-2 = (%s, %s) after 0130, want (running, {\"version\":\"v1\"})", state, scope)
	}

	// And so does everything that references them.
	for _, c := range []struct {
		table, query string
	}{
		{"workflow_task_dependencies", `SELECT count(*) FROM workflow_task_dependencies WHERE workflow_task_id = 'wft-3' AND depends_on_task_id = 'wft-1'`},
		{"workflow_task_relationships", `SELECT count(*) FROM workflow_task_relationships WHERE task_id = 'wft-1' AND related_task_id = 'wft-2' AND overlap_json = '["internal/api"]'`},
		{"workflow_task_worktrees", `SELECT count(*) FROM workflow_task_worktrees WHERE task_id = 'wft-2' AND worktree_path = '/tmp/wt-2'`},
	} {
		var n int
		if err := db.QueryRow(c.query).Scan(&n); err != nil {
			t.Fatalf("read %s: %v", c.table, err)
		}
		if n != 1 {
			t.Fatalf("%s lost its row in the 0130 rebuild (found %d)", c.table, n)
		}
	}

	// The point of the migration: a task can be parked, durably, with detail.
	if _, err := db.Exec(`
		UPDATE workflow_tasks SET state = 'needs_attention',
			attention_reason = 'integration_merge_conflict',
			attention_json = '{"reason":"integration_merge_conflict","conflictingFiles":["a.go"]}',
			attention_at = CURRENT_TIMESTAMP
		WHERE id = 'wft-2'`); err != nil {
		t.Fatalf("the widened CHECK does not accept 'needs_attention': %v", err)
	}
	var reason, body string
	if err := db.QueryRow(`SELECT attention_reason, attention_json FROM workflow_tasks WHERE id = 'wft-2'`).Scan(&reason, &body); err != nil {
		t.Fatalf("read attention: %v", err)
	}
	if reason != "integration_merge_conflict" || body == "" {
		t.Fatalf("attention = (%s, %s), want the parked detail persisted", reason, body)
	}

	// An unexplained stop is unrepresentable, not merely discouraged.
	if _, err := db.Exec(`UPDATE workflow_tasks SET state = 'needs_attention', attention_reason = '' WHERE id = 'wft-3'`); err == nil {
		t.Fatal("a task was parked with no reason; the CHECK is missing")
	}

	// The rebuild is faithful, not a bare copy: every constraint the children
	// carried is still enforced.
	if _, err := db.Exec(`INSERT INTO workflow_task_dependencies (workflow_task_id, depends_on_task_id) VALUES ('wft-1','wft-1')`); err == nil {
		t.Fatal("self-dependency was accepted; the CHECK constraint was lost in the rebuild")
	}
	if _, err := db.Exec(`INSERT INTO workflow_task_dependencies (workflow_task_id, depends_on_task_id) VALUES ('wft-2','nope')`); err == nil {
		t.Fatal("dangling dependency was accepted; the foreign key was lost in the rebuild")
	}
	if _, err := db.Exec(`
		INSERT INTO workflow_task_relationships (workflow_run_id, task_id, related_task_id, relation, created_at)
		VALUES ('wf-master', 'wft-3', 'wft-1', 'independent', CURRENT_TIMESTAMP)`); err == nil {
		t.Fatal("an uncanonicalized pair was accepted; the task_id < related_task_id CHECK was lost")
	}
	if _, err := db.Exec(`
		INSERT INTO workflow_task_worktrees (task_id, workflow_run_id, project_id, repo_path, worktree_path,
			branch, target_branch, base_sha, execution_mode, state, created_at, updated_at)
		VALUES ('wft-3', 'wf-master', 'p', '/tmp/p', '/tmp/wt-3', 'ao/wft-3', 'main', 'def', 'direct_branch', 'active', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`); err == nil {
		t.Fatal("direct_branch was accepted as an execution_mode; the CHECK was lost in the rebuild")
	}
}
