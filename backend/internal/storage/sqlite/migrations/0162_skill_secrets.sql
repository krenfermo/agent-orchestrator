-- Skills phase 5: scoped secret delivery.
--
-- ADR 0005 designed this control and did not build it. What it needs is not a
-- vault -- AO is not becoming one -- but the two things a vault would give and
-- that nothing in AO currently has: a place to keep a named secret sealed, and
-- a grant narrow enough to answer "who may read this, on what project, running
-- which package version in which mode, until when".
--
-- Values are sealed with internal/secretbox before they reach a row, the same
-- way an SMTP password already is. The ciphertext column never holds
-- plaintext, and the Go type that carries a decrypted value redacts itself in
-- String and MarshalJSON so it cannot reach a log or a report by accident.
--
-- Three tables:
--
--   skill_secrets        the sealed value, one row per name
--   skill_secret_grants  who may read it, bound to a full scope, with expiry
--   skill_secret_leases  one attempt's single-use right to redeem those grants
--
-- The lease table is what makes replay answerable. A grant says "this scope may
-- read SENTRY_DSN until Friday"; a lease says "attempt a7 of run r3 may redeem
-- it once, in the next two minutes". An earlier attempt cannot use a later
-- attempt's lease, and a consumed lease cannot be redeemed twice.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE skill_secrets (
    -- The reference. It is a NAME and is safe to log; the value is not here in
    -- any readable form.
    name          TEXT      PRIMARY KEY,
    description   TEXT      NOT NULL DEFAULT '',
    -- secretbox ciphertext. Never plaintext, never a hash or prefix of one:
    -- a prefix is enough to confirm a guess.
    sealed_value  TEXT      NOT NULL,
    created_at    TIMESTAMP NOT NULL,
    created_by    TEXT      NOT NULL DEFAULT '',
    updated_at    TIMESTAMP NOT NULL
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE skill_secret_grants (
    id          TEXT      PRIMARY KEY,
    secret_name TEXT      NOT NULL REFERENCES skill_secrets (name) ON DELETE CASCADE,
    -- The full scope. Every column is NOT NULL on purpose: a grant missing one
    -- of them is a grant to somebody, somewhere, running something.
    tenant_id   TEXT      NOT NULL,
    project_id  TEXT      NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    skill_id    TEXT      NOT NULL,
    version     TEXT      NOT NULL,
    mode_id     TEXT      NOT NULL,
    granted_by  TEXT      NOT NULL,
    granted_at  TIMESTAMP NOT NULL,
    -- Required. A grant that never expires is one nobody removes.
    expires_at  TIMESTAMP NOT NULL,
    revoked_at  TIMESTAMP,
    -- One grant per (secret, scope). Re-granting replaces rather than
    -- accumulating, so "what may this scope read" has one answer.
    UNIQUE (secret_name, tenant_id, project_id, skill_id, version, mode_id)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_skill_secret_grants_scope
    ON skill_secret_grants (project_id, skill_id, version, mode_id);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE skill_secret_leases (
    id          TEXT      PRIMARY KEY,
    tenant_id   TEXT      NOT NULL,
    project_id  TEXT      NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    skill_id    TEXT      NOT NULL,
    version     TEXT      NOT NULL,
    mode_id     TEXT      NOT NULL,
    -- The execution this lease belongs to. Both are required: a lease bound to
    -- a run but not an attempt would let a retried attempt redeem the previous
    -- one's right.
    run_id      TEXT      NOT NULL,
    attempt_id  TEXT      NOT NULL,
    -- JSON array of the secret names this lease covers.
    refs        TEXT      NOT NULL,
    issued_at   TIMESTAMP NOT NULL,
    expires_at  TIMESTAMP NOT NULL,
    -- Stamped on delivery. A lease is redeemable exactly once.
    consumed_at TIMESTAMP,
    UNIQUE (run_id, attempt_id)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_skill_secret_leases_expires ON skill_secret_leases (expires_at);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS skill_secret_leases;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE IF EXISTS skill_secret_grants;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE IF EXISTS skill_secrets;
-- +goose StatementEnd
