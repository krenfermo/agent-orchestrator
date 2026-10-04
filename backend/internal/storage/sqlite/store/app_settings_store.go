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

// EmailNotificationSettings is the stored SMTP destination for notification
// emails, plus which events may use it.
type EmailNotificationSettings struct {
	Enabled               bool
	Recipient             string
	SMTPHost              string
	SMTPPort              int
	SMTPUsername          string
	SMTPPasswordEncrypted string
	SMTPTLS               string
	// OnCompleted/OnNeedsAttention/OnFailed select which events may be emailed.
	// All three are subordinate to Enabled: the master switch off means no mail
	// regardless of what is selected here.
	OnCompleted      bool
	OnNeedsAttention bool
	OnFailed         bool
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
			OnCompleted:           row.EmailOnCompleted != 0,
			OnNeedsAttention:      row.EmailOnNeedsAttention != 0,
			OnFailed:              row.EmailOnFailed != 0,
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
	flag := func(v bool) int64 {
		if v {
			return 1
		}
		return 0
	}
	if err := s.qw.SetEmailNotificationSettings(ctx, gen.SetEmailNotificationSettingsParams{
		EmailNotificationsEnabled: flag(cfg.Enabled),
		EmailRecipient:            cfg.Recipient,
		SmtpHost:                  cfg.SMTPHost,
		SmtpPort:                  int64(cfg.SMTPPort),
		SmtpUsername:              cfg.SMTPUsername,
		SmtpPasswordEncrypted:     cfg.SMTPPasswordEncrypted,
		SmtpTls:                   cfg.SMTPTLS,
		EmailOnCompleted:          flag(cfg.OnCompleted),
		EmailOnNeedsAttention:     flag(cfg.OnNeedsAttention),
		EmailOnFailed:             flag(cfg.OnFailed),
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
