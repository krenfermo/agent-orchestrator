-- Restart/crash recovery for the AO-owned task worktree: the two states and
-- the two facts a reconciliation pass needs, and could not previously read.
--
-- 0128 gave the worktree four states (creating, active, released, failed).
-- They describe a directory, and they are enough right up to the moment the
-- task's work actually lands, at which point three questions appear that none
-- of them can answer:
--
--   * "has this already been integrated?" A crash between the ref update and
--     the cleanup leaves an ACTIVE worktree on a branch full of commits, which
--     is byte-for-byte the state of a task that has not integrated yet. The
--     next pass reads it as ready and integrates it a second time. `integrated`
--     is written after the audit record and before the first removal, so that
--     window has a durable witness of its own.
--
--   * "may this branch be deleted?" Only when the work on it is provably
--     reachable from where it landed. integrated_sha records where that was,
--     as the integration's own audit record reported it — a fact that stops
--     being derivable the moment the target ref moves again. Without it,
--     cleanup would be deleting commits on the strength of a state name.
--
--   * "was this deliberately kept?" A failed or cancelled task's commits may
--     be the only copy of work somebody still wants. `preserved` is a durable
--     "do not clean this up", so a later pass reads a decision instead of
--     re-deriving one from whatever happens to be on disk.
--
-- branch_deleted is recorded rather than re-derived because "the branch is
-- absent" and "AO deleted the branch" are different facts, and only the second
-- one means cleanup finished. A row released with branch_deleted = 0 still has
-- work sitting on an ao/* branch, and a reconcile pass has to finish that.
--
-- SQLite cannot widen a CHECK in place, so the state column forces a table
-- rebuild. Unlike 0119/0130, workflow_task_worktrees is referenced by nothing —
-- it is a leaf, with foreign keys pointing OUT of it at workflow_tasks and
-- workflow_runs — so dropping it cascades nowhere and no backup tables are
-- needed. The rows are carried across directly.
--
-- Nothing about existing rows changes: every record keeps its state, and the
-- two new columns default to "never integrated, branch still there", which is
-- what every record written before this migration in fact was.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE workflow_task_worktrees_new (
    task_id          TEXT PRIMARY KEY REFERENCES workflow_tasks(id) ON DELETE CASCADE,
    workflow_run_id  TEXT NOT NULL REFERENCES workflow_runs(id) ON DELETE CASCADE,
    project_id       TEXT NOT NULL,
    repo_path        TEXT NOT NULL,
    worktree_path    TEXT NOT NULL,
    branch           TEXT NOT NULL,
    target_branch    TEXT NOT NULL,
    base_sha         TEXT NOT NULL,
    dependencies_json TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(dependencies_json)),
    execution_mode   TEXT NOT NULL CHECK (execution_mode IN (
        'isolated_worktree',
        'smart_parallel_worktrees'
    )),
    state            TEXT NOT NULL CHECK (state IN (
        'creating',
        'active',
        'integrated',
        'released',
        'preserved',
        'failed'
    )),
    -- The commit this task's work reached its target at. Empty until the
    -- integration is a durable fact; the authorization for deleting the branch.
    integrated_sha   TEXT NOT NULL DEFAULT '',
    -- 1 only when AO deleted the ao/* branch itself.
    branch_deleted   INTEGER NOT NULL DEFAULT 0,
    detail           TEXT NOT NULL DEFAULT '',
    created_at       TIMESTAMP NOT NULL,
    updated_at       TIMESTAMP NOT NULL,
    released_at      TIMESTAMP
);
-- +goose StatementEnd

-- +goose StatementBegin
INSERT INTO workflow_task_worktrees_new (task_id, workflow_run_id, project_id, repo_path,
    worktree_path, branch, target_branch, base_sha, dependencies_json, execution_mode,
    state, integrated_sha, branch_deleted, detail, created_at, updated_at, released_at)
SELECT task_id, workflow_run_id, project_id, repo_path,
    worktree_path, branch, target_branch, base_sha, dependencies_json, execution_mode,
    state, '', 0, detail, created_at, updated_at, released_at
FROM workflow_task_worktrees;
-- +goose StatementEnd

-- +goose StatementBegin
DROP INDEX IF EXISTS idx_workflow_task_worktrees_run;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE workflow_task_worktrees;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE workflow_task_worktrees_new RENAME TO workflow_task_worktrees;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_workflow_task_worktrees_run
    ON workflow_task_worktrees(workflow_run_id);
-- +goose StatementEnd

-- +goose StatementBegin
-- Startup reconciliation reads every record the manager is not yet done with,
-- across every run, so an AO worktree belonging to a run that has since gone
-- terminal is still matched against what is on disk rather than orphaned.
CREATE INDEX idx_workflow_task_worktrees_state
    ON workflow_task_worktrees(state);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_workflow_task_worktrees_state;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_workflow_task_worktrees_run;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TABLE workflow_task_worktrees_old (
    task_id          TEXT PRIMARY KEY REFERENCES workflow_tasks(id) ON DELETE CASCADE,
    workflow_run_id  TEXT NOT NULL REFERENCES workflow_runs(id) ON DELETE CASCADE,
    project_id       TEXT NOT NULL,
    repo_path        TEXT NOT NULL,
    worktree_path    TEXT NOT NULL,
    branch           TEXT NOT NULL,
    target_branch    TEXT NOT NULL,
    base_sha         TEXT NOT NULL,
    dependencies_json TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(dependencies_json)),
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
-- The two states this migration added have no pre-0131 spelling. `integrated`
-- is a worktree whose work landed and whose directory is still there, which is
-- exactly what `active` meant before; `preserved` is a kept-on-purpose failure,
-- which is what `failed` meant.
INSERT INTO workflow_task_worktrees_old (task_id, workflow_run_id, project_id, repo_path,
    worktree_path, branch, target_branch, base_sha, dependencies_json, execution_mode,
    state, detail, created_at, updated_at, released_at)
SELECT task_id, workflow_run_id, project_id, repo_path,
    worktree_path, branch, target_branch, base_sha, dependencies_json, execution_mode,
    CASE state WHEN 'integrated' THEN 'active' WHEN 'preserved' THEN 'failed' ELSE state END,
    detail, created_at, updated_at, released_at
FROM workflow_task_worktrees;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE workflow_task_worktrees;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE workflow_task_worktrees_old RENAME TO workflow_task_worktrees;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX idx_workflow_task_worktrees_run
    ON workflow_task_worktrees(workflow_run_id);
-- +goose StatementEnd
