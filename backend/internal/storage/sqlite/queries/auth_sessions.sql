-- Checkpoint 8P-A: auth_sessions. No trailing ORDER BY/LIMIT -- see
-- users.sql's note on the sqlc v1.31.1 SQLite codegen bug; sort/limit in Go.

-- name: InsertAuthSession :one
INSERT INTO auth_sessions (id, user_id, token_hash, created_at, expires_at, last_seen_at, revoked_at, auth_method, issuer, subject)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING id, user_id, token_hash, created_at, expires_at, last_seen_at, revoked_at, auth_method, issuer, subject;

-- name: GetAuthSessionByTokenHash :one
SELECT id, user_id, token_hash, created_at, expires_at, last_seen_at, revoked_at, auth_method, issuer, subject
FROM auth_sessions WHERE token_hash = ?;

-- name: TouchAuthSessionLastSeen :execrows
UPDATE auth_sessions SET last_seen_at = ? WHERE id = ? AND revoked_at IS NULL;

-- name: RevokeAuthSessionByTokenHash :execrows
UPDATE auth_sessions SET revoked_at = ? WHERE token_hash = ? AND revoked_at IS NULL;

-- name: RevokeAllAuthSessionsForUser :execrows
UPDATE auth_sessions SET revoked_at = ? WHERE user_id = ? AND revoked_at IS NULL;
