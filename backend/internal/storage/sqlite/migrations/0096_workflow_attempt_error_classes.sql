-- +goose Up
-- +goose StatementBegin
-- Checkpoint 8B: Workflow -> Codex Worker Execution.
--
-- SQLite cannot widen a CHECK constraint in place, so widening
-- workflow_attempts.error_class requires the standard table-rebuild dance.
-- Confirmed before this migration was written: workflow_attempts (added in
-- 0094, Checkpoint 8A) has no consumers outside internal/storage/sqlite's own
-- queries/workflow.sql + generated code, and no CDC trigger references it
-- (workflow tables deliberately have no CDC trigger at all, see 0094's
-- comment) — so this rebuild is low-risk.
--
-- The new values cover what Checkpoint 8B needs now (session_create_failed,
-- agent_start_failed, prompt_delivery_failed, runtime_failed,
-- worker_terminated_unexpectedly, ambiguous_worker_state) plus intentional
-- headroom so a future checkpoint's error taxonomy does not force another
-- destructive rebuild.
CREATE TABLE workflow_attempts_new (
    id                TEXT PRIMARY KEY,
    workflow_step_id  TEXT NOT NULL REFERENCES workflow_steps (id) ON DELETE CASCADE,
    attempt_number    INTEGER NOT NULL,
    harness           TEXT NOT NULL DEFAULT '',
    model             TEXT NOT NULL DEFAULT '',
    started_at        TIMESTAMP NOT NULL,
    finished_at       TIMESTAMP,
    outcome           TEXT CHECK (outcome IS NULL OR outcome IN ('succeeded','failed','cancelled')),
    error_class       TEXT CHECK (error_class IS NULL OR error_class IN (
        'rate_limited','auth','transient','tool','test_failed','review_changes_requested',
        'session_create_failed','agent_start_failed','prompt_delivery_failed','runtime_failed',
        'worker_terminated_unexpectedly','ambiguous_worker_state'
    )),
    retry_after       TIMESTAMP,
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
    id                TEXT PRIMARY KEY,
    workflow_step_id  TEXT NOT NULL REFERENCES workflow_steps (id) ON DELETE CASCADE,
    attempt_number    INTEGER NOT NULL,
    harness           TEXT NOT NULL DEFAULT '',
    model             TEXT NOT NULL DEFAULT '',
    started_at        TIMESTAMP NOT NULL,
    finished_at       TIMESTAMP,
    outcome           TEXT CHECK (outcome IS NULL OR outcome IN ('succeeded','failed','cancelled')),
    error_class       TEXT CHECK (error_class IS NULL OR error_class IN
        ('rate_limited','auth','transient','tool','test_failed','review_changes_requested')),
    retry_after       TIMESTAMP,
    UNIQUE (workflow_step_id, attempt_number)
);
INSERT INTO workflow_attempts_old SELECT id, workflow_step_id, attempt_number, harness, model,
    started_at, finished_at, outcome,
    CASE WHEN error_class IN (
        'rate_limited','auth','transient','tool','test_failed','review_changes_requested'
    ) THEN error_class ELSE NULL END,
    retry_after
FROM workflow_attempts;
DROP TABLE workflow_attempts;
ALTER TABLE workflow_attempts_old RENAME TO workflow_attempts;
CREATE INDEX idx_workflow_attempts_step ON workflow_attempts (workflow_step_id, attempt_number);
-- +goose StatementEnd
