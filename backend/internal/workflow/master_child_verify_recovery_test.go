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
