-- Completion notifications: "your task finished" and "your workflow finished".
--
-- Three schema facts have to change together, and all three need a table
-- rebuild because SQLite cannot alter a CHECK constraint in place.
--
-- 1. `type` gains 'task_completed' and 'workflow_completed'.
--
-- 2. `session_id` becomes nullable. A workflow run is not a session and has no
--    session to borrow -- a master run coordinates many of them -- so the old
--    NOT NULL would have forced a run-level notification to invent an owner.
--    The new CHECK keeps every row anchored to exactly one of the two.
--
-- 3. `dedupe_key` is added. The existing open-row dedupe index only holds while
--    a row is unseen or still unresolved, which is right for an issue that
--    comes and goes but wrong for "this finished": once the user reads it, the
--    same completion could be reported again by a retry or a daemon restart.
--    dedupe_key names the one real-world EVENT a row reports (a session's
--    completion receipt timestamp, a workflow run id) and a permanent unique
--    index over it makes a second row for that event impossible. It stays ''
--    for the four pre-existing types, whose dedupe semantics are unchanged.

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
INSERT INTO notifications_new
    (id, session_id, project_id, workflow_run_id, pr_url, dedupe_key,
     type, title, body, status, created_at, resolved_at)
SELECT id, session_id, project_id, '', pr_url, '',
       type, title, body, status, created_at, resolved_at
FROM notifications;
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

-- The pre-existing open-row dedupe, unchanged in meaning for the rows it still
-- governs. Two additions, both consequences of dedupe_key existing:
--
-- COALESCE keeps it usable now that session_id can be NULL -- SQLite treats
-- NULLs as distinct in a unique index, so a run-level row would otherwise never
-- dedupe here at all.
--
-- dedupe_key = '' scopes it to rows that opt into open-row dedupe. An
-- event-keyed row is deduped by its event instead, and must not also be
-- collapsed by session+type: two turns of the same session both finishing, or
-- two runs of the same project both completing, are different events and each
-- deserves its own row.
-- +goose StatementBegin
CREATE UNIQUE INDEX idx_notifications_open_dedupe
    ON notifications(COALESCE(session_id, ''), type, pr_url)
    WHERE dedupe_key = '' AND (status = 'unread' OR resolved_at IS NULL);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_notifications_unresolved
    ON notifications(resolved_at, created_at DESC, id DESC);
-- +goose StatementEnd

-- Permanent and unconditional: one row per reported event, forever.
-- +goose StatementBegin
CREATE UNIQUE INDEX idx_notifications_event_dedupe
    ON notifications(type, dedupe_key)
    WHERE dedupe_key <> '';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE TABLE notifications_old (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    pr_url TEXT NOT NULL DEFAULT '',
    type TEXT NOT NULL CHECK (
        type IN (
            'needs_input',
            'ready_to_merge',
            'pr_merged',
            'pr_closed_unmerged'
        )
    ),
    title TEXT NOT NULL,
    body TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'unread' CHECK (status IN ('read', 'unread')),
    created_at TIMESTAMP NOT NULL,
    resolved_at TIMESTAMP
);
-- +goose StatementEnd

-- +goose StatementBegin
INSERT INTO notifications_old
    (id, session_id, project_id, pr_url, type, title, body, status, created_at, resolved_at)
SELECT id, session_id, project_id, pr_url, type, title, body, status, created_at, resolved_at
FROM notifications
WHERE session_id IS NOT NULL
  AND type IN ('needs_input', 'ready_to_merge', 'pr_merged', 'pr_closed_unmerged');
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
    ON notifications(session_id, type, pr_url)
    WHERE status = 'unread' OR resolved_at IS NULL;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX idx_notifications_unresolved
    ON notifications(resolved_at, created_at DESC, id DESC);
-- +goose StatementEnd
