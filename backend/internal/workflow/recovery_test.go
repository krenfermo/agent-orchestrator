package workflow_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

func TestReconcileMovesInterruptedStepsAndRunToNeedsAttention(t *testing.T) {
	c, store := newCoordinator()
	ctx := context.Background()

	detail, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	runID := detail.Run.ID
	// The VERIFY step, not the plan step: since CP24-CP27 the plan step is no
	// longer an example of "mid-execution with no independent fact source" --
	// its work is a pure template expansion boot recovery now re-derives and
	// finishes (see resumeInterruptedStart). The generic interrupted-step rule
	// this test covers is unchanged, and verify is a step kind it still owns.
	interruptedStepID := detail.Steps[4].Step.ID

	now := time.Now().UTC()
	if _, err := store.UpdateWorkflowRunState(ctx, runID, domain.WorkflowRunPending, domain.WorkflowRunRunning, now); err != nil {
		t.Fatalf("force run running: %v", err)
	}
	if ok, err := store.UpdateWorkflowStepState(ctx, interruptedStepID, domain.WorkflowStepPending, domain.WorkflowStepReady, now); err != nil || !ok {
		t.Fatalf("force step ready: ok=%v err=%v", ok, err)
	}
	if ok, err := store.UpdateWorkflowStepState(ctx, interruptedStepID, domain.WorkflowStepReady, domain.WorkflowStepRunning, now); err != nil || !ok {
		t.Fatalf("force step running: ok=%v err=%v", ok, err)
	}

	if err := c.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	got, err := c.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.Run.State != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state after reconcile = %q, want needs_attention", got.Run.State)
	}
	if got.Steps[4].Step.State != domain.WorkflowStepWaiting {
		t.Fatalf("step state after reconcile = %q, want waiting", got.Steps[4].Step.State)
	}

	// Idempotent: running Reconcile a second time changes nothing further.
	if err := c.Reconcile(ctx); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	again, err := c.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun after second reconcile: %v", err)
	}
	if again.Run.State != domain.WorkflowRunNeedsAttention || again.Steps[4].Step.State != domain.WorkflowStepWaiting {
		t.Fatalf("state changed on second reconcile: run=%q step=%q", again.Run.State, again.Steps[0].Step.State)
	}
}

// TestReconcileAfterRestartRealStore simulates a daemon restart: create a run
// with a step forced into running on a real sqlite store, close and reopen a
// fresh store instance against the same file, then run the recovery
// reconciler and assert the step becomes waiting and the run becomes
// needs_attention. A second reconcile pass must be a no-op.
func TestReconcileAfterRestartRealStore(t *testing.T) {
	dataDir := t.TempDir()
	store1, err := sqlite.Open(dataDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	ctx := context.Background()

	if err := store1.UpsertProject(ctx, domain.ProjectRecord{
		ID: "proj-restart", Path: "/tmp/proj-restart", RegisteredAt: time.Now().UTC().Truncate(time.Second),
	}); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	coord1 := workflowcore.New(workflowcore.Deps{Store: store1})
	var detail workflowcore.RunDetail
	detail, err = coord1.CreateRun(ctx, "proj-restart", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	runID := detail.Run.ID
	// See TestReconcileMovesInterruptedStepsAndRunToNeedsAttention: the verify
	// step is the one with no independent fact source now that the plan step's
	// interrupted start is re-derivable.
	interruptedStepID := detail.Steps[4].Step.ID

	now := time.Now().UTC()
	if _, err := store1.UpdateWorkflowRunState(ctx, runID, domain.WorkflowRunPending, domain.WorkflowRunRunning, now); err != nil {
		t.Fatalf("force run running: %v", err)
	}
	if ok, err := store1.UpdateWorkflowStepState(ctx, interruptedStepID, domain.WorkflowStepPending, domain.WorkflowStepReady, now); err != nil || !ok {
		t.Fatalf("force step ready: ok=%v err=%v", ok, err)
	}
	if ok, err := store1.UpdateWorkflowStepState(ctx, interruptedStepID, domain.WorkflowStepReady, domain.WorkflowStepRunning, now); err != nil || !ok {
		t.Fatalf("force step running: ok=%v err=%v", ok, err)
	}
	if err := store1.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	// Simulate the restart: a fresh Store instance over the same database file.
	store2, err := sqlite.Open(dataDir)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = store2.Close() })
	coord2 := workflowcore.New(workflowcore.Deps{Store: store2})

	if err := coord2.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile after restart: %v", err)
	}
	got, err := coord2.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun after restart: %v", err)
	}
	if got.Run.State != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state after restart reconcile = %q, want needs_attention", got.Run.State)
	}
	if got.Steps[4].Step.State != domain.WorkflowStepWaiting {
		t.Fatalf("step state after restart reconcile = %q, want waiting", got.Steps[4].Step.State)
	}

	if err := coord2.Reconcile(ctx); err != nil {
		t.Fatalf("second Reconcile after restart: %v", err)
	}
	again, err := coord2.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun after second restart reconcile: %v", err)
	}
	if again.Run.State != domain.WorkflowRunNeedsAttention || again.Steps[4].Step.State != domain.WorkflowStepWaiting {
		t.Fatalf("state changed on second post-restart reconcile: run=%q step=%q", again.Run.State, again.Steps[4].Step.State)
	}
}
