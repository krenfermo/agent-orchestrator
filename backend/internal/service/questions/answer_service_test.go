package questions_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/questions"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

func TestAnswerService_Answer_HumanPathDeliversExactlyOnce(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	ctx := context.Background()
	now := time.Date(2026, time.August, 16, 10, 0, 0, 0, time.UTC)
	sessionID := domain.SessionID("sess-svc-1")

	saved, _, err := store.InsertWorkflowQuestion(ctx, domain.WorkflowQuestion{
		ID:            "q-svc-1",
		WorkflowRunID: "run-svc-1",
		SessionID:     &sessionID,
		Fingerprint:   "fp-svc-1",
		QuestionText:  "Should the retry cooldown be 2s or 8s?",
		StructuredChoices: []domain.QuestionChoice{
			{ID: "2s", Label: "2 seconds"},
			{ID: "8s", Label: "8 seconds"},
		},
		Certainty: domain.QuestionCertaintyInferred,
		State:     domain.QuestionStateHumanRequired,
		CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("seed question: %v", err)
	}

	sender := &fakeSender{}
	svc := &questions.AnswerService{Store: store, Runs: store, Sender: sender, Clock: func() time.Time { return now }}

	choiceID := "8s"
	updated, err := svc.Answer(ctx, "run-svc-1", string(saved.ID), &choiceID, nil)
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if updated.State != domain.QuestionStateAnswered {
		t.Fatalf("state = %v, want answered", updated.State)
	}
	if updated.AnswerText != "8 seconds" {
		t.Fatalf("answerText = %q, want %q", updated.AnswerText, "8 seconds")
	}
	if !updated.Delivered {
		t.Fatalf("expected delivered=true after Answer's immediate delivery attempt")
	}
	if sender.calls != 1 {
		t.Fatalf("Send calls = %d, want exactly 1", sender.calls)
	}

	// Double answer must be rejected, not silently overwritten.
	custom := "something else"
	if _, err := svc.Answer(ctx, "run-svc-1", string(saved.ID), nil, &custom); err != questions.ErrNotAnswerable {
		t.Fatalf("second Answer err = %v, want ErrNotAnswerable", err)
	}
	if sender.calls != 1 {
		t.Fatalf("Send calls after rejected double-answer = %d, want still 1", sender.calls)
	}
}

func TestAnswerService_Answer_InvalidChoiceRejected(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	ctx := context.Background()
	now := time.Now().UTC()

	saved, _, err := store.InsertWorkflowQuestion(ctx, domain.WorkflowQuestion{
		ID:                "q-svc-2",
		WorkflowRunID:     "run-svc-2",
		Fingerprint:       "fp-svc-2",
		QuestionText:      "Should the retry cooldown be 2s or 8s?",
		StructuredChoices: []domain.QuestionChoice{{ID: "2s", Label: "2 seconds"}},
		Certainty:         domain.QuestionCertaintyInferred,
		State:             domain.QuestionStateHumanRequired,
		CreatedAt:         now,
	})
	if err != nil {
		t.Fatalf("seed question: %v", err)
	}

	svc := &questions.AnswerService{Store: store, Runs: store}
	badChoice := "does-not-exist"
	if _, err := svc.Answer(ctx, "run-svc-2", string(saved.ID), &badChoice, nil); err != questions.ErrInvalidChoice {
		t.Fatalf("err = %v, want ErrInvalidChoice", err)
	}
}

func TestAnswerService_Answer_AmbiguousBodyRejected(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	ctx := context.Background()
	now := time.Now().UTC()
	saved, _, err := store.InsertWorkflowQuestion(ctx, domain.WorkflowQuestion{
		ID: "q-svc-3", WorkflowRunID: "run-svc-3", Fingerprint: "fp-svc-3",
		QuestionText: "x", Certainty: domain.QuestionCertaintyInferred,
		State: domain.QuestionStateHumanRequired, CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("seed question: %v", err)
	}
	svc := &questions.AnswerService{Store: store, Runs: store}

	choice := "a"
	custom := "b"
	if _, err := svc.Answer(ctx, "run-svc-3", string(saved.ID), &choice, &custom); err != questions.ErrAmbiguousAnswer {
		t.Fatalf("both set: err = %v, want ErrAmbiguousAnswer", err)
	}
	if _, err := svc.Answer(ctx, "run-svc-3", string(saved.ID), nil, nil); err != questions.ErrAmbiguousAnswer {
		t.Fatalf("neither set: err = %v, want ErrAmbiguousAnswer", err)
	}
}

func TestAnswerService_Answer_WrongRunRejected(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	ctx := context.Background()
	now := time.Now().UTC()
	saved, _, err := store.InsertWorkflowQuestion(ctx, domain.WorkflowQuestion{
		ID: "q-svc-4", WorkflowRunID: "run-svc-4", Fingerprint: "fp-svc-4",
		QuestionText: "x", Certainty: domain.QuestionCertaintyInferred,
		State: domain.QuestionStateHumanRequired, CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("seed question: %v", err)
	}
	svc := &questions.AnswerService{Store: store, Runs: store}
	custom := "answer"
	if _, err := svc.Answer(ctx, "some-other-run", string(saved.ID), nil, &custom); err != questions.ErrWrongRun {
		t.Fatalf("err = %v, want ErrWrongRun", err)
	}
}
