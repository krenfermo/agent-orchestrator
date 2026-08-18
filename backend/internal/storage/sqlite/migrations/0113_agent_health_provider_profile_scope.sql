-- +goose Up
-- +goose StatementBegin
-- Checkpoint 8P-C: scope agent health facts to the exact (user, provider
-- profile) that produced them, so one user's cooldown/outage never leaks
-- into another user's routing decisions. Both new columns are nullable and
-- existing rows are left NULL -- a "legacy/global" tier that remains
-- readable as a compatibility fallback only when no scoped row exists yet
-- for a given user+profile+harness (see service/capacity's precedence
-- rule); it is never merged with, or allowed to override, a scoped row.
ALTER TABLE agent_health_events ADD COLUMN user_id TEXT REFERENCES users (id);
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE agent_health_events ADD COLUMN provider_profile_id TEXT REFERENCES provider_profiles (id);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_agent_health_events_scope ON agent_health_events (user_id, provider_profile_id, harness, created_at DESC);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_agent_health_events_scope;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE agent_health_events DROP COLUMN provider_profile_id;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE agent_health_events DROP COLUMN user_id;
-- +goose StatementEnd
