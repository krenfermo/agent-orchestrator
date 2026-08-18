-- Checkpoint 8P-D: widen workflow_wake_schedules.reason to include
-- 'autonomous_progress' -- the headless progression heartbeat wake for a
-- master/objective run under AutonomousMode (see internal/workflow/wake's
-- ReasonAutonomousProgress doc comment). SQLite has no ALTER TABLE ... DROP
-- CONSTRAINT, so this rebuilds the table with the widened CHECK, exactly
-- mirroring 0106's own DDL otherwise. 0106 itself is never hand-edited.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE workflow_wake_schedules_new (
    id                 TEXT PRIMARY KEY,
    workflow_run_id    TEXT NOT NULL,
    workflow_step_id   TEXT,
    reason             TEXT NOT NULL CHECK (reason IN (
        'capacity_reset',
        'capacity_probe',
        'transient_retry',
        'question_resolver_capacity',
        'reviewer_capacity',
        'worker_capacity',
        'planner_capacity',
        'autonomous_progress'
    )),
    status             TEXT NOT NULL CHECK (status IN ('pending','claimed','completed','cancelled')),
    idempotency_key    TEXT NOT NULL UNIQUE,
    scheduled_at       TIMESTAMP NOT NULL,
    known_reset_at     TIMESTAMP,
    attempt_count      INTEGER NOT NULL DEFAULT 0,
    claimed_by         TEXT,
    claimed_at         TIMESTAMP,
    completed_at       TIMESTAMP,
    cancelled_at       TIMESTAMP,
    last_error         TEXT NOT NULL DEFAULT '',
    created_at         TIMESTAMP NOT NULL,
    updated_at         TIMESTAMP NOT NULL
);
-- +goose StatementEnd

-- +goose StatementBegin
INSERT INTO workflow_wake_schedules_new
SELECT id, workflow_run_id, workflow_step_id, reason, status, idempotency_key,
       scheduled_at, known_reset_at, attempt_count, claimed_by, claimed_at,
       completed_at, cancelled_at, last_error, created_at, updated_at
FROM workflow_wake_schedules;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE workflow_wake_schedules;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE workflow_wake_schedules_new RENAME TO workflow_wake_schedules;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_workflow_wake_schedules_status_scheduled
    ON workflow_wake_schedules (status, scheduled_at);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_workflow_wake_schedules_run_status
    ON workflow_wake_schedules (workflow_run_id, status);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE TABLE workflow_wake_schedules_old (
    id                 TEXT PRIMARY KEY,
    workflow_run_id    TEXT NOT NULL,
    workflow_step_id   TEXT,
    reason             TEXT NOT NULL CHECK (reason IN (
        'capacity_reset',
        'capacity_probe',
        'transient_retry',
        'question_resolver_capacity',
        'reviewer_capacity',
        'worker_capacity',
        'planner_capacity'
    )),
    status             TEXT NOT NULL CHECK (status IN ('pending','claimed','completed','cancelled')),
    idempotency_key    TEXT NOT NULL UNIQUE,
    scheduled_at       TIMESTAMP NOT NULL,
    known_reset_at     TIMESTAMP,
    attempt_count      INTEGER NOT NULL DEFAULT 0,
    claimed_by         TEXT,
    claimed_at         TIMESTAMP,
    completed_at       TIMESTAMP,
    cancelled_at       TIMESTAMP,
    last_error         TEXT NOT NULL DEFAULT '',
    created_at         TIMESTAMP NOT NULL,
    updated_at         TIMESTAMP NOT NULL
);
-- +goose StatementEnd

-- +goose StatementBegin
INSERT INTO workflow_wake_schedules_old
SELECT id, workflow_run_id, workflow_step_id, reason, status, idempotency_key,
       scheduled_at, known_reset_at, attempt_count, claimed_by, claimed_at,
       completed_at, cancelled_at, last_error, created_at, updated_at
FROM workflow_wake_schedules
WHERE reason != 'autonomous_progress';
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE workflow_wake_schedules;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE workflow_wake_schedules_old RENAME TO workflow_wake_schedules;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_workflow_wake_schedules_status_scheduled
    ON workflow_wake_schedules (status, scheduled_at);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_workflow_wake_schedules_run_status
    ON workflow_wake_schedules (workflow_run_id, status);
-- +goose StatementEnd
