package controllers_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	"github.com/aoagents/agent-orchestrator/backend/internal/mailer"
	settingssvc "github.com/aoagents/agent-orchestrator/backend/internal/service/settings"
)

// The SMTP password the test asserts never appears on the wire.
const wirePassword = "gmail-app-password"

type fakeSettingsService struct {
	email     settingssvc.EmailSettings
	gotUpdate settingssvc.EmailUpdate
	testErr   error
	testCall  int
}

func (f *fakeSettingsService) Get(context.Context) (settingssvc.Snapshot, error) {
	return settingssvc.Snapshot{DefaultSessionMode: domain.SessionModeTUI}, nil
}

func (f *fakeSettingsService) SetDefaultSessionMode(context.Context, domain.SessionMode) (settingssvc.Snapshot, error) {
	return settingssvc.Snapshot{DefaultSessionMode: domain.SessionModeTUI}, nil
}

func (f *fakeSettingsService) ChatHarnesses([]domain.AgentHarness) []domain.AgentHarness { return nil }

func (f *fakeSettingsService) EmailNotifications(context.Context) (settingssvc.EmailSettings, error) {
	return f.email, nil
}

func (f *fakeSettingsService) SetEmailNotifications(_ context.Context, update settingssvc.EmailUpdate) (settingssvc.EmailSettings, error) {
	f.gotUpdate = update
	f.email.Enabled = update.Enabled
	f.email.Recipient = update.Recipient
	f.email.Host = update.Host
	f.email.Username = update.Username
	if update.Password != nil {
		f.email.PasswordSet = *update.Password != ""
	}
	return f.email, nil
}

func (f *fakeSettingsService) SendTestEmail(context.Context) error {
	f.testCall++
	return f.testErr
}

func newSettingsTestServer(t *testing.T, svc controllers.SettingsService) *httptest.Server {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(httpd.NewRouterWithControl(
		config.Config{}, log, nil, httpd.APIDeps{Settings: svc}, httpd.ControlDeps{},
	))
	t.Cleanup(srv.Close)
	return srv
}

func configuredEmail() settingssvc.EmailSettings {
	return settingssvc.EmailSettings{
		Enabled: true, Recipient: "someone@example.com", Host: "smtp.gmail.com",
		Port: 587, Username: "someone@gmail.com", TLS: mailer.TLSStartTLS, PasswordSet: true,
	}
}

// The load-bearing property of this whole surface: the stored password is
// write-only. It cannot leave through a response body, so it cannot end up in a
// proxy log, a support bundle, or a screenshot of the network tab.
func TestEmailNotificationSettingsAPI_NeverReturnsThePassword(t *testing.T) {
	svc := &fakeSettingsService{email: configuredEmail()}
	srv := newSettingsTestServer(t, svc)

	raw, status, _ := doRequest(t, srv, "GET", "/api/v1/settings/email-notifications", "")
	body := string(raw)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, body)
	}
	if strings.Contains(body, wirePassword) || strings.Contains(strings.ToLower(body), `"password"`) {
		t.Fatalf("the response carries a password field: %s", body)
	}

	var resp struct {
		EmailNotifications struct {
			Enabled     bool   `json:"enabled"`
			Recipient   string `json:"recipient"`
			Host        string `json:"host"`
			Port        int    `json:"port"`
			Username    string `json:"username"`
			TLS         string `json:"tls"`
			PasswordSet bool   `json:"passwordSet"`
		} `json:"emailNotifications"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode: %v; body=%s", err, body)
	}
	got := resp.EmailNotifications
	if !got.Enabled || got.Recipient != "someone@example.com" || got.Host != "smtp.gmail.com" ||
		got.Port != 587 || got.Username != "someone@gmail.com" || got.TLS != "starttls" {
		t.Fatalf("response = %+v", got)
	}
	if !got.PasswordSet {
		t.Fatal("passwordSet = false, so the UI cannot tell a credential is stored")
	}
}

func TestEmailNotificationSettingsAPI_UpdateAcceptsANewPassword(t *testing.T) {
	svc := &fakeSettingsService{}
	srv := newSettingsTestServer(t, svc)

	raw, status, _ := doRequest(t, srv, "PATCH", "/api/v1/settings/email-notifications", `{
		"enabled": true, "recipient": "someone@example.com", "host": "smtp.gmail.com",
		"port": 587, "username": "someone@gmail.com", "tls": "starttls", "password": "`+wirePassword+`"
	}`)
	body := string(raw)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, body)
	}
	if svc.gotUpdate.Password == nil || *svc.gotUpdate.Password != wirePassword {
		t.Fatalf("password did not reach the service: %+v", svc.gotUpdate.Password)
	}
	// And it still does not come back out.
	if strings.Contains(body, wirePassword) {
		t.Fatalf("the update response echoed the password: %s", body)
	}
}

// An omitted password means "leave the stored one alone". Collapsing it to ""
// here would erase the credential on every save from an untouched form.
func TestEmailNotificationSettingsAPI_OmittedPasswordIsDistinctFromEmpty(t *testing.T) {
	svc := &fakeSettingsService{email: configuredEmail()}
	srv := newSettingsTestServer(t, svc)

	if _, status, _ := doRequest(t, srv, "PATCH", "/api/v1/settings/email-notifications",
		`{"enabled": true, "recipient": "someone@example.com", "host": "smtp.gmail.com", "port": 587}`); status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if svc.gotUpdate.Password != nil {
		t.Fatalf("an omitted password reached the service as %q", *svc.gotUpdate.Password)
	}

	if _, status, _ := doRequest(t, srv, "PATCH", "/api/v1/settings/email-notifications",
		`{"enabled": false, "recipient": "someone@example.com", "host": "smtp.gmail.com", "port": 587, "password": ""}`); status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if svc.gotUpdate.Password == nil || *svc.gotUpdate.Password != "" {
		t.Fatalf("an explicit clear did not reach the service: %+v", svc.gotUpdate.Password)
	}
}

func TestEmailNotificationSettingsAPI_SendTest(t *testing.T) {
	svc := &fakeSettingsService{email: configuredEmail()}
	srv := newSettingsTestServer(t, svc)

	raw, status, _ := doRequest(t, srv, "POST", "/api/v1/settings/email-notifications/test", "")
	body := string(raw)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, body)
	}
	if svc.testCall != 1 {
		t.Fatalf("SendTestEmail called %d times, want 1", svc.testCall)
	}
	if !strings.Contains(body, `"sent":true`) || !strings.Contains(body, "someone@example.com") {
		t.Fatalf("response = %s", body)
	}
}

// The whole point of a test button is that the real reason reaches the user.
func TestEmailNotificationSettingsAPI_SendTestSurfacesTheFailure(t *testing.T) {
	svc := &fakeSettingsService{
		email:   configuredEmail(),
		testErr: apierr.Invalid("EMAIL_TEST_FAILED", "535 5.7.8 Username and Password not accepted", nil),
	}
	srv := newSettingsTestServer(t, svc)

	raw, status, _ := doRequest(t, srv, "POST", "/api/v1/settings/email-notifications/test", "")
	body := string(raw)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", status, body)
	}
	if !strings.Contains(body, "535") {
		t.Fatalf("response hides the server's own reason: %s", body)
	}
}
