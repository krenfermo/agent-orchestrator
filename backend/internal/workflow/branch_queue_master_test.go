package workflow_test

// Checkpoint 8P-E.13A Phase 5: a master run must stay truthful while its child
// is queued behind a branch.
//
// The real objective wf-40209d5f went to needs_attention with the canonical
// reason child_needs_attention — a genuine human decision, on a Board card
// saying "Te necesita" — while the only thing that had actually happened was
// that its child was waiting for another workflow to release
// feat/engineering-control-center. Nobody had a decision to make.
//
// Drives the real autonomous stack (real sqlite store, real wake.Scheduler,
// real wakepoller) like master_sync_test.go, and injects the branch wait out of
// band rather than calling GetRun from the test.

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

func TestChildQueuedOnABranchNeverBecomesTheParentsHumanDecision(t *testing.T) {
	fx := newAutonomousFixture(t, twoTaskDependentPlan())
	ctx := context.Background()
	seedUser(t, fx.store, "user-1", "prof-claude-1", "prof-codex-1", true, fx.clk.Now())

	created, err := fx.coord.CreateObjectiveRun(ctx, "p", "Build users", domain.WorkflowPlanApprovalManual)
	if err != nil {
		t.Fatalf("CreateObjectiveRun: %v", err)
	}
	stampOwnerAndApplyPolicy(t, fx, created.Run.ID, "user-1")

	var taskID, childID string
	driveCycles(t, fx, 6, func(int) {
		if tid, id, ok := activeChildRunID(t, fx, created.Run.ID); ok && childID == "" {
			taskID, childID = tid, id
		}
	})
	if childID == "" {
		t.Fatal("no child run was ever dispatched; fixture did not reach the state under test")
	}

	parkChildOnBranch(t, fx, childID)

	// One reconcile pass over the parent, which is what a wake fires.
	if _, err := fx.coord.GetRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("GetRun(master): %v", err)
	}

	master, _, err := fx.store.GetWorkflowRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GetWorkflowRun(master): %v", err)
	}
	if master.State == domain.WorkflowRunNeedsAttention {
		t.Fatalf("master state = %q: a child queued behind a branch stopped the objective", master.State)
	}
	cps, err := fx.store.ListWorkflowCheckpoints(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("ListWorkflowCheckpoints(master): %v", err)
	}
	for _, cp := range cps {
		if cp.DurablePhase == workflowcore.ReasonChildNeedsAttention {
			t.Fatalf("master recorded %q for a child that is merely waiting: %q", cp.DurablePhase, cp.NextAction)
		}
	}
	tasks, err := fx.store.ListWorkflowTasks(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("ListWorkflowTasks: %v", err)
	}
	for _, task := range tasks {
		if task.ID != taskID {
			continue
		}
		if task.State != domain.WorkflowTaskRunning {
			t.Fatalf("task state = %q, want running: its child is waiting, not finished", task.State)
		}
	}
	// And no dependent task was let through while the first one is still out.
	for _, task := range tasks {
		if task.ID != taskID && task.State == domain.WorkflowTaskRunning {
			t.Fatalf("task %q started while its dependency was still queued on a branch", task.Title)
		}
	}
}

// parkChildOnBranch writes the exact durable shape a branch-blocked child has:
// run state Waiting plus a waiting_for_branch checkpoint naming the holder.
func parkChildOnBranch(t *testing.T, fx *autonomousFixture, childID string) {
	t.Helper()
	ctx := context.Background()
	child, ok, err := fx.store.GetWorkflowRun(ctx, childID)
	if err != nil || !ok {
		t.Fatalf("GetWorkflowRun(child): %v (found=%v)", err, ok)
	}
	if child.State != domain.WorkflowRunWaiting {
		if _, err := fx.store.UpdateWorkflowRunState(ctx, childID, child.State, domain.WorkflowRunWaiting, fx.clk.Now()); err != nil {
			t.Fatalf("park child in waiting: %v", err)
		}
	}
	if _, err := fx.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:            "wfc-branch-wait-fixture",
		WorkflowRunID: childID,
		ProjectID:     child.ProjectID,
		Branch:        "feat/engineering-control-center",
		WorktreePath:  "/repos/agent-orchestrator",
		NextAction: "waiting_for_branch: branch lock: feat/engineering-control-center is held by workflow " +
			"wf-3220567f — will resume automatically once that workflow releases the branch",
		DurablePhase:   "waiting_for_branch",
		PayloadVersion: "v1",
		RetryState:     `{"branch":"feat/engineering-control-center","repoPath":"/repos/agent-orchestrator","heldByWorkflowRunId":"wf-3220567f"}`,
		CreatedAt:      fx.clk.Now().Add(time.Second),
	}); err != nil {
		t.Fatalf("record branch wait: %v", err)
	}
}
