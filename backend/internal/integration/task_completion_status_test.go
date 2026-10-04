// End-to-end guard for the terminal status of an ordinary task: a task that
// finishes its work must read Completed, and must still read Completed after it
// has been sitting there for a week and after the daemon has been restarted.
//
// It is an integration test because the defect it guards lived in the seam
// between packages that each looked right alone: the agent reports its turn
// ended, the reducer writes activity=idle, the store persists a row, and the
// service derives a display status from it. Nothing along that path recorded
// that the work had FINISHED, so the finished task and the task that never did
// anything produced the same row and the board called both Inactive.
package integration

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/lifecycle"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	sessionsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/session"
	sessionmanager "github.com/aoagents/agent-orchestrator/backend/internal/session_manager"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

// completionStack is newStack over a caller-owned data directory, so the same
// database can be closed and reopened the way a daemon restart does.
type completionStack struct {
	store *sqlite.Store
	sm    *sessionsvc.Service
	mgr   *sessionmanager.Manager
	lcm   *lifecycle.Manager
}

func newCompletionStack(t *testing.T, dataDir string) *completionStack {
	t.Helper()
	ctx := context.Background()
	store, err := sqlite.Open(dataDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.UpsertProject(ctx, domain.ProjectRecord{
		ID:           "mer",
		Path:         "/repo/mer",
		RegisteredAt: time.Now(),
		Config: domain.ProjectConfig{
			Worker:       domain.RoleOverride{Harness: domain.HarnessClaudeCode},
			Orchestrator: domain.RoleOverride{Harness: domain.HarnessClaudeCode},
		},
	}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	msg := &captureMessenger{}
	lcm := lifecycle.New(store, msg)
	mgr := sessionmanager.New(sessionmanager.Deps{
		Runtime: &stubRuntime{}, Agents: stubAgents{}, Workspace: &stubWorkspace{},
		Store: store, Messenger: msg, Lifecycle: lcm,
		LookPath: func(string) (string, error) { return "/usr/bin/true", nil },
	})
	lcm.SetCompletionTerminator(mgr)
	return &completionStack{store: store, sm: sessionsvc.New(mgr, store), mgr: mgr, lcm: lcm}
}

func (s *completionStack) status(t *testing.T, id domain.SessionID) domain.SessionStatus {
	t.Helper()
	sess, err := s.sm.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	return sess.Status
}

func TestFinishedTaskReadsCompletedAndStaysThatWay(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	st := newCompletionStack(t, dataDir)

	sess, _, _, err := st.mgr.Spawn(ctx, ports.SpawnConfig{
		ProjectID: "mer", Kind: domain.KindWorker, Branch: "b", Prompt: "do it",
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	launch := func() string {
		rec, _, err := st.store.GetSession(ctx, sess.ID)
		if err != nil {
			t.Fatalf("read session: %v", err)
		}
		return rec.Metadata.RuntimeLaunchID
	}()

	// The task works, then reports that its turn is over — the two hooks a
	// finished task actually produces.
	started := time.Now().UTC().Add(-2 * time.Hour)
	if err := st.lcm.ApplyActivitySignal(ctx, sess.ID, ports.ActivitySignal{
		Valid: true, State: domain.ActivityActive, Event: "user-prompt-submit",
		Timestamp: started, LaunchID: launch,
	}); err != nil {
		t.Fatalf("prompt signal: %v", err)
	}
	if got := st.status(t, sess.ID); got != domain.StatusWorking {
		t.Fatalf("status while working = %q, want %q", got, domain.StatusWorking)
	}
	if err := st.lcm.ApplyActivitySignal(ctx, sess.ID, ports.ActivitySignal{
		Valid: true, State: domain.ActivityIdle, Event: "stop",
		Timestamp: started.Add(time.Hour), LaunchID: launch,
	}); err != nil {
		t.Fatalf("stop signal: %v", err)
	}

	// 1. A successful task is Completed, and it is still alive: nothing was
	//    terminated to get there.
	if got := st.status(t, sess.ID); got != domain.StatusCompleted {
		t.Fatalf("status after the turn ended = %q, want %q", got, domain.StatusCompleted)
	}
	if rec, _, _ := st.store.GetSession(ctx, sess.ID); rec.IsTerminated {
		t.Fatal("a finished task was terminated; it must stay alive and answerable")
	}

	// 2. It stays Completed while nothing happens to it. The reaper's own view
	//    of a live-but-quiet session must not take the status away.
	if err := st.lcm.ApplyRuntimeObservation(ctx, sess.ID, ports.RuntimeFacts{
		Runtime: ports.ProbeAlive, Workload: ports.ProbeAlive,
	}); err != nil {
		t.Fatalf("runtime observation: %v", err)
	}
	if got := st.status(t, sess.ID); got != domain.StatusCompleted {
		t.Fatalf("status after an idle sweep = %q, want %q", got, domain.StatusCompleted)
	}

	// 3. It survives a daemon restart: close everything and rebuild the stack
	//    over the same database, which is all a restart is.
	if err := st.store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	restarted := newCompletionStack(t, dataDir)
	if got := restarted.status(t, sess.ID); got != domain.StatusCompleted {
		t.Fatalf("status after restart = %q, want %q", got, domain.StatusCompleted)
	}

	// 4. New work takes it out of Completed again.
	rec, _, err := restarted.store.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("read session: %v", err)
	}
	if err := restarted.lcm.ApplyActivitySignal(ctx, sess.ID, ports.ActivitySignal{
		Valid: true, State: domain.ActivityActive, Event: "user-prompt-submit",
		Timestamp: time.Now().UTC(), LaunchID: rec.Metadata.RuntimeLaunchID,
	}); err != nil {
		t.Fatalf("second prompt signal: %v", err)
	}
	if got := restarted.status(t, sess.ID); got != domain.StatusWorking {
		t.Fatalf("status after a new prompt = %q, want %q", got, domain.StatusWorking)
	}
}

// The counter-case, and the one that keeps Completed meaningful: a session that
// is merely quiet has no completion to report and stays Idle no matter how long
// it sits there.
func TestIdleTaskWithoutAReportedTurnEndStaysIdle(t *testing.T) {
	ctx := context.Background()
	st := newCompletionStack(t, t.TempDir())

	sess, _, _, err := st.mgr.Spawn(ctx, ports.SpawnConfig{
		ProjectID: "mer", Kind: domain.KindWorker, Branch: "b", Prompt: "do it",
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	rec, _, err := st.store.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("read session: %v", err)
	}

	// An untagged idle reading — an old CLI, or a runtime probe's view of a
	// quiet pane. It is not a report that any work finished.
	if err := st.lcm.ApplyActivitySignal(ctx, sess.ID, ports.ActivitySignal{
		Valid: true, State: domain.ActivityIdle,
		Timestamp: time.Now().UTC(), LaunchID: rec.Metadata.RuntimeLaunchID,
	}); err != nil {
		t.Fatalf("idle signal: %v", err)
	}

	if got := st.status(t, sess.ID); got != domain.StatusIdle {
		t.Fatalf("status of a quiet task = %q, want %q", got, domain.StatusIdle)
	}
	if stored, _, _ := st.store.GetSession(ctx, sess.ID); !stored.TurnCompletedAt.IsZero() {
		t.Fatalf("a quiet task recorded a completion receipt: %v", stored.TurnCompletedAt)
	}
}
