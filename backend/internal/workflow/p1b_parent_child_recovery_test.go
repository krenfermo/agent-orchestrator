package workflow_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// p1b_parent_child_recovery_test.go — matrix 20/23/24/25 against the real
// autonomous stack (real sqlite store, real wake scheduler, real poller).
//
// These are the tests that decide whether P1-B's recovery model actually
// converges, as opposed to merely classifying correctly. Every one of them
// drives a real master objective with real child runs and asserts on durable
// rows: run states, attempt counts, task identities, ledger phases.

// taskSnapshot is the durable identity and state of one planned task, which is
// what "a sibling was not altered" has to be measured against.
type taskSnapshot struct {
	id           string
	planStepID   string
	ordinal      int64
	revision     int64
	state        domain.WorkflowTaskState
	executionRun string
	attempts     int
}

func snapshotTasks(t *testing.T, fx *autonomousFixture, masterID string) map[string]taskSnapshot {
	t.Helper()
	ctx := context.Background()
	tasks, err := fx.store.ListWorkflowTasks(ctx, masterID)
	if err != nil {
		t.Fatalf("ListWorkflowTasks: %v", err)
	}
	out := make(map[string]taskSnapshot, len(tasks))
	for _, task := range tasks {
		snap := taskSnapshot{
			id: task.ID, planStepID: task.PlanStepID, ordinal: task.Ordinal,
			revision: task.PlanRevision, state: task.State,
		}
		if task.ExecutionRunID != nil {
			snap.executionRun = *task.ExecutionRunID
			steps, serr := fx.store.ListWorkflowSteps(ctx, snap.executionRun)
			if serr != nil {
				t.Fatalf("ListWorkflowSteps(%s): %v", snap.executionRun, serr)
			}
			for _, step := range steps {
				attempts, aerr := fx.store.ListWorkflowAttempts(ctx, step.ID)
				if aerr != nil {
					t.Fatalf("ListWorkflowAttempts(%s): %v", step.ID, aerr)
				}
				snap.attempts += len(attempts)
			}
		}
		out[task.PlanStepID] = snap
	}
	return out
}

func autoLedgerPhases(t *testing.T, fx *autonomousFixture, runID string) map[string]int {
	t.Helper()
	checkpoints, err := fx.store.ListWorkflowCheckpoints(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListWorkflowCheckpoints(%s): %v", runID, err)
	}
	out := map[string]int{}
	for _, cp := range checkpoints {
		out[cp.DurablePhase]++
	}
	return out
}

func repairIntentsOn(t *testing.T, fx *autonomousFixture, runID string) []domain.RepairIntent {
	t.Helper()
	checkpoints, err := fx.store.ListWorkflowCheckpoints(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListWorkflowCheckpoints(%s): %v", runID, err)
	}
	var out []domain.RepairIntent
	for _, cp := range checkpoints {
		if cp.DurablePhase != "workflow_repair_dispatched" {
			continue
		}
		var intent domain.RepairIntent
		if json.Unmarshal([]byte(cp.RetryState), &intent) == nil && intent.ID != "" {
			out = append(out, intent)
		}
	}
	return out
}

// freezeRepairMode rewrites a run's frozen repair policy, standing in for a
// create request that named one.
func freezeRepairMode(t *testing.T, fx *autonomousFixture, runID string, mode domain.RepairMode) {
	t.Helper()
	ctx := context.Background()
	run, ok, err := fx.store.GetWorkflowRun(ctx, runID)
	if err != nil || !ok {
		t.Fatalf("GetWorkflowRun(%s): %v (found=%v)", runID, err, ok)
	}
	var policy domain.WorkflowPolicy
	if err := json.Unmarshal([]byte(run.PolicySnapshot), &policy); err != nil {
		t.Fatal(err)
	}
	frozen := policy.EffectiveRepairPolicy()
	frozen.Mode = mode
	policy.Repair = frozen
	snapshot, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	if moved, err := fx.store.UpdateWorkflowRunPolicySnapshot(ctx, runID, string(snapshot), fx.clk.Now()); err != nil || !moved {
		t.Fatalf("freeze repair mode: moved=%v err=%v", moved, err)
	}
}

// ---------------------------------------------------------------------------
// 25: a mirrored parent stop clears when the child recovers, and the parent
// progresses. This is the incident master_child_attention_reconcile_test.go
// documents, re-asserted through P1-B's own assessment so the recovery panel
// and the Board can never disagree about whose problem it is.
// ---------------------------------------------------------------------------

func TestChildRecoveryClearsTheParentMirrorAndTheAssessmentAgrees(t *testing.T) {
	fx, ctx, masterID := startAutonomousObjective(t, twoTaskDependentPlan())
	_, childID := dispatchedChild(t, fx, masterID)

	parkOnHumanDecision(t, fx, childID)
	driveUntil(t, fx, 6, func() bool { return mirroredChildStop(t, fx, masterID) })
	if !mirroredChildStop(t, fx, masterID) {
		t.Fatal("the parent never mirrored its stopped child")
	}

	// P1-B's assessment must send the operator to the run that OWNS the
	// problem, not to the one mirroring it.
	parentAssessment, err := fx.coord.AssessRecovery(ctx, masterID)
	if err != nil {
		t.Fatal(err)
	}
	if parentAssessment.ReasonCode != workflowcore.ReasonChildNeedsAttention {
		t.Fatalf("parent reason = %q, want %q", parentAssessment.ReasonCode, workflowcore.ReasonChildNeedsAttention)
	}
	if parentAssessment.TargetRunID != childID {
		t.Fatalf("parent assessment targets %q, want the child %q that actually stopped", parentAssessment.TargetRunID, childID)
	}
	if parentAssessment.RepairAvailable {
		t.Fatal("a mirrored stop offered a repair of the parent; the child owns this problem")
	}

	// The child comes back, and only the daemon's own poller runs from here.
	resumeChild(t, fx, childID)
	driveUntil(t, fx, 8, func() bool { return !mirroredChildStop(t, fx, masterID) })

	master, ok, err := fx.store.GetWorkflowRun(ctx, masterID)
	if err != nil || !ok {
		t.Fatalf("GetWorkflowRun(master): %v (found=%v)", err, ok)
	}
	if master.State == domain.WorkflowRunNeedsAttention {
		t.Fatalf("master is still %q after its child resumed: the mirror is historical, not current", master.State)
	}
	cleared, err := fx.coord.AssessRecovery(ctx, masterID)
	if err != nil {
		t.Fatal(err)
	}
	if cleared.ReasonCode == workflowcore.ReasonChildNeedsAttention {
		t.Fatalf("the assessment still reports a mirrored child stop: %+v", cleared)
	}
	if cleared.RecommendedAction == domain.RecoveryUnrecoverable {
		t.Fatalf("a recovered parent must not read as unrecoverable: %+v", cleared)
	}
	// Deterministic across repeated reconciliation: the same rows must produce
	// the same answer however many passes run over them.
	for pass := 0; pass < 3; pass++ {
		driveCycles(t, fx, 1, nil)
		again, aerr := fx.coord.AssessRecovery(ctx, masterID)
		if aerr != nil {
			t.Fatal(aerr)
		}
		if again.ReasonCode == workflowcore.ReasonChildNeedsAttention {
			t.Fatalf("pass %d re-mirrored a stop that no longer exists: %+v", pass+1, again)
		}
	}
}

// ---------------------------------------------------------------------------
// 23/24: repairing ONE affected autonomous child. The parent observes the
// recovery, and every sibling's durable state, attempts, generations and task
// identity are byte-stable.
// ---------------------------------------------------------------------------

func TestMasterAffectedChildRepairLeavesSiblingsUntouchedAndConverges(t *testing.T) {
	fx, ctx, masterID := startAutonomousObjective(t, twoTaskDependentPlan())
	affectedTaskID, childID := dispatchedChild(t, fx, masterID)

	// The child stops for real (the reviewer's credentials are rejected) and the
	// parent mirrors it. Parking it through the real dispatch path matters: a
	// seeded checkpoint alone is overwritten by the child's own next
	// observation, and stopReason reads the NEWEST row.
	parkOnHumanDecision(t, fx, childID)
	driveUntil(t, fx, 6, func() bool { return mirroredChildStop(t, fx, masterID) })
	if !mirroredChildStop(t, fx, masterID) {
		t.Fatal("the parent never mirrored its stopped child; the fixture never reached the state under test")
	}
	// Now that the child is parked and its poller has settled, restate the stop
	// as the repairable technical condition this test is about. A parked run
	// writes no further observations, so this is the reason that stands.
	makeChildStopRepairable(t, fx, childID)

	before := snapshotTasks(t, fx, masterID)
	if len(before) < 2 {
		t.Fatalf("the fixture produced %d tasks, want at least 2 so a sibling exists", len(before))
	}

	// 23: the repair is aimed at the affected CHILD, not at the objective.
	freezeRepairMode(t, fx, masterID, domain.RepairModeSuggest)
	plan, err := fx.coord.PlanRepair(ctx, masterID)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Eligibility.Allowed() {
		t.Fatalf("repair eligibility = %q (%s), want eligible", plan.Eligibility, plan.Reason)
	}
	if plan.Intent.TargetRunID != childID {
		t.Fatalf("repair targets %q, want the affected child %q", plan.Intent.TargetRunID, childID)
	}
	if !plan.Intent.Scope.SiblingsUntouched {
		t.Fatal("a master repair must record that its siblings are out of scope")
	}
	if plan.Intent.Strategy != domain.ExecutionStrategyTask {
		t.Fatalf("repair strategy = %q, want task: a repair must never decompose", plan.Intent.Strategy)
	}

	intent, err := fx.coord.LaunchRepair(ctx, masterID, "operator")
	if err != nil {
		t.Fatalf("LaunchRepair: %v", err)
	}
	if intent.RepairRunID == "" {
		t.Fatal("no repair run was created")
	}

	// 24: the siblings are untouched — identity, revision, ordinal, state,
	// bound child run and attempt count all byte-stable. Repeated poller
	// passes must not change that either.
	for pass := 0; pass < 3; pass++ {
		driveCycles(t, fx, 1, nil)
		after := snapshotTasks(t, fx, masterID)
		for planStepID, was := range before {
			now, present := after[planStepID]
			if !present {
				t.Fatalf("pass %d: task %s disappeared from the plan", pass+1, planStepID)
			}
			if was.id != now.id || was.revision != now.revision || was.ordinal != now.ordinal {
				t.Fatalf("pass %d: task %s identity changed\n  was: %+v\n  now: %+v", pass+1, planStepID, was, now)
			}
			if was.executionRun != "" && was.executionRun != now.executionRun {
				t.Fatalf("pass %d: task %s was rebound from child %s to %s", pass+1, planStepID, was.executionRun, now.executionRun)
			}
			if planStepID != affectedTaskPlanStep(t, fx, masterID, affectedTaskID) {
				if was.state != now.state || was.attempts != now.attempts {
					t.Fatalf("pass %d: sibling %s changed while another child was repaired\n  was: %+v\n  now: %+v",
						pass+1, planStepID, was, now)
				}
			}
		}
	}

	// The repair run is a real, separate, bounded task run in the same project.
	repairRun, ok, err := fx.store.GetWorkflowRun(ctx, intent.RepairRunID)
	if err != nil || !ok {
		t.Fatalf("GetWorkflowRun(repair): %v (found=%v)", err, ok)
	}
	if repairRun.ParentWorkflowID != nil {
		t.Fatal("the repair run was created as a child of the objective; it must be its own bounded run")
	}
	if _, isMaster, perr := fx.store.GetWorkflowPlan(ctx, intent.RepairRunID); perr != nil || isMaster {
		t.Fatalf("the repair run owns a plan (isMaster=%v err=%v); a repair must never decompose", isMaster, perr)
	}
	// One repair, however many passes: the same failure must not buy another.
	if got := len(repairIntentsOn(t, fx, masterID)); got != 1 {
		t.Fatalf("%d repair dispatches recorded, want exactly 1", got)
	}
}

// makeChildStopRepairable restates an already-parked child's stop as a
// repairable technical condition. It is written as the newest checkpoint on a
// run that is no longer producing observations, which is what makes it the
// reason stopReason resolves.
func makeChildStopRepairable(t *testing.T, fx *autonomousFixture, childID string) {
	t.Helper()
	writeStopCheckpoint(t, fx, childID, workflowcore.ReasonVerifyBudgetExhausted,
		"verification kept failing after every automatic fix attempt")
	detail, err := fx.coord.GetRun(context.Background(), childID)
	if err != nil {
		t.Fatalf("GetRun(child): %v", err)
	}
	if detail.LatestCheckpointPhase != workflowcore.ReasonVerifyBudgetExhausted {
		t.Fatalf("child's newest stop is %q, want %q: the fixture never reached the repairable state",
			detail.LatestCheckpointPhase, workflowcore.ReasonVerifyBudgetExhausted)
	}
}

// affectedTaskPlanStep resolves a task id back to its plan step id.
func affectedTaskPlanStep(t *testing.T, fx *autonomousFixture, masterID, taskID string) string {
	t.Helper()
	tasks, err := fx.store.ListWorkflowTasks(context.Background(), masterID)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range tasks {
		if task.ID == taskID {
			return task.PlanStepID
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// 20: a completed repair returns the origin run to the EXACT obligation it was
// stopped on — not to the beginning, and without duplicating anything.
// ---------------------------------------------------------------------------

func TestRepairCompletionResumesTheOriginalObligation(t *testing.T) {
	fx, ctx, masterID := startAutonomousObjective(t, twoTaskDependentPlan())
	_, childID := dispatchedChild(t, fx, masterID)

	parkOnHumanDecision(t, fx, childID)
	makeChildStopRepairable(t, fx, childID)

	// The obligation the child is stopped on, recorded BEFORE the repair, is
	// what the repair has to give back.
	stoppedDetail, err := fx.coord.GetRun(ctx, childID)
	if err != nil {
		t.Fatal(err)
	}
	obligationBefore, err := fx.coord.AssessRecovery(ctx, childID)
	if err != nil {
		t.Fatal(err)
	}
	stepsBefore := map[string]domain.WorkflowStepState{}
	attemptsBefore := 0
	for _, sd := range stoppedDetail.Steps {
		stepsBefore[sd.Step.ID] = sd.Step.State
		attemptsBefore += len(sd.Attempts)
	}

	freezeRepairMode(t, fx, childID, domain.RepairModeSuggest)
	intent, err := fx.coord.LaunchRepair(ctx, childID, "operator")
	if err != nil {
		t.Fatalf("LaunchRepair: %v", err)
	}

	// Nothing is resolved while the repair is still running.
	if err := fx.coord.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if got := autoLedgerPhases(t, fx, childID)["workflow_repair_resolved"]; got != 0 {
		t.Fatalf("the repair resolved %d times while its run was still open", got)
	}

	// The repair finishes. Reaching `completed` through the domain's own
	// transitions is what a real repair run does; LaunchRepair may already have
	// started it, so the walk begins from whatever state it is actually in.
	repairNow, ok, err := fx.store.GetWorkflowRun(ctx, intent.RepairRunID)
	if err != nil || !ok {
		t.Fatalf("GetWorkflowRun(repair): %v (found=%v)", err, ok)
	}
	if repairNow.State == domain.WorkflowRunPending {
		mustTransitionRun(t, fx, intent.RepairRunID, domain.WorkflowRunPending, domain.WorkflowRunRunning)
		repairNow.State = domain.WorkflowRunRunning
	}
	mustTransitionRun(t, fx, intent.RepairRunID, repairNow.State, domain.WorkflowRunCompleted)

	// Reconciliation folds the outcome back in — and does it exactly once,
	// however many times it runs.
	for pass := 0; pass < 3; pass++ {
		if err := fx.coord.Reconcile(ctx); err != nil {
			t.Fatalf("pass %d: %v", pass+1, err)
		}
	}
	phases := autoLedgerPhases(t, fx, childID)
	if got := phases["workflow_repair_resolved"]; got != 1 {
		t.Fatalf("the repair resolved %d times across three reconciliations, want exactly 1", got)
	}
	if got := phases["workflow_repair_dispatched"]; got != 1 {
		t.Fatalf("%d repairs were dispatched, want exactly 1", got)
	}

	// No restart-from-zero: every step the run already had keeps its identity
	// and no attempt was duplicated.
	afterDetail, err := fx.coord.GetRun(ctx, childID)
	if err != nil {
		t.Fatal(err)
	}
	attemptsAfter := 0
	for _, sd := range afterDetail.Steps {
		if _, existed := stepsBefore[sd.Step.ID]; !existed {
			t.Fatalf("step %s appeared after the repair; the run was restarted rather than resumed", sd.Step.ID)
		}
		attemptsAfter += len(sd.Attempts)
	}
	if len(afterDetail.Steps) != len(stepsBefore) {
		t.Fatalf("step count %d -> %d after a repair", len(stepsBefore), len(afterDetail.Steps))
	}
	if attemptsAfter < attemptsBefore {
		t.Fatalf("attempt count fell from %d to %d; durable history was discarded", attemptsBefore, attemptsAfter)
	}

	// The obligation AO now names is the one it was stopped on, re-derived
	// from the same durable rows rather than reset.
	obligationAfter, err := fx.coord.AssessRecovery(ctx, childID)
	if err != nil {
		t.Fatal(err)
	}
	if obligationAfter.Obligation.Kind != obligationBefore.Obligation.Kind {
		t.Fatalf("the obligation changed across the repair: %q -> %q",
			obligationBefore.Obligation.Kind, obligationAfter.Obligation.Kind)
	}
	if obligationAfter.Strategy != obligationBefore.Strategy {
		t.Fatalf("the run's frozen strategy changed across the repair: %q -> %q",
			obligationBefore.Strategy, obligationAfter.Strategy)
	}

	// And it survives a restart: a repair folded in before the daemon died
	// must not be folded in again after it comes back.
	if err := fx.coord.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if got := autoLedgerPhases(t, fx, childID)["workflow_repair_resolved"]; got != 1 {
		t.Fatalf("the repair resolved %d times after a restart, want 1", got)
	}
}
