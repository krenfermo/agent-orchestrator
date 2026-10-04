-- Skills phase 8: image approvals, the trust root for what AO may execute.
-- No trailing ORDER BY/LIMIT -- see users.sql's note on the sqlc v1.31.1 SQLite
-- codegen bug; sort in Go.

-- name: UpsertSkillImageApproval :one
INSERT INTO skill_image_approvals (
    id, tenant_id, project_id, skill_id, version, mode_id, tool,
    reference, digest, approved_by, approved_at, expires_at, revoked_at, note
)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (tenant_id, project_id, skill_id, version, mode_id, tool) DO UPDATE SET
    reference = excluded.reference,
    digest = excluded.digest,
    approved_by = excluded.approved_by,
    approved_at = excluded.approved_at,
    expires_at = excluded.expires_at,
    revoked_at = excluded.revoked_at,
    note = excluded.note
RETURNING id, tenant_id, project_id, skill_id, version, mode_id, tool,
    reference, digest, approved_by, approved_at, expires_at, revoked_at, note;

-- name: GetSkillImageApprovalForScope :one
SELECT id, tenant_id, project_id, skill_id, version, mode_id, tool,
    reference, digest, approved_by, approved_at, expires_at, revoked_at, note
FROM skill_image_approvals
WHERE tenant_id = ? AND project_id = ? AND skill_id = ? AND version = ?
    AND mode_id = ? AND tool = ?;

-- name: GetSkillImageApprovalByID :one
SELECT id, tenant_id, project_id, skill_id, version, mode_id, tool,
    reference, digest, approved_by, approved_at, expires_at, revoked_at, note
FROM skill_image_approvals WHERE id = ?;

-- name: ListSkillImageApprovals :many
SELECT id, tenant_id, project_id, skill_id, version, mode_id, tool,
    reference, digest, approved_by, approved_at, expires_at, revoked_at, note
FROM skill_image_approvals;

-- name: ListSkillImageApprovalsForProject :many
SELECT id, tenant_id, project_id, skill_id, version, mode_id, tool,
    reference, digest, approved_by, approved_at, expires_at, revoked_at, note
FROM skill_image_approvals WHERE project_id = ?;

-- name: RevokeSkillImageApproval :execrows
UPDATE skill_image_approvals SET revoked_at = ?
WHERE id = ? AND revoked_at IS NULL;
