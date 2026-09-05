-- P4-C: tenants (organizations) and tenant membership. No trailing
-- ORDER BY/LIMIT on any :many query here -- sqlc v1.31.1's SQLite codegen
-- mis-generates those (see users.sql's note); ordering is done in Go in the
-- store layer.

-- name: InsertTenant :one
INSERT INTO tenants (id, name, slug, description, status, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
RETURNING id, name, slug, description, status, created_at, updated_at;

-- name: GetTenantByID :one
SELECT id, name, slug, description, status, created_at, updated_at
FROM tenants WHERE id = ?;

-- name: GetTenantBySlug :one
SELECT id, name, slug, description, status, created_at, updated_at
FROM tenants WHERE slug = ?;

-- name: ListTenants :many
SELECT id, name, slug, description, status, created_at, updated_at
FROM tenants;

-- name: UpdateTenant :execrows
UPDATE tenants SET name = ?, slug = ?, description = ?, updated_at = ? WHERE id = ?;

-- name: UpdateTenantStatus :execrows
UPDATE tenants SET status = ?, updated_at = ? WHERE id = ?;

-- name: CountTenants :one
SELECT COUNT(*) FROM tenants;

-- UpsertTenantMembership is the only write path for a membership: re-adding a
-- user who already belongs changes their role instead of creating a second
-- row, which the (tenant_id, user_id) unique index makes structurally true
-- rather than a convention.
-- name: UpsertTenantMembership :one
INSERT INTO tenant_memberships (id, tenant_id, user_id, role, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (tenant_id, user_id) DO UPDATE SET role = excluded.role, updated_at = excluded.updated_at
RETURNING id, tenant_id, user_id, role, created_at, updated_at;

-- name: DeleteTenantMembership :execrows
DELETE FROM tenant_memberships WHERE tenant_id = ? AND user_id = ?;

-- name: DeleteTenantMembershipsForUser :execrows
DELETE FROM tenant_memberships WHERE user_id = ?;

-- name: ListTenantMembers :many
SELECT id, tenant_id, user_id, role, created_at, updated_at
FROM tenant_memberships WHERE tenant_id = ?;

-- name: CountTenantMembersWithRole :one
SELECT COUNT(*) FROM tenant_memberships WHERE tenant_id = ? AND role = ?;

-- name: GetTenantMembership :one
SELECT id, tenant_id, user_id, role, created_at, updated_at
FROM tenant_memberships WHERE tenant_id = ? AND user_id = ?;

-- ListActiveTenantMembershipsForUser is the hot path in the authorization
-- resolver: one indexed lookup per request. Archived tenants are excluded here
-- rather than filtered in Go so an archived organization stops conferring
-- access at the source, exactly as ListActiveTeamIDsForUser does for teams.
-- name: ListActiveTenantMembershipsForUser :many
SELECT m.id, m.tenant_id, m.user_id, m.role, m.created_at, m.updated_at
FROM tenant_memberships m
JOIN tenants t ON t.id = m.tenant_id
WHERE m.user_id = ? AND t.status = 'active';

-- Project tenancy. GetProjectTenant answers "which organization owns this
-- project"; ListProjectTenancyByIDs answers it for a bounded set in one round
-- trip, which is what the resolver needs to decide whether a grant it just
-- read is even in a tenant the caller belongs to.
-- name: GetProjectTenant :one
SELECT tenant_id FROM projects WHERE id = ?;

-- name: SetProjectTenant :execrows
UPDATE projects SET tenant_id = ? WHERE id = ?;

-- name: ListProjectTenancyByIDs :many
SELECT id, tenant_id FROM projects WHERE id IN (sqlc.slice('ids'));

-- name: ListProjectIDsByTenant :many
SELECT id FROM projects WHERE tenant_id = ?;

-- name: CountProjectsInTenant :one
SELECT COUNT(*) FROM projects WHERE tenant_id = ?;

-- Team tenancy. A team belongs to exactly one organization; a team in tenant A
-- holding a grant on a project in tenant B is the cross-tenant hole this
-- column closes.
-- name: GetTeamTenant :one
SELECT tenant_id FROM teams WHERE id = ?;

-- name: SetTeamTenant :execrows
UPDATE teams SET tenant_id = ? WHERE id = ?;

-- name: ListTeamIDsByTenant :many
SELECT id FROM teams WHERE tenant_id = ?;

-- name: ListTeamTenancyByIDs :many
SELECT id, tenant_id FROM teams WHERE id IN (sqlc.slice('ids'));
