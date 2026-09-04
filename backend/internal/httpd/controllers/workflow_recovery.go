package controllers

import (
	"net/http"
	"strings"
	"time"

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
	RepairEligibility string `json:"repairEligibility" enum:"eligible,ineligible,budget_exhausted,policy_disabled,unknown_condition,artifact_unprovable"`
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
	// Execution is the technical detail behind the recommendation: which
	// attempt, whose provider, on what session, holding what authority. Absent
	// when AO cannot identify one (P3-D §12/§14).
	Execution *WorkflowRecoveryExecutionView `json:"execution,omitempty"`
	// Version is the assessment-rules version, so a recommendation stays
	// explainable after the rules change.
	Version string `json:"version,omitempty"`
}

// WorkflowRecoveryExecutionView is the execution a recovery answer is about.
//
// Identities and bounded classifications only (P3-D §35): no prompt text, no
// provider credentials, no terminal contents. Every field is projected from
// rows the run detail already carries, so nothing here can disagree with the
// ledger it came from.
type WorkflowRecoveryExecutionView struct {
	StepID   string `json:"stepId"`
	StepKind string `json:"stepKind,omitempty"`
	// AttemptID/AttemptNumber name the durable attempt row.
	AttemptID     string `json:"attemptId,omitempty"`
	AttemptNumber int64  `json:"attemptNumber,omitempty"`
	// Provider is the attempt's harness.
	Provider string `json:"provider,omitempty"`
	// SessionID is the agent session the step durably owns, when it owns one.
	SessionID string `json:"sessionId,omitempty"`
	// LifecycleState is the step's own state.
	LifecycleState string `json:"lifecycleState,omitempty"`
	// Authority is what AO may conclude about this attempt row: whether it is
	// the one that currently holds authority, history, superseded by a later
	// cycle, or a legacy row whose cycle cannot be proven.
	Authority string `json:"authority,omitempty" enum:"active,concluded,superseded,legacy_unproven"`
	// StartedAt/FinishedAt bound the attempt.
	StartedAt  string `json:"startedAt,omitempty"`
	FinishedAt string `json:"finishedAt,omitempty"`
	// Outcome/ErrorClass are the attempt's own conclusion, absent while open.
	Outcome    string `json:"outcome,omitempty"`
	ErrorClass string `json:"errorClass,omitempty"`
	// LastEventPhase/LastEventAt are the last thing AO can prove happened here.
	LastEventPhase string `json:"lastEventPhase,omitempty"`
	LastEventAt    string `json:"lastEventAt,omitempty"`
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
	Eligibility string `json:"eligibility" enum:"eligible,ineligible,budget_exhausted,policy_disabled,unknown_condition,artifact_unprovable"`
	// Mode, Spent and Budget are the frozen policy in force for this run.
	Mode   string `json:"mode" enum:"disabled,suggest,automatic"`
	Spent  int    `json:"spent"`
	Budget int    `json:"budget"`
	Reason string `json:"reason,omitempty"`
	// AutomaticAllowed reports whether AO may launch it unasked.
	AutomaticAllowed bool `json:"automaticAllowed"`
	// Intent is present only for an eligible repair.
	Intent *WorkflowRepairIntentView `json:"intent,omitempty"`
	// Artifact is what a repair would actually work on -- which branch, which
	// commit, which review's findings. It is present for an eligible repair AND
	// for an `artifact_unprovable` refusal, because that refusal's whole content
	// is which fact could not be established.
	Artifact *WorkflowRepairArtifactView `json:"artifact,omitempty"`
}

// WorkflowRepairArtifactView is the frozen identity of the thing under repair.
//
// It exists so an operator authorizing a repair is authorizing something
// specific, and so a refused one says what was missing rather than only that
// something was. See domain.RepairArtifactAuthority.
type WorkflowRepairArtifactView struct {
	// Resolved is whether AO established the artifact at all. Refusal names
	// which refusal when it did not.
	Resolved bool   `json:"resolved"`
	Refusal  string `json:"refusal,omitempty" enum:"repair_artifact_unavailable,repair_artifact_uncommitted,repair_workspace_mismatch"`
	Detail   string `json:"detail,omitempty"`
	// HasArtifact is false for an origin that has never executed. Such a repair
	// legitimately starts from the project's default branch.
	HasArtifact bool `json:"hasArtifact"`
	// OriginRunID/OriginTaskID place the artifact.
	OriginRunID  string `json:"originRunId,omitempty"`
	OriginTaskID string `json:"originTaskId,omitempty"`
	// Branch and BaseSHA are what a repair checkout is cut from.
	Branch  string `json:"branch,omitempty"`
	BaseSHA string `json:"baseSha,omitempty"`
	// Source says how BaseSHA was established.
	Source string `json:"source,omitempty" enum:"observed_worktree,reconstructed_from_ledger,shared_checkout,no_artifact"`
	// Placement is the origin's frozen execution placement.
	Placement string `json:"placement,omitempty" enum:"direct_branch,isolated_worktree"`
	// ReviewRunID and FindingsCount identify the review that asked for the
	// repair and how much it said. The findings themselves are not surfaced
	// here; they are on the review.
	ReviewRunID   string `json:"reviewRunId,omitempty"`
	FindingsCount int    `json:"findingsCount,omitempty"`
	// ChangedFiles is what the artifact changes relative to its merge target.
	ChangedFiles []string `json:"changedFiles,omitempty"`
}

func workflowRepairArtifactView(a domain.RepairArtifactAuthority) *WorkflowRepairArtifactView {
	if a.OriginRunID == "" {
		return nil
	}
	return &WorkflowRepairArtifactView{
		Resolved: a.Resolved, Refusal: string(a.Refusal), Detail: a.Detail,
		HasArtifact: a.HasArtifact, OriginRunID: a.OriginRunID, OriginTaskID: a.OriginTaskID,
		Branch: a.OriginBranch, BaseSHA: a.BaseSHA, Source: string(a.Source),
		Placement: string(a.Placement), ReviewRunID: a.ReviewRunID,
		FindingsCount: a.FindingsCount, ChangedFiles: a.ChangedFiles,
	}
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
	// Status is P3-D's "how is AO trying to recover this run": the state it is
	// in, what it is waiting for, and the bounded history that got it there.
	// Absent only when the daemon could not compose it.
	Status *WorkflowRecoveryStatusView `json:"status,omitempty"`
}

// WorkflowRecoveryStatusView is the recovery projection (P3-D §6/§7).
//
// It is deliberately NOT a second copy of the presentation: the presentation
// answers what a person reading the page sees, and this answers which of AO's
// own recovery mechanisms is in play. Identities and bounded classifications
// only — no prompt text, no pane contents, no credentials.
type WorkflowRecoveryStatusView struct {
	RunID  string `json:"runId"`
	TaskID string `json:"taskId,omitempty"`
	// State is the closed recovery vocabulary. `running` is deliberately not a
	// member: a run waiting for capacity, queued behind a branch lock, retrying
	// a provider or rebuilding after a restart are different situations with
	// different next steps.
	State string `json:"state" enum:"healthy_running,waiting_capacity,waiting_branch,waiting_provider,waiting_dialog_delivery,verifying_result,automatic_recovery_pending,repair_running,failover_running,restart_recovery,needs_human,terminal"`
	// Summary is the one sentence every surface leads with, composed on the
	// server so the Board, the run detail and the CLI cannot disagree.
	Summary string `json:"summary"`
	// AOIsActing reports that AO, not the person, is the next actor.
	AOIsActing bool   `json:"aoIsActing"`
	Waiting    bool   `json:"waiting"`
	StopReason string `json:"stopReason,omitempty"`
	// Execution is which attempt this is about.
	Execution  *WorkflowRecoveryExecutionView `json:"execution,omitempty"`
	Repair     WorkflowRecoveryRepairView     `json:"repair"`
	Failover   []WorkflowRecoveryAttemptView  `json:"failover,omitempty"`
	Capacity   WorkflowRecoveryCapacityView   `json:"capacity"`
	Branch     WorkflowRecoveryBranchView     `json:"branch"`
	Dialog     WorkflowRecoveryDialogView     `json:"dialog"`
	NextWakeAt *time.Time                     `json:"nextWakeAt,omitempty"`
	RetryCount int64                          `json:"retryCount,omitempty"`
	Timeline   []WorkflowRecoveryEventView    `json:"timeline,omitempty"`
	Version    string                         `json:"version,omitempty"`
}

// WorkflowRecoveryRepairView is the automatic repair in the terms a person asks
// about: attempt N of M, on what provider, why, and whether another remains.
type WorkflowRecoveryRepairView struct {
	Active            bool   `json:"active"`
	Attempt           int    `json:"attempt"`
	Budget            int    `json:"budget"`
	RunID             string `json:"runId,omitempty"`
	Exhausted         bool   `json:"exhausted"`
	NextRetryPossible bool   `json:"nextRetryPossible"`
	Quiescent         bool   `json:"quiescent"`
	WhyStarted        string `json:"whyStarted,omitempty"`
	Detail            string `json:"detail,omitempty"`
}

// WorkflowRecoveryAttemptView is one provider attempt in the failover chain.
type WorkflowRecoveryAttemptView struct {
	AttemptID     string     `json:"attemptId"`
	AttemptNumber int64      `json:"attemptNumber,omitempty"`
	Provider      string     `json:"provider,omitempty"`
	Outcome       string     `json:"outcome,omitempty"`
	ErrorClass    string     `json:"errorClass,omitempty"`
	StartedAt     *time.Time `json:"startedAt,omitempty"`
	FinishedAt    *time.Time `json:"finishedAt,omitempty"`
}

// WorkflowRecoveryCapacityView is the admission answer. `read` false means
// nobody looked, which is never the same as "no claims".
type WorkflowRecoveryCapacityView struct {
	Read            bool     `json:"read"`
	Waiting         int      `json:"waiting"`
	Held            int      `json:"held"`
	Kinds           []string `json:"kinds,omitempty"`
	ClaimID         string   `json:"claimId,omitempty"`
	DispatchKey     string   `json:"dispatchKey,omitempty"`
	FossilSuspected bool     `json:"fossilSuspected,omitempty"`
}

// WorkflowRecoveryBranchView is who owns the branch. Read-only projection: no
// GET in this file acquires or releases a lock.
type WorkflowRecoveryBranchView struct {
	Branch          string `json:"branch,omitempty"`
	HeldByRunID     string `json:"heldByRunId,omitempty"`
	HeldBySessionID string `json:"heldBySessionId,omitempty"`
	Waiting         bool   `json:"waiting"`
}

// WorkflowRecoveryDialogView is the question pipeline's position and nothing
// else about it: no prompt, no options, no keystrokes.
type WorkflowRecoveryDialogView struct {
	State      string `json:"state,omitempty" enum:"captured,resolving,delivery_pending,delivered,unreadable,human_required"`
	Source     string `json:"source,omitempty"`
	Unreadable bool   `json:"unreadable,omitempty"`
}

// WorkflowRecoveryEventView is one bounded, significant thing that happened.
type WorkflowRecoveryEventView struct {
	Kind   string    `json:"kind"`
	Phase  string    `json:"phase,omitempty"`
	At     time.Time `json:"at"`
	StepID string    `json:"stepId,omitempty"`
	Detail string    `json:"detail,omitempty"`
}

// workflowRecoveryStatusView projects the daemon's status onto the wire.
func workflowRecoveryStatusView(s workflowcore.RecoveryStatus) *WorkflowRecoveryStatusView {
	if s.RunID == "" {
		return nil
	}
	v := &WorkflowRecoveryStatusView{
		RunID:      s.RunID,
		TaskID:     s.TaskID,
		State:      string(s.State),
		Summary:    s.Describe(),
		AOIsActing: s.State.AOIsActing(),
		Waiting:    s.State.Waiting(),
		StopReason: s.StopReason,
		Execution:  workflowRecoveryExecutionView(s.Execution),
		Repair: WorkflowRecoveryRepairView{
			Active: s.Repair.Active, Attempt: s.Repair.Attempt, Budget: s.Repair.Budget,
			RunID: s.Repair.RunID, Exhausted: s.Repair.Exhausted,
			NextRetryPossible: s.Repair.NextRetryPossible, Quiescent: s.Repair.Quiescent,
			WhyStarted: s.Repair.WhyStarted, Detail: s.Repair.Detail,
		},
		Capacity: WorkflowRecoveryCapacityView{
			Read: s.Capacity.Read, Waiting: s.Capacity.Waiting, Held: s.Capacity.Held,
			Kinds: s.Capacity.Kinds, ClaimID: s.Capacity.ClaimID,
			DispatchKey: s.Capacity.DispatchKey, FossilSuspected: s.Capacity.FossilSuspected,
		},
		Branch: WorkflowRecoveryBranchView{
			Branch: s.Branch.Branch, HeldByRunID: s.Branch.HeldByRunID,
			HeldBySessionID: s.Branch.HeldBySessionID, Waiting: s.Branch.Waiting,
		},
		Dialog: WorkflowRecoveryDialogView{
			State: s.Dialog.State, Source: s.Dialog.Source, Unreadable: s.Dialog.Unreadable,
		},
		NextWakeAt: s.NextWakeAt,
		RetryCount: s.RetryCount,
		Version:    s.Version,
	}
	for _, a := range s.Failover {
		started := a.StartedAt
		v.Failover = append(v.Failover, WorkflowRecoveryAttemptView{
			AttemptID: a.AttemptID, AttemptNumber: a.AttemptNumber, Provider: a.Provider,
			Outcome: a.Outcome, ErrorClass: a.ErrorClass,
			StartedAt: timePtrOrNil(started), FinishedAt: a.FinishedAt,
		})
	}
	for _, e := range s.Timeline {
		v.Timeline = append(v.Timeline, WorkflowRecoveryEventView{
			Kind: e.Kind, Phase: e.Phase, At: e.At, StepID: e.StepID, Detail: e.Detail,
		})
	}
	return v
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
		Execution:         workflowRecoveryExecutionView(a.Execution),
		Version:           a.Version,
	}
}

// workflowRecoveryExecutionView projects the execution, or nothing at all when
// AO could not identify one. A zero-valued object would read as "AO looked and
// there is no attempt", which is a different and unearned claim.
func workflowRecoveryExecutionView(e workflowcore.RecoveryExecution) *WorkflowRecoveryExecutionView {
	if e.Empty() {
		return nil
	}
	v := &WorkflowRecoveryExecutionView{
		StepID:         e.StepID,
		StepKind:       e.StepKind,
		AttemptID:      e.AttemptID,
		AttemptNumber:  e.AttemptNumber,
		Provider:       e.Provider,
		SessionID:      e.SessionID,
		LifecycleState: e.LifecycleState,
		Authority:      string(e.Authority),
		Outcome:        e.Outcome,
		ErrorClass:     e.ErrorClass,
		LastEventPhase: e.LastEventPhase,
	}
	if !e.StartedAt.IsZero() {
		v.StartedAt = e.StartedAt.UTC().Format(time.RFC3339)
	}
	if e.FinishedAt != nil && !e.FinishedAt.IsZero() {
		v.FinishedAt = e.FinishedAt.UTC().Format(time.RFC3339)
	}
	if !e.LastEventAt.IsZero() {
		v.LastEventAt = e.LastEventAt.UTC().Format(time.RFC3339)
	}
	return v
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
		Intent:   workflowRepairIntentView(p.Intent),
		Artifact: workflowRepairArtifactView(p.Artifact),
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
	// The recovery status rides on the SAME route rather than a new one: "what
	// should I do" and "what is AO doing" are two halves of one question, and a
	// caller that had to ask twice could see two different moments. A daemon
	// that cannot compose it omits the field rather than failing the read — the
	// assessment above is still the answer an operator came for.
	if status, serr := svc.RecoveryStatusFor(r.Context(), runID); serr == nil {
		out.Status = workflowRecoveryStatusView(status)
	}
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
	runID := chi.URLParam(r, "workflowId")
	// P3-C §15: regeneration DISCARDS the plan the run is executing and mints a
	// new revision. A click computed against an earlier reading is the one case
	// here that can destroy something, so it is revalidated. Its sibling
	// /plan/reuse is deliberately NOT: it refuses anything but an exact plan
	// match, so a stale click there is a correct refusal or a correct no-op.
	var authority WorkflowActionAuthorityRequest
	_ = decodeJSON(r, &authority)
	if c.refuseStaleAction(w, r, runID, workflowcore.ActionRegeneratePlan, authority.authority()) {
		return
	}
	detail, assessment, err := svc.RegeneratePlan(r.Context(), runID)
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
	// P3-C §15: a Repair click computed against an earlier reading is refused
	// rather than duplicated. The body is optional — a client that sends no
	// authority proof still gets the pre-P3-C behaviour — but a repair that is
	// ALREADY running is refused either way, because that is a fact about now
	// rather than a comparison against what the caller captured.
	var authority WorkflowActionAuthorityRequest
	_ = decodeJSON(r, &authority)
	if c.refuseStaleAction(w, r, runID, workflowcore.ActionRepair, authority.authority()) {
		return
	}
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
