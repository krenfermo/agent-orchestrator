package controllers

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	workflowsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/workflow"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
)

// workflow_placement.go — P1-D §O: the read-only view of the two new
// authorities.
//
// One route, not three. "Why has this run not launched" is one question, and
// answering it from three endpoints an operator has to correlate by hand is how
// the situation became unreadable in the first place: the placement, the
// provider attempt and the admission verdict are the three legs of one answer.
//
// Nothing here is writable. The placement can be inspected and never moved: a
// route that could re-point a placement would be a route that could aim a
// running agent at a different checkout, and the operator command that exists
// instead is "look at why it stopped".
//
// No tokens. A placement's owner token names a daemon incarnation and is AO's
// own local identifier, but it is an ownership credential in shape, and nothing
// an operator needs to diagnose a stuck run. It is not projected.

// ExecutionPlacementView is one frozen execution placement.
type ExecutionPlacementView struct {
	// Type is the FROZEN placement: what AO decided once, before any mutation,
	// and has not re-derived from project configuration since.
	Type string `json:"type" enum:"direct_branch,isolated_worktree"`
	// PlacementGeneration is the placement's OWN generation, distinct from the
	// task's lifecycle generation. A provider failover advances neither.
	PlacementGeneration int64  `json:"placementGeneration"`
	LifecycleGeneration int64  `json:"lifecycleGeneration"`
	State               string `json:"state" enum:"selected,waiting,preparing,ready,active,reviewing,integrating,integrated,conflict,preserved,terminal"`
	// Provenance says whether this placement was frozen at selection or
	// recovered from durable facts for a legacy run. The distinction matters:
	// a recovered placement rests on evidence that PROVED the mode, and a run
	// with no such evidence has no placement at all rather than a guessed one.
	Provenance string `json:"provenance" enum:"frozen_at_selection,recovered_from_durable_facts"`
	TaskID     string `json:"taskId,omitempty"`
	RepoPath   string `json:"repoPath,omitempty"`
	BaseBranch string `json:"baseBranch,omitempty"`
	BaseSHA    string `json:"baseSha,omitempty"`
	// ExecutionBranch is what the agent writes to; MergeTarget is where the
	// work is meant to land. They are different for an isolated placement and
	// the same for a direct-branch one.
	ExecutionBranch string `json:"executionBranch,omitempty"`
	WorktreePath    string `json:"worktreePath,omitempty"`
	MergeTarget     string `json:"mergeTarget,omitempty"`
	IntegratedSHA   string `json:"integratedSha,omitempty"`
	WaitingReason   string `json:"waitingReason,omitempty"`
	Detail          string `json:"detail,omitempty"`
	// Current reports whether this is the newest generation for its
	// obligation. Everything else is history, and history is retained because
	// it is what makes a stale-writer refusal diagnosable afterwards.
	Current bool `json:"current"`
}

// ProviderAttemptView is one durable provider attempt.
type ProviderAttemptView struct {
	ID      string `json:"id"`
	Ordinal int64  `json:"ordinal"`
	// Provider and Profile name who was tried.
	Provider string `json:"provider,omitempty"`
	Profile  string `json:"profile,omitempty"`
	State    string `json:"state" enum:"planned,admitted,launching,running,completed,failed_safe,failed_ambiguous,superseded,abandoned"`
	// Safety is the durable failover classification, taken at the moment the
	// evidence existed rather than recomputed later.
	Safety        string `json:"safety,omitempty" enum:"safe_before_execution,safe_after_proven_no_mutation,ambiguous_execution,completed_execution"`
	FailureClass  string `json:"failureClass,omitempty"`
	FailureReason string `json:"failureReason,omitempty"`
	// LifecycleGeneration and PlacementGeneration are UNCHANGED across a
	// failover, which is what makes a provider attempt not a task generation.
	LifecycleGeneration int64  `json:"lifecycleGeneration"`
	PlacementGeneration int64  `json:"placementGeneration"`
	WorkflowStepID      string `json:"workflowStepId,omitempty"`
	RuntimeSessionID    string `json:"runtimeSessionId,omitempty"`
	CapacityClaimID     string `json:"capacityClaimId,omitempty"`
	// MutationEvidence is the digest that PROVED a
	// safe_after_proven_no_mutation classification. Empty for every other
	// class, and its absence is what makes that class unclaimable.
	MutationEvidence     string `json:"mutationEvidence,omitempty"`
	PredecessorAttemptID string `json:"predecessorAttemptId,omitempty"`
	SuccessorAttemptID   string `json:"successorAttemptId,omitempty"`
	// Authoritative reports whether this attempt is the one currently entitled
	// to act. A chain where none is means the provider budget is spent.
	Authoritative bool `json:"authoritative"`
}

// AdmissionStateView is why the run has not launched.
type AdmissionStateView struct {
	// WaitingReason is the closed vocabulary. It is never a generic "waiting":
	// when AO withholds a launch it knows which authority is withholding it.
	WaitingReason string `json:"waitingReason,omitempty" enum:"capacity_wait,branch_wait,placement_wait,provider_wait,dependency_wait,lifecycle_superseded,strategy_refused"`
	Detail        string `json:"detail,omitempty"`
	// AutoResume separates "AO is queuing, sit tight" from "nothing changes
	// until somebody decides something".
	AutoResume bool `json:"autoResume"`
	// SpendsRetryBudget is always false, and is surfaced so that guarantee is
	// checkable from the API rather than only from a comment.
	SpendsRetryBudget   bool   `json:"spendsRetryBudget"`
	PlacementReady      bool   `json:"placementReady"`
	PlacementState      string `json:"placementState,omitempty"`
	PlacementGeneration int64  `json:"placementGeneration,omitempty"`
	CapacityClaimID     string `json:"capacityClaimId,omitempty"`
	CurrentAttemptID    string `json:"currentAttemptId,omitempty"`
}

// WorkflowPlacementResponse is the body of
// GET /api/v1/workflows/{workflowId}/placement.
type WorkflowPlacementResponse struct {
	Placements       []ExecutionPlacementView `json:"placements"`
	ProviderAttempts []ProviderAttemptView    `json:"providerAttempts"`
	Admission        AdmissionStateView       `json:"admission"`
}

// placementSvc resolves the optional placement capability, answering 501 when
// the deployment has none rather than showing something weaker.
func (c *WorkflowsController) placementSvc(w http.ResponseWriter, r *http.Request, method, route string) (workflowsvc.PlacementManager, bool) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, method, route)
		return nil, false
	}
	svc, ok := c.Svc.(workflowsvc.PlacementManager)
	if !ok {
		apispec.NotImplemented(w, r, method, route)
		return nil, false
	}
	return svc, true
}

func (c *WorkflowsController) getPlacement(w http.ResponseWriter, r *http.Request) {
	svc, ok := c.placementSvc(w, r, http.MethodGet, "/api/v1/workflows/{workflowId}/placement")
	if !ok {
		return
	}
	runID := chi.URLParam(r, "workflowId")
	placements, err := svc.ListPlacements(r.Context(), runID)
	if err != nil {
		writeWorkflowError(w, r, err)
		return
	}
	attempts, err := svc.ListProviderAttempts(r.Context(), runID)
	if err != nil {
		writeWorkflowError(w, r, err)
		return
	}
	admission, err := svc.AdmissionState(r.Context(), runID)
	if err != nil {
		writeWorkflowError(w, r, err)
		return
	}
	out := WorkflowPlacementResponse{
		Placements:       make([]ExecutionPlacementView, 0, len(placements)),
		ProviderAttempts: make([]ProviderAttemptView, 0, len(attempts)),
		Admission: AdmissionStateView{
			WaitingReason: string(admission.WaitingReason), Detail: admission.Detail,
			AutoResume: admission.AutoResume, SpendsRetryBudget: admission.SpendsRetryBudget,
			PlacementReady: admission.PlacementReady, PlacementState: string(admission.PlacementState),
			PlacementGeneration: admission.PlacementGeneration,
			CapacityClaimID:     admission.CapacityClaimID, CurrentAttemptID: admission.CurrentAttemptID,
		},
	}
	for _, p := range placements {
		out.Placements = append(out.Placements, ExecutionPlacementView{
			Type: string(p.Type), PlacementGeneration: p.PlacementGeneration,
			LifecycleGeneration: p.LifecycleGeneration, State: string(p.State),
			Provenance: string(p.Provenance), TaskID: p.TaskID, RepoPath: p.RepoPath,
			BaseBranch: p.BaseBranch, BaseSHA: p.BaseSHA, ExecutionBranch: p.ExecutionBranch,
			WorktreePath: p.WorktreePath, MergeTarget: p.MergeTarget,
			IntegratedSHA: p.IntegratedSHA, WaitingReason: p.WaitingReason,
			Detail: p.Detail, Current: p.Current,
		})
	}
	for _, a := range attempts {
		out.ProviderAttempts = append(out.ProviderAttempts, ProviderAttemptView{
			ID: a.ID, Ordinal: a.Ordinal, Provider: string(a.Provider), Profile: string(a.Profile),
			State: string(a.State), Safety: string(a.Safety), FailureClass: string(a.FailureClass),
			FailureReason: a.FailureReason, LifecycleGeneration: a.LifecycleGeneration,
			PlacementGeneration: a.PlacementGeneration, WorkflowStepID: a.WorkflowStepID,
			RuntimeSessionID: a.RuntimeSessionID, CapacityClaimID: a.CapacityClaimID,
			MutationEvidence: a.MutationEvidence, PredecessorAttemptID: a.PredecessorAttemptID,
			SuccessorAttemptID: a.SuccessorAttemptID, Authoritative: a.Authoritative,
		})
	}
	envelope.WriteJSON(w, http.StatusOK, out)
}
