-- Record WHAT KIND OF TURN each provider call was, next to what it cost.
--
-- P5/P6 gave a run its shape: 193 calls, context 54k -> 324k. The first
-- question anybody asks of that number is the one the ledger still cannot
-- answer -- how many of the 193 were WORK and how many were coordination,
-- polling, or re-reading. dynamics.go says so in its own words: the activity
-- signal carries a tool name and is never persisted, so "excessive polling"
-- and "the same suite run again" were named as underivable rather than
-- approximated.
--
-- They are derivable, from a source AO already reads end to end: the provider
-- transcript the usage parser tails. Every billed assistant message in it
-- carries the content blocks of that one call, and the block TYPE plus the
-- tool NAME is enough to say whether the model spent a turn editing, reading,
-- running something, waiting, or only talking. That is the whole of this
-- column.
--
-- WHAT IS DELIBERATELY NOT STORED. No command text, no tool arguments, no
-- prompt, no message body, no file path. A turn class is a single closed-
-- vocabulary token derived in the parser and then discarded along with the
-- record it came from -- the same discipline the token vector already follows.
-- A ledger that had to hold the command in order to notice it was repeated
-- would be a store of user content AO has no reason to hold, and this column
-- exists precisely so that ledger never has to.
--
-- Empty string means UNCLASSIFIED, and unclassified is not a class: it is what
-- every row written before this migration carries, what a Codex rollout
-- carries (its envelope does not expose per-call tool blocks the same way),
-- and what a malformed record carries. Read models must report an unclassified
-- share rather than folding it into any real class -- unknown is not zero, the
-- same rule observed_at already follows.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE model_usage_events ADD COLUMN turn_class TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- Existing rows keep the '' default. They are deliberately NOT backfilled by
-- re-reading transcripts: a transcript may have been rotated, truncated or
-- deleted since, and a class guessed from a file that no longer matches the
-- row would be worse than admitting the row predates the column.

-- The attribution view is rebuilt only to carry the new column through to the
-- trajectory read. A view has no rows and nothing references it, so this is a
-- projection change and not the table rebuild AGENTS.md warns about.
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
ALTER TABLE model_usage_events DROP COLUMN turn_class;
-- +goose StatementEnd
