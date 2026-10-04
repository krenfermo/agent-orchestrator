-- +goose Up
-- +goose StatementBegin
-- Checkpoint 8A: Workflow Durable Foundation. These tables are structure
-- only — nothing in this checkpoint launches an agent, executes a step, or
-- auto-advances a run. See docs/plans/engineering-control-center-master-plan.md
-- (Checkpoint 8).
--
-- Deliberate CDC deviation: change_log.event_type (migration 0006) has a SQL
-- CHECK allowlist of exactly 8 session/PR event types. workflow_runs is
-- project-scoped, not session-scoped, and does not semantically fit any of
-- them. Widening that CHECK requires a full SQLite table rebuild (see
-- 0066_chat_session_mode.sql's warning) and misusing session_updated would
-- corrupt the existing session-invalidation contract. So: no CDC trigger for
-- any workflow table in this checkpoint. Clients poll instead (TanStack Query
-- refetchInterval while a run is non-terminal).
CREATE TABLE workflow_runs (
    id              TEXT PRIMARY KEY,
    project_id      TEXT NOT NULL REFERENCES projects (id),
    objective       TEXT NOT NULL CHECK (length(objective) > 0),
    state           TEXT NOT NULL CHECK (state IN
        ('pending','running','waiting','needs_attention','completed','failed','cancelled')),
    policy_version  TEXT NOT NULL DEFAULT 'v1',
    policy_snapshot TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(policy_snapshot)),
    created_at      TIMESTAMP NOT NULL,
    updated_at      TIMESTAMP NOT NULL,
    completed_at    TIMESTAMP,
    cancelled_at    TIMESTAMP
);
CREATE INDEX idx_workflow_runs_project ON workflow_runs (project_id, created_at);
-- Recovery scan needs a cheap non-terminal filter; keep the terminal set here
-- in sync with domain.WorkflowRunState.Terminal().
CREATE INDEX idx_workflow_runs_nonterminal ON workflow_runs (state)
    WHERE state NOT IN ('completed','failed','cancelled');
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE workflow_steps (
    id                          TEXT PRIMARY KEY,
    workflow_run_id             TEXT NOT NULL REFERENCES workflow_runs (id) ON DELETE CASCADE,
    kind                        TEXT NOT NULL CHECK (kind IN
        ('plan','work','review','fix','verify','advance')),
    ordinal                     INTEGER NOT NULL,
    depends_on_step_id          TEXT REFERENCES workflow_steps (id),
    state                       TEXT NOT NULL CHECK (state IN
        ('pending','ready','running','waiting','completed','failed','cancelled')),
    assigned_harness            TEXT NOT NULL DEFAULT '',
    session_id                  TEXT REFERENCES sessions (id),
    review_run_id               TEXT REFERENCES review_run (id),
    expected_artifacts_version  TEXT NOT NULL DEFAULT '',
    created_at                  TIMESTAMP NOT NULL,
    updated_at                  TIMESTAMP NOT NULL,
    completed_at                TIMESTAMP,
    UNIQUE (workflow_run_id, ordinal)
);
CREATE INDEX idx_workflow_steps_run ON workflow_steps (workflow_run_id, ordinal);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE workflow_attempts (
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
CREATE INDEX idx_workflow_attempts_step ON workflow_attempts (workflow_step_id, attempt_number);
-- +goose StatementEnd

-- +goose StatementBegin
-- Append-only. Never update a checkpoint row; insert a new one to advance.
CREATE TABLE workflow_checkpoints (
    id                TEXT PRIMARY KEY,
    workflow_run_id   TEXT NOT NULL REFERENCES workflow_runs (id) ON DELETE CASCADE,
    workflow_step_id  TEXT REFERENCES workflow_steps (id),
    attempt_id        TEXT REFERENCES workflow_attempts (id),
    project_id        TEXT NOT NULL REFERENCES projects (id),
    session_id        TEXT REFERENCES sessions (id),
    branch            TEXT NOT NULL DEFAULT '',
    worktree_path     TEXT NOT NULL DEFAULT '',
    base_sha          TEXT NOT NULL DEFAULT '',
    head_sha          TEXT NOT NULL DEFAULT '',
    review_run_id     TEXT REFERENCES review_run (id),
    review_verdict    TEXT NOT NULL DEFAULT '',
    retry_state       TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(retry_state)),
    next_action       TEXT NOT NULL DEFAULT '',
    durable_phase     TEXT NOT NULL DEFAULT '',
    payload_version   TEXT NOT NULL DEFAULT 'v1',
    created_at        TIMESTAMP NOT NULL
);
CREATE INDEX idx_workflow_checkpoints_run ON workflow_checkpoints (workflow_run_id, created_at);
-- +goose StatementEnd

-- +goose StatementBegin
-- Durable idempotent-command staging. In 8A nothing dispatches these; the
-- mechanism only needs to prove idempotent enqueue + listing works.
CREATE TABLE workflow_outbox (
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
CREATE INDEX idx_workflow_outbox_run ON workflow_outbox (workflow_run_id);
CREATE INDEX idx_workflow_outbox_pending ON workflow_outbox (status) WHERE status = 'pending';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS workflow_outbox;
DROP TABLE IF EXISTS workflow_checkpoints;
DROP TABLE IF EXISTS workflow_attempts;
DROP TABLE IF EXISTS workflow_steps;
DROP TABLE IF EXISTS workflow_runs;
-- +goose StatementEnd
