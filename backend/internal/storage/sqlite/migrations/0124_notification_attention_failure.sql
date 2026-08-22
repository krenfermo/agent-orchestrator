-- Attention and failure notifications: "your task needs a decision" and
-- "your workflow ended without completing".
--
-- Until now the only durable notification a run could raise was its own
-- completion, so the one outcome that actually needs a person -- a run parked
-- in needs_attention, or one that ended failed -- reached nobody unless they
-- happened to be looking at the Board. A run that stops at 2am on an exhausted
-- fix budget is precisely the case the optional email fan-out exists for.
--
-- Adding the four types needs a table rebuild for the same reason 0122 did:
-- SQLite cannot alter a CHECK constraint in place. Everything else about the
-- table -- the nullable session_id, the anchoring CHECK, dedupe_key and both
-- dedupe indexes -- is 0122's, reproduced verbatim. The new types are
-- event-keyed like the completion pair, so the permanent
-- idx_notifications_event_dedupe is what makes "exactly one per stop" true
-- across retries, reconciles, and restarts.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE notifications_new (
    id TEXT PRIMARY KEY,
    session_id TEXT REFERENCES sessions(id) ON DELETE CASCADE,
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    workflow_run_id TEXT NOT NULL DEFAULT '',
    pr_url TEXT NOT NULL DEFAULT '',
    dedupe_key TEXT NOT NULL DEFAULT '',
    type TEXT NOT NULL CHECK (
        type IN (
            'needs_input',
            'ready_to_merge',
            'pr_merged',
            'pr_closed_unmerged',
            'task_completed',
            'workflow_completed',
            'task_needs_attention',
            'workflow_needs_attention',
            'task_failed',
            'workflow_failed'
        )
    ),
    title TEXT NOT NULL,
    body TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'unread' CHECK (status IN ('read', 'unread')),
    created_at TIMESTAMP NOT NULL,
    resolved_at TIMESTAMP,
    CHECK (session_id IS NOT NULL OR workflow_run_id <> '')
);
-- +goose StatementEnd

-- +goose StatementBegin
INSERT INTO notifications_new
    (id, session_id, project_id, workflow_run_id, pr_url, dedupe_key,
     type, title, body, status, created_at, resolved_at)
SELECT id, session_id, project_id, workflow_run_id, pr_url, dedupe_key,
       type, title, body, status, created_at, resolved_at
FROM notifications;
-- +goose StatementEnd

-- +goose StatementBegin
DROP INDEX IF EXISTS idx_notifications_event_dedupe;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_notifications_unresolved;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_notifications_open_dedupe;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_notifications_status;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE notifications;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE notifications_new RENAME TO notifications;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_notifications_status
    ON notifications(status, created_at DESC);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE UNIQUE INDEX idx_notifications_open_dedupe
    ON notifications(COALESCE(session_id, ''), type, pr_url)
    WHERE dedupe_key = '' AND (status = 'unread' OR resolved_at IS NULL);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX idx_notifications_unresolved
    ON notifications(resolved_at, created_at DESC, id DESC);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE UNIQUE INDEX idx_notifications_event_dedupe
    ON notifications(type, dedupe_key)
    WHERE dedupe_key <> '';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE TABLE notifications_old (
    id TEXT PRIMARY KEY,
    session_id TEXT REFERENCES sessions(id) ON DELETE CASCADE,
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    workflow_run_id TEXT NOT NULL DEFAULT '',
    pr_url TEXT NOT NULL DEFAULT '',
    dedupe_key TEXT NOT NULL DEFAULT '',
    type TEXT NOT NULL CHECK (
        type IN (
            'needs_input',
            'ready_to_merge',
            'pr_merged',
            'pr_closed_unmerged',
            'task_completed',
            'workflow_completed'
        )
    ),
    title TEXT NOT NULL,
    body TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'unread' CHECK (status IN ('read', 'unread')),
    created_at TIMESTAMP NOT NULL,
    resolved_at TIMESTAMP,
    CHECK (session_id IS NOT NULL OR workflow_run_id <> '')
);
-- +goose StatementEnd

-- +goose StatementBegin
INSERT INTO notifications_old
    (id, session_id, project_id, workflow_run_id, pr_url, dedupe_key,
     type, title, body, status, created_at, resolved_at)
SELECT id, session_id, project_id, workflow_run_id, pr_url, dedupe_key,
       type, title, body, status, created_at, resolved_at
FROM notifications
WHERE type NOT IN ('task_needs_attention', 'workflow_needs_attention',
                   'task_failed', 'workflow_failed');
-- +goose StatementEnd

-- +goose StatementBegin
DROP INDEX IF EXISTS idx_notifications_event_dedupe;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_notifications_unresolved;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_notifications_open_dedupe;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_notifications_status;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE notifications;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE notifications_old RENAME TO notifications;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_notifications_status
    ON notifications(status, created_at DESC);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE UNIQUE INDEX idx_notifications_open_dedupe
    ON notifications(COALESCE(session_id, ''), type, pr_url)
    WHERE dedupe_key = '' AND (status = 'unread' OR resolved_at IS NULL);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX idx_notifications_unresolved
    ON notifications(resolved_at, created_at DESC, id DESC);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE UNIQUE INDEX idx_notifications_event_dedupe
    ON notifications(type, dedupe_key)
    WHERE dedupe_key <> '';
-- +goose StatementEnd
