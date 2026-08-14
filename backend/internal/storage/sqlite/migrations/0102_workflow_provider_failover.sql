-- +goose Up
-- +goose StatementBegin
-- Checkpoint 8H: durable provider failure classification + attempt budget +
-- Codex->Claude failover. Same table-rebuild dance as 0096/0097/0099/0100
-- (SQLite cannot widen a CHECK constraint in place). Adds exactly the two
-- new error_class values 8H's classifier needs (capacity_exhausted,
-- binary_missing) that no prior checkpoint's taxonomy already covered.
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
        'verify_artifact_missing','verify_artifact_mismatch','verify_workspace_changed','verify_ambiguous',
        'capacity_exhausted','binary_missing'
    )),
    retry_after TIMESTAMP,
    UNIQUE (workflow_step_id, attempt_number)
);
INSERT INTO workflow_attempts_new SELECT * FROM workflow_attempts;
DROP TABLE workflow_attempts;
ALTER TABLE workflow_attempts_new RENAME TO workflow_attempts;
CREATE INDEX idx_workflow_attempts_step ON workflow_attempts (workflow_step_id, attempt_number);
-- +goose StatementEnd

-- +goose StatementBegin
-- Same rebuild dance for workflow_outbox.command_type: 8H adds
-- switch_worker_agent, the durable command a live-session Codex->Claude
-- failover dispatches through session_manager's existing agent-switching
-- saga (never a second switching mechanism).
CREATE TABLE workflow_outbox_new (
    id                TEXT PRIMARY KEY,
    workflow_run_id   TEXT NOT NULL REFERENCES workflow_runs (id) ON DELETE CASCADE,
    workflow_step_id  TEXT REFERENCES workflow_steps (id),
    idempotency_key   TEXT NOT NULL UNIQUE,
    command_type      TEXT NOT NULL CHECK (command_type IN
        ('spawn_worker_session','trigger_review','send_message','cancel_session','switch_worker_agent')),
    payload           TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(payload)),
    status            TEXT NOT NULL DEFAULT 'pending' CHECK (status IN
        ('pending','dispatched','acknowledged','failed')),
    created_at        TIMESTAMP NOT NULL,
    dispatched_at     TIMESTAMP,
    acknowledged_at   TIMESTAMP,
    failed_at         TIMESTAMP,
    error_class       TEXT NOT NULL DEFAULT ''
);
INSERT INTO workflow_outbox_new SELECT * FROM workflow_outbox;
DROP TABLE workflow_outbox;
ALTER TABLE workflow_outbox_new RENAME TO workflow_outbox;
CREATE INDEX idx_workflow_outbox_run ON workflow_outbox (workflow_run_id);
CREATE INDEX idx_workflow_outbox_pending ON workflow_outbox (status) WHERE status = 'pending';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE TABLE workflow_outbox_old (
    id                TEXT PRIMARY KEY,
    workflow_run_id   TEXT NOT NULL REFERENCES workflow_runs (id) ON DELETE CASCADE,
    workflow_step_id  TEXT REFERENCES workflow_steps (id),
    idempotency_key   TEXT NOT NULL UNIQUE,
    command_type      TEXT NOT NULL CHECK (command_type IN
        ('spawn_worker_session','trigger_review','send_message','cancel_session')),
    payload           TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(payload)),
    status            TEXT NOT NULL DEFAULT 'pending' CHECK (status IN
        ('pending','dispatched','acknowledged','failed')),
    created_at        TIMESTAMP NOT NULL,
    dispatched_at     TIMESTAMP,
    acknowledged_at   TIMESTAMP,
    failed_at         TIMESTAMP,
    error_class       TEXT NOT NULL DEFAULT ''
);
INSERT INTO workflow_outbox_old SELECT id, workflow_run_id, workflow_step_id, idempotency_key,
    command_type, payload, status, created_at, dispatched_at, acknowledged_at, failed_at, error_class
FROM workflow_outbox WHERE command_type <> 'switch_worker_agent';
DROP TABLE workflow_outbox;
ALTER TABLE workflow_outbox_old RENAME TO workflow_outbox;
CREATE INDEX idx_workflow_outbox_run ON workflow_outbox (workflow_run_id);
CREATE INDEX idx_workflow_outbox_pending ON workflow_outbox (status) WHERE status = 'pending';
-- +goose StatementEnd

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
        'worker_terminated_unexpectedly','ambiguous_worker_state','reviewer_launch_failed',
        'fix_budget_exhausted','verify_command_failed','verify_timeout','verify_environment_error',
        'verify_artifact_missing','verify_artifact_mismatch','verify_workspace_changed','verify_ambiguous'
    )),
    retry_after TIMESTAMP,
    UNIQUE (workflow_step_id, attempt_number)
);
INSERT INTO workflow_attempts_old SELECT id, workflow_step_id, attempt_number, harness, model,
    started_at, finished_at, outcome,
    CASE WHEN error_class IN (
        'rate_limited','auth','transient','tool','test_failed','review_changes_requested',
        'session_create_failed','agent_start_failed','prompt_delivery_failed','runtime_failed',
        'worker_terminated_unexpectedly','ambiguous_worker_state','reviewer_launch_failed',
        'fix_budget_exhausted','verify_command_failed','verify_timeout','verify_environment_error',
        'verify_artifact_missing','verify_artifact_mismatch','verify_workspace_changed','verify_ambiguous'
    ) THEN error_class ELSE NULL END,
    retry_after
FROM workflow_attempts;
DROP TABLE workflow_attempts;
ALTER TABLE workflow_attempts_old RENAME TO workflow_attempts;
CREATE INDEX idx_workflow_attempts_step ON workflow_attempts (workflow_step_id, attempt_number);
-- +goose StatementEnd
