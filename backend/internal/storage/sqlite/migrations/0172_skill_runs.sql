-- Skills Frente 2 / 2B: a skill run is a durable record.
--
-- Until now POST /projects/{id}/skills/{skillId}/run held the HTTP request open
-- for the whole container run and returned the report in the response body.
-- Nothing kept it: a closed tab, a daemon restart or a second click lost the
-- result, and the only trace was a one-line run_executed row in skill_audit.
--
-- Two tables, because two different questions are asked of a run:
--
--   skill_runs          what was run, under which authorization, by which
--                       runner, and how it ended -- one row per accepted run
--   skill_run_findings  what it found, one row per finding, so a finding can be
--                       listed, filtered and counted without decoding a blob
--
-- The report is ALSO kept whole (report_json) with its SHA-256. The normalized
-- rows answer queries; the report is the evidence, including the coverage that
-- says what was NOT read and the boundary evidence that says how it ran. A
-- finding table alone would lose both, and a report alone would make every
-- "how many high findings" a JSON scan. report_sha256 is computed over the
-- exact bytes stored, so a reader can prove the report was not edited after
-- the run ended.
--
-- The state machine (service/skills/runs.go is the one writer):
--
--   queued -> running -> succeeded | failed | refused | cancelled
--   queued ->            failed | refused | cancelled
--
-- A row exists only for a run AO ACCEPTED. A request refused before acceptance
-- (skill not activated, capability not granted, mode not executable, no runner)
-- gets a 4xx and a run_refused audit row, as before, and no run: there was
-- never anything to execute.
--
--   refused    the execution boundary declined to launch the run or to trust
--              its output (image not approved or revoked, staging unusable,
--              isolation not demonstrated). Nothing an operator should retry
--              without changing something.
--   failed     execution was attempted and produced no result AO can stand
--              behind (runtime error, timeout, invalid report, or the daemon
--              that owned the run stopped while it was queued or running).
--   cancelled  a person asked for it to stop.
--
-- There is deliberately no foreign key to skill_installs: a run is history, and
-- the most interesting runs are the ones for a version since uninstalled. The
-- project foreign key cascades, like skill_activations: a run has no meaning
-- without the project it scanned.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE skill_runs (
    id                TEXT      PRIMARY KEY,
    project_id        TEXT      NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    skill_id          TEXT      NOT NULL,
    skill_version     TEXT      NOT NULL,
    mode_id           TEXT      NOT NULL,
    -- The AO tool that implements the mode (ao.static-scan/v1). Recorded
    -- because the mode-to-tool map is code and can change between releases.
    tool              TEXT      NOT NULL DEFAULT '',
    state             TEXT      NOT NULL CHECK (state IN
                          ('queued','running','succeeded','failed','refused','cancelled')),
    -- Optional caller-supplied key. A retry carrying the same key gets the same
    -- run back instead of a second one (see the unique index below).
    idempotency_key   TEXT,
    requested_by      TEXT      NOT NULL DEFAULT '',
    -- The manifest's DECLARED inputs after validation. Never a secret: the
    -- catalog's input types are enum/string/list and secrets travel through a
    -- separate delivery that this path does not enable.
    inputs_json       TEXT      NOT NULL DEFAULT '{}',
    -- The capabilities the authorization decision actually granted for this
    -- mode, not the activation's whole grant.
    capabilities_json TEXT      NOT NULL DEFAULT '[]',
    -- The package digest the activation resolved to when the run was accepted.
    package_digest    TEXT      NOT NULL DEFAULT '',
    -- AO's own attestation at acceptance: which runner, which controls.
    runner_id         TEXT      NOT NULL DEFAULT '',
    runner_controls   TEXT      NOT NULL DEFAULT '[]',
    -- The daemon instance that accepted the run and is the only one allowed to
    -- execute it. A different instance finding it non-terminal knows its owner
    -- is gone.
    owner_instance    TEXT      NOT NULL,
    -- Filled from the runner's report once the image is resolved.
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

    -- Terminality is a fact of the row, not something a reader infers: a
    -- terminal state always has finished_at and a non-terminal one never does.
    CHECK ((state IN ('succeeded','failed','refused','cancelled')) = (finished_at IS NOT NULL)),
    -- A running or finished-by-execution run has started; a queued one has not.
    CHECK (state <> 'queued' OR started_at IS NULL),
    CHECK (state NOT IN ('running','succeeded') OR started_at IS NOT NULL),
    -- Only a succeeded run carries a report, and it always carries its digest.
    CHECK ((state = 'succeeded') = (report_json IS NOT NULL)),
    CHECK ((report_json IS NULL) = (report_sha256 = '')),
    -- A failed or refused run always says why.
    CHECK (state NOT IN ('failed','refused') OR error_code <> '')
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_skill_runs_project ON skill_runs (project_id, created_at DESC);
-- +goose StatementEnd

-- +goose StatementBegin
-- Non-terminal runs are what a restarting daemon has to account for.
CREATE INDEX idx_skill_runs_active ON skill_runs (state) WHERE state IN ('queued','running');
-- +goose StatementEnd

-- +goose StatementBegin
-- At most one in-flight run per (project, skill, mode). A double click or a
-- client retry without a key reaches the SAME run instead of a second
-- container over the same checkout. Enforced by the schema, not only by the
-- service that usually checks it first.
CREATE UNIQUE INDEX uq_skill_runs_single_flight
    ON skill_runs (project_id, skill_id, mode_id)
    WHERE state IN ('queued','running');
-- +goose StatementEnd

-- +goose StatementBegin
CREATE UNIQUE INDEX uq_skill_runs_idempotency
    ON skill_runs (project_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE skill_run_findings (
    run_id         TEXT    NOT NULL REFERENCES skill_runs(id) ON DELETE CASCADE,
    ordinal        INTEGER NOT NULL CHECK (ordinal >= 0),
    rule_id        TEXT    NOT NULL,
    severity       TEXT    NOT NULL,
    category       TEXT    NOT NULL DEFAULT '',
    title          TEXT    NOT NULL DEFAULT '',
    -- Repo-relative. A finding carries the rule and the location, never the
    -- matched text (skillrunner.ScanFinding), so nothing here can hold the
    -- secret a rule was written to find.
    path           TEXT    NOT NULL DEFAULT '',
    line           INTEGER NOT NULL DEFAULT 0,
    recommendation TEXT    NOT NULL DEFAULT '',
    confidence     TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (run_id, ordinal)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_skill_run_findings_severity ON skill_run_findings (run_id, severity);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS skill_run_findings;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE IF EXISTS skill_runs;
-- +goose StatementEnd
