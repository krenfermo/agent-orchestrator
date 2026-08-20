-- Checkpoint 8P-E.8: adds users.role, distinguishing the installation
-- owner/admin from ordinary members. Existing rows backfill to 'member';
-- the daemon's own startup/registration path is responsible for promoting
-- exactly one row to 'owner' (see service/authsvc.Bootstrap/RegisterFirstUser),
-- never this migration.
--
-- ux_users_single_owner is the concurrency-safety mechanism for first-run
-- account creation: two simultaneous "create the first account" requests
-- both attempting to insert a role='owner' row race at the SQL layer, and
-- exactly one wins -- no application-level locking required.

-- +goose NO TRANSACTION
-- +goose Up
-- +goose StatementBegin
PRAGMA foreign_keys=OFF;

CREATE TABLE users_new (
    id            TEXT PRIMARY KEY,
    display_name  TEXT NOT NULL,
    email         TEXT NOT NULL,
    username      TEXT NOT NULL,
    password_hash TEXT NOT NULL,
    status        TEXT NOT NULL CHECK (status IN ('active', 'disabled')),
    role          TEXT NOT NULL DEFAULT 'member' CHECK (role IN ('owner', 'member')),
    created_at    TIMESTAMP NOT NULL,
    updated_at    TIMESTAMP NOT NULL
);

INSERT INTO users_new (id, display_name, email, username, password_hash, status, role, created_at, updated_at)
SELECT id, display_name, email, username, password_hash, status, 'member', created_at, updated_at
FROM users;

DROP TABLE users;
ALTER TABLE users_new RENAME TO users;

CREATE UNIQUE INDEX idx_users_email ON users (email);
CREATE UNIQUE INDEX idx_users_username ON users (username);
CREATE UNIQUE INDEX ux_users_single_owner ON users (role) WHERE role = 'owner';

PRAGMA foreign_keys=ON;
PRAGMA foreign_key_check;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
PRAGMA foreign_keys=OFF;

CREATE TABLE users_old (
    id            TEXT PRIMARY KEY,
    display_name  TEXT NOT NULL,
    email         TEXT NOT NULL,
    username      TEXT NOT NULL,
    password_hash TEXT NOT NULL,
    status        TEXT NOT NULL CHECK (status IN ('active', 'disabled')),
    created_at    TIMESTAMP NOT NULL,
    updated_at    TIMESTAMP NOT NULL
);

INSERT INTO users_old (id, display_name, email, username, password_hash, status, created_at, updated_at)
SELECT id, display_name, email, username, password_hash, status, created_at, updated_at
FROM users;

DROP TABLE users;
ALTER TABLE users_old RENAME TO users;

CREATE UNIQUE INDEX idx_users_email ON users (email);
CREATE UNIQUE INDEX idx_users_username ON users (username);

PRAGMA foreign_keys=ON;
PRAGMA foreign_key_check;
-- +goose StatementEnd
