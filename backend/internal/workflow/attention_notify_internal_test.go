package workflow

// The "your task stopped and needs you" notification.
//
// Before this, a run that parked in needs_attention — or ended failed —
// reached nobody. The only thing AO could announce was a run that finished,
// which is the one outcome that does not need anyone. A fix budget that runs
// out at 2am is exactly what the optional email fan-out exists for.
//
// These tests pin the two properties that make that safe to email: only a stop
// a person actually owns notifies, and one real event produces exactly one
// notification however many times AO re-derives it — per poll, per reconcile,
// and across a daemon restart.

import (
	stdctx "context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/notify"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// childRun creates a run that belongs to a planned task of a master plan —
// what the Board calls a Task, as opposed to the Workflow that owns it.
func childRun(t *testing.T, c *Coordinator, ctx stdctx.Context, objective string) domain.WorkflowRun {
	t.Helper()
	parent, err := c.CreateRun(ctx, "p", "the master objective")
	if err != nil {
		t.Fatalf("CreateRun(parent): %v", err)
	}
	parentID, taskID := parent.Run.ID, "task-1"
	detail, err := c.createSingleTaskRun(ctx, "p", objective, &parentID, &taskID)
	if err != nil {
		t.Fatalf("createSingleTaskRun: %v", err)
	}
	return detail.Run
}

func TestAttentionStopNotifiesATaskAndAWorkflowDifferently(t *testing.T) {
	for _, tc := range []struct {
		name string
		make func(t *testing.T, c *Coordinator, ctx stdctx.Context) domain.WorkflowRun
		want domain.NotificationType
	}{
		{"planned task", func(t *testing.T, c *Coordinator, ctx stdctx.Context) domain.WorkflowRun {
			return childRun(t, c, ctx, "Build the Post-Run QA evidence collector")
		}, domain.NotificationTaskNeedsAttention},
		{"workflow", func(t *testing.T, c *Coordinator, ctx stdctx.Context) domain.WorkflowRun {
			return runningRun(t, c, ctx, "Post-Run QA evidence")
		}, domain.NotificationWorkflowNeedsAttention},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink := &recordingSink{}
			c, _, ctx := newCompletionCoordinator(t, sink)
			run := tc.make(t, c, ctx)

			c.recordAttentionStop(ctx, run, nil, ReasonFixBudgetExhausted,
				"fix_budget_exhausted: the reviewer still requests changes after 4 review cycles (max_fix_cycles=3)")

			intents := sink.seen()
			if len(intents) != 1 {
				t.Fatalf("intents = %d, want 1 (%+v)", len(intents), intents)
			}
			intent := intents[0]
			if intent.Type != tc.want {
				t.Fatalf("type = %q, want %q", intent.Type, tc.want)
			}
			if intent.WorkflowRunID != run.ID {
				t.Fatalf("run anchor = %q, want %q", intent.WorkflowRunID, run.ID)
			}
			if intent.DedupeKey != run.ID+"@"+string(tc.want) {
				t.Fatalf("dedupe key = %q", intent.DedupeKey)
			}
			if intent.AttentionReason != ReasonFixBudgetExhausted {
				t.Fatalf("reason = %q, want %q", intent.AttentionReason, ReasonFixBudgetExhausted)
			}
			if intent.Detail == "" {
				t.Fatal("no detail: the notification cannot say what happened")
			}
		})
	}
}

// A terminal failure is a different event from a stop, and says so.
func TestObservedRunFailureNotifiesItsOwnEvent(t *testing.T) {
	sink := &recordingSink{}
	c, _, ctx := newCompletionCoordinator(t, sink)
	run := childRun(t, c, ctx, "Build the Post-Run QA evidence collector")

	c.notifyRunFailed(ctx, run, "Task 1 (Build the collector) ended without completing.")

	intents := sink.seen()
	if len(intents) != 1 {
		t.Fatalf("intents = %d, want 1", len(intents))
	}
	if intents[0].Type != domain.NotificationTaskFailed {
		t.Fatalf("type = %q, want task_failed", intents[0].Type)
	}
	if intents[0].DedupeKey != run.ID+"@task_failed" {
		t.Fatalf("dedupe key = %q", intents[0].DedupeKey)
	}
}

// A stop AO retries by itself is not a decision anyone can be asked to make.
// Emailing about a branch queue that clears in forty seconds is how a channel
// gets muted, which would cost the user the stops that do matter.
func TestSelfRemediableAndUnnamedStopsNotifyNobody(t *testing.T) {
	sink := &recordingSink{}
	c, _, ctx := newCompletionCoordinator(t, sink)
	run := runningRun(t, c, ctx, "Post-Run QA evidence")

	c.recordAttentionStop(ctx, run, nil, ReasonBranchQueued, "another run holds the branch")
	c.recordAttentionStop(ctx, run, nil, ReasonReviewCapacityRetry, "the reviewer is at capacity")
	c.recordAttentionStop(ctx, run, nil, "a_reason_nobody_registered", "unclassified")

	if got := sink.seen(); len(got) != 0 {
		t.Fatalf("intents = %d, want 0 (%+v)", len(got), got)
	}
}

// notificationsFixture wires the REAL notification writer over the real store,
// so what is asserted is rows and emails, not intents.
// The fan-out runs on its own goroutine, so this has to be safe to read from
// the test while a send is still in flight.
type recordingEmailer struct {
	mu   sync.Mutex
	sent []domain.NotificationRecord
}

func (e *recordingEmailer) EmailNotification(_ stdctx.Context, rec domain.NotificationRecord) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sent = append(e.sent, rec)
	return nil
}

func (e *recordingEmailer) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.sent)
}

func newNotifyingCoordinator(t *testing.T) (*Coordinator, *sqlite.Store, *recordingEmailer, stdctx.Context) {
	t.Helper()
	store := sqlitetest.MustOpen(t)
	ctx := stdctx.Background()
	if err := store.UpsertProject(ctx, domain.ProjectRecord{
		ID: "p", Path: t.TempDir(), RegisteredAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	emailer := &recordingEmailer{}
	writer := notify.New(notify.Deps{
		Store:   store,
		Emailer: emailer,
		Logger:  slog.Default(),
		Clock:   func() time.Time { return time.Now().UTC() },
	})
	coord := New(Deps{
		Store: store, Projects: store, Notifications: writer,
		Clock: func() time.Time { return time.Now().UTC() },
	})
	return coord, store, emailer, ctx
}

func storedNotifications(t *testing.T, store *sqlite.Store, ctx stdctx.Context) []domain.NotificationRecord {
	t.Helper()
	rows, err := store.ListNotifications(ctx, domain.NotificationListAll, time.Time{}, "", 100)
	if err != nil {
		t.Fatalf("ListNotifications: %v", err)
	}
	return rows
}

// The whole point of the durable event key: a stop AO re-derives on every poll,
// re-observes after a reconcile, and re-observes again after a restart is still
// one notification and one email.
func TestRepeatedAndRestartedStopsNotifyExactlyOnce(t *testing.T) {
	c, store, emailer, ctx := newNotifyingCoordinator(t)
	run := childRun(t, c, ctx, "Build the Post-Run QA evidence collector")

	// Every pass a live daemon would make over the same unchanged stop.
	for range 5 {
		c.recordAttentionStop(ctx, run, nil, ReasonFixBudgetExhausted,
			"fix_budget_exhausted: the reviewer still requests changes after 4 review cycles (max_fix_cycles=3)")
	}
	// A restart: a brand-new writer and coordinator over the same durable rows.
	restarted, _, restartedEmailer, _ := newNotifyingCoordinatorOver(t, store)
	restarted.recordAttentionStop(ctx, run, nil, ReasonFixBudgetExhausted,
		"fix_budget_exhausted: the reviewer still requests changes after 4 review cycles (max_fix_cycles=3)")

	rows := storedNotifications(t, store, ctx)
	attention := 0
	for _, row := range rows {
		if row.Type == domain.NotificationTaskNeedsAttention {
			attention++
		}
	}
	if attention != 1 {
		t.Fatalf("stored task_needs_attention rows = %d, want exactly 1", attention)
	}
	waitForEmails(t, emailer, 1)
	if got := restartedEmailer.count(); got != 0 {
		t.Fatalf("the restarted daemon re-emailed a stop the user already saw (%d)", got)
	}
}

// A run that stops and later fails reports both: they are different facts about
// different moments, and collapsing them would hide the outcome.
func TestAStopAndALaterFailureAreBothReported(t *testing.T) {
	c, store, emailer, ctx := newNotifyingCoordinator(t)
	run := childRun(t, c, ctx, "Build the Post-Run QA evidence collector")

	c.recordAttentionStop(ctx, run, nil, ReasonFixBudgetExhausted, "the fix budget ran out")
	c.notifyRunFailed(ctx, run, "the task ended without completing")
	c.notifyRunFailed(ctx, run, "the task ended without completing")

	byType := map[domain.NotificationType]int{}
	for _, row := range storedNotifications(t, store, ctx) {
		byType[row.Type]++
	}
	if byType[domain.NotificationTaskNeedsAttention] != 1 || byType[domain.NotificationTaskFailed] != 1 {
		t.Fatalf("rows by type = %+v, want one of each", byType)
	}
	waitForEmails(t, emailer, 2)
}

// The completion family is untouched by any of this: one row, one email, same
// type, same key.
func TestCompletionNotificationIsUnchanged(t *testing.T) {
	c, store, emailer, ctx := newNotifyingCoordinator(t)
	run := runningRun(t, c, ctx, "Post-Run QA evidence")

	for range 3 {
		if _, err := c.completeRun(ctx, run, domain.WorkflowRunRunning); err != nil {
			t.Fatalf("completeRun: %v", err)
		}
	}

	rows := storedNotifications(t, store, ctx)
	if len(rows) != 1 {
		t.Fatalf("stored rows = %d, want 1", len(rows))
	}
	if rows[0].Type != domain.NotificationWorkflowCompleted {
		t.Fatalf("type = %q, want workflow_completed", rows[0].Type)
	}
	if rows[0].DedupeKey != run.ID {
		t.Fatalf("dedupe key = %q, want the run id — the completion key must not change", rows[0].DedupeKey)
	}
	waitForEmails(t, emailer, 1)
}

func newNotifyingCoordinatorOver(t *testing.T, store *sqlite.Store) (*Coordinator, *sqlite.Store, *recordingEmailer, stdctx.Context) {
	t.Helper()
	emailer := &recordingEmailer{}
	writer := notify.New(notify.Deps{
		Store: store, Emailer: emailer, Logger: slog.Default(),
		Clock: func() time.Time { return time.Now().UTC() },
	})
	coord := New(Deps{
		Store: store, Projects: store, Notifications: writer,
		Clock: func() time.Time { return time.Now().UTC() },
	})
	return coord, store, emailer, stdctx.Background()
}

// waitForEmails polls for the expected count and then keeps watching briefly,
// because the fan-out is deliberately asynchronous: a slow mail server must
// never delay the work being reported. The trailing window is what makes "and
// no more than that" a real assertion rather than a race the test wins by
// checking too early.
func waitForEmails(t *testing.T, emailer *recordingEmailer, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && emailer.count() < want {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if got := emailer.count(); got != want {
		t.Fatalf("emails sent = %d, want %d", got, want)
	}
}
