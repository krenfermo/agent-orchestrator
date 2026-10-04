-- Skills Frente 2 / 2E: a full security audit is a PARENT run of child runs.
--
-- A full audit runs static-code, secret-scan, dependencies and authz-review,
-- each as its OWN skill run -- its own tool, runner, attestation, image
-- approval, report and digest, exactly as 2B/2C/2D execute them -- and a parent
-- run that launches them in sequence and consolidates their verified reports.
-- Two things the 0172 schema cannot say are needed for that, and both are
-- here:
--
--   parent_run_id   which audit a child run belongs to. NULL for every run
--                   that is not part of an audit, which is every run 0172 ever
--                   stored. No foreign key: a run is history (0172's rule), and
--                   the parent and its children are in the same table and
--                   deleted together by the project cascade.
--
--   state 'partial' the audit ended with a consolidated report, but not every
--                   mode it planned produced a verified result. It is a
--                   TERMINAL state of its own, never 'succeeded': a partial
--                   scan announced as a completed audit is the one outcome
--                   this feature must not produce. Like succeeded it carries a
--                   report and its digest; like failed it always says why.
--
-- WHY skill_runs IS REBUILT, WITH PRAGMA foreign_keys=OFF.
-- The state CHECK and the report invariants have to change, SQLite has no ALTER
-- for a CHECK, and skill_run_findings references skill_runs ON DELETE CASCADE.
-- SQLite treats DROP TABLE as deleting every row for foreign-key purposes, so a
-- naive rebuild would cascade away every stored finding. The pragma is set
-- together with the goose no-transaction annotation, because inside goose's
-- transaction it is silently ignored. The rebuild is declared in
-- migrate_rebuild_fk_safety_test.go. skill_runs has no triggers and no views.

-- +goose NO TRANSACTION

-- +goose Up
-- +goose StatementBegin
PRAGMA foreign_keys=OFF;

CREATE TABLE skill_runs_2e (
    id                TEXT      PRIMARY KEY,
    project_id        TEXT      NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    skill_id          TEXT      NOT NULL,
    skill_version     TEXT      NOT NULL,
    mode_id           TEXT      NOT NULL,
    tool              TEXT      NOT NULL DEFAULT '',
    state             TEXT      NOT NULL CHECK (state IN
                          ('queued','running','succeeded','partial','failed','refused','cancelled')),
    idempotency_key   TEXT,
    requested_by      TEXT      NOT NULL DEFAULT '',
    inputs_json       TEXT      NOT NULL DEFAULT '{}',
    capabilities_json TEXT      NOT NULL DEFAULT '[]',
    package_digest    TEXT      NOT NULL DEFAULT '',
    runner_id         TEXT      NOT NULL DEFAULT '',
    runner_controls   TEXT      NOT NULL DEFAULT '[]',
    owner_instance    TEXT      NOT NULL,
    image_digest      TEXT      NOT NULL DEFAULT '',
    approval_id       TEXT      NOT NULL DEFAULT '',
    approved_by       TEXT      NOT NULL DEFAULT '',
    summary           TEXT      NOT NULL DEFAULT '',
    finding_count     INTEGER   NOT NULL DEFAULT 0 CHECK (finding_count >= 0),
    truncated         INTEGER   NOT NULL DEFAULT 0 CHECK (truncated IN (0,1)),
    report_json       TEXT,
    report_sha256     TEXT      NOT NULL DEFAULT '',
    error_code        TEXT      NOT NULL DEFAULT '',
    error_message     TEXT      NOT NULL DEFAULT '',
    cancel_requested  INTEGER   NOT NULL DEFAULT 0 CHECK (cancel_requested IN (0,1)),
    created_at        TIMESTAMP NOT NULL,
    started_at        TIMESTAMP,
    finished_at       TIMESTAMP,
    updated_at        TIMESTAMP NOT NULL,
    -- The audit this run is a child of; NULL when it is not part of one.
    parent_run_id     TEXT,

    CHECK ((state IN ('succeeded','partial','failed','refused','cancelled')) = (finished_at IS NOT NULL)),
    CHECK (state <> 'queued' OR started_at IS NULL),
    CHECK (state NOT IN ('running','succeeded','partial') OR started_at IS NOT NULL),
    -- A succeeded or partial run carries a report, and always its digest.
    CHECK ((state IN ('succeeded','partial')) = (report_json IS NOT NULL)),
    CHECK ((report_json IS NULL) = (report_sha256 = '')),
    -- A failed, refused or PARTIAL run always says why.
    CHECK (state NOT IN ('failed','refused','partial') OR error_code <> ''),
    -- A run is not its own parent.
    CHECK (parent_run_id IS NULL OR parent_run_id <> id)
);

INSERT INTO skill_runs_2e (
    id, project_id, skill_id, skill_version, mode_id, tool, state, idempotency_key,
    requested_by, inputs_json, capabilities_json, package_digest, runner_id,
    runner_controls, owner_instance, image_digest, approval_id, approved_by, summary,
    finding_count, truncated, report_json, report_sha256, error_code, error_message,
    cancel_requested, created_at, started_at, finished_at, updated_at
)
SELECT
    id, project_id, skill_id, skill_version, mode_id, tool, state, idempotency_key,
    requested_by, inputs_json, capabilities_json, package_digest, runner_id,
    runner_controls, owner_instance, image_digest, approval_id, approved_by, summary,
    finding_count, truncated, report_json, report_sha256, error_code, error_message,
    cancel_requested, created_at, started_at, finished_at, updated_at
FROM skill_runs;

DROP TABLE skill_runs;
ALTER TABLE skill_runs_2e RENAME TO skill_runs;

CREATE INDEX idx_skill_runs_project ON skill_runs (project_id, created_at DESC);
CREATE INDEX idx_skill_runs_active ON skill_runs (state) WHERE state IN ('queued','running');
CREATE UNIQUE INDEX uq_skill_runs_single_flight
    ON skill_runs (project_id, skill_id, mode_id)
    WHERE state IN ('queued','running');
CREATE UNIQUE INDEX uq_skill_runs_idempotency
    ON skill_runs (project_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;
CREATE INDEX idx_skill_runs_parent ON skill_runs (parent_run_id) WHERE parent_run_id IS NOT NULL;

PRAGMA foreign_keys=ON;
PRAGMA foreign_key_check;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
PRAGMA foreign_keys=OFF;

-- A partial run has no representation in 0172; it goes back as failed, which
-- keeps its reason and loses its report, as 0172 requires of a failed run.
CREATE TABLE skill_runs_2b (
    id                TEXT      PRIMARY KEY,
    project_id        TEXT      NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    skill_id          TEXT      NOT NULL,
    skill_version     TEXT      NOT NULL,
    mode_id           TEXT      NOT NULL,
    tool              TEXT      NOT NULL DEFAULT '',
    state             TEXT      NOT NULL CHECK (state IN
                          ('queued','running','succeeded','failed','refused','cancelled')),
    idempotency_key   TEXT,
    requested_by      TEXT      NOT NULL DEFAULT '',
    inputs_json       TEXT      NOT NULL DEFAULT '{}',
    capabilities_json TEXT      NOT NULL DEFAULT '[]',
    package_digest    TEXT      NOT NULL DEFAULT '',
    runner_id         TEXT      NOT NULL DEFAULT '',
    runner_controls   TEXT      NOT NULL DEFAULT '[]',
    owner_instance    TEXT      NOT NULL,
    image_digest      TEXT      NOT NULL DEFAULT '',
    approval_id       TEXT      NOT NULL DEFAULT '',
    approved_by       TEXT      NOT NULL DEFAULT '',
    summary           TEXT      NOT NULL DEFAULT '',
    finding_count     INTEGER   NOT NULL DEFAULT 0 CHECK (finding_count >= 0),
    truncated         INTEGER   NOT NULL DEFAULT 0 CHECK (truncated IN (0,1)),
    report_json       TEXT,
    report_sha256     TEXT      NOT NULL DEFAULT '',
    error_code        TEXT      NOT NULL DEFAULT '',
    error_message     TEXT      NOT NULL DEFAULT '',
    cancel_requested  INTEGER   NOT NULL DEFAULT 0 CHECK (cancel_requested IN (0,1)),
    created_at        TIMESTAMP NOT NULL,
    started_at        TIMESTAMP,
    finished_at       TIMESTAMP,
    updated_at        TIMESTAMP NOT NULL,
    CHECK ((state IN ('succeeded','failed','refused','cancelled')) = (finished_at IS NOT NULL)),
    CHECK (state <> 'queued' OR started_at IS NULL),
    CHECK (state NOT IN ('running','succeeded') OR started_at IS NOT NULL),
    CHECK ((state = 'succeeded') = (report_json IS NOT NULL)),
    CHECK ((report_json IS NULL) = (report_sha256 = '')),
    CHECK (state NOT IN ('failed','refused') OR error_code <> '')
);

INSERT INTO skill_runs_2b (
    id, project_id, skill_id, skill_version, mode_id, tool, state, idempotency_key,
    requested_by, inputs_json, capabilities_json, package_digest, runner_id,
    runner_controls, owner_instance, image_digest, approval_id, approved_by, summary,
    finding_count, truncated, report_json, report_sha256, error_code, error_message,
    cancel_requested, created_at, started_at, finished_at, updated_at
)
SELECT
    id, project_id, skill_id, skill_version, mode_id, tool,
    CASE WHEN state = 'partial' THEN 'failed' ELSE state END,
    idempotency_key, requested_by, inputs_json, capabilities_json, package_digest,
    runner_id, runner_controls, owner_instance, image_digest, approval_id, approved_by,
    summary, finding_count, truncated,
    CASE WHEN state = 'partial' THEN NULL ELSE report_json END,
    CASE WHEN state = 'partial' THEN '' ELSE report_sha256 END,
    error_code, error_message, cancel_requested, created_at, started_at, finished_at, updated_at
FROM skill_runs;

DROP TABLE skill_runs;
ALTER TABLE skill_runs_2b RENAME TO skill_runs;

CREATE INDEX idx_skill_runs_project ON skill_runs (project_id, created_at DESC);
CREATE INDEX idx_skill_runs_active ON skill_runs (state) WHERE state IN ('queued','running');
CREATE UNIQUE INDEX uq_skill_runs_single_flight
    ON skill_runs (project_id, skill_id, mode_id)
    WHERE state IN ('queued','running');
CREATE UNIQUE INDEX uq_skill_runs_idempotency
    ON skill_runs (project_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;

PRAGMA foreign_keys=ON;
PRAGMA foreign_key_check;
-- +goose StatementEnd
