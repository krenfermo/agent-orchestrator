-- name: InsertWorkflowPlan :one
INSERT INTO workflow_plans (workflow_run_id, status, approval_mode, prompt_context_version,
    command_status, created_at, updated_at)
VALUES (?, 'pending', ?, ?, 'idle', ?, ?)
RETURNING *;

-- name: GetWorkflowPlan :one
SELECT * FROM workflow_plans WHERE workflow_run_id = ?;

-- name: StartWorkflowPlanCommand :execrows
UPDATE workflow_plans SET status = 'running', command_status = 'running', provider = ?, model = ?,
    context_manifest_json = ?, updated_at = ?
WHERE workflow_run_id = ? AND status = 'pending' AND command_status IN ('idle','pending');

-- name: PersistWorkflowPlanResponse :execrows
UPDATE workflow_plans SET command_status = 'responded', generated_plan_json = ?, generated_at = ?, updated_at = ?
WHERE workflow_run_id = ? AND status = 'running' AND command_status = 'running';

-- name: FinishWorkflowPlan :execrows
UPDATE workflow_plans SET status = ?, command_status = ?, validation_json = ?, plan_hash = ?,
    error_class = ?, updated_at = ?
WHERE workflow_run_id = ? AND status = 'running' AND command_status IN ('running','responded');

-- name: ApproveWorkflowPlan :execrows
UPDATE workflow_plans SET status = 'approved', approved_at = ?, updated_at = ?
WHERE workflow_run_id = ? AND status = 'validated';

-- name: SetWorkflowPlanApprovalMode :execrows
UPDATE workflow_plans SET approval_mode = ?, updated_at = ?
WHERE workflow_run_id = ? AND status != 'approved';

-- name: RejectWorkflowPlan :execrows
UPDATE workflow_plans SET status = 'rejected', rejected_at = ?, updated_at = ?
WHERE workflow_run_id = ? AND status IN ('pending','validated','invalid');

-- name: InsertWorkflowTask :exec
INSERT OR IGNORE INTO workflow_tasks (id, workflow_run_id, plan_step_id, ordinal, title, description,
    acceptance_criteria_json, verify_json, scope_json, state, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: InsertWorkflowTaskDependency :exec
INSERT OR IGNORE INTO workflow_task_dependencies (workflow_task_id, depends_on_task_id) VALUES (?, ?);

-- name: ListWorkflowTasks :many
SELECT t.*, COALESCE((SELECT json_group_array(d.depends_on_task_id)
    FROM workflow_task_dependencies d WHERE d.workflow_task_id = t.id), '[]') AS dependencies_json
FROM workflow_tasks t WHERE t.workflow_run_id = ? ORDER BY t.ordinal;

-- name: UpdateWorkflowTaskState :execrows
UPDATE workflow_tasks SET state = sqlc.arg(state), updated_at = sqlc.arg(updated_at),
    completed_at = CASE WHEN sqlc.arg(state) = 'completed' THEN sqlc.arg(completed_at) ELSE completed_at END
WHERE id = sqlc.arg(id) AND state = sqlc.arg(expected_state);

-- name: ParkWorkflowTaskForAttention :execrows
UPDATE workflow_tasks SET state = 'needs_attention',
    attention_reason = sqlc.arg(attention_reason), attention_json = sqlc.arg(attention_json),
    attention_at = sqlc.arg(attention_at), updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id) AND state = sqlc.arg(expected_state);

-- name: ResumeWorkflowTaskFromAttention :execrows
UPDATE workflow_tasks SET state = sqlc.arg(state), attention_reason = '',
    attention_at = NULL, updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id) AND state = 'needs_attention';

-- name: UpdateWorkflowTaskScope :execrows
UPDATE workflow_tasks SET scope_json = ?, updated_at = ? WHERE id = ?;

-- name: UpsertWorkflowTaskRelationship :exec
INSERT INTO workflow_task_relationships (workflow_run_id, task_id, related_task_id, relation,
    reason, detail, overlap_json, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(task_id, related_task_id) DO UPDATE SET
    relation = excluded.relation, reason = excluded.reason, detail = excluded.detail,
    overlap_json = excluded.overlap_json, created_at = excluded.created_at;

-- name: ListWorkflowTaskRelationships :many
SELECT * FROM workflow_task_relationships WHERE workflow_run_id = ?
ORDER BY task_id, related_task_id;

-- name: SetWorkflowTaskExecutionRun :execrows
UPDATE workflow_tasks SET execution_run_id = ?, state = 'running', updated_at = ?
WHERE id = ? AND execution_run_id IS NULL AND state = 'eligible';

-- name: FindWorkflowRunByPlannedTask :one
SELECT id FROM workflow_runs WHERE planned_task_id = ?;
