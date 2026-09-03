-- P3-E usage ledger reads. Every aggregate here folds model_usage_events, the
-- same append-only rows the V1 pipeline already writes exactly once; nothing in
-- this file stores a total. A run's number is therefore identical before and
-- after a restart because it is recomputed from the ledger, not carried.

-- name: InsertUsageAttributionWindow :exec
-- Idempotent by dedupe_key: a dispatch replayed by failover, resume, or a wake
-- re-opens the same window instead of splitting a role's tokens across two.
INSERT INTO usage_attribution_windows (
    dedupe_key, subject_kind, session_id, project_id, workflow_run_id,
    parent_workflow_run_id, task_id, workflow_step_id, attempt_id,
    attempt_ordinal, cycle, role, harness, provider, model, opened_at, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (dedupe_key) DO NOTHING;

-- name: InsertDirectUsageEvent :exec
-- One usage fact reported in a provider RESPONSE rather than in a transcript
-- (the planner's `claude --print` envelope). usage_source_id is NULL because
-- there is no artifact and no cursor: nothing was tailed, so nothing can be
-- re-read, and the (binding_id, source_event_key) uniqueness alone carries
-- exactly-once.
INSERT INTO model_usage_events (
    binding_id, usage_source_id, model_id, input_tokens, uncached_input_tokens,
    cache_read_tokens, cache_write_tokens, output_tokens, reasoning_tokens,
    source_event_key, observed_at, recorded_at
) VALUES (?, NULL, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (binding_id, source_event_key) DO NOTHING;

-- name: ListUsageAttributionWindowsForRun :many
-- has_usage_binding matches on the SUBJECT, so a reviewer pane and a resolver
-- pane each answer for themselves.
--
-- Since P3-E's completion pass every provider-backed role can carry a binding,
-- so a false here no longer means "unmeasurable by architecture" -- it means
-- this surface has not reported a provider conversation yet. That is still not
-- zero (a role that spent tokens AO has not yet bound is UNKNOWN), but it is a
-- transient state rather than a permanent property of being a reviewer.
SELECT
    w.*,
    CAST(EXISTS (
        SELECT 1 FROM usage_bindings b
        WHERE b.subject_kind = w.subject_kind AND b.subject_id = w.session_id
    ) AS INTEGER) AS has_usage_binding
FROM usage_attribution_windows w
WHERE w.workflow_run_id = ?
ORDER BY w.opened_at, w.id;

-- name: ListUsageAttributionWindowsForSession :many
SELECT * FROM usage_attribution_windows
WHERE session_id = ?
ORDER BY opened_at, id;

-- name: AggregateWorkflowRunUsage :many
-- SHAPE MATTERS HERE, and the reason is quadratic.
--
-- usage_event_attribution resolves each event's role window with two correlated
-- subqueries. Written as a plain join against the window table, SQLite drives
-- from the WINDOWS and re-resolves every event of a session once per window of
-- that session -- so a session with four role windows and two hundred events
-- pays eight hundred resolutions instead of two hundred. At ten thousand events
-- that measured 1.2s; the shape below measured under 30ms.
--
-- The CTE below resolves each event exactly once, and CROSS JOIN pins the
-- join order so the windows are then looked up by integer primary key. Neither
-- is decoration: drop either and the planner reverts to the quadratic plan.
WITH attributed AS (
    SELECT
    a.subject_kind         AS subject_kind,
    a.subject_id           AS subject_id,
    a.window_id            AS window_id,
    a.model_id             AS model_id,
    a.input_tokens         AS input_tokens,
    a.uncached_input_tokens AS uncached_input_tokens,
    a.cache_read_tokens    AS cache_read_tokens,
    a.cache_write_tokens   AS cache_write_tokens,
    a.output_tokens        AS output_tokens,
    a.reasoning_tokens     AS reasoning_tokens,
    a.attribution_basis    AS attribution_basis
    FROM usage_event_attribution a
    -- Scoped on the SUBJECT. The view's session_id is the real sessions FK and
    -- is NULL for a pane or a planner invocation, so scoping on it would drop
    -- exactly the roles this pass exists to make visible.
    --
    -- The subject is compared as one concatenated key rather than as a row
    -- value: sqlc's SQLite grammar cannot parse `(a, b) IN (SELECT a, b ...)`,
    -- and an EXISTS would make the subquery CORRELATED -- re-run per event
    -- instead of materialized once, which is the quadratic shape the rest of
    -- this file is carefully built to avoid. The separator is a unit separator,
    -- which cannot occur in a subject kind or id.
    WHERE a.subject_kind || char(31) || a.subject_id IN (
        SELECT s.subject_kind || char(31) || s.session_id FROM usage_attribution_windows s
        WHERE s.workflow_run_id = sqlc.arg(workflow_run_id)
    )    -- LIMIT -1 is not a limit. It is the documented way to stop SQLite
    -- flattening a single-use CTE back into the outer query: flattened, the
    -- planner reverts to the quadratic plan this shape exists to avoid. (The
    -- clearer `AS MATERIALIZED` hint says the same thing, but sqlc's SQLite
    -- grammar cannot parse it.)
    LIMIT -1
)
SELECT
    w.role                        AS role,
    w.cycle                       AS cycle,
    w.harness                     AS harness,
    w.provider                    AS provider,
    w.attempt_id                  AS attempt_id,
    w.attempt_ordinal             AS attempt_ordinal,
    w.task_id                     AS task_id,
    a.model_id                    AS model_id,
    CAST(SUM(a.input_tokens) AS INTEGER)          AS input_tokens,
    CAST(SUM(a.uncached_input_tokens) AS INTEGER) AS uncached_input_tokens,
    CAST(SUM(a.cache_read_tokens) AS INTEGER)     AS cache_read_tokens,
    CAST(SUM(a.cache_write_tokens) AS INTEGER)    AS cache_write_tokens,
    CAST(SUM(a.output_tokens) AS INTEGER)         AS output_tokens,
    CAST(COALESCE(SUM(a.reasoning_tokens), 0) AS INTEGER) AS reasoning_tokens,
    CAST(COUNT(a.reasoning_tokens) AS INTEGER)    AS reasoning_event_count,
    CAST(COUNT(*) AS INTEGER)                     AS event_count,
    CAST(SUM(CASE WHEN a.attribution_basis = 'approximate' THEN 1 ELSE 0 END) AS INTEGER) AS approximate_count
FROM attributed a
CROSS JOIN usage_attribution_windows w ON w.id = a.window_id
WHERE w.workflow_run_id = sqlc.arg(workflow_run_id)
GROUP BY w.role, w.cycle, w.harness, w.provider, w.attempt_id, w.attempt_ordinal, w.task_id, a.model_id
ORDER BY w.role, w.cycle, w.attempt_ordinal, a.model_id;

-- name: AggregateProjectUsage :many
-- Bucketed by the WINDOW's opened_at, not the event's own time: a period asks
-- "what did the work AO started in this period cost", and the dispatch instant
-- is a fact AO recorded itself, unlike a provider transcript timestamp which
-- may be absent. The read model labels the period basis so nobody reads these
-- windows as a provider's billing period.
--
-- Same CTE + CROSS JOIN shape as AggregateWorkflowRunUsage, for the
-- same quadratic reason. See that query's comment.
WITH attributed AS (
    SELECT
    a.subject_kind         AS subject_kind,
    a.subject_id           AS subject_id,
    a.window_id            AS window_id,
    a.model_id             AS model_id,
    a.input_tokens         AS input_tokens,
    a.uncached_input_tokens AS uncached_input_tokens,
    a.cache_read_tokens    AS cache_read_tokens,
    a.cache_write_tokens   AS cache_write_tokens,
    a.output_tokens        AS output_tokens,
    a.reasoning_tokens     AS reasoning_tokens,
    a.attribution_basis    AS attribution_basis
    FROM usage_event_attribution a
    WHERE a.subject_kind || char(31) || a.subject_id IN (
        SELECT s.subject_kind || char(31) || s.session_id FROM usage_attribution_windows s
        WHERE s.project_id = sqlc.arg(project_id)
          AND s.opened_at >= sqlc.arg(from_at)
          AND s.opened_at < sqlc.arg(to_at)
    )    -- LIMIT -1 is not a limit. It is the documented way to stop SQLite
    -- flattening a single-use CTE back into the outer query: flattened, the
    -- planner reverts to the quadratic plan this shape exists to avoid. (The
    -- clearer `AS MATERIALIZED` hint says the same thing, but sqlc's SQLite
    -- grammar cannot parse it.)
    LIMIT -1
)
SELECT
    w.workflow_run_id AS workflow_run_id,
    w.role            AS role,
    w.harness         AS harness,
    w.provider        AS provider,
    a.model_id        AS model_id,
    CAST(SUM(a.input_tokens) AS INTEGER)          AS input_tokens,
    CAST(SUM(a.uncached_input_tokens) AS INTEGER) AS uncached_input_tokens,
    CAST(SUM(a.cache_read_tokens) AS INTEGER)     AS cache_read_tokens,
    CAST(SUM(a.cache_write_tokens) AS INTEGER)    AS cache_write_tokens,
    CAST(SUM(a.output_tokens) AS INTEGER)         AS output_tokens,
    CAST(COALESCE(SUM(a.reasoning_tokens), 0) AS INTEGER) AS reasoning_tokens,
    CAST(COUNT(a.reasoning_tokens) AS INTEGER)    AS reasoning_event_count,
    CAST(COUNT(*) AS INTEGER)                     AS event_count,
    CAST(SUM(CASE WHEN a.attribution_basis = 'approximate' THEN 1 ELSE 0 END) AS INTEGER) AS approximate_count
FROM attributed a
CROSS JOIN usage_attribution_windows w ON w.id = a.window_id
WHERE w.project_id = sqlc.arg(project_id)
  AND w.opened_at >= sqlc.arg(from_at)
  AND w.opened_at < sqlc.arg(to_at)
GROUP BY w.workflow_run_id, w.role, w.harness, w.provider, a.model_id
ORDER BY w.workflow_run_id, w.role, a.model_id;

-- name: AggregateCompactRunUsageForProject :many
-- The Board's per-card figure. One grouped scan for the whole project rather
-- than a query per card, so a board with fifty runs stays one round trip. Same
-- CTE + CROSS JOIN shape as AggregateWorkflowRunUsage.
WITH attributed AS (
    SELECT
    a.subject_kind         AS subject_kind,
    a.subject_id           AS subject_id,
    a.window_id            AS window_id,
    a.model_id             AS model_id,
    a.input_tokens         AS input_tokens,
    a.uncached_input_tokens AS uncached_input_tokens,
    a.cache_read_tokens    AS cache_read_tokens,
    a.cache_write_tokens   AS cache_write_tokens,
    a.output_tokens        AS output_tokens,
    a.reasoning_tokens     AS reasoning_tokens,
    a.attribution_basis    AS attribution_basis
    FROM usage_event_attribution a
    WHERE a.subject_kind || char(31) || a.subject_id IN (
        SELECT s.subject_kind || char(31) || s.session_id FROM usage_attribution_windows s
        WHERE s.project_id = sqlc.arg(project_id)
    )    -- LIMIT -1 is not a limit. It is the documented way to stop SQLite
    -- flattening a single-use CTE back into the outer query: flattened, the
    -- planner reverts to the quadratic plan this shape exists to avoid. (The
    -- clearer `AS MATERIALIZED` hint says the same thing, but sqlc's SQLite
    -- grammar cannot parse it.)
    LIMIT -1
)
SELECT
    w.workflow_run_id AS workflow_run_id,
    w.provider        AS provider,
    a.model_id        AS model_id,
    CAST(SUM(a.input_tokens) AS INTEGER)          AS input_tokens,
    CAST(SUM(a.uncached_input_tokens) AS INTEGER) AS uncached_input_tokens,
    CAST(SUM(a.cache_read_tokens) AS INTEGER)     AS cache_read_tokens,
    CAST(SUM(a.cache_write_tokens) AS INTEGER)    AS cache_write_tokens,
    CAST(SUM(a.output_tokens) AS INTEGER)         AS output_tokens,
    CAST(COUNT(*) AS INTEGER)                     AS event_count,
    CAST(SUM(CASE WHEN a.attribution_basis = 'approximate' THEN 1 ELSE 0 END) AS INTEGER) AS approximate_count
FROM attributed a
CROSS JOIN usage_attribution_windows w ON w.id = a.window_id
WHERE w.project_id = sqlc.arg(project_id)
  AND w.workflow_run_id <> ''
GROUP BY w.workflow_run_id, w.provider, a.model_id
ORDER BY w.workflow_run_id;

-- name: CountProjectUsageWorkflows :one
SELECT CAST(COUNT(DISTINCT workflow_run_id) AS INTEGER)
FROM usage_attribution_windows
WHERE project_id = sqlc.arg(project_id)
  AND workflow_run_id <> ''
  AND opened_at >= sqlc.arg(from_at)
  AND opened_at < sqlc.arg(to_at);

-- name: AggregateRunFamilyUsage :many
-- A parent autonomous run's true spend: its own windows plus every child's.
-- One query, so a parent's budget check cannot see a stale child total. Same
-- CTE + CROSS JOIN shape as AggregateWorkflowRunUsage.
WITH attributed AS (
    SELECT
    a.subject_kind         AS subject_kind,
    a.subject_id           AS subject_id,
    a.window_id            AS window_id,
    a.model_id             AS model_id,
    a.input_tokens         AS input_tokens,
    a.uncached_input_tokens AS uncached_input_tokens,
    a.cache_read_tokens    AS cache_read_tokens,
    a.cache_write_tokens   AS cache_write_tokens,
    a.output_tokens        AS output_tokens,
    a.reasoning_tokens     AS reasoning_tokens,
    a.attribution_basis    AS attribution_basis
    FROM usage_event_attribution a
    WHERE a.subject_kind || char(31) || a.subject_id IN (
        SELECT s.subject_kind || char(31) || s.session_id FROM usage_attribution_windows s
        WHERE s.workflow_run_id = sqlc.arg(run_id)
           OR s.parent_workflow_run_id = sqlc.arg(run_id)
    )    -- LIMIT -1 is not a limit. It is the documented way to stop SQLite
    -- flattening a single-use CTE back into the outer query: flattened, the
    -- planner reverts to the quadratic plan this shape exists to avoid. (The
    -- clearer `AS MATERIALIZED` hint says the same thing, but sqlc's SQLite
    -- grammar cannot parse it.)
    LIMIT -1
)
SELECT
    w.workflow_run_id AS workflow_run_id,
    w.provider        AS provider,
    a.model_id        AS model_id,
    CAST(SUM(a.input_tokens) AS INTEGER)       AS input_tokens,
    CAST(SUM(a.cache_read_tokens) AS INTEGER)  AS cache_read_tokens,
    CAST(SUM(a.cache_write_tokens) AS INTEGER) AS cache_write_tokens,
    CAST(SUM(a.uncached_input_tokens) AS INTEGER) AS uncached_input_tokens,
    CAST(SUM(a.output_tokens) AS INTEGER)      AS output_tokens,
    CAST(COUNT(*) AS INTEGER)                  AS event_count
FROM attributed a
CROSS JOIN usage_attribution_windows w ON w.id = a.window_id
WHERE w.workflow_run_id = sqlc.arg(run_id) OR w.parent_workflow_run_id = sqlc.arg(run_id)
GROUP BY w.workflow_run_id, w.provider, a.model_id;
