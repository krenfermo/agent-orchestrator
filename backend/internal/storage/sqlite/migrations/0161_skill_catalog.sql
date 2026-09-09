-- Skills phase 2: the durable skill catalog.
--
-- Phase 1 (internal/skillcatalog, ADR 0003) shipped the manifest contract, the
-- capability model and a file-backed registry. The file was deliberate: it let
-- the catalog ship and be tested without adding a migration while the workflow
-- schema was in flight. It is the wrong home for the finished feature -- a JSON
-- file has no transaction with the rest of AO, no foreign key to projects, and
-- no place to record who granted what.
--
-- Three tables, because three different questions are being answered:
--
--   skill_installs      what packages exist on this installation
--   skill_activations   which projects enabled which pinned version, with what grant
--   skill_audit         who did each of those things, and when
--
-- The package FILES stay on disk under <dataDir>/skills/catalog/packages. The
-- row carries the manifest and the digest that was verified at install time;
-- resolution re-reads the files and re-verifies the digest, so a package edited
-- underneath AO fails closed rather than resolving from a stale row.
--
-- Installs are installation-wide, activations are per project. That asymmetry
-- is the decision: a package is an artifact an administrator vetted once, and
-- reach is granted per project on top of it. Tenant isolation therefore rides
-- on project access -- every activation route resolves through the same
-- project authorization as the rest of AO, so a member of one organization
-- cannot see or change another's activations. There is deliberately no
-- tenant_id column: a second scope here would be a second answer to a question
-- projects already answer.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE skill_installs (
    -- (id, version) is the identity. Versions live side by side so a project
    -- pinned to an older one keeps working after a newer one is installed.
    skill_id      TEXT      NOT NULL,
    version       TEXT      NOT NULL,
    name          TEXT      NOT NULL,
    description   TEXT      NOT NULL DEFAULT '',
    risk_level    TEXT      NOT NULL CHECK (risk_level IN ('low','medium','high','critical')),
    origin_type   TEXT      NOT NULL CHECK (origin_type IN ('builtin','local','git')),
    origin_ref    TEXT      NOT NULL DEFAULT '',
    -- The content digest verified at install time, over every package file
    -- except the manifest itself (which carries it).
    digest        TEXT      NOT NULL,
    -- The validated manifest as JSON. Stored so a list or a detail view needs
    -- no disk read, and so a package whose files went missing can still be
    -- reported and uninstalled rather than becoming unmanageable.
    manifest_json TEXT      NOT NULL,
    -- Absolute path of the installed package root.
    package_dir   TEXT      NOT NULL,
    installed_at  TIMESTAMP NOT NULL,
    installed_by  TEXT      NOT NULL DEFAULT '',
    PRIMARY KEY (skill_id, version)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE skill_activations (
    project_id   TEXT      NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    skill_id     TEXT      NOT NULL,
    -- The PINNED version. There is no "latest" activation: installing a newer
    -- package must not silently change what a project already approved.
    version      TEXT      NOT NULL,
    enabled      INTEGER   NOT NULL DEFAULT 0 CHECK (enabled IN (0,1)),
    -- JSON array of granted capability names. Empty for a disabled row:
    -- disabling revokes the grant rather than parking it for silent reuse, so
    -- re-enabling means granting again.
    capabilities TEXT      NOT NULL DEFAULT '[]',
    approved_by  TEXT      NOT NULL DEFAULT '',
    approved_at  TIMESTAMP,
    updated_at   TIMESTAMP NOT NULL,
    -- One activation per (project, skill). A project can never hold two
    -- different grants for the same skill.
    PRIMARY KEY (project_id, skill_id),
    -- An activation cannot outlive the exact version it pinned. This is what
    -- makes "uninstall refuses while enabled" an invariant of the schema and
    -- not only of the service that usually enforces it.
    FOREIGN KEY (skill_id, version) REFERENCES skill_installs (skill_id, version)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_skill_activations_skill ON skill_activations (skill_id, version);
-- +goose StatementEnd

-- +goose StatementBegin
-- The audit trail for the catalog. It is append-only and deliberately NOT
-- foreign-keyed to skill_installs: the most interesting rows are the ones about
-- a package that has since been uninstalled, and a cascade would delete exactly
-- the history somebody would want to read.
CREATE TABLE skill_audit (
    id           TEXT      PRIMARY KEY,
    occurred_at  TIMESTAMP NOT NULL,
    -- Who did it. Empty means an unauthenticated local caller on the loopback
    -- listener, which is a real and recordable case, not a missing value.
    actor        TEXT      NOT NULL DEFAULT '',
    action       TEXT      NOT NULL CHECK (action IN (
                     'install','uninstall','enable','disable','grant_changed','install_rejected'
                 )),
    skill_id     TEXT      NOT NULL,
    version      TEXT      NOT NULL DEFAULT '',
    project_id   TEXT,
    digest       TEXT      NOT NULL DEFAULT '',
    -- JSON array of the capabilities in effect after this event.
    capabilities TEXT      NOT NULL DEFAULT '[]',
    -- Human-readable reason, used mainly by install_rejected.
    detail       TEXT      NOT NULL DEFAULT ''
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_skill_audit_occurred_at ON skill_audit (occurred_at);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_skill_audit_skill ON skill_audit (skill_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS skill_audit;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE IF EXISTS skill_activations;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE IF EXISTS skill_installs;
-- +goose StatementEnd
