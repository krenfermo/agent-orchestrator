-- Checkpoint 8P-E.1: widens provider_profiles.auth_state's CHECK constraint
-- to include 'not_installed', distinguishing "the probe couldn't even
-- resolve the CLI binary" from 'unknown' ("binary present, auth state
-- couldn't be determined") -- see service/providerprofile/probe.go.

-- +goose NO TRANSACTION
-- +goose Up
-- +goose StatementBegin
PRAGMA foreign_keys=OFF;

CREATE TABLE provider_profiles_new (
    id                 TEXT PRIMARY KEY,
    user_id            TEXT NOT NULL REFERENCES users (id),
    provider           TEXT NOT NULL,
    harness            TEXT NOT NULL,
    display_name       TEXT NOT NULL,
    enabled            INTEGER NOT NULL DEFAULT 1,
    auth_state         TEXT NOT NULL CHECK (auth_state IN ('unknown', 'authenticated', 'unauthenticated', 'error', 'not_installed')),
    auth_method        TEXT NOT NULL CHECK (auth_method IN ('browser_oauth', 'device_flow', 'cli_bootstrap', 'api_key', 'external_login', 'unsupported')),
    default_model      TEXT,
    capabilities       TEXT NOT NULL DEFAULT '[]',
    secret_ciphertext  BLOB,
    created_at         TIMESTAMP NOT NULL,
    updated_at         TIMESTAMP NOT NULL
);

INSERT INTO provider_profiles_new (id, user_id, provider, harness, display_name, enabled, auth_state, auth_method, default_model, capabilities, secret_ciphertext, created_at, updated_at)
SELECT id, user_id, provider, harness, display_name, enabled, auth_state, auth_method, default_model, capabilities, secret_ciphertext, created_at, updated_at
FROM provider_profiles;

DROP TABLE provider_profiles;
ALTER TABLE provider_profiles_new RENAME TO provider_profiles;

CREATE INDEX idx_provider_profiles_user_id ON provider_profiles (user_id);
CREATE UNIQUE INDEX idx_provider_profiles_user_provider_harness ON provider_profiles (user_id, provider, harness);

PRAGMA foreign_keys=ON;
PRAGMA foreign_key_check;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- SQLite cannot narrow this CHECK safely once not_installed rows may exist.
-- Keep the widened constraint in place, matching the existing best-effort
-- CHECK-widening down migration style (see 0028).
-- +goose StatementEnd
