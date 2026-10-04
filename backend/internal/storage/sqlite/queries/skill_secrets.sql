-- Skills phase 5: scoped secret delivery. No trailing ORDER BY/LIMIT -- see
-- users.sql's note on the sqlc v1.31.1 SQLite codegen bug; sort in Go.

-- name: UpsertSkillSecret :one
INSERT INTO skill_secrets (name, description, sealed_value, created_at, created_by, updated_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (name) DO UPDATE SET
    description = excluded.description,
    sealed_value = excluded.sealed_value,
    updated_at = excluded.updated_at
RETURNING name, description, sealed_value, created_at, created_by, updated_at;

-- name: GetSkillSecret :one
SELECT name, description, sealed_value, created_at, created_by, updated_at
FROM skill_secrets WHERE name = ?;

-- name: ListSkillSecretNames :many
SELECT name, description, created_at, created_by, updated_at FROM skill_secrets;

-- name: DeleteSkillSecret :execrows
DELETE FROM skill_secrets WHERE name = ?;

-- name: UpsertSkillSecretGrant :one
INSERT INTO skill_secret_grants (
    id, secret_name, tenant_id, project_id, skill_id, version, mode_id,
    granted_by, granted_at, expires_at, revoked_at
)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (secret_name, tenant_id, project_id, skill_id, version, mode_id) DO UPDATE SET
    granted_by = excluded.granted_by,
    granted_at = excluded.granted_at,
    expires_at = excluded.expires_at,
    revoked_at = excluded.revoked_at
RETURNING id, secret_name, tenant_id, project_id, skill_id, version, mode_id,
    granted_by, granted_at, expires_at, revoked_at;

-- name: ListSkillSecretGrantsForScope :many
SELECT id, secret_name, tenant_id, project_id, skill_id, version, mode_id,
    granted_by, granted_at, expires_at, revoked_at
FROM skill_secret_grants
WHERE tenant_id = ? AND project_id = ? AND skill_id = ? AND version = ? AND mode_id = ?;

-- name: ListSkillSecretGrantsForSecret :many
SELECT id, secret_name, tenant_id, project_id, skill_id, version, mode_id,
    granted_by, granted_at, expires_at, revoked_at
FROM skill_secret_grants WHERE secret_name = ?;

-- name: RevokeSkillSecretGrant :execrows
UPDATE skill_secret_grants SET revoked_at = ?
WHERE id = ? AND revoked_at IS NULL;

-- name: RevokeSkillSecretGrantsForSecret :execrows
UPDATE skill_secret_grants SET revoked_at = ?
WHERE secret_name = ? AND revoked_at IS NULL;

-- name: InsertSkillSecretLease :one
INSERT INTO skill_secret_leases (
    id, tenant_id, project_id, skill_id, version, mode_id,
    run_id, attempt_id, refs, issued_at, expires_at, consumed_at
)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING id, tenant_id, project_id, skill_id, version, mode_id,
    run_id, attempt_id, refs, issued_at, expires_at, consumed_at;

-- name: GetSkillSecretLease :one
SELECT id, tenant_id, project_id, skill_id, version, mode_id,
    run_id, attempt_id, refs, issued_at, expires_at, consumed_at
FROM skill_secret_leases WHERE id = ?;

-- name: ConsumeSkillSecretLease :execrows
UPDATE skill_secret_leases SET consumed_at = ?
WHERE id = ? AND consumed_at IS NULL AND expires_at > ?;

-- name: DeleteExpiredSkillSecretLeases :execrows
DELETE FROM skill_secret_leases WHERE expires_at <= ?;
