-- Checkpoint 8P-E.13 Phase 6: widen workflow_tasks.state to include 'failed'.
--
-- Before this, a master run's task vocabulary had no way to say "this task's
-- child run ended badly": the only terminal states were 'completed' and
-- 'cancelled'. So when a child run failed, reconcileMasterTasks moved the
-- PARENT to needs_attention and left the task itself at 'running' — forever,
-- because nothing ever revisits a task whose child is already terminal. That
-- is the stale "Task 1/7 running" the Board showed for a task that had in fact
-- stopped hours earlier.
--
-- SQLite has no ALTER TABLE ... DROP CONSTRAINT, so this rebuilds the table
-- with the widened CHECK. Unlike 0114/0118 (which rebuilt
-- workflow_wake_schedules, a table nothing references), workflow_tasks IS
-- referenced — by workflow_task_dependencies, twice, both
-- ON DELETE CASCADE.
--
-- That makes the naive rebuild destructive, and not theoretically: running the
-- first draft of this migration against a copy of a real ~/.ao/data/ao.db took
-- workflow_task_dependencies from 8 rows to 0. Foreign keys are enforced while
-- these migrations run, so `DROP TABLE workflow_tasks` cascaded and deleted the
-- entire dependency graph of every master run. The application would then have
-- read every task as having no dependencies at all, made every blocked task
-- immediately eligible, and dispatched a whole plan out of order.
--
-- PRAGMA foreign_keys is not an escape hatch here: goose wraps a migration in a
-- transaction, and SQLite silently ignores that pragma inside one. So the
-- dependency rows are explicitly parked in an unconstrained backup table,
-- carried across the rebuild, and restored — which is correct whether or not
-- foreign keys happen to be enforced.

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
DROP TABLE workflow_task_dependencies;
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
INSERT INTO workflow_tasks_new (
    id, workflow_run_id, plan_step_id, ordinal, title, description,
    acceptance_criteria_json, verify_json, state, execution_run_id,
    created_at, updated_at, completed_at
)
SELECT id, workflow_run_id, plan_step_id, ordinal, title, description,
       acceptance_criteria_json, verify_json, state, execution_run_id,
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
DROP TABLE workflow_task_dependencies_bak;
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
DROP TABLE workflow_task_dependencies;
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
    state TEXT NOT NULL CHECK (state IN ('blocked','eligible','running','completed','cancelled')),
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
    acceptance_criteria_json, verify_json, state, execution_run_id,
    created_at, updated_at, completed_at
)
SELECT id, workflow_run_id, plan_step_id, ordinal, title, description,
       acceptance_criteria_json, verify_json,
       CASE state WHEN 'failed' THEN 'cancelled' ELSE state END,
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
DROP TABLE workflow_task_dependencies_bak;
-- +goose StatementEnd
