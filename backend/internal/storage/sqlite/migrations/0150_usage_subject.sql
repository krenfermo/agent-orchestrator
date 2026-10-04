-- +goose NO TRANSACTION
-- +goose Up
-- +goose StatementBegin
-- P3-E completion: a usage binding's subject is no longer required to be an AO
-- session.
--
-- WHAT THIS FIXES. Three execution roles spend real provider tokens and none of
-- them could be metered, because usage_bindings.session_id was NOT NULL with a
-- foreign key into `sessions`:
--
--   * the REVIEWER runs in a runtime pane (workflow_reviewer_launcher.go),
--   * the DECISION RESOLVER runs in a runtime pane (decision_resolver_launcher.go),
--   * the PLANNER shells out to `claude --print` (adapters/planner/command).
--
-- None is a row in `sessions`, so none could carry a binding, so every token
-- they spent was invisible and every run total was a lower bound. That is the
-- one real gap P3-E's own completion bar named.
--
-- THE SHAPE. A binding now names a SUBJECT: a (kind, id) pair. `session` is one
-- kind among several, and session_id stays as a real column — populated for
-- session subjects, NULL for the rest — so every session-scoped read, index,
-- CDC trigger and foreign key that existed before keeps working unchanged
-- against exactly the rows it always saw.
--
-- WHY A REBUILD, AND WHY IT IS NOT DESTRUCTIVE. SQLite cannot drop a NOT NULL,
-- drop a foreign key, or replace a UNIQUE constraint in place. So the table is
-- rebuilt — the same way migration 0052 rebuilt these very tables — with every
-- existing row copied and, critically, its `id` PRESERVED, so usage_sources and
-- model_usage_events keep pointing at the same bindings. Nothing is dropped,
-- nothing is recomputed, and a database that has only session-backed bindings
-- ends up byte-for-byte equivalent to what it had, plus two columns.
--
-- NO TIMESTAMP IS PART OF ANY IDENTITY HERE. The new uniqueness key is
-- (subject_kind, subject_id, harness, native_root_id) — the same identity as
-- before with `session_id` generalized, so a re-discovered pane binds to the row
-- it already had rather than minting a second one.
PRAGMA foreign_keys=OFF;
BEGIN IMMEDIATE;

CREATE TABLE usage_bindings_p3e (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    -- The generalized subject. `session` keeps session_id populated; every
    -- other kind leaves it NULL and lives entirely on (subject_kind, subject_id).
    --
    -- runtime_pane      a reviewer or decision-resolver pane, identified by the
    --                   durable authority that owns it (a review run id, a
    --                   question-resolution id) -- never by "the most recent
    --                   process", which is exactly the inference this column
    --                   exists to make unnecessary.
    -- planner_invocation one bounded `claude --print` planner call.
    -- provider_attempt   reserved for a future surface that binds usage to a
    --                    provider attempt directly.
    subject_kind       TEXT NOT NULL DEFAULT 'session'
                         CHECK (subject_kind IN ('session', 'runtime_pane', 'planner_invocation', 'provider_attempt')),
    subject_id         TEXT NOT NULL CHECK (trim(subject_id) <> ''),
    -- NULL for a non-session subject. Kept as a real, foreign-keyed column so
    -- every pre-existing session-scoped query, index and CDC trigger keeps
    -- meaning precisely what it meant before.
    session_id         TEXT REFERENCES sessions (id) ON DELETE CASCADE,
    harness            TEXT NOT NULL CHECK (harness IN ('claude-code', 'codex')),
    native_root_id     TEXT NOT NULL CHECK (trim(native_root_id) <> ''),
    initial_model_id   TEXT NOT NULL DEFAULT '',
    state              TEXT NOT NULL CHECK (state IN ('discovering', 'active', 'finalizing', 'complete', 'partial')),
    last_error_code    TEXT NOT NULL DEFAULT '',
    updated_at         TIMESTAMP NOT NULL,
    -- A session subject must carry its session; a non-session subject must not.
    -- Stated as a constraint rather than a convention so no writer can leave a
    -- pane binding half-attached to a session it does not belong to.
    CHECK ((subject_kind = 'session') = (session_id IS NOT NULL)),
    UNIQUE (subject_kind, subject_id, harness, native_root_id)
);

-- Every existing row, id preserved, reclassified as the session subject it
-- always was.
INSERT INTO usage_bindings_p3e
    (id, subject_kind, subject_id, session_id, harness, native_root_id,
     initial_model_id, state, last_error_code, updated_at)
SELECT id, 'session', session_id, session_id, harness, native_root_id,
       initial_model_id, state, last_error_code, updated_at
FROM usage_bindings;

DROP TRIGGER IF EXISTS usage_sources_cdc_update;
DROP TRIGGER IF EXISTS usage_bindings_cdc_update;
DROP TRIGGER IF EXISTS usage_bindings_cdc_insert;
DROP VIEW IF EXISTS usage_event_attribution;
DROP VIEW IF EXISTS usage_session_integrity;
DROP TABLE usage_bindings;
ALTER TABLE usage_bindings_p3e RENAME TO usage_bindings;

CREATE INDEX idx_usage_bindings_session_state ON usage_bindings (session_id, state);
CREATE INDEX idx_usage_bindings_subject ON usage_bindings (subject_kind, subject_id);

-- The CDC triggers are recreated GUARDED. change_log.project_id is NOT NULL and
-- its session_id references `sessions`, so a pane binding firing the old
-- unguarded trigger would either violate that constraint or write a change
-- event about a session that does not exist. A non-session subject has no
-- session to invalidate, so it emits nothing -- which is correct, not a gap.
CREATE TRIGGER usage_bindings_cdc_insert AFTER INSERT ON usage_bindings
WHEN NEW.session_id IS NOT NULL
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    SELECT s.project_id, NEW.session_id, 'session_updated', json_object('id', NEW.session_id), NEW.updated_at
    FROM sessions s WHERE s.id = NEW.session_id;
END;

CREATE TRIGGER usage_bindings_cdc_update AFTER UPDATE ON usage_bindings
WHEN NEW.session_id IS NOT NULL
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    SELECT s.project_id, NEW.session_id, 'session_updated', json_object('id', NEW.session_id), NEW.updated_at
    FROM sessions s WHERE s.id = NEW.session_id;
END;

CREATE TRIGGER usage_sources_cdc_update AFTER UPDATE ON usage_sources
WHEN OLD.anomaly_count IS NOT NEW.anomaly_count
  OR OLD.last_error_code IS NOT NEW.last_error_code
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    SELECT s.project_id, ub.session_id, 'session_updated', json_object('id', ub.session_id), NEW.updated_at
    FROM usage_bindings ub JOIN sessions s ON s.id = ub.session_id
    WHERE ub.id = NEW.binding_id AND ub.session_id IS NOT NULL;
END;

-- Unchanged in meaning: a per-SESSION integrity read model. Pane and planner
-- bindings have no session and are simply not part of it.
CREATE VIEW usage_session_integrity AS
SELECT ub.session_id,
    CAST(MAX(CASE
        WHEN ub.state = 'partial'
          OR ub.last_error_code NOT IN ('', 'source_discovery_pending', 'artifact_missing', 'source_read_failed')
          OR (us.last_error_code <> 'artifact_replaced' AND (
              us.anomaly_count > 0
              OR us.last_error_code NOT IN ('', 'source_discovery_pending', 'artifact_missing', 'source_read_failed')
          ))
        THEN 1 ELSE 0
    END) AS INTEGER) AS incomplete
FROM usage_bindings ub
LEFT JOIN usage_sources us ON us.binding_id = ub.id
WHERE ub.session_id IS NOT NULL
GROUP BY ub.session_id;

-- model_usage_events.usage_source_id becomes nullable, for exactly one reason:
-- the PLANNER reports its spend in a response, not in a transcript.
--
-- AO invokes it as `claude --print --no-session-persistence`, which writes no
-- JSONL at all. Its tokens are stated once, in the JSON envelope the adapter
-- already parses, and there is no artifact to point a source row at. The
-- alternatives were both worse than a nullable column: inventing a usage_sources
-- row would give the watcher a file path that does not exist and will never
-- appear, and dropping the planner's usage would leave a provider-backed role
-- permanently unmeasured -- the exact gap this checkpoint closes.
--
-- Same rebuild discipline as above: every row copied, every id preserved, so the
-- ledger is byte-for-byte what it was plus one relaxed constraint. Exactly-once
-- is untouched -- it has always lived in UNIQUE (binding_id, source_event_key),
-- never in the source reference.
CREATE TABLE model_usage_events_p3e (
    id                      INTEGER PRIMARY KEY AUTOINCREMENT,
    binding_id              INTEGER NOT NULL REFERENCES usage_bindings (id) ON DELETE CASCADE,
    -- NULL means "reported in a response, not read from an artifact".
    usage_source_id         INTEGER REFERENCES usage_sources (id) ON DELETE CASCADE,
    model_id                TEXT NOT NULL CHECK (trim(model_id) <> ''),
    input_tokens            INTEGER NOT NULL CHECK (input_tokens >= 0),
    uncached_input_tokens   INTEGER NOT NULL CHECK (uncached_input_tokens >= 0 AND uncached_input_tokens <= input_tokens),
    cache_read_tokens       INTEGER NOT NULL CHECK (cache_read_tokens >= 0 AND cache_read_tokens <= input_tokens),
    cache_write_tokens      INTEGER NOT NULL CHECK (cache_write_tokens >= 0 AND cache_write_tokens <= input_tokens),
    output_tokens           INTEGER NOT NULL CHECK (output_tokens >= 0),
    reasoning_tokens        INTEGER CHECK (reasoning_tokens IS NULL OR (reasoning_tokens >= 0 AND reasoning_tokens <= output_tokens)),
    source_event_key        TEXT NOT NULL CHECK (trim(source_event_key) <> ''),
    observed_at             TIMESTAMP,
    recorded_at             TIMESTAMP,
    UNIQUE (binding_id, source_event_key)
);

INSERT INTO model_usage_events_p3e
    (id, binding_id, usage_source_id, model_id, input_tokens, uncached_input_tokens,
     cache_read_tokens, cache_write_tokens, output_tokens, reasoning_tokens,
     source_event_key, observed_at, recorded_at)
SELECT id, binding_id, usage_source_id, model_id, input_tokens, uncached_input_tokens,
       cache_read_tokens, cache_write_tokens, output_tokens, reasoning_tokens,
       source_event_key, observed_at, recorded_at
FROM model_usage_events;

DROP TABLE model_usage_events;
ALTER TABLE model_usage_events_p3e RENAME TO model_usage_events;

CREATE INDEX idx_model_usage_events_binding_model ON model_usage_events (binding_id, model_id);
CREATE INDEX idx_model_usage_events_usage_source ON model_usage_events (usage_source_id);
CREATE INDEX idx_model_usage_events_observed ON model_usage_events (observed_at);

COMMIT;
PRAGMA foreign_keys=ON;
-- +goose StatementEnd

-- +goose StatementBegin
-- The attribution window's subject, matching the binding's. Additive: every
-- existing window was opened for an AO session, which is what the default says.
-- The window's `session_id` column keeps its name and becomes the subject id --
-- renaming it would rewrite a table for a word.
ALTER TABLE usage_attribution_windows ADD COLUMN subject_kind TEXT NOT NULL DEFAULT 'session';
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_usage_windows_subject_opened
    ON usage_attribution_windows (subject_kind, session_id, opened_at DESC, id DESC);
-- +goose StatementEnd

-- +goose StatementBegin
-- Attribution now resolves on the SUBJECT rather than on the session, so a
-- reviewer pane's events land in the reviewer's window and a resolver pane's in
-- the resolver's. Everything else about the rule is unchanged: an event belongs
-- to the newest window opened at or before its observed time, or -- lacking one
-- -- to the subject's earliest window, so exactly one window per event and no
-- total can double count.
CREATE VIEW usage_event_attribution AS
SELECT
    e.id                    AS event_id,
    e.binding_id            AS binding_id,
    b.subject_kind          AS subject_kind,
    b.subject_id            AS subject_id,
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
          WHERE w.subject_kind = b.subject_kind
            AND w.session_id = b.subject_id
            AND e.observed_at IS NOT NULL
            AND w.opened_at <= e.observed_at
          ORDER BY w.opened_at DESC, w.id DESC
          LIMIT 1),
        (SELECT w2.id FROM usage_attribution_windows w2
          WHERE w2.subject_kind = b.subject_kind
            AND w2.session_id = b.subject_id
          ORDER BY w2.opened_at ASC, w2.id ASC
          LIMIT 1)
    ) AS window_id,
    CASE WHEN e.observed_at IS NOT NULL AND EXISTS (
            SELECT 1 FROM usage_attribution_windows w3
             WHERE w3.subject_kind = b.subject_kind
               AND w3.session_id = b.subject_id
               AND w3.opened_at <= e.observed_at
         ) THEN 'exact' ELSE 'approximate' END AS attribution_basis
FROM model_usage_events e
JOIN usage_bindings b ON b.id = e.binding_id;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- A downgrade cannot re-impose NOT NULL on session_id without deleting every
-- pane and planner binding, which would silently destroy recorded provider
-- usage. It is therefore refused rather than approximated.
SELECT RAISE(FAIL, 'migration 0150 cannot be reversed without discarding non-session usage bindings');
-- +goose StatementEnd
