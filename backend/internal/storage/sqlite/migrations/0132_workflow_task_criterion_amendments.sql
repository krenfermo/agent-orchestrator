-- An acceptance criterion can go stale. Give AO a way to say so.
--
-- A planned task's acceptance criteria are written once, when the plan is
-- accepted, and are then read as absolute truth: the reviewer is handed them
-- verbatim and told to judge strictly against them. That is right almost
-- always, and wrong in one specific way -- a criterion that describes a
-- PRECONDITION OF THE ENVIRONMENT rather than a property of the work.
--
-- The incident that forced this: master run wf-872e7f57's Task 8 was required
-- to leave `backend/internal/postrunqa/*.go` "modified-but-uncommitted",
-- because they were uncommitted when the objective was written. Six hours
-- later a person committed them, in full, as 70296042b. Nothing was lost and
-- nothing was wrong with the work -- but the criterion had become impossible
-- to satisfy, and the reviewer correctly kept blocking on it. AO had exactly
-- three ways out: edit SQLite by hand, replan the whole master (destroying
-- seven completed tasks), or fabricate a dirty working tree. All three are
-- worse than the problem.
--
-- So the criterion becomes amendable, under conditions strict enough that the
-- mechanism cannot become a way to talk the reviewer out of a real finding:
--
--   * A human must approve it. approved_by is NOT NULL and non-empty, and the
--     application refuses the amendment without it. An agent cannot amend its
--     own acceptance criteria.
--   * The original text is kept forever. This table is append-only; the task
--     row carries the CURRENT criteria and this carries how they got that way.
--   * A reason and at least one piece of evidence are required. "It was
--     inconvenient" is not an amendment; "the state it describes was committed
--     in 70296042b" is.
--   * The work must be reviewed again afterwards, by a fresh independent
--     review. Amending a criterion invalidates the verdict that was reached
--     under the old one -- see requireFreshReviewAfterAmendment.
--
-- Two dispositions, because they are different claims. 'amended' replaces the
-- criterion with a new one that must still be met. 'declared_obsolete' removes
-- it, and is only honest when the criterion describes something outside the
-- work entirely.
--
-- NOTE for a future rebuild of workflow_tasks: this is now the FOURTH table
-- referencing it ON DELETE CASCADE (after workflow_task_dependencies,
-- workflow_task_relationships and workflow_task_worktrees). Migration 0130's
-- comment explains why that matters -- foreign keys are enforced while goose
-- runs and PRAGMA foreign_keys is ignored inside its transaction, so a naive
-- `DROP TABLE workflow_tasks` cascades all four away. Park this one too.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE workflow_task_criterion_amendments (
    id               TEXT PRIMARY KEY,
    workflow_run_id  TEXT NOT NULL REFERENCES workflow_runs(id) ON DELETE CASCADE,
    task_id          TEXT NOT NULL REFERENCES workflow_tasks(id) ON DELETE CASCADE,
    -- Which criterion, as it was indexed at the moment of the amendment, plus
    -- its exact text. The index alone would stop identifying anything as soon
    -- as a later amendment removes an earlier criterion; the text is what makes
    -- the record readable years later.
    criterion_index    INTEGER NOT NULL CHECK (criterion_index >= 0),
    original_criterion TEXT NOT NULL CHECK (length(original_criterion) > 0),
    -- The replacement. Empty exactly when the disposition is declared_obsolete.
    amended_criterion  TEXT NOT NULL DEFAULT '',
    disposition        TEXT NOT NULL CHECK (disposition IN ('amended', 'declared_obsolete')),
    -- Why the criterion no longer describes reality, and what proves it.
    reason        TEXT NOT NULL CHECK (length(reason) > 0),
    evidence_json TEXT NOT NULL DEFAULT '[]'
        CHECK (json_valid(evidence_json) AND json_array_length(evidence_json) > 0),
    -- The person who approved it. The whole legitimacy of the mechanism rests
    -- on this being a human, so it may not be empty.
    approved_by TEXT NOT NULL CHECK (length(approved_by) > 0),
    -- The review run whose verdict the amendment invalidated, when there was
    -- one. Empty for a criterion amended before any review happened.
    superseded_review_run_id TEXT NOT NULL DEFAULT '',
    created_at               TIMESTAMP NOT NULL,
    -- The two dispositions are different claims, and each constrains the
    -- replacement text: declaring a criterion obsolete removes it, amending one
    -- must actually say what now has to be met.
    CHECK ((disposition = 'declared_obsolete' AND length(amended_criterion) = 0)
        OR (disposition = 'amended' AND length(amended_criterion) > 0))
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_workflow_task_criterion_amendments_task
    ON workflow_task_criterion_amendments(task_id, created_at);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_workflow_task_criterion_amendments_run
    ON workflow_task_criterion_amendments(workflow_run_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_workflow_task_criterion_amendments_run;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_workflow_task_criterion_amendments_task;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS workflow_task_criterion_amendments;
-- +goose StatementEnd
