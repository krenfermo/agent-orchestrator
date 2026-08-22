package settings

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/mailer"
)

// EmailSettings is the completion-email configuration as the service and its
// callers exchange it.
//
// There is deliberately no password field on the way OUT. The stored secret is
// write-only from every caller's point of view: Get reports whether one is set
// and nothing more, which is what keeps it out of API responses, logs, support
// bundles, and screenshots by construction rather than by remembering to strip
// it at each boundary.
type EmailSettings struct {
	Enabled     bool
	Recipient   string
	Host        string
	Port        int
	Username    string
	TLS         mailer.TLSMode
	PasswordSet bool
}

// EmailUpdate is one requested change to the completion-email configuration.
type EmailUpdate struct {
	Enabled   bool
	Recipient string
	Host      string
	Port      int
	Username  string
	TLS       string
	// Password carries a new plaintext password. Nil means "leave the stored
	// one alone" — the UI never receives the current password, so it cannot
	// echo it back, and without this distinction every save from a form the
	// user did not retype would silently erase the credential. A non-nil
	// pointer to "" is an explicit clear.
	Password *string
}

// DefaultSMTPPort is the STARTTLS submission port, and what Gmail's app
// passwords expect.
const DefaultSMTPPort = 587

// EmailNotifications returns the current completion-email configuration.
func (s *Service) EmailNotifications(ctx context.Context) (EmailSettings, error) {
	snapshot, err := s.store.GetAppSettings(ctx)
	if err != nil {
		return EmailSettings{}, err
	}
	return emailSettingsFrom(snapshot.Email), nil
}

// SetEmailNotifications validates and persists the completion-email
// configuration, encrypting a supplied password before it reaches storage.
func (s *Service) SetEmailNotifications(ctx context.Context, update EmailUpdate) (EmailSettings, error) {
	current, err := s.store.GetAppSettings(ctx)
	if err != nil {
		return EmailSettings{}, err
	}
	next, err := s.resolveEmailUpdate(current.Email, update)
	if err != nil {
		return EmailSettings{}, err
	}
	if err := s.store.SetEmailNotifications(ctx, next, s.now()); err != nil {
		return EmailSettings{}, err
	}
	return emailSettingsFrom(next), nil
}

// SendTestEmail sends one message to the configured recipient so the user finds
// out here, with the form in front of them, rather than the first time a task
// finishes at 2am.
//
// It deliberately does NOT require Enabled: proving the credentials work is
// exactly what you do before turning the feature on.
func (s *Service) SendTestEmail(ctx context.Context) error {
	if s.sender == nil {
		return apierr.Invalid("EMAIL_SENDER_UNAVAILABLE", "Email sending is not available in this build", nil)
	}
	cfg, err := s.mailerConfig(ctx)
	if err != nil {
		return err
	}
	err = s.sender.Send(cfg, mailer.Message{
		Subject: "Agent Orchestrator test email",
		Body: "This is a test message from Agent Orchestrator.\n\n" +
			"If you are reading it, completion notifications can reach this address.",
	})
	if err != nil {
		// The mailer never puts the password in an error, so this is safe to
		// hand back to the user — and the whole point of a test button is that
		// the real reason reaches them.
		return apierr.Invalid("EMAIL_TEST_FAILED", err.Error(), nil)
	}
	return nil
}

// mailerConfig resolves the stored settings into a sendable config, decrypting
// the password at the last possible moment.
func (s *Service) mailerConfig(ctx context.Context) (mailer.Config, error) {
	snapshot, err := s.store.GetAppSettings(ctx)
	if err != nil {
		return mailer.Config{}, err
	}
	stored := snapshot.Email
	password := ""
	if stored.PasswordCiphertext != "" {
		if s.secrets == nil {
			return mailer.Config{}, errors.New("settings: secret box is required to read the SMTP password")
		}
		password, err = s.secrets.Open(stored.PasswordCiphertext)
		if err != nil {
			return mailer.Config{}, apierr.Invalid(
				"EMAIL_PASSWORD_UNREADABLE",
				"The stored SMTP password could not be decrypted. Re-enter it in Settings.",
				nil,
			)
		}
	}
	tls, err := mailer.ParseTLSMode(stored.TLS)
	if err != nil {
		return mailer.Config{}, apierr.Invalid("EMAIL_TLS_INVALID", err.Error(), nil)
	}
	cfg := mailer.Config{
		Recipient: stored.Recipient,
		Host:      stored.Host,
		Port:      stored.Port,
		Username:  stored.Username,
		Password:  password,
		TLS:       tls,
	}
	if err := cfg.Validate(); err != nil {
		return mailer.Config{}, apierr.Invalid("EMAIL_SETTINGS_INCOMPLETE", err.Error(), nil)
	}
	return cfg, nil
}

func (s *Service) resolveEmailUpdate(current EmailConfig, update EmailUpdate) (EmailConfig, error) {
	tls, err := mailer.ParseTLSMode(update.TLS)
	if err != nil {
		return EmailConfig{}, apierr.Invalid("EMAIL_TLS_INVALID", err.Error(), nil)
	}
	port := update.Port
	if port == 0 {
		port = DefaultSMTPPort
	}
	next := EmailConfig{
		Enabled:            update.Enabled,
		Recipient:          strings.TrimSpace(update.Recipient),
		Host:               strings.TrimSpace(update.Host),
		Port:               port,
		Username:           strings.TrimSpace(update.Username),
		PasswordCiphertext: current.PasswordCiphertext,
		TLS:                string(tls),
	}
	if update.Password != nil {
		if *update.Password == "" {
			next.PasswordCiphertext = ""
		} else {
			if s.secrets == nil {
				return EmailConfig{}, errors.New("settings: secret box is required to store an SMTP password")
			}
			sealed, sealErr := s.secrets.Seal(*update.Password)
			if sealErr != nil {
				return EmailConfig{}, fmt.Errorf("settings: seal smtp password: %w", sealErr)
			}
			next.PasswordCiphertext = sealed
		}
	}
	// Only a configuration that is being switched ON has to be complete. A user
	// half-way through filling the form, or deliberately turning the feature
	// off, must still be able to save what they have.
	if next.Enabled {
		probe := mailer.Config{
			Recipient: next.Recipient,
			Host:      next.Host,
			Port:      next.Port,
			Username:  next.Username,
			TLS:       tls,
		}
		if next.PasswordCiphertext != "" {
			// Validate only needs to know a password EXISTS; it is never
			// decrypted here.
			probe.Password = "set"
		}
		if err := probe.Validate(); err != nil {
			return EmailConfig{}, apierr.Invalid("EMAIL_SETTINGS_INCOMPLETE", err.Error(), nil)
		}
	}
	return next, nil
}

func emailSettingsFrom(cfg EmailConfig) EmailSettings {
	tls, err := mailer.ParseTLSMode(cfg.TLS)
	if err != nil {
		tls = mailer.TLSStartTLS
	}
	port := cfg.Port
	if port == 0 {
		port = DefaultSMTPPort
	}
	return EmailSettings{
		Enabled:     cfg.Enabled,
		Recipient:   cfg.Recipient,
		Host:        cfg.Host,
		Port:        port,
		Username:    cfg.Username,
		TLS:         tls,
		PasswordSet: cfg.PasswordCiphertext != "",
	}
}
