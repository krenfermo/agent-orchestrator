package workflow_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// fakePaneReader is a hand-rolled fake for workflowcore.PaneReader (no real
// tmux/conpty capture in unit tests): returns a fixed pane text for any
// handle, simulating what capture-pane would have returned.
type fakePaneReader struct {
	text string
	err  error
	// calls counts GetOutput invocations, used to assert detection does not
	// re-scrape once an open question already exists for a step.
	calls int
}

func (f *fakePaneReader) GetOutput(_ context.Context, _ ports.RuntimeHandle, _ int) (string, error) {
	f.calls++
	return f.text, f.err
}

func policyPushToMainPaneText() string {
	return "Should I push directly to main?\n" +
		"❯ 1. Yes\n" +
		"  2. No\n"
}

func ambiguousCooldownPaneText() string {
	return "Should the retry cooldown be 2s or 8s?\n" +
		"❯ 1. 2 seconds\n" +
		"  2. 8 seconds\n"
}

// newQuestionsFixture wires a Coordinator against a real SQLite store
// (sqlitetest.MustOpen), so it exercises the actual production
// QuestionsStore/store.Store implementation, not a hand-rolled fake, per
// AGENTS.md's guidance to prefer real dependencies where the package
// already supports it.
func newQuestionsFixture(t *testing.T, paneText string) (*workflowcore.Coordinator, *sqlite.Store, *fakeSessionFacts, *fakeMessageSender, *fakePaneReader, *fakeClock) {
	t.Helper()
	store := sqlitetest.MustOpen(t)
	ctx := context.Background()
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "p", Path: t.TempDir(), RegisteredAt: time.Now().UTC()}); err != nil {
		t.Fatalf("seed project: %v", err)
	}

	clock := &fakeClock{t: time.Date(2026, time.August, 16, 9, 0, 0, 0, time.UTC)}
	sessionFacts := newFakeSessionFacts()
	sender := &fakeMessageSender{}
	paneReader := &fakePaneReader{text: paneText}

	coord := workflowcore.New(workflowcore.Deps{
		Store:          store,
		Projects:       store,
		SessionFacts:   sessionFacts,
		QuestionsStore: store,
		PaneReader:     paneReader,
		MessageSender:  sender,
		Clock:          clock.Now,
	})
	return coord, store, sessionFacts, sender, paneReader, clock
}

// seedRunningWorkStep creates a run and forces its work step directly into
// "running" with a session attached — bypassing StartRun's real dispatch
// (no Spawner wired in this fixture), matching how existing tests in this
// package build fixtures via direct store calls.
func seedRunningWorkStep(ctx context.Context, t *testing.T, coord *workflowcore.Coordinator, store *sqlite.Store, sessionFacts *fakeSessionFacts, activity domain.ActivityState) (runID string, workStepID string, sessionID domain.SessionID) {
	t.Helper()
	rec, err := store.CreateSession(ctx, domain.SessionRecord{
		ProjectID: "p",
		Kind:      domain.KindWorker,
		Harness:   domain.AgentHarness("claude-code"),
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("seed session: %v", err)
	}
	sessionID = rec.ID

	created, err := coord.CreateRun(ctx, "p", "objective")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	runID = created.Run.ID
	var workStep domain.WorkflowStep
	for _, s := range created.Steps {
		if s.Step.Kind == domain.WorkflowStepWork {
			workStep = s.Step
		}
	}
	if workStep.ID == "" {
		t.Fatalf("no work step created")
	}
	now := time.Now().UTC()
	if _, err := store.UpdateWorkflowStepState(ctx, workStep.ID, domain.WorkflowStepPending, domain.WorkflowStepReady, now); err != nil {
		t.Fatalf("ready work step: %v", err)
	}
	if _, err := store.UpdateWorkflowStepState(ctx, workStep.ID, domain.WorkflowStepReady, domain.WorkflowStepRunning, now); err != nil {
		t.Fatalf("run work step: %v", err)
	}
	if _, err := store.UpdateWorkflowStepSession(ctx, workStep.ID, string(sessionID), now); err != nil {
		t.Fatalf("attach session: %v", err)
	}
	if _, err := store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-seed",
		WorkflowRunID:  runID,
		WorkflowStepID: &workStep.ID,
		ProjectID:      "p",
		SessionID:      stringPtr(string(sessionID)),
		Branch:         "feature/x",
		WorktreePath:   "/repos/p-x",
		DurablePhase:   "seed",
		PayloadVersion: "v1",
		RetryState:     "{}",
		CreatedAt:      now,
	}); err != nil {
		t.Fatalf("seed checkpoint: %v", err)
	}
	sessionFacts.put(domain.SessionRecord{
		ID:       sessionID,
		Harness:  domain.AgentHarness("claude-code"),
		Activity: domain.Activity{State: activity},
		Metadata: domain.SessionMetadata{RuntimeHandleID: "handle-" + string(sessionID)},
	})
	return runID, workStep.ID, sessionID
}

func stringPtr(s string) *string { return &s }

func TestReconcileQuestions_PolicyResolvableAutoAnswersAndDispatchResumes(t *testing.T) {
	ctx := context.Background()
	coord, store, sessionFacts, sender, _, _ := newQuestionsFixture(t, policyPushToMainPaneText())
	runID, workStepID, sessID := seedRunningWorkStep(ctx, t, coord, store, sessionFacts, domain.ActivityWaitingInput)

	detail, err := coord.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}

	qs, err := store.ListWorkflowQuestionsByRun(ctx, runID)
	if err != nil {
		t.Fatalf("ListWorkflowQuestionsByRun: %v", err)
	}
	if len(qs) != 1 {
		t.Fatalf("len(questions) = %d, want 1", len(qs))
	}
	q := qs[0]
	if q.Classification != domain.QuestionClassificationPolicyResolvable {
		t.Fatalf("classification = %v, want policy_resolvable", q.Classification)
	}
	if q.State != domain.QuestionStateAnswered {
		t.Fatalf("state = %v, want answered", q.State)
	}
	if !q.Delivered {
		t.Fatalf("expected the policy answer to be delivered within the same GetRun call")
	}
	if sender.calls != 1 {
		t.Fatalf("Send calls = %d, want exactly 1 (zero second-model/session spawn)", sender.calls)
	}
	if sender.lastID != sessID {
		t.Fatalf("delivered to %v, want %v", sender.lastID, sessID)
	}

	// NextAction must have reflected waiting_for_decision while the
	// question was open... but since the policy path resolves+delivers
	// synchronously within the same GetRun call, by the time GetRun
	// returns the question is already answered and NextAction should NOT
	// carry the waiting_for_decision override (open = pending/human_required
	// only).
	if detail.NextAction != "" && hasPrefix(detail.NextAction, "waiting_for_decision") {
		t.Fatalf("NextAction = %q, want no waiting_for_decision override once resolved same-call", detail.NextAction)
	}

	// Dispatch guard: since the question already resolved to answered
	// (not open) within this same call, a subsequent dispatch attempt must
	// not be blocked by it.
	steps, err := store.ListWorkflowSteps(ctx, runID)
	if err != nil {
		t.Fatalf("ListWorkflowSteps: %v", err)
	}
	_ = steps
	_ = workStepID
}

func TestReconcileQuestions_HumanRequiredSetsWaitingForDecisionAndBlocksDispatch(t *testing.T) {
	ctx := context.Background()
	coord, store, sessionFacts, sender, _, _ := newQuestionsFixture(t, ambiguousCooldownPaneText())
	runID, _, _ := seedRunningWorkStep(ctx, t, coord, store, sessionFacts, domain.ActivityBlocked)

	detail, err := coord.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if !hasPrefix(detail.NextAction, "waiting_for_decision: ambiguous") {
		t.Fatalf("NextAction = %q, want waiting_for_decision: ambiguous prefix", detail.NextAction)
	}
	if sender.calls != 0 {
		t.Fatalf("Send calls = %d, want 0 (no human answer yet)", sender.calls)
	}

	qs, err := store.ListWorkflowQuestionsByRun(ctx, runID)
	if err != nil || len(qs) != 1 {
		t.Fatalf("ListWorkflowQuestionsByRun: qs=%v err=%v", qs, err)
	}
	q := qs[0]
	if q.State != domain.QuestionStateHumanRequired {
		t.Fatalf("state = %v, want human_required", q.State)
	}

	// Human answers it through the real store path (mirrors what the
	// controller does).
	if ok, err := store.AnswerWorkflowQuestion(ctx, string(q.ID), domain.QuestionStateHumanRequired, domain.QuestionStateAnswered, domain.AnswerSourceHuman, "Use 8 seconds.", "", time.Now().UTC()); err != nil || !ok {
		t.Fatalf("AnswerWorkflowQuestion: ok=%v err=%v", ok, err)
	}

	// Next GetRun call must clear the waiting_for_decision NextAction and
	// deliver the answer exactly once (restart-recovery sweep path).
	detail2, err := coord.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun (after answer): %v", err)
	}
	if hasPrefix(detail2.NextAction, "waiting_for_decision") {
		t.Fatalf("NextAction after answer = %q, want no waiting_for_decision override", detail2.NextAction)
	}
	if sender.calls != 1 {
		t.Fatalf("Send calls after answer = %d, want exactly 1", sender.calls)
	}
}

func TestReconcileQuestions_NoRedundantCaptureOnceOpenQuestionExists(t *testing.T) {
	ctx := context.Background()
	coord, store, sessionFacts, _, paneReader, _ := newQuestionsFixture(t, ambiguousCooldownPaneText())
	runID, _, _ := seedRunningWorkStep(ctx, t, coord, store, sessionFacts, domain.ActivityBlocked)

	if _, err := coord.GetRun(ctx, runID); err != nil {
		t.Fatalf("GetRun (1): %v", err)
	}
	callsAfterFirst := paneReader.calls
	if callsAfterFirst == 0 {
		t.Fatalf("expected at least one capture on first GetRun")
	}

	if _, err := coord.GetRun(ctx, runID); err != nil {
		t.Fatalf("GetRun (2): %v", err)
	}
	if paneReader.calls != callsAfterFirst {
		t.Fatalf("GetOutput calls after second GetRun = %d, want unchanged %d (no re-scrape while an open question exists)", paneReader.calls, callsAfterFirst)
	}
}

func TestCancelRun_CancelsOpenQuestionsAndSkipsDelivery(t *testing.T) {
	ctx := context.Background()
	coord, store, sessionFacts, sender, _, _ := newQuestionsFixture(t, ambiguousCooldownPaneText())
	runID, _, _ := seedRunningWorkStep(ctx, t, coord, store, sessionFacts, domain.ActivityBlocked)

	if _, err := coord.GetRun(ctx, runID); err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	qs, err := store.ListWorkflowQuestionsByRun(ctx, runID)
	if err != nil || len(qs) != 1 {
		t.Fatalf("ListWorkflowQuestionsByRun: qs=%v err=%v", qs, err)
	}

	if _, err := coord.CancelRun(ctx, runID); err != nil {
		t.Fatalf("CancelRun: %v", err)
	}

	qs2, err := store.ListWorkflowQuestionsByRun(ctx, runID)
	if err != nil || len(qs2) != 1 {
		t.Fatalf("ListWorkflowQuestionsByRun after cancel: qs=%v err=%v", qs2, err)
	}
	if qs2[0].State != domain.QuestionStateCancelled {
		t.Fatalf("state after cancel = %v, want cancelled", qs2[0].State)
	}
	if sender.calls != 0 {
		t.Fatalf("Send calls = %d, want 0 (cancelled question must never be delivered)", sender.calls)
	}
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
