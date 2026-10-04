-- Durable email outbox queries.
--
-- The worker never holds a row in memory across a restart: every state change
-- below is a conditional UPDATE whose WHERE clause re-asserts the state the
-- worker believed it had. A daemon that dies mid-send leaves a 'sending' row
-- whose lease expires, and ReclaimExpiredEmailOutboxLeases hands it back.

-- Enqueue is idempotent on notification_id, which is the primary key. The
-- notification itself is already deduped permanently by event, so a re-observed
-- event inserts no notification and therefore owes no second email.
-- name: EnqueueNotificationEmail :execrows
INSERT INTO notification_email_outbox (
    notification_id, recipient, state, subject, body,
    attempt_count, max_attempts, next_attempt_at,
    last_error, created_at, updated_at
) VALUES (
    sqlc.arg(notification_id), sqlc.arg(recipient), 'pending',
    sqlc.arg(subject), sqlc.arg(body),
    0, sqlc.arg(max_attempts), sqlc.arg(next_attempt_at),
    '', sqlc.arg(created_at), sqlc.arg(created_at)
)
ON CONFLICT (notification_id) DO NOTHING;

-- name: GetNotificationEmailOutboxEntry :one
SELECT * FROM notification_email_outbox
WHERE notification_id = sqlc.arg(notification_id);

-- The claim candidates: due rows in a claimable state, oldest first. The worker
-- claims each one individually with ClaimNotificationEmail, so two workers
-- racing on the same batch is safe.
-- name: ListDueNotificationEmails :many
SELECT *
FROM notification_email_outbox
WHERE state IN ('pending', 'failed')
  AND next_attempt_at <= sqlc.arg(now)
ORDER BY next_attempt_at, notification_id
LIMIT sqlc.arg(page_limit);

-- Claim is the concurrency boundary. The WHERE re-asserts the claimable state,
-- so exactly one caller can move a given row into 'sending': a second one
-- updates zero rows and skips it. attempt_count is spent HERE rather than after
-- the send, so a crash mid-send still costs an attempt and a permanently
-- wedging message cannot retry forever.
-- name: ClaimNotificationEmail :execrows
UPDATE notification_email_outbox
SET state = 'sending',
    attempt_count = attempt_count + 1,
    lease_expires_at = sqlc.arg(lease_expires_at),
    updated_at = sqlc.arg(now)
WHERE notification_id = sqlc.arg(notification_id)
  AND state IN ('pending', 'failed')
  AND next_attempt_at <= sqlc.arg(now);

-- name: MarkNotificationEmailSent :execrows
UPDATE notification_email_outbox
SET state = 'sent',
    lease_expires_at = NULL,
    last_error = '',
    updated_at = sqlc.arg(now),
    completed_at = sqlc.arg(now)
WHERE notification_id = sqlc.arg(notification_id)
  AND state = 'sending';

-- A transient failure with attempts left: park until the backoff deadline.
-- name: MarkNotificationEmailRetry :execrows
UPDATE notification_email_outbox
SET state = 'failed',
    lease_expires_at = NULL,
    last_error = sqlc.arg(last_error),
    next_attempt_at = sqlc.arg(next_attempt_at),
    updated_at = sqlc.arg(now)
WHERE notification_id = sqlc.arg(notification_id)
  AND state = 'sending';

-- Terminal give-up: a permanent rejection, or the attempt budget is spent.
-- name: MarkNotificationEmailDead :execrows
UPDATE notification_email_outbox
SET state = 'dead',
    lease_expires_at = NULL,
    last_error = sqlc.arg(last_error),
    updated_at = sqlc.arg(now),
    completed_at = sqlc.arg(now)
WHERE notification_id = sqlc.arg(notification_id)
  AND state = 'sending';

-- Restart safety: a 'sending' row whose lease has expired was claimed by a
-- daemon that died. Its attempt was already counted, so it returns to 'failed'
-- (the retry state) and becomes due immediately rather than resetting.
-- name: ReclaimExpiredEmailOutboxLeases :execrows
UPDATE notification_email_outbox
SET state = 'failed',
    lease_expires_at = NULL,
    next_attempt_at = sqlc.arg(now),
    last_error = 'reclaimed: the sending daemon did not finish',
    updated_at = sqlc.arg(now)
WHERE state = 'sending'
  AND lease_expires_at IS NOT NULL
  AND lease_expires_at <= sqlc.arg(now);

-- Operator/test visibility.
-- name: CountNotificationEmailsByState :one
SELECT COUNT(*)
FROM notification_email_outbox
WHERE state = CAST(sqlc.arg(state) AS TEXT);
