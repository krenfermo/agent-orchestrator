-- +goose Up
-- +goose StatementBegin
-- Checkpoint 8E: structured failure classes for the local verification layer.
CREATE TABLE workflow_attempts_new (
    id TEXT PRIMARY KEY,
    workflow_step_id TEXT NOT NULL REFERENCES workflow_steps (id) ON DELETE CASCADE,
    attempt_number INTEGER NOT NULL,
    harness TEXT NOT NULL DEFAULT '',
    model TEXT NOT NULL DEFAULT '',
    started_at TIMESTAMP NOT NULL,
    finished_at TIMESTAMP,
    outcome TEXT CHECK (outcome IS NULL OR outcome IN ('succeeded','failed','cancelled')),
    error_class TEXT CHECK (error_class IS NULL OR error_class IN (
        'rate_limited','auth','transient','tool','test_failed','review_changes_requested',
        'session_create_failed','agent_start_failed','prompt_delivery_failed','runtime_failed',
        'worker_terminated_unexpectedly','ambiguous_worker_state','reviewer_launch_failed',
        'fix_budget_exhausted','verify_command_failed','verify_timeout','verify_environment_error',
        'verify_artifact_missing','verify_artifact_mismatch','verify_workspace_changed','verify_ambiguous'
    )),
    retry_after TIMESTAMP,
    UNIQUE (workflow_step_id, attempt_number)
);
INSERT INTO workflow_attempts_new SELECT * FROM workflow_attempts;
DROP TABLE workflow_attempts;
ALTER TABLE workflow_attempts_new RENAME TO workflow_attempts;
CREATE INDEX idx_workflow_attempts_step ON workflow_attempts (workflow_step_id, attempt_number);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE TABLE workflow_attempts_old (
    id TEXT PRIMARY KEY,
    workflow_step_id TEXT NOT NULL REFERENCES workflow_steps (id) ON DELETE CASCADE,
    attempt_number INTEGER NOT NULL,
    harness TEXT NOT NULL DEFAULT '',
    model TEXT NOT NULL DEFAULT '',
    started_at TIMESTAMP NOT NULL,
    finished_at TIMESTAMP,
    outcome TEXT CHECK (outcome IS NULL OR outcome IN ('succeeded','failed','cancelled')),
    error_class TEXT CHECK (error_class IS NULL OR error_class IN (
        'rate_limited','auth','transient','tool','test_failed','review_changes_requested',
        'session_create_failed','agent_start_failed','prompt_delivery_failed','runtime_failed',
        'worker_terminated_unexpectedly','ambiguous_worker_state','reviewer_launch_failed','fix_budget_exhausted'
    )),
    retry_after TIMESTAMP,
    UNIQUE (workflow_step_id, attempt_number)
);
INSERT INTO workflow_attempts_old SELECT id, workflow_step_id, attempt_number, harness, model,
    started_at, finished_at, outcome,
    CASE WHEN error_class IN (
        'rate_limited','auth','transient','tool','test_failed','review_changes_requested',
        'session_create_failed','agent_start_failed','prompt_delivery_failed','runtime_failed',
        'worker_terminated_unexpectedly','ambiguous_worker_state','reviewer_launch_failed','fix_budget_exhausted'
    ) THEN error_class ELSE NULL END,
    retry_after
FROM workflow_attempts;
DROP TABLE workflow_attempts;
ALTER TABLE workflow_attempts_old RENAME TO workflow_attempts;
CREATE INDEX idx_workflow_attempts_step ON workflow_attempts (workflow_step_id, attempt_number);
-- +goose StatementEnd
