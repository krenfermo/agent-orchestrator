-- Skills phase 8: the trust root for container images.
--
-- ADR 0005 section 4 named the decision nobody had made: who may publish an
-- image this installation will execute. The answer taken here is the most
-- conservative of the three it listed -- explicit administrative approval, per
-- digest and per scope. Not signature verification (AO verifies none), not an
-- AO-operated registry (there is none), and emphatically not "whatever image is
-- on the host", which is what the runner did before this migration and which
-- trusts whoever last ran docker pull.
--
-- What a row means: a named administrator looked at these exact bytes and said
-- this scope may execute them to back this tool. It is NOT a publisher
-- signature and must never be described as one.
--
-- The scope columns are the same five that skill_secret_grants carries, for the
-- same reason: a newer package version is a different manifest asking for
-- different capabilities, and it has to be looked at again.
--
-- This migration also widens skill_audit's action CHECK. The image decisions
-- belong in the audit trail that already answers "who installed this package",
-- because it is the same reviewer asking the same kind of question, and two
-- audit tables would be two places to forget to look. SQLite cannot alter a
-- CHECK in place, so the table is rebuilt; nothing references skill_audit by
-- foreign key, which is what makes the rebuild safe.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE skill_image_approvals (
    id           TEXT      PRIMARY KEY,
    -- The full scope. Every column NOT NULL: an approval missing one of them
    -- authorizes somebody, somewhere, to run something.
    tenant_id    TEXT      NOT NULL,
    project_id   TEXT      NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    skill_id     TEXT      NOT NULL,
    version      TEXT      NOT NULL,
    mode_id      TEXT      NOT NULL,
    -- The AO tool contract the image backs. An image approved for the static
    -- scan is not approved for a future tool with a different command.
    tool         TEXT      NOT NULL,
    -- The repository name, recorded for whoever reads this row. Nothing
    -- resolves through it; the digest is the identity.
    reference    TEXT      NOT NULL,
    -- sha256:<64 lowercase hex>. Immutable by construction. The CHECK is a
    -- backstop under the Go validator, not a substitute for it.
    digest       TEXT      NOT NULL CHECK (
                     length(digest) = 71 AND substr(digest, 1, 7) = 'sha256:'
                 ),
    approved_by  TEXT      NOT NULL,
    approved_at  TIMESTAMP NOT NULL,
    -- Optional, unlike a secret grant's required expiry. A base image is a
    -- long-lived artifact; forcing a date here would train people to set one
    -- far away. Revocation is what makes this safe, and it is immediate for
    -- new runs.
    expires_at   TIMESTAMP,
    revoked_at   TIMESTAMP,
    -- What the administrator says they checked. Required by the Go validator:
    -- an approval with no stated reason is indistinguishable from a mistake.
    note         TEXT      NOT NULL DEFAULT '',
    -- One approval per (scope, tool). Re-approving REPLACES, so "which image
    -- may this scope run" has exactly one answer and a second digest cannot
    -- accumulate quietly beside the first.
    UNIQUE (tenant_id, project_id, skill_id, version, mode_id, tool)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_skill_image_approvals_scope
    ON skill_image_approvals (project_id, skill_id, version, mode_id);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_skill_image_approvals_digest ON skill_image_approvals (digest);
-- +goose StatementEnd

-- Widen the audit vocabulary. SQLite has no ALTER for a CHECK, so this is the
-- documented rebuild: create beside, copy, drop, rename, recreate indexes.
-- Every existing row survives -- the new CHECK is a superset of the old one.
-- +goose StatementBegin
CREATE TABLE skill_audit_new (
    id           TEXT      PRIMARY KEY,
    occurred_at  TIMESTAMP NOT NULL,
    actor        TEXT      NOT NULL DEFAULT '',
    action       TEXT      NOT NULL CHECK (action IN (
                     'install','uninstall','enable','disable','grant_changed','install_rejected',
                     'image_approved','image_revoked','run_executed','run_refused'
                 )),
    skill_id     TEXT      NOT NULL,
    version      TEXT      NOT NULL DEFAULT '',
    project_id   TEXT,
    digest       TEXT      NOT NULL DEFAULT '',
    capabilities TEXT      NOT NULL DEFAULT '[]',
    detail       TEXT      NOT NULL DEFAULT ''
);
-- +goose StatementEnd

-- +goose StatementBegin
INSERT INTO skill_audit_new (
    id, occurred_at, actor, action, skill_id, version, project_id,
    digest, capabilities, detail
)
SELECT id, occurred_at, actor, action, skill_id, version, project_id,
    digest, capabilities, detail
FROM skill_audit;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE skill_audit;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE skill_audit_new RENAME TO skill_audit;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_skill_audit_occurred_at ON skill_audit (occurred_at);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_skill_audit_skill ON skill_audit (skill_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS skill_image_approvals;
-- +goose StatementEnd
