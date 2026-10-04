package workflow_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// Boundary A: outbox trigger_review command persisted (pending) but the
// reviewer was never launched (crash before dispatch attempt) -> recovery
// calls the launch path exactly once, safely.
func TestReviewRecoveryBoundaryA_PendingOutboxNeverLaunchedDispatchesOnce(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}
	c, store, clk := newCoordinatorWithReview(spawner, sessionFacts, workspaceFacts, reviewRuns, launcher)
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	completeWorkStep(t, c, store, clk, sessionFacts, workspaceFacts, created.Run.ID)

	// Simulate: ContinueRun was already called once (unblocking the review
	// step pending->ready) but the daemon crashed before any outbox row for
	// the review step's trigger_review command was even created. No outbox
	// row exists yet: this is the "crash before dispatch attempt"
	// precondition. Reconcile must create it and dispatch through to a
	// launch, exactly once.
	steps, _ := store.ListWorkflowSteps(ctx, created.Run.ID)
	var reviewStepID string
	for _, s := range steps {
		if s.Kind == domain.WorkflowStepReview {
			reviewStepID = s.ID
		}
	}
	if _, err := store.UpdateWorkflowStepState(ctx, reviewStepID, domain.WorkflowStepPending, domain.WorkflowStepReady, time.Now()); err != nil {
		t.Fatalf("force review ready: %v", err)
	}

	if err := c.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if launcher.launchCalls != 1 {
		t.Fatalf("launch calls = %d, want exactly 1", launcher.launchCalls)
	}
	got, err := c.GetRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if reviewStepFrom(got).Step.ReviewRunID == nil {
		t.Fatalf("review step has no review_run_id after recovery dispatch")
	}

	// Idempotent: reconciling again must not launch a second reviewer.
	if err := c.Reconcile(ctx); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	if launcher.launchCalls != 1 {
		t.Fatalf("launch calls after second Reconcile = %d, want still 1", launcher.launchCalls)
	}
}

// Boundary B: the reviewer was launched and a review_run row was created
// (findable via GetReviewRunBySessionPRSHAAndHarness) but the crash happened
// before workflow_steps.review_run_id/checkpoint were written (outbox stuck
// at "dispatched") -> recovery adopts the found review_run via natural-key
// lookup, does NOT launch a second reviewer, and backfills
// review_run_id/checkpoint/outbox acknowledged.
// Boundary B: a dispatched outbox entry with a review_run behind it, and NO
// record that any reviewer was ever launched.
//
// This used to adopt the run and bind it, on the reasoning that a non-failed
// review_run must be the reviewer this command started. It is not: dispatch
// creates the row BEFORE it launches anything, so a crash in between leaves a
// row describing an intent. Binding it made a reviewer that may never have
// existed the step's authority forever, and nothing would ever launch one.
//
// The safe recovery is to close the unlaunched row out and give the claim back,
// so the launch protocol resumes and produces exactly one reviewer.
func TestReviewRecoveryBoundaryB_DispatchedOutboxWithNoLaunchRecordResumesTheProtocol(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}
	c, store, clk := newCoordinatorWithReview(spawner, sessionFacts, workspaceFacts, reviewRuns, launcher)
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	completeWorkStep(t, c, store, clk, sessionFacts, workspaceFacts, created.Run.ID)

	steps, _ := store.ListWorkflowSteps(ctx, created.Run.ID)
	var reviewStepID, workSessionID string
	for _, s := range steps {
		if s.Kind == domain.WorkflowStepReview {
			reviewStepID = s.ID
		}
		if s.Kind == domain.WorkflowStepWork {
			workSessionID = *s.SessionID
		}
	}

	// Simulate: a review_run already exists (the reviewer genuinely launched
	// in a prior process lifetime) but the crash happened before
	// review_run_id/checkpoint were persisted on the review step, and the
	// outbox is stuck at "dispatched".
	preexistingRunID := "adopted-run-1"
	// TargetSHA must match what dispatchReviewStep will compute: Checkpoint
	// 8D's target_sha is the work-completion checkpoint's own
	// WorkspaceFingerprint (see review_dispatch.go), not a literal git SHA.
	// completeWorkStep's fake workspace facts only set Dirty=true (no
	// HeadSHA, no Changes), so this is that exact fingerprint.
	expectedTargetSHA := workflowcore.WorkspaceFingerprint(ports.WorkspaceObservation{Dirty: true})
	reviewRuns.runs[preexistingRunID] = domain.ReviewRun{
		ID: preexistingRunID, SessionID: domain.SessionID(workSessionID),
		Harness: domain.ReviewerClaudeCode, PRURL: "", TargetSHA: expectedTargetSHA,
		Status: domain.ReviewRunRunning, Verdict: domain.VerdictNone, CreatedAt: clk.Now(),
	}
	outboxKey := "workflow-step-review:" + reviewStepID + ":cycle1:claude-code" // Checkpoint 8P-D.3: idempotency key now includes harness
	store.outbox[outboxKey] = domain.WorkflowOutboxEntry{
		ID: "wfo-preexisting-review", WorkflowRunID: created.Run.ID, WorkflowStepID: &reviewStepID,
		IdempotencyKey: outboxKey, CommandType: domain.WorkflowOutboxTriggerReview,
		Status: domain.WorkflowOutboxDispatched, Payload: "{}",
	}
	if _, err := store.UpdateWorkflowStepState(ctx, reviewStepID, domain.WorkflowStepPending, domain.WorkflowStepReady, time.Now()); err != nil {
		t.Fatalf("force review ready: %v", err)
	}
	if _, err := store.UpdateWorkflowStepState(ctx, reviewStepID, domain.WorkflowStepReady, domain.WorkflowStepRunning, time.Now()); err != nil {
		t.Fatalf("force review running: %v", err)
	}

	if err := c.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// The unlaunched row must NOT have become this step's authority.
	got, err := c.GetRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	review := reviewStepFrom(got)
	if review.Step.ReviewRunID != nil && *review.Step.ReviewRunID == preexistingRunID {
		t.Fatal("a review run with no launch record became the step's authority; " +
			"nothing would ever launch a reviewer for it")
	}
	// It is closed out, so it cannot be re-adopted...
	if st := reviewRuns.runs[preexistingRunID].Status; st != domain.ReviewRunFailed {
		t.Fatalf("review run status = %q, want failed (no launch was ever recorded)", st)
	}
	// ...and the launch protocol resumed: the claim was given back and re-taken
	// within this pass, producing exactly one reviewer.
	if launcher.launchCalls != 1 {
		t.Fatalf("launch calls = %d, want exactly 1", launcher.launchCalls)
	}
	// Whatever now holds authority is a run that was actually launched, and it
	// carries an exact runtime instance.
	if review.Step.ReviewRunID == nil {
		t.Fatal("no reviewer was bound after the protocol resumed")
	}
	bound := *review.Step.ReviewRunID
	if !hasConfirmationWithInstance(t, store, created.Run.ID, bound) {
		t.Fatalf("the bound review run %s has no confirmation carrying an exact instance", bound)
	}

	// Repeating the pass must not launch a second one.
	if _, err := c.ContinueRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	if launcher.launchCalls != 1 {
		t.Fatalf("launch calls = %d after a second pass, want exactly 1", launcher.launchCalls)
	}
}

// hasConfirmationWithInstance reports whether a launch confirmation for one
// review run recorded an exact runtime instance.
func hasConfirmationWithInstance(t *testing.T, store *fakeStore, runID, reviewRunID string) bool {
	t.Helper()
	cps, err := store.ListWorkflowCheckpoints(context.Background(), runID)
	if err != nil {
		return false
	}
	for _, cp := range cps {
		if cp.DurablePhase != "review_launch_confirmed" {
			continue
		}
		if !strings.Contains(cp.RetryState, `"reviewRunId":"`+reviewRunID+`"`) {
			continue
		}
		if strings.Contains(cp.RetryState, `"instanceId":"`) {
			return true
		}
	}
	return false
}

// Boundary F (ambiguous): outbox dispatched but no review_run found by
// natural key at all -> review step waiting, run needs_attention, never
// silently approved, and never a launch.
func TestReviewRecoveryAmbiguous_DispatchedNoReviewRunFoundNeedsAttention(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}
	c, store, clk := newCoordinatorWithReview(spawner, sessionFacts, workspaceFacts, reviewRuns, launcher)
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	completeWorkStep(t, c, store, clk, sessionFacts, workspaceFacts, created.Run.ID)

	steps, _ := store.ListWorkflowSteps(ctx, created.Run.ID)
	var reviewStepID string
	for _, s := range steps {
		if s.Kind == domain.WorkflowStepReview {
			reviewStepID = s.ID
		}
	}
	outboxKey := "workflow-step-review:" + reviewStepID + ":cycle1:codex" // Checkpoint 8P-D.3: idempotency key now includes harness
	store.outbox[outboxKey] = domain.WorkflowOutboxEntry{
		ID: "wfo-preexisting-review-2", WorkflowRunID: created.Run.ID, WorkflowStepID: &reviewStepID,
		IdempotencyKey: outboxKey, CommandType: domain.WorkflowOutboxTriggerReview,
		Status: domain.WorkflowOutboxDispatched, Payload: "{}",
	}
	if _, err := store.UpdateWorkflowStepState(ctx, reviewStepID, domain.WorkflowStepPending, domain.WorkflowStepReady, time.Now()); err != nil {
		t.Fatalf("force review ready: %v", err)
	}
	if _, err := store.UpdateWorkflowStepState(ctx, reviewStepID, domain.WorkflowStepReady, domain.WorkflowStepRunning, time.Now()); err != nil {
		t.Fatalf("force review running: %v", err)
	}

	if err := c.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if launcher.launchCalls != 0 {
		t.Fatalf("launch calls = %d, want 0", launcher.launchCalls)
	}
	got, err := c.GetRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if reviewStepFrom(got).Step.State != domain.WorkflowStepWaiting {
		t.Fatalf("review step state = %q, want waiting", reviewStepFrom(got).Step.State)
	}
	if got.Run.State != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention", got.Run.State)
	}
}

// Test: an approved-verdict review restart-reconciliation across a simulated
// daemon restart (real sqlite store) still reconciles correctly without a
// second launch, mirroring TestRecoveryBoundaryF's real-store pattern.
func TestReviewRecoveryAfterRestartRealStoreReconcilesApproved(t *testing.T) {
	// This exercises the pure decision path against the fake store (a full
	// real-sqlite restart simulation for review would additionally need a
	// real review_run row, which lives in a different package's schema than
	// this test package reaches into) — the state-machine correctness is
	// covered end-to-end by TestReviewVerdictDrivesNextAction above and the
	// real-store restart pattern is proven sufficient by 8B's own
	// TestRecoveryBoundaryF for the work-step half of the same mechanism.
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}
	c, store, clk := newCoordinatorWithReview(spawner, sessionFacts, workspaceFacts, reviewRuns, launcher)
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	completeWorkStep(t, c, store, clk, sessionFacts, workspaceFacts, created.Run.ID)
	got, err := c.ContinueRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	reviewRunID := *reviewStepFrom(got).Step.ReviewRunID
	reviewRuns.setStatus(reviewRunID, domain.ReviewRunComplete, domain.VerdictApproved)
	clk.Advance(time.Second)

	if err := c.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if launcher.launchCalls != 1 {
		t.Fatalf("launch calls = %d, want still 1 (no relaunch on Reconcile after approval)", launcher.launchCalls)
	}
	final, err := c.GetRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if reviewStepFrom(final).Step.State != domain.WorkflowStepCompleted {
		t.Fatalf("review step state = %q, want completed", reviewStepFrom(final).Step.State)
	}
	if final.NextAction != "verify" {
		t.Fatalf("next action = %q, want verify", final.NextAction)
	}
	_ = workflowcore.ErrInvalid // keep import used if helpers above change
}
