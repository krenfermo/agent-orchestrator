-- P4-B: users / teams / RBAC. P4-A answered "who authenticated?"; these four
-- schema changes are what let AO answer "what may they do?" durably, instead
-- of the ownership-equality checks scattered across controllers today.
--
--  1. users.role gains 'admin' and 'viewer'. 0116 shipped the closed set
--     ('owner','member'), which can express "the installation owner" and
--     "everyone else" and nothing in between -- no delegated administrator,
--     no read-only account. Existing rows keep their exact value; the
--     ux_users_single_owner partial unique index is recreated unchanged, so
--     the single-owner invariant (and the race-safety it buys first-run
--     account creation) survives verbatim. This is a table rebuild because
--     SQLite cannot alter a CHECK constraint in place, following 0116's own
--     pattern; users carries no CDC triggers, so nothing else is lost with it.
--
--  2. teams / team_memberships -- first-class durable groups. A team is a
--     subject that can hold project access, so that granting five people
--     access to a repository is one row rather than five that drift apart.
--     memberships.role ('maintainer','member') is recorded for display and
--     for a later slice to delegate team administration; P4-B itself gates
--     every team mutation on the global teams.manage permission, so no
--     authorization decision reads it yet.
--
--  3. project_grants -- the project-scoped access edge, deliberately
--     polymorphic in its SUBJECT (a user or a team) and singular in its
--     OBJECT (one project). Both halves matter: one table means one place to
--     ask "who can reach this project", and one place for P4-C to add an
--     organization column beside project_id rather than a parallel table.
--
--     The unique index is on (project_id, subject_kind, subject_id), so a
--     subject holds at most one role per project and re-granting is an
--     upsert rather than a silent duplicate.
--
-- Deliberately NOT here: any tenant/organization id. P4-C introduces that
-- scope; adding a column now that every row would set to the same value
-- would be a migration to undo, not one to build on. What P4-B does commit
-- to is the shape that makes it cheap: authorization already resolves
-- through a (permission, scope, resource) triple, so a third scope is a new
-- case, not a rewrite.

-- +goose NO TRANSACTION
-- +goose Up
-- +goose StatementBegin
PRAGMA foreign_keys=OFF;

CREATE TABLE users_p4b (
    id            TEXT PRIMARY KEY,
    display_name  TEXT NOT NULL,
    email         TEXT NOT NULL,
    username      TEXT NOT NULL,
    password_hash TEXT NOT NULL,
    status        TEXT NOT NULL CHECK (status IN ('active', 'disabled')),
    role          TEXT NOT NULL DEFAULT 'member' CHECK (role IN ('owner', 'admin', 'member', 'viewer')),
    created_at    TIMESTAMP NOT NULL,
    updated_at    TIMESTAMP NOT NULL
);

INSERT INTO users_p4b (id, display_name, email, username, password_hash, status, role, created_at, updated_at)
SELECT id, display_name, email, username, password_hash, status, role, created_at, updated_at
FROM users;

DROP TABLE users;
ALTER TABLE users_p4b RENAME TO users;

CREATE UNIQUE INDEX idx_users_email ON users (email);
CREATE UNIQUE INDEX idx_users_username ON users (username);
CREATE UNIQUE INDEX ux_users_single_owner ON users (role) WHERE role = 'owner';

PRAGMA foreign_keys=ON;
PRAGMA foreign_key_check;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE teams (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    slug        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    status      TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'archived')),
    created_at  TIMESTAMP NOT NULL,
    updated_at  TIMESTAMP NOT NULL
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE UNIQUE INDEX ux_teams_slug ON teams (slug);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE team_memberships (
    id         TEXT PRIMARY KEY,
    team_id    TEXT NOT NULL REFERENCES teams (id) ON DELETE CASCADE,
    user_id    TEXT NOT NULL REFERENCES users (id),
    role       TEXT NOT NULL DEFAULT 'member' CHECK (role IN ('maintainer', 'member')),
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE UNIQUE INDEX ux_team_memberships_team_user ON team_memberships (team_id, user_id);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_team_memberships_user ON team_memberships (user_id);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE project_grants (
    id           TEXT PRIMARY KEY,
    project_id   TEXT NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    subject_kind TEXT NOT NULL CHECK (subject_kind IN ('user', 'team')),
    subject_id   TEXT NOT NULL,
    role         TEXT NOT NULL CHECK (role IN ('admin', 'member', 'viewer')),
    created_at   TIMESTAMP NOT NULL,
    updated_at   TIMESTAMP NOT NULL,
    created_by   TEXT NOT NULL DEFAULT ''
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE UNIQUE INDEX ux_project_grants_subject
    ON project_grants (project_id, subject_kind, subject_id);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_project_grants_subject ON project_grants (subject_kind, subject_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS project_grants;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE IF EXISTS team_memberships;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE IF EXISTS teams;
-- +goose StatementEnd

-- +goose StatementBegin
PRAGMA foreign_keys=OFF;

CREATE TABLE users_p4b_down (
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

INSERT INTO users_p4b_down (id, display_name, email, username, password_hash, status, role, created_at, updated_at)
SELECT id, display_name, email, username, password_hash, status,
       CASE WHEN role IN ('owner', 'member') THEN role ELSE 'member' END,
       created_at, updated_at
FROM users;

DROP TABLE users;
ALTER TABLE users_p4b_down RENAME TO users;

CREATE UNIQUE INDEX idx_users_email ON users (email);
CREATE UNIQUE INDEX idx_users_username ON users (username);
CREATE UNIQUE INDEX ux_users_single_owner ON users (role) WHERE role = 'owner';

PRAGMA foreign_keys=ON;
PRAGMA foreign_key_check;
-- +goose StatementEnd
