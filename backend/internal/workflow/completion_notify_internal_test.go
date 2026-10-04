package workflow

import (
	stdctx "context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// The "your workflow finished" notification. completeRun is the single door
// into WorkflowRunCompleted, and the notification hangs off the CAS result
// rather than sitting beside it — which is what makes "exactly once per run"
// a property of the state machine instead of a convention callers must keep.

type recordingSink struct {
	mu      sync.Mutex
	intents []ports.NotificationIntent
	err     error
}

func (s *recordingSink) Notify(_ stdctx.Context, intent ports.NotificationIntent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.intents = append(s.intents, intent)
	return s.err
}

func (s *recordingSink) seen() []ports.NotificationIntent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ports.NotificationIntent(nil), s.intents...)
}

func newCompletionCoordinator(t *testing.T, sink NotificationSink) (*Coordinator, *sqlite.Store, stdctx.Context) {
	t.Helper()
	store := sqlitetest.MustOpen(t)
	ctx := stdctx.Background()
	if err := store.UpsertProject(ctx, domain.ProjectRecord{
		ID: "p", Path: t.TempDir(), RegisteredAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	coord := New(Deps{
		Store:         store,
		Projects:      store,
		Notifications: sink,
		Clock:         func() time.Time { return time.Now().UTC() },
	})
	return coord, store, ctx
}

func runningRun(t *testing.T, c *Coordinator, ctx stdctx.Context, objective string) domain.WorkflowRun {
	t.Helper()
	detail, err := c.CreateRun(ctx, "p", objective)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := c.store.UpdateWorkflowRunState(
		ctx, detail.Run.ID, domain.WorkflowRunPending, domain.WorkflowRunRunning, time.Now().UTC(),
	); err != nil {
		t.Fatalf("advance to running: %v", err)
	}
	detail.Run.State = domain.WorkflowRunRunning
	return detail.Run
}

func TestCompleteRunNotifiesOnce(t *testing.T) {
	sink := &recordingSink{}
	c, store, ctx := newCompletionCoordinator(t, sink)
	run := runningRun(t, c, ctx, "ship the thing")

	completed, err := c.completeRun(ctx, run, domain.WorkflowRunRunning)
	if err != nil {
		t.Fatalf("completeRun: %v", err)
	}
	if !completed {
		t.Fatal("completeRun reported no transition on a running run")
	}

	stored, ok, err := store.GetWorkflowRun(ctx, run.ID)
	if err != nil || !ok {
		t.Fatalf("GetWorkflowRun: %v ok=%v", err, ok)
	}
	if stored.State != domain.WorkflowRunCompleted {
		t.Fatalf("run state = %q, want completed", stored.State)
	}

	intents := sink.seen()
	if len(intents) != 1 {
		t.Fatalf("intents = %d, want 1 (%+v)", len(intents), intents)
	}
	intent := intents[0]
	if intent.Type != domain.NotificationWorkflowCompleted {
		t.Fatalf("type = %q, want workflow_completed", intent.Type)
	}
	if intent.WorkflowRunID != run.ID || intent.DedupeKey != run.ID {
		t.Fatalf("run anchor/dedupe = %q/%q, want %q", intent.WorkflowRunID, intent.DedupeKey, run.ID)
	}
	if intent.ProjectID != "p" {
		t.Fatalf("project = %q, want p", intent.ProjectID)
	}
	if intent.WorkflowObjective != "ship the thing" {
		t.Fatalf("objective = %q", intent.WorkflowObjective)
	}
	// A run-level notification has no session to borrow, and inventing one
	// would put the wrong owner on the row.
	if intent.SessionID != "" {
		t.Fatalf("session = %q, want none for a run-level notification", intent.SessionID)
	}
}

// The retry/restart guarantee, taken straight from the state machine: terminal
// states have no outgoing transitions, so the second CAS matches no row and
// nothing is announced again.
func TestCompleteRunNotifiesNothingOnRepeat(t *testing.T) {
	sink := &recordingSink{}
	c, _, ctx := newCompletionCoordinator(t, sink)
	run := runningRun(t, c, ctx, "ship the thing")

	for range 3 {
		if _, err := c.completeRun(ctx, run, domain.WorkflowRunRunning); err != nil {
			t.Fatalf("completeRun: %v", err)
		}
	}
	if got := sink.seen(); len(got) != 1 {
		t.Fatalf("intents = %d, want 1 — a repeated completion re-announced itself", len(got))
	}
}

// A run that is not in the expected state never completes, so it never
// notifies: the notification cannot get ahead of the durable fact.
func TestCompleteRunDoesNotNotifyWhenTheCASMisses(t *testing.T) {
	sink := &recordingSink{}
	c, _, ctx := newCompletionCoordinator(t, sink)
	detail, err := c.CreateRun(ctx, "p", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	// Still pending, but told to complete from running.
	completed, err := c.completeRun(ctx, detail.Run, domain.WorkflowRunRunning)
	if err != nil {
		t.Fatalf("completeRun: %v", err)
	}
	if completed {
		t.Fatal("completeRun claimed a transition that the state machine forbids")
	}
	if got := sink.seen(); len(got) != 0 {
		t.Fatalf("intents = %d, want 0", len(got))
	}
}

// A notification store that is down must not turn a workflow that genuinely
// finished into one that failed.
func TestCompleteRunSurvivesANotificationFailure(t *testing.T) {
	sink := &recordingSink{err: errors.New("notification store unavailable")}
	c, store, ctx := newCompletionCoordinator(t, sink)
	run := runningRun(t, c, ctx, "ship the thing")

	completed, err := c.completeRun(ctx, run, domain.WorkflowRunRunning)
	if err != nil {
		t.Fatalf("completeRun returned an error for a failed notification: %v", err)
	}
	if !completed {
		t.Fatal("completeRun reported no transition")
	}
	stored, _, err := store.GetWorkflowRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetWorkflowRun: %v", err)
	}
	if stored.State != domain.WorkflowRunCompleted {
		t.Fatalf("run state = %q, want completed", stored.State)
	}
}

// A nil sink is the supported "no notifications wired" configuration, not a
// crash waiting for the first workflow to finish.
func TestCompleteRunWithoutASinkStillCompletes(t *testing.T) {
	c, store, ctx := newCompletionCoordinator(t, nil)
	run := runningRun(t, c, ctx, "ship the thing")

	if _, err := c.completeRun(ctx, run, domain.WorkflowRunRunning); err != nil {
		t.Fatalf("completeRun: %v", err)
	}
	stored, _, err := store.GetWorkflowRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetWorkflowRun: %v", err)
	}
	if stored.State != domain.WorkflowRunCompleted {
		t.Fatalf("run state = %q, want completed", stored.State)
	}
}
