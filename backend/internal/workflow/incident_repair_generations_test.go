package workflow_test

// incident_repair_generations_test.go — the repair half of wf-724a1e97.
//
// THE DURABLE SEQUENCE:
//
//	00:39  fix_budget_exhausted
//	00:43  workflow_repair_dispatched  generation 1  -> wf-913f54d8
//	01:03  wf-913f54d8 COMPLETED
//	01:04  workflow_repair_dispatched  generation 2  -> wf-f5025a7c   (25s later)
//	03:17  workflow_repair_resolved    generation 1  "superseded by generation 2
//	                                                  and will not resume anything"
//
// A repair that COMPLETED had its result thrown away by a repair that was
// launched only because nobody had folded the first one in yet. The semantics
// this pins:
//
//	original incident
//	  -> bounded repair generation
//	  -> resolved      => the origin's obligation is discharged
//	  -> unresolved    => the result returns to the ORIGINAL incident
//	  -> budget spent  => a person
//
// and never a tree.

import (
	"context"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// repairResolutionsFor folds the ledger's resolution rows into
// generation -> outcome, which is the fact the incident turned on.
func repairResolutionsFor(t *testing.T, store *crashStore, runID string) map[int]string {
	t.Helper()
	out := map[int]string{}
	checkpoints, err := store.ListWorkflowCheckpoints(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, cp := range checkpoints {
		if cp.DurablePhase != "workflow_repair_resolved" {
			continue
		}
		var body struct {
			Generation int    `json:"generation"`
			Outcome    string `json:"outcome"`
		}
		if jsonUnmarshalString(cp.RetryState, &body) == nil && body.Generation > 0 {
			out[body.Generation] = body.Outcome
		}
	}
	return out
}

// While generation N's repair run is still in flight, asking again returns the
// SAME generation. One failure buys one repair agent.
func TestIncidentRepair_TwoRequestsWhileOneIsInFlightAreOneGeneration(t *testing.T) {
	ctx := context.Background()
	store, reboot, runID := stoppedTaskRun(t, workflowcore.ReasonFixBudgetExhausted)

	first, err := reboot().LaunchRepair(ctx, runID, "operator")
	if err != nil {
		t.Fatalf("LaunchRepair: %v", err)
	}
	second, err := reboot().LaunchRepair(ctx, runID, "operator")
	if err != nil {
		t.Fatalf("LaunchRepair (second): %v", err)
	}

	if second.Generation != first.Generation || second.RepairRunID != first.RepairRunID {
		t.Fatalf("second request produced generation %d (%s), want the in-flight generation %d (%s)",
			second.Generation, second.RepairRunID, first.Generation, first.RepairRunID)
	}
	if launched := repairRunsFor(t, store, runID); len(launched) != 1 {
		t.Fatalf("repair runs launched = %d, want exactly 1", len(launched))
	}
}

// The incident's own ordering: generation 1 COMPLETES, and only then is another
// repair asked for. Its completion must be folded into the origin BEFORE the
// next generation exists, so it can never be recorded as "superseded ... and
// will not resume anything".
func TestIncidentRepair_ACompletedGenerationIsNeverDiscardedAsSuperseded(t *testing.T) {
	ctx := context.Background()
	store, reboot, runID := stoppedTaskRun(t, workflowcore.ReasonFixBudgetExhausted)

	first, err := reboot().LaunchRepair(ctx, runID, "operator")
	if err != nil {
		t.Fatalf("LaunchRepair: %v", err)
	}
	// wf-913f54d8 completed at 01:03:38.
	completeRunRow(t, store, first.RepairRunID)

	// 25 seconds later, generation 2 was asked for.
	if _, err := reboot().LaunchRepair(ctx, runID, "operator"); err != nil &&
		!strings.Contains(err.Error(), "repair") {
		t.Fatalf("LaunchRepair (generation 2): %v", err)
	}

	resolutions := repairResolutionsFor(t, store, runID)
	got, ok := resolutions[first.Generation]
	if !ok {
		t.Fatalf("generation %d was never resolved; resolutions = %v", first.Generation, resolutions)
	}
	if got != "completed" {
		t.Fatalf("generation %d resolved as %q, want %q: a repair that COMPLETED must discharge the obligation it was blocking, not be discarded",
			first.Generation, got, "completed")
	}
}

// A repair that ended without repairing hands the result back to the ORIGINAL
// incident — the origin is a person's again, and no descendant repair is
// created for the repair run itself.
func TestIncidentRepair_AnUnresolvedRepairReturnsToTheOriginalIncident(t *testing.T) {
	ctx := context.Background()
	store, reboot, runID := stoppedTaskRun(t, workflowcore.ReasonFixBudgetExhausted)

	intent, err := reboot().LaunchRepair(ctx, runID, "operator")
	if err != nil {
		t.Fatalf("LaunchRepair: %v", err)
	}
	if _, err := reboot().CancelRun(ctx, intent.RepairRunID); err != nil {
		t.Fatalf("CancelRun(repair): %v", err)
	}
	if err := reboot().Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	resolutions := repairResolutionsFor(t, store, runID)
	if got := resolutions[intent.Generation]; got != string(domain.WorkflowRunCancelled) {
		t.Fatalf("generation %d resolved as %q, want %q", intent.Generation, got, domain.WorkflowRunCancelled)
	}
	// And the repair run itself never becomes the parent of another repair.
	plan, err := reboot().PlanRepair(ctx, intent.RepairRunID)
	if err != nil {
		t.Fatalf("PlanRepair(repair run): %v", err)
	}
	if plan.Eligibility == domain.RepairEligible {
		t.Fatalf("the repair run is itself repairable: %+v", plan)
	}
}

// completeRunRow drives a run row to `completed` through legal transitions,
// which is what a repair run that actually repaired something leaves behind.
func completeRunRow(t *testing.T, store *crashStore, runID string) {
	t.Helper()
	ctx := context.Background()
	run, ok, err := store.GetWorkflowRun(ctx, runID)
	if err != nil || !ok {
		t.Fatalf("GetWorkflowRun(%s): %v (found=%v)", runID, err, ok)
	}
	for _, to := range []domain.WorkflowRunState{domain.WorkflowRunRunning, domain.WorkflowRunCompleted} {
		if run.State == to {
			continue
		}
		if !domain.ValidWorkflowRunTransition(run.State, to) {
			t.Fatalf("cannot move repair run %s from %q to %q", runID, run.State, to)
		}
		if _, err := store.UpdateWorkflowRunState(ctx, runID, run.State, to, run.UpdatedAt); err != nil {
			t.Fatalf("UpdateWorkflowRunState(%s -> %s): %v", run.State, to, err)
		}
		run.State = to
	}
}
