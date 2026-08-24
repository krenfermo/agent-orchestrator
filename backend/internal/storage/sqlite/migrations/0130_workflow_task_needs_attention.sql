-- Task-level attention as DOMAIN state, not a decorative checkpoint.
--
-- A task whose integration hits a conflict AO may not resolve is stopped, and
-- until now nothing in the schema could say so. The conflict was recorded as a
-- checkpoint on the master run and the task row stayed at 'running' — which
-- meant every reconcile pass (one per board poll) read a task that looked
-- ready, tried the same integration again, rebased the same worktree onto the
-- same target, hit the same conflict, and wrote another checkpoint and another
-- notification. A restart lost even that, because a checkpoint nobody folds is
-- not a state anybody resumes from.
--
-- So the stop becomes state:
--
--   * state = 'needs_attention' — a NON-terminal state a task only leaves by
--     an explicit human resume. Reconciliation skips it, its dependents stay
--     blocked, and its independent siblings are untouched.
--
--   * attention_reason / attention_json / attention_at — everything a person
--     needs to act, on the row that is stopped rather than scattered across a
--     ledger: the stable reason code, and a JSON body carrying the conflicting
--     files, the source / base / target-before SHAs, the integration strategy
--     that was attempted and the recommended action. Inline for the same
--     reason scope_json is (0127): it is read and written as a whole, with the
--     task that owns it, and never queried across runs.
--
-- SQLite cannot widen a CHECK in place, so the state column forces a table
-- rebuild — and workflow_tasks is now referenced by THREE tables, all
-- ON DELETE CASCADE: workflow_task_dependencies (0101), workflow_task_relationships
-- (0127) and workflow_task_worktrees (0128). Foreign keys are enforced while
-- these migrations run and goose wraps each one in a transaction, inside which
-- PRAGMA foreign_keys is silently ignored, so `DROP TABLE workflow_tasks`
-- would cascade all three away. 0119 learned that against a copy of a real
-- ~/.ao/data/ao.db (the dependency graph went from 8 rows to 0); this repeats
-- its remedy for all three children — park the rows in unconstrained backup
-- tables, rebuild, restore, drop the backups.
--
-- Nothing about existing rows changes: every task keeps its state, and the two
-- new columns default to "no attention", which is what every task recorded
-- before this migration in fact had.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE workflow_task_dependencies_bak (
    workflow_task_id   TEXT NOT NULL,
    depends_on_task_id TEXT NOT NULL
);
-- +goose StatementEnd

-- +goose StatementBegin
INSERT INTO workflow_task_dependencies_bak (workflow_task_id, depends_on_task_id)
SELECT workflow_task_id, depends_on_task_id FROM workflow_task_dependencies;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE workflow_task_relationships_bak AS SELECT * FROM workflow_task_relationships;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE workflow_task_worktrees_bak AS SELECT * FROM workflow_task_worktrees;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE workflow_task_dependencies;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE workflow_task_relationships;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE workflow_task_worktrees;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE workflow_tasks_new (
    id TEXT PRIMARY KEY,
    workflow_run_id TEXT NOT NULL REFERENCES workflow_runs(id) ON DELETE CASCADE,
    plan_step_id TEXT NOT NULL,
    ordinal INTEGER NOT NULL,
    title TEXT NOT NULL CHECK (length(title) > 0),
    description TEXT NOT NULL CHECK (length(description) > 0),
    acceptance_criteria_json TEXT NOT NULL CHECK (json_valid(acceptance_criteria_json)),
    verify_json TEXT NOT NULL CHECK (json_valid(verify_json)),
    scope_json TEXT NOT NULL DEFAULT '{}',
    state TEXT NOT NULL CHECK (state IN (
        'blocked','eligible','running','needs_attention','completed','failed','cancelled'
    )),
    -- The stable machine-checkable reason the task is parked
    -- (integration.AttentionReason today). '' whenever state is not
    -- 'needs_attention'.
    attention_reason TEXT NOT NULL DEFAULT '',
    -- domain.WorkflowTaskAttention: conflicting files, source/base/target-before
    -- SHAs, the strategy attempted, the recommended action, and the id of the
    -- attempt that produced it. '{}' when there is no attention.
    attention_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(attention_json)),
    attention_at TIMESTAMP,
    execution_run_id TEXT REFERENCES workflow_runs(id),
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    completed_at TIMESTAMP,
    UNIQUE(workflow_run_id, plan_step_id),
    UNIQUE(workflow_run_id, ordinal),
    UNIQUE(execution_run_id),
    -- A parked task must say why. Enforcing it here rather than in Go is what
    -- makes "needs_attention with no reason" unrepresentable instead of merely
    -- discouraged: an unexplained stop is the failure mode this whole column
    -- set exists to prevent.
    CHECK (state <> 'needs_attention' OR length(attention_reason) > 0)
);
-- +goose StatementEnd

-- +goose StatementBegin
INSERT INTO workflow_tasks_new (
    id, workflow_run_id, plan_step_id, ordinal, title, description,
    acceptance_criteria_json, verify_json, scope_json, state, execution_run_id,
    created_at, updated_at, completed_at
)
SELECT id, workflow_run_id, plan_step_id, ordinal, title, description,
       acceptance_criteria_json, verify_json, scope_json, state, execution_run_id,
       created_at, updated_at, completed_at
FROM workflow_tasks;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE workflow_tasks;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE workflow_tasks_new RENAME TO workflow_tasks;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_workflow_tasks_run ON workflow_tasks(workflow_run_id, ordinal);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE workflow_task_dependencies (
    workflow_task_id TEXT NOT NULL REFERENCES workflow_tasks(id) ON DELETE CASCADE,
    depends_on_task_id TEXT NOT NULL REFERENCES workflow_tasks(id) ON DELETE CASCADE,
    PRIMARY KEY(workflow_task_id, depends_on_task_id),
    CHECK(workflow_task_id <> depends_on_task_id)
);
-- +goose StatementEnd

-- +goose StatementBegin
INSERT INTO workflow_task_dependencies (workflow_task_id, depends_on_task_id)
SELECT workflow_task_id, depends_on_task_id FROM workflow_task_dependencies_bak;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE workflow_task_relationships (
    workflow_run_id  TEXT NOT NULL REFERENCES workflow_runs(id) ON DELETE CASCADE,
    task_id          TEXT NOT NULL REFERENCES workflow_tasks(id) ON DELETE CASCADE,
    related_task_id  TEXT NOT NULL REFERENCES workflow_tasks(id) ON DELETE CASCADE,
    relation         TEXT NOT NULL CHECK (relation IN (
        'functional_dependency',
        'probable_write_conflict',
        'independent'
    )),
    reason           TEXT NOT NULL DEFAULT '',
    detail           TEXT NOT NULL DEFAULT '',
    overlap_json     TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(overlap_json)),
    created_at       TIMESTAMP NOT NULL,
    PRIMARY KEY (task_id, related_task_id),
    CHECK (task_id < related_task_id)
);
-- +goose StatementEnd

-- +goose StatementBegin
INSERT INTO workflow_task_relationships (workflow_run_id, task_id, related_task_id,
    relation, reason, detail, overlap_json, created_at)
SELECT workflow_run_id, task_id, related_task_id, relation, reason, detail,
       overlap_json, created_at
FROM workflow_task_relationships_bak;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_workflow_task_relationships_run
    ON workflow_task_relationships(workflow_run_id);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE workflow_task_worktrees (
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
INSERT INTO workflow_task_worktrees (task_id, workflow_run_id, project_id, repo_path,
    worktree_path, branch, target_branch, base_sha, dependencies_json, execution_mode,
    state, detail, created_at, updated_at, released_at)
SELECT task_id, workflow_run_id, project_id, repo_path, worktree_path, branch,
       target_branch, base_sha, dependencies_json, execution_mode, state, detail,
       created_at, updated_at, released_at
FROM workflow_task_worktrees_bak;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_workflow_task_worktrees_run
    ON workflow_task_worktrees(workflow_run_id);
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE workflow_task_dependencies_bak;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE workflow_task_relationships_bak;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE workflow_task_worktrees_bak;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE TABLE workflow_task_dependencies_bak (
    workflow_task_id   TEXT NOT NULL,
    depends_on_task_id TEXT NOT NULL
);
-- +goose StatementEnd

-- +goose StatementBegin
INSERT INTO workflow_task_dependencies_bak (workflow_task_id, depends_on_task_id)
SELECT workflow_task_id, depends_on_task_id FROM workflow_task_dependencies;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE workflow_task_relationships_bak AS SELECT * FROM workflow_task_relationships;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE workflow_task_worktrees_bak AS SELECT * FROM workflow_task_worktrees;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE workflow_task_dependencies;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE workflow_task_relationships;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE workflow_task_worktrees;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE workflow_tasks_old (
    id TEXT PRIMARY KEY,
    workflow_run_id TEXT NOT NULL REFERENCES workflow_runs(id) ON DELETE CASCADE,
    plan_step_id TEXT NOT NULL,
    ordinal INTEGER NOT NULL,
    title TEXT NOT NULL CHECK (length(title) > 0),
    description TEXT NOT NULL CHECK (length(description) > 0),
    acceptance_criteria_json TEXT NOT NULL CHECK (json_valid(acceptance_criteria_json)),
    verify_json TEXT NOT NULL CHECK (json_valid(verify_json)),
    scope_json TEXT NOT NULL DEFAULT '{}',
    state TEXT NOT NULL CHECK (state IN ('blocked','eligible','running','completed','failed','cancelled')),
    execution_run_id TEXT REFERENCES workflow_runs(id),
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    completed_at TIMESTAMP,
    UNIQUE(workflow_run_id, plan_step_id),
    UNIQUE(workflow_run_id, ordinal),
    UNIQUE(execution_run_id)
);
-- +goose StatementEnd

-- +goose StatementBegin
INSERT INTO workflow_tasks_old (
    id, workflow_run_id, plan_step_id, ordinal, title, description,
    acceptance_criteria_json, verify_json, scope_json, state, execution_run_id,
    created_at, updated_at, completed_at
)
SELECT id, workflow_run_id, plan_step_id, ordinal, title, description,
       acceptance_criteria_json, verify_json, scope_json,
       -- A parked task has no pre-0130 spelling. 'running' is the state it in
       -- fact held before this migration existed, and the one the older code
       -- knows how to reconcile.
       CASE state WHEN 'needs_attention' THEN 'running' ELSE state END,
       execution_run_id, created_at, updated_at, completed_at
FROM workflow_tasks;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE workflow_tasks;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE workflow_tasks_old RENAME TO workflow_tasks;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_workflow_tasks_run ON workflow_tasks(workflow_run_id, ordinal);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE workflow_task_dependencies (
    workflow_task_id TEXT NOT NULL REFERENCES workflow_tasks(id) ON DELETE CASCADE,
    depends_on_task_id TEXT NOT NULL REFERENCES workflow_tasks(id) ON DELETE CASCADE,
    PRIMARY KEY(workflow_task_id, depends_on_task_id),
    CHECK(workflow_task_id <> depends_on_task_id)
);
-- +goose StatementEnd

-- +goose StatementBegin
INSERT INTO workflow_task_dependencies (workflow_task_id, depends_on_task_id)
SELECT workflow_task_id, depends_on_task_id FROM workflow_task_dependencies_bak;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE workflow_task_relationships (
    workflow_run_id  TEXT NOT NULL REFERENCES workflow_runs(id) ON DELETE CASCADE,
    task_id          TEXT NOT NULL REFERENCES workflow_tasks(id) ON DELETE CASCADE,
    related_task_id  TEXT NOT NULL REFERENCES workflow_tasks(id) ON DELETE CASCADE,
    relation         TEXT NOT NULL CHECK (relation IN (
        'functional_dependency',
        'probable_write_conflict',
        'independent'
    )),
    reason           TEXT NOT NULL DEFAULT '',
    detail           TEXT NOT NULL DEFAULT '',
    overlap_json     TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(overlap_json)),
    created_at       TIMESTAMP NOT NULL,
    PRIMARY KEY (task_id, related_task_id),
    CHECK (task_id < related_task_id)
);
-- +goose StatementEnd

-- +goose StatementBegin
INSERT INTO workflow_task_relationships (workflow_run_id, task_id, related_task_id,
    relation, reason, detail, overlap_json, created_at)
SELECT workflow_run_id, task_id, related_task_id, relation, reason, detail,
       overlap_json, created_at
FROM workflow_task_relationships_bak;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_workflow_task_relationships_run
    ON workflow_task_relationships(workflow_run_id);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE workflow_task_worktrees (
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
INSERT INTO workflow_task_worktrees (task_id, workflow_run_id, project_id, repo_path,
    worktree_path, branch, target_branch, base_sha, dependencies_json, execution_mode,
    state, detail, created_at, updated_at, released_at)
SELECT task_id, workflow_run_id, project_id, repo_path, worktree_path, branch,
       target_branch, base_sha, dependencies_json, execution_mode, state, detail,
       created_at, updated_at, released_at
FROM workflow_task_worktrees_bak;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_workflow_task_worktrees_run
    ON workflow_task_worktrees(workflow_run_id);
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE workflow_task_dependencies_bak;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE workflow_task_relationships_bak;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE workflow_task_worktrees_bak;
-- +goose StatementEnd
