-- P4-A: federated (OIDC) identities. No trailing ORDER BY/LIMIT on any :many
-- query here -- see users.sql's note on the sqlc v1.31.1 SQLite codegen bug;
-- sort/limit is done in Go in the store layer instead.

-- name: InsertExternalIdentity :one
INSERT INTO external_identities (id, user_id, issuer, subject, email, email_verified, display_name, created_at, updated_at, last_login_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING id, user_id, issuer, subject, email, email_verified, display_name, created_at, updated_at, last_login_at;

-- name: GetExternalIdentityByIssuerSubject :one
SELECT id, user_id, issuer, subject, email, email_verified, display_name, created_at, updated_at, last_login_at
FROM external_identities WHERE issuer = ? AND subject = ?;

-- name: ListExternalIdentitiesForUser :many
SELECT id, user_id, issuer, subject, email, email_verified, display_name, created_at, updated_at, last_login_at
FROM external_identities WHERE user_id = ?;

-- name: UpdateExternalIdentityClaims :execrows
UPDATE external_identities
SET email = ?, email_verified = ?, display_name = ?, updated_at = ?, last_login_at = ?
WHERE id = ?;
