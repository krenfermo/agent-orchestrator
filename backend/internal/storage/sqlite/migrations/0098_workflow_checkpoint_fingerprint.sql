-- +goose Up
-- +goose StatementBegin
-- Checkpoint 8D: Automatic Fix -> Re-review Loop.
--
-- fingerprint_before/fingerprint_after carry workflow.WorkspaceFingerprint's
-- deterministic worktree-state hash on a fix attempt's checkpoint:
-- fingerprint_before is the state a changes_requested verdict addressed;
-- fingerprint_after is the newly observed state once that fix cycle is
-- judged to have genuinely landed. Additive ALTER TABLE ADD COLUMN, same
-- shape as 0095_workflow_step_artifact.sql.
ALTER TABLE workflow_checkpoints ADD COLUMN fingerprint_before TEXT NOT NULL DEFAULT '';
ALTER TABLE workflow_checkpoints ADD COLUMN fingerprint_after TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE workflow_checkpoints DROP COLUMN fingerprint_before;
ALTER TABLE workflow_checkpoints DROP COLUMN fingerprint_after;
-- +goose StatementEnd
