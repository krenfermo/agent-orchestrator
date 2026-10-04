package workflow_test

import (
	"context"
	"testing"
	"time"

	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

const boardRetention = 30 * time.Minute

// A run that has started is on the Board with a real phase and its own
// checklist — never a card with nothing on it.
func TestProjectBoardProjectsAStartedRun(t *testing.T) {
	sched := &recordingWakeScheduler{}
	c, store, clk := newHeartbeatCoordinator(sched)
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	markAutonomous(t, store, created.Run.ID, clk.Now())

	entries, err := c.ProjectBoard(ctx, "proj-1", boardRetention)
	if err != nil {
		t.Fatalf("ProjectBoard: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("board entries = %d, want 1", len(entries))
	}
	entry := entries[0]
	if entry.Run.ID != created.Run.ID {
		t.Fatalf("entry run = %q, want %q", entry.Run.ID, created.Run.ID)
	}
	if entry.ActivePhase != workflowcore.PhaseQueued {
		t.Fatalf("phase = %q, want queued for a created-but-not-started run", entry.ActivePhase)
	}
	if entry.ExecutionMode != "autonomous" {
		t.Fatalf("executionMode = %q, want autonomous", entry.ExecutionMode)
	}
	if len(entry.Steps) != 5 {
		t.Fatalf("checklist length = %d, want 5 (advance excluded)", len(entry.Steps))
	}
	if entry.Lifecycle.Attention != workflowcore.AttentionNone {
		t.Fatalf("attention = %q, want none: a queued run is not a problem", entry.Lifecycle.Attention)
	}
}

// The Board polls every run in a project every couple of seconds. Unlike
// GetRun, it must not drive dispatch as a side effect of being looked at:
// progression is the wake poller's job.
func TestProjectBoardHasNoDispatchSideEffects(t *testing.T) {
	sched := &recordingWakeScheduler{}
	c, store, clk := newHeartbeatCoordinator(sched)
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	markAutonomous(t, store, created.Run.ID, clk.Now())

	before, _, err := store.GetWorkflowRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GetWorkflowRun: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := c.ProjectBoard(ctx, "proj-1", boardRetention); err != nil {
			t.Fatalf("ProjectBoard: %v", err)
		}
	}
	after, _, err := store.GetWorkflowRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GetWorkflowRun: %v", err)
	}
	if after.State != before.State || !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("reading the board mutated the run: %v/%v -> %v/%v", before.State, before.UpdatedAt, after.State, after.UpdatedAt)
	}
	if len(sched.reasons) != 0 {
		t.Fatalf("reading the board scheduled wakes: %v", sched.reasons)
	}
}

// A cancelled run stays visible for the retention window, then drops off. A run
// that vanishes the instant it ends is indistinguishable from one that never
// existed.
func TestProjectBoardDropsLongFinishedRuns(t *testing.T) {
	sched := &recordingWakeScheduler{}
	c, _, clk := newHeartbeatCoordinator(sched)
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := c.CancelRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("CancelRun: %v", err)
	}

	entries, err := c.ProjectBoard(ctx, "proj-1", boardRetention)
	if err != nil {
		t.Fatalf("ProjectBoard: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("board entries = %d, want 1: a just-cancelled run stays visible", len(entries))
	}
	if entries[0].ActivePhase != workflowcore.PhaseCancelled {
		t.Fatalf("phase = %q, want cancelled", entries[0].ActivePhase)
	}

	clk.Advance(boardRetention + time.Minute)
	entries, err = c.ProjectBoard(ctx, "proj-1", boardRetention)
	if err != nil {
		t.Fatalf("ProjectBoard: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("board entries = %d, want 0 once retention has passed", len(entries))
	}
}

// Child runs belong under their parent, not beside it: a seven-task objective
// must read as one workflow with seven tasks.
func TestProjectBoardOmitsChildRunsFromTheTopLevel(t *testing.T) {
	sched := &recordingWakeScheduler{}
	c, store, _ := newHeartbeatCoordinator(sched)
	ctx := context.Background()

	parent, err := c.CreateRun(ctx, "proj-1", "objective")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	child, err := c.CreateRun(ctx, "proj-1", "task 1")
	if err != nil {
		t.Fatalf("CreateRun child: %v", err)
	}
	linkChildRun(t, store, child.Run.ID, parent.Run.ID)

	entries, err := c.ProjectBoard(ctx, "proj-1", boardRetention)
	if err != nil {
		t.Fatalf("ProjectBoard: %v", err)
	}
	if len(entries) != 1 || entries[0].Run.ID != parent.Run.ID {
		ids := make([]string, 0, len(entries))
		for _, e := range entries {
			ids = append(ids, e.Run.ID)
		}
		t.Fatalf("board entries = %v, want only the parent %q", ids, parent.Run.ID)
	}
}

// linkChildRun stamps the master->child link the coordinator writes when it
// dispatches a planned task.
func linkChildRun(t *testing.T, store *fakeStore, childID, parentID string) {
	t.Helper()
	run, ok, err := store.GetWorkflowRun(context.Background(), childID)
	if err != nil || !ok {
		t.Fatalf("GetWorkflowRun(%q): %v (found=%v)", childID, err, ok)
	}
	run.ParentWorkflowID = &parentID
	store.runs[childID] = run
}
