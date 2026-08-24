package sqlite

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// TestMigration0131PreservesWorktreeRecordsAndAddsCleanupState checks the one
// thing a table rebuild can silently get wrong: the rows.
//
// 0131 widens workflow_task_worktrees.state, which SQLite can only do by
// rebuilding, and the records at stake are the durable identity of every AO
// worktree currently on disk -- which task owns a directory, which ao/* branch
// holds its commits, what it was cut from. Losing them would not lose the
// worktrees; it would lose the only thing that makes them attributable, so
// nothing could ever clean them up or explain them again.
//
// Unlike 0119/0130 this table is a leaf -- nothing references it -- so no
// backup tables are needed. Foreign keys are enabled anyway so the test
// reproduces production enforcement rather than passing under a relaxed one.
func TestMigration0131PreservesWorktreeRecordsAndAddsCleanupState(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "ao.db") + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	upTo(t, db, 130)

	if _, err := db.Exec(`INSERT INTO projects (id, path, registered_at) VALUES ('p', '/tmp/p', CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO workflow_runs (id, project_id, objective, state, policy_version, policy_snapshot, created_at, updated_at)
		VALUES ('wf-master', 'p', 'objective', 'running', 'v1', '{}', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	for _, tk := range []struct {
		id      string
		ordinal int
	}{{"wft-1", 1}, {"wft-2", 2}, {"wft-3", 3}} {
		if _, err := db.Exec(`
			INSERT INTO workflow_tasks (id, workflow_run_id, plan_step_id, ordinal, title, description,
				acceptance_criteria_json, verify_json, scope_json, state, created_at, updated_at)
			VALUES (?, 'wf-master', ?, ?, 'title', 'description', '[]', '{}', '{"version":"v1"}', 'running', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
			tk.id, "step-"+tk.id, tk.ordinal); err != nil {
			t.Fatalf("seed task %s: %v", tk.id, err)
		}
	}
	// One of each pre-0131 state, with the dependency pinning that is the
	// hardest thing to re-derive if it is lost.
	for _, wt := range []struct{ task, state, deps string }{
		{"wft-1", "active", `[{"taskId":"wft-2","sha":"dep111"}]`},
		{"wft-2", "released", `[]`},
		{"wft-3", "failed", `[]`},
	} {
		if _, err := db.Exec(`
			INSERT INTO workflow_task_worktrees (task_id, workflow_run_id, project_id, repo_path, worktree_path,
				branch, target_branch, base_sha, dependencies_json, execution_mode, state, detail, created_at, updated_at)
			VALUES (?, 'wf-master', 'p', '/tmp/p', '/tmp/wt-' || ?, 'ao/' || ?, 'main', 'base' || ?, ?,
				'isolated_worktree', ?, 'seeded', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
			wt.task, wt.task, wt.task, wt.task, wt.deps, wt.state); err != nil {
			t.Fatalf("seed worktree %s: %v", wt.task, err)
		}
	}

	upTo(t, db, 131)

	var count int
	if err := db.QueryRow(`SELECT count(*) FROM workflow_task_worktrees`).Scan(&count); err != nil {
		t.Fatalf("count worktrees: %v", err)
	}
	if count != 3 {
		t.Fatalf("worktree records after the rebuild = %d, want 3", count)
	}

	var state, branch, base, deps, detail, integrated string
	var branchDeleted int
	if err := db.QueryRow(`
		SELECT state, branch, base_sha, dependencies_json, detail, integrated_sha, branch_deleted
		FROM workflow_task_worktrees WHERE task_id = 'wft-1'`).
		Scan(&state, &branch, &base, &deps, &detail, &integrated, &branchDeleted); err != nil {
		t.Fatalf("read wft-1: %v", err)
	}
	if state != "active" || branch != "ao/wft-1" || base != "basewft-1" || detail != "seeded" {
		t.Fatalf("wft-1 = state %q branch %q base %q detail %q, want its pre-migration values", state, branch, base, detail)
	}
	if deps != `[{"taskId":"wft-2","sha":"dep111"}]` {
		t.Fatalf("wft-1 dependencies = %q, want the pinned commit to survive", deps)
	}
	// The new columns default to the truth about every pre-0131 row: it never
	// recorded an integrated commit, and nothing ever deleted its branch.
	if integrated != "" || branchDeleted != 0 {
		t.Fatalf("wft-1 new columns = %q/%d, want empty and 0", integrated, branchDeleted)
	}

	// The two new states are accepted, and an invented one is still refused --
	// the widened CHECK has to be widened, not removed.
	if _, err := db.Exec(`UPDATE workflow_task_worktrees SET state = 'integrated', integrated_sha = 'landed1', branch_deleted = 0 WHERE task_id = 'wft-1'`); err != nil {
		t.Fatalf("set integrated: %v", err)
	}
	if _, err := db.Exec(`UPDATE workflow_task_worktrees SET state = 'preserved' WHERE task_id = 'wft-3'`); err != nil {
		t.Fatalf("set preserved: %v", err)
	}
	if _, err := db.Exec(`UPDATE workflow_task_worktrees SET state = 'tidied' WHERE task_id = 'wft-2'`); err == nil {
		t.Fatal("the state CHECK accepted a value this build does not understand")
	}

	// The foreign key still cascades, which is what a rebuild most easily
	// breaks: a deleted run must still take its worktree records with it.
	if _, err := db.Exec(`DELETE FROM workflow_runs WHERE id = 'wf-master'`); err != nil {
		t.Fatalf("delete run: %v", err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM workflow_task_worktrees`).Scan(&count); err != nil {
		t.Fatalf("count after cascade: %v", err)
	}
	if count != 0 {
		t.Fatalf("%d worktree records survived their run's deletion", count)
	}
}
