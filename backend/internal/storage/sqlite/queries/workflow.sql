-- name: InsertWorkflowRun :one
INSERT INTO workflow_runs (
    id, project_id, objective, state, policy_version, policy_snapshot,
    created_at, updated_at, completed_at, cancelled_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL)
RETURNING id, project_id, objective, state, policy_version, policy_snapshot,
          created_at, updated_at, completed_at, cancelled_at;

-- name: GetWorkflowRun :one
SELECT id, project_id, objective, state, policy_version, policy_snapshot,
       created_at, updated_at, completed_at, cancelled_at
FROM workflow_runs
WHERE id = ?;

-- name: ListWorkflowRuns :many
SELECT id, project_id, objective, state, policy_version, policy_snapshot,
       created_at, updated_at, completed_at, cancelled_at
FROM workflow_runs
ORDER BY created_at DESC, id DESC;

-- name: ListWorkflowRunsByProject :many
SELECT id, project_id, objective, state, policy_version, policy_snapshot,
       created_at, updated_at, completed_at, cancelled_at
FROM workflow_runs
WHERE project_id = ?
ORDER BY created_at DESC, id DESC;

-- name: ListNonTerminalWorkflowRuns :many
SELECT id, project_id, objective, state, policy_version, policy_snapshot,
       created_at, updated_at, completed_at, cancelled_at
FROM workflow_runs
WHERE state NOT IN ('completed', 'failed', 'cancelled')
ORDER BY created_at;

-- name: UpdateWorkflowRunState :execrows
UPDATE workflow_runs
SET state = sqlc.arg(state), updated_at = sqlc.arg(updated_at),
    completed_at = sqlc.arg(completed_at), cancelled_at = sqlc.arg(cancelled_at)
WHERE id = sqlc.arg(id) AND state = sqlc.arg(expected_state);

-- name: InsertWorkflowStep :one
INSERT INTO workflow_steps (
    id, workflow_run_id, kind, ordinal, depends_on_step_id, state,
    assigned_harness, session_id, review_run_id, expected_artifacts_version,
    created_at, updated_at, completed_at, artifact_json
) VALUES (?, ?, ?, ?, ?, ?, '', NULL, NULL, '', ?, ?, NULL, ?)
RETURNING id, workflow_run_id, kind, ordinal, depends_on_step_id, state,
          assigned_harness, session_id, review_run_id, expected_artifacts_version,
          created_at, updated_at, completed_at, artifact_json;

-- name: GetWorkflowStep :one
SELECT id, workflow_run_id, kind, ordinal, depends_on_step_id, state,
       assigned_harness, session_id, review_run_id, expected_artifacts_version,
       created_at, updated_at, completed_at, artifact_json
FROM workflow_steps
WHERE id = ?;

-- name: ListWorkflowStepsByRun :many
SELECT id, workflow_run_id, kind, ordinal, depends_on_step_id, state,
       assigned_harness, session_id, review_run_id, expected_artifacts_version,
       created_at, updated_at, completed_at, artifact_json
FROM workflow_steps
WHERE workflow_run_id = ?
ORDER BY ordinal;

-- name: UpdateWorkflowStepState :execrows
UPDATE workflow_steps
SET state = sqlc.arg(state), updated_at = sqlc.arg(updated_at),
    completed_at = sqlc.arg(completed_at)
WHERE id = sqlc.arg(id) AND state = sqlc.arg(expected_state);

-- name: UpdateWorkflowStepArtifact :execrows
-- Persists the plan step's deterministic PlanArtifact (or, later, another
-- step kind's artifact). Not a state transition, so no expected/next guard
-- beyond the row existing.
UPDATE workflow_steps
SET artifact_json = sqlc.arg(artifact_json), updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id);

-- name: UpdateWorkflowStepSession :execrows
-- Backfills the work step's session_id once a spawn is durably known to have
-- happened (either just-dispatched or adopted via natural-key lookup).
-- Guarded on session_id currently NULL so a second caller can never clobber
-- an already-associated session.
UPDATE workflow_steps
SET session_id = sqlc.arg(session_id), updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id) AND session_id IS NULL;

-- name: SetWorkflowStepReviewRun :execrows
-- Checkpoint 8D: workflow_steps.review_run_id now means "the current/most-
-- recent review_run for this step" (mutable across review cycles), not
-- write-once as Checkpoint 8C originally modeled it. Unconditional update, no
-- WHERE review_run_id IS NULL guard: the primary anti-duplication guard
-- against creating two review_runs for the same cycle is the outbox
-- idempotency key (cycle-specific), not this column. Supersedes 8C's
-- write-once UpdateWorkflowStepReviewRun, which 8D's cycling dispatch flow
-- fully replaces (its one call site now uses this instead).
UPDATE workflow_steps
SET review_run_id = sqlc.arg(review_run_id), updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id);

-- name: GetMaxWorkflowAttemptNumber :one
SELECT CAST(COALESCE(MAX(attempt_number), 0) AS INTEGER) AS max_attempt_number
FROM workflow_attempts
WHERE workflow_step_id = ?;

-- name: InsertWorkflowAttempt :one
INSERT INTO workflow_attempts (
    id, workflow_step_id, attempt_number, harness, model,
    started_at, finished_at, outcome, error_class, retry_after
) VALUES (?, ?, ?, ?, ?, ?, NULL, NULL, NULL, NULL)
RETURNING id, workflow_step_id, attempt_number, harness, model,
          started_at, finished_at, outcome, error_class, retry_after;

-- name: ListWorkflowAttemptsByStep :many
SELECT id, workflow_step_id, attempt_number, harness, model,
       started_at, finished_at, outcome, error_class, retry_after
FROM workflow_attempts
WHERE workflow_step_id = ?
ORDER BY attempt_number;

-- name: GetLatestWorkflowAttemptByStep :one
SELECT id, workflow_step_id, attempt_number, harness, model,
       started_at, finished_at, outcome, error_class, retry_after
FROM workflow_attempts
WHERE workflow_step_id = ?
ORDER BY attempt_number DESC
LIMIT 1;

-- name: InsertWorkflowCheckpoint :one
INSERT INTO workflow_checkpoints (
    id, workflow_run_id, workflow_step_id, attempt_id, project_id, session_id,
    branch, worktree_path, base_sha, head_sha, review_run_id, review_verdict,
    retry_state, next_action, durable_phase, payload_version, created_at,
    fingerprint_before, fingerprint_after
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING id, workflow_run_id, workflow_step_id, attempt_id, project_id, session_id,
          branch, worktree_path, base_sha, head_sha, review_run_id, review_verdict,
          retry_state, next_action, durable_phase, payload_version, created_at,
          fingerprint_before, fingerprint_after;

-- name: ListWorkflowCheckpointsByRun :many
SELECT id, workflow_run_id, workflow_step_id, attempt_id, project_id, session_id,
       branch, worktree_path, base_sha, head_sha, review_run_id, review_verdict,
       retry_state, next_action, durable_phase, payload_version, created_at,
       fingerprint_before, fingerprint_after
FROM workflow_checkpoints
WHERE workflow_run_id = ?
ORDER BY created_at, id;

-- name: GetLatestWorkflowCheckpointByStep :one
SELECT id, workflow_run_id, workflow_step_id, attempt_id, project_id, session_id,
       branch, worktree_path, base_sha, head_sha, review_run_id, review_verdict,
       retry_state, next_action, durable_phase, payload_version, created_at,
       fingerprint_before, fingerprint_after
FROM workflow_checkpoints
WHERE workflow_step_id = ?
ORDER BY created_at DESC, id DESC
LIMIT 1;

-- name: InsertWorkflowOutboxEntry :execrows
INSERT INTO workflow_outbox (
    id, workflow_run_id, workflow_step_id, idempotency_key, command_type,
    payload, status, created_at, dispatched_at, acknowledged_at, failed_at, error_class
) VALUES (?, ?, ?, ?, ?, ?, 'pending', ?, NULL, NULL, NULL, '')
ON CONFLICT (idempotency_key) DO NOTHING;

-- name: GetWorkflowOutboxByIdempotencyKey :one
SELECT id, workflow_run_id, workflow_step_id, idempotency_key, command_type,
       payload, status, created_at, dispatched_at, acknowledged_at, failed_at, error_class
FROM workflow_outbox
WHERE idempotency_key = ?;

-- name: ListWorkflowOutboxByRun :many
SELECT id, workflow_run_id, workflow_step_id, idempotency_key, command_type,
       payload, status, created_at, dispatched_at, acknowledged_at, failed_at, error_class
FROM workflow_outbox
WHERE workflow_run_id = ?
ORDER BY created_at, id;

-- name: UpdateWorkflowOutboxStatus :execrows
-- Compare-and-swap outbox status transition (pending -> dispatched ->
-- acknowledged, or -> failed from either pending or dispatched). Timestamps
-- are set by the caller only for the column matching the target status; the
-- store layer null-guards the other two.
UPDATE workflow_outbox
SET status = sqlc.arg(status),
    dispatched_at = sqlc.arg(dispatched_at),
    acknowledged_at = sqlc.arg(acknowledged_at),
    failed_at = sqlc.arg(failed_at),
    error_class = sqlc.arg(error_class)
WHERE id = sqlc.arg(id) AND status = sqlc.arg(expected_status);

-- name: UpdateWorkflowAttemptOutcome :execrows
-- Updates an existing attempt row's terminal facts. Used both when a dispatch
-- attempt concludes (success/failure) and when a later fact-based observation
-- refines an already-recorded attempt's error_class (e.g. to
-- ambiguous_worker_state) without creating a second attempt row for the same
-- execution.
UPDATE workflow_attempts
SET finished_at = sqlc.arg(finished_at),
    outcome = sqlc.arg(outcome),
    error_class = sqlc.arg(error_class)
WHERE id = sqlc.arg(id);
