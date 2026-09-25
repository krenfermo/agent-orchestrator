-- Frente 3 / 3C: record WHAT an agent looked at, beside what it cost.
--
-- model_usage_events says how many tokens each provider call spent and, since
-- 0169, what kind of turn it was. It cannot say which files the agent opened,
-- how often it re-opened them, how many searches it ran before its first edit,
-- or how much of the context it paid for was handed over by AO, injected by
-- the harness, or fetched by the agent itself. Those are the numbers the
-- Project Memory pilot (3D) has to compare, and they are derivable from the
-- same provider transcript the usage parser already tails.
--
-- ONE ROW PER OBSERVATION: a tool call the model made, a prompt delivered to
-- it, or material its harness injected. Keyed exactly-once by an identity
-- taken from the artifact (a tool-call id, a record uuid), never from a clock,
-- so a transcript re-read from offset zero re-derives the same keys and every
-- insert is a no-op -- the discipline model_usage_events already follows.
--
-- WHAT IS DELIBERATELY NOT STORED. No file content, no command, no search
-- pattern, no tool argument, no prompt, no result. `path` is the one argument
-- kept, and only when it lies inside the project root AO recorded for the
-- subject AND passes the 3B repository boundary (not a secret, not under an
-- excluded directory). Every other target is reduced to a `path_scope` word;
-- a path outside the project is never written. `result_bytes` is a LENGTH.
--
-- NULL IS NOT ZERO. result_bytes / result_items / result_error are NULL until
-- the result record is observed, and stay NULL when the harness never reports
-- one. Read models must report the unobserved share, the rule observed_at,
-- turn_class and the cache-TTL columns already follow.
--
-- No CHECK constraints on the vocabulary columns, deliberately: widening a
-- CHECK in SQLite forces a table rebuild, and the write path already refuses
-- any value outside the closed vocabularies (domain.AgentToolObservation).
--
-- COVERAGE. A missing row is not a zero either. agent_tool_coverage records,
-- per usage source, which byte range of the transcript the 3C extractor has
-- parsed, with which extractor version, and WHEN it first did. A usage event of
-- the source recorded before first_covered_at was ingested by a binary without
-- the extractor, so the tool activity around it is unknown: a read model then
-- reports that subject's tool figures as unavailable instead of an observed
-- zero. (A source can legitimately start mid-file -- the collector resumes a
-- known transcript at its previous offset under a new row -- so "parsed from
-- byte zero" would wrongly condemn it; "no event escaped the extractor" does
-- not.)
--
-- Purely additive: two new tables, two indexes and a view. Nothing existing is
-- altered, nothing is backfilled -- a transcript may have been rotated since,
-- and an observation guessed from a file that no longer matches would be
-- worse than admitting the run predates the table. The Down drops all of it.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE agent_tool_observations (
    id               INTEGER PRIMARY KEY,
    binding_id       INTEGER NOT NULL REFERENCES usage_bindings (id) ON DELETE CASCADE,
    usage_source_id  INTEGER REFERENCES usage_sources (id) ON DELETE CASCADE,
    observation_key  TEXT    NOT NULL,
    -- source_event_key of the billed message that issued the call, when the
    -- transcript links them ('' = not linkable: Codex, prompts, injections).
    event_key        TEXT    NOT NULL DEFAULT '',
    -- Byte offset of the carrying record: the order within one transcript.
    ordinal          INTEGER NOT NULL,
    observed_at      DATETIME,
    origin           TEXT    NOT NULL,
    op               TEXT    NOT NULL,
    tool_name        TEXT    NOT NULL DEFAULT '',
    path_scope       TEXT    NOT NULL,
    path             TEXT,
    result_bytes     INTEGER,
    result_items     INTEGER,
    result_error     INTEGER,
    recorded_at      DATETIME NOT NULL,
    UNIQUE (binding_id, observation_key)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_agent_tool_observations_binding_order
    ON agent_tool_observations (binding_id, ordinal);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_agent_tool_observations_source
    ON agent_tool_observations (usage_source_id);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE agent_tool_coverage (
    usage_source_id     INTEGER PRIMARY KEY REFERENCES usage_sources (id) ON DELETE CASCADE,
    binding_id          INTEGER NOT NULL REFERENCES usage_bindings (id) ON DELETE CASCADE,
    -- Lowest and highest byte offsets the extractor has parsed. A source is
    -- fully covered when covered_from = 0 and covered_to reaches its cursor.
    covered_from        INTEGER NOT NULL,
    covered_to          INTEGER NOT NULL,
    -- Extractor versions that wrote this source's observations. They differ
    -- when a classifier change landed mid-transcript.
    min_extractor       INTEGER NOT NULL,
    max_extractor       INTEGER NOT NULL,
    first_covered_at    DATETIME NOT NULL,
    updated_at          DATETIME NOT NULL
);
-- +goose StatementEnd

-- The same window resolution usage_event_attribution applies to a usage event,
-- applied to an observation: the newest window of the same subject opened at
-- or before the observation, else the subject's earliest window (reported as
-- approximate). A view has no rows and nothing references it.
-- +goose StatementBegin
CREATE VIEW agent_tool_observation_attribution AS
SELECT
    o.id               AS observation_id,
    o.binding_id       AS binding_id,
    o.usage_source_id  AS usage_source_id,
    b.subject_kind     AS subject_kind,
    b.subject_id       AS subject_id,
    b.harness          AS harness,
    o.observation_key  AS observation_key,
    o.event_key        AS event_key,
    o.ordinal          AS ordinal,
    o.observed_at      AS observed_at,
    o.origin           AS origin,
    o.op               AS op,
    o.tool_name        AS tool_name,
    o.path_scope       AS path_scope,
    o.path             AS path,
    o.result_bytes     AS result_bytes,
    o.result_items     AS result_items,
    o.result_error     AS result_error,
    COALESCE(
        (SELECT w.id FROM usage_attribution_windows w
          WHERE w.subject_kind = b.subject_kind
            AND w.session_id = b.subject_id
            AND o.observed_at IS NOT NULL
            AND w.opened_at <= o.observed_at
          ORDER BY w.opened_at DESC, w.id DESC
          LIMIT 1),
        (SELECT w2.id FROM usage_attribution_windows w2
          WHERE w2.subject_kind = b.subject_kind
            AND w2.session_id = b.subject_id
          ORDER BY w2.opened_at ASC, w2.id ASC
          LIMIT 1)
    ) AS window_id,
    CASE WHEN o.observed_at IS NOT NULL AND EXISTS (
            SELECT 1 FROM usage_attribution_windows w3
             WHERE w3.subject_kind = b.subject_kind
               AND w3.session_id = b.subject_id
               AND w3.opened_at <= o.observed_at
         ) THEN 'exact' ELSE 'approximate' END AS attribution_basis
FROM agent_tool_observations o
JOIN usage_bindings b ON b.id = o.binding_id;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP VIEW IF EXISTS agent_tool_observation_attribution;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE IF EXISTS agent_tool_coverage;
-- +goose StatementEnd

-- +goose StatementBegin
DROP INDEX IF EXISTS idx_agent_tool_observations_source;
-- +goose StatementEnd

-- +goose StatementBegin
DROP INDEX IF EXISTS idx_agent_tool_observations_binding_order;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE IF EXISTS agent_tool_observations;
-- +goose StatementEnd
