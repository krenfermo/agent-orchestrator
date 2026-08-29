package controllers

import (
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

// workflow_recovery.go — P1-B §K: the recovery surface, as named operations.
//
// The alternative was to keep widening POST /continue until it did everything,
// and that is exactly what §K rules out: an operator pressing one ambiguous
// button cannot know beforehand whether it will resume a worker, approve a
// plan, regenerate one, or write code. Each of those is now its own route with
// its own preconditions, and /continue keeps working and now says which
// recovery action it took.

// WorkflowRecoveryView is the deterministic recovery assessment for one run.
// Everything in it is derived from durable facts; nothing is a model's opinion.
type WorkflowRecoveryView struct {
	// RecommendedAction is the ONE thing AO advises doing.
	RecommendedAction string `json:"recommendedAction" enum:"resume,reuse_plan,regenerate_plan,repair,authenticate,inspect_repository,operator_action,restart_required,abandon,terminal,unrecoverable"`
	// ReasonCode is the canonical stop reason behind it.
	ReasonCode string `json:"reasonCode,omitempty"`
	// Explanation is AO's own sentence, and for a human-owned stop it is the
	// same sentence the Board shows, never a second wording of it.
	Explanation string `json:"explanation,omitempty"`
	// AutomaticAllowed reports whether AO may take the action itself.
	AutomaticAllowed bool `json:"automaticAllowed"`
	// PlanReusable classifies the durable plan.
	PlanReusable string `json:"planReusable" enum:"not_applicable,exact,stale_but_revalidatable,not_reusable"`
	// RepairAvailable and RepairEligibility are the repair decision and the
	// reason behind it.
	RepairAvailable   bool   `json:"repairAvailable"`
	RepairEligibility string `json:"repairEligibility" enum:"eligible,ineligible,budget_exhausted,policy_disabled,unknown_condition"`
	// BlockingCondition names, in AO's words, what stands between this run and
	// progress.
	BlockingCondition string `json:"blockingCondition,omitempty"`
	// Obligation is the durable obligation a resume would discharge.
	Obligation string `json:"obligation,omitempty" enum:"none,plan_generation,plan_approval,plan_dispatch,work_dispatch,work_observation,review_dispatch,review_observation,fix_delivery,fix_observation,verify,convergence,terminal"`
	// ObligationDetail is the sentence explaining that obligation.
	ObligationDetail string `json:"obligationDetail,omitempty"`
	// Strategy is the run's frozen execution strategy (P1-A).
	Strategy string `json:"strategy,omitempty" enum:"task,autonomous,master"`
	// TargetRunID is the run an operator should act on: this one, or the child
	// whose stop this one is mirroring.
	TargetRunID string `json:"targetRunId,omitempty"`
	StepID      string `json:"stepId,omitempty"`
	TaskID      string `json:"taskId,omitempty"`
	// Version is the assessment-rules version, so a recommendation stays
	// explainable after the rules change.
	Version string `json:"version,omitempty"`
}

// WorkflowResumeView is what a resume actually did.
type WorkflowResumeView struct {
	// Obligation and ObligationDetail are what was outstanding when the call
	// started.
	Obligation       string `json:"obligation" enum:"none,plan_generation,plan_approval,plan_dispatch,work_dispatch,work_observation,review_dispatch,review_observation,fix_delivery,fix_observation,verify,convergence,terminal"`
	ObligationDetail string `json:"obligationDetail,omitempty"`
	// Performed reports whether AO re-entered the resume path. False means the
	// obligation was a person's, or there was nothing to do -- never an error.
	Performed bool `json:"performed"`
}

// WorkflowPlanReuseView is the plan's reuse classification.
type WorkflowPlanReuseView struct {
	Reusability string `json:"reusability" enum:"not_applicable,exact,stale_but_revalidatable,not_reusable"`
	// Revision is the plan's durable generation.
	Revision int64 `json:"revision"`
	// PlanHash is its recorded structural identity.
	PlanHash string `json:"planHash,omitempty"`
	// TaskCount is how many tasks the current revision holds.
	TaskCount int `json:"taskCount"`
	// Reason is AO's sentence about the classification, and ContextDrift names
	// the manifest comparison's verdict when the plan is stale.
	Reason       string `json:"reason,omitempty"`
	ContextDrift string `json:"contextDrift,omitempty"`
}

// WorkflowRepairPlanView is what a Repair Agent would do, without doing it.
type WorkflowRepairPlanView struct {
	Eligibility string `json:"eligibility" enum:"eligible,ineligible,budget_exhausted,policy_disabled,unknown_condition"`
	// Mode, Spent and Budget are the frozen policy in force for this run.
	Mode   string `json:"mode" enum:"disabled,suggest,automatic"`
	Spent  int    `json:"spent"`
	Budget int    `json:"budget"`
	Reason string `json:"reason,omitempty"`
	// AutomaticAllowed reports whether AO may launch it unasked.
	AutomaticAllowed bool `json:"automaticAllowed"`
	// Intent is present only for an eligible repair.
	Intent *WorkflowRepairIntentView `json:"intent,omitempty"`
}

// WorkflowRepairIntentView is the durable record of one repair.
type WorkflowRepairIntentView struct {
	ID              string `json:"id"`
	TargetRunID     string `json:"targetRunId"`
	TargetStepID    string `json:"targetStepId,omitempty"`
	ConditionReason string `json:"conditionReason"`
	// EvidenceDigest identifies the FAILURE being repaired, so two intents can
	// be compared without disclosing anything about it.
	EvidenceDigest string `json:"evidenceDigest"`
	Generation     int    `json:"generation"`
	ProjectID      string `json:"projectId"`
	// Strategy is always "task": a repair is bounded work.
	Strategy           string   `json:"strategy" enum:"task,autonomous,master"`
	AcceptanceCriteria []string `json:"acceptanceCriteria,omitempty"`
	RepairRunID        string   `json:"repairRunId,omitempty"`
	AuthorizedBy       string   `json:"authorizedBy,omitempty"`
	Mode               string   `json:"mode,omitempty" enum:"disabled,suggest,automatic"`
	PolicyVersion      string   `json:"policyVersion,omitempty"`
}

// WorkflowRepairResponse is the body of POST /workflows/{id}/repair.
type WorkflowRepairResponse struct {
	Repair   WorkflowRepairPlanView    `json:"repair"`
	Intent   *WorkflowRepairIntentView `json:"intent,omitempty"`
	Workflow WorkflowRunDetailView     `json:"workflow"`
}

// WorkflowRecoveryResponse is the body of GET /workflows/{id}/recovery.
type WorkflowRecoveryResponse struct {
	Recovery WorkflowRecoveryView   `json:"recovery"`
	Repair   WorkflowRepairPlanView `json:"repair"`
	Plan     WorkflowPlanReuseView  `json:"plan"`
}

func workflowRecoveryView(a workflowcore.RecoveryAssessment) *WorkflowRecoveryView {
	if a.RecommendedAction == "" {
		return nil
	}
	return &WorkflowRecoveryView{
		RecommendedAction: string(a.RecommendedAction),
		ReasonCode:        a.ReasonCode,
		Explanation:       a.Explanation,
		AutomaticAllowed:  a.AutomaticAllowed,
		PlanReusable:      string(a.PlanReusable),
		RepairAvailable:   a.RepairAvailable,
		RepairEligibility: string(a.RepairEligibility),
		BlockingCondition: a.BlockingCondition,
		Obligation:        string(a.Obligation.Kind),
		ObligationDetail:  a.Obligation.Explanation,
		Strategy:          string(a.Strategy),
		TargetRunID:       a.TargetRunID,
		StepID:            a.StepID,
		TaskID:            a.TaskID,
		Version:           a.Version,
	}
}

func workflowPlanReuseView(a workflowcore.PlanReuseAssessment) WorkflowPlanReuseView {
	return WorkflowPlanReuseView{
		Reusability: string(a.Reusability), Revision: a.Revision, PlanHash: a.PlanHash,
		TaskCount: a.TaskCount, Reason: a.Reason, ContextDrift: a.ContextDrift,
	}
}

func workflowRepairIntentView(intent domain.RepairIntent) *WorkflowRepairIntentView {
	if intent.ID == "" {
		return nil
	}
	return &WorkflowRepairIntentView{
		ID: intent.ID, TargetRunID: intent.TargetRunID, TargetStepID: intent.TargetStepID,
		ConditionReason: intent.ConditionReason, EvidenceDigest: intent.EvidenceDigest,
		Generation: intent.Generation, ProjectID: intent.ProjectID,
		Strategy: string(intent.Strategy), AcceptanceCriteria: intent.AcceptanceCriteria,
		RepairRunID: intent.RepairRunID, AuthorizedBy: intent.AuthorizedBy,
		Mode: string(intent.Mode), PolicyVersion: intent.PolicyVersion,
	}
}

func workflowRepairPlanView(p workflowcore.RepairPlan) WorkflowRepairPlanView {
	return WorkflowRepairPlanView{
		Eligibility: string(p.Eligibility), Mode: string(p.Mode), Spent: p.Spent,
		Budget: p.Budget, Reason: p.Reason, AutomaticAllowed: p.AutomaticAllowed,
		Intent: workflowRepairIntentView(p.Intent),
	}
}

// recoverySvc resolves the optional recovery capability, answering 501 when the
// deployment has none rather than doing something weaker.
func (c *WorkflowsController) recoverySvc(w http.ResponseWriter, r *http.Request, method, route string) (workflowsvc.RecoveryManager, bool) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, method, route)
		return nil, false
	}
	svc, ok := c.Svc.(workflowsvc.RecoveryManager)
	if !ok {
		apispec.NotImplemented(w, r, method, route)
		return nil, false
	}
	return svc, true
}

// getRecovery answers "what should I do about this run", deterministically.
func (c *WorkflowsController) getRecovery(w http.ResponseWriter, r *http.Request) {
	svc, ok := c.recoverySvc(w, r, http.MethodGet, "/api/v1/workflows/{workflowId}/recovery")
	if !ok {
		return
	}
	runID := chi.URLParam(r, "workflowId")
	assessment, err := svc.AssessRecovery(r.Context(), runID)
	if err != nil {
		writeWorkflowError(w, r, err)
		return
	}
	repair, err := svc.PlanRepair(r.Context(), runID)
	if err != nil {
		writeWorkflowError(w, r, err)
		return
	}
	out := WorkflowRecoveryResponse{Repair: workflowRepairPlanView(repair)}
	if view := workflowRecoveryView(assessment); view != nil {
		out.Recovery = *view
	}
	out.Plan = WorkflowPlanReuseView{Reusability: string(assessment.PlanReusable)}
	envelope.WriteJSON(w, http.StatusOK, out)
}

// resume discharges exactly the run's outstanding durable obligation, and says
// which one it was.
func (c *WorkflowsController) resume(w http.ResponseWriter, r *http.Request) {
	svc, ok := c.recoverySvc(w, r, http.MethodPost, "/api/v1/workflows/{workflowId}/resume")
	if !ok {
		return
	}
	detail, report, err := svc.ResumeRun(r.Context(), chi.URLParam(r, "workflowId"))
	if err != nil {
		writeWorkflowError(w, r, err)
		return
	}
	view := c.workflowRunDetailView(r.Context(), detail)
	view.Run.Recovery = workflowRecoveryView(report.Assessment)
	view.Resume = &WorkflowResumeView{
		Obligation:       string(report.Obligation.Kind),
		ObligationDetail: report.Obligation.Explanation,
		Performed:        report.Performed,
	}
	envelope.WriteJSON(w, http.StatusOK, WorkflowRunResponse{Workflow: view})
}

// reusePlan executes an existing plan revision as it stands.
func (c *WorkflowsController) reusePlan(w http.ResponseWriter, r *http.Request) {
	svc, ok := c.recoverySvc(w, r, http.MethodPost, "/api/v1/workflows/{workflowId}/plan/reuse")
	if !ok {
		return
	}
	detail, assessment, err := svc.ReusePlan(r.Context(), chi.URLParam(r, "workflowId"))
	if err != nil {
		writeWorkflowError(w, r, err)
		return
	}
	view := c.workflowRunDetailView(r.Context(), detail)
	planView := workflowPlanReuseView(assessment)
	view.PlanReuse = &planView
	envelope.WriteJSON(w, http.StatusOK, WorkflowRunResponse{Workflow: view})
}

// regeneratePlan mints a new durable plan revision.
func (c *WorkflowsController) regeneratePlan(w http.ResponseWriter, r *http.Request) {
	svc, ok := c.recoverySvc(w, r, http.MethodPost, "/api/v1/workflows/{workflowId}/plan/regenerate")
	if !ok {
		return
	}
	detail, assessment, err := svc.RegeneratePlan(r.Context(), chi.URLParam(r, "workflowId"))
	if err != nil {
		writeWorkflowError(w, r, err)
		return
	}
	view := c.workflowRunDetailView(r.Context(), detail)
	planView := workflowPlanReuseView(assessment)
	view.PlanReuse = &planView
	envelope.WriteJSON(w, http.StatusOK, WorkflowRunResponse{Workflow: view})
}

// repair launches a bounded Repair Agent on the caller's authority.
//
// GET-style previewing is deliberately part of the same surface: a caller that
// asks and is refused gets the full RepairPlan explaining why, with the same
// vocabulary the recovery assessment uses.
func (c *WorkflowsController) repair(w http.ResponseWriter, r *http.Request) {
	svc, ok := c.recoverySvc(w, r, http.MethodPost, "/api/v1/workflows/{workflowId}/repair")
	if !ok {
		return
	}
	runID := chi.URLParam(r, "workflowId")
	authorizedBy := "operator"
	if user, found := identity.FromContext(r.Context()); found {
		authorizedBy = string(user.ID)
	}
	plan, err := svc.PlanRepair(r.Context(), runID)
	if err != nil {
		writeWorkflowError(w, r, err)
		return
	}
	intent, err := svc.LaunchRepair(r.Context(), runID, authorizedBy)
	if err != nil {
		// A refusal is a 4xx with the reason, never a 500: an ineligible
		// condition is a correct answer about the run, not a server fault.
		if strings.Contains(err.Error(), workflowcore.ErrRepairIneligible.Error()) ||
			strings.Contains(err.Error(), workflowcore.ErrRepairUnsafeTarget.Error()) {
			envelope.WriteAPIError(w, r, http.StatusConflict, "conflict", "REPAIR_NOT_AVAILABLE", err.Error(), nil)
			return
		}
		writeWorkflowError(w, r, err)
		return
	}
	detail, err := c.Svc.GetRun(r.Context(), runID)
	if err != nil {
		writeWorkflowError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusAccepted, WorkflowRepairResponse{
		Repair:   workflowRepairPlanView(plan),
		Intent:   workflowRepairIntentView(intent),
		Workflow: c.workflowRunDetailView(r.Context(), detail),
	})
}
