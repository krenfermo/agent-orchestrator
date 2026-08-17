-- Checkpoint 8P-A: application-identity users. AO has been single-tenant
-- until now (no login, no session, everything implicitly owned by whoever
-- runs the daemon). This table is the durable identity record a session
-- cookie resolves to, and the row projects/workflow_runs will point at via
-- their new nullable owner columns (0109).
--
-- password_hash is a bcrypt hash (golang.org/x/crypto/bcrypt), never
-- plaintext -- see service/authsvc. There is no "default admin" row shipped
-- here: the very first admin is created explicitly at daemon startup from
-- AO_BOOTSTRAP_ADMIN_EMAIL/AO_BOOTSTRAP_ADMIN_PASSWORD, or via `ao admin
-- create`, never seeded by a migration.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE users (
    id            TEXT PRIMARY KEY,
    display_name  TEXT NOT NULL,
    email         TEXT NOT NULL,
    username      TEXT NOT NULL,
    password_hash TEXT NOT NULL,
    status        TEXT NOT NULL CHECK (status IN ('active', 'disabled')),
    created_at    TIMESTAMP NOT NULL,
    updated_at    TIMESTAMP NOT NULL
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE UNIQUE INDEX idx_users_email ON users (email);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE UNIQUE INDEX idx_users_username ON users (username);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS users;
-- +goose StatementEnd
