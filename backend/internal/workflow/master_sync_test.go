package workflow_test

// Checkpoint 8P-E.13 Phases 6 and 7: a parent task must never outlive the truth
// about its child, and a stop AO can remediate must never silence the run's
// heartbeat.
//
// Both tests drive the real autonomous stack (real sqlite store, real
// wake.Scheduler, real wakepoller) and inject facts out of band, exactly as
// autonomous_progression_test.go does — never by calling GetRun from the test.

import (
	"context"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
	"github.com/aoagents/agent-orchestrator/backend/internal/workflow/wake"
)

// TestChildCancellationDoesNotLeaveTheTaskRunning is the "Task 1 of 7 running"
// regression.
//
// The real master run wf-fe15c59d held task 1 at `running` for the entire time
// its child sat stopped, because reconcileMasterTasks only had an answer for a
// child that COMPLETED. Every other child outcome fell through, leaving the
// task row untouched forever — and since the parent had already moved to
// needs_attention, its heartbeat stopped too, so nothing ever revisited it.
func TestChildCancellationDoesNotLeaveTheTaskRunning(t *testing.T) {
	fx := newAutonomousFixture(t, twoTaskDependentPlan())
	ctx := context.Background()
	seedUser(t, fx.store, "user-1", "prof-claude-1", "prof-codex-1", true, fx.clk.Now())

	created, err := fx.coord.CreateObjectiveRun(ctx, "p", "Build users", domain.WorkflowPlanApprovalManual)
	if err != nil {
		t.Fatalf("CreateObjectiveRun: %v", err)
	}
	stampOwnerAndApplyPolicy(t, fx, created.Run.ID, "user-1")

	// Drive until a child run exists, then end it out of band — the equivalent
	// of a user cancelling that task's run, or its worker dying for good.
	var childID string
	driveCycles(t, fx, 6, func(int) {
		if _, id, ok := activeChildRunID(t, fx, created.Run.ID); ok && childID == "" {
			childID = id
		}
	})
	if childID == "" {
		t.Fatal("no child run was ever dispatched; fixture did not reach the state under test")
	}
	if _, err := fx.coord.CancelRun(ctx, childID); err != nil {
		t.Fatalf("CancelRun(child): %v", err)
	}

	driveCycles(t, fx, 4, nil)

	tasks, err := fx.store.ListWorkflowTasks(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("ListWorkflowTasks: %v", err)
	}
	for _, task := range tasks {
		if task.ExecutionRunID != nil && *task.ExecutionRunID == childID {
			if task.State == domain.WorkflowTaskRunning {
				t.Fatalf("task %d is still %q while its child run is terminal", task.Ordinal, task.State)
			}
			if !task.State.Terminal() {
				t.Fatalf("task %d state = %q, want a terminal state mirroring its cancelled child", task.Ordinal, task.State)
			}
			return
		}
	}
	t.Fatalf("no task references child run %s", childID)
}

// TestMasterStopHasAReasonAndAnAction: when a child's fate does stop the
// objective, the parent must be able to say what happened and what to do — not
// park in a needs_attention nobody can read.
func TestMasterStopHasAReasonAndAnAction(t *testing.T) {
	fx := newAutonomousFixture(t, twoTaskDependentPlan())
	ctx := context.Background()
	seedUser(t, fx.store, "user-1", "prof-claude-1", "prof-codex-1", true, fx.clk.Now())

	created, err := fx.coord.CreateObjectiveRun(ctx, "p", "Build users", domain.WorkflowPlanApprovalManual)
	if err != nil {
		t.Fatalf("CreateObjectiveRun: %v", err)
	}
	stampOwnerAndApplyPolicy(t, fx, created.Run.ID, "user-1")

	var childID string
	driveCycles(t, fx, 6, func(int) {
		if _, id, ok := activeChildRunID(t, fx, created.Run.ID); ok && childID == "" {
			childID = id
		}
	})
	if childID == "" {
		t.Fatal("no child run was ever dispatched")
	}
	if _, err := fx.coord.CancelRun(ctx, childID); err != nil {
		t.Fatalf("CancelRun(child): %v", err)
	}
	driveCycles(t, fx, 4, nil)

	detail, err := fx.coord.GetRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GetRun(master): %v", err)
	}
	life := workflowcore.DeriveLifecycle(workflowcore.LifecycleInput{Detail: detail, Questions: detail.Questions})
	if life.Attention == workflowcore.AttentionHuman && life.AttentionAction == "" {
		t.Fatalf("master run reports human_decision with reason=%q and no action", life.AttentionReason)
	}
	if detail.Run.State == domain.WorkflowRunNeedsAttention && life.AttentionReason == "" {
		t.Fatal("master run is stopped with no reason recorded anywhere")
	}
}

// TestHeartbeatSurvivesASelfRemediableStop is Phase 7's guarantee, asserted
// where it actually bit: a planner parked for a bounded retry moves the run
// nowhere a human can help, and before this checkpoint
// maybeScheduleAutonomousHeartbeat's unconditional `run.State ==
// NeedsAttention: return` meant any such stop permanently ended the only thing
// that would have driven the run forward.
//
// Here the planner fails once (retry scheduled) and then succeeds. Nothing but
// the poller ever touches the run, so the run only reaches a plan at all if the
// wake survived the stop.
func TestHeartbeatSurvivesASelfRemediableStop(t *testing.T) {
	fx := newAutonomousFixture(t, twoTaskDependentPlan())
	ctx := context.Background()
	seedUser(t, fx.store, "user-1", "prof-claude-1", "prof-codex-1", true, fx.clk.Now())
	fx.planner.failNext(plannerTimeoutErr())

	created, err := fx.coord.CreateObjectiveRun(ctx, "p", "Build users", domain.WorkflowPlanApprovalManual)
	if err != nil {
		t.Fatalf("CreateObjectiveRun: %v", err)
	}
	stampOwnerAndApplyPolicy(t, fx, created.Run.ID, "user-1")

	driveCycles(t, fx, 8, nil)

	plan, ok, err := fx.store.GetWorkflowPlan(ctx, created.Run.ID)
	if err != nil || !ok {
		t.Fatalf("GetWorkflowPlan: ok=%v err=%v", ok, err)
	}
	if plan.Status == domain.WorkflowPlanPending {
		t.Fatal("plan never regenerated after the planner's first timeout: the retry wake did not survive the stop")
	}
	if fx.planner.calls < 2 {
		t.Fatalf("planner calls = %d, want at least 2 (the failure plus its retry)", fx.planner.calls)
	}
}

// TestRestartResumesAfterASelfRemediableStop proves the same continuation
// survives a daemon restart: a brand-new Coordinator over the same store, with
// only Reconcile called, must leave a live wake behind for a run parked on a
// retryable stop. This is the "Mac slept / Electron closed / daemon restarted"
// case, exercised against real durable rows rather than in-process state.
func TestRestartResumesAfterASelfRemediableStop(t *testing.T) {
	fx := newAutonomousFixture(t, twoTaskDependentPlan())
	ctx := context.Background()
	seedUser(t, fx.store, "user-1", "prof-claude-1", "prof-codex-1", true, fx.clk.Now())
	fx.planner.failAlways(plannerTimeoutErr())

	created, err := fx.coord.CreateObjectiveRun(ctx, "p", "Build users", domain.WorkflowPlanApprovalManual)
	if err != nil {
		t.Fatalf("CreateObjectiveRun: %v", err)
	}
	stampOwnerAndApplyPolicy(t, fx, created.Run.ID, "user-1")
	driveCycles(t, fx, 2, nil)

	// Simulate the restart: a fresh Coordinator over the same durable rows,
	// with nothing carried over in memory.
	restarted := newAutonomousCoordinator(fx.store, fx.clk, fx.spawner, fx.planner, fx.ws, fx.launcher, fx.verifier, fx.sender, fx.wake, fx.emails, fx.newID)
	if err := restarted.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile after restart: %v", err)
	}

	next, err := fx.wake.NextForRun(ctx, domain.WorkflowRunID(created.Run.ID))
	if err != nil {
		t.Fatalf("NextForRun: %v", err)
	}
	if next == nil {
		t.Fatal("no wake survives the restart: the run would sit stopped forever with nothing to re-drive it")
	}
	if next.Reason != wake.ReasonTransientRetry && next.Reason != wake.ReasonAutonomousProgress {
		t.Fatalf("wake reason = %q, want a retry or heartbeat wake", next.Reason)
	}
}
