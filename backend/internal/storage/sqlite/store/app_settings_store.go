package store

import (
	"context"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// Daemon-owned user preferences.
//
// The row is seeded by migration, so a read is a plain SELECT and no caller has
// to handle "settings do not exist yet".

// AppSettings is the durable preference set. Field-compatible with
// service/settings.Snapshot, which the daemon wiring adapts.
type AppSettings struct {
	// DefaultSessionMode is the interface a new session gets when the spawn does
	// not name one. Never applied to an existing session: only an explicit
	// interface transition changes a live session's committed mode, so
	// changing this only affects sessions created afterwards.
	DefaultSessionMode domain.SessionMode
	// EmailNotifications is the durable configuration for the optional
	// completion-email fan-out. SMTPPasswordEncrypted is ciphertext produced by
	// internal/secretbox — the store never sees, and never stores, plaintext.
	EmailNotifications EmailNotificationSettings
	UpdatedAt          time.Time
}

// EmailNotificationSettings is the stored SMTP destination for completion
// emails.
type EmailNotificationSettings struct {
	Enabled               bool
	Recipient             string
	SMTPHost              string
	SMTPPort              int
	SMTPUsername          string
	SMTPPasswordEncrypted string
	SMTPTLS               string
}

// GetAppSettings reads the preference row.
func (s *Store) GetAppSettings(ctx context.Context) (AppSettings, error) {
	row, err := s.qr.GetAppSettings(ctx)
	if err != nil {
		return AppSettings{}, fmt.Errorf("read app settings: %w", err)
	}
	return AppSettings{
		// Normalized on read: a value written by a build that knows a mode this
		// one does not must still resolve to something dispatchable.
		DefaultSessionMode: domain.NormalizeSessionMode(row.DefaultSessionMode),
		EmailNotifications: EmailNotificationSettings{
			Enabled:               row.EmailNotificationsEnabled != 0,
			Recipient:             row.EmailRecipient,
			SMTPHost:              row.SmtpHost,
			SMTPPort:              int(row.SmtpPort),
			SMTPUsername:          row.SmtpUsername,
			SMTPPasswordEncrypted: row.SmtpPasswordEncrypted,
			SMTPTLS:               row.SmtpTls,
		},
		UpdatedAt: row.UpdatedAt,
	}, nil
}

// SetEmailNotificationSettings persists the completion-email configuration.
// The password arrives already encrypted; passing plaintext here would put a
// readable credential in the database file and is never done.
func (s *Store) SetEmailNotificationSettings(
	ctx context.Context,
	cfg EmailNotificationSettings,
	now time.Time,
) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	enabled := int64(0)
	if cfg.Enabled {
		enabled = 1
	}
	if err := s.qw.SetEmailNotificationSettings(ctx, gen.SetEmailNotificationSettingsParams{
		EmailNotificationsEnabled: enabled,
		EmailRecipient:            cfg.Recipient,
		SmtpHost:                  cfg.SMTPHost,
		SmtpPort:                  int64(cfg.SMTPPort),
		SmtpUsername:              cfg.SMTPUsername,
		SmtpPasswordEncrypted:     cfg.SMTPPasswordEncrypted,
		SmtpTls:                   cfg.SMTPTLS,
		UpdatedAt:                 now,
	}); err != nil {
		// Deliberately does not wrap the params: a %v on them would print the
		// ciphertext, and error strings travel further than anyone expects.
		return fmt.Errorf("set email notification settings: %w", err)
	}
	return nil
}

// SetDefaultSessionMode persists the default interface for new sessions.
func (s *Store) SetDefaultSessionMode(ctx context.Context, mode domain.SessionMode, now time.Time) error {
	if !mode.Valid() {
		return fmt.Errorf("invalid session mode %q", mode)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.qw.SetDefaultSessionMode(ctx, gen.SetDefaultSessionModeParams{
		DefaultSessionMode: mode,
		UpdatedAt:          now,
	}); err != nil {
		return fmt.Errorf("set default session mode: %w", err)
	}
	return nil
}
