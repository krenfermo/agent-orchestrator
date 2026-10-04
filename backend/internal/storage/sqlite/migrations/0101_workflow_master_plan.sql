-- +goose Up
-- Checkpoint 8F: durable master plans and their execution units. Existing
-- workflow_runs remain valid single-task V1 runs; only master runs have a
-- workflow_plans row, and each planned task points at a normal V1 child run.
ALTER TABLE workflow_runs ADD COLUMN parent_workflow_id TEXT REFERENCES workflow_runs(id);
ALTER TABLE workflow_runs ADD COLUMN planned_task_id TEXT;
CREATE UNIQUE INDEX idx_workflow_runs_planned_task ON workflow_runs(planned_task_id)
    WHERE planned_task_id IS NOT NULL;

CREATE TABLE workflow_plans (
    workflow_run_id TEXT PRIMARY KEY REFERENCES workflow_runs(id) ON DELETE CASCADE,
    status TEXT NOT NULL CHECK (status IN ('pending','running','validated','approved','invalid','rejected')),
    approval_mode TEXT NOT NULL CHECK (approval_mode IN ('manual','auto')),
    provider TEXT NOT NULL DEFAULT '',
    model TEXT NOT NULL DEFAULT '',
    prompt_context_version TEXT NOT NULL DEFAULT 'v1',
    context_manifest_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(context_manifest_json)),
    generated_plan_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(generated_plan_json)),
    validation_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(validation_json)),
    plan_hash TEXT NOT NULL DEFAULT '',
    command_status TEXT NOT NULL DEFAULT 'idle' CHECK (command_status IN
        ('idle','pending','running','responded','completed','failed')),
    error_class TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    generated_at TIMESTAMP,
    approved_at TIMESTAMP,
    rejected_at TIMESTAMP
);

CREATE TABLE workflow_tasks (
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
CREATE INDEX idx_workflow_tasks_run ON workflow_tasks(workflow_run_id, ordinal);

CREATE TABLE workflow_task_dependencies (
    workflow_task_id TEXT NOT NULL REFERENCES workflow_tasks(id) ON DELETE CASCADE,
    depends_on_task_id TEXT NOT NULL REFERENCES workflow_tasks(id) ON DELETE CASCADE,
    PRIMARY KEY(workflow_task_id, depends_on_task_id),
    CHECK(workflow_task_id <> depends_on_task_id)
);

-- +goose Down
DROP TABLE IF EXISTS workflow_task_dependencies;
DROP TABLE IF EXISTS workflow_tasks;
DROP TABLE IF EXISTS workflow_plans;
DROP INDEX IF EXISTS idx_workflow_runs_planned_task;
ALTER TABLE workflow_runs DROP COLUMN planned_task_id;
ALTER TABLE workflow_runs DROP COLUMN parent_workflow_id;
