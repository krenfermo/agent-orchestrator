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

// DecisionsService is the controller-facing contract for Checkpoint 8K-B's
// resolver callback. Satisfied by *questions.ResolverAnswerService.
type DecisionsService interface {
	Resolve(ctx context.Context, pathSessionID string, in questions.ResolveInput) (domain.WorkflowQuestionResolution, error)
}

// DecisionsController owns the session-scoped /decisions routes. A nil Svc
// returns 501, matching every other optional surface in this package.
type DecisionsController struct {
	Svc DecisionsService
}

// Register mounts the decision routes on the supplied router.
func (c *DecisionsController) Register(r chi.Router) {
	r.Post("/sessions/{resolverSessionId}/decisions/resolve", c.resolve)
}

func (c *DecisionsController) resolve(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "POST", "/api/v1/sessions/{resolverSessionId}/decisions/resolve")
		return
	}
	resolverSessionID := strings.TrimSpace(chi.URLParam(r, "resolverSessionId"))
	if resolverSessionID == "" {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "DECISION_RESOLVER_SESSION_REQUIRED", "Resolver session id is required", nil)
		return
	}
	var in ResolveDecisionRequest
	if err := decodeJSON(r, &in); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid JSON body", nil)
		return
	}
	resolution, err := c.Svc.Resolve(r.Context(), resolverSessionID, questions.ResolveInput{
		RunID:              in.RunID,
		Answer:             in.Answer,
		ReasonSummary:      in.ReasonSummary,
		EvidenceReferences: in.EvidenceReferences,
		Certainty:          domain.QuestionCertainty(in.Certainty),
		RequiresHuman:      in.RequiresHuman,
	})
	if err != nil {
		writeDecisionResolveError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, ResolveDecisionResponse{Resolution: workflowQuestionResolutionResponse(resolution)})
}

func writeDecisionResolveError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, questions.ErrResolutionNotFound):
		envelope.WriteAPIError(w, r, http.StatusNotFound, "not_found", "DECISION_RESOLUTION_NOT_FOUND", err.Error(), nil)
	case errors.Is(err, questions.ErrResolutionWrongSession):
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "DECISION_RESOLUTION_WRONG_SESSION", err.Error(), nil)
	case errors.Is(err, questions.ErrResolutionConflict):
		envelope.WriteAPIError(w, r, http.StatusConflict, "conflict", "DECISION_RESOLUTION_CONFLICT", err.Error(), nil)
	case errors.Is(err, questions.ErrResolutionNotRunning):
		envelope.WriteAPIError(w, r, http.StatusConflict, "conflict", "DECISION_RESOLUTION_NOT_RUNNING", err.Error(), nil)
	case errors.Is(err, questions.ErrResolutionInvalid):
		envelope.WriteAPIError(w, r, http.StatusUnprocessableEntity, "unprocessable", "DECISION_RESOLUTION_INVALID", err.Error(), nil)
	default:
		envelope.WriteAPIError(w, r, http.StatusInternalServerError, "internal", "DECISION_RESOLUTION_OPERATION_FAILED", "Decision resolve operation failed", nil)
	}
}
