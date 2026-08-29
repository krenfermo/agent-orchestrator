-- name: FreezeExecutionPlacement :execrows
-- Freeze a placement. Idempotent by (run, task, step, generation) AND refused
-- outright by the live partial unique index when a different placement is
-- already outstanding for the same obligation -- which is what makes "frozen
-- once" a database property rather than a convention two racing passes have to
-- honour.
INSERT OR IGNORE INTO execution_placements (
    id, workflow_run_id, task_id, workflow_step_id, project_id,
    placement_generation, lifecycle_generation, placement_type,
    repo_path, base_branch, base_sha, execution_branch,
    worktree_path, worktree_record_id, merge_target, owner_token,
    state, provenance, waiting_reason, integrated_sha, detail,
    created_at, updated_at
) VALUES (
    ?, ?, ?, ?, ?,
    ?, ?, ?,
    ?, ?, ?, ?,
    ?, ?, ?, ?,
    ?, ?, ?, ?, ?,
    ?, ?
);

-- name: GetLiveExecutionPlacement :one
-- The CURRENT authority for one obligation: the placement that has not reached
-- a terminal state. There is at most one, by index.
SELECT * FROM execution_placements
WHERE workflow_run_id = sqlc.arg(workflow_run_id)
  AND task_id = sqlc.arg(task_id)
  AND workflow_step_id = sqlc.arg(workflow_step_id)
  AND state NOT IN ('integrated','preserved','terminal');

-- name: GetExecutionPlacementByGeneration :one
SELECT * FROM execution_placements
WHERE workflow_run_id = sqlc.arg(workflow_run_id)
  AND task_id = sqlc.arg(task_id)
  AND workflow_step_id = sqlc.arg(workflow_step_id)
  AND placement_generation = sqlc.arg(placement_generation);

-- name: GetExecutionPlacementByID :one
SELECT * FROM execution_placements WHERE id = ?;

-- name: MaxExecutionPlacementGeneration :one
-- The newest generation recorded for one obligation, terminal rows included.
-- A caller holding a generation below this is stale, whatever the live row says.
SELECT CAST(COALESCE(MAX(placement_generation), 0) AS INTEGER) AS generation FROM execution_placements
WHERE workflow_run_id = sqlc.arg(workflow_run_id)
  AND task_id = sqlc.arg(task_id)
  AND workflow_step_id = sqlc.arg(workflow_step_id);

-- name: TransitionExecutionPlacementState :execrows
-- Every placement transition is a compare-and-set on the exact identity AND the
-- expected current state. A pass working from a stale read matches zero rows and
-- is refused; it never overwrites a state somebody else moved to.
UPDATE execution_placements
SET state = sqlc.arg(next_state),
    waiting_reason = sqlc.arg(waiting_reason),
    detail = sqlc.arg(detail),
    updated_at = sqlc.arg(updated_at),
    -- Stamped only when the caller says this transition is a finalisation.
    -- Expressed as a nullable parameter rather than as a CASE over the target
    -- state so the statement has ONE source of truth for "is this terminal" --
    -- the state vocabulary in domain, applied by the store -- instead of a
    -- second list here that could drift from it.
    finalized_at = COALESCE(sqlc.narg(finalized_at), finalized_at)
WHERE workflow_run_id = sqlc.arg(workflow_run_id)
  AND task_id = sqlc.arg(task_id)
  AND workflow_step_id = sqlc.arg(workflow_step_id)
  AND placement_generation = sqlc.arg(placement_generation)
  AND state = sqlc.arg(expected_state);

-- name: RecordExecutionPlacementPreparation :execrows
-- Fill in the facts that only exist once the placement is materialised: the
-- commit it was cut at, and the worktree record that now owns the checkout.
-- Conditional on the generation and on the placement not yet being terminal, so
-- a stale generation cannot rewrite a newer placement's provenance.
UPDATE execution_placements
SET base_sha = sqlc.arg(base_sha),
    worktree_path = sqlc.arg(worktree_path),
    worktree_record_id = sqlc.arg(worktree_record_id),
    updated_at = sqlc.arg(updated_at)
WHERE workflow_run_id = sqlc.arg(workflow_run_id)
  AND task_id = sqlc.arg(task_id)
  AND workflow_step_id = sqlc.arg(workflow_step_id)
  AND placement_generation = sqlc.arg(placement_generation)
  AND state NOT IN ('integrated','preserved','terminal');

-- name: MarkExecutionPlacementIntegrated :execrows
-- The one transition that authorizes cleanup, and it must NAME the commit the
-- work landed at. A placement claiming integration without a SHA is refused by
-- the caller; this statement makes the SHA and the state one write.
UPDATE execution_placements
SET state = 'integrated',
    integrated_sha = sqlc.arg(integrated_sha),
    updated_at = sqlc.arg(updated_at),
    finalized_at = sqlc.arg(updated_at)
WHERE workflow_run_id = sqlc.arg(workflow_run_id)
  AND task_id = sqlc.arg(task_id)
  AND workflow_step_id = sqlc.arg(workflow_step_id)
  AND placement_generation = sqlc.arg(placement_generation)
  AND state NOT IN ('integrated','preserved','terminal')
  AND CAST(sqlc.arg(integrated_sha) AS TEXT) <> '';

-- name: RetireSupersededExecutionPlacements :execrows
-- Retire every non-terminal placement for one obligation BELOW a given
-- generation. It is how a replacement placement takes authority: the old row
-- becomes terminal, so the live index admits the new one and every stale-
-- generation guard in the coordinator starts refusing the old.
UPDATE execution_placements
SET state = 'terminal',
    detail = sqlc.arg(detail),
    updated_at = sqlc.arg(updated_at),
    finalized_at = sqlc.arg(updated_at)
WHERE workflow_run_id = sqlc.arg(workflow_run_id)
  AND task_id = sqlc.arg(task_id)
  AND workflow_step_id = sqlc.arg(workflow_step_id)
  AND placement_generation < sqlc.arg(placement_generation)
  AND state NOT IN ('integrated','preserved','terminal');

-- name: ListExecutionPlacementsForRun :many
SELECT * FROM execution_placements
WHERE workflow_run_id = ?
ORDER BY task_id, workflow_step_id, placement_generation;

-- name: ListLiveExecutionPlacements :many
-- Everything still outstanding, for recovery sweeps.
SELECT * FROM execution_placements
WHERE state NOT IN ('integrated','preserved','terminal')
ORDER BY created_at, id;
