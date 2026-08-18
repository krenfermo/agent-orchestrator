package workflow_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// newCoordinatorWithFix wires a coordinator with 8B's work-step deps, 8C's
// review deps, and 8D's MessageSender — the full stack needed to exercise
// the automatic review<->fix loop end to end against fakes.
func newCoordinatorWithFix(spawner workflowcore.Spawner, sessionFacts workflowcore.SessionFacts, workspaceFacts workflowcore.WorkspaceFacts, reviewRuns *fakeReviewRuns, launcher *fakeReviewerLauncher, sender *fakeMessageSender) (*workflowcore.Coordinator, *fakeStore, *fakeClock) {
	store := newFakeStore()
	clk := &fakeClock{t: time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)}
	var idSeq int
	c := workflowcore.New(workflowcore.Deps{
		Store:            store,
		Spawner:          spawner,
		SessionFacts:     sessionFacts,
		WorkspaceFacts:   workspaceFacts,
		ReviewRuns:       reviewRuns,
		ReviewerLauncher: launcher,
		MessageSender:    sender,
		Clock:            clk.Now,
		NewID: func() string {
			idSeq++
			return fmt.Sprintf("id%d", idSeq)
		},
	})
	return c, store, clk
}

func fixStepFrom(detail workflowcore.RunDetail) workflowcore.StepDetail {
	for _, sd := range detail.Steps {
		if sd.Step.Kind == domain.WorkflowStepFix {
			return sd
		}
	}
	panic("no fix step in run detail")
}

// driveToChangesRequested runs a workflow through StartRun, work completion,
// cycle-1 review dispatch (via ContinueRun), and a changes_requested
// verdict, returning the run detail once the review step is resting at
// "waiting" and (per Checkpoint 8D) the fix step has been auto-dispatched by
// the same GetRun call that observed the verdict.
func driveToChangesRequested(t *testing.T, c *workflowcore.Coordinator, store *fakeStore, clk *fakeClock, sessionFacts *fakeSessionFacts, workspaceFacts *fakeWorkspaceFacts, reviewRuns *fakeReviewRuns, runID string) workflowcore.RunDetail {
	t.Helper()
	ctx := context.Background()
	completeWorkStep(t, c, store, clk, sessionFacts, workspaceFacts, runID)
	got, err := c.ContinueRun(ctx, runID)
	if err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	reviewRunID := *reviewStepFrom(got).Step.ReviewRunID
	reviewRuns.setStatus(reviewRunID, domain.ReviewRunComplete, domain.VerdictChangesRequested)
	reviewRuns.runs[reviewRunID] = withBody(reviewRuns.runs[reviewRunID], "fix the thing")
	clk.Advance(time.Second)

	final, err := c.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun after changes_requested: %v", err)
	}
	return final
}

func withBody(r domain.ReviewRun, body string) domain.ReviewRun {
	r.Body = body
	return r
}

// Test 1/2/3: changes_requested creates exactly one fix attempt, findings are
// sent exactly once, and the SAME worker session is reused (no new Spawn).
func TestChangesRequestedDispatchesFixExactlyOnceReusingSession(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}
	sender := &fakeMessageSender{}
	c, store, clk := newCoordinatorWithFix(spawner, sessionFacts, workspaceFacts, reviewRuns, launcher, sender)
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	got := driveToChangesRequested(t, c, store, clk, sessionFacts, workspaceFacts, reviewRuns, created.Run.ID)

	review := reviewStepFrom(got)
	if review.Step.State != domain.WorkflowStepWaiting {
		t.Fatalf("review step state = %q, want waiting", review.Step.State)
	}
	fix := fixStepFrom(got)
	if fix.Step.State != domain.WorkflowStepRunning {
		t.Fatalf("fix step state = %q, want running", fix.Step.State)
	}
	attempts, err := store.ListWorkflowAttempts(ctx, fix.Step.ID)
	if err != nil || len(attempts) != 1 {
		t.Fatalf("fix step attempts = %+v, err=%v, want exactly 1", attempts, err)
	}
	// Checkpoint 8L: the fix attempt's harness must reflect whichever
	// harness ExecutionRouter actually selected for the worker (this
	// no-verification-plan "ship the thing" objective classifies as normal
	// complexity, whose default preference is claude-code), never a
	// hardcoded literal — the fix message is delivered into that SAME live
	// session, so mislabeling it here would misattribute fix-cycle usage
	// telemetry to the wrong provider.
	if attempts[0].AttemptNumber != 1 || attempts[0].Harness != "claude-code" {
		t.Fatalf("fix attempt = %+v, want attempt_number=1 harness=claude-code", attempts[0])
	}
	if sender.calls != 1 {
		t.Fatalf("MessageSender.Send calls = %d, want exactly 1", sender.calls)
	}
	if string(sender.lastID) != *workStepFrom(got).Step.SessionID {
		t.Fatalf("Send target session = %q, want the work step's own worker session %q", sender.lastID, *workStepFrom(got).Step.SessionID)
	}
	if spawner.calls != 1 {
		t.Fatalf("Spawner.Spawn calls = %d, want exactly 1 (no new session for the fix cycle)", spawner.calls)
	}

	// Repeated GetRun calls (simulating the frontend's poll) must not send
	// the findings a second time.
	if _, err := c.GetRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("second GetRun: %v", err)
	}
	if _, err := c.GetRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("third GetRun: %v", err)
	}
	if sender.calls != 1 {
		t.Fatalf("MessageSender.Send calls after repeated GetRun = %d, want still 1", sender.calls)
	}
}

// Test 4/5: an unchanged workspace fingerprint does not resolve the fix
// cycle or trigger a new review (mirrors 8B's "idle with no verifiable
// change" conservatism).
func TestFixCycleUnchangedFingerprintStaysAmbiguous(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}
	sender := &fakeMessageSender{}
	c, store, clk := newCoordinatorWithFix(spawner, sessionFacts, workspaceFacts, reviewRuns, launcher, sender)
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	got := driveToChangesRequested(t, c, store, clk, sessionFacts, workspaceFacts, reviewRuns, created.Run.ID)
	work := workStepFrom(got)

	// Worker goes idle but the workspace fingerprint is IDENTICAL to before
	// the fix was dispatched (workspaceFacts.obs never changed): must not
	// resolve the fix cycle or trigger a new review.
	sessionFacts.put(domain.SessionRecord{
		ID: domain.SessionID(*work.Step.SessionID), ProjectID: "proj-1",
		Activity: domain.Activity{State: domain.ActivityIdle}, IsTerminated: false,
		FirstSignalAt: time.Now(),
		Metadata:      domain.SessionMetadata{WorkspacePath: "/ws/wf", Branch: "ao/wf"},
	})
	clk.Advance(10 * time.Second)
	unchanged, err := c.GetRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GetRun (unchanged fingerprint): %v", err)
	}
	if fixStepFrom(unchanged).Step.State != domain.WorkflowStepWaiting {
		t.Fatalf("fix step state after unchanged fingerprint = %q, want waiting (ambiguous, not resolved)", fixStepFrom(unchanged).Step.State)
	}
	if reviewRuns.insertCalls != 1 {
		t.Fatalf("InsertReviewRun calls after unchanged fingerprint = %d, want still 1 (no new review)", reviewRuns.insertCalls)
	}
	if unchanged.Run.State != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention (ambiguous fix outcome)", unchanged.Run.State)
	}
}

func TestFixCycleBeforeFirstSignalRemainsInProgress(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}
	sender := &fakeMessageSender{}
	c, store, clk := newCoordinatorWithFix(spawner, sessionFacts, workspaceFacts, reviewRuns, launcher, sender)
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	got := driveToChangesRequested(t, c, store, clk, sessionFacts, workspaceFacts, reviewRuns, created.Run.ID)
	work := workStepFrom(got)
	sessionFacts.put(domain.SessionRecord{
		ID: domain.SessionID(*work.Step.SessionID), ProjectID: "proj-1",
		Activity: domain.Activity{State: domain.ActivityIdle}, IsTerminated: false,
		Metadata: domain.SessionMetadata{WorkspacePath: "/ws/wf", Branch: "ao/wf"},
	})
	clk.Advance(10 * time.Second)

	unchanged, err := c.GetRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GetRun before first signal: %v", err)
	}
	if fixStepFrom(unchanged).Step.State != domain.WorkflowStepRunning {
		t.Fatalf("fix step state = %q, want running", fixStepFrom(unchanged).Step.State)
	}
	if reviewRuns.insertCalls != 1 {
		t.Fatalf("InsertReviewRun calls = %d, want 1", reviewRuns.insertCalls)
	}
}

// Test 4/6: a genuinely new fingerprint resolves the fix cycle and
// dispatches exactly one new review_run (cycle 2).
func TestFixCycleChangedFingerprintDispatchesNextReview(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}
	sender := &fakeMessageSender{}
	c, store, clk := newCoordinatorWithFix(spawner, sessionFacts, workspaceFacts, reviewRuns, launcher, sender)
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	got := driveToChangesRequested(t, c, store, clk, sessionFacts, workspaceFacts, reviewRuns, created.Run.ID)
	work := workStepFrom(got)
	if reviewRuns.insertCalls != 1 {
		t.Fatalf("InsertReviewRun calls before fix delivery = %d, want 1", reviewRuns.insertCalls)
	}

	// Simulate a genuine change: different workspace observation -> a
	// different workspace fingerprint than the one changes_requested was
	// reviewing.
	workspaceFacts.obs = ports.WorkspaceObservation{Dirty: true, Changes: []ports.WorkspaceChange{{Path: "a.go", Status: " M"}}}
	sessionFacts.put(domain.SessionRecord{
		ID: domain.SessionID(*work.Step.SessionID), ProjectID: "proj-1",
		Activity: domain.Activity{State: domain.ActivityIdle}, IsTerminated: false,
		Metadata: domain.SessionMetadata{WorkspacePath: "/ws/wf", Branch: "ao/wf"},
	})
	clk.Advance(10 * time.Second)

	changed, err := c.GetRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GetRun (changed fingerprint): %v", err)
	}
	if fixStepFrom(changed).Step.State != domain.WorkflowStepWaiting {
		t.Fatalf("fix step state after changed fingerprint = %q, want waiting (resolved)", fixStepFrom(changed).Step.State)
	}
	review := reviewStepFrom(changed)
	if review.Step.State != domain.WorkflowStepRunning {
		t.Fatalf("review step state after fix resolved = %q, want running (cycle 2 dispatched)", review.Step.State)
	}
	if reviewRuns.insertCalls != 2 {
		t.Fatalf("InsertReviewRun calls = %d, want exactly 2 (cycle 1 + cycle 2)", reviewRuns.insertCalls)
	}

	// Idempotent: repeated GetRun calls must not create a 3rd review_run.
	if _, err := c.GetRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("second GetRun: %v", err)
	}
	if reviewRuns.insertCalls != 2 {
		t.Fatalf("InsertReviewRun calls after repeated GetRun = %d, want still 2", reviewRuns.insertCalls)
	}
}

// TestFixAttemptHarnessTracksActualWorkerHarness is Checkpoint 8L.1's real-
// E2E-discovered regression test: recordFixDispatchSuccess used to hardcode
// the fix attempt's harness as the literal "codex" (correct only by
// accident before 8L, when the worker was always Codex). Checkpoint 8P-C's
// legacy/trusted-local compatibility default (domain.DefaultUserExecutionPolicy)
// prefers Claude Code first for every complexity tier (no more hardcoded
// trivial-prefers-Codex rule — see checkpoint brief §17), so this run's
// worker routes to Claude Code; the test still proves the fix path derives
// its harness from the work step's actual last attempt rather than from any
// hardcoded assumption in either direction.
func TestFixAttemptHarnessTracksActualWorkerHarness(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}
	sender := &fakeMessageSender{}
	c, store, clk := newCoordinatorWithFix(spawner, sessionFacts, workspaceFacts, reviewRuns, launcher, sender)
	ctx := context.Background()

	// A single exact-content file check biases pre-dispatch complexity to
	// trivial (see worker_routing.go's plannedComplexityFacts), so the
	// default policy prefers Codex for this run's worker.
	created, err := c.CreateRun(ctx, "proj-1", "ship the thing", workflowcore.VerificationPlan{
		Files: []workflowcore.VerificationFileCheck{{Path: "trivial.txt", Exists: true, ExactContent: strPtr("trivial\n")}},
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	got := driveToChangesRequested(t, c, store, clk, sessionFacts, workspaceFacts, reviewRuns, created.Run.ID)

	work := workStepFrom(got)
	workAttempts, err := store.ListWorkflowAttempts(ctx, work.Step.ID)
	if err != nil || len(workAttempts) == 0 || workAttempts[len(workAttempts)-1].Harness != "claude-code" {
		t.Fatalf("work attempts = %+v, err=%v, want the worker to have routed to claude-code (legacy-compat default)", workAttempts, err)
	}

	fix := fixStepFrom(got)
	fixAttempts, err := store.ListWorkflowAttempts(ctx, fix.Step.ID)
	if err != nil || len(fixAttempts) != 1 || fixAttempts[0].Harness != "claude-code" {
		t.Fatalf("fix attempts = %+v, err=%v, want exactly 1 with harness=claude-code (matching the actual worker)", fixAttempts, err)
	}
}

func strPtr(s string) *string { return &s }

// Test 7: approved after a fix cycle produces next_action="verify" (reached
// via cycle 2).
func TestApprovedAfterFixCycleProducesVerify(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}
	sender := &fakeMessageSender{}
	c, store, clk := newCoordinatorWithFix(spawner, sessionFacts, workspaceFacts, reviewRuns, launcher, sender)
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	got := driveToChangesRequested(t, c, store, clk, sessionFacts, workspaceFacts, reviewRuns, created.Run.ID)
	work := workStepFrom(got)

	// Deliver a genuine new fingerprint so the fix cycle resolves.
	workspaceFacts.obs = ports.WorkspaceObservation{Dirty: true, Changes: []ports.WorkspaceChange{{Path: "a.go", Status: " M"}}}
	sessionFacts.put(domain.SessionRecord{
		ID: domain.SessionID(*work.Step.SessionID), ProjectID: "proj-1",
		Activity: domain.Activity{State: domain.ActivityIdle}, IsTerminated: false,
		Metadata: domain.SessionMetadata{WorkspacePath: "/ws/wf", Branch: "ao/wf"},
	})
	clk.Advance(10 * time.Second)
	afterFix, err := c.GetRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GetRun after fix delivered: %v", err)
	}
	if fixStepFrom(afterFix).Step.State != domain.WorkflowStepWaiting {
		t.Fatalf("fix step state = %q, want waiting (resolved)", fixStepFrom(afterFix).Step.State)
	}
	review2 := reviewStepFrom(afterFix)
	if review2.Step.State != domain.WorkflowStepRunning {
		t.Fatalf("review step state after fix resolved = %q, want running (cycle 2 dispatched)", review2.Step.State)
	}
	if reviewRuns.insertCalls != 2 {
		t.Fatalf("InsertReviewRun calls = %d, want exactly 2 (cycle 1 + cycle 2)", reviewRuns.insertCalls)
	}

	// Approve cycle 2.
	reviewRunID2 := *review2.Step.ReviewRunID
	reviewRuns.setStatus(reviewRunID2, domain.ReviewRunComplete, domain.VerdictApproved)
	clk.Advance(time.Second)

	final, err := c.GetRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GetRun after approval: %v", err)
	}
	if reviewStepFrom(final).Step.State != domain.WorkflowStepCompleted {
		t.Fatalf("review step state = %q, want completed", reviewStepFrom(final).Step.State)
	}
	if final.NextAction != "verify" {
		t.Fatalf("next action = %q, want verify", final.NextAction)
	}
	// Fix step never reaches completed in this checkpoint's scope.
	if fixStepFrom(final).Step.State == domain.WorkflowStepCompleted {
		t.Fatalf("fix step must never reach completed in Checkpoint 8D")
	}
}

// Test 9/10: max_fix_cycles budget is respected — no 4th fix dispatch when
// policy says 3, and budget exhaustion -> needs_attention/human_attention,
// fix step stays waiting (not failed).
func TestFixBudgetExhaustionStopsAtMaxCyclesAndSurfacesNeedsAttention(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}
	sender := &fakeMessageSender{}
	c, store, clk := newCoordinatorWithFix(spawner, sessionFacts, workspaceFacts, reviewRuns, launcher, sender)
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	got := driveToChangesRequested(t, c, store, clk, sessionFacts, workspaceFacts, reviewRuns, created.Run.ID)
	work := workStepFrom(got)

	// Cycle through changes_requested -> fix delivered -> re-review, three
	// more times (default policy MaxFixCycles=3; cycle 1's changes_requested
	// already happened above, so this drives cycles 2 and 3 to their own
	// changes_requested verdicts, then a would-be cycle 4 must be refused).
	driveFixDelivery := func() workflowcore.RunDetail {
		t.Helper()
		workspaceFacts.obs = ports.WorkspaceObservation{HeadSHA: fmt.Sprintf("sha-%d", clk.Now().Unix())}
		sessionFacts.put(domain.SessionRecord{
			ID: domain.SessionID(*work.Step.SessionID), ProjectID: "proj-1",
			Activity: domain.Activity{State: domain.ActivityIdle}, IsTerminated: false,
			Metadata: domain.SessionMetadata{WorkspacePath: "/ws/wf", Branch: "ao/wf"},
		})
		clk.Advance(10 * time.Second)
		d, err := c.GetRun(ctx, created.Run.ID)
		if err != nil {
			t.Fatalf("GetRun (drive fix delivery): %v", err)
		}
		return d
	}

	for cycle := 2; cycle <= 3; cycle++ {
		afterFix := driveFixDelivery()
		review := reviewStepFrom(afterFix)
		if review.Step.State != domain.WorkflowStepRunning {
			t.Fatalf("cycle %d: review step state = %q, want running", cycle, review.Step.State)
		}
		reviewRuns.setStatus(*review.Step.ReviewRunID, domain.ReviewRunComplete, domain.VerdictChangesRequested)
		reviewRuns.runs[*review.Step.ReviewRunID] = withBody(reviewRuns.runs[*review.Step.ReviewRunID], "still not right")
		clk.Advance(time.Second)
		afterVerdict, err := c.GetRun(ctx, created.Run.ID)
		if err != nil {
			t.Fatalf("GetRun after cycle %d verdict: %v", cycle, err)
		}
		if cycle < 3 {
			if fixStepFrom(afterVerdict).Step.State != domain.WorkflowStepRunning {
				t.Fatalf("cycle %d: fix step state = %q, want running (next cycle dispatched, within budget)", cycle, fixStepFrom(afterVerdict).Step.State)
			}
		} else {
			// Cycle 3's changes_requested is the 3rd review_run created for
			// this session — exactly at MaxFixCycles(3), so a 3rd fix IS
			// still within budget (fix cycle 3 addresses review cycle 3).
			if fixStepFrom(afterVerdict).Step.State != domain.WorkflowStepRunning {
				t.Fatalf("cycle %d: fix step state = %q, want running (3rd fix cycle, still within budget)", cycle, fixStepFrom(afterVerdict).Step.State)
			}
		}
	}

	// Deliver fix cycle 3 and let review cycle 4 land changes_requested
	// again: this is the (budget+1)th cycle and must be refused.
	afterFix3 := driveFixDelivery()
	review4 := reviewStepFrom(afterFix3)
	if review4.Step.State != domain.WorkflowStepRunning {
		t.Fatalf("review cycle 4: state = %q, want running", review4.Step.State)
	}
	reviewRuns.setStatus(*review4.Step.ReviewRunID, domain.ReviewRunComplete, domain.VerdictChangesRequested)
	reviewRuns.runs[*review4.Step.ReviewRunID] = withBody(reviewRuns.runs[*review4.Step.ReviewRunID], "still not right, 4th time")
	clk.Advance(time.Second)

	final, err := c.GetRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GetRun after cycle 4 verdict: %v", err)
	}
	if final.Run.State != domain.WorkflowRunNeedsAttention {
		t.Fatalf("run state = %q, want needs_attention (budget exhausted)", final.Run.State)
	}
	if final.NextAction != "human_attention" {
		t.Fatalf("next action = %q, want human_attention", final.NextAction)
	}
	if fixStepFrom(final).Step.State == domain.WorkflowStepFailed {
		t.Fatalf("fix step must not be failed on budget exhaustion, want it resting at waiting")
	}
	if fixStepFrom(final).Step.State != domain.WorkflowStepWaiting {
		t.Fatalf("fix step state = %q, want waiting", fixStepFrom(final).Step.State)
	}
	attempts, err := store.ListWorkflowAttempts(ctx, fixStepFrom(final).Step.ID)
	if err != nil {
		t.Fatalf("ListWorkflowAttempts: %v", err)
	}
	if len(attempts) != 3 {
		t.Fatalf("fix attempts = %d, want exactly 3 (no 4th dispatch)", len(attempts))
	}
}

// Test 18: cancel during an active fix cycle stops all further progression.
func TestCancelDuringFixCycleStopsProgression(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}
	sender := &fakeMessageSender{}
	c, store, clk := newCoordinatorWithFix(spawner, sessionFacts, workspaceFacts, reviewRuns, launcher, sender)
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	got := driveToChangesRequested(t, c, store, clk, sessionFacts, workspaceFacts, reviewRuns, created.Run.ID)
	if fixStepFrom(got).Step.State != domain.WorkflowStepRunning {
		t.Fatalf("precondition: fix step must be running before cancel, got %q", fixStepFrom(got).Step.State)
	}

	cancelled, err := c.CancelRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("CancelRun: %v", err)
	}
	if cancelled.Run.State != domain.WorkflowRunCancelled {
		t.Fatalf("run state = %q, want cancelled", cancelled.Run.State)
	}
	if fixStepFrom(cancelled).Step.State != domain.WorkflowStepCancelled {
		t.Fatalf("fix step state = %q, want cancelled", fixStepFrom(cancelled).Step.State)
	}

	sendCallsBefore := sender.calls
	insertCallsBefore := reviewRuns.insertCalls
	spawnCallsBefore := spawner.calls
	outboxBefore := len(store.outbox)

	if _, err := c.GetRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("GetRun after cancel: %v", err)
	}
	if sender.calls != sendCallsBefore {
		t.Fatalf("Send calls after cancel+GetRun = %d, want still %d", sender.calls, sendCallsBefore)
	}
	if reviewRuns.insertCalls != insertCallsBefore {
		t.Fatalf("InsertReviewRun calls after cancel+GetRun = %d, want still %d", reviewRuns.insertCalls, insertCallsBefore)
	}
	if spawner.calls != spawnCallsBefore {
		t.Fatalf("Spawn calls after cancel+GetRun = %d, want still %d", spawner.calls, spawnCallsBefore)
	}
	if len(store.outbox) != outboxBefore {
		t.Fatalf("outbox entry count after cancel+GetRun = %d, want still %d (no new entries)", len(store.outbox), outboxBefore)
	}
}

// Test 22: an initial approved verdict (cycle 1, no changes_requested ever)
// creates zero fix attempts and zero fix-related outbox entries — regression
// guard that the 8C happy path still behaves identically under 8D.
func TestApprovedCycle1CreatesNoFixAttempts(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}
	sender := &fakeMessageSender{}
	c, store, clk := newCoordinatorWithFix(spawner, sessionFacts, workspaceFacts, reviewRuns, launcher, sender)
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

	final, err := c.GetRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if final.NextAction != "verify" {
		t.Fatalf("next action = %q, want verify", final.NextAction)
	}
	fix := fixStepFrom(final)
	if fix.Step.State != domain.WorkflowStepPending {
		t.Fatalf("fix step state = %q, want still pending", fix.Step.State)
	}
	attempts, err := store.ListWorkflowAttempts(ctx, fix.Step.ID)
	if err != nil || len(attempts) != 0 {
		t.Fatalf("fix step attempts = %+v, err=%v, want none", attempts, err)
	}
	if sender.calls != 0 {
		t.Fatalf("MessageSender.Send calls = %d, want 0", sender.calls)
	}
	for _, entry := range store.outbox {
		if entry.CommandType == domain.WorkflowOutboxSendMessage {
			t.Fatalf("unexpected send_message outbox entry for an approved-cycle-1 run: %+v", entry)
		}
	}
}

// Test 23: regression for the 8C double-continue bug — a single call from
// work=completed, review=pending reliably dispatches review in one call.
func TestContinueRunSingleCallDispatchesReviewFromCompletedWork(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}
	sender := &fakeMessageSender{}
	c, store, clk := newCoordinatorWithFix(spawner, sessionFacts, workspaceFacts, reviewRuns, launcher, sender)
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
	// The real Codex worker already finished (idle + dirty worktree), but no
	// prior GetRun poll has observed that yet: the work step is still
	// durably "running" in storage, and the review step is still "pending".
	// This is exactly the race the 8C double-continue bug hit.
	sessionFacts.put(domain.SessionRecord{
		ID: domain.SessionID(*work.Step.SessionID), ProjectID: "proj-1",
		Activity: domain.Activity{State: domain.ActivityIdle}, IsTerminated: false,
		Metadata: domain.SessionMetadata{WorkspacePath: "/ws/wf", Branch: "ao/wf"},
	})
	workspaceFacts.obs.Dirty = true
	clk.Advance(10 * time.Second)

	steps, _ := store.ListWorkflowSteps(ctx, created.Run.ID)
	for _, s := range steps {
		if s.Kind == domain.WorkflowStepWork && s.State != domain.WorkflowStepRunning {
			t.Fatalf("precondition violated: work step must still be durably running, got %q", s.State)
		}
		if s.Kind == domain.WorkflowStepReview && s.State != domain.WorkflowStepPending {
			t.Fatalf("precondition violated: review step must still be durably pending, got %q", s.State)
		}
	}

	got, err := c.ContinueRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("ContinueRun: %v", err)
	}
	review := reviewStepFrom(got)
	if review.Step.State != domain.WorkflowStepRunning {
		t.Fatalf("review step state after a SINGLE ContinueRun call = %q, want running (not pending — that was the 8C bug)", review.Step.State)
	}
	if review.Step.ReviewRunID == nil {
		t.Fatalf("review step has no review_run_id after a single ContinueRun call")
	}
	if launcher.launchCalls != 1 {
		t.Fatalf("launch calls = %d, want exactly 1", launcher.launchCalls)
	}
}

// TestFixLifecycleDecisionPersistedExactlyOnceAcrossRepeatedPolls covers
// Checkpoint 8M test requirement #4/#12: a fix cycle's session_lifecycle_decision
// checkpoint (REUSE — fix loop keeps reusing the same session by default) is
// recorded exactly once for a given cycle even though maybeDispatchFix can
// be re-entered on every GetRun poll — see fix_dispatch.go's doc comment on
// why the decision is applied inside dispatchFixFromPending, the single
// outbox-idempotency-guarded call site, not in cascade.go's maybeDispatchFix.
func TestFixLifecycleDecisionPersistedExactlyOnceAcrossRepeatedPolls(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}
	sender := &fakeMessageSender{}
	c, store, clk := newCoordinatorWithFix(spawner, sessionFacts, workspaceFacts, reviewRuns, launcher, sender)
	ctx := context.Background()

	created, err := c.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	driveToChangesRequested(t, c, store, clk, sessionFacts, workspaceFacts, reviewRuns, created.Run.ID)
	if sender.calls != 1 {
		t.Fatalf("sender calls after first fix dispatch = %d, want 1", sender.calls)
	}

	// Simulate the frontend's repeated 2s poll: several more GetRun calls
	// with nothing new to observe.
	for i := 0; i < 3; i++ {
		if _, err := c.GetRun(ctx, created.Run.ID); err != nil {
			t.Fatalf("GetRun poll %d: %v", i, err)
		}
	}
	if sender.calls != 1 {
		t.Fatalf("sender calls after repeated polling = %d, want still 1 (no duplicate fix message)", sender.calls)
	}

	cps, err := store.ListWorkflowCheckpoints(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("ListWorkflowCheckpoints: %v", err)
	}
	lifecycleCount := 0
	for _, cp := range cps {
		if cp.DurablePhase != "session_lifecycle_decision" {
			continue
		}
		decision, _, ok := workflowcore.DecodeSessionLifecycleDecisionForTest(cp.RetryState)
		if !ok {
			t.Fatalf("session_lifecycle_decision checkpoint did not decode: %+v", cp)
		}
		if decision.Role == domain.WorkflowRoleFixWorker {
			lifecycleCount++
			if decision.Action != domain.LifecycleReuse {
				t.Fatalf("fix lifecycle decision action = %q, want reuse", decision.Action)
			}
		}
	}
	if lifecycleCount != 1 {
		t.Fatalf("fix_worker session_lifecycle_decision checkpoints = %d, want exactly 1 despite repeated polling", lifecycleCount)
	}
}

// TestFixLifecycleSurvivesRestartNoDuplicateDispatch covers Checkpoint 8M
// test requirement #13: a fix cycle's lifecycle decision/context pack is
// durable — a fresh Coordinator over the same store (the exact "restart"
// pattern failover_test.go/recovery_boundaries_test.go already use) must
// not re-dispatch the fix message or record a second lifecycle decision.
func TestFixLifecycleSurvivesRestartNoDuplicateDispatch(t *testing.T) {
	sessionFacts := newFakeSessionFacts()
	spawner := &fakeSpawner{rec: domain.SessionRecord{Metadata: domain.SessionMetadata{Branch: "ao/wf", WorkspacePath: "/ws/wf"}}, facts: sessionFacts}
	workspaceFacts := &fakeWorkspaceFacts{}
	reviewRuns := newFakeReviewRuns()
	launcher := &fakeReviewerLauncher{}
	sender1 := &fakeMessageSender{}
	c1, store, clk := newCoordinatorWithFix(spawner, sessionFacts, workspaceFacts, reviewRuns, launcher, sender1)
	ctx := context.Background()

	created, err := c1.CreateRun(ctx, "proj-1", "ship the thing")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	driveToChangesRequested(t, c1, store, clk, sessionFacts, workspaceFacts, reviewRuns, created.Run.ID)
	if sender1.calls != 1 {
		t.Fatalf("sender1 calls = %d, want 1", sender1.calls)
	}

	// Simulate a daemon restart: a fresh Coordinator over the same durable
	// store, with its own fresh MessageSender (a real restart loses no
	// in-memory state that matters — everything the fix cycle needs is
	// already durable).
	sender2 := &fakeMessageSender{}
	c2 := workflowcore.New(workflowcore.Deps{
		Store: store, Spawner: spawner, SessionFacts: sessionFacts, WorkspaceFacts: workspaceFacts,
		ReviewRuns: reviewRuns, ReviewerLauncher: launcher, MessageSender: sender2, Clock: clk.Now,
		NewID: func() string { return "restart-id" },
	})
	if _, err := c2.GetRun(ctx, created.Run.ID); err != nil {
		t.Fatalf("GetRun after restart: %v", err)
	}
	if sender2.calls != 0 {
		t.Fatalf("sender2 calls = %d, want 0 (no duplicate fix dispatch after restart)", sender2.calls)
	}

	cps, err := store.ListWorkflowCheckpoints(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("ListWorkflowCheckpoints: %v", err)
	}
	lifecycleCount := 0
	for _, cp := range cps {
		if cp.DurablePhase != "session_lifecycle_decision" {
			continue
		}
		if decision, _, ok := workflowcore.DecodeSessionLifecycleDecisionForTest(cp.RetryState); ok && decision.Role == domain.WorkflowRoleFixWorker {
			lifecycleCount++
		}
	}
	if lifecycleCount != 1 {
		t.Fatalf("fix_worker session_lifecycle_decision checkpoints after restart = %d, want still exactly 1", lifecycleCount)
	}
}
