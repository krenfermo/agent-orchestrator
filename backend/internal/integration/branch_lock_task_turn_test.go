// This file is the end-to-end regression guard for Checkpoint 8P-E.14A: an
// ordinary task on a direct-branch project must give its repository+branch back
// when its work finishes, so the next task can start on that branch.
//
// It is an integration test rather than a unit one because the defect it guards
// lived exactly in the seam between three packages that each looked correct on
// their own: session_manager took the lock at spawn, lifecycle released it in
// MarkTerminated, and nothing ever called MarkTerminated for a task that simply
// finished — a successful ordinary task goes idle and stays alive. Only a wired
// view of spawn -> hook -> reducer -> branch_locks -> next spawn can see that.
//
// Everything here is real except the runtime, the agent adapter and the
// workspace: a real sqlite.Store, a real branchlock.Manager over the real
// branch_locks table, a real session_manager and a real lifecycle.Manager,
// wired exactly as internal/daemon wires them.
package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/branchlock"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/lifecycle"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	sessionmanager "github.com/aoagents/agent-orchestrator/backend/internal/session_manager"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// branchLockStack is newStack's direct-branch sibling: the same real components,
// plus the branch-lock manager and the same two adapters daemon.go installs.
type branchLockStack struct {
	store *sqlite.Store
	mgr   *sessionmanager.Manager
	lcm   *lifecycle.Manager
	locks *branchlock.Manager
}

// taskBranchLocks is internal/daemon's sessionBranchLocks: the lock a task takes
// when it starts.
type taskBranchLocks struct{ mgr *branchlock.Manager }

func (a taskBranchLocks) AcquireForSession(ctx context.Context, projectID domain.ProjectID, sessionID domain.SessionID) ([]domain.BranchLock, error) {
	return a.mgr.Acquire(ctx, branchlock.AcquireRequest{ProjectID: projectID, SessionID: string(sessionID)})
}

func (a taskBranchLocks) ReleaseSession(ctx context.Context, sessionID, reason string) (int64, error) {
	return a.mgr.ReleaseSession(ctx, sessionID, reason)
}

// turnBranchLocks is internal/daemon's sessionTurnBranchLocks: the lock a task
// takes and gives back at its turn boundaries.
type turnBranchLocks struct{ mgr *branchlock.Manager }

func (a turnBranchLocks) AcquireForSession(ctx context.Context, projectID domain.ProjectID, sessionID domain.SessionID) ([]domain.BranchLock, error) {
	return a.mgr.ReacquireForSession(ctx, projectID, string(sessionID))
}

func (a turnBranchLocks) ReleaseSession(ctx context.Context, sessionID, reason string) (int64, error) {
	return a.mgr.ReleaseSession(ctx, sessionID, reason)
}

func newBranchLockStack(t *testing.T) *branchLockStack {
	t.Helper()
	ctx := context.Background()
	store, err := sqlitetest.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.UpsertProject(ctx, domain.ProjectRecord{
		ID:           "mer",
		Path:         t.TempDir(),
		RegisteredAt: time.Now(),
		Config: domain.ProjectConfig{
			ExecutionMode: domain.ExecutionDirectBranch,
			DefaultBranch: "feat/control-center",
			Worker:        domain.RoleOverride{Harness: domain.HarnessClaudeCode},
			Orchestrator:  domain.RoleOverride{Harness: domain.HarnessClaudeCode},
		},
	}); err != nil {
		t.Fatal(err)
	}
	msg := &captureMessenger{}
	lcm := lifecycle.New(store, msg)
	mgr := sessionmanager.New(sessionmanager.Deps{
		Runtime: &stubRuntime{}, Agents: stubAgents{}, Workspace: &stubWorkspace{},
		Store: store, Messenger: msg, Lifecycle: lcm,
		LookPath: func(string) (string, error) { return "/usr/bin/true", nil },
	})
	lcm.SetCompletionTerminator(mgr)
	// No Preflight: these repositories are empty temp dirs, and the dirty gate
	// has its own coverage in internal/branchlock.
	locks := branchlock.New(branchlock.Deps{Store: store, OwnerToken: "daemon-1"})
	mgr.SetBranchLocks(taskBranchLocks{mgr: locks})
	lcm.SetBranchLocks(turnBranchLocks{mgr: locks})
	return &branchLockStack{store: store, mgr: mgr, lcm: lcm, locks: locks}
}

func (s *branchLockStack) spawn(t *testing.T, prompt string) domain.SessionRecord {
	t.Helper()
	rec, _, _, err := s.mgr.Spawn(context.Background(), ports.SpawnConfig{
		ProjectID: "mer", Kind: domain.KindWorker, Prompt: prompt,
	})
	if err != nil {
		t.Fatalf("spawn %q: %v", prompt, err)
	}
	return rec
}

// holder returns the session that currently owns the project's branch, if any.
func (s *branchLockStack) holder(t *testing.T) (domain.BranchLock, bool) {
	t.Helper()
	held, err := s.store.ListHeldBranchLocks(context.Background())
	if err != nil {
		t.Fatalf("list held branch locks: %v", err)
	}
	if len(held) == 0 {
		return domain.BranchLock{}, false
	}
	if len(held) > 1 {
		t.Fatalf("held locks = %d, want at most one writer of one branch", len(held))
	}
	return held[0], true
}

// signal delivers one agent hook through the same reducer the HTTP activity
// endpoint calls, stamped with the session's live generation the way the real
// hook is.
func (s *branchLockStack) signal(t *testing.T, id domain.SessionID, sig ports.ActivitySignal) {
	t.Helper()
	rec, found, err := s.store.GetSession(context.Background(), id)
	if err != nil || !found {
		t.Fatalf("get session %s: found=%v err=%v", id, found, err)
	}
	sig.LaunchID = rec.Metadata.RuntimeLaunchID
	if sig.Timestamp.IsZero() {
		sig.Timestamp = time.Now().UTC()
	}
	if err := s.lcm.ApplyActivitySignal(context.Background(), id, sig); err != nil {
		t.Fatalf("activity signal %q for %s: %v", sig.Event, id, err)
	}
}

// stop delivers the activity signal a finished agent turn reports (the Stop
// hook).
func (s *branchLockStack) stop(t *testing.T, id domain.SessionID) {
	t.Helper()
	s.signal(t, id, ports.ActivitySignal{Valid: true, State: domain.ActivityIdle, Event: "stop"})
}

// The incident, reproduced and fixed: a direct-branch ordinary task takes the
// branch, does its work, reports its turn finished, and the branch is free for
// the next task — with the finished session still alive and untouched, exactly
// as the user sees it on the board.
func TestOrdinaryTaskReleasesItsBranchWhenItsWorkFinishes(t *testing.T) {
	ctx := context.Background()
	st := newBranchLockStack(t)

	first := st.spawn(t, "do the work")
	held, ok := st.holder(t)
	if !ok || !held.SessionOwned() || held.SessionID != string(first.ID) {
		t.Fatalf("holder = %#v ok=%v, want the first task to own the branch", held, ok)
	}
	if held.Branch != "feat/control-center" {
		t.Fatalf("locked branch = %q, want the project's configured branch", held.Branch)
	}

	// While the first task is working, the branch is exclusive. This is the
	// property the checkpoint must not weaken.
	if _, _, _, err := st.mgr.Spawn(ctx, ports.SpawnConfig{ProjectID: "mer", Kind: domain.KindWorker, Prompt: "second"}); !errors.Is(err, sessionmanager.ErrBranchBusy) {
		t.Fatalf("second spawn err = %v, want ErrBranchBusy while the first task is mid-turn", err)
	}

	// The work finishes: the agent reports its turn ended. Nothing kills the
	// session, and nothing marks it terminated — this is what "Inactivo with its
	// final report posted" is.
	st.stop(t, first.ID)

	rec, found, err := st.store.GetSession(ctx, first.ID)
	if err != nil || !found {
		t.Fatalf("get session: found=%v err=%v", found, err)
	}
	if rec.IsTerminated {
		t.Fatal("the finished task was terminated; a finished ordinary task stays alive")
	}
	if rec.Activity.State != domain.ActivityIdle {
		t.Fatalf("activity = %q, want idle", rec.Activity.State)
	}
	if held, ok := st.holder(t); ok {
		t.Fatalf("holder = %#v, want the branch given back when the work finished", held)
	}

	// And the next task really can start on the same branch.
	second := st.spawn(t, "the follow-up task")
	held, ok = st.holder(t)
	if !ok || held.SessionID != string(second.ID) {
		t.Fatalf("holder = %#v ok=%v, want the second task to own the branch", held, ok)
	}
}

// The other half of turn-scoped ownership, and what makes releasing at the end
// of a turn safe: when the user gives a finished task more work, it takes its
// branch back — and when someone else took the branch in the meantime, it does
// not steal it.
func TestFollowUpTurnTakesTheBranchBackWhenItIsFree(t *testing.T) {
	st := newBranchLockStack(t)
	first := st.spawn(t, "do the work")
	st.stop(t, first.ID)

	// A follow-up prompt to the same finished task.
	st.signal(t, first.ID, ports.ActivitySignal{Valid: true, State: domain.ActivityActive, Event: "user-prompt-submit"})
	held, ok := st.holder(t)
	if !ok || held.SessionID != string(first.ID) {
		t.Fatalf("holder = %#v ok=%v, want the follow-up turn to take the branch back", held, ok)
	}

	// And with the branch owned by a second task, a follow-up turn on the first
	// one leaves that ownership alone rather than taking it over.
	st.stop(t, first.ID)
	second := st.spawn(t, "the next task")
	st.signal(t, first.ID, ports.ActivitySignal{Valid: true, State: domain.ActivityActive, Event: "user-prompt-submit"})
	held, ok = st.holder(t)
	if !ok || held.SessionID != string(second.ID) {
		t.Fatalf("holder = %#v ok=%v, want the second task to keep the branch it owns", held, ok)
	}
}

// The idle-vs-mid-task distinction. A task paused on the user is not a finished
// one: it is one turn, still open, waiting for an answer. Its branch stays its
// own — releasing here is what would let a second writer in behind its back.
func TestTaskPausedOnTheUserKeepsItsBranch(t *testing.T) {
	ctx := context.Background()
	st := newBranchLockStack(t)
	first := st.spawn(t, "ask me something")

	for _, signal := range []ports.ActivitySignal{
		{Valid: true, State: domain.ActivityActive, Event: "pre-tool-use", ToolUseID: "t1", ToolName: "Bash"},
		{Valid: true, State: domain.ActivityWaitingInput, Event: "notification"},
		{Valid: true, State: domain.ActivityBlocked, Event: "permission-request", ToolName: "Bash"},
	} {
		st.signal(t, first.ID, signal)
		held, ok := st.holder(t)
		if !ok || held.SessionID != string(first.ID) {
			t.Fatalf("after %q the holder = %#v ok=%v, want the task to keep its branch", signal.Event, held, ok)
		}
	}

	// A second task must still be refused: the first one is not done.
	if _, _, _, err := st.mgr.Spawn(ctx, ports.SpawnConfig{ProjectID: "mer", Kind: domain.KindWorker, Prompt: "second"}); !errors.Is(err, sessionmanager.ErrBranchBusy) {
		t.Fatalf("second spawn err = %v, want ErrBranchBusy while the first task is mid-turn", err)
	}
}

// A task that ends any other way frees the branch too. Killing is how a
// cancelled or failed task ends in AO, and it converges on MarkTerminated.
func TestKilledTaskReleasesItsBranch(t *testing.T) {
	ctx := context.Background()
	st := newBranchLockStack(t)
	first := st.spawn(t, "work that gets cancelled")

	if _, err := st.mgr.Kill(ctx, first.ID); err != nil {
		t.Fatalf("kill: %v", err)
	}
	if held, ok := st.holder(t); ok {
		t.Fatalf("holder = %#v, want a cancelled task's branch freed", held)
	}
	if _, _, _, err := st.mgr.Spawn(ctx, ports.SpawnConfig{ProjectID: "mer", Kind: domain.KindWorker, Prompt: "next"}); err != nil {
		t.Fatalf("spawn after cancellation: %v", err)
	}
}

// Restart recovery. If the release at turn end never happened — the daemon was
// killed mid-turn, the hook never arrived, the release failed — boot
// reconciliation must still not leave a finished task holding the branch
// forever. This is the leg that would have unstuck the real incident's lock.
func TestBootReconciliationFreesABranchNoTaskIsStillWriting(t *testing.T) {
	ctx := context.Background()
	st := newBranchLockStack(t)
	first := st.spawn(t, "work that finished before the crash")

	// The session went idle without the release landing: exactly the durable
	// state the incident left behind.
	rec, _, err := st.store.GetSession(ctx, first.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	rec.Activity = domain.Activity{State: domain.ActivityIdle, LastActivityAt: time.Now().UTC()}
	if err := st.store.UpdateSession(ctx, rec); err != nil {
		t.Fatalf("update session: %v", err)
	}
	if _, ok := st.holder(t); !ok {
		t.Fatal("precondition: the stale lock should still be held")
	}

	// A new daemon instance boots.
	restarted := branchlock.New(branchlock.Deps{Store: st.store, OwnerToken: "daemon-2"})
	res, err := restarted.Reconcile(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.Released != 1 {
		t.Fatalf("reconcile = %#v, want the finished task's stale lock released", res)
	}
	if held, ok := st.holder(t); ok {
		t.Fatalf("holder = %#v, want the branch free after reconciliation", held)
	}
}
