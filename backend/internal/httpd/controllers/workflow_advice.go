package controllers

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	workflowsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/workflow"
	workflowcore "github.com/aoagents/agent-orchestrator/backend/internal/workflow"
)

// workflow_advice.go — P3-C §24: the "what do I do?" surface.
//
// Before this route a client that wanted the answer had to read the run detail,
// the recovery assessment and the repair plan, and then compose them itself —
// which is exactly the frontend heuristic the checkpoint forbids, because three
// clients composing them three ways is three different answers to one question.
// GET /workflows/{id}/advice is that composition, done once, on the server, from
// durable facts.
//
// It is a strict READ. workflowcore.Advice is a pure projection and the
// coordinator method behind it writes nothing, so polling this route cannot
// move a run — §32's "no GET/read endpoint performs mutation" is a structural
// property here, not a convention someone has to remember.

// WorkflowAdviceView is the deterministic answer to "what do I do now".
type WorkflowAdviceView struct {
	// Category is the four-way classification, plus the honest fifth answer a
	// run that is simply working has.
	Category string `json:"category" enum:"no_action_required,auto_recoverable,wait_only,human_action,terminal"`
	// Stage is the human status vocabulary, the same value the Board card and
	// the run detail page render.
	Stage string `json:"stage,omitempty"`
	// SummaryCode is the stable key a UI renders its headline from. Summary and
	// Explanation are AO's own English fallbacks for a client with no copy for
	// it — never the primary contract.
	SummaryCode string `json:"summaryCode,omitempty"`
	Summary     string `json:"summary,omitempty"`
	Explanation string `json:"explanation,omitempty"`
	// ReasonCode is the canonical attention reason, never a new vocabulary.
	ReasonCode string `json:"reasonCode,omitempty"`
	// RequiresHuman is the single flag any surface uses to decide whether to
	// interrupt somebody.
	RequiresHuman bool `json:"requiresHuman"`
	// AutomaticAction is what AO intends to do by itself;
	// AutomaticActionActive reports that it is already happening.
	AutomaticAction       string `json:"automaticAction,omitempty" enum:"launch_repair,repair_in_flight,scheduled_retry,provider_failover,await_capacity,await_branch,resolve_question,fresh_review"`
	AutomaticActionActive bool   `json:"automaticActionActive"`
	// AutomaticActionBlockedReason names why an automatic action AO would
	// otherwise take is not on offer.
	AutomaticActionBlockedReason string `json:"automaticActionBlockedReason,omitempty"`
	// RecommendedAction is the ONE thing AO suggests a person do, empty when
	// the honest answer is "nothing".
	RecommendedAction string `json:"recommendedAction,omitempty"`
	// AvailableActions and BlockedActions are the closed offer set and the
	// reasons behind every refusal. A blocked action is REPORTED rather than
	// hidden: "why is this greyed out" is answerable, "where did the button go"
	// is not.
	AvailableActions []string                    `json:"availableActions,omitempty"`
	BlockedActions   []WorkflowAdviceBlockedView `json:"blockedActions,omitempty"`
	// ExpectedNextStage is where the run goes next if nothing else changes.
	// Empty when AO cannot say — never guessed.
	ExpectedNextStage string `json:"expectedNextStage,omitempty"`
	// Retryable and Repairable are the two capability answers a client should
	// never re-derive.
	Retryable         bool   `json:"retryable"`
	Repairable        bool   `json:"repairable"`
	RepairEligibility string `json:"repairEligibility,omitempty" enum:"eligible,ineligible,budget_exhausted,policy_disabled,unknown_condition"`
	RepairSpent       int    `json:"repairSpent"`
	RepairBudget      int    `json:"repairBudget"`
	// WaitUntil / WaitReason are the run's soonest open wake, when there is one.
	WaitUntil  *time.Time `json:"waitUntil,omitempty"`
	WaitReason string     `json:"waitReason,omitempty"`
	// TargetRunID is the run somebody should actually act on: this one, or the
	// child whose stop this one mirrors.
	TargetRunID string `json:"targetRunId,omitempty"`
	// Authority is the proof a mutating action must be revalidated against.
	Authority WorkflowAdviceAuthorityView `json:"authority"`
	// Version is the advice-rules version, so an answer stays explainable after
	// the rules change.
	Version string `json:"version,omitempty"`
}

// WorkflowAdviceBlockedView is one action AO will not perform, with the stable
// code that says why.
type WorkflowAdviceBlockedView struct {
	Action string `json:"action"`
	Reason string `json:"reason"`
}

// WorkflowAdviceAuthorityView is the generation/stop proof a client sends back
// when it executes an action computed against this reading.
type WorkflowAdviceAuthorityView struct {
	PlacementGeneration int64      `json:"placementGeneration,omitempty"`
	LifecycleGeneration int64      `json:"lifecycleGeneration,omitempty"`
	RepairGeneration    int        `json:"repairGeneration,omitempty"`
	StopPhase           string     `json:"stopPhase,omitempty"`
	StopAt              *time.Time `json:"stopAt,omitempty"`
	RunState            string     `json:"runState,omitempty"`
}

// WorkflowAdviceResponse is the route's body.
type WorkflowAdviceResponse struct {
	Advice WorkflowAdviceView `json:"advice"`
}

// workflowAdviceView projects the core value onto the API contract.
func workflowAdviceView(a workflowcore.Advice) WorkflowAdviceView {
	out := WorkflowAdviceView{
		Category:                     string(a.Category),
		Stage:                        string(a.Stage),
		SummaryCode:                  a.SummaryCode,
		Summary:                      a.Summary,
		Explanation:                  a.Explanation,
		ReasonCode:                   a.ReasonCode,
		RequiresHuman:                a.RequiresHuman,
		AutomaticAction:              string(a.AutomaticAction),
		AutomaticActionActive:        a.AutomaticActionActive,
		AutomaticActionBlockedReason: a.AutomaticActionBlockedReason,
		RecommendedAction:            string(a.RecommendedAction),
		ExpectedNextStage:            string(a.ExpectedNextStage),
		Retryable:                    a.Retryable,
		Repairable:                   a.Repairable,
		RepairEligibility:            string(a.RepairEligibility),
		RepairSpent:                  a.RepairSpent,
		RepairBudget:                 a.RepairBudget,
		WaitReason:                   a.WaitReason,
		TargetRunID:                  a.TargetRunID,
		Version:                      a.Version,
		Authority: WorkflowAdviceAuthorityView{
			PlacementGeneration: a.Authority.PlacementGeneration,
			LifecycleGeneration: a.Authority.LifecycleGeneration,
			RepairGeneration:    a.Authority.RepairGeneration,
			StopPhase:           a.Authority.StopPhase,
			RunState:            string(a.Authority.RunState),
		},
	}
	for _, id := range a.AvailableActions {
		out.AvailableActions = append(out.AvailableActions, string(id))
	}
	for _, b := range a.BlockedActions {
		out.BlockedActions = append(out.BlockedActions, WorkflowAdviceBlockedView{
			Action: string(b.ID), Reason: b.Reason,
		})
	}
	if a.WaitUntil != nil {
		v := a.WaitUntil.UTC()
		out.WaitUntil = &v
	}
	if !a.Authority.StopAt.IsZero() {
		at := a.Authority.StopAt.UTC()
		out.Authority.StopAt = &at
	}
	return out
}

// advisorSvc resolves the optional advisor capability, answering 501 when the
// deployment has none rather than doing something weaker.
func (c *WorkflowsController) advisorSvc(w http.ResponseWriter, r *http.Request, method, route string) (workflowsvc.AdvisorManager, bool) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, method, route)
		return nil, false
	}
	svc, ok := c.Svc.(workflowsvc.AdvisorManager)
	if !ok {
		apispec.NotImplemented(w, r, method, route)
		return nil, false
	}
	return svc, true
}

// getAdvice answers "what do I do now" for one run. It writes nothing.
func (c *WorkflowsController) getAdvice(w http.ResponseWriter, r *http.Request) {
	svc, ok := c.advisorSvc(w, r, http.MethodGet, "/api/v1/workflows/{workflowId}/advice")
	if !ok {
		return
	}
	advice, err := svc.AdviceFor(r.Context(), chi.URLParam(r, "workflowId"))
	if err != nil {
		writeWorkflowError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, WorkflowAdviceResponse{Advice: workflowAdviceView(advice)})
}

// WorkflowActionAuthorityRequest is the proof a client sends back when it
// executes an action it read out of an Advice.
//
// Every field is optional and an omitted one is simply not compared. That is
// deliberate: a client that sends no proof gets exactly the pre-P3-C behaviour,
// because the executing paths are each individually idempotent and the worst a
// missing check produces is a no-op. What the proof buys is the ability to tell
// the person WHY nothing happened, which a silent no-op cannot.
type WorkflowActionAuthorityRequest struct {
	PlacementGeneration int64  `json:"placementGeneration,omitempty"`
	LifecycleGeneration int64  `json:"lifecycleGeneration,omitempty"`
	RepairGeneration    int    `json:"repairGeneration,omitempty"`
	StopPhase           string `json:"stopPhase,omitempty"`
	RunState            string `json:"runState,omitempty"`
}

// authority converts the request into the core proof value.
func (a *WorkflowActionAuthorityRequest) authority() workflowcore.AdviceAuthority {
	if a == nil {
		return workflowcore.AdviceAuthority{}
	}
	return workflowcore.AdviceAuthority{
		PlacementGeneration: a.PlacementGeneration,
		LifecycleGeneration: a.LifecycleGeneration,
		RepairGeneration:    a.RepairGeneration,
		StopPhase:           a.StopPhase,
	}
}

// refuseStaleAction revalidates one mutating action against the run as it
// stands now, and writes the 409 a stale click deserves.
//
// It returns true when the caller must STOP. A deployment with no advisor
// capability, or a coordinator that cannot answer, returns false: the action
// then proceeds exactly as it did before P3-C rather than being refused on the
// strength of a check that could not run.
//
// The refusal is a 409 rather than a 400 because nothing about the request was
// malformed — the world moved. And it carries a stable code plus AO's own
// sentence, so the UI can say "AO is already repairing this run" instead of
// "conflict".
func (c *WorkflowsController) refuseStaleAction(
	w http.ResponseWriter, r *http.Request, runID string,
	action workflowcore.ActionID, expected workflowcore.AdviceAuthority,
) bool {
	svc, ok := c.Svc.(workflowsvc.AdvisorManager)
	if !ok {
		return false
	}
	mismatch, err := svc.RevalidateActionAuthority(r.Context(), runID, action, expected)
	if err != nil || mismatch == workflowcore.AuthorityMismatchNone {
		return false
	}
	envelope.WriteAPIError(w, r, http.StatusConflict, "conflict", "ACTION_SUPERSEDED",
		mismatch.Describe(), map[string]any{"mismatch": string(mismatch), "action": string(action)})
	return true
}
