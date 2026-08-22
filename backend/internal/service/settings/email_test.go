package settings

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/mailer"
)

const plaintextPassword = "gmail-app-password"

type fakeStore struct {
	snapshot Snapshot
	saved    []EmailConfig
	getErr   error
}

func (f *fakeStore) GetAppSettings(context.Context) (Snapshot, error) {
	if f.getErr != nil {
		return Snapshot{}, f.getErr
	}
	return f.snapshot, nil
}

func (f *fakeStore) SetDefaultSessionMode(context.Context, domain.SessionMode, time.Time) error {
	return nil
}

func (f *fakeStore) SetEmailNotifications(_ context.Context, cfg EmailConfig, _ time.Time) error {
	f.saved = append(f.saved, cfg)
	f.snapshot.Email = cfg
	return nil
}

// reversibleBox is a deliberately trivial stand-in for the real AES box: these
// tests are about which values move where, not about the cryptography, which
// internal/secretbox covers on its own.
type reversibleBox struct{ sealErr error }

func (b reversibleBox) Seal(plaintext string) (string, error) {
	if b.sealErr != nil {
		return "", b.sealErr
	}
	if plaintext == "" {
		return "", nil
	}
	return "sealed:" + plaintext, nil
}

func (b reversibleBox) Open(ciphertext string) (string, error) {
	if ciphertext == "" {
		return "", nil
	}
	rest, ok := strings.CutPrefix(ciphertext, "sealed:")
	if !ok {
		return "", errors.New("not sealed by this box")
	}
	return rest, nil
}

type fakeSender struct {
	sent []mailer.Config
	err  error
}

func (f *fakeSender) Send(cfg mailer.Config, _ mailer.Message) error {
	f.sent = append(f.sent, cfg)
	return f.err
}

func configured() EmailConfig {
	return EmailConfig{
		Enabled:            true,
		Recipient:          "someone@example.com",
		Host:               "smtp.gmail.com",
		Port:               587,
		Username:           "someone@gmail.com",
		PasswordCiphertext: "sealed:" + plaintextPassword,
		TLS:                "starttls",
		// All three on, matching the schema default (migration 0125): an install
		// that already had email enabled keeps receiving every event.
		Events: EmailEvents{Completed: true, NeedsAttention: true, Failed: true},
	}
}

func newService(store *fakeStore, sender Sender) *Service {
	return New(store, nil, func() time.Time { return time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC) }).
		WithEmail(reversibleBox{}, sender)
}

// The password is write-only across this boundary. Nothing a caller can read
// back carries it, which is what keeps it out of API responses and logs by
// construction rather than by remembering to strip it at each hop.
func TestEmailNotificationsReportsThePasswordWithoutReturningIt(t *testing.T) {
	store := &fakeStore{snapshot: Snapshot{Email: configured()}}
	got, err := newService(store, nil).EmailNotifications(context.Background())
	if err != nil {
		t.Fatalf("EmailNotifications: %v", err)
	}
	if !got.PasswordSet {
		t.Fatal("PasswordSet = false with a stored password")
	}
	if rendered := strings.Join([]string{got.Recipient, got.Host, got.Username, string(got.TLS)}, "|"); strings.Contains(rendered, plaintextPassword) {
		t.Fatalf("a returned field carried the password: %s", rendered)
	}
}

func TestEmailNotificationsReportsNoPasswordWhenUnset(t *testing.T) {
	cfg := configured()
	cfg.PasswordCiphertext = ""
	store := &fakeStore{snapshot: Snapshot{Email: cfg}}
	got, err := newService(store, nil).EmailNotifications(context.Background())
	if err != nil {
		t.Fatalf("EmailNotifications: %v", err)
	}
	if got.PasswordSet {
		t.Fatal("PasswordSet = true with no stored password")
	}
}

func TestSetEmailNotificationsSealsANewPassword(t *testing.T) {
	store := &fakeStore{}
	password := plaintextPassword
	if _, err := newService(store, nil).SetEmailNotifications(context.Background(), EmailUpdate{
		Enabled: true, Recipient: "someone@example.com", Host: "smtp.gmail.com",
		Port: 587, Username: "someone@gmail.com", TLS: "starttls", Password: &password,
	}); err != nil {
		t.Fatalf("SetEmailNotifications: %v", err)
	}
	if len(store.saved) != 1 {
		t.Fatalf("saved %d configs, want 1", len(store.saved))
	}
	saved := store.saved[0]
	if saved.PasswordCiphertext == plaintextPassword {
		t.Fatal("the password was stored in plaintext")
	}
	if saved.PasswordCiphertext != "sealed:"+plaintextPassword {
		t.Fatalf("PasswordCiphertext = %q, want it sealed", saved.PasswordCiphertext)
	}
}

// The UI never receives the current password, so it cannot echo one back.
// Without this, every save from a form the user did not retype would silently
// erase the credential.
func TestSetEmailNotificationsKeepsTheStoredPasswordWhenNoneIsSent(t *testing.T) {
	store := &fakeStore{snapshot: Snapshot{Email: configured()}}
	if _, err := newService(store, nil).SetEmailNotifications(context.Background(), EmailUpdate{
		Enabled: true, Recipient: "other@example.com", Host: "smtp.gmail.com",
		Port: 587, Username: "someone@gmail.com", TLS: "starttls",
	}); err != nil {
		t.Fatalf("SetEmailNotifications: %v", err)
	}
	saved := store.saved[0]
	if saved.PasswordCiphertext != configured().PasswordCiphertext {
		t.Fatalf("PasswordCiphertext = %q, want the stored one preserved", saved.PasswordCiphertext)
	}
	if saved.Recipient != "other@example.com" {
		t.Fatalf("Recipient = %q, want the update applied", saved.Recipient)
	}
}

// An explicit empty string is the deliberate clear, and has to be
// distinguishable from "not sent".
func TestSetEmailNotificationsClearsThePasswordOnAnExplicitEmptyString(t *testing.T) {
	store := &fakeStore{snapshot: Snapshot{Email: configured()}}
	empty := ""
	if _, err := newService(store, nil).SetEmailNotifications(context.Background(), EmailUpdate{
		Enabled: false, Recipient: "someone@example.com", Host: "smtp.gmail.com",
		Port: 587, Username: "someone@gmail.com", TLS: "starttls", Password: &empty,
	}); err != nil {
		t.Fatalf("SetEmailNotifications: %v", err)
	}
	if got := store.saved[0].PasswordCiphertext; got != "" {
		t.Fatalf("PasswordCiphertext = %q, want cleared", got)
	}
}

// Only a configuration being switched ON has to be complete: a user half-way
// through the form, or turning the feature off, must still be able to save.
func TestSetEmailNotificationsOnlyValidatesWhenEnabling(t *testing.T) {
	store := &fakeStore{}
	if _, err := newService(store, nil).SetEmailNotifications(context.Background(), EmailUpdate{
		Enabled: false, Recipient: "", Host: "",
	}); err != nil {
		t.Fatalf("saving an incomplete, disabled config failed: %v", err)
	}

	_, err := newService(&fakeStore{}, nil).SetEmailNotifications(context.Background(), EmailUpdate{
		Enabled: true, Recipient: "", Host: "smtp.gmail.com", Port: 587,
	})
	if err == nil {
		t.Fatal("enabling with no recipient was accepted")
	}
}

// An omitted port defaults to 587: the submission port, and what Gmail app
// passwords expect.
func TestSetEmailNotificationsDefaultsThePort(t *testing.T) {
	store := &fakeStore{}
	if _, err := newService(store, nil).SetEmailNotifications(context.Background(), EmailUpdate{
		Enabled: false, Recipient: "someone@example.com", Host: "smtp.gmail.com",
	}); err != nil {
		t.Fatalf("SetEmailNotifications: %v", err)
	}
	if got := store.saved[0].Port; got != DefaultSMTPPort {
		t.Fatalf("Port = %d, want %d", got, DefaultSMTPPort)
	}
}

func TestSetEmailNotificationsRejectsAnUnknownTLSMode(t *testing.T) {
	if _, err := newService(&fakeStore{}, nil).SetEmailNotifications(context.Background(), EmailUpdate{
		Enabled: false, TLS: "ssl-maybe",
	}); err == nil {
		t.Fatal("an unknown TLS mode was accepted")
	}
}

// The test button exists to prove the credentials work before the feature is
// switched on, so it must not require it to be on already.
func TestSendTestEmailDoesNotRequireTheFeatureToBeEnabled(t *testing.T) {
	cfg := configured()
	cfg.Enabled = false
	sender := &fakeSender{}
	if err := newService(&fakeStore{snapshot: Snapshot{Email: cfg}}, sender).SendTestEmail(context.Background()); err != nil {
		t.Fatalf("SendTestEmail: %v", err)
	}
	if len(sender.sent) != 1 {
		t.Fatalf("sent %d messages, want 1", len(sender.sent))
	}
	// The plaintext is decrypted at the last possible moment, and only here.
	if sender.sent[0].Password != plaintextPassword {
		t.Fatalf("Password = %q, want the decrypted password", sender.sent[0].Password)
	}
}

func TestSendTestEmailReportsWhyItFailed(t *testing.T) {
	sender := &fakeSender{err: errors.New("535 5.7.8 Username and Password not accepted")}
	err := newService(&fakeStore{snapshot: Snapshot{Email: configured()}}, sender).SendTestEmail(context.Background())
	if err == nil {
		t.Fatal("SendTestEmail hid a delivery failure")
	}
	if !strings.Contains(err.Error(), "535") {
		t.Fatalf("error = %v, want the server's own reason", err)
	}
}

func TestSendTestEmailRefusesAnIncompleteConfig(t *testing.T) {
	cfg := configured()
	cfg.Host = ""
	sender := &fakeSender{}
	if err := newService(&fakeStore{snapshot: Snapshot{Email: cfg}}, sender).SendTestEmail(context.Background()); err == nil {
		t.Fatal("SendTestEmail accepted a config with no host")
	}
	if len(sender.sent) != 0 {
		t.Fatal("SendTestEmail dialed with an incomplete config")
	}
}

// A password sealed with a key that is now gone (a restored database, a wiped
// data dir) has to surface as "re-enter it", not as a silent failure to send.
func TestSendTestEmailReportsAnUnreadablePassword(t *testing.T) {
	cfg := configured()
	cfg.PasswordCiphertext = "not-sealed-by-this-box"
	err := newService(&fakeStore{snapshot: Snapshot{Email: cfg}}, &fakeSender{}).SendTestEmail(context.Background())
	if err == nil {
		t.Fatal("an unreadable password was treated as usable")
	}
}

// The completion-email fan-out must be silent while the feature is off — most
// installs never turn it on, and a warning per finished task would be noise.
func TestEmailNotificationSkipsSendWhenDisabled(t *testing.T) {
	cfg := configured()
	cfg.Enabled = false
	sender := &fakeSender{}
	emailer := NewNotificationEmailer(newService(&fakeStore{snapshot: Snapshot{Email: cfg}}, sender))

	if err := emailer.EmailNotification(context.Background(), domain.NotificationRecord{
		ID: "ntf-1", SessionID: "mer-1", ProjectID: "mer",
		Type: domain.NotificationTaskCompleted, Title: "checkout-flow finished",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("EmailNotification: %v", err)
	}
	if len(sender.sent) != 0 {
		t.Fatalf("sent %d messages while disabled", len(sender.sent))
	}
}

func TestEmailNotificationSendsWhenEnabled(t *testing.T) {
	sender := &fakeSender{}
	emailer := NewNotificationEmailer(newService(&fakeStore{snapshot: Snapshot{Email: configured()}}, sender))

	if err := emailer.EmailNotification(context.Background(), domain.NotificationRecord{
		ID: "ntf-1", WorkflowRunID: "wf-1", ProjectID: "mer",
		Type: domain.NotificationWorkflowCompleted, Title: "ship the thing finished",
		Body: "Every task in this workflow run completed.", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("EmailNotification: %v", err)
	}
	if len(sender.sent) != 1 {
		t.Fatalf("sent %d messages, want 1", len(sender.sent))
	}
	if got := sender.sent[0].Recipient; got != "someone@example.com" {
		t.Fatalf("Recipient = %q", got)
	}
}

func TestEmailMessageNamesTheFinishedWork(t *testing.T) {
	msg := message(domain.NotificationRecord{
		ID: "ntf-1", WorkflowRunID: "wf-1", ProjectID: "mer",
		Type: domain.NotificationWorkflowCompleted, Title: "ship the thing finished",
		CreatedAt: time.Date(2026, 8, 22, 9, 0, 0, 0, time.UTC),
	})
	if !strings.HasPrefix(msg.Subject, "[AO] Workflow finished:") {
		t.Fatalf("Subject = %q", msg.Subject)
	}
	for _, want := range []string{"ship the thing finished", "wf-1", "mer"} {
		if !strings.Contains(msg.Body, want) {
			t.Fatalf("body missing %q:\n%s", want, msg.Body)
		}
	}
}

// The attention and failure families reach the same mailbox as a completion,
// under a subject that says which one it is.
func TestEmailNotificationSendsAttentionAndFailureEvents(t *testing.T) {
	for _, tc := range []struct {
		name        string
		typ         domain.NotificationType
		wantSubject string
	}{
		{"task needs attention", domain.NotificationTaskNeedsAttention, "[AO] Task needs attention:"},
		{"workflow needs attention", domain.NotificationWorkflowNeedsAttention, "[AO] Workflow needs attention:"},
		{"task failed", domain.NotificationTaskFailed, "[AO] Task failed:"},
		{"workflow failed", domain.NotificationWorkflowFailed, "[AO] Workflow failed:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := domain.NotificationRecord{
				ID: "ntf-1", WorkflowRunID: "wf-1", ProjectID: "mer",
				Type: tc.typ, Title: "build the collector needs your attention",
				Body: "Reason: fix_budget_exhausted", CreatedAt: time.Now().UTC(),
			}
			sender := &fakeSender{}
			emailer := NewNotificationEmailer(newService(&fakeStore{snapshot: Snapshot{Email: configured()}}, sender))
			if err := emailer.EmailNotification(context.Background(), rec); err != nil {
				t.Fatalf("EmailNotification: %v", err)
			}
			if len(sender.sent) != 1 {
				t.Fatalf("sent %d messages, want 1", len(sender.sent))
			}
			msg := message(rec)
			if !strings.HasPrefix(msg.Subject, tc.wantSubject) {
				t.Fatalf("Subject = %q, want prefix %q", msg.Subject, tc.wantSubject)
			}
			if !strings.Contains(msg.Body, "Reason: fix_budget_exhausted") {
				t.Fatalf("body does not carry why AO stopped:\n%s", msg.Body)
			}
		})
	}
}

// Turning one event off must silence that event and nothing else — the whole
// point of the selection is keeping the mail you want.
func TestEmailNotificationHonorsPerEventSelection(t *testing.T) {
	cfg := configured()
	cfg.Events = EmailEvents{Completed: true, NeedsAttention: false, Failed: true}
	sender := &fakeSender{}
	emailer := NewNotificationEmailer(newService(&fakeStore{snapshot: Snapshot{Email: cfg}}, sender))
	ctx := context.Background()
	now := time.Now().UTC()

	if err := emailer.EmailNotification(ctx, domain.NotificationRecord{
		ID: "ntf-1", WorkflowRunID: "wf-1", ProjectID: "mer",
		Type: domain.NotificationWorkflowNeedsAttention, Title: "stopped", CreatedAt: now,
	}); err != nil {
		t.Fatalf("EmailNotification(attention): %v", err)
	}
	if len(sender.sent) != 0 {
		t.Fatalf("sent %d messages for a deselected event", len(sender.sent))
	}

	if err := emailer.EmailNotification(ctx, domain.NotificationRecord{
		ID: "ntf-2", WorkflowRunID: "wf-1", ProjectID: "mer",
		Type: domain.NotificationWorkflowCompleted, Title: "finished", CreatedAt: now,
	}); err != nil {
		t.Fatalf("EmailNotification(completion): %v", err)
	}
	if len(sender.sent) != 1 {
		t.Fatalf("sent %d messages for a selected event, want 1", len(sender.sent))
	}
}

// A PR-family notification has no email event behind it and must never be
// mailed, whatever the selection says.
func TestEmailNotificationIgnoresNonEmailableTypes(t *testing.T) {
	sender := &fakeSender{}
	emailer := NewNotificationEmailer(newService(&fakeStore{snapshot: Snapshot{Email: configured()}}, sender))
	if err := emailer.EmailNotification(context.Background(), domain.NotificationRecord{
		ID: "ntf-1", SessionID: "s-1", ProjectID: "mer", PRURL: "https://example.test/pr/1",
		Type: domain.NotificationReadyToMerge, Title: "PR #1 is ready to merge", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("EmailNotification: %v", err)
	}
	if len(sender.sent) != 0 {
		t.Fatalf("sent %d messages for a non-emailable type", len(sender.sent))
	}
}

// A saved form that predates the event selection must not silently clear it.
func TestSetEmailNotificationsKeepsTheStoredEventSelectionWhenOmitted(t *testing.T) {
	store := &fakeStore{snapshot: Snapshot{Email: configured()}}
	svc := newService(store, &fakeSender{})
	if _, err := svc.SetEmailNotifications(context.Background(), EmailUpdate{
		Enabled: true, Recipient: "someone@example.com", Host: "smtp.gmail.com",
		Port: 587, Username: "someone@gmail.com", TLS: "starttls",
	}); err != nil {
		t.Fatalf("SetEmailNotifications: %v", err)
	}
	if len(store.saved) != 1 {
		t.Fatalf("saved %d configs, want 1", len(store.saved))
	}
	if got := store.saved[0].Events; !got.Completed || !got.NeedsAttention || !got.Failed {
		t.Fatalf("Events = %+v; an update that named no events cleared the stored selection", got)
	}
}

func TestSetEmailNotificationsPersistsAnExplicitEventSelection(t *testing.T) {
	store := &fakeStore{snapshot: Snapshot{Email: configured()}}
	svc := newService(store, &fakeSender{})
	events := EmailEvents{Completed: false, NeedsAttention: true, Failed: true}
	got, err := svc.SetEmailNotifications(context.Background(), EmailUpdate{
		Enabled: true, Recipient: "someone@example.com", Host: "smtp.gmail.com",
		Port: 587, Username: "someone@gmail.com", TLS: "starttls", Events: &events,
	})
	if err != nil {
		t.Fatalf("SetEmailNotifications: %v", err)
	}
	if got.Events != events {
		t.Fatalf("returned Events = %+v, want %+v", got.Events, events)
	}
	if store.saved[0].Events != events {
		t.Fatalf("stored Events = %+v, want %+v", store.saved[0].Events, events)
	}
}
