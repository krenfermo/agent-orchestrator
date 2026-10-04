-- +goose Up
-- +goose StatementBegin
-- Checkpoint 8B: Workflow -> Codex Worker Execution.
--
-- artifact_json is a generic small JSON slot on workflow_steps. The plan step
-- stores its structured PlanArtifact (objective, task prompt, acceptance
-- criteria) here; other step kinds may use it later. Deliberately not a new
-- table: this is one deterministic value per step, not a durable log.
ALTER TABLE workflow_steps ADD COLUMN artifact_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(artifact_json));
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE workflow_steps DROP COLUMN artifact_json;
-- +goose StatementEnd
