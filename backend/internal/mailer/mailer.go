// Package mailer sends AO's optional completion emails over SMTP.
//
// Two rules shape everything here. A send is never allowed to fail the work it
// reports on — the caller treats every error as a log line, never as a failed
// task or workflow. And the password never leaves this package except into the
// SMTP AUTH exchange: Config's String/LogValue redact it, and no error this
// package returns embeds it.
package mailer

import (
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net"
	"net/smtp"
	"strings"
	"time"
)

// TLSMode selects how the connection to the SMTP server is protected.
type TLSMode string

const (
	// TLSStartTLS connects in the clear and upgrades with STARTTLS. This is the
	// port-587 default and what Gmail expects with an app password.
	TLSStartTLS TLSMode = "starttls"
	// TLSImplicit wraps the whole connection in TLS from the first byte
	// (port 465).
	TLSImplicit TLSMode = "implicit"
	// TLSNone sends in the clear. Only sensible for a relay on localhost.
	TLSNone TLSMode = "none"
)

// Valid reports whether m is a supported TLS mode.
func (m TLSMode) Valid() bool {
	switch m {
	case TLSStartTLS, TLSImplicit, TLSNone:
		return true
	default:
		return false
	}
}

// ParseTLSMode parses a stored or wire value, defaulting an empty one to
// STARTTLS rather than to the insecure mode.
func ParseTLSMode(v string) (TLSMode, error) {
	trimmed := TLSMode(strings.TrimSpace(strings.ToLower(v)))
	if trimmed == "" {
		return TLSStartTLS, nil
	}
	if !trimmed.Valid() {
		return "", fmt.Errorf("unsupported TLS mode %q", v)
	}
	return trimmed, nil
}

// Config is one resolved SMTP destination.
type Config struct {
	Recipient string
	Host      string
	Port      int
	Username  string
	// Password is held in memory only for the duration of a send. It is never
	// logged, never formatted into an error, and never returned by any API.
	Password string
	TLS      TLSMode
}

// ErrIncomplete reports a configuration that cannot produce a send.
var ErrIncomplete = errors.New("mailer: incomplete SMTP configuration")

// Validate checks that a config can actually be used to send.
func (c Config) Validate() error {
	if strings.TrimSpace(c.Recipient) == "" {
		return fmt.Errorf("%w: recipient is required", ErrIncomplete)
	}
	if !strings.Contains(c.Recipient, "@") {
		return fmt.Errorf("%w: recipient must be an email address", ErrIncomplete)
	}
	if strings.TrimSpace(c.Host) == "" {
		return fmt.Errorf("%w: SMTP host is required", ErrIncomplete)
	}
	if c.Port <= 0 || c.Port > 65535 {
		return fmt.Errorf("%w: SMTP port must be between 1 and 65535", ErrIncomplete)
	}
	if !c.TLS.Valid() {
		return fmt.Errorf("%w: unsupported TLS mode %q", ErrIncomplete, c.TLS)
	}
	// A username with no password is a misconfiguration that surfaces as an
	// opaque auth failure hours later; catch it while the user is looking.
	if strings.TrimSpace(c.Username) != "" && c.Password == "" {
		return fmt.Errorf("%w: SMTP password is required when a username is set", ErrIncomplete)
	}
	return nil
}

// Redacted returns a copy safe to print, with the password replaced.
func (c Config) Redacted() Config {
	if c.Password != "" {
		c.Password = "[redacted]"
	}
	return c
}

// String implements fmt.Stringer so an accidental %v or %s can never print the
// password.
func (c Config) String() string {
	return fmt.Sprintf("smtp %s:%d tls=%s user=%q recipient=%q password=%s",
		c.Host, c.Port, c.TLS, c.Username, c.Recipient, passwordMarker(c.Password))
}

// LogValue implements slog.LogValuer for the same reason String does.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("host", c.Host),
		slog.Int("port", c.Port),
		slog.String("tls", string(c.TLS)),
		slog.String("username", c.Username),
		slog.String("recipient", c.Recipient),
		slog.String("password", passwordMarker(c.Password)),
	)
}

func passwordMarker(password string) string {
	if password == "" {
		return "[unset]"
	}
	return "[redacted]"
}

// Message is one email to send.
type Message struct {
	Subject string
	Body    string
}

// Sender delivers a message to a configured SMTP server.
type Sender struct {
	// Dial is injectable so tests can drive a local listener; nil uses a
	// timeout-bounded TCP dial.
	Dial func(network, address string) (net.Conn, error)
	// Timeout bounds the whole exchange. A mail server that hangs must not pin
	// a goroutine forever.
	Timeout time.Duration
}

// DefaultTimeout bounds one send when the Sender does not set its own.
const DefaultTimeout = 30 * time.Second

// Send delivers one message. Errors describe what failed without ever
// including the password.
func (s *Sender) Send(cfg Config, msg Message) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	address := net.JoinHostPort(cfg.Host, fmt.Sprint(cfg.Port))
	conn, err := s.dial(address, cfg)
	if err != nil {
		return fmt.Errorf("mailer: connect %s: %w", address, err)
	}
	deadline := s.timeout()
	_ = conn.SetDeadline(time.Now().Add(deadline))

	client, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("mailer: smtp handshake %s: %w", address, err)
	}
	defer func() { _ = client.Close() }()

	if cfg.TLS == TLSStartTLS {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return fmt.Errorf("mailer: %s does not offer STARTTLS", address)
		}
		if err := client.StartTLS(&tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12}); err != nil {
			return fmt.Errorf("mailer: starttls %s: %w", address, err)
		}
	}
	if strings.TrimSpace(cfg.Username) != "" {
		// PlainAuth refuses to send credentials over an unencrypted link unless
		// the host is localhost, which is exactly the guard we want: a typo in
		// the TLS mode must not leak an app password onto the wire.
		if err := client.Auth(smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)); err != nil {
			return fmt.Errorf("mailer: authenticate as %q: %w", cfg.Username, err)
		}
	}
	from := cfg.Username
	if strings.TrimSpace(from) == "" || !strings.Contains(from, "@") {
		from = cfg.Recipient
	}
	if err := client.Mail(from); err != nil {
		return fmt.Errorf("mailer: sender rejected: %w", err)
	}
	if err := client.Rcpt(cfg.Recipient); err != nil {
		return fmt.Errorf("mailer: recipient rejected: %w", err)
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("mailer: data: %w", err)
	}
	if _, err := w.Write([]byte(render(from, cfg.Recipient, msg))); err != nil {
		_ = w.Close()
		return fmt.Errorf("mailer: write body: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("mailer: close body: %w", err)
	}
	if err := client.Quit(); err != nil {
		return fmt.Errorf("mailer: quit: %w", err)
	}
	return nil
}

func (s *Sender) timeout() time.Duration {
	if s != nil && s.Timeout > 0 {
		return s.Timeout
	}
	return DefaultTimeout
}

func (s *Sender) dial(address string, cfg Config) (net.Conn, error) {
	if s != nil && s.Dial != nil {
		return s.Dial("tcp", address)
	}
	dialer := &net.Dialer{Timeout: s.timeout()}
	if cfg.TLS == TLSImplicit {
		return tls.DialWithDialer(dialer, "tcp", address,
			&tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12})
	}
	return dialer.Dial("tcp", address)
}

// Render builds the RFC 5322 bytes for a message. Exported for tests that
// assert on the encoding without opening a socket.
func Render(from, to string, msg Message) string { return render(from, to, msg) }

func render(from, to string, msg Message) string {
	// Subjects carry session and workflow titles, which are user text and
	// routinely non-ASCII; encoded-word keeps them readable instead of mangled.
	subject := mime.QEncoding.Encode("utf-8", msg.Subject)
	var b strings.Builder
	b.WriteString("From: " + from + "\r\n")
	b.WriteString("To: " + to + "\r\n")
	b.WriteString("Subject: " + subject + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("\r\n")
	b.WriteString(strings.ReplaceAll(msg.Body, "\n", "\r\n"))
	b.WriteString("\r\n")
	return b.String()
}
