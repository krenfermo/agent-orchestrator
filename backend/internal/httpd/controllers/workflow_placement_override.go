package controllers

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/identity"
	workflowsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/workflow"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// workflow_placement_override.go — P1-E §B/§C: the two WRITE routes over the
// frozen placement.
//
// P1-D shipped the placement surface read-only and said why: a route that can
// re-point a placement is a route that can aim a running agent at a different
// checkout. These two routes exist because that affordance needed a model, not
// because the objection was wrong — and the model is visible in their shapes:
//
//	POST .../placement/override     a REQUEST. Applies at the freeze; recorded
//	                                and inert if a placement is already frozen.
//	POST .../placement/transition   a generation TRANSITION. Refused unless
//	                                every authority over the old placement has
//	                                provably let go.
//
// Neither route can change where a RUNNING obligation writes. The transition
// retires a generation and freezes a successor; the successor is materialised
// by the ordinary launch path under the ordinary admission gate. There is no
// "move this worktree" operation, and there is deliberately no way to ask for
// one.
//
// A refused transition is 409 with the refusing authority named in the body,
// not a 500 and not a silent 200: "AO will not do this yet, and here is who is
// holding it" is the answer an operator can act on.

// PlacementOverrideRequestBody asks for a placement for one obligation.
type PlacementOverrideRequestBody struct {
	// Placement is what the operator wants. `auto` is a real value: it
	// withdraws any standing override and defers to selection policy.
	Placement string `json:"placement" enum:"auto,direct_branch,isolated_worktree" required:"true"`
	// TaskID scopes the request to one planned task. Empty means the run's own
	// obligation.
	TaskID string `json:"taskId,omitempty"`
	// Reason is recorded on the durable request. It is what makes the audit
	// row answer "why" as well as "what".
	Reason string `json:"reason,omitempty"`
}

// PlacementOverrideView is one recorded request.
type PlacementOverrideView struct {
	Placement   string `json:"placement" enum:"auto,direct_branch,isolated_worktree"`
	RequestedBy string `json:"requestedBy,omitempty"`
	Reason      string `json:"reason,omitempty"`
	State       string `json:"state" enum:"requested,applied,superseded,refused"`
	// AppliedGeneration names the placement generation that consumed this
	// request, so "which placement did my override produce" is answerable
	// rather than inferred from timestamps.
	AppliedGeneration int64  `json:"appliedGeneration,omitempty"`
	TaskID            string `json:"taskId,omitempty"`
	Detail            string `json:"detail,omitempty"`
}

// PlacementOverrideResponse is the body of POST .../placement/override.
type PlacementOverrideResponse struct {
	Override PlacementOverrideView `json:"override"`
	// AppliesAtFreeze reports the ordinary path: nothing is frozen yet, and the
	// next freeze will use this request.
	AppliesAtFreeze bool `json:"appliesAtFreeze"`
	// RequiresTransition reports that a placement is already frozen, so this
	// request changes NOTHING until a transition consumes it. It is surfaced
	// rather than implied so an operator is never left believing a request took
	// effect when it did not.
	RequiresTransition bool `json:"requiresTransition"`
	// CurrentPlacement is the frozen placement, when there is one.
	CurrentPlacement *ExecutionPlacementView `json:"currentPlacement,omitempty"`
}

// PlacementTransitionRequestBody asks to replace a frozen placement generation.
type PlacementTransitionRequestBody struct {
	Placement string `json:"placement" enum:"auto,direct_branch,isolated_worktree" required:"true"`
	TaskID    string `json:"taskId,omitempty"`
	Reason    string `json:"reason,omitempty"`
	// ExpectedState is the placement state the requester believes the current
	// generation is in. A mismatch is refused rather than corrected: the
	// request describes a world that no longer holds. Optional — the quiescence
	// proof is the safety property; this is an additional guard an operator may
	// choose to use.
	ExpectedState string `json:"expectedState,omitempty" enum:"selected,waiting,preparing,ready,active,reviewing,integrating,integrated,conflict,preserved,terminal"`
	// ExpectedGeneration is the generation the requester means to supersede.
	// Zero means "whatever is current". A stale non-zero value is refused,
	// which is what makes this safe to retry from a page an operator has had
	// open for a while.
	ExpectedGeneration int64 `json:"expectedGeneration,omitempty"`
}

// PlacementQuiescenceView is the proof, or the part of it that failed.
type PlacementQuiescenceView struct {
	Quiesced               bool `json:"quiesced"`
	RunActive              bool `json:"runActive"`
	NoProviderAttempt      bool `json:"noProviderAttempt"`
	NoCapacityClaim        bool `json:"noCapacityClaim"`
	NoLiveRuntime          bool `json:"noLiveRuntime"`
	NoBranchAuthority      bool `json:"noBranchAuthority"`
	NoIntegrationAuthority bool `json:"noIntegrationAuthority"`
	// Digest is the recorded form: the facts each authority answered with, and
	// a short hash of them, so two proofs can be compared without reading prose.
	Digest string `json:"digest,omitempty"`
}

// PlacementTransitionView is one recorded transition, refusals included.
type PlacementTransitionView struct {
	FromGeneration int64  `json:"fromGeneration"`
	ToGeneration   int64  `json:"toGeneration,omitempty"`
	FromType       string `json:"fromType,omitempty" enum:"direct_branch,isolated_worktree"`
	ToType         string `json:"toType,omitempty" enum:"direct_branch,isolated_worktree"`
	Placement      string `json:"placement" enum:"auto,direct_branch,isolated_worktree"`
	RequestedBy    string `json:"requestedBy,omitempty"`
	Reason         string `json:"reason,omitempty"`
	ExpectedState  string `json:"expectedState,omitempty"`
	State          string `json:"state" enum:"requested,applied,refused"`
	// RefusalReason names the AUTHORITY that said no, from a closed vocabulary,
	// because the operator's next action differs per authority: a held capacity
	// claim clears on its own, a live runtime needs stopping, a drifted state
	// means the request has to be re-made against what is true now.
	RefusalReason string `json:"refusalReason,omitempty" enum:"no_operator_authority,unknown_placement_request,no_frozen_placement,placement_not_current,lifecycle_state_drifted,active_provider_attempt,held_capacity_claim,live_runtime,held_branch_authority,outstanding_integration,run_is_terminal,authority_unreadable"`
	Quiescence    string `json:"quiescence,omitempty"`
	TaskID        string `json:"taskId,omitempty"`
	Detail        string `json:"detail,omitempty"`
}

// PlacementTransitionResponse is the body of POST .../placement/transition.
type PlacementTransitionResponse struct {
	// Applied reports that a replacement generation now exists.
	Applied bool `json:"applied"`
	// AlreadyApplied reports that this generation had already been superseded
	// and the returned rows are the ones that did it. A repeated request is
	// this, never a second generation.
	AlreadyApplied bool                    `json:"alreadyApplied"`
	Transition     PlacementTransitionView `json:"transition"`
	From           *ExecutionPlacementView `json:"from,omitempty"`
	To             *ExecutionPlacementView `json:"to,omitempty"`
	Quiescence     PlacementQuiescenceView `json:"quiescence"`
}

// placementOverrideSvc resolves the optional write capability, answering 501
// when the deployment has none rather than pretending to have moved something.
func (c *WorkflowsController) placementOverrideSvc(w http.ResponseWriter, r *http.Request, method, route string) (workflowsvc.PlacementOverrideManager, bool) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, method, route)
		return nil, false
	}
	svc, ok := c.Svc.(workflowsvc.PlacementOverrideManager)
	if !ok {
		apispec.NotImplemented(w, r, method, route)
		return nil, false
	}
	return svc, true
}

// operatorIdentity names who is asking. It mirrors the repair route's rule: the
// authenticated user when there is one, and the honest constant "operator" on
// the unauthenticated loopback listener, which is the only deployment where
// there is nobody to name.
func operatorIdentity(r *http.Request) string {
	if user, found := identity.FromContext(r.Context()); found && string(user.ID) != "" {
		return string(user.ID)
	}
	return "operator"
}

func (c *WorkflowsController) requestPlacementOverride(w http.ResponseWriter, r *http.Request) {
	svc, ok := c.placementOverrideSvc(w, r, http.MethodPost, "/api/v1/workflows/{workflowId}/placement/override")
	if !ok {
		return
	}
	var body PlacementOverrideRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err.Error() != "EOF" {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_BODY", "request body is not valid JSON", nil)
		return
	}
	outcome, err := svc.RequestPlacementOverride(r.Context(), workflowcore.PlacementOverrideRequestInput{
		RunID:       chi.URLParam(r, "workflowId"),
		TaskID:      strings.TrimSpace(body.TaskID),
		Requested:   domain.PlacementOverrideRequest(strings.TrimSpace(body.Placement)),
		RequestedBy: operatorIdentity(r),
		Reason:      body.Reason,
	})
	if err != nil {
		writeWorkflowError(w, r, err)
		return
	}
	out := PlacementOverrideResponse{
		Override:           placementOverrideView(outcome.Override),
		AppliesAtFreeze:    outcome.AppliesAtFreeze,
		RequiresTransition: outcome.RequiresTransition,
	}
	if outcome.RequiresTransition {
		view := executionPlacementViewOf(outcome.CurrentPlacement, true)
		out.CurrentPlacement = &view
	}
	envelope.WriteJSON(w, http.StatusAccepted, out)
}

func (c *WorkflowsController) transitionPlacement(w http.ResponseWriter, r *http.Request) {
	svc, ok := c.placementOverrideSvc(w, r, http.MethodPost, "/api/v1/workflows/{workflowId}/placement/transition")
	if !ok {
		return
	}
	var body PlacementTransitionRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err.Error() != "EOF" {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_BODY", "request body is not valid JSON", nil)
		return
	}
	outcome, err := svc.TransitionPlacement(r.Context(), workflowcore.PlacementTransitionInput{
		RunID:              chi.URLParam(r, "workflowId"),
		TaskID:             strings.TrimSpace(body.TaskID),
		Requested:          domain.PlacementOverrideRequest(strings.TrimSpace(body.Placement)),
		RequestedBy:        operatorIdentity(r),
		Reason:             body.Reason,
		ExpectedState:      domain.ExecutionPlacementState(strings.TrimSpace(body.ExpectedState)),
		ExpectedGeneration: body.ExpectedGeneration,
	})
	if err != nil {
		writeWorkflowError(w, r, err)
		return
	}
	out := PlacementTransitionResponse{
		Applied:        outcome.Applied,
		AlreadyApplied: outcome.AlreadyApplied,
		Transition:     placementTransitionViewOf(outcome.Transition),
		Quiescence: PlacementQuiescenceView{
			Quiesced:               outcome.Quiescence.Quiesced(),
			RunActive:              outcome.Quiescence.RunActive,
			NoProviderAttempt:      outcome.Quiescence.NoProviderAttempt,
			NoCapacityClaim:        outcome.Quiescence.NoCapacityClaim,
			NoLiveRuntime:          outcome.Quiescence.NoLiveRuntime,
			NoBranchAuthority:      outcome.Quiescence.NoBranchAuthority,
			NoIntegrationAuthority: outcome.Quiescence.NoIntegrationAuthority,
			Digest:                 outcome.Quiescence.Digest,
		},
	}
	if outcome.From.PlacementGeneration > 0 {
		view := executionPlacementViewOf(outcome.From, false)
		out.From = &view
	}
	if outcome.To.PlacementGeneration > 0 {
		view := executionPlacementViewOf(outcome.To, true)
		out.To = &view
	}
	if outcome.Refusal != "" {
		// 409, with the refusing authority named in the details. A refusal is a
		// correct answer about the run's current state, not a server fault, and
		// what an operator needs from it is WHICH authority is holding the
		// placement -- their next action differs per authority. The quiescence
		// digest rides along so the refusal can be diffed against the proof a
		// later, successful attempt records.
		envelope.WriteAPIError(w, r, http.StatusConflict, "conflict", "PLACEMENT_TRANSITION_REFUSED",
			string(outcome.Refusal)+": "+outcome.Detail, map[string]any{
				"refusalReason":  string(outcome.Refusal),
				"fromGeneration": outcome.From.PlacementGeneration,
				"quiescence":     outcome.Quiescence.Digest,
			})
		return
	}
	envelope.WriteJSON(w, http.StatusAccepted, out)
}

func placementOverrideView(o domain.ExecutionPlacementOverride) PlacementOverrideView {
	return PlacementOverrideView{
		Placement: string(o.Requested), RequestedBy: o.RequestedBy, Reason: o.Reason,
		State: string(o.State), AppliedGeneration: o.AppliedGeneration,
		TaskID: o.TaskID, Detail: o.Detail,
	}
}

func placementTransitionViewOf(t domain.ExecutionPlacementTransition) PlacementTransitionView {
	return PlacementTransitionView{
		FromGeneration: t.FromGeneration, ToGeneration: t.ToGeneration,
		FromType: string(t.FromType), ToType: string(t.ToType),
		Placement: string(t.Requested), RequestedBy: t.RequestedBy, Reason: t.Reason,
		ExpectedState: string(t.ExpectedState), State: string(t.State),
		RefusalReason: string(t.RefusalReason), Quiescence: t.QuiescenceDigest,
		TaskID: t.TaskID, Detail: t.Detail,
	}
}

// executionPlacementViewOf projects a placement record for these two routes. It
// reuses the read route's view type verbatim, so an operator reading a
// transition's `to` sees the same shape `GET .../placement` will show them a
// moment later — and no token, for the same reason it exposes none there.
func executionPlacementViewOf(p domain.ExecutionPlacement, current bool) ExecutionPlacementView {
	return ExecutionPlacementView{
		Type: string(p.Type), PlacementGeneration: p.PlacementGeneration,
		LifecycleGeneration: p.LifecycleGeneration, State: string(p.State),
		Provenance: string(p.Provenance), TaskID: p.TaskID, RepoPath: p.RepoPath,
		BaseBranch: p.BaseBranch, BaseSHA: p.BaseSHA, ExecutionBranch: p.ExecutionBranch,
		WorktreePath: p.WorktreePath, MergeTarget: p.MergeTarget,
		IntegratedSHA: p.IntegratedSHA, WaitingReason: p.WaitingReason,
		Detail: p.Detail, Current: current,
	}
}
