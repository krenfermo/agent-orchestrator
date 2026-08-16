package questions_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/claudecode"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/questions"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// fakeSender is a hand-rolled fake for questions.MessageSender: no HTTP, no
// network, just a call counter and captured args, per AGENTS.md's "no
// network calls in tests unless the package already has an integration/e2e
// pattern for them" rule.
type fakeSender struct {
	calls    int
	sessions []domain.SessionID
	messages []string
	err      error
}

func (f *fakeSender) Send(_ context.Context, id domain.SessionID, message string, _ *ports.SpawnAttachment) error {
	f.calls++
	f.sessions = append(f.sessions, id)
	f.messages = append(f.messages, message)
	return f.err
}

func policyQuestionPane() string {
	return "Should I push directly to main?\n" +
		"❯ 1. Yes\n" +
		"  2. No\n"
}

func ambiguousQuestionPane() string {
	return "Should the retry cooldown be 2s or 8s?\n" +
		"❯ 1. 2 seconds\n" +
		"  2. 8 seconds\n"
}

func TestDetect_PolicyResolvableDeliversWithNoSecondModelCall(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	ctx := context.Background()
	now := time.Date(2026, time.August, 16, 10, 0, 0, 0, time.UTC)
	sessionID := domain.SessionID("sess-1")
	stepID := domain.WorkflowStepID("step-1")

	res, err := questions.Detect(ctx, store, claudecode.QuestionParser{}, questions.DetectInput{
		RunID:                  domain.WorkflowRunID("run-1"),
		StepID:                 &stepID,
		SessionID:              &sessionID,
		AskingHarness:          domain.AgentHarness("claude-code"),
		PaneText:               policyQuestionPane(),
		CaptureProvider:        "tmux",
		PolicyVersionAtCapture: "v1",
		Branch:                 "feature/foo",
		WorktreePath:           "/repos/foo",
		MaxAutoAnswered:        5,
		Now:                    now,
		NewID:                  func() string { return "q-policy-1" },
	})
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if !res.Inserted {
		t.Fatalf("expected first detect to insert a new row")
	}
	if res.Question.Classification != domain.QuestionClassificationPolicyResolvable {
		t.Fatalf("classification = %v, want policy_resolvable", res.Question.Classification)
	}
	if res.Question.State != domain.QuestionStateAnswered {
		t.Fatalf("state = %v, want answered (policy resolver should have run synchronously, no LLM/session spawn)", res.Question.State)
	}
	if res.Question.AnswerSource == nil || *res.Question.AnswerSource != domain.AnswerSourcePolicy {
		t.Fatalf("answer source = %v, want policy", res.Question.AnswerSource)
	}

	sender := &fakeSender{}
	delivered, err := questions.DeliverAnswered(ctx, store, sender, "run-1", now.Add(time.Second))
	if err != nil {
		t.Fatalf("DeliverAnswered: %v", err)
	}
	if delivered != 1 {
		t.Fatalf("delivered = %d, want 1", delivered)
	}
	if sender.calls != 1 {
		t.Fatalf("Send calls = %d, want exactly 1 (zero second-model/session spawn)", sender.calls)
	}
	if sender.sessions[0] != sessionID {
		t.Fatalf("delivered to session %v, want %v", sender.sessions[0], sessionID)
	}

	// A second sweep must be a safe no-op: delivered stays true, Send not
	// called again.
	delivered2, err := questions.DeliverAnswered(ctx, store, sender, "run-1", now.Add(2*time.Second))
	if err != nil {
		t.Fatalf("DeliverAnswered (second sweep): %v", err)
	}
	if delivered2 != 0 {
		t.Fatalf("second sweep delivered = %d, want 0", delivered2)
	}
	if sender.calls != 1 {
		t.Fatalf("Send calls after second sweep = %d, want still 1", sender.calls)
	}
}

func TestDetect_IdempotentOnRepeatedFingerprint(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	ctx := context.Background()
	now := time.Date(2026, time.August, 16, 10, 0, 0, 0, time.UTC)
	stepID := domain.WorkflowStepID("step-1")

	in := questions.DetectInput{
		RunID:                  domain.WorkflowRunID("run-idem"),
		StepID:                 &stepID,
		AskingHarness:          domain.AgentHarness("claude-code"),
		PaneText:               ambiguousQuestionPane(),
		PolicyVersionAtCapture: "v1",
		MaxAutoAnswered:        5,
		Now:                    now,
		NewID:                  func() string { return "q-first" },
	}

	res1, err := questions.Detect(ctx, store, claudecode.QuestionParser{}, in)
	if err != nil {
		t.Fatalf("Detect (poll 1): %v", err)
	}
	if !res1.Inserted {
		t.Fatalf("expected first poll to insert")
	}

	// Second poll cycle, same pane, before the activity state changes:
	// same fingerprint, must not duplicate, reclassify, or double-answer.
	in.Now = now.Add(30 * time.Second)
	in.NewID = func() string { return "q-second-should-not-be-used" }
	res2, err := questions.Detect(ctx, store, claudecode.QuestionParser{}, in)
	if err != nil {
		t.Fatalf("Detect (poll 2): %v", err)
	}
	if res2.Inserted {
		t.Fatalf("expected second poll with identical fingerprint to be a no-op, not a fresh insert")
	}
	if res2.Question.ID != res1.Question.ID {
		t.Fatalf("second poll returned a different question id: %v vs %v", res2.Question.ID, res1.Question.ID)
	}

	all, err := store.ListWorkflowQuestionsByRun(ctx, "run-idem")
	if err != nil {
		t.Fatalf("ListWorkflowQuestionsByRun: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("len(all questions) = %d, want 1 (no duplicate row)", len(all))
	}
}

func TestDetect_UnknownCertaintyIsAlwaysHumanRequired(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	ctx := context.Background()
	now := time.Date(2026, time.August, 16, 10, 0, 0, 0, time.UTC)

	res, err := questions.Detect(ctx, store, claudecode.QuestionParser{}, questions.DetectInput{
		RunID:                  domain.WorkflowRunID("run-unknown"),
		AskingHarness:          domain.AgentHarness("claude-code"),
		PaneText:               "Working...\n\nWaiting for your response.\n",
		PolicyVersionAtCapture: "v1",
		MaxAutoAnswered:        5,
		Now:                    now,
		NewID:                  func() string { return "q-unknown" },
	})
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if res.Question.Certainty != domain.QuestionCertaintyUnknown {
		t.Fatalf("certainty = %v, want unknown", res.Question.Certainty)
	}
	if res.Question.QuestionText != "" {
		t.Fatalf("question text = %q, want empty (never invented)", res.Question.QuestionText)
	}
	if res.Question.State != domain.QuestionStateHumanRequired {
		t.Fatalf("state = %v, want human_required", res.Question.State)
	}
}

func TestHumanAnswer_DeliversExactlyOnce(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	ctx := context.Background()
	now := time.Date(2026, time.August, 16, 10, 0, 0, 0, time.UTC)
	sessionID := domain.SessionID("sess-human")
	stepID := domain.WorkflowStepID("step-human")

	res, err := questions.Detect(ctx, store, claudecode.QuestionParser{}, questions.DetectInput{
		RunID:                  domain.WorkflowRunID("run-human"),
		StepID:                 &stepID,
		SessionID:              &sessionID,
		AskingHarness:          domain.AgentHarness("claude-code"),
		PaneText:               ambiguousQuestionPane(),
		PolicyVersionAtCapture: "v1",
		MaxAutoAnswered:        5,
		Now:                    now,
		NewID:                  func() string { return "q-human-1" },
	})
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if res.Question.State != domain.QuestionStateHumanRequired {
		t.Fatalf("state = %v, want human_required", res.Question.State)
	}

	ok, err := store.AnswerWorkflowQuestion(ctx, string(res.Question.ID), domain.QuestionStateHumanRequired, domain.QuestionStateAnswered, domain.AnswerSourceHuman, "Use 8 seconds.", "", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("AnswerWorkflowQuestion: %v", err)
	}
	if !ok {
		t.Fatalf("expected human answer to apply (question was in human_required state)")
	}

	sender := &fakeSender{}
	delivered, err := questions.DeliverAnswered(ctx, store, sender, "run-human", now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("DeliverAnswered: %v", err)
	}
	if delivered != 1 || sender.calls != 1 {
		t.Fatalf("delivered=%d senderCalls=%d, want 1 and 1", delivered, sender.calls)
	}
}

func TestRestartRecovery_UndeliveredAnsweredQuestionIsSweptExactlyOnce(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	ctx := context.Background()
	now := time.Date(2026, time.August, 16, 10, 0, 0, 0, time.UTC)
	sessionID := domain.SessionID("sess-restart")

	// Simulate the "answered but not yet delivered" state directly, as if
	// a daemon crash happened between answering and the delivery sweep.
	q := domain.WorkflowQuestion{
		ID:            "q-restart-1",
		WorkflowRunID: "run-restart",
		SessionID:     &sessionID,
		Fingerprint:   "fp-restart-1",
		QuestionText:  "Should I push directly to main?",
		Certainty:     domain.QuestionCertaintyInferred,
		State:         domain.QuestionStateAnswered,
		CreatedAt:     now,
	}
	saved, inserted, err := store.InsertWorkflowQuestion(ctx, q)
	if err != nil {
		t.Fatalf("InsertWorkflowQuestion: %v", err)
	}
	if !inserted {
		t.Fatalf("expected fresh insert")
	}
	answeredAt := now.Add(time.Minute)
	ok, err := store.AnswerWorkflowQuestion(ctx, string(saved.ID), domain.QuestionStateAnswered, domain.QuestionStateAnswered, domain.AnswerSourcePolicy, "No, use branch x.", "", answeredAt)
	if err != nil || !ok {
		t.Fatalf("seed answer: ok=%v err=%v", ok, err)
	}

	sender := &fakeSender{}
	delivered, err := questions.DeliverAnswered(ctx, store, sender, "run-restart", now.Add(5*time.Minute))
	if err != nil {
		t.Fatalf("DeliverAnswered (recovery sweep): %v", err)
	}
	if delivered != 1 || sender.calls != 1 {
		t.Fatalf("recovery sweep delivered=%d senderCalls=%d, want 1 and 1", delivered, sender.calls)
	}

	// Calling it again afterward must be a safe no-op.
	delivered2, err := questions.DeliverAnswered(ctx, store, sender, "run-restart", now.Add(6*time.Minute))
	if err != nil {
		t.Fatalf("DeliverAnswered (second call): %v", err)
	}
	if delivered2 != 0 || sender.calls != 1 {
		t.Fatalf("after second sweep delivered=%d senderCalls=%d, want 0 and still 1", delivered2, sender.calls)
	}
}

func TestCancelOpenWorkflowQuestions_NoDeliveryAfterCancel(t *testing.T) {
	store := sqlitetest.MustOpen(t)
	ctx := context.Background()
	now := time.Date(2026, time.August, 16, 10, 0, 0, 0, time.UTC)
	sessionID := domain.SessionID("sess-cancel")

	stepID := domain.WorkflowStepID("step-cancel")
	res, err := questions.Detect(ctx, store, claudecode.QuestionParser{}, questions.DetectInput{
		RunID:                  domain.WorkflowRunID("run-cancel"),
		StepID:                 &stepID,
		SessionID:              &sessionID,
		AskingHarness:          domain.AgentHarness("claude-code"),
		PaneText:               ambiguousQuestionPane(),
		PolicyVersionAtCapture: "v1",
		MaxAutoAnswered:        5,
		Now:                    now,
		NewID:                  func() string { return "q-cancel-1" },
	})
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if res.Question.State != domain.QuestionStateHumanRequired {
		t.Fatalf("state = %v, want human_required", res.Question.State)
	}

	n, err := store.CancelOpenWorkflowQuestionsByRun(ctx, "run-cancel")
	if err != nil {
		t.Fatalf("CancelOpenWorkflowQuestionsByRun: %v", err)
	}
	if n != 1 {
		t.Fatalf("cancelled %d rows, want 1", n)
	}

	got, ok, err := store.GetWorkflowQuestion(ctx, string(res.Question.ID))
	if err != nil || !ok {
		t.Fatalf("GetWorkflowQuestion: ok=%v err=%v", ok, err)
	}
	if got.State != domain.QuestionStateCancelled {
		t.Fatalf("state after cancel = %v, want cancelled", got.State)
	}

	// No delivery attempt for a cancelled question: it is not
	// state=answered, so the sweep must ignore it.
	sender := &fakeSender{}
	delivered, err := questions.DeliverAnswered(ctx, store, sender, "run-cancel", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("DeliverAnswered: %v", err)
	}
	if delivered != 0 || sender.calls != 0 {
		t.Fatalf("delivered=%d senderCalls=%d, want 0 and 0 for a cancelled question", delivered, sender.calls)
	}
}
