-- name: SupersedeOutstandingPlacementOverrides :execrows
-- Retire whatever is currently outstanding for one obligation, so the next
-- request can take the outstanding slot. Run in the same critical section as
-- the insert below: together they are "the newest request wins", and apart they
-- would be a window in which an obligation has no request at all.
UPDATE execution_placement_overrides
SET state = 'superseded',
    detail = sqlc.arg(detail),
    updated_at = sqlc.arg(updated_at),
    resolved_at = sqlc.arg(updated_at)
WHERE workflow_run_id = sqlc.arg(workflow_run_id)
  AND task_id = sqlc.arg(task_id)
  AND workflow_step_id = sqlc.arg(workflow_step_id)
  AND state = 'requested';

-- name: RequestExecutionPlacementOverride :execrows
-- Record what an operator asked for. Refused by the outstanding partial unique
-- index if something is still outstanding, which is what makes the supersede
-- above load-bearing rather than tidy.
INSERT OR IGNORE INTO execution_placement_overrides (
    id, workflow_run_id, task_id, workflow_step_id, project_id,
    requested_placement, requested_by, reason, state, applied_generation,
    detail, created_at, updated_at
) VALUES (
    ?, ?, ?, ?, ?,
    ?, ?, ?, ?, ?,
    ?, ?, ?
);

-- name: GetOutstandingPlacementOverride :one
-- The request the next freeze or transition will consume. At most one, by index.
SELECT * FROM execution_placement_overrides
WHERE workflow_run_id = sqlc.arg(workflow_run_id)
  AND task_id = sqlc.arg(task_id)
  AND workflow_step_id = sqlc.arg(workflow_step_id)
  AND state = 'requested';

-- name: ResolvePlacementOverride :execrows
-- Mark a request consumed, naming the generation that consumed it. Conditioned
-- on the row still being outstanding, so two passes consuming one request
-- cannot both believe they did.
UPDATE execution_placement_overrides
SET state = sqlc.arg(next_state),
    applied_generation = sqlc.arg(applied_generation),
    detail = sqlc.arg(detail),
    updated_at = sqlc.arg(updated_at),
    resolved_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id)
  AND state = 'requested';

-- name: ListPlacementOverridesForRun :many
SELECT * FROM execution_placement_overrides
WHERE workflow_run_id = ?
ORDER BY created_at, id;

-- name: RecordPlacementTransition :execrows
-- Write the intent BEFORE the replacement it describes. A crash between this
-- row and the retirement it authorizes leaves an explanation for a move that
-- may not have happened, which is recoverable; the reverse leaves a move
-- nobody can account for.
--
-- INSERT OR IGNORE against the surviving partial unique index is what makes a
-- repeated transition request idempotent: the second attempt inserts nothing
-- and the caller reads back the row that already exists.
INSERT OR IGNORE INTO execution_placement_transitions (
    id, workflow_run_id, task_id, workflow_step_id, project_id,
    from_generation, to_generation,
    from_placement_type, from_repo_path, from_execution_branch,
    from_worktree_path, from_base_sha,
    requested_placement, to_placement_type, requested_by, reason,
    expected_state, quiescence_digest, state, refusal_reason, detail,
    created_at, updated_at
) VALUES (
    ?, ?, ?, ?, ?,
    ?, ?,
    ?, ?, ?,
    ?, ?,
    ?, ?, ?, ?,
    ?, ?, ?, ?, ?,
    ?, ?
);

-- name: CompletePlacementTransition :execrows
-- Name the generation the replacement actually got, once it exists. Conditioned
-- on the transition still being `requested`, so a second pass cannot re-point a
-- completed transition at a different successor.
UPDATE execution_placement_transitions
SET state = 'applied',
    to_generation = sqlc.arg(to_generation),
    to_placement_type = sqlc.arg(to_placement_type),
    detail = sqlc.arg(detail),
    updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id)
  AND state = 'requested';

-- name: GetSurvivingPlacementTransition :one
-- The transition that already superseded one generation, if any. This is the
-- idempotency read: a repeated request finds it and returns it unchanged.
SELECT * FROM execution_placement_transitions
WHERE workflow_run_id = sqlc.arg(workflow_run_id)
  AND task_id = sqlc.arg(task_id)
  AND workflow_step_id = sqlc.arg(workflow_step_id)
  AND from_generation = sqlc.arg(from_generation)
  AND state <> 'refused';

-- name: ListPlacementTransitionsForRun :many
SELECT * FROM execution_placement_transitions
WHERE workflow_run_id = ?
ORDER BY created_at, id;
