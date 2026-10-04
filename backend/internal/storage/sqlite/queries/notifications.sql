-- Notification queries.
--
-- Every list and count is scoped by recipient. AO is single-user today and the
-- only value written is 'local', so this changes no result -- it exists so the
-- authority boundary is in the SQL rather than in a caller's discipline, and so
-- P4-B/P4-C add principals without revisiting every query. See
-- domain.NotificationRecipient.

-- name: CreateNotification :one
INSERT INTO notifications (
    id, session_id, project_id, workflow_run_id, task_id, pr_url, dedupe_key,
    type, title, body, status, created_at, read_at,
    recipient, severity, delivery_state, source, source_event_id
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: ListUnreadNotificationsPage :many
SELECT *
FROM notifications
WHERE status = 'unread'
  AND recipient = CAST(sqlc.arg(recipient) AS TEXT)
  AND (
    CAST(sqlc.arg(before_id) AS TEXT) = ''
    OR created_at < sqlc.arg(before_created_at)
    OR (created_at = sqlc.arg(before_created_at) AND id < CAST(sqlc.arg(before_id) AS TEXT))
  )
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(page_limit);

-- Unresolved is the still-actionable set: the underlying issue has not gone
-- away yet. Terminal facts (pr_merged, pr_closed_unmerged) describe something
-- that already happened, so they are unseen-only and never listed here.
-- name: ListUnresolvedNotificationsPage :many
SELECT *
FROM notifications
WHERE resolved_at IS NULL
  AND type IN ('needs_input', 'ready_to_merge')
  AND recipient = CAST(sqlc.arg(recipient) AS TEXT)
  AND (
    CAST(sqlc.arg(before_id) AS TEXT) = ''
    OR created_at < sqlc.arg(before_created_at)
    OR (created_at = sqlc.arg(before_created_at) AND id < CAST(sqlc.arg(before_id) AS TEXT))
  )
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(page_limit);

-- name: ListNotificationsPage :many
SELECT *
FROM notifications
WHERE recipient = CAST(sqlc.arg(recipient) AS TEXT)
  AND (
    CAST(sqlc.arg(before_id) AS TEXT) = ''
    OR created_at < sqlc.arg(before_created_at)
    OR (created_at = sqlc.arg(before_created_at) AND id < CAST(sqlc.arg(before_id) AS TEXT))
  )
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(page_limit);

-- Served by idx_notifications_recipient_status: the unread badge must never
-- read the full notification history.
-- name: CountUnreadNotifications :one
SELECT COUNT(*)
FROM notifications
WHERE status = 'unread'
  AND recipient = CAST(sqlc.arg(recipient) AS TEXT);

-- name: CountUnresolvedNotifications :one
SELECT COUNT(*)
FROM notifications
WHERE resolved_at IS NULL
  AND type IN ('needs_input', 'ready_to_merge')
  AND recipient = CAST(sqlc.arg(recipient) AS TEXT);

-- status and read_at carry the same fact and are always written together:
-- status = 'read' <=> read_at IS NOT NULL. Both updates are already idempotent
-- through the status = 'unread' guard, so replaying a mark-read is a no-op
-- rather than a second timestamp.
-- name: MarkNotificationRead :one
UPDATE notifications
SET status = 'read', read_at = sqlc.arg(read_at)
WHERE id = sqlc.arg(id) AND status = 'unread'
RETURNING *;

-- name: MarkAllNotificationsRead :execrows
UPDATE notifications
SET status = 'read', read_at = sqlc.arg(read_at)
WHERE status = 'unread'
  AND recipient = CAST(sqlc.arg(recipient) AS TEXT);

-- name: ResolveSessionNotificationsByType :many
UPDATE notifications
SET resolved_at = sqlc.arg(resolved_at)
WHERE session_id = sqlc.arg(session_id)
  AND type = sqlc.arg(type)
  AND resolved_at IS NULL
RETURNING *;

-- name: ResolvePRNotificationsByType :many
UPDATE notifications
SET resolved_at = sqlc.arg(resolved_at)
WHERE pr_url = sqlc.arg(pr_url)
  AND type = sqlc.arg(type)
  AND resolved_at IS NULL
RETURNING *;

-- Readiness is more than open/closed: draft, CI, review decision, unresolved
-- human comments, and mergeability all block a merge. Rather than restate that
-- rule in SQL and let it drift from the live path, this returns the open rows
-- and lets domain.MergeReadiness judge them against the stored PR facts.
-- name: ListOpenReadyToMergeNotifications :many
SELECT *
FROM notifications
WHERE type = 'ready_to_merge'
  AND resolved_at IS NULL;

-- Restart reconciliation: a resolution transition observed while the daemon was
-- down never reaches lifecycle, so open rows are re-checked against the durable
-- session/PR facts on startup.
-- name: ResolveStaleNeedsInputNotifications :many
UPDATE notifications
SET resolved_at = sqlc.arg(resolved_at)
WHERE type = 'needs_input'
  AND resolved_at IS NULL
  AND session_id IN (
    SELECT id FROM sessions
    WHERE is_terminated = TRUE
       OR activity_state NOT IN ('waiting_input', 'blocked')
  )
RETURNING *;

-- name: GetOpenNotificationByDedupe :one
SELECT *
FROM notifications
WHERE COALESCE(session_id, '') = CAST(sqlc.arg(session_id) AS TEXT)
  AND type = sqlc.arg(type)
  AND pr_url = sqlc.arg(pr_url)
  AND dedupe_key = ''
  AND (status = 'unread' OR resolved_at IS NULL)
LIMIT 1;

-- The permanent, event-scoped counterpart to GetOpenNotificationByDedupe: it
-- matches whether or not the row is still open, so one completion event can
-- never produce a second notification -- not on a retry, not after a restart.
-- name: GetNotificationByEventDedupe :one
SELECT *
FROM notifications
WHERE type = sqlc.arg(type)
  AND dedupe_key = CAST(sqlc.arg(dedupe_key) AS TEXT)
LIMIT 1;

-- P4-C: the organization-scoped halves of the reads above.
--
-- They are separate queries rather than an optional clause on the originals
-- because sqlc.slice() renders IN () for an empty set, which SQLite rejects --
-- so an "unscoped" caller would have to pass a dummy id and a flag to ignore
-- it, and a mistake in that flag would silently unscope a scoped read. Two
-- queries where the scoped one CANNOT be called without a project set is the
-- version that fails closed.
--
-- A notification is scoped by its project because that is what it is about:
-- every row carries a NOT NULL project_id, and P4-C decided that a person who
-- may not see a project may not see anything AO says about it either.

-- name: ListUnreadNotificationsPageInProjects :many
SELECT *
FROM notifications
WHERE status = 'unread'
  AND recipient = CAST(sqlc.arg(recipient) AS TEXT)
  AND project_id IN (sqlc.slice('project_ids'))
  AND (
    CAST(sqlc.arg(before_id) AS TEXT) = ''
    OR created_at < sqlc.arg(before_created_at)
    OR (created_at = sqlc.arg(before_created_at) AND id < CAST(sqlc.arg(before_id) AS TEXT))
  )
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(page_limit);

-- name: ListUnresolvedNotificationsPageInProjects :many
SELECT *
FROM notifications
WHERE resolved_at IS NULL
  AND type IN ('needs_input', 'ready_to_merge')
  AND recipient = CAST(sqlc.arg(recipient) AS TEXT)
  AND project_id IN (sqlc.slice('project_ids'))
  AND (
    CAST(sqlc.arg(before_id) AS TEXT) = ''
    OR created_at < sqlc.arg(before_created_at)
    OR (created_at = sqlc.arg(before_created_at) AND id < CAST(sqlc.arg(before_id) AS TEXT))
  )
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(page_limit);

-- name: ListNotificationsPageInProjects :many
SELECT *
FROM notifications
WHERE recipient = CAST(sqlc.arg(recipient) AS TEXT)
  AND project_id IN (sqlc.slice('project_ids'))
  AND (
    CAST(sqlc.arg(before_id) AS TEXT) = ''
    OR created_at < sqlc.arg(before_created_at)
    OR (created_at = sqlc.arg(before_created_at) AND id < CAST(sqlc.arg(before_id) AS TEXT))
  )
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(page_limit);

-- name: CountUnreadNotificationsInProjects :one
SELECT COUNT(*)
FROM notifications
WHERE status = 'unread'
  AND recipient = CAST(sqlc.arg(recipient) AS TEXT)
  AND project_id IN (sqlc.slice('project_ids'));

-- name: CountUnresolvedNotificationsInProjects :one
SELECT COUNT(*)
FROM notifications
WHERE resolved_at IS NULL
  AND type IN ('needs_input', 'ready_to_merge')
  AND recipient = CAST(sqlc.arg(recipient) AS TEXT)
  AND project_id IN (sqlc.slice('project_ids'));

-- name: MarkAllNotificationsReadInProjects :execrows
UPDATE notifications
SET status = 'read', read_at = sqlc.arg(read_at)
WHERE status = 'unread'
  AND recipient = CAST(sqlc.arg(recipient) AS TEXT)
  AND project_id IN (sqlc.slice('project_ids'));

-- GetNotificationProject answers "which project is this notification about",
-- which is the only question the acknowledgement routes need in order to
-- refuse one belonging to a project the caller cannot see. Deliberately
-- narrower than fetching the row: the caller must not learn the title of a
-- notification it is about to be told does not exist.
-- name: GetNotificationProject :one
SELECT project_id FROM notifications WHERE id = ?;
