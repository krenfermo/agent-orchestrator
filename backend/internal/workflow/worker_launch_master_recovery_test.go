package workflow_test

// J: the master half of the wf-57f90ff2 incident.
//
// The child (task 4) stopped on a worker that never launched. The master
// mirrored that as child_needs_attention and every later task stayed blocked.
// This test drives the whole loop on the real autonomous stack — real sqlite
// store, real wake.Scheduler, real wakepoller, real notify.Manager — and proves
// the two halves of the fix meet:
//
//   - one ordinary Continue on the CHILD reopens its dispatch and starts
//     exactly one worker, and
//   - the master's existing reconcileMirroredChildStop then clears its own
//     mirror by itself, with nobody pressing anything on the parent.

import (
	"context"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestMasterClearsChildNeedsAttentionAfterAWorkerLaunchRecovery(t *testing.T) {
	fx, ctx, masterID := startAutonomousObjective(t, twoTaskDependentPlan())

	// The runtime refuses to start a worker — the incident's own error, used
	// here as an ordinary input.
	fx.spawner.failWith = errTmuxNoSuchSession
	_, childID := dispatchedChild(t, fx, masterID)

	// Drive the daemon until the child has spent its bounded launch budget and
	// genuinely stopped. Nothing is seeded: the stop is produced by the real
	// dispatch path.
	driveUntil(t, fx, 20, func() bool {
		run, ok, err := fx.store.GetWorkflowRun(ctx, childID)
		return err == nil && ok && run.State == domain.WorkflowRunNeedsAttention
	})
	child, ok, err := fx.store.GetWorkflowRun(ctx, childID)
	if err != nil || !ok {
		t.Fatalf("GetWorkflowRun(child): %v ok=%v", err, ok)
	}
	if child.State != domain.WorkflowRunNeedsAttention {
		t.Fatalf("child state = %q, want needs_attention: the launch failure never parked it", child.State)
	}
	if got := childWorkStepState(t, fx, childID); got != domain.WorkflowStepFailed {
		t.Fatalf("child work step = %q, want failed (the incident's shape)", got)
	}

	// The master must mirror it — that is the state the incident got stuck in.
	driveUntil(t, fx, 8, func() bool { return mirroredChildStop(t, fx, masterID) })
	if !mirroredChildStop(t, fx, masterID) {
		t.Fatal("the master never mirrored its stopped child; the fixture never reached the state under test")
	}
	spawnsAtStop := len(fx.spawner.calls)

	// The operator fixes the runtime and presses Continue ONCE, on the child.
	fx.spawner.failWith = nil
	if _, err := fx.coord.ContinueRun(ctx, childID); err != nil {
		t.Fatalf("ContinueRun(child): %v", err)
	}

	if got := len(fx.spawner.calls); got != spawnsAtStop+1 {
		t.Fatalf("spawn calls = %d, want exactly one more than %d", got, spawnsAtStop)
	}
	child, _, err = fx.store.GetWorkflowRun(ctx, childID)
	if err != nil {
		t.Fatalf("GetWorkflowRun(child): %v", err)
	}
	if child.State == domain.WorkflowRunNeedsAttention {
		t.Fatalf("child state = %q, want it out of needs_attention after the reopen", child.State)
	}
	if got := childWorkStepState(t, fx, childID); got != domain.WorkflowStepRunning {
		t.Fatalf("child work step = %q, want running", got)
	}

	// And now the parent, with nobody touching it: the daemon's own poller must
	// clear the mirror and return the master to running.
	driveUntil(t, fx, 10, func() bool { return !mirroredChildStop(t, fx, masterID) })
	if mirroredChildStop(t, fx, masterID) {
		t.Fatal("the master is still mirroring a child that has recovered")
	}
	master, ok, err := fx.store.GetWorkflowRun(ctx, masterID)
	if err != nil || !ok {
		t.Fatalf("GetWorkflowRun(master): %v ok=%v", err, ok)
	}
	if master.State == domain.WorkflowRunNeedsAttention {
		t.Fatalf("master state = %q, want it back out of needs_attention", master.State)
	}

	// The recovery must not have cost a duplicate child run or a duplicate task.
	tasks, err := fx.store.ListWorkflowTasks(ctx, masterID)
	if err != nil {
		t.Fatalf("ListWorkflowTasks: %v", err)
	}
	children := map[string]int{}
	for _, task := range tasks {
		if task.ExecutionRunID != nil {
			children[*task.ExecutionRunID]++
		}
	}
	if children[childID] != 1 {
		t.Fatalf("task->child mapping for %s = %d, want exactly 1", childID, children[childID])
	}
}

func childWorkStepState(t *testing.T, fx *autonomousFixture, runID string) domain.WorkflowStepState {
	t.Helper()
	steps, err := fx.store.ListWorkflowSteps(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListWorkflowSteps(%s): %v", runID, err)
	}
	for _, s := range steps {
		if s.Kind == domain.WorkflowStepWork {
			return s.State
		}
	}
	t.Fatalf("run %s has no work step", runID)
	return ""
}
