-- P4-I: agent credentials.
--
-- The defect this closes: under AO_AUTH_MODE=oidc every permission-gated route
-- resolves its principal from the ao_session cookie, and an AGENT AO LAUNCHED
-- ITSELF holds no cookie and has no way to obtain one -- there is no browser in
-- a tmux pane. So `ao review submit` from inside a reviewer answered 401
-- NOT_AUTHENTICATED, the verdict never reached AO, the review_run stayed
-- 'running', and thirty minutes later the staleness threshold parked the whole
-- workflow on review_state_ambiguous. AO refused to record the review its own
-- reviewer had finished, then reported that it could not prove what the review
-- concluded. (Observed on wf-98ab416c-fddd-4c1e-98b9-89a7e3eaae00.)
--
-- The two shortcuts are both refused on purpose:
--
--   * exempting reviews/submit from authorization the way /reviews/{id}/activity
--     is exempted. Activity is a heartbeat; a verdict decides whether work
--     merges. An unauthenticated write of a verdict is a worse defect than the
--     one it fixes.
--   * letting the agent present the operator's own CLI credential. It works,
--     and it silently promotes every agent to whatever the operator may do --
--     across every project, in every organization, for as long as that session
--     lives.
--
-- What this table stores instead is a credential that is FOR the agent: minted
-- by the daemon at launch, acting on behalf of the run's owner but capped by the
-- agent's ROLE, and bound to the one project, session, workflow run and step the
-- launch was for. RBAC and tenancy are untouched -- an agent's authority is the
-- INTERSECTION of this row and what user_id may do, so it can never exceed the
-- person it works for and is normally far below them.
--
-- The raw token never rests here, only its SHA-256, exactly as auth_sessions has
-- done since 0107. Rows are expiring and revocable, and a revocation names one
-- runtime incarnation rather than a reusable session name.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE agent_credentials (
    id                  TEXT PRIMARY KEY,
    token_hash          TEXT NOT NULL,
    role                TEXT NOT NULL CHECK (role IN ('reviewer','worker')),
    user_id             TEXT NOT NULL REFERENCES users (id),
    project_id          TEXT NOT NULL,
    session_id          TEXT NOT NULL DEFAULT '',
    workflow_run_id     TEXT NOT NULL DEFAULT '',
    workflow_step_id    TEXT NOT NULL DEFAULT '',
    review_run_id       TEXT NOT NULL DEFAULT '',
    runtime_handle      TEXT NOT NULL DEFAULT '',
    runtime_instance_id TEXT NOT NULL DEFAULT '',
    generation          INTEGER NOT NULL DEFAULT 0,
    permissions         TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(permissions)),
    created_at          TIMESTAMP NOT NULL,
    expires_at          TIMESTAMP NOT NULL,
    last_seen_at        TIMESTAMP NOT NULL,
    revoked_at          TIMESTAMP
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE UNIQUE INDEX ux_agent_credentials_token_hash ON agent_credentials (token_hash);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_agent_credentials_review_run ON agent_credentials (review_run_id)
    WHERE review_run_id != '';
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_agent_credentials_session ON agent_credentials (session_id)
    WHERE session_id != '';
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_agent_credentials_expires_at ON agent_credentials (expires_at);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS agent_credentials;
-- +goose StatementEnd
