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

-- P5-A phase 2C: worker credentials.
--
-- A worker's session does not exist until the spawn that creates it returns,
-- and the credential has to be in that spawn's environment. So a worker
-- credential is minted UNBOUND and bound exactly once, by the statement below,
-- when the launch confirms which session it produced.
--
-- The guard is what keeps "a binding is not a hint" true. session_id = ''
-- means the credential has never been bound and AgentAuthority.MayReachSession
-- already refuses every session route for it, so an unbound credential is inert
-- rather than permissive. The WHERE clause can therefore only ever move it from
-- unbound to bound, once: a second attempt matches no row, and a revoked
-- credential can never be resurrected into a binding.
--
-- name: BindAgentCredentialSession :execrows
UPDATE agent_credentials SET session_id = ?
WHERE id = ?
  AND session_id = ''
  AND revoked_at IS NULL;

-- name: RevokeAgentCredential :execrows
UPDATE agent_credentials SET revoked_at = ?
WHERE id = ? AND revoked_at IS NULL;

-- The worker half of the derived-lifetime rule the two review-run queries above
-- implement, and deliberately the same shape:
--
--     a worker credential may live exactly as long as its work step is running.
--
-- NOT EXISTS rather than a state comparison so a step whose row is gone also
-- counts as finished. `pending` is excluded from the live set on purpose: a step
-- that has gone back to pending is not the launch this credential was minted
-- for, and a credential outliving its own launch is the whole failure mode.
--
-- name: ListRevocableWorkerAgentCredentials :many
SELECT id, workflow_step_id, runtime_handle
FROM agent_credentials
WHERE revoked_at IS NULL
  AND role = 'worker'
  AND workflow_step_id != ''
  AND NOT EXISTS (
    SELECT 1 FROM workflow_steps s
    WHERE s.id = agent_credentials.workflow_step_id
      AND s.state IN ('ready', 'running', 'waiting')
  );

-- name: RevokeStaleWorkerAgentCredentials :execrows
UPDATE agent_credentials SET revoked_at = ?
WHERE revoked_at IS NULL
  AND role = 'worker'
  AND workflow_step_id != ''
  AND NOT EXISTS (
    SELECT 1 FROM workflow_steps s
    WHERE s.id = agent_credentials.workflow_step_id
      AND s.state IN ('ready', 'running', 'waiting')
  );

-- REPLACEMENT. The sweep above cannot discharge this one: when a step is
-- re-dispatched, the step is still running, so its predecessor's credential
-- still satisfies the "may live" predicate even though the launch it was minted
-- for is over.
--
-- So a replacement ends its predecessors explicitly, and the discriminator is
-- the ATTEMPT rather than the step: every worker credential for this step that
-- belongs to a DIFFERENT attempt is, by definition, one whose launch has been
-- superseded. Called from exactly one place -- the launcher, as it mints the new
-- one -- which is what keeps this from becoming the "remember to revoke at every
-- transition" design the reconciler exists to avoid.
--
-- REPLACEMENT. The sweep above cannot discharge this one: when a step is
-- re-dispatched, the step is still running, so its predecessor's credential
-- still satisfies the "may live" predicate even though the launch it was minted
-- for is over.
--
-- So a replacement ends its predecessors explicitly, and the discriminator is
-- the ATTEMPT rather than the step: every worker credential for this step that
-- belongs to a DIFFERENT attempt is one whose launch has been superseded.
-- Called from exactly one place, the launcher, as it mints the new one, which
-- is what keeps this from becoming the "remember to revoke at every transition"
-- design the reconciler exists to avoid.
--
-- KEEP THIS FILE PURE ASCII. sqlc v1.31.1 slices these statements by rune index
-- against a byte offset, so a single non-ASCII character anywhere above -- an
-- em dash in a comment is enough -- silently truncates a later statement
-- mid-token and produces SQL that fails at runtime with "incomplete input".
--
-- name: RevokeSupersededWorkerAgentCredentials :execrows
UPDATE agent_credentials SET revoked_at = ?
WHERE workflow_step_id = ?
  AND workflow_step_id != ''
  AND role = 'worker'
  AND runtime_instance_id != ?
  AND revoked_at IS NULL;
