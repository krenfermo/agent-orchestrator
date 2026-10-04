-- Daemon-owned email notification settings, alongside the existing
-- default_session_mode preference in the same singleton row.
--
-- Why daemon-owned rather than renderer-owned: the sender runs in the daemon,
-- and a completion can be reported while no window is open at all. A renderer
-- held preference would simply never be consulted.
--
-- smtp_password_encrypted holds AES-256-GCM ciphertext, never plaintext and
-- never a hash. A hash is not an option here: SMTP AUTH needs the original
-- secret back, so this is one of the rare credentials AO must be able to
-- decrypt. The key lives in a 0600 file under the AO data dir (see
-- internal/secretbox), so the DB alone is not enough to recover it, and the
-- value is never returned by any API response or written to any log.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE app_settings ADD COLUMN email_notifications_enabled INTEGER NOT NULL DEFAULT 0;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE app_settings ADD COLUMN email_recipient TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE app_settings ADD COLUMN smtp_host TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE app_settings ADD COLUMN smtp_port INTEGER NOT NULL DEFAULT 587;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE app_settings ADD COLUMN smtp_username TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE app_settings ADD COLUMN smtp_password_encrypted TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd
-- smtp_tls: 'starttls' (587, the Gmail app-password default), 'implicit' (465),
-- or 'none' for a local relay that offers no TLS at all.
-- +goose StatementBegin
ALTER TABLE app_settings ADD COLUMN smtp_tls TEXT NOT NULL DEFAULT 'starttls'
    CHECK (smtp_tls IN ('starttls', 'implicit', 'none'));
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE app_settings DROP COLUMN smtp_tls;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE app_settings DROP COLUMN smtp_password_encrypted;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE app_settings DROP COLUMN smtp_username;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE app_settings DROP COLUMN smtp_port;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE app_settings DROP COLUMN smtp_host;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE app_settings DROP COLUMN email_recipient;
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE app_settings DROP COLUMN email_notifications_enabled;
-- +goose StatementEnd
