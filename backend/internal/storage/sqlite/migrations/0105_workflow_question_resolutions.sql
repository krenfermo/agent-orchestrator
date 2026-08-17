-- Checkpoint 8K-B (pass 1 of 3): foundation for the cross-provider Decision
-- Resolver. workflow_question_resolutions holds one row per resolution
-- attempt for a workflow_question classified auto_resolvable (0104 reserved
-- that classification value; this is the first checkpoint that emits it).
-- Mirrors review_run's status shape (0012_add_review_tables.sql): a
-- resolution attempt has its own lifecycle distinct from the question it
-- answers, because a question may need more than one attempt (provider
-- unavailable, resolver crash, staleness sweep) before it resolves. As with
-- 0094/0103/0104, only bounded evidence metadata is persisted — never a full
-- transcript.
--
-- The partial unique index below enforces "at most one running resolution
-- attempt per question" at the SQL layer, so a duplicate dispatch race can
-- never create two concurrent resolver sessions for the same question.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE workflow_question_resolutions (
    id                  TEXT PRIMARY KEY,
    workflow_question_id TEXT NOT NULL,
    workflow_run_id     TEXT NOT NULL,
    asking_session_id   TEXT,
    resolver_harness    TEXT NOT NULL,
    -- Nullable until the resolver session is actually launched (pass 2);
    -- the resolution row is created first so the CAS/uniqueness guard below
    -- applies from the moment dispatch is attempted, not just once a
    -- session id exists.
    resolver_session_id TEXT,
    status              TEXT NOT NULL CHECK (status IN ('pending','running','complete','failed','cancelled')),
    answer              TEXT,
    reason_summary      TEXT,
    -- JSON array of bounded evidence references (file paths / line ranges /
    -- command output pointers), never raw transcript text.
    evidence_references TEXT,
    certainty           TEXT CHECK (certainty IN ('actual','inferred','unknown')),
    requires_human      INTEGER NOT NULL DEFAULT 0,
    created_at          TIMESTAMP NOT NULL,
    updated_at          TIMESTAMP NOT NULL,
    completed_at        TIMESTAMP
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE UNIQUE INDEX idx_workflow_question_resolutions_active
    ON workflow_question_resolutions (workflow_question_id)
    WHERE status = 'running';
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_workflow_question_resolutions_question_status
    ON workflow_question_resolutions (workflow_question_id, status);
-- +goose StatementEnd

-- +goose StatementBegin
-- Current pointer to the live resolution attempt for a question. Plain ADD
-- COLUMN: this does not touch workflow_questions' existing CHECK
-- constraints (state/classification), so it is a metadata-only change that
-- does not require a table rebuild.
ALTER TABLE workflow_questions ADD COLUMN resolving_run_id TEXT;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE workflow_questions DROP COLUMN resolving_run_id;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE IF EXISTS workflow_question_resolutions;
-- +goose StatementEnd
