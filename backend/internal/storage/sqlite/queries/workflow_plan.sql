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

-- name: PersistNormalizedWorkflowPlan :execrows
-- P9 (docs/worker-lifecycle-audit.md): re-persisting the NORMALIZED plan is a
-- distinct write from PersistWorkflowPlanResponse and must not reuse its CAS.
-- The response write moves command_status running -> responded; by the time
-- normalization has run, command_status is already 'responded', so the old
-- statement matched zero rows every time and the normalized form only ever
-- survived in memory. This one is conditioned on the state that actually
-- holds (running/responded) AND on the exact bytes the caller read, so a
-- writer working from a stale read of generated_plan_json is rejected rather
-- than clobbering a newer plan.
UPDATE workflow_plans SET generated_plan_json = sqlc.arg(generated_plan_json), updated_at = sqlc.arg(updated_at)
WHERE workflow_run_id = sqlc.arg(workflow_run_id) AND status = 'running' AND command_status = 'responded'
    AND generated_plan_json = sqlc.arg(expected_plan_json);

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
    acceptance_criteria_json, verify_json, scope_json, state, plan_revision, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: InsertWorkflowTaskDependency :exec
INSERT OR IGNORE INTO workflow_task_dependencies (workflow_task_id, depends_on_task_id) VALUES (?, ?);

-- name: ListWorkflowTasks :many
-- P1-B: scoped to the plan's CURRENT revision, so every existing reader
-- (reconcile, convergence, the Board, integration) becomes revision-aware
-- without a single call-site change. A task minted for a superseded plan is
-- structurally invisible here -- which is what makes "a stale old-plan child
-- cannot regain authority" a property of the schema rather than of a check
-- somebody has to remember to write. The rows themselves are retained and
-- remain auditable through ListWorkflowTasksAtRevision.
--
-- COALESCE(p.revision, 1): a task row whose run has no plan row at all cannot
-- happen today, but reading it as revision 1 keeps this query total rather
-- than silently dropping such a row.
SELECT t.*, COALESCE((SELECT json_group_array(d.depends_on_task_id)
    FROM workflow_task_dependencies d WHERE d.workflow_task_id = t.id), '[]') AS dependencies_json
FROM workflow_tasks t
LEFT JOIN workflow_plans p ON p.workflow_run_id = t.workflow_run_id
WHERE t.workflow_run_id = ? AND t.plan_revision = COALESCE(p.revision, 1)
ORDER BY t.ordinal;

-- name: GetWorkflowTask :one
-- One task by id, whatever plan revision it belongs to.
--
-- Deliberately NOT revision-scoped, unlike ListWorkflowTasks: this answers
-- "what is this row", asked by a caller that already holds the id, rather than
-- "which tasks are authoritative for this run". A caller that has an id has it
-- because something durable named it, and refusing to resolve a superseded
-- task would make an event about one unexplainable.
SELECT t.*, COALESCE((SELECT json_group_array(d.depends_on_task_id)
    FROM workflow_task_dependencies d WHERE d.workflow_task_id = t.id), '[]') AS dependencies_json
FROM workflow_tasks t WHERE t.id = ?;

-- name: ListWorkflowTasksAtRevision :many
-- The audit view: one superseded revision's tasks, exactly as they were.
SELECT t.*, COALESCE((SELECT json_group_array(d.depends_on_task_id)
    FROM workflow_task_dependencies d WHERE d.workflow_task_id = t.id), '[]') AS dependencies_json
FROM workflow_tasks t
WHERE t.workflow_run_id = ? AND t.plan_revision = ?
ORDER BY t.ordinal;

-- name: RegenerateWorkflowPlan :execrows
-- P1-B: mint a new plan revision. See Store.RegenerateWorkflowPlan.
UPDATE workflow_plans
SET revision = revision + 1,
    status = 'pending',
    command_status = 'idle',
    provider = '',
    model = '',
    generated_plan_json = '{}',
    validation_json = '{}',
    plan_hash = '',
    error_class = '',
    generated_at = NULL,
    approved_at = NULL,
    rejected_at = NULL,
    updated_at = ?
WHERE workflow_run_id = ? AND revision = ?
  AND status IN ('validated','approved','invalid','rejected');

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

-- name: UpdateWorkflowTaskAcceptanceCriteria :execrows
UPDATE workflow_tasks SET acceptance_criteria_json = ?, updated_at = ? WHERE id = ?;

-- name: InsertWorkflowTaskCriterionAmendment :exec
INSERT INTO workflow_task_criterion_amendments (id, workflow_run_id, task_id, criterion_index,
    original_criterion, amended_criterion, disposition, reason, evidence_json, approved_by,
    superseded_review_run_id, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: ListWorkflowTaskCriterionAmendments :many
SELECT * FROM workflow_task_criterion_amendments WHERE workflow_run_id = ?
ORDER BY created_at, id;

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
