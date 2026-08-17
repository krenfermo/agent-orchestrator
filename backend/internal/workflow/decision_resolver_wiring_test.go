package workflow_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/questions"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// fakeDecisionResolverLauncher is a hand-rolled fake for
// workflowcore.DecisionResolverLauncher, mirroring fakeReviewerAdapter's role
// in workflow_reviewer_launcher_test.go: it records every launch request so
// tests can assert dispatch happened at most once, and an optional onLaunch
// hook lets a test perform a REAL HTTP callback (simulating the `ao decision
// resolve` CLI) against a real httptest server.
type fakeDecisionResolverLauncher struct {
	calls        []workflowcore.DecisionResolverLaunchRequest
	preflightErr error
	launchErr    error
	onLaunch     func(req workflowcore.DecisionResolverLaunchRequest)
}

func (f *fakeDecisionResolverLauncher) Preflight(context.Context, domain.AgentHarness, string) error {
	return f.preflightErr
}

func (f *fakeDecisionResolverLauncher) Launch(_ context.Context, req workflowcore.DecisionResolverLaunchRequest) (workflowcore.DecisionResolverLaunchResult, error) {
	f.calls = append(f.calls, req)
	if f.launchErr != nil {
		return workflowcore.DecisionResolverLaunchResult{}, f.launchErr
	}
	if f.onLaunch != nil {
		f.onLaunch(req)
	}
	return workflowcore.DecisionResolverLaunchResult{HandleID: "handle-" + req.ResolutionID}, nil
}

// newDecisionResolverFixture wires a Coordinator against a real SQLite store
// (mirroring newQuestionsFixture in questions_wiring_test.go), additionally
// supplying a DecisionResolverLauncher.
func newDecisionResolverFixture(t *testing.T, paneText string, launcher workflowcore.DecisionResolverLauncher) (*workflowcore.Coordinator, *sqlite.Store, *fakeSessionFacts, *fakeMessageSender, *fakeClock) {
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
		Store:                    store,
		Projects:                 store,
		SessionFacts:             sessionFacts,
		QuestionsStore:           store,
		PaneReader:               paneReader,
		MessageSender:            sender,
		DecisionResolverLauncher: launcher,
		Clock:                    clock.Now,
	})
	return coord, store, sessionFacts, sender, clock
}

func autoResolvableDiscoveryPaneText() string {
	return "Which existing helper should I use for retrying HTTP calls?\n" +
		"❯ 1. httputil.Retry\n" +
		"  2. Write a new one\n"
}

// seedRunWithPolicy creates a run plus a single real work step directly
// through the real store (bypassing Coordinator.CreateRun, which always
// seeds DefaultWorkflowPolicy) so tests can control
// WorkflowPolicy.AllowSameProviderResolver, mirroring failover_test.go's own
// "overwrite the policy snapshot" pattern but against the real SQLite store
// rather than the in-memory fakeStore (this package's question-wiring
// fixtures already use the real store for QuestionsStore, so staying on one
// store keeps the fixture consistent). Returns the run and the real step id
// (workflow_checkpoints.workflow_step_id has a FK to workflow_steps, so a
// fabricated step id cannot be used).
func seedRunWithPolicy(t *testing.T, ctx context.Context, store *sqlite.Store, policyJSON string) (domain.WorkflowRun, string) {
	t.Helper()
	now := time.Now().UTC()
	runID := "wf-" + t.Name()
	stepID := "step-" + t.Name()
	run := domain.WorkflowRun{
		ID:             runID,
		ProjectID:      "p",
		Objective:      "objective",
		State:          domain.WorkflowRunRunning,
		PolicyVersion:  "v1",
		PolicySnapshot: policyJSON,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	steps := []domain.WorkflowStep{{
		ID:            stepID,
		WorkflowRunID: runID,
		Kind:          domain.WorkflowStepWork,
		Ordinal:       1,
		State:         domain.WorkflowStepRunning,
		CreatedAt:     now,
		UpdatedAt:     now,
	}}
	inserted, _, err := store.CreateWorkflowRun(ctx, run, steps)
	if err != nil {
		t.Fatalf("CreateWorkflowRun: %v", err)
	}
	return inserted, stepID
}

// seedResolvingQuestion inserts a checkpoint (for worktree/branch) and a
// state=resolving question directly, simulating what Detect would have
// produced for an auto_resolvable question — used by the provider-selection
// tests below, which test dispatch in isolation from real pane detection.
func seedResolvingQuestion(t *testing.T, ctx context.Context, store *sqlite.Store, runID, stepIDStr string, askingHarness domain.AgentHarness) domain.WorkflowQuestion {
	t.Helper()
	now := time.Now().UTC()
	stepID := domain.WorkflowStepID(stepIDStr)
	if _, err := store.CreateWorkflowCheckpoint(ctx, domain.WorkflowCheckpoint{
		ID:             "wfc-" + t.Name(),
		WorkflowRunID:  runID,
		WorkflowStepID: &stepIDStr,
		ProjectID:      "p",
		Branch:         "feature/x",
		WorktreePath:   "/repos/p-x",
		DurablePhase:   "seed",
		PayloadVersion: "v1",
		RetryState:     "{}",
		CreatedAt:      now,
	}); err != nil {
		t.Fatalf("seed checkpoint: %v", err)
	}
	sessionID := domain.SessionID("asking-" + t.Name())
	q, _, err := store.InsertWorkflowQuestion(ctx, domain.WorkflowQuestion{
		ID:                   domain.WorkflowQuestionID("q-" + t.Name()),
		WorkflowRunID:        domain.WorkflowRunID(runID),
		WorkflowStepID:       &stepID,
		SessionID:            &sessionID,
		AskingHarness:        askingHarness,
		Fingerprint:          "fp-" + t.Name(),
		QuestionText:         "which helper should I use?",
		Certainty:            domain.QuestionCertaintyInferred,
		Classification:       domain.QuestionClassificationAutoResolvable,
		ClassificationReason: "test-seeded",
		State:                domain.QuestionStateResolving,
		CreatedAt:            now,
	})
	if err != nil {
		t.Fatalf("seed question: %v", err)
	}
	return q
}

// --- Provider selection ---

func TestDecisionProviderSelect_ClaudeAsksCodexPreferred(t *testing.T) {
	launcher := &fakeDecisionResolverLauncher{}
	coord, store, _, _, _ := newDecisionResolverFixture(t, "", launcher)
	ctx := context.Background()

	run, stepID := seedRunWithPolicy(t, ctx, store, `{"version":"v1","maxFixCycles":3}`)
	seedResolvingQuestion(t, ctx, store, run.ID, stepID, domain.HarnessClaudeCode)

	if _, err := coord.GetRun(ctx, run.ID); err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if len(launcher.calls) != 1 {
		t.Fatalf("launcher calls = %d, want 1", len(launcher.calls))
	}
	if launcher.calls[0].Harness != domain.HarnessCodex {
		t.Fatalf("resolver harness = %q, want codex (opposite of claude-code)", launcher.calls[0].Harness)
	}
}

func TestDecisionProviderSelect_CodexAsksClaudePreferred(t *testing.T) {
	launcher := &fakeDecisionResolverLauncher{}
	coord, store, _, _, _ := newDecisionResolverFixture(t, "", launcher)
	ctx := context.Background()

	run, stepID := seedRunWithPolicy(t, ctx, store, `{"version":"v1","maxFixCycles":3}`)
	seedResolvingQuestion(t, ctx, store, run.ID, stepID, domain.HarnessCodex)

	if _, err := coord.GetRun(ctx, run.ID); err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if len(launcher.calls) != 1 {
		t.Fatalf("launcher calls = %d, want 1", len(launcher.calls))
	}
	if launcher.calls[0].Harness != domain.HarnessClaudeCode {
		t.Fatalf("resolver harness = %q, want claude-code (opposite of codex)", launcher.calls[0].Harness)
	}
}

func TestDecisionProviderSelect_PreferredUnavailableNoSameProviderWaitsForCapacity(t *testing.T) {
	launcher := &fakeDecisionResolverLauncher{}
	coord, store, _, _, clock := newDecisionResolverFixture(t, "", launcher)
	ctx := context.Background()

	run, stepID := seedRunWithPolicy(t, ctx, store, `{"version":"v1","maxFixCycles":3}`) // AllowSameProviderResolver defaults false
	seedResolvingQuestion(t, ctx, store, run.ID, stepID, domain.HarnessClaudeCode)

	if _, err := store.RecordAgentHealthEvent(ctx, domain.AgentHealthEvent{
		ID: "ahe-1", Harness: domain.HarnessCodex, State: domain.AgentHealthUnavailable, Reason: "test", CreatedAt: clock.Now(),
	}); err != nil {
		t.Fatalf("RecordAgentHealthEvent: %v", err)
	}

	detail, err := coord.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if len(launcher.calls) != 0 {
		t.Fatalf("launcher calls = %d, want 0 (no same-provider fallback allowed)", len(launcher.calls))
	}
	if !hasPrefix(detail.NextAction, "waiting_for_capacity") {
		t.Fatalf("NextAction = %q, want waiting_for_capacity prefix", detail.NextAction)
	}

	// Confirm this NEVER converts to human_required — the question must stay
	// resolving and be retried on a later pass.
	qs, err := store.ListWorkflowQuestionsByRun(ctx, run.ID)
	if err != nil || len(qs) != 1 {
		t.Fatalf("ListWorkflowQuestionsByRun: qs=%v err=%v", qs, err)
	}
	if qs[0].State != domain.QuestionStateResolving {
		t.Fatalf("question state = %v, want still resolving", qs[0].State)
	}
}

func TestDecisionProviderSelect_PreferredUnavailableSameProviderAllowedFallsBack(t *testing.T) {
	launcher := &fakeDecisionResolverLauncher{}
	coord, store, _, _, clock := newDecisionResolverFixture(t, "", launcher)
	ctx := context.Background()

	run, stepID := seedRunWithPolicy(t, ctx, store, `{"version":"v1","maxFixCycles":3,"allowSameProviderResolver":true}`)
	seedResolvingQuestion(t, ctx, store, run.ID, stepID, domain.HarnessClaudeCode)

	if _, err := store.RecordAgentHealthEvent(ctx, domain.AgentHealthEvent{
		ID: "ahe-1", Harness: domain.HarnessCodex, State: domain.AgentHealthUnavailable, Reason: "test", CreatedAt: clock.Now(),
	}); err != nil {
		t.Fatalf("RecordAgentHealthEvent: %v", err)
	}

	if _, err := coord.GetRun(ctx, run.ID); err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if len(launcher.calls) != 1 {
		t.Fatalf("launcher calls = %d, want 1 (same-provider fallback)", len(launcher.calls))
	}
	if launcher.calls[0].Harness != domain.HarnessClaudeCode {
		t.Fatalf("resolver harness = %q, want claude-code (same as asking provider)", launcher.calls[0].Harness)
	}
}

// --- Real end-to-end backend flow ---

// TestDecisionResolver_EndToEnd_DetectDispatchCallbackDeliver is Checkpoint
// 8K-B pass 2's required real end-to-end backend flow test: an unambiguous
// technical question is captured by the real Detect path from a controlled
// pane-text fixture, classified auto_resolvable, dispatched to a (fake
// runtime, real everything else) resolver launcher whose Launch call issues
// a REAL HTTP POST to a REAL httpd router + *questions.ResolverAnswerService
// + the SAME real SQLite store (simulating the `ao decision resolve` CLI
// hitting the real daemon), then observed and delivered on the next GetRun.
func TestDecisionResolver_EndToEnd_DetectDispatchCallbackDeliver(t *testing.T) {
	ctx := context.Background()
	launcher := &fakeDecisionResolverLauncher{}
	coord, store, sessionFacts, sender, _ := newDecisionResolverFixture(t, autoResolvableDiscoveryPaneText(), launcher)
	runID, _, askingSessionID := seedRunningWorkStep(t, ctx, coord, store, sessionFacts, domain.ActivityWaitingInput)

	// Real HTTP server backed by the real router/service/store, exactly what
	// a live daemon would expose for `ao decision resolve` to call.
	svc := &questions.ResolverAnswerService{Store: store}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(httpd.NewRouterWithControl(config.Config{}, log, nil, httpd.APIDeps{Decisions: svc}, httpd.ControlDeps{}))
	t.Cleanup(srv.Close)

	// onLaunch simulates the resolver harness itself: it was NEVER told the
	// answer by AO (it only received the prompt/context pack via req.Prompt),
	// so this callback stands in for the resolver's own independent
	// evidence-based conclusion, submitted via the real HTTP contract.
	launcher.onLaunch = func(req workflowcore.DecisionResolverLaunchRequest) {
		if req.Prompt == "" {
			t.Fatalf("resolver was launched with no context pack/prompt")
		}
		payload := map[string]any{
			"runId":              req.ResolutionID,
			"answer":             "httputil.Retry",
			"reasonSummary":      "only one retry helper exists in the repo",
			"evidenceReferences": []string{"pkg/httputil/retry.go"},
			"certainty":          "actual",
		}
		body, _ := json.Marshal(payload)
		resp, err := http.Post(srv.URL+"/api/v1/sessions/"+string(req.ResolverSessionID)+"/decisions/resolve", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("resolver callback HTTP POST: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("resolver callback status = %d body=%s", resp.StatusCode, b)
		}
	}

	// Pass 1: Detect captures+classifies the question as auto_resolvable and
	// dispatches the resolver (launcher.Launch fires the real HTTP call
	// above synchronously).
	if _, err := coord.GetRun(ctx, runID); err != nil {
		t.Fatalf("GetRun (detect+dispatch): %v", err)
	}
	qs, err := store.ListWorkflowQuestionsByRun(ctx, runID)
	if err != nil || len(qs) != 1 {
		t.Fatalf("ListWorkflowQuestionsByRun: qs=%v err=%v", qs, err)
	}
	q := qs[0]
	if q.Classification != domain.QuestionClassificationAutoResolvable {
		t.Fatalf("classification = %v, want auto_resolvable", q.Classification)
	}
	if len(launcher.calls) != 1 {
		t.Fatalf("launcher calls = %d, want exactly 1", len(launcher.calls))
	}

	// Pass 2: observeResolutionStep sees the real completed resolution row
	// (written by the real HTTP call above) and delivers the answer.
	detail, err := coord.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun (observe+deliver): %v", err)
	}
	_ = detail
	qs2, err := store.ListWorkflowQuestionsByRun(ctx, runID)
	if err != nil || len(qs2) != 1 {
		t.Fatalf("ListWorkflowQuestionsByRun (after observe): qs=%v err=%v", qs2, err)
	}
	q2 := qs2[0]
	if q2.State != domain.QuestionStateAnswered {
		t.Fatalf("question state = %v, want answered", q2.State)
	}
	if q2.AnswerSource == nil || *q2.AnswerSource != domain.AnswerSourceResolver {
		t.Fatalf("answer source = %v, want resolver", q2.AnswerSource)
	}
	if q2.AnswerText != "httputil.Retry" {
		t.Fatalf("answer text = %q, want httputil.Retry", q2.AnswerText)
	}
	if !q2.Delivered {
		t.Fatalf("expected the resolver answer to be delivered")
	}
	if sender.calls != 1 {
		t.Fatalf("Send calls = %d, want exactly 1", sender.calls)
	}
	if sender.lastID != askingSessionID {
		t.Fatalf("delivered to %v, want the asking session %v", sender.lastID, askingSessionID)
	}

	// Restart-recovery: a third GetRun call must not double-launch or
	// double-deliver.
	if _, err := coord.GetRun(ctx, runID); err != nil {
		t.Fatalf("GetRun (3rd, restart-recovery check): %v", err)
	}
	if len(launcher.calls) != 1 {
		t.Fatalf("launcher calls after 3rd GetRun = %d, want still 1 (no double-launch)", len(launcher.calls))
	}
	if sender.calls != 1 {
		t.Fatalf("Send calls after 3rd GetRun = %d, want still 1 (no double-delivery)", sender.calls)
	}
}

// TestDecisionResolver_RestartRecoveryFreshCoordinatorDoesNotDoubleLaunch
// simulates a daemon restart between dispatch and callback: a brand new
// Coordinator instance (same store, same launcher, no in-memory state
// carried over) must not launch a second resolver session for the same
// question — the partial unique index on
// workflow_question_resolutions(workflow_question_id) WHERE status='running'
// plus the question's resolving_run_id pointer are what back this, not
// anything held in process memory.
func TestDecisionResolver_RestartRecoveryFreshCoordinatorDoesNotDoubleLaunch(t *testing.T) {
	ctx := context.Background()
	launcher := &fakeDecisionResolverLauncher{}
	coord, store, sessionFacts, sender, _ := newDecisionResolverFixture(t, autoResolvableDiscoveryPaneText(), launcher)
	runID, _, _ := seedRunningWorkStep(t, ctx, coord, store, sessionFacts, domain.ActivityWaitingInput)

	if _, err := coord.GetRun(ctx, runID); err != nil {
		t.Fatalf("GetRun (dispatch): %v", err)
	}
	if len(launcher.calls) != 1 {
		t.Fatalf("launcher calls = %d, want 1", len(launcher.calls))
	}

	// "Restart": a fresh Coordinator, wired against the exact same store and
	// launcher, standing in for a new daemon process.
	freshCoord := workflowcore.New(workflowcore.Deps{
		Store:                    store,
		Projects:                 store,
		SessionFacts:             sessionFacts,
		QuestionsStore:           store,
		PaneReader:               &fakePaneReader{text: autoResolvableDiscoveryPaneText()},
		MessageSender:            sender,
		DecisionResolverLauncher: launcher,
	})
	if err := freshCoord.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile (post-restart): %v", err)
	}
	if _, err := freshCoord.GetRun(ctx, runID); err != nil {
		t.Fatalf("GetRun (post-restart): %v", err)
	}
	if len(launcher.calls) != 1 {
		t.Fatalf("launcher calls after restart = %d, want still 1 (no double-launch)", len(launcher.calls))
	}
}

// TestDecisionResolver_NeverAnsweredWithoutResolverResponseGoesHumanRequired
// confirms staleness forces human_required and never invents an answer.
func TestDecisionResolver_NeverAnsweredWithoutResolverResponseGoesHumanRequired(t *testing.T) {
	ctx := context.Background()
	launcher := &fakeDecisionResolverLauncher{} // no onLaunch: resolver "never calls back"
	coord, store, sessionFacts, sender, clock := newDecisionResolverFixture(t, autoResolvableDiscoveryPaneText(), launcher)
	runID, _, _ := seedRunningWorkStep(t, ctx, coord, store, sessionFacts, domain.ActivityWaitingInput)

	if _, err := coord.GetRun(ctx, runID); err != nil {
		t.Fatalf("GetRun (dispatch): %v", err)
	}
	if len(launcher.calls) != 1 {
		t.Fatalf("launcher calls = %d, want 1", len(launcher.calls))
	}

	// Advance well past the staleness threshold with no callback ever
	// arriving.
	clock.Advance(31 * time.Minute)

	if _, err := coord.GetRun(ctx, runID); err != nil {
		t.Fatalf("GetRun (staleness sweep): %v", err)
	}
	qs, err := store.ListWorkflowQuestionsByRun(ctx, runID)
	if err != nil || len(qs) != 1 {
		t.Fatalf("ListWorkflowQuestionsByRun: qs=%v err=%v", qs, err)
	}
	q := qs[0]
	if q.State != domain.QuestionStateHumanRequired {
		t.Fatalf("question state = %v, want human_required after staleness", q.State)
	}
	if q.AnswerText != "" || q.AnswerSource != nil {
		t.Fatalf("question = %+v, want no invented answer field", q)
	}
	if sender.calls != 0 {
		t.Fatalf("Send calls = %d, want 0 (never deliver an invented answer)", sender.calls)
	}
}
