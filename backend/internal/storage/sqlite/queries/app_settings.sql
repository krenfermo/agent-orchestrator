-- Daemon-owned user preferences. One row, seeded by migration 0042, so a read
-- never has to handle absence.

-- name: GetAppSettings :one
SELECT * FROM app_settings WHERE id = 1;

-- name: SetDefaultSessionMode :exec
UPDATE app_settings SET default_session_mode = ?, updated_at = ? WHERE id = 1;

-- Email notification settings. The password is written pre-encrypted by the
-- Go layer (internal/secretbox); SQLite never sees the plaintext.
-- name: SetEmailNotificationSettings :exec
UPDATE app_settings
SET email_notifications_enabled = sqlc.arg(email_notifications_enabled),
    email_recipient             = sqlc.arg(email_recipient),
    smtp_host                   = sqlc.arg(smtp_host),
    smtp_port                   = sqlc.arg(smtp_port),
    smtp_username               = sqlc.arg(smtp_username),
    smtp_password_encrypted     = sqlc.arg(smtp_password_encrypted),
    smtp_tls                    = sqlc.arg(smtp_tls),
    updated_at                  = sqlc.arg(updated_at)
WHERE id = 1;
