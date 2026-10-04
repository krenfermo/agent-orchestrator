-- P4-B: teams and team membership. No trailing ORDER BY/LIMIT on any :many
-- query here -- sqlc v1.31.1's SQLite codegen mis-generates those (see
-- users.sql's own note); ordering is done in Go in the store layer.

-- name: InsertTeam :one
INSERT INTO teams (id, name, slug, description, status, created_at, updated_at, tenant_id)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
RETURNING id, name, slug, description, status, created_at, updated_at, tenant_id;

-- name: GetTeamByID :one
SELECT id, name, slug, description, status, created_at, updated_at, tenant_id
FROM teams WHERE id = ?;

-- name: GetTeamBySlug :one
SELECT id, name, slug, description, status, created_at, updated_at, tenant_id
FROM teams WHERE slug = ?;

-- name: ListTeams :many
SELECT id, name, slug, description, status, created_at, updated_at, tenant_id
FROM teams;

-- name: UpdateTeam :execrows
UPDATE teams SET name = ?, slug = ?, description = ?, updated_at = ? WHERE id = ?;

-- name: UpdateTeamStatus :execrows
UPDATE teams SET status = ?, updated_at = ? WHERE id = ?;

-- name: DeleteTeam :execrows
DELETE FROM teams WHERE id = ?;

-- name: CountTeams :one
SELECT COUNT(*) FROM teams;

-- name: InsertTeamMembership :one
INSERT INTO team_memberships (id, team_id, user_id, role, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (team_id, user_id) DO UPDATE SET role = excluded.role, updated_at = excluded.updated_at
RETURNING id, team_id, user_id, role, created_at, updated_at;

-- name: DeleteTeamMembership :execrows
DELETE FROM team_memberships WHERE team_id = ? AND user_id = ?;

-- name: ListTeamMembers :many
SELECT id, team_id, user_id, role, created_at, updated_at
FROM team_memberships WHERE team_id = ?;

-- name: ListTeamMembershipsForUser :many
SELECT id, team_id, user_id, role, created_at, updated_at
FROM team_memberships WHERE user_id = ?;

-- ListActiveTeamIDsForUser is the hot path in the authorization resolver: one
-- indexed lookup per request, never a scan of every membership row. Archived
-- teams are excluded here rather than filtered in Go so an archived team's
-- grants stop conferring access at the source.
-- name: ListActiveTeamIDsForUser :many
SELECT m.team_id
FROM team_memberships m
JOIN teams t ON t.id = m.team_id
WHERE m.user_id = ? AND t.status = 'active';
