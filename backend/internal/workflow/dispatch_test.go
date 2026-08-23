package workflow_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// fakeClock lets tests control the coordinator's notion of "now", including
// advancing it past observeWorkStep's ObserveWorkspace throttle window.
type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time          { return c.t }
func (c *fakeClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

// fakeSpawner is a hand-rolled fake for workflowcore.Spawner (no real
// session_manager/Codex call in unit tests). When facts is set, a successful
// Spawn auto-registers the new session into it as active/not-terminated —
// mirroring how a real Spawn's session row is immediately visible through
// the same store SessionFacts reads from. Without this, tests that call
// GetRun/StartRun (which opportunistically observes a just-dispatched
// running work step) would see "session not found" and spuriously fail the
// step immediately after a successful dispatch, which is a fake-decoupling
// artifact, not real behavior. Tests that need a specific later activity
// state re-register (facts.put) after the fact, before their next
// GetRun/Reconcile call.
type fakeSpawner struct {
	calls int
	rec   domain.SessionRecord
	err   error
	facts *fakeSessionFacts
	// lastCfg captures the most recent Spawn call's full config (Checkpoint
	// 8P-B.1 tests assert on cfg.RuntimeEnv/cfg.Owner here).
	lastCfg ports.SpawnConfig
}

func (f *fakeSpawner) Spawn(_ context.Context, cfg ports.SpawnConfig) (domain.SessionRecord, int, int, error) {
	f.calls++
	f.lastCfg = cfg
	if f.err != nil {
		return domain.SessionRecord{}, 0, 0, f.err
	}
	rec := f.rec
	if rec.ID == "" {
		rec.ID = domain.SessionID(fmt.Sprintf("sess-%d", f.calls))
	}
	rec.ProjectID = cfg.ProjectID
	rec.Harness = cfg.Harness
	rec.Kind = cfg.Kind
	rec.IssueID = cfg.IssueID
	if f.facts != nil {
		registered := rec
		registered.Activity = domain.Activity{State: domain.ActivityActive}
		registered.IsTerminated = false
		f.facts.put(registered)
	}
	return rec, len(cfg.Prompt), 0, nil
}

// fakeSessionFacts is a hand-rolled fake for workflowcore.SessionFacts.
type fakeSessionFacts struct {
	byID    map[domain.SessionID]domain.SessionRecord
	byIssue map[string]domain.SessionRecord
}

func newFakeSessionFacts() *fakeSessionFacts {
	return &fakeSessionFacts{byID: map[domain.SessionID]domain.SessionRecord{}, byIssue: map[string]domain.SessionRecord{}}
}

func (f *fakeSessionFacts) put(rec domain.SessionRecord) {
	f.byID[rec.ID] = rec
	f.byIssue[string(rec.ProjectID)+"|"+string(rec.IssueID)] = rec
}

func (f *fakeSessionFacts) GetSession(_ context.Context, id domain.SessionID) (domain.SessionRecord, bool, error) {
	rec, ok := f.byID[id]
	return rec, ok, nil
}

func (f *fakeSessionFacts) FindSessionByProjectAndIssueID(_ context.Context, projectID domain.ProjectID, issueID domain.IssueID) (domain.SessionRecord, bool, error) {
	rec, ok := f.byIssue[string(projectID)+"|"+string(issueID)]
	return rec, ok, nil
}

// fakeWorkspaceFacts is a hand-rolled fake for workflowcore.WorkspaceFacts.
type fakeWorkspaceFacts struct {
	obs   ports.WorkspaceObservation
	err   error
	calls int
}

func (f *fakeWorkspaceFacts) MaterializeIntegrationCommit(_ context.Context, _ ports.WorkspaceInfo, _, _, _ string, _ []string) (string, string, bool, error) {
	return "", "", false, nil
}

func (f *fakeWorkspaceFacts) ObserveWorkspace(_ context.Context, _ ports.WorkspaceInfo) (ports.WorkspaceObservation, error) {
	f.calls++
	if f.err != nil {
		return ports.WorkspaceObservation{}, f.err
	}
	return f.obs, nil
}

func newCoordinatorFull(spawner workflowcore.Spawner, sessionFacts workflowcore.SessionFacts, workspaceFacts workflowcore.WorkspaceFacts) (*workflowcore.Coordinator, *fakeStore, *fakeClock) {
	store := newFakeStore()
	clk := &fakeClock{t: time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)}
	var idSeq int
	c := workflowcore.New(workflowcore.Deps{
		Store:          store,
		Spawner:        spawner,
		SessionFacts:   sessionFacts,
		WorkspaceFacts: workspaceFacts,
		Clock:          clk.Now,
		NewID: func() string {
			idSeq++
			return fmt.Sprintf("id%d", idSeq)
		},
	})
	return c, store, clk
}

func workStepFrom(detail workflowcore.RunDetail) workflowcore.StepDetail {
	for _, sd := range detail.Steps {
		if sd.Step.Kind == domain.WorkflowStepWork {
			return sd
		}
	}
	panic("no work step in run detail")
}

func planStepFrom(detail workflowcore.RunDetail) workflowcore.StepDetail {
	for _, sd := range detail.Steps {
		if sd.Step.Kind == domain.WorkflowStepPlan {
			return sd
		}
	}
	panic("no plan step in run detail")
}

func reviewStepFrom(detail workflowcore.RunDetail) workflowcore.StepDetail {
	for _, sd := range detail.Steps {
		if sd.Step.Kind == domain.WorkflowStepReview {
			return sd
		}
	}
	panic("no review step in run detail")
}

// Test 1: StartRun creates exactly one Codex session.
func TestStartRunCreatesExactlyOneCodexSession(t *testing.T) {
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}}
	c, _, _ := newCoordinatorFull(spawner, newFakeSessionFacts(), &fakeWorkspaceFacts{})
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	detail, err := c.StartRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if spawner.calls != 1 {
		t.Fatalf("spawner calls = %d, want 1", spawner.calls)
	}
	work := workStepFrom(detail)
	if work.Step.SessionID == nil || *work.Step.SessionID != "sess-1" {
		t.Fatalf("work step session id = %v, want sess-1", work.Step.SessionID)
	}
}

// Regression: discovered by the real Codex E2E run. dispatchFromPending
// updates the outbox row pending->dispatched but must also keep the
// in-memory entry.Status in sync, or the later pending->acknowledged CAS
// (which uses entry.Status as its expected value) silently no-ops against a
// DB row that has already moved to "dispatched" — leaving the outbox
// permanently stuck at "dispatched" instead of "acknowledged" even though
// the spawn genuinely succeeded.
func TestDispatchSuccessAdvancesOutboxToAcknowledged(t *testing.T) {
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}}
	c, store, _ := newCoordinatorFull(spawner, newFakeSessionFacts(), &fakeWorkspaceFacts{})
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := c.StartRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	found := false
	for _, entry := range store.outbox {
		if entry.CommandType != domain.WorkflowOutboxSpawnWorkerSession || entry.WorkflowRunID != created.Run.ID {
			continue
		}
		found = true
		if entry.Status != domain.WorkflowOutboxAcknowledged {
			t.Fatalf("outbox entry status = %q, want %q", entry.Status, domain.WorkflowOutboxAcknowledged)
		}
	}
	if !found {
		t.Fatalf("no spawn_worker_session outbox entry found for run %s", created.Run.ID)
	}
}

// Test 2: calling StartRun twice is idempotent (spawner call count stays 1).
func TestStartRunTwiceIsIdempotent(t *testing.T) {
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}}
	c, _, _ := newCoordinatorFull(spawner, newFakeSessionFacts(), &fakeWorkspaceFacts{})
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := c.StartRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("first StartRun: %v", err)
	}
	if _, err := c.StartRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("second StartRun: %v", err)
	}
	if spawner.calls != 1 {
		t.Fatalf("spawner calls after two StartRun calls = %d, want 1", spawner.calls)
	}
}

// Test 3: plan step transitions to completed with a well-formed PlanArtifact.
func TestStartRunPlanStepArtifactPersisted(t *testing.T) {
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}}
	c, _, _ := newCoordinatorFull(spawner, newFakeSessionFacts(), &fakeWorkspaceFacts{})
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	detail, err := c.StartRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	plan := planStepFrom(detail)
	if plan.Step.State != domain.WorkflowStepCompleted {
		t.Fatalf("plan step state = %q, want completed", plan.Step.State)
	}
	artifact, err := workflowcore.UnmarshalPlanArtifact(plan.Step.ArtifactJSON)
	if err != nil {
		t.Fatalf("UnmarshalPlanArtifact: %v", err)
	}
	if artifact.Objective != "ship the thing" {
		t.Fatalf("artifact objective = %q, want %q", artifact.Objective, "ship the thing")
	}
	if artifact.TaskPrompt == "" || len(artifact.AcceptanceCriteria) == 0 {
		t.Fatalf("artifact incomplete: %+v", artifact)
	}
}

// Test 5: exactly one workflow_attempt row is created for the successful dispatch.
func TestStartRunCreatesExactlyOneAttempt(t *testing.T) {
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}}
	c, store, _ := newCoordinatorFull(spawner, newFakeSessionFacts(), &fakeWorkspaceFacts{})
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	detail, err := c.StartRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	work := workStepFrom(detail)
	attempts, err := store.ListWorkflowAttempts(ctx, work.Step.ID)
	if err != nil {
		t.Fatalf("ListWorkflowAttempts: %v", err)
	}
	if len(attempts) != 1 {
		t.Fatalf("attempts for work step = %d, want 1: %+v", len(attempts), attempts)
	}

	// Calling StartRun again must not create a second attempt.
	if _, err := c.StartRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("second StartRun: %v", err)
	}
	attempts, err = store.ListWorkflowAttempts(ctx, work.Step.ID)
	if err != nil {
		t.Fatalf("ListWorkflowAttempts after second StartRun: %v", err)
	}
	if len(attempts) != 1 {
		t.Fatalf("attempts after second StartRun = %d, want still 1", len(attempts))
	}
}

// Test 6: outbox idempotency — two dispatch calls with the same step never
// produce two spawn_worker_session outbox rows, and never cause a second Spawn.
func TestDispatchOutboxIdempotencyNeverDoubleSpawns(t *testing.T) {
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}}
	c, store, _ := newCoordinatorFull(spawner, newFakeSessionFacts(), &fakeWorkspaceFacts{})
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := c.StartRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if _, err := c.GetRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if spawner.calls != 1 {
		t.Fatalf("spawner calls = %d, want 1", spawner.calls)
	}
	spawnEntries := 0
	for _, entry := range store.outbox {
		if entry.CommandType == domain.WorkflowOutboxSpawnWorkerSession && entry.WorkflowRunID == created.Run.ID {
			spawnEntries++
		}
	}
	if spawnEntries != 1 {
		t.Fatalf("spawn_worker_session outbox entries = %d, want 1", spawnEntries)
	}
}

// TestDispatchFailureRecordsFailedAttemptAndSchedulesBoundedRetry covers Spawn
// itself failing with an untyped, unclassifiable error. Checkpoint 8H replaced
// the old blanket session_create_failed default with the real provider-neutral
// classifier (failure_classifier.go): an untyped error with no typed sentinel
// or known rate-limit/capacity/auth phrase classifies as agent_start_failed
// with unknown certainty — still accurate ("dispatch failed to start the
// agent") but not eligible for automatic failover, since an unclassified
// failure must never silently trigger a provider switch.
//
// What it must NOT do any more is strand the run. This test used to assert
// step -> failed and run -> needs_attention on the very first such failure,
// which is exactly what turned wf-57f90ff2's momentary tmux hiccup into a
// permanently stuck objective. A failure AO can prove happened before any
// worker existed now takes worker_launch_recovery.go's bounded retry: the
// failed attempt row is still recorded (audit history is never lost), and the
// run stays running with a durable, retryable launch record behind it.
func TestDispatchFailureRecordsFailedAttemptAndSchedulesBoundedRetry(t *testing.T) {
	spawner := &fakeSpawner{err: errors.New("boom")}
	c, store, _ := newCoordinatorFull(spawner, newFakeSessionFacts(), &fakeWorkspaceFacts{})
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	detail, err := c.StartRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if detail.Run.State == domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want the run left running for its own bounded retry", detail.Run.State)
	}
	work := workStepFrom(detail)
	if work.Step.State == domain.WorkflowStepFailed {
		t.Fatal("work step must not be terminally failed while an automatic retry is still owed")
	}
	if spawner.calls != 1 {
		t.Fatalf("spawn calls = %d, want exactly 1 (the retry is scheduled, not run inline)", spawner.calls)
	}
	attempts, err := store.ListWorkflowAttempts(ctx, work.Step.ID)
	if err != nil || len(attempts) != 1 {
		t.Fatalf("attempts = %+v, err=%v, want exactly 1", attempts, err)
	}
	if attempts[0].Outcome != domain.WorkflowAttemptFailed || attempts[0].ErrorClass != domain.WorkflowErrorAgentStartFailed {
		t.Fatalf("attempt = %+v, want failed/agent_start_failed", attempts[0])
	}
	if !hasRetryableWorkerLaunchRecord(t, store, created.Run.ID) {
		t.Fatal("expected a durable, retryable worker_launch_error record")
	}
}

// hasRetryableWorkerLaunchRecord reports whether the run's ledger carries a
// worker-launch failure record that AO has declared retryable.
func hasRetryableWorkerLaunchRecord(t *testing.T, store interface {
	ListWorkflowCheckpoints(context.Context, string) ([]domain.WorkflowCheckpoint, error)
}, runID string) bool {
	t.Helper()
	cps, err := store.ListWorkflowCheckpoints(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListWorkflowCheckpoints: %v", err)
	}
	for _, cp := range cps {
		if cp.DurablePhase == "worker_launch_error" && strings.Contains(cp.RetryState, `"retryable":true`) {
			return true
		}
	}
	return false
}

// TestWorkerNeverSignalsEventuallyFailsWithoutDoubleDispatch is Checkpoint
// 8P-E.3's real-topology regression: a real browser-driven autonomous run
// (provider profile authenticated, Spawn() returning success, health
// recorded available/dispatch-succeeded) reproduced a worker whose runtime
// process launched but never got past its own startup — no FirstSignalAt,
// ever — leaving the work step "running" and the master's autonomous
// heartbeat rescheduling itself forever (69 attempts and counting) while the
// UI misreported "Waiting for capacity". This proves the fixed contract:
// before the startup grace period elapses, AO keeps waiting (no false
// failure for an ordinarily-slow cold start, no duplicate Spawn); after it
// elapses, the step deterministically fails as agent_start_failed and the
// run reaches needs_attention — never staying stuck, never spawning a
// second worker to "retry" on its own.
func TestWorkerNeverSignalsEventuallyFailsWithoutDoubleDispatch(t *testing.T) {
	facts := newFakeSessionFacts()
	spawner := &fakeSpawner{
		rec:   domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}},
		facts: facts,
	}
	c, store, clk := newCoordinatorFull(spawner, facts, &fakeWorkspaceFacts{})
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	detail, err := c.StartRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if spawner.calls != 1 {
		t.Fatalf("spawner calls after dispatch = %d, want 1", spawner.calls)
	}
	work := workStepFrom(detail)
	sessionID := domain.SessionID(*work.Step.SessionID)

	// The real runtime process launched (Spawn succeeded) but never
	// progressed past its own startup: idle, no FirstSignalAt, exactly what
	// the stuck ao-autonomous-test-2 session showed.
	facts.put(domain.SessionRecord{
		ID:       sessionID,
		Activity: domain.Activity{State: domain.ActivityIdle},
		Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"},
	})

	// Still within the startup grace period: no verdict yet, and critically
	// no second worker gets spawned to "retry" a merely-slow cold start.
	clk.Advance(2 * time.Minute)
	got, err := c.GetRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GetRun (within grace): %v", err)
	}
	if workStepFrom(got).Step.State != domain.WorkflowStepRunning {
		t.Fatalf("work step state within grace = %q, want still running", workStepFrom(got).Step.State)
	}
	if got.Run.State == domain.WorkflowRunNeedsAttention {
		t.Fatalf("run reached needs_attention before the startup grace period elapsed")
	}
	if spawner.calls != 1 {
		t.Fatalf("spawner calls within grace = %d, want still 1 (no duplicate worker)", spawner.calls)
	}

	// Past the startup grace period with FirstSignalAt still unset: AO must
	// stop waiting and reach a deterministic, non-capacity failure state.
	clk.Advance(9 * time.Minute)
	got, err = c.GetRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GetRun (past grace): %v", err)
	}
	if got.Run.State != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state past grace = %q, want needs_attention", got.Run.State)
	}
	work = workStepFrom(got)
	if work.Step.State != domain.WorkflowStepFailed {
		t.Fatalf("work step state past grace = %q, want failed", work.Step.State)
	}
	attempts, err := store.ListWorkflowAttempts(ctx, work.Step.ID)
	if err != nil || len(attempts) != 1 {
		t.Fatalf("attempts = %+v, err=%v, want exactly 1 (no duplicate dispatch)", attempts, err)
	}
	if attempts[0].Outcome != domain.WorkflowAttemptFailed || attempts[0].ErrorClass != domain.WorkflowErrorAgentStartFailed {
		t.Fatalf("attempt = %+v, want failed/agent_start_failed (not capacity_exhausted or any other class)", attempts[0])
	}
	if spawner.calls != 1 {
		t.Fatalf("spawner calls past grace = %d, want still 1 (never a second worker spawned to self-retry)", spawner.calls)
	}

	// A further GetRun (the daemon poller re-entering a still-non-terminal
	// run) must not spawn again either: the run is needs_attention, which is
	// exactly the state maybeScheduleAutonomousHeartbeat treats as "stop
	// rescheduling" -- proving the fix also bounds the wake-explosion this
	// checkpoint found (69 attempts and climbing on the real run).
	clk.Advance(time.Hour)
	if _, err := c.GetRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("GetRun (later poll): %v", err)
	}
	if spawner.calls != 1 {
		t.Fatalf("spawner calls after a later poll = %d, want still 1", spawner.calls)
	}
}

// TestWorkerWithFirstSignalNeverForceFailedByStartupTimeout is the
// successful-path counterpart to
// TestWorkerNeverSignalsEventuallyFailsWithoutDoubleDispatch: once the
// worker actually produces a first signal and real work evidence lands, it
// completes normally — the startup-grace failure never applies once
// FirstSignalAt is set, however long the task subsequently runs.
func TestWorkerWithFirstSignalNeverForceFailedByStartupTimeout(t *testing.T) {
	facts := newFakeSessionFacts()
	spawner := &fakeSpawner{
		rec:   domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}},
		facts: facts,
	}
	workspaceFacts := &fakeWorkspaceFacts{obs: ports.WorkspaceObservation{HeadSHA: "new-sha"}}
	c, _, clk := newCoordinatorFull(spawner, facts, workspaceFacts)
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	detail, err := c.StartRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	sessionID := domain.SessionID(*workStepFrom(detail).Step.SessionID)

	// The worker actually started (a hook fired, FirstSignalAt populated),
	// went idle, and left real, git-verified work behind.
	facts.put(domain.SessionRecord{
		ID:            sessionID,
		Activity:      domain.Activity{State: domain.ActivityIdle},
		Metadata:      domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"},
		FirstSignalAt: clk.Now(),
	})

	// Advance well past workStepFirstSignalTimeout: a worker that genuinely
	// started must never be force-failed by the startup-grace check just
	// because the call happens to land long after dispatch.
	clk.Advance(time.Hour)
	got, err := c.GetRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	work := workStepFrom(got)
	if work.Step.State != domain.WorkflowStepCompleted {
		t.Fatalf("work step state = %q, want completed (real work evidence, FirstSignalAt was set)", work.Step.State)
	}
	for _, a := range work.Attempts {
		if a.ErrorClass == domain.WorkflowErrorAgentStartFailed {
			t.Fatalf("attempt wrongly classified agent_start_failed despite a real first signal: %+v", a)
		}
	}
	if spawner.calls != 1 {
		t.Fatalf("spawner calls = %d, want 1", spawner.calls)
	}
}

// TestIdleWorkerWithNoEvidenceFinalizesItsAttempt is the regression test for
// the attempt-finalization gap found in production run
// wf-a9b8d51a-3cab-4026-8573-60f8b9759b17 (child of
// wf-1ee7fcb8-53d0-4c16-8039-fefdcb4909ba). Its DB row read
// error_class=ambiguous_worker_state with outcome AND finished_at both still
// NULL: observeWorkStep's idle-with-no-evidence branch lands the step on
// Waiting rather than a terminal state, so the pre-fix finalization switch
// (which only matched Completed/Failed) refined the error class but never
// resolved the attempt. Because observeWorkStep only re-enters while the step
// is Running and nothing resumes a work step out of Waiting, that left the
// attempt permanently reading as in-flight — and hid it from the UI, since
// task_checkpoint_summary only surfaces ActiveErrors for failed attempts.
func TestIdleWorkerWithNoEvidenceFinalizesItsAttempt(t *testing.T) {
	facts := newFakeSessionFacts()
	spawner := &fakeSpawner{
		rec:   domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}},
		facts: facts,
	}
	// An observable workspace that reports no evidence whatsoever: no new
	// HEAD, nothing dirty/staged/untracked. This is the real ambiguity —
	// the worker is alive and signalled, but produced nothing AO can verify.
	c, store, clk := newCoordinatorFull(spawner, facts, &fakeWorkspaceFacts{})
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	detail, err := c.StartRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	work := workStepFrom(detail)
	sessionID := domain.SessionID(*work.Step.SessionID)

	// The worker genuinely started (a hook fired, so FirstSignalAt is set —
	// this is NOT the startup-failure case) and then went idle empty-handed.
	facts.put(domain.SessionRecord{
		ID:            sessionID,
		Activity:      domain.Activity{State: domain.ActivityIdle},
		Metadata:      domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"},
		FirstSignalAt: clk.Now(),
	})

	clk.Advance(time.Minute)
	got, err := c.GetRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.Run.State != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention", got.Run.State)
	}
	work = workStepFrom(got)
	if work.Step.State != domain.WorkflowStepWaiting {
		t.Fatalf("work step state = %q, want waiting", work.Step.State)
	}

	attempts, err := store.ListWorkflowAttempts(ctx, work.Step.ID)
	if err != nil || len(attempts) != 1 {
		t.Fatalf("attempts = %+v, err=%v, want exactly 1", attempts, err)
	}
	a := attempts[0]
	if a.ErrorClass != domain.WorkflowErrorAmbiguousWorkerState {
		t.Fatalf("attempt error class = %q, want ambiguous_worker_state", a.ErrorClass)
	}
	// The gap itself: both of these were unset before the fix.
	if a.Outcome != domain.WorkflowAttemptFailed {
		t.Fatalf("attempt outcome = %q, want failed — an attempt AO has given up on must not keep reading as in-flight", a.Outcome)
	}
	if a.FinishedAt == nil {
		t.Fatal("attempt finished_at is still NULL — AO stopped observing this attempt, so it must be stamped finished")
	}

	// Giving up must not silently spawn a replacement worker.
	if spawner.calls != 1 {
		t.Fatalf("spawner calls = %d, want 1", spawner.calls)
	}
}

// Ambiguous branch 1: outbox found "dispatched" but the natural-key session
// lookup finds nothing at all -> waiting/needs_attention, never a silent
// success, never a second Spawn call.
func TestAmbiguousDispatchedNoSessionFoundNeedsAttention(t *testing.T) {
	spawner := &fakeSpawner{}
	c, store, _ := newCoordinatorFull(spawner, newFakeSessionFacts(), &fakeWorkspaceFacts{})
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	steps, _ := store.ListWorkflowSteps(ctx, created.Run.ID)
	var workStepID string
	for _, s := range steps {
		if s.Kind == domain.WorkflowStepWork {
			workStepID = s.ID
		}
	}
	// Simulate a crash exactly at "we are about to call Spawn" (outbox marked
	// dispatched) with Spawn never actually invoked and no session ever created.
	store.outbox["workflow-step-spawn:"+workStepID] = domain.WorkflowOutboxEntry{
		ID: "wfo-preexisting", WorkflowRunID: created.Run.ID, WorkflowStepID: &workStepID,
		IdempotencyKey: "workflow-step-spawn:" + workStepID, CommandType: domain.WorkflowOutboxSpawnWorkerSession,
		Status: domain.WorkflowOutboxDispatched, Payload: "{}",
	}
	if _, err := store.UpdateWorkflowRunState(ctx, created.Run.ID, domain.WorkflowRunPending, domain.WorkflowRunRunning, time.Now()); err != nil {
		t.Fatalf("force running: %v", err)
	}
	if _, err := store.UpdateWorkflowStepState(ctx, workStepID, domain.WorkflowStepPending, domain.WorkflowStepReady, time.Now()); err != nil {
		t.Fatalf("force work step ready: %v", err)
	}
	if _, err := store.UpdateWorkflowStepState(ctx, workStepID, domain.WorkflowStepReady, domain.WorkflowStepRunning, time.Now()); err != nil {
		t.Fatalf("force work step running: %v", err)
	}

	if err := c.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if spawner.calls != 0 {
		t.Fatalf("spawner calls = %d, want 0 (never call Spawn from the ambiguous branch)", spawner.calls)
	}
	got, err := c.GetRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.Run.State != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention", got.Run.State)
	}
	if workStepFrom(got).Step.State != domain.WorkflowStepWaiting {
		t.Fatalf("work step state = %q, want waiting", workStepFrom(got).Step.State)
	}
}

// Ambiguous branch 2: outbox "dispatched", natural-key lookup finds an
// orphan session row with no workspace (the partial-seed crash gap) ->
// waiting/needs_attention, outbox stays "dispatched", never a second Spawn.
func TestAmbiguousDispatchedOrphanSessionNoWorkspaceNeedsAttention(t *testing.T) {
	spawner := &fakeSpawner{}
	sessionFacts := newFakeSessionFacts()
	c, store, _ := newCoordinatorFull(spawner, sessionFacts, &fakeWorkspaceFacts{})
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	steps, _ := store.ListWorkflowSteps(ctx, created.Run.ID)
	var workStepID string
	for _, s := range steps {
		if s.Kind == domain.WorkflowStepWork {
			workStepID = s.ID
		}
	}
	issueID := "workflow-step:" + workStepID
	sessionFacts.put(domain.SessionRecord{ID: "orphan-1", ProjectID: "proj-1", IssueID: domain.IssueID(issueID)}) // empty workspace/branch

	outboxKey := "workflow-step-spawn:" + workStepID
	store.outbox[outboxKey] = domain.WorkflowOutboxEntry{
		ID: "wfo-preexisting", WorkflowRunID: created.Run.ID, WorkflowStepID: &workStepID,
		IdempotencyKey: outboxKey, CommandType: domain.WorkflowOutboxSpawnWorkerSession,
		Status: domain.WorkflowOutboxDispatched, Payload: "{}",
	}
	if _, err := store.UpdateWorkflowRunState(ctx, created.Run.ID, domain.WorkflowRunPending, domain.WorkflowRunRunning, time.Now()); err != nil {
		t.Fatalf("force running: %v", err)
	}
	if _, err := store.UpdateWorkflowStepState(ctx, workStepID, domain.WorkflowStepPending, domain.WorkflowStepReady, time.Now()); err != nil {
		t.Fatalf("force work step ready: %v", err)
	}
	if _, err := store.UpdateWorkflowStepState(ctx, workStepID, domain.WorkflowStepReady, domain.WorkflowStepRunning, time.Now()); err != nil {
		t.Fatalf("force work step running: %v", err)
	}

	if err := c.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if spawner.calls != 0 {
		t.Fatalf("spawner calls = %d, want 0", spawner.calls)
	}
	got, err := c.GetRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.Run.State != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention", got.Run.State)
	}
	if workStepFrom(got).Step.State != domain.WorkflowStepWaiting {
		t.Fatalf("work step state = %q, want waiting", workStepFrom(got).Step.State)
	}
	if workStepFrom(got).Step.SessionID != nil {
		t.Fatalf("work step must not adopt an orphan session with no workspace")
	}
	if entry := store.outbox[outboxKey]; entry.Status != domain.WorkflowOutboxDispatched {
		t.Fatalf("outbox status = %q, want unchanged dispatched", entry.Status)
	}
}

// Test 13: cancelling a run with an active work-step session leaves the
// session untouched (by construction: no stop/kill port is wired) and no
// further dispatch happens even if GetRun-triggered observation runs after.
func TestCancelRunLeavesWorkerSessionUntouchedAndStopsFurtherDispatch(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
	c, _, _ := newCoordinatorFull(spawner, sessionFacts, &fakeWorkspaceFacts{})
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	detail, err := c.StartRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	work := workStepFrom(detail)
	sessionFacts.put(domain.SessionRecord{
		ID: domain.SessionID(*work.Step.SessionID), ProjectID: "proj-1",
		Activity: domain.Activity{State: domain.ActivityActive}, IsTerminated: false,
	})

	cancelled, err := c.CancelRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("CancelRun: %v", err)
	}
	if cancelled.Run.State != domain.WorkflowRunCancelled {
		t.Fatalf("run state = %q, want cancelled", cancelled.Run.State)
	}
	if workStepFrom(cancelled).Step.State != domain.WorkflowStepCancelled {
		t.Fatalf("work step state = %q, want cancelled", workStepFrom(cancelled).Step.State)
	}
	if workStepFrom(cancelled).Step.SessionID == nil {
		t.Fatalf("cancelled work step lost its session id; the session record itself must be left alone, not this reference")
	}

	// A later GetRun (which would normally opportunistically observe a
	// running work step) must not re-dispatch or touch anything: the run/step
	// are terminal now.
	if _, err := c.GetRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("GetRun after cancel: %v", err)
	}
	if spawner.calls != 1 {
		t.Fatalf("spawner calls after cancel+GetRun = %d, want still 1 (no re-dispatch)", spawner.calls)
	}
}

// Test 14: after a full successful work completion, review must still be
// exactly pending — no code path in 8B touches it.
func TestReviewStepUntouchedAfterWorkCompletion(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{obs: ports.WorkspaceObservation{HeadSHA: "base-sha"}}
	c, _, clk := newCoordinatorFull(spawner, sessionFacts, workspaceFacts)
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	detail, err := c.StartRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	work := workStepFrom(detail)
	sessionFacts.put(domain.SessionRecord{
		ID: domain.SessionID(*work.Step.SessionID), ProjectID: "proj-1",
		Activity: domain.Activity{State: domain.ActivityIdle}, IsTerminated: false,
		Metadata: domain.SessionMetadata{WorkspacePath: "/ws/wf", Branch: "ao/wf"},
	})
	workspaceFacts.obs = ports.WorkspaceObservation{HeadSHA: "new-sha"} // committed work
	clk.Advance(10 * time.Second)                                       // clear the observation throttle

	got, err := c.GetRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if workStepFrom(got).Step.State != domain.WorkflowStepCompleted {
		t.Fatalf("work step state = %q, want completed", workStepFrom(got).Step.State)
	}
	review := reviewStepFrom(got)
	if review.Step.State != domain.WorkflowStepPending {
		t.Fatalf("review step state = %q, want still pending (8B never touches review)", review.Step.State)
	}
	if review.Step.ReviewRunID != nil {
		t.Fatalf("review step has a review_run_id %v; no code path in 8B can create one", review.Step.ReviewRunID)
	}
}

// Regression: discovered by Checkpoint 8C's real E2E run. The checkpoint
// observeWorkStep writes when a work step transitions to completed must
// still carry Branch/WorktreePath forward from session facts — a caller
// resolving "the latest checkpoint for this step" (as Checkpoint 8C's review
// dispatch does, to find the worktree to launch the reviewer against) would
// otherwise silently lose them the moment work observation supersedes the
// earlier "worker_dispatched" checkpoint that did carry them, and the real
// reviewer launch failed with "workspace path is required" as a direct
// result.
func TestWorkStepCompletionCheckpointCarriesBranchAndWorktreePath(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{obs: ports.WorkspaceObservation{HeadSHA: "base-sha"}}
	c, store, clk := newCoordinatorFull(spawner, sessionFacts, workspaceFacts)
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	detail, err := c.StartRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	work := workStepFrom(detail)
	sessionFacts.put(domain.SessionRecord{
		ID: domain.SessionID(*work.Step.SessionID), ProjectID: "proj-1",
		Activity: domain.Activity{State: domain.ActivityIdle}, IsTerminated: false,
		Metadata: domain.SessionMetadata{WorkspacePath: "/ws/wf", Branch: "ao/wf"},
	})
	workspaceFacts.obs = ports.WorkspaceObservation{Dirty: true} // uncommitted work evidence
	clk.Advance(10 * time.Second)                                // clear the observation throttle

	got, err := c.GetRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if workStepFrom(got).Step.State != domain.WorkflowStepCompleted {
		t.Fatalf("work step state = %q, want completed", workStepFrom(got).Step.State)
	}

	cp, ok, err := store.GetLatestWorkflowCheckpointByStep(ctx, work.Step.ID)
	if err != nil {
		t.Fatalf("GetLatestWorkflowCheckpointByStep: %v", err)
	}
	if !ok {
		t.Fatalf("no checkpoint found for work step %s", work.Step.ID)
	}
	if cp.WorktreePath != "/ws/wf" {
		t.Fatalf("latest checkpoint WorktreePath = %q, want /ws/wf", cp.WorktreePath)
	}
	if cp.Branch != "ao/wf" {
		t.Fatalf("latest checkpoint Branch = %q, want ao/wf", cp.Branch)
	}
}
