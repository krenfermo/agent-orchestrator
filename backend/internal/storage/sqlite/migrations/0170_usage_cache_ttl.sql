-- Record WHICH CACHE LIFETIME each call's cache creation was created with.
--
-- Creating a cache entry that lives for an hour costs more than creating one
-- that lives for five minutes -- on Opus 5, $10.00 per MTok against $6.25. The
-- provider reports which one it made, on every billed message, in the
-- `cache_creation` block beside the total. AO folded both into
-- cache_write_tokens and every read that went back through this table priced
-- all of it at the cheap rate.
--
-- It is not a rounding error. Across every Claude transcript AO holds -- 4,523
-- billed messages in 75 files -- 97.9% of cache creation is at the ONE-HOUR
-- lifetime. Pricing the measured run wf-1c2cb9bd this way understated it by
-- 4.8%, and the 28-call session that compacted twice by 17.9%. With the
-- lifetime applied, AO's own figure moves from 93.6% to 98.0% of what the
-- harness says that session cost.
--
-- P7.2B1 decoded the split in the parser and could not store it, so the true
-- figure lived one layer above the ledger and the ledger disclosed an
-- assumption instead. This column pair is where it lands.
--
-- NULL IS NOT ZERO, AND THIS IS THE WHOLE REASON THE COLUMNS ARE NULLABLE.
-- A row written before this migration carries NULL, which means "the lifetime
-- of these writes was never observed" -- a different fact from "no cache was
-- created", which is what a 0 would say and which is already expressible in
-- cache_write_tokens. A DEFAULT 0 would have made every historical row assert
-- that its writes were short-lived, which is the exact claim this migration
-- exists to stop AO making. Read models must report the unobserved share
-- rather than folding it into the short lifetime, the same rule observed_at
-- and turn_class already follow.
--
-- NOTHING IS BACKFILLED HERE. This migration reads no file, estimates nothing,
-- apportions no aggregate and assumes neither lifetime. It is purely
-- structural and deterministic. Reconstructing the split for historical rows
-- from transcripts that may since have been rotated, truncated or deleted is a
-- separate, explicit, idempotent operation if it is ever worth doing at all.
--
-- No CHECK constraint ties the pair to cache_write_tokens, deliberately:
-- SQLite cannot add one without rebuilding the table, and AGENTS.md is right
-- that a rebuild of a table with incoming references is not worth a
-- constraint. The write path enforces the sum and refuses to persist a split
-- that disagrees with its total.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE model_usage_events ADD COLUMN cache_write_5m_tokens INTEGER;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE model_usage_events ADD COLUMN cache_write_1h_tokens INTEGER;
-- +goose StatementEnd

-- The attribution view is rebuilt only to carry the new columns through to the
-- ledger and trajectory reads. A view has no rows and nothing references it, so
-- this is a projection change and not the table rebuild AGENTS.md warns about
-- -- the same reasoning migration 0169 used for the same view.
-- +goose StatementBegin
DROP VIEW IF EXISTS usage_event_attribution;
-- +goose StatementEnd

-- +goose StatementBegin
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
    e.cache_write_5m_tokens AS cache_write_5m_tokens,
    e.cache_write_1h_tokens AS cache_write_1h_tokens,
    e.output_tokens         AS output_tokens,
    e.reasoning_tokens      AS reasoning_tokens,
    e.turn_class            AS turn_class,
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
DROP VIEW IF EXISTS usage_event_attribution;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE model_usage_events DROP COLUMN cache_write_1h_tokens;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE model_usage_events DROP COLUMN cache_write_5m_tokens;
-- +goose StatementEnd

-- +goose StatementBegin
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
    e.turn_class            AS turn_class,
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
