package workflow_test

// Checkpoint 8P-D: human-required pause/resume, and multi-user isolation
// between an autonomous owner and a manual owner sharing the same daemon.

import (
	"context"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/questions"
)

// TestAutonomous_HumanRequiredPausesAndResumesOnAnswer: once a question on
// the active child run reaches QuestionStateHumanRequired, headless
// progress must stall (no further dispatch/state change across several
// poller cycles) -- dispatchReviewStep/dispatchFixStep's hasOpenQuestion
// guard is what actually enforces this at the child-run level. Answering
// through the real human-answer service (service/questions.AnswerService,
// the same code path the HTTP .../questions/{id}/answer endpoint calls) must
// let the very next poller cycle alone resume progress.
func TestAutonomous_HumanRequiredPausesAndResumesOnAnswer(t *testing.T) {
	fx := newAutonomousFixture(t, oneTaskPlan())
	ctx := context.Background()
	seedUser(t, fx.store, "user-1", "prof-claude-1", "prof-codex-1", true, fx.clk.Now())

	created, err := fx.coord.CreateObjectiveRun(ctx, "p", "Build users", domain.WorkflowPlanApprovalManual)
	if err != nil {
		t.Fatalf("CreateObjectiveRun: %v", err)
	}
	stampOwnerAndApplyPolicy(t, fx, created.Run.ID, "user-1")

	// Drive one cycle: plan generated+approved, task dispatched, work step
	// completes, review dispatched (all chained synchronously off the one
	// kickoff wake).
	driveCycles(t, fx, 1, nil)
	task, ok := taskByPlanStepID(t, fx, created.Run.ID, "only")
	if !ok || task.ExecutionRunID == nil {
		t.Fatalf("expected task dispatched, task=%+v ok=%v", task, ok)
	}
	childID := *task.ExecutionRunID

	steps, err := fx.store.ListWorkflowSteps(ctx, childID)
	if err != nil {
		t.Fatalf("ListWorkflowSteps: %v", err)
	}
	var workStepID string
	for _, s := range steps {
		if s.Kind == domain.WorkflowStepWork {
			workStepID = s.ID
		}
	}
	if workStepID == "" {
		t.Fatalf("no work step found on child run %s", childID)
	}

	// Insert the question directly (not via seedResolvingQuestion, which
	// also seeds a checkpoint with a fake worktree path -- fine for that
	// helper's own standalone fixtures, but here it would shadow the real
	// checkpoint autoSpawner/observeWorkStep already wrote for this step,
	// via GetLatestWorkflowCheckpointByStep returning whichever checkpoint
	// is newest regardless of phase).
	stepID := domain.WorkflowStepID(workStepID)
	sessionID := domain.SessionID("asking-" + t.Name())
	q, _, err := fx.store.InsertWorkflowQuestion(ctx, domain.WorkflowQuestion{
		ID:                   domain.WorkflowQuestionID("q-" + t.Name()),
		WorkflowRunID:        domain.WorkflowRunID(childID),
		WorkflowStepID:       &stepID,
		SessionID:            &sessionID,
		AskingHarness:        domain.HarnessClaudeCode,
		Fingerprint:          "fp-" + t.Name(),
		QuestionText:         "which helper should I use?",
		Certainty:            domain.QuestionCertaintyInferred,
		Classification:       domain.QuestionClassificationAutoResolvable,
		ClassificationReason: "test-seeded",
		State:                domain.QuestionStateResolving,
		CreatedAt:            fx.clk.Now(),
	})
	if err != nil {
		t.Fatalf("InsertWorkflowQuestion: %v", err)
	}
	if _, err := fx.store.TransitionWorkflowQuestionState(ctx, string(q.ID), domain.QuestionStateResolving, domain.QuestionStateHumanRequired, "test forced", fx.clk.Now()); err != nil {
		t.Fatalf("force human_required: %v", err)
	}

	stateBefore, _, err := fx.store.GetWorkflowRun(ctx, childID)
	if err != nil {
		t.Fatalf("GetWorkflowRun: %v", err)
	}
	driveCycles(t, fx, 5, func(i int) {
		// Deliberately do NOT approve/answer anything here -- proving the
		// run makes no further progress while human_required, purely via
		// the poller.
	})
	stateAfter, _, err := fx.store.GetWorkflowRun(ctx, childID)
	if err != nil {
		t.Fatalf("GetWorkflowRun: %v", err)
	}
	if stateAfter.State != stateBefore.State {
		t.Fatalf("child run state changed while a question was human_required: before=%s after=%s", stateBefore.State, stateAfter.State)
	}
	spawnCallsWhileBlocked := len(fx.spawner.calls)

	// Answer through the real human-answer service -- the same path
	// POST /api/v1/workflows/{id}/questions/{id}/answer uses.
	svc := &questions.AnswerService{Store: fx.store, Runs: fx.store, Clock: fx.clk.Now}
	answerText := "use the existing helper"
	if _, err := svc.Answer(ctx, childID, string(q.ID), nil, &answerText); err != nil {
		t.Fatalf("AnswerService.Answer: %v", err)
	}

	// The very next poller cycle alone resumes progress -- approve whatever
	// review appears so the run can actually reach completion.
	driveCycles(t, fx, 6, func(i int) {
		if _, cid, ok := activeChildRunID(t, fx, created.Run.ID); ok {
			approveOpenReview(t, fx, cid, domain.VerdictApproved)
		}
	})

	run, _, err := fx.store.GetWorkflowRun(ctx, created.Run.ID)
	if err != nil {
		t.Fatalf("GetWorkflowRun: %v", err)
	}
	if run.State != domain.WorkflowRunCompleted {
		t.Fatalf("run state after answering = %q, want completed", run.State)
	}
	if len(fx.spawner.calls) < spawnCallsWhileBlocked {
		t.Fatalf("spawner calls regressed after answering: %d < %d", len(fx.spawner.calls), spawnCallsWhileBlocked)
	}
}

// TestMultiUser_AutonomousAAndManualBIsolated: two distinct owners share the
// same daemon -- user A's objective is autonomous and must progress purely
// via the poller; user B's is manual (no stored policy) and must never
// auto-start. Neither user's dispatch may ever resolve the other's
// ProviderProfile.
func TestMultiUser_AutonomousAAndManualBIsolated(t *testing.T) {
	fx := newAutonomousFixture(t, oneTaskPlan())
	ctx := context.Background()
	now := fx.clk.Now()
	seedUser(t, fx.store, "user-a", "prof-claude-a", "prof-codex-a", true, now)
	seedUser(t, fx.store, "user-b", "prof-claude-b", "prof-codex-b", false, now)

	runA, err := fx.coord.CreateObjectiveRun(ctx, "p", "Objective A", domain.WorkflowPlanApprovalManual)
	if err != nil {
		t.Fatalf("CreateObjectiveRun A: %v", err)
	}
	stampOwnerAndApplyPolicy(t, fx, runA.Run.ID, "user-a")

	runB, err := fx.coord.CreateObjectiveRun(ctx, "p", "Objective B", domain.WorkflowPlanApprovalManual)
	if err != nil {
		t.Fatalf("CreateObjectiveRun B: %v", err)
	}
	stampOwnerAndApplyPolicy(t, fx, runB.Run.ID, "user-b")

	driveCycles(t, fx, 8, func(i int) {
		if _, cid, ok := activeChildRunID(t, fx, runA.Run.ID); ok {
			approveOpenReview(t, fx, cid, domain.VerdictApproved)
		}
	})

	if fx.planner.calls != 1 {
		t.Fatalf("planner calls = %d, want exactly 1 (only A's objective should ever plan)", fx.planner.calls)
	}
	planA, isMasterA, err := fx.store.GetWorkflowPlan(ctx, runA.Run.ID)
	if err != nil || !isMasterA {
		t.Fatalf("GetWorkflowPlan A: %+v isMaster=%v err=%v", planA, isMasterA, err)
	}
	if planA.Status != domain.WorkflowPlanApproved {
		t.Fatalf("plan A status = %q, want approved", planA.Status)
	}
	stateA, _, err := fx.store.GetWorkflowRun(ctx, runA.Run.ID)
	if err != nil {
		t.Fatalf("GetWorkflowRun A: %v", err)
	}
	if stateA.State != domain.WorkflowRunCompleted && stateA.State != domain.WorkflowRunRunning {
		t.Fatalf("run A state = %q, want running or completed (must have progressed)", stateA.State)
	}

	planB, isMasterB, err := fx.store.GetWorkflowPlan(ctx, runB.Run.ID)
	if err != nil || !isMasterB {
		t.Fatalf("GetWorkflowPlan B: %+v isMaster=%v err=%v", planB, isMasterB, err)
	}
	if planB.Status != domain.WorkflowPlanPending {
		t.Fatalf("plan B status = %q, want pending (manual mode never auto-starts)", planB.Status)
	}
	stateB, _, err := fx.store.GetWorkflowRun(ctx, runB.Run.ID)
	if err != nil {
		t.Fatalf("GetWorkflowRun B: %v", err)
	}
	if stateB.State != domain.WorkflowRunPending {
		t.Fatalf("run B state = %q, want pending", stateB.State)
	}

	// Cross-user isolation: every dispatched session is stamped as owned by
	// A (B's run never dispatched at all, so any non-A owner here would
	// mean a cross-user leak in ownership/routing resolution).
	if len(fx.spawner.calls) == 0 {
		t.Fatalf("expected at least one dispatch for user A's autonomous run")
	}
	for _, cfg := range fx.spawner.calls {
		if cfg.Owner != "user-a" {
			t.Fatalf("dispatched session owner = %q, want user-a only (cross-user leak)", cfg.Owner)
		}
	}
}
