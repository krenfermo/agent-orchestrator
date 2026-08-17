-- Checkpoint 8P-A: minimal ownership plumbing on top of the existing
-- projects/workflow_runs tables (0109's nullable owner_user_id/user_id
-- columns). Deliberately narrow: these queries only ever read/write the
-- owner column itself, never any other project/workflow_run field, so this
-- file stays independent of projects.sql / workflow.sql's own row shapes.

-- name: GetProjectOwner :one
SELECT owner_user_id FROM projects WHERE id = ?;

-- name: SetProjectOwner :execrows
UPDATE projects SET owner_user_id = ? WHERE id = ?;

-- name: BackfillProjectOwners :execrows
UPDATE projects SET owner_user_id = ? WHERE owner_user_id IS NULL;

-- name: ListProjectIDsByOwner :many
SELECT id FROM projects WHERE owner_user_id = ?;

-- name: GetWorkflowRunOwner :one
SELECT user_id FROM workflow_runs WHERE id = ?;

-- name: SetWorkflowRunOwner :execrows
UPDATE workflow_runs SET user_id = ? WHERE id = ?;

-- name: BackfillWorkflowRunOwners :execrows
UPDATE workflow_runs SET user_id = ? WHERE user_id IS NULL;

-- name: ListWorkflowRunIDsByOwner :many
SELECT id FROM workflow_runs WHERE user_id = ?;
