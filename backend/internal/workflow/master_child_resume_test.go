package workflow_test

// Checkpoint 8P-E.13A.2, master half: the exact pair of rows found in
// ~/.ao/data on 2026-08-21.
//
//	wf-40209d5f  master, needs_attention, attention reason child_needs_attention.
//	wf-507d9a93  its child, needs_attention, plan=completed work=completed
//	             review=pending, latest checkpoint
//	             durable_phase=worker_observed_worker_result_available
//	             next_action=start_review.
//
// The child had already been recovered from its branch-lock wait and had
// already finished its work. Nothing was waiting on a person. It still never
// reviewed, because:
//
//   - reconcileMasterTasksOnce only offered ContinueRun — the ONLY path that
//     unblocks cycle 1's review — to a child in running/waiting; and
//   - the parent's mirrored child_needs_attention is a human-owned reason, so
//     the parent's own autonomous heartbeat stopped rescheduling itself, which
//     removed the last thing that would ever have called back.
//
// Drives the real autonomous stack (real sqlite store, real wake.Scheduler,
// real wakepoller), like branch_queue_master_test.go.

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

func TestRecoveredChildResumesIntoReviewAndClearsTheMastersAttention(t *testing.T) {
	fx := newAutonomousFixture(t, twoTaskDependentPlan())
	ctx := context.Background()
	seedUser(t, fx.store, "user-1", "prof-claude-1", "prof-codex-1", true, fx.clk.Now())

	created, err := fx.coord.CreateObjectiveRun(ctx, "p", "Build users", domain.WorkflowPlanApprovalManual)
	if err != nil {
		t.Fatalf("CreateObjectiveRun: %v", err)
	}
	stampOwnerAndApplyPolicy(t, fx, created.Run.ID, "user-1")

	// One cycle is enough to reach a dispatched child with a running work step.
	var taskID, childID string
	driveCycles(t, fx, 1, nil)
	if tid, id, ok := activeChildRunID(t, fx, created.Run.ID); ok {
		taskID, childID = tid, id
	}
	if childID == "" {
		t.Fatal("no child run was dispatched; fixture did not reach the state under test")
	}

	seedRecoveredButStrandedChild(t, fx, childID)
	seedMirroredChildStop(t, fx, created.Run.ID, childID)

	// From here on: only the daemon's own poller. No GetRun from a browser, no
	// ContinueRun from a person, no restart.
	driveCycles(t, fx, 4, nil)

	child, _, err := fx.store.GetWorkflowRun(ctx, childID)
	if err != nil {
		t.Fatalf("GetWorkflowRun(child): %v", err)
	}
	if child.State == domain.WorkflowRunNeedsAttention {
		t.Fatalf("child state = %q: a resolved branch wait still reads as a stop over completed work", child.State)
	}
	if got := realStepState(t, fx, childID, domain.WorkflowStepReview); got != domain.WorkflowStepRunning {
		t.Fatalf("child review step = %q, want running: its work completed and next_action was start_review", got)
	}
	if !realHasCheckpointPhase(t, fx, childID, "review_dispatched") {
		t.Fatal("no review dispatch checkpoint was written for the recovered child")
	}

	master, _, err := fx.store.GetWorkflowRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GetWorkflowRun(master): %v", err)
	}
	if master.State != domain.WorkflowRunRunning {
		t.Fatalf("master state = %q, want running while its task runs", master.State)
	}
	detail, err := fx.coord.GetRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GetRun(master): %v", err)
	}
	life := workflowcore.DeriveLifecycle(workflowcore.LifecycleInput{Detail: detail, Questions: detail.Questions})
	if life.AttentionReason == workflowcore.ReasonChildNeedsAttention {
		t.Fatalf("master still reports %q after its child resumed: %#v", life.AttentionReason, life)
	}
	if life.Attention == workflowcore.AttentionHuman {
		t.Fatalf("master asks for a human decision nobody has to make: %#v", life)
	}

	tasks, err := fx.store.ListWorkflowTasks(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("ListWorkflowTasks: %v", err)
	}
	for _, task := range tasks {
		if task.ID == taskID && task.State != domain.WorkflowTaskRunning {
			t.Fatalf("task state = %q, want running while its child reviews", task.State)
		}
		if task.ID != taskID && task.State == domain.WorkflowTaskRunning {
			t.Fatalf("task %q started while its dependency was still under review", task.Title)
		}
	}
}

// seedRecoveredButStrandedChild writes wf-507d9a93's durable state verbatim:
// the work step completed with its worker session attached, the review step
// still pending, the run row still carrying the needs_attention its branch wait
// left behind, and — on top of it — the generic observation checkpoint that
// names no canonical reason at all.
func seedRecoveredButStrandedChild(t *testing.T, fx *autonomousFixture, childID string) {
	t.Helper()
	ctx := context.Background()
	steps, err := fx.store.ListWorkflowSteps(ctx, childID)
	if err != nil {
		t.Fatalf("ListWorkflowSteps(child): %v", err)
	}
	var workStepID, sessionID string
	for _, s := range steps {
		if s.Kind != domain.WorkflowStepWork {
			continue
		}
		workStepID = s.ID
		if s.SessionID != nil {
			sessionID = *s.SessionID
		}
		if s.State != domain.WorkflowStepCompleted {
			if _, err := fx.store.UpdateWorkflowStepState(ctx, s.ID, s.State, domain.WorkflowStepCompleted, fx.clk.Now()); err != nil {
				t.Fatalf("complete work step: %v", err)
			}
		}
	}
	if workStepID == "" || sessionID == "" {
		t.Fatalf("child work step = %q with session %q; the fixture never dispatched a worker", workStepID, sessionID)
	}
	sess, found, err := fx.store.GetSession(ctx, domain.SessionID(sessionID))
	if err != nil || !found {
		t.Fatalf("GetSession(%s): %v (found=%v)", sessionID, err, found)
	}
	child, ok, err := fx.store.GetWorkflowRun(ctx, childID)
	if err != nil || !ok {
		t.Fatalf("GetWorkflowRun(child): %v (found=%v)", err, ok)
	}
	if child.State != domain.WorkflowRunNeedsAttention {
		if _, err := fx.store.UpdateWorkflowRunState(ctx, childID, child.State, domain.WorkflowRunNeedsAttention, fx.clk.Now()); err != nil {
			t.Fatalf("park child in needs_attention: %v", err)
		}
	}
	if _, err := fx.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID: "wfc-stranded-child", WorkflowRunID: childID, WorkflowStepID: &workStepID,
		ProjectID: child.ProjectID, SessionID: &sessionID,
		Branch: sess.Metadata.Branch, WorktreePath: sess.Metadata.WorkspacePath,
		NextAction:     "start_review",
		DurablePhase:   "worker_observed_worker_result_available",
		PayloadVersion: "v1", RetryState: "{}",
		CreatedAt: fx.clk.Now().Add(time.Second),
	}); err != nil {
		t.Fatalf("record stranded child checkpoint: %v", err)
	}
}

// seedMirroredChildStop writes wf-40209d5f's half: the master stopped purely
// because its child had, with the canonical reason that says so.
func seedMirroredChildStop(t *testing.T, fx *autonomousFixture, masterID, childID string) {
	t.Helper()
	ctx := context.Background()
	master, ok, err := fx.store.GetWorkflowRun(ctx, masterID)
	if err != nil || !ok {
		t.Fatalf("GetWorkflowRun(master): %v (found=%v)", err, ok)
	}
	if master.State != domain.WorkflowRunNeedsAttention {
		if _, err := fx.store.UpdateWorkflowRunState(ctx, masterID, master.State, domain.WorkflowRunNeedsAttention, fx.clk.Now()); err != nil {
			t.Fatalf("park master in needs_attention: %v", err)
		}
	}
	if _, err := fx.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID: "wfc-master-mirror", WorkflowRunID: masterID, ProjectID: master.ProjectID,
		NextAction:     "task 1 stopped and needs a decision — run " + childID,
		DurablePhase:   workflowcore.ReasonChildNeedsAttention,
		PayloadVersion: "v1", RetryState: "{}",
		CreatedAt: fx.clk.Now().Add(2 * time.Second),
	}); err != nil {
		t.Fatalf("record mirrored child stop: %v", err)
	}
}

// realStepState / realHasCheckpointPhase are branch_lock_deadlock_test.go's
// helpers of the same shape, over the real sqlite store this fixture uses.
func realStepState(t *testing.T, fx *autonomousFixture, runID string, kind domain.WorkflowStepKind) domain.WorkflowStepState {
	t.Helper()
	steps, err := fx.store.ListWorkflowSteps(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListWorkflowSteps: %v", err)
	}
	for _, s := range steps {
		if s.Kind == kind {
			return s.State
		}
	}
	t.Fatalf("run %s has no %s step", runID, kind)
	return ""
}

func realHasCheckpointPhase(t *testing.T, fx *autonomousFixture, runID, phase string) bool {
	t.Helper()
	cps, err := fx.store.ListWorkflowCheckpoints(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListWorkflowCheckpoints: %v", err)
	}
	for _, cp := range cps {
		if cp.DurablePhase == phase {
			return true
		}
	}
	return false
}
