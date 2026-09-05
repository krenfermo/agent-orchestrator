-- P4-E: external work-management integration.
--
-- Every read here is project-scoped, and that is the tenant boundary: a
-- project's tenancy is projects.tenant_id, and a caller that could not resolve
-- the project through authz never reaches these queries at all.

-- name: GetWorkItemConfig :one
SELECT * FROM work_item_configs WHERE project_id = ?;

-- name: ListWorkItemConfigs :many
-- The sync worker's working set: every project that has switched the
-- integration on. Disabled and unconfigured projects are excluded here rather
-- than filtered in Go, so a worker tick over an installation with one
-- configured project out of fifty reads one row.
SELECT * FROM work_item_configs WHERE enabled = 1 ORDER BY project_id;

-- name: UpsertWorkItemConfig :exec
INSERT INTO work_item_configs (
    project_id, provider, base_url, workspace,
    external_project_id, external_project_name, external_project_key,
    api_token_encrypted, enabled, sync_states, sync_comments,
    created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(project_id) DO UPDATE SET
    provider              = excluded.provider,
    base_url              = excluded.base_url,
    workspace             = excluded.workspace,
    external_project_id   = excluded.external_project_id,
    external_project_name = excluded.external_project_name,
    external_project_key  = excluded.external_project_key,
    api_token_encrypted   = excluded.api_token_encrypted,
    enabled               = excluded.enabled,
    sync_states           = excluded.sync_states,
    sync_comments         = excluded.sync_comments,
    updated_at            = excluded.updated_at;

-- name: SetWorkItemConfigCheck :exec
-- Records the last preflight, so the settings surface can report health
-- without probing the provider on every page load.
UPDATE work_item_configs
SET last_check_at = ?, last_check_ok = ?, last_check_error = ?, updated_at = ?
WHERE project_id = ?;

-- name: DeleteWorkItemConfig :execrows
DELETE FROM work_item_configs WHERE project_id = ?;

-- name: GetWorkItemLink :one
SELECT * FROM work_item_links WHERE id = ?;

-- name: GetWorkItemLinkByScope :one
SELECT * FROM work_item_links
WHERE project_id = ? AND scope = ? AND scope_id = ?;

-- name: ListWorkItemLinks :many
SELECT * FROM work_item_links WHERE project_id = ? ORDER BY created_at DESC;

-- name: ListWorkItemLinksByExternalItem :many
-- The inbound reconciliation read: an external item changed, which AO work is
-- it about. Answered from the indexed identifiers, never from a title.
SELECT * FROM work_item_links
WHERE provider = ? AND workspace = ? AND external_item_id = ?;

-- name: UpsertWorkItemLink :exec
-- Re-linking REPLACES. The unique index on (project_id, scope, scope_id) is
-- what makes "which item does this run update" a question with one answer.
INSERT INTO work_item_links (
    id, project_id, scope, scope_id,
    provider, workspace, external_project_id, external_item_id, external_item_key,
    origin, sync_enabled, last_seen_title, last_seen_state, last_seen_at,
    created_by, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(project_id, scope, scope_id) DO UPDATE SET
    provider            = excluded.provider,
    workspace           = excluded.workspace,
    external_project_id = excluded.external_project_id,
    external_item_id    = excluded.external_item_id,
    external_item_key   = excluded.external_item_key,
    origin              = excluded.origin,
    sync_enabled        = excluded.sync_enabled,
    last_seen_title     = excluded.last_seen_title,
    last_seen_state     = excluded.last_seen_state,
    last_seen_at        = excluded.last_seen_at,
    updated_at          = excluded.updated_at;

-- name: TouchWorkItemLinkSnapshot :exec
-- Refreshes the display cache only. It deliberately cannot change which item a
-- link points at: a provider read must never be able to re-target a link.
UPDATE work_item_links
SET last_seen_title = ?, last_seen_state = ?, last_seen_at = ?, updated_at = ?
WHERE id = ?;

-- name: SetWorkItemLinkSync :execrows
UPDATE work_item_links SET sync_enabled = ?, updated_at = ? WHERE id = ?;

-- name: DeleteWorkItemLink :execrows
DELETE FROM work_item_links WHERE id = ? AND project_id = ?;

-- name: EnqueueWorkItemSync :execrows
-- INSERT OR IGNORE against the unique dedupe key: the same real-world moment
-- enqueues once, however many times the lifecycle observes it. A caller that
-- gets 0 rows has not failed -- the intent was already recorded.
INSERT OR IGNORE INTO work_item_sync_outbox (
    id, project_id, link_id, event, body, target_state,
    dedupe_key, status, attempts, next_attempt_at, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, 'pending', 0, ?, ?, ?);

-- name: ClaimDueWorkItemSyncs :many
-- The worker's claim. Ordered oldest-first so a queue drains in the order the
-- events actually happened -- posting "completed" before "started" would make
-- an external board's history a lie.
SELECT * FROM work_item_sync_outbox
WHERE status = 'pending'
  AND (next_attempt_at IS NULL OR next_attempt_at <= ?)
ORDER BY created_at
LIMIT ?;

-- name: MarkWorkItemSyncDone :execrows
UPDATE work_item_sync_outbox
SET status = 'done', attempts = attempts + 1, last_error = '', updated_at = ?
WHERE id = ? AND status = 'pending';

-- name: DeferWorkItemSync :execrows
-- A transient failure: count the attempt, record why, and come back later.
UPDATE work_item_sync_outbox
SET attempts = attempts + 1, next_attempt_at = ?, last_error = ?, updated_at = ?
WHERE id = ? AND status = 'pending';

-- name: MarkWorkItemSyncFailed :execrows
-- Terminal. The row stays: an operator asking "why did this never reach Plane"
-- gets the reason instead of an absence.
UPDATE work_item_sync_outbox
SET status = 'failed', attempts = attempts + 1, last_error = ?, updated_at = ?
WHERE id = ? AND status = 'pending';

-- name: CountWorkItemSyncByStatus :many
SELECT status, COUNT(*) AS total
FROM work_item_sync_outbox WHERE project_id = ? GROUP BY status;

-- name: ListWorkItemSyncForProject :many
SELECT * FROM work_item_sync_outbox
WHERE project_id = ? ORDER BY created_at DESC LIMIT ?;

-- name: DeleteSettledWorkItemSyncsBefore :execrows
-- Retention for the drained queue. Failed rows are kept: they are the ones
-- somebody still has to look at.
DELETE FROM work_item_sync_outbox WHERE status = 'done' AND updated_at < ?;

-- name: InsertWorkItemSyncAudit :exec
INSERT INTO work_item_sync_audit (
    id, project_id, link_id, provider, operation,
    external_item_id, external_item_key, outcome, error_kind, detail,
    attempts, duration_ms, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: ListWorkItemSyncAudit :many
SELECT * FROM work_item_sync_audit
WHERE project_id = ? ORDER BY created_at DESC LIMIT ?;
