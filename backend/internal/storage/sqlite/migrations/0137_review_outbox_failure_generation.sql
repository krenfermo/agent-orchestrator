-- 0137: the outbox row remembers WHICH failure put it into `failed`.
--
-- A human resume of a reviewer launch reopens one outbox entry, failed ->
-- pending. Until now that transition was compare-and-swapped on the row id and
-- the status alone, which cannot express what the resume actually owns.
--
-- The entry is REUSED across retries: one row, one idempotency key, failed ->
-- pending -> failed as many times as the launch fails. So "this row is failed"
-- is true of two different failures, and a resume that validated failure F1 and
-- was then delayed would still satisfy id + status once somebody else had
-- resumed F1, dispatched, and failed again as F2. It reopened F2 -- a fresh
-- budget epoch and a fresh launch, from a human decision that was never made
-- about F2. Re-reading the state in Go before the swap does not close it: that
-- is a check and a write, and the generation can turn over in between.
--
-- failure_generation is the missing durable fact. Every reviewer-launch failure
-- stamps the row with the identity of the failure that caused it -- the claim's
-- idempotency key plus the id of that failure record -- in the same statement
-- that moves the row to `failed`. The resume then names the generation it
-- observed IN THE SWAP ITSELF, so SQLite decides, atomically, whether the state
-- being reopened is still the state the person acted on.
--
-- Empty string means "no reviewer-launch generation is recorded for this row":
-- a pending/dispatched row, a row failed by some other mechanism, or a row that
-- was already failed before this migration existed. Every non-generation
-- transition clears it back to empty, so a stale value can never be inherited
-- by a later state.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE workflow_outbox ADD COLUMN failure_generation TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE workflow_outbox DROP COLUMN failure_generation;
-- +goose StatementEnd
