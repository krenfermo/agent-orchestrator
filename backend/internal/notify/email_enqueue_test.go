package notify

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// Email is a courtesy, never a dependency of the work -- and the courtesy still
// has to arrive. These two tests pin the halves of that which are easiest to
// regress when someone touches the write path.

type ctxRecordingEmailer struct {
	calls   int
	ctxErrs []error
	err     error
}

func (c *ctxRecordingEmailer) EmailNotification(ctx context.Context, _ domain.NotificationRecord) error {
	c.calls++
	c.ctxErrs = append(c.ctxErrs, ctx.Err())
	return c.err
}

// The regression this guards: a caller's context is routinely cancelled the
// instant the work finishes, which is exactly when the "it finished"
// notification is written. If the enqueue inherited that context it would fail
// in the common case, and the email would be lost -- silently, because every
// failure here is only a log line.
func TestEmailEnqueueDoesNotInheritACancelledCallerContext(t *testing.T) {
	store := &fakeStore{}
	emailer := &ctxRecordingEmailer{}
	m := New(Deps{Store: store, Emailer: emailer, NewID: func() string { return "ntf_1" }})

	// The caller is already done with its context by the time Notify returns.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := m.Notify(ctx, Intent{
		Type: domain.NotificationWorkflowCompleted, ProjectID: "mer", WorkflowRunID: "wf-1",
		DedupeKey: "wf-1@workflow_completed", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if emailer.calls != 1 {
		t.Fatalf("emailer calls = %d, want 1", emailer.calls)
	}
	if err := emailer.ctxErrs[0]; err != nil {
		t.Fatalf("the enqueue ran on a cancelled context (%v); it must use its own", err)
	}
}

// A failing enqueue is a log line, never a failed Notify: the notification row
// is already stored, and the work being reported is long since done.
func TestEmailEnqueueFailureDoesNotFailNotify(t *testing.T) {
	store := &fakeStore{}
	emailer := &ctxRecordingEmailer{err: context.DeadlineExceeded}
	m := New(Deps{Store: store, Emailer: emailer, NewID: func() string { return "ntf_1" }})

	if err := m.Notify(context.Background(), Intent{
		Type: domain.NotificationWorkflowCompleted, ProjectID: "mer", WorkflowRunID: "wf-1",
		DedupeKey: "wf-1@workflow_completed", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Notify returned %v, want nil despite the enqueue failing", err)
	}
	if len(store.rows) != 1 {
		t.Fatalf("stored notifications = %d, want the row kept", len(store.rows))
	}
}

// Only newly INSERTED rows owe an email. That is what makes the store's
// permanent event dedupe the email's dedupe too: a replayed event stores
// nothing, so it mails nothing -- no restart re-announcement, no second copy.
// (The full round trip over real SQLite is covered by the end-to-end restart
// test in the store package; this pins the rule at the layer that applies it.)
func TestADeduplicatedRowOwesNoEmail(t *testing.T) {
	store := &fakeStore{duplicate: true}
	emailer := &ctxRecordingEmailer{}
	m := New(Deps{Store: store, Emailer: emailer, NewID: func() string { return "ntf_1" }})

	if err := m.Notify(context.Background(), Intent{
		Type: domain.NotificationWorkflowCompleted, ProjectID: "mer", WorkflowRunID: "wf-1",
		DedupeKey: "wf-1@workflow_completed", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if emailer.calls != 0 {
		t.Fatalf("emailer calls = %d, want 0: a row that was not inserted owes no email", emailer.calls)
	}
}
