-- P4-I: agent_credentials. No trailing ORDER BY/LIMIT -- see users.sql's note
-- on the sqlc v1.31.1 SQLite codegen bug; sort/limit in Go.

-- name: InsertAgentCredential :one
INSERT INTO agent_credentials (
    id, token_hash, role, user_id, project_id, session_id,
    workflow_run_id, workflow_step_id, review_run_id,
    runtime_handle, runtime_instance_id, generation, permissions,
    created_at, expires_at, last_seen_at, revoked_at
)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING id, token_hash, role, user_id, project_id, session_id,
    workflow_run_id, workflow_step_id, review_run_id,
    runtime_handle, runtime_instance_id, generation, permissions,
    created_at, expires_at, last_seen_at, revoked_at;

-- name: GetAgentCredentialByTokenHash :one
SELECT id, token_hash, role, user_id, project_id, session_id,
    workflow_run_id, workflow_step_id, review_run_id,
    runtime_handle, runtime_instance_id, generation, permissions,
    created_at, expires_at, last_seen_at, revoked_at
FROM agent_credentials WHERE token_hash = ?;

-- name: ListAgentCredentialsForReviewRun :many
SELECT id, token_hash, role, user_id, project_id, session_id,
    workflow_run_id, workflow_step_id, review_run_id,
    runtime_handle, runtime_instance_id, generation, permissions,
    created_at, expires_at, last_seen_at, revoked_at
FROM agent_credentials WHERE review_run_id = ?;

-- name: TouchAgentCredentialLastSeen :execrows
UPDATE agent_credentials SET last_seen_at = ? WHERE id = ? AND revoked_at IS NULL;

-- name: RevokeAgentCredentialsForReviewRun :execrows
UPDATE agent_credentials SET revoked_at = ?
WHERE review_run_id = ? AND review_run_id != '' AND revoked_at IS NULL;

-- name: RevokeAgentCredentialsForSession :execrows
UPDATE agent_credentials SET revoked_at = ?
WHERE session_id = ? AND session_id != '' AND revoked_at IS NULL;
