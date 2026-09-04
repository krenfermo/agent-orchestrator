-- P4-A: OIDC/SSO. Three durable facts the existing password-only identity
-- layer (0107/0108/0116) has nowhere to put:
--
--  1. external_identities -- the canonical external identity, (issuer, sub),
--     NEVER email. Email can change at the provider and may be unverified, so
--     it is stored as a claim snapshot for display and never as the join key.
--     A user may hold several (one per issuer); a given (issuer, sub) belongs
--     to exactly one user, enforced by the unique index rather than by
--     application code.
--
--  2. oidc_login_flows -- one row per in-flight Authorization Code request,
--     holding the state (this row's id), nonce, and PKCE code_verifier the
--     callback must check against. Durable rather than in-memory so a daemon
--     restart mid-login fails closed on a missing flow instead of on a lost
--     map, and so single-use consumption is a SQL fact (consumed_at) rather
--     than a lock. Nothing secret from the provider is stored here: no
--     authorization code, no access/refresh token, no id_token.
--
--     authenticated_user_id is stamped by the callback once every check has
--     passed. The DESKTOP flow then mints its AO session at pickup time, so
--     the raw session token still never rests in the database -- only its
--     SHA-256 does, in auth_sessions, exactly as the password flow already
--     does. handoff_secret_hash is the SHA-256 of a secret the desktop
--     supervisor generated and kept on loopback; it never travels to the
--     provider, so possession of the state value alone claims nothing.
--
--  3. auth_sessions.auth_method/issuer/subject -- a session now records HOW it
--     was authenticated. Existing rows backfill to 'password', which is what
--     every session predating this migration is.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE external_identities (
    id             TEXT PRIMARY KEY,
    user_id        TEXT NOT NULL REFERENCES users (id),
    issuer         TEXT NOT NULL,
    subject        TEXT NOT NULL,
    email          TEXT NOT NULL DEFAULT '',
    email_verified INTEGER NOT NULL DEFAULT 0,
    display_name   TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMP NOT NULL,
    updated_at     TIMESTAMP NOT NULL,
    last_login_at  TIMESTAMP
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE UNIQUE INDEX ux_external_identities_issuer_subject
    ON external_identities (issuer, subject);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_external_identities_user_id ON external_identities (user_id);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE oidc_login_flows (
    id                    TEXT PRIMARY KEY,
    nonce                 TEXT NOT NULL,
    code_verifier         TEXT NOT NULL,
    redirect_uri          TEXT NOT NULL,
    return_to             TEXT NOT NULL DEFAULT '',
    client_kind           TEXT NOT NULL CHECK (client_kind IN ('browser', 'desktop')),
    handoff_secret_hash   TEXT NOT NULL DEFAULT '',
    authenticated_user_id TEXT REFERENCES users (id),
    authenticated_at      TIMESTAMP,
    created_at            TIMESTAMP NOT NULL,
    expires_at            TIMESTAMP NOT NULL,
    consumed_at           TIMESTAMP
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_oidc_login_flows_expires_at ON oidc_login_flows (expires_at);
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE auth_sessions ADD COLUMN auth_method TEXT NOT NULL DEFAULT 'password';
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE auth_sessions ADD COLUMN issuer TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE auth_sessions ADD COLUMN subject TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS oidc_login_flows;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE IF EXISTS external_identities;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE auth_sessions DROP COLUMN subject;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE auth_sessions DROP COLUMN issuer;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE auth_sessions DROP COLUMN auth_method;
-- +goose StatementEnd
