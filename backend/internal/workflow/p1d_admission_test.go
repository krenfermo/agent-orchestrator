package workflow_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/branchlock"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// p1d_admission_test.go — P1-D §C/§D and test matrix S14–S25.
//
// What is under test is not that a blocked run stops. It is that AO says WHICH
// authority stopped it. Before this gate, a run queued behind another run's
// branch and a run whose machine was full both surfaced as "waiting", and an
// operator had no way to tell a situation that resolves itself in seconds from
// one that never resolves at all.
//
// The other half — waiting costs nothing — is asserted over the whole
// vocabulary rather than per reason, because "no wait spends retry budget" is
// the property, and a test per reason would keep passing if a seventh reason
// were added that did.

func admissionFixture(t *testing.T, mode domain.ExecutionMode) *placementFixture {
	t.Helper()
	return newPlacementFixtureWithMode(t, mode)
}

// admissionFixtureWithCapacity rebuilds the fixture's coordinator with the real
// capacity scheduler wired under the given bounds, over the SAME durable store.
func (f *placementFixture) withCapacity(t *testing.T, limits domain.CapacityLimits) *workflowcore.Coordinator {
	t.Helper()
	coord := workflowcore.New(workflowcore.Deps{
		Store: f.store, Projects: f.store,
		Placements: f.store, ProviderAttempts: f.store, TaskWorktreeRecords: f.store,
		BranchLocks:    cessionBranchLocks{mgr: f.locks},
		Capacity:       f.store,
		CapacityLimits: limits,
		InstanceToken:  "daemon-p1d",
	})
	f.coord = coord
	return coord
}

// S14: the machine has room, but another run owns the branch. That is
// branch_wait, and it must not be reported as a capacity problem.
func TestAdmissionReportsBranchWaitWhenCapacityIsFree(t *testing.T) {
	f := admissionFixture(t, domain.ExecutionDirectBranch)
	ctx := context.Background()
	f.withCapacity(t, domain.CapacityLimits{})

	// A different run holds the repository+branch this one needs.
	blocker := seedCompetingRunHoldingTheBranch(t, f)
	if blocker == "" {
		t.Fatal("setup: no competing holder")
	}

	decision, err := f.coord.AdmitForTest(ctx, f.run, f.step, domain.HarnessCodex, false, true, true)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if decision.Admitted {
		t.Fatal("a run was admitted while another run held its branch")
	}
	if decision.Reason != domain.AdmissionBranchWait {
		t.Fatalf("reason = %q, want branch_wait", decision.Reason)
	}
	// And it did NOT take a capacity claim on the way to being refused: a
	// launch that a branch was going to refuse must not occupy a slot no other
	// run can use.
	assertNoOutstandingCapacityClaims(t, f, f.run.ID)
}

// S15: the branch is safe and the machine is full. That is capacity_wait.
func TestAdmissionReportsCapacityWaitWhenTheBranchIsSafe(t *testing.T) {
	f := admissionFixture(t, domain.ExecutionDirectBranch)
	ctx := context.Background()
	// A generous per-workflow bound, so what refuses this run is the MACHINE
	// being full rather than the fairness rule -- the two are different
	// refusals and this test is about the first.
	limits := domain.CapacityLimits{
		Global: 4, PerWorkflow: 4,
		PerKind: map[domain.ExecutionKind]int{domain.ExecutionKindWorker: 4},
	}.Normalize()
	f.withCapacity(t, limits)
	fillEveryWorkerSlot(t, f, limits)

	decision, err := f.coord.AdmitForTest(ctx, f.run, f.step, domain.HarnessCodex, false, true, true)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if decision.Admitted {
		t.Fatal("a run was admitted onto a full machine")
	}
	if decision.Reason != domain.AdmissionCapacityWait {
		t.Fatalf("reason = %q, want capacity_wait", decision.Reason)
	}
	// The branch WAS acquired on the way through — it is legitimately this
	// run's now — which is exactly why the reason must name capacity and not
	// the branch.
	if len(decision.BranchAuthority) == 0 {
		t.Fatal("the decision records no branch authority, so it cannot claim the branch was safe")
	}
}

// S16: the placement is frozen but not launchable. That is placement_wait, and
// it is a different thing from branch_wait even though both concern the
// checkout.
func TestAdmissionReportsPlacementWaitWhenThePlacementIsNotReady(t *testing.T) {
	f := admissionFixture(t, domain.ExecutionIsolatedWorktree)
	ctx := context.Background()
	f.withCapacity(t, domain.CapacityLimits{})

	placement, _, err := f.coord.EnsureExecutionPlacement(ctx, f.run, f.step)
	if err != nil {
		t.Fatal(err)
	}
	// Another pass is materialising it right now. A second launch into a
	// placement being prepared is the duplicate the state exists to prevent.
	moved, err := f.store.TransitionExecutionPlacement(ctx, f.run.ID, "", "",
		placement.PlacementGeneration, domain.PlacementSelected, domain.PlacementPreparing,
		"", "another pass is creating the checkout", time.Now().UTC())
	if err != nil || !moved {
		t.Fatalf("transition to preparing: moved=%v err=%v", moved, err)
	}

	decision, err := f.coord.AdmitForTest(ctx, f.run, f.step, domain.HarnessCodex, false, true, true)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if decision.Admitted {
		t.Fatal("a run was admitted into a placement that is still being prepared")
	}
	if decision.Reason != domain.AdmissionPlacementWait {
		t.Fatalf("reason = %q, want placement_wait", decision.Reason)
	}
	if decision.PlacementState != domain.PlacementPreparing {
		t.Fatalf("the decision reports placement state %q, want preparing", decision.PlacementState)
	}
	assertNoOutstandingCapacityClaims(t, f, f.run.ID)
}

// S17: routing has no usable provider. That is provider_wait, reported from
// routing's own verdict rather than from a second opinion formed here.
func TestAdmissionReportsProviderWait(t *testing.T) {
	f := admissionFixture(t, domain.ExecutionIsolatedWorktree)
	ctx := context.Background()
	f.withCapacity(t, domain.CapacityLimits{})

	decision, err := f.coord.AdmitForTest(ctx, f.run, f.step, "", true, true, true)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if decision.Reason != domain.AdmissionProviderWait {
		t.Fatalf("reason = %q, want provider_wait", decision.Reason)
	}
	// Provider eligibility is checked before the placement is frozen, so a run
	// with no provider does not leave a placement behind for a launch that was
	// never going to happen.
	all, lerr := f.store.ListExecutionPlacementsForRun(ctx, f.run.ID)
	if lerr != nil {
		t.Fatal(lerr)
	}
	if len(all) != 0 {
		t.Fatalf("a provider wait froze a placement: %+v", all)
	}
}

// S18: dependencies are the task graph's question; admission classifies the
// answer rather than re-deriving it.
func TestAdmissionReportsDependencyWait(t *testing.T) {
	f := admissionFixture(t, domain.ExecutionIsolatedWorktree)
	ctx := context.Background()
	f.withCapacity(t, domain.CapacityLimits{})

	decision, err := f.coord.AdmitForTest(ctx, f.run, f.step, domain.HarnessCodex, false, false, true)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if decision.Reason != domain.AdmissionDependencyWait {
		t.Fatalf("reason = %q, want dependency_wait", decision.Reason)
	}
}

// S19/S20: waiting is free. Asserted over the WHOLE vocabulary, because the
// property is "no admission wait spends retry budget or holds capacity" and a
// test per reason would keep passing if a new reason were added that did.
func TestNoAdmissionWaitSpendsRetryBudgetOrHoldsCapacity(t *testing.T) {
	for _, reason := range []domain.AdmissionWaitReason{
		domain.AdmissionCapacityWait,
		domain.AdmissionBranchWait,
		domain.AdmissionPlacementWait,
		domain.AdmissionProviderWait,
		domain.AdmissionDependencyWait,
		domain.AdmissionLifecycleSuperseded,
		domain.AdmissionStrategyRefused,
	} {
		if !reason.IsKnown() {
			t.Fatalf("%q is not a known admission reason", reason)
		}
		if reason.SpendsRetryBudget() {
			t.Fatalf("%q spends retry budget; no wait may", reason)
		}
		if reason.ConsumesCapacity() {
			t.Fatalf("%q holds a runtime slot while waiting; no wait may", reason)
		}
	}
	// The generic reason is the one thing the vocabulary must not have a use
	// for: a refusal always names an authority.
	if domain.AdmissionWaitNone != "" {
		t.Fatal("AdmissionWaitNone must be the zero value so a refusal cannot accidentally carry it")
	}
}

// S19/S20 (behavioural half): a real refusal writes no attempt, so the run's
// retry budget is genuinely untouched rather than merely declared so.
func TestAdmissionRefusalRecordsNoAttempt(t *testing.T) {
	f := admissionFixture(t, domain.ExecutionIsolatedWorktree)
	ctx := context.Background()
	f.withCapacity(t, domain.CapacityLimits{})

	before := attemptCountFor(t, f, f.step.ID)
	for i := 0; i < 3; i++ {
		if _, err := f.coord.AdmitForTest(ctx, f.run, f.step, domain.HarnessCodex, false, false, true); err != nil {
			t.Fatalf("Admit: %v", err)
		}
	}
	if after := attemptCountFor(t, f, f.step.ID); after != before {
		t.Fatalf("three refusals produced %d new attempts; waiting must be free", after-before)
	}
}

// S21: a refusal is durably classified, and a restart reads back the SAME
// classification rather than re-deriving one against a world that has moved.
func TestRestartPreservesTheWaitingClassification(t *testing.T) {
	f := admissionFixture(t, domain.ExecutionIsolatedWorktree)
	ctx := context.Background()
	f.withCapacity(t, domain.CapacityLimits{})

	placement, _, err := f.coord.EnsureExecutionPlacement(ctx, f.run, f.step)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.TransitionExecutionPlacement(ctx, f.run.ID, "", "",
		placement.PlacementGeneration, domain.PlacementSelected, domain.PlacementPreparing,
		"", "being prepared", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	decision, err := f.coord.AdmitForTest(ctx, f.run, f.step, domain.HarnessCodex, false, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.coord.RecordAdmissionWaitForTest(ctx, f.run, f.step, decision); err != nil {
		t.Fatalf("record wait: %v", err)
	}

	rebooted := workflowcore.New(workflowcore.Deps{
		Store: f.store, Projects: f.store,
		Placements: f.store, ProviderAttempts: f.store, Capacity: f.store,
		InstanceToken: "daemon-after-restart",
	})
	state, err := rebooted.AdmissionState(ctx, f.run.ID)
	if err != nil {
		t.Fatalf("AdmissionState after restart: %v", err)
	}
	if state.WaitingReason != domain.AdmissionPlacementWait {
		t.Fatalf("after restart the waiting reason is %q, want placement_wait", state.WaitingReason)
	}
	if state.SpendsRetryBudget {
		t.Fatal("a restored wait claims to spend retry budget")
	}
}

// S22: reconciling the same refusal repeatedly converges. One claim, one lock,
// one placement, however many passes re-derive the same intent.
func TestRepeatedAdmissionCreatesNoDuplicateAuthority(t *testing.T) {
	f := admissionFixture(t, domain.ExecutionDirectBranch)
	ctx := context.Background()
	f.withCapacity(t, domain.CapacityLimits{})

	for i := 0; i < 5; i++ {
		if _, err := f.coord.AdmitForTest(ctx, f.run, f.step, domain.HarnessCodex, false, true, true); err != nil {
			t.Fatalf("Admit pass %d: %v", i, err)
		}
	}
	placements, err := f.store.ListExecutionPlacementsForRun(ctx, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(placements) != 1 {
		t.Fatalf("five admissions produced %d placements; one obligation has one placement", len(placements))
	}
	locks, err := f.store.ListHeldBranchLocksByRun(ctx, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(locks) > 1 {
		t.Fatalf("five admissions produced %d branch locks for one run", len(locks))
	}
	claims, err := f.store.ListCapacityClaimsForRun(ctx, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 {
		t.Fatalf("five admissions produced %d capacity claims; one launch intent has one claim", len(claims))
	}
	attempts, err := f.store.ListProviderAttemptsForRun(ctx, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 {
		t.Fatalf("five admissions produced %d provider attempts; a repeated pass is not a new attempt", len(attempts))
	}
}

// S23: two isolated siblings are physically independent, so both may hold a
// slot at once — the scheduler's bound is the only thing limiting them.
func TestIsolatedSiblingsAdmitConcurrentlyWithinSchedulerCapacity(t *testing.T) {
	f := admissionFixture(t, domain.ExecutionIsolatedWorktree)
	ctx := context.Background()
	f.withCapacity(t, domain.CapacityLimits{})

	sibling := newSiblingRun(t, f, "the second isolated task")
	first, err := f.coord.AdmitForTest(ctx, f.run, f.step, domain.HarnessCodex, false, true, true)
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.coord.AdmitForTest(ctx, sibling.run, sibling.step, domain.HarnessCodex, false, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Admitted || !second.Admitted {
		t.Fatalf("two isolated siblings did not both admit: %+v / %+v", first, second)
	}
	if first.PlacementGeneration == 0 || second.PlacementGeneration == 0 {
		t.Fatal("an admitted launch carries no placement generation")
	}
	// Different obligations, different placements, different worktree branches.
	if first.ProviderAttemptID == second.ProviderAttemptID {
		t.Fatal("two siblings share one provider attempt")
	}
}

// S24: two DIRECT-BRANCH siblings target the same repository and branch, so
// they serialize — and the second one's refusal names the branch.
func TestDirectBranchSiblingsSerializeOnTheBranch(t *testing.T) {
	f := admissionFixture(t, domain.ExecutionDirectBranch)
	ctx := context.Background()
	f.withCapacity(t, domain.CapacityLimits{})

	sibling := newSiblingRun(t, f, "the second direct-branch task")
	first, err := f.coord.AdmitForTest(ctx, f.run, f.step, domain.HarnessCodex, false, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Admitted {
		t.Fatalf("the first direct-branch sibling was refused: %+v", first)
	}
	second, err := f.coord.AdmitForTest(ctx, sibling.run, sibling.step, domain.HarnessCodex, false, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if second.Admitted {
		t.Fatal("two direct-branch siblings were both admitted to write one branch")
	}
	if second.Reason != domain.AdmissionBranchWait {
		t.Fatalf("the serialized sibling reports %q, want branch_wait", second.Reason)
	}
}

// S25: one workflow cannot monopolize the machine. The per-workflow bound is
// enforced inside the granting statement, so a run with many eligible steps
// still leaves room for another workflow to reach the front of the queue.
func TestOneWorkflowCannotMonopolizeCapacity(t *testing.T) {
	f := admissionFixture(t, domain.ExecutionIsolatedWorktree)
	ctx := context.Background()
	limits := domain.CapacityLimits{
		Global: 8, PerWorkflow: 1,
		PerKind: map[domain.ExecutionKind]int{domain.ExecutionKindWorker: 8},
	}.Normalize()
	f.withCapacity(t, limits)

	if _, err := f.coord.AdmitForTest(ctx, f.run, f.step, domain.HarnessCodex, false, true, true); err != nil {
		t.Fatal(err)
	}
	// A second launch intent on the SAME workflow, under a per-workflow bound
	// of one.
	extra := secondLaunchIntentOf(t, f, f.run.ID, f.step.ID)
	second, err := f.coord.AdmitForTest(ctx, f.run, extra, domain.HarnessCodex, false, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if second.Admitted {
		t.Fatal("one workflow took more slots than its per-workflow bound allows")
	}
	if second.Reason != domain.AdmissionCapacityWait {
		t.Fatalf("the fairness refusal reports %q, want capacity_wait", second.Reason)
	}
	// And the machine is genuinely not full — another workflow still gets in.
	other := newSiblingRun(t, f, "a different workflow")
	third, err := f.coord.AdmitForTest(ctx, other.run, other.step, domain.HarnessCodex, false, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if !third.Admitted {
		t.Fatalf("the per-workflow bound blocked an unrelated workflow too: %+v", third)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func newSiblingRun(t *testing.T, f *placementFixture, objective string) *placementFixture {
	t.Helper()
	created, err := f.coord.CreateTaskRun(context.Background(), workflowcoreTaskRequest(t, objective))
	if err != nil {
		t.Fatal(err)
	}
	return &placementFixture{
		store: f.store, coord: f.coord, locks: f.locks,
		run: created.Run, step: workStepOf(t, f.store, created.Run.ID),
	}
}

// secondLaunchIntentOf returns another of the run's own steps, so a test can
// present ONE workflow with two launch intents. Which step it is does not
// matter to the fairness bound -- that bound counts held claims per
// workflow_run_id, not per step kind -- and using a step the run genuinely has
// keeps the claim's dispatch key the one production would mint.
func secondLaunchIntentOf(t *testing.T, f *placementFixture, runID string, exclude string) domain.WorkflowStep {
	t.Helper()
	steps, err := f.store.ListWorkflowSteps(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		if s.ID != exclude {
			return s
		}
	}
	t.Fatalf("run %s has only one step, so it cannot present two launch intents", runID)
	return domain.WorkflowStep{}
}

// seedCompetingRunHoldingTheBranch makes another run the legitimate owner of
// the repository+branch, through the real lock manager.
func seedCompetingRunHoldingTheBranch(t *testing.T, f *placementFixture) string {
	t.Helper()
	ctx := context.Background()
	other, err := f.coord.CreateTaskRun(ctx, workflowcoreTaskRequest(t, "the run that got there first"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.locks.Acquire(ctx, branchlock.AcquireRequest{
		ProjectID: "p", RunID: other.Run.ID, StepID: workStepOf(t, f.store, other.Run.ID).ID,
	}); err != nil {
		t.Fatalf("competing run could not take the branch: %v", err)
	}
	return other.Run.ID
}

func fillEveryWorkerSlot(t *testing.T, f *placementFixture, limits domain.CapacityLimits) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	run := domain.WorkflowRun{
		ID: "wf-machine-filler", ProjectID: "p", Objective: "filler", State: domain.WorkflowRunRunning,
		PolicyVersion: "v1", PolicySnapshot: "{}", CreatedAt: now, UpdatedAt: now,
	}
	if _, _, err := f.store.CreateWorkflowRun(ctx, run, nil); err != nil {
		t.Fatalf("seed filler run: %v", err)
	}
	for i := 0; i < limits.LimitFor(domain.ExecutionKindWorker); i++ {
		key := fmt.Sprintf("filler-%d", i)
		if _, err := f.store.EnqueueCapacityClaim(ctx, domain.CapacityClaim{
			ID: "cap-" + key, Kind: domain.ExecutionKindWorker, State: domain.CapacityClaimQueued,
			WorkflowRunID: run.ID, WorkflowStepID: key, LifecycleGeneration: 1,
			DispatchKey: key, ProjectID: "p", Priority: domain.CapacityPriorityNormal,
			EnqueuedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("enqueue filler claim: %v", err)
		}
		granted, err := f.store.AcquireCapacity(ctx, key, 1, limits, domain.ExecutionKindWorker, now)
		if err != nil || !granted {
			t.Fatalf("filler could not take worker slot %d: granted=%v err=%v", i, granted, err)
		}
	}
}

func assertNoOutstandingCapacityClaims(t *testing.T, f *placementFixture, runID string) {
	t.Helper()
	claims, err := f.store.ListCapacityClaimsForRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, claim := range claims {
		if claim.State != domain.CapacityClaimReleased {
			t.Fatalf("a refused admission left a %s capacity claim; a launch that was going to be refused must not occupy a slot", claim.State)
		}
	}
}

func attemptCountFor(t *testing.T, f *placementFixture, stepID string) int {
	t.Helper()
	attempts, err := f.store.ListWorkflowAttempts(context.Background(), stepID)
	if err != nil {
		t.Fatal(err)
	}
	return len(attempts)
}
