-- Which events the email fan-out is allowed to send.
--
-- 0123 shipped one switch: email on or off. That was enough while "finished"
-- was the only thing AO could email about. Now that a stop and a failure can
-- also be emailed, the user needs to be able to say which of the three they
-- want, without having to give up the other two to silence one.
--
-- All three default to 1, so nothing changes for an install that already had
-- email on: it keeps getting completion mail exactly as before, and gains the
-- two events it had no way to hear about at all. The master switch
-- (email_notifications_enabled) still gates everything and still defaults off,
-- so no install starts sending mail it was not already sending.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE app_settings ADD COLUMN email_on_completed INTEGER NOT NULL DEFAULT 1;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE app_settings ADD COLUMN email_on_needs_attention INTEGER NOT NULL DEFAULT 1;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE app_settings ADD COLUMN email_on_failed INTEGER NOT NULL DEFAULT 1;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE app_settings DROP COLUMN email_on_failed;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE app_settings DROP COLUMN email_on_needs_attention;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE app_settings DROP COLUMN email_on_completed;
-- +goose StatementEnd
