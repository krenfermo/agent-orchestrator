package daemon

import (
	"context"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	settingssvc "github.com/aoagents/agent-orchestrator/backend/internal/service/settings"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// settingsStore adapts the SQLite store to the settings service's Store.
//
// The two define their own snapshot types so neither depends on the other's; this
// is the one place that knows both, keeping the translation in the wiring.
type settingsStore struct{ store *sqlite.Store }

var _ settingssvc.Store = settingsStore{}

func (s settingsStore) GetAppSettings(ctx context.Context) (settingssvc.Snapshot, error) {
	row, err := s.store.GetAppSettings(ctx)
	if err != nil {
		return settingssvc.Snapshot{}, err
	}
	return settingssvc.Snapshot{
		DefaultSessionMode: row.DefaultSessionMode,
		Email: settingssvc.EmailConfig{
			Enabled:            row.EmailNotifications.Enabled,
			Recipient:          row.EmailNotifications.Recipient,
			Host:               row.EmailNotifications.SMTPHost,
			Port:               row.EmailNotifications.SMTPPort,
			Username:           row.EmailNotifications.SMTPUsername,
			PasswordCiphertext: row.EmailNotifications.SMTPPasswordEncrypted,
			TLS:                row.EmailNotifications.SMTPTLS,
		},
		UpdatedAt: row.UpdatedAt,
	}, nil
}

func (s settingsStore) SetEmailNotifications(
	ctx context.Context,
	cfg settingssvc.EmailConfig,
	now time.Time,
) error {
	return s.store.SetEmailNotificationSettings(ctx, store.EmailNotificationSettings{
		Enabled:               cfg.Enabled,
		Recipient:             cfg.Recipient,
		SMTPHost:              cfg.Host,
		SMTPPort:              cfg.Port,
		SMTPUsername:          cfg.Username,
		SMTPPasswordEncrypted: cfg.PasswordCiphertext,
		SMTPTLS:               cfg.TLS,
	}, now)
}

func (s settingsStore) SetDefaultSessionMode(
	ctx context.Context,
	mode domain.SessionMode,
	now time.Time,
) error {
	return s.store.SetDefaultSessionMode(ctx, mode, now)
}
