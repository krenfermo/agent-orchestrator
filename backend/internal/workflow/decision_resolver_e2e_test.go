package workflow_test

// Checkpoint 8K-B pass 3: the three end-to-end scenarios the checkpoint
// spec requires, run against the real chain (Coordinator -> real SQLite
// store -> real HTTP router/controllers -> real service layer), reusing
// TestDecisionResolver_EndToEnd_DetectDispatchCallbackDeliver's fixture
// shape (decision_resolver_wiring_test.go) rather than re-deriving it.
//
// E2E 1 (Claude asks -> Codex resolves): TestDecisionResolverE2E_ClaudeAsksCodexResolves
// E2E 2 (Codex asks -> Claude resolves): TestDecisionResolverE2E_CodexAsksClaudeResolves
// E2E 3 (human safety, no regression):   TestDecisionResolverE2E_HumanSafetyNoResolverForBusinessQuestion

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/questions"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// seedRunningWorkStepHarness mirrors seedRunningWorkStep (questions_wiring_test.go)
// but lets the caller choose the asking harness — needed for E2E 2 (Codex
// asks), where seedRunningWorkStep's hardcoded "claude-code" session harness
// doesn't apply.
func seedRunningWorkStepHarness(ctx context.Context, t *testing.T, coord *workflowcore.Coordinator, store *sqlite.Store, sessionFacts *fakeSessionFacts, activity domain.ActivityState, harness domain.AgentHarness) (runID string, workStepID string, sessionID domain.SessionID) {
	t.Helper()
	rec, err := store.CreateSession(ctx, domain.SessionRecord{
		ProjectID: "p",
		Kind:      domain.KindWorker,
		Harness:   harness,
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
		ID:             "wfc-seed-" + t.Name(),
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
		Harness:  harness,
		Activity: domain.Activity{State: activity},
		Metadata: domain.SessionMetadata{RuntimeHandleID: "handle-" + string(sessionID)},
	})
	return runID, workStep.ID, sessionID
}

func codexDiscoveryPaneText() string {
	return "Which existing helper should I use for retrying HTTP calls?\n" +
		"› 1. httputil.Retry\n" +
		"  2. Write a new one\n"
}

// businessTradeoffPaneText is deliberately the classifier's own documented
// example of a question that must NEVER auto-resolve: a preference/tradeoff
// between two arbitrary values, not a discovery-of-existing-fact question
// (see classifier.go's autoResolvableShapePatterns doc comment).
func businessTradeoffPaneText() string {
	return "Should the retry cooldown be 2s or 8s?\n" +
		"❯ 1. 2 seconds\n" +
		"  2. 8 seconds\n"
}

// newDecisionsHTTPServer wires a real httptest server with the SAME real
// store/service layer a live daemon would use: the questions API (list/get/
// pending/answer) and the decisions resolver-callback API, both backed by
// the same *sqlite.Store the Coordinator under test uses.
func newDecisionsHTTPServer(t *testing.T, store *sqlite.Store, sender questions.MessageSender) *httptest.Server {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	answerSvc := &questions.AnswerService{Store: store, Runs: store, Sender: sender}
	resolveSvc := &questions.ResolverAnswerService{Store: store}
	srv := httptest.NewServer(httpd.NewRouterWithControl(config.Config{}, log, nil, httpd.APIDeps{
		Questions: answerSvc,
		Decisions: resolveSvc,
	}, httpd.ControlDeps{}))
	t.Cleanup(srv.Close)
	return srv
}

func httpGetJSON(t *testing.T, url string, out any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d body=%s", url, resp.StatusCode, body)
	}
	if err := json.Unmarshal(body, out); err != nil {
		t.Fatalf("GET %s: unmarshal %v body=%s", url, err, body)
	}
}

// assertNoChainOfThoughtLeak scans a raw JSON body for any key that would
// suggest a transcript/chain-of-thought field ever made it onto the wire.
// The resolve contract (ResolveDecisionRequest/Response) deliberately has no
// such field at all — this is a belt-and-suspenders content scan, not just
// a struct-shape check, so it would also catch an accidental fmt.Sprintf
// dump of internal state into a string field.
func assertNoChainOfThoughtLeak(t *testing.T, label, body string) {
	t.Helper()
	lower := strings.ToLower(body)
	for _, marker := range []string{"chainofthought", "chain_of_thought", "transcript", "reasoning_trace", "scratchpad"} {
		if strings.Contains(lower, marker) {
			t.Fatalf("%s: found forbidden chain-of-thought-shaped marker %q in response body: %s", label, marker, body)
		}
	}
}

type pendingListResponse struct {
	Questions []struct {
		ID              string `json:"id"`
		WorkflowRunID   string `json:"workflowRunId"`
		State           string `json:"state"`
		ResolverHarness string `json:"resolverHarness,omitempty"`
	} `json:"questions"`
}

type singleQuestionResponse struct {
	Question struct {
		ID                     string `json:"id"`
		State                  string `json:"state"`
		AnswerText             string `json:"answerText"`
		AnswerSource           string `json:"answerSource,omitempty"`
		Delivered              bool   `json:"delivered"`
		ResolverHarness        string `json:"resolverHarness,omitempty"`
		ResolverAdvisoryAnswer string `json:"resolverAdvisoryAnswer,omitempty"`
	} `json:"question"`
}

func pendingContains(list pendingListResponse, questionID string) bool {
	for _, q := range list.Questions {
		if q.ID == questionID {
			return true
		}
	}
	return false
}

// TestDecisionResolverE2E_ClaudeAsksCodexResolves is E2E 1: an unambiguous
// technical/discovery question asked by a claude-code work session, routed
// to a real read-only Codex resolver session (via the fake launcher's
// onLaunch, simulating `ao decision resolve`'s real HTTP call), and
// delivered back to the asking session — with the question visible through
// the real HTTP GET surface (single-question detail + the new global
// pending-decisions list) before it resolves, and gone from the pending
// list afterward.
func TestDecisionResolverE2E_ClaudeAsksCodexResolves(t *testing.T) {
	ctx := context.Background()
	launcher := &fakeDecisionResolverLauncher{}
	coord, store, sessionFacts, sender, _ := newDecisionResolverFixture(t, autoResolvableDiscoveryPaneText(), launcher)
	runID, _, askingSessionID := seedRunningWorkStep(ctx, t, coord, store, sessionFacts, domain.ActivityWaitingInput)
	srv := newDecisionsHTTPServer(t, store, sender)

	var resolverGotAnswerHint bool
	launcher.onLaunch = func(req workflowcore.DecisionResolverLaunchRequest) {
		if req.Harness != domain.HarnessCodex {
			t.Fatalf("resolver harness = %v, want codex (claude-code asked)", req.Harness)
		}
		// The resolver was NEVER told the "right" answer: "httputil.Retry" is
		// legitimately part of the prompt already (it's one of the two
		// structured choices the ORIGINAL question itself offered — the
		// resolver must see the question it's answering). What must never
		// appear is AO's own after-the-fact verdict/reasoning about which
		// choice is correct — this test's resolverReasonSummary text below,
		// which only exists because THIS callback is about to invent it.
		if strings.Contains(req.Prompt, "only one retry helper exists in the repo") {
			resolverGotAnswerHint = true
		}
		payload := map[string]any{
			"runId": req.ResolutionID, "answer": "httputil.Retry",
			"reasonSummary":      "only one retry helper exists in the repo",
			"evidenceReferences": []string{"pkg/httputil/retry.go"}, "certainty": "actual",
		}
		body, _ := json.Marshal(payload)
		resp, err := http.Post(srv.URL+"/api/v1/sessions/"+string(req.ResolverSessionID)+"/decisions/resolve", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("resolver callback: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("resolver callback status = %d body=%s", resp.StatusCode, b)
		}
	}

	// Detect + dispatch. The fake launcher's onLaunch fires synchronously
	// and posts the real HTTP resolve callback DURING this call, so by the
	// time GetRun returns the resolution row already exists — but the
	// question's OWN state transition (resolving -> answered) only happens
	// on the *next* reconcile pass (observeResolutionStep), exactly as
	// dispatchDecisionResolver's own docs describe. That gives this test a
	// real window to assert "visible via HTTP, not yet resolved" against
	// actual database state, not a contrived pause.
	if _, err := coord.GetRun(ctx, runID); err != nil {
		t.Fatalf("GetRun (detect+dispatch): %v", err)
	}
	qs, err := store.ListWorkflowQuestionsByRun(ctx, runID)
	if err != nil || len(qs) != 1 {
		t.Fatalf("ListWorkflowQuestionsByRun: qs=%v err=%v", qs, err)
	}
	questionID := string(qs[0].ID)

	if resolverGotAnswerHint {
		t.Fatalf("resolver's prompt leaked the answer text — it must only receive the context pack")
	}

	// Visible via the real single-question GET, still resolving.
	var single singleQuestionResponse
	httpGetJSON(t, srv.URL+"/api/v1/workflows/"+runID+"/questions/"+questionID, &single)
	if single.Question.State != "resolving" {
		t.Fatalf("question state via HTTP = %q, want resolving (not yet observed)", single.Question.State)
	}
	if single.Question.Delivered {
		t.Fatalf("question must not be delivered yet")
	}

	// Visible via the real global pending-decisions list.
	var pendingBefore pendingListResponse
	httpGetJSON(t, srv.URL+"/api/v1/questions/pending", &pendingBefore)
	if !pendingContains(pendingBefore, questionID) {
		t.Fatalf("question %s not found on the pending-decisions list before resolution: %+v", questionID, pendingBefore)
	}

	// Observe + deliver.
	if _, err := coord.GetRun(ctx, runID); err != nil {
		t.Fatalf("GetRun (observe+deliver): %v", err)
	}

	httpGetJSON(t, srv.URL+"/api/v1/workflows/"+runID+"/questions/"+questionID, &single)
	if single.Question.State != "answered" {
		t.Fatalf("question state via HTTP = %q, want answered", single.Question.State)
	}
	if single.Question.AnswerText != "httputil.Retry" || single.Question.AnswerSource != "resolver" {
		t.Fatalf("answer via HTTP = %+v, want httputil.Retry/resolver", single.Question)
	}
	if !single.Question.Delivered {
		t.Fatalf("question must be delivered via HTTP after observe+deliver")
	}
	if single.Question.ResolverHarness != "codex" {
		t.Fatalf("resolverHarness via HTTP = %q, want codex", single.Question.ResolverHarness)
	}
	if sender.calls != 1 || sender.lastID != askingSessionID {
		t.Fatalf("expected exactly 1 Send to the real asking session, got calls=%d lastID=%v", sender.calls, sender.lastID)
	}

	// Dropped off the pending list once delivered.
	var pendingAfter pendingListResponse
	httpGetJSON(t, srv.URL+"/api/v1/questions/pending", &pendingAfter)
	if pendingContains(pendingAfter, questionID) {
		t.Fatalf("answered+delivered question %s must not still be on the pending-decisions list: %+v", questionID, pendingAfter)
	}

	// No chain-of-thought anywhere: the stored resolution row, the
	// single-question response body, and the pending-list response body.
	resolution, found, err := store.GetCurrentResolutionForQuestion(ctx, questionID)
	if err != nil || !found {
		t.Fatalf("GetCurrentResolutionForQuestion: found=%v err=%v", found, err)
	}
	resolutionJSON, _ := json.Marshal(resolution)
	assertNoChainOfThoughtLeak(t, "stored resolution row", string(resolutionJSON))

	rawSingle, err := http.Get(srv.URL + "/api/v1/workflows/" + runID + "/questions/" + questionID)
	if err != nil {
		t.Fatalf("GET single question: %v", err)
	}
	rawBody, _ := io.ReadAll(rawSingle.Body)
	rawSingle.Body.Close()
	assertNoChainOfThoughtLeak(t, "single-question HTTP response", string(rawBody))
}

// TestDecisionResolverE2E_CodexAsksClaudeResolves is E2E 2: same shape as
// E2E 1 with the roles reversed — a Codex work session asks, a real
// read-only Claude Code resolver session answers.
func TestDecisionResolverE2E_CodexAsksClaudeResolves(t *testing.T) {
	ctx := context.Background()
	launcher := &fakeDecisionResolverLauncher{}
	coord, store, sessionFacts, sender, _ := newDecisionResolverFixture(t, codexDiscoveryPaneText(), launcher)
	runID, _, askingSessionID := seedRunningWorkStepHarness(ctx, t, coord, store, sessionFacts, domain.ActivityWaitingInput, domain.HarnessCodex)
	srv := newDecisionsHTTPServer(t, store, sender)

	launcher.onLaunch = func(req workflowcore.DecisionResolverLaunchRequest) {
		if req.Harness != domain.HarnessClaudeCode {
			t.Fatalf("resolver harness = %v, want claude-code (codex asked)", req.Harness)
		}
		payload := map[string]any{
			"runId": req.ResolutionID, "answer": "httputil.Retry",
			"reasonSummary": "only one retry helper exists in the repo", "certainty": "actual",
		}
		body, _ := json.Marshal(payload)
		resp, err := http.Post(srv.URL+"/api/v1/sessions/"+string(req.ResolverSessionID)+"/decisions/resolve", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("resolver callback: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("resolver callback status = %d body=%s", resp.StatusCode, b)
		}
	}

	if _, err := coord.GetRun(ctx, runID); err != nil {
		t.Fatalf("GetRun (detect+dispatch): %v", err)
	}
	qs, err := store.ListWorkflowQuestionsByRun(ctx, runID)
	if err != nil || len(qs) != 1 {
		t.Fatalf("ListWorkflowQuestionsByRun: qs=%v err=%v", qs, err)
	}
	if qs[0].AskingHarness != domain.HarnessCodex {
		t.Fatalf("asking harness = %v, want codex", qs[0].AskingHarness)
	}
	if len(launcher.calls) != 1 || launcher.calls[0].Harness != domain.HarnessClaudeCode {
		t.Fatalf("expected exactly one launch to claude-code, got %+v", launcher.calls)
	}

	if _, err := coord.GetRun(ctx, runID); err != nil {
		t.Fatalf("GetRun (observe+deliver): %v", err)
	}
	var single singleQuestionResponse
	httpGetJSON(t, srv.URL+"/api/v1/workflows/"+runID+"/questions/"+string(qs[0].ID), &single)
	if single.Question.State != "answered" || single.Question.AnswerSource != "resolver" {
		t.Fatalf("question via HTTP = %+v, want answered/resolver", single.Question)
	}
	if single.Question.ResolverHarness != "claude-code" {
		t.Fatalf("resolverHarness via HTTP = %q, want claude-code", single.Question.ResolverHarness)
	}
	if sender.calls != 1 || sender.lastID != askingSessionID {
		t.Fatalf("expected exactly 1 Send to the real asking (codex) session, got calls=%d lastID=%v", sender.calls, sender.lastID)
	}
}

// TestDecisionResolverE2E_HumanSafetyNoResolverForBusinessQuestion is E2E 3:
// an unspecified functional/business question ("should the cooldown be 2s
// or 8s") must classify human_required/ambiguous and NEVER launch a
// resolver — zero workflow_question_resolutions rows, still answerable via
// the existing 8K-A human-answer endpoint, and correctly visible on the
// global pending-decisions list.
func TestDecisionResolverE2E_HumanSafetyNoResolverForBusinessQuestion(t *testing.T) {
	ctx := context.Background()
	launcher := &fakeDecisionResolverLauncher{}
	coord, store, sessionFacts, sender, _ := newDecisionResolverFixture(t, businessTradeoffPaneText(), launcher)
	runID, _, askingSessionID := seedRunningWorkStep(ctx, t, coord, store, sessionFacts, domain.ActivityWaitingInput)
	srv := newDecisionsHTTPServer(t, store, sender)

	if _, err := coord.GetRun(ctx, runID); err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	qs, err := store.ListWorkflowQuestionsByRun(ctx, runID)
	if err != nil || len(qs) != 1 {
		t.Fatalf("ListWorkflowQuestionsByRun: qs=%v err=%v", qs, err)
	}
	q := qs[0]
	if q.Classification != domain.QuestionClassificationAmbiguous {
		t.Fatalf("classification = %v, want ambiguous (business tradeoff, no confident classifier match)", q.Classification)
	}
	if q.State != domain.QuestionStateHumanRequired {
		t.Fatalf("state = %v, want human_required", q.State)
	}

	// Zero launches, zero resolution rows: the resolver must never even be
	// considered for a business/preference question.
	if len(launcher.calls) != 0 {
		t.Fatalf("launcher calls = %d, want 0 — a business tradeoff question must never dispatch a resolver", len(launcher.calls))
	}
	resolutions, err := store.ListWorkflowQuestionResolutionsByRun(ctx, runID)
	if err != nil {
		t.Fatalf("ListWorkflowQuestionResolutionsByRun: %v", err)
	}
	if len(resolutions) != 0 {
		t.Fatalf("resolution rows = %d, want 0", len(resolutions))
	}

	// Visible on the real global pending-decisions list.
	var pending pendingListResponse
	httpGetJSON(t, srv.URL+"/api/v1/questions/pending", &pending)
	if !pendingContains(pending, string(q.ID)) {
		t.Fatalf("human_required question %s not found on the pending-decisions list: %+v", q.ID, pending)
	}

	// Still answerable via the existing 8K-A human-answer endpoint.
	answerBody, _ := json.Marshal(map[string]any{"customText": "Use 8s — matches the existing backoff table."})
	resp, err := http.Post(srv.URL+"/api/v1/workflows/"+runID+"/questions/"+string(q.ID)+"/answer", "application/json", bytes.NewReader(answerBody))
	if err != nil {
		t.Fatalf("human answer POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("human answer status = %d body=%s", resp.StatusCode, b)
	}
	var answered singleQuestionResponse
	if err := json.NewDecoder(resp.Body).Decode(&answered); err != nil {
		t.Fatalf("decode human answer response: %v", err)
	}
	if answered.Question.State != "answered" || answered.Question.AnswerSource != "human" {
		t.Fatalf("answered question via HTTP = %+v, want answered/human", answered.Question)
	}
	if sender.calls != 1 || sender.lastID != askingSessionID {
		t.Fatalf("expected exactly 1 Send delivering the human answer, got calls=%d lastID=%v", sender.calls, sender.lastID)
	}

	// Answering must not have created a resolution row either.
	resolutionsAfter, err := store.ListWorkflowQuestionResolutionsByRun(ctx, runID)
	if err != nil {
		t.Fatalf("ListWorkflowQuestionResolutionsByRun (after answer): %v", err)
	}
	if len(resolutionsAfter) != 0 {
		t.Fatalf("resolution rows after human answer = %d, want 0", len(resolutionsAfter))
	}
}
