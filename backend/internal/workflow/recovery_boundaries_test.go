package workflow_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// Boundary A: outbox command persisted (status=pending) but Spawner.Spawn was
// never called (crash before dispatch attempt) -> recovery/re-dispatch calls
// Spawn exactly once, safely.
func TestRecoveryBoundaryA_PendingOutboxNeverSpawnedDispatchesOnce(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
	c, store, _ := newCoordinatorFull(spawner, sessionFacts, &fakeWorkspaceFacts{})
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	// Get the run to a state where the work step is "ready" with a plan
	// artifact recorded, but simulate a crash before the outbox row for the
	// spawn command exists at all — the boundary-A precondition explicitly
	// allows "outbox pending" as the persisted evidence; here we go one step
	// further and start from nothing persisted yet, which dispatchWorkStep
	// must handle identically (it creates the pending row itself).
	steps, _ := store.ListWorkflowSteps(ctx, created.Run.ID)
	var planStepID, workStepID string
	for _, s := range steps {
		switch s.Kind {
		case domain.WorkflowStepPlan:
			planStepID = s.ID
		case domain.WorkflowStepWork:
			workStepID = s.ID
		}
	}
	now := time.Now().UTC()
	if _, err := store.UpdateWorkflowRunState(ctx, created.Run.ID, domain.WorkflowRunPending, domain.WorkflowRunRunning, now); err != nil {
		t.Fatalf("force run running: %v", err)
	}
	if _, err := store.UpdateWorkflowStepState(ctx, planStepID, domain.WorkflowStepReady, domain.WorkflowStepRunning, now); err != nil {
		t.Fatalf("force plan running: %v", err)
	}
	if _, err := store.UpdateWorkflowStepState(ctx, planStepID, domain.WorkflowStepRunning, domain.WorkflowStepCompleted, now); err != nil {
		t.Fatalf("force plan completed: %v", err)
	}
	if _, err := store.UpdateWorkflowStepState(ctx, workStepID, domain.WorkflowStepPending, domain.WorkflowStepReady, now); err != nil {
		t.Fatalf("force work ready: %v", err)
	}
	// No outbox row exists yet for the work step's spawn command: this is the
	// "crash before dispatch attempt" precondition.

	if err := c.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if spawner.calls != 1 {
		t.Fatalf("spawner calls = %d, want exactly 1", spawner.calls)
	}
	got, err := c.GetRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if workStepFrom(got).Step.SessionID == nil {
		t.Fatalf("work step has no session id after recovery dispatch")
	}

	// Idempotent: reconciling again must not call Spawn a second time.
	if err := c.Reconcile(ctx); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	if spawner.calls != 1 {
		t.Fatalf("spawner calls after second Reconcile = %d, want still 1", spawner.calls)
	}
}

// Boundary B: Spawn succeeded (session exists with populated workspace,
// findable via natural key) but the crash happened before step.SessionID/
// checkpoint were written (outbox stuck at "dispatched") -> recovery adopts
// the found session via natural-key lookup, does NOT call Spawn again,
// backfills step.SessionID/attempt/checkpoint/outbox acknowledged.
func TestRecoveryBoundaryB_DispatchedOutboxAdoptsFoundSessionWithoutRespawning(t *testing.T) {
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
	issueID := domain.IssueID("workflow-step:" + workStepID)
	sessionFacts.put(domain.SessionRecord{
		ID: "sess-adopted", ProjectID: "proj-1", IssueID: issueID,
		Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"},
	})

	now := time.Now().UTC()
	outboxKey := "workflow-step-spawn:" + workStepID
	store.outbox[outboxKey] = domain.WorkflowOutboxEntry{
		ID: "wfo-preexisting", WorkflowRunID: created.Run.ID, WorkflowStepID: &workStepID,
		IdempotencyKey: outboxKey, CommandType: domain.WorkflowOutboxSpawnWorkerSession,
		Status: domain.WorkflowOutboxDispatched, Payload: "{}",
	}
	if _, err := store.UpdateWorkflowRunState(ctx, created.Run.ID, domain.WorkflowRunPending, domain.WorkflowRunRunning, now); err != nil {
		t.Fatalf("force run running: %v", err)
	}
	if _, err := store.UpdateWorkflowStepState(ctx, workStepID, domain.WorkflowStepPending, domain.WorkflowStepReady, now); err != nil {
		t.Fatalf("force work ready: %v", err)
	}
	if _, err := store.UpdateWorkflowStepState(ctx, workStepID, domain.WorkflowStepReady, domain.WorkflowStepRunning, now); err != nil {
		t.Fatalf("force work running: %v", err)
	}

	if err := c.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if spawner.calls != 0 {
		t.Fatalf("spawner calls = %d, want 0 (adopt, never respawn)", spawner.calls)
	}
	got, err := c.GetRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	work := workStepFrom(got)
	if work.Step.SessionID == nil || *work.Step.SessionID != "sess-adopted" {
		t.Fatalf("work step session id = %v, want sess-adopted", work.Step.SessionID)
	}
	attempts, err := store.ListWorkflowAttempts(ctx, workStepID)
	if err != nil || len(attempts) != 1 {
		t.Fatalf("attempts = %+v, err=%v, want exactly 1", attempts, err)
	}
	if entry := store.outbox[outboxKey]; entry.Status != domain.WorkflowOutboxAcknowledged {
		t.Fatalf("outbox status = %q, want acknowledged", entry.Status)
	}
}

// Boundary C/D: since prompt delivery lives inside Spawn itself in this
// codebase, a found session (adopted, boundary B) or a stale/duplicate
// dispatch call after full bookkeeping was already written (StartRun called
// again) must both be pure no-ops with respect to Spawn — never a second
// delivery attempt, never a second session.
func TestRecoveryBoundaryCD_StaleDispatchAfterFullBookkeepingIsNoOp(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
	c, _, _ := newCoordinatorFull(spawner, sessionFacts, &fakeWorkspaceFacts{})
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := c.StartRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if spawner.calls != 1 {
		t.Fatalf("spawner calls after first StartRun = %d, want 1", spawner.calls)
	}

	// A stale/duplicate StartRun call (guard #1 in the dispatch algorithm:
	// step.SessionID != nil short-circuits before even touching the outbox).
	if _, err := c.StartRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("duplicate StartRun: %v", err)
	}
	// A subsequent Reconcile pass must also be a no-op.
	if err := c.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if spawner.calls != 1 {
		t.Fatalf("spawner calls after duplicate StartRun + Reconcile = %d, want still 1", spawner.calls)
	}
}

// fakeSessionFactsAdapter lets the restart-simulation tests plug a
// process-lifetime fake SessionFacts into a fresh Coordinator built over the
// same real sqlite.Store handle, mirroring 8A's
// TestReconcileAfterRestartRealStore pattern.
func newRestartSimStore(t *testing.T) (*sqlite.Store, string) {
	t.Helper()
	dataDir := t.TempDir()
	store, err := sqlite.Open(dataDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, dataDir
}

// Boundary E: session exists, is not terminated, activity is active (worker
// genuinely still working) at boot recovery -> step stays running, run stays
// running — recovery must NOT force it to waiting/needs_attention just
// because the daemon restarted. This is the concrete behavior change from
// 8A's blanket "running -> waiting" rule, scoped to work steps only.
func TestRecoveryBoundaryE_ActiveSessionStaysRunningAcrossRestart(t *testing.T) {
	store, dataDir := newRestartSimStore(t)
	ctx := context.Background()
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "proj-e", Path: "/tmp/proj-e", RegisteredAt: time.Now().UTC().Truncate(time.Second)}); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	// workflow_steps.session_id has a real FK against sessions(id) (migration
	// 0094), so a boundary test against the real store needs an actual
	// sessions row to reference — seed one the way session_manager.Manager.Spawn
	// would, and have the fake Spawner return that same id.
	seedSession, err := store.CreateSession(ctx, domain.SessionRecord{
		ProjectID: "proj-e", Kind: domain.KindWorker,
		Activity: domain.Activity{State: domain.ActivityIdle, LastActivityAt: time.Now()},
		Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"},
	})
	if err != nil {
		t.Fatalf("seed session: %v", err)
	}

	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{rec: domain.SessionRecord{ID: seedSession.ID, Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
	coord1 := workflowcore.New(workflowcore.Deps{Store: store, Spawner: spawner, SessionFacts: sessionFacts, WorkspaceFacts: &fakeWorkspaceFacts{}})

	created, err := coord1.CreateRun(ctx, "proj-e", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	detail, err := coord1.StartRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	work := workStepFrom(detail)
	sessionFacts.put(domain.SessionRecord{
		ID: domain.SessionID(*work.Step.SessionID), ProjectID: "proj-e",
		Activity: domain.Activity{State: domain.ActivityActive}, IsTerminated: false,
		Metadata: domain.SessionMetadata{WorkspacePath: "/ws/wf", Branch: "ao/wf"},
	})
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	store2, err := sqlite.Open(dataDir)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = store2.Close() })
	spawner2 := &fakeSpawner{}
	coord2 := workflowcore.New(workflowcore.Deps{Store: store2, Spawner: spawner2, SessionFacts: sessionFacts, WorkspaceFacts: &fakeWorkspaceFacts{}})

	if err := coord2.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile after restart: %v", err)
	}
	if spawner2.calls != 0 {
		t.Fatalf("spawner calls on restart-reconcile = %d, want 0 (session already associated)", spawner2.calls)
	}
	got, err := coord2.GetRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GetRun after restart: %v", err)
	}
	if workStepFrom(got).Step.State != domain.WorkflowStepRunning {
		t.Fatalf("work step state after restart reconcile = %q, want still running (8A's blanket waiting rule must not apply to an actively-working session)", workStepFrom(got).Step.State)
	}
	if got.Run.State == domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state after restart reconcile = %q, want NOT needs_attention while the worker is genuinely active", got.Run.State)
	}
}

// Boundary F: session exists, is terminated, with commit evidence (HeadSHA !=
// baseSHA) that arrived after the daemon died mid-observation -> recovery
// reconciles it to completed/waiting+start_review, not stuck in limbo.
func TestRecoveryBoundaryF_TerminatedWithCommitEvidenceCompletesAcrossRestart(t *testing.T) {
	store, dataDir := newRestartSimStore(t)
	ctx := context.Background()
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "proj-f", Path: "/tmp/proj-f", RegisteredAt: time.Now().UTC().Truncate(time.Second)}); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	seedSession, err := store.CreateSession(ctx, domain.SessionRecord{
		ProjectID: "proj-f", Kind: domain.KindWorker,
		Activity: domain.Activity{State: domain.ActivityIdle, LastActivityAt: time.Now()},
		Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"},
	})
	if err != nil {
		t.Fatalf("seed session: %v", err)
	}

	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{rec: domain.SessionRecord{ID: seedSession.ID, Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{obs: ports.WorkspaceObservation{HeadSHA: "base-sha"}}
	clock1 := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	coord1 := workflowcore.New(workflowcore.Deps{
		Store: store, Spawner: spawner, SessionFacts: sessionFacts, WorkspaceFacts: workspaceFacts,
		Clock: func() time.Time { return clock1 },
	})

	created, err := coord1.CreateRun(ctx, "proj-f", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	detail, err := coord1.StartRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	work := workStepFrom(detail)
	sessionFacts.put(domain.SessionRecord{
		ID: domain.SessionID(*work.Step.SessionID), ProjectID: "proj-f",
		Activity: domain.Activity{State: domain.ActivityExited}, IsTerminated: true,
		Metadata: domain.SessionMetadata{WorkspacePath: "/ws/wf", Branch: "ao/wf"},
	})
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	store2, err := sqlite.Open(dataDir)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = store2.Close() })
	spawner2 := &fakeSpawner{}
	workspaceFacts2 := &fakeWorkspaceFacts{obs: ports.WorkspaceObservation{HeadSHA: "new-sha-after-restart"}}
	clock2 := clock1.Add(10 * time.Second) // clear observeWorkStep's ObserveWorkspace throttle window
	coord2 := workflowcore.New(workflowcore.Deps{
		Store: store2, Spawner: spawner2, SessionFacts: sessionFacts, WorkspaceFacts: workspaceFacts2,
		Clock: func() time.Time { return clock2 },
	})

	if err := coord2.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile after restart: %v", err)
	}
	if spawner2.calls != 0 {
		t.Fatalf("spawner calls on restart-reconcile = %d, want 0", spawner2.calls)
	}
	got, err := coord2.GetRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GetRun after restart: %v", err)
	}
	if workStepFrom(got).Step.State != domain.WorkflowStepCompleted {
		t.Fatalf("work step state after restart reconcile = %q, want completed", workStepFrom(got).Step.State)
	}
	if got.Run.State != domain.WorkflowRunWaiting {
		t.Fatalf("run state after restart reconcile = %q, want waiting", got.Run.State)
	}
	if got.NextAction != "start_review" {
		t.Fatalf("run next action = %q, want start_review", got.NextAction)
	}
}
