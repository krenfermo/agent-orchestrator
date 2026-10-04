package workflow_test

// Checkpoint 8P-D: planner auto-start + auto-approval. All three tests here
// drive an objective run purely via wakepoller.Poller.RunDueOnce after the
// one manual "kickoff" call (ApplyExecutionPolicySnapshot, mirroring
// stampOwner's real HTTP-controller sequence) -- never a direct
// GeneratePlan/ApprovePlan/GetRun/ContinueRun call from the test itself.

import (
	"context"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// TestManualMode_NoAutoGeneratePlan: no ApplyExecutionPolicySnapshot call at
// all (userID never resolved, exactly like an unowned/pre-8P-A run) -- the
// planner must never auto-start, the plan stays Pending, and the run stays
// Pending, no matter how many poller cycles pass.
func TestManualMode_NoAutoGeneratePlan(t *testing.T) {
	fx := newAutonomousFixture(t, twoTaskDependentPlan())
	ctx := context.Background()

	created, err := fx.coord.CreateObjectiveRun(ctx, "p", "Build users", domain.WorkflowPlanApprovalManual)
	if err != nil {
		t.Fatalf("CreateObjectiveRun: %v", err)
	}

	driveCycles(t, fx, 5, nil)

	if fx.planner.calls != 0 {
		t.Fatalf("planner calls = %d, want 0 (manual mode must never auto-start)", fx.planner.calls)
	}
	plan, isMaster, err := fx.store.GetWorkflowPlan(ctx, created.Run.ID)
	if err != nil || !isMaster {
		t.Fatalf("GetWorkflowPlan: plan=%+v isMaster=%v err=%v", plan, isMaster, err)
	}
	if plan.Status != domain.WorkflowPlanPending {
		t.Fatalf("plan status = %q, want pending", plan.Status)
	}
	run, _, err := fx.store.GetWorkflowRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GetWorkflowRun: %v", err)
	}
	if run.State != domain.WorkflowRunPending {
		t.Fatalf("run state = %q, want pending", run.State)
	}
	if next, err := fx.wake.NextForRun(ctx, domain.WorkflowRunID(created.Run.ID)); err != nil {
		t.Fatalf("NextForRun: %v", err)
	} else if next != nil {
		t.Fatalf("expected no wake scheduled for a manual-mode run, got %+v", next)
	}
}

// TestAutonomous_PlannerStartsAndAutoApprovesValidPlan is Checkpoint 8P-D's
// central claim: applying an AUTONOMOUS execution policy snapshot is the
// ONLY driving call this test ever makes -- the planner auto-starts, the
// plan auto-approves (approvalMode recorded as "auto" even though the
// client requested Manual), and the first eligible task dispatches, purely
// via the poller.
func TestAutonomous_PlannerStartsAndAutoApprovesValidPlan(t *testing.T) {
	fx := newAutonomousFixture(t, twoTaskDependentPlan())
	ctx := context.Background()
	seedUser(t, fx.store, "user-1", "prof-claude-1", "prof-codex-1", true, fx.clk.Now())

	created, err := fx.coord.CreateObjectiveRun(ctx, "p", "Build users", domain.WorkflowPlanApprovalManual)
	if err != nil {
		t.Fatalf("CreateObjectiveRun: %v", err)
	}
	stampOwnerAndApplyPolicy(t, fx, created.Run.ID, "user-1")

	driveCycles(t, fx, 6, nil)

	if fx.planner.calls != 1 {
		t.Fatalf("planner calls = %d, want exactly 1", fx.planner.calls)
	}
	record, isMaster, err := fx.store.GetWorkflowPlan(ctx, created.Run.ID)
	if err != nil || !isMaster {
		t.Fatalf("GetWorkflowPlan: %+v isMaster=%v err=%v", record, isMaster, err)
	}
	if record.Status != domain.WorkflowPlanApproved {
		t.Fatalf("plan status = %q, want approved", record.Status)
	}
	if record.ApprovalMode != domain.WorkflowPlanApprovalAuto {
		t.Fatalf("plan approval mode = %q, want auto (policy-decided, not client-requested)", record.ApprovalMode)
	}
	task, ok := taskByPlanStepID(t, fx, created.Run.ID, "model")
	if !ok {
		t.Fatalf("task for plan step %q not found", "model")
	}
	if task.ExecutionRunID == nil {
		t.Fatalf("first eligible task has no ExecutionRunID -- was never dispatched")
	}
}

// TestAutonomous_InvalidPlanNeverAutoApproves proves the safety gate is
// unconditional: an autonomous policy never bypasses NormalizeAndValidatePlan.
// A plan with a dependency cycle must land the run in NeedsAttention, the
// plan in Invalid, and never create a single task/child run -- again driven
// purely by the poller after the one ApplyExecutionPolicySnapshot kickoff.
func TestAutonomous_InvalidPlanNeverAutoApproves(t *testing.T) {
	fx := newAutonomousFixture(t, invalidCyclePlan())
	ctx := context.Background()
	seedUser(t, fx.store, "user-1", "prof-claude-1", "prof-codex-1", true, fx.clk.Now())

	created, err := fx.coord.CreateObjectiveRun(ctx, "p", "Build users", domain.WorkflowPlanApprovalManual)
	if err != nil {
		t.Fatalf("CreateObjectiveRun: %v", err)
	}
	stampOwnerAndApplyPolicy(t, fx, created.Run.ID, "user-1")

	driveCycles(t, fx, 6, nil)

	if fx.planner.calls != 1 {
		t.Fatalf("planner calls = %d, want exactly 1", fx.planner.calls)
	}
	run, _, err := fx.store.GetWorkflowRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GetWorkflowRun: %v", err)
	}
	if run.State != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention", run.State)
	}
	record, isMaster, err := fx.store.GetWorkflowPlan(ctx, created.Run.ID)
	if err != nil || !isMaster {
		t.Fatalf("GetWorkflowPlan: %+v isMaster=%v err=%v", record, isMaster, err)
	}
	if record.Status != domain.WorkflowPlanInvalid {
		t.Fatalf("plan status = %q, want invalid", record.Status)
	}
	tasks, err := fx.store.ListWorkflowTasks(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("ListWorkflowTasks: %v", err)
	}
	if len(tasks) != 0 {
		t.Fatalf("invalid plan created %d tasks, want 0", len(tasks))
	}

	// A needs_attention run must not keep re-scheduling the headless
	// heartbeat forever -- eventually no wake remains pending for it.
	driveCycles(t, fx, 3, nil)
	next, err := fx.wake.NextForRun(ctx, domain.WorkflowRunID(created.Run.ID))
	if err != nil {
		t.Fatalf("NextForRun: %v", err)
	}
	if next != nil {
		t.Fatalf("expected no further wake for a needs_attention run, got %+v", next)
	}
}
