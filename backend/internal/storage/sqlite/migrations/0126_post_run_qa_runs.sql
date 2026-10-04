-- Durable envelope for the Post-Run QA gate: the automated check-and-repair
-- pass that runs after a task's or a workflow's work looks finished, but
-- before AO reports it complete.
--
-- The gate is resumable, so its state has to be durable in the same database
-- as every other lifecycle fact. One row here is one pass of the gate over one
-- subject. The Go-side state model lives in internal/postrunqa; this table is
-- its only storage.
--
-- Design notes:
--
--   * (subject_kind, subject_id) is polymorphic across two lifecycle tables --
--     'task' points at workflow_tasks.id, 'workflow' at workflow_runs.id --
--     so it deliberately carries no foreign key. The non-unique index below
--     is what makes the "latest pass for this subject" lookup cheap.
--
--   * A subject re-entering the gate gets a NEW row rather than overwriting
--     the previous pass, so the history of what the gate decided about a
--     subject survives a retry. Hence no unique constraint on the subject
--     pair, and a started_at-ordered "latest" query.
--
--   * findings_json holds the whole finding list as one JSON document rather
--     than a child table. Findings are only ever read and written as a set,
--     with the run that produced them, and are never queried across runs --
--     the same reasoning that keeps workflow_question_resolutions'
--     evidence_references and workflow_tasks' acceptance_criteria_json inline.
--
--   * max_repair_cycles is stored per row, not read from a constant at load
--     time, so a pass that started under one budget keeps that budget for its
--     whole life even if the default changes underneath it. It defaults to 2
--     here and in Go (postrunqa.DefaultMaxRepairCycles); the Go default also
--     covers a row whose column somehow holds 0.
--
--   * result is '' until the run reaches a terminal phase. It is stored rather
--     than derived from phase so a reader gets the verdict without having to
--     know which phases count as success.
--
-- No CDC trigger: nothing surfaces gate state over SSE yet. One is added when
-- a UI reads it, together with the projection that needs it.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE post_run_qa_runs (
    id                 TEXT PRIMARY KEY,
    subject_kind       TEXT NOT NULL CHECK (subject_kind IN ('task','workflow')),
    subject_id         TEXT NOT NULL,
    phase              TEXT NOT NULL CHECK (phase IN (
        'pending',
        'checking',
        'auto_fixing',
        'clean',
        'needs_attention'
    )),
    findings_json      TEXT NOT NULL DEFAULT '[]',
    repair_cycle_count INTEGER NOT NULL DEFAULT 0,
    max_repair_cycles  INTEGER NOT NULL DEFAULT 2,
    result             TEXT NOT NULL DEFAULT '' CHECK (result IN ('','clean','needs_attention')),
    started_at         TIMESTAMP NOT NULL,
    -- NULL until the run reaches a terminal phase.
    completed_at       TIMESTAMP
);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX post_run_qa_runs_subject_idx
    ON post_run_qa_runs (subject_kind, subject_id, started_at DESC);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS post_run_qa_runs_subject_idx;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS post_run_qa_runs;
-- +goose StatementEnd
