-- Checkpoint 8N: durable, restart-safe wake-up scheduling for a workflow run
-- parked in WorkflowRunWaiting/WorkflowStepWaiting on provider capacity.
-- Today nothing wakes a waiting run back up except a human hitting the API
-- or a full daemon restart's Reconcile pass -- this table is the durable
-- record of "when should AO try this run again, and has anyone already
-- claimed that attempt."
--
-- Design: append-and-CAS-claim, mirroring 0105's resolution-attempt shape
-- and 0012_add_review_tables.sql's review_run status lifecycle:
--   * A row is created (or, if one is already pending/claimed for the same
--     idempotency_key, updated in place -- see workflow_wake_schedules.sql's
--     upsert-by-idempotency-key query) whenever a run/step enters a
--     capacity-wait state. idempotency_key is derived from
--     (run, step-or-role, reason) so re-entering the same wait (e.g. a
--     second reconcile pass landing before the first wake fires) never
--     creates a second competing row.
--   * A lightweight daemon poller claims due rows (status='pending' AND
--     scheduled_at <= now, OR a lease-expired 'claimed' row) by CAS'ing
--     status to 'claimed' with a claimant identity and claimed_at lease
--     timestamp -- this is what makes firing wakes safe across concurrent
--     daemon instances/restarts: a crash between claim and complete leaves
--     the row 'claimed' with a stale claimed_at, and the next poll's due
--     query picks it back up once the lease window has passed, rather than
--     it being silently lost or double-fired by two live pollers at once.
--   * Completion/failure is a second CAS write (status: claimed -> completed
--     or claimed -> pending/cancelled), never a blind UPDATE, so a claim
--     that raced against a manual human "continue" call (which does not go
--     through this table at all) still lands cleanly.
--
-- known_reset_at is NULLABLE and, per this checkpoint's own hard rule, is
-- NEVER a fabricated timestamp: it is populated only when a real
-- AgentHealthEvent.CooldownUntil is known for the harness that was waiting.
-- When nil, scheduled_at is instead computed from bounded exponential
-- backoff with jitter (see workflow/wake/scheduler.go's
-- computeScheduledAt) -- the backoff ceiling is the only place anything
-- resembling a "blind fixed poll" appears, and even then it is capped, not
-- unconditional.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE workflow_wake_schedules (
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
    -- NULL means "backoff-computed" -- never an invented reset time. See
    -- this migration's own doc comment above.
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
-- Primary due-query index: "which pending/lease-expired-claimed rows are
-- due right now."
CREATE INDEX idx_workflow_wake_schedules_status_scheduled
    ON workflow_wake_schedules (status, scheduled_at);
-- +goose StatementEnd

-- +goose StatementBegin
-- Cancel-all-for-run and NextForRun lookups.
CREATE INDEX idx_workflow_wake_schedules_run_status
    ON workflow_wake_schedules (workflow_run_id, status);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS workflow_wake_schedules;
-- +goose StatementEnd
