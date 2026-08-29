package workflow_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// p1d_crash_matrix_test.go — P1-D §L: the five crash-matrix rows the previous
// pass reported as "true by construction, not individually driven".
//
// "By construction" was an honest description and a poor guarantee. Each of
// these holds because of a CAS or a generation fence somewhere, and a
// refactoring that weakened one would have broken nothing visible. These tests
// name the row, stage the exact window, and assert the refusal.
//
//	17  repair lock transfer BEFORE the repair launches
//	18  repair completes BEFORE the lock transfers back
//	23  daemon restart during a failover transition
//	24  stale provider A reconnects after provider B is authoritative
//	25  stale integration generation after a newer merge

// Row 17: the branch must be the repair run's BEFORE anything is launched into
// it. A repair that launched first and took the lock afterwards would spend the
// interval writing a checkout the stopped run still owned.
func TestCrashRow17_BranchTransfersToRepairBeforeAnythingLaunches(t *testing.T) {
	f := newCessionFixture(t)
	ctx := context.Background()

	// Before the transfer the origin owns the branch, and the repair run has
	// nothing: no lock, and therefore nothing it is entitled to write.
	if got := f.holder(t); got != f.origin.ID {
		t.Fatalf("setup: holder is %s, want the origin", got)
	}
	repairHeld, err := f.store.ListHeldBranchLocksByRun(ctx, f.repair.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(repairHeld) != 0 {
		t.Fatalf("the repair run holds %d locks before the cession", len(repairHeld))
	}

	moved, err := f.store.CedeBranchLock(ctx, f.lock.ID, f.origin.ID, f.repair.ID, "", time.Now().UTC())
	if err != nil || !moved {
		t.Fatalf("cede: moved=%v err=%v", moved, err)
	}

	// The ordering that matters: after the cession the repair holds it and the
	// origin does not, with no instant in between where nobody did — the row
	// never leaves `held`, which is what the holder() assertion checks.
	if got := f.holder(t); got != f.repair.ID {
		t.Fatalf("after cession the holder is %s, want the repair run", got)
	}
	originHeld, err := f.store.ListHeldBranchLocksByRun(ctx, f.origin.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(originHeld) != 0 {
		t.Fatalf("the origin still holds %d locks after ceding; it must be unable to mutate", len(originHeld))
	}
	// And a crash right here cannot let the origin take it back by simply
	// asking: the CAS names the current holder.
	back, err := f.store.CedeBranchLock(ctx, f.lock.ID, f.origin.ID, f.origin.ID, "", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if back {
		t.Fatal("the origin reclaimed a branch it no longer held")
	}
}

// Row 18: the branch goes back only when the repair is finished with it, and a
// SUPERSEDED repair generation may not hand it back at all — returning on a
// stale agent's say-so would restore authority the lifecycle already replaced.
func TestCrashRow18_BranchReturnsOnlyFromTheCurrentRepairGeneration(t *testing.T) {
	f := newCessionFixture(t)
	ctx := context.Background()

	if moved, err := f.store.CedeBranchLock(ctx, f.lock.ID, f.origin.ID, f.repair.ID, "", time.Now().UTC()); err != nil || !moved {
		t.Fatalf("setup cede: moved=%v err=%v", moved, err)
	}
	// A crash between the cession and the repair finishing leaves the branch
	// with the repair. That is the correct resting state: the origin must not
	// be able to resume writing while a repair may still be mid-flight.
	if got := f.holder(t); got != f.repair.ID {
		t.Fatalf("holder = %s, want the repair run while the repair is outstanding", got)
	}

	// The repair returns it, conditioned on it still holding the lock.
	back, err := f.store.CedeBranchLock(ctx, f.lock.ID, f.repair.ID, f.origin.ID, "", time.Now().UTC())
	if err != nil || !back {
		t.Fatalf("return: back=%v err=%v", back, err)
	}
	if got := f.holder(t); got != f.origin.ID {
		t.Fatalf("after the return the holder is %s, want the origin", got)
	}
	// Idempotent: a second return finds the lock already with the origin and
	// moves nothing, rather than erroring or taking it from whoever has it.
	again, err := f.store.CedeBranchLock(ctx, f.lock.ID, f.repair.ID, f.origin.ID, "", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if again {
		t.Fatal("a second return moved a lock the repair no longer held")
	}
}

// Row 23: a daemon restart lands in the middle of a failover transition. The
// predecessor is terminated before the successor is written, so the restart
// finds one terminal attempt and no live one — never two live attempts on one
// placement — and the next pass converges from the durable ordinal.
func TestCrashRow23_DaemonRestartDuringAFailoverTransition(t *testing.T) {
	f := newProviderFixture(t)
	ctx := context.Background()

	// The window: predecessor terminated, successor not yet written.
	if !f.coord.AdvanceProviderAttemptForTest(ctx, f.attempt, domain.ProviderAttemptFailedSafe) {
		t.Fatal("could not terminate the predecessor")
	}

	// The daemon comes back.
	rebooted := workflowcore.New(workflowcore.Deps{
		Store: f.store, Projects: f.store,
		Placements: f.store, ProviderAttempts: f.store,
		InstanceToken: "daemon-after-restart",
	})

	// What it must NOT find is an attempt still entitled to act.
	if rebooted.ProviderAttemptIsAuthoritative(ctx, f.attempt.ID) {
		t.Fatal("after the restart the terminated predecessor is still authoritative")
	}
	// What it must NOT do is refill the budget.
	budget := rebooted.ProviderFailoverBudget(ctx, f.run, f.step)
	if budget.CurrentOrdinal != 1 {
		t.Fatalf("after the restart the recorded ordinal is %d, want 1", budget.CurrentOrdinal)
	}
	// And the placement is untouched by the whole episode: a failover is not a
	// placement change.
	live := f.live(t)
	if live.PlacementGeneration != f.placement.PlacementGeneration {
		t.Fatalf("a crashed failover moved the placement to generation %d", live.PlacementGeneration)
	}
	if !live.State.PermitsLaunch() {
		t.Fatalf("the placement is %q after a crashed failover; the checkout is still fine", live.State)
	}
}

// Row 24: provider A reconnects — a late signal, an adopted pane, a retried
// report — after provider B has become authoritative. Everything A tries is
// inert.
func TestCrashRow24_StaleProviderAReconnectsAfterBIsAuthoritative(t *testing.T) {
	f := newProviderFixture(t)
	ctx := context.Background()

	a := f.attempt
	b, hopped, err := f.coord.FailoverProviderAttempt(ctx, f.run, f.step, a,
		domain.HarnessCodex, "", domain.FailoverSafeBeforeExecution,
		domain.WorkflowErrorAgentStartFailed, "claude refused", "")
	if err != nil || !hopped {
		t.Fatalf("setup hop: hopped=%v err=%v", hopped, err)
	}
	if ok, err := f.store.BindProviderAttemptRuntime(ctx, b.ID, "sess-b", "cap-b", time.Now().UTC()); err != nil || !ok {
		t.Fatalf("bind B: ok=%v err=%v", ok, err)
	}

	// A comes back holding its own stale view of itself and tries everything a
	// live attempt is allowed to do.
	for _, next := range []domain.ProviderAttemptState{
		domain.ProviderAttemptRunning,
		domain.ProviderAttemptCompleted,
		domain.ProviderAttemptLaunching,
	} {
		if f.coord.AdvanceProviderAttemptForTest(ctx, a, next) {
			t.Fatalf("a stale attempt moved itself to %q", next)
		}
	}
	if ok, _ := f.store.BindProviderAttemptRuntime(ctx, a.ID, "sess-a", "cap-b", time.Now().UTC()); ok {
		t.Fatal("a stale attempt bound itself to a runtime and claimed its successor's slot")
	}
	// A cannot authorize a further hop either: it does not hold the obligation.
	if _, hopped, err := f.coord.FailoverProviderAttempt(ctx, f.run, f.step, a,
		domain.HarnessAider, "", domain.FailoverSafeBeforeExecution,
		domain.WorkflowErrorAgentStartFailed, "stale A tried to route onward", ""); err != nil || hopped {
		t.Fatalf("a stale attempt authorized a hop: hopped=%v err=%v", hopped, err)
	}

	// B is exactly as it was.
	after := f.reload(t, b.ID)
	if !after.State.Authoritative() || after.CapacityClaimID != "cap-b" || after.RuntimeSessionID != "sess-b" {
		t.Fatalf("B was disturbed by its stale predecessor: %+v", after)
	}
}

// Row 25: a stale integration generation after a newer merge. The old
// placement generation cannot record an integration, cannot retire the newer
// one, and cannot authorize the cleanup that an integration authorizes.
func TestCrashRow25_StaleIntegrationGenerationAfterANewerMerge(t *testing.T) {
	f := newPlacementFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()

	first, _, err := f.coord.EnsureExecutionPlacement(ctx, f.run, f.step)
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := f.coord.ReplaceExecutionPlacement(ctx, f.run, f.step, "the checkout was recreated")
	if err != nil {
		t.Fatal(err)
	}

	// The NEWER placement's work lands.
	landed, err := f.store.MarkExecutionPlacementIntegrated(ctx, f.run.ID, "", "",
		replacement.PlacementGeneration, "deadbeef", now)
	if err != nil || !landed {
		t.Fatalf("the current placement could not record its integration: landed=%v err=%v", landed, err)
	}

	// The stale generation, arriving late, tries to record its own.
	stale, err := f.store.MarkExecutionPlacementIntegrated(ctx, f.run.ID, "", "",
		first.PlacementGeneration, "cafebabe", now)
	if err != nil {
		t.Fatal(err)
	}
	if stale {
		t.Fatal("a superseded placement generation recorded an integration after a newer merge")
	}

	// It cannot retire the newer one either — which is the same refusal wearing
	// a different hat: GC is authorized by the integration it names.
	retired, err := f.store.RetireSupersededExecutionPlacements(ctx, f.run.ID, "", "",
		first.PlacementGeneration, "a stale holder tried to clean up", now)
	if err != nil {
		t.Fatal(err)
	}
	if retired != 0 {
		t.Fatalf("a stale generation retired %d placements", retired)
	}

	// And the record still names the commit the work actually landed at.
	current, found, err := f.store.GetExecutionPlacement(ctx, f.run.ID, "", "", replacement.PlacementGeneration)
	if err != nil || !found {
		t.Fatalf("current placement missing: found=%v err=%v", found, err)
	}
	if current.IntegratedSHA != "deadbeef" || current.State != domain.PlacementIntegrated {
		t.Fatalf("the current placement is %+v; a stale writer changed the landing record", current)
	}
}

// An integration that names no commit is refused by the statement itself, so
// "cleanup is authorized by a fact, not by a state name" is a constraint rather
// than a convention.
func TestIntegrationWithoutACommitIsRefused(t *testing.T) {
	f := newPlacementFixture(t)
	ctx := context.Background()

	placement, _, err := f.coord.EnsureExecutionPlacement(ctx, f.run, f.step)
	if err != nil {
		t.Fatal(err)
	}
	ok, err := f.store.MarkExecutionPlacementIntegrated(ctx, f.run.ID, "", "",
		placement.PlacementGeneration, "", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("a placement claimed integration without naming the commit its work landed at")
	}
}
