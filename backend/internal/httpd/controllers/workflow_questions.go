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
	// ListPending backs Checkpoint 8K-B pass 3's global "Pending Decisions"
	// inbox: every open/in-flight question across ALL runs, optionally
	// filtered to the given states.
	ListPending(ctx context.Context, states []string) ([]domain.WorkflowQuestion, error)
	// GetResolution returns the current Decision Resolver attempt for a
	// question, if any (pass 3's resolver-field enrichment).
	GetResolution(ctx context.Context, questionID string) (domain.WorkflowQuestionResolution, bool, error)
	// ListResolutionsByRun backs pass 3's Decisions telemetry section.
	ListResolutionsByRun(ctx context.Context, runID string) ([]domain.WorkflowQuestionResolution, error)
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
	r.Get("/questions/pending", c.pending)
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
	envelope.WriteJSON(w, http.StatusOK, ListWorkflowQuestionsResponse{Questions: enrichedQuestionResponses(r.Context(), c.Svc, qs)})
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
	envelope.WriteJSON(w, http.StatusOK, WorkflowQuestionResponseBody{Question: enrichedQuestionResponse(r.Context(), c.Svc, q)})
}

// pendingDecisionsAllowedStates is Checkpoint 8K-B pass 3's fixed filter
// vocabulary for GET /questions/pending's ?state= query param. Mirrors the
// three states surfaced on the global inbox: human_required and resolving
// are real persisted QuestionState values; waiting_for_capacity is not a
// persisted state (a resolving question with no dispatched resolution
// attempt yet — see ResolvingRunID) but is accepted here as an alias for
// resolving so a caller filtering the inbox down to "stuck on capacity"
// doesn't need to know that implementation detail.
var pendingDecisionsAllowedStates = map[string]string{
	"human_required":       "human_required",
	"resolving":            "resolving",
	"waiting_for_capacity": "resolving",
}

func (c *WorkflowQuestionsController) pending(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/questions/pending")
		return
	}
	var states []string
	seen := map[string]bool{}
	for _, raw := range r.URL.Query()["state"] {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		mapped, ok := pendingDecisionsAllowedStates[raw]
		if !ok {
			envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_STATE_FILTER",
				"state must be one of human_required, resolving, waiting_for_capacity", nil)
			return
		}
		if !seen[mapped] {
			seen[mapped] = true
			states = append(states, mapped)
		}
	}
	qs, err := c.Svc.ListPending(r.Context(), states)
	if err != nil {
		writeWorkflowQuestionError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, ListWorkflowQuestionsResponse{Questions: enrichedQuestionResponses(r.Context(), c.Svc, qs)})
}

// resolutionGetter is the narrow read contract enrichedQuestionResponses
// needs — satisfied by WorkflowQuestionsService (both
// WorkflowQuestionsController.Svc and WorkflowsController.QuestionsReader
// share this exact interface, so both call sites reuse the same enrichment
// helper rather than duplicating it).
type resolutionGetter interface {
	GetResolution(ctx context.Context, questionID string) (domain.WorkflowQuestionResolution, bool, error)
}

// enrichedQuestionResponses/enrichedQuestionResponse attach resolver fields
// to each question that could plausibly have a resolution row (see
// resolverEnrichmentEligible), fetched via GetResolution. Best-effort: a
// lookup error leaves that one question's resolver fields unset rather than
// failing the whole request — the base question data is still correct and
// more valuable than a 500.
func enrichedQuestionResponses(ctx context.Context, svc resolutionGetter, qs []domain.WorkflowQuestion) []WorkflowQuestionResponse {
	out := make([]WorkflowQuestionResponse, 0, len(qs))
	for _, q := range qs {
		out = append(out, enrichedQuestionResponse(ctx, svc, q))
	}
	return out
}

func enrichedQuestionResponse(ctx context.Context, svc resolutionGetter, q domain.WorkflowQuestion) WorkflowQuestionResponse {
	out := workflowQuestionResponse(q)
	if svc == nil || !resolverEnrichmentEligible(q) {
		return out
	}
	resolution, found, err := svc.GetResolution(ctx, string(q.ID))
	if err != nil {
		return out
	}
	return applyResolverEnrichment(out, resolution, found)
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
