package workflow_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/branchlock"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// p1d_branch_cession_test.go — P1-D §L / matrix 21-24.
//
// The property under test is not "the lock moves". It is that the lock moves
// ONLY between the two runs that are entitled to hold it, only in the
// directions the lifecycle allows, and never through a state in which nobody
// holds it. Every failure mode here ends with a branch owned by the wrong run,
// which on a direct-branch project means two agents writing one checkout.

// cessionFixture is a real sqlite store with two runs and one held lock: the
// shape a direct-branch repair starts from.
type cessionFixture struct {
	store  *crashStore
	origin domain.WorkflowRun
	repair domain.WorkflowRun
	lock   domain.BranchLock
}

// workflowcoreTaskRequest is a bounded task-run request for the two runs a
// cession needs.
func workflowcoreTaskRequest(t *testing.T, objective string) workflowcore.TaskRunRequest {
	t.Helper()
	return workflowcore.TaskRunRequest{
		ProjectID: "p", Objective: objective,
		Strategy: explicitStrategy(t, domain.ExecutionStrategyTask),
	}
}

// cessionBranchLocks is the workflow-level view of the branch-lock manager,
// mirroring the daemon's own adapter. The coordinator type-asserts this shape
// for the Cede capability, so the test exercises the same assertion production
// does rather than a manager the coordinator never sees.
type cessionBranchLocks struct{ mgr *branchlock.Manager }

func (c cessionBranchLocks) Acquire(ctx context.Context, req workflowcore.BranchLockRequest) ([]domain.BranchLock, error) {
	return c.mgr.Acquire(ctx, branchlock.AcquireRequest{
		ProjectID: req.ProjectID, RunID: req.RunID, StepID: req.StepID, SessionID: req.SessionID,
	})
}
func (c cessionBranchLocks) ReleaseRun(ctx context.Context, runID, reason string) (int64, error) {
	return c.mgr.ReleaseRun(ctx, runID, reason)
}
func (c cessionBranchLocks) HeldByRun(ctx context.Context, runID string) ([]domain.BranchLock, error) {
	return c.mgr.HeldByRun(ctx, runID)
}
func (c cessionBranchLocks) Renew(ctx context.Context, runID, stepID, sessionID string) {
	c.mgr.Renew(ctx, runID, stepID, sessionID)
}
func (c cessionBranchLocks) RecoverStale(ctx context.Context, runID string) (int64, error) {
	return c.mgr.RecoverStale(ctx, runID)
}

// Holder answers "who owns this branch right now" (P3-C). The double
// implements it because the production adapter does: without it the
// commit-and-continue authority proof degrades to a refusal, and the fixture
// would then be testing the degraded path rather than the real one.
func (c cessionBranchLocks) Holder(ctx context.Context, repoPath, branch string) (domain.BranchLock, bool, error) {
	return c.mgr.Holder(ctx, repoPath, branch)
}
func (c cessionBranchLocks) Cede(ctx context.Context, lockID, fromRunID, toRunID, toStepID string) (bool, error) {
	return c.mgr.Cede(ctx, lockID, fromRunID, toRunID, toStepID)
}

func workflowCoordinatorFor(t *testing.T, store *crashStore) *workflowcore.Coordinator {
	t.Helper()
	return workflowcore.New(workflowcore.Deps{
		Store: store, Projects: store,
		BranchLocks: cessionBranchLocks{mgr: branchlock.New(branchlock.Deps{Store: store})},
	})
}

func newCessionFixture(t *testing.T) *cessionFixture {
	t.Helper()
	ctx := context.Background()
	store, reboot := newCrashFixture(t, validMasterPlan())
	c := reboot()

	origin, err := c.CreateTaskRun(ctx, workflowcoreTaskRequest(t, "the original run"))
	if err != nil {
		t.Fatal(err)
	}
	repair, err := c.CreateTaskRun(ctx, workflowcoreTaskRequest(t, "the repair run"))
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	lock, err := store.AcquireBranchLock(ctx, domain.BranchLock{
		ID: "bl-1", LockKey: "/repo\x1fmain", ProjectID: "p",
		RepoPath: "/repo", RepoName: domain.RootWorkspaceRepoName, Branch: "main",
		OwnershipKind: domain.BranchLockOwnershipDirectBranch,
		WorkflowRunID: origin.Run.ID, OwnerToken: "daemon-1",
		State: domain.BranchLockHeld, BaseSHA: "abc123",
		AcquiredAt: now, RenewedAt: now, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("seed branch lock: %v", err)
	}
	return &cessionFixture{store: store, origin: origin.Run, repair: repair.Run, lock: lock}
}

func (f *cessionFixture) holder(t *testing.T) string {
	t.Helper()
	lock, found, err := f.store.GetBranchLock(context.Background(), f.lock.ID)
	if err != nil || !found {
		t.Fatalf("GetBranchLock: %v (found=%v)", err, found)
	}
	if !lock.Held() {
		t.Fatalf("the branch lock is %s; a cession must never leave the branch unowned", lock.State)
	}
	return lock.WorkflowRunID
}

// Matrix 21/22: the lock can be handed to a repair, and the originating run
// stops holding it the moment it does.
func TestBranchLockCedesToRepairAndOriginLosesIt(t *testing.T) {
	f := newCessionFixture(t)
	ctx := context.Background()

	if got := f.holder(t); got != f.origin.ID {
		t.Fatalf("the origin does not hold its own branch: holder=%s", got)
	}
	moved, err := f.store.CedeBranchLock(ctx, f.lock.ID, f.origin.ID, f.repair.ID, "", time.Now().UTC())
	if err != nil || !moved {
		t.Fatalf("cede: moved=%v err=%v", moved, err)
	}
	if got := f.holder(t); got != f.repair.ID {
		t.Fatalf("after cession the holder is %s, want the repair run %s", got, f.repair.ID)
	}
	// 22: the origin cannot mutate, because it no longer holds the lock — and
	// it cannot take it back by simply asking, because the CAS names the
	// current holder.
	held, err := f.store.ListHeldBranchLocksByRun(ctx, f.origin.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(held) != 0 {
		t.Fatalf("the origin still holds %d locks while a repair owns its branch", len(held))
	}
}

// Matrix 23: a stale view of ownership can neither take the lock nor give it
// away. Both directions are refusals, and both leave the holder unchanged.
func TestStaleHolderCannotCedeOrReclaim(t *testing.T) {
	f := newCessionFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if moved, err := f.store.CedeBranchLock(ctx, f.lock.ID, f.origin.ID, f.repair.ID, "", now); err != nil || !moved {
		t.Fatalf("initial cede: moved=%v err=%v", moved, err)
	}

	// A pass that still believes the ORIGIN holds it tries to hand it on.
	// It names the wrong current holder, so it matches nothing.
	if moved, err := f.store.CedeBranchLock(ctx, f.lock.ID, f.origin.ID, "wf-someone-else", "", now); err != nil || moved {
		t.Fatalf("a stale holder ceded the lock onward: moved=%v err=%v", moved, err)
	}
	if got := f.holder(t); got != f.repair.ID {
		t.Fatalf("holder changed to %s on a refused cession", got)
	}

	// And a third run cannot simply claim it from whoever holds it.
	if moved, err := f.store.CedeBranchLock(ctx, f.lock.ID, "wf-stranger", "wf-stranger", "", now); err != nil || moved {
		t.Fatalf("a stranger took the lock: moved=%v err=%v", moved, err)
	}
	if got := f.holder(t); got != f.repair.ID {
		t.Fatalf("holder changed to %s after a stranger's attempt", got)
	}
}

// Matrix 24: the return is a transfer in the other direction, and it happens
// exactly once. A second return is refused rather than silently re-applied.
func TestBranchLockReturnsOnceAndIsIdempotent(t *testing.T) {
	f := newCessionFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if moved, err := f.store.CedeBranchLock(ctx, f.lock.ID, f.origin.ID, f.repair.ID, "", now); err != nil || !moved {
		t.Fatalf("cede: moved=%v err=%v", moved, err)
	}
	if moved, err := f.store.CedeBranchLock(ctx, f.lock.ID, f.repair.ID, f.origin.ID, "", now); err != nil || !moved {
		t.Fatalf("return: moved=%v err=%v", moved, err)
	}
	if got := f.holder(t); got != f.origin.ID {
		t.Fatalf("after the return the holder is %s, want the origin %s", got, f.origin.ID)
	}
	// A repeated return names a holder that is no longer current: refused.
	if moved, err := f.store.CedeBranchLock(ctx, f.lock.ID, f.repair.ID, f.origin.ID, "", now); err != nil || moved {
		t.Fatalf("a repeated return moved the lock again: moved=%v err=%v", moved, err)
	}
	if got := f.holder(t); got != f.origin.ID {
		t.Fatalf("holder = %s after a refused repeat", got)
	}
}

// The lock is never released as part of a cession: at every point in the
// exchange exactly one run holds a HELD row. A release-then-acquire would open
// a window for a third run to take the branch, which is the same bug with
// extra steps.
func TestCessionNeverLeavesTheBranchUnowned(t *testing.T) {
	f := newCessionFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()

	for i, hop := range []struct{ from, to string }{
		{f.origin.ID, f.repair.ID},
		{f.repair.ID, f.origin.ID},
		{f.origin.ID, f.repair.ID},
	} {
		if moved, err := f.store.CedeBranchLock(ctx, f.lock.ID, hop.from, hop.to, "", now); err != nil || !moved {
			t.Fatalf("hop %d: moved=%v err=%v", i, moved, err)
		}
		// holder() itself fails the test if the row is not held.
		if got := f.holder(t); got != hop.to {
			t.Fatalf("hop %d: holder = %s, want %s", i, got, hop.to)
		}
		// And exactly one held row exists for this branch throughout.
		held, err := f.store.ListHeldBranchLocks(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(held) != 1 {
			t.Fatalf("hop %d: %d held locks for one branch", i, len(held))
		}
	}
}

// A repair that is no longer the current generation may not hand a branch back:
// its authority was superseded, and returning on its say-so would let a stale
// agent decide who writes the repository.
func TestSupersededRepairGenerationCannotReturnTheBranch(t *testing.T) {
	f := newCessionFixture(t)
	ctx := context.Background()
	c := workflowCoordinatorFor(t, f.store)

	// Two repair dispatches on the ledger: generation 2 is current.
	for gen := 1; gen <= 2; gen++ {
		intent := domain.RepairIntent{
			ID: "wfr-" + string(rune('0'+gen)), WorkflowRunID: f.origin.ID, TargetRunID: f.origin.ID,
			ConditionReason: "verify_budget_exhausted", EvidenceDigest: "digest", Generation: gen,
			RepairRunID: f.repair.ID, Strategy: domain.ExecutionStrategyTask,
		}
		payload, _ := json.Marshal(intent)
		if _, err := f.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
			ID: "wfc-repair-" + string(rune('0'+gen)), WorkflowRunID: f.origin.ID, ProjectID: "p",
			DurablePhase: "workflow_repair_dispatched", PayloadVersion: "v1",
			RetryState: string(payload), CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
	}

	stale := domain.RepairIntent{ID: "wfr-1", WorkflowRunID: f.origin.ID, RepairRunID: f.repair.ID, Generation: 1}
	if err := c.ReturnBranchLockFromRepairForTest(ctx, f.origin, stale); err == nil {
		t.Fatal("a superseded repair generation was allowed to return a branch lock")
	}
}
