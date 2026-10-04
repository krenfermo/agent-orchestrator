-- 0135: the verdict a reviewer produced after AO had already given up on it.
--
-- The incident (wf-756988ae, review run 4ac56ac5): AO's reviewer-stall
-- detection classified a still-working Codex reviewer as a provider capacity
-- stall 18 minutes after dispatch, and CancelRunningReviewRunsBySessionAnd-
-- Harness moved its review_run to 'cancelled'. The reviewer then finished,
-- found no task-scoped defect, and ran the official command:
--
--   ao review submit agent-orchestrator-44 --run 4ac56ac5… --verdict approved
--
-- AO answered REVIEW_INVALID: "review run … is not running", because
-- UpdateReviewRunResult is guarded on `status = 'running'` and submitOne's
-- status switch has no arm for a cancelled run. A real review — a real read of
-- a real diff — was destroyed by AO's own bookkeeping, and the workflow was
-- left waiting on the very run it had just cancelled.
--
-- Why the verdict is NOT recorded by flipping the run back to 'complete':
-- idx_review_run_session_pr_sha_harness is a partial UNIQUE index over
-- (session_id, pr_url, target_sha, harness) that admits exactly those rows with
-- status NOT IN ('failed','cancelled') AND (status='running' OR verdict NOT IN
-- ('','changes_requested')). A cancelled run promoted to complete/approved
-- would enter that index — and collide with the replacement review of the same
-- target the retry is expressly there to create. Late arrival must not be able
-- to fail, so the verdict lands in columns of its own and the row keeps the
-- terminal status AO gave it.
--
-- The separation is also the correctness model, not just an index workaround:
--   * `verdict` stays what AO durably accepted while the run was authoritative;
--   * `late_verdict` is what the reviewer produced after AO stopped listening —
--     preserved in full, and adoptable ONLY while the workflow step still
--     points at this run (workflow/review_authority.go). A late verdict on a
--     run that has since been superseded is evidence, never authority;
--   * `superseded_by` names the replacement that took over, so "which review is
--     authoritative" is answerable from durable state alone after a restart.
--
-- Strictly additive: four nullable/defaulted columns, nothing backfilled, no
-- index touched. A pre-0135 row reads back with an empty late verdict and no
-- supersede pointer, which is exactly what it is.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE review_run ADD COLUMN late_verdict TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE review_run ADD COLUMN late_verdict_body TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE review_run ADD COLUMN late_verdict_at TIMESTAMP;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE review_run ADD COLUMN superseded_by TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- The single-winner claim for a replacement review.
--
-- Review-authority reconciliation runs from boot, from every wake and from every
-- poll, and nothing stopped two of those landing at once: both would read "no
-- authorization yet", both would append one, and the step would consume two
-- retry generations and could launch two replacement reviewers for one
-- abandoned review. In-memory locking cannot fix that -- the callers are not
-- necessarily in one process, and they must survive restarts.
--
-- So the authorization row IS the claim, and the database picks the winner: at
-- most one `review_authority_rebind` checkpoint may exist per (step, abandoned
-- review run). The loser's INSERT fails with a UNIQUE violation, which the store
-- surfaces as domain.ErrDuplicateWorkflowCheckpoint and the reconciler reads as
-- "somebody else already claimed this" -- a no-op, not an error.
--
-- Scoped by `WHERE durable_phase = 'review_authority_rebind'` so it constrains
-- nothing else: every other checkpoint phase stays append-only exactly as it is,
-- and rows with a NULL step or NULL review run (which SQLite treats as distinct
-- in a UNIQUE index anyway) are unaffected.
-- +goose StatementBegin
CREATE UNIQUE INDEX IF NOT EXISTS idx_workflow_review_authority_claim
    ON workflow_checkpoints (workflow_step_id, review_run_id)
    WHERE durable_phase = 'review_authority_rebind';
-- +goose StatementEnd

-- The completion receipt is also single-winner. Two recovery passes may both
-- prove that the outcome is durable before either writes the receipt; this
-- index makes their finalization converge to one row. Unlike the rebind claim,
-- a duplicate here is success because callers verify the reflected outcome
-- before attempting the insert.
-- +goose StatementBegin
CREATE UNIQUE INDEX IF NOT EXISTS idx_workflow_review_late_verdict_adoption
    ON workflow_checkpoints (workflow_step_id, review_run_id)
    WHERE durable_phase = 'review_late_verdict_adopted';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_workflow_review_late_verdict_adoption;
-- +goose StatementEnd

-- +goose StatementBegin
DROP INDEX IF EXISTS idx_workflow_review_authority_claim;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE review_run DROP COLUMN superseded_by;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE review_run DROP COLUMN late_verdict_at;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE review_run DROP COLUMN late_verdict_body;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE review_run DROP COLUMN late_verdict;
-- +goose StatementEnd
