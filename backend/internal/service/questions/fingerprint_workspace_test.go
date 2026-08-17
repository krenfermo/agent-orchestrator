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

func autoResolvableQuestionPane() string {
	return "Which existing helper should I use for retrying HTTP calls?\n" +
		"❯ 1. httputil.Retry\n" +
		"  2. Write a new one\n"
}

// TestDetect_WorkspaceFingerprintThreadingProducesDistinctQuestions is
// Checkpoint 8K-B's required fingerprint test: identical question text
// captured under two different workspace fingerprints must produce two
// distinct question rows (a genuinely different diff is a new decision),
// while the SAME workspace fingerprint dedupes as before (8K-A's existing
// behavior, unchanged). Exercises questions.Detect/Fingerprint directly
// (workflow's own detectQuestionForStep now threads
// WorkspaceFingerprintAtCapture through from the step's latest checkpoint —
// see questions_wiring.go — but its own dispatch-guard prevents a second
// capture attempt for the same step while the first question is still open,
// so the fingerprint-differentiation behavior itself is exercised here at
// the Detect level, same as this package's other Detect tests).
func TestDetect_WorkspaceFingerprintThreadingProducesDistinctQuestions(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	ctx := context.Background()
	now := time.Date(2026, time.August, 16, 10, 0, 0, 0, time.UTC)
	sessionID := domain.SessionID("sess-1")
	stepID := domain.WorkflowStepID("step-1")

	in := questions.DetectInput{
		RunID:                  domain.WorkflowRunID("run-1"),
		StepID:                 &stepID,
		SessionID:              &sessionID,
		AskingHarness:          domain.AgentHarness("claude-code"),
		PaneText:               autoResolvableQuestionPane(),
		CaptureProvider:        "tmux",
		PolicyVersionAtCapture: "v1",
		Branch:                 "feature/foo",
		WorktreePath:           "/repo/foo",
		Now:                    now,
	}

	// First capture at workspace fingerprint "fp-a".
	inA := in
	inA.WorkspaceFingerprintAtCapture = "fp-a"
	inA.NewID = func() string { return "q-fp-a" }
	resA, err := questions.Detect(ctx, store, claudecode.QuestionParser{}, inA)
	if err != nil {
		t.Fatalf("Detect (fp-a): %v", err)
	}
	if !resA.Inserted {
		t.Fatalf("expected fp-a capture to insert a new question")
	}

	// Same fingerprint again: must dedupe (8K-A's existing INSERT OR IGNORE
	// behavior, unchanged).
	inADup := inA
	inADup.NewID = func() string { return "q-fp-a-dup" }
	resADup, err := questions.Detect(ctx, store, claudecode.QuestionParser{}, inADup)
	if err != nil {
		t.Fatalf("Detect (fp-a dup): %v", err)
	}
	if resADup.Inserted {
		t.Fatalf("expected same-fingerprint recapture to dedupe, not insert")
	}
	if resADup.Question.ID != resA.Question.ID {
		t.Fatalf("dedup returned a different question id: %v vs %v", resADup.Question.ID, resA.Question.ID)
	}

	// Different workspace fingerprint, identical question text: a genuinely
	// new question row (the desired "same question, different diff" case).
	inB := in
	inB.WorkspaceFingerprintAtCapture = "fp-b"
	inB.NewID = func() string { return "q-fp-b" }
	resB, err := questions.Detect(ctx, store, claudecode.QuestionParser{}, inB)
	if err != nil {
		t.Fatalf("Detect (fp-b): %v", err)
	}
	if !resB.Inserted {
		t.Fatalf("expected a different workspace fingerprint to insert a NEW question, not dedupe")
	}
	if resB.Question.ID == resA.Question.ID {
		t.Fatalf("fp-b question got the same id as fp-a: %v", resB.Question.ID)
	}
	if resB.Question.Fingerprint == resA.Question.Fingerprint {
		t.Fatalf("fp-a and fp-b produced the same stored fingerprint, want distinct")
	}

	all, err := store.ListWorkflowQuestionsByRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("ListWorkflowQuestionsByRun: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("len(questions) = %d, want 2 (one per distinct workspace fingerprint)", len(all))
	}
}
