package mailer

import (
	"bufio"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

const testPassword = "hunter2-app-password"

func plainConfig() Config {
	return Config{
		Recipient: "someone@example.com",
		Host:      "127.0.0.1",
		Port:      2525,
		Username:  "sender@example.com",
		Password:  testPassword,
		TLS:       TLSNone,
	}
}

// The single most important property in this package: the password must not
// escape through any of the ways a value normally gets printed.
func TestConfigNeverPrintsThePassword(t *testing.T) {
	cfg := plainConfig()
	// Routed through an any so the %s case exercises the Stringer path rather
	// than being folded into a direct String() call by the compiler.
	var asAny any = cfg
	printed := []string{
		cfg.String(),
		fmt.Sprintf("%v", cfg),
		fmt.Sprintf("%s", asAny),
		fmt.Sprintf("%+v", cfg.Redacted()),
		cfg.LogValue().String(),
	}
	for i, out := range printed {
		if strings.Contains(out, testPassword) {
			t.Fatalf("rendering %d leaked the password: %s", i, out)
		}
		if !strings.Contains(out, "[redacted]") {
			t.Fatalf("rendering %d does not mark the password redacted: %s", i, out)
		}
	}
}

func TestConfigLogValueIsRedactedThroughSlog(t *testing.T) {
	var sb strings.Builder
	logger := slog.New(slog.NewTextHandler(&sb, nil))
	logger.Info("sending", "smtp", plainConfig())
	if strings.Contains(sb.String(), testPassword) {
		t.Fatalf("slog leaked the password: %s", sb.String())
	}
}

// An unset password reads as unset rather than as redacted, so a support log
// can still distinguish "no credential configured" from "one is configured".
func TestUnsetPasswordIsMarkedUnset(t *testing.T) {
	cfg := plainConfig()
	cfg.Password = ""
	if !strings.Contains(cfg.String(), "[unset]") {
		t.Fatalf("String() = %s, want it to mark the password unset", cfg.String())
	}
}

func TestValidate(t *testing.T) {
	for _, tt := range []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{name: "complete", mutate: func(*Config) {}},
		{name: "no recipient", mutate: func(c *Config) { c.Recipient = "" }, wantErr: true},
		{name: "recipient is not an address", mutate: func(c *Config) { c.Recipient = "someone" }, wantErr: true},
		{name: "no host", mutate: func(c *Config) { c.Host = "" }, wantErr: true},
		{name: "port zero", mutate: func(c *Config) { c.Port = 0 }, wantErr: true},
		{name: "port out of range", mutate: func(c *Config) { c.Port = 70000 }, wantErr: true},
		{name: "unknown tls mode", mutate: func(c *Config) { c.TLS = "sometimes" }, wantErr: true},
		// A username with no password fails as an opaque auth error hours
		// later; catching it here means the user sees it while looking at the
		// form.
		{name: "username without password", mutate: func(c *Config) { c.Password = "" }, wantErr: true},
		{name: "no auth at all", mutate: func(c *Config) { c.Username = ""; c.Password = "" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := plainConfig()
			tt.mutate(&cfg)
			err := cfg.Validate()
			if tt.wantErr != (err != nil) {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), testPassword) {
				t.Fatalf("validation error leaked the password: %v", err)
			}
		})
	}
}

// An empty stored value defaults to STARTTLS, never to the insecure mode: a
// missing setting must not silently downgrade the connection.
func TestParseTLSMode(t *testing.T) {
	for _, tt := range []struct {
		in      string
		want    TLSMode
		wantErr bool
	}{
		{in: "", want: TLSStartTLS},
		{in: "starttls", want: TLSStartTLS},
		{in: "  IMPLICIT ", want: TLSImplicit},
		{in: "none", want: TLSNone},
		{in: "ssl", wantErr: true},
	} {
		got, err := ParseTLSMode(tt.in)
		if tt.wantErr != (err != nil) {
			t.Fatalf("ParseTLSMode(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
		}
		if err == nil && got != tt.want {
			t.Fatalf("ParseTLSMode(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// Subjects carry session and workflow titles, which are user text and routinely
// non-ASCII. Encoded-word keeps them readable rather than mangled.
func TestRenderEncodesNonASCIISubjects(t *testing.T) {
	out := Render("a@example.com", "b@example.com", Message{Subject: "Tarea terminada ✅", Body: "line one\nline two"})
	if strings.Contains(out, "✅") {
		t.Fatalf("subject was not encoded: %s", out)
	}
	if !strings.Contains(out, "Subject: =?utf-8?") {
		t.Fatalf("subject is not an encoded word: %s", out)
	}
	// SMTP is CRLF; a bare LF in the body would corrupt the message.
	if strings.Contains(strings.ReplaceAll(out, "\r\n", ""), "\n") {
		t.Fatalf("body contains a bare LF: %q", out)
	}
	if !strings.Contains(out, "line one\r\nline two") {
		t.Fatalf("body lines were not preserved: %q", out)
	}
}

func TestSendDeliversToTheServer(t *testing.T) {
	srv := startFakeSMTP(t)
	cfg := plainConfig()
	sender := &Sender{Dial: srv.dial, Timeout: 5 * time.Second}

	if err := sender.Send(cfg, Message{Subject: "Task finished", Body: "all done"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	got := srv.transcript()
	for _, want := range []string{"MAIL FROM:<sender@example.com>", "RCPT TO:<someone@example.com>", "all done"} {
		if !strings.Contains(got, want) {
			t.Fatalf("transcript missing %q:\n%s", want, got)
		}
	}
}

func TestSendRefusesAnIncompleteConfig(t *testing.T) {
	cfg := plainConfig()
	cfg.Host = ""
	if err := (&Sender{}).Send(cfg, Message{Subject: "x"}); err == nil {
		t.Fatal("Send accepted a config with no host")
	}
}

// Connection failures have to come back as ordinary errors the caller can log,
// not panics, and without the password in the text.
func TestSendReportsAConnectionFailure(t *testing.T) {
	cfg := plainConfig()
	sender := &Sender{
		Dial:    func(string, string) (net.Conn, error) { return nil, fmt.Errorf("connection refused") },
		Timeout: time.Second,
	}
	err := sender.Send(cfg, Message{Subject: "x"})
	if err == nil {
		t.Fatal("Send succeeded against a refused connection")
	}
	if strings.Contains(err.Error(), testPassword) {
		t.Fatalf("connection error leaked the password: %v", err)
	}
}

// A typo in the TLS mode must not put an app password on the wire in the
// clear. net/smtp's PlainAuth enforces this for us against any host but
// localhost, and the test pins that we rely on it rather than bypassing it.
func TestSendRefusesCleartextAuthToARemoteHost(t *testing.T) {
	srv := startFakeSMTP(t)
	cfg := plainConfig()
	cfg.Host = "smtp.example.com"
	sender := &Sender{Dial: srv.dial, Timeout: 5 * time.Second}

	err := sender.Send(cfg, Message{Subject: "x", Body: "y"})
	if err == nil {
		t.Fatal("Send authenticated over an unencrypted connection to a remote host")
	}
	if strings.Contains(err.Error(), testPassword) {
		t.Fatalf("auth error leaked the password: %v", err)
	}
	if strings.Contains(srv.transcript(), testPassword) {
		t.Fatalf("the password reached the wire:\n%s", srv.transcript())
	}
}

// fakeSMTP is a minimal in-process SMTP server: enough of the protocol to let
// net/smtp complete a session, and nothing more.
type fakeSMTP struct {
	listener net.Listener

	mu    sync.Mutex
	lines []string
}

func startFakeSMTP(t *testing.T) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &fakeSMTP{listener: ln}
	t.Cleanup(func() { _ = ln.Close() })
	go srv.serve()
	return srv
}

func (s *fakeSMTP) dial(string, string) (net.Conn, error) {
	return net.Dial("tcp", s.listener.Addr().String())
}

func (s *fakeSMTP) transcript() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.lines, "\n")
}

func (s *fakeSMTP) record(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lines = append(s.lines, line)
}

func (s *fakeSMTP) serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *fakeSMTP) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	reader := bufio.NewReader(conn)
	write := func(format string, args ...any) {
		_, _ = fmt.Fprintf(conn, format+"\r\n", args...)
	}
	write("220 fake ESMTP")
	inData := false
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		s.record(line)
		if inData {
			if line == "." {
				inData = false
				write("250 2.0.0 Ok")
			}
			continue
		}
		upper := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(upper, "EHLO"):
			// AUTH PLAIN is advertised so net/smtp attempts it; every other
			// extension is deliberately absent to keep the exchange minimal.
			write("250-fake")
			write("250 AUTH PLAIN")
		case strings.HasPrefix(upper, "HELO"):
			write("250 fake")
		case strings.HasPrefix(upper, "AUTH"):
			write("235 2.7.0 Authentication successful")
		case strings.HasPrefix(upper, "MAIL FROM"), strings.HasPrefix(upper, "RCPT TO"):
			write("250 2.1.0 Ok")
		case strings.HasPrefix(upper, "DATA"):
			inData = true
			write("354 End data with <CR><LF>.<CR><LF>")
		case strings.HasPrefix(upper, "QUIT"):
			write("221 2.0.0 Bye")
			return
		default:
			write("250 2.0.0 Ok")
		}
	}
}
