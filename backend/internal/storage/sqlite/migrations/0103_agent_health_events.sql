-- +goose Up
-- +goose StatementBegin
-- Checkpoint 8H: minimal durable agent health, scoped to harness only (no
-- per-account/per-model dimension yet — §5 of the checkpoint explicitly asks
-- for the minimum needed for failover, not a full ResourceManager). Append
-- -only: a harness's current health is derived by reading its latest event,
-- following the same "persist facts, derive state" rule AO already applies
-- elsewhere (see workflow_checkpoints, 0094's comment).
CREATE TABLE agent_health_events (
    id                   TEXT PRIMARY KEY,
    harness              TEXT NOT NULL,
    state                TEXT NOT NULL CHECK (state IN ('available','cooldown','unavailable','unknown')),
    reason               TEXT NOT NULL DEFAULT '',
    failure_class        TEXT NOT NULL DEFAULT '',
    cooldown_until       TIMESTAMP,
    consecutive_failures INTEGER NOT NULL DEFAULT 0,
    created_at           TIMESTAMP NOT NULL
);
CREATE INDEX idx_agent_health_events_harness ON agent_health_events (harness, created_at DESC);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS agent_health_events;
-- +goose StatementEnd
