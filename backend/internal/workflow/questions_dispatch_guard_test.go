package workflow_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// TestDispatchWorkStep_BlockedByOpenQuestion_ThenResumesAfterCancel is an
// explicit, direct exercise of the dispatch-guard added to dispatchWorkStep
// (Checkpoint 8K-A): with a real Spawner wired, StartRun must NOT spawn a
// session for the work step while an open question exists on it, and must
// dispatch normally once the question is no longer open.
func TestDispatchWorkStep_BlockedByOpenQuestion_ThenResumesAfterCancel(t *testing.T) {
	ctx := context.Background()
	store := sqlitetest.MustOpen(t)
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "p", Path: t.TempDir(), RegisteredAt: time.Now().UTC()}); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	sessionFacts := newFakeSessionFacts()
	// The real sqlite store enforces workflow_steps.session_id's FK against
	// a real sessions row, unlike the in-memory fakeStore other dispatch
	// tests use — so the fakeSpawner here must hand back an ID that
	// actually exists in the sessions table.
	seededSession, err := store.CreateSession(ctx, domain.SessionRecord{
		ProjectID: "p",
		Kind:      domain.KindWorker,
		Harness:   domain.AgentHarness("codex"),
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("seed spawned session: %v", err)
	}
	spawner := &fakeSpawner{rec: domain.SessionRecord{ID: seededSession.ID}, facts: sessionFacts}

	coord := workflowcore.New(workflowcore.Deps{
		Store:          store,
		Projects:       store,
		Spawner:        spawner,
		SessionFacts:   sessionFacts,
		QuestionsStore: store,
	})

	created, err := coord.CreateRun(ctx, "p", "objective")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	runID := created.Run.ID
	var workStepID string
	for _, s := range created.Steps {
		if s.Step.Kind == domain.WorkflowStepWork {
			workStepID = s.Step.ID
		}
	}
	if workStepID == "" {
		t.Fatalf("no work step")
	}

	// Seed an open (human_required) question directly on the work step,
	// simulating one detected on a prior cycle before the run was ever
	// started (edge case, but exercises the guard independent of the
	// detector itself).
	if _, _, err := store.InsertWorkflowQuestion(ctx, domain.WorkflowQuestion{
		ID:            "wfq-guard-1",
		WorkflowRunID: domain.WorkflowRunID(runID),
		WorkflowStepID: func() *domain.WorkflowStepID {
			id := domain.WorkflowStepID(workStepID)
			return &id
		}(),
		Fingerprint:    "fp-guard-1",
		QuestionText:   "Should the retry cooldown be 2s or 8s?",
		Certainty:      domain.QuestionCertaintyInferred,
		Classification: domain.QuestionClassificationAmbiguous,
		State:          domain.QuestionStateHumanRequired,
		CreatedAt:      time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed open question: %v", err)
	}

	if _, err := coord.StartRun(ctx, runID); err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if spawner.calls != 0 {
		t.Fatalf("Spawn calls = %d, want 0 while an open question blocks dispatch", spawner.calls)
	}
	steps, err := store.ListWorkflowSteps(ctx, runID)
	if err != nil {
		t.Fatalf("ListWorkflowSteps: %v", err)
	}
	for _, s := range steps {
		if s.ID == workStepID && s.SessionID != nil {
			t.Fatalf("work step got a session attached despite the open question guard")
		}
	}

	// Cancel the question directly (simulating human resolution via the
	// answer path setting it to a non-open state — here we use the run
	// cancel's bulk method target state to exercise "no longer open").
	if ok, err := store.AnswerWorkflowQuestion(ctx, "wfq-guard-1", domain.QuestionStateHumanRequired, domain.QuestionStateAnswered, domain.AnswerSourceHuman, "Use 8 seconds.", "", time.Now().UTC()); err != nil || !ok {
		t.Fatalf("AnswerWorkflowQuestion: ok=%v err=%v", ok, err)
	}

	// Reconcile (the daemon-boot recovery pass) is what retries a "ready"
	// work step that never got dispatched — StartRun itself only fires
	// once (pending->running); a step left "ready" behind an open-question
	// guard is exactly the case Reconcile exists to pick back up.
	if err := coord.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if spawner.calls == 0 {
		t.Fatalf("Spawn calls = %d, want >0 once the question is no longer open", spawner.calls)
	}
}
