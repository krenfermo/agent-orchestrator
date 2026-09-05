-- P4-C: tenants (organizations). P4-B answered "what may this account do?";
-- this migration answers "inside which organization?", which is the one
-- question the authorization triple (permission, scope, resource) could not
-- express, because every scope it had -- the installation and one project --
-- assumed a single organization owned both.
--
-- WHY A NEW NOUN RATHER THAN REUSING `teams`. A team is a SUBJECT: a bag of
-- users that can hold a project grant, so that granting five people access is
-- one row. A tenant is an AUTHORITY: it OWNS projects and teams, and it is the
-- boundary a person cannot see across. Overloading one table to mean both
-- would make "add Ana to the team" and "give Ana the run of the organization"
-- the same statement, which is exactly the confusion this slice exists to
-- prevent. They are separate tables, and a team belongs to a tenant.
--
-- WHAT CARRIES tenant_id, AND WHAT DELIBERATELY DOES NOT.
--
--   projects.tenant_id  -- yes. A project is the root of everything AO scopes:
--                          sessions, workflow runs, notifications, usage,
--                          memory and the code graph all hang off a project id
--                          and none of them carries an owner of its own. One
--                          explicit column here is what makes tenancy a fact
--                          to read rather than a join to reconstruct.
--   teams.tenant_id     -- yes. A team is tenant-owned: a team in tenant A
--                          holding a grant on a project in tenant B is the
--                          cross-tenant hole this slice must not leave open,
--                          and the cheapest way to close it is to make the
--                          team itself have a home.
--   project_grants      -- NO. A grant's tenancy is its project's tenancy,
--                          full stop. A tenant_id column here could disagree
--                          with projects.tenant_id, and a denormalized copy
--                          that can drift out of step with the authority it
--                          copies is not a safety property, it is a second
--                          thing to keep true. service/rbac validates at write
--                          time that a grant's subject and project share a
--                          tenant; the resolver re-checks at read time.
--   sessions / workflow_runs / notifications / usage / memory / code graph
--                       -- NO, for the same reason: each is already reached
--                          through its project, and authorization resolves
--                          every one of them by asking about that project.
--                          Adding a second tenancy key to each would create
--                          six more chances to disagree with the first.
--
-- THE DEFAULT TENANT. A fixed id, created unconditionally, on a fresh database
-- as well as an existing one. That is what makes the single-user installation
-- -- still the overwhelmingly common one -- keep behaving exactly as it did:
-- one tenant, everything in it, no selector to notice. It also means fresh and
-- migrated databases converge on the same shape, so a test that passes on one
-- is evidence about the other. Backfill maps each existing user's INSTALLATION
-- role onto the same-named tenant role, which preserves every existing
-- authority exactly and widens none: an installation viewer becomes a tenant
-- viewer, not a tenant member.

-- +goose NO TRANSACTION
-- +goose Up
-- +goose StatementBegin
CREATE TABLE tenants (
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
CREATE UNIQUE INDEX ux_tenants_slug ON tenants (slug);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE tenant_memberships (
    id         TEXT PRIMARY KEY,
    tenant_id  TEXT NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    user_id    TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    role       TEXT NOT NULL DEFAULT 'member'
        CHECK (role IN ('owner', 'admin', 'member', 'viewer')),
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE UNIQUE INDEX ux_tenant_memberships_tenant_user
    ON tenant_memberships (tenant_id, user_id);
-- +goose StatementEnd

-- The resolver's hot path: "which tenants is this user in", one indexed lookup
-- per request rather than a scan of every membership in the installation.
-- +goose StatementBegin
CREATE INDEX idx_tenant_memberships_user ON tenant_memberships (user_id);
-- +goose StatementEnd

-- The default tenant. Its id is fixed and its slug is 'default' so that the
-- backfill below, the bootstrap path in Go, and the tests all name the same
-- row without passing an id around.
-- +goose StatementBegin
INSERT INTO tenants (id, name, slug, description, status, created_at, updated_at)
VALUES (
    'tnt_default',
    'Default',
    'default',
    'The organization every project and account created before P4-C belongs to.',
    'active',
    datetime('now'),
    datetime('now')
);
-- +goose StatementEnd

-- projects.tenant_id and teams.tenant_id. ADD COLUMN with a non-NULL default
-- backfills every existing row in the same statement, which is why this is not
-- a table rebuild: `projects` is referenced by a dozen foreign keys and carries
-- the CDC triggers, and rebuilding it to add one column would risk all of that
-- to gain nothing. foreign_keys is toggled off only because SQLite refuses
-- ADD COLUMN with a REFERENCES clause while enforcement is on; the tenant row
-- above already exists, so foreign_key_check below proves the result sound.
-- +goose StatementBegin
PRAGMA foreign_keys=OFF;

ALTER TABLE projects ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'tnt_default'
    REFERENCES tenants (id);

ALTER TABLE teams ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'tnt_default'
    REFERENCES tenants (id);

PRAGMA foreign_keys=ON;
PRAGMA foreign_key_check;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_projects_tenant ON projects (tenant_id);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_teams_tenant ON teams (tenant_id);
-- +goose StatementEnd

-- Membership backfill. Every existing account joins the default tenant at the
-- tenant role that matches the installation role it already holds. A user with
-- a role this build does not recognise joins as a viewer: an unreadable role is
-- not a licence, which is the same rule the resolver applies in Go.
-- +goose StatementBegin
INSERT INTO tenant_memberships (id, tenant_id, user_id, role, created_at, updated_at)
SELECT 'tnm_default_' || u.id,
       'tnt_default',
       u.id,
       CASE u.role
           WHEN 'owner'  THEN 'owner'
           WHEN 'admin'  THEN 'admin'
           WHEN 'member' THEN 'member'
           ELSE 'viewer'
       END,
       datetime('now'),
       datetime('now')
FROM users u;
-- +goose StatementEnd

-- +goose Down
-- Genuinely reversible: the Down drops state that has no meaning without
-- tenants, and touches nothing that existed before the Up. Projects, teams,
-- grants, owners and memberships all survive it unchanged -- a rolled-back
-- installation is single-tenant again, which is precisely what it was.
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_projects_tenant;
-- +goose StatementEnd

-- +goose StatementBegin
DROP INDEX IF EXISTS idx_teams_tenant;
-- +goose StatementEnd

-- +goose StatementBegin
PRAGMA foreign_keys=OFF;

ALTER TABLE projects DROP COLUMN tenant_id;
ALTER TABLE teams DROP COLUMN tenant_id;

PRAGMA foreign_keys=ON;
PRAGMA foreign_key_check;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE IF EXISTS tenant_memberships;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE IF EXISTS tenants;
-- +goose StatementEnd
