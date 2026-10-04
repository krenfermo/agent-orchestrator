package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

func TestWorkflowWakeSchedule_UpsertClaimCompleteRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, time.August, 16, 12, 0, 0, 0, time.UTC)
	seedProject(t, s, "proj-wake-1")
	if _, _, err := s.CreateWorkflowRun(ctx, sampleWorkflowRun("proj-wake-1", "wake-run-1", now), nil); err != nil {
		t.Fatalf("create workflow run: %v", err)
	}

	created, err := s.UpsertWorkflowWakeSchedule(ctx, store.WorkflowWakeSchedule{
		ID:             "wfwk-1",
		WorkflowRunID:  "wake-run-1",
		Reason:         "worker_capacity",
		IdempotencyKey: "wfwake:wake-run-1:role:worker_capacity:worker_capacity",
		ScheduledAt:    now,
		CreatedAt:      now,
		UpdatedAt:      now,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if created.Status != "pending" || created.AttemptCount != 1 {
		t.Fatalf("unexpected inserted row: %+v", created)
	}

	// Idempotent re-schedule of the same key must update the same row, not
	// create a second one.
	rescheduled, err := s.UpsertWorkflowWakeSchedule(ctx, store.WorkflowWakeSchedule{
		ID:             "wfwk-should-not-be-used",
		WorkflowRunID:  "wake-run-1",
		Reason:         "worker_capacity",
		IdempotencyKey: "wfwake:wake-run-1:role:worker_capacity:worker_capacity",
		ScheduledAt:    now.Add(time.Minute),
		CreatedAt:      now,
		UpdatedAt:      now.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("reschedule via upsert: %v", err)
	}
	if rescheduled.ID != created.ID {
		t.Fatalf("expected same row id on idempotent upsert, got %s vs %s", rescheduled.ID, created.ID)
	}
	if rescheduled.AttemptCount != 2 {
		t.Fatalf("expected attempt_count 2 after reschedule, got %d", rescheduled.AttemptCount)
	}

	due, err := s.ListDueWorkflowWakeSchedules(ctx, now.Add(2*time.Minute), now.Add(-time.Hour), 25)
	if err != nil {
		t.Fatalf("list due: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("expected 1 due row, got %d", len(due))
	}

	ok, err := s.ClaimWorkflowWakeSchedule(ctx, created.ID, "pending", "claimant-1", now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !ok {
		t.Fatalf("expected claim to succeed")
	}

	// A second claim attempt against the now-stale expected status must not
	// double-claim.
	okAgain, err := s.ClaimWorkflowWakeSchedule(ctx, created.ID, "pending", "claimant-2", now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if okAgain {
		t.Fatalf("expected second claim to lose the CAS race")
	}

	completed, err := s.CompleteWorkflowWakeSchedule(ctx, created.ID, now.Add(3*time.Minute))
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if !completed {
		t.Fatalf("expected complete to succeed")
	}

	stillDue, err := s.ListDueWorkflowWakeSchedules(ctx, now.Add(10*time.Minute), now.Add(-time.Hour), 25)
	if err != nil {
		t.Fatalf("list due after complete: %v", err)
	}
	if len(stillDue) != 0 {
		t.Fatalf("expected no due rows after completion, got %d", len(stillDue))
	}
}

func TestWorkflowWakeSchedule_CancelAllForRun(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, time.August, 16, 12, 0, 0, 0, time.UTC)
	seedProject(t, s, "proj-wake-2")
	if _, _, err := s.CreateWorkflowRun(ctx, sampleWorkflowRun("proj-wake-2", "wake-run-2", now), nil); err != nil {
		t.Fatalf("create workflow run: %v", err)
	}

	if _, err := s.UpsertWorkflowWakeSchedule(ctx, store.WorkflowWakeSchedule{
		ID: "wfwk-a", WorkflowRunID: "wake-run-2", Reason: "worker_capacity",
		IdempotencyKey: "k1", ScheduledAt: now, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("insert a: %v", err)
	}
	if _, err := s.UpsertWorkflowWakeSchedule(ctx, store.WorkflowWakeSchedule{
		ID: "wfwk-b", WorkflowRunID: "wake-run-2", Reason: "reviewer_capacity",
		IdempotencyKey: "k2", ScheduledAt: now, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("insert b: %v", err)
	}

	n, err := s.CancelAllWorkflowWakeSchedulesByRun(ctx, "wake-run-2", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("cancel all: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 rows cancelled, got %d", n)
	}

	pending, err := s.ListPendingWorkflowWakeSchedulesByRun(ctx, "wake-run-2")
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("expected no pending rows left, got %d", len(pending))
	}
}

// Checkpoint 8P-E13A.1 regression, found in the field rather than in a test.
//
// idempotency_key is globally UNIQUE, so the upsert's "existing row is
// finished -> insert a fresh row" branch could never succeed: it always failed
// with a constraint violation, and the caller (scheduleBranchLockWake,
// maybeScheduleAutonomousHeartbeat) treats a scheduling failure as best-effort
// and swallows it. The observable result in ~/.ao/data was a queued workflow
// whose branch_lock wake had completed once and could never be scheduled again
// — a run that only ever moved when a human clicked Continue.
func TestWorkflowWakeSchedule_UpsertRevivesAFinishedScope(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, time.August, 21, 12, 0, 0, 0, time.UTC)
	seedProject(t, s, "proj-wake-3")
	if _, _, err := s.CreateWorkflowRun(ctx, sampleWorkflowRun("proj-wake-3", "wake-run-3", now), nil); err != nil {
		t.Fatalf("create workflow run: %v", err)
	}
	const key = "wfwake:wake-run-3:role:branch_lock:branch_lock"

	first, err := s.UpsertWorkflowWakeSchedule(ctx, store.WorkflowWakeSchedule{
		ID: "wfwk-first", WorkflowRunID: "wake-run-3", Reason: "worker_capacity",
		IdempotencyKey: key, ScheduledAt: now, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("first schedule: %v", err)
	}
	if _, err := s.ClaimWorkflowWakeSchedule(ctx, first.ID, "pending", "claimant", now); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := s.CompleteWorkflowWakeSchedule(ctx, first.ID, now.Add(time.Minute)); err != nil {
		t.Fatalf("complete: %v", err)
	}

	// The same scope waits again. This is the call that used to fail.
	again, err := s.UpsertWorkflowWakeSchedule(ctx, store.WorkflowWakeSchedule{
		ID: "wfwk-second", WorkflowRunID: "wake-run-3", Reason: "worker_capacity",
		IdempotencyKey: key, ScheduledAt: now.Add(2 * time.Minute),
		CreatedAt: now, UpdatedAt: now.Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatalf("re-schedule after completion: %v", err)
	}
	if again.ID != first.ID {
		t.Fatalf("revived row id = %q, want the existing scope's row %q", again.ID, first.ID)
	}
	if again.Status != "pending" {
		t.Fatalf("status = %q, want pending: the scope is waiting again", again.Status)
	}
	if again.AttemptCount != 0 {
		t.Fatalf("attempt_count = %d, want 0: a new wait must not inherit the finished one's backoff", again.AttemptCount)
	}
	if !again.ScheduledAt.Equal(now.Add(2 * time.Minute)) {
		t.Fatalf("scheduled_at = %v, want the new wait's time", again.ScheduledAt)
	}

	due, err := s.ListDueWorkflowWakeSchedules(ctx, now.Add(3*time.Minute), now.Add(-time.Hour), 25)
	if err != nil {
		t.Fatalf("list due: %v", err)
	}
	if len(due) != 1 || due[0].ID != first.ID {
		t.Fatalf("due = %+v, want the revived row to actually fire", due)
	}

	// A cancelled scope revives on the same terms — CancelRun cancels every
	// wake a run owns, and a later run-level retry must still be schedulable.
	if _, err := s.CancelWorkflowWakeSchedule(ctx, first.ID, now.Add(4*time.Minute)); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	revivedAgain, err := s.UpsertWorkflowWakeSchedule(ctx, store.WorkflowWakeSchedule{
		ID: "wfwk-third", WorkflowRunID: "wake-run-3", Reason: "worker_capacity",
		IdempotencyKey: key, ScheduledAt: now.Add(5 * time.Minute),
		CreatedAt: now, UpdatedAt: now.Add(5 * time.Minute),
	})
	if err != nil {
		t.Fatalf("re-schedule after cancellation: %v", err)
	}
	if revivedAgain.ID != first.ID || revivedAgain.Status != "pending" {
		t.Fatalf("revived row = %+v, want the same row back at pending", revivedAgain)
	}
}
