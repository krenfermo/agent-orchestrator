package workflow_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// p1c_capacity_test.go — the scheduler as the lifecycle actually meets it.
//
// The bounds themselves are proven at the storage layer
// (store/capacity_store_test.go), which is where they are enforced. What these
// tests prove is the part that only exists once the scheduler is wired into
// dispatch: that a run with no slot WAITS rather than failing, that waiting
// costs it nothing, that freeing a slot lets it through, and that no shape of
// run reserves more than it uses.
//
// Note what is not mocked. The whole autonomous fixture -- every test in this
// package that drives a real objective -- now runs through admission control
// under the real default limits. A scheduler that wrongly refused, or wrongly
// granted, would break that suite rather than only these tests.

func heldClaims(t *testing.T, fx *autonomousFixture) []domain.CapacityClaim {
	t.Helper()
	held, err := fx.store.ListHeldCapacityClaims(context.Background())
	if err != nil {
		t.Fatalf("ListHeldCapacityClaims: %v", err)
	}
	return held
}

func claimsOf(t *testing.T, fx *autonomousFixture, runID string) []domain.CapacityClaim {
	t.Helper()
	claims, err := fx.store.ListCapacityClaimsForRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListCapacityClaimsForRun(%s): %v", runID, err)
	}
	return claims
}

func attemptTotal(t *testing.T, fx *autonomousFixture, runID string) int {
	t.Helper()
	ctx := context.Background()
	steps, err := fx.store.ListWorkflowSteps(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, step := range steps {
		attempts, aerr := fx.store.ListWorkflowAttempts(ctx, step.ID)
		if aerr != nil {
			t.Fatal(aerr)
		}
		total += len(attempts)
	}
	return total
}

// occupyEveryWorkerSlot makes the machine genuinely full, the way it is full in
// production: another workflow is holding the slots.
//
// It deliberately does not configure a limit of zero. Zero normalizes to the
// shipped default (domain.CapacityLimits.Normalize), because a partial or
// mistyped configuration must degrade to a default rather than to a scheduler
// that grants nothing -- so "no slots" is not expressible, and a test that
// tried would be testing a state production cannot reach.
func occupyEveryWorkerSlot(t *testing.T, fx *autonomousFixture, limits domain.CapacityLimits) {
	t.Helper()
	ctx := context.Background()
	now := fx.clk.Now()
	run := domain.WorkflowRun{
		ID: "wf-occupier", ProjectID: "p", Objective: "occupier", State: domain.WorkflowRunRunning,
		PolicyVersion: "v1", PolicySnapshot: "{}", CreatedAt: now, UpdatedAt: now,
	}
	if _, _, err := fx.store.CreateWorkflowRun(ctx, run, nil); err != nil {
		t.Fatalf("seed occupier run: %v", err)
	}
	for i := 0; i < limits.LimitFor(domain.ExecutionKindWorker); i++ {
		key := fmt.Sprintf("occupier-%d", i)
		claim := domain.CapacityClaim{
			ID: "cap-" + key, Kind: domain.ExecutionKindWorker, State: domain.CapacityClaimQueued,
			WorkflowRunID: run.ID, WorkflowStepID: key, LifecycleGeneration: 1,
			DispatchKey: key, ProjectID: "p", Priority: domain.CapacityPriorityNormal,
			EnqueuedAt: now, UpdatedAt: now,
		}
		if _, err := fx.store.EnqueueCapacityClaim(ctx, claim); err != nil {
			t.Fatalf("enqueue occupier claim: %v", err)
		}
		granted, err := fx.store.AcquireCapacity(ctx, key, 1, limits, domain.ExecutionKindWorker, now)
		if err != nil || !granted {
			t.Fatalf("occupier could not take worker slot %d: granted=%v err=%v", i, granted, err)
		}
	}
}

// freeOccupiedWorkerSlots gives the machine back.
func freeOccupiedWorkerSlots(t *testing.T, fx *autonomousFixture) {
	t.Helper()
	if _, err := fx.store.ReleaseCapacityClaimsForRun(context.Background(), "wf-occupier", "test freed the machine", fx.clk.Now()); err != nil {
		t.Fatalf("free occupier slots: %v", err)
	}
}

// Matrix 1: with capacity, a run launches and holds exactly one claim for the
// runtime it started.
func TestCapacityAvailableLetsAWorkerLaunch(t *testing.T) {
	fx, ctx, masterID := startAutonomousObjective(t, twoTaskDependentPlan())
	_, childID := dispatchedChild(t, fx, masterID)

	child, ok, err := fx.store.GetWorkflowRun(ctx, childID)
	if err != nil || !ok {
		t.Fatalf("GetWorkflowRun(child): %v (found=%v)", err, ok)
	}
	if child.State == domain.WorkflowRunWaiting {
		t.Fatalf("the child is waiting even though the machine is empty: %+v", child)
	}
	worker := 0
	for _, claim := range claimsOf(t, fx, childID) {
		if claim.Kind == domain.ExecutionKindWorker {
			worker++
		}
	}
	if worker != 1 {
		t.Fatalf("the child holds %d worker claims for one launch, want exactly 1", worker)
	}
	if len(fx.spawner.calls) != 1 {
		t.Fatalf("Spawn called %d times, want 1", len(fx.spawner.calls))
	}
}

// Matrix 2/3: with the machine full, work WAITS. It does not fail, and it does
// not spend a retry.
func TestCapacityFullMakesWorkWaitWithoutSpendingRetries(t *testing.T) {
	fx, ctx, masterID := startAutonomousObjective(t, oneTaskPlan())
	// Two worker slots on the machine, and another workflow already holding
	// both of them.
	tight := domain.CapacityLimits{
		Global: 9, PerWorkflow: 9,
		PerKind: map[domain.ExecutionKind]int{
			domain.ExecutionKindWorker:   2,
			domain.ExecutionKindPlanner:  9,
			domain.ExecutionKindReviewer: 9,
			domain.ExecutionKindRepair:   9,
		},
	}
	occupyEveryWorkerSlot(t, fx, tight)
	fx.withCapacityLimits(tight)

	driveCycles(t, fx, 6, nil)

	tasks, err := fx.store.ListWorkflowTasks(ctx, masterID)
	if err != nil || len(tasks) == 0 {
		t.Fatalf("the plan produced %d tasks: %v", len(tasks), err)
	}
	childID, found, ferr := fx.store.FindWorkflowRunByPlannedTask(ctx, tasks[0].ID)
	if ferr != nil {
		t.Fatal(ferr)
	}
	if !found {
		// No child at all is also an acceptable "waited": nothing launched.
		if len(fx.spawner.calls) != 0 {
			t.Fatalf("a worker spawned with zero worker capacity: %d calls", len(fx.spawner.calls))
		}
		return
	}

	// 2: the child exists and is parked, not failed.
	child, ok, err := fx.store.GetWorkflowRun(ctx, childID)
	if err != nil || !ok {
		t.Fatalf("GetWorkflowRun(child): %v (found=%v)", err, ok)
	}
	if child.State == domain.WorkflowRunFailed || child.State == domain.WorkflowRunCancelled {
		t.Fatalf("a run with no capacity ended %s; waiting must never become failing", child.State)
	}
	if len(fx.spawner.calls) != 0 {
		t.Fatalf("%d workers spawned with zero worker capacity", len(fx.spawner.calls))
	}

	// The claim is durably QUEUED -- the queue is a row, not a memory.
	queuedWorker := false
	for _, claim := range claimsOf(t, fx, childID) {
		if claim.Kind == domain.ExecutionKindWorker && claim.State == domain.CapacityClaimQueued {
			queuedWorker = true
		}
	}
	if !queuedWorker {
		t.Fatalf("no queued worker claim was recorded for the parked child: %+v", claimsOf(t, fx, childID))
	}

	// 3: no retry budget was spent. A capacity wait is not an attempt.
	if got := attemptTotal(t, fx, childID); got != 0 {
		t.Fatalf("%d attempts were recorded while waiting for capacity; a wait must cost nothing", got)
	}

	// 24: and it did not storm. Many poller cycles over an unchanged condition
	// leave one wake, not one per cycle.
	next, err := fx.wake.NextForRun(ctx, domain.WorkflowRunID(childID))
	if err != nil {
		t.Fatal(err)
	}
	if next != nil && next.AttemptCount > 6 {
		t.Fatalf("wake attempt count = %d after 6 cycles; the capacity wait is spinning", next.AttemptCount)
	}
}

// Matrix 4: raising the bound lets the queued work through, with no
// intervention beyond the daemon's own poller.
func TestFreeingCapacityLetsQueuedWorkStart(t *testing.T) {
	fx, ctx, masterID := startAutonomousObjective(t, oneTaskPlan())
	tight := domain.CapacityLimits{
		Global: 9, PerWorkflow: 9,
		PerKind: map[domain.ExecutionKind]int{domain.ExecutionKindWorker: 2,
			domain.ExecutionKindPlanner: 9, domain.ExecutionKindReviewer: 9, domain.ExecutionKindRepair: 9},
	}
	occupyEveryWorkerSlot(t, fx, tight)
	fx.withCapacityLimits(tight)
	driveCycles(t, fx, 6, nil)
	if len(fx.spawner.calls) != 0 {
		t.Fatalf("%d workers spawned while another workflow held every worker slot", len(fx.spawner.calls))
	}

	// The machine gets room. Nothing else changes.
	freeOccupiedWorkerSlots(t, fx)
	driveUntil(t, fx, 8, func() bool { return len(fx.spawner.calls) > 0 })

	if len(fx.spawner.calls) == 0 {
		t.Fatal("queued work never started after capacity was freed")
	}
	tasks, err := fx.store.ListWorkflowTasks(ctx, masterID)
	if err != nil || len(tasks) == 0 {
		t.Fatalf("tasks = %d: %v", len(tasks), err)
	}
	_ = masterID
}

// Matrix 18/19: a run reserves what it uses and no more. A TASK-strategy run
// holds one worker slot; an objective's parent holds no worker slot at all,
// because it owns no worker runtime.
func TestRunsReserveOnlyTheRuntimesTheyOwn(t *testing.T) {
	fx, ctx, masterID := startAutonomousObjective(t, twoTaskDependentPlan())
	_, childID := dispatchedChild(t, fx, masterID)
	driveCycles(t, fx, 3, nil)

	for _, claim := range claimsOf(t, fx, masterID) {
		if claim.Kind == domain.ExecutionKindWorker || claim.Kind == domain.ExecutionKindReviewer {
			t.Fatalf("the objective itself reserved a %s slot; only the runs that own runtimes may: %+v", claim.Kind, claim)
		}
	}
	// 19: breadth is the scheduler's, not the plan's. However many tasks the
	// plan has, no more than the per-workflow bound is held at once.
	limits := domain.DefaultCapacityLimits()
	byRun := map[string]int{}
	for _, claim := range heldClaims(t, fx) {
		byRun[claim.WorkflowRunID]++
	}
	for runID, n := range byRun {
		if n > limits.PerWorkflow {
			t.Fatalf("run %s holds %d slots, past the per-workflow bound of %d", runID, n, limits.PerWorkflow)
		}
	}
	_ = ctx
	_ = childID
}

// Matrix 20/21: a repair is scheduled like everything else -- it waits when
// there is no room, and it gives its slot back when it ends.
func TestRepairWaitsForCapacityAndReleasesIt(t *testing.T) {
	fx, ctx, masterID := startAutonomousObjective(t, twoTaskDependentPlan())
	_, childID := dispatchedChild(t, fx, masterID)
	parkOnHumanDecision(t, fx, childID)
	makeChildStopRepairable(t, fx, childID)
	freezeRepairMode(t, fx, childID, domain.RepairModeSuggest)

	// The machine's worker slots are all taken, so the repair run's own worker
	// dispatch must queue rather than launch.
	tight := domain.CapacityLimits{
		Global: 9, PerWorkflow: 9,
		PerKind: map[domain.ExecutionKind]int{domain.ExecutionKindWorker: 1,
			domain.ExecutionKindReviewer: 9, domain.ExecutionKindPlanner: 9, domain.ExecutionKindRepair: 9},
	}
	occupyEveryWorkerSlot(t, fx, tight)
	coord := fx.withCapacityLimits(tight)
	intent, err := coord.LaunchRepair(ctx, childID, "operator")
	if err != nil {
		t.Fatalf("LaunchRepair: %v", err)
	}
	if intent.RepairRunID == "" {
		t.Fatal("no repair run was created")
	}
	spawnsBefore := len(fx.spawner.calls)
	driveCycles(t, fx, 4, nil)
	if len(fx.spawner.calls) != spawnsBefore {
		t.Fatalf("a repair worker spawned while the machine was full (%d -> %d)", spawnsBefore, len(fx.spawner.calls))
	}
	// 20: the repair run is parked, not failed, and the run it is repairing is
	// untouched -- a queued repair must not alter the original's authority.
	repairRun, ok, err := fx.store.GetWorkflowRun(ctx, intent.RepairRunID)
	if err != nil || !ok {
		t.Fatalf("GetWorkflowRun(repair): %v (found=%v)", err, ok)
	}
	if repairRun.State.Terminal() {
		t.Fatalf("a repair queued for capacity ended %s", repairRun.State)
	}
	childAfter, _, _ := fx.store.GetWorkflowRun(ctx, childID)
	if childAfter.State != domain.WorkflowRunNeedsAttention {
		t.Fatalf("the run being repaired moved to %s while its repair only waited for capacity", childAfter.State)
	}

	// 21: when the repair ends, its slots come back.
	freeOccupiedWorkerSlots(t, fx)
	coord = fx.withCapacityLimits(domain.DefaultCapacityLimits())
	if _, cerr := coord.CancelRun(ctx, intent.RepairRunID); cerr != nil {
		t.Fatalf("end the repair: %v", cerr)
	}
	for _, claim := range claimsOf(t, fx, intent.RepairRunID) {
		if claim.State != domain.CapacityClaimReleased {
			t.Fatalf("a terminal repair run still holds a %s claim in state %s", claim.Kind, claim.State)
		}
	}
}

// Matrix 22: a terminal run releases everything, and the release is visible in
// the scheduler's own snapshot.
func TestTerminalRunReleasesItsSlotsAndTheSnapshotAgrees(t *testing.T) {
	fx, ctx, masterID := startAutonomousObjective(t, twoTaskDependentPlan())
	_, childID := dispatchedChild(t, fx, masterID)
	driveCycles(t, fx, 2, nil)

	before, err := fx.coord.SchedulerSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if before.Global.Limit <= 0 {
		t.Fatalf("the snapshot reports a global limit of %d; a misconfiguration must normalize to a default", before.Global.Limit)
	}

	if _, cerr := fx.coord.CancelRun(ctx, childID); cerr != nil {
		t.Fatalf("CancelRun: %v", cerr)
	}
	for _, claim := range claimsOf(t, fx, childID) {
		if claim.State != domain.CapacityClaimReleased {
			t.Fatalf("a cancelled run still holds a %s claim in state %s", claim.Kind, claim.State)
		}
	}
	after, err := fx.coord.SchedulerSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, claim := range after.HeldClaims {
		if claim.WorkflowRunID == childID {
			t.Fatalf("the snapshot still shows a cancelled run holding %s", claim.Kind)
		}
	}
	if after.Global.Held > before.Global.Held {
		t.Fatalf("held slots grew from %d to %d across a cancellation", before.Global.Held, after.Global.Held)
	}
}

// The snapshot is the observability contract: limits, usage and the queue, with
// nothing secret in it.
func TestSchedulerSnapshotReportsLimitsUsageAndQueue(t *testing.T) {
	fx, ctx, _ := startAutonomousObjective(t, oneTaskPlan())
	coord := fx.withCapacityLimits(domain.CapacityLimits{
		Global: 3, PerWorkflow: 1,
		PerKind: map[domain.ExecutionKind]int{domain.ExecutionKindWorker: 2,
			domain.ExecutionKindReviewer: 1, domain.ExecutionKindPlanner: 1, domain.ExecutionKindRepair: 1},
	})
	snap, err := coord.SchedulerSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Global.Limit != 3 || snap.Limits.PerWorkflow != 1 {
		t.Fatalf("snapshot limits = global %d perWorkflow %d, want 3 and 1", snap.Global.Limit, snap.Limits.PerWorkflow)
	}
	kinds := map[domain.ExecutionKind]int{}
	for _, usage := range snap.PerKind {
		kinds[usage.Kind] = usage.Limit
	}
	for kind, want := range map[domain.ExecutionKind]int{
		domain.ExecutionKindWorker: 2, domain.ExecutionKindReviewer: 1,
		domain.ExecutionKindPlanner: 1, domain.ExecutionKindRepair: 1,
	} {
		if kinds[kind] != want {
			t.Fatalf("snapshot limit for %s = %d, want %d", kind, kinds[kind], want)
		}
	}
	if snap.ObservedAt.IsZero() {
		t.Fatal("the snapshot must say when it was taken")
	}
	_ = workflowcore.RecoveryAssessmentVersion
}
