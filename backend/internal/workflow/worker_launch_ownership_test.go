package workflow_test

// L: the launch retry/reopen path must not become a hole in branch and
// worktree ownership.
//
// This is a structural property, not a coincidence: every retry and every human
// reopen re-enters dispatchFromPending under the SAME outbox entry, and
// ensureBranchLock runs there, BEFORE the outbox CAS and before Spawn. These
// tests hold that structure in place by exercising the two ways ownership can
// refuse — the branch is held by another run, and the primary checkout is dirty
// — through a retry and through a reopen rather than through a first dispatch.

import (
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// directBranchLocks builds a fakeBranchLocks in direct-branch mode for proj-1,
// i.e. a project whose runs must own repo+branch before anything is spawned.
func directBranchLocks(repoPath string) *fakeBranchLocks {
	locks := newFakeBranchLocks()
	locks.targets["proj-1"] = []branchTarget{{repoPath: repoPath, branch: "feat/engineering-control-center"}}
	return locks
}

// A launch retry must queue behind the branch's real holder, exactly like a
// first dispatch does — never "the retry is owed, so take the branch".
func TestWorkerLaunchRetry_StillWaitsForTheBranchItDoesNotOwn(t *testing.T) {
	spawn := &launchSpawner{failWith: tmuxNoSuchSessionErr, failCount: 1}
	locks := directBranchLocks("/repo")
	f := newLaunchFixtureWithLocks(t, spawn, locks)
	f.start()

	if len(spawn.calls) != 1 {
		t.Fatalf("spawn calls = %d, want 1 (the first dispatch held the branch and failed)", len(spawn.calls))
	}
	// Another workflow takes the branch while this run is waiting out its retry
	// — the ordinary consequence of a failed launch releasing nothing it never
	// really used.
	key := domain.BranchLockKey("/repo", "feat/engineering-control-center")
	locks.held[key] = domain.BranchLock{
		ID: "blk-other", LockKey: key, ProjectID: "proj-1", RepoPath: "/repo",
		RepoName: domain.RootWorkspaceRepoName, Branch: "feat/engineering-control-center",
		WorkflowRunID: "wf-someone-else", State: domain.BranchLockHeld,
	}

	f.clk.Advance(2 * time.Minute)
	if _, err := f.poller.RunDueOnce(f.ctx); err != nil {
		t.Fatalf("RunDueOnce: %v", err)
	}

	if len(spawn.calls) != 1 {
		t.Fatalf("spawn calls = %d: the retry spawned into a branch owned by another run", len(spawn.calls))
	}
	if got := f.run().State; got != domain.WorkflowRunWaiting {
		t.Fatalf("run state = %q, want waiting (queued for the branch)", got)
	}
	if !f.hasPhase("waiting_for_branch") {
		t.Fatalf("expected a waiting_for_branch record; phases = %v", f.checkpointPhases())
	}

	// Once the branch frees up, the same retry proceeds — the wait was a queue,
	// not a dead end.
	delete(locks.held, key)
	f.clk.Advance(2 * time.Minute)
	if _, err := f.coord.ContinueRun(f.ctx, f.runID); err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	if len(spawn.calls) != 2 {
		t.Fatalf("spawn calls after the branch freed = %d, want 2", len(spawn.calls))
	}
	if f.sessionCount() != 1 {
		t.Fatalf("sessions = %d, want 1", f.sessionCount())
	}
}

// A human reopen of a durably failed dispatch is still refused while the user's
// primary checkout holds uncommitted work. "The person pressed Continue" is
// never a licence to touch a dirty working tree.
func TestWorkerLaunchReopen_StillRefusesADirtyPrimaryCheckout(t *testing.T) {
	spawn := &launchSpawner{}
	locks := directBranchLocks("/repo")
	f := newLaunchFixtureWithLocks(t, spawn, locks)
	f.startWithoutDispatch()
	f.seedHistoricalDispatchFailure()

	locks.dirty["/repo"] = true
	f.restart()
	if _, err := f.coord.ContinueRun(f.ctx, f.runID); err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}

	if len(spawn.calls) != 0 {
		t.Fatalf("spawn calls = %d, want 0: a reopen must not spawn into a dirty primary checkout", len(spawn.calls))
	}
	if !f.hasPhase("dirty_worktree") {
		t.Fatalf("expected the dirty-worktree stop to be recorded; phases = %v", f.checkpointPhases())
	}

	// And when the person commits/stashes their work, the very same reopened
	// entry dispatches — once.
	delete(locks.dirty, "/repo")
	f.clk.Advance(time.Minute)
	if _, err := f.coord.ContinueRun(f.ctx, f.runID); err != nil {
		t.Fatalf("ContinueRun after the checkout was cleaned: %v", err)
	}
	if len(spawn.calls) != 1 {
		t.Fatalf("spawn calls = %d, want exactly 1", len(spawn.calls))
	}
	if f.outboxCount() != 1 {
		t.Fatalf("spawn outbox entries = %d, want 1", f.outboxCount())
	}
}

// A project that is NOT in direct-branch mode (isolated_worktree /
// smart_parallel_worktrees: no repo+branch target to own) is unaffected by any
// of this — the retry path adds no new ownership requirement and removes none.
func TestWorkerLaunchRetry_IsolatedWorktreeProjectsAreUnaffected(t *testing.T) {
	spawn := &launchSpawner{failWith: tmuxNoSuchSessionErr, failCount: 1}
	locks := newFakeBranchLocks() // no targets => not direct-branch
	f := newLaunchFixtureWithLocks(t, spawn, locks)
	f.start()

	f.clk.Advance(2 * time.Minute)
	if _, err := f.poller.RunDueOnce(f.ctx); err != nil {
		t.Fatalf("RunDueOnce: %v", err)
	}
	if len(spawn.calls) != 2 {
		t.Fatalf("spawn calls = %d, want 2 (one failure, one retry)", len(spawn.calls))
	}
	if got := f.workStep(); got.SessionID == nil {
		t.Fatal("the retry did not link a session")
	}
	if len(locks.releases) != 0 {
		t.Fatalf("branch locks released = %v, want none for a non-direct-branch project", locks.releases)
	}
	if f.sessionCount() != 1 {
		t.Fatalf("sessions = %d, want 1", f.sessionCount())
	}
}
