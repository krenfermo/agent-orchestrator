-- Checkpoint 8P-B.1: nullable ownership column on sessions, mirroring
-- 0109's projects.owner_user_id/workflow_runs.user_id pattern exactly.
-- Deliberately NOT added to the domain.SessionRecord struct or its
-- CreateSession/UpdateSession read/write path -- ownership is stamped and
-- read through a narrow, separate store surface (GetSessionOwner/
-- SetSessionOwner), same as projects/workflow_runs, so the widely used
-- SessionRecord type and its many existing call sites stay untouched.
--
-- Stays nullable indefinitely: a session created before this checkpoint,
-- or while no owner could be resolved, has no owner until backfilled, and
-- that is a valid, expected state -- never treated as corruption. An
-- unowned session stays visible to everyone (same as an unowned project),
-- matching the ownerForbidden convention in httpd/controllers.
--
-- Note for 8P-C: if a future checkpoint also needs migration 0111, renumber
-- that one to 0112 -- this file claims 0111 first.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE sessions ADD COLUMN owner_user_id TEXT;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_sessions_owner_user_id ON sessions (owner_user_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_sessions_owner_user_id;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE sessions DROP COLUMN owner_user_id;
-- +goose StatementEnd
