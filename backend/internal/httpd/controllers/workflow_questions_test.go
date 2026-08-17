package controllers_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/questions"
)

// fakeQuestionsService is a hand-rolled fake for
// controllers.WorkflowQuestionsService (no real store/session_manager in
// these controller-level tests — the service layer itself is exercised for
// real in internal/service/questions and internal/workflow).
type fakeQuestionsService struct {
	byRun map[string][]domain.WorkflowQuestion
	err   error

	answerCalls    int
	lastAnswerRun  string
	lastAnswerQ    string
	lastChoiceID   *string
	lastCustomText *string
	answerErr      error
	answerResult   domain.WorkflowQuestion

	resolution      domain.WorkflowQuestionResolution
	resolutionFound bool
}

func (f *fakeQuestionsService) ListByRun(_ context.Context, runID string) ([]domain.WorkflowQuestion, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.byRun[runID], nil
}

func (f *fakeQuestionsService) Get(_ context.Context, runID, questionID string) (domain.WorkflowQuestion, error) {
	if f.err != nil {
		return domain.WorkflowQuestion{}, f.err
	}
	for _, q := range f.byRun[runID] {
		if string(q.ID) == questionID {
			return q, nil
		}
	}
	return domain.WorkflowQuestion{}, questions.ErrNotFound
}

func (f *fakeQuestionsService) ListPending(_ context.Context, states []string) ([]domain.WorkflowQuestion, error) {
	if f.err != nil {
		return nil, f.err
	}
	allowed := map[string]bool{}
	for _, s := range states {
		allowed[s] = true
	}
	var out []domain.WorkflowQuestion
	for _, qs := range f.byRun {
		for _, q := range qs {
			if len(allowed) == 0 {
				if q.State == domain.QuestionStateHumanRequired || q.State == domain.QuestionStateResolving {
					out = append(out, q)
				}
				continue
			}
			if allowed[string(q.State)] {
				out = append(out, q)
			}
		}
	}
	return out, nil
}

func (f *fakeQuestionsService) GetResolution(_ context.Context, _ string) (domain.WorkflowQuestionResolution, bool, error) {
	return f.resolution, f.resolutionFound, nil
}

func (f *fakeQuestionsService) ListResolutionsByRun(_ context.Context, _ string) ([]domain.WorkflowQuestionResolution, error) {
	if !f.resolutionFound {
		return nil, nil
	}
	return []domain.WorkflowQuestionResolution{f.resolution}, nil
}

func (f *fakeQuestionsService) Answer(_ context.Context, runID, questionID string, choiceID, customText *string) (domain.WorkflowQuestion, error) {
	f.answerCalls++
	f.lastAnswerRun = runID
	f.lastAnswerQ = questionID
	f.lastChoiceID = choiceID
	f.lastCustomText = customText
	if f.answerErr != nil {
		return domain.WorkflowQuestion{}, f.answerErr
	}
	return f.answerResult, nil
}

func newQuestionsTestServer(t *testing.T, svc *fakeQuestionsService) *httptest.Server {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(httpd.NewRouterWithControl(config.Config{}, log, nil, httpd.APIDeps{Questions: svc}, httpd.ControlDeps{}))
	t.Cleanup(srv.Close)
	return srv
}

func TestWorkflowQuestions_ListHappyPath(t *testing.T) {
	svc := &fakeQuestionsService{byRun: map[string][]domain.WorkflowQuestion{
		"wf-1": {{ID: "q-1", WorkflowRunID: "wf-1", QuestionText: "Should I push to main?", State: domain.QuestionStateHumanRequired, Certainty: domain.QuestionCertaintyInferred, Classification: domain.QuestionClassificationPolicyResolvable}},
	}}
	srv := newQuestionsTestServer(t, svc)

	body, status, headers := doRequest(t, srv, "GET", "/api/v1/workflows/wf-1/questions", "")
	assertJSON(t, headers)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	var out struct {
		Questions []struct {
			ID string `json:"id"`
		} `json:"questions"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, body)
	}
	if len(out.Questions) != 1 || out.Questions[0].ID != "q-1" {
		t.Fatalf("unexpected questions: %+v", out.Questions)
	}
}

func TestWorkflowQuestions_PendingAcrossRuns(t *testing.T) {
	svc := &fakeQuestionsService{byRun: map[string][]domain.WorkflowQuestion{
		"wf-1": {
			{ID: "q-1", WorkflowRunID: "wf-1", State: domain.QuestionStateHumanRequired},
			{ID: "q-2", WorkflowRunID: "wf-1", State: domain.QuestionStateAnswered},
		},
		"wf-2": {
			{ID: "q-3", WorkflowRunID: "wf-2", State: domain.QuestionStateResolving},
		},
	}}
	srv := newQuestionsTestServer(t, svc)

	body, status, _ := doRequest(t, srv, "GET", "/api/v1/questions/pending", "")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	var out struct {
		Questions []struct {
			ID string `json:"id"`
		} `json:"questions"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, body)
	}
	if len(out.Questions) != 2 {
		t.Fatalf("expected 2 pending questions across runs (q-1 human_required, q-3 resolving), got %+v", out.Questions)
	}
}

func TestWorkflowQuestions_PendingInvalidStateFilter400(t *testing.T) {
	svc := &fakeQuestionsService{byRun: map[string][]domain.WorkflowQuestion{}}
	srv := newQuestionsTestServer(t, svc)

	body, status, _ := doRequest(t, srv, "GET", "/api/v1/questions/pending?state=not_a_real_state", "")
	if status != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400", status, body)
	}
}

func TestWorkflowQuestions_ResolverAdvisoryNeverConfusedWithAnswer(t *testing.T) {
	resolvingRunID := domain.WorkflowQuestionResolutionID("wqr-1")
	svc := &fakeQuestionsService{
		byRun: map[string][]domain.WorkflowQuestion{
			"wf-1": {{
				ID: "q-1", WorkflowRunID: "wf-1", State: domain.QuestionStateHumanRequired,
				Classification: domain.QuestionClassificationAutoResolvable, ResolvingRunID: &resolvingRunID,
			}},
		},
		resolution: domain.WorkflowQuestionResolution{
			ID: resolvingRunID, ResolverHarness: domain.HarnessCodex,
			RequiresHuman: true, Answer: "resolver's best guess, not a decision", ReasonSummary: "could not verify against tests",
		},
		resolutionFound: true,
	}
	srv := newQuestionsTestServer(t, svc)

	body, status, _ := doRequest(t, srv, "GET", "/api/v1/workflows/wf-1/questions/q-1", "")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	var out struct {
		Question struct {
			AnswerText             string `json:"answerText"`
			ResolverHarness        string `json:"resolverHarness"`
			ResolverAdvisoryAnswer string `json:"resolverAdvisoryAnswer"`
		} `json:"question"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, body)
	}
	if out.Question.AnswerText != "" {
		t.Fatalf("AnswerText must stay empty for a human_required question, got %q", out.Question.AnswerText)
	}
	if out.Question.ResolverAdvisoryAnswer != "resolver's best guess, not a decision" {
		t.Fatalf("expected resolver advisory answer surfaced separately, got %+v", out.Question)
	}
	if out.Question.ResolverHarness != "codex" {
		t.Fatalf("expected resolverHarness=codex, got %+v", out.Question)
	}
}

func TestWorkflowQuestions_GetWrongRun404(t *testing.T) {
	svc := &fakeQuestionsService{byRun: map[string][]domain.WorkflowQuestion{
		"wf-1": {{ID: "q-1", WorkflowRunID: "wf-1", State: domain.QuestionStateHumanRequired}},
	}}
	srv := newQuestionsTestServer(t, svc)

	body, status, _ := doRequest(t, srv, "GET", "/api/v1/workflows/wf-OTHER/questions/q-1", "")
	if status != http.StatusNotFound {
		t.Fatalf("status=%d body=%s, want 404", status, body)
	}
}

func TestWorkflowQuestions_AnswerHappyPath(t *testing.T) {
	svc := &fakeQuestionsService{
		byRun: map[string][]domain.WorkflowQuestion{
			"wf-1": {{ID: "q-1", WorkflowRunID: "wf-1", State: domain.QuestionStateHumanRequired}},
		},
		answerResult: domain.WorkflowQuestion{ID: "q-1", WorkflowRunID: "wf-1", State: domain.QuestionStateAnswered, AnswerText: "Use 8 seconds."},
	}
	srv := newQuestionsTestServer(t, svc)

	body, status, headers := doRequest(t, srv, "POST", "/api/v1/workflows/wf-1/questions/q-1/answer", `{"customText":"Use 8 seconds."}`)
	assertJSON(t, headers)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if svc.answerCalls != 1 {
		t.Fatalf("answerCalls=%d, want 1", svc.answerCalls)
	}
	if svc.lastCustomText == nil || *svc.lastCustomText != "Use 8 seconds." {
		t.Fatalf("lastCustomText=%v", svc.lastCustomText)
	}
	var out struct {
		Question struct {
			State      string `json:"state"`
			AnswerText string `json:"answerText"`
		} `json:"question"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, body)
	}
	if out.Question.State != "answered" || out.Question.AnswerText != "Use 8 seconds." {
		t.Fatalf("unexpected question in response: %+v", out.Question)
	}
}

func TestWorkflowQuestions_AnswerWrongState409(t *testing.T) {
	svc := &fakeQuestionsService{answerErr: questions.ErrNotAnswerable}
	srv := newQuestionsTestServer(t, svc)

	body, status, _ := doRequest(t, srv, "POST", "/api/v1/workflows/wf-1/questions/q-1/answer", `{"customText":"x"}`)
	if status != http.StatusConflict {
		t.Fatalf("status=%d body=%s, want 409", status, body)
	}
}

func TestWorkflowQuestions_AnswerInvalidChoice422(t *testing.T) {
	svc := &fakeQuestionsService{answerErr: questions.ErrInvalidChoice}
	srv := newQuestionsTestServer(t, svc)

	choice := "nonexistent"
	body, status, _ := doRequest(t, srv, "POST", "/api/v1/workflows/wf-1/questions/q-1/answer", `{"choiceId":"`+choice+`"}`)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s, want 422", status, body)
	}
}

func TestWorkflowQuestions_DoubleAnswerRejected(t *testing.T) {
	svc := &fakeQuestionsService{}
	srv := newQuestionsTestServer(t, svc)

	// First answer succeeds (default answerErr nil).
	_, status1, _ := doRequest(t, srv, "POST", "/api/v1/workflows/wf-1/questions/q-1/answer", `{"customText":"first"}`)
	if status1 != http.StatusOK {
		t.Fatalf("first answer status=%d", status1)
	}

	// Simulate the service now reporting the question is no longer
	// answerable (already answered) for a second attempt.
	svc.answerErr = questions.ErrNotAnswerable
	body2, status2, _ := doRequest(t, srv, "POST", "/api/v1/workflows/wf-1/questions/q-1/answer", `{"customText":"second"}`)
	if status2 != http.StatusConflict {
		t.Fatalf("second answer status=%d body=%s, want 409 (double-answer rejected)", status2, body2)
	}
	if svc.answerCalls != 2 {
		t.Fatalf("answerCalls=%d, want 2", svc.answerCalls)
	}
}

func TestWorkflowQuestions_AmbiguousBodyRejected(t *testing.T) {
	svc := &fakeQuestionsService{answerErr: questions.ErrAmbiguousAnswer}
	srv := newQuestionsTestServer(t, svc)

	body, status, _ := doRequest(t, srv, "POST", "/api/v1/workflows/wf-1/questions/q-1/answer", `{"choiceId":"a","customText":"b"}`)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s, want 422", status, body)
	}
}

func TestWorkflowQuestions_Headless501(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(httpd.NewRouterWithControl(config.Config{}, log, nil, httpd.APIDeps{}, httpd.ControlDeps{}))
	t.Cleanup(srv.Close)

	body, status, _ := doRequest(t, srv, "GET", "/api/v1/workflows/wf-1/questions", "")
	if status != http.StatusNotImplemented {
		t.Fatalf("status=%d body=%s, want 501", status, body)
	}
}
