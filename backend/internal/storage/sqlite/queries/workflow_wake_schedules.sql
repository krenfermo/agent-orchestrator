-- name: InsertWorkflowWakeSchedule :one
-- Checkpoint 8N: durable wake-up scheduling row for a run/step parked on
-- provider capacity. Plain insert; the idempotency_key UNIQUE constraint
-- (0106) is what the store layer's upsert-by-idempotency-key flow relies on
-- to detect "a wake for this exact scope already exists" under writeMu,
-- rather than an ON CONFLICT clause (kept deliberately simple -- see this
-- file's own note on the sqlc SQLite-codegen bug below).
INSERT INTO workflow_wake_schedules (
    id, workflow_run_id, workflow_step_id, reason, status, idempotency_key,
    scheduled_at, known_reset_at, attempt_count, claimed_by, claimed_at,
    completed_at, cancelled_at, last_error, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING id, workflow_run_id, workflow_step_id, reason, status, idempotency_key,
          scheduled_at, known_reset_at, attempt_count, claimed_by, claimed_at,
          completed_at, cancelled_at, last_error, created_at, updated_at;

-- name: GetWorkflowWakeSchedule :one
SELECT id, workflow_run_id, workflow_step_id, reason, status, idempotency_key,
       scheduled_at, known_reset_at, attempt_count, claimed_by, claimed_at,
       completed_at, cancelled_at, last_error, created_at, updated_at
FROM workflow_wake_schedules
WHERE id = ?;

-- name: GetWorkflowWakeScheduleByIdempotencyKey :one
-- Store-layer upsert-by-idempotency-key reads this first (under writeMu) to
-- decide insert-new vs reschedule-existing. Plain lookup, not a CAS.
SELECT id, workflow_run_id, workflow_step_id, reason, status, idempotency_key,
       scheduled_at, known_reset_at, attempt_count, claimed_by, claimed_at,
       completed_at, cancelled_at, last_error, created_at, updated_at
FROM workflow_wake_schedules
WHERE idempotency_key = ?;

-- name: RescheduleWorkflowWakeSchedule :execrows
-- Used both by the idempotent-upsert path (a wake for the same scope
-- already exists and is still pending/claimed -- fold the new attempt into
-- it rather than creating a duplicate row) and by Fail's "reschedule with
-- backoff" path. Always resets status back to pending and clears any stale
-- claim, since a reschedule always supersedes whatever claim state the row
-- was in.
UPDATE workflow_wake_schedules
SET status = 'pending', scheduled_at = ?, known_reset_at = ?,
    attempt_count = attempt_count + 1, claimed_by = NULL, claimed_at = NULL,
    last_error = ?, updated_at = ?
WHERE id = ? AND status IN ('pending', 'claimed');

-- name: ReviveWorkflowWakeSchedule :execrows
-- Checkpoint 8P-E13A.1: bring a FINISHED wake row back for a new wait on the
-- same scope.
--
-- idempotency_key is globally UNIQUE (0106), so the store's original
-- "existing row is completed/cancelled -> insert a fresh row" branch could
-- never succeed: it always hit the unique constraint, and the wake was
-- silently lost. A run whose branch_lock wake had completed once could
-- therefore never be scheduled again, which is exactly how a queued workflow
-- stopped resuming on its own. Reviving the row in place keeps one row per
-- scope, which is what the unique constraint was expressing all along.
--
-- attempt_count resets to 0: this is a NEW wait, not another retry of the old
-- one, and inheriting the finished row's backoff would start it half an hour
-- late.
UPDATE workflow_wake_schedules
SET status = 'pending', scheduled_at = ?, known_reset_at = ?,
    attempt_count = 0, claimed_by = NULL, claimed_at = NULL,
    completed_at = NULL, cancelled_at = NULL, last_error = '', updated_at = ?
WHERE id = ? AND status IN ('completed', 'cancelled');

-- name: ListDueWorkflowWakeSchedules :many
-- Due set for the daemon poller: pending rows whose scheduled_at has
-- passed, plus claimed rows whose claim lease has expired (a prior claimant
-- crashed mid-fire without completing or failing the wake). No trailing
-- ORDER BY/LIMIT -- see this file's note below; the store layer bounds the
-- batch size and sorts in Go if needed.
SELECT id, workflow_run_id, workflow_step_id, reason, status, idempotency_key,
       scheduled_at, known_reset_at, attempt_count, claimed_by, claimed_at,
       completed_at, cancelled_at, last_error, created_at, updated_at
FROM workflow_wake_schedules
WHERE scheduled_at <= ? AND (status = 'pending' OR (status = 'claimed' AND claimed_at <= ?));

-- name: ClaimWorkflowWakeSchedule :execrows
-- CAS-style claim, mirroring workflow_question_resolutions.sql's
-- TransitionResolutionStatus: applies only if the row is still in
-- expected_status (pending, or claimed with a since-expired lease), so two
-- pollers racing on the same due row never both believe they claimed it.
UPDATE workflow_wake_schedules
SET status = 'claimed', claimed_by = ?, claimed_at = ?, updated_at = ?
WHERE id = ? AND status = sqlc.arg(expected_status);

-- name: CompleteWorkflowWakeSchedule :execrows
-- Applies only from claimed, so a completion racing a concurrent
-- cancel/reschedule loses cleanly (0 rows, not an error -- the caller
-- treats that as "someone else already handled it").
UPDATE workflow_wake_schedules
SET status = 'completed', completed_at = ?, updated_at = ?
WHERE id = ? AND status = 'claimed';

-- name: CancelWorkflowWakeSchedule :execrows
UPDATE workflow_wake_schedules
SET status = 'cancelled', cancelled_at = ?, updated_at = ?
WHERE id = ? AND status IN ('pending', 'claimed');

-- name: CancelAllWorkflowWakeSchedulesByRun :execrows
-- Run-cancel cascade (mirrors CancelOpenWorkflowQuestionsByRun's own
-- run-scoped cancel-all shape): every pending/claimed wake for the run
-- stops mattering once the run itself is cancelled.
UPDATE workflow_wake_schedules
SET status = 'cancelled', cancelled_at = ?, updated_at = ?
WHERE workflow_run_id = ? AND status IN ('pending', 'claimed');

-- name: ListPendingWorkflowWakeSchedulesByRun :many
-- NextForRun's read source: every still-open (pending or claimed) wake for
-- a run. No trailing ORDER BY -- the store layer picks the earliest
-- scheduled_at in Go. Deliberately has no LIMIT either; a run has at most a
-- small handful of open wakes at once (one per step/role/reason scope), so
-- an unbounded scan here is not a real cost.
SELECT id, workflow_run_id, workflow_step_id, reason, status, idempotency_key,
       scheduled_at, known_reset_at, attempt_count, claimed_by, claimed_at,
       completed_at, cancelled_at, last_error, created_at, updated_at
FROM workflow_wake_schedules
WHERE workflow_run_id = ? AND status IN ('pending', 'claimed');

-- Note on sqlc v1.31.1's reproduced SQLite-codegen bug (see
-- workflow_question_resolutions.sql for the original, more detailed
-- writeup): every query in this file avoids a trailing ORDER BY/LIMIT
-- clause, and this file contains no non-ASCII characters anywhere,
-- including in comments. Do not add either without re-running
-- `npm run sqlc` and manually inspecting every generated
-- `const ... = ...` block in workflow_wake_schedules.sql.go for
-- truncation/cross-query corruption first.
