-- name: UpsertTaskWorktree :exec
INSERT INTO workflow_task_worktrees (task_id, workflow_run_id, project_id, repo_path,
    worktree_path, branch, target_branch, base_sha, dependencies_json, execution_mode,
    state, integrated_sha, branch_deleted, detail, created_at, updated_at, released_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(task_id) DO UPDATE SET
    workflow_run_id = excluded.workflow_run_id,
    project_id = excluded.project_id,
    repo_path = excluded.repo_path,
    worktree_path = excluded.worktree_path,
    branch = excluded.branch,
    target_branch = excluded.target_branch,
    base_sha = excluded.base_sha,
    dependencies_json = excluded.dependencies_json,
    execution_mode = excluded.execution_mode,
    state = excluded.state,
    integrated_sha = excluded.integrated_sha,
    branch_deleted = excluded.branch_deleted,
    detail = excluded.detail,
    updated_at = excluded.updated_at,
    released_at = excluded.released_at;

-- name: GetTaskWorktree :one
SELECT * FROM workflow_task_worktrees WHERE task_id = ?;

-- name: ListTaskWorktreesByRun :many
SELECT * FROM workflow_task_worktrees WHERE workflow_run_id = ? ORDER BY task_id;

-- name: ListUnfinishedTaskWorktrees :many
-- Every record the lifecycle manager is not yet done with, across every run.
-- Startup reconciliation reads this rather than iterating runs: a worktree
-- belonging to a run that has since gone terminal is exactly the orphan a
-- reconcile pass exists to find, and a run-scoped read would never see it.
--
-- A released row with branch_deleted = 0 is deliberately included: its
-- directory is gone but its ao/* branch is not, so cleanup has not finished.
SELECT * FROM workflow_task_worktrees
WHERE state IN ('creating', 'active', 'integrated')
   OR (state = 'released' AND branch_deleted = 0)
ORDER BY task_id;
