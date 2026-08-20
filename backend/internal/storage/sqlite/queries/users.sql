-- Checkpoint 8P-A: users. No trailing ORDER BY/LIMIT on any :many query here
-- -- sqlc v1.31.1's SQLite codegen has a known bug where a trailing
-- ORDER BY/LIMIT on a query silently mis-generates (see
-- workflow_wake_schedules.sql's own note); sort/limit is done in Go in the
-- store layer instead.

-- name: InsertUser :one
INSERT INTO users (id, display_name, email, username, password_hash, status, role, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING id, display_name, email, username, password_hash, status, role, created_at, updated_at;

-- name: GetUserByID :one
SELECT id, display_name, email, username, password_hash, status, role, created_at, updated_at
FROM users WHERE id = ?;

-- name: GetUserByEmail :one
SELECT id, display_name, email, username, password_hash, status, role, created_at, updated_at
FROM users WHERE email = ?;

-- name: GetUserByUsername :one
SELECT id, display_name, email, username, password_hash, status, role, created_at, updated_at
FROM users WHERE username = ?;

-- name: CountUsers :one
SELECT COUNT(*) FROM users;

-- name: ListUsers :many
SELECT id, display_name, email, username, password_hash, status, role, created_at, updated_at
FROM users;

-- name: UpdateUserPasswordHash :execrows
UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?;

-- name: UpdateUserStatus :execrows
UPDATE users SET status = ?, updated_at = ? WHERE id = ?;

-- name: UpdateUserRole :execrows
UPDATE users SET role = ?, updated_at = ? WHERE id = ?;

-- name: CountOwners :one
SELECT COUNT(*) FROM users WHERE role = 'owner';
