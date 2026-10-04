-- The canonical notification model (P4-D): three run-scoped event types, and
-- the recipient/severity/delivery/provenance columns the model was missing.
--
-- WHAT THIS IS NOT. It does not add a second notifications table, a parallel
-- event log, or a new dedupe rule. The one `notifications` table (0011, 0031,
-- 0041, 0122, 0124) is already the durable authority behind the notification
-- center, its cursor-paginated history, its live stream, and the optional email
-- fan-out; a second store would immediately disagree with it about read and
-- resolution state. Everything here extends that table in place.
--
-- THE THREE NEW TYPES. Each is produced from a durable authority that already
-- exists, and each is event-keyed, so 0124's permanent idx_notifications_event_dedupe
-- is what makes "exactly one per real event" true across retries, reconciles
-- and restarts:
--
--   human_question_required  <- sessions.activity_state entering blocked
--   repair_exhausted         <- the attempt map persisted in pr.last_nudge_signature
--   integration_failed       <- a review_run row written with status failed
--
-- They are deliberately NOT a second spelling of workflow_needs_attention:
-- that one is raised by the workflow coordinator from an attention checkpoint
-- on a workflow RUN, while these three are session-scoped facts the lifecycle
-- reducer and the review engine observe. Both remain in place, unchanged.
--
-- Adding types needs a table rebuild for the same reason 0122 and 0124 did:
-- SQLite cannot alter a CHECK constraint in place. The rebuild reproduces
-- 0124's shape verbatim and appends the new columns.
--
-- recipient is the principal abstraction P4-D §12 asks for and nothing more.
-- AO runs single-user / local-trusted, so every row is addressed to 'local'.
-- The column exists so a later change (P4-B users/teams, P4-C tenants) can
-- introduce real principals by writing other values and scoping reads by
-- recipient. No users/teams table is created here and nothing reads recipient
-- as an identity yet: inventing one before there are multiple principals would
-- be a guess at its shape.

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
            'workflow_failed',
            'human_question_required',
            'repair_exhausted',
            'integration_failed'
        )
    ),
    title TEXT NOT NULL,
    body TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'unread' CHECK (status IN ('read', 'unread')),
    created_at TIMESTAMP NOT NULL,
    resolved_at TIMESTAMP,
    -- The principal this notification is addressed to. See the header.
    recipient TEXT NOT NULL DEFAULT 'local',
    -- task_id is the optional planned-task anchor P4-D asks the model to carry.
    -- Free-form: a planned task id lives in workflow_tasks, which a notification
    -- must not hard-reference, because a notification outlives the run it
    -- reports on.
    task_id TEXT NOT NULL DEFAULT '',
    -- How loudly to surface this, independent of type, so a delivery channel
    -- can filter without enumerating every type.
    severity TEXT NOT NULL DEFAULT 'info'
        CHECK (severity IN ('info', 'warning', 'critical')),
    -- WHEN the acknowledgement happened; `status` only records THAT it did.
    -- Both are written together and mean the same thing
    -- (status = 'read' <=> read_at IS NOT NULL). status stays because the REST
    -- and live wire shapes and the frontend already read it.
    read_at TIMESTAMP,
    -- Fan-out state to outbound channels. Persisting the row IS the in-app
    -- delivery, so rows are born 'delivered'; a row that also owes an email
    -- is written 'pending' and driven forward by the outbox (0155).
    delivery_state TEXT NOT NULL DEFAULT 'delivered'
        CHECK (delivery_state IN ('pending', 'delivered', 'failed', 'suppressed')),
    -- Provenance: which producer emitted this row, and the durable id of the
    -- event it read. Free-form on purpose: a new producer should not need a
    -- migration.
    source TEXT NOT NULL DEFAULT '',
    source_event_id TEXT NOT NULL DEFAULT '',
    CHECK (session_id IS NOT NULL OR workflow_run_id <> '')
);
-- +goose StatementEnd

-- +goose StatementBegin
INSERT INTO notifications_new
    (id, session_id, project_id, workflow_run_id, pr_url, dedupe_key,
     type, title, body, status, created_at, resolved_at,
     recipient, task_id, severity, read_at, delivery_state, source, source_event_id)
SELECT id, session_id, project_id, workflow_run_id, pr_url, dedupe_key,
       type, title, body, status, created_at, resolved_at,
       -- Every existing row belongs to the single local principal.
       'local',
       '',
       -- Backfill severity from what each existing type means.
       CASE
           WHEN type IN ('needs_input', 'pr_closed_unmerged',
                         'task_needs_attention', 'workflow_needs_attention')
               THEN 'warning'
           WHEN type IN ('task_failed', 'workflow_failed') THEN 'critical'
           ELSE 'info'
       END,
       -- status is the only record that an acknowledgement happened; created_at
       -- is the closest defensible stand-in for when. An unread row has none.
       CASE WHEN status = 'read' THEN created_at END,
       -- Existing rows were delivered in-app the moment they were written, and
       -- their email fan-out (if any) already ran or already failed silently.
       'delivered',
       'lifecycle',
       ''
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

-- 0124's four indexes, reproduced verbatim.
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

-- Recipient-scoped pagination. The (created_at DESC, id DESC) tail matches the
-- keyset cursor the history queries already use, so a recipient-scoped page
-- costs an index walk rather than a scan. P4-D §16.
-- +goose StatementBegin
CREATE INDEX idx_notifications_recipient_created_at
    ON notifications(recipient, created_at DESC, id DESC);
-- +goose StatementEnd
-- Recipient-scoped unread pagination and the unread COUNT, which must never
-- read the full history.
-- +goose StatementBegin
CREATE INDEX idx_notifications_recipient_status
    ON notifications(recipient, status, created_at DESC, id DESC);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- One-way, and deliberately so. Narrowing the type CHECK again would reject
-- rows this build has already written -- a session that asked a question, a
-- repair that ran out of attempts, a review that failed -- so a rollback that
-- tried to rebuild the narrow table would abort part-way on real data. Dropping
-- the added columns has the same problem from the other side: read_at and
-- delivery_state are the only record that an acknowledgement or a send
-- happened. Keeping the widened constraint and the columns is the same
-- best-effort shape 0028 and 0074 use, and it is safe against any data valid
-- under this migration's Up.
SELECT 1;
-- +goose StatementEnd
