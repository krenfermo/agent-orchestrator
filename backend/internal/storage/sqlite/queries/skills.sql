-- Skills phase 2: catalog persistence. No trailing ORDER BY/LIMIT -- see
-- users.sql's note on the sqlc v1.31.1 SQLite codegen bug; sort in Go.

-- name: UpsertSkillInstall :one
INSERT INTO skill_installs (
    skill_id, version, name, description, risk_level,
    origin_type, origin_ref, digest, manifest_json, package_dir,
    installed_at, installed_by
)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (skill_id, version) DO UPDATE SET
    name = excluded.name,
    description = excluded.description,
    risk_level = excluded.risk_level,
    origin_type = excluded.origin_type,
    origin_ref = excluded.origin_ref,
    digest = excluded.digest,
    manifest_json = excluded.manifest_json,
    package_dir = excluded.package_dir,
    installed_at = excluded.installed_at,
    installed_by = excluded.installed_by
RETURNING skill_id, version, name, description, risk_level,
    origin_type, origin_ref, digest, manifest_json, package_dir,
    installed_at, installed_by;

-- name: GetSkillInstall :one
SELECT skill_id, version, name, description, risk_level,
    origin_type, origin_ref, digest, manifest_json, package_dir,
    installed_at, installed_by
FROM skill_installs WHERE skill_id = ? AND version = ?;

-- name: ListSkillInstalls :many
SELECT skill_id, version, name, description, risk_level,
    origin_type, origin_ref, digest, manifest_json, package_dir,
    installed_at, installed_by
FROM skill_installs;

-- name: ListSkillInstallVersions :many
SELECT skill_id, version, name, description, risk_level,
    origin_type, origin_ref, digest, manifest_json, package_dir,
    installed_at, installed_by
FROM skill_installs WHERE skill_id = ?;

-- name: DeleteSkillInstall :execrows
DELETE FROM skill_installs WHERE skill_id = ? AND version = ?;

-- name: UpsertSkillActivation :one
INSERT INTO skill_activations (
    project_id, skill_id, version, enabled, capabilities,
    approved_by, approved_at, updated_at
)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (project_id, skill_id) DO UPDATE SET
    version = excluded.version,
    enabled = excluded.enabled,
    capabilities = excluded.capabilities,
    approved_by = excluded.approved_by,
    approved_at = excluded.approved_at,
    updated_at = excluded.updated_at
RETURNING project_id, skill_id, version, enabled, capabilities,
    approved_by, approved_at, updated_at;

-- name: GetSkillActivation :one
SELECT project_id, skill_id, version, enabled, capabilities,
    approved_by, approved_at, updated_at
FROM skill_activations WHERE project_id = ? AND skill_id = ?;

-- name: ListSkillActivationsForProject :many
SELECT project_id, skill_id, version, enabled, capabilities,
    approved_by, approved_at, updated_at
FROM skill_activations WHERE project_id = ?;

-- name: ListSkillActivationsForSkillVersion :many
SELECT project_id, skill_id, version, enabled, capabilities,
    approved_by, approved_at, updated_at
FROM skill_activations WHERE skill_id = ? AND version = ?;

-- name: DeleteSkillActivation :execrows
DELETE FROM skill_activations WHERE project_id = ? AND skill_id = ?;

-- name: InsertSkillAuditEntry :exec
INSERT INTO skill_audit (
    id, occurred_at, actor, action, skill_id, version,
    project_id, digest, capabilities, detail
)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: ListSkillAuditForSkill :many
SELECT id, occurred_at, actor, action, skill_id, version,
    project_id, digest, capabilities, detail
FROM skill_audit WHERE skill_id = ?;

-- name: ListSkillAuditForProject :many
SELECT id, occurred_at, actor, action, skill_id, version,
    project_id, digest, capabilities, detail
FROM skill_audit WHERE project_id = ?;
