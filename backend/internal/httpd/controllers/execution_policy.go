package controllers

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/identity"
	executionpolicysvc "github.com/aoagents/agent-orchestrator/backend/internal/service/executionpolicy"
)

// ExecutionPolicyView is the wire shape of a domain.UserExecutionPolicy.
type ExecutionPolicyView struct {
	AutonomousMode           bool     `json:"autonomousMode"`
	PlannerPriority          []string `json:"plannerPriority"`
	WorkerPriority           []string `json:"workerPriority"`
	ReviewerPriority         []string `json:"reviewerPriority"`
	DecisionResolverPriority []string `json:"decisionResolverPriority"`
	FallbackBehavior         string   `json:"fallbackBehavior" enum:"use_next_available,wait_for_preferred"`
	ReviewIndependence       string   `json:"reviewIndependence" enum:"require_different_provider,allow_same_provider_fallback"`
	UpdatedAt                string   `json:"updatedAt,omitempty"`
}

func executionPolicyView(p domain.UserExecutionPolicy) ExecutionPolicyView {
	view := ExecutionPolicyView{
		AutonomousMode:           p.AutonomousMode,
		PlannerPriority:          profileIDStrings(p.PlannerPriority),
		WorkerPriority:           profileIDStrings(p.WorkerPriority),
		ReviewerPriority:         profileIDStrings(p.ReviewerPriority),
		DecisionResolverPriority: profileIDStrings(p.DecisionResolverPriority),
		FallbackBehavior:         string(p.FallbackBehavior),
		ReviewIndependence:       string(p.ReviewIndependence),
	}
	if !p.UpdatedAt.IsZero() {
		view.UpdatedAt = p.UpdatedAt.UTC().Format(rfc3339Milli)
	}
	return view
}

func profileIDStrings(ids []domain.ProviderProfileID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, string(id))
	}
	return out
}

func profileIDsFromStrings(ss []string) []domain.ProviderProfileID {
	out := make([]domain.ProviderProfileID, 0, len(ss))
	for _, s := range ss {
		out = append(out, domain.ProviderProfileID(s))
	}
	return out
}

// ExecutionPolicyResponse wraps a single policy.
type ExecutionPolicyResponse struct {
	Policy ExecutionPolicyView `json:"policy"`
}

// PutExecutionPolicyRequest is the body of PUT /api/v1/execution-policy.
type PutExecutionPolicyRequest struct {
	AutonomousMode           bool     `json:"autonomousMode"`
	PlannerPriority          []string `json:"plannerPriority"`
	WorkerPriority           []string `json:"workerPriority"`
	ReviewerPriority         []string `json:"reviewerPriority"`
	DecisionResolverPriority []string `json:"decisionResolverPriority"`
	FallbackBehavior         string   `json:"fallbackBehavior"`
	ReviewIndependence       string   `json:"reviewIndependence"`
}

// ExecutionPolicyController owns the /execution-policy route (Checkpoint
// 8P-C). The server always derives the current user from the authenticated
// request via identity.Require -- the client never sends a userId, and
// nothing here trusts one from the body. A nil Mgr keeps the route
// registered but answers OpenAPI-backed 501s, matching every other optional
// controller in this package.
type ExecutionPolicyController struct {
	Mgr executionpolicysvc.Manager
}

// Register mounts the execution-policy routes on the supplied router.
func (c *ExecutionPolicyController) Register(r chi.Router) {
	r.Get("/execution-policy", c.get)
	r.Put("/execution-policy", c.put)
}

func (c *ExecutionPolicyController) get(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, http.MethodGet, "/api/v1/execution-policy")
		return
	}
	user, err := identity.Require(r)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	p, err := c.Mgr.Get(r.Context(), user.ID)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, ExecutionPolicyResponse{Policy: executionPolicyView(p)})
}

func (c *ExecutionPolicyController) put(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, http.MethodPut, "/api/v1/execution-policy")
		return
	}
	user, err := identity.Require(r)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	var in PutExecutionPolicyRequest
	if err := decodeJSONStrict(r, &in); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid JSON body", nil)
		return
	}
	p, err := c.Mgr.Put(r.Context(), user.ID, executionpolicysvc.PutInput{
		AutonomousMode:           in.AutonomousMode,
		PlannerPriority:          profileIDsFromStrings(in.PlannerPriority),
		WorkerPriority:           profileIDsFromStrings(in.WorkerPriority),
		ReviewerPriority:         profileIDsFromStrings(in.ReviewerPriority),
		DecisionResolverPriority: profileIDsFromStrings(in.DecisionResolverPriority),
		FallbackBehavior:         domain.FallbackBehavior(in.FallbackBehavior),
		ReviewIndependence:       domain.ReviewIndependence(in.ReviewIndependence),
	})
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, ExecutionPolicyResponse{Policy: executionPolicyView(p)})
}
