package workflow_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// p1b_recovery_test.go — P1-B against a real store: plan reuse, plan
// revisions, and the Repair Agent's eligibility, generation and bounds.

// parkRun stops a run on a canonical reason, exactly the way recordAttentionStop
// does: a checkpoint whose durable_phase IS the reason, and the run in
// needs_attention. Going through the store rather than a helper keeps the test
// honest about what the production readers actually read.
func parkRun(t *testing.T, store *crashStore, runID, projectID, reason string) {
	t.Helper()
	ctx := context.Background()
	if _, err := store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID: "wfc-park-" + reason + "-" + runID, WorkflowRunID: runID, ProjectID: projectID,
		// Deliberately NOT "human_attention": that exact literal is
		// resolveAttentionReason's legacy carrier for an exhausted fix budget,
		// and writing it here would classify every parked run as that.
		DurablePhase: reason, NextAction: "", PayloadVersion: "v1", RetryState: "{}",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed stop checkpoint: %v", err)
	}
	run, ok, err := store.GetWorkflowRun(ctx, runID)
	if err != nil || !ok {
		t.Fatalf("GetWorkflowRun(%s): ok=%v err=%v", runID, ok, err)
	}
	if run.State == domain.WorkflowRunNeedsAttention {
		return
	}
	if _, err := store.UpdateWorkflowRunState(ctx, runID, run.State, domain.WorkflowRunNeedsAttention, time.Now().UTC()); err != nil {
		t.Fatalf("park run %s: %v", runID, err)
	}
}

func setRepairMode(t *testing.T, store *crashStore, runID string, mode domain.RepairMode) {
	t.Helper()
	ctx := context.Background()
	run, ok, err := store.GetWorkflowRun(ctx, runID)
	if err != nil || !ok {
		t.Fatalf("GetWorkflowRun(%s): ok=%v err=%v", runID, ok, err)
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
	if ok, err := store.UpdateWorkflowRunPolicySnapshot(ctx, runID, string(snapshot), time.Now().UTC()); err != nil || !ok {
		t.Fatalf("set repair mode: ok=%v err=%v", ok, err)
	}
}

// approvedObjective drives a master objective through planning so it holds a
// real, approved, validated plan with real tasks.
func approvedObjective(t *testing.T) (*crashStore, func() *workflowcore.Coordinator, string) {
	t.Helper()
	ctx := context.Background()
	store, reboot := newCrashFixture(t, validMasterPlan())
	created, err := reboot().CreateObjectiveRunWithStrategy(ctx, "p", "Build users",
		domain.WorkflowPlanApprovalAuto, explicitStrategy(t, domain.ExecutionStrategyAutonomous))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reboot().GeneratePlan(ctx, created.Run.ID); err != nil {
		t.Fatal(err)
	}
	return store, reboot, created.Run.ID
}

// ---------------------------------------------------------------------------
// 7/8/10: a valid plan survives a restart, is reusable, and reuse preserves the
// durable task identities rather than minting new ones.
// ---------------------------------------------------------------------------

func TestValidPlanIsReusableAcrossRestartAndPreservesTaskIdentities(t *testing.T) {
	ctx := context.Background()
	store, reboot, runID := approvedObjective(t)

	before, err := store.ListWorkflowTasks(ctx, runID)
	if err != nil || len(before) == 0 {
		t.Fatalf("ListWorkflowTasks: %d tasks err=%v", len(before), err)
	}
	beforeIDs := map[string]int64{}
	for _, task := range before {
		beforeIDs[task.ID] = task.Ordinal
	}

	// A restart, then the assessment. Nothing about a restart may change what
	// the plan is or whether it can be reused.
	if err := reboot().Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	assessment, err := reboot().AssessRecovery(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if assessment.PlanReusable != domain.PlanReuseExact {
		t.Fatalf("plan reusability = %q, want exact", assessment.PlanReusable)
	}

	if _, reuse, err := reboot().ReusePlan(ctx, runID); err != nil {
		t.Fatalf("ReusePlan: %v (%s)", err, reuse.Reason)
	}

	after, err := store.ListWorkflowTasks(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("task count %d -> %d; reuse must not create or drop tasks", len(before), len(after))
	}
	for _, task := range after {
		ordinal, ok := beforeIDs[task.ID]
		if !ok {
			t.Fatalf("reuse minted a new task identity %s", task.ID)
		}
		if ordinal != task.Ordinal {
			t.Fatalf("task %s moved from ordinal %d to %d", task.ID, ordinal, task.Ordinal)
		}
	}
}

// 9/10: a plan whose stored bytes no longer match the hash it was approved
// under cannot be reused, and reuse REFUSES rather than executing it.
func TestPlanWithBrokenIdentityCannotBeReused(t *testing.T) {
	ctx := context.Background()
	store, reboot, runID := approvedObjective(t)

	// Rewrite the plan bytes without touching the hash: the row now claims an
	// identity its content does not have.
	plan, _, err := store.GetWorkflowPlan(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	var generated workflowcore.MasterPlan
	if err := json.Unmarshal([]byte(plan.GeneratedPlanJSON), &generated); err != nil {
		t.Fatal(err)
	}
	generated.Steps[0].Title = "Something else entirely"
	tampered, _ := json.Marshal(generated)
	store.mutatePlan = func(r domain.WorkflowPlanRecord) domain.WorkflowPlanRecord {
		r.GeneratedPlanJSON = string(tampered)
		return r
	}
	t.Cleanup(func() { store.mutatePlan = nil })

	assessment, err := reboot().AssessRecovery(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if assessment.PlanReusable != domain.PlanReuseNotReusable {
		t.Fatalf("reusability = %q, want not_reusable: the bytes no longer match the approved hash", assessment.PlanReusable)
	}
	if _, _, err := reboot().ReusePlan(ctx, runID); err == nil {
		t.Fatal("ReusePlan executed a plan whose identity AO cannot prove")
	}
}

// 9: a plan whose project context has moved is stale. It is NOT thrown away,
// and it does NOT silently run: an explicit decision is owed.
func TestStalePlanContextRequiresARevalidationDecision(t *testing.T) {
	ctx := context.Background()
	store, _, runID := approvedObjective(t)

	// Move the recorded manifest away from what the builder produces today:
	// the plan is real and validated, and the project it was planned against
	// is not the project as it now stands.
	store.mutatePlan = func(r domain.WorkflowPlanRecord) domain.WorkflowPlanRecord {
		r.ContextManifestJSON = `{"version":"v0","projectId":"p","projectPath":"/somewhere/else","documents":[]}`
		return r
	}
	t.Cleanup(func() { store.mutatePlan = nil })
	c := workflowcore.New(workflowcore.Deps{Store: store, Projects: store,
		Planner: &staticPlanner{plan: validMasterPlan()}, PlannerContextBuilder: staticContext{}})

	assessment, err := c.AssessRecovery(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if assessment.PlanReusable != domain.PlanReuseStaleRevalidatable {
		t.Fatalf("reusability = %q, want stale_but_revalidatable", assessment.PlanReusable)
	}
	if _, _, err := c.ReusePlan(ctx, runID); err == nil {
		t.Fatal("a stale plan executed silently, which is exactly what plan reuse exists to prevent")
	}
}

// ---------------------------------------------------------------------------
// 11/12/13: regeneration mints a new durable revision; the superseded
// revision's tasks stop being authoritative but are retained; and repeated
// regeneration is bounded.
// ---------------------------------------------------------------------------

func TestRegenerationMintsANewRevisionAndRetiresTheOldTasks(t *testing.T) {
	ctx := context.Background()
	store, reboot, runID := approvedObjective(t)

	oldTasks, err := store.ListWorkflowTasks(ctx, runID)
	if err != nil || len(oldTasks) == 0 {
		t.Fatalf("ListWorkflowTasks: %d err=%v", len(oldTasks), err)
	}
	oldIDs := map[string]struct{}{}
	for _, task := range oldTasks {
		oldIDs[task.ID] = struct{}{}
	}

	if _, assessment, err := reboot().RegeneratePlan(ctx, runID); err != nil {
		t.Fatalf("RegeneratePlan: %v (%s)", err, assessment.Reason)
	}
	plan, _, err := store.GetWorkflowPlan(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Revision != 2 {
		t.Fatalf("plan revision = %d, want 2", plan.Revision)
	}
	if plan.Status != domain.WorkflowPlanPending {
		t.Fatalf("regenerated plan status = %q, want pending", plan.Status)
	}
	// 12: the superseded revision's tasks are invisible to every execution
	// reader, so a child bound to one can never advance this objective again.
	current, err := store.ListWorkflowTasks(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(current) != 0 {
		t.Fatalf("the superseded revision's %d tasks are still authoritative", len(current))
	}
	// ...and they are retained, not deleted.
	retained, err := store.ListWorkflowTasksAtRevision(ctx, runID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(retained) != len(oldTasks) {
		t.Fatalf("revision 1 retained %d of %d tasks; the audit trail was destroyed", len(retained), len(oldTasks))
	}

	// The new revision plans into fresh identities that cannot collide with
	// the retired ones.
	if _, err := reboot().GeneratePlan(ctx, runID); err != nil {
		t.Fatal(err)
	}
	fresh, err := store.ListWorkflowTasks(ctx, runID)
	if err != nil || len(fresh) == 0 {
		t.Fatalf("revision 2 produced %d tasks err=%v", len(fresh), err)
	}
	for _, task := range fresh {
		if _, collided := oldIDs[task.ID]; collided {
			t.Fatalf("revision 2 task %s reused a retired identity", task.ID)
		}
		if task.PlanRevision != 2 {
			t.Fatalf("task %s carries revision %d, want 2", task.ID, task.PlanRevision)
		}
	}
}

// 13: a second regeneration from a stale view is refused rather than minting a
// second revision, and the total is bounded.
func TestRepeatedRegenerationIsBoundedAndIdempotent(t *testing.T) {
	ctx := context.Background()
	store, reboot, runID := approvedObjective(t)

	// Two callers reading the same revision: one wins, one is refused.
	first := reboot()
	second := reboot()
	staleDetail, err := second.GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	_ = staleDetail
	if _, _, err := first.RegeneratePlan(ctx, runID); err != nil {
		t.Fatalf("first regeneration: %v", err)
	}
	// The second caller's view is now stale: the plan is `pending` at revision
	// 2, which has nothing to regenerate. It must refuse, not mint revision 3.
	if _, _, err := second.RegeneratePlan(ctx, runID); err == nil {
		t.Fatal("a second regeneration from a stale view minted another revision")
	}
	plan, _, err := store.GetWorkflowPlan(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Revision != 2 {
		t.Fatalf("plan revision = %d after two requests, want 2", plan.Revision)
	}

	// And the bound holds. Plan and regenerate until AO refuses; it must refuse
	// before an objective can be decomposed without limit, and it must say why.
	var refusal error
	for attempt := 0; attempt < 6 && refusal == nil; attempt++ {
		if _, err := reboot().GeneratePlan(ctx, runID); err != nil {
			t.Fatalf("GeneratePlan on attempt %d: %v", attempt, err)
		}
		_, _, refusal = reboot().RegeneratePlan(ctx, runID)
	}
	if refusal == nil {
		t.Fatal("regeneration never refused; an objective can be re-planned without bound")
	}
	if !strings.Contains(refusal.Error(), "already been planned") {
		t.Fatalf("refusal = %v, want the bound's own explanation", refusal)
	}
	final, _, err := store.GetWorkflowPlan(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Revision > 3 {
		t.Fatalf("plan reached revision %d, past the bound", final.Revision)
	}
}

// ---------------------------------------------------------------------------
// 14/15/16/17/18/21/22/26: the Repair Agent.
// ---------------------------------------------------------------------------

func stoppedTaskRun(t *testing.T, reason string) (*crashStore, func() *workflowcore.Coordinator, string) {
	t.Helper()
	ctx := context.Background()
	store, reboot := newCrashFixture(t, validMasterPlan())
	created, err := reboot().CreateTaskRun(ctx, workflowcore.TaskRunRequest{
		ProjectID: "p", Objective: "Add the feature", Strategy: explicitStrategy(t, domain.ExecutionStrategyTask),
	})
	if err != nil {
		t.Fatal(err)
	}
	parkRun(t, store, created.Run.ID, "p", reason)
	return store, reboot, created.Run.ID
}

// 14: a repairable technical failure exposes the repair action.
func TestRepairableFailureExposesTheRepairAction(t *testing.T) {
	ctx := context.Background()
	_, reboot, runID := stoppedTaskRun(t, workflowcore.ReasonVerifyBudgetExhausted)

	assessment, err := reboot().AssessRecovery(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if !assessment.RepairAvailable || assessment.RepairEligibility != domain.RepairEligible {
		t.Fatalf("assessment = %+v, want an available repair", assessment)
	}
	if assessment.RecommendedAction != domain.RecoveryRepair {
		t.Fatalf("recommended action = %q, want repair", assessment.RecommendedAction)
	}
	// The default policy only SUGGESTS: AO must not claim it may do this alone.
	if assessment.AutomaticAllowed {
		t.Fatal("the default (suggest) repair policy must not report the repair as automatic")
	}
	plan, err := reboot().PlanRepair(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Intent.Strategy != domain.ExecutionStrategyTask {
		t.Fatalf("repair strategy = %q, want task: a repair is bounded work", plan.Intent.Strategy)
	}
	if len(plan.Intent.AcceptanceCriteria) == 0 || plan.Intent.EvidenceDigest == "" {
		t.Fatalf("repair intent is missing its criteria or evidence: %+v", plan.Intent)
	}
}

// 15: a provenance failure never exposes it, whatever the policy says.
func TestUnprovableProvenanceFailureNeverExposesRepair(t *testing.T) {
	ctx := context.Background()
	for _, reason := range []string{
		workflowcore.ReasonVerifyApprovedHeadUnprovable,
		workflowcore.ReasonFixGenerationUnprovable,
		workflowcore.ReasonReviewStateAmbiguous,
	} {
		t.Run(reason, func(t *testing.T) {
			store, reboot, runID := stoppedTaskRun(t, reason)
			// Even with the most permissive policy this run could carry.
			setRepairMode(t, store, runID, domain.RepairModeAutomatic)

			assessment, err := reboot().AssessRecovery(ctx, runID)
			if err != nil {
				t.Fatal(err)
			}
			if assessment.RepairAvailable || assessment.RepairEligibility != domain.RepairIneligible {
				t.Fatalf("%s: assessment = %+v, want no repair", reason, assessment)
			}
			if assessment.RecommendedAction == domain.RecoveryRepair {
				t.Fatalf("%s: AO recommended repairing an unprovable-provenance stop", reason)
			}
			if _, err := reboot().LaunchRepair(ctx, runID, "operator"); err == nil {
				t.Fatalf("%s: a repair launched for a condition it must never touch", reason)
			}
		})
	}
}

// 27: a stop AO cannot name fails closed -- no repair, no recommendation it
// cannot justify, and no error either.
func TestUnclassifiedStopFailsClosed(t *testing.T) {
	ctx := context.Background()
	store, reboot, runID := stoppedTaskRun(t, "something_nobody_registered")
	setRepairMode(t, store, runID, domain.RepairModeAutomatic)

	assessment, err := reboot().AssessRecovery(ctx, runID)
	if err != nil {
		t.Fatalf("an unclassifiable stop must be an answer, not an error: %v", err)
	}
	if assessment.RecommendedAction != domain.RecoveryUnrecoverable {
		t.Fatalf("recommended action = %q, want unrecoverable", assessment.RecommendedAction)
	}
	if assessment.RepairEligibility != domain.RepairUnknownCondition || assessment.RepairAvailable {
		t.Fatalf("an unnamed stop offered a repair: %+v", assessment)
	}
	if assessment.BlockingCondition == "" || assessment.Explanation == "" {
		t.Fatal("a fail-closed answer must still say what is missing")
	}
}

// 16/17/18: the three policy states, each doing exactly what it says.
func TestRepairPolicyStatesGovernAutomaticLaunch(t *testing.T) {
	ctx := context.Background()
	for _, tt := range []struct {
		mode        domain.RepairMode
		wantLaunch  bool
		description string
	}{
		{domain.RepairModeDisabled, false, "disabled never launches"},
		{domain.RepairModeSuggest, false, "suggest waits for an operator"},
		{domain.RepairModeAutomatic, true, "automatic launches exactly one"},
	} {
		t.Run(string(tt.mode), func(t *testing.T) {
			store, reboot, runID := stoppedTaskRun(t, workflowcore.ReasonVerifyBudgetExhausted)
			setRepairMode(t, store, runID, tt.mode)

			// Three reconciliation passes: whatever the policy does, it must do
			// it once. (26: repeated reconcile creates no duplicate repair.)
			for pass := 1; pass <= 3; pass++ {
				if err := reboot().Reconcile(ctx); err != nil {
					t.Fatalf("pass %d: %v", pass, err)
				}
			}
			launched := repairRunsFor(t, store, runID)
			switch {
			case tt.wantLaunch && len(launched) != 1:
				t.Fatalf("%s: %d repairs launched, want exactly 1", tt.description, len(launched))
			case !tt.wantLaunch && len(launched) != 0:
				t.Fatalf("%s: %d repairs launched, want 0", tt.description, len(launched))
			}
		})
	}
}

// 21: the budget is spent, and the escalation is a durable fact rather than an
// absence.
func TestRepairExhaustionEscalatesToAHuman(t *testing.T) {
	ctx := context.Background()
	store, reboot, runID := stoppedTaskRun(t, workflowcore.ReasonVerifyBudgetExhausted)
	setRepairMode(t, store, runID, domain.RepairModeAutomatic)

	// Drive past the budget. Each repair must actually END before the next one
	// can be spent -- while one is still running it IS this failure's repair,
	// and asking again returns it rather than buying another (§F). Cancelling
	// it is the honest way to reach the next generation, and it is what a
	// repair that failed to fix anything looks like from here.
	for spent := 0; spent < domain.DefaultMaxRepairCycles+2; spent++ {
		intent, err := reboot().LaunchRepair(ctx, runID, "operator")
		if err != nil {
			break
		}
		if _, cerr := reboot().CancelRun(ctx, intent.RepairRunID); cerr != nil {
			t.Fatalf("end repair %d: %v", intent.Generation, cerr)
		}
	}
	launched := repairRunsFor(t, store, runID)
	if len(launched) > domain.DefaultMaxRepairCycles {
		t.Fatalf("%d repairs launched, want at most the %d-cycle budget", len(launched), domain.DefaultMaxRepairCycles)
	}

	// The escalation is a durable fact on the ledger, not the absence of one,
	// and it becomes the run's own reason -- with a remedy a person can act on.
	escalated := false
	checkpoints, err := store.ListWorkflowCheckpoints(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, cp := range checkpoints {
		if cp.DurablePhase == "workflow_repair_escalated" {
			escalated = true
		}
	}
	if !escalated {
		t.Fatal("the repair budget was spent with nothing durable saying so")
	}
	assessment, err := reboot().AssessRecovery(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if assessment.RepairAvailable {
		t.Fatalf("a repair is still offered after escalation: %+v", assessment)
	}
	if assessment.Explanation == "" || assessment.RecommendedAction == domain.RecoveryRepair {
		t.Fatalf("escalation must hand the run to a person with something to do: %+v", assessment)
	}
}

// 19: a stale repair generation drives nothing.
func TestStaleRepairGenerationIsRefused(t *testing.T) {
	ctx := context.Background()
	store, reboot, runID := stoppedTaskRun(t, workflowcore.ReasonVerifyBudgetExhausted)
	setRepairMode(t, store, runID, domain.RepairModeAutomatic)

	first, err := reboot().LaunchRepair(ctx, runID, "operator")
	if err != nil {
		t.Fatalf("first repair: %v", err)
	}
	if first.Generation != 1 {
		t.Fatalf("first repair generation = %d, want 1", first.Generation)
	}
	// The SAME failure asked for again is the same repair, not a second one.
	again, err := reboot().LaunchRepair(ctx, runID, "operator")
	if err != nil {
		t.Fatalf("repeat repair: %v", err)
	}
	if again.RepairRunID != first.RepairRunID {
		t.Fatalf("the same failure produced two repair runs (%s, %s)", first.RepairRunID, again.RepairRunID)
	}
	if got := len(repairRunsFor(t, store, runID)); got != 1 {
		t.Fatalf("%d repair dispatches recorded for one failure, want 1", got)
	}
}

// 22/29: a repair of a TASK run stays a bounded task, and neither the origin
// run's strategy nor the repair's is anything else.
func TestTaskRepairStaysBoundedAndPreservesStrategy(t *testing.T) {
	ctx := context.Background()
	store, reboot, runID := stoppedTaskRun(t, workflowcore.ReasonFixBudgetExhausted)
	setRepairMode(t, store, runID, domain.RepairModeAutomatic)

	intent, err := reboot().LaunchRepair(ctx, runID, "operator")
	if err != nil {
		t.Fatalf("LaunchRepair: %v", err)
	}
	if intent.Strategy != domain.ExecutionStrategyTask {
		t.Fatalf("repair intent strategy = %q, want task", intent.Strategy)
	}
	repairStrategy := strategyOf(t, store, intent.RepairRunID)
	if repairStrategy.Effective != domain.ExecutionStrategyTask {
		t.Fatalf("repair run strategy = %q, want task: a repair must never decompose", repairStrategy.Effective)
	}
	// The origin run's own frozen strategy is untouched by having been
	// repaired, across restarts.
	for pass := 1; pass <= 2; pass++ {
		if err := reboot().Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		if got := strategyOf(t, store, runID); got.Effective != domain.ExecutionStrategyTask {
			t.Fatalf("pass %d: origin strategy became %q", pass, got.Effective)
		}
		if got := strategyOf(t, store, intent.RepairRunID); got.Effective != domain.ExecutionStrategyTask {
			t.Fatalf("pass %d: repair strategy became %q", pass, got.Effective)
		}
	}
	// The repair run has no plan row: it did not decompose anything.
	if _, isMaster, err := store.GetWorkflowPlan(ctx, intent.RepairRunID); err != nil || isMaster {
		t.Fatalf("the repair run owns a master plan (isMaster=%v err=%v)", isMaster, err)
	}
}

// repairRunsFor reads the repair dispatches recorded on a run's ledger.
func repairRunsFor(t *testing.T, store *crashStore, runID string) []string {
	t.Helper()
	checkpoints, err := store.ListWorkflowCheckpoints(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, cp := range checkpoints {
		if cp.DurablePhase != "workflow_repair_dispatched" {
			continue
		}
		var intent domain.RepairIntent
		if json.Unmarshal([]byte(cp.RetryState), &intent) == nil && intent.RepairRunID != "" {
			out = append(out, intent.RepairRunID)
		}
	}
	return out
}
