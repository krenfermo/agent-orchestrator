-- Checkpoint 8P-B: provider profiles. Every query except InsertProviderProfile
-- is scoped by user_id -- ownership is enforced at the SQL layer, not just in
-- Go, so a bug upstream can't leak a cross-user row. No trailing ORDER
-- BY/LIMIT on :many queries (see users.sql's note on the sqlc v1.31.1 bug);
-- sort is done in Go in the store layer instead.

-- name: InsertProviderProfile :one
INSERT INTO provider_profiles (
    id, user_id, provider, harness, display_name, enabled,
    auth_state, auth_method, default_model, capabilities, secret_ciphertext,
    created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING id, user_id, provider, harness, display_name, enabled,
    auth_state, auth_method, default_model, capabilities, secret_ciphertext,
    created_at, updated_at;

-- name: ListProviderProfilesByUser :many
SELECT id, user_id, provider, harness, display_name, enabled,
    auth_state, auth_method, default_model, capabilities, secret_ciphertext,
    created_at, updated_at
FROM provider_profiles WHERE user_id = ?;

-- name: GetProviderProfileByIDForUser :one
SELECT id, user_id, provider, harness, display_name, enabled,
    auth_state, auth_method, default_model, capabilities, secret_ciphertext,
    created_at, updated_at
FROM provider_profiles WHERE id = ? AND user_id = ?;

-- name: GetProviderProfileOwner :one
SELECT user_id FROM provider_profiles WHERE id = ?;

-- name: UpdateProviderProfileForUser :execrows
UPDATE provider_profiles
SET display_name = ?, enabled = ?, default_model = ?, updated_at = ?
WHERE id = ? AND user_id = ?;

-- name: UpdateProviderProfileAuthStateForUser :execrows
UPDATE provider_profiles
SET auth_state = ?, updated_at = ?
WHERE id = ? AND user_id = ?;
