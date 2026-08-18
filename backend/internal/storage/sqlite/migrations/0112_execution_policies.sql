-- +goose Up
-- +goose StatementBegin
-- Checkpoint 8P-C: per-user configurable execution/routing policy. Replaces
-- the fixed Claude<->Codex RoutingPolicy with a user-owned ordering of their
-- own ProviderProfiles per role. One row per user (upsert semantics via the
-- unique index below) -- there is no history table; a workflow run that
-- wants to remain unaffected by a later edit captures its own copy into
-- workflow_runs.policy_snapshot at creation time (see domain.WorkflowPolicy).
--
-- Priority columns store a JSON array of provider_profiles.id strings, same
-- convention as provider_profiles.capabilities. Referential integrity to
-- provider_profiles is enforced in the service layer (ownership + capability
-- validation), not by SQL, because the column is a JSON array rather than a
-- single foreign key.
CREATE TABLE user_execution_policies (
    id                         TEXT PRIMARY KEY,
    user_id                    TEXT NOT NULL REFERENCES users (id),
    version                    TEXT NOT NULL,
    autonomous_mode            INTEGER NOT NULL DEFAULT 0,
    planner_priority           TEXT NOT NULL DEFAULT '[]',
    worker_priority            TEXT NOT NULL DEFAULT '[]',
    reviewer_priority          TEXT NOT NULL DEFAULT '[]',
    decision_resolver_priority TEXT NOT NULL DEFAULT '[]',
    fallback_behavior          TEXT NOT NULL CHECK (fallback_behavior IN ('use_next_available', 'wait_for_preferred')),
    review_independence        TEXT NOT NULL CHECK (review_independence IN ('require_different_provider', 'allow_same_provider_fallback')),
    created_at                 TIMESTAMP NOT NULL,
    updated_at                 TIMESTAMP NOT NULL
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE UNIQUE INDEX idx_user_execution_policies_user_id ON user_execution_policies (user_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_user_execution_policies_user_id;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE IF EXISTS user_execution_policies;
-- +goose StatementEnd
