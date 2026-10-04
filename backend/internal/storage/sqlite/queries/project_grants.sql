-- P4-B: project-scoped access grants. A grant's subject is a user or a team;
-- its object is exactly one project. No trailing ORDER BY/LIMIT on :many
-- queries (see users.sql's sqlc note).

-- UpsertProjectGrant is the only write path for a grant: re-granting a subject
-- that already has access changes its role instead of creating a second row,
-- which is what the (project_id, subject_kind, subject_id) unique index makes
-- structurally true rather than a convention.
-- name: UpsertProjectGrant :one
INSERT INTO project_grants (id, project_id, subject_kind, subject_id, role, created_at, updated_at, created_by)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (project_id, subject_kind, subject_id)
DO UPDATE SET role = excluded.role, updated_at = excluded.updated_at
RETURNING id, project_id, subject_kind, subject_id, role, created_at, updated_at, created_by;

-- name: DeleteProjectGrant :execrows
DELETE FROM project_grants WHERE project_id = ? AND subject_kind = ? AND subject_id = ?;

-- name: DeleteProjectGrantsBySubject :execrows
DELETE FROM project_grants WHERE subject_kind = ? AND subject_id = ?;

-- name: ListProjectGrantsByProject :many
SELECT id, project_id, subject_kind, subject_id, role, created_at, updated_at, created_by
FROM project_grants WHERE project_id = ?;

-- name: ListProjectGrantsBySubject :many
SELECT id, project_id, subject_kind, subject_id, role, created_at, updated_at, created_by
FROM project_grants WHERE subject_kind = ? AND subject_id = ?;
