package controllers

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/questions"
)

// WorkflowQuestionIDParam is the {questionId} path parameter for
// single-question routes, used alongside {workflowId}.
type WorkflowQuestionIDParam struct {
	WorkflowID string `path:"workflowId" description:"Workflow run identifier."`
	QuestionID string `path:"questionId" description:"Workflow question identifier."`
}

// WorkflowQuestionsService is the controller-facing contract for
// Checkpoint 8K-A's durable question API. Satisfied by
// *questions.AnswerService.
type WorkflowQuestionsService interface {
	ListByRun(ctx context.Context, runID string) ([]domain.WorkflowQuestion, error)
	Get(ctx context.Context, runID, questionID string) (domain.WorkflowQuestion, error)
	Answer(ctx context.Context, runID, questionID string, choiceID, customText *string) (domain.WorkflowQuestion, error)
}

// WorkflowQuestionsController owns the /workflows/{workflowId}/questions
// routes. A nil Svc answers 501, matching every other optional surface in
// this package.
type WorkflowQuestionsController struct {
	Svc WorkflowQuestionsService
}

// Register mounts the question routes on the supplied router.
func (c *WorkflowQuestionsController) Register(r chi.Router) {
	r.Get("/workflows/{workflowId}/questions", c.list)
	r.Get("/workflows/{workflowId}/questions/{questionId}", c.get)
	r.Post("/workflows/{workflowId}/questions/{questionId}/answer", c.answer)
}

func (c *WorkflowQuestionsController) list(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/workflows/{workflowId}/questions")
		return
	}
	runID := strings.TrimSpace(chi.URLParam(r, "workflowId"))
	qs, err := c.Svc.ListByRun(r.Context(), runID)
	if err != nil {
		writeWorkflowQuestionError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, ListWorkflowQuestionsResponse{Questions: workflowQuestionResponses(qs)})
}

func (c *WorkflowQuestionsController) get(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/workflows/{workflowId}/questions/{questionId}")
		return
	}
	runID := strings.TrimSpace(chi.URLParam(r, "workflowId"))
	questionID := strings.TrimSpace(chi.URLParam(r, "questionId"))
	q, err := c.Svc.Get(r.Context(), runID, questionID)
	if err != nil {
		writeWorkflowQuestionError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, WorkflowQuestionResponseBody{Question: workflowQuestionResponse(q)})
}

// AnswerWorkflowQuestionRequest is the body of
// POST /api/v1/workflows/{workflowId}/questions/{questionId}/answer.
// Exactly one of ChoiceID/CustomText must be set.
type AnswerWorkflowQuestionRequest struct {
	ChoiceID   *string `json:"choiceId,omitempty" description:"One of the question's structured choice ids."`
	CustomText *string `json:"customText,omitempty" description:"Free-text human answer, when no structured choice applies."`
}

func (c *WorkflowQuestionsController) answer(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "POST", "/api/v1/workflows/{workflowId}/questions/{questionId}/answer")
		return
	}
	runID := strings.TrimSpace(chi.URLParam(r, "workflowId"))
	questionID := strings.TrimSpace(chi.URLParam(r, "questionId"))
	var in AnswerWorkflowQuestionRequest
	if err := decodeJSON(r, &in); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid JSON body", nil)
		return
	}
	q, err := c.Svc.Answer(r.Context(), runID, questionID, in.ChoiceID, in.CustomText)
	if err != nil {
		writeWorkflowQuestionError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, WorkflowQuestionResponseBody{Question: workflowQuestionResponse(q)})
}

func writeWorkflowQuestionError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, questions.ErrNotFound):
		envelope.WriteAPIError(w, r, http.StatusNotFound, "not_found", "WORKFLOW_QUESTION_NOT_FOUND", err.Error(), nil)
	case errors.Is(err, questions.ErrWrongRun):
		envelope.WriteAPIError(w, r, http.StatusNotFound, "not_found", "WORKFLOW_QUESTION_WRONG_RUN", err.Error(), nil)
	case errors.Is(err, questions.ErrNotAnswerable):
		envelope.WriteAPIError(w, r, http.StatusConflict, "conflict", "WORKFLOW_QUESTION_NOT_ANSWERABLE", err.Error(), nil)
	case errors.Is(err, questions.ErrRunCancelled):
		envelope.WriteAPIError(w, r, http.StatusConflict, "conflict", "WORKFLOW_RUN_CANCELLED", err.Error(), nil)
	case errors.Is(err, questions.ErrInvalidChoice):
		envelope.WriteAPIError(w, r, http.StatusUnprocessableEntity, "unprocessable", "WORKFLOW_QUESTION_INVALID_CHOICE", err.Error(), nil)
	case errors.Is(err, questions.ErrAmbiguousAnswer):
		envelope.WriteAPIError(w, r, http.StatusUnprocessableEntity, "unprocessable", "WORKFLOW_QUESTION_AMBIGUOUS_ANSWER", err.Error(), nil)
	default:
		envelope.WriteAPIError(w, r, http.StatusInternalServerError, "internal", "WORKFLOW_QUESTION_OPERATION_FAILED", "Workflow question operation failed", nil)
	}
}
