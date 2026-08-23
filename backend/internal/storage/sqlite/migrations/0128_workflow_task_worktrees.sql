-- Durable identity for the AO-owned worktrees a plan's tasks run in.
--
-- Before this, the only persisted worktree was session_worktrees: keyed by
-- session, carrying a path, a branch and a base SHA, and nothing about the
-- plan the work belongs to. That is enough for one session cleaning up after
-- itself and not enough for anything a multi-task plan needs to ask, because
-- three questions have no answer in it:
--
--   * "which task is this directory for" -- an orphan worktree under the data
--     dir is unattributable, so cleanup either guesses or leaves it forever;
--   * "where is this work meant to land" -- the throwaway ao/* branch is not
--     the target branch, and re-deriving the target from project config reads
--     whatever the config says NOW, not what it said when the task started;
--   * "what was this built on" -- the base commit and the commit each
--     dependency sat at. Both move. Once they have moved, an integration
--     conflict cannot be explained, only observed.
--
-- One row per task, so a task has at most one AO worktree at a time and a
-- retry reuses the row instead of accumulating rows nobody can tell apart.
-- direct_branch tasks never get a row: their work happens in the user's own
-- checkout, there is no AO-owned tree to track, and a row would assert
-- otherwise.
--
-- ON DELETE CASCADE on both columns mirrors workflow_task_relationships: a
-- deleted run takes its tasks and their worktree records with it. The
-- directory on disk is NOT the DB's business -- the lifecycle manager removes
-- it before the row is released.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE workflow_task_worktrees (
    task_id          TEXT PRIMARY KEY REFERENCES workflow_tasks(id) ON DELETE CASCADE,
    workflow_run_id  TEXT NOT NULL REFERENCES workflow_runs(id) ON DELETE CASCADE,
    project_id       TEXT NOT NULL,
    -- The user's own repository the worktree was cut from, so teardown always
    -- runs against the repo that holds the registration.
    repo_path        TEXT NOT NULL,
    worktree_path    TEXT NOT NULL,
    -- The AO-owned throwaway branch (ao/*) the agent commits to.
    branch           TEXT NOT NULL,
    -- Where the work is ultimately meant to land. Never equal to branch.
    target_branch    TEXT NOT NULL,
    base_sha         TEXT NOT NULL,
    -- [{"taskId":...,"sha":...}] for every dependency task, sorted by taskId.
    dependencies_json TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(dependencies_json)),
    -- isolated_worktree or smart_parallel_worktrees; direct_branch never
    -- produces a row.
    execution_mode   TEXT NOT NULL CHECK (execution_mode IN (
        'isolated_worktree',
        'smart_parallel_worktrees'
    )),
    state            TEXT NOT NULL CHECK (state IN (
        'creating',
        'active',
        'released',
        'failed'
    )),
    detail           TEXT NOT NULL DEFAULT '',
    created_at       TIMESTAMP NOT NULL,
    updated_at       TIMESTAMP NOT NULL,
    released_at      TIMESTAMP
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_workflow_task_worktrees_run
    ON workflow_task_worktrees(workflow_run_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_workflow_task_worktrees_run;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS workflow_task_worktrees;
-- +goose StatementEnd
