-- name: UpsertPostRunQARun :one
-- Save one pass of the Post-Run QA gate. Keyed on the run's own id, so
-- advancing a live pass (phase, findings, repair cycle, verdict) rewrites its
-- row while a subject's earlier passes are left untouched.
INSERT INTO post_run_qa_runs (
    id, subject_kind, subject_id, phase, findings_json,
    repair_cycle_count, max_repair_cycles, result, started_at, completed_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
    subject_kind = excluded.subject_kind,
    subject_id = excluded.subject_id,
    phase = excluded.phase,
    findings_json = excluded.findings_json,
    repair_cycle_count = excluded.repair_cycle_count,
    max_repair_cycles = excluded.max_repair_cycles,
    result = excluded.result,
    started_at = excluded.started_at,
    completed_at = excluded.completed_at
RETURNING id, subject_kind, subject_id, phase, findings_json,
          repair_cycle_count, max_repair_cycles, result, started_at, completed_at;

-- name: GetPostRunQARun :one
SELECT id, subject_kind, subject_id, phase, findings_json,
       repair_cycle_count, max_repair_cycles, result, started_at, completed_at
FROM post_run_qa_runs
WHERE id = ?;

-- name: GetLatestPostRunQARunForSubject :one
-- What a restarting daemon asks: is this subject's gate still open, and where
-- did it get to? id breaks a started_at tie so the answer is deterministic.
SELECT id, subject_kind, subject_id, phase, findings_json,
       repair_cycle_count, max_repair_cycles, result, started_at, completed_at
FROM post_run_qa_runs
WHERE subject_kind = ? AND subject_id = ?
ORDER BY started_at DESC, id DESC
LIMIT 1;
