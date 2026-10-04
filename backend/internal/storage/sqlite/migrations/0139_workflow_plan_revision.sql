-- P1-B: durable plan REVISION identity.
--
-- Regenerating a plan used to overwrite the only plan row a run has. The old
-- plan stopped existing, and its task rows -- which are NOT overwritten,
-- because InsertWorkflowTask is INSERT OR IGNORE against
-- UNIQUE(workflow_run_id, plan_step_id) -- stayed behind and stayed
-- authoritative. A child run bound to one of them kept being reconciled by a
-- parent that had since been re-planned.
--
-- A revision number fixes both halves: the plan row says which generation is
-- current, every task row records the generation it was minted for, and
-- ListWorkflowTasks returns only the current generation's tasks. A stale task
-- (and therefore its child) is structurally invisible to the parent rather
-- than merely discouraged, while every one of its rows is retained and
-- auditable.
--
-- Two ADD COLUMNs and one index, on purpose. workflow_tasks is referenced by
-- three ON DELETE CASCADE children (dependencies, relationships, worktrees),
-- and 0119/0130 both learned against a real ~/.ao/data/ao.db what rebuilding
-- it costs. Nothing here rebuilds anything: every existing plan becomes
-- revision 1, every existing task becomes plan_revision 1, and revision 1 is
-- exactly what every existing reader already sees.
--
-- The two UNIQUE constraints on workflow_tasks (workflow_run_id, plan_step_id)
-- and (workflow_run_id, ordinal) are therefore left in place, and a later
-- revision's rows avoid them by construction rather than by widening them: see
-- workflow/plan_revision.go, where revision > 1 namespaces plan_step_id and
-- offsets ordinal. Revision 1 keeps its exact historical spelling, so the
-- CP9(b) canonical task identity is unchanged for every row already on disk.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE workflow_plans ADD COLUMN revision INTEGER NOT NULL DEFAULT 1;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE workflow_tasks ADD COLUMN plan_revision INTEGER NOT NULL DEFAULT 1;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_workflow_tasks_plan_revision
    ON workflow_tasks(workflow_run_id, plan_revision, ordinal);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_workflow_tasks_plan_revision;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE workflow_tasks DROP COLUMN plan_revision;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE workflow_plans DROP COLUMN revision;
-- +goose StatementEnd
