package branchlock_test

import (
	"context"
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/branchlock"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// Checkpoint 8P-E.14. Before this checkpoint a branch lock could only be owned
// by a workflow run, so an ordinary task took no lock at all and contended with
// nothing. These tests pin the property that fixes that: a task and a workflow
// compete for one repository+branch, in both orderings, over the same manager
// and the same lock key.

// A task session can own a lock in its own right.
func TestAcquireBySessionOwnsTheBranch(t *testing.T) {
	store := mustOpenDirectProjectStore(t, "proj", "/repos/ao", "main")
	mgr := newManager(t, store, &fakePreflight{}, "owner-1")

	locks, err := mgr.Acquire(context.Background(), branchlock.AcquireRequest{ProjectID: "proj", SessionID: "proj-1"})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if len(locks) != 1 {
		t.Fatalf("locks = %d, want 1", len(locks))
	}
	if !locks[0].SessionOwned() {
		t.Fatalf("lock %#v should be session-owned", locks[0])
	}
	if locks[0].SessionID != "proj-1" || locks[0].WorkflowRunID != "" {
		t.Fatalf("owner = run %q / session %q, want session-only", locks[0].WorkflowRunID, locks[0].SessionID)
	}
}

// An acquisition that names neither a run nor a session would write a row no
// release path could ever match, permanently occupying the branch.
func TestAcquireWithoutAnOwnerIsRefused(t *testing.T) {
	store := mustOpenDirectProjectStore(t, "proj", "/repos/ao", "main")
	mgr := newManager(t, store, &fakePreflight{}, "owner-1")

	if _, err := mgr.Acquire(context.Background(), branchlock.AcquireRequest{ProjectID: "proj"}); err == nil {
		t.Fatal("acquire with no owner succeeded, want refusal")
	}
}

// The incident direction: a workflow owns the branch, and a task must not be
// able to take it. Requirement D/E, first ordering.
func TestWorkflowHoldsBranchThenTaskIsBlockedUntilRelease(t *testing.T) {
	store := mustOpenDirectProjectStore(t, "proj", "/repos/ao", "feat/engineering-control-center")
	mgr := newManager(t, store, &fakePreflight{}, "owner-1")
	ctx := context.Background()

	if _, err := mgr.Acquire(ctx, branchlock.AcquireRequest{ProjectID: "proj", RunID: "WF-1", StepID: "step-1"}); err != nil {
		t.Fatalf("workflow acquire: %v", err)
	}

	_, err := mgr.Acquire(ctx, branchlock.AcquireRequest{ProjectID: "proj", SessionID: "proj-9"})
	var conflict domain.BranchLockConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("task acquire err = %v, want a branch-lock conflict", err)
	}
	if conflict.Holder.WorkflowRunID != "WF-1" {
		t.Fatalf("holder = %#v, want the workflow run", conflict.Holder)
	}
	// The operator has to be told who to go look at, not just that it is busy.
	if got := conflict.Holder.OwnerDescription(); got != "workflow WF-1" {
		t.Fatalf("owner description = %q", got)
	}
	// Critically: the branch itself is untouched and there is exactly one owner.
	assertSingleHolder(t, store, "WF-1")

	if _, err := mgr.ReleaseRun(ctx, "WF-1", "run completed"); err != nil {
		t.Fatalf("release run: %v", err)
	}
	if _, err := mgr.Acquire(ctx, branchlock.AcquireRequest{ProjectID: "proj", SessionID: "proj-9"}); err != nil {
		t.Fatalf("task acquire after release: %v", err)
	}
	assertSingleHolder(t, store, "")
}

// The inverse ordering: a task owns the branch and a workflow must wait for it,
// exactly as it would wait for another workflow. Requirement D/E, second
// ordering.
func TestTaskHoldsBranchThenWorkflowWaitsUntilRelease(t *testing.T) {
	store := mustOpenDirectProjectStore(t, "proj", "/repos/ao", "feat/engineering-control-center")
	mgr := newManager(t, store, &fakePreflight{}, "owner-1")
	ctx := context.Background()

	if _, err := mgr.Acquire(ctx, branchlock.AcquireRequest{ProjectID: "proj", SessionID: "proj-4"}); err != nil {
		t.Fatalf("task acquire: %v", err)
	}

	_, err := mgr.Acquire(ctx, branchlock.AcquireRequest{ProjectID: "proj", RunID: "WF-2", StepID: "step-1"})
	var conflict domain.BranchLockConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("workflow acquire err = %v, want a branch-lock conflict", err)
	}
	if !errors.Is(err, domain.ErrBranchLockHeld) {
		t.Fatalf("err = %v, want it to unwrap to ErrBranchLockHeld so the coordinator parks in waiting_for_branch", err)
	}
	if conflict.Holder.SessionID != "proj-4" {
		t.Fatalf("holder = %#v, want the task session", conflict.Holder)
	}
	if got := conflict.Holder.OwnerDescription(); got != "task session proj-4" {
		t.Fatalf("owner description = %q", got)
	}

	if _, err := mgr.ReleaseSession(ctx, "proj-4", "task terminated"); err != nil {
		t.Fatalf("release session: %v", err)
	}
	if _, err := mgr.Acquire(ctx, branchlock.AcquireRequest{ProjectID: "proj", RunID: "WF-2", StepID: "step-1"}); err != nil {
		t.Fatalf("workflow acquire after release: %v", err)
	}
	assertSingleHolder(t, store, "WF-2")
}

// Two tasks contend with each other too. This is the case a bare
// WorkflowRunID comparison got wrong: both session-owned requests carry an
// empty run id, so comparing run ids alone made the second task look like a
// re-entry of the first and handed both the same branch.
func TestTwoTasksCannotBothOwnTheSameBranch(t *testing.T) {
	store := mustOpenDirectProjectStore(t, "proj", "/repos/ao", "main")
	mgr := newManager(t, store, &fakePreflight{}, "owner-1")
	ctx := context.Background()

	if _, err := mgr.Acquire(ctx, branchlock.AcquireRequest{ProjectID: "proj", SessionID: "proj-1"}); err != nil {
		t.Fatalf("first task acquire: %v", err)
	}
	_, err := mgr.Acquire(ctx, branchlock.AcquireRequest{ProjectID: "proj", SessionID: "proj-2"})
	if !errors.Is(err, domain.ErrBranchLockHeld) {
		t.Fatalf("second task acquire err = %v, want a conflict", err)
	}
	assertSingleHolder(t, store, "")
}

// Re-acquiring is still idempotent for the same session, so a restore or a
// reconcile pass does not conflict with itself.
func TestSameSessionReacquireIsIdempotent(t *testing.T) {
	store := mustOpenDirectProjectStore(t, "proj", "/repos/ao", "main")
	mgr := newManager(t, store, &fakePreflight{}, "owner-1")
	ctx := context.Background()

	first, err := mgr.Acquire(ctx, branchlock.AcquireRequest{ProjectID: "proj", SessionID: "proj-1"})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	again, err := mgr.Acquire(ctx, branchlock.AcquireRequest{ProjectID: "proj", SessionID: "proj-1"})
	if err != nil {
		t.Fatalf("re-acquire: %v", err)
	}
	if first[0].ID != again[0].ID {
		t.Fatalf("re-acquire produced a new lock %q (was %q)", again[0].ID, first[0].ID)
	}
}

// HeldBySession reports only what the session owns itself. A workflow lock that
// merely names this session as its current worker belongs to the run, which
// outlives the session, and must not be released when the session ends.
func TestHeldBySessionExcludesWorkflowOwnedLocks(t *testing.T) {
	store := mustOpenDirectProjectStore(t, "proj", "/repos/ao", "main")
	mgr := newManager(t, store, &fakePreflight{}, "owner-1")
	ctx := context.Background()

	if _, err := mgr.Acquire(ctx, branchlock.AcquireRequest{ProjectID: "proj", RunID: "WF-1", SessionID: "proj-1"}); err != nil {
		t.Fatalf("workflow acquire: %v", err)
	}
	held, err := mgr.HeldBySession(ctx, "proj-1")
	if err != nil {
		t.Fatalf("held by session: %v", err)
	}
	if len(held) != 0 {
		t.Fatalf("held = %d, want 0: the run owns this lock, not the session", len(held))
	}
	n, err := mgr.ReleaseSession(ctx, "proj-1", "task terminated")
	if err != nil {
		t.Fatalf("release session: %v", err)
	}
	if n != 0 {
		t.Fatalf("released %d workflow-owned locks, want 0", n)
	}
	assertSingleHolder(t, store, "WF-1")
}

// Restart recovery. A task lock must not become a zombie: reconciliation
// decides it from the owning session's durable state, exactly as it decides a
// workflow lock from the run's.
func TestReconcileReleasesLocksOfTerminatedAndMissingTaskSessions(t *testing.T) {
	ctx := context.Background()
	store := mustOpenDirectProjectStore(t, "proj", "/repos/ao", "main")

	live := mustCreateSession(t, store, "proj", false)
	dead := mustCreateSession(t, store, "proj", true)

	// Three locks on three different repositories so all can be held at once.
	seedDirectProject(t, store, "proj-b", "/repos/b", "main")
	seedDirectProject(t, store, "proj-c", "/repos/c", "main")

	mgr := newManager(t, store, &fakePreflight{}, "owner-1")
	if _, err := mgr.Acquire(ctx, branchlock.AcquireRequest{ProjectID: "proj", SessionID: string(live.ID)}); err != nil {
		t.Fatalf("live acquire: %v", err)
	}
	if _, err := mgr.Acquire(ctx, branchlock.AcquireRequest{ProjectID: "proj-b", SessionID: string(dead.ID)}); err != nil {
		t.Fatalf("terminated acquire: %v", err)
	}
	if _, err := mgr.Acquire(ctx, branchlock.AcquireRequest{ProjectID: "proj-c", SessionID: "proj-does-not-exist"}); err != nil {
		t.Fatalf("missing-session acquire: %v", err)
	}

	// A fresh daemon instance: a different owner token, as after a restart.
	restarted := newManager(t, store, &fakePreflight{}, "owner-2")
	res, err := restarted.Reconcile(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.Released != 2 {
		t.Fatalf("released = %d, want the terminated and the missing session's locks", res.Released)
	}
	// The live task keeps its branch: releasing it would let a second writer
	// start on top of work only that session knows about.
	if res.Adopted != 1 {
		t.Fatalf("adopted = %d, want the live task's lock taken over by this instance", res.Adopted)
	}
	held, err := store.ListHeldBranchLocks(ctx)
	if err != nil {
		t.Fatalf("list held: %v", err)
	}
	if len(held) != 1 || held[0].SessionID != string(live.ID) {
		t.Fatalf("held = %#v, want only the live task's lock", held)
	}
}

// An idle task is the normal state of a session between agent turns.
// Reconciliation must not mistake it for a dead one and hand its branch away.
func TestReconcileKeepsAnIdleTaskSessionsLock(t *testing.T) {
	ctx := context.Background()
	store := mustOpenDirectProjectStore(t, "proj", "/repos/ao", "main")
	idle := mustCreateSession(t, store, "proj", false)

	mgr := newManager(t, store, &fakePreflight{}, "owner-1")
	if _, err := mgr.Acquire(ctx, branchlock.AcquireRequest{ProjectID: "proj", SessionID: string(idle.ID)}); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	res, err := mgr.Reconcile(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.Released != 0 || res.Kept != 1 {
		t.Fatalf("reconcile = %#v, want the idle task's lock kept", res)
	}
}

// ---- helpers ----

func mustOpenDirectProjectStore(t *testing.T, id, path, branch string) *sqlite.Store {
	t.Helper()
	store := sqlitetest.MustOpen(t)
	seedDirectProject(t, store, id, path, branch)
	return store
}

func mustCreateSession(t *testing.T, store *sqlite.Store, projectID domain.ProjectID, terminated bool) domain.SessionRecord {
	t.Helper()
	rec, err := store.CreateSession(context.Background(), domain.SessionRecord{
		ProjectID: projectID,
		Kind:      domain.KindWorker,
		Harness:   domain.HarnessClaudeCode,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if terminated {
		rec.IsTerminated = true
		if err := store.UpdateSession(context.Background(), rec); err != nil {
			t.Fatalf("terminate session: %v", err)
		}
	}
	return rec
}

// assertSingleHolder asserts the database holds exactly one lock, optionally
// owned by the named run. The uniqueness itself is the property under test:
// "only one writer owns the repo+branch".
func assertSingleHolder(t *testing.T, store *sqlite.Store, wantRunID string) {
	t.Helper()
	held, err := store.ListHeldBranchLocks(context.Background())
	if err != nil {
		t.Fatalf("list held: %v", err)
	}
	if len(held) != 1 {
		t.Fatalf("held locks = %d, want exactly 1 writer", len(held))
	}
	if wantRunID != "" && held[0].WorkflowRunID != wantRunID {
		t.Fatalf("holder run = %q, want %q", held[0].WorkflowRunID, wantRunID)
	}
}
