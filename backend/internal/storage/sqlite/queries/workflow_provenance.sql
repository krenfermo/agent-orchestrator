-- Migration 0133's three durable facts: dispatch checkpoints, AO-owned
-- mutation provenance, and a verify attempt's deadline + review target.
--
-- Both tables are append-only. There is no update statement for either, on
-- purpose: a provenance record is evidence of a moment, and a moment that gets
-- edited is no longer evidence of anything.

-- name: InsertWorkflowDispatchCheckpoint :one
-- Migration 0134 added the rest of the launch evidence: where the launch was
-- aimed (branch, worktree, base SHA, workspace fingerprint), which process and
-- session came out of it (runtime handle, launch generation, the harness's own
-- session id), and when the launch itself happened. Every one of them is
-- written exactly as the caller observed it -- an empty string or a NULL
-- launched_at means the writer could not read that fact, and is never filled
-- in from a neighbouring one.
INSERT INTO workflow_dispatch_checkpoints (
    id, workflow_run_id, workflow_step_id, attempt_id, checkpoint_id,
    phase, idempotency_key, harness, session_id,
    launch_stage, launch_outcome, error_class, evidence_json, detail,
    branch, worktree_path, base_sha, workspace_fingerprint,
    runtime_handle_id, runtime_launch_id, agent_session_id,
    launched_at, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: ListWorkflowDispatchCheckpointsByRun :many
SELECT * FROM workflow_dispatch_checkpoints
WHERE workflow_run_id = ?
ORDER BY created_at, id;

-- name: ListWorkflowDispatchCheckpointsByStep :many
SELECT * FROM workflow_dispatch_checkpoints
WHERE workflow_step_id = ?
ORDER BY created_at, id;

-- name: GetLatestWorkflowDispatchCheckpointByStep :one
-- The current dispatch state of one step. Newest wins: a launch-failure record
-- older than a 'worker_dispatched' one has been superseded by a worker that
-- actually launched, and must never be read as current (see
-- workflow/worker_launch_recovery.go).
SELECT * FROM workflow_dispatch_checkpoints
WHERE workflow_step_id = ?
ORDER BY created_at DESC, id DESC
LIMIT 1;

-- name: InsertWorkflowMutationProvenance :one
-- Migration 0146 added what a MEMORY PROMOTION has to be able to prove, which
-- is more than the verification path ever needed: which repository (durably,
-- not by branch name), how the work was placed, which semantic boundary this
-- row records, which generation observed it, and where an integration landed.
--
-- ON CONFLICT DO NOTHING on the partial unique index over idempotency_key is
-- what makes a boundary record exactly-once. A duplicate completion callback
-- and a daemon that died between the mutation and the row derive the same key
-- from the same facts, so the second write is a no-op rather than a second row
-- that later reads as a second mutation. RETURNING * therefore yields no row
-- on a duplicate, and the store reads the existing one back -- see
-- RecordWorkflowMutationProvenance.
INSERT INTO workflow_mutation_provenance (
    id, workflow_run_id, workflow_step_id, attempt_id, task_id,
    provenance_class, harness, session_id, branch, worktree_path,
    base_sha, head_sha, fingerprint_before, fingerprint_after,
    reason, evidence_json, observed_at, created_at,
    project_id, repo_identity, repo_path, placement, boundary, generation,
    integration_target_ref, integration_target_before_sha,
    integration_target_after_sha, integration_method, idempotency_key
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT DO NOTHING
RETURNING *;

-- name: GetWorkflowMutationProvenanceByIdempotencyKey :one
-- Read back the row a duplicate write collapsed onto. Together with the
-- ON CONFLICT above this is the exactly-once pair: the caller always ends up
-- holding the one row that describes the boundary, whether it wrote it or
-- somebody else already had.
SELECT * FROM workflow_mutation_provenance
WHERE idempotency_key = ? AND idempotency_key <> ''
LIMIT 1;

-- name: ListWorkflowMutationProvenanceByTask :many
-- Every boundary AO durably recorded for one planned task, oldest first.
--
-- This is the read a memory promotion makes, and the reason migration 0146
-- indexed (task_id, boundary, created_at): promotion happens on the dispatch
-- path's tail, and it must not scan a run's whole provenance history to find
-- out whether an integration was ever recorded.
SELECT * FROM workflow_mutation_provenance
WHERE task_id = ? AND task_id <> ''
ORDER BY created_at, id;

-- name: GetLatestWorkflowMutationProvenanceByTaskBoundary :one
-- The current state of one boundary for one task. Newest wins: a later
-- integration supersedes an earlier one (a re-integration after a repair), and
-- promotion must be pinned to the last one AO actually observed.
SELECT * FROM workflow_mutation_provenance
WHERE task_id = ? AND task_id <> '' AND boundary = ?
ORDER BY generation DESC, created_at DESC, id DESC
LIMIT 1;

-- name: ListWorkflowMutationProvenanceByRun :many
SELECT * FROM workflow_mutation_provenance
WHERE workflow_run_id = ?
ORDER BY created_at, id;

-- name: ListWorkflowMutationProvenanceByStep :many
SELECT * FROM workflow_mutation_provenance
WHERE workflow_step_id = ?
ORDER BY created_at, id;

-- name: GetLatestWorkflowMutationProvenanceByBranch :one
-- Who last changed this run's branch. This is the read the verification path
-- needs when two fingerprints disagree: not "did the tree move" (which the
-- fingerprints already answer) but "whose change is this".
SELECT * FROM workflow_mutation_provenance
WHERE workflow_run_id = ? AND branch = ?
ORDER BY created_at DESC, id DESC
LIMIT 1;

-- name: SetWorkflowAttemptDeadline :execrows
-- Records when an attempt must have concluded by. Unconditional: a deadline
-- may legitimately be extended (a capacity wait, a human continue), and the
-- attempt row is not the audit trail -- the checkpoint chain is.
UPDATE workflow_attempts
SET deadline_at = sqlc.arg(deadline_at)
WHERE id = sqlc.arg(id);

-- name: SetWorkflowAttemptReviewTarget :execrows
-- Pins the reviewed artifact a verify attempt is judging.
--
-- Guarded on the target being unset, so a re-review that moves the review
-- step's own review_run_id can never retarget an in-flight verification at
-- something no reviewer approved. Re-verifying different work means a NEW
-- attempt row, which is exactly the record that should exist for it.
UPDATE workflow_attempts
SET review_target_review_run_id = sqlc.arg(review_target_review_run_id),
    review_target_fingerprint = sqlc.arg(review_target_fingerprint),
    review_target_head_sha = sqlc.arg(review_target_head_sha)
WHERE id = sqlc.arg(id)
  AND review_target_review_run_id IS NULL
  AND review_target_fingerprint = '';
