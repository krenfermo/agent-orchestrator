-- Frente 3 / 3C: agent tool observations (migration 0175). Metadata only: see
-- the migration for what is deliberately never stored.

-- name: InsertAgentToolObservation :exec
-- Exactly-once by (binding_id, observation_key): a re-read of the same
-- transcript re-derives the same key and this is a no-op.
INSERT INTO agent_tool_observations (
    binding_id, usage_source_id, observation_key, event_key, ordinal, observed_at,
    origin, op, tool_name, path_scope, path, result_bytes, recorded_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (binding_id, observation_key) DO NOTHING;

-- name: CompleteAgentToolObservation :execrows
-- A tool call's result always arrives in a LATER record than the call, and
-- possibly a later chunk, so it completes the row instead of being inserted.
-- Set once: a re-read of the same result is a no-op, and a result whose call
-- was never recorded (a sidechain call seen from the main transcript) updates
-- nothing.
UPDATE agent_tool_observations
SET result_bytes = ?, result_items = ?, result_error = ?
WHERE binding_id = ? AND observation_key = ? AND result_bytes IS NULL;

-- name: UpsertAgentToolCoverage :exec
-- Widens a source's parsed range and extractor-version span. Runs in the
-- chunk's transaction, so coverage never claims a range whose observations
-- did not commit.
INSERT INTO agent_tool_coverage (
    usage_source_id, binding_id, covered_from, covered_to, min_extractor, max_extractor,
    pre_coverage_events, first_covered_at, updated_at
) VALUES (
    sqlc.arg(usage_source_id), sqlc.arg(binding_id), sqlc.arg(covered_from), sqlc.arg(covered_to),
    sqlc.arg(extractor), sqlc.arg(extractor), sqlc.arg(pre_coverage_events), sqlc.arg(updated_at), sqlc.arg(updated_at)
)
ON CONFLICT (usage_source_id) DO UPDATE SET
    covered_from  = MIN(agent_tool_coverage.covered_from, excluded.covered_from),
    covered_to    = MAX(agent_tool_coverage.covered_to, excluded.covered_to),
    min_extractor = MIN(agent_tool_coverage.min_extractor, excluded.min_extractor),
    max_extractor = MAX(agent_tool_coverage.max_extractor, excluded.max_extractor),
    updated_at    = excluded.updated_at;

-- name: CountUsageEventsForSource :one
SELECT COUNT(*) FROM model_usage_events WHERE usage_source_id = sqlc.arg(usage_source_id);

-- name: ListRunToolCoverage :many
-- Every transcript source of one run's subjects with what the extractor has
-- parsed of it. covered_from is -1 for a source the extractor never saw.
-- events_before_coverage counts the source's usage events ingested WITHOUT the
-- extractor: those that existed when it first covered the source, or all of
-- them when it never did.
SELECT
    b.subject_kind                                  AS subject_kind,
    b.subject_id                                    AS subject_id,
    s.id                                            AS usage_source_id,
    s.byte_offset                                   AS byte_offset,
    CAST(COALESCE(c.covered_from, -1) AS INTEGER)   AS covered_from,
    CAST(COALESCE(c.covered_to, -1) AS INTEGER)     AS covered_to,
    CAST(COALESCE(c.min_extractor, 0) AS INTEGER)   AS min_extractor,
    CAST(COALESCE(c.max_extractor, 0) AS INTEGER)   AS max_extractor,
    CAST(COALESCE(c.pre_coverage_events,
        (SELECT COUNT(*) FROM model_usage_events e WHERE e.usage_source_id = s.id)) AS INTEGER) AS events_before_coverage
FROM usage_bindings b
JOIN usage_sources s ON s.binding_id = b.id
LEFT JOIN agent_tool_coverage c ON c.usage_source_id = s.id
WHERE b.subject_kind || char(31) || b.subject_id IN (
    SELECT w.subject_kind || char(31) || w.session_id FROM usage_attribution_windows w
    WHERE w.workflow_run_id = sqlc.arg(workflow_run_id)
)
ORDER BY b.subject_kind, b.subject_id, s.id;

-- name: GetUsageSubjectWorkspaceRoot :one
-- The project root AO itself recorded for a usage subject: the directory an
-- agent's file paths are normalised against. Never taken from the transcript.
--   session                   -> the session's workspace
--   runtime_pane (review run) -> the reviewed session's workspace
--   runtime_pane (resolution) -> the asking session's workspace
--   planner_invocation        -> the project checkout
-- '' when AO has none, in which case every path stays unresolved.
WITH subject AS (
    SELECT CAST(sqlc.arg(subject_kind) AS TEXT) AS kind,
           CAST(sqlc.arg(subject_id) AS TEXT)   AS id
)
SELECT CAST(COALESCE(
    (SELECT s.workspace_path FROM subject, sessions s
      WHERE subject.kind = 'session' AND s.id = subject.id AND s.workspace_path <> ''),
    (SELECT s.workspace_path FROM subject, review_run r JOIN sessions s ON s.id = r.session_id
      WHERE subject.kind = 'runtime_pane' AND r.id = subject.id AND s.workspace_path <> ''),
    (SELECT s.workspace_path FROM subject, workflow_question_resolutions q
       JOIN sessions s ON s.id = q.asking_session_id
      WHERE subject.kind = 'runtime_pane' AND q.id = subject.id AND s.workspace_path <> ''),
    (SELECT p.path FROM subject, workflow_runs w JOIN projects p ON p.id = w.project_id
      WHERE subject.kind = 'planner_invocation'
        AND instr(subject.id, '#') > 0
        AND w.id = substr(subject.id, 1, instr(subject.id, '#') - 1)),
    ''
) AS TEXT) AS root;

-- name: ListRunToolObservations :many
-- Every observation of one run's subjects, resolved to the run's role window.
-- Same CTE + LIMIT -1 + CROSS JOIN shape as AggregateWorkflowRunUsage, for the
-- same reason: written as a plain join it re-resolves every row once per
-- window.
WITH attributed AS (
    SELECT
    a.subject_kind      AS subject_kind,
    a.subject_id        AS subject_id,
    a.harness           AS harness,
    a.window_id         AS window_id,
    a.usage_source_id   AS usage_source_id,
    a.observation_key   AS observation_key,
    a.event_key         AS event_key,
    a.ordinal           AS ordinal,
    a.observed_at       AS observed_at,
    a.origin            AS origin,
    a.op                AS op,
    a.tool_name         AS tool_name,
    a.path_scope        AS path_scope,
    a.path              AS path,
    a.result_bytes      AS result_bytes,
    a.result_items      AS result_items,
    a.result_error      AS result_error,
    a.attribution_basis AS attribution_basis
    FROM agent_tool_observation_attribution a
    WHERE a.subject_kind || char(31) || a.subject_id IN (
        SELECT s.subject_kind || char(31) || s.session_id FROM usage_attribution_windows s
        WHERE s.workflow_run_id = sqlc.arg(workflow_run_id)
    )
    LIMIT -1
)
SELECT
    w.role              AS role,
    w.cycle             AS cycle,
    w.project_id        AS project_id,
    a.subject_kind      AS subject_kind,
    a.subject_id        AS subject_id,
    a.harness           AS harness,
    a.usage_source_id   AS usage_source_id,
    a.observation_key   AS observation_key,
    a.event_key         AS event_key,
    a.ordinal           AS ordinal,
    a.observed_at       AS observed_at,
    a.origin            AS origin,
    a.op                AS op,
    a.tool_name         AS tool_name,
    a.path_scope        AS path_scope,
    a.path              AS path,
    a.result_bytes      AS result_bytes,
    a.result_items      AS result_items,
    a.result_error      AS result_error,
    a.attribution_basis AS attribution_basis
FROM attributed a
CROSS JOIN usage_attribution_windows w ON w.id = a.window_id
WHERE w.workflow_run_id = sqlc.arg(workflow_run_id)
ORDER BY a.subject_kind, a.subject_id, a.usage_source_id, a.ordinal, a.observation_key;

-- name: ListRunExplorationCalls :many
-- One row per provider call of one run's subjects, with the subject and the
-- role window it resolved to. Unlike ListRunContextTrajectoryEvents, events
-- with no observed_at are KEPT: a model-call count must not shrink because a
-- call could not be placed in time.
WITH attributed AS (
    SELECT
    a.subject_kind          AS subject_kind,
    a.subject_id            AS subject_id,
    a.harness               AS harness,
    a.window_id             AS window_id,
    a.model_id              AS model_id,
    a.input_tokens          AS input_tokens,
    a.uncached_input_tokens AS uncached_input_tokens,
    a.cache_read_tokens     AS cache_read_tokens,
    a.cache_write_tokens    AS cache_write_tokens,
    a.output_tokens         AS output_tokens,
    a.turn_class            AS turn_class,
    a.observed_at           AS observed_at
    FROM usage_event_attribution a
    WHERE a.subject_kind || char(31) || a.subject_id IN (
        SELECT s.subject_kind || char(31) || s.session_id FROM usage_attribution_windows s
        WHERE s.workflow_run_id = sqlc.arg(workflow_run_id)
    )
    LIMIT -1
)
SELECT
    w.role                  AS role,
    w.cycle                 AS cycle,
    w.project_id            AS project_id,
    a.subject_kind          AS subject_kind,
    a.subject_id            AS subject_id,
    a.harness               AS harness,
    a.model_id              AS model_id,
    a.input_tokens          AS input_tokens,
    a.uncached_input_tokens AS uncached_input_tokens,
    a.cache_read_tokens     AS cache_read_tokens,
    a.cache_write_tokens    AS cache_write_tokens,
    a.output_tokens         AS output_tokens,
    a.turn_class            AS turn_class,
    a.observed_at           AS observed_at
FROM attributed a
CROSS JOIN usage_attribution_windows w ON w.id = a.window_id
WHERE w.workflow_run_id = sqlc.arg(workflow_run_id)
ORDER BY a.subject_kind, a.subject_id, a.observed_at;
