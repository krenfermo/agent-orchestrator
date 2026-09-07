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

-- The two queries below derive a credential lifetime from the review run it was
-- minted for, which is what makes revocation recoverable: the obligation is not
-- a queue entry that can be lost, it is re-derivable from durable rows on every
-- pass. NOT EXISTS rather than a status comparison so a review run whose row is
-- gone also counts as closed -- only a run AO still considers RUNNING keeps its
-- reviewer able to speak, and nothing else ever does.
--
-- The predicate deliberately never anticipates closure. A reviewer whose run is
-- still running keeps its credential however long it takes, because taking it
-- away early recreates the exact failure this credential exists to prevent: a
-- finished review that cannot be recorded.

-- name: ListRevocableAgentCredentials :many
SELECT id, review_run_id, runtime_handle
FROM agent_credentials
WHERE revoked_at IS NULL
  AND review_run_id != ''
  AND NOT EXISTS (
    SELECT 1 FROM review_run r
    WHERE r.id = agent_credentials.review_run_id AND r.status = 'running'
  );

-- name: RevokeClosedReviewRunAgentCredentials :execrows
UPDATE agent_credentials SET revoked_at = ?
WHERE revoked_at IS NULL
  AND review_run_id != ''
  AND NOT EXISTS (
    SELECT 1 FROM review_run r
    WHERE r.id = agent_credentials.review_run_id AND r.status = 'running'
  );

-- name: RevokeAgentCredentialsForClosedReviewRun :execrows
UPDATE agent_credentials SET revoked_at = ?
WHERE review_run_id = ?
  AND review_run_id != ''
  AND revoked_at IS NULL
  AND NOT EXISTS (
    SELECT 1 FROM review_run r
    WHERE r.id = agent_credentials.review_run_id AND r.status = 'running'
  );
