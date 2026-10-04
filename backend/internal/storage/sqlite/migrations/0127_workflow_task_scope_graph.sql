-- Durable write-set / conflict model for a master plan's task DAG.
--
-- Before this, a planned task persisted what it was (title, description,
-- acceptance criteria, verify checks) and what it came after
-- (workflow_task_dependencies), but nothing about what it would TOUCH. So the
-- only question scheduling and integration could answer about two tasks was
-- "is there a dependency edge?" -- and two independent tasks that both rewrite
-- the same file look exactly like two genuinely independent tasks right up to
-- the moment their integration collides.
--
-- Two additions:
--
--   * workflow_tasks.scope_json -- the task's estimated read/write scope,
--     likely packages/components, explicitly named files and symbols, its
--     execution strategy, and its integration dependencies, as one JSON
--     document (domain.WorkflowTaskScope). It is inline rather than a child
--     table for the same reason acceptance_criteria_json is: it is only ever
--     read and written as a whole, with the task that owns it, and is never
--     queried across runs.
--
--   * workflow_task_relationships -- one row per UNORDERED pair of tasks in a
--     plan, carrying the pair's classification and the reason behind it.
--     Storing the decision is the point: scheduling and integration read what
--     the classifier concluded instead of re-deriving it from text that may
--     since have been normalized, re-planned, or refreshed with what a
--     completed task actually wrote.
--
-- The pair is canonicalized as task_id < related_task_id and the CHECK
-- enforces it, so a pair can be inserted exactly once and no reader has to
-- defend against seeing it twice in opposite orders. Direction, where it
-- matters, already lives in workflow_task_dependencies; a functional
-- dependency row here names the direction in its detail text.
--
-- ON DELETE CASCADE on both task columns mirrors workflow_task_dependencies:
-- a deleted run takes its tasks, their dependency edges, and their pair
-- classifications with it.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE workflow_tasks ADD COLUMN scope_json TEXT NOT NULL DEFAULT '{}';
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE workflow_task_relationships (
    workflow_run_id  TEXT NOT NULL REFERENCES workflow_runs(id) ON DELETE CASCADE,
    task_id          TEXT NOT NULL REFERENCES workflow_tasks(id) ON DELETE CASCADE,
    related_task_id  TEXT NOT NULL REFERENCES workflow_tasks(id) ON DELETE CASCADE,
    relation         TEXT NOT NULL CHECK (relation IN (
        'functional_dependency',
        'probable_write_conflict',
        'independent'
    )),
    -- Stable machine-checkable code (workflow.TaskRelationReason) plus the
    -- sentence behind it.
    reason           TEXT NOT NULL DEFAULT '',
    detail           TEXT NOT NULL DEFAULT '',
    -- The specific overlapping paths that made this a write conflict, so the
    -- decision is checkable rather than merely assertable. '[]' otherwise.
    overlap_json     TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(overlap_json)),
    created_at       TIMESTAMP NOT NULL,
    PRIMARY KEY (task_id, related_task_id),
    CHECK (task_id < related_task_id)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_workflow_task_relationships_run
    ON workflow_task_relationships(workflow_run_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_workflow_task_relationships_run;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS workflow_task_relationships;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE workflow_tasks DROP COLUMN scope_json;
-- +goose StatementEnd
