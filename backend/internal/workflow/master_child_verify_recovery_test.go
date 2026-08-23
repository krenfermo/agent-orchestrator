package workflow_test

// The parent half of the wf-6528a538 incident.
//
// The child run that stopped on a stale verification infrastructure failure was
// task 1 of a master objective. Its parent mirrored the stop as
// child_needs_attention, and every later task stayed blocked behind it. So a
// recovery that only heals the child is only half a fix: the person corrected
// ONE thing (AO's verifier) and pressed Continue on the ONE run that stopped, and
// the objective has to carry on from there by itself.
//
// This test drives the real autonomous stack — real sqlite store, real
// wake.Scheduler, real wakepoller, real notify.Manager — and the only human act
// in it is a single Continue on the CHILD.

import (
	"context"
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// verifierHostFailure is what a host that cannot start the verifier says. It
// classifies as a transient runtime infrastructure failure — never a code
// defect — which is the class this recovery exists for.
var verifierHostFailure = errors.New("fork/exec go: cannot allocate memory")

func TestChildVerifyRecoveryUnblocksItsMasterObjective(t *testing.T) {
	fx, ctx, masterID := startAutonomousObjective(t, twoTaskDependentPlan())
	_, childID := dispatchedChild(t, fx, masterID)

	// 1. AO's own verifier cannot run on this host. The child's work is fine and
	// its review is approved; verification simply never delivers a verdict.
	fx.verifier.err = verifierHostFailure
	driveUntil(t, fx, 40, func() bool { return runState(t, fx, childID) == domain.WorkflowRunNeedsAttention })
	driveCycles(t, fx, 10, func(int) {
		if _, active, ok := activeChildRunID(t, fx, masterID); ok {
			approveOpenReview(t, fx, active, domain.VerdictApproved)
		}
	})
	if got := runState(t, fx, childID); got != domain.WorkflowRunNeedsAttention {
		t.Fatalf("child state = %q, want needs_attention: the fixture never reached a stopped verification", got)
	}
	if got := childStepState(t, fx, childID, domain.WorkflowStepVerify); got != domain.WorkflowStepFailed {
		t.Fatalf("child verify step = %q, want failed: the fixture stopped for some other reason", got)
	}
	if got := newestCheckpointPhase(t, fx, childID); got != workflowcore.ReasonVerifyInfraFailed {
		t.Fatalf("child stop reason = %q, want %q", got, workflowcore.ReasonVerifyInfraFailed)
	}

	// 2. The parent mirrors it and stops, which is what blocks task 2.
	driveUntil(t, fx, 8, func() bool { return mirroredChildStop(t, fx, masterID) })
	if !mirroredChildStop(t, fx, masterID) {
		t.Fatal("the parent never mirrored its stopped child; the objective was never blocked")
	}

	// 3. The one human act: the host is repaired, and Continue is pressed on the
	// run that actually stopped. The parent is never touched.
	fx.verifier.err = nil
	if _, err := fx.coord.ContinueRun(ctx, childID); err != nil {
		t.Fatalf("ContinueRun(child): %v", err)
	}
	if got := runState(t, fx, childID); got == domain.WorkflowRunNeedsAttention {
		t.Fatalf("child state = %q after Continue: the stale verification was not reopened", got)
	}
	if got := countCheckpointPhase(t, fx, childID, "verify_recovery_requested"); got != 1 {
		t.Fatalf("child verify_recovery_requested checkpoints = %d, want exactly 1", got)
	}

	// 4. Everything from here is the daemon's own poller.
	driveCycles(t, fx, 40, func(int) {
		if _, active, ok := activeChildRunID(t, fx, masterID); ok {
			approveOpenReview(t, fx, active, domain.VerdictApproved)
		}
	})

	if got := runState(t, fx, childID); got != domain.WorkflowRunCompleted {
		t.Fatalf("child state = %q, want completed", got)
	}
	if got := childStepState(t, fx, childID, domain.WorkflowStepVerify); got != domain.WorkflowStepCompleted {
		t.Fatalf("child verify step = %q, want completed", got)
	}
	if mirroredChildStop(t, fx, masterID) {
		t.Fatal("the parent still mirrors child_needs_attention after its child recovered and completed")
	}
	master, _, err := fx.store.GetWorkflowRun(ctx, masterID)
	if err != nil {
		t.Fatalf("GetWorkflowRun(master): %v", err)
	}
	if master.State != domain.WorkflowRunCompleted {
		t.Fatalf("master state = %q, want completed: the objective never advanced past the recovered task", master.State)
	}
	taskA, _ := taskByPlanStepID(t, fx, masterID, "model")
	taskB, _ := taskByPlanStepID(t, fx, masterID, "tests")
	if taskA.State != domain.WorkflowTaskCompleted || taskB.State != domain.WorkflowTaskCompleted {
		t.Fatalf("tasks not both completed: A=%s B=%s", taskA.State, taskB.State)
	}
	if taskB.ExecutionRunID == nil {
		t.Fatal("task 2 was never dispatched: the parent never advanced past the recovered task")
	}
	// The recovered task kept its identity: no duplicate run, no duplicate task.
	if taskA.ExecutionRunID == nil || *taskA.ExecutionRunID != childID {
		t.Fatalf("task 1's execution run = %v, want the original %q", taskA.ExecutionRunID, childID)
	}
}

// TestChildFreshReviewRecoveryUnblocksItsMasterObjective is the same parent-side
// property for the drift half of the incident (Checkpoint 8P-E.14D): the child's
// workspace no longer matches the approval by the time the recovery runs,
// because the worker's uncommitted work was preserved across AO's own repair.
//
// The child must obtain a fresh review of the current workspace and verify THAT,
// and the objective must carry on from there — still on one human act, still
// with no touch of the parent.
func TestChildFreshReviewRecoveryUnblocksItsMasterObjective(t *testing.T) {
	fx, ctx, masterID := startAutonomousObjective(t, twoTaskDependentPlan())
	// A commit the approval can be pinned to. Without a HEAD on record AO refuses
	// to attribute any drift at all, which is the conservative default this test
	// deliberately steps out of.
	fx.ws.obs.HeadSHA = "head-before-the-repair"
	_, childID := dispatchedChild(t, fx, masterID)

	// 1. Same starting point as the test above: AO's verifier cannot run.
	fx.verifier.err = verifierHostFailure
	driveUntil(t, fx, 40, func() bool { return runState(t, fx, childID) == domain.WorkflowRunNeedsAttention })
	driveCycles(t, fx, 10, func(int) {
		if _, active, ok := activeChildRunID(t, fx, masterID); ok {
			approveOpenReview(t, fx, active, domain.VerdictApproved)
		}
	})
	if got := runState(t, fx, childID); got != domain.WorkflowRunNeedsAttention {
		t.Fatalf("child state = %q, want needs_attention", got)
	}
	driveUntil(t, fx, 8, func() bool { return mirroredChildStop(t, fx, masterID) })
	if !mirroredChildStop(t, fx, masterID) {
		t.Fatal("the parent never mirrored its stopped child")
	}

	// 2. AO is repaired, and the worker's uncommitted task changes are preserved
	// across the restart — so the worktree no longer hashes to what review
	// approved, at the very same commit.
	fx.verifier.err = nil
	fx.ws.obs.Changes = append(fx.ws.obs.Changes,
		ports.WorkspaceChange{Path: "internal/postrunqa/classify.go", Status: " M"})

	// 3. The one human act, on the child alone.
	if _, err := fx.coord.ContinueRun(ctx, childID); err != nil {
		t.Fatalf("ContinueRun(child): %v", err)
	}

	// 4. Everything else is the daemon's own poller, plus the reviewer answering
	// the fresh question AO asked it.
	driveCycles(t, fx, 40, func(int) {
		if _, active, ok := activeChildRunID(t, fx, masterID); ok {
			approveOpenReview(t, fx, active, domain.VerdictApproved)
		}
	})

	if got := countCheckpointPhase(t, fx, childID, "verify_fresh_review_required"); got != 1 {
		t.Fatalf("child verify_fresh_review_required checkpoints = %d, want exactly 1", got)
	}
	if got := countCheckpointPhase(t, fx, childID, "verify_fresh_review_approved"); got != 1 {
		t.Fatalf("child verify_fresh_review_approved checkpoints = %d, want exactly 1", got)
	}
	if got := runState(t, fx, childID); got != domain.WorkflowRunCompleted {
		t.Fatalf("child state = %q, want completed", got)
	}
	if got := childStepState(t, fx, childID, domain.WorkflowStepVerify); got != domain.WorkflowStepCompleted {
		t.Fatalf("child verify step = %q, want completed", got)
	}
	if mirroredChildStop(t, fx, masterID) {
		t.Fatal("the parent still mirrors child_needs_attention after its child recovered and completed")
	}
	master, _, err := fx.store.GetWorkflowRun(ctx, masterID)
	if err != nil {
		t.Fatalf("GetWorkflowRun(master): %v", err)
	}
	if master.State != domain.WorkflowRunCompleted {
		t.Fatalf("master state = %q, want completed: the objective never advanced past the recovered task", master.State)
	}
	taskA, _ := taskByPlanStepID(t, fx, masterID, "model")
	taskB, _ := taskByPlanStepID(t, fx, masterID, "tests")
	if taskB.ExecutionRunID == nil {
		t.Fatal("task 2 was never dispatched")
	}
	if taskA.ExecutionRunID == nil || *taskA.ExecutionRunID != childID {
		t.Fatalf("task 1's execution run = %v, want the original %q", taskA.ExecutionRunID, childID)
	}
}

// TestChildWorkspaceChangeRecoveryUnblocksItsMasterObjective is the parent half
// of the already-persisted case: the child's recovery generation reached a
// verify_workspace_changed verdict and PARKED on it, and only a later Continue
// can move it.
//
// Everything here is real code end to end. The first Continue's drift is refused
// because HEAD has moved (AO will not absorb somebody else's commit), which
// leaves exactly the durable shape wf-6528a538 was found in: run
// needs_attention, verify failed, and a generation-1 verify_workspace_changed
// result on disk. The person then restores the commit, keeping the worker's
// uncommitted work, and presses Continue again — which is the transition under
// test.
func TestChildWorkspaceChangeRecoveryUnblocksItsMasterObjective(t *testing.T) {
	fx, ctx, masterID := startAutonomousObjective(t, twoTaskDependentPlan())
	fx.ws.obs.HeadSHA = "head-at-approval"
	_, childID := dispatchedChild(t, fx, masterID)

	// 1. AO's verifier cannot run; the child stops and the parent mirrors it.
	fx.verifier.err = verifierHostFailure
	driveUntil(t, fx, 40, func() bool { return runState(t, fx, childID) == domain.WorkflowRunNeedsAttention })
	driveCycles(t, fx, 10, func(int) {
		if _, active, ok := activeChildRunID(t, fx, masterID); ok {
			approveOpenReview(t, fx, active, domain.VerdictApproved)
		}
	})
	driveUntil(t, fx, 8, func() bool { return mirroredChildStop(t, fx, masterID) })
	if !mirroredChildStop(t, fx, masterID) {
		t.Fatal("the parent never mirrored its stopped child")
	}

	// 2. The host is repaired, but the worktree has BOTH the worker's own
	// uncommitted work and somebody else's commit in it. The recovery runs,
	// refuses to absorb the commit, and parks on verify_workspace_changed.
	fx.verifier.err = nil
	fx.ws.obs.Changes = append(fx.ws.obs.Changes,
		ports.WorkspaceChange{Path: "internal/postrunqa/classify.go", Status: " M"})
	fx.ws.obs.HeadSHA = "someone-elses-commit"
	if _, err := fx.coord.ContinueRun(ctx, childID); err != nil {
		t.Fatalf("ContinueRun(child) #1: %v", err)
	}
	driveCycles(t, fx, 10, func(int) {})

	if got := countCheckpointPhase(t, fx, childID, "verify_recovery_requested"); got != 1 {
		t.Fatalf("child verify_recovery_requested checkpoints = %d, want exactly 1", got)
	}
	if got := countCheckpointPhase(t, fx, childID, "verify_fresh_review_required"); got != 0 {
		t.Fatalf("child verify_fresh_review_required checkpoints = %d, want 0: an external commit was absorbed", got)
	}
	if got := runState(t, fx, childID); got != domain.WorkflowRunNeedsAttention {
		t.Fatalf("child state = %q, want needs_attention on the refused drift", got)
	}
	if got := childStepState(t, fx, childID, domain.WorkflowStepVerify); got != domain.WorkflowStepFailed {
		t.Fatalf("child verify step = %q, want failed: the historical shape was never reached", got)
	}

	// 3. The person restores the commit, keeping the task's uncommitted work, and
	// presses Continue on the child alone. This is the recovery under test.
	fx.ws.obs.HeadSHA = "head-at-approval"
	if _, err := fx.coord.ContinueRun(ctx, childID); err != nil {
		t.Fatalf("ContinueRun(child) #2: %v", err)
	}
	if got := countCheckpointPhase(t, fx, childID, "verify_fresh_review_required"); got != 1 {
		t.Fatalf("child verify_fresh_review_required checkpoints = %d, want exactly 1: Continue was a no-op", got)
	}

	// 4. The daemon's own poller, plus the reviewer answering the fresh question.
	driveCycles(t, fx, 40, func(int) {
		if _, active, ok := activeChildRunID(t, fx, masterID); ok {
			approveOpenReview(t, fx, active, domain.VerdictApproved)
		}
	})

	if got := countCheckpointPhase(t, fx, childID, "verify_fresh_review_required"); got != 1 {
		t.Fatalf("child verify_fresh_review_required checkpoints = %d, want still exactly 1", got)
	}
	if got := countCheckpointPhase(t, fx, childID, "verify_fresh_review_approved"); got != 1 {
		t.Fatalf("child verify_fresh_review_approved checkpoints = %d, want exactly 1", got)
	}
	if got := countCheckpointPhase(t, fx, childID, "verify_recovery_requested"); got != 1 {
		t.Fatalf("child verify_recovery_requested checkpoints = %d, want still exactly 1: a second generation was consumed", got)
	}
	if got := runState(t, fx, childID); got != domain.WorkflowRunCompleted {
		t.Fatalf("child state = %q, want completed", got)
	}
	if mirroredChildStop(t, fx, masterID) {
		t.Fatal("the parent still mirrors child_needs_attention after its child recovered and completed")
	}
	master, _, err := fx.store.GetWorkflowRun(ctx, masterID)
	if err != nil {
		t.Fatalf("GetWorkflowRun(master): %v", err)
	}
	if master.State != domain.WorkflowRunCompleted {
		t.Fatalf("master state = %q, want completed: the objective never advanced", master.State)
	}
	taskA, _ := taskByPlanStepID(t, fx, masterID, "model")
	taskB, _ := taskByPlanStepID(t, fx, masterID, "tests")
	if taskB.ExecutionRunID == nil {
		t.Fatal("task 2 was never dispatched")
	}
	// Same child run throughout: no duplicate run, no duplicate task.
	if taskA.ExecutionRunID == nil || *taskA.ExecutionRunID != childID {
		t.Fatalf("task 1's execution run = %v, want the original %q", taskA.ExecutionRunID, childID)
	}
}

// runState is the child's durable run state, read straight from the store so the
// assertions never depend on a read path that itself has side effects.
func runState(t *testing.T, fx *autonomousFixture, runID string) domain.WorkflowRunState {
	t.Helper()
	run, ok, err := fx.store.GetWorkflowRun(context.Background(), runID)
	if err != nil || !ok {
		t.Fatalf("GetWorkflowRun(%s): %v (found=%v)", runID, err, ok)
	}
	return run.State
}

func childStepState(t *testing.T, fx *autonomousFixture, runID string, kind domain.WorkflowStepKind) domain.WorkflowStepState {
	t.Helper()
	steps, err := fx.store.ListWorkflowSteps(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListWorkflowSteps(%s): %v", runID, err)
	}
	for _, s := range steps {
		if s.Kind == kind {
			return s.State
		}
	}
	t.Fatalf("run %s has no %s step", runID, kind)
	return ""
}
