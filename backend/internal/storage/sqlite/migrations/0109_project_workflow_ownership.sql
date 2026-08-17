-- Checkpoint 8P-A: nullable ownership columns on projects/workflow_runs.
-- Deliberately NOT backfilled here in SQL -- the bootstrap admin's id does
-- not exist yet at migration time (migrations run before the Go-side
-- bootstrap routine that creates the first admin user, see
-- service/authsvc's bootstrap and daemon.go). Backfilling any pre-existing
-- NULL owner_user_id/user_id rows to that admin's id happens once, in Go,
-- immediately after the admin user is created, so no existing project or
-- workflow run is silently orphaned.
--
-- Both columns stay nullable indefinitely: a row created while no user is
-- resolved (e.g. AO_TRUSTED_LOCAL_MODE with no bootstrap admin yet) has no
-- owner until backfilled, and that is a valid, expected state -- never
-- treated as corruption.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE projects ADD COLUMN owner_user_id TEXT;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE workflow_runs ADD COLUMN user_id TEXT;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_projects_owner_user_id ON projects (owner_user_id);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_workflow_runs_user_id ON workflow_runs (user_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_workflow_runs_user_id;
-- +goose StatementEnd

-- +goose StatementBegin
DROP INDEX IF EXISTS idx_projects_owner_user_id;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE workflow_runs DROP COLUMN user_id;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE projects DROP COLUMN owner_user_id;
-- +goose StatementEnd
