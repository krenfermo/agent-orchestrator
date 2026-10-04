package questions_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/claudecode"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/questions"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// TestDetect_AutoResolvableBudgetIsEnforced is Checkpoint 8K-B pass 2's
// budget-sharing test: confirms the chosen decision behaves as documented
// (see detector.go's doc comment on the widened classification condition) —
// an auto_resolvable question on a step that has already exhausted
// MaxAutoAnsweredQuestionsPerStep via prior policy-answered questions on
// that SAME step goes straight to human_required, not resolving. Before this
// pass, auto_resolvable never read PolicyAnsweredCount at all, so this
// budget was silently unenforced for it.
func TestDetect_AutoResolvableBudgetIsEnforced(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	ctx := context.Background()
	now := time.Date(2026, time.August, 16, 10, 0, 0, 0, time.UTC)
	stepID := domain.WorkflowStepID("step-budget-1")

	// Seed one already policy-answered question on this step, exhausting a
	// budget of 1. InsertWorkflowQuestion only persists the captured-state
	// columns (never answer_source/answer_text at insert time — those are
	// always written through the real answer transition), so seeding a
	// realistic "already answered" row takes the same two-step path
	// production code uses: insert pending, then AnswerWorkflowQuestion.
	prior, _, err := store.InsertWorkflowQuestion(ctx, domain.WorkflowQuestion{
		ID:             "q-prior-policy",
		WorkflowRunID:  "run-1",
		WorkflowStepID: &stepID,
		Fingerprint:    "fp-prior",
		QuestionText:   "Should I push directly to main?",
		Certainty:      domain.QuestionCertaintyInferred,
		Classification: domain.QuestionClassificationPolicyResolvable,
		State:          domain.QuestionStatePending,
		CreatedAt:      now,
	})
	if err != nil {
		t.Fatalf("seed prior policy-answered question: %v", err)
	}
	if ok, err := store.AnswerWorkflowQuestion(ctx, string(prior.ID), domain.QuestionStatePending, domain.QuestionStateAnswered,
		domain.AnswerSourcePolicy, "No, open a PR instead.", "", now); err != nil || !ok {
		t.Fatalf("answer prior question: ok=%v err=%v", ok, err)
	}

	sessionID := domain.SessionID("sess-1")
	res, err := questions.Detect(ctx, store, claudecode.QuestionParser{}, questions.DetectInput{
		RunID:                  domain.WorkflowRunID("run-1"),
		StepID:                 &stepID,
		SessionID:              &sessionID,
		AskingHarness:          domain.AgentHarness("claude-code"),
		PaneText:               autoResolvableQuestionPane(),
		CaptureProvider:        "tmux",
		PolicyVersionAtCapture: "v1",
		Branch:                 "feature/foo",
		WorktreePath:           "/repos/foo",
		MaxAutoAnswered:        1,
		Now:                    now,
		NewID:                  func() string { return "q-auto-1" },
	})
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if !res.Inserted {
		t.Fatalf("expected the new question to be inserted (different fingerprint/step id from the seeded one)")
	}
	if res.Question.Classification != domain.QuestionClassificationAutoResolvable {
		t.Fatalf("classification = %v, want auto_resolvable", res.Question.Classification)
	}
	if res.Question.State != domain.QuestionStateHumanRequired {
		t.Fatalf("state = %v, want human_required (budget exhausted by the step's prior policy-answered question)", res.Question.State)
	}

	// Confirm the reference counting: on a step with NO prior auto-answered
	// question, the same budget=1 must still allow the FIRST auto_resolvable
	// question to reach resolving.
	otherStep := domain.WorkflowStepID("step-budget-2")
	res2, err := questions.Detect(ctx, store, claudecode.QuestionParser{}, questions.DetectInput{
		RunID:                  domain.WorkflowRunID("run-1"),
		StepID:                 &otherStep,
		SessionID:              &sessionID,
		AskingHarness:          domain.AgentHarness("claude-code"),
		PaneText:               autoResolvableQuestionPane(),
		CaptureProvider:        "tmux",
		PolicyVersionAtCapture: "v1",
		Branch:                 "feature/bar",
		WorktreePath:           "/repos/bar",
		MaxAutoAnswered:        1,
		Now:                    now,
		NewID:                  func() string { return "q-auto-2" },
	})
	if err != nil {
		t.Fatalf("Detect (other step): %v", err)
	}
	if res2.Question.State != domain.QuestionStateResolving {
		t.Fatalf("state = %v, want resolving (no prior auto-answered question on this step)", res2.Question.State)
	}
}
