package branchlock_test

import (
	"context"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/branchlock"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// Checkpoint 8P-E.14A: a task session's ownership of its branch lasts a turn,
// not the session's whole life. These pin the acquire half of that — the half
// that makes releasing at the end of a turn safe.

// The full lifecycle the incident broke, end to end at this layer: a task takes
// the branch, finishes its turn and gives it back, and the next task gets it.
func TestSessionOwnershipIsReturnedAtTurnEndAndTakenByTheNextTask(t *testing.T) {
	ctx := context.Background()
	store := mustOpenDirectProjectStore(t, "proj", "/repos/ao", "main")
	first := mustCreateSession(t, store, "proj", false)
	second := mustCreateSession(t, store, "proj", false)
	mgr := newManager(t, store, &fakePreflight{}, "owner-1")

	if _, err := mgr.Acquire(ctx, branchlock.AcquireRequest{ProjectID: "proj", SessionID: string(first.ID)}); err != nil {
		t.Fatalf("first task acquire: %v", err)
	}
	// While the first task's turn is open the branch is genuinely exclusive.
	if _, err := mgr.Acquire(ctx, branchlock.AcquireRequest{ProjectID: "proj", SessionID: string(second.ID)}); !branchlock.IsConflict(err) {
		t.Fatalf("second task acquire err = %v, want a conflict while the first is mid-turn", err)
	}

	released, err := mgr.ReleaseSession(ctx, string(first.ID), "task turn ended (stop)")
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if released != 1 {
		t.Fatalf("released = %d, want the finished task's lock", released)
	}
	if _, err := mgr.Acquire(ctx, branchlock.AcquireRequest{ProjectID: "proj", SessionID: string(second.ID)}); err != nil {
		t.Fatalf("second task acquire after release: %v", err)
	}
	assertSingleHolderSession(t, store, string(second.ID))
}

// A session's own uncommitted work must not lock it out of its own branch on
// its next turn. The dirty gate exists to protect a human's changes, and
// between two turns of one task the changes in the tree are that task's.
func TestReacquireForOwnNextTurnIsNotBlockedByItsOwnUncommittedWork(t *testing.T) {
	ctx := context.Background()
	store := mustOpenDirectProjectStore(t, "proj", "/repos/ao", "main")
	session := mustCreateSession(t, store, "proj", false)
	pre := &fakePreflight{dirty: map[string]bool{}}
	mgr := newManager(t, store, pre, "owner-1")

	if _, err := mgr.Acquire(ctx, branchlock.AcquireRequest{ProjectID: "proj", SessionID: string(session.ID)}); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if _, err := mgr.ReleaseSession(ctx, string(session.ID), "task turn ended (stop)"); err != nil {
		t.Fatalf("release: %v", err)
	}
	// The turn left work in the tree, as agent turns do.
	pre.dirty["/repos/ao"] = true

	if _, err := mgr.ReacquireForSession(ctx, "proj", string(session.ID)); err != nil {
		t.Fatalf("reacquire for own next turn: %v", err)
	}
	assertSingleHolderSession(t, store, string(session.ID))
}

// The exemption is for the previous owner only. A different task meeting the
// same uncommitted changes still gets the full refusal: AO cannot tell whose
// they are, and starting fresh work over them is the thing the gate prevents.
func TestADifferentTaskStillMeetsTheDirtyRefusal(t *testing.T) {
	ctx := context.Background()
	store := mustOpenDirectProjectStore(t, "proj", "/repos/ao", "main")
	first := mustCreateSession(t, store, "proj", false)
	second := mustCreateSession(t, store, "proj", false)
	pre := &fakePreflight{dirty: map[string]bool{}}
	mgr := newManager(t, store, pre, "owner-1")

	if _, err := mgr.Acquire(ctx, branchlock.AcquireRequest{ProjectID: "proj", SessionID: string(first.ID)}); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if _, err := mgr.ReleaseSession(ctx, string(first.ID), "task turn ended (stop)"); err != nil {
		t.Fatalf("release: %v", err)
	}
	pre.dirty["/repos/ao"] = true

	_, err := mgr.ReacquireForSession(ctx, "proj", string(second.ID))
	if !branchlock.IsDirty(err) {
		t.Fatalf("second task reacquire err = %v, want the dirty-repository refusal", err)
	}
}

// A turn start on a lock the session already holds costs nothing and changes
// nothing: no second row, no new lock id.
func TestReacquireIsANoOpWhenTheSessionAlreadyHoldsTheBranch(t *testing.T) {
	ctx := context.Background()
	store := mustOpenDirectProjectStore(t, "proj", "/repos/ao", "main")
	session := mustCreateSession(t, store, "proj", false)
	mgr := newManager(t, store, &fakePreflight{}, "owner-1")

	first, err := mgr.Acquire(ctx, branchlock.AcquireRequest{ProjectID: "proj", SessionID: string(session.ID)})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if _, err := mgr.ReacquireForSession(ctx, "proj", string(session.ID)); err != nil {
		t.Fatalf("reacquire: %v", err)
	}
	held, err := store.ListHeldBranchLocks(ctx)
	if err != nil {
		t.Fatalf("list held: %v", err)
	}
	if len(held) != 1 || held[0].ID != first[0].ID {
		t.Fatalf("held = %#v, want the original lock untouched", held)
	}
}

// A workflow's worker session submits prompts like any other session. Its turns
// must not make it contend with the run that owns the branch on its behalf —
// the run owns it, the worker does not, and the run outlives the worker.
func TestReacquireDoesNotContendWithTheWorkflowRunThatOwnsTheWorker(t *testing.T) {
	ctx := context.Background()
	store := mustOpenDirectProjectStore(t, "proj", "/repos/ao", "main")
	worker := mustCreateSession(t, store, "proj", false)
	mgr := newManager(t, store, &fakePreflight{}, "owner-1")

	if _, err := mgr.Acquire(ctx, branchlock.AcquireRequest{
		ProjectID: "proj", RunID: "WF-1", StepID: "step-1", SessionID: string(worker.ID),
	}); err != nil {
		t.Fatalf("workflow acquire: %v", err)
	}
	locks, err := mgr.ReacquireForSession(ctx, "proj", string(worker.ID))
	if err != nil {
		t.Fatalf("worker turn start: %v, want it to leave the run's lock alone", err)
	}
	if len(locks) != 0 {
		t.Fatalf("locks = %#v, want none taken in the worker's own name", locks)
	}
	assertSingleHolder(t, store, "WF-1")
}

// A turn end is scoped to its own owner. One session's finished turn never
// frees another session's branch, and never frees a workflow's.
func TestTurnEndNeverReleasesAnotherOwnersLock(t *testing.T) {
	ctx := context.Background()
	store := mustOpenDirectProjectStore(t, "proj", "/repos/ao", "main")
	owner := mustCreateSession(t, store, "proj", false)
	stranger := mustCreateSession(t, store, "proj", false)
	mgr := newManager(t, store, &fakePreflight{}, "owner-1")

	if _, err := mgr.Acquire(ctx, branchlock.AcquireRequest{ProjectID: "proj", SessionID: string(owner.ID)}); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	released, err := mgr.ReleaseSession(ctx, string(stranger.ID), "task turn ended (stop)")
	if err != nil {
		t.Fatalf("release by a non-owner: %v", err)
	}
	if released != 0 {
		t.Fatalf("released = %d, want a non-owner to free nothing", released)
	}
	assertSingleHolderSession(t, store, string(owner.ID))
}

// A workflow's own lock lifecycle is untouched by any of this: it is released
// by its run, not by the turns of whichever session is currently its worker.
func TestWorkflowLockStillOnlyEndsWithItsRun(t *testing.T) {
	ctx := context.Background()
	store := mustOpenDirectProjectStore(t, "proj", "/repos/ao", "main")
	worker := mustCreateSession(t, store, "proj", false)
	mgr := newManager(t, store, &fakePreflight{}, "owner-1")

	if _, err := mgr.Acquire(ctx, branchlock.AcquireRequest{
		ProjectID: "proj", RunID: "WF-1", SessionID: string(worker.ID),
	}); err != nil {
		t.Fatalf("workflow acquire: %v", err)
	}
	if n, err := mgr.ReleaseSession(ctx, string(worker.ID), "task turn ended (stop)"); err != nil || n != 0 {
		t.Fatalf("worker turn end released %d locks (err %v), want 0", n, err)
	}
	assertSingleHolder(t, store, "WF-1")

	if n, err := mgr.ReleaseRun(ctx, "WF-1", "workflow run completed"); err != nil || n != 1 {
		t.Fatalf("release run = %d (err %v), want the run's own lock freed", n, err)
	}
}

func assertSingleHolderSession(t *testing.T, store interface {
	ListHeldBranchLocks(context.Context) ([]domain.BranchLock, error)
}, wantSessionID string) {
	t.Helper()
	held, err := store.ListHeldBranchLocks(context.Background())
	if err != nil {
		t.Fatalf("list held: %v", err)
	}
	if len(held) != 1 {
		t.Fatalf("held locks = %d, want exactly 1 writer", len(held))
	}
	if !held[0].SessionOwned() || held[0].SessionID != wantSessionID {
		t.Fatalf("holder = %#v, want the branch owned by session %q", held[0], wantSessionID)
	}
}
