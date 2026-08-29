-- name: EnqueueCapacityClaim :execrows
-- Admit a launch intent to the queue. Idempotent by dispatch_key: a repeated
-- reconcile, wake or restart re-derives the same key and inserts nothing.
INSERT OR IGNORE INTO capacity_claims (
    id, execution_kind, state, workflow_run_id, workflow_step_id, task_id,
    lifecycle_generation, dispatch_key, owner_id, project_id, priority,
    enqueued_at, updated_at
) VALUES (?, ?, 'queued', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: PromoteCapacityClaim :execrows
-- Grant a slot, bounded, in one atomic statement. See Store.AcquireCapacity.
UPDATE capacity_claims
SET state = 'held', held_at = ?, updated_at = ?
WHERE capacity_claims.dispatch_key = ?
  AND capacity_claims.state = 'queued'
  AND capacity_claims.lifecycle_generation = ?
  AND (SELECT COUNT(*) FROM capacity_claims h WHERE h.state = 'held') < CAST(? AS INTEGER)
  AND (SELECT COUNT(*) FROM capacity_claims k
       WHERE k.state = 'held' AND k.execution_kind = capacity_claims.execution_kind) < CAST(? AS INTEGER)
  AND (SELECT COUNT(*) FROM capacity_claims w
       WHERE w.state = 'held' AND w.workflow_run_id = capacity_claims.workflow_run_id) < CAST(? AS INTEGER);

-- name: GetCapacityClaimByDispatchKey :one
SELECT * FROM capacity_claims WHERE dispatch_key = ?;

-- name: BindCapacityClaimRuntime :execrows
-- Record which runtime incarnation a held claim paid for. Conditional on the
-- claim still being held under the same generation, so a stale writer cannot
-- attach a runtime to somebody else's slot.
UPDATE capacity_claims
SET runtime_handle = ?, runtime_instance_id = ?, updated_at = ?
WHERE dispatch_key = ? AND state = 'held' AND lifecycle_generation = ?;

-- name: ReleaseCapacityClaim :execrows
-- Return a slot. Conditional on the generation, so a stale generation can
-- never release a newer claim, and idempotent by construction: a claim already
-- released matches zero rows and the caller reads that as "already free".
UPDATE capacity_claims
SET state = 'released', released_at = ?, release_reason = ?, updated_at = ?
WHERE dispatch_key = ? AND state IN ('queued','held') AND lifecycle_generation = ?;

-- name: ReleaseCapacityClaimsForRun :execrows
-- Release every outstanding claim a run still holds, whatever its generation.
-- Reserved for a run that has reached a terminal state: at that point no
-- generation of it may launch anything, so there is no newer claim to protect.
UPDATE capacity_claims
SET state = 'released', released_at = ?, release_reason = ?, updated_at = ?
WHERE workflow_run_id = ? AND state IN ('queued','held');

-- name: CountHeldCapacityClaims :one
SELECT COUNT(*) FROM capacity_claims WHERE state = 'held';

-- name: CountHeldCapacityClaimsByKind :many
SELECT execution_kind, COUNT(*) AS held FROM capacity_claims
WHERE state = 'held' GROUP BY execution_kind;

-- name: CountQueuedCapacityClaimsByKind :many
SELECT execution_kind, COUNT(*) AS queued FROM capacity_claims
WHERE state = 'queued' GROUP BY execution_kind;

-- name: ListHeldCapacityClaims :many
SELECT * FROM capacity_claims WHERE state = 'held'
ORDER BY held_at, id;

-- name: ListQueuedCapacityClaims :many
-- The scheduler's deterministic order: priority first, then age, then id as a
-- total tiebreak so two claims enqueued in the same clock tick still have one
-- defined winner.
SELECT * FROM capacity_claims WHERE state = 'queued'
ORDER BY priority, enqueued_at, id
LIMIT ?;

-- name: ListCapacityClaimsForRun :many
SELECT * FROM capacity_claims WHERE workflow_run_id = ?
ORDER BY enqueued_at, id;

-- name: ListOutstandingCapacityClaims :many
-- Everything not yet released, for recovery sweeps.
SELECT * FROM capacity_claims WHERE state IN ('queued','held')
ORDER BY enqueued_at, id;
