-- +goose Up
-- +goose StatementBegin
-- Checkpoint 8K-A: durable question detection + classification + policy
-- resolution. Follows AO's "persist facts, derive state" convention (see
-- 0094's comment, and 0103's agent_health_events): a worker's stuck-on-a-
-- question moment is captured once as a durable row, classified
-- deterministically at insert time (never left "persisted but
-- unclassified"), and driven to resolution (policy or human) without ever
-- storing the full pane transcript/chain-of-thought — only bounded
-- evidence metadata about how the question text was captured.
CREATE TABLE workflow_questions (
    id                     TEXT PRIMARY KEY,
    workflow_run_id        TEXT NOT NULL,
    workflow_step_id       TEXT,
    workflow_attempt_id    TEXT,
    session_id             TEXT,
    asking_harness         TEXT,
    asking_role            TEXT,
    fingerprint            TEXT NOT NULL,
    question_text          TEXT NOT NULL DEFAULT '',
    structured_choices     TEXT,
    capture_provider       TEXT,
    capture_parser_version TEXT,
    capture_range_lines    INTEGER,
    certainty              TEXT CHECK (certainty IN ('actual','inferred','unknown')),
    classification         TEXT CHECK (classification IN ('policy_resolvable','auto_resolvable','human_required','ambiguous')),
    classification_reason  TEXT,
    state                  TEXT CHECK (state IN ('pending','resolving','answered','human_required','cancelled')),
    created_at             TIMESTAMP NOT NULL,
    answered_at            TIMESTAMP,
    answer_source          TEXT,
    answer_text            TEXT,
    answer_reference       TEXT,
    delivered              INTEGER NOT NULL DEFAULT 0,
    delivered_at           TIMESTAMP
);
CREATE UNIQUE INDEX idx_workflow_questions_fingerprint ON workflow_questions (fingerprint);
CREATE INDEX idx_workflow_questions_run_state ON workflow_questions (workflow_run_id, state);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS workflow_questions;
-- +goose StatementEnd
