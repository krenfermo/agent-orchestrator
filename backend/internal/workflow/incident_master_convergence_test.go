package workflow_test

// incident_master_convergence_test.go — the reachability half of wf-724a1e97,
// through the real autonomous stack.
//
// The objective's own heartbeat ran 185 times over the incident and could not
// have converged its child on any of them, because reconcileMasterTasksOnce
// only ever offered ContinueRun to a child in running/waiting or to one whose
// stop is NOT human-owned — and fix_budget_exhausted is human-owned by
// disposition. So the one transition able to notice a post-stop head change was
// reachable solely from a person's own button on the CHILD run.
//
// This test drives the real sqlite store, the real wake.Scheduler and the real
// wakepoller, and touches neither. Nobody presses anything.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

func TestParkedChildWithANewHeadConvergesFromTheObjectivesOwnHeartbeat(t *testing.T) {
	fx := newAutonomousFixture(t, twoTaskDependentPlan())
	ctx := context.Background()
	seedUser(t, fx.store, "user-1", "prof-claude-1", "prof-codex-1", true, fx.clk.Now())

	created, err := fx.coord.CreateObjectiveRun(ctx, "p", "Build users", domain.WorkflowPlanApprovalManual)
	if err != nil {
		t.Fatalf("CreateObjectiveRun: %v", err)
	}
	stampOwnerAndApplyPolicy(t, fx, created.Run.ID, "user-1")

	driveCycles(t, fx, 1, nil)
	_, childID, ok := activeChildRunID(t, fx, created.Run.ID)
	if !ok || childID == "" {
		t.Fatal("no child run was dispatched; fixture did not reach the state under test")
	}

	// The child, in wf-724a1e97's exact resting shape.
	reviewOfA := seedFixBudgetExhaustedChild(t, fx, childID)
	seedMirroredChildStop(t, fx, created.Run.ID, childID)

	// A change appears that AO did not make, and its worker has been silent for
	// an hour. This is 247d3bc5f.
	fx.ws.obs.HeadSHA = "247d3bc5f"
	quietenSessions(t, fx)
	launchesBefore := fx.launcher.launchCalls

	// From here on: only the daemon's own poller.
	driveCycles(t, fx, 6, nil)

	child, _, err := fx.store.GetWorkflowRun(ctx, childID)
	if err != nil {
		t.Fatalf("GetWorkflowRun(child): %v", err)
	}
	if !realHasCheckpointPhase(t, fx, childID, "human_applied_fix_observed") {
		t.Fatalf("the objective's heartbeat never noticed the new head; child state = %q", child.State)
	}
	if got := fx.launcher.launchCalls - launchesBefore; got != 1 {
		t.Fatalf("reviewer launches = %d, want exactly 1 fresh authoritative review", got)
	}
	steps, err := fx.store.ListWorkflowSteps(ctx, childID)
	if err != nil {
		t.Fatalf("ListWorkflowSteps(child): %v", err)
	}
	for _, s := range steps {
		if s.Kind != domain.WorkflowStepReview {
			continue
		}
		if s.ReviewRunID == nil {
			t.Fatal("the child's review step names no review run after convergence")
		}
		if *s.ReviewRunID == reviewOfA {
			t.Fatalf("the child is still bound to review %s, which judged the old head", reviewOfA)
		}
		fresh, found, gerr := fx.store.GetReviewRun(ctx, *s.ReviewRunID)
		if gerr != nil || !found {
			t.Fatalf("GetReviewRun: %v (found=%v)", gerr, found)
		}
		if fresh.TargetSHA == "" {
			t.Fatal("the fresh review records no target state")
		}
		prev, pfound, perr := fx.store.GetReviewRun(ctx, reviewOfA)
		if perr != nil || !pfound {
			t.Fatalf("GetReviewRun(previous): %v (found=%v)", perr, pfound)
		}
		if prev.SupersededBy != fresh.ID {
			t.Fatalf("previous review superseded_by = %q, want %q", prev.SupersededBy, fresh.ID)
		}
	}
}

// seedFixBudgetExhaustedChild writes the child's durable state verbatim: work
// completed with its worker session attached, a real review run that requested
// changes about the tree AS IT THEN WAS, review and fix resting at waiting, the
// run parked, and the canonical stop on the ledger. It returns that review's id.
func seedFixBudgetExhaustedChild(t *testing.T, fx *autonomousFixture, childID string) string {
	t.Helper()
	ctx := context.Background()
	steps, err := fx.store.ListWorkflowSteps(ctx, childID)
	if err != nil {
		t.Fatalf("ListWorkflowSteps(child): %v", err)
	}
	child, ok, err := fx.store.GetWorkflowRun(ctx, childID)
	if err != nil || !ok {
		t.Fatalf("GetWorkflowRun(child): %v (found=%v)", err, ok)
	}

	var workStepID, sessionID, reviewStepID, fixStepID string
	for _, s := range steps {
		switch s.Kind {
		case domain.WorkflowStepWork:
			workStepID = s.ID
			if s.SessionID != nil {
				sessionID = *s.SessionID
			}
			if s.State != domain.WorkflowStepCompleted {
				if _, err := fx.store.UpdateWorkflowStepState(ctx, s.ID, s.State, domain.WorkflowStepCompleted, fx.clk.Now()); err != nil {
					t.Fatalf("complete work step: %v", err)
				}
			}
		case domain.WorkflowStepReview:
			reviewStepID = s.ID
			moveStepTo(t, fx, s, domain.WorkflowStepWaiting)
		case domain.WorkflowStepFix:
			fixStepID = s.ID
			moveStepTo(t, fx, s, domain.WorkflowStepWaiting)
		}
	}
	if workStepID == "" || sessionID == "" || reviewStepID == "" || fixStepID == "" {
		t.Fatalf("child is missing a step or its worker session (work=%q session=%q review=%q fix=%q)",
			workStepID, sessionID, reviewStepID, fixStepID)
	}
	sess, found, err := fx.store.GetSession(ctx, domain.SessionID(sessionID))
	if err != nil || !found {
		t.Fatalf("GetSession(%s): %v (found=%v)", sessionID, err, found)
	}

	// The review that judged the tree as it then was, recorded through the same
	// store the coordinator reads.
	oldFingerprint := workflowcore.WorkspaceFingerprint(fx.ws.obs)
	harness := domain.ReviewerHarness(domain.HarnessClaudeCode)
	if err := fx.store.UpsertReview(ctx, domain.Review{
		ID: "rev-incident", SessionID: domain.SessionID(sessionID), ProjectID: domain.ProjectID(child.ProjectID), Harness: harness,
		CreatedAt: fx.clk.Now(), UpdatedAt: fx.clk.Now(),
	}); err != nil {
		t.Fatalf("UpsertReview: %v", err)
	}
	reviewRunID := "rr-incident-a"
	if err := fx.store.InsertReviewRun(ctx, domain.ReviewRun{
		ID: reviewRunID, ReviewID: "rev-incident", SessionID: domain.SessionID(sessionID),
		Harness: harness, TargetSHA: oldFingerprint, Status: domain.ReviewRunComplete,
		Verdict: domain.VerdictChangesRequested, Body: "the audit doc still claims harness reads are observed",
		CreatedAt: fx.clk.Now(),
	}); err != nil {
		t.Fatalf("InsertReviewRun: %v", err)
	}
	if _, err := fx.store.SetWorkflowStepReviewRun(ctx, reviewStepID, reviewRunID, fx.clk.Now()); err != nil {
		t.Fatalf("bind review run: %v", err)
	}

	if child.State != domain.WorkflowRunNeedsAttention {
		if _, err := fx.store.UpdateWorkflowRunState(ctx, childID, child.State, domain.WorkflowRunNeedsAttention, fx.clk.Now()); err != nil {
			t.Fatalf("park child: %v", err)
		}
	}
	// The three fix cycles that were actually spent. The budget is a fold over
	// exactly these rows (fix_budget.go), so a child parked on
	// fix_budget_exhausted with no dispatch records would have budget left and
	// the ordinary loop would take over — which is a different state, and not
	// the one this test is about.
	for cycle := 1; cycle <= domain.DefaultWorkflowPolicy().MaxFixCycles; cycle++ {
		if _, err := fx.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
			ID:             fmt.Sprintf("wfc-child-fix-%d", cycle),
			WorkflowRunID:  childID,
			WorkflowStepID: &fixStepID,
			ProjectID:      child.ProjectID,
			SessionID:      &sessionID,
			DurablePhase:   "fix_dispatched",
			PayloadVersion: "v1",
			RetryState:     fmt.Sprintf(`{"cycleNumber":%d}`, cycle),
			NextAction:     fmt.Sprintf("fix cycle %d dispatched", cycle),
			CreatedAt:      fx.clk.Now(),
		}); err != nil {
			t.Fatalf("record fix cycle %d: %v", cycle, err)
		}
	}
	if _, err := fx.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID: "wfc-child-budget", WorkflowRunID: childID, WorkflowStepID: &fixStepID,
		ProjectID: child.ProjectID, SessionID: &sessionID,
		Branch: sess.Metadata.Branch, WorktreePath: sess.Metadata.WorkspacePath,
		NextAction:     "fix_budget_exhausted: the reviewer still requests changes after 3 fix cycles (max_fix_cycles=3)",
		DurablePhase:   workflowcore.ReasonFixBudgetExhausted,
		PayloadVersion: "v1", RetryState: "{}",
		CreatedAt: fx.clk.Now().Add(time.Second),
	}); err != nil {
		t.Fatalf("record the stop: %v", err)
	}
	// A work-step checkpoint naming the session and worktree, which is how the
	// recovery proves the workspace it reads belongs to this run.
	if _, err := fx.store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID: "wfc-child-work", WorkflowRunID: childID, WorkflowStepID: &workStepID,
		ProjectID: child.ProjectID, SessionID: &sessionID,
		Branch: sess.Metadata.Branch, WorktreePath: sess.Metadata.WorkspacePath,
		NextAction:     "start_review",
		DurablePhase:   "worker_observed_worker_result_available",
		PayloadVersion: "v1", RetryState: "{}",
		CreatedAt: fx.clk.Now(),
	}); err != nil {
		t.Fatalf("record the work observation: %v", err)
	}
	return reviewRunID
}

func moveStepTo(t *testing.T, fx *autonomousFixture, s domain.WorkflowStep, to domain.WorkflowStepState) {
	t.Helper()
	if s.State == to {
		return
	}
	if _, err := fx.store.UpdateWorkflowStepState(context.Background(), s.ID, s.State, to, fx.clk.Now()); err != nil {
		t.Fatalf("move %s step %s -> %s: %v", s.Kind, s.State, to, err)
	}
}

// quietenSessions makes every session provably not in flight: a turn finished
// an hour ago and nothing since. It is the shape agent-orchestrator-52 had.
func quietenSessions(t *testing.T, fx *autonomousFixture) {
	t.Helper()
	ctx := context.Background()
	sessions, err := fx.store.ListSessions(ctx, "p")
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	for _, rec := range sessions {
		rec.Activity = domain.Activity{State: domain.ActivityIdle, LastActivityAt: fx.clk.Now().Add(-time.Hour)}
		rec.TurnCompletedAt = fx.clk.Now().Add(-time.Hour)
		rec.UpdatedAt = fx.clk.Now()
		if err := fx.store.UpdateSession(ctx, rec); err != nil {
			t.Fatalf("UpdateSession(%s): %v", rec.ID, err)
		}
	}
}
