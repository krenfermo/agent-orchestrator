package workflow

// Checkpoint 8N.1 §H/§20: proves the wake scheduler/poller closure does not
// regress Checkpoint 8M.1's master-integration git-state propagation —
// task 2's real dispatch, resumed purely by the daemon poller after a real
// capacity wait (never a manual ContinueRun call from this test), must still
// see task 1's physically integrated source in its own worktree before
// anything else happens.

import (
	stdctx "context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/workspace/gitworktree"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	"github.com/aoagents/agent-orchestrator/backend/internal/workflow/wake"
)

// runDueOnceInline reproduces wakepoller.Poller.RunDueOnce's exact
// claim/resume/complete-or-fail sequence (wakepoller/poller.go). It cannot
// be imported directly from this file: wakepoller imports package workflow
// (for RunDetail/ErrNotFound/ErrAlreadyTerminal), and this file, being an
// internal test (package workflow, not workflow_test), would create an
// import cycle. wakepoller/poller_test.go and the external
// workflow_test-package wake_integration_test.go both already exercise the
// real wakepoller.Poller directly — this inline copy exists solely so this
// one master-integration test can call the unexported
// createSingleTaskRun/promoteTaskToIntegration/masterTaskBaseRef helpers
// that only package workflow's own test files can see.
func runDueOnceInline(ctx stdctx.Context, sched *wake.Scheduler, coord *Coordinator) int {
	claimed, err := sched.ClaimDue(ctx, "test-poller", 10)
	if err != nil {
		return 0
	}
	for _, sch := range claimed {
		_, resumeErr := coord.ContinueRun(ctx, string(sch.WorkflowRunID))
		if resumeErr == nil {
			_ = sched.Complete(ctx, sch.ID)
		} else {
			_, _ = sched.Fail(ctx, sch, resumeErr.Error())
		}
	}
	return len(claimed)
}

// masterWakeFakeClock is this file's own manually-advanced clock — distinct
// from workflow_test's fakeClock (a different, external test package this
// internal-test file cannot see).
type masterWakeFakeClock struct{ t time.Time }

func (c *masterWakeFakeClock) Now() time.Time          { return c.t }
func (c *masterWakeFakeClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

// worktreeSpawner is a Spawner that, on every call, actually materializes a
// real git worktree via the same gitworktree.Workspace adapter production
// dispatch uses (ws.Create with cfg.BaseRef as BaseBranch) — so this test
// can assert the resulting worktree's real file contents, not just that
// Spawn was called with the right config value.
type worktreeSpawner struct {
	ws    *gitworktree.Workspace
	store *sqlite.Store
	calls []ports.SpawnConfig
	infos []ports.WorkspaceInfo
}

func (s *worktreeSpawner) Spawn(ctx stdctx.Context, cfg ports.SpawnConfig) (domain.SessionRecord, int, int, error) {
	s.calls = append(s.calls, cfg)
	now := time.Now().UTC()
	sess, err := s.store.CreateSession(ctx, domain.SessionRecord{
		ProjectID: cfg.ProjectID, Kind: cfg.Kind, Harness: cfg.Harness, IssueID: cfg.IssueID,
		Activity: domain.Activity{State: domain.ActivityIdle}, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		return domain.SessionRecord{}, 0, 0, err
	}
	info, err := s.ws.Create(ctx, ports.WorkspaceConfig{
		ProjectID: cfg.ProjectID, SessionID: sess.ID,
		Branch: "ao/task-2-attempt-" + string(sess.ID), BaseBranch: cfg.BaseRef,
	})
	if err != nil {
		return domain.SessionRecord{}, 0, 0, err
	}
	s.infos = append(s.infos, info)
	sess.Metadata = domain.SessionMetadata{Branch: info.Branch, WorkspacePath: info.Path}
	return sess, len(cfg.Prompt), 0, nil
}

// TestMasterIntegration_CapacityWaitThenWake_Task2SeesTask1BeforeItWrites is
// Checkpoint 8N.1's E2E H/§20/§23: task 1 completes and is integrated; task
// 2's real dispatch (through StartRun -> routeWorkerDispatch, not the
// fixture's hand-seeded e2eDispatchTask shortcut) parks on capacity, gets a
// durable worker_capacity wake, and is resumed exclusively by the daemon
// poller (wakepoller.Poller.RunDueOnce — never a manual ContinueRun call
// from this test). Once resumed, its real worktree (created by the exact
// same BaseRef masterTaskBaseRef/attemptWorkHarness compute in production)
// must already contain task 1's helper.py.
func TestMasterIntegration_CapacityWaitThenWake_Task2SeesTask1BeforeItWrites(t *testing.T) {
	git := e2eGit(t)
	tmp := t.TempDir()
	repo := e2eOriginRepo(t, git, tmp)
	ws, err := gitworktree.New(gitworktree.Options{Binary: git, ManagedRoot: filepath.Join(tmp, "managed"), RepoResolver: gitworktree.StaticRepoResolver{"p": repo}})
	if err != nil {
		t.Fatalf("new gitworktree adapter: %v", err)
	}
	store := sqlitetest.MustOpen(t)
	ctx := stdctx.Background()
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "p", Path: repo, RegisteredAt: time.Now().UTC()}); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	clk := &masterWakeFakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	spawner := &worktreeSpawner{ws: ws, store: store}
	wakeSched := wake.New(store, clk.Now, newWakeIntIDSeqMaster(), wake.Config{})
	coord := New(Deps{
		Store: store, Projects: store, WorkspaceFacts: ws,
		IntegrationLocks: newLaneStub(),
		Spawner:          spawner, SessionFacts: store,
		WakeScheduler: wakeSched, Clock: clk.Now,
		NewID: newWakeIntIDSeqMaster(),
	})

	// Task 1: completes and integrates via the same simulated-output
	// convention master_integration_e2e_internal_test.go's own suite uses
	// for git-state propagation (not what's under test here).
	master := domain.WorkflowRun{ID: "wf-master", ProjectID: "p", Objective: "capacity-wait chain", State: domain.WorkflowRunRunning, PolicyVersion: policyVersionV1, PolicySnapshot: "{}", CreatedAt: clk.Now(), UpdatedAt: clk.Now()}
	if _, _, err := store.CreateWorkflowRun(ctx, master, nil); err != nil {
		t.Fatalf("seed master: %v", err)
	}
	task1, detail1, info1 := e2eDispatchTask(t, ctx, coord, store, ws, master, "task-1", "", map[string]string{
		"helper.py": "def helper():\n    return 42\n",
	})
	e2eCommitWork(t, info1.Path, "task-1")
	if err := coord.promoteTaskToIntegration(ctx, master, task1, detail1); err != nil {
		t.Fatalf("promote task 1: %v", err)
	}
	integrationRef := coord.masterTaskBaseRef(ctx, domain.WorkflowRun{ID: "probe", ParentWorkflowID: &master.ID})
	if integrationRef == "" {
		t.Fatal("expected a non-empty integration ref after task 1's promotion")
	}

	// Both worker harnesses in cooldown before task 2's real dispatch.
	reset := clk.Now().Add(30 * time.Minute)
	for _, h := range []domain.AgentHarness{domain.HarnessCodex, domain.HarnessClaudeCode} {
		if _, err := store.RecordAgentHealthEvent(ctx, domain.AgentHealthEvent{
			ID: "ahe-" + string(h), Harness: h, State: domain.AgentHealthCooldown,
			Reason: "test", CooldownUntil: &reset, CreatedAt: clk.Now(),
		}); err != nil {
			t.Fatalf("RecordAgentHealthEvent(%s): %v", h, err)
		}
	}

	// Task 2: a REAL child execution run (createSingleTaskRun, the same
	// unexported constructor reconcileMasterTasks itself uses), dispatched
	// for real through StartRun.
	taskID2 := "task-2"
	created2, err := coord.createSingleTaskRun(ctx, "p", "task 2", &master.ID, &taskID2)
	if err != nil {
		t.Fatalf("createSingleTaskRun task 2: %v", err)
	}
	detail2, err := coord.StartRun(ctx, created2.Run.ID)
	if err != nil {
		t.Fatalf("StartRun task 2: %v", err)
	}
	if detail2.Run.State != domain.WorkflowRunWaiting {
		t.Fatalf("task 2 run state = %q, want waiting", detail2.Run.State)
	}
	if len(spawner.calls) != 0 {
		t.Fatalf("spawner calls = %d, want 0 before capacity recovers", len(spawner.calls))
	}

	next, err := wakeSched.NextForRun(ctx, domain.WorkflowRunID(created2.Run.ID))
	if err != nil || next == nil {
		t.Fatalf("expected a durable wake for task 2, got %+v err=%v", next, err)
	}
	if next.Reason != wake.ReasonWorkerCapacity {
		t.Fatalf("wake reason = %q, want worker_capacity", next.Reason)
	}

	// Capacity recovers — the production fact source, never a forced state.
	if _, err := store.RecordAgentHealthEvent(ctx, domain.AgentHealthEvent{
		ID: "ahe-recovered", Harness: domain.HarnessClaudeCode, State: domain.AgentHealthAvailable,
		Reason: "recovered", CreatedAt: clk.Now(),
	}); err != nil {
		t.Fatalf("RecordAgentHealthEvent(recovered): %v", err)
	}

	// Resume PURELY via the daemon poller primitive — no ContinueRun/GetRun
	// call from this test.
	clk.Advance(next.ScheduledAt.Sub(clk.Now()) + time.Second)
	n := runDueOnceInline(ctx, wakeSched, coord)
	if n != 1 {
		t.Fatalf("claimed = %d, want exactly 1", n)
	}
	if len(spawner.calls) != 1 {
		t.Fatalf("spawner calls = %d, want exactly 1 (no duplicate dispatch)", len(spawner.calls))
	}
	if spawner.calls[0].BaseRef != integrationRef {
		t.Fatalf("task 2's real dispatch used BaseRef %q, want task 1's integration ref %q", spawner.calls[0].BaseRef, integrationRef)
	}

	// The actual physical proof: task 2's real worktree, materialized by the
	// wake-resumed dispatch, already has task 1's file.
	if _, err := os.Stat(filepath.Join(spawner.infos[0].Path, "helper.py")); err != nil {
		t.Fatalf("task 2's post-wake worktree missing task 1's helper.py: %v", err)
	}

	got, _, err := store.GetWorkflowRun(ctx, created2.Run.ID)
	if err != nil {
		t.Fatalf("GetWorkflowRun: %v", err)
	}
	if got.State != domain.WorkflowRunRunning {
		t.Fatalf("task 2 run state after wake-resumed dispatch = %q, want running", got.State)
	}
}

func newWakeIntIDSeqMaster() func() string {
	n := 0
	return func() string {
		n++
		return "mid" + string(rune('0'+n))
	}
}
