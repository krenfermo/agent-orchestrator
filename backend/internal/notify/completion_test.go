package notify

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func taskIntent() Intent {
	return Intent{
		Type:               domain.NotificationTaskCompleted,
		SessionID:          "mer-1",
		ProjectID:          "mer",
		SessionDisplayName: "checkout-flow",
		DedupeKey:          "mer-1@2026-08-22T09:00:00Z",
		CreatedAt:          time.Date(2026, 8, 22, 9, 0, 0, 0, time.UTC),
	}
}

func workflowIntent() Intent {
	return Intent{
		Type:              domain.NotificationWorkflowCompleted,
		ProjectID:         "mer",
		WorkflowRunID:     "wf-1",
		WorkflowObjective: "ship the thing",
		DedupeKey:         "wf-1",
		CreatedAt:         time.Date(2026, 8, 22, 9, 0, 0, 0, time.UTC),
	}
}

func TestEnrichTaskCompleted(t *testing.T) {
	rec, err := enrich(taskIntent())
	if err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if rec.Title != "checkout-flow finished" {
		t.Fatalf("Title = %q", rec.Title)
	}
	if rec.Body == "" {
		t.Fatal("a completion notification with no body says nothing")
	}
	if rec.DedupeKey != taskIntent().DedupeKey {
		t.Fatalf("DedupeKey = %q, want it carried through", rec.DedupeKey)
	}
	if rec.PRURL != "" {
		t.Fatalf("PRURL = %q, want none", rec.PRURL)
	}
}

// A run has no session, so the row is anchored to the run itself. The old
// enrich would have rejected this for lack of a PR URL.
func TestEnrichWorkflowCompleted(t *testing.T) {
	rec, err := enrich(workflowIntent())
	if err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if rec.Title != "ship the thing finished" {
		t.Fatalf("Title = %q, want the objective a human wrote", rec.Title)
	}
	if rec.WorkflowRunID != "wf-1" || rec.SessionID != "" {
		t.Fatalf("anchor = session %q / run %q, want run-only", rec.SessionID, rec.WorkflowRunID)
	}
}

// A run with no objective falls back to its id rather than to a generic word,
// so two finished runs are still distinguishable in the panel.
func TestEnrichWorkflowCompletedFallsBackToTheRunID(t *testing.T) {
	intent := workflowIntent()
	intent.WorkflowObjective = ""
	rec, err := enrich(intent)
	if err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if rec.Title != "Workflow wf-1 finished" {
		t.Fatalf("Title = %q", rec.Title)
	}
}

// Producing a completion with no dedupe key would quietly fall back to open-row
// dedupe, which lets a restart re-announce work the user already saw finish.
func TestEnrichRejectsACompletionWithNoDedupeKey(t *testing.T) {
	for _, intent := range []Intent{taskIntent(), workflowIntent()} {
		intent.DedupeKey = ""
		if _, err := enrich(intent); !errors.Is(err, domain.ErrInvalidNotificationRecord) {
			t.Fatalf("enrich(%s) error = %v, want ErrInvalidNotificationRecord", intent.Type, err)
		}
	}
}

// The PR-outcome family is meaningless without the PR it is about; relaxing the
// rule for completions must not have relaxed it for them.
func TestEnrichStillRequiresAPRForPROutcomes(t *testing.T) {
	if _, err := enrich(Intent{
		Type: domain.NotificationPRMerged, SessionID: "mer-1", ProjectID: "mer",
		CreatedAt: time.Now().UTC(),
	}); !errors.Is(err, domain.ErrInvalidNotificationRecord) {
		t.Fatalf("error = %v, want ErrInvalidNotificationRecord", err)
	}
}

type recordingEmailer struct {
	mu   sync.Mutex
	seen []domain.NotificationRecord
	err  error
	done chan struct{}
}

func newRecordingEmailer() *recordingEmailer {
	return &recordingEmailer{done: make(chan struct{}, 8)}
}

func (e *recordingEmailer) EmailNotification(_ context.Context, rec domain.NotificationRecord) error {
	e.mu.Lock()
	e.seen = append(e.seen, rec)
	e.mu.Unlock()
	e.done <- struct{}{}
	return e.err
}

func (e *recordingEmailer) records() []domain.NotificationRecord {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]domain.NotificationRecord(nil), e.seen...)
}

// The fan-out runs on its own goroutine, so a test has to wait for it rather
// than assume it already ran.
func (e *recordingEmailer) await(t *testing.T) {
	t.Helper()
	select {
	case <-e.done:
	case <-time.After(2 * time.Second):
		t.Fatal("email fan-out never ran")
	}
}

type emailFakeStore struct {
	created  []domain.NotificationRecord
	inserted bool
	err      error
}

func (f *emailFakeStore) CreateNotification(_ context.Context, rec domain.NotificationRecord) (domain.NotificationRecord, bool, error) {
	if f.err != nil {
		return domain.NotificationRecord{}, false, f.err
	}
	f.created = append(f.created, rec)
	return rec, f.inserted, nil
}

func (f *emailFakeStore) ResolveSessionNotifications(context.Context, domain.SessionID, domain.NotificationType, time.Time) ([]domain.NotificationRecord, error) {
	return nil, nil
}

func (f *emailFakeStore) ResolvePRNotifications(context.Context, string, domain.NotificationType, time.Time) ([]domain.NotificationRecord, error) {
	return nil, nil
}

func (f *emailFakeStore) ReconcileResolvedNotifications(context.Context, time.Time) ([]domain.NotificationRecord, error) {
	return nil, nil
}

func TestNotifyEmailsANewCompletion(t *testing.T) {
	emailer := newRecordingEmailer()
	store := &emailFakeStore{inserted: true}
	m := New(Deps{Store: store, Emailer: emailer})

	if err := m.Notify(context.Background(), taskIntent()); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	emailer.await(t)
	if got := emailer.records(); len(got) != 1 || got[0].Type != domain.NotificationTaskCompleted {
		t.Fatalf("emailed = %+v", got)
	}
}

// The store's dedupe is the email's dedupe: a row that was not inserted is a
// completion the user has already been told about.
func TestNotifyDoesNotEmailADuplicate(t *testing.T) {
	emailer := newRecordingEmailer()
	m := New(Deps{Store: &emailFakeStore{inserted: false}, Emailer: emailer})

	if err := m.Notify(context.Background(), taskIntent()); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	select {
	case <-emailer.done:
		t.Fatal("a deduplicated notification was emailed")
	case <-time.After(100 * time.Millisecond):
	}
}

// Email is a courtesy, not a delivery channel for everything: a needs-input
// ping is about something happening right now on screen.
func TestNotifyDoesNotEmailNonCompletions(t *testing.T) {
	emailer := newRecordingEmailer()
	m := New(Deps{Store: &emailFakeStore{inserted: true}, Emailer: emailer})

	if err := m.Notify(context.Background(), Intent{
		Type: domain.NotificationNeedsInput, SessionID: "mer-1", ProjectID: "mer",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	select {
	case <-emailer.done:
		t.Fatal("a needs-input notification was emailed")
	case <-time.After(100 * time.Millisecond):
	}
}

// A mail server that is down, an expired app password, an offline machine:
// none of them may turn a task that genuinely finished into a failure.
func TestNotifySucceedsWhenTheEmailFails(t *testing.T) {
	emailer := newRecordingEmailer()
	emailer.err = errors.New("smtp: connection refused")
	m := New(Deps{Store: &emailFakeStore{inserted: true}, Emailer: emailer})

	if err := m.Notify(context.Background(), taskIntent()); err != nil {
		t.Fatalf("Notify returned an error for a failed email: %v", err)
	}
	emailer.await(t)
}

// The caller's context is routinely cancelled the instant the work it
// describes finishes; the send must not be cancelled with it.
func TestNotifyEmailsEvenWhenTheCallerContextIsCancelled(t *testing.T) {
	emailer := newRecordingEmailer()
	m := New(Deps{Store: &emailFakeStore{inserted: true}, Emailer: emailer})

	ctx, cancel := context.WithCancel(context.Background())
	if err := m.Notify(ctx, taskIntent()); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	cancel()
	emailer.await(t)
}
