-- Ownership kind makes the purpose of a durable git lock auditable. The
-- existing key continues to provide exclusion across direct writes and target
-- integration, while isolated task branches have distinct keys.
-- +goose Up
ALTER TABLE branch_locks ADD COLUMN ownership_kind TEXT NOT NULL DEFAULT 'direct_branch'
  CHECK (ownership_kind IN ('direct_branch','isolated_task_workspace','target_integration'));

-- +goose Down
ALTER TABLE branch_locks DROP COLUMN ownership_kind;
