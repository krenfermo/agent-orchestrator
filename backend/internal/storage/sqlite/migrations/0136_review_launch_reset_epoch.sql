-- 0136: the reviewer-launch budget RESET is single-winner per failed generation.
--
-- A human continuing a stopped run resets the automatic launch budget. Two
-- resumes can both observe that no reset exists yet, and both write one — and a
-- delayed second reset then opens an epoch that hides attempts the first reset's
-- epoch had already spent, handing the budget back a second time.
--
-- The reset is therefore claimed, not merely recorded, and the claim is keyed by
-- the FAILED GENERATION being resumed -- the claim's idempotency key plus the id
-- of the launch-failure record that put the entry into `failed` -- carried in
-- head_sha as 'review-launch-reset-gen-<key>|<record id>'.
--
-- Keying it on the epoch NUMBER instead would not constrain the case that
-- actually happens. A delayed resume reads a history that already contains the
-- winner's epoch and computes the next one, so its key is one nobody holds and
-- its insert always succeeds; it then loses the stale failed->pending swap,
-- having already written a durable reset that -- being newer -- hides every
-- attempt the winner's epoch spent. Uniqueness must answer "has this failure
-- already been resumed?", not "has anybody opened epoch N?".
--
-- So: at most one reset per (step, failed generation). A duplicate insert fails
-- the index, which the caller reads as "somebody already resumed this failure" —
-- a no-op, not an error — and it opens no epoch of its own.
--
-- Scoped by `WHERE durable_phase = 'reviewer_launch_human_retry'` so it
-- constrains nothing else; every other checkpoint phase stays append-only.
--
-- NOTE: never write '' inside a comment in this file. sqlc's SQLite lexer treats
-- it as an unterminated string literal and silently emits the following
-- statement truncated, with no error at generate time.

-- +goose Up
-- BACKFILL FIRST. Unlike the review-authority claims in 0135, this phase is not
-- new: every build before this one wrote reviewer_launch_human_retry with no
-- head_sha at all. A user who has continued a stopped reviewer launch twice on
-- one step therefore already holds two rows that collide under the index below,
-- and CREATE UNIQUE INDEX would fail on them -- wedging startup on exactly the
-- databases that have exercised this path the most.
--
-- The rows are the durable retry history, so none of them may be deleted; they
-- are made DISTINCT instead, in a namespace of their own. A legacy row claims no
-- generation and must never be mistaken for one: it is keyed by its own id, not
-- by a generation, and the reader validates the record body regardless -- a
-- legacy reset names no epoch, no claim and no failed generation, so it fails
-- closed rather than handing the retry budget back on evidence AO cannot
-- verify.
--
-- Guarded by the two NOT LIKE clauses so re-running it is a no-op.
-- +goose StatementBegin
UPDATE workflow_checkpoints
   SET head_sha = 'review-launch-reset-legacy-' || id
 WHERE durable_phase = 'reviewer_launch_human_retry'
   AND head_sha NOT LIKE 'review-launch-reset-gen-%'
   AND head_sha NOT LIKE 'review-launch-reset-legacy-%';
-- +goose StatementEnd

-- +goose StatementBegin
CREATE UNIQUE INDEX IF NOT EXISTS idx_workflow_review_launch_reset_epoch
    ON workflow_checkpoints (workflow_step_id, head_sha)
    WHERE durable_phase = 'reviewer_launch_human_retry';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_workflow_review_launch_reset_epoch;
-- +goose StatementEnd
