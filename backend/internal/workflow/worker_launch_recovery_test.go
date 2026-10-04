package workflow_test

// Regression coverage for the wf-57f90ff2 incident: a work step whose worker
// never launched.
//
// The durable shape on disk was:
//
//	workflow_steps      work = failed
//	workflow_outbox     spawn_worker_session = failed, agent_start_failed
//	workflow_runs       needs_attention
//	newest checkpoint   dispatch_failed / "work dispatch failed
//	                    (agent_start_failed): spawn agent-orchestrator-29:
//	                    runtime: tmux runtime: set status ...: no such session"
//
// and nothing — not a poll, not a restart, not Continue — could move it.
//
// Everything below runs against a real *sqlite.Store and the real Coordinator
// dispatch/recovery paths. The only fakes are the Spawner (a real agent process
// is not a unit test's business) and the clock.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowports "github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
	"github.com/aoagents/agent-orchestrator/backend/internal/workflow/wake"
	"github.com/aoagents/agent-orchestrator/backend/internal/workflow/wakepoller"
)

// errTmuxNoSuchSession is the verbatim runtime error from the incident. It is
// used as an ordinary input, never matched on: nothing in the fix knows this
// string, and these tests would pass just as well with any other runtime error.
var errTmuxNoSuchSession = errors.New(
	"spawn agent-orchestrator-29: runtime: tmux runtime: set status agent-orchestrator-29: exit status 1: no such session: agent-orchestrator-29")

// launchSpawner is a Spawner whose failures are programmable per call and whose
// successes write a real session row (the production FK from
// workflow_steps.session_id to sessions.id is enforced by the real store).
type launchSpawner struct {
	store *sqlite.Store
	// failWith is returned for the first failCount calls; afterwards Spawn
	// succeeds. failCount < 0 means "fail forever".
	failWith  error
	failCount int
	calls     []workflowports.SpawnConfig
}

func (s *launchSpawner) Spawn(ctx context.Context, cfg workflowports.SpawnConfig) (domain.SessionRecord, int, int, error) {
	s.calls = append(s.calls, cfg)
	if s.failWith != nil && (s.failCount < 0 || len(s.calls) <= s.failCount) {
		return domain.SessionRecord{}, 0, 0, s.failWith
	}
	rec := domain.SessionRecord{
		ID:        domain.SessionID("sess-" + strconv.Itoa(len(s.calls))),
		ProjectID: cfg.ProjectID,
		Kind:      cfg.Kind,
		Harness:   cfg.Harness,
		IssueID:   cfg.IssueID,
		Activity:  domain.Activity{State: domain.ActivityIdle},
		Metadata:  domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"},
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	created, err := s.store.CreateSession(ctx, rec)
	if err != nil {
		return domain.SessionRecord{}, 0, 0, err
	}
	return created, len(cfg.Prompt), 0, nil
}

// launchFixture is a single non-master ("child") run on a real store, wired the
// way the daemon wires one: real wake scheduler, real poller, real store as
// both Store and SessionFacts.
type launchFixture struct {
	t       *testing.T
	ctx     context.Context
	store   *sqlite.Store
	clk     *fakeClock
	wake    *wake.Scheduler
	poller  *wakepoller.Poller
	coord   *workflowcore.Coordinator
	spawner *launchSpawner
	// branchLocks is the ownership gate under test in the direct_branch
	// safety cases; nil in every other fixture (pre-8P-E.11 behavior).
	branchLocks workflowcore.BranchLocks
	runID       string
	workID      string
}

func newLaunchFixture(t *testing.T, spawn *launchSpawner) *launchFixture {
	return newLaunchFixtureWithLocks(t, spawn, nil)
}

// newLaunchFixtureWithLocks is newLaunchFixture with a branch-lock dependency
// wired, so tests can prove a retry or a reopen still passes through the same
// direct_branch/worktree ownership gate an ordinary first dispatch does.
func newLaunchFixtureWithLocks(t *testing.T, spawn *launchSpawner, locks workflowcore.BranchLocks) *launchFixture {
	t.Helper()
	store := sqlitetest.MustOpen(t)
	ctx := context.Background()
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "proj-1", Path: t.TempDir(), RegisteredAt: time.Now().UTC()}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	f := &launchFixture{t: t, ctx: ctx, store: store, spawner: spawn}
	f.clk = &fakeClock{t: time.Date(2026, 8, 23, 14, 55, 0, 0, time.UTC)}
	spawn.store = store
	f.wake = wake.New(store, f.clk.Now, launchIDSeq("wk"), wake.Config{})
	f.coord = workflowcore.New(workflowcore.Deps{
		Store:         store,
		Projects:      store,
		Spawner:       spawn,
		SessionFacts:  store,
		WakeScheduler: f.wake,
		BranchLocks:   locks,
		Clock:         f.clk.Now,
		NewID:         launchIDSeq("id"),
	})
	f.branchLocks = locks
	f.poller = wakepoller.New(f.wake, f.coord, wakepoller.Config{Clock: f.clk.Now})
	return f
}

// restart builds a brand-new Coordinator over the same store, which is what a
// daemon restart actually is: no in-memory state survives, only the rows.
func (f *launchFixture) restart() {
	f.t.Helper()
	f.coord = workflowcore.New(workflowcore.Deps{
		Store:         f.store,
		Projects:      f.store,
		Spawner:       f.spawner,
		SessionFacts:  f.store,
		WakeScheduler: f.wake,
		BranchLocks:   f.branchLocks,
		Clock:         f.clk.Now,
		NewID:         launchIDSeq("id2"),
	})
	f.poller = wakepoller.New(f.wake, f.coord, wakepoller.Config{Clock: f.clk.Now})
}

func launchIDSeq(prefix string) func() string {
	n := 0
	return func() string {
		n++
		return fmt.Sprintf("%s%d", prefix, n)
	}
}

// start creates and starts the run through the real entry points.
func (f *launchFixture) start() {
	f.t.Helper()
	created, err := f.coord.CreateRun(f.ctx, "proj-1", "Ownership-aware branch locking and parallel dispatch")
	if err != nil {
		f.t.Fatalf("CreateRun: %v", err)
	}
	f.runID = created.Run.ID
	if _, err := f.coord.StartRun(f.ctx, f.runID); err != nil {
		f.t.Fatalf("StartRun: %v", err)
	}
	f.workID = f.workStep().ID
}

func (f *launchFixture) run() domain.WorkflowRun {
	f.t.Helper()
	run, ok, err := f.store.GetWorkflowRun(f.ctx, f.runID)
	if err != nil || !ok {
		f.t.Fatalf("GetWorkflowRun: %v ok=%v", err, ok)
	}
	return run
}

func (f *launchFixture) workStep() domain.WorkflowStep {
	f.t.Helper()
	steps, err := f.store.ListWorkflowSteps(f.ctx, f.runID)
	if err != nil {
		f.t.Fatalf("ListWorkflowSteps: %v", err)
	}
	for _, s := range steps {
		if s.Kind == domain.WorkflowStepWork {
			return s
		}
	}
	f.t.Fatal("run has no work step")
	return domain.WorkflowStep{}
}

func (f *launchFixture) outbox() domain.WorkflowOutboxEntry {
	f.t.Helper()
	entries, err := f.store.ListWorkflowOutboxByRun(f.ctx, f.runID)
	if err != nil {
		f.t.Fatalf("ListWorkflowOutboxByRun: %v", err)
	}
	for _, e := range entries {
		if e.CommandType == domain.WorkflowOutboxSpawnWorkerSession {
			return e
		}
	}
	f.t.Fatal("run has no spawn_worker_session outbox entry")
	return domain.WorkflowOutboxEntry{}
}

func (f *launchFixture) outboxCount() int {
	f.t.Helper()
	entries, err := f.store.ListWorkflowOutboxByRun(f.ctx, f.runID)
	if err != nil {
		f.t.Fatalf("ListWorkflowOutboxByRun: %v", err)
	}
	n := 0
	for _, e := range entries {
		if e.CommandType == domain.WorkflowOutboxSpawnWorkerSession {
			n++
		}
	}
	return n
}

func (f *launchFixture) checkpointPhases() []string {
	f.t.Helper()
	cps, err := f.store.ListWorkflowCheckpoints(f.ctx, f.runID)
	if err != nil {
		f.t.Fatalf("ListWorkflowCheckpoints: %v", err)
	}
	out := make([]string, 0, len(cps))
	for _, cp := range cps {
		out = append(out, cp.DurablePhase)
	}
	return out
}

func (f *launchFixture) hasPhase(phase string) bool {
	for _, p := range f.checkpointPhases() {
		if p == phase {
			return true
		}
	}
	return false
}

func (f *launchFixture) sessionCount() int {
	f.t.Helper()
	sessions, err := f.store.ListSessions(f.ctx, "proj-1")
	if err != nil {
		f.t.Fatalf("ListSessions: %v", err)
	}
	return len(sessions)
}

// ---------------------------------------------------------------------------
// B. A confirmed pre-work spawn failure retries — exactly once per due window.
// ---------------------------------------------------------------------------

func TestWorkerLaunchFailure_TransientSpawnFailureRetriesExactlyOnce(t *testing.T) {
	spawn := &launchSpawner{failWith: errTmuxNoSuchSession, failCount: 1}
	f := newLaunchFixture(t, spawn)
	f.start()

	// The first dispatch failed. The run must NOT be stranded, the step must
	// not be terminal, and no second Spawn may have been attempted inline.
	if len(spawn.calls) != 1 {
		t.Fatalf("spawn calls after the first failure = %d, want 1", len(spawn.calls))
	}
	if got := f.run().State; got == domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q: a transient pre-work launch failure must not strand the run", got)
	}
	if got := f.workStep().State; got.Terminal() {
		t.Fatalf("work step state = %q, want non-terminal while a retry is owed", got)
	}
	if got := f.outbox().Status; got != domain.WorkflowOutboxPending {
		t.Fatalf("outbox status = %q, want pending (the same entry, re-armed)", got)
	}
	if !f.hasPhase("worker_launch_error") {
		t.Fatalf("no durable worker_launch_error record; phases = %v", f.checkpointPhases())
	}

	// Before the retry delay elapses, nothing may re-dispatch — not a boot
	// reconcile, not another poll. Otherwise the whole budget burns inside one
	// second and the transient condition never gets a chance to clear.
	if err := f.coord.Reconcile(f.ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(spawn.calls) != 1 {
		t.Fatalf("spawn calls after an early reconcile = %d, want still 1", len(spawn.calls))
	}

	// A durable wake must be what drives the retry, headlessly.
	next, err := f.wake.NextForRun(f.ctx, domain.WorkflowRunID(f.runID))
	if err != nil || next == nil {
		t.Fatalf("expected a durable wake for the launch retry, got %+v err=%v", next, err)
	}
	f.clk.Advance(2 * time.Minute)
	n, err := f.poller.RunDueOnce(f.ctx)
	if err != nil {
		t.Fatalf("RunDueOnce: %v", err)
	}
	if n != 1 {
		t.Fatalf("wakes claimed = %d, want 1", n)
	}

	if len(spawn.calls) != 2 {
		t.Fatalf("spawn calls after the wake = %d, want exactly 2 (one retry, not a storm)", len(spawn.calls))
	}
	step := f.workStep()
	if step.SessionID == nil {
		t.Fatal("the retry succeeded but the work step was not linked to its session")
	}
	if got := f.outbox().Status; got != domain.WorkflowOutboxAcknowledged {
		t.Fatalf("outbox status = %q, want acknowledged", got)
	}
	if f.outboxCount() != 1 {
		t.Fatalf("spawn outbox entries = %d, want exactly 1 (retries reuse the same idempotency key)", f.outboxCount())
	}
	if f.sessionCount() != 1 {
		t.Fatalf("sessions created = %d, want exactly 1", f.sessionCount())
	}
}

// ---------------------------------------------------------------------------
// C + K. Repeated reconcile/Continue never duplicates a worker or a session.
// ---------------------------------------------------------------------------

func TestWorkerLaunchRetry_RepeatedReconcileAndContinueNeverDuplicateAWorker(t *testing.T) {
	spawn := &launchSpawner{failWith: errTmuxNoSuchSession, failCount: 1}
	f := newLaunchFixture(t, spawn)
	f.start()

	f.clk.Advance(2 * time.Minute)
	if _, err := f.poller.RunDueOnce(f.ctx); err != nil {
		t.Fatalf("RunDueOnce: %v", err)
	}
	if len(spawn.calls) != 2 {
		t.Fatalf("spawn calls after the retry = %d, want 2", len(spawn.calls))
	}

	// 50 alternating reconcile/Continue passes over the recovered state.
	for i := 0; i < 50; i++ {
		f.clk.Advance(time.Minute)
		if i%2 == 0 {
			if err := f.coord.Reconcile(f.ctx); err != nil {
				t.Fatalf("Reconcile %d: %v", i, err)
			}
			continue
		}
		if _, err := f.coord.ContinueRun(f.ctx, f.runID); err != nil {
			t.Fatalf("ContinueRun %d: %v", i, err)
		}
	}

	if len(spawn.calls) != 2 {
		t.Fatalf("spawn calls after 50 passes = %d, want still 2", len(spawn.calls))
	}
	if f.sessionCount() != 1 {
		t.Fatalf("sessions after 50 passes = %d, want 1", f.sessionCount())
	}
	if f.outboxCount() != 1 {
		t.Fatalf("spawn outbox entries after 50 passes = %d, want 1", f.outboxCount())
	}
	attempts, err := f.store.ListWorkflowAttempts(f.ctx, f.workID)
	if err != nil {
		t.Fatalf("ListWorkflowAttempts: %v", err)
	}
	// One failed attempt (the launch that never started) and one open attempt
	// (the worker that did). Nothing accumulates per poll.
	if len(attempts) != 2 {
		t.Fatalf("attempts after 50 passes = %d, want exactly 2", len(attempts))
	}
}

// ---------------------------------------------------------------------------
// D. A daemon restart between the failure and the retry is safe: the retry
//    state is entirely on disk, and the new process neither loses it nor
//    double-spawns because of it.
// ---------------------------------------------------------------------------

func TestWorkerLaunchRetry_SurvivesDaemonRestartWithoutDuplicating(t *testing.T) {
	spawn := &launchSpawner{failWith: errTmuxNoSuchSession, failCount: 1}
	f := newLaunchFixture(t, spawn)
	f.start()
	if len(spawn.calls) != 1 {
		t.Fatalf("spawn calls = %d, want 1", len(spawn.calls))
	}

	f.restart()

	// A fresh process's boot reconcile must respect the durable retry pacing it
	// has never seen in memory.
	if err := f.coord.Reconcile(f.ctx); err != nil {
		t.Fatalf("Reconcile after restart: %v", err)
	}
	if len(spawn.calls) != 1 {
		t.Fatalf("spawn calls after a restart inside the retry window = %d, want still 1", len(spawn.calls))
	}

	f.clk.Advance(2 * time.Minute)
	if err := f.coord.Reconcile(f.ctx); err != nil {
		t.Fatalf("Reconcile after the retry became due: %v", err)
	}
	if len(spawn.calls) != 2 {
		t.Fatalf("spawn calls after the retry became due = %d, want 2", len(spawn.calls))
	}
	// And the restart must not have cost the run its single-session guarantee.
	if f.sessionCount() != 1 || f.outboxCount() != 1 {
		t.Fatalf("sessions=%d outbox=%d, want 1 and 1", f.sessionCount(), f.outboxCount())
	}
}

// ---------------------------------------------------------------------------
// E. An AMBIGUOUS spawn outcome is never resolved by launching another worker.
// ---------------------------------------------------------------------------

func TestWorkerLaunch_AmbiguousDispatchNeverSpawnsASecondWorker(t *testing.T) {
	spawn := &launchSpawner{}
	f := newLaunchFixture(t, spawn)
	f.startWithoutDispatch()

	// The shape a crash between "about to Spawn" and "Spawn returned" leaves
	// behind: the command is durably `dispatched`, the step is running, and no
	// session exists — so AO genuinely cannot tell whether a worker was started.
	f.seedAmbiguousDispatch()

	f.clk.Advance(10 * time.Minute)
	f.restart()
	for i := 0; i < 5; i++ {
		if _, err := f.coord.ContinueRun(f.ctx, f.runID); err != nil {
			t.Fatalf("ContinueRun %d: %v", i, err)
		}
	}

	if len(spawn.calls) != 0 {
		t.Fatalf("spawn calls = %d, want 0: an ambiguous dispatch must never be resolved by starting another worker", len(spawn.calls))
	}
	// It must be escalated as ambiguous, not silently retried and not silently
	// declared failed.
	if !f.hasPhase("worker_dispatch_ambiguous") {
		t.Fatalf("expected the ambiguity to be escalated; phases = %v", f.checkpointPhases())
	}
	if f.hasPhase("worker_launch_human_retry") {
		t.Fatal("an ambiguous dispatch must never be reopened as a confirmed pre-work failure")
	}
}

// ---------------------------------------------------------------------------
// F. When a session for this step's natural key DOES exist, Continue adopts it
//    instead of retrying — even though the durable record says the launch
//    failed. Positive evidence beats a stale record, always.
// ---------------------------------------------------------------------------

func TestWorkerLaunchRecovery_AdoptsAnExistingSessionInsteadOfRespawning(t *testing.T) {
	spawn := &launchSpawner{failWith: errTmuxNoSuchSession, failCount: -1}
	f := newLaunchFixture(t, spawn)
	f.start()
	f.exhaustLaunchBudget()

	if got := f.workStep().State; got != domain.WorkflowStepFailed {
		t.Fatalf("work step state = %q, want failed after the budget ran out", got)
	}
	spawnsBefore := len(spawn.calls)

	// A worker session for this step's natural key turns up after all — the
	// exact situation in which respawning would double-run the task.
	issueID := domain.IssueID("workflow-step:" + f.workID)
	adopted, err := f.store.CreateSession(f.ctx, domain.SessionRecord{
		ID: "adopted-1", ProjectID: "proj-1", Kind: domain.KindWorker,
		Harness: domain.HarnessClaudeCode, IssueID: issueID,
		Activity:  domain.Activity{State: domain.ActivityActive},
		Metadata:  domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if _, err := f.coord.ContinueRun(f.ctx, f.runID); err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}

	if len(spawn.calls) != spawnsBefore {
		t.Fatalf("spawn calls = %d, want still %d: an existing session must be adopted, never duplicated",
			len(spawn.calls), spawnsBefore)
	}
	step := f.workStep()
	if step.SessionID == nil || *step.SessionID != string(adopted.ID) {
		got := "<nil>"
		if step.SessionID != nil {
			got = *step.SessionID
		}
		t.Fatalf("work step session = %q, want the adopted session", got)
	}
	if got := f.run().State; got == domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want the stop cleared once a live worker was adopted", got)
	}
}

// exhaustLaunchBudget drives the run through every automatic retry AO allows,
// using only the production wake/reconcile paths, until the run is genuinely
// out of budget.
func (f *launchFixture) exhaustLaunchBudget() {
	f.t.Helper()
	for i := 0; i < 12; i++ {
		if f.run().State == domain.WorkflowRunNeedsAttention {
			return
		}
		// Repeated launch failures also feed provider health, which can park the
		// run on capacity before the launch budget is spent. That is its own
		// (correct) mechanism and its own tests; keep the providers healthy here
		// through the production fact source so what runs out is the thing under
		// test — the launch retry budget.
		f.markProvidersAvailable(i)
		f.clk.Advance(2 * time.Minute)
		if _, err := f.poller.RunDueOnce(f.ctx); err != nil {
			f.t.Fatalf("RunDueOnce: %v", err)
		}
		if err := f.coord.Reconcile(f.ctx); err != nil {
			f.t.Fatalf("Reconcile: %v", err)
		}
	}
	f.t.Fatalf("the launch retry budget never ran out; run state = %q", f.run().State)
}

// markProvidersAvailable records a real AgentHealthEvent saying both harnesses
// are usable right now — the same durable fact a successful dispatch records.
func (f *launchFixture) markProvidersAvailable(seq int) {
	f.t.Helper()
	for _, h := range []domain.AgentHarness{domain.HarnessCodex, domain.HarnessClaudeCode} {
		if _, err := f.store.RecordAgentHealthEvent(f.ctx, domain.AgentHealthEvent{
			ID:      fmt.Sprintf("ahe-%s-%d", h, seq),
			Harness: h, State: domain.AgentHealthAvailable,
			Reason: "test", CreatedAt: f.clk.Now(),
		}); err != nil {
			f.t.Fatalf("RecordAgentHealthEvent(%s): %v", h, err)
		}
	}
}

// ---------------------------------------------------------------------------
// G. The budget is real: a failure that keeps failing reaches a truthful
//    needs_attention with a reason that names what actually happened.
// ---------------------------------------------------------------------------

func TestWorkerLaunchRetry_BudgetExhaustionReachesTruthfulNeedsAttention(t *testing.T) {
	spawn := &launchSpawner{failWith: errTmuxNoSuchSession, failCount: -1}
	f := newLaunchFixture(t, spawn)
	f.start()
	f.exhaustLaunchBudget()

	if got := f.run().State; got != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention once the budget is spent", got)
	}
	if got := f.workStep().State; got != domain.WorkflowStepFailed {
		t.Fatalf("work step state = %q, want failed", got)
	}
	if got := f.outbox().Status; got != domain.WorkflowOutboxFailed {
		t.Fatalf("outbox status = %q, want failed", got)
	}
	if !f.hasPhase("worker_launch_retries_exhausted") {
		t.Fatalf("expected the honest 'every retry was used' stop; phases = %v", f.checkpointPhases())
	}
	// Bounded means bounded: the spawner must not have been called an unbounded
	// number of times to get here.
	if len(spawn.calls) > 4 {
		t.Fatalf("spawn calls = %d, want a small bounded number", len(spawn.calls))
	}
	// And the stop must reach the user as a decision with a concrete action,
	// not as something AO is still quietly retrying.
	detail, err := f.coord.GetRun(f.ctx, f.runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	att := workflowcore.ClassifyAttention(detail, nil, workflowcore.PhaseNeedsAttention)
	if att.Attention != workflowcore.AttentionHuman || att.Reason != "worker_launch_retries_exhausted" || att.Action == "" {
		t.Fatalf("attention = %+v, want worker_launch_retries_exhausted with a concrete action", att)
	}
}

// ---------------------------------------------------------------------------
// H. Auth/config failures are NEVER retried as transient startup.
// ---------------------------------------------------------------------------

func TestWorkerLaunch_NonRetryableAuthFailureIsNotRetried(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"auth", workflowports.ErrChatAuthRequired},
		{"provider profile", workflowports.ErrProviderProfileRequired},
		{"binary missing", workflowports.ErrAgentBinaryNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spawn := &launchSpawner{failWith: tc.err, failCount: -1}
			f := newLaunchFixture(t, spawn)
			f.start()

			if got := f.run().State; got != domain.WorkflowRunNeedsAttention {
				t.Fatalf("run state = %q, want needs_attention immediately: credentials and installation are not hiccups", got)
			}
			if got := f.workStep().State; got != domain.WorkflowStepFailed {
				t.Fatalf("work step state = %q, want failed", got)
			}
			if f.hasPhase("worker_launch_retry") {
				t.Fatalf("a non-retryable failure must never schedule a transient retry; phases = %v", f.checkpointPhases())
			}
			if next, err := f.wake.NextForRun(f.ctx, domain.WorkflowRunID(f.runID)); err == nil && next != nil && next.Reason == wake.ReasonTransientRetry {
				t.Fatal("a non-retryable failure must not schedule a transient_retry wake")
			}
			// Repeated polls/reconciles must not quietly retry it either.
			before := len(spawn.calls)
			for i := 0; i < 5; i++ {
				f.clk.Advance(10 * time.Minute)
				if err := f.coord.Reconcile(f.ctx); err != nil {
					t.Fatalf("Reconcile: %v", err)
				}
			}
			if len(spawn.calls) != before {
				t.Fatalf("spawn calls = %d, want still %d", len(spawn.calls), before)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// I. The already-persisted historical shape recovers through one ordinary
//    Continue — no database surgery, no knowledge of this run or this error.
// ---------------------------------------------------------------------------

// seedHistoricalDispatchFailure writes the exact durable rows wf-57f90ff2 has
// on disk, using the same store API the old binary wrote them through: an
// outbox entry that reached `dispatched` and then `failed` with
// agent_start_failed, a failed attempt, a terminally failed work step, a run in
// needs_attention, and a `dispatch_failed` checkpoint carrying the incident's
// own next_action. Deliberately NO worker_launch_error record — this is what a
// run stopped by the OLD code looks like, which is the whole point.
func (f *launchFixture) seedHistoricalDispatchFailure() {
	f.t.Helper()
	ctx, now := f.ctx, f.clk.Now()
	step := f.workStep()

	entry, _, err := f.store.EnqueueWorkflowOutboxEntry(ctx, domain.WorkflowOutboxEntry{
		ID: "wfo-historical", WorkflowRunID: f.runID, WorkflowStepID: &step.ID,
		IdempotencyKey: "workflow-step-spawn:" + step.ID,
		CommandType:    domain.WorkflowOutboxSpawnWorkerSession,
		Payload:        `{"projectId":"proj-1","harness":"codex","issueId":"workflow-step:` + step.ID + `"}`,
		CreatedAt:      now,
	})
	if err != nil {
		f.t.Fatalf("seed outbox: %v", err)
	}
	for _, next := range []domain.WorkflowOutboxStatus{domain.WorkflowOutboxDispatched, domain.WorkflowOutboxFailed} {
		if _, err := f.store.UpdateWorkflowOutboxStatus(ctx, entry.ID, entry.Status, next, now, "agent_start_failed"); err != nil {
			f.t.Fatalf("seed outbox -> %s: %v", next, err)
		}
		entry.Status = next
	}

	attempt, err := f.store.CreateWorkflowAttempt(ctx, "wfa-historical", step.ID, "claude-code", "", now)
	if err != nil {
		f.t.Fatalf("seed attempt: %v", err)
	}
	if err := f.store.UpdateWorkflowAttemptOutcome(ctx, attempt.ID, now,
		domain.WorkflowAttemptFailed, domain.WorkflowErrorAgentStartFailed); err != nil {
		f.t.Fatalf("seed attempt outcome: %v", err)
	}

	for _, next := range []domain.WorkflowStepState{domain.WorkflowStepRunning, domain.WorkflowStepFailed} {
		if _, err := f.store.UpdateWorkflowStepState(ctx, step.ID, step.State, next, now); err != nil {
			f.t.Fatalf("seed step -> %s: %v", next, err)
		}
		step.State = next
	}
	if _, err := f.store.UpdateWorkflowRunState(ctx, f.runID,
		domain.WorkflowRunRunning, domain.WorkflowRunNeedsAttention, now); err != nil {
		f.t.Fatalf("seed run state: %v", err)
	}
	if _, err := f.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID: "wfc-historical", WorkflowRunID: f.runID, WorkflowStepID: &step.ID, ProjectID: "proj-1",
		NextAction: "work dispatch failed (agent_start_failed): spawn agent-orchestrator-29: " +
			"runtime: tmux runtime: set status agent-orchestrator-29: exit status 1: no such session: agent-orchestrator-29",
		DurablePhase: "dispatch_failed", PayloadVersion: "v1", RetryState: "{}", CreatedAt: now,
	}); err != nil {
		f.t.Fatalf("seed checkpoint: %v", err)
	}
}

func TestWorkerLaunchRecovery_ContinueRecoversTheHistoricalDispatchFailure(t *testing.T) {
	// No spawner during seeding: the run must reach the historical shape
	// without this test having dispatched anything of its own.
	spawn := &launchSpawner{}
	f := newLaunchFixture(t, spawn)
	f.startWithoutDispatch()
	f.seedHistoricalDispatchFailure()

	// Sanity: this is the incident's shape, not a convenient approximation.
	if got := f.workStep().State; got != domain.WorkflowStepFailed {
		t.Fatalf("seeded work step = %q, want failed", got)
	}
	if got := f.outbox().Status; got != domain.WorkflowOutboxFailed {
		t.Fatalf("seeded outbox = %q, want failed", got)
	}
	if got := f.run().State; got != domain.WorkflowRunNeedsAttention {
		t.Fatalf("seeded run = %q, want needs_attention", got)
	}

	// A brand-new process (the fixed daemon) reads only these rows.
	f.restart()
	f.spawner.store = f.store

	// Polling must change nothing: only an explicit Continue may reopen a
	// terminal state.
	if _, err := f.coord.GetRun(f.ctx, f.runID); err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if err := f.coord.Reconcile(f.ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(spawn.calls) != 0 {
		t.Fatalf("spawn calls from polling = %d, want 0", len(spawn.calls))
	}

	// One ordinary Continue.
	if _, err := f.coord.ContinueRun(f.ctx, f.runID); err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}

	if len(spawn.calls) != 1 {
		t.Fatalf("spawn calls after one Continue = %d, want exactly 1", len(spawn.calls))
	}
	step := f.workStep()
	if step.State != domain.WorkflowStepRunning || step.SessionID == nil {
		t.Fatalf("work step = %+v, want running with a session", step)
	}
	if got := f.run().State; got != domain.WorkflowRunRunning {
		t.Fatalf("run state = %q, want running", got)
	}
	if got := f.outbox().Status; got != domain.WorkflowOutboxAcknowledged {
		t.Fatalf("outbox status = %q, want acknowledged", got)
	}
	if f.outboxCount() != 1 {
		t.Fatalf("spawn outbox entries = %d, want 1 (the SAME entry was reopened)", f.outboxCount())
	}
	// The reopen must be recorded, and must say it recognised the legacy shape
	// rather than this particular run.
	cps, err := f.store.ListWorkflowCheckpoints(f.ctx, f.runID)
	if err != nil {
		t.Fatalf("ListWorkflowCheckpoints: %v", err)
	}
	found := false
	for _, cp := range cps {
		if cp.DurablePhase == "worker_launch_human_retry" && strings.Contains(cp.RetryState, "legacy_dispatch_failed") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected an audited legacy reopen; phases = %v", f.checkpointPhases())
	}

	// K, on the historical shape: repeated Continue never starts a second worker.
	for i := 0; i < 50; i++ {
		f.clk.Advance(time.Minute)
		if _, err := f.coord.ContinueRun(f.ctx, f.runID); err != nil {
			t.Fatalf("ContinueRun %d: %v", i, err)
		}
	}
	if len(spawn.calls) != 1 {
		t.Fatalf("spawn calls after 50 further Continues = %d, want still 1", len(spawn.calls))
	}
	if f.sessionCount() != 1 {
		t.Fatalf("sessions = %d, want 1", f.sessionCount())
	}
}

// startWithoutDispatch brings a run to "work step ready, run running" using the
// real StartRun, but with the coordinator's spawner temporarily unwired so no
// dispatch is attempted. That is exactly the state the incident's run was in
// the instant before its work dispatch.
func (f *launchFixture) startWithoutDispatch() {
	f.t.Helper()
	noSpawn := workflowcore.New(workflowcore.Deps{
		Store: f.store, Projects: f.store, SessionFacts: f.store,
		WakeScheduler: f.wake, BranchLocks: f.branchLocks,
		Clock: f.clk.Now, NewID: launchIDSeq("seed"),
	})
	created, err := noSpawn.CreateRun(f.ctx, "proj-1", "Ownership-aware branch locking and parallel dispatch")
	if err != nil {
		f.t.Fatalf("CreateRun: %v", err)
	}
	f.runID = created.Run.ID
	if _, err := noSpawn.StartRun(f.ctx, f.runID); err != nil {
		f.t.Fatalf("StartRun: %v", err)
	}
	f.workID = f.workStep().ID
}

// The human reopen is bounded too: a person pressing Continue on a cause that
// was never transient must not be an unbounded session factory.
func TestWorkerLaunchRecovery_HumanReopenIsBounded(t *testing.T) {
	spawn := &launchSpawner{failWith: errTmuxNoSuchSession, failCount: -1}
	f := newLaunchFixture(t, spawn)
	f.start()
	f.exhaustLaunchBudget()

	for i := 0; i < 25; i++ {
		f.markProvidersAvailable(100 + i)
		f.clk.Advance(5 * time.Minute)
		if _, err := f.coord.ContinueRun(f.ctx, f.runID); err != nil {
			t.Fatalf("ContinueRun %d: %v", i, err)
		}
		if _, err := f.poller.RunDueOnce(f.ctx); err != nil {
			t.Fatalf("RunDueOnce %d: %v", i, err)
		}
	}
	// Every reopen costs at most one automatic budget's worth of spawns, and
	// there are at most maxWorkerLaunchRecoveryGenerations reopens.
	if len(spawn.calls) > 16 {
		t.Fatalf("spawn calls after 25 Continues = %d, want a bounded number", len(spawn.calls))
	}
	if got := f.run().State; got != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention once reopens are exhausted", got)
	}
	if f.sessionCount() != 0 {
		t.Fatalf("sessions = %d, want 0 — every spawn failed", f.sessionCount())
	}
}

// seedAmbiguousDispatch writes the durable state a daemon crash between the
// outbox CAS and Spawn's return leaves: the spawn command is `dispatched`, the
// work step is running, and no session exists anywhere.
func (f *launchFixture) seedAmbiguousDispatch() {
	f.t.Helper()
	ctx, now := f.ctx, f.clk.Now()
	step := f.workStep()
	entry, _, err := f.store.EnqueueWorkflowOutboxEntry(ctx, domain.WorkflowOutboxEntry{
		ID: "wfo-ambiguous", WorkflowRunID: f.runID, WorkflowStepID: &step.ID,
		IdempotencyKey: "workflow-step-spawn:" + step.ID,
		CommandType:    domain.WorkflowOutboxSpawnWorkerSession,
		Payload:        `{"projectId":"proj-1","harness":"codex","issueId":"workflow-step:` + step.ID + `"}`,
		CreatedAt:      now,
	})
	if err != nil {
		f.t.Fatalf("seed outbox: %v", err)
	}
	if _, err := f.store.UpdateWorkflowOutboxStatus(ctx, entry.ID, entry.Status, domain.WorkflowOutboxDispatched, now, ""); err != nil {
		f.t.Fatalf("seed outbox -> dispatched: %v", err)
	}
	if _, err := f.store.UpdateWorkflowStepState(ctx, step.ID, step.State, domain.WorkflowStepRunning, now); err != nil {
		f.t.Fatalf("seed step -> running: %v", err)
	}
}
