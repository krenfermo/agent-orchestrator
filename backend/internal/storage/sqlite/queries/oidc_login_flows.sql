-- P4-A: in-flight OIDC Authorization Code requests. The row id IS the `state`
-- parameter, so a callback that names an unknown/expired/consumed state finds
-- nothing and fails closed. No trailing ORDER BY/LIMIT -- see users.sql's note
-- on the sqlc v1.31.1 SQLite codegen bug.

-- name: InsertOIDCLoginFlow :one
INSERT INTO oidc_login_flows (id, nonce, code_verifier, redirect_uri, return_to, client_kind, handoff_secret_hash, authenticated_user_id, authenticated_at, created_at, expires_at, consumed_at)
VALUES (?, ?, ?, ?, ?, ?, ?, NULL, NULL, ?, ?, NULL)
RETURNING id, nonce, code_verifier, redirect_uri, return_to, client_kind, handoff_secret_hash, authenticated_user_id, authenticated_at, created_at, expires_at, consumed_at;

-- name: GetOIDCLoginFlow :one
SELECT id, nonce, code_verifier, redirect_uri, return_to, client_kind, handoff_secret_hash, authenticated_user_id, authenticated_at, created_at, expires_at, consumed_at
FROM oidc_login_flows WHERE id = ?;

-- name: MarkOIDCLoginFlowAuthenticated :execrows
UPDATE oidc_login_flows
SET authenticated_user_id = ?, authenticated_at = ?
WHERE id = ? AND consumed_at IS NULL AND authenticated_user_id IS NULL;

-- name: ConsumeOIDCLoginFlow :execrows
UPDATE oidc_login_flows SET consumed_at = ? WHERE id = ? AND consumed_at IS NULL;

-- name: DeleteExpiredOIDCLoginFlows :execrows
DELETE FROM oidc_login_flows WHERE expires_at < ?;
