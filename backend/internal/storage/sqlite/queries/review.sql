-- name: UpsertReview :exec
INSERT INTO review (id, session_id, project_id, harness, pr_url, reviewer_handle_id, agent_session_id, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (session_id, harness) DO UPDATE SET
    project_id = excluded.project_id,
    pr_url = excluded.pr_url,
    reviewer_handle_id = excluded.reviewer_handle_id,
    agent_session_id = CASE WHEN excluded.agent_session_id != '' THEN excluded.agent_session_id ELSE review.agent_session_id END,
    updated_at = excluded.updated_at;

-- name: GetReviewBySession :one
SELECT id, session_id, project_id, harness, pr_url, reviewer_handle_id, agent_session_id, created_at, updated_at
FROM review WHERE session_id = ? ORDER BY updated_at DESC, created_at DESC, id DESC LIMIT 1;

-- name: GetReviewBySessionAndHarness :one
SELECT id, session_id, project_id, harness, pr_url, reviewer_handle_id, agent_session_id, created_at, updated_at
FROM review WHERE session_id = ? AND harness = ?;

-- name: GetReviewByID :one
SELECT id, session_id, project_id, harness, pr_url, reviewer_handle_id, agent_session_id, created_at, updated_at
FROM review WHERE id = ?;

-- name: ListReviewsBySession :many
SELECT id, session_id, project_id, harness, pr_url, reviewer_handle_id, agent_session_id, created_at, updated_at
FROM review WHERE session_id = ? ORDER BY updated_at DESC, created_at DESC, id DESC;

-- name: ClearReviewerHandle :exec
UPDATE review SET reviewer_handle_id = '', updated_at = CURRENT_TIMESTAMP WHERE session_id = ?;

-- name: ClearReviewerHandleByHarness :exec
UPDATE review SET reviewer_handle_id = '', updated_at = CURRENT_TIMESTAMP WHERE session_id = ? AND harness = ?;

-- name: UpdateReviewAgentSessionID :execrows
UPDATE review SET agent_session_id = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?;

-- name: InsertReviewRun :exec
INSERT INTO review_run (id, review_id, session_id, batch_id, harness, trigger_source, pr_url, target_sha, status, verdict, body, github_review_id, created_at, auto_inject_review)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: UpdateReviewRunResult :execrows
UPDATE review_run SET status = ?, verdict = ?, body = ?, github_review_id = ?, auto_inject_review = ? WHERE id = ? AND status = 'running';

-- name: SupersedeStaleRunningReviewRuns :execrows
UPDATE review_run SET status = 'failed', body = ? WHERE session_id = ? AND pr_url = ? AND target_sha != ? AND status = 'running' AND verdict = '';

-- name: CancelRunningReviewRunsBySession :execrows
UPDATE review_run SET status = 'cancelled', body = ? WHERE session_id = ? AND status = 'running' AND verdict = '';

-- name: CancelRunningReviewRunsBySessionAndHarness :execrows
UPDATE review_run SET status = 'cancelled', body = ? WHERE session_id = ? AND harness = ? AND status = 'running' AND verdict = '';

-- name: MarkReviewRunDelivered :execrows
UPDATE review_run SET status = 'delivered', delivered_at = ? WHERE id = ? AND status = 'complete' AND delivered_at IS NULL;

-- name: GetReviewRun :one
SELECT id, review_id, session_id, harness, pr_url, target_sha, status, verdict, body, created_at, github_review_id, delivered_at, batch_id, auto_inject_review, trigger_source, late_verdict, late_verdict_body, late_verdict_at, superseded_by
FROM review_run WHERE id = ?;

-- name: GetReviewRunBySessionPRAndSHA :one
SELECT id, review_id, session_id, harness, pr_url, target_sha, status, verdict, body, created_at, github_review_id, delivered_at, batch_id, auto_inject_review, trigger_source, late_verdict, late_verdict_body, late_verdict_at, superseded_by
FROM review_run WHERE session_id = ? AND pr_url = ? AND target_sha = ? ORDER BY created_at DESC LIMIT 1;

-- name: GetReviewRunBySessionPRSHAAndHarness :one
SELECT id, review_id, session_id, harness, pr_url, target_sha, status, verdict, body, created_at, github_review_id, delivered_at, batch_id, auto_inject_review, trigger_source, late_verdict, late_verdict_body, late_verdict_at, superseded_by
FROM review_run WHERE session_id = ? AND pr_url = ? AND target_sha = ? AND harness = ? ORDER BY created_at DESC LIMIT 1;

-- name: ListReviewRunsBySession :many
SELECT id, review_id, session_id, harness, pr_url, target_sha, status, verdict, body, created_at, github_review_id, delivered_at, batch_id, auto_inject_review, trigger_source, late_verdict, late_verdict_body, late_verdict_at, superseded_by
FROM review_run WHERE session_id = ? ORDER BY created_at DESC;

-- name: ListRunningReviewRunsBySession :many
SELECT id, review_id, session_id, harness, pr_url, target_sha, status, verdict, body, created_at, github_review_id, delivered_at, batch_id, auto_inject_review, trigger_source, late_verdict, late_verdict_body, late_verdict_at, superseded_by
FROM review_run WHERE session_id = ? AND status = 'running' AND verdict = '' ORDER BY created_at DESC;

-- name: ListReviewRunsByBatch :many
SELECT id, review_id, session_id, harness, pr_url, target_sha, status, verdict, body, created_at, github_review_id, delivered_at, batch_id, auto_inject_review, trigger_source, late_verdict, late_verdict_body, late_verdict_at, superseded_by
FROM review_run WHERE session_id = ? AND batch_id = ? ORDER BY created_at ASC, id ASC;

-- name: RecordLateReviewVerdict :execrows
-- The verdict a reviewer produced after AO had already closed its run out.
--
-- Guarded on the run being terminal-WITHOUT-a-verdict (exactly the states AO's
-- own stall/supersede bookkeeping writes) and on no late verdict having been
-- recorded yet, so a retried submit is idempotent rather than a second opinion.
-- The run keeps its terminal status: this records what the reviewer said, and
-- says nothing about whether it still speaks for the workflow. See migration
-- 0135.
UPDATE review_run
SET late_verdict = ?, late_verdict_body = ?, late_verdict_at = ?
WHERE id = ?
  AND status IN ('cancelled', 'failed')
  AND verdict = ''
  AND late_verdict = '';

-- name: MarkReviewRunSupersededBy :execrows
-- Names the replacement that took authority over a closed-out run, so "which
-- review speaks for this step" survives a restart.
--
-- Write-once. The idempotent-replay case -- superseded_by already holding the
-- replacement that is now replaying, because a crash landed between this write
-- and the pointer rebind -- is resolved by the store, which re-reads after a
-- zero-row result. That is sound rather than a check-then-act race precisely
-- BECAUSE this column is write-once: once set it can never change again, so a
-- read taken after the failed swap is as authoritative as the swap.
--
-- NOTE: do not write a pair of single quotes anywhere in this comment block.
-- sqlc's SQLite lexer treats it as a string literal, mis-lexes the statement
-- below and emits it TRUNCATED, with no error at generate time -- the failure
-- surfaces at runtime as "SQL logic error: incomplete input".
UPDATE review_run
SET superseded_by = ?
WHERE id = ? AND superseded_by = '' AND id != ?;
