-- The durable email outbox (P4-D §6, §7).
--
-- WHY THIS EXISTS. Email fan-out today is a goroutine with a background
-- context: notify.Manager.fanOutEmail sends and logs whatever happens. That is
-- correct about the thing it protects -- a mail server must never fail the work
-- being reported -- but it makes delivery entirely non-durable. A daemon that
-- exits between the notification INSERT and the SMTP handshake loses the email
-- with no record that it was ever owed, and a mail server that is down for
-- ninety seconds loses every notification raised in that window. Neither is
-- visible afterwards: the only trace is a log line.
--
-- The outbox makes the OWING durable. A row is written in the same transaction
-- as nothing -- see below -- but it is written before any send is attempted, so
-- a crash at any point leaves a row that says what is still owed, and the
-- worker picks it up on the next pass.
--
-- SEMANTICS. In-app notification delivery is exactly-once: the notifications
-- row IS the delivery, and 0124's permanent event-dedupe index makes the INSERT
-- itself idempotent. Email is AT-LEAST-ONCE and says so out loud: a send that
-- succeeds on the wire but crashes before the row is marked 'sent' is retried,
-- and SMTP has no way to make that exactly-once. Duplicate mail is the failure
-- mode chosen over lost mail, which is the right trade for "your workflow
-- stopped at 2am".
--
-- ONE ROW PER NOTIFICATION. notification_id is the primary key, not a
-- surrogate. The notification row is already deduped permanently by event, so
-- keying the outbox by it inherits that idempotency exactly: re-observing the
-- same event inserts no notification, and therefore owes no second email.
--
-- STATES. pending -> sending -> sent, with failed as the retry state and dead
-- as the terminal give-up:
--
--   pending  nothing has claimed it yet
--   sending  a worker holds it; attempt_count already incremented
--   sent     the transport accepted it; terminal
--   failed   a transient failure; next_attempt_at holds the backoff deadline
--   dead     permanently rejected, or the attempt budget is spent; terminal
--
-- A 'sending' row whose lease expired is reclaimable: the daemon that held it
-- died. That is what makes a crash mid-send converge rather than strand.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE notification_email_outbox (
    -- The notification this email reports. One email owed per notification,
    -- inheriting the notification's own permanent event dedupe.
    notification_id TEXT PRIMARY KEY
        REFERENCES notifications(id) ON DELETE CASCADE,
    -- Denormalized so the worker can select and route without joining, and so
    -- an entry stays explainable after its notification is gone.
    recipient TEXT NOT NULL DEFAULT 'local',
    state TEXT NOT NULL DEFAULT 'pending'
        CHECK (state IN ('pending', 'sending', 'sent', 'failed', 'dead')),
    -- Rendered at enqueue time from the notification, so a retry sends the same
    -- message the event produced rather than re-rendering against state that
    -- has since moved on. Bodies here are the concise summaries P4-D §9
    -- prescribes -- never prompts, transcripts, secrets, or provider output.
    subject TEXT NOT NULL DEFAULT '',
    body TEXT NOT NULL DEFAULT '',
    attempt_count INTEGER NOT NULL DEFAULT 0,
    -- The retry budget for THIS row, frozen at enqueue, so raising the default
    -- does not silently revive rows already given up on.
    max_attempts INTEGER NOT NULL DEFAULT 5,
    -- When this row next becomes eligible. Set to the enqueue time for a fresh
    -- row and to the backoff deadline after a transient failure.
    next_attempt_at TIMESTAMP NOT NULL,
    -- Lease held while state = 'sending'. A row whose lease is in the past was
    -- claimed by a daemon that died; it is reclaimable.
    lease_expires_at TIMESTAMP,
    -- The last failure, for the operator. Bounded by the writer.
    last_error TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    -- Terminal states carry a completion time; live states do not.
    completed_at TIMESTAMP,
    CHECK (attempt_count >= 0),
    CHECK (max_attempts > 0)
);
-- +goose StatementEnd

-- The worker's claim query: the oldest due row in a claimable state. Partial,
-- because a healthy install is almost entirely 'sent' rows and the index should
-- not carry them.
-- +goose StatementBegin
CREATE INDEX idx_notification_email_outbox_due
    ON notification_email_outbox(next_attempt_at, notification_id)
    WHERE state IN ('pending', 'failed');
-- +goose StatementEnd

-- Reclaiming rows a dead daemon left mid-send.
-- +goose StatementBegin
CREATE INDEX idx_notification_email_outbox_leased
    ON notification_email_outbox(lease_expires_at)
    WHERE state = 'sending';
-- +goose StatementEnd

-- Operator visibility: "what is stuck, what did we give up on".
-- +goose StatementBegin
CREATE INDEX idx_notification_email_outbox_state
    ON notification_email_outbox(state, created_at DESC);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS notification_email_outbox;
-- +goose StatementEnd
