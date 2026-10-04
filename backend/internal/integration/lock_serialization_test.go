package integration

import (
	"context"
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/branchlock"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// The single-lane property is only worth as much as the lock it is built on,
// so these run against the real branchlock.Manager over the real branch_locks
// table rather than the in-memory fake the coordinator's own tests use. What
// they establish is the exclusion SHAPE: one integration per target branch,
// shared with whatever else writes that branch, and shared with nothing else.

func integLockManager(t *testing.T) *branchlock.Manager {
	t.Helper()
	return branchlock.New(branchlock.Deps{Store: sqlitetest.MustOpen(t), OwnerToken: "daemon-1"})
}

func TestIntegrationLaneSerializesOnTheDurableBranchLock(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	locker := NewBranchLocker(integLockManager(t))
	req := LockRequest{
		ProjectID:     "prj-1",
		WorkflowRunID: "wf-1",
		TaskID:        "task-1",
		RepoPath:      "/repo",
		TargetBranch:  "main",
	}

	handle, err := locker.Acquire(ctx, req)
	if err != nil {
		t.Fatalf("first acquisition: %v", err)
	}

	// A second task integrating the SAME target has to wait, and has to be told
	// so in a way it can act on rather than as an opaque failure.
	second := req
	second.TaskID, second.WorkflowRunID = "task-2", "wf-2"
	if _, err := locker.Acquire(ctx, second); !errors.Is(err, ErrLockBusy) {
		t.Fatalf("second acquisition: err = %v, want ErrLockBusy", err)
	}
	if _, err := locker.Acquire(ctx, second); !errors.Is(err, domain.ErrBranchLockHeld) {
		t.Fatalf("ErrLockBusy no longer matches the branch-lock error callers already handle: %v", err)
	}

	// A task integrating a DIFFERENT target is not affected at all: the
	// exclusion is per repository+branch, which is what lets everything else
	// keep running while one target is being written.
	other := req
	other.TaskID, other.WorkflowRunID, other.TargetBranch = "task-3", "wf-3", "release"
	otherHandle, err := locker.Acquire(ctx, other)
	if err != nil {
		t.Fatalf("an unrelated target was blocked: %v", err)
	}
	if otherHandle.LockKey == handle.LockKey {
		t.Fatal("two different target branches produced one lock key")
	}

	// Releasing hands the lane to whoever was waiting.
	if err := locker.Release(ctx, handle, "integration finished"); err != nil {
		t.Fatal(err)
	}
	if _, err := locker.Acquire(ctx, second); err != nil {
		t.Fatalf("after release: %v", err)
	}
}

// An integration and a direct writer of one branch must exclude each other:
// they are the same mutable surface, so they share a lock key even though they
// are different kinds of ownership.
func TestIntegrationLaneExcludesADirectWriterOfTheSameBranch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mgr := integLockManager(t)
	locker := NewBranchLocker(mgr)

	// A direct-branch style owner takes /repo#main first. (Kind is passed
	// explicitly so the acquisition needs no project row or preflight; the key
	// it produces is the same one direct-branch execution computes.)
	if _, err := mgr.Acquire(ctx, branchlock.AcquireRequest{
		ProjectID: "prj-1", SessionID: "ses-writer",
		Kind: domain.BranchLockOwnershipTaskWorkspace, RepoPath: "/repo", Branch: "main",
	}); err != nil {
		t.Fatalf("writer acquisition: %v", err)
	}

	if _, err := locker.Acquire(ctx, LockRequest{
		ProjectID: "prj-1", WorkflowRunID: "wf-1", TaskID: "task-1",
		RepoPath: "/repo", TargetBranch: "main",
	}); !errors.Is(err, ErrLockBusy) {
		t.Fatalf("integration ran while another owner held the branch: err = %v", err)
	}

	// A task worktree on its own ao/* branch shares nothing with the target.
	if _, err := mgr.Acquire(ctx, branchlock.AcquireRequest{
		ProjectID: "prj-1", SessionID: "ses-task", Kind: domain.BranchLockOwnershipTaskWorkspace,
		RepoPath: "/repo", Branch: "ao/task-9",
	}); err != nil {
		t.Fatalf("an isolated task branch was blocked by the target lane: %v", err)
	}
}

// The acquisition is recorded as an integration, so an operator reading
// branch_locks can tell why a branch is busy.
func TestIntegrationLaneRecordsItsOwnershipKind(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := sqlitetest.MustOpen(t)
	mgr := branchlock.New(branchlock.Deps{Store: store, OwnerToken: "daemon-1"})

	if _, err := NewBranchLocker(mgr).Acquire(ctx, LockRequest{
		ProjectID: "prj-1", WorkflowRunID: "wf-1", TaskID: "task-1",
		RepoPath: "/repo", TargetBranch: "main",
	}); err != nil {
		t.Fatal(err)
	}

	locks, err := store.ListHeldBranchLocks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(locks) != 1 {
		t.Fatalf("held locks = %d, want 1", len(locks))
	}
	if locks[0].OwnershipKind != domain.BranchLockOwnershipTargetIntegration {
		t.Fatalf("ownership kind = %q, want %q", locks[0].OwnershipKind, domain.BranchLockOwnershipTargetIntegration)
	}
	if locks[0].Branch != "main" || locks[0].RepoPath != "/repo" {
		t.Fatalf("lock = %+v", locks[0])
	}
}

// Releasing one integration must free exactly that pair -- not everything else
// the same run holds, which for a run integrating one of several tasks would
// hand away branches its other tasks are still using.
func TestReleasingAnIntegrationFreesOnlyItsOwnTarget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mgr := integLockManager(t)
	locker := NewBranchLocker(mgr)

	main, err := locker.Acquire(ctx, LockRequest{
		ProjectID: "prj-1", WorkflowRunID: "wf-1", TaskID: "task-1", RepoPath: "/repo", TargetBranch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := locker.Acquire(ctx, LockRequest{
		ProjectID: "prj-1", WorkflowRunID: "wf-1", TaskID: "task-2", RepoPath: "/repo", TargetBranch: "release",
	}); err != nil {
		t.Fatal(err)
	}

	if err := locker.Release(ctx, main, "integration finished"); err != nil {
		t.Fatal(err)
	}
	if _, err := locker.Acquire(ctx, LockRequest{
		ProjectID: "prj-1", WorkflowRunID: "wf-9", TaskID: "task-9", RepoPath: "/repo", TargetBranch: "main",
	}); err != nil {
		t.Fatalf("main was not freed: %v", err)
	}
	if _, err := locker.Acquire(ctx, LockRequest{
		ProjectID: "prj-1", WorkflowRunID: "wf-9", TaskID: "task-9", RepoPath: "/repo", TargetBranch: "release",
	}); !errors.Is(err, ErrLockBusy) {
		t.Fatalf("releasing main also freed release: err = %v", err)
	}
}
