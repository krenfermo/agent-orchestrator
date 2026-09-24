-- Skill runs (migration 0172). service/skills/runs.go is the only writer; every
-- state change is a compare-and-set on the current state, so two writers can
-- never both move the same run.

-- name: InsertSkillRun :exec
INSERT INTO skill_runs (
    id, project_id, skill_id, skill_version, mode_id, tool, state,
    idempotency_key, requested_by, inputs_json, capabilities_json,
    package_digest, runner_id, runner_controls, owner_instance,
    created_at, updated_at, parent_run_id
)
VALUES (?, ?, ?, ?, ?, ?, 'queued', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: ListSkillRunChildren :many
-- The child runs of one audit (migration 0173), oldest first.
SELECT * FROM skill_runs WHERE parent_run_id = ? ORDER BY created_at, id;

-- name: GetSkillRun :one
SELECT * FROM skill_runs WHERE id = ?;

-- name: GetSkillRunForProject :one
SELECT * FROM skill_runs WHERE project_id = ? AND id = ?;

-- name: ListSkillRunsForProject :many
SELECT * FROM skill_runs
WHERE project_id = ?
ORDER BY created_at DESC, id DESC
LIMIT ?;

-- name: GetActiveSkillRun :one
SELECT * FROM skill_runs
WHERE project_id = ? AND skill_id = ? AND mode_id = ? AND state IN ('queued','running');

-- name: GetSkillRunByIdempotencyKey :one
SELECT * FROM skill_runs WHERE project_id = ? AND idempotency_key = ?;

-- name: ListActiveSkillRuns :many
SELECT * FROM skill_runs WHERE state IN ('queued','running') ORDER BY created_at, id;

-- name: MarkSkillRunRunning :execrows
UPDATE skill_runs
SET state = 'running', started_at = sqlc.arg(started_at), updated_at = sqlc.arg(started_at)
WHERE id = sqlc.arg(id) AND state = 'queued' AND cancel_requested = 0;

-- name: FinishSkillRunSucceeded :execrows
UPDATE skill_runs
SET state = 'succeeded',
    image_digest = sqlc.arg(image_digest),
    approval_id = sqlc.arg(approval_id),
    approved_by = sqlc.arg(approved_by),
    summary = sqlc.arg(summary),
    finding_count = sqlc.arg(finding_count),
    truncated = sqlc.arg(truncated),
    report_json = sqlc.arg(report_json),
    report_sha256 = sqlc.arg(report_sha256),
    finished_at = sqlc.arg(finished_at),
    updated_at = sqlc.arg(finished_at)
WHERE id = sqlc.arg(id) AND state = 'running';

-- name: FinishSkillRunPartial :execrows
-- An audit that consolidated a report while not every planned mode produced a
-- verified result (migration 0173). It is never 'succeeded'.
UPDATE skill_runs
SET state = 'partial',
    summary = sqlc.arg(summary),
    finding_count = sqlc.arg(finding_count),
    report_json = sqlc.arg(report_json),
    report_sha256 = sqlc.arg(report_sha256),
    error_code = sqlc.arg(error_code),
    error_message = sqlc.arg(error_message),
    finished_at = sqlc.arg(finished_at),
    updated_at = sqlc.arg(finished_at)
WHERE id = sqlc.arg(id) AND state = 'running';

-- name: FinishSkillRunUnsuccessful :execrows
UPDATE skill_runs
SET state = sqlc.arg(new_state),
    image_digest = sqlc.arg(image_digest),
    approval_id = sqlc.arg(approval_id),
    approved_by = sqlc.arg(approved_by),
    summary = sqlc.arg(summary),
    error_code = sqlc.arg(error_code),
    error_message = sqlc.arg(error_message),
    finished_at = sqlc.arg(finished_at),
    updated_at = sqlc.arg(finished_at)
WHERE id = sqlc.arg(id) AND state IN ('queued','running');

-- name: RequestSkillRunCancel :execrows
UPDATE skill_runs
SET cancel_requested = 1, updated_at = sqlc.arg(updated_at)
WHERE project_id = sqlc.arg(project_id) AND id = sqlc.arg(id) AND state IN ('queued','running');

-- name: InsertSkillRunFinding :exec
INSERT INTO skill_run_findings (
    run_id, ordinal, rule_id, severity, category, title, path, line, recommendation, confidence
)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: ListSkillRunFindings :many
SELECT * FROM skill_run_findings WHERE run_id = ? ORDER BY ordinal;
