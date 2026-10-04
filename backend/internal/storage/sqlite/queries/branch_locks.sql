-- Note on sqlc v1.31.1's SQLite-codegen bug: every query in this file avoids
-- a trailing ORDER BY/LIMIT clause and the file contains no non-ASCII
-- characters anywhere, including in comments. See
-- workflow_wake_schedules.sql for the full writeup. Do not add either
-- without re-running `npm run sqlc` and inspecting the generated file for
-- truncation first.

-- name: InsertBranchLock :one
-- Acquire. Deliberately a plain INSERT with no ON CONFLICT clause: the
-- partial UNIQUE index on (lock_key) WHERE state='held' (migration 0117) is
-- what makes a second concurrent acquisition fail, and the store layer maps
-- that constraint violation to "already held by someone else" after reading
-- back the current holder. Swallowing the conflict here would throw away
-- exactly the signal the waiting workflow needs.
INSERT INTO branch_locks (
    id, lock_key, project_id, repo_path, repo_name, branch,
    workflow_run_id, workflow_step_id, session_id, owner_token, state,
    base_sha, acquired_at, renewed_at, released_at, release_reason,
    created_at, updated_at, ownership_kind
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING id, lock_key, project_id, repo_path, repo_name, branch,
          workflow_run_id, workflow_step_id, session_id, owner_token, state,
          base_sha, acquired_at, renewed_at, released_at, release_reason,
          created_at, updated_at, ownership_kind;

-- name: GetHeldBranchLock :one
-- The current holder of one repository+branch, if any.
SELECT id, lock_key, project_id, repo_path, repo_name, branch,
       workflow_run_id, workflow_step_id, session_id, owner_token, state,
       base_sha, acquired_at, renewed_at, released_at, release_reason,
       created_at, updated_at, ownership_kind
FROM branch_locks
WHERE lock_key = ? AND state = 'held';

-- name: GetBranchLock :one
SELECT id, lock_key, project_id, repo_path, repo_name, branch,
       workflow_run_id, workflow_step_id, session_id, owner_token, state,
       base_sha, acquired_at, renewed_at, released_at, release_reason,
       created_at, updated_at, ownership_kind
FROM branch_locks
WHERE id = ?;

-- name: ListHeldBranchLocks :many
-- Boot reconciliation reads every held lock at once and decides each one's
-- fate against live workflow-run state.
SELECT id, lock_key, project_id, repo_path, repo_name, branch,
       workflow_run_id, workflow_step_id, session_id, owner_token, state,
       base_sha, acquired_at, renewed_at, released_at, release_reason,
       created_at, updated_at, ownership_kind
FROM branch_locks
WHERE state = 'held';

-- name: ListHeldBranchLocksByProject :many
SELECT id, lock_key, project_id, repo_path, repo_name, branch,
       workflow_run_id, workflow_step_id, session_id, owner_token, state,
       base_sha, acquired_at, renewed_at, released_at, release_reason,
       created_at, updated_at, ownership_kind
FROM branch_locks
WHERE project_id = ? AND state = 'held';

-- name: ListHeldBranchLocksByRun :many
SELECT id, lock_key, project_id, repo_path, repo_name, branch,
       workflow_run_id, workflow_step_id, session_id, owner_token, state,
       base_sha, acquired_at, renewed_at, released_at, release_reason,
       created_at, updated_at, ownership_kind
FROM branch_locks
WHERE workflow_run_id = ? AND state = 'held';

-- name: ReleaseBranchLock :execrows
-- CAS-style release: applies only from 'held', so a release racing a
-- reconciliation that already released the same row loses cleanly with 0 rows
-- instead of resurrecting terminal state.
UPDATE branch_locks
SET state = 'released', released_at = ?, release_reason = ?, updated_at = ?
WHERE id = ? AND state = 'held';

-- name: ReleaseBranchLocksByRun :execrows
-- Run-terminal cascade: every lock a cancelled/failed/completed run still
-- holds is released in one statement.
UPDATE branch_locks
SET state = 'released', released_at = ?, release_reason = ?, updated_at = ?
WHERE workflow_run_id = ? AND state = 'held';

-- name: RenewBranchLock :execrows
-- Liveness heartbeat for a lock whose run is still progressing. Never changes
-- ownership; a renewal that finds the row no longer held affects 0 rows.
UPDATE branch_locks
SET renewed_at = ?, workflow_step_id = ?, session_id = ?, updated_at = ?
WHERE id = ? AND state = 'held' AND workflow_run_id = ?;

-- name: AdoptBranchLock :execrows
-- Restart reconciliation: a held lock whose workflow run is still live but
-- whose owner_token belongs to a previous daemon instance is transferred to
-- the current instance rather than released. Releasing it would let a second
-- workflow start writing the same branch while the recovered run resumes;
-- leaving the stale token would make every later reconcile pass re-evaluate
-- the same row forever.
UPDATE branch_locks
SET owner_token = ?, renewed_at = ?, updated_at = ?
WHERE id = ? AND state = 'held';


-- name: CedeBranchLock :execrows
-- P1-D: hand a held direct-branch lock from its current holder to another run,
-- conditioned on WHO holds it right now.
--
-- It is a transfer and never a steal: the predicate names the run that must
-- currently hold the lock, so a pass working from a stale view of ownership
-- matches zero rows and is refused. The lock row itself never leaves the
-- 'held' state, so no window exists in which the branch has no owner and a
-- third run could acquire it.
--
-- The reverse hand-back is the same statement with the two run ids swapped,
-- which is why there is one query rather than two: cession and return are the
-- same operation, and giving them separate SQL would let them drift apart.
UPDATE branch_locks
SET workflow_run_id = ?, workflow_step_id = ?, session_id = '', renewed_at = ?, updated_at = ?
WHERE id = ? AND state = 'held' AND workflow_run_id = ?;
