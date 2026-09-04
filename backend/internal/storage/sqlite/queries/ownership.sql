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

-- Checkpoint 8P-B.1: same narrow pattern for sessions.owner_user_id
-- (0111). Stamped best-effort at spawn time from the workflow run's
-- already-resolved owner; never trusted from client input.

-- name: GetSessionOwner :one
SELECT owner_user_id FROM sessions WHERE id = ?;

-- name: SetSessionOwner :execrows
UPDATE sessions SET owner_user_id = ? WHERE id = ?;

-- P4-B: the project a session or a workflow run belongs to. Authorization for
-- a session/run is a question about its PROJECT, so the resolver needs the
-- project id and nothing else; selecting one indexed column keeps the gate off
-- the full row every request would otherwise load.

-- name: GetSessionProjectID :one
SELECT project_id FROM sessions WHERE id = ?;

-- name: GetWorkflowRunProjectID :one
SELECT project_id FROM workflow_runs WHERE id = ?;
