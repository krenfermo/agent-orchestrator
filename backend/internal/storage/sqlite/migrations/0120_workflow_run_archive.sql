-- Cancel-and-archive: a durable "this run is history" marker on workflow_runs.
--
-- The Board's active lane previously had exactly one way for a card to stop
-- being shown: the run reaching a terminal state and then ageing out of the
-- 30-minute completion retention. A run parked in needs_attention is not
-- terminal, so it never aged out — which is how a workflow stopped weeks ago by
-- child_failed / master_integration_promotion_failed stayed on the Board
-- forever, long after the incident was superseded.
--
-- archived_at is deliberately a nullable timestamp on the run, not a new state:
-- the run's state vocabulary is the operational fact (what the workflow did),
-- and archiving is a presentation fact (whether a human still wants to see it
-- in the active lane). Keeping them separate is what makes archiving safe to
-- add without touching the run state machine, and what keeps every
-- workflow_runs / workflow_steps / workflow_attempts / workflow_checkpoints row
-- queryable exactly as before — nothing is ever deleted.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE workflow_runs ADD COLUMN archived_at TIMESTAMP;
-- +goose StatementEnd

-- +goose StatementBegin
-- The Board reads "top-level, not archived" on every poll; the history view
-- reads "archived, newest first". Both are served by this partial index.
CREATE INDEX idx_workflow_runs_archived ON workflow_runs (project_id, archived_at)
    WHERE archived_at IS NOT NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_workflow_runs_archived;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE workflow_runs DROP COLUMN archived_at;
-- +goose StatementEnd
