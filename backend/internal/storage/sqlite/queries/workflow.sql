-- name: InsertWorkflowRun :one
INSERT INTO workflow_runs (
    id, project_id, objective, state, policy_version, policy_snapshot,
    created_at, updated_at, completed_at, cancelled_at, parent_workflow_id, planned_task_id
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL, ?, ?)
RETURNING id, project_id, objective, state, policy_version, policy_snapshot,
          created_at, updated_at, completed_at, cancelled_at, parent_workflow_id, planned_task_id, user_id,
          archived_at;

-- name: GetWorkflowRun :one
SELECT id, project_id, objective, state, policy_version, policy_snapshot,
       created_at, updated_at, completed_at, cancelled_at, parent_workflow_id, planned_task_id, user_id,
       archived_at
FROM workflow_runs
WHERE id = ?;

-- name: ListWorkflowRuns :many
SELECT id, project_id, objective, state, policy_version, policy_snapshot,
       created_at, updated_at, completed_at, cancelled_at, parent_workflow_id, planned_task_id, user_id,
       archived_at
FROM workflow_runs
WHERE parent_workflow_id IS NULL
ORDER BY created_at DESC, id DESC;

-- name: ListWorkflowRunsByProject :many
SELECT id, project_id, objective, state, policy_version, policy_snapshot,
       created_at, updated_at, completed_at, cancelled_at, parent_workflow_id, planned_task_id, user_id,
       archived_at
FROM workflow_runs
WHERE project_id = ? AND parent_workflow_id IS NULL
ORDER BY created_at DESC, id DESC;

-- name: ListChildWorkflowRuns :many
-- Every child run of a master, in creation order. Cancellation cascades over
-- this rather than over workflow_tasks: the parent link is the durable fact
-- that a run belongs to a master, and it exists even for a child whose task row
-- was never updated with its execution run id.
SELECT id, project_id, objective, state, policy_version, policy_snapshot,
       created_at, updated_at, completed_at, cancelled_at, parent_workflow_id, planned_task_id, user_id,
       archived_at
FROM workflow_runs
WHERE parent_workflow_id = ?
ORDER BY created_at, id;

-- name: ListArchivedWorkflowRunsByProject :many
-- The "Mostrar archivados" history view. Archived runs are never deleted, only
-- moved out of the active Board, so this is a plain newest-first read of them.
SELECT id, project_id, objective, state, policy_version, policy_snapshot,
       created_at, updated_at, completed_at, cancelled_at, parent_workflow_id, planned_task_id, user_id,
       archived_at
FROM workflow_runs
WHERE project_id = ? AND parent_workflow_id IS NULL AND archived_at IS NOT NULL
ORDER BY archived_at DESC, id DESC
LIMIT ?;

-- name: ArchiveWorkflowRun :execrows
-- Sets the archive marker exactly once. Deliberately CAS'd on
-- archived_at IS NULL so a repeated cancel-and-archive is a no-op that keeps
-- the ORIGINAL archive timestamp instead of bumping it, and on the terminal
-- state set so a still-running workflow can never be hidden from the Board.
UPDATE workflow_runs
SET archived_at = sqlc.arg(archived_at), updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id)
  AND archived_at IS NULL
  AND state IN ('completed', 'failed', 'cancelled');

-- name: ListNonTerminalWorkflowRuns :many
SELECT id, project_id, objective, state, policy_version, policy_snapshot,
       created_at, updated_at, completed_at, cancelled_at, parent_workflow_id, planned_task_id, user_id,
       archived_at
FROM workflow_runs
WHERE state NOT IN ('completed', 'failed', 'cancelled')
ORDER BY created_at;

-- name: UpdateWorkflowRunPolicySnapshot :execrows
-- Checkpoint 8P-C: writes the run's policy_snapshot exactly once, right
-- after creation, to embed the workflow owner's execution policy (their
-- ProviderProfile priority order at that moment) -- never touched again
-- afterwards, so a later Settings edit cannot reroute an in-flight run.
UPDATE workflow_runs
SET policy_snapshot = sqlc.arg(policy_snapshot), updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id);

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

-- name: RebindWorkflowStepReviewRunFrom :execrows
-- Compare-and-swap of a review step's AUTHORITY POINTER, when it currently
-- names a specific run.
--
-- The unconditional setter it replaces on the dispatch path let a replacement
-- bind over a pointer whose ownership had changed under it, so two dispatchers
-- could each believe they owned the step. Zero rows means somebody else owns it
-- now, and the caller must not proceed as its owner.
--
-- It guards three things, not one, because a pointer swap alone permits a state
-- nothing downstream can read: the step carrying the OLD review's adopted
-- outcome while pointing at the live NEW one.
--
--   * the pointer is still what this dispatcher expected;
--   * the step has not already RESOLVED (a completed/failed/cancelled review
--     step is not replaceable, whatever its pointer says);
--   * and the run being replaced carries no LATE verdict.
--
-- The third is deliberately about late verdicts only, not verdicts in general.
-- An on-time verdict being superseded is the ordinary review cycle: R1 asks for
-- changes, the fix lands, and cycle N+1's review replaces it. A LATE verdict is
-- the one that races: it can arrive after reconciliation has already released
-- the step, and adoption may be applying its outcome at this very moment. This
-- is what makes those two mutually exclusive rather than merely ordered --
-- whichever writes first, the other's guard fails.
UPDATE workflow_steps
SET review_run_id = sqlc.arg(next_review_run_id), updated_at = sqlc.arg(updated_at)
WHERE workflow_steps.id = sqlc.arg(id)
  AND workflow_steps.review_run_id = sqlc.arg(expected_review_run_id)
  AND workflow_steps.state NOT IN ('completed', 'failed', 'cancelled')
  AND NOT EXISTS (
      SELECT 1 FROM review_run AS r
      WHERE r.id = sqlc.arg(predecessor_review_run_id)
        AND r.late_verdict != ''
  );

-- name: ClaimWorkflowStepReviewRunIfUnset :execrows
-- The other half of the same CAS: claim a step whose pointer review-authority
-- reconciliation has released. Guarded on NULL so exactly one of several
-- concurrent replacement dispatchers can take it.
UPDATE workflow_steps
SET review_run_id = sqlc.arg(review_run_id), updated_at = sqlc.arg(updated_at)
WHERE workflow_steps.id = sqlc.arg(id)
  AND workflow_steps.review_run_id IS NULL
  AND workflow_steps.state NOT IN ('completed', 'failed', 'cancelled')
  AND NOT EXISTS (
      SELECT 1 FROM review_run AS r
      WHERE r.id = sqlc.arg(predecessor_review_run_id)
        AND r.late_verdict != ''
  );

-- name: UpdateWorkflowStepStateIfReviewRun :execrows
-- A step transition that is valid only while the named review run is still the
-- step's authority.
--
-- Late-verdict adoption spans several writes. Re-reading the pointer before them
-- cannot bind: a replacement can take authority in the gap between the check and
-- the write, and the adopter would then complete or park a step that no longer
-- belongs to its review. Conditioning each write on the pointer removes the gap
-- -- every individual write either lands while the run is still authoritative or
-- does not land at all.
UPDATE workflow_steps
SET state = sqlc.arg(state), updated_at = sqlc.arg(updated_at),
    completed_at = sqlc.arg(completed_at)
WHERE id = sqlc.arg(id)
  AND state = sqlc.arg(expected_state)
  AND review_run_id = sqlc.arg(review_run_id);

-- name: ReleaseWorkflowStepReviewRunIfNoLateVerdict :execrows
-- Releases a review step's authority pointer ONLY while the run it names still
-- has no late verdict -- one statement, so the check and the release cannot be
-- interleaved.
--
-- Without this atomicity a reconciler could read "no late verdict", have the
-- reviewer durably write one an instant later, and then clear the pointer
-- anyway: a valid authoritative verdict orphaned, and a replacement reviewer
-- dispatched over work that was already done. Zero rows affected means the
-- decision is stale -- either a verdict landed or the pointer already moved --
-- and the caller must re-read rather than proceed.
UPDATE workflow_steps
SET review_run_id = NULL, updated_at = sqlc.arg(updated_at)
WHERE workflow_steps.id = sqlc.arg(id)
  AND workflow_steps.review_run_id = sqlc.arg(review_run_id)
  AND NOT EXISTS (
      SELECT 1 FROM review_run AS r
      WHERE r.id = sqlc.arg(review_run_id)
        AND r.late_verdict != ''
  );

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
          started_at, finished_at, outcome, error_class, retry_after,
          deadline_at, review_target_review_run_id, review_target_fingerprint, review_target_head_sha;

-- name: ListWorkflowAttemptsByStep :many
SELECT id, workflow_step_id, attempt_number, harness, model,
       started_at, finished_at, outcome, error_class, retry_after,
       deadline_at, review_target_review_run_id, review_target_fingerprint, review_target_head_sha
FROM workflow_attempts
WHERE workflow_step_id = ?
ORDER BY attempt_number;

-- name: GetLatestWorkflowAttemptByStep :one
SELECT id, workflow_step_id, attempt_number, harness, model,
       started_at, finished_at, outcome, error_class, retry_after,
       deadline_at, review_target_review_run_id, review_target_fingerprint, review_target_head_sha
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
       payload, status, created_at, dispatched_at, acknowledged_at, failed_at, error_class,
       failure_generation, dispatch_generation
FROM workflow_outbox
WHERE idempotency_key = ?;

-- name: ListWorkflowRunIDsByCheckpointPhase :many
-- The run ids that carry at least one checkpoint of a given durable phase.
--
-- It exists for one job: finding runs that still owe something EXTERNAL. Every
-- other recovery entry point starts from the non-terminal runs, because a
-- terminal run has nothing left to decide -- but a terminal run can still own a
-- reviewer process AO started and never cleaned up, and nothing that only looks
-- at live runs will ever find it.
SELECT DISTINCT workflow_run_id
FROM workflow_checkpoints
WHERE durable_phase = ?;

-- name: ListWorkflowOutboxByRun :many
SELECT id, workflow_run_id, workflow_step_id, idempotency_key, command_type,
       payload, status, created_at, dispatched_at, acknowledged_at, failed_at, error_class,
       failure_generation, dispatch_generation
FROM workflow_outbox
WHERE workflow_run_id = ?
ORDER BY created_at, id;

-- name: UpdateWorkflowOutboxStatus :execrows
-- Compare-and-swap outbox status transition (pending -> dispatched ->
-- acknowledged, or -> failed from either pending or dispatched). Timestamps
-- are set by the caller only for the column matching the target status; the
-- store layer null-guards the other two.
--
-- failure_generation and dispatch_generation are both CLEARED by every
-- transition through here. A token describes ONE state of the row -- the
-- failure that failed it, or the dispatch that owns it -- and carrying either
-- into the next state would let a caller that observed the old one match a row
-- that has since moved on. A failure that records its generation uses
-- FailWorkflowOutboxWithGeneration; a claim that takes ownership uses
-- ClaimWorkflowOutboxDispatch.
UPDATE workflow_outbox
SET status = sqlc.arg(status),
    dispatched_at = sqlc.arg(dispatched_at),
    acknowledged_at = sqlc.arg(acknowledged_at),
    failed_at = sqlc.arg(failed_at),
    error_class = sqlc.arg(error_class),
    failure_generation = '',
    dispatch_generation = ''
WHERE id = sqlc.arg(id) AND status = sqlc.arg(expected_status);

-- name: ClaimWorkflowOutboxDispatch :execrows
-- The launch-ownership claim: pending -> dispatched, stamping WHO owns it.
--
-- The token is the id of the review_dispatch_authorized checkpoint written
-- immediately before this statement, so ownership is durable, reconstructable,
-- and named by the same artifact the rest of the launch protocol reads. Every
-- ownership-dependent transition off dispatched then names it back.
UPDATE workflow_outbox
SET status = 'dispatched',
    dispatched_at = sqlc.arg(dispatched_at),
    error_class = '',
    failure_generation = '',
    dispatch_generation = sqlc.arg(dispatch_generation)
WHERE id = sqlc.arg(id) AND status = 'pending';

-- name: FailWorkflowOutboxWithGeneration :execrows
-- Move an outbox entry to failed AND stamp the failure that did it, in one
-- statement. The stamp is what a later human resume compare-and-swaps against,
-- so it must land with the state it describes -- never afterwards, where a
-- crash would leave a failed row nobody can prove the generation of.
--
-- It also PROVES OWNERSHIP, in the same statement. id + status = dispatched is
-- not proof: a dispatch that paused after recording its launch error can find
-- the row dispatched again to somebody else -- released by recovery, reclaimed
-- by a second dispatch -- and would then fail a live generation and stamp its
-- own failure onto it, which a human resume could later reopen. So the claim
-- token this caller was given is part of the predicate: a caller that no longer
-- owns the row changes nothing.
UPDATE workflow_outbox
SET status = 'failed',
    failed_at = sqlc.arg(failed_at),
    error_class = sqlc.arg(error_class),
    failure_generation = sqlc.arg(failure_generation),
    dispatch_generation = ''
WHERE id = sqlc.arg(id)
  AND status = sqlc.arg(expected_status)
  AND dispatch_generation = sqlc.arg(dispatch_generation);

-- name: ReleaseDispatchedWorkflowOutboxGeneration :execrows
-- Give a claim back: dispatched -> pending, for the EXACT dispatch that holds
-- it. The mirror of the fail above and ownership-dependent for the same reason
-- -- a stale release would hand the claim of a live dispatch to somebody else.
UPDATE workflow_outbox
SET status = 'pending',
    dispatched_at = NULL,
    error_class = sqlc.arg(error_class),
    failure_generation = '',
    dispatch_generation = ''
WHERE id = sqlc.arg(id)
  AND status = 'dispatched'
  AND dispatch_generation = sqlc.arg(dispatch_generation);

-- name: ReopenFailedWorkflowOutboxGeneration :execrows
-- The human resume reopen: failed -> pending for ONE named failed generation.
--
-- The generation is part of THIS statement, not of a check preceding it. That
-- is the whole point: id + status = failed is satisfied by any failure of
-- this row, so a resume that observed failure F1 and arrived after F1 had been
-- resumed, redispatched and failed again as F2 would reopen F2 -- a launch and
-- a fresh budget epoch nobody asked for. With the generation in the predicate,
-- such a resume updates zero rows and its caller no-ops.
--
-- The reopened row carries no generation: it is no longer failed, and the next
-- failure will stamp its own.
UPDATE workflow_outbox
SET status = 'pending',
    failed_at = NULL,
    error_class = sqlc.arg(error_class),
    failure_generation = ''
WHERE id = sqlc.arg(id)
  AND status = 'failed'
  AND failure_generation = sqlc.arg(failure_generation);

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

-- name: InsertAgentHealthEvent :one
-- Checkpoint 8H: append-only durable fact about one harness's dispatch
-- outcome. Never updated; GetLatestAgentHealthEvent derives current health.
-- user_id/provider_profile_id (Checkpoint 8P-C) are nullable: empty for an
-- unowned run / trusted-local dispatch with no matched profile, in which
-- case the event is a "legacy/global" fact (see GetLatestAgentHealthEvent).
INSERT INTO agent_health_events (
    id, harness, user_id, provider_profile_id, state, reason, failure_class, cooldown_until,
    consecutive_failures, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING id, harness, user_id, provider_profile_id, state, reason, failure_class, cooldown_until,
          consecutive_failures, created_at;

-- name: GetLatestAgentHealthEvent :one
-- Legacy/global read: the latest TRULY UNSCOPED event for a harness (both
-- user_id and provider_profile_id NULL) -- never a scoped row belonging to
-- ANY user. This is what keeps one user's scoped health fact from leaking
-- into another user's (or the pre-8P-C global dashboard's) read when their
-- own scoped lookup misses; see healthScope's precedence-rule doc comment.
SELECT id, harness, user_id, provider_profile_id, state, reason, failure_class, cooldown_until,
       consecutive_failures, created_at
FROM agent_health_events
WHERE harness = ? AND user_id IS NULL AND provider_profile_id IS NULL
ORDER BY created_at DESC, id DESC
LIMIT 1;

-- name: GetLatestAgentHealthEventScoped :one
-- Checkpoint 8P-C: the most specific health fact for one user's exact
-- provider profile. Never matches another user's/profile's rows, and never
-- matches a legacy/global row (user_id/provider_profile_id NULL) -- that
-- fallback is an explicit, separate step in Go (service/capacity), not a
-- SQL COALESCE, so the precedence is auditable in one place.
SELECT id, harness, user_id, provider_profile_id, state, reason, failure_class, cooldown_until,
       consecutive_failures, created_at
FROM agent_health_events
WHERE harness = ? AND user_id = ? AND provider_profile_id = ?
ORDER BY created_at DESC, id DESC
LIMIT 1;
