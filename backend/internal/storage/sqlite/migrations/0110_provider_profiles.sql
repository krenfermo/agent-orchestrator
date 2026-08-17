-- Checkpoint 8P-B: per-user provider profiles. Replaces the assumption of
-- global providers with one row per (user, provider connection). Every row
-- is directly, non-nullably owned -- unlike projects/workflow_runs (0109)
-- there is no "unowned, visible to everyone" state here: a profile always
-- belongs to exactly the user who created it, from the moment it's
-- inserted, because the owning user always resolves by the time a profile
-- can be created (trusted-local mode resolves to the bootstrap admin; see
-- httpd/identity).
--
-- auth_state/auth_method describe how AO believes the CLI/provider is
-- currently authenticated; AO does not store OAuth secrets it doesn't
-- manage (Claude Code / Codex manage their own CLI credential storage under
-- the user's runtime-home -- see internal/runtimehome). secret_ciphertext
-- exists only as a future boundary for secret-backed providers (e.g. a
-- future api_key provider); no current provider writes it.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE provider_profiles (
    id                 TEXT PRIMARY KEY,
    user_id            TEXT NOT NULL REFERENCES users (id),
    provider           TEXT NOT NULL,
    harness            TEXT NOT NULL,
    display_name       TEXT NOT NULL,
    enabled            INTEGER NOT NULL DEFAULT 1,
    auth_state         TEXT NOT NULL CHECK (auth_state IN ('unknown', 'authenticated', 'unauthenticated', 'error')),
    auth_method        TEXT NOT NULL CHECK (auth_method IN ('browser_oauth', 'device_flow', 'cli_bootstrap', 'api_key', 'external_login', 'unsupported')),
    default_model      TEXT,
    capabilities       TEXT NOT NULL DEFAULT '[]',
    secret_ciphertext  BLOB,
    created_at         TIMESTAMP NOT NULL,
    updated_at         TIMESTAMP NOT NULL
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_provider_profiles_user_id ON provider_profiles (user_id);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE UNIQUE INDEX idx_provider_profiles_user_provider_harness ON provider_profiles (user_id, provider, harness);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_provider_profiles_user_provider_harness;
-- +goose StatementEnd

-- +goose StatementBegin
DROP INDEX IF EXISTS idx_provider_profiles_user_id;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE IF EXISTS provider_profiles;
-- +goose StatementEnd
