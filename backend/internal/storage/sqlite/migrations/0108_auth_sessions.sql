-- Checkpoint 8P-A: session tokens for cookie-based application identity.
-- The raw session token is handed to the client exactly once, in the
-- Set-Cookie response at login -- this table only ever stores its SHA-256
-- hash (token_hash), mirroring the password-hash-not-plaintext discipline
-- users.password_hash already follows. ResolveSession hashes the incoming
-- cookie value and looks it up here; there is no way to recover a usable
-- token from a compromised DB alone.
--
-- revoked_at is NULLABLE and set on logout; expired-but-unrevoked rows are
-- simply rows whose expires_at has passed -- ResolveSession checks both.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE auth_sessions (
    id            TEXT PRIMARY KEY,
    user_id       TEXT NOT NULL REFERENCES users (id),
    token_hash    TEXT NOT NULL,
    created_at    TIMESTAMP NOT NULL,
    expires_at    TIMESTAMP NOT NULL,
    last_seen_at  TIMESTAMP NOT NULL,
    revoked_at    TIMESTAMP
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE UNIQUE INDEX idx_auth_sessions_token_hash ON auth_sessions (token_hash);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_auth_sessions_user_id ON auth_sessions (user_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS auth_sessions;
-- +goose StatementEnd
