-- +goose Up
-- +goose StatementBegin
-- P3-E: attribute the usage ledger to the work that spent it, in time.
--
-- WHAT WAS ALREADY TRUE. migration 0052 built a durable, exactly-once token
-- ledger: model_usage_events, keyed UNIQUE (binding_id, source_event_key), fed
-- by a cursor-and-parser-state pipeline that survives restart because the
-- cursor is durable and the key is derived from the artifact, never from the
-- clock. Nothing here changes that. The ledger stays the source of truth and
-- stays keyed exactly as it was.
--
-- WHAT WAS MISSING, AND THE BUG IT CAUSED. The ledger knows a session. It did
-- not know which ROLE inside that session spent a token. AO deliberately sends
-- a repair prompt into the worker's own session (fix_dispatch.go: Send targets
-- the worker session), so worker and fix_worker share one AO session, one
-- harness, and one native root -- therefore one usage binding. The 8J read
-- model asked "what did this step's session use?" once per step and added the
-- answers up (workflow_usage_view.go's sumKnownTokens), so a run with one
-- worker step and two fix steps reported the same session's tokens three
-- times. The totals were not merely unattributed, they were inflated.
--
-- THE FIX IS A TIME WINDOW, NOT A COLUMN ON THE EVENT. A usage event cannot
-- carry a role: it is parsed out of a provider transcript that has never heard
-- of AO's roles, and stamping one at ingest would be a guess that a later
-- re-read could not correct. What AO does know, durably and at the moment it
-- happens, is when it handed a role a session. So a role is recorded as a
-- half-open window over one session's timeline, and an event belongs to the
-- last window opened at or before the event's own observed time. Exactly one
-- window per event, so a total cannot double count; derived at read time, so
-- a restart cannot duplicate it and a corrected window fixes every past total.
--
-- observed_at is the provider's own event time, read from the transcript
-- record. It is nullable because an artifact that carried none must not be
-- given an invented one: such an event falls back to the session's earliest
-- window and the read model reports that attribution as approximate rather
-- than exact. recorded_at is when AO ingested it, which is what a period
-- ("last 7 days") can fall back to when observed_at is absent.
--
-- The CREATE .. IF NOT EXISTS block below is a repair, not a schema change. A
-- database whose 0052 was recorded-but-skipped (migrate_test.go seeds exactly
-- that shape) has no usage tables at all, and an unconditional ALTER against a
-- missing table fails the whole migration chain. Re-declaring 0052's own
-- compact shape here costs nothing on a healthy database -- every statement is
-- a no-op -- and lets a skipped-0052 database reach this version instead of
-- being stranded. It deliberately mirrors 0052's definitions exactly; the two
-- added columns are appended by the ALTERs that follow, so both paths converge
-- on one shape.
CREATE TABLE IF NOT EXISTS usage_bindings (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id         TEXT NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    harness            TEXT NOT NULL CHECK (harness IN ('claude-code', 'codex')),
    native_root_id     TEXT NOT NULL CHECK (trim(native_root_id) <> ''),
    initial_model_id   TEXT NOT NULL DEFAULT '',
    state              TEXT NOT NULL CHECK (state IN ('discovering', 'active', 'finalizing', 'complete', 'partial')),
    last_error_code    TEXT NOT NULL DEFAULT '',
    updated_at         TIMESTAMP NOT NULL,
    UNIQUE (session_id, harness, native_root_id)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS usage_sources (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    binding_id          INTEGER NOT NULL REFERENCES usage_bindings (id) ON DELETE CASCADE,
    kind                TEXT NOT NULL CHECK (kind IN ('claude_main', 'claude_subagent', 'codex_rollout')),
    native_session_id   TEXT NOT NULL DEFAULT '',
    subagent_id         TEXT NOT NULL DEFAULT '',
    artifact_path       TEXT NOT NULL CHECK (trim(artifact_path) <> ''),
    file_identity       TEXT NOT NULL DEFAULT '',
    generation          INTEGER NOT NULL DEFAULT 0 CHECK (generation >= 0),
    byte_offset         INTEGER NOT NULL DEFAULT 0 CHECK (byte_offset >= 0),
    parser_state_json   TEXT NOT NULL DEFAULT '{}',
    state               TEXT NOT NULL CHECK (state IN ('pending', 'active', 'complete', 'error')),
    failure_count       INTEGER NOT NULL DEFAULT 0 CHECK (failure_count >= 0),
    anomaly_count       INTEGER NOT NULL DEFAULT 0 CHECK (anomaly_count >= 0),
    next_retry_at       TIMESTAMP,
    last_error_code     TEXT NOT NULL DEFAULT '',
    updated_at          TIMESTAMP NOT NULL,
    UNIQUE (binding_id, artifact_path, generation)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS model_usage_events (
    id                      INTEGER PRIMARY KEY AUTOINCREMENT,
    binding_id              INTEGER NOT NULL REFERENCES usage_bindings (id) ON DELETE CASCADE,
    usage_source_id         INTEGER NOT NULL REFERENCES usage_sources (id) ON DELETE CASCADE,
    model_id                TEXT NOT NULL CHECK (trim(model_id) <> ''),
    input_tokens            INTEGER NOT NULL CHECK (input_tokens >= 0),
    uncached_input_tokens   INTEGER NOT NULL CHECK (uncached_input_tokens >= 0 AND uncached_input_tokens <= input_tokens),
    cache_read_tokens       INTEGER NOT NULL CHECK (cache_read_tokens >= 0 AND cache_read_tokens <= input_tokens),
    cache_write_tokens      INTEGER NOT NULL CHECK (cache_write_tokens >= 0 AND cache_write_tokens <= input_tokens),
    output_tokens           INTEGER NOT NULL CHECK (output_tokens >= 0),
    reasoning_tokens        INTEGER CHECK (reasoning_tokens IS NULL OR (reasoning_tokens >= 0 AND reasoning_tokens <= output_tokens)),
    source_event_key        TEXT NOT NULL CHECK (trim(source_event_key) <> ''),
    UNIQUE (binding_id, source_event_key)
);
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE model_usage_events ADD COLUMN observed_at TIMESTAMP;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE model_usage_events ADD COLUMN recorded_at TIMESTAMP;
-- +goose StatementEnd

-- +goose StatementBegin
-- One role's claim on one session's timeline.
--
-- dedupe_key is the whole idempotency story. A window is opened by a dispatch,
-- and a dispatch is retried: by failover, by the resume path after a restart,
-- by a wake. Opening is therefore INSERT .. ON CONFLICT(dedupe_key) DO NOTHING
-- against a key derived from the durable obligation (run, step, attempt, role,
-- cycle) and never from the clock, so replaying a dispatch re-opens the same
-- window rather than splitting a role's tokens across two.
--
-- session_id is TEXT with no foreign key on purpose. A window may be opened
-- for a reviewer or a decision resolver, and those run in runtime panes that
-- are not rows in `sessions` (see workflow_reviewer_launcher.go). Such a
-- window resolves to no usage binding at all, which is exactly how the read
-- model learns to report "not observable" instead of zero.
CREATE TABLE usage_attribution_windows (
    id                     INTEGER PRIMARY KEY AUTOINCREMENT,
    dedupe_key             TEXT NOT NULL UNIQUE CHECK (trim(dedupe_key) <> ''),
    session_id             TEXT NOT NULL CHECK (trim(session_id) <> ''),
    project_id             TEXT NOT NULL DEFAULT '',
    workflow_run_id        TEXT NOT NULL DEFAULT '',
    parent_workflow_run_id TEXT NOT NULL DEFAULT '',
    task_id                TEXT NOT NULL DEFAULT '',
    workflow_step_id       TEXT NOT NULL DEFAULT '',
    attempt_id             TEXT NOT NULL DEFAULT '',
    -- The failover hop this window belongs to, so a failed provider attempt
    -- keeps its own tokens instead of having them folded into its successor.
    attempt_ordinal        INTEGER NOT NULL DEFAULT 0,
    -- The repair cycle, 0 for base execution. This is what lets a run answer
    -- "base execution 40k, repair +18k" without a second ledger.
    cycle                  INTEGER NOT NULL DEFAULT 0,
    role                   TEXT NOT NULL CHECK (trim(role) <> ''),
    harness                TEXT NOT NULL DEFAULT '',
    provider               TEXT NOT NULL DEFAULT '',
    model                  TEXT NOT NULL DEFAULT '',
    opened_at              TIMESTAMP NOT NULL,
    created_at             TIMESTAMP NOT NULL
);
-- +goose StatementEnd

-- The resolution lookup: newest window at or before an event's observed time.
-- +goose StatementBegin
CREATE INDEX idx_usage_windows_session_opened
    ON usage_attribution_windows (session_id, opened_at DESC, id DESC);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_usage_windows_run ON usage_attribution_windows (workflow_run_id, role);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_usage_windows_project ON usage_attribution_windows (project_id, opened_at);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_usage_windows_parent ON usage_attribution_windows (parent_workflow_run_id)
    WHERE parent_workflow_run_id <> '';
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_model_usage_events_observed ON model_usage_events (observed_at);
-- +goose StatementEnd

-- +goose StatementBegin
-- The attribution itself, derived and never stored.
--
-- Every event of a session that has at least one window resolves to exactly
-- one window: the newest opened at or before its observed time, or -- when it
-- has no observed time, or predates every window -- the session's earliest.
-- "exact" vs "approximate" is carried out to the caller rather than smoothed
-- over, because a role breakdown built from fallbacks is a weaker claim than
-- one built from timestamps and must be labeled as one.
CREATE VIEW usage_event_attribution AS
SELECT
    e.id                    AS event_id,
    e.binding_id            AS binding_id,
    b.session_id            AS session_id,
    b.harness               AS harness,
    e.model_id              AS model_id,
    e.input_tokens          AS input_tokens,
    e.uncached_input_tokens AS uncached_input_tokens,
    e.cache_read_tokens     AS cache_read_tokens,
    e.cache_write_tokens    AS cache_write_tokens,
    e.output_tokens         AS output_tokens,
    e.reasoning_tokens      AS reasoning_tokens,
    e.observed_at           AS observed_at,
    e.recorded_at           AS recorded_at,
    COALESCE(
        (SELECT w.id FROM usage_attribution_windows w
          WHERE w.session_id = b.session_id
            AND e.observed_at IS NOT NULL
            AND w.opened_at <= e.observed_at
          ORDER BY w.opened_at DESC, w.id DESC
          LIMIT 1),
        (SELECT w2.id FROM usage_attribution_windows w2
          WHERE w2.session_id = b.session_id
          ORDER BY w2.opened_at ASC, w2.id ASC
          LIMIT 1)
    ) AS window_id,
    CASE WHEN e.observed_at IS NOT NULL AND EXISTS (
            SELECT 1 FROM usage_attribution_windows w3
             WHERE w3.session_id = b.session_id AND w3.opened_at <= e.observed_at
         ) THEN 'exact' ELSE 'approximate' END AS attribution_basis
FROM model_usage_events e
JOIN usage_bindings b ON b.id = e.binding_id;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP VIEW IF EXISTS usage_event_attribution;
DROP TABLE IF EXISTS usage_attribution_windows;
-- Older supported SQLite versions cannot drop a column safely; the two added
-- columns are nullable and ignored by older binaries.
-- +goose StatementEnd
